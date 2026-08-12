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
const sonarrWriterSeriesJSON = `{
	"id": 3,
	"title": "Mr. & Mrs. Smith <Special Edition>",
	"monitored": true,
	"qualityProfileId": 1,
	"tags": [3, 9],
	"sizeOnDisk": 9876543210123,
	"overview": "Cheech & Chong: 4 < 5 > 3",
	"seasons": [
		{"seasonNumber": 1, "monitored": true, "statistics": {"episodeFileCount": 2, "totalEpisodeCount": 2, "sizeOnDisk": 9876543210123}},
		{"seasonNumber": 2, "monitored": true, "statistics": {"episodeFileCount": 1, "totalEpisodeCount": 1}}
	],
	"someFutureField": {"nested": ["a", "b"], "flag": true}
}`

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

	written, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, true)
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

	written, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false)
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

	if _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false); err != nil {
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

// TestUnmonitorSeason_WriteMode_SendsUnescapedBytesForHTMLSensitiveCharacters
// mirrors the Radarr twin: json.Marshal rewrites "&", "<" and ">" into
// six-character unicode escapes inside every string it encodes — including
// inside a json.RawMessage, which it re-scans rather than copying — so a
// series titled "Mr. & Mrs. Smith" would be PUT back with bytes Sonarr never
// sent.
func TestUnmonitorSeason_WriteMode_SendsUnescapedBytesForHTMLSensitiveCharacters(t *testing.T) {
	fake := newSonarrWriterFake(t, sonarrWriterSeriesJSON, sonarrWriterEpisodesJSON)
	logger, _ := newDecisionTestLogger(slog.LevelInfo)

	if _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false); err != nil {
		t.Fatalf("unmonitorSeason returned error = %v", err)
	}
	for _, w := range fake.writes() {
		if w.path != "/api/v3/series/3" {
			continue
		}
		body := string(w.body)
		// The escape sequences are built rather than written out, so the
		// assertion cannot be defeated by the test source itself being edited
		// into the very characters it is looking for (same construction as the
		// Radarr twin).
		for _, r := range []rune{'&', '<', '>'} {
			escaped := fmt.Sprintf("\\u%04x", r)
			if strings.Contains(body, escaped) {
				t.Errorf("PUT body contains the HTML-escaped sequence %s for %q; the fetched bytes must go back unmodified:\n%s", escaped, r, body)
			}
		}
		for _, literal := range []string{"Mr. & Mrs. Smith <Special Edition>", "Cheech & Chong: 4 < 5 > 3"} {
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

	written, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false)
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

	written, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false)
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

	written, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false)
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

	written, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false)
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

	written, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false)
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

// TestUnmonitorSeason_EpisodeEchoUnverifiable_SeasonWriteStillProceeds is the
// other half of the same rule, on the other call: an unverifiable episode
// echo must NOT abort the series write (that would strand the episodes it
// probably already changed), but written=true requires BOTH calls confirmed,
// so the season is reported as accepted-but-unconfirmed and the next cycle
// reconciles it.
func TestUnmonitorSeason_EpisodeEchoUnverifiable_SeasonWriteStillProceeds(t *testing.T) {
	fake := newSonarrWriterFake(t, sonarrWriterSeriesJSON, sonarrWriterEpisodesJSON)
	empty := ""
	fake.episodeMonitorEcho = &empty
	logger, _ := newDecisionTestLogger(slog.LevelInfo)

	written, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false)
	if written {
		t.Error("written = true, want false: a confirmed write requires BOTH calls confirmed")
	}
	if !errors.Is(err, errWriteUnverified) {
		t.Fatalf("err = %v, want an errWriteUnverified-wrapped error", err)
	}
	sawSeriesPut := false
	for _, w := range fake.writes() {
		if w.path == "/api/v3/series/3" {
			sawSeriesPut = true
		}
	}
	if !sawSeriesPut {
		t.Error("the series PUT must still be sent: aborting here would strand episodes the write probably already changed")
	}
}

// --- pre-write re-verification refusals ------------------------------------

