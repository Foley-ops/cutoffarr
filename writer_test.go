package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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
