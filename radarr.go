package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// movieStreamSanityLimit bounds how much of the /movie response this phase
// will read before giving up. It deliberately supersedes maxResponseBodyBytes
// (4 MB, connectivity.go) for this endpoint only: a real movie library can
// legitimately exceed 4 MB, but an unbounded read is still not acceptable.
// Reaching this cap, or any other body truncation, is treated the same as
// any other decode failure per plan §2.6 (warn and skip the instance for
// the cycle). Truncation is caught either by the JSON decoder failing
// outright mid-element, or — for a body cut exactly on an element boundary,
// which the decoder does not treat as an error on its own — by the
// explicit closing-bracket check after the element loop in fetchMovies.
const movieStreamSanityLimit = 512 * 1024 * 1024 // 512 MB

// wantedCutoffPageSize is the page size requested from /wanted/cutoff, per
// plan §5.
const wantedCutoffPageSize = 100

// maxWantedCutoffPages bounds how many pages of /wanted/cutoff this phase
// will fetch, guarding against a server whose totalRecords never becomes
// reachable (e.g. a buggy or inconsistent value). It is a var rather than a
// const so tests can lower it and exercise the cap without making 1000 real
// HTTP round trips.
var maxWantedCutoffPages = 1000

// movieListElement decodes the subset of one /api/v3/movie array element
// that this phase cares about. Fields are pointers so that "key absent from
// the JSON" (nil) can be told apart from "key present with the zero value"
// — see systemStatusResponse in connectivity.go for why this distinction
// matters and drives the warn-on-absent-key mechanism reused here.
type movieListElement struct {
	ID               *int              `json:"id"`
	Title            *string           `json:"title"`
	Monitored        *bool             `json:"monitored"`
	HasFile          *bool             `json:"hasFile"`
	QualityProfileID *int              `json:"qualityProfileId"`
	Tags             *[]int            `json:"tags"`
	MovieFile        *movieFileElement `json:"movieFile"`
}

// movieFileElement decodes the subset of a movie's movieFile object that
// this phase cares about. It is only expected to be present at all when the
// movie actually has a file (hasFile == true); its absence is normal and
// must never warn.
//
// ID and QualityCutoffNotMet are used by the Phase 3 decision engine, not
// by Phase 2's --samples logging (which continues to warn only on Quality
// and CustomFormatScore per the existing convention — see logSampleMovies):
//   - ID correlates this movie's file against the array GET
//     /api/v3/moviefile?movieId=<id> returns (per the STRICT decision
//     rule's tie-break: "take the element whose id matches the movie's
//     movieFile.id, or the single element").
//   - QualityCutoffNotMet is Radarr's own quality-cutoff determination for
//     this file, used only by the decision engine's cross-check to
//     independently verify the /wanted/cutoff-derived id set against data
//     computed by a different Radarr code path (never used to *decide*
//     would-unmonitor/skip, per the STRICT rule §5 which is wanted/cutoff
//     set membership only).
type movieFileElement struct {
	ID                  *int                     `json:"id"`
	Quality             *movieFileQualityElement `json:"quality"`
	CustomFormatScore   *int                     `json:"customFormatScore"`
	QualityCutoffNotMet *bool                    `json:"qualityCutoffNotMet"`
}

// movieFileQualityElement mirrors the expected movieFile.quality.quality.name
// nesting from plan §5 (a "quality" object with its own nested "quality"
// field).
type movieFileQualityElement struct {
	Quality *movieFileQualityNameElement `json:"quality"`
}

type movieFileQualityNameElement struct {
	Name *string `json:"name"`
}

// movieFileQualityName walks the movieFile.quality.quality.name chain,
// returning nil if any link is absent. Kept separate from the struct
// literals above so the multi-level pointer chase happens in one place.
func movieFileQualityName(mf *movieFileElement) *string {
	if mf == nil || mf.Quality == nil || mf.Quality.Quality == nil {
		return nil
	}
	return mf.Quality.Quality.Name
}