func TestUnmonitorSeason_FreshPayloadCarriesExclusionTag_Refuses(t *testing.T) {
	seriesJSON := strings.Replace(sonarrWriterSeriesJSON, `"tags": [3, 9],`, `"tags": [3, 42, 9],`, 1)
	fake := newSonarrWriterFake(t, seriesJSON, sonarrWriterEpisodesJSON)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	written, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 42, true, false)
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
		{"tags absent", strings.Replace(sonarrWriterSeriesJSON, `"tags": [3, 9],`, ``, 1)},
		{"tags JSON null", strings.Replace(sonarrWriterSeriesJSON, `"tags": [3, 9],`, `"tags": null,`, 1)},
		{"tags not an array", strings.Replace(sonarrWriterSeriesJSON, `"tags": [3, 9],`, `"tags": "3,9",`, 1)},
		{"tags contain a null element", strings.Replace(sonarrWriterSeriesJSON, `"tags": [3, 9],`, `"tags": [3, null, 9],`, 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newSonarrWriterFake(t, tc.seriesJSON, sonarrWriterEpisodesJSON)
			logger, buf := newDecisionTestLogger(slog.LevelInfo)

			written, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 42, true, false)
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

// TestUnmonitorSeason_SeriesNoLongerMonitored_RefusesAsARace: the series was
// unmonitored between the scan and the write pass. Unmonitoring one of its
// seasons is no longer meaningful, and — more importantly — the season's own
// state can no longer be read as evidence of anything the decision was based
// on.
func TestUnmonitorSeason_SeriesNoLongerMonitored_RefusesAsARace(t *testing.T) {
	seriesJSON := strings.Replace(sonarrWriterSeriesJSON, `"monitored": true,
	"qualityProfileId"`, `"monitored": false,
	"qualityProfileId"`, 1)
	fake := newSonarrWriterFake(t, seriesJSON, sonarrWriterEpisodesJSON)
	logger, _ := newDecisionTestLogger(slog.LevelInfo)

	written, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false)
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
	seriesJSON := strings.Replace(sonarrWriterSeriesJSON, `{"seasonNumber": 1, "monitored": true,`, `{"seasonNumber": 1, "monitored": false,`, 1)
	fake := newSonarrWriterFake(t, seriesJSON, sonarrWriterEpisodesJSON)
	logger, _ := newDecisionTestLogger(slog.LevelInfo)

	written, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false)
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
			seriesJSON := strings.Replace(sonarrWriterSeriesJSON,
				`{"seasonNumber": 1, "monitored": true, "statistics": {"episodeFileCount": 2, "totalEpisodeCount": 2, "sizeOnDisk": 9876543210123}}`,
				tc.season, 1)
			fake := newSonarrWriterFake(t, seriesJSON, sonarrWriterEpisodesJSON)
			logger, buf := newDecisionTestLogger(slog.LevelInfo)

			written, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false)
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
		{"season vanished", strings.Replace(sonarrWriterSeriesJSON,
			`{"seasonNumber": 1, "monitored": true, "statistics": {"episodeFileCount": 2, "totalEpisodeCount": 2, "sizeOnDisk": 9876543210123}},`, ``, 1)},
		{"season duplicated", strings.Replace(sonarrWriterSeriesJSON,
			`{"seasonNumber": 1, "monitored": true, "statistics": {"episodeFileCount": 2, "totalEpisodeCount": 2, "sizeOnDisk": 9876543210123}},`,
			`{"seasonNumber": 1, "monitored": true, "statistics": {"episodeFileCount": 2, "totalEpisodeCount": 2}},{"seasonNumber": 1, "monitored": true, "statistics": {"episodeFileCount": 2, "totalEpisodeCount": 2}},`, 1)},
		{"seasons key absent", strings.Replace(sonarrWriterSeriesJSON,
			`"seasons": [
		{"seasonNumber": 1, "monitored": true, "statistics": {"episodeFileCount": 2, "totalEpisodeCount": 2, "sizeOnDisk": 9876543210123}},
		{"seasonNumber": 2, "monitored": true, "statistics": {"episodeFileCount": 1, "totalEpisodeCount": 1}}
	],`, ``, 1)},
		{"seasons JSON null", strings.Replace(sonarrWriterSeriesJSON,
			`"seasons": [
		{"seasonNumber": 1, "monitored": true, "statistics": {"episodeFileCount": 2, "totalEpisodeCount": 2, "sizeOnDisk": 9876543210123}},
		{"seasonNumber": 2, "monitored": true, "statistics": {"episodeFileCount": 1, "totalEpisodeCount": 1}}
	],`, `"seasons": null,`, 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newSonarrWriterFake(t, tc.seriesJSON, sonarrWriterEpisodesJSON)
			logger, buf := newDecisionTestLogger(slog.LevelInfo)

			written, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false)
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

	written, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false)
	if written {
		t.Error("written = true, want false")
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

	written, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false)
	if written {
		t.Error("written = true, want false")
	}
	if !isWriteRefusal(err) {
		t.Fatalf("err = %v, want a counted write refusal", err)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("nothing may be written: %+v", writes)
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

	written, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false)
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
	seriesJSON := strings.Replace(sonarrWriterSeriesJSON, `"id": 3,`, `"id": 4,`, 1)
	fake := newSonarrWriterFake(t, seriesJSON, sonarrWriterEpisodesJSON)
	logger, _ := newDecisionTestLogger(slog.LevelInfo)

	written, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false)
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

	if _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false); err != nil {
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
		if _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, season, 0, false, false); err != nil {
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
			accounted := c["unmonitored"] + c["writeEchoUnverified"] + c["writeErrors"] + c["writeRehearsalErrors"] + c["writesRefused"] + c["withheldWrites"]
			if accounted != c["wouldUnmonitor"] {
				t.Errorf("the summary accounts for %d of %d promised writes; every would-unmonitor season must end in exactly one counted outcome:\n%s", accounted, c["wouldUnmonitor"], out)
			}
			for _, always := range []string{"unmonitored=", "writesRefused=", "withheldWrites=", "writeErrors="} {
				if !strings.Contains(out, always) {
					t.Errorf("the summary must always carry %s, including as 0:\n%s", always, out)
				}
			}
		})
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
// controller resolution 4: --only-id <seriesId> names a series, and both the
// report and the write pass are scoped to it. Every series is still
// EVALUATED, because the cross-check validates the data the decision rests on
// rather than the target — the same split runRadarrDecisionEngine makes.
func TestRunSonarrDecisionEngine_OnlyID_ScopesReportAndWritesToOneSeries(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "]"
	filesJSON := "[" + episodeFileJSON(500, 1, 200, false) + "]"
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	fake.seriesDetail[1] = sonarrWriteEngineSeriesDetail(1, "Named Show", 1, 1, true)
	fake.seriesDetail[2] = sonarrWriteEngineSeriesDetail(2, "Other Show", 1, 1, true)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	series := []seriesElement{
		testSeries(1, "Named Show", true, 1, []int{}, testSeason(1, true, 1, 1)),
		testSeries(2, "Other Show", true, 1, []int{}, testSeason(1, true, 1, 1)),
	}
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude", 2, false)

	out := buf.String()
	for _, w := range fake.writes() {
		if w.path == "/api/v3/series/1" {
			t.Errorf("a --only-id run must never write a series it does not name: %+v", w)
		}
	}
	sawNamed := false
	for _, w := range fake.writes() {
		if w.path == "/api/v3/series/2" {
			sawNamed = true
		}
	}
	if !sawNamed {
		t.Errorf("expected the named series to be written, got %+v", fake.writes())
	}
	c := sonarrSummaryCounters(t, out)
	if c["wouldUnmonitor"] != 1 || c["unmonitored"] != 1 {
		t.Errorf("wouldUnmonitor/unmonitored = %d/%d, want 1/1 (the named series only):\n%s", c["wouldUnmonitor"], c["unmonitored"], out)
	}
	if !strings.Contains(out, "onlyId=2") {
		t.Errorf("the summary must say the run was scoped:\n%s", out)
	}
	// Report lines only. The cross-check's own lines DO name the unnamed
	// series, and correctly so: it samples the whole library because it
	// validates the data the decision rests on, not the target.
	for _, line := range strings.Split(out, "\n") {
		isReport := strings.Contains(line, "msg=would-unmonitor") || strings.Contains(line, "msg=skip") || strings.Contains(line, "msg=unmonitor ")
		if isReport && strings.Contains(line, `series="Named Show"`) {
			t.Errorf("a --only-id run must not REPORT a series it does not name: %q", line)
		}
	}
	if !strings.Contains(out, `msg="cross-check season" instance=sonarr-main seriesId=1`) {
		t.Errorf("the cross-check must still sample the whole library — it validates the data, not the target:\n%s", out)
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
	// fake's detail body carries must come back untouched.
	if !strings.Contains(string(writes[1].body), statefulSonarrExtraField) {
		t.Errorf("the season PUT dropped or rewrote a field this codebase knows nothing about:\n%s", writes[1].body)
	}
	if !strings.Contains(out, "unmonitored=1") {
		t.Errorf("expected unmonitored=1 in the summary (SEASONS, the decision unit):\n%s", out)
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
	if !strings.Contains(out2, "unmonitored=1") {
		t.Errorf("run 2 must converge and finish the write:\n%s", out2)
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
	seriesJSON := strings.Replace(sonarrWriterSeriesJSON,
		`{"seasonNumber": 1, "monitored": true, "statistics": {"episodeFileCount": 2, "totalEpisodeCount": 2, "sizeOnDisk": 9876543210123}}`,
		`{"seasonNumber": 1, "monitored": true, "statistics": {"episodeFileCount": 0, "totalEpisodeCount": 0}}`, 1)
	fake := newSonarrWriterFake(t, seriesJSON, `[]`)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	written, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false)
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

			if _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, tc.season, 42, tc.tagActive, false); err != nil {
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
