package main

import (
	"bytes"
	"context"
	"encoding/json"
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

// recordedRequest captures one request a write-path fake server received,
// so tests can assert on method, path, and exact body bytes.
type recordedRequest struct {
	method string
	path   string
	body   []byte
}

// writerTestServer serves movieJSON at GET /api/v3/movie/{id} and accepts a
// PUT to the same path with putStatus, recording EVERY request it receives
// (of any method, to any path) into the returned slice pointer. Recording
// everything — not just PUTs to the expected path — is what lets the
// dry-run tests assert the total absence of writes rather than merely the
// absence of an expected write.
func writerTestServer(t *testing.T, movieJSON string, putStatus int) (*httptest.Server, *[]recordedRequest) {
	t.Helper()
	var got []recordedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, recordedRequest{method: r.Method, path: r.URL.Path, body: body})

		switch r.Method {
		case http.MethodGet:
			w.Write([]byte(movieJSON))
		case http.MethodPut:
			w.WriteHeader(putStatus)
			if putStatus >= 400 {
				w.Write([]byte(`{"message":"write rejected"}`))
				return
			}
			// Radarr echoes the updated object back; the writer must not
			// depend on that, but the fake behaves like the real thing.
			w.Write(body)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	return srv, &got
}

// writerTestMovieJSON is a movie object shaped like a real Radarr
// /api/v3/movie/{id} response: monitored plus a spread of other fields the
// write path must round-trip untouched, including "someFutureField" — a key
// this codebase has no knowledge of at all, standing in for any field a
// future Radarr version adds. §2.4 ("send the object back otherwise
// unmodified") is only meaningfully tested if such a key is present.
const writerTestMovieJSON = `{
	"id": 7,
	"title": "Round Trip Movie",
	"monitored": true,
	"hasFile": true,
	"qualityProfileId": 1,
	"tags": [3, 9],
	"sizeOnDisk": 9876543210123,
	"ratings": {"imdb": {"votes": 1234, "value": 7.4}},
	"movieFile": {"id": 42, "customFormatScore": 200, "relativePath": "Round Trip Movie.mkv"},
	"someFutureField": {"nested": ["a", "b"], "flag": true}
}`

func writerTestInstance(url string) Instance {
	return Instance{Name: "radarr-main", Type: "radarr", URL: url, APIKey: "key"}
}

// filterRequests returns only the recorded requests whose method is in
// methods.
func filterRequests(got []recordedRequest, methods ...string) []recordedRequest {
	var out []recordedRequest
	for _, r := range got {
		for _, m := range methods {
			if r.method == m {
				out = append(out, r)
				break
			}
		}
	}
	return out
}

// TestUnmonitorMovie_DryRun_MakesNoWriteRequest is one half of the
// project's most important guarantee (§2.1: "In dry-run mode the app
// performs zero write requests — not one"), pinned at the unit level: the
// fresh GET still happens (so dry-run rehearses the entire write path), but
// no PUT — indeed no non-GET request of any kind — ever reaches the server.
func TestUnmonitorMovie_DryRun_MakesNoWriteRequest(t *testing.T) {
	srv, got := writerTestServer(t, writerTestMovieJSON, http.StatusOK)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelDebug)
	inst := writerTestInstance(srv.URL)
	client := NewAPIClient(inst.URL, inst.APIKey)

	written, err := unmonitorMovie(context.Background(), logger, client, inst, 7, true)
	if err != nil {
		t.Fatalf("unmonitorMovie returned error: %v\n%s", err, buf.String())
	}
	if written {
		t.Error("written = true in dry-run, want false: no write may be reported as performed")
	}

	writes := filterRequests(*got, http.MethodPut, http.MethodPost, http.MethodDelete, http.MethodPatch)
	if len(writes) != 0 {
		t.Errorf("dry-run made %d write request(s), want ZERO: %+v", len(writes), writes)
	}
	if len(filterRequests(*got, http.MethodGet)) != 1 {
		t.Errorf("expected exactly one fresh GET before the dry-run gate, got %d: %+v", len(filterRequests(*got, http.MethodGet)), *got)
	}
}

