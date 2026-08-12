package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
)

// sonarr.go is Phase 6's read-only Sonarr data layer: series list, per-series
// episodes, per-series episode files, and the paged /wanted/cutoff (episode
// records, not movies). It mirrors radarr.go's shape and error-handling
// contract (plan §2.6: never return an error, only ok=false + a warning
// naming the instance) closely enough that a reader of one recognizes the
// other, while reusing radarr.go's shared machinery (fetchWantedCutoffPages,
// fetchBody, fetchLargeBody, derefOrAbsent, decodeTagIDs/rawObjectField)
// rather than forking a second copy of any of it — binding controller
// resolution.
//
// Season decision logic (evaluateSeries/evaluateSeason,
// runSonarrDecisionEngine) lives in decision.go alongside Radarr's, so the
// two engines' shared vocabulary (reason consts, cross-check shape, summary
// conventions) stays visibly in one place.

// seriesElement decodes the subset of one /api/v3/series array element the
// season decision engine needs. Fields are pointers (Tags is *[]int, same
// convention as movieListElement.Tags in radarr.go) so "key absent from the
// JSON" can be told apart from "key present with the zero value" — see
// systemStatusResponse (connectivity.go) for why that distinction matters.
// Seasons IS a pointer (REVIEW FIX, Phase 6 branch review): it was
// originally a bare slice on the theory that "seasons key absent" and
// "seasons": [] both mean "nothing to evaluate", which is true, but misses
// the binding note that "All load-bearing Sonarr fields decode through
// pointers" for a reason independent of that theory — a monitored series
// whose seasons key is absent (a field-name drift, a truncated/malformed
// response body that still happens to parse) must be told apart from one
// that genuinely has zero seasons, so evaluateSeries can warn on the absent
// case via warnIfFieldAbsent exactly like every other load-bearing field,
// instead of silently producing zero decisions and zero log lines for a
// series still counted in totalSeriesMonitored.
type seriesElement struct {
	ID               *int                   `json:"id"`
	Title            *string                `json:"title"`
	Monitored        *bool                  `json:"monitored"`
	QualityProfileID *int                   `json:"qualityProfileId"`
	Tags             *[]int                 `json:"tags"`
	Seasons          *[]seriesSeasonElement `json:"seasons"`
}

// seriesSeasonElement decodes one element of a series's "seasons" array —
// confirmed live shape: {seasonNumber, monitored, statistics{...}}.
type seriesSeasonElement struct {
	SeasonNumber *int                     `json:"seasonNumber"`
	Monitored    *bool                    `json:"monitored"`
	Statistics   *seasonStatisticsElement `json:"statistics"`
}

// seasonStatisticsElement decodes the subset of a season's "statistics"
// object the STRICT decision rule's rule 2 (complete on disk) needs:
// episodeFileCount == totalEpisodeCount && totalEpisodeCount > 0.
type seasonStatisticsElement struct {
	EpisodeFileCount  *int `json:"episodeFileCount"`
	TotalEpisodeCount *int `json:"totalEpisodeCount"`
}

