package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// --- the Sonarr write-path unit fake ---------------------------------------
//
// The Radarr twin of this file (writer_test.go) drives unmonitorMovie against
// writerTestServer; this one drives unmonitorSeason against a fake with the
// two extra endpoints a season write needs — GET /api/v3/episode (the
// write-time airing/monitored re-verification) and PUT
// /api/v3/episode/monitor — plus GET/PUT /api/v3/series/{id}. Every request
// of every method to every path is recorded, so a dry-run assertion is about
// requests nobody thought to make rather than only the ones expected.

type sonarrWriterFake struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []recordedRequest

	seriesJSON   string
	episodesJSON string

	// Response overrides. seriesPutStatus/episodeMonitorStatus default to
	// 200; the *Echo fields default to "behave like Sonarr" (echo the updated
	// resource), and are set to "" for a server that accepts the write and
	// says nothing at all — the shape a write path can neither confirm nor
	// call a failure.
	seriesPutStatus      int
	episodeMonitorStatus int
	seriesPutEcho        *string
	episodeMonitorEcho   *string

	// episodeMonitorTruncated answers the episode write with a 200, a
	// Content-Length that promises more than it sends, and a closed
	// connection — the only way to reach the "the server took the write and
	// its response body could not be READ" branch, which no echo fixture can
	// produce.
	episodeMonitorTruncated bool
}

// sonarrWriterSeriesJSON is a series object shaped like a real Sonarr
// /api/v3/series/{id} response: the season array the write path performs its
// single mutation inside, plus a spread of fields the write must round-trip
// untouched — including "someFutureField" (a key this codebase has no
// knowledge of at all) and a sizeOnDisk large enough that a float64 round
// trip would corrupt it.
//
// The title and overview deliberately contain "&", "<" and ">": the three
// characters encoding/json's default HTML escaping rewrites inside every
// string it encodes, including inside a json.RawMessage. Without them the
// byte-for-byte assertion would pass while being false for a large share of a
// real library.
//
// The SEASON objects carry them too, and that placement is load-bearing rather
// than decorative. §2.4's surgery re-encodes the seasons array through
// encodeRawArray, one level below the top-level object encodePayload handles,
// so escapable bytes at the top level say nothing about whether that second
// encoder escapes. Real Sonarr seasons routinely carry escapable strings —
// images[].remoteUrl query strings, statistics.releaseGroups — so without a
// season-level "&" or "<" here, deleting encodeRawArray's SetEscapeHTML(false)
// leaves the whole suite green while the write path silently rewrites bytes
// Sonarr never sent. Both the sibling-season byte comparison and the whole-body
// \u00xx scan bite on these members; neither bites without them.
const sonarrWriterSeriesJSON = `{
	"id": 3,
	"title": "Mr. & Mrs. Smith <Special Edition>",
	"monitored": true,
	"qualityProfileId": 1,
	"tags": [3, 9],
	"sizeOnDisk": 9876543210123,
	"overview": "Cheech & Chong: 4 < 5 > 3",
	` + sonarrWriterSeasonsBlock + `
	"someFutureField": {"nested": ["a", "b"], "flag": true}
}`

// The fixture's two season objects and the array that holds them, named so
// that the tests which swap one out for a broken variant can NAME the bytes
// they replace instead of transcribing them. A transcribed copy stops matching
// the moment the fixture grows a field, and strings.Replace reports a miss by
// returning the input unchanged — which is exactly how adding the
// images/releaseGroups members above turned three refusal tests into
// happy-path tests that still passed. See mustReplace, which turns that miss
// into a failure.
const (
	sonarrWriterSeason1JSON = `{"seasonNumber": 1, "monitored": true, "images": [{"coverType": "poster", "remoteUrl": "https://img/s1?a=1&b=2&c=<d>"}], "statistics": {"episodeFileCount": 2, "totalEpisodeCount": 2, "sizeOnDisk": 9876543210123, "releaseGroups": ["Cheech & Chong <RAW>"]}}`
	sonarrWriterSeason2JSON = `{"seasonNumber": 2, "monitored": true, "images": [{"coverType": "poster", "remoteUrl": "https://img/s2?a=1&b=2&c=<d>"}], "statistics": {"episodeFileCount": 1, "totalEpisodeCount": 1, "releaseGroups": ["Mr. & Mrs. <Group>"]}}`

	sonarrWriterSeasonsBlock = `"seasons": [
		` + sonarrWriterSeason1JSON + `,
		` + sonarrWriterSeason2JSON + `
	],`
)

// mustReplace is strings.Replace(…, 1) with its silent-miss failure mode
// removed: a fixture surgery that matches nothing must fail the test loudly,
// not hand back an untouched fixture that quietly makes the test assert
// something else.
func mustReplace(t *testing.T, s, old, replacement string) string {
	t.Helper()
	if !strings.Contains(s, old) {
		t.Fatalf("fixture surgery matched nothing — the fixture and this test have drifted apart.\nlooking for:\n%s\nin:\n%s", old, s)
	}
	return strings.Replace(s, old, replacement, 1)
}

// sonarrWriterEpisodesJSON is the matching /api/v3/episode?seriesId=3
// response: season 1 has the two aired, monitored episodes its statistics
// claim, season 2 has one.
const sonarrWriterEpisodesJSON = `[
	{"id": 100, "seriesId": 3, "seasonNumber": 1, "episodeNumber": 1, "monitored": true, "hasFile": true, "airDateUtc": "2015-01-01T00:00:00Z", "episodeFileId": 500},
	{"id": 101, "seriesId": 3, "seasonNumber": 1, "episodeNumber": 2, "monitored": true, "hasFile": true, "airDateUtc": "2015-01-02T00:00:00Z", "episodeFileId": 501},
	{"id": 200, "seriesId": 3, "seasonNumber": 2, "episodeNumber": 1, "monitored": true, "hasFile": true, "airDateUtc": "2016-01-01T00:00:00Z", "episodeFileId": 600}
]`

func newSonarrWriterFake(t *testing.T, seriesJSON, episodesJSON string) *sonarrWriterFake {
	t.Helper()
	f := &sonarrWriterFake{
		seriesJSON:           seriesJSON,
		episodesJSON:         episodesJSON,
		seriesPutStatus:      http.StatusOK,
		episodeMonitorStatus: http.StatusOK,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/episode/monitor", f.handle(f.serveEpisodeMonitor))
	mux.HandleFunc("/api/v3/episode", f.handle(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(f.episodesJSON))
	}))
	mux.HandleFunc("/api/v3/series/", f.handle(f.serveSeriesDetail))
	mux.HandleFunc("/", f.handle(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *sonarrWriterFake) handle(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{method: r.Method, path: r.URL.Path, body: body, contentType: r.Header.Get("Content-Type")})
		f.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		next(w, r)
	}
}