// TestUnmonitorMovie_WriteMode_PutsFullObjectWithOnlyMonitoredChanged is
// §2.4's pin: the PUT body must be the GET body with exactly one key
// changed. Every other key — including "someFutureField", which this
// codebase knows nothing about — must survive byte-for-byte. This is why
// the payload is never built from movieListElement (a tiny decoded subset:
// round-tripping it would silently destroy every field it does not model).
func TestUnmonitorMovie_WriteMode_PutsFullObjectWithOnlyMonitoredChanged(t *testing.T) {
	srv, got := writerTestServer(t, writerTestMovieJSON, http.StatusOK)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelDebug)
	inst := writerTestInstance(srv.URL)
	client := NewAPIClient(inst.URL, inst.APIKey)

	written, err := unmonitorMovie(context.Background(), logger, client, inst, 7, false)
	if err != nil {
		t.Fatalf("unmonitorMovie returned error: %v\n%s", err, buf.String())
	}
	if !written {
		t.Fatal("written = false, want true after a successful PUT")
	}

	puts := filterRequests(*got, http.MethodPut)
	if len(puts) != 1 {
		t.Fatalf("expected exactly 1 PUT, got %d: %+v", len(puts), *got)
	}
	if puts[0].path != "/api/v3/movie/7" {
		t.Errorf("PUT path = %q, want /api/v3/movie/7", puts[0].path)
	}

	var sent, fetched map[string]json.RawMessage
	if err := json.Unmarshal(puts[0].body, &sent); err != nil {
		t.Fatalf("PUT body is not a JSON object: %v (body=%s)", err, puts[0].body)
	}
	if err := json.Unmarshal([]byte(writerTestMovieJSON), &fetched); err != nil {
		t.Fatalf("fixture is not a JSON object: %v", err)
	}

	if len(sent) != len(fetched) {
		t.Errorf("PUT body has %d keys, want %d (no key may be added or dropped): sent=%v", len(sent), len(fetched), sent)
	}
	for key, want := range fetched {
		gotVal, present := sent[key]
		if !present {
			t.Errorf("key %q is missing from the PUT body; the fetched object must be sent back otherwise unmodified", key)
			continue
		}
		if key == "monitored" {
			if string(gotVal) != "false" {
				t.Errorf("monitored = %s in the PUT body, want false", gotVal)
			}
			continue
		}
		if !jsonBytesEqual(t, gotVal, want) {
			t.Errorf("key %q changed: PUT body has %s, fetched object had %s", key, gotVal, want)
		}
	}
}

// jsonBytesEqual compares two raw JSON values byte-for-byte after removing
// insignificant whitespace, so an assertion is not defeated by the fixture's
// pretty-printing while still catching any real change of value, ordering
// inside an array, or numeric reformatting (e.g. a large id turned into
// 9.876543210123e+12 by a map[string]any round trip).
func jsonBytesEqual(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	return string(compactJSON(t, a)) == string(compactJSON(t, b))
}

func compactJSON(t *testing.T, raw json.RawMessage) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("compacting %s: %v", raw, err)
	}
	return buf.Bytes()
}