// wantedCutoffEnvelope decodes the /api/v3/wanted/cutoff paging envelope
// shared by Radarr (movies) and Sonarr (episodes, Phase 6): only the paging
// shape (page/totalRecords/records) is common between them, so Records is
// left as raw JSON here and decoded into the endpoint-specific record shape
// by each caller's own handleRecord callback (see fetchWantedCutoffPages).
// Per plan §5, an entirely absent totalRecords means paging cannot be done
// safely (we cannot tell when to stop) and is treated as malformed per
// §2.6: warn and skip the instance for the cycle.
type wantedCutoffEnvelope struct {
	TotalRecords *int               `json:"totalRecords"`
	Records      *[]json.RawMessage `json:"records"`
}

// wantedCutoffRecord decodes one Radarr /api/v3/wanted/cutoff record: a
// movie, identified minimally by id (title is kept only for the
// missing-id warning's context).
type wantedCutoffRecord struct {
	ID    *int    `json:"id"`
	Title *string `json:"title"`
}

// movieCounts summarizes the full /movie library scan.
type movieCounts struct {
	total     int
	monitored int
	hasFile   int
}

// sampleMovieMatch holds both the decoded (pointer-typed) view of a sample
// movie and its exact raw JSON bytes, so the raw bytes can be dumped at
// debug level without any re-marshaling.
type sampleMovieMatch struct {
	element movieListElement
	raw     json.RawMessage
}

// inspectRadarrLibrary performs this phase's read-only Radarr library
// inspection for a single instance: GET /api/v3/movie followed by the
// fully-paged GET /api/v3/wanted/cutoff. Like checkInstanceConnectivity, it
// never returns an error: the binding error-handling rule (plan §2.6) is
// "skip that instance for the cycle and log a warning", so callers loop
// over every configured radarr instance regardless of what happened to any
// previous one. It returns every decoded movie (refactor b) and the
// wanted/cutoff id set so a caller (the Phase 3 decision engine) can
// evaluate the whole library without re-fetching either endpoint; ok=false
// means either fetch failed or produced an unusable (partial) result, and
// movies/wantedIDs are both nil in that case — callers must not proceed.
func inspectRadarrLibrary(ctx context.Context, logger *slog.Logger, inst Instance, samples []string) ([]movieListElement, map[int]bool, bool) {
	client := NewAPIClient(inst.URL, inst.APIKey)

	counts, movies, matches, ok := fetchMovies(ctx, logger, client, inst, samples)
	if !ok {
		return nil, nil, false
	}

	logger.Info("movie library",
		"instance", inst.Name, "type", inst.Type,
		"total", counts.total, "monitored", counts.monitored, "hasFile", counts.hasFile)

	logSampleMovies(logger, inst, samples, matches)

	wantedIDs, ok := fetchWantedCutoff(ctx, logger, client, inst)
	if !ok {
		return nil, nil, false
	}

	logSampleCutoffStatus(logger, inst, samples, matches, wantedIDs)

	return movies, wantedIDs, true
}