func (f *sonarrWriterFake) serveSeriesDetail(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.Write([]byte(f.seriesJSON))
	case http.MethodPut:
		if f.seriesPutStatus >= 400 {
			w.WriteHeader(f.seriesPutStatus)
			w.Write([]byte(`{"message":"write rejected by fake"}`))
			return
		}
		w.WriteHeader(f.seriesPutStatus)
		if f.seriesPutEcho != nil {
			w.Write([]byte(*f.seriesPutEcho))
			return
		}
		// Sonarr answers a successful PUT with the updated object.
		body, _ := io.ReadAll(r.Body)
		w.Write(body)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// serveEpisodeMonitor answers PUT /api/v3/episode/monitor the way Sonarr
// does: with the updated episode resources, each carrying the monitored value
// that was just applied.
func (f *sonarrWriterFake) serveEpisodeMonitor(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if f.episodeMonitorStatus >= 400 {
		w.WriteHeader(f.episodeMonitorStatus)
		w.Write([]byte(`{"message":"episode monitor write rejected by fake"}`))
		return
	}
	if f.episodeMonitorTruncated {
		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 200\r\n\r\n")
		buf.WriteString(`[{"id":100,`)
		buf.Flush()
		return
	}
	body, _ := io.ReadAll(r.Body)
	w.WriteHeader(f.episodeMonitorStatus)
	if f.episodeMonitorEcho != nil {
		w.Write([]byte(*f.episodeMonitorEcho))
		return
	}
	var req struct {
		EpisodeIDs []int `json:"episodeIds"`
		Monitored  bool  `json:"monitored"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		w.Write([]byte(`[]`))
		return
	}
	var elems []string
	for _, id := range req.EpisodeIDs {
		elems = append(elems, fmt.Sprintf(`{"id":%d,"seriesId":3,"monitored":%t}`, id, req.Monitored))
	}
	w.Write([]byte("[" + strings.Join(elems, ",") + "]"))
}

func (f *sonarrWriterFake) all() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

func (f *sonarrWriterFake) writes() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedRequest
	for _, r := range f.requests {
		if r.method != http.MethodGet {
			out = append(out, r)
		}
	}
	return out
}

func (f *sonarrWriterFake) instance() Instance {
	return Instance{Name: "sonarr-main", Type: "sonarr", URL: f.srv.URL, APIKey: "key"}
}

func (f *sonarrWriterFake) client() *APIClient {
	return NewAPIClient(f.srv.URL, "key")
}

// --- §2.1: the dry-run guarantee -------------------------------------------

// TestUnmonitorSeason_DryRun_MakesNoWriteRequest is the Sonarr half of the
// project's most important guarantee (§2.1: "In dry-run mode the app performs
// zero write requests — not one"), pinned at the unit level. Both fresh GETs
// still happen — that is what makes dry-run a rehearsal of the write rather
// than a different code path — but neither of the two write calls is ever
// sent.
func TestUnmonitorSeason_DryRun_MakesNoWriteRequest(t *testing.T) {
	fake := newSonarrWriterFake(t, sonarrWriterSeriesJSON, sonarrWriterEpisodesJSON)
	logger, _ := newDecisionTestLogger(slog.LevelDebug)

	written, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, true, false)
	if err != nil {
		t.Fatalf("unmonitorSeason returned error = %v, want nil", err)
	}
	if written {
		t.Error("written = true in dry-run, want false")
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("dry-run made %d write request(s), want ZERO: %+v", len(writes), writes)
	}
	// The rehearsal must be real: both fresh reads happen before either gate.
	gets := filterRequests(fake.all(), http.MethodGet)
	var sawSeries, sawEpisodes bool
	for _, r := range gets {
		switch r.path {
		case "/api/v3/series/3":
			sawSeries = true
		case "/api/v3/episode":
			sawEpisodes = true
		}
	}
	if !sawSeries || !sawEpisodes {
		t.Errorf("dry-run must still rehearse both fresh reads, got %+v", gets)
	}
}

// --- the happy path, and §2.4's byte-preservation mandate ------------------

// TestUnmonitorSeason_WriteMode_EpisodesFirstThenSeries pins the binding
// write ORDER (controller resolution 1): if /episode/monitor succeeds but the
// series PUT fails, the season is still monitored, so the next cycle
// re-evaluates and converges. The reverse order strands episodes monitored
// forever — the season goes unmonitored, rule 1 skips it next cycle, and
// nothing ever revisits the episodes.
func TestUnmonitorSeason_WriteMode_EpisodesFirstThenSeries(t *testing.T) {
	fake := newSonarrWriterFake(t, sonarrWriterSeriesJSON, sonarrWriterEpisodesJSON)
	logger, _ := newDecisionTestLogger(slog.LevelDebug)

	written, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false, false)
	if err != nil {
		t.Fatalf("unmonitorSeason returned error = %v, want nil", err)
	}
	if !written {
		t.Fatal("written = false, want true: both halves of the write were confirmed")
	}

	writes := fake.writes()
	if len(writes) != 2 {
		t.Fatalf("expected exactly 2 write requests (episode monitor, then series), got %d: %+v", len(writes), writes)
	}
	if writes[0].path != "/api/v3/episode/monitor" || writes[0].method != http.MethodPut {
		t.Errorf("first write = %s %s, want PUT /api/v3/episode/monitor", writes[0].method, writes[0].path)
	}
	if writes[1].path != "/api/v3/series/3" || writes[1].method != http.MethodPut {
		t.Errorf("second write = %s %s, want PUT /api/v3/series/3", writes[1].method, writes[1].path)
	}
	for _, w := range writes {
		if w.contentType != "application/json" {
			t.Errorf("%s %s: Content-Type = %q, want application/json (Sonarr's stack answers an untyped body with 415)", w.method, w.path, w.contentType)
		}
	}

	// The episode monitor body is the mandated shape, naming exactly the
	// target season's monitored episodes and nothing else.
	var req struct {
		EpisodeIDs []int `json:"episodeIds"`
		Monitored  *bool `json:"monitored"`
	}
	if err := json.Unmarshal(writes[0].body, &req); err != nil {
		t.Fatalf("episode monitor body is not the mandated JSON object: %v: %s", err, writes[0].body)
	}
	if req.Monitored == nil || *req.Monitored {
		t.Errorf("episode monitor body monitored = %v, want false", req.Monitored)
	}
	if fmt.Sprint(req.EpisodeIDs) != fmt.Sprint([]int{100, 101}) {
		t.Errorf("episodeIds = %v, want [100 101] (season 1's episodes only — season 2's must never be touched)", req.EpisodeIDs)
	}
}

// TestUnmonitorSeason_WriteMode_PutsFullObjectWithOnlyTheTargetSeasonChanged
// is §2.4's pin: the PUT body must be the fresh GET body with exactly one
// value substituted — seasons[n].monitored for the target season — and every
// other byte, including the series-level "monitored", the sibling season's
// whole object, a 13-digit sizeOnDisk, and a key this codebase knows nothing
// about, preserved exactly.
func TestUnmonitorSeason_WriteMode_PutsFullObjectWithOnlyTheTargetSeasonChanged(t *testing.T) {
	fake := newSonarrWriterFake(t, sonarrWriterSeriesJSON, sonarrWriterEpisodesJSON)
	logger, _ := newDecisionTestLogger(slog.LevelInfo)

	if _, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false, false); err != nil {
		t.Fatalf("unmonitorSeason returned error = %v, want nil", err)
	}

	puts := filterRequests(fake.all(), http.MethodPut)
	var seriesPut recordedRequest
	for _, p := range puts {
		if p.path == "/api/v3/series/3" {
			seriesPut = p
		}
	}
	if seriesPut.body == nil {
		t.Fatalf("no PUT to /api/v3/series/3: %+v", puts)
	}

	var sent, fetched map[string]json.RawMessage
	if err := json.Unmarshal(seriesPut.body, &sent); err != nil {
		t.Fatalf("PUT body is not a JSON object: %v: %s", err, seriesPut.body)
	}
	if err := json.Unmarshal([]byte(sonarrWriterSeriesJSON), &fetched); err != nil {
		t.Fatalf("fixture is not a JSON object: %v", err)
	}
	if len(sent) != len(fetched) {
		t.Errorf("PUT body has %d keys, fetched object had %d: the write must add and remove nothing", len(sent), len(fetched))
	}
	for key, want := range fetched {
		got, present := sent[key]
		if !present {
			t.Errorf("key %q was dropped from the PUT body", key)
			continue
		}
		if key == "seasons" {
			continue // compared element-wise below
		}
		if !jsonBytesEqual(t, got, want) {
			t.Errorf("key %q changed: sent %s, fetched %s", key, got, want)
		}
	}

	// The series-level monitored flag is the one this project must NEVER
	// write, in any code path: unmonitoring happens to a season.
	if !jsonBytesEqual(t, sent["monitored"], json.RawMessage("true")) {
		t.Errorf("series-level monitored = %s, want the fetched value true — no code path may ever change it", sent["monitored"])
	}

	var sentSeasons, fetchedSeasons []json.RawMessage
	if err := json.Unmarshal(sent["seasons"], &sentSeasons); err != nil {
		t.Fatalf("sent seasons is not an array: %v", err)
	}
	if err := json.Unmarshal(fetched["seasons"], &fetchedSeasons); err != nil {
		t.Fatalf("fetched seasons is not an array: %v", err)
	}
	if len(sentSeasons) != len(fetchedSeasons) {
		t.Fatalf("sent %d seasons, fetched %d", len(sentSeasons), len(fetchedSeasons))
	}
	// Season 2 (the sibling) must come back byte-identical.
	if !jsonBytesEqual(t, sentSeasons[1], fetchedSeasons[1]) {
		t.Errorf("the sibling season was modified: sent %s, fetched %s", sentSeasons[1], fetchedSeasons[1])
	}
	// Season 1 (the target) must differ in exactly one value.
	var sentTarget, fetchedTarget map[string]json.RawMessage
	if err := json.Unmarshal(sentSeasons[0], &sentTarget); err != nil {
		t.Fatalf("sent target season is not an object: %v", err)
	}
	if err := json.Unmarshal(fetchedSeasons[0], &fetchedTarget); err != nil {
		t.Fatalf("fetched target season is not an object: %v", err)
	}
	if string(sentTarget["monitored"]) != "false" {
		t.Errorf("target season monitored = %s, want false", sentTarget["monitored"])
	}
	for key, want := range fetchedTarget {
		if key == "monitored" {
			continue
		}
		if !jsonBytesEqual(t, sentTarget[key], want) {
			t.Errorf("target season key %q changed: sent %s, fetched %s", key, sentTarget[key], want)
		}
	}
}

// assertNoHTMLEscapes fails when a body carries any of the three escapes
// encoding/json writes by default. The escape sequences are BUILT rather than
// written out, so the assertion cannot be defeated by this file being edited
// into the very characters it is looking for (the Radarr twin's construction).
func assertNoHTMLEscapes(t *testing.T, what string, body []byte) {
	t.Helper()
	for _, r := range []rune{'&', '<', '>'} {
		escaped := fmt.Sprintf("\\u%04x", r)
		if strings.Contains(string(body), escaped) {
			t.Errorf("%s contains the HTML-escaped sequence %s for %q; the fetched bytes must go back unmodified:\n%s", what, escaped, r, body)
		}
	}
}

// TestUnmonitorSeason_WriteMode_SendsUnescapedBytesForHTMLSensitiveCharacters
// mirrors the Radarr twin: json.Marshal rewrites "&", "<" and ">" into
// six-character unicode escapes inside every string it encodes — including
// inside a json.RawMessage, which it re-scans rather than copying — so a
// series titled "Mr. & Mrs. Smith" would be PUT back with bytes Sonarr never
// sent.
func TestUnmonitorSeason_WriteMode_SendsUnescapedBytesForHTMLSensitiveCharacters(t *testing.T) {
	fake := newSonarrWriterFake(t, sonarrWriterSeriesJSON, sonarrWriterEpisodesJSON)
	logger, _ := newDecisionTestLogger(slog.LevelInfo)

	if _, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false, false); err != nil {
		t.Fatalf("unmonitorSeason returned error = %v", err)
	}
	for _, w := range fake.writes() {
		if w.path != "/api/v3/series/3" {
			continue
		}
		body := string(w.body)
		assertNoHTMLEscapes(t, "PUT body", w.body)
		// Two of these live INSIDE season objects, which the write re-encodes
		// through a second encoder (encodeRawArray) one level below the one
		// that handles the top-level object. Without them this test passes
		// with that encoder's SetEscapeHTML(false) deleted.
		for _, literal := range []string{
			"Mr. & Mrs. Smith <Special Edition>",
			"Cheech & Chong: 4 < 5 > 3",
			"https://img/s1?a=1&b=2&c=<d>",
			"Mr. & Mrs. <Group>",
		} {
			if !strings.Contains(body, literal) {
				t.Errorf("PUT body does not carry %q verbatim:\n%s", literal, body)
			}
		}
		if strings.HasSuffix(body, "\n") {
			t.Errorf("PUT body has a trailing newline the fetched object did not have:\n%q", body)
		}
		if !strings.Contains(body, "9876543210123") {
			t.Errorf("a 13-digit sizeOnDisk must survive the round trip exactly:\n%s", body)
		}
	}
}

// TestUnmonitorSeason_NoMonitoredEpisodesLeft_SkipsTheEpisodeCallOnly is the
// binding "if zero episodes need the call, skip /episode/monitor (log debug)
// and proceed to the series PUT" rule — the shape a partially converged
// season takes on the cycle after a partial failure.
func TestUnmonitorSeason_NoMonitoredEpisodesLeft_SkipsTheEpisodeCallOnly(t *testing.T) {
	episodes := `[
		{"id": 100, "seriesId": 3, "seasonNumber": 1, "episodeNumber": 1, "monitored": false, "hasFile": true, "airDateUtc": "2015-01-01T00:00:00Z", "episodeFileId": 500},
		{"id": 101, "seriesId": 3, "seasonNumber": 1, "episodeNumber": 2, "monitored": false, "hasFile": true, "airDateUtc": "2015-01-02T00:00:00Z", "episodeFileId": 501}
	]`
	fake := newSonarrWriterFake(t, sonarrWriterSeriesJSON, episodes)
	logger, buf := newDecisionTestLogger(slog.LevelDebug)

	written, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false, false)
	if err != nil {
		t.Fatalf("unmonitorSeason returned error = %v, want nil", err)
	}
	if !written {
		t.Error("written = false, want true: the season write is still needed and was confirmed")
	}
	writes := fake.writes()
	if len(writes) != 1 || writes[0].path != "/api/v3/series/3" {
		t.Fatalf("expected exactly one write (the series PUT), got %+v", writes)
	}
	if !strings.Contains(buf.String(), "level=DEBUG") {
		t.Errorf("the skipped episode call must be visible at debug:\n%s", buf.String())
	}
}

// TestUnmonitorSeason_SomeEpisodesAlreadyUnmonitored_WritesOnlyTheRest is the
// binding "include ONLY episodes with monitored == true" rule at the one place
// it matters, and the shape no fixture had: a season with one monitored and one
// already-unmonitored episode.
//
// Every other fixture is all-monitored (so a full list and a correct list are
// the same bytes) or all-unmonitored (so the call is skipped entirely) —
// neither can tell a subset apart from the whole season. This is exactly what a
// retry after a partially completed episode write looks like, and "the retry is
// smaller, not duplicated" is the reason controller resolution 1 chose the
// episodes-first order.
func TestUnmonitorSeason_SomeEpisodesAlreadyUnmonitored_WritesOnlyTheRest(t *testing.T) {
	episodes := `[
		{"id": 100, "seriesId": 3, "seasonNumber": 1, "episodeNumber": 1, "monitored": false, "hasFile": true, "airDateUtc": "2015-01-01T00:00:00Z", "episodeFileId": 500},
		{"id": 101, "seriesId": 3, "seasonNumber": 1, "episodeNumber": 2, "monitored": true, "hasFile": true, "airDateUtc": "2015-01-02T00:00:00Z", "episodeFileId": 501}
	]`
	fake := newSonarrWriterFake(t, sonarrWriterSeriesJSON, episodes)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	written, recovery, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false, false)
	if err != nil || !written {
		t.Fatalf("written/err = %t/%v, want true/nil:\n%s", written, err, buf.String())
	}
	// A season with a monitored episode left in it is an ordinary write, not a
	// recovery, however many of its episodes are already done.
	if recovery {
		t.Error("recovery = true: one already-unmonitored episode does not make a partially-written season")
	}

	var episodeCalls []recordedRequest
	for _, w := range fake.writes() {
		if w.path == episodeMonitorPath {
			episodeCalls = append(episodeCalls, w)
		}
	}
	if len(episodeCalls) != 1 {
		t.Fatalf("expected exactly one episode call, got %+v", episodeCalls)
	}
	var req struct {
		EpisodeIDs []int `json:"episodeIds"`
		Monitored  *bool `json:"monitored"`
	}
	if err := json.Unmarshal(episodeCalls[0].body, &req); err != nil {
		t.Fatalf("the episode call body is not the expected shape: %v (%s)", err, episodeCalls[0].body)
	}
	if len(req.EpisodeIDs) != 1 || req.EpisodeIDs[0] != 101 {
		t.Errorf("episodeIds = %v, want [101] — an episode already unmonitored needs no write, and including it would make every retry a duplicate: %s", req.EpisodeIDs, episodeCalls[0].body)
	}
	if req.Monitored == nil || *req.Monitored {
		t.Errorf("the episode call must ask for monitored:false: %s", episodeCalls[0].body)
	}
}

// --- partial-failure and failure paths -------------------------------------

// TestUnmonitorSeason_EpisodeMonitorRejected_NeverSendsTheSeriesPut pins the
// abort direction: if the episode call fails outright, the season must stay
// monitored. Writing the series anyway would leave episodes monitored inside
// an unmonitored season, which rule 1 excludes from every future cycle — the
// stranded state the binding write order exists to prevent.
func TestUnmonitorSeason_EpisodeMonitorRejected_NeverSendsTheSeriesPut(t *testing.T) {
	fake := newSonarrWriterFake(t, sonarrWriterSeriesJSON, sonarrWriterEpisodesJSON)
	fake.episodeMonitorStatus = http.StatusInternalServerError
	logger, _ := newDecisionTestLogger(slog.LevelInfo)

	written, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false, false)
	if written {
		t.Error("written = true, want false: nothing was written")
	}
	if err == nil {
		t.Fatal("err = nil, want a write error")
	}
	if isWriteRefusal(err) {
		t.Errorf("a rejected write is a failure, not a refusal: %v", err)
	}
	for _, w := range fake.writes() {
		if w.path == "/api/v3/series/3" {
			t.Errorf("the series PUT must never be sent after the episode call failed: %+v", w)
		}
	}
}

// TestUnmonitorSeason_SeriesPutRejectedAfterEpisodes_NamesTheCompletedHalf is
// controller resolution 1's partial-completion contract: the episode call
// landed, the series call did not, and the error must say so — the season is
// still monitored, so the next cycle re-evaluates it and converges (the
// episodes already false are excluded from the next attempt's id list).
func TestUnmonitorSeason_SeriesPutRejectedAfterEpisodes_NamesTheCompletedHalf(t *testing.T) {
	fake := newSonarrWriterFake(t, sonarrWriterSeriesJSON, sonarrWriterEpisodesJSON)
	fake.seriesPutStatus = http.StatusInternalServerError
	logger, _ := newDecisionTestLogger(slog.LevelInfo)

	written, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false, false)
	if written {
		t.Error("written = true, want false: a confirmed write requires BOTH calls")
	}
	if err == nil {
		t.Fatal("err = nil, want a write error")
	}
	if !strings.Contains(err.Error(), "episode") {
		t.Errorf("the error must name the half that DID complete so a human knows the state the season was left in, got: %v", err)
	}
	// Attempted once, never retried (§2.6).
	seriesPuts := 0
	for _, w := range fake.writes() {
		if w.path == "/api/v3/series/3" {
			seriesPuts++
		}
	}
	if seriesPuts != 1 {
		t.Errorf("series PUT attempts = %d, want exactly 1 (never retried within a cycle)", seriesPuts)
	}
}

// TestUnmonitorSeason_SeriesEchoUnverifiable_IsNotAConfirmedWrite: a 2xx
// alone is not proof. An empty body cannot settle whether the season is now
// unmonitored, which is neither a failure nor a success.
func TestUnmonitorSeason_SeriesEchoUnverifiable_IsNotAConfirmedWrite(t *testing.T) {
	fake := newSonarrWriterFake(t, sonarrWriterSeriesJSON, sonarrWriterEpisodesJSON)
	empty := ""
	fake.seriesPutEcho = &empty
	logger, _ := newDecisionTestLogger(slog.LevelInfo)

	written, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false, false)
	if written {
		t.Error("written = true, want false: only a confirmed change may be counted as one")
	}
	if !errors.Is(err, errWriteUnverified) {
		t.Fatalf("err = %v, want an errWriteUnverified-wrapped error", err)
	}
}

// TestUnmonitorSeason_SeriesEchoSaysStillMonitored_IsAWriteError draws the
// line the Radarr twin draws: an echo that CONTRADICTS the write is a
// confirmed failure, not an unverifiable one.
func TestUnmonitorSeason_SeriesEchoSaysStillMonitored_IsAWriteError(t *testing.T) {
	fake := newSonarrWriterFake(t, sonarrWriterSeriesJSON, sonarrWriterEpisodesJSON)
	echo := `{"id": 3, "monitored": true, "seasons": [{"seasonNumber": 1, "monitored": true}]}`
	fake.seriesPutEcho = &echo
	logger, _ := newDecisionTestLogger(slog.LevelInfo)

	written, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false, false)
	if written {
		t.Error("written = true, want false")
	}
	if err == nil {
		t.Fatal("err = nil, want a write error")
	}
	if errors.Is(err, errWriteUnverified) {
		t.Errorf("an echo that says the season is STILL monitored is a confirmed failure, not an unverifiable one: %v", err)
	}
}

// TestUnmonitorSeason_SeriesEchoShapesThatCannotConfirm covers the season
// echo's "cannot tell" shapes one at a time, and exists because they were only
// ever covered transitively by the empty-body case above.
//
// The first case is the load-bearing one (REVIEW FIX, round 3): the echo's
// IDENTITY check had NO test at all — every echo fixture in this suite returned
// the right id, so deleting verifySeriesIdentity from verifySeasonWriteEcho left
// the whole suite green. That check is binding (controller resolution 2:
// "identity-check the echo (series id)") precisely because this echo is the ONLY
// evidence the project accepts that a write landed: a proxy, redirect, or cache
// answering the PUT with a DIFFERENT series' object that happens to carry this
// season number as monitored:false would otherwise be logged msg=unmonitor and
// counted unmonitored=1 for a write that never happened. The next cycle would
// self-heal, but the report the live acceptance is read from would already be
// false.
//
// Every case here must be errWriteUnverified — "cannot confirm", never "confirmed
// failure" — because none of them is the server saying the season is still
// monitored.
func TestUnmonitorSeason_SeriesEchoShapesThatCannotConfirm(t *testing.T) {
	cases := []struct {
		name        string
		echo        string
		wantInError string
	}{{
		name:        "another series' object",
		echo:        `{"id":4,"monitored":true,"seasons":[{"seasonNumber":1,"monitored":false}]}`,
		wantInError: "series 4",
	}, {
		name:        "no id at all",
		echo:        `{"monitored":true,"seasons":[{"seasonNumber":1,"monitored":false}]}`,
		wantInError: "unidentifiable",
	}, {
		name:        "no seasons key",
		echo:        `{"id":3,"monitored":true}`,
		wantInError: `"seasons"`,
	}, {
		name:        "seasons is not an array",
		echo:        `{"id":3,"monitored":true,"seasons":{"seasonNumber":1}}`,
		wantInError: `"seasons"`,
	}, {
		name:        "the target season is not mentioned",
		echo:        `{"id":3,"monitored":true,"seasons":[{"seasonNumber":2,"monitored":false}]}`,
		wantInError: "does not mention this season",
	}, {
		name:        "the target season's monitored is JSON null",
		echo:        `{"id":3,"monitored":true,"seasons":[{"seasonNumber":1,"monitored":null}]}`,
		wantInError: "does not confirm",
	}, {
		// The echo answers the one question this project asks it twice, and
		// differently. Scanning to the first match resolved that in the
		// server's favour — a confirmed write on a body that also says the
		// season is still monitored — while locateTargetSeason refuses the
		// identical ambiguity on the way in. The order is deliberate: the
		// CONFIRMING element comes first, so a first-match implementation
		// passes this test only by being wrong.
		name:        "the target season appears twice, contradicting itself",
		echo:        `{"id":3,"monitored":true,"seasons":[{"seasonNumber":1,"monitored":false},{"seasonNumber":1,"monitored":true}]}`,
		wantInError: "more than once",
	}, {
		name:        "not a JSON object",
		echo:        `[{"id":3}]`,
		wantInError: "not a JSON object",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newSonarrWriterFake(t, sonarrWriterSeriesJSON, sonarrWriterEpisodesJSON)
			echo := tc.echo
			fake.seriesPutEcho = &echo
			logger, _ := newDecisionTestLogger(slog.LevelInfo)

			written, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false, false)
			if written {
				t.Error("written = true, want false: only an echo this series confirmed may be counted as a write")
			}
			if !errors.Is(err, errWriteUnverified) {
				t.Fatalf("err = %v, want an errWriteUnverified-wrapped error", err)
			}
			if !strings.Contains(err.Error(), tc.wantInError) {
				t.Errorf("error must say what could not be confirmed (want %q):\n%v", tc.wantInError, err)
			}
			// The unconfirmable echo arrives AFTER both writes went out, so the
			// episode call is expected; what must not happen is a retry.
			seriesPuts := 0
			for _, w := range fake.writes() {
				if w.path == "/api/v3/series/3" {
					seriesPuts++
				}
			}
			if seriesPuts != 1 {
				t.Errorf("series PUT attempts = %d, want exactly 1 (never retried within a cycle)", seriesPuts)
			}
		})
	}
}

// TestUnmonitorSeason_EpisodeEchoDoesNotConfirm_WithholdsTheSeasonWrite is the
// other half of the same rule, on the other call — and the CRITICAL review-round
// correction to it.
//
// The season write may go out only once the episode write has been CONFIRMED.
// Every other answer — an echo naming a requested episode as still monitored, a
// body that is not the expected shape, a body that could not be read at all —
// leaves it genuinely unknown whether any episode was unmonitored, and sending
// the season PUT anyway is the one road to the stranded state the binding
// episodes-first order exists to prevent: the season goes unmonitored, rule 1
// removes it from every future cycle, and a still-monitored episode inside it
// is never revisited while Sonarr keeps grabbing for it.
//
// Aborting strands nothing. The season stays MONITORED, so the next cycle
// re-reads /episode, drops whatever really landed from the id list, and retries.
// Abort converges in both branches; proceeding strands in one.
func TestUnmonitorSeason_EpisodeEchoDoesNotConfirm_WithholdsTheSeasonWrite(t *testing.T) {
	cases := []struct {
		name        string
		echo        string
		contradicts bool
		wantInError []string
		// wantInLog defaults to wantInError; it exists only because slog
		// escapes the quotes inside a body snippet, so a JSON fixture reads
		// differently in the log than in the error string.
		wantInLog []string
	}{
		{
			// The shape the whole finding is about: Sonarr answered 200 and
			// said, in the same breath, that the episodes are STILL monitored.
			name:        "the echo says the episodes are still monitored",
			echo:        `[{"id":100,"monitored":true},{"id":101,"monitored":true}]`,
			contradicts: true,
			wantInError: []string{"100", "still monitored"},
		},
		{
			name:        "the echo does not mention a requested episode",
			echo:        `[{"id":100,"monitored":false}]`,
			wantInError: []string{"101"},
		},
		{
			name:        "the echo does not say whether an episode is monitored",
			echo:        `[{"id":100,"monitored":null},{"id":101,"monitored":false}]`,
			wantInError: []string{"100"},
		},
		{
			// Last-wins on a duplicate id let the server's serialization order
			// decide whether the write landed. The CONFIRMING element comes
			// last, so an implementation that keeps the last one passes only by
			// being wrong.
			name:        "the echo mentions one episode twice, contradicting itself",
			echo:        `[{"id":100,"monitored":true},{"id":100,"monitored":false},{"id":101,"monitored":false}]`,
			wantInError: []string{"100", "more than once"},
		},
		{
			name:        "the body is not a JSON array of objects",
			echo:        `{"message":"ok"}`,
			wantInError: []string{`{"message":"ok"}`},
			wantInLog:   []string{`message`},
		},
		{
			name:        "the body is empty",
			echo:        ``,
			wantInError: []string{"episode monitor"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newSonarrWriterFake(t, sonarrWriterSeriesJSON, sonarrWriterEpisodesJSON)
			echo := tc.echo
			fake.episodeMonitorEcho = &echo
			logger, buf := newDecisionTestLogger(slog.LevelInfo)

			written, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false, false)
			if written {
				t.Error("written = true, want false")
			}
			if err == nil {
				t.Fatal("err = nil, want a write error")
			}
			for _, w := range fake.writes() {
				if w.path == "/api/v3/series/3" {
					t.Errorf("the series PUT must NOT be sent when the episode write is unconfirmed: %+v", w)
				}
			}
			// Never errWriteUnverified: that class means "probably applied,
			// treat it as done and let the next cycle reconcile it", and the
			// season here is definitively NOT unmonitored. Reporting it that
			// way would tell a human the opposite of what happened.
			if errors.Is(err, errWriteUnverified) {
				t.Errorf("an unconfirmed EPISODE write withholds the season write, so it is a write failure, not an accepted-but-unconfirmed write: %v", err)
			}
			if isWriteRefusal(err) {
				t.Errorf("a write was sent, so this is a failure and not a refusal: %v", err)
			}
			if errors.Is(err, errEpisodeMonitorContradicted) != tc.contradicts {
				t.Errorf("errors.Is(err, errEpisodeMonitorContradicted) = %t, want %t: an echo that CONTRADICTS the write must be distinguishable from one that merely cannot confirm it: %v",
					errors.Is(err, errEpisodeMonitorContradicted), tc.contradicts, err)
			}
			if errors.Is(err, errEpisodeMonitorUnconfirmed) == tc.contradicts {
				t.Errorf("errors.Is(err, errEpisodeMonitorUnconfirmed) = %t on a contradicts=%t case: the two classes must be exclusive: %v",
					errors.Is(err, errEpisodeMonitorUnconfirmed), tc.contradicts, err)
			}
			// The diagnostic finding: this is the one failure the first live
			// run is expected to hit, so the error a human reads must say what
			// came back and which episode was unconfirmed.
			for _, want := range tc.wantInError {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error must carry %q so the unrecognized response can be diagnosed, got: %v", want, err)
				}
			}
			if !strings.Contains(err.Error(), "series 3 season 1") {
				t.Errorf("the error must name the season it belongs to, got: %v", err)
			}
			// The same diagnostic has to reach the LOG, not only the returned
			// error: the WARN that reports this used to carry an episode COUNT
			// and nothing else, so the one failure the first live run is
			// expected to hit was the one failure the log could not describe.
			line := ""
			for _, l := range strings.Split(buf.String(), "\n") {
				if strings.Contains(l, "episode monitor write could not be confirmed") {
					line = l
				}
			}
			if line == "" {
				t.Fatalf("expected a WARN reporting the unconfirmed episode write:\n%s", buf.String())
			}
			wantInLog := tc.wantInLog
			if wantInLog == nil {
				wantInLog = tc.wantInError
			}
			for _, want := range append([]string{"error="}, wantInLog...) {
				if !strings.Contains(line, want) {
					t.Errorf("the WARN must carry %q, got: %s", want, line)
				}
			}
		})
	}
}

// TestUnmonitorSeason_EpisodeMonitorBodyUnreadable_WithholdsTheSeasonWrite
// covers the branch no echo fixture can reach: the write was accepted and the
// RESPONSE BODY could not be read at all (a truncated connection). It used to
// return `false, nil` — no log, no wrapped error, no diagnostic of any kind —
// and the season write went out on it.
func TestUnmonitorSeason_EpisodeMonitorBodyUnreadable_WithholdsTheSeasonWrite(t *testing.T) {
	fake := newSonarrWriterFake(t, sonarrWriterSeriesJSON, sonarrWriterEpisodesJSON)
	fake.episodeMonitorTruncated = true
	logger, _ := newDecisionTestLogger(slog.LevelInfo)

	written, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false, false)
	if written {
		t.Error("written = true, want false")
	}
	if err == nil {
		t.Fatal("err = nil, want a write error naming the unreadable body")
	}
	for _, w := range fake.writes() {
		if w.path == "/api/v3/series/3" {
			t.Errorf("the series PUT must NOT be sent when the episode response could not even be read: %+v", w)
		}
	}
	if !strings.Contains(err.Error(), "could not be read") {
		t.Errorf("the error must say the body could not be read, got: %v", err)
	}
	if !errors.Is(err, errEpisodeMonitorUnconfirmed) {
		t.Errorf("err = %v, want an errEpisodeMonitorUnconfirmed-wrapped error", err)
	}
}

// --- pre-write re-verification refusals ------------------------------------

func TestUnmonitorSeason_FreshPayloadCarriesExclusionTag_Refuses(t *testing.T) {
	seriesJSON := mustReplace(t, sonarrWriterSeriesJSON, `"tags": [3, 9],`, `"tags": [3, 42, 9],`)
	fake := newSonarrWriterFake(t, seriesJSON, sonarrWriterEpisodesJSON)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	written, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 42, true, false, false)
	if written {
		t.Error("written = true, want false")
	}
	if !errors.Is(err, errExcludedAtWrite) {
		t.Fatalf("err = %v, want errExcludedAtWrite", err)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("§2.5: the exclusion tag always wins, so nothing may be written: %+v", writes)
	}
	if !strings.Contains(buf.String(), ReasonExcludedByTag) {
		t.Errorf("expected the refusal to name the reason:\n%s", buf.String())
	}
}

func TestUnmonitorSeason_FreshPayloadTagsUnverifiable_Refuses(t *testing.T) {
	cases := []struct {
		name       string
		seriesJSON string
	}{
		{"tags absent", mustReplace(t, sonarrWriterSeriesJSON, `"tags": [3, 9],`, ``)},
		{"tags JSON null", mustReplace(t, sonarrWriterSeriesJSON, `"tags": [3, 9],`, `"tags": null,`)},
		{"tags not an array", mustReplace(t, sonarrWriterSeriesJSON, `"tags": [3, 9],`, `"tags": "3,9",`)},
		{"tags contain a null element", mustReplace(t, sonarrWriterSeriesJSON, `"tags": [3, 9],`, `"tags": [3, null, 9],`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newSonarrWriterFake(t, tc.seriesJSON, sonarrWriterEpisodesJSON)
			logger, buf := newDecisionTestLogger(slog.LevelInfo)

			written, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 42, true, false, false)
			if written {
				t.Error("written = true, want false")
			}
			if !errors.Is(err, errTagsUnverifiable) {
				t.Fatalf("err = %v, want errTagsUnverifiable", err)
			}
			if writes := fake.writes(); len(writes) != 0 {
				t.Errorf("nothing may be written when the exclusion tag cannot be checked: %+v", writes)
			}
			if !strings.Contains(buf.String(), "level=WARN") {
				t.Errorf("expected a warning:\n%s", buf.String())
			}
		})
	}
}

// TestUnmonitorSeason_SharedRefusalsNeverCallASeasonAMovie pins the wording of
// the refusal sentinels both write paths now share. They were written for the
// Radarr path and still said so, so a Sonarr refusal read "series 3 season 1:
// movie is already unmonitored as of the pre-write fetch" — a message naming
// the wrong kind of thing, on the one line a human has to read to understand
// why a promised write did not happen. It was invisible only because
// runSonarrWritePass counts refusals without logging err.
//
// The subject prefix names the item; the sentinel must not contradict it.
func TestUnmonitorSeason_SharedRefusalsNeverCallASeasonAMovie(t *testing.T) {
	cases := []struct {
		name       string
		seriesJSON string
		tagActive  bool
		sentinel   error
	}{
		{"the exclusion tag reappeared", mustReplace(t, sonarrWriterSeriesJSON, `"tags": [3, 9],`, `"tags": [3, 42, 9],`), true, errExcludedAtWrite},
		{"tags are JSON null", mustReplace(t, sonarrWriterSeriesJSON, `"tags": [3, 9],`, `"tags": null,`), true, errTagsUnverifiable},
		{"the series is already unmonitored", mustReplace(t, sonarrWriterSeriesJSON, `"monitored": true,
	"qualityProfileId"`, `"monitored": false,
	"qualityProfileId"`), false, errAlreadyUnmonitoredAtWrite},
		{"the season's monitored is JSON null", mustReplace(t, sonarrWriterSeriesJSON, sonarrWriterSeason1JSON,
			`{"seasonNumber": 1, "monitored": null, "statistics": {"episodeFileCount": 2, "totalEpisodeCount": 2}}`), false, errMonitoredUnverifiable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newSonarrWriterFake(t, tc.seriesJSON, sonarrWriterEpisodesJSON)
			logger, _ := newDecisionTestLogger(slog.LevelInfo)

			_, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 42, tc.tagActive, false, false)
			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("err = %v, want %v", err, tc.sentinel)
			}
			if strings.Contains(err.Error(), "movie") {
				t.Errorf("a Sonarr refusal must never call a season a movie: %v", err)
			}
			if !strings.Contains(err.Error(), "series 3 season 1") {
				t.Errorf("the refusal must name the season it is about: %v", err)
			}
		})
	}
}

// TestUnmonitorSeason_SeriesLevelMonitoredUnreadable_Refuses is the
// series-level twin of the season-level test below, and it had no coverage at
// all: only monitored:false was exercised, so readMonitoredFlag's noun="series"
// path — the one that guards the field this whole file exists never to write —
// was never taken by an untrusted shape. A "monitored": null decodes into a
// plain bool as false with no error, which would turn "the series is no longer
// monitored" into a claim nobody ever observed.
func TestUnmonitorSeason_SeriesLevelMonitoredUnreadable_Refuses(t *testing.T) {
	cases := []struct {
		name  string
		field string
	}{
		{"series monitored absent", ``},
		{"series monitored JSON null", `"monitored": null,`},
		{"series monitored not a boolean", `"monitored": "yes",`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seriesJSON := mustReplace(t, sonarrWriterSeriesJSON, `"monitored": true,`, tc.field)
			fake := newSonarrWriterFake(t, seriesJSON, sonarrWriterEpisodesJSON)
			logger, buf := newDecisionTestLogger(slog.LevelInfo)

			written, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false, false)
			if written {
				t.Error("written = true, want false")
			}
			if !errors.Is(err, errMonitoredUnverifiable) {
				t.Fatalf("err = %v, want errMonitoredUnverifiable", err)
			}
			if !strings.Contains(err.Error(), "series") {
				t.Errorf("the refusal must say WHICH monitored field it could not read — the series' or the season's: %v", err)
			}
			if writes := fake.writes(); len(writes) != 0 {
				t.Errorf("nothing may be written when the series-level monitored flag cannot be read: %+v", writes)
			}
			if !strings.Contains(buf.String(), "level=WARN") {
				t.Errorf("expected a warning:\n%s", buf.String())
			}
		})
	}
}

// TestUnmonitorSeason_SeriesNoLongerMonitored_RefusesAsARace: the series was
// unmonitored between the scan and the write pass. Unmonitoring one of its
// seasons is no longer meaningful, and — more importantly — the season's own
// state can no longer be read as evidence of anything the decision was based
// on.
func TestUnmonitorSeason_SeriesNoLongerMonitored_RefusesAsARace(t *testing.T) {
	seriesJSON := mustReplace(t, sonarrWriterSeriesJSON, `"monitored": true,
	"qualityProfileId"`, `"monitored": false,
	"qualityProfileId"`)
	fake := newSonarrWriterFake(t, seriesJSON, sonarrWriterEpisodesJSON)
	logger, _ := newDecisionTestLogger(slog.LevelInfo)

	written, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false, false)
	if written {
		t.Error("written = true, want false")
	}
	if !errors.Is(err, errAlreadyUnmonitoredAtWrite) {
		t.Fatalf("err = %v, want errAlreadyUnmonitoredAtWrite", err)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("nothing may be written: %+v", writes)
	}
}

func TestUnmonitorSeason_SeasonAlreadyUnmonitored_RefusesAsARace(t *testing.T) {
	seriesJSON := mustReplace(t, sonarrWriterSeriesJSON, `{"seasonNumber": 1, "monitored": true,`, `{"seasonNumber": 1, "monitored": false,`)
	fake := newSonarrWriterFake(t, seriesJSON, sonarrWriterEpisodesJSON)
	logger, _ := newDecisionTestLogger(slog.LevelInfo)

	written, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false, false)
	if written {
		t.Error("written = true, want false")
	}
	if !errors.Is(err, errAlreadyUnmonitoredAtWrite) {
		t.Fatalf("err = %v, want errAlreadyUnmonitoredAtWrite", err)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("§2.4's spirit is to change exactly one thing, and only when it needs changing: %+v", writes)
	}
}

// TestUnmonitorSeason_SeasonMonitoredUnreadable_Refuses covers the null-decode
// discipline on the one field the write pivots on: absent, JSON null, and a
// non-boolean value are all untrusted input, and a plain `var b bool` decode
// turns null into false with no error at all — which would make the
// already-unmonitored branch state an observation that was never made.
func TestUnmonitorSeason_SeasonMonitoredUnreadable_Refuses(t *testing.T) {
	cases := []struct {
		name   string
		season string
	}{
		{"monitored absent", `{"seasonNumber": 1, "statistics": {"episodeFileCount": 2, "totalEpisodeCount": 2}}`},
		{"monitored JSON null", `{"seasonNumber": 1, "monitored": null, "statistics": {"episodeFileCount": 2, "totalEpisodeCount": 2}}`},
		{"monitored not a boolean", `{"seasonNumber": 1, "monitored": "yes", "statistics": {"episodeFileCount": 2, "totalEpisodeCount": 2}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seriesJSON := mustReplace(t, sonarrWriterSeriesJSON, sonarrWriterSeason1JSON, tc.season)
			fake := newSonarrWriterFake(t, seriesJSON, sonarrWriterEpisodesJSON)
			logger, buf := newDecisionTestLogger(slog.LevelInfo)

			written, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false, false)
			if written {
				t.Error("written = true, want false")
			}
			if !errors.Is(err, errMonitoredUnverifiable) {
				t.Fatalf("err = %v, want errMonitoredUnverifiable", err)
			}
			if writes := fake.writes(); len(writes) != 0 {
				t.Errorf("nothing may be written: %+v", writes)
			}
			if !strings.Contains(buf.String(), "level=WARN") {
				t.Errorf("expected a warning:\n%s", buf.String())
			}
		})
	}
}

// TestUnmonitorSeason_SeasonVanishedOrDuplicated_Refuses: the target season
// must be found exactly once in the fresh payload. Zero matches means the
// season this decision was about no longer exists; two means nothing can say
// which one the decision meant.
func TestUnmonitorSeason_SeasonVanishedOrDuplicated_Refuses(t *testing.T) {
	cases := []struct {
		name       string
		seriesJSON string
	}{
		{"season vanished", mustReplace(t, sonarrWriterSeriesJSON, sonarrWriterSeason1JSON+`,`, ``)},
		{"season duplicated", mustReplace(t, sonarrWriterSeriesJSON, sonarrWriterSeason1JSON+`,`,
			`{"seasonNumber": 1, "monitored": true, "statistics": {"episodeFileCount": 2, "totalEpisodeCount": 2}},{"seasonNumber": 1, "monitored": true, "statistics": {"episodeFileCount": 2, "totalEpisodeCount": 2}},`)},
		{"seasons key absent", mustReplace(t, sonarrWriterSeriesJSON, sonarrWriterSeasonsBlock, ``)},
		{"seasons JSON null", mustReplace(t, sonarrWriterSeriesJSON, sonarrWriterSeasonsBlock, `"seasons": null,`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newSonarrWriterFake(t, tc.seriesJSON, sonarrWriterEpisodesJSON)
			logger, buf := newDecisionTestLogger(slog.LevelInfo)

			written, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false, false)
			if written {
				t.Error("written = true, want false")
			}
			if !isWriteRefusal(err) {
				t.Fatalf("err = %v, want a counted write refusal", err)
			}
			if writes := fake.writes(); len(writes) != 0 {
				t.Errorf("nothing may be written: %+v", writes)
			}
			if !strings.Contains(buf.String(), "level=WARN") {
				t.Errorf("expected a warning:\n%s", buf.String())
			}
		})
	}
}