// TestUnmonitorMovie_WriteMode_PreservesLargeIntegersExactly pins the
// specific corruption a map[string]any decode would cause: encoding/json
// decodes every JSON number into a float64 there, so a large integer such
// as sizeOnDisk would be re-encoded in exponent form (or lose precision
// outright). Decoding into json.RawMessage keeps the original bytes.
func TestUnmonitorMovie_WriteMode_PreservesLargeIntegersExactly(t *testing.T) {
	srv, got := writerTestServer(t, `{"id": 7, "monitored": true, "sizeOnDisk": 9007199254740993}`, http.StatusOK)
	defer srv.Close()

	logger, _ := newDecisionTestLogger(slog.LevelDebug)
	inst := writerTestInstance(srv.URL)
	client := NewAPIClient(inst.URL, inst.APIKey)

	if _, err := unmonitorMovie(context.Background(), logger, client, inst, 7, false); err != nil {
		t.Fatalf("unmonitorMovie returned error: %v", err)
	}

	puts := filterRequests(*got, http.MethodPut)
	if len(puts) != 1 {
		t.Fatalf("expected exactly 1 PUT, got %d", len(puts))
	}
	if !strings.Contains(string(puts[0].body), "9007199254740993") {
		t.Errorf("PUT body lost the exact large integer: %s", puts[0].body)
	}
}

// TestUnmonitorMovie_FreshGetShowsAlreadyUnmonitored_NoPut covers the
// scan-to-write race: the movie was monitored when the library was scanned
// but is monitored:false by the time the write pass reaches it. Changing
// nothing is then the correct action (§2.4's spirit: change exactly one
// thing, and only when it needs changing).
func TestUnmonitorMovie_FreshGetShowsAlreadyUnmonitored_NoPut(t *testing.T) {
	srv, got := writerTestServer(t, `{"id": 7, "title": "Already Done", "monitored": false}`, http.StatusOK)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := writerTestInstance(srv.URL)
	client := NewAPIClient(inst.URL, inst.APIKey)

	written, err := unmonitorMovie(context.Background(), logger, client, inst, 7, false)
	if err != nil {
		t.Fatalf("unmonitorMovie returned error: %v", err)
	}
	if written {
		t.Error("written = true, want false: nothing was changed")
	}
	if len(filterRequests(*got, http.MethodPut)) != 0 {
		t.Errorf("expected zero PUTs for an already-unmonitored movie, got %+v", *got)
	}
	out := buf.String()
	if !strings.Contains(out, "already unmonitored") {
		t.Errorf("expected an info log explaining no write was needed:\n%s", out)
	}
	if !strings.Contains(out, "level=INFO") {
		t.Errorf("expected the already-unmonitored log at info level:\n%s", out)
	}
}

// TestUnmonitorMovie_MonitoredFieldAbsent_RefusesToWrite is the
// untrusted-input guard on the write path itself: if the fresh object has
// no "monitored" key at all, our assumed field name may not match this
// Radarr version, and setting it would ADD a key to the object rather than
// change one. §2.6 says never guess: error out and write nothing.
func TestUnmonitorMovie_MonitoredFieldAbsent_RefusesToWrite(t *testing.T) {
	srv, got := writerTestServer(t, `{"id": 7, "title": "No Monitored Key"}`, http.StatusOK)
	defer srv.Close()

	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := writerTestInstance(srv.URL)
	client := NewAPIClient(inst.URL, inst.APIKey)

	written, err := unmonitorMovie(context.Background(), logger, client, inst, 7, false)
	if err == nil {
		t.Fatal("unmonitorMovie returned nil error, want an error when monitored is absent")
	}
	if written {
		t.Error("written = true, want false")
	}
	if !strings.Contains(err.Error(), "monitored") {
		t.Errorf("error %q does not name the missing field", err.Error())
	}
	if len(filterRequests(*got, http.MethodPut)) != 0 {
		t.Errorf("expected zero PUTs, got %+v", *got)
	}
}