// fetchMovies streams GET /api/v3/movie, tallying library-wide counts,
// capturing the decoded fields plus raw JSON for any movie whose title
// matches a configured sample, and (refactor b) collecting every decoded
// movie into a returned slice so the decision engine can evaluate the
// whole library from this single fetch.
//
// FIX 7 (controller-mandated correction after the initial Phase 3 review):
// decoding is streaming (json.Decoder reading one array element into a
// json.RawMessage at a time) rather than json.Unmarshal-ing the whole body
// at once, so the raw response is never buffered in full — required
// because /movie can exceed the 4 MB cap used for the connectivity
// endpoints. This bounds the memory cost of the *raw JSON* to roughly one
// element at a time, same as before refactor (b). It does NOT bound the
// function's overall peak memory to a single movie's size any more: every
// decoded movieListElement is now retained in the returned movies slice
// for the whole call (that is refactor (b)'s entire purpose — the decision
// engine needs every movie), so overall memory is O(number of movies) —
// the size of one decoded struct times the library size, which is far
// smaller than the raw JSON per movie would be, but is no longer O(1).
// sampleMovieMatch.raw (the exact bytes for --samples output) is retained
// only for movies matching a configured sample, unchanged from before.
func fetchMovies(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance, samples []string) (movieCounts, []movieListElement, map[string]sampleMovieMatch, bool) {
	var counts movieCounts
	wanted := sampleLookupSet(samples)
	matches := make(map[string]sampleMovieMatch, len(wanted))
	var movies []movieListElement

	resp, err := client.Do(ctx, http.MethodGet, "/api/v3/movie", nil)
	if err != nil {
		logger.Warn("skipping instance: movie request failed",
			"instance", inst.Name, "type", inst.Type, "error", err)
		return counts, nil, nil, false
	}
	defer resp.Body.Close()

	dec := json.NewDecoder(io.LimitReader(resp.Body, movieStreamSanityLimit))

	tok, err := dec.Token()
	if err != nil {
		logger.Warn("skipping instance: movie response is not valid JSON",
			"instance", inst.Name, "type", inst.Type, "error", err)
		return counts, nil, nil, false
	}
	if delim, isDelim := tok.(json.Delim); !isDelim || delim != '[' {
		logger.Warn("skipping instance: movie response is not a JSON array",
			"instance", inst.Name, "type", inst.Type)
		return counts, nil, nil, false
	}

	for dec.More() {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			logger.Warn("skipping instance: movie response element is not valid JSON",
				"instance", inst.Name, "type", inst.Type, "error", err)
			return counts, nil, nil, false
		}

		var m movieListElement
		if err := json.Unmarshal(raw, &m); err != nil {
			logger.Warn("skipping instance: movie response element is not valid JSON",
				"instance", inst.Name, "type", inst.Type, "error", err)
			return counts, nil, nil, false
		}

		// REVIEW FIX (Phase 6, carried forward from the phase-5 branch
		// review): movieListElement.Tags is *[]int, so the struct decode
		// above already tells "tags absent" apart from "tags present" — but
		// a *[]int destination still silently turns a null ARRAY ELEMENT
		// ("tags": [3, null, 9]) into tag id 0 with no decode error at all,
		// since encoding/json leaves a non-pointer destination at its zero
		// value when it meets JSON null. Re-inspecting the raw "tags" bytes
		// through decodeTagIDs (shared.go), which decodes through []*int
		// first specifically to make a null element visible, catches what
		// the struct decode cannot. This is per-movie, not instance-fatal
		// (unlike the decode failure above): the movie's Tags is normalized
		// back to nil, exactly what "tags": null already produces, so it
		// flows into decision.go rule 4's already-correct, already-tested
		// untrusted-tags handling instead of silently trusting a corrupted
		// array.
		if m.Tags != nil {
			if rawTags, found := rawObjectField(raw, "tags"); found {
				if _, err := decodeTagIDs(rawTags); err != nil {
					logger.Warn("movie tags array contains an unusable element (e.g. JSON null); treating tags as unverifiable",
						"instance", inst.Name, "type", inst.Type, "title", titleOrAbsent(m.Title), "error", err)
					m.Tags = nil
				}
			}
		}

		counts.total++
		if m.Monitored != nil && *m.Monitored {
			counts.monitored++
		}
		if m.HasFile != nil && *m.HasFile {
			counts.hasFile++
		}

		movies = append(movies, m)

		if m.Title != nil {
			key := normalizeTitle(*m.Title)
			if wanted[key] {
				// If two library movies normalize to the same title (e.g.
				// two cuts/releases sharing a name), the later one in scan
				// order silently overwrites the earlier match here. Known
				// limitation, acceptable for Phase 2's ad hoc sampling —
				// disambiguating same-titled movies (e.g. by year) is out
				// of scope for this phase.
				matches[key] = sampleMovieMatch{element: m, raw: raw}
			}
		}
	}

	// dec.More() above returns false both when the array closed cleanly
	// (the next token really is "]") and when the underlying reader hit
	// EOF or the sanity limit partway through — e.g. a body cut exactly
	// between two complete elements (server died, proxy dropped the
	// connection). It swallows that read error rather than surfacing it,
	// so on its own the loop above cannot tell "the library legitimately
	// ended" apart from "the response was cut off here". Explicitly
	// requiring the next token to be the closing "]" catches the latter:
	// a truncated tail fails here instead of silently returning partial
	// counts as if nothing were wrong.
	closeTok, err := dec.Token()
	if err != nil || closeTok != json.Delim(']') {
		logger.Warn("skipping instance: movie response is not a well-formed JSON array (missing closing bracket)",
			"instance", inst.Name, "type", inst.Type, "error", err)
		return counts, nil, nil, false
	}

	return counts, movies, matches, true
}