// TestUnmonitorSeason_SeasonStartedAiringBeforeTheWrite_Refuses is the
// binding pre-write airing re-verification: the single most important Sonarr
// guard is re-run against FRESH /episode data, because a season that started
// airing between the decision and the write must be refused, not written.
func TestUnmonitorSeason_SeasonStartedAiringBeforeTheWrite_Refuses(t *testing.T) {
	episodes := `[
		{"id": 100, "seriesId": 3, "seasonNumber": 1, "episodeNumber": 1, "monitored": true, "hasFile": true, "airDateUtc": "2015-01-01T00:00:00Z", "episodeFileId": 500},
		{"id": 101, "seriesId": 3, "seasonNumber": 1, "episodeNumber": 2, "monitored": true, "hasFile": false, "airDateUtc": "2099-01-02T00:00:00Z", "episodeFileId": 0}
	]`
	fake := newSonarrWriterFake(t, sonarrWriterSeriesJSON, episodes)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	written, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false, false)
	if written {
		t.Error("written = true, want false")
	}
	// The sentinel, not merely "some refusal": errSeasonAiringAtWrite exists
	// solely to make THIS refusal — the decision was right when it was made and
	// the world changed underneath it — distinguishable at a glance from a
	// data-shape problem. Nothing asserted it, so it was deletable with a green
	// suite.
	if !errors.Is(err, errSeasonAiringAtWrite) {
		t.Fatalf("err = %v, want an errSeasonAiringAtWrite-wrapped error", err)
	}
	if !isWriteRefusal(err) {
		t.Fatalf("err = %v, want a counted write refusal", err)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("a season that started airing since the decision must never be written: %+v", writes)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning:\n%s", buf.String())
	}
}