// TestUnmonitorMovie_FreshGetIdMismatch_RefusesToWrite guards against
// writing back an object that is not the one we asked for (a
// misconfigured reverse proxy, a redirect, a caching layer). Writing
// monitored:false onto the wrong movie is exactly the class of mistake this
// project can never make, so a mismatch is fatal for this item.
func TestUnmonitorMovie_FreshGetIdMismatch_RefusesToWrite(t *testing.T) {
	srv, got := writerTestServer(t, `{"id": 99, "title": "Wrong Movie", "monitored": true}`, http.StatusOK)
	defer srv.Close()

	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := writerTestInstance(srv.URL)
	client := NewAPIClient(inst.URL, inst.APIKey)

	written, err := unmonitorMovie(context.Background(), logger, client, inst, 7, false)
	if err == nil {
		t.Fatal("unmonitorMovie returned nil error, want an error when the fetched object's id does not match")
	}
	if written {
		t.Error("written = true, want false")
	}
	if !strings.Contains(err.Error(), "99") || !strings.Contains(err.Error(), "7") {
		t.Errorf("error %q does not state both the requested and returned ids", err.Error())
	}
	if len(filterRequests(*got, http.MethodPut)) != 0 {
		t.Errorf("expected zero PUTs, got %+v", *got)
	}
}