// logSampleMovies logs, for each configured sample title (in the order
// given), either its full detail (info) plus raw JSON (debug), or a warning
// naming it when no library movie matched. Non-sample movies are never
// logged per-movie, per plan scope.
func logSampleMovies(logger *slog.Logger, inst Instance, samples []string, matches map[string]sampleMovieMatch) {
	for _, sample := range samples {
		match, found := matches[normalizeTitle(sample)]
		if !found {
			logger.Warn("sample title not found in movie library",
				"instance", inst.Name, "type", inst.Type, "sample", sample)
			continue
		}

		m := match.element
		logger.Info("sample movie",
			"instance", inst.Name, "type", inst.Type, "sample", sample,
			"id", derefOrAbsent(m.ID), "title", derefOrAbsent(m.Title),
			"monitored", derefOrAbsent(m.Monitored), "hasFile", derefOrAbsent(m.HasFile),
			"qualityProfileId", derefOrAbsent(m.QualityProfileID), "tags", fmt.Sprint(derefOrAbsent(m.Tags)))

		warnIfFieldAbsent(logger, inst, "movie", "id", m.ID == nil)
		warnIfFieldAbsent(logger, inst, "movie", "monitored", m.Monitored == nil)
		warnIfFieldAbsent(logger, inst, "movie", "hasFile", m.HasFile == nil)
		warnIfFieldAbsent(logger, inst, "movie", "qualityProfileId", m.QualityProfileID == nil)
		warnIfFieldAbsent(logger, inst, "movie", "tags", m.Tags == nil)

		// Quality name and custom format score are only expected to exist
		// when the movie actually has a file; movieFile being nil entirely
		// (no file yet) is normal and must not warn about either field.
		if m.MovieFile != nil {
			qualityName := movieFileQualityName(m.MovieFile)
			logger.Info("sample movie file",
				"instance", inst.Name, "type", inst.Type, "sample", sample,
				"qualityName", derefOrAbsent(qualityName), "customFormatScore", derefOrAbsent(m.MovieFile.CustomFormatScore))

			warnIfFieldAbsent(logger, inst, "movie", "movieFile.quality.quality.name", qualityName == nil)
			warnIfFieldAbsent(logger, inst, "movie", "movieFile.customFormatScore", m.MovieFile.CustomFormatScore == nil)
		}

		logger.Debug("sample movie raw response",
			"instance", inst.Name, "type", inst.Type, "sample", sample, "body", string(match.raw))
	}
}