// TestUnmonitorSeason_UndatedEpisodeAtWriteTime_Refuses is the same guard's
// untrusted-input half: an absent or unparseable airDateUtc means the airing
// status cannot be determined, which fails safe exactly as it does at
// decision time.
func TestUnmonitorSeason_UndatedEpisodeAtWriteTime_Refuses(t *testing.T) {
	episodes := `[
		{"id": 100, "seriesId": 3, "seasonNumber": 1, "episodeNumber": 1, "monitored": true, "hasFile": true, "airDateUtc": "2015-01-01T00:00:00Z", "episodeFileId": 500},
		{"id": 101, "seriesId": 3, "seasonNumber": 1, "episodeNumber": 2, "monitored": true, "hasFile": true, "airDateUtc": null, "episodeFileId": 501}
	]`
	fake := newSonarrWriterFake(t, sonarrWriterSeriesJSON, episodes)
	logger, _ := newDecisionTestLogger(slog.LevelInfo)

	written, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false, false)
	if written {
		t.Error("written = true, want false")
	}
	// The same sentinel as the not-yet-aired case above, and deliberately so:
	// "cannot tell whether it aired" fails safe into the same refusal, and a
	// human reading either one is being told the same thing about the same
	// guard.
	if !errors.Is(err, errSeasonAiringAtWrite) {
		t.Fatalf("err = %v, want an errSeasonAiringAtWrite-wrapped error", err)
	}
	if !isWriteRefusal(err) {
		t.Fatalf("err = %v, want a counted write refusal", err)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("nothing may be written: %+v", writes)
	}
}

// TestUnmonitorSeason_EpisodeWithoutSeasonNumberAtWriteTime_Warns pins the
// pre-write twin of the decision engine's own drop warning. An episode whose
// seasonNumber is absent belongs to no season and is excluded from the set the
// airing guard runs over — which used to happen silently here while the
// structurally identical decision-time drop warned.
//
// The fixture is the case that makes silence indefensible: the season's own
// statistics still match after the drop, so the completeness guard never fires
// and the write goes through with an episode of this series unaccounted for
// anywhere in the log. (When the counts DO disagree the guard fires, but it
// names the symptom — "2 episodes but statistics claims 3" — and an operator
// watching that refusal repeat every cycle has nothing to go on.)
func TestUnmonitorSeason_EpisodeWithoutSeasonNumberAtWriteTime_Warns(t *testing.T) {
	episodes := `[
		{"id": 100, "seriesId": 3, "seasonNumber": 1, "episodeNumber": 1, "monitored": true, "hasFile": true, "airDateUtc": "2015-01-01T00:00:00Z", "episodeFileId": 500},
		{"id": 101, "seriesId": 3, "seasonNumber": 1, "episodeNumber": 2, "monitored": true, "hasFile": true, "airDateUtc": "2015-01-02T00:00:00Z", "episodeFileId": 501},
		{"id": 999, "seriesId": 3, "episodeNumber": 3, "monitored": true, "hasFile": true, "airDateUtc": "2015-01-03T00:00:00Z", "episodeFileId": 502}
	]`
	fake := newSonarrWriterFake(t, sonarrWriterSeriesJSON, episodes)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	written, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false, false)
	if err != nil || !written {
		t.Fatalf("written/err = %t/%v, want true/nil — the season's own counts still match, which is exactly why the drop must not be silent:\n%s", written, err, buf.String())
	}
	line := logLineContaining(t, buf.String(), "missing seasonNumber")
	if !strings.Contains(line, "level=WARN") {
		t.Errorf("the drop must WARN, as the identical decision-time drop does:\n%s", line)
	}
	if !strings.Contains(line, "episodeId=999") {
		t.Errorf("the warning must name the episode it dropped:\n%s", line)
	}
}

// TestUnmonitorSeason_FreshEpisodeSetIncomplete_Refuses: the airing guard is
// only as sound as the episode set it runs over. A /episode response short of
// the season's own statistics.totalEpisodeCount is the exact shape that lets
// a still-to-air episode go unseen — the hole the decision engine's own
// completeness guard closes, re-run here because this is where the
// consequence actually lands.
func TestUnmonitorSeason_FreshEpisodeSetIncomplete_Refuses(t *testing.T) {
	episodes := `[
		{"id": 100, "seriesId": 3, "seasonNumber": 1, "episodeNumber": 1, "monitored": true, "hasFile": true, "airDateUtc": "2015-01-01T00:00:00Z", "episodeFileId": 500}
	]`
	fake := newSonarrWriterFake(t, sonarrWriterSeriesJSON, episodes)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	written, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false, false)
	if written {
		t.Error("written = true, want false")
	}
	if !isWriteRefusal(err) {
		t.Fatalf("err = %v, want a counted write refusal", err)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("nothing may be written on an incomplete episode set: %+v", writes)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning:\n%s", buf.String())
	}
}

// TestUnmonitorSeason_FreshGetIdMismatch_RefusesToWrite guards against a
// proxy, redirect, or cache handing back a different series: writing
// monitored:false onto the wrong show is precisely the mistake this project
// must never make.
func TestUnmonitorSeason_FreshGetIdMismatch_RefusesToWrite(t *testing.T) {
	seriesJSON := mustReplace(t, sonarrWriterSeriesJSON, `"id": 3,`, `"id": 4,`)
	fake := newSonarrWriterFake(t, seriesJSON, sonarrWriterEpisodesJSON)
	logger, _ := newDecisionTestLogger(slog.LevelInfo)

	written, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false, false)
	if written {
		t.Error("written = true, want false")
	}
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("nothing may be written to an unidentifiable object: %+v", writes)
	}
}

// TestUnmonitorSeason_NeverTouchesAnyOtherEndpoint bounds the write path's
// whole API surface: two reads and two writes, and nothing else — no
// /command, no /queue, no refresh.
func TestUnmonitorSeason_NeverTouchesAnyOtherEndpoint(t *testing.T) {
	fake := newSonarrWriterFake(t, sonarrWriterSeriesJSON, sonarrWriterEpisodesJSON)
	logger, _ := newDecisionTestLogger(slog.LevelInfo)

	if _, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false, false); err != nil {
		t.Fatalf("unmonitorSeason returned error = %v", err)
	}
	allowed := map[string]bool{
		"/api/v3/series/3":        true,
		"/api/v3/episode":         true,
		"/api/v3/episode/monitor": true,
	}
	for _, r := range fake.all() {
		if !allowed[r.path] {
			t.Errorf("unexpected request %s %s: the season write path may only touch the series, episode, and episode/monitor endpoints", r.method, r.path)
		}
	}
}

// TestUnmonitorSeason_TwoSeasonsOfOneSeries_EachWriteChangesOnlyItsOwn drives
// the per-season write unit twice against the same series: each PUT must
// carry the season it names flipped and every other season exactly as the
// fresh GET returned it.
func TestUnmonitorSeason_TwoSeasonsOfOneSeries_EachWriteChangesOnlyItsOwn(t *testing.T) {
	fake := newSonarrWriterFake(t, sonarrWriterSeriesJSON, sonarrWriterEpisodesJSON)
	logger, _ := newDecisionTestLogger(slog.LevelInfo)

	for _, season := range []int{1, 2} {
		if _, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, season, 0, false, false, false); err != nil {
			t.Fatalf("season %d: unmonitorSeason returned error = %v", season, err)
		}
	}

	var seriesPuts []recordedRequest
	for _, w := range fake.writes() {
		if w.path == "/api/v3/series/3" {
			seriesPuts = append(seriesPuts, w)
		}
	}
	if len(seriesPuts) != 2 {
		t.Fatalf("expected 2 series PUTs, got %d", len(seriesPuts))
	}
	for i, want := range []struct{ s1, s2 string }{{"false", "true"}, {"true", "false"}} {
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(seriesPuts[i].body, &payload); err != nil {
			t.Fatalf("PUT %d body is not a JSON object: %v", i, err)
		}
		var seasons []struct {
			SeasonNumber int  `json:"seasonNumber"`
			Monitored    bool `json:"monitored"`
		}
		if err := json.Unmarshal(payload["seasons"], &seasons); err != nil {
			t.Fatalf("PUT %d seasons is not an array: %v", i, err)
		}
		got := map[int]string{}
		for _, s := range seasons {
			got[s.SeasonNumber] = strconv.FormatBool(s.Monitored)
		}
		if got[1] != want.s1 || got[2] != want.s2 {
			t.Errorf("PUT %d: season1=%s season2=%s, want season1=%s season2=%s", i, got[1], got[2], want.s1, want.s2)
		}
	}
}

// --- the recovery path and its own gate ------------------------------------
//
// Binding controller ruling (Phase 7 final): recovery is a SEPARATE, named
// write path with its OWN gate, not an allowance inside the ordinary one. A
// pending season qualifies as a recovery iff the FRESH pre-write data shows a
// still-monitored, complete-on-disk, fully-aired, tag-clean season every one of
// whose episodes is already unmonitored — so only the season flag is left to
// write, and the write cannot strand anything. The gate matrix these tests
// walk row by row:
//
//	                   passed+evidence   inconclusive   FAILED / unrecognized
//	ordinary write     written           withheld       withheld
//	recovery write     recovered         recovered      withheld
//
// plus the dry-run rehearsal, which writes nothing in every cell.

// logLineContaining returns the first log line containing want. Unlike
// reportLineWithMsg it matches a substring rather than a whole msg= field, so
// it can find a line by a message whose spaces slog quotes.
func logLineContaining(t *testing.T, out, want string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, want) {
			return line
		}
	}
	t.Fatalf("expected a log line containing %q:\n%s", want, out)
	return ""
}

// sonarrRecoveryFixture builds the shape a partially completed write leaves
// behind, plus an ordinary pending season beside it: series 3 season 1 is
// still MONITORED while both of its episodes are already unmonitored (the
// episode call landed, the season PUT did not), and season 2 is an ordinary
// would-unmonitor season whose episode is still monitored.
func sonarrRecoveryFixture(t *testing.T) *sonarrWriterFake {
	t.Helper()
	const episodes = `[
		{"id": 100, "seriesId": 3, "seasonNumber": 1, "episodeNumber": 1, "monitored": false, "hasFile": true, "airDateUtc": "2015-01-01T00:00:00Z", "episodeFileId": 500},
		{"id": 101, "seriesId": 3, "seasonNumber": 1, "episodeNumber": 2, "monitored": false, "hasFile": true, "airDateUtc": "2015-01-02T00:00:00Z", "episodeFileId": 501},
		{"id": 200, "seriesId": 3, "seasonNumber": 2, "episodeNumber": 1, "monitored": true, "hasFile": true, "airDateUtc": "2016-01-01T00:00:00Z", "episodeFileId": 600}
	]`
	return newSonarrWriterFake(t, sonarrWriterSeriesJSON, episodes)
}

func sonarrRecoveryDecisions() []seasonDecision {
	return []seasonDecision{
		sonarrCandidateDecision(3, 1, true, []seasonCrossCheckEpisode{
			{episodeID: 100, monitored: boolPtr(false), qualityCutoffNotMet: boolPtr(false)},
			{episodeID: 101, monitored: boolPtr(false), qualityCutoffNotMet: boolPtr(false)},
		}),
		sonarrCandidateDecision(3, 2, true, []seasonCrossCheckEpisode{
			{episodeID: 200, monitored: boolPtr(true), qualityCutoffNotMet: boolPtr(false)},
		}),
	}
}

// TestRunSonarrWritePass_RecoveryUnderPassedGate_IsCountedSeparately is the
// matrix's top-left cell, and it is the one the ruling changed most: a
// recovery is a property of the SEASON (decided from fresh data), not of the
// gate it went through. With the ordinary gate wide open, the season whose
// episodes are all already unmonitored is still a recovery — counted as
// recoveredWrites, never as unmonitored, and warned about by name — while the
// ordinary season beside it is an ordinary write.
//
// Folding the two together is exactly what the counter exists to prevent: a
// summary reading unmonitored=2 would claim two seasons were taken out of
// circulation by this cycle's decisions, when one of them was finished off from
// a previous cycle's half-done write.
func TestRunSonarrWritePass_RecoveryUnderPassedGate_IsCountedSeparately(t *testing.T) {
	fake := sonarrRecoveryFixture(t)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	// An explicit pass with would-unmonitor evidence: the ordinary gate is OPEN.
	cc := crossCheckResult{status: crossCheckStatusPassed, verified: 2, writeVerified: 2}
	unmonitored, recovered, writeErrors, echoUnverified, refused, withheld := runSonarrWritePass(
		context.Background(), logger, fake.client(), fake.instance(), sonarrRecoveryDecisions(), cc, 0, false, false)

	out := buf.String()
	if unmonitored != 1 || recovered != 1 {
		t.Fatalf("unmonitored/recovered = %d/%d, want 1/1 — a recovery completion is never counted as an ordinary unmonitor:\n%s", unmonitored, recovered, out)
	}
	if writeErrors+echoUnverified+refused+withheld != 0 {
		t.Errorf("writeErrors/echoUnverified/refused/withheld = %d/%d/%d/%d, want all 0:\n%s", writeErrors, echoUnverified, refused, withheld, out)
	}
	if unmonitored+recovered+writeErrors+echoUnverified+refused+withheld != 2 {
		t.Errorf("the accounting identity must hold over both pending seasons:\n%s", out)
	}

	// The recovery has its own WARN, naming its season.
	recoveryLine := logLineContaining(t, out, "completing a previously partial season unmonitor")
	if !strings.Contains(recoveryLine, "level=WARN") {
		t.Errorf("a recovery is warned about, not merely logged:\n%s", recoveryLine)
	}
	if !strings.Contains(recoveryLine, "season=1") || !strings.Contains(recoveryLine, "seriesId=3") {
		t.Errorf("the recovery line must name WHICH season it completed:\n%s", recoveryLine)
	}
	// Nothing was bypassed: the ordinary gate was open, and the absence of
	// gateBlocked= is how a reader tells that apart from a recovery that ran
	// while the gate was shut.
	if strings.Contains(recoveryLine, "gateBlocked=") {
		t.Errorf("the ordinary gate was open, so no authorization was bypassed:\n%s", recoveryLine)
	}
	if strings.Contains(out, "writes withheld for this instance") {
		t.Errorf("an open gate logs no withheld line at all:\n%s", out)
	}

	// The ordinary season's line stays exactly what every other Sonarr write
	// logs, and carries no recovery marker.
	ordinary := logLineContaining(t, out, "msg=unmonitor ")
	if !strings.Contains(ordinary, "season=2") {
		t.Errorf("the ordinary write's line must be season 2's:\n%s", ordinary)
	}
	if strings.Contains(ordinary, "recovery=") {
		t.Errorf("an ordinary write is never marked recovery:\n%s", ordinary)
	}

	// The recovery really is the season PUT alone: there was nothing left to
	// unmonitor at the episode level, so exactly one episode call (season 2's)
	// may have gone out.
	episodeCalls := 0
	for _, w := range fake.writes() {
		if w.path == episodeMonitorPath {
			episodeCalls++
			if !strings.Contains(string(w.body), "200") {
				t.Errorf("the only episode call may be the ordinary season's: %s", w.body)
			}
		}
	}
	if episodeCalls != 1 {
		t.Errorf("episode calls = %d, want 1 (the recovery season has no monitored episode left): %+v", episodeCalls, fake.writes())
	}
}