func TestUnmonitorMovie_GetFails_ReturnsErrorWithoutWriting(t *testing.T) {
	var got []recordedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, recordedRequest{method: r.Method, path: r.URL.Path, body: body})
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"message":"boom"}`))
	}))
	defer srv.Close()

	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := writerTestInstance(srv.URL)
	client := NewAPIClient(inst.URL, inst.APIKey)

	written, err := unmonitorMovie(context.Background(), logger, client, inst, 7, false)
	if err == nil {
		t.Fatal("unmonitorMovie returned nil error, want an error when the fresh GET fails")
	}
	if written {
		t.Error("written = true, want false")
	}
	if len(filterRequests(got, http.MethodPut)) != 0 {
		t.Errorf("expected zero PUTs after a failed GET, got %+v", got)
	}
}

func TestUnmonitorMovie_GetReturnsMalformedJSON_ReturnsErrorWithoutWriting(t *testing.T) {
	srv, got := writerTestServer(t, `not json at all`, http.StatusOK)
	defer srv.Close()

	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := writerTestInstance(srv.URL)
	client := NewAPIClient(inst.URL, inst.APIKey)

	if _, err := unmonitorMovie(context.Background(), logger, client, inst, 7, false); err == nil {
		t.Fatal("unmonitorMovie returned nil error, want an error for a malformed fresh GET response")
	}
	if len(filterRequests(*got, http.MethodPut)) != 0 {
		t.Errorf("expected zero PUTs, got %+v", *got)
	}
}

// TestUnmonitorMovie_PutNonTwoxx_ReturnsErrorAndNeverRetries pins §2.6's
// no-retry rule at the write site: a rejected PUT is reported once and
// attempted exactly once.
func TestUnmonitorMovie_PutNonTwoxx_ReturnsErrorAndNeverRetries(t *testing.T) {
	srv, got := writerTestServer(t, writerTestMovieJSON, http.StatusBadRequest)
	defer srv.Close()

	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := writerTestInstance(srv.URL)
	client := NewAPIClient(inst.URL, inst.APIKey)

	written, err := unmonitorMovie(context.Background(), logger, client, inst, 7, false)
	if err == nil {
		t.Fatal("unmonitorMovie returned nil error, want an error for a non-2xx PUT")
	}
	if written {
		t.Error("written = true, want false: the PUT was rejected")
	}
	if !strings.Contains(err.Error(), "write rejected") {
		t.Errorf("error %q does not carry the response body snippet", err.Error())
	}
	if n := len(filterRequests(*got, http.MethodPut)); n != 1 {
		t.Errorf("made %d PUT attempts, want exactly 1 (writes are never retried within a cycle)", n)
	}
}

// TestUnmonitorMovie_NeverTouchesAnyOtherEndpoint pins §2.3: the write path
// talks to /api/v3/movie/{id} and nothing else — never /api/v3/command,
// never a delete, never a search trigger.
func TestUnmonitorMovie_NeverTouchesAnyOtherEndpoint(t *testing.T) {
	srv, got := writerTestServer(t, writerTestMovieJSON, http.StatusOK)
	defer srv.Close()

	logger, _ := newDecisionTestLogger(slog.LevelDebug)
	inst := writerTestInstance(srv.URL)
	client := NewAPIClient(inst.URL, inst.APIKey)

	if _, err := unmonitorMovie(context.Background(), logger, client, inst, 7, false); err != nil {
		t.Fatalf("unmonitorMovie returned error: %v", err)
	}

	for _, r := range *got {
		if r.path != "/api/v3/movie/7" {
			t.Errorf("write path touched %s %s; only /api/v3/movie/7 is permitted", r.method, r.path)
		}
		if r.method != http.MethodGet && r.method != http.MethodPut {
			t.Errorf("write path issued a %s request; only GET and PUT are permitted", r.method)
		}
		if strings.Contains(string(r.body), "deleteFiles") {
			t.Errorf("request body mentions deleteFiles, which must never be sent: %s", r.body)
		}
	}
}

// --- radarrFake: a whole-pipeline recording fake --------------------------
//
// The write-path tests need something the read-only phases never did: a
// fake that serves EVERY endpoint a full run touches while recording every
// request it receives, of any method, to any path. Only a whole-server
// recorder can support the project's most important assertion — that a
// dry-run makes zero write requests — because that claim is about requests
// the code did not make, to endpoints nobody thought to stub.

type radarrFake struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []recordedRequest

	// Fixtures. Set directly by a test after construction and before the
	// run under test starts; the handlers read them under the same mutex.
	moviesJSON   string
	wantedJSON   string
	tagsJSON     string
	profilesJSON string
	cfScore      int
	detail       map[int]string // GET /api/v3/movie/{id} bodies, by id
	putStatus    map[int]int    // PUT /api/v3/movie/{id} status, by id (default 200)
}

// newRadarrFake starts a fake Radarr serving moviesJSON as the library and
// detail as the per-movie objects the write path re-fetches. Everything
// else defaults to the standard fixtures used across the decision-engine
// tests: one profile (id 1, HD-1080p, upgradeAllowed, cutoffFormatScore
// 100), no tags, an empty wanted/cutoff set, and a custom format score of
// 200 (comfortably above the cutoff, so rule 6 passes).
func newRadarrFake(t *testing.T, moviesJSON string, detail map[int]string) *radarrFake {
	t.Helper()
	f := &radarrFake{
		moviesJSON:   moviesJSON,
		wantedJSON:   emptyWantedCutoffJSON,
		tagsJSON:     decisionEngineNoTagsJSON,
		profilesJSON: decisionEngineProfilesJSON,
		cfScore:      200,
		detail:       detail,
		putStatus:    map[int]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/system/status", f.handle(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"appName": "Radarr", "version": "5.14.0.9383"}`))
	}))
	mux.HandleFunc("/api/v3/qualityprofile", f.handle(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(f.profilesJSON))
	}))
	mux.HandleFunc("/api/v3/tag", f.handle(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(f.tagsJSON))
	}))
	mux.HandleFunc("/api/v3/wanted/cutoff", f.handle(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(f.wantedJSON))
	}))
	mux.HandleFunc("/api/v3/moviefile", f.handle(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `[{"id": 1, "customFormatScore": %d}]`, f.cfScore)
	}))
	mux.HandleFunc("/api/v3/movie", f.handle(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(f.moviesJSON))
	}))
	mux.HandleFunc("/api/v3/movie/", f.handle(f.serveMovieDetail))
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// handle wraps a handler so every request — method, path, and exact body
// bytes — is recorded before it is served.
func (f *radarrFake) handle(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{method: r.Method, path: r.URL.Path, body: body})
		f.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		next(w, r)
	}
}