// fetchWantedCutoffPages fully pages GET /api/v3/wanted/cutoff
// (pageSize=100), invoking handleRecord once per raw record element in page
// order, and owns nothing about what a record MEANS — that is entirely up
// to the caller's handleRecord (a movie for Radarr, an episode for Sonarr;
// see fetchWantedCutoff below and fetchSonarrWantedCutoff in sonarr.go).
// What this function owns is the paging machinery both endpoints share
// (mandated refactor, generalized further in Phase 6 rather than forking a
// second copy for Sonarr): the defensive completeness contract —
//
//   - totalRecords absent from the first page's envelope means paging
//     cannot be done safely; warn and skip the instance for the cycle
//     (ok=false), same as any other malformed response.
//   - a page returning 0 records ends paging; if fewer records were fetched
//     than totalRecords claimed, the result is only partial.
//   - a hard cap of maxWantedCutoffPages bounds the loop; hitting it without
//     reaching totalRecords also leaves the result partial.
//   - handleRecord returning false makes the WHOLE result untrustworthy from
//     that point on (mirrors a Radarr record missing its id, or a Sonarr
//     record missing seriesId/seasonNumber: without the field the record can
//     never be reconstructed, so nothing built from it can be authoritative)
//     — handleRecord is responsible for logging its own specific reason
//     before returning false, since only it knows what shape it expected.
//
// completeness contract (mandated refactor, extended by a controller-
// mandated fix to also cover the handleRecord-false case): an incomplete or
// untrustworthy result — any of the above — returns ok=false (warn +
// instance skipped), never a partial success. Both callers' decision
// engines treat absence from their derived set as "would-unmonitor"; a
// partial result would silently manufacture false positives in that
// dangerous direction, so it must never be handed off as if it were the
// whole truth.
func fetchWantedCutoffPages(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance, handleRecord func(page int, raw json.RawMessage) bool) (fetched, totalRecords int, ok bool) {
	completed := false
	// endedEarly distinguishes, for the post-loop incomplete-result warning,
	// "stopped because a page returned 0 records before totalRecords was
	// reached" (already warned about specifically inside the loop above)
	// from "ran out of the maxWantedCutoffPages budget" (warned about
	// below, only in the latter case, so the message accurately says what
	// actually happened).
	endedEarly := false

	for page := 1; page <= maxWantedCutoffPages; page++ {
		query := url.Values{
			"page":     {strconv.Itoa(page)},
			"pageSize": {strconv.Itoa(wantedCutoffPageSize)},
		}

		body, err := fetchLargeBody(ctx, client, "/api/v3/wanted/cutoff", query)
		if err != nil {
			logger.Warn("skipping instance: wanted/cutoff request failed",
				"instance", inst.Name, "type", inst.Type, "page", page, "error", err)
			return 0, 0, false
		}

		var envelope wantedCutoffEnvelope
		if err := json.Unmarshal(body, &envelope); err != nil {
			logger.Warn("skipping instance: wanted/cutoff response is not valid JSON",
				"instance", inst.Name, "type", inst.Type, "page", page, "error", err)
			return 0, 0, false
		}

		// An entirely absent "records" key is malformed per §2.6: there is
		// no way to tell whether the server simply omitted the field
		// (our assumed field name may be wrong) or genuinely meant "no
		// records on this page", so it cannot be treated the same as a
		// present-but-empty "records": [] (handled below as the normal
		// end-of-paging case).
		if envelope.Records == nil {
			logger.Warn("skipping instance: wanted/cutoff response missing records",
				"instance", inst.Name, "type", inst.Type, "page", page)
			return 0, 0, false
		}
		records := *envelope.Records

		if page == 1 {
			if envelope.TotalRecords == nil {
				logger.Warn("skipping instance: wanted/cutoff response missing totalRecords",
					"instance", inst.Name, "type", inst.Type)
				return 0, 0, false
			}
			totalRecords = *envelope.TotalRecords
		}

		if len(records) == 0 {
			if fetched != totalRecords {
				// Leave completed=false: fewer records were fetched than
				// totalRecords claimed, so the result below is only
				// partial. Per the completeness contract this must not be
				// returned as ok=true; the check after the loop handles it.
				logger.Warn("wanted/cutoff paging stopped: page returned 0 records before totalRecords was reached",
					"instance", inst.Name, "type", inst.Type, "page", page, "fetched", fetched, "totalRecords", totalRecords)
				endedEarly = true
			} else {
				completed = true
			}
			break
		}

		for _, raw := range records {
			if !handleRecord(page, raw) {
				return 0, 0, false
			}
		}
		fetched += len(records)

		if fetched >= totalRecords {
			completed = true
			break
		}
	}

	if !completed {
		if !endedEarly {
			logger.Warn("wanted/cutoff paging hit the page cap without completing",
				"instance", inst.Name, "type", inst.Type, "pageCap", maxWantedCutoffPages, "fetched", fetched, "totalRecords", totalRecords)
		}
		logger.Warn("skipping instance: wanted/cutoff id set is incomplete",
			"instance", inst.Name, "type", inst.Type, "fetched", fetched, "totalRecords", totalRecords)
		return 0, 0, false
	}

	return fetched, totalRecords, true
}