// TestRunSonarrWritePass_RecoveryUnderInconclusiveGate_CompletesWhileTheRestIsWithheld
// is the matrix's middle column, and the whole reason the recovery path has a
// gate of its own: an instance recovering from a partial write reports
// inconclusive BY CONSTRUCTION (the half-written season's episodes are all
// unmonitored, so cross-check shape (a) excludes every one of them and nothing
// remains to verify). If the ordinary gate governed the retry, the gate would
// block its own convergence forever.
func TestRunSonarrWritePass_RecoveryUnderInconclusiveGate_CompletesWhileTheRestIsWithheld(t *testing.T) {
	fake := sonarrRecoveryFixture(t)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	// The shape an instance recovering from a partial write really produces.
	cc := crossCheckResult{status: crossCheckStatusInconclusive, unverifiable: 1, writeUnverifiable: 1}
	unmonitored, recovered, writeErrors, echoUnverified, refused, withheld := runSonarrWritePass(
		context.Background(), logger, fake.client(), fake.instance(), sonarrRecoveryDecisions(), cc, 0, false, false)

	out := buf.String()
	if unmonitored != 0 || recovered != 1 || withheld != 1 {
		t.Fatalf("unmonitored/recovered/withheld = %d/%d/%d, want 0/1/1 — the recovery finishes, the ordinary season stays withheld:\n%s", unmonitored, recovered, withheld, out)
	}
	if writeErrors+echoUnverified+refused != 0 {
		t.Errorf("writeErrors/echoUnverified/refused = %d/%d/%d, want 0/0/0:\n%s", writeErrors, echoUnverified, refused, out)
	}
	if unmonitored+recovered+writeErrors+echoUnverified+refused+withheld != 2 {
		t.Errorf("the accounting identity must still hold over both pending seasons:\n%s", out)
	}

	// Season 2 — the ordinary pending season — must not have been written by
	// the path that let season 1 through, and its episodes must not have been
	// touched either.
	for _, w := range fake.writes() {
		if w.path == episodeMonitorPath {
			t.Errorf("no episode call may be sent under a shut ordinary gate: %+v", w)
			continue
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(w.body, &payload); err != nil {
			continue
		}
		var seasons []struct {
			SeasonNumber int  `json:"seasonNumber"`
			Monitored    bool `json:"monitored"`
		}
		if err := json.Unmarshal(payload["seasons"], &seasons); err != nil {
			continue
		}
		for _, s := range seasons {
			if s.SeasonNumber == 2 && !s.Monitored {
				t.Errorf("only a recovery season may be written while the ordinary gate is shut: %s", w.body)
			}
		}
	}

	// The write that happened without ordinary authorization names itself on
	// its OWN line: an instance-level count says how many, never which.
	recoveryLine := logLineContaining(t, out, "completing a previously partial season unmonitor")
	if !strings.Contains(recoveryLine, "season=1") {
		t.Errorf("the recovery line must name WHICH season it completed:\n%s", recoveryLine)
	}
	if !strings.Contains(recoveryLine, "gateBlocked=") {
		t.Errorf("a recovery that ran while the ordinary gate was shut must name the authorization it did not have:\n%s", recoveryLine)
	}
	if !strings.Contains(recoveryLine, "recoveryReason=") {
		t.Errorf("the recovery line must carry the justification for writing anyway:\n%s", recoveryLine)
	}

	// The gate line reports what the gate ACTUALLY withheld and what the
	// recovery path ACTUALLY completed — facts, because it is written after the
	// pass rather than predicted before it.
	gateLine := logLineContaining(t, out, "writes withheld for this instance")
	if !strings.Contains(gateLine, "withheldWrites=1") {
		t.Errorf("the gate line must report the writes it really withheld (1 of the 2 pending):\n%s", gateLine)
	}
	if !strings.Contains(gateLine, "recoveredWrites=1") {
		t.Errorf("the gate line must report what the recovery path completed in spite of it:\n%s", gateLine)
	}
	// F7: a shut gate authorized none of these writes, so nothing may claim
	// writes "proceeded" on the strength of the cross-check it just failed.
	if strings.Contains(out, "writes proceeding on a partially verified cross-check") {
		t.Errorf("a shut gate must not also report writes proceeding on its own sample:\n%s", out)
	}
}

// TestRunSonarrWritePass_RecoveryUnderFailedOrUnrecognizedCrossCheck_IsBlocked
// is the matrix's right-hand column: a disagreement stops this project before
// writes, and that rule has no exceptions — a data layer with proven
// disagreements taints every read a decision rests on, recovery-shaped or not.
// An unrecognized status is a bug signal and is treated identically.
//
// Nothing is even FETCHED in this cell: with both gates shut the pass returns
// exactly where the Radarr twin does.
func TestRunSonarrWritePass_RecoveryUnderFailedOrUnrecognizedCrossCheck_IsBlocked(t *testing.T) {
	for _, status := range []string{crossCheckStatusFailed, "some-future-status"} {
		t.Run(status, func(t *testing.T) {
			fake := sonarrRecoveryFixture(t)
			logger, buf := newDecisionTestLogger(slog.LevelInfo)

			cc := crossCheckResult{status: status, unverifiable: 1, writeUnverifiable: 1}
			unmonitored, recovered, _, _, _, withheld := runSonarrWritePass(
				context.Background(), logger, fake.client(), fake.instance(), sonarrRecoveryDecisions(), cc, 0, false, false)

			out := buf.String()
			if unmonitored != 0 || recovered != 0 || withheld != 2 {
				t.Errorf("unmonitored/recovered/withheld = %d/%d/%d, want 0/0/2:\n%s", unmonitored, recovered, withheld, out)
			}
			if reqs := fake.all(); len(reqs) != 0 {
				t.Errorf("a %s cross-check must not even READ, let alone write: %+v", status, reqs)
			}
			gateLine := logLineContaining(t, out, "writes withheld for this instance")
			if !strings.Contains(gateLine, "the recovery path is shut too") {
				t.Errorf("the gate line must say the recovery path was blocked as well, and why:\n%s", gateLine)
			}
			if !strings.Contains(gateLine, "withheldWrites=2") || !strings.Contains(gateLine, "recoveredWrites=0") {
				t.Errorf("the gate line must account for every pending season:\n%s", gateLine)
			}
		})
	}
}

// TestRunSonarrWritePass_NonRecoverySeasonUnderInconclusiveGate_IsWithheldUnfetched
// is the matrix's ordinary/inconclusive cell on its own, with no recovery
// season in the pass to carry it: an ordinary would-unmonitor season under a
// shut ordinary gate is withheld, and — since the recovery path is the only
// one open and this season is not a candidate for it — nothing is fetched for
// it either.
func TestRunSonarrWritePass_NonRecoverySeasonUnderInconclusiveGate_IsWithheldUnfetched(t *testing.T) {
	fake := sonarrRecoveryFixture(t)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	ordinaryOnly := sonarrRecoveryDecisions()[1:]
	cc := crossCheckResult{status: crossCheckStatusInconclusive, unverifiable: 1, writeUnverifiable: 1}
	unmonitored, recovered, writeErrors, echoUnverified, refused, withheld := runSonarrWritePass(
		context.Background(), logger, fake.client(), fake.instance(), ordinaryOnly, cc, 0, false, false)

	out := buf.String()
	if unmonitored != 0 || recovered != 0 || withheld != 1 {
		t.Fatalf("unmonitored/recovered/withheld = %d/%d/%d, want 0/0/1:\n%s", unmonitored, recovered, withheld, out)
	}
	if writeErrors+echoUnverified+refused != 0 {
		t.Errorf("writeErrors/echoUnverified/refused = %d/%d/%d, want 0/0/0:\n%s", writeErrors, echoUnverified, refused, out)
	}
	if reqs := fake.all(); len(reqs) != 0 {
		t.Errorf("a season the recovery path cannot admit is withheld without a fetch: %+v", reqs)
	}
	gateLine := logLineContaining(t, out, "writes withheld for this instance")
	if !strings.Contains(gateLine, "withheldWrites=1") || !strings.Contains(gateLine, "recoveredWrites=0") {
		t.Errorf("the gate line must account for the pending season:\n%s", gateLine)
	}
}

// TestRunSonarrWritePass_DryRun_GateBlocked_RecoveryIsRehearsedNotWritten is
// §2.1 on the one path that bypasses the ordinary gate, and it is the test that
// most needed writing.
//
// The recovery path is the only one in this project that reaches
// unmonitorSeason without ordinary cross-check authorization; it deliberately
// runs where the ordinary gate says no, which makes it the likeliest place for
// a future edit to hoist a write above the OTHER gate too. Everything that
// keeps the dry-run promise here lives inside unmonitorSeason.
//
// So: a SHUT ordinary gate, an admitted recovery season, dryRun=true. Both
// seasons must end withheld, the catch-all writes() must be empty, the fresh
// GETs must still have happened (a rehearsal that never ran would satisfy
// "zero writes" while proving nothing), and no line may claim a write.
func TestRunSonarrWritePass_DryRun_GateBlocked_RecoveryIsRehearsedNotWritten(t *testing.T) {
	fake := sonarrRecoveryFixture(t)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	// Byte-identical to the write-mode test above except for the last argument.
	cc := crossCheckResult{status: crossCheckStatusInconclusive, unverifiable: 1, writeUnverifiable: 1}
	unmonitored, recovered, writeErrors, echoUnverified, refused, withheld := runSonarrWritePass(
		context.Background(), logger, fake.client(), fake.instance(), sonarrRecoveryDecisions(), cc, 0, false, true)

	out := buf.String()
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("a dry run must send ZERO write requests of any method to any path, even for a recovery season: %+v\n%s", writes, out)
	}
	if unmonitored != 0 || recovered != 0 {
		t.Errorf("unmonitored/recovered = %d/%d, want 0/0 — a rehearsal completes nothing:\n%s", unmonitored, recovered, out)
	}
	if withheld != 2 {
		t.Errorf("withheld = %d, want 2 — the recovery season is withheld at unmonitorSeason's own §2.1 gates, exactly like the ordinary one:\n%s", withheld, out)
	}
	if writeErrors+echoUnverified+refused != 0 {
		t.Errorf("writeErrors/echoUnverified/refused = %d/%d/%d, want 0/0/0:\n%s", writeErrors, echoUnverified, refused, out)
	}
	if unmonitored+recovered+writeErrors+echoUnverified+refused+withheld != 2 {
		t.Errorf("the accounting identity must hold over both pending seasons in dry-run too:\n%s", out)
	}

	// The rehearsal is REAL: the recovery season was admitted and its fresh
	// GETs went out. Without this, "zero writes" would also be satisfied by a
	// recovery path that never ran at all.
	sawSeriesGet, sawEpisodeGet := false, false
	for _, r := range fake.all() {
		if r.method != http.MethodGet {
			continue
		}
		switch r.path {
		case "/api/v3/series/3":
			sawSeriesGet = true
		case "/api/v3/episode":
			sawEpisodeGet = true
		}
	}
	if !sawSeriesGet || !sawEpisodeGet {
		t.Errorf("the dry-run rehearsal must still take the fresh look at the world (series=%t episode=%t):\n%+v", sawSeriesGet, sawEpisodeGet, fake.all())
	}

	// No write happened, so no line may report one.
	if strings.Contains(out, "msg=unmonitor ") {
		t.Errorf("a dry run must not log a completed unmonitor:\n%s", out)
	}
	if strings.Contains(out, "completing a previously partial season unmonitor") {
		t.Errorf("a dry run must not report a recovery that never happened:\n%s", out)
	}

	gateLine := logLineContaining(t, out, "writes withheld for this instance")
	if !strings.Contains(gateLine, "dryRun=true") {
		t.Errorf("the gate line must say which mode it is in:\n%s", gateLine)
	}
	// Both numbers are facts about this pass, and both agree with the summary's
	// counters of the same names — the previous shape had to rename them in
	// dry-run to stop them claiming writes that were never sent.
	if !strings.Contains(gateLine, "withheldWrites=2") {
		t.Errorf("in dry-run every pending season ends withheld, and this line must say so:\n%s", gateLine)
	}
	if !strings.Contains(gateLine, "recoveredWrites=0") {
		t.Errorf("a rehearsal completes no recovery, and the line must not imply one:\n%s", gateLine)
	}
}

// TestUnmonitorSeason_RecoveryVerdictComesFromTheFreshData proves where the
// recovery verdict is decided. The decision handed in says nothing at all here
// — unmonitorSeason is called directly — and the fresh /episode read is what
// makes this season a recovery: every episode already unmonitored, so the write
// is the season PUT alone and the function says so on its way out.
func TestUnmonitorSeason_RecoveryVerdictComesFromTheFreshData(t *testing.T) {
	fake := sonarrRecoveryFixture(t)
	logger, buf := newDecisionTestLogger(slog.LevelDebug)

	written, recovery, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false, false)
	if err != nil {
		t.Fatalf("unmonitorSeason returned error = %v, want nil:\n%s", err, buf.String())
	}
	if !written || !recovery {
		t.Fatalf("written/recovery = %t/%t, want true/true: a season whose every episode is already unmonitored is a recovery whatever the caller believed:\n%s", written, recovery, buf.String())
	}
	for _, w := range fake.writes() {
		if w.path == episodeMonitorPath {
			t.Errorf("a recovery is the season PUT alone; there is no monitored episode to write: %+v", w)
		}
	}

	// The ordinary shape of the same fixture is NOT a recovery, which is what
	// makes the verdict discriminating rather than always-true.
	ordinary := newSonarrWriterFake(t, sonarrWriterSeriesJSON, sonarrWriterEpisodesJSON)
	written, recovery, err = unmonitorSeason(context.Background(), logger, ordinary.client(), ordinary.instance(), 3, 1, 0, false, false, false)
	if err != nil || !written {
		t.Fatalf("written/err = %t/%v, want true/nil", written, err)
	}
	if recovery {
		t.Error("recovery = true for a season with monitored episodes: the verdict must be the fresh data's, not a constant")
	}
}

// TestUnmonitorSeason_RecoveryRequired_MonitoredEpisodeAtWriteTime_Refuses is
// what keeps the recovery path narrow at the moment of consequence. The path
// admits a season on decision-time evidence; the fresh data is the authority.
// A season that has regained a monitored episode since is an ORDINARY write,
// and an ordinary write needs the ordinary gate — which, whenever this
// precondition is being enforced, is shut.
func TestUnmonitorSeason_RecoveryRequired_MonitoredEpisodeAtWriteTime_Refuses(t *testing.T) {
	fake := newSonarrWriterFake(t, sonarrWriterSeriesJSON, sonarrWriterEpisodesJSON)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	written, recovery, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false, true)
	if written || recovery {
		t.Errorf("written/recovery = %t/%t, want false/false", written, recovery)
	}
	if !errors.Is(err, errNotRecoveryAtWrite) {
		t.Fatalf("err = %v, want an errNotRecoveryAtWrite-wrapped error", err)
	}
	if !isWriteRefusal(err) {
		t.Errorf("this must count as writesRefused — no write was sent and something DID need doing: %v", err)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("nothing may be written when the recovery path's precondition no longer holds, got %+v", writes)
	}
	if !strings.Contains(buf.String(), "recovery pass") {
		t.Errorf("the refusal must name the path it belongs to:\n%s", buf.String())
	}
}

// TestRunSonarrWritePass_GateBlocked_NonCandidateRefusesAtWriteTime_IsCounted
// closes the loop between the two halves above: a season the recovery path
// admits on decision-time evidence but the fresh data disqualifies must land in
// writesRefused, not vanish. It is the identity's proof that a stale admission
// cannot make a pending season disappear from the accounting.
func TestRunSonarrWritePass_GateBlocked_NonCandidateRefusesAtWriteTime_IsCounted(t *testing.T) {
	// Decision time said every episode of season 1 was unmonitored; the fresh
	// fixture says otherwise.
	fake := newSonarrWriterFake(t, sonarrWriterSeriesJSON, sonarrWriterEpisodesJSON)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	cc := crossCheckResult{status: crossCheckStatusInconclusive, unverifiable: 1, writeUnverifiable: 1}
	unmonitored, recovered, writeErrors, echoUnverified, refused, withheld := runSonarrWritePass(
		context.Background(), logger, fake.client(), fake.instance(), sonarrRecoveryDecisions()[:1], cc, 0, false, false)

	out := buf.String()
	if refused != 1 {
		t.Fatalf("writesRefused = %d, want 1 — a stale recovery admission the fresh data disqualifies is refused, not written and not lost:\n%s", refused, out)
	}
	if unmonitored+recovered+writeErrors+echoUnverified+withheld != 0 {
		t.Errorf("no other counter may move: got unmonitored=%d recovered=%d writeErrors=%d echoUnverified=%d withheld=%d:\n%s",
			unmonitored, recovered, writeErrors, echoUnverified, withheld, out)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("nothing may be written: %+v", writes)
	}
}

// ============================================================================
// The Sonarr write pass, driven through runSonarrDecisionEngine
// ============================================================================

// sonarrWriteEngineSeriesDetail renders the GET /api/v3/series/{id} body the
// write path re-fetches: one series, one monitored season whose statistics
// match the engine fake's episode fixture.
func sonarrWriteEngineSeriesDetail(id int, title string, seasonNumber, episodes int, monitored bool) string {
	return fmt.Sprintf(`{"id":%d,"title":%q,"monitored":true,"qualityProfileId":1,"tags":[],"seasons":[{"seasonNumber":%d,"monitored":%t,"statistics":{"episodeFileCount":%d,"totalEpisodeCount":%d}}]}`,
		id, title, seasonNumber, monitored, episodes, episodes)
}

// sonarrSummaryCounters parses every integer attr off the run's single
// "sonarr decision summary" line, mirroring summaryCounters (decision_test.go)
// for the Radarr summary.
func sonarrSummaryCounters(t *testing.T, output string) map[string]int {
	t.Helper()
	return summaryCountersFor(t, output, "sonarr decision summary")
}

// TestRunSonarrDecisionEngine_WriteMode_ConfirmedWriteCountsAsUnmonitored is
// the Sonarr write pass end to end through the engine: a season that passes
// every rule, a cross-check that verifies it, and a write both of whose
// halves the server confirms.
func TestRunSonarrDecisionEngine_WriteMode_ConfirmedWriteCountsAsUnmonitored(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "]"
	filesJSON := "[" + episodeFileJSON(500, 1, 200, false) + "]"
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	fake.seriesDetail[1] = sonarrWriteEngineSeriesDetail(1, "Writable Show", 1, 1, true)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	series := []seriesElement{testSeries(1, "Writable Show", true, 1, []int{}, testSeason(1, true, 1, 1))}
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude", 0, false)

	out := buf.String()
	c := sonarrSummaryCounters(t, out)
	if c["wouldUnmonitor"] != 1 {
		t.Fatalf("wouldUnmonitor = %d, want 1; this test proves nothing otherwise:\n%s", c["wouldUnmonitor"], out)
	}
	if c["unmonitored"] != 1 {
		t.Errorf("unmonitored = %d, want 1 (seasons, the decision unit — not episodes):\n%s", c["unmonitored"], out)
	}
	if !strings.Contains(out, "msg=unmonitor ") {
		t.Errorf("expected a msg=unmonitor report line for the written season:\n%s", out)
	}

	writes := fake.writes()
	if len(writes) != 2 {
		t.Fatalf("expected exactly 2 write requests for one season (episode monitor, then series), got %d: %+v", len(writes), writes)
	}
	if writes[0].path != "/api/v3/episode/monitor" || writes[1].path != "/api/v3/series/1" {
		t.Errorf("write order = %s then %s, want /api/v3/episode/monitor then /api/v3/series/1", writes[0].path, writes[1].path)
	}
}