func (f *radarrFake) serveMovieDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/api/v3/movie/"))
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		body, found := f.detail[id]
		if !found {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(body))
	case http.MethodPut:
		status := http.StatusOK
		if s, found := f.putStatus[id]; found {
			status = s
		}
		w.WriteHeader(status)
		if status >= 400 {
			w.Write([]byte(`{"message":"write rejected by fake"}`))
		}
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// writes returns every non-read request the fake received. Anything that is
// not a GET counts: the dry-run guarantee is "zero write requests", not
// "zero PUTs to the endpoint we expected".
func (f *radarrFake) writes() []recordedRequest {
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

func (f *radarrFake) puts() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return filterRequests(f.requests, http.MethodPut)
}

func (f *radarrFake) instance() Instance {
	return Instance{Name: "radarr-main", Type: "radarr", URL: f.srv.URL, APIKey: "key"}
}

// monitoredMovieDetail is a full-object fixture for GET /api/v3/movie/{id},
// carrying a field this codebase does not model so round-trip preservation
// is exercised through the engine too, not only in the unit tests.
func monitoredMovieDetail(id int, title string) string {
	return fmt.Sprintf(`{"id": %d, "title": %q, "monitored": true, "hasFile": true, "qualityProfileId": 1, "tags": [], "someFutureField": {"keep": "me"}}`, id, title)
}

// libraryMovie is one element of the fake's GET /api/v3/movie response.
func libraryMovie(id int, title string, monitored, hasFile bool) string {
	return fmt.Sprintf(`{"id": %d, "title": %q, "monitored": %t, "hasFile": %t, "qualityProfileId": 1, "tags": [], "movieFile": {"id": 1, "qualityCutoffNotMet": false}}`,
		id, title, monitored, hasFile)
}

// --- end-to-end write behaviour through run() -----------------------------

// writeTestConfig writes a config pointing at url, with dry_run set
// explicitly (never left to the default, so each test states which mode it
// is exercising).
func writeTestConfig(t *testing.T, url string, dryRun bool) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := fmt.Sprintf(`
dry_run: %t
instances:
  - name: radarr-main
    type: radarr
    url: %s
    api_key: key1
`, dryRun, url)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	return path
}

// TestRun_DryRun_MakesZeroWriteRequestsAcrossTheEntireRun is the single most
// important test in this project. It is not "no PUT was sent to the endpoint
// we expected": the fake records every request of every method to every
// path, and the assertion is that across a complete run — one in which the
// decision engine really does fire, the cross-check passes, and the write
// pass is entered and walks the full write path for two separate movies —
// not one non-GET request was made. The would-unmonitor report lines must
// still be there, because a dry-run whose report went quiet would be
// indistinguishable from a run that found nothing to do.
func TestRun_DryRun_MakesZeroWriteRequestsAcrossTheEntireRun(t *testing.T) {
	fake := newRadarrFake(t,
		"["+libraryMovie(1, "Would Unmonitor A", true, true)+","+libraryMovie(2, "Would Unmonitor B", true, true)+"]",
		map[int]string{
			1: monitoredMovieDetail(1, "Would Unmonitor A"),
			2: monitoredMovieDetail(2, "Would Unmonitor B"),
		})

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", writeTestConfig(t, fake.srv.URL, true), "--once"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}

	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("dry-run made %d write request(s) across the run, want ZERO: %+v", len(writes), writes)
	}

	out := stdout.String()
	if !strings.Contains(out, `title="Would Unmonitor A"`) || !strings.Contains(out, `title="Would Unmonitor B"`) {
		t.Errorf("expected both would-unmonitor report lines in dry-run:\n%s", out)
	}
	if !strings.Contains(out, "unmonitored=0") {
		t.Errorf("expected unmonitored=0 in the dry-run summary:\n%s", out)
	}
}