// fetchSeriesLibrary streams GET /api/v3/series, mirroring fetchMovies
// (radarr.go) closely: a streaming decode (json.Decoder reading one array
// element into a json.RawMessage at a time, capped at
// movieStreamSanityLimit — reused rather than a second constant, since a
// large Sonarr library deserves the same defensive bound a large Radarr
// library does) so the raw response is never buffered in full, plus the
// same per-element tags-null-element normalization (REVIEW FIX, Phase 6,
// carried forward from the phase-5 branch review — see radarr.go's
// fetchMovies for the identical fix on the movie side): a null element
// inside an otherwise well-formed "tags" array (e.g. "tags": [3, null, 9])
// decodes through *[]int with NO error, silently becoming tag id 0, so it
// is independently re-checked against the raw bytes and, if found,
// normalizes Tags back to nil — the same shape "tags": null already
// produces — so it flows into the existing, already-tested untrusted-tags
// handling (evaluateSeries in decision.go) instead of trusting a corrupted
// array.
func fetchSeriesLibrary(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance) ([]seriesElement, bool) {
	resp, err := client.Do(ctx, http.MethodGet, "/api/v3/series", nil)
	if err != nil {
		logger.Warn("skipping instance: series request failed",
			"instance", inst.Name, "type", inst.Type, "error", err)
		return nil, false
	}
	defer resp.Body.Close()

	dec := json.NewDecoder(io.LimitReader(resp.Body, movieStreamSanityLimit))

	tok, err := dec.Token()
	if err != nil {
		logger.Warn("skipping instance: series response is not valid JSON",
			"instance", inst.Name, "type", inst.Type, "error", err)
		return nil, false
	}
	if delim, isDelim := tok.(json.Delim); !isDelim || delim != '[' {
		logger.Warn("skipping instance: series response is not a JSON array",
			"instance", inst.Name, "type", inst.Type)
		return nil, false
	}

	var series []seriesElement
	monitoredCount := 0

	for dec.More() {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			logger.Warn("skipping instance: series response element is not valid JSON",
				"instance", inst.Name, "type", inst.Type, "error", err)
			return nil, false
		}

		var s seriesElement
		if err := json.Unmarshal(raw, &s); err != nil {
			logger.Warn("skipping instance: series response element is not valid JSON",
				"instance", inst.Name, "type", inst.Type, "error", err)
			return nil, false
		}

		if s.Tags != nil {
			if rawTags, found := rawObjectField(raw, "tags"); found {
				if _, err := decodeTagIDs(rawTags); err != nil {
					logger.Warn("series tags array contains an unusable element (e.g. JSON null); treating tags as unverifiable",
						"instance", inst.Name, "type", inst.Type, "title", titleOrAbsent(s.Title), "error", err)
					s.Tags = nil
				}
			}
		}

		if s.Monitored != nil && *s.Monitored {
			monitoredCount++
		}
		series = append(series, s)
	}

	// Same rationale as fetchMovies (radarr.go): dec.More() returning false
	// cannot on its own distinguish "the array legitimately closed" from "the
	// response was cut off partway through" (e.g. a body cut exactly between
	// two complete elements). Requiring the next token to be the closing "]"
	// catches a truncated tail instead of silently returning partial data.
	closeTok, err := dec.Token()
	if err != nil || closeTok != json.Delim(']') {
		logger.Warn("skipping instance: series response is not a well-formed JSON array (missing closing bracket)",
			"instance", inst.Name, "type", inst.Type, "error", err)
		return nil, false
	}

	logger.Info("series library",
		"instance", inst.Name, "type", inst.Type, "total", len(series), "monitored", monitoredCount)

	return series, true
}

// episodeElement decodes the subset of one /api/v3/episode?seriesId=X array
// element the season decision engine needs. AirDateUtc is decoded as
// *string, not *time.Time: a malformed date string then fails only THIS
// episode's airing-guard determination (episodeHasAired in decision.go
// treats a parse failure the same as "absent" — fail safe, per the binding
// airing-guard rule), rather than an invalid date anywhere in a large
// episode list aborting the entire decode the way a *time.Time field's
// UnmarshalJSON error would.
type episodeElement struct {
	ID            *int    `json:"id"`
	SeasonNumber  *int    `json:"seasonNumber"`
	EpisodeNumber *int    `json:"episodeNumber"`
	Monitored     *bool   `json:"monitored"`
	HasFile       *bool   `json:"hasFile"`
	AirDateUtc    *string `json:"airDateUtc"`
	EpisodeFileID *int    `json:"episodeFileId"`
}