// TestRunSonarrDecisionEngine_DryRun_RehearsesAndWithholds is §2.1 at the
// engine level: the decisions are made, the write pass runs as a full
// rehearsal (the fresh reads happen), and not one write request is sent.
func TestRunSonarrDecisionEngine_DryRun_RehearsesAndWithholds(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "]"
	filesJSON := "[" + episodeFileJSON(500, 1, 200, false) + "]"
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	fake.seriesDetail[1] = sonarrWriteEngineSeriesDetail(1, "Rehearsed Show", 1, 1, true)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	series := []seriesElement{testSeries(1, "Rehearsed Show", true, 1, []int{}, testSeason(1, true, 1, 1))}
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude", 0, true)

	out := buf.String()
	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("dry-run made %d write request(s), want ZERO: %+v", len(writes), writes)
	}
	// The rehearsal really ran: the write path's own fresh GET happened.
	sawFreshGet := false
	for _, r := range fake.all() {
		if r.method == http.MethodGet && r.path == "/api/v3/series/1" {
			sawFreshGet = true
		}
	}
	if !sawFreshGet {
		t.Errorf("dry-run must rehearse the write path, including its fresh GET:\n%+v", fake.all())
	}

	c := sonarrSummaryCounters(t, out)
	if c["withheldWrites"] != 1 || c["unmonitored"] != 0 {
		t.Errorf("withheldWrites/unmonitored = %d/%d, want 1/0:\n%s", c["withheldWrites"], c["unmonitored"], out)
	}
	// writeErrors is unconditionally 0 in dry-run — no write was attempted,
	// so none can have failed — and the rehearsal's own failures are reported
	// under their own name so a report a human reads as "nothing was
	// attempted" never carries a number that reads as "N writes failed".
	if !strings.Contains(out, "writeRehearsalErrors=0") {
		t.Errorf("a dry-run summary must carry writeRehearsalErrors, not writeEchoUnverified:\n%s", out)
	}
	if strings.Contains(out, "writeEchoUnverified") {
		t.Errorf("writeEchoUnverified cannot occur in dry-run and must not be printed there:\n%s", out)
	}
}

// TestRunSonarrDecisionEngine_EveryWouldUnmonitorSeasonIsAccountedForInTheSummary
// is the Sonarr half of the Phase 5 accounting identity (binding controller
// resolution 5):
//
//	wouldUnmonitor == unmonitored + writeEchoUnverified + writeErrors
//	                  + writeRehearsalErrors + writesRefused + withheldWrites
//
// Each case makes the single would-unmonitor season end a different way; the
// named counter must absorb it, and the identity is what proves nothing fell
// between the counters.
func TestRunSonarrDecisionEngine_EveryWouldUnmonitorSeasonIsAccountedForInTheSummary(t *testing.T) {
	const okEpisodes = `[{"id": 100, "seriesId": 1, "seasonNumber": 1, "episodeNumber": 1, "monitored": true, "hasFile": true, "airDateUtc": "2015-01-01T00:00:00Z", "episodeFileId": 500}]`
	cases := []struct {
		name         string
		detail       map[int]string
		tagsJSON     string
		seriesPut    map[int]int
		seriesEcho   map[int]string
		episodeMonPU int
		episodesJSON string
		dryRun       bool
		wantCounter  string
	}{
		{
			name:        "confirmed write",
			detail:      map[int]string{1: sonarrWriteEngineSeriesDetail(1, "Accounted Show", 1, 1, true)},
			wantCounter: "unmonitored",
		},
		{
			name:        "the server rejected the season PUT",
			detail:      map[int]string{1: sonarrWriteEngineSeriesDetail(1, "Accounted Show", 1, 1, true)},
			seriesPut:   map[int]int{1: http.StatusInternalServerError},
			wantCounter: "writeErrors",
		},
		{
			name:        "the season PUT was accepted but the echo confirms nothing",
			detail:      map[int]string{1: sonarrWriteEngineSeriesDetail(1, "Accounted Show", 1, 1, true)},
			seriesEcho:  map[int]string{1: ""},
			wantCounter: "writeEchoUnverified",
		},
		{
			name:        "the series vanished before the pre-write fetch",
			detail:      map[int]string{},
			wantCounter: "writeErrors",
		},
		{
			name:        "the exclusion tag was added between scan and write",
			detail:      map[int]string{1: `{"id":1,"title":"Accounted Show","monitored":true,"tags":[42],"seasons":[{"seasonNumber":1,"monitored":true,"statistics":{"episodeFileCount":1,"totalEpisodeCount":1}}]}`},
			tagsJSON:    `[{"id": 42, "label": "cutoffarr-exclude"}]`,
			wantCounter: "writesRefused",
		},
		{
			name:        "the fresh payload's tags cannot be verified",
			detail:      map[int]string{1: `{"id":1,"title":"Accounted Show","monitored":true,"seasons":[{"seasonNumber":1,"monitored":true,"statistics":{"episodeFileCount":1,"totalEpisodeCount":1}}]}`},
			tagsJSON:    `[{"id": 42, "label": "cutoffarr-exclude"}]`,
			wantCounter: "writesRefused",
		},
		{
			name:        "something else unmonitored the season first",
			detail:      map[int]string{1: sonarrWriteEngineSeriesDetail(1, "Accounted Show", 1, 1, false)},
			wantCounter: "writesRefused",
		},
		{
			name:        "the season's monitored is JSON null",
			detail:      map[int]string{1: `{"id":1,"title":"Accounted Show","monitored":true,"tags":[],"seasons":[{"seasonNumber":1,"monitored":null,"statistics":{"episodeFileCount":1,"totalEpisodeCount":1}}]}`},
			wantCounter: "writesRefused",
		},
		{
			name:   "the season started airing between scan and write",
			detail: map[int]string{1: sonarrWriteEngineSeriesDetail(1, "Accounted Show", 1, 1, true)},
			// The decision-time fixture is aired; the write-time re-read is not.
			episodesJSON: `[{"id": 100, "seriesId": 1, "seasonNumber": 1, "episodeNumber": 1, "monitored": true, "hasFile": true, "airDateUtc": "2099-01-01T00:00:00Z", "episodeFileId": 500}]`,
			wantCounter:  "writesRefused",
		},
		{
			// The recovery path's own row: the decision was made on a monitored
			// episode, but by write time every episode of the season is already
			// unmonitored — so only the season flag is left to write, and the
			// confirmed write lands in recoveredWrites rather than unmonitored.
			name:         "a recovery completed the season",
			detail:       map[int]string{1: sonarrWriteEngineSeriesDetail(1, "Accounted Show", 1, 1, true)},
			episodesJSON: `[{"id": 100, "seriesId": 1, "seasonNumber": 1, "episodeNumber": 1, "monitored": false, "hasFile": true, "airDateUtc": "2015-01-01T00:00:00Z", "episodeFileId": 500}]`,
			wantCounter:  "recoveredWrites",
		},
		{
			name:        "dry-run withheld the write at the gate",
			detail:      map[int]string{1: sonarrWriteEngineSeriesDetail(1, "Accounted Show", 1, 1, true)},
			dryRun:      true,
			wantCounter: "withheldWrites",
		},
		{
			name:        "dry-run rehearsal failed before the gate",
			detail:      map[int]string{},
			dryRun:      true,
			wantCounter: "writeRehearsalErrors",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			filesJSON := "[" + episodeFileJSON(500, 1, 200, false) + "]"
			fake := newSonarrEngineFake(t, okEpisodes, filesJSON)
			if tc.tagsJSON != "" {
				fake.tagsJSON = tc.tagsJSON
			}
			for id, body := range tc.detail {
				fake.seriesDetail[id] = body
			}
			for id, status := range tc.seriesPut {
				fake.seriesPutStatus[id] = status
			}
			for id, echo := range tc.seriesEcho {
				fake.seriesPutEcho[id] = echo
			}
			if tc.episodesJSON != "" {
				// The decision engine reads the healthy fixture; only the
				// write path's own fresh re-read sees the changed world.
				fake.writeTimeEpisodeJSON = tc.episodesJSON
			}

			logger, buf := newDecisionTestLogger(slog.LevelInfo)
			series := []seriesElement{testSeries(1, "Accounted Show", true, 1, []int{}, testSeason(1, true, 1, 1))}
			runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude", 0, tc.dryRun)

			out := buf.String()
			c := sonarrSummaryCounters(t, out)
			if c["wouldUnmonitor"] != 1 {
				t.Fatalf("wouldUnmonitor = %d, want 1: this case must actually reach the write pass or it proves nothing:\n%s", c["wouldUnmonitor"], out)
			}
			if c[tc.wantCounter] != 1 {
				t.Errorf("%s = %d, want 1: this outcome must be counted under that name:\n%s", tc.wantCounter, c[tc.wantCounter], out)
			}
			accounted := c["unmonitored"] + c["recoveredWrites"] + c["writeEchoUnverified"] + c["writeErrors"] + c["writeRehearsalErrors"] + c["writesRefused"] + c["withheldWrites"]
			if accounted != c["wouldUnmonitor"] {
				t.Errorf("the summary accounts for %d of %d promised writes; every would-unmonitor season must end in exactly one counted outcome:\n%s", accounted, c["wouldUnmonitor"], out)
			}
			// Every counter of the identity, present in the mode that can
			// produce it, including as 0 — an absent number must never be
			// readable as "none happened". The two mode-specific ones were
			// unpinned: writeEchoUnverified is printed only in write mode and
			// writeRehearsalErrors only in dry-run, so a loop that named
			// neither left two of the identity's terms free to disappear.
			always := []string{"unmonitored=", "recoveredWrites=", "writesRefused=", "withheldWrites=", "writeErrors="}
			if tc.dryRun {
				always = append(always, "writeRehearsalErrors=")
			} else {
				always = append(always, "writeEchoUnverified=")
			}
			for _, want := range always {
				if !strings.Contains(out, want) {
					t.Errorf("the summary must always carry %s, including as 0:\n%s", want, out)
				}
			}
		})
	}
}

// TestRunSonarrDecisionEngine_TwoPendingSeasons_OneRefuses_IdentityStillHolds
// pins the accounting identity at wouldUnmonitor=2 — every other case pins it
// at 1, which cannot distinguish "each season ends in exactly one counted
// outcome" from "the pass counts one thing once".
//
// It is also the only proof of the per-season isolation the write loop's doc
// comment claims ("one season's refusal cannot drag along another"): the season
// that refuses is the FIRST one, so a `continue` silently becoming a `return`
// or `break` would take the second season's write down with it and the
// summary would report one pending write that simply vanished.
func TestRunSonarrDecisionEngine_TwoPendingSeasons_OneRefuses_IdentityStillHolds(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "," + episodeJSON(200, 2, 1, pastAirDate, 600) + "]"
	filesJSON := "[" + episodeFileJSON(500, 1, 200, false) + "," + episodeFileJSON(600, 2, 200, false) + "]"
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	// The write-time truth: something unmonitored season 1 between the scan and
	// the write (a race — a refusal), while season 2 is still writable.
	fake.seriesDetail[1] = `{"id":1,"title":"Two Season Show","monitored":true,"qualityProfileId":1,"tags":[],"seasons":[` +
		`{"seasonNumber":1,"monitored":false,"statistics":{"episodeFileCount":1,"totalEpisodeCount":1}},` +
		`{"seasonNumber":2,"monitored":true,"statistics":{"episodeFileCount":1,"totalEpisodeCount":1}}]}`
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	series := []seriesElement{testSeries(1, "Two Season Show", true, 1, []int{}, testSeason(1, true, 1, 1), testSeason(2, true, 1, 1))}
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude", 0, false)

	out := buf.String()
	c := sonarrSummaryCounters(t, out)
	if c["wouldUnmonitor"] != 2 {
		t.Fatalf("wouldUnmonitor = %d, want 2; this test proves nothing otherwise:\n%s", c["wouldUnmonitor"], out)
	}
	if c["unmonitored"] != 1 || c["writesRefused"] != 1 {
		t.Errorf("unmonitored/writesRefused = %d/%d, want 1/1 — the refusal must not take the other season with it:\n%s", c["unmonitored"], c["writesRefused"], out)
	}
	accounted := c["unmonitored"] + c["recoveredWrites"] + c["writeEchoUnverified"] + c["writeErrors"] + c["writeRehearsalErrors"] + c["writesRefused"] + c["withheldWrites"]
	if accounted != c["wouldUnmonitor"] {
		t.Errorf("the summary accounts for %d of %d promised writes:\n%s", accounted, c["wouldUnmonitor"], out)
	}

	// Season 2's write really happened, and it is the only one.
	var seriesPuts []recordedRequest
	for _, w := range fake.writes() {
		if w.path == "/api/v3/series/1" {
			seriesPuts = append(seriesPuts, w)
		}
	}
	if len(seriesPuts) != 1 {
		t.Fatalf("expected exactly 1 series PUT (season 2's), got %d: %+v", len(seriesPuts), seriesPuts)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(seriesPuts[0].body, &payload); err != nil {
		t.Fatalf("the series PUT body is not a JSON object: %v", err)
	}
	var seasons []struct {
		SeasonNumber int  `json:"seasonNumber"`
		Monitored    bool `json:"monitored"`
	}
	if err := json.Unmarshal(payload["seasons"], &seasons); err != nil {
		t.Fatalf("the series PUT's seasons is not an array: %v", err)
	}
	for _, s := range seasons {
		if s.SeasonNumber == 2 && s.Monitored {
			t.Errorf("the surviving write must be season 2's: %s", seriesPuts[0].body)
		}
	}
}

// TestRunSonarrDecisionEngine_GateBlocked_WithheldWritesAccountsForThePass is
// the identity's remaining term, which no case above can reach: when the
// cross-check refuses to authorize the pass, every pending write is withheld
// before unmonitorSeason is called even once.
func TestRunSonarrDecisionEngine_GateBlocked_WithheldWritesAccountsForThePass(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "]"
	// qualityCutoffNotMet=true while the (empty) wanted set says the episode
	// is not below cutoff: two Sonarr code paths disagreeing, so the
	// cross-check FAILS and the gate blocks the whole pass.
	filesJSON := "[" + episodeFileJSON(500, 1, 200, true) + "]"
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	fake.seriesDetail[1] = sonarrWriteEngineSeriesDetail(1, "Blocked Show", 1, 1, true)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	series := []seriesElement{testSeries(1, "Blocked Show", true, 1, []int{}, testSeason(1, true, 1, 1))}
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude", 0, false)

	out := buf.String()
	if !strings.Contains(out, "crossCheck=FAILED") {
		t.Fatalf("expected the cross-check to fail in this fixture:\n%s", out)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("a blocked gate must write nothing at all, got %+v", writes)
	}
	c := sonarrSummaryCounters(t, out)
	if c["withheldWrites"] != c["wouldUnmonitor"] || c["wouldUnmonitor"] == 0 {
		t.Errorf("withheldWrites = %d, want it to account for all %d pending writes:\n%s", c["withheldWrites"], c["wouldUnmonitor"], out)
	}
	if !strings.Contains(out, "writes withheld for this instance") {
		t.Errorf("a blocked pass must say so out loud — \"nothing was written\" and \"nothing needed writing\" must never look the same:\n%s", out)
	}
}

// TestRunSonarrDecisionEngine_OnlyID_ScopesReportAndWritesToOneSeries is
// controller resolution 4 in full: --only-id <seriesId> names a series and
// means ALL ITS ELIGIBLE SEASONS IN ONE RUN, with both the report and the
// write pass scoped to it. Every series is still EVALUATED, because the
// cross-check validates the data the decision rests on rather than the target
// — the same split runRadarrDecisionEngine makes.
//
// The named series carries TWO eligible seasons on purpose: the "all its
// eligible seasons" half of that resolution had no coverage at all while every
// fixture gave each series exactly one. Each series also carries one
// already-unmonitored season, because alreadyUnmonitored and
// totalSeriesMonitored both moved INSIDE the --only-id scope in this phase and
// neither was asserted — an unscoped count of either would have gone unnoticed.
//
// The titles say which series is which: id 2 is the one --only-id names, so id
// 2 is "Named Show". They used to be the other way round, which made every
// assertion in here read backwards.
func TestRunSonarrDecisionEngine_OnlyID_ScopesReportAndWritesToOneSeries(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "," + episodeJSON(200, 2, 1, pastAirDate, 600) + "]"
	filesJSON := "[" + episodeFileJSON(500, 1, 200, false) + "," + episodeFileJSON(600, 2, 200, false) + "]"
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	fake.seriesDetail[1] = sonarrWriteEngineSeriesDetail(1, "Other Show", 1, 1, true)
	fake.seriesDetail[2] = `{"id":2,"title":"Named Show","monitored":true,"qualityProfileId":1,"tags":[],"seasons":[` +
		`{"seasonNumber":1,"monitored":true,"statistics":{"episodeFileCount":1,"totalEpisodeCount":1}},` +
		`{"seasonNumber":2,"monitored":true,"statistics":{"episodeFileCount":1,"totalEpisodeCount":1}}]}`
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	series := []seriesElement{
		testSeries(1, "Other Show", true, 1, []int{}, testSeason(1, true, 1, 1), testSeason(9, false, 1, 1)),
		testSeries(2, "Named Show", true, 1, []int{}, testSeason(1, true, 1, 1), testSeason(2, true, 1, 1), testSeason(9, false, 1, 1)),
	}
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude", 2, false)

	out := buf.String()
	for _, w := range fake.writes() {
		if w.path == "/api/v3/series/1" {
			t.Errorf("a --only-id run must never write a series it does not name: %+v", w)
		}
	}
	namedPuts := 0
	for _, w := range fake.writes() {
		if w.path == "/api/v3/series/2" {
			namedPuts++
		}
	}
	if namedPuts != 2 {
		t.Errorf("series PUTs to the named series = %d, want 2 — --only-id N means all of series N's eligible seasons in one run: %+v", namedPuts, fake.writes())
	}
	c := sonarrSummaryCounters(t, out)
	if c["wouldUnmonitor"] != 2 || c["unmonitored"] != 2 {
		t.Errorf("wouldUnmonitor/unmonitored = %d/%d, want 2/2 (both eligible seasons of the named series, and nothing else):\n%s", c["wouldUnmonitor"], c["unmonitored"], out)
	}
	// Both counters this phase moved inside the scope. Each library series has
	// one already-unmonitored season and both series are monitored, so an
	// unscoped count would read 2 for each of these.
	if c["totalSeriesMonitored"] != 1 {
		t.Errorf("totalSeriesMonitored = %d, want 1 — a scoped run counts the series it was scoped to, not the library:\n%s", c["totalSeriesMonitored"], out)
	}
	if c["alreadyUnmonitored"] != 1 {
		t.Errorf("alreadyUnmonitored = %d, want 1 — only the named series' already-unmonitored season may be counted:\n%s", c["alreadyUnmonitored"], out)
	}
	if c["seasonsEvaluated"] != 2 {
		t.Errorf("seasonsEvaluated = %d, want 2 (the named series' two monitored seasons):\n%s", c["seasonsEvaluated"], out)
	}
	if !strings.Contains(out, "onlyId=2") {
		t.Errorf("the summary must say the run was scoped:\n%s", out)
	}
	// Report lines only. The cross-check's own lines DO name the unnamed
	// series, and correctly so: it samples the whole library because it
	// validates the data the decision rests on, not the target.
	for _, line := range strings.Split(out, "\n") {
		isReport := strings.Contains(line, "msg=would-unmonitor") || strings.Contains(line, "msg=skip") || strings.Contains(line, "msg=unmonitor ")
		if isReport && strings.Contains(line, `series="Other Show"`) {
			t.Errorf("a --only-id run must not REPORT a series it does not name: %q", line)
		}
	}
	if !strings.Contains(out, `msg="cross-check season" instance=sonarr-main seriesId=1`) {
		t.Errorf("the cross-check must still sample the whole library — it validates the data, not the target:\n%s", out)
	}
}

// TestRunSonarrDecisionEngine_OnlyID_UnmonitoredSeries_SaysSoRatherThanNothing
// pins the one line standing between a scoped run on an unmonitored series and
// total silence about the series a human explicitly named.
//
// The series IS in the library (checked before anything is fetched, and
// matching there required an id), so the only thing that can keep it out of
// the report is rule 1's series half. Without this line the run would print a
// summary of zeroes and never mention why — the exact silent-evaporation
// failure main.go's fatal guards exist to prevent — and the line had no test
// at all, so deleting it left the suite green.
func TestRunSonarrDecisionEngine_OnlyID_UnmonitoredSeries_SaysSoRatherThanNothing(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "]"
	filesJSON := "[" + episodeFileJSON(500, 1, 200, false) + "]"
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	series := []seriesElement{
		testSeries(1, "Monitored Show", true, 1, []int{}, testSeason(1, true, 1, 1)),
		testSeries(2, "Unmonitored Show", false, 1, []int{}, testSeason(1, true, 1, 1)),
	}
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude", 2, false)

	out := buf.String()
	line := logLineContaining(t, out, "--only-id series produced no decision")
	if !strings.Contains(line, "onlyId=2") {
		t.Errorf("the line must name the id the human asked about:\n%s", line)
	}
	if !strings.Contains(line, "rule 1") {
		t.Errorf("the line must say WHY there is nothing to report:\n%s", line)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("nothing may be written for an unmonitored series: %+v", writes)
	}
	c := sonarrSummaryCounters(t, out)
	if c["totalSeriesMonitored"] != 0 || c["wouldUnmonitor"] != 0 {
		t.Errorf("totalSeriesMonitored/wouldUnmonitor = %d/%d, want 0/0 — this test only proves something while the report really is empty:\n%s",
			c["totalSeriesMonitored"], c["wouldUnmonitor"], out)
	}
}

// TestRunSonarrDecisionEngine_OnlyID_UnknownSeries_WarnsAndWritesNothing
// mirrors the Radarr precedent: a mistyped id is a warning, not a silent
// no-op, and nothing further is fetched or written for the instance.
func TestRunSonarrDecisionEngine_OnlyID_UnknownSeries_WarnsAndWritesNothing(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "]"
	filesJSON := "[" + episodeFileJSON(500, 1, 200, false) + "]"
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	series := []seriesElement{testSeries(1, "Only Show", true, 1, []int{}, testSeason(1, true, 1, 1))}
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude", 99, false)

	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "onlyId=99") {
		t.Errorf("expected a warning naming the id that matched nothing:\n%s", out)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("nothing may be written, got %+v", writes)
	}
	if strings.Contains(out, "msg=would-unmonitor") {
		t.Errorf("no decisions may be reported for an id this library does not contain:\n%s", out)
	}
}