// TestRun_DryRunForcedByFlag_MakesZeroWriteRequests pins the interaction
// between the two switches that matter here: config says dry_run: false,
// --dry-run forces it back on, and the run must then write nothing.
func TestRun_DryRunForcedByFlag_MakesZeroWriteRequests(t *testing.T) {
	fake := newRadarrFake(t,
		"["+libraryMovie(1, "Would Unmonitor A", true, true)+"]",
		map[int]string{1: monitoredMovieDetail(1, "Would Unmonitor A")})

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", writeTestConfig(t, fake.srv.URL, false), "--once", "--dry-run"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}

	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("--dry-run must force zero writes even when the config disables dry-run, got %+v", writes)
	}
	if !strings.Contains(stdout.String(), "dry_run=true") {
		t.Errorf("expected the startup printout to show dry-run forced on:\n%s", stdout.String())
	}
}

// TestRun_WriteMode_OnlyID_MakesExactlyOnePutForTheNamedMovie is the Phase 4
// acceptance criterion, run against a fake: `--once --only-id N` with
// dry_run false writes exactly one movie — the named one — and the PUT
// carries the fetched object back with only monitored changed.
func TestRun_WriteMode_OnlyID_MakesExactlyOnePutForTheNamedMovie(t *testing.T) {
	fake := newRadarrFake(t,
		"["+libraryMovie(1, "Bystander Movie", true, true)+","+libraryMovie(2, "Chosen Movie", true, true)+"]",
		map[int]string{
			1: monitoredMovieDetail(1, "Bystander Movie"),
			2: monitoredMovieDetail(2, "Chosen Movie"),
		})

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", writeTestConfig(t, fake.srv.URL, false), "--once", "--only-id", "2"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}

	writes := fake.writes()
	if len(writes) != 1 {
		t.Fatalf("expected exactly 1 write request across the run, got %d: %+v", len(writes), writes)
	}
	if writes[0].method != http.MethodPut || writes[0].path != "/api/v3/movie/2" {
		t.Fatalf("write was %s %s, want PUT /api/v3/movie/2", writes[0].method, writes[0].path)
	}

	var sent map[string]json.RawMessage
	if err := json.Unmarshal(writes[0].body, &sent); err != nil {
		t.Fatalf("PUT body is not a JSON object: %v", err)
	}
	if string(sent["monitored"]) != "false" {
		t.Errorf("monitored = %s in the PUT body, want false", sent["monitored"])
	}
	if string(sent["title"]) != `"Chosen Movie"` {
		t.Errorf("title = %s in the PUT body, want the fetched movie's own title", sent["title"])
	}
	if _, present := sent["someFutureField"]; !present {
		t.Error("the PUT body dropped a field this codebase does not model; the fetched object must go back otherwise unmodified")
	}

	out := stdout.String()
	if !strings.Contains(out, "msg=unmonitor ") || !strings.Contains(out, `title="Chosen Movie"`) {
		t.Errorf("expected an unmonitor log line for the written movie:\n%s", out)
	}
	if !strings.Contains(out, "unmonitored=1") {
		t.Errorf("expected unmonitored=1 in the summary:\n%s", out)
	}
	// No REPORT line may mention any other movie. Cross-check lines are a
	// deliberate exception and are asserted on separately: --only-id scopes
	// what is reported and written, not what is validated.
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "msg=would-unmonitor") && !strings.Contains(line, "msg=skip") && !strings.Contains(line, "msg=unmonitor ") {
			continue
		}
		if strings.Contains(line, "Bystander Movie") {
			t.Errorf("--only-id mode produced a report line for another movie: %s", line)
		}
	}
	if !strings.Contains(out, `msg=cross-check instance=radarr-main id=1 title="Bystander Movie"`) {
		t.Errorf("expected the cross-check to still have validated the whole library, not just the target:\n%s", out)
	}
}