// fetchWantedCutoff fully pages GET /api/v3/wanted/cutoff via
// fetchWantedCutoffPages, returning the set of movie ids it contains. A
// record missing its id can never be added to the set and can never be
// reconstructed, so it makes the whole set non-authoritative (handleRecord
// returns false, which fetchWantedCutoffPages treats as instance-fatal —
// see its doc comment for the completeness contract this preserves).
func fetchWantedCutoff(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance) (map[int]bool, bool) {
	ids := make(map[int]bool)

	fetched, totalRecords, ok := fetchWantedCutoffPages(ctx, logger, client, inst, func(page int, raw json.RawMessage) bool {
		var r wantedCutoffRecord
		if err := json.Unmarshal(raw, &r); err != nil {
			logger.Warn("skipping instance: wanted/cutoff record is not valid JSON",
				"instance", inst.Name, "type", inst.Type, "page", page, "error", err)
			return false
		}
		if r.ID == nil {
			// Without an id this record can't be cross-referenced against
			// the /movie library at all; title is the only other
			// identifying information the envelope carries, so it is the
			// natural context to report here. This makes the whole id set
			// non-authoritative — there is no way to recover which movie
			// this record meant, so its true cutoff-not-met membership can
			// never be reconstructed — which is the same "partial set
			// masquerading as complete" hazard refactor (a) guards against
			// for the empty-page-early and page-cap cases: warn and skip
			// the instance for the cycle instead of silently returning an
			// incomplete set as if it were the whole truth.
			logger.Warn("skipping instance: wanted/cutoff record missing id field; id set cannot be trusted",
				"instance", inst.Name, "type", inst.Type, "page", page, "title", derefOrAbsent(r.Title))
			return false
		}
		ids[*r.ID] = true
		return true
	})
	if !ok {
		return nil, false
	}

	logger.Info("wanted/cutoff",
		"instance", inst.Name, "type", inst.Type, "totalRecords", totalRecords, "fetched", fetched)

	return ids, true
}

// logSampleCutoffStatus logs, for each sample that matched a library movie
// with a known id, whether that id is present in the /wanted/cutoff set.
// Samples that were never found in the library (already warned about by
// logSampleMovies) or whose id field was absent are silently skipped here.
func logSampleCutoffStatus(logger *slog.Logger, inst Instance, samples []string, matches map[string]sampleMovieMatch, wantedIDs map[int]bool) {
	for _, sample := range samples {
		match, found := matches[normalizeTitle(sample)]
		if !found || match.element.ID == nil {
			continue
		}
		id := *match.element.ID
		logger.Info("sample cutoff status",
			"instance", inst.Name, "type", inst.Type, "sample", sample,
			"id", id, "inWantedCutoff", wantedIDs[id])
	}
}

// fetchLargeBody issues a GET request against path with the given query
// parameters (via APIClient.DoQuery) and returns the full response body,
// capped at movieStreamSanityLimit. It mirrors fetchBody in connectivity.go
// (same cap-reached-means-malformed treatment, same DoQuery plumbing) but
// is kept as separate code here rather than sharing that function, because
// it uses a different, much larger cap required by this phase's binding
// large-response handling (the 4 MB connectivity cap is too small for
// /wanted/cutoff, even though individual pages are small).
func fetchLargeBody(ctx context.Context, client *APIClient, path string, query url.Values) ([]byte, error) {
	resp, err := client.DoQuery(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, movieStreamSanityLimit))
	if err != nil {
		return nil, fmt.Errorf("reading response body from %s: %w", path, err)
	}
	if len(body) == movieStreamSanityLimit {
		return nil, fmt.Errorf("reading response body from %s: response reached the %d byte limit (possibly truncated)", path, movieStreamSanityLimit)
	}
	return body, nil
}

// normalizeTitle folds a title to a comparison key: trimmed of surrounding
// whitespace and lowercased, matching plan §5's "case-insensitively (trim
// whitespace)" sample-matching rule.
func normalizeTitle(title string) string {
	return strings.ToLower(strings.TrimSpace(title))
}

// sampleLookupSet builds a normalized-title set from a --samples list for
// O(1) matching against each movie's title during the library scan.
func sampleLookupSet(samples []string) map[string]bool {
	set := make(map[string]bool, len(samples))
	for _, s := range samples {
		set[normalizeTitle(s)] = true
	}
	return set
}

// parseSamples splits a --samples flag value into a clean list of sample
// movie titles: split on comma, each trimmed of surrounding whitespace,
// with empty entries (e.g. from a trailing comma or an all-blank flag
// value) dropped.
func parseSamples(flagValue string) []string {
	if strings.TrimSpace(flagValue) == "" {
		return nil
	}
	parts := strings.Split(flagValue, ",")
	samples := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			continue
		}
		samples = append(samples, trimmed)
	}
	return samples
}