// ============================================================================
// run()-level: the Sonarr write path against the stateful fake
// ============================================================================

// writeSonarrTestConfig writes a config pointed at one sonarr instance, with
// dry_run and log_level explicit (the no-op test needs debug to observe the
// mandated "already unmonitored" lines).
func writeSonarrTestConfig(t *testing.T, url string, dryRun bool, logLevel string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := fmt.Sprintf(`
dry_run: %t
log_level: %s
instances:
  - name: sonarr-main
    type: sonarr
    url: %s
    api_key: key1
`, dryRun, logLevel, url)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	return path
}

// writableSonarrFake builds the fixture the run()-level write tests share: one
// series with a season that passes every rule (season 1) and a season that is
// still airing (season 2), so exactly one season is ever written and the other
// is proof that the write touched nothing it was not asked to.
func writableSonarrFake(t *testing.T) *statefulSonarrFake {
	t.Helper()
	return newStatefulSonarrFake(t,
		[]*statefulSonarrSeries{
			{id: 1, title: "Write Me", monitored: true, profileID: 1, tags: []int{},
				seasons: []statefulSonarrSeason{
					{number: 1, monitored: true, episodeFileCount: 1, totalEpisodeCount: 1},
					{number: 2, monitored: true, episodeFileCount: 1, totalEpisodeCount: 1},
				}},
		},
		[]*statefulSonarrEpisode{
			{id: 100, seriesID: 1, seasonNumber: 1, episodeNumber: 1, monitored: true, hasFile: true, airDateUtc: pastAirDate, episodeFileID: 500},
			{id: 200, seriesID: 1, seasonNumber: 2, episodeNumber: 1, monitored: true, hasFile: true, airDateUtc: futureAirDate, episodeFileID: 600},
		},
		[]*statefulSonarrEpisodeFile{
			{id: 500, seasonNumber: 1, customFormatScore: 200, qualityCutoffNotMet: false},
			{id: 600, seasonNumber: 2, customFormatScore: 200, qualityCutoffNotMet: false},
		},
	)
}

// TestRun_SonarrWriteMode_UnmonitorsTheSeasonAndNeverTheSeries is this phase's
// central acceptance criterion, machine-verified end to end through run():
// the eligible season really is unmonitored in the fake's own state, its
// episodes with it, and the series-level monitored flag — plus every other
// season — is exactly as it was.
func TestRun_SonarrWriteMode_UnmonitorsTheSeasonAndNeverTheSeries(t *testing.T) {
	fake := writableSonarrFake(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", writeSonarrTestConfig(t, fake.srv.URL, false, "info"), "--once"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()

	if !fake.seriesMonitored(1) {
		t.Error("the SERIES monitored flag was changed; unmonitoring only ever happens to a season")
	}
	if fake.seasonMonitored(1, 1) {
		t.Errorf("season 1 is still monitored; it should have been unmonitored:\n%s", out)
	}
	if !fake.seasonMonitored(1, 2) {
		t.Error("season 2 (still airing) was unmonitored; the write must touch only the season it names")
	}
	if fake.episodeMonitored(100) {
		t.Error("season 1's episode is still monitored; a season flipped without its episodes leaves episodes that can still grab")
	}
	if !fake.episodeMonitored(200) {
		t.Error("season 2's episode was unmonitored; the episode write must name only the target season's episodes")
	}

	writes := fake.writes()
	if len(writes) != 2 {
		t.Fatalf("expected exactly 2 write requests for one season, got %d: %+v", len(writes), writes)
	}
	if writes[0].path != "/api/v3/episode/monitor" || writes[1].path != "/api/v3/series/1" {
		t.Errorf("write order = %s then %s, want the episode call first", writes[0].path, writes[1].path)
	}

	// The byte-preservation mandate, at run() level: the unknown field the
	// fake's detail body carries must come back untouched, and the escapable
	// characters inside the fake's SEASON objects must not be rewritten by the
	// second encoder the seasons array goes through.
	if !strings.Contains(string(writes[1].body), statefulSonarrExtraField) {
		t.Errorf("the season PUT dropped or rewrote a field this codebase knows nothing about:\n%s", writes[1].body)
	}
	assertNoHTMLEscapes(t, "the season PUT", writes[1].body)
	if !strings.Contains(out, "unmonitored=1") {
		t.Errorf("expected unmonitored=1 in the summary (SEASONS, the decision unit):\n%s", out)
	}
}

// twoWritableSeasonsSonarrFake is writableSonarrFake's multi-season twin: ONE
// series with TWO fully eligible seasons (aired, complete, above cutoff) plus a
// third that is still airing. It exists because the per-season write unit means
// each season write is a full-object PUT built from its OWN fresh GET, so the
// second write must carry the first one's result rather than the payload the
// run started with — a property no static fake can test, and the exact shape
// binding resolution 4 describes ("--only-id N names series N, all its eligible
// seasons in one run").
func twoWritableSeasonsSonarrFake(t *testing.T) *statefulSonarrFake {
	t.Helper()
	return newStatefulSonarrFake(t,
		[]*statefulSonarrSeries{
			{id: 1, title: "Write Both", monitored: true, profileID: 1, tags: []int{},
				seasons: []statefulSonarrSeason{
					{number: 1, monitored: true, episodeFileCount: 1, totalEpisodeCount: 1},
					{number: 2, monitored: true, episodeFileCount: 2, totalEpisodeCount: 2},
					{number: 3, monitored: true, episodeFileCount: 1, totalEpisodeCount: 1},
				}},
		},
		[]*statefulSonarrEpisode{
			{id: 100, seriesID: 1, seasonNumber: 1, episodeNumber: 1, monitored: true, hasFile: true, airDateUtc: pastAirDate, episodeFileID: 500},
			{id: 200, seriesID: 1, seasonNumber: 2, episodeNumber: 1, monitored: true, hasFile: true, airDateUtc: pastAirDate, episodeFileID: 600},
			{id: 201, seriesID: 1, seasonNumber: 2, episodeNumber: 2, monitored: true, hasFile: true, airDateUtc: pastAirDate, episodeFileID: 601},
			{id: 300, seriesID: 1, seasonNumber: 3, episodeNumber: 1, monitored: true, hasFile: true, airDateUtc: futureAirDate, episodeFileID: 700},
		},
		[]*statefulSonarrEpisodeFile{
			{id: 500, seasonNumber: 1, customFormatScore: 200, qualityCutoffNotMet: false},
			{id: 600, seasonNumber: 2, customFormatScore: 200, qualityCutoffNotMet: false},
			{id: 601, seasonNumber: 2, customFormatScore: 200, qualityCutoffNotMet: false},
			{id: 700, seasonNumber: 3, customFormatScore: 200, qualityCutoffNotMet: false},
		},
	)
}

// TestRun_SonarrWriteMode_TwoSeasonsOfOneSeries_NeitherWriteRevertsTheOther is
// the central correctness consequence of choosing the SEASON as the write unit:
// writing season 2 means PUTting the whole series object back, so if that
// payload came from anywhere but a fresh GET taken AFTER season 1's write, it
// would carry season 1 as still monitored and silently re-monitor the season
// this project had just unmonitored.
//
// The only prior multi-season coverage drove unmonitorSeason twice against a
// STATIC fake whose GET never reflected the first PUT — it asserted the second
// body carried season1=true, which reads like proof of the property while being
// incapable of catching a stale-payload or cached-fetch regression. This one
// runs the real pipeline against MUTATING state.
func TestRun_SonarrWriteMode_TwoSeasonsOfOneSeries_NeitherWriteRevertsTheOther(t *testing.T) {
	fake := twoWritableSeasonsSonarrFake(t)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--config", writeSonarrTestConfig(t, fake.srv.URL, false, "info"), "--once"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()

	// The live state after the run: both eligible seasons unmonitored, the
	// airing one and the series itself untouched.
	if !fake.seriesMonitored(1) {
		t.Error("the SERIES monitored flag was changed; unmonitoring only ever happens to a season")
	}
	for _, season := range []int{1, 2} {
		if fake.seasonMonitored(1, season) {
			t.Errorf("season %d is still monitored after the run; both eligible seasons of one series must be written in a single run:\n%s", season, out)
		}
	}
	if !fake.seasonMonitored(1, 3) {
		t.Error("season 3 (still airing) was unmonitored")
	}
	for _, ep := range []int{100, 200, 201} {
		if fake.episodeMonitored(ep) {
			t.Errorf("episode %d is still monitored; each season's episodes go with it", ep)
		}
	}
	if fake.episodeMonitored(300) == false {
		t.Error("season 3's episode was unmonitored; the episode write must name only the target season's episodes")
	}

	writes := fake.writes()
	if len(writes) != 4 {
		t.Fatalf("expected exactly 4 writes (episode call + series PUT, per season), got %d: %+v", len(writes), writes)
	}
	wantPaths := []string{episodeMonitorPath, "/api/v3/series/1", episodeMonitorPath, "/api/v3/series/1"}
	for i, want := range wantPaths {
		if writes[i].path != want {
			t.Errorf("write %d went to %s, want %s (episodes first, per season, one season at a time)", i, writes[i].path, want)
		}
	}

	// The two episode calls each name only their own season's episodes.
	for i, want := range [][]int{{100}, {200, 201}} {
		var req struct {
			EpisodeIDs []int `json:"episodeIds"`
			Monitored  bool  `json:"monitored"`
		}
		if err := json.Unmarshal(writes[i*2].body, &req); err != nil {
			t.Fatalf("episode call %d body is not the expected shape: %v", i, err)
		}
		if fmt.Sprint(req.EpisodeIDs) != fmt.Sprint(want) || req.Monitored {
			t.Errorf("episode call %d = %v monitored=%t, want %v monitored=false", i, req.EpisodeIDs, req.Monitored, want)
		}
	}

	// The property this test exists for: each series PUT preserves the OTHER
	// season's value as of that moment. The first carries season 2 still
	// monitored; the SECOND must carry season 1 already unmonitored, which is
	// only possible if it was assembled from a fresh GET taken after the first
	// write landed.
	for i, want := range []map[int]bool{
		{1: false, 2: true, 3: true},
		{1: false, 2: false, 3: true},
	} {
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(writes[i*2+1].body, &payload); err != nil {
			t.Fatalf("series PUT %d body is not a JSON object: %v", i, err)
		}
		var seasons []struct {
			SeasonNumber int  `json:"seasonNumber"`
			Monitored    bool `json:"monitored"`
		}
		if err := json.Unmarshal(payload["seasons"], &seasons); err != nil {
			t.Fatalf("series PUT %d seasons is not an array: %v", i, err)
		}
		got := map[int]bool{}
		for _, s := range seasons {
			got[s.SeasonNumber] = s.Monitored
		}
		for number, wantMonitored := range want {
			if got[number] != wantMonitored {
				t.Errorf("series PUT %d: season %d monitored = %t, want %t — a season write must never revert a season this run already wrote:\n%s",
					i, number, got[number], wantMonitored, writes[i*2+1].body)
			}
		}
		var seriesMonitored bool
		if err := json.Unmarshal(payload["monitored"], &seriesMonitored); err != nil || !seriesMonitored {
			t.Errorf("series PUT %d changed the series-level monitored flag: %s", i, writes[i*2+1].body)
		}
		if !strings.Contains(string(writes[i*2+1].body), statefulSonarrExtraField) {
			t.Errorf("series PUT %d dropped a field this codebase knows nothing about: %s", i, writes[i*2+1].body)
		}
		assertNoHTMLEscapes(t, fmt.Sprintf("series PUT %d", i), writes[i*2+1].body)
	}

	if !strings.Contains(out, "unmonitored=2") {
		t.Errorf("expected unmonitored=2 in the summary (SEASONS, the decision unit):\n%s", out)
	}
}

// TestRun_SonarrWriteMode_SecondRun_IsANoOp is the Sonarr half of Phase 5's
// no-op contract: run 1 writes, run 2 — against the SAME fake, now reflecting
// run 1's writes — makes ZERO write requests of any method to any path,
// reports the season as already unmonitored, and is free of WARN and ERROR,
// because a daemon loop repeats this cycle forever.
func TestRun_SonarrWriteMode_SecondRun_IsANoOp(t *testing.T) {
	fake := writableSonarrFake(t)
	configPath := writeSonarrTestConfig(t, fake.srv.URL, false, "debug")

	var stdout1, stderr1 bytes.Buffer
	if code := run([]string{"--config", configPath, "--once"}, &stdout1, &stderr1); code != 0 {
		t.Fatalf("run 1: exit code = %d, want 0; stderr=%s", code, stderr1.String())
	}
	writes1 := len(fake.writes())
	if writes1 != 2 {
		t.Fatalf("run 1: expected 2 writes, got %d: %+v", writes1, fake.writes())
	}
	if !strings.Contains(stdout1.String(), "unmonitored=1") {
		t.Fatalf("run 1: expected unmonitored=1:\n%s", stdout1.String())
	}

	var stdout2, stderr2 bytes.Buffer
	if code := run([]string{"--config", configPath, "--once"}, &stdout2, &stderr2); code != 0 {
		t.Fatalf("run 2: exit code = %d, want 0; stderr=%s", code, stderr2.String())
	}
	if writes2 := fake.writes(); len(writes2) != writes1 {
		t.Fatalf("run 2: made %d additional write request(s) of any method to any path, want ZERO: %+v", len(writes2)-writes1, writes2[writes1:])
	}

	out2 := stdout2.String()
	if strings.Contains(out2, "level=WARN") || strings.Contains(out2, "level=ERROR") {
		t.Errorf("run 2: a no-op cycle must produce no WARN or ERROR at all:\n%s", out2)
	}
	if strings.Contains(out2, "msg=would-unmonitor") {
		t.Errorf("run 2: a no-op cycle must produce no would-unmonitor lines:\n%s", out2)
	}
	for _, want := range []string{"wouldUnmonitor=0", "unmonitored=0", "alreadyUnmonitored=1"} {
		if !strings.Contains(out2, want) {
			t.Errorf("run 2: expected %s in the summary:\n%s", want, out2)
		}
	}
	if !strings.Contains(out2, `msg="season already unmonitored"`) {
		t.Errorf("run 2: the season must be visible at debug and nowhere above it:\n%s", out2)
	}
}

// TestRun_SonarrWriteMode_PartialFailure_ConvergesOnTheNextRun is controller
// resolution 1's whole justification, machine-verified: the episode call
// lands, the season PUT is rejected, and the season is therefore left
// MONITORED — which is exactly why the next cycle re-evaluates it and
// finishes the job. The retry is also smaller, not duplicated: the episodes
// already unmonitored are excluded from the second attempt's id list, so run
// 2 sends the season PUT alone.
//
// It is also the recovery path's end-to-end proof, and the reason that path
// has a gate of its own. Run 2's cross-check is INCONCLUSIVE by construction:
// the season's only episode is already unmonitored, so cross-check shape (a)
// excludes it and nothing comparable is left to verify. The ordinary gate can
// therefore never authorize this retry — under it alone, a single 500 on the
// season half would leave the season half-written forever — and the write that
// converges is counted as recoveredWrites, never as unmonitored.
func TestRun_SonarrWriteMode_PartialFailure_ConvergesOnTheNextRun(t *testing.T) {
	fake := writableSonarrFake(t)
	fake.series[1].seasons = fake.series[1].seasons[:1]
	delete(fake.episodes, 200)
	fake.episodeOrder = []int{100}
	fake.seriesPutStatus[1] = http.StatusInternalServerError
	configPath := writeSonarrTestConfig(t, fake.srv.URL, false, "info")

	var stdout1, stderr1 bytes.Buffer
	if code := run([]string{"--config", configPath, "--once"}, &stdout1, &stderr1); code != 0 {
		t.Fatalf("run 1: exit code = %d, want 0 (a per-instance write failure is not a process failure); stderr=%s", code, stderr1.String())
	}
	out1 := stdout1.String()
	if !strings.Contains(out1, "writeErrors=1") || !strings.Contains(out1, "unmonitored=0") {
		t.Errorf("run 1: a partial completion is a write error, not a write:\n%s", out1)
	}
	if fake.episodeMonitored(100) {
		t.Error("run 1: the episode call landed, so the episode must be unmonitored in the fake's state")
	}
	if !fake.seasonMonitored(1, 1) {
		t.Fatal("run 1: the season PUT was rejected, so the season must still be monitored — that is what makes the next cycle converge")
	}

	// The server recovers.
	delete(fake.seriesPutStatus, 1)
	writes1 := len(fake.writes())

	var stdout2, stderr2 bytes.Buffer
	if code := run([]string{"--config", configPath, "--once"}, &stdout2, &stderr2); code != 0 {
		t.Fatalf("run 2: exit code = %d, want 0; stderr=%s", code, stderr2.String())
	}
	out2 := stdout2.String()
	c2 := sonarrSummaryCounters(t, out2)
	if c2["recoveredWrites"] != 1 || c2["unmonitored"] != 0 {
		t.Errorf("run 2: recoveredWrites/unmonitored = %d/%d, want 1/0 — convergence is a recovery, and a recovery is never counted as an ordinary unmonitor:\n%s",
			c2["recoveredWrites"], c2["unmonitored"], out2)
	}
	if c2["recoveredWrites"]+c2["unmonitored"]+c2["writeErrors"]+c2["writeEchoUnverified"]+c2["writesRefused"]+c2["withheldWrites"] != c2["wouldUnmonitor"] {
		t.Errorf("run 2: the accounting identity must hold:\n%s", out2)
	}
	if !strings.Contains(out2, "crossCheck=inconclusive") {
		t.Errorf("run 2's cross-check is inconclusive by construction; if it is not, this test is no longer proving the recovery path converges:\n%s", out2)
	}
	recoveryLine := logLineContaining(t, out2, "completing a previously partial season unmonitor")
	if !strings.Contains(recoveryLine, "season=1") || !strings.Contains(recoveryLine, "gateBlocked=") {
		t.Errorf("run 2: the recovery must name its season and the authorization it did not have:\n%s", recoveryLine)
	}
	if fake.seasonMonitored(1, 1) {
		t.Error("run 2: the season should now be unmonitored")
	}
	run2Writes := fake.writes()[writes1:]
	if len(run2Writes) != 1 || run2Writes[0].path != "/api/v3/series/1" {
		t.Errorf("run 2 must send the season PUT alone — every episode is already unmonitored, so the episode call is not needed: %+v", run2Writes)
	}
}

// TestRun_MixedRadarrAndSonarr_BothReportAndWriteIndependently is the
// carried-forward coexistence pin: every prior end-to-end test drove a config
// containing exactly one instance TYPE, so nothing proved the two engines'
// report formats and summaries coexist in one pass — one summary line each,
// each carrying its own counters, each instance written on its own terms.
//
// This is also the shape §2.6's "skip that instance for the cycle" rule is
// about, and the shape Phase 8's daemon loop will repeat forever.
func TestRun_MixedRadarrAndSonarr_BothReportAndWriteIndependently(t *testing.T) {
	radarr := newStatefulRadarrFake(t, []*statefulRadarrMovie{
		wouldUnmonitorStatefulMovie(1, "Mixed Config Movie"),
	})
	sonarr := writableSonarrFake(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := fmt.Sprintf(`
dry_run: false
instances:
  - name: radarr-main
    type: radarr
    url: %s
    api_key: key1
  - name: sonarr-main
    type: sonarr
    url: %s
    api_key: key2
`, radarr.srv.URL, sonarr.srv.URL)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--config", path, "--once"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()

	radarrSummaries, sonarrSummaries := 0, 0
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "radarr decision summary") {
			radarrSummaries++
			if !strings.Contains(line, "instance=radarr-main") || !strings.Contains(line, "unmonitored=1") {
				t.Errorf("the radarr summary must carry its own instance and counters: %s", line)
			}
			if strings.Contains(line, "totalSeriesMonitored") || strings.Contains(line, "seasonsEvaluated") {
				t.Errorf("the radarr summary must not borrow the sonarr summary's attrs: %s", line)
			}
		}
		if strings.Contains(line, "sonarr decision summary") {
			sonarrSummaries++
			if !strings.Contains(line, "instance=sonarr-main") || !strings.Contains(line, "unmonitored=1") {
				t.Errorf("the sonarr summary must carry its own instance and counters: %s", line)
			}
			if strings.Contains(line, "totalMonitored=") {
				t.Errorf("the sonarr summary must not borrow the radarr summary's attrs: %s", line)
			}
			if !strings.Contains(line, "totalSeriesMonitored=") || !strings.Contains(line, "seasonsEvaluated=") {
				t.Errorf("the sonarr summary must carry its own season-granularity attrs: %s", line)
			}
		}
	}
	if radarrSummaries != 1 || sonarrSummaries != 1 {
		t.Fatalf("expected exactly one summary line per engine, got radarr=%d sonarr=%d:\n%s", radarrSummaries, sonarrSummaries, out)
	}

	// Both report formats appear, and each engine's report lines carry the
	// attrs of its own decision unit.
	movieLine := reportLineWithMsg(t, out, "unmonitor")
	if !strings.Contains(movieLine, "id=1") && !strings.Contains(movieLine, "seriesId=1") {
		t.Errorf("expected a per-item write line: %s", movieLine)
	}
	if n := len(radarr.puts()); n != 1 {
		t.Errorf("the radarr must make exactly its own 1 PUT, got %d", n)
	}
	if n := len(sonarr.writes()); n != 2 {
		t.Errorf("the sonarr must make exactly its own 2 writes, got %d: %+v", n, sonarr.writes())
	}
	if !sonarr.seriesMonitored(1) {
		t.Error("the sonarr's series-level monitored flag was changed")
	}
}