// fetchEpisodes fetches GET /api/v3/episode?seriesId=<seriesID> for a single
// series — every episode across every season in one call, per the confirmed
// live API shape. Used only for a series that has already passed every
// series-level check (monitored, tags, profile+upgradeAllowed) and has at
// least one monitored season (binding evaluation-order resolution: series-
// level cheap checks first, exactly one /episode fetch per surviving
// series). A request or decode failure is per-series, not instance-fatal
// (§2.6): the caller treats ok=false as "could not fetch episode data" and
// skips just this series's monitored seasons, continuing to the rest of the
// library.
func fetchEpisodes(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance, seriesID int) ([]episodeElement, bool) {
	// REVIEW FIX (Phase 6 final round, F2): fetchLargeBody (radarr.go), not
	// connectivity.go's fetchBody, which caps at 4 MB — a limit meant for
	// small, fixed-shape endpoints like /system/status and /qualityprofile,
	// not a per-series episode list. A large real-world library (this
	// deploy's live anime library has 1000+-episode series) can plausibly
	// exceed 4 MB here, at which point that series would be skipped on
	// every cycle forever, reported as a transport failure rather than a
	// size ceiling it never actually needed to hit. Matches the existing
	// Radarr precedent: /movie and /wanted/cutoff both use the 512 MB limit.
	query := url.Values{"seriesId": {strconv.Itoa(seriesID)}}
	body, err := fetchLargeBody(ctx, client, "/api/v3/episode", query)
	if err != nil {
		logger.Warn("episode request failed",
			"instance", inst.Name, "type", inst.Type, "seriesId", seriesID, "error", err)
		return nil, false
	}

	var episodes []episodeElement
	if err := json.Unmarshal(body, &episodes); err != nil {
		logger.Warn("episode response is not valid JSON",
			"instance", inst.Name, "type", inst.Type, "seriesId", seriesID, "error", err)
		return nil, false
	}

	return episodes, true
}

// episodeFileElement decodes one element of the array GET
// /api/v3/episodefile?seriesId=<seriesID> returns — the only endpoint where
// customFormatScore actually exists for a Sonarr episode file (mirrors
// Radarr's /moviefile discovery from Phase 2: the score lives on the file
// endpoint only, confirmed on live Sonarr 4.0.19.2979).
type episodeFileElement struct {
	ID                  *int  `json:"id"`
	SeasonNumber        *int  `json:"seasonNumber"`
	CustomFormatScore   *int  `json:"customFormatScore"`
	QualityCutoffNotMet *bool `json:"qualityCutoffNotMet"`
}

// fetchEpisodeFiles fetches GET /api/v3/episodefile?seriesId=<seriesID> —
// every episode file for the series, across every season, in one call.
// evaluateSeries calls this for a series with at least one season that has
// already passed rules 1-6 of the STRICT decision rule (binding
// evaluation-order resolution: this is the most expensive fetch to avoid
// making needlessly, fetched lazily and at most once per series during
// evaluation). runSonarrCrossCheck (decision.go) is the one other caller
// (REVIEW FIX): it makes its own small, bounded, on-demand call to this
// same function for sampled skip-side seasons that never reached rule 7 at
// all, so their skip reason can still be independently verified. A request
// or decode failure is per-series, not instance-fatal (§2.6).
func fetchEpisodeFiles(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance, seriesID int) ([]episodeFileElement, bool) {
	// REVIEW FIX (Phase 6 final round, F2): same fetchLargeBody swap as
	// fetchEpisodes above, for the same reason — a large series's episode
	// file list is at least as likely to cross the 4 MB fetchBody cap as
	// its episode list.
	query := url.Values{"seriesId": {strconv.Itoa(seriesID)}}
	body, err := fetchLargeBody(ctx, client, "/api/v3/episodefile", query)
	if err != nil {
		logger.Warn("episodefile request failed",
			"instance", inst.Name, "type", inst.Type, "seriesId", seriesID, "error", err)
		return nil, false
	}

	var files []episodeFileElement
	if err := json.Unmarshal(body, &files); err != nil {
		logger.Warn("episodefile response is not valid JSON",
			"instance", inst.Name, "type", inst.Type, "seriesId", seriesID, "error", err)
		return nil, false
	}

	return files, true
}

// sonarrWantedRecord decodes one Sonarr /api/v3/wanted/cutoff record: an
// EPISODE, not a movie (contrast wantedCutoffRecord, radarr.go). Per the
// confirmed live API facts, records also carry episodeNumber, title,
// airDateUtc, monitored, and hasFile; this phase only needs id (the
// cross-check's per-episode qualityCutoffNotMet comparison) and
// seriesId+seasonNumber (rule 4's season lookup), so the rest are not
// decoded here.
type sonarrWantedRecord struct {
	ID           *int `json:"id"`
	SeriesID     *int `json:"seriesId"`
	SeasonNumber *int `json:"seasonNumber"`
}

// seasonKey identifies one season across a whole Sonarr instance: seriesId
// and seasonNumber together, since season numbers repeat across different
// series (every show has its own season 1).
type seasonKey struct {
	seriesID     int
	seasonNumber int
}