// TestRun_WriteMode_CrossCheckFailure_MakesZeroWriteRequests is the
// end-to-end version of the cross-check gate: bad data in the library stops
// the write from ever being attempted, through the real main.go wiring, and
// the run still exits 0.
func TestRun_WriteMode_CrossCheckFailure_MakesZeroWriteRequests(t *testing.T) {
	// The library says this movie's own qualityCutoffNotMet is true while
	// the wanted/cutoff set (empty) says it is not below cutoff: a
	// disagreement between two Radarr code paths.
	disagreeing := `{"id": 1, "title": "Disagreeing Movie", "monitored": true, "hasFile": true, "qualityProfileId": 1, "tags": [], "movieFile": {"id": 1, "qualityCutoffNotMet": true}}`
	fake := newRadarrFake(t, "["+disagreeing+"]", map[int]string{1: monitoredMovieDetail(1, "Disagreeing Movie")})

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", writeTestConfig(t, fake.srv.URL, false), "--once"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}

	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("a failed cross-check must stop every write end to end, got %+v", writes)
	}
	out := stdout.String()
	if !strings.Contains(out, "crossCheck=FAILED") {
		t.Errorf("expected the summary to state the cross-check FAILED:\n%s", out)
	}
	if !strings.Contains(out, "withheld") {
		t.Errorf("expected a warning explaining writes were withheld:\n%s", out)
	}
}

// TestRun_WriteMode_PutRejected_LogsErrorCountsItAndStillExitsZero pins
// §2.6 end to end: a rejected write is reported and counted, the run
// continues, and a per-item API failure is not a process failure.
func TestRun_WriteMode_PutRejected_LogsErrorCountsItAndStillExitsZero(t *testing.T) {
	fake := newRadarrFake(t,
		"["+libraryMovie(1, "Rejected Movie", true, true)+"]",
		map[int]string{1: monitoredMovieDetail(1, "Rejected Movie")})
	fake.putStatus[1] = http.StatusConflict

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", writeTestConfig(t, fake.srv.URL, false), "--once"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 even when a write is rejected; stderr=%s", code, stderr.String())
	}

	if n := len(fake.puts()); n != 1 {
		t.Errorf("made %d PUT attempts, want exactly 1 (no retries within a cycle)", n)
	}
	out := stdout.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "Rejected Movie") {
		t.Errorf("expected an error-level log naming the rejected write:\n%s", out)
	}
	if !strings.Contains(out, "writeErrors=1") || !strings.Contains(out, "unmonitored=0") {
		t.Errorf("expected writeErrors=1 and unmonitored=0 in the summary:\n%s", out)
	}
}

// TestRun_OnlyID_UnknownMovie_WarnsAndWritesNothing covers the id the human
// mistypes: nothing is written, the id is named, and the run still exits 0.
func TestRun_OnlyID_UnknownMovie_WarnsAndWritesNothing(t *testing.T) {
	fake := newRadarrFake(t,
		"["+libraryMovie(1, "The Only Movie", true, true)+"]",
		map[int]string{1: monitoredMovieDetail(1, "The Only Movie")})

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", writeTestConfig(t, fake.srv.URL, false), "--once", "--only-id", "4242"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}

	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("an unknown --only-id must write nothing, got %+v", writes)
	}
	out := stdout.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "4242") {
		t.Errorf("expected a warning naming the id that was not found:\n%s", out)
	}
	if strings.Contains(out, "msg=would-unmonitor") || strings.Contains(out, "msg=skip") {
		t.Errorf("an unknown --only-id must produce no decisions:\n%s", out)
	}
}

// TestRun_WriteMode_SonarrInstanceIsNeverWritten pins the phase boundary:
// Sonarr writes arrive in Phase 7, so a sonarr instance must still be
// connectivity-only even with dry_run false and a movie id named.
func TestRun_WriteMode_SonarrInstanceIsNeverWritten(t *testing.T) {
	fake := newRadarrFake(t,
		"["+libraryMovie(1, "Some Movie", true, true)+"]",
		map[int]string{1: monitoredMovieDetail(1, "Some Movie")})

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := fmt.Sprintf(`
dry_run: false
instances:
  - name: sonarr-main
    type: sonarr
    url: %s
    api_key: key1
`, fake.srv.URL)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path, "--once", "--only-id", "1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}

	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("a sonarr instance must never be written in this phase, got %+v", writes)
	}
}