// TestUnmonitorSeason_FreshStatisticsClaimNoEpisodes_Refuses closes the
// vacuous-guard corner: a season whose fresh statistics claim zero episodes
// makes the airing loop run over nothing and "pass" by having nothing to
// check. Rule 2 requires totalEpisodeCount > 0 at decision time for exactly
// that reason, and the fresh payload has to clear the same bar.
func TestUnmonitorSeason_FreshStatisticsClaimNoEpisodes_Refuses(t *testing.T) {
	seriesJSON := mustReplace(t, sonarrWriterSeriesJSON, sonarrWriterSeason1JSON,
		`{"seasonNumber": 1, "monitored": true, "statistics": {"episodeFileCount": 0, "totalEpisodeCount": 0}}`)
	fake := newSonarrWriterFake(t, seriesJSON, `[]`)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	written, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false, false)
	if written {
		t.Error("written = true, want false")
	}
	if !isWriteRefusal(err) {
		t.Fatalf("err = %v, want a counted write refusal", err)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("nothing may be written: %+v", writes)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning:\n%s", buf.String())
	}
}

// TestAssembleSeasonWrite_RefusesToEmitAChangedSeriesLevelMonitored is the
// binding "a test must prove no sonarr write pass can emit a payload whose
// series-level monitored differs from the fresh GET" mandate, aimed at the
// guard rather than at one happy path.
//
// The scenario is the one that would actually cause it: the target season
// object turning out to BE the series object (an aliasing or lookup bug), so
// the single mutation lands on the series' own monitored flag and unmonitors
// the entire show. The assembly must refuse to produce a payload at all, so
// such a bug fails at the moment of assembly instead of at Sonarr.
func TestAssembleSeasonWrite_RefusesToEmitAChangedSeriesLevelMonitored(t *testing.T) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(sonarrWriterSeriesJSON), &payload); err != nil {
		t.Fatalf("fixture is not a JSON object: %v", err)
	}
	var seasons []json.RawMessage
	if err := json.Unmarshal(payload["seasons"], &seasons); err != nil {
		t.Fatalf("fixture seasons is not an array: %v", err)
	}

	// payload is handed in as its own "target season": the mutation therefore
	// lands on the series-level monitored flag.
	encoded, err := assembleSeasonWrite(payload, seasons, 0, payload, "series 3 season 1")
	if err == nil {
		t.Fatalf("assembleSeasonWrite produced a payload that changes the series-level monitored flag:\n%s", encoded)
	}
	if !strings.Contains(err.Error(), "monitored") || !strings.Contains(err.Error(), "season") {
		t.Errorf("the refusal must say what it refused and why, got: %v", err)
	}
	if encoded != nil {
		t.Errorf("no payload may be returned alongside the refusal: %s", encoded)
	}
}

// TestUnmonitorSeason_AcrossPayloadShapes_SeriesLevelMonitoredIsAlwaysTheFetchedValue
// is the same mandate stated as a property over the write pass itself: across
// every payload shape and target season this suite can produce, EVERY series
// PUT body carries exactly the series-level monitored value the fresh GET
// returned. A single happy-path assertion proves one write; this proves the
// invariant the phase actually promises.
func TestUnmonitorSeason_AcrossPayloadShapes_SeriesLevelMonitoredIsAlwaysTheFetchedValue(t *testing.T) {
	cases := []struct {
		name       string
		seriesJSON string
		season     int
		tagActive  bool
	}{
		{"target is the first season", sonarrWriterSeriesJSON, 1, false},
		{"target is the last season", sonarrWriterSeriesJSON, 2, false},
		{"exclusion tag active but absent from the series", sonarrWriterSeriesJSON, 1, true},
		{"series carries no unknown fields at all", `{"id":3,"title":"Plain","monitored":true,"tags":[],"seasons":[{"seasonNumber":1,"monitored":true,"statistics":{"episodeFileCount":2,"totalEpisodeCount":2}}]}`, 1, false},
		{"seasons in descending order", `{"id":3,"title":"Descending","monitored":true,"tags":[],"seasons":[{"seasonNumber":2,"monitored":true,"statistics":{"episodeFileCount":1,"totalEpisodeCount":1}},{"seasonNumber":1,"monitored":true,"statistics":{"episodeFileCount":2,"totalEpisodeCount":2}}]}`, 1, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newSonarrWriterFake(t, tc.seriesJSON, sonarrWriterEpisodesJSON)
			logger, _ := newDecisionTestLogger(slog.LevelInfo)

			if _, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, tc.season, 42, tc.tagActive, false, false); err != nil {
				t.Fatalf("unmonitorSeason returned error = %v", err)
			}

			var fetched map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tc.seriesJSON), &fetched); err != nil {
				t.Fatalf("fixture is not a JSON object: %v", err)
			}
			puts := 0
			for _, w := range fake.writes() {
				if w.path != "/api/v3/series/3" {
					continue
				}
				puts++
				var sent map[string]json.RawMessage
				if err := json.Unmarshal(w.body, &sent); err != nil {
					t.Fatalf("PUT body is not a JSON object: %v", err)
				}
				if !jsonBytesEqual(t, sent["monitored"], fetched["monitored"]) {
					t.Errorf("series-level monitored = %s, want the fetched %s", sent["monitored"], fetched["monitored"])
				}
			}
			if puts != 1 {
				t.Fatalf("expected exactly 1 season PUT, got %d", puts)
			}
		})
	}
}

// TestUnmonitorSeason_EpisodeWithUnreadableStateAtWriteTime_Refuses closes the
// stranding hole the write ORDER exists to prevent, on the one path that
// could still reach it: an episode of the target season whose own id or
// monitored value cannot be read is excluded from the episode-monitor call by
// construction (it cannot be named, or its state is unknown) — and if the
// season PUT then went out anyway, that episode would be left monitored inside
// a season rule 1 excludes from every future cycle. Nothing would ever revisit
// it, which is exactly the stranded state the binding episodes-first order was
// chosen to avoid. Untrusted input on a load-bearing field is a refusal here,
// as it is everywhere else in this write path.
func TestUnmonitorSeason_EpisodeWithUnreadableStateAtWriteTime_Refuses(t *testing.T) {
	cases := []struct {
		name     string
		episodes string
	}{
		{"episode has no id", `[
			{"id": 100, "seriesId": 3, "seasonNumber": 1, "episodeNumber": 1, "monitored": true, "hasFile": true, "airDateUtc": "2015-01-01T00:00:00Z", "episodeFileId": 500},
			{"seriesId": 3, "seasonNumber": 1, "episodeNumber": 2, "monitored": true, "hasFile": true, "airDateUtc": "2015-01-02T00:00:00Z", "episodeFileId": 501}
		]`},
		{"episode monitored is JSON null", `[
			{"id": 100, "seriesId": 3, "seasonNumber": 1, "episodeNumber": 1, "monitored": true, "hasFile": true, "airDateUtc": "2015-01-01T00:00:00Z", "episodeFileId": 500},
			{"id": 101, "seriesId": 3, "seasonNumber": 1, "episodeNumber": 2, "monitored": null, "hasFile": true, "airDateUtc": "2015-01-02T00:00:00Z", "episodeFileId": 501}
		]`},
		{"episode monitored is absent", `[
			{"id": 100, "seriesId": 3, "seasonNumber": 1, "episodeNumber": 1, "monitored": true, "hasFile": true, "airDateUtc": "2015-01-01T00:00:00Z", "episodeFileId": 500},
			{"id": 101, "seriesId": 3, "seasonNumber": 1, "episodeNumber": 2, "hasFile": true, "airDateUtc": "2015-01-02T00:00:00Z", "episodeFileId": 501}
		]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newSonarrWriterFake(t, sonarrWriterSeriesJSON, tc.episodes)
			logger, buf := newDecisionTestLogger(slog.LevelInfo)

			written, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false, false)
			if written {
				t.Error("written = true, want false")
			}
			if !isWriteRefusal(err) {
				t.Fatalf("err = %v, want a counted write refusal", err)
			}
			if writes := fake.writes(); len(writes) != 0 {
				t.Errorf("neither half may be written when an episode's state is unreadable: %+v", writes)
			}
			if !strings.Contains(buf.String(), "level=WARN") {
				t.Errorf("expected a warning:\n%s", buf.String())
			}
		})
	}
}

// TestUnmonitorSeason_SeasonNoLongerCompleteOnDisk_Refuses re-runs rule 2's
// own condition against the fresh payload. "This season is finished, stop
// monitoring it" rests on the season being complete on disk; if a file was
// removed between the scan and the write, that premise is gone, and
// unmonitoring the season would strand the missing episode forever — rule 1
// excludes an unmonitored season from every future cycle, so nothing would
// ever notice the gap. The value is already decoded from the same statistics
// object the completeness check reads.
func TestUnmonitorSeason_SeasonNoLongerCompleteOnDisk_Refuses(t *testing.T) {
	seriesJSON := mustReplace(t, sonarrWriterSeriesJSON, sonarrWriterSeason1JSON,
		`{"seasonNumber": 1, "monitored": true, "statistics": {"episodeFileCount": 1, "totalEpisodeCount": 2}}`)
	fake := newSonarrWriterFake(t, seriesJSON, sonarrWriterEpisodesJSON)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	written, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false, false)
	if written {
		t.Error("written = true, want false")
	}
	if !isWriteRefusal(err) {
		t.Fatalf("err = %v, want a counted write refusal", err)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("a season that is no longer complete on disk must not be written: %+v", writes)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning:\n%s", buf.String())
	}
}
