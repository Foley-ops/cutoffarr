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
// Reaching this cap is treated the same as any other decode failure per
// plan §2.6 (warn and skip the instance for the cycle) — a response
// truncated by the limit reader naturally fails JSON decoding.
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
type movieFileElement struct {
	Quality           *movieFileQualityElement `json:"quality"`
	CustomFormatScore *int                     `json:"customFormatScore"`
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

// wantedCutoffPage decodes the /api/v3/wanted/cutoff envelope. Per plan §5,
// an entirely absent totalRecords means paging cannot be done safely (we
// cannot tell when to stop) and is treated as malformed per §2.6: warn and
// skip the instance for the cycle.
type wantedCutoffPage struct {
	TotalRecords *int                 `json:"totalRecords"`
	Records      []wantedCutoffRecord `json:"records"`
}

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
// previous one.
func inspectRadarrLibrary(ctx context.Context, logger *slog.Logger, inst Instance, samples []string) {
	client := NewAPIClient(inst.URL, inst.APIKey)

	counts, matches, ok := fetchMovies(ctx, logger, client, inst, samples)
	if !ok {
		return
	}

	logger.Info("movie library",
		"instance", inst.Name, "type", inst.Type,
		"total", counts.total, "monitored", counts.monitored, "hasFile", counts.hasFile)

	logSampleMovies(logger, inst, samples, matches)

	wantedIDs, ok := fetchWantedCutoff(ctx, logger, client, inst)
	if !ok {
		return
	}

	logSampleCutoffStatus(logger, inst, samples, matches, wantedIDs)
}

// fetchMovies streams GET /api/v3/movie, tallying library-wide counts and
// capturing the decoded fields plus raw JSON for any movie whose title
// matches a configured sample. Decoding is streaming (json.Decoder reading
// one array element into a json.RawMessage at a time) rather than
// json.Unmarshal-ing the whole body, so peak memory is bounded by a single
// movie's size rather than the entire library — required because /movie
// can exceed the 4 MB cap used for the connectivity endpoints.
func fetchMovies(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance, samples []string) (movieCounts, map[string]sampleMovieMatch, bool) {
	var counts movieCounts
	wanted := sampleLookupSet(samples)
	matches := make(map[string]sampleMovieMatch, len(wanted))

	resp, err := client.Do(ctx, http.MethodGet, "/api/v3/movie", nil)
	if err != nil {
		logger.Warn("skipping instance: movie request failed",
			"instance", inst.Name, "type", inst.Type, "error", err)
		return counts, nil, false
	}
	defer resp.Body.Close()

	dec := json.NewDecoder(io.LimitReader(resp.Body, movieStreamSanityLimit))

	tok, err := dec.Token()
	if err != nil {
		logger.Warn("skipping instance: movie response is not valid JSON",
			"instance", inst.Name, "type", inst.Type, "error", err)
		return counts, nil, false
	}
	if delim, isDelim := tok.(json.Delim); !isDelim || delim != '[' {
		logger.Warn("skipping instance: movie response is not a JSON array",
			"instance", inst.Name, "type", inst.Type)
		return counts, nil, false
	}

	for dec.More() {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			logger.Warn("skipping instance: movie response element is not valid JSON",
				"instance", inst.Name, "type", inst.Type, "error", err)
			return counts, nil, false
		}

		var m movieListElement
		if err := json.Unmarshal(raw, &m); err != nil {
			logger.Warn("skipping instance: movie response element is not valid JSON",
				"instance", inst.Name, "type", inst.Type, "error", err)
			return counts, nil, false
		}

		counts.total++
		if m.Monitored != nil && *m.Monitored {
			counts.monitored++
		}
		if m.HasFile != nil && *m.HasFile {
			counts.hasFile++
		}

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

	return counts, matches, true
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

// fetchWantedCutoff fully pages GET /api/v3/wanted/cutoff (pageSize=100),
// returning the set of movie ids it contains. Defensive paging per plan §5:
//   - totalRecords absent from the first page's envelope means paging
//     cannot be done safely; warn and skip the instance for the cycle
//     (ok=false), same as any other malformed response.
//   - a page returning 0 records ends paging; if fewer records were fetched
//     than totalRecords claimed, warn (but this is not a skip: whatever was
//     fetched is still returned).
//   - a hard cap of maxWantedCutoffPages bounds the loop; hitting it also
//     warns without being a skip.
func fetchWantedCutoff(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance) (map[int]bool, bool) {
	ids := make(map[int]bool)
	var totalRecords int
	fetched := 0
	completed := false

	for page := 1; page <= maxWantedCutoffPages; page++ {
		query := url.Values{
			"page":     {strconv.Itoa(page)},
			"pageSize": {strconv.Itoa(wantedCutoffPageSize)},
		}

		body, err := fetchLargeBody(ctx, client, "/api/v3/wanted/cutoff", query)
		if err != nil {
			logger.Warn("skipping instance: wanted/cutoff request failed",
				"instance", inst.Name, "type", inst.Type, "page", page, "error", err)
			return nil, false
		}

		var envelope wantedCutoffPage
		if err := json.Unmarshal(body, &envelope); err != nil {
			logger.Warn("skipping instance: wanted/cutoff response is not valid JSON",
				"instance", inst.Name, "type", inst.Type, "page", page, "error", err)
			return nil, false
		}

		if page == 1 {
			if envelope.TotalRecords == nil {
				logger.Warn("skipping instance: wanted/cutoff response missing totalRecords",
					"instance", inst.Name, "type", inst.Type)
				return nil, false
			}
			totalRecords = *envelope.TotalRecords
		}

		if len(envelope.Records) == 0 {
			if fetched != totalRecords {
				logger.Warn("wanted/cutoff paging stopped: page returned 0 records before totalRecords was reached",
					"instance", inst.Name, "type", inst.Type, "page", page, "fetched", fetched, "totalRecords", totalRecords)
			}
			completed = true
			break
		}

		for _, r := range envelope.Records {
			if r.ID == nil {
				// Without an id this record can't be cross-referenced
				// against the /movie library at all; title is the only
				// other identifying information the envelope carries
				// (plan §5: "decode records minimally: id, title"), so it
				// is the natural context to report here.
				logger.Warn("wanted/cutoff record missing id field; excluded from the cutoff set",
					"instance", inst.Name, "type", inst.Type, "page", page, "title", derefOrAbsent(r.Title))
				continue
			}
			ids[*r.ID] = true
		}
		fetched += len(envelope.Records)

		if fetched >= totalRecords {
			completed = true
			break
		}
	}

	if !completed {
		logger.Warn("wanted/cutoff paging hit the page cap without completing",
			"instance", inst.Name, "type", inst.Type, "pageCap", maxWantedCutoffPages, "fetched", fetched, "totalRecords", totalRecords)
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
// parameters and returns the full response body, capped at
// movieStreamSanityLimit. It mirrors fetchBody in connectivity.go (same
// cap-reached-means-malformed treatment) but is kept as separate code here
// rather than sharing that function, for two reasons: it uses a different,
// much larger cap per plan §5's binding large-response handling, and it
// needs to attach query parameters, which APIClient.Do cannot do (see
// doGet below).
func fetchLargeBody(ctx context.Context, client *APIClient, path string, query url.Values) ([]byte, error) {
	resp, err := doGet(ctx, client, path, query)
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

// doGet issues a GET request against path with query attached, using
// client's base URL, API key, and http.Client (same package, so its
// unexported fields are reachable directly rather than duplicating
// NewAPIClient's construction). It exists because APIClient.Do joins its
// path argument with url.JoinPath, which treats "?" as a literal path
// character and percent-encodes it (confirmed empirically) — embedding a
// query string in the path passed to Do would corrupt it. doGet instead
// joins only the path, then attaches the query via url.Values.Encode.
// Request construction and non-2xx handling (with the same
// errorBodySnippetLimit-bounded body snippet) otherwise match APIClient.Do.
func doGet(ctx context.Context, client *APIClient, path string, query url.Values) (*http.Response, error) {
	joined, err := url.JoinPath(client.baseURL, path)
	if err != nil {
		return nil, fmt.Errorf("radarr: building request url from base %q and path %q: %w", client.baseURL, path, err)
	}
	reqURL, err := url.Parse(joined)
	if err != nil {
		return nil, fmt.Errorf("radarr: parsing joined url %q: %w", joined, err)
	}
	reqURL.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("radarr: building request: %w", err)
	}
	req.Header.Set("X-Api-Key", client.apiKey)

	resp, err := client.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("radarr: GET %s: %w", reqURL.String(), err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodySnippetLimit))
		return nil, fmt.Errorf("radarr: GET %s: unexpected status %d: %s", reqURL.String(), resp.StatusCode, snippet)
	}

	return resp, nil
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