// fetchSonarrWantedCutoff fully pages GET /api/v3/wanted/cutoff by reusing
// fetchWantedCutoffPages (radarr.go) — the shared paging machinery and
// completeness contract, not a second copy of it — building BOTH:
//
//   - wantedEpisodeIDs: every episode id present in the wanted/cutoff set,
//     for the cross-check's per-episode qualityCutoffNotMet comparison
//     (runSonarrCrossCheck, decision.go).
//   - wantedSeasons: the (seriesId, seasonNumber) lookup built DIRECTLY from
//     the wanted records' own fields — binding controller resolution
//     ("Build the season lookup keyed on (seriesId, seasonNumber) directly
//     from records"). This answers rule 4 ("no episode of that season
//     appears in /wanted/cutoff") without ever needing to correlate ids
//     against the separately-fetched /episode data.
//
// A record missing id, seriesId, OR seasonNumber makes the WHOLE result
// untrustworthy — the same completeness contract fetchWantedCutoff
// (Radarr) applies to a record missing its id: without seriesId/
// seasonNumber the record can't be placed into wantedSeasons, and without
// id it can't be placed into wantedEpisodeIDs, so either gap means the
// derived data can never be reconstructed as complete, and
// fetchWantedCutoffPages's handleRecord-returns-false contract makes that
// instance-fatal (warn + skip, ok=false) rather than silently partial.
func fetchSonarrWantedCutoff(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance) (wantedEpisodeIDs map[int]bool, wantedSeasons map[seasonKey]bool, ok bool) {
	wantedEpisodeIDs = make(map[int]bool)
	wantedSeasons = make(map[seasonKey]bool)

	fetched, totalRecords, ok := fetchWantedCutoffPages(ctx, logger, client, inst, func(page int, raw json.RawMessage) bool {
		var r sonarrWantedRecord
		if err := json.Unmarshal(raw, &r); err != nil {
			logger.Warn("skipping instance: wanted/cutoff record is not valid JSON",
				"instance", inst.Name, "type", inst.Type, "page", page, "error", err)
			return false
		}
		if r.ID == nil || r.SeriesID == nil || r.SeasonNumber == nil {
			logger.Warn("skipping instance: wanted/cutoff record missing id, seriesId, or seasonNumber; the result cannot be trusted",
				"instance", inst.Name, "type", inst.Type, "page", page,
				"id", derefOrAbsent(r.ID), "seriesId", derefOrAbsent(r.SeriesID), "seasonNumber", derefOrAbsent(r.SeasonNumber))
			return false
		}
		wantedEpisodeIDs[*r.ID] = true
		wantedSeasons[seasonKey{seriesID: *r.SeriesID, seasonNumber: *r.SeasonNumber}] = true
		return true
	})
	if !ok {
		return nil, nil, false
	}

	logger.Info("wanted/cutoff",
		"instance", inst.Name, "type", inst.Type, "totalRecords", totalRecords, "fetched", fetched)

	return wantedEpisodeIDs, wantedSeasons, true
}

// inspectSonarrLibrary performs this phase's read-only Sonarr library
// inspection for a single instance: GET /api/v3/series followed by the
// fully-paged GET /api/v3/wanted/cutoff. Mirrors inspectRadarrLibrary
// (radarr.go) exactly in shape and error-handling contract (plan §2.6):
// never returns an error, ok=false means either fetch failed or produced an
// unusable (partial) result, and every other return value is nil in that
// case — callers (the season decision engine) must not proceed.
func inspectSonarrLibrary(ctx context.Context, logger *slog.Logger, inst Instance) ([]seriesElement, map[int]bool, map[seasonKey]bool, bool) {
	client := NewAPIClient(inst.URL, inst.APIKey)

	series, ok := fetchSeriesLibrary(ctx, logger, client, inst)
	if !ok {
		return nil, nil, nil, false
	}

	wantedEpisodeIDs, wantedSeasons, ok := fetchSonarrWantedCutoff(ctx, logger, client, inst)
	if !ok {
		return nil, nil, nil, false
	}

	return series, wantedEpisodeIDs, wantedSeasons, true
}
