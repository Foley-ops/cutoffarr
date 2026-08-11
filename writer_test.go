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

// recordedRequest captures one request a write-path fake server received,
// so tests can assert on method, path, exact body bytes, and Content-Type.
//
// contentType is not bookkeeping: DoJSON exists solely to set it, because
// Radarr's ASP.NET Core stack answers a body it cannot type with 415, and
// nothing else in the suite binds the write path to DoJSON. Without this
// field, swapping client.DoJSON for client.Do in writer.go leaves every test
// in this project green and fails on the first live write.
type recordedRequest struct {
	method      string
	path        string
	body        []byte
	contentType string
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
		got = append(got, recordedRequest{method: r.Method, path: r.URL.Path, body: body, contentType: r.Header.Get("Content-Type")})

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
//
// The title and overview deliberately contain "&", "<" and ">". Those three
// characters are the ones encoding/json's default HTML escaping rewrites
// into unicode escapes inside every string it encodes, so without them in
// the fixture the byte-for-byte assertion below would pass while being false
// for a large share of a real library ("Cheech & Chong", "Fast & Furious",
// any title with an <angle-bracketed> edition suffix).
const writerTestMovieJSON = `{
	"id": 7,
	"title": "Mr. & Mrs. Smith <Special Edition>",
	"monitored": true,
	"hasFile": true,
	"qualityProfileId": 1,
	"tags": [3, 9],
	"sizeOnDisk": 9876543210123,
	"overview": "Cheech & Chong: 4 < 5 > 3",
	"ratings": {"imdb": {"votes": 1234, "value": 7.4}},
	"movieFile": {"id": 42, "customFormatScore": 200, "relativePath": "Mr. & Mrs. Smith <Special Edition>.mkv"},
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
	// The header is the whole reason DoJSON exists: Radarr's ASP.NET Core
	// stack answers a body it cannot type with 415, and Do/DoQuery set no
	// Content-Type at all. Nothing else in this suite would notice
	// client.DoJSON being swapped for client.Do — the fake would happily
	// serve it — so the first live write would be the test.
	if puts[0].contentType != "application/json" {
		t.Errorf("PUT Content-Type = %q, want application/json (Radarr rejects an untyped body with 415)", puts[0].contentType)
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

// TestUnmonitorMovie_WriteMode_SendsUnescapedBytesForHTMLSensitiveCharacters
// pins the round-trip claim at the byte level rather than the value level.
// encoding/json's Marshal escapes "&", "<" and ">" inside every string it
// encodes, so a movie titled "Mr. & Mrs. Smith" would be PUT back with those
// characters replaced by unicode escapes — a different byte sequence from
// the one Radarr sent, even though it decodes to the same string. §2.4 says
// the object goes
// back "otherwise unmodified", and the binding mandate says byte-for-byte on
// every other key, so the encoder must have HTML escaping switched off.
func TestUnmonitorMovie_WriteMode_SendsUnescapedBytesForHTMLSensitiveCharacters(t *testing.T) {
	srv, got := writerTestServer(t, writerTestMovieJSON, http.StatusOK)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelDebug)
	inst := writerTestInstance(srv.URL)
	client := NewAPIClient(inst.URL, inst.APIKey)

	if _, err := unmonitorMovie(context.Background(), logger, client, inst, 7, false); err != nil {
		t.Fatalf("unmonitorMovie returned error: %v\n%s", err, buf.String())
	}

	puts := filterRequests(*got, http.MethodPut)
	if len(puts) != 1 {
		t.Fatalf("expected exactly 1 PUT, got %d", len(puts))
	}
	body := string(puts[0].body)
	// The escape sequences are built rather than written out, so the
	// assertion cannot be defeated by the test source itself being edited
	// into the very characters it is looking for.
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

// TestUnmonitorMovie_PutRedirected_IsNotReportedAsAWrite is the end of the
// redirect story at the write site: whatever the transport does, a write may
// only be reported as performed when the server actually confirmed it. A 302
// on the PUT must surface as an error, never as written=true.
func TestUnmonitorMovie_PutRedirected_IsNotReportedAsAWrite(t *testing.T) {
	var got []recordedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, recordedRequest{method: r.Method, path: r.URL.Path, body: body})
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/movie/7":
			w.Write([]byte(writerTestMovieJSON))
		case r.Method == http.MethodPut:
			// The classic reverse-proxy answer: "the resource lives over
			// there". net/http's default policy would re-issue this as a
			// GET, and that GET would succeed.
			http.Redirect(w, r, "/api/v3/movie/7", http.StatusFound)
		default:
			w.Write([]byte(writerTestMovieJSON))
		}
	}))
	defer srv.Close()

	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := writerTestInstance(srv.URL)
	client := NewAPIClient(inst.URL, inst.APIKey)

	written, err := unmonitorMovie(context.Background(), logger, client, inst, 7, false)
	if err == nil {
		t.Fatal("unmonitorMovie returned nil error for a redirected PUT")
	}
	if written {
		t.Error("written = true for a redirected PUT: the movie is still monitored, so this would be a false success")
	}
	if n := len(filterRequests(got, http.MethodGet)); n != 1 {
		t.Errorf("saw %d GETs, want exactly 1 (the pre-write fetch); a followed redirect would add another: %+v", n, got)
	}
}

// TestUnmonitorMovie_PutEchoesStillMonitored_IsAnError pins the last check
// in the chain: Radarr echoes the updated object back, and that echo is the
// only evidence the write actually took effect. A 2xx whose body still says
// monitored:true (a cache answering the write, a proxy replaying the old
// object, a Radarr that rejected the change without saying so) must not be
// counted as a write — Phase 5's no-op contract reads that count.
func TestUnmonitorMovie_PutEchoesStillMonitored_IsAnError(t *testing.T) {
	var got []recordedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, recordedRequest{method: r.Method, path: r.URL.Path, body: body})
		// Both the GET and the PUT answer with a still-monitored object.
		w.Write([]byte(`{"id": 7, "title": "Stubborn Movie", "monitored": true}`))
	}))
	defer srv.Close()

	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := writerTestInstance(srv.URL)
	client := NewAPIClient(inst.URL, inst.APIKey)

	written, err := unmonitorMovie(context.Background(), logger, client, inst, 7, false)
	if err == nil {
		t.Fatal("unmonitorMovie returned nil error when the response said the movie was still monitored")
	}
	if written {
		t.Error("written = true when the server's own echo says monitored is still true")
	}
	if !strings.Contains(err.Error(), monitoredKey) {
		t.Errorf("error %q does not name the field that failed to change", err.Error())
	}
	if n := len(filterRequests(got, http.MethodPut)); n != 1 {
		t.Errorf("made %d PUT attempts, want exactly 1 (an unconfirmed write is never retried)", n)
	}
}

// TestUnmonitorMovie_PutEchoesUnmonitored_IsAWrite is the positive half of
// the same check: the ordinary Radarr response — the updated object, with
// monitored now false — is what a confirmed write looks like.
func TestUnmonitorMovie_PutEchoesUnmonitored_IsAWrite(t *testing.T) {
	srv, _ := writerTestServer(t, writerTestMovieJSON, http.StatusOK)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := writerTestInstance(srv.URL)
	client := NewAPIClient(inst.URL, inst.APIKey)

	written, err := unmonitorMovie(context.Background(), logger, client, inst, 7, false)
	if err != nil {
		t.Fatalf("unmonitorMovie returned error: %v\n%s", err, buf.String())
	}
	if !written {
		t.Error("written = false after a PUT the server confirmed with monitored:false")
	}
}

// TestUnmonitorMovie_PutReturnsEmptyBodyOn2xx_IsUnconfirmedNotAWrite covers
// the branch of verifyWriteEcho that the live server is most likely to take
// and that no test previously exercised: a 2xx with nothing in the body.
// Radarr answers PUT /api/v3/movie/{id} with the updated resource, but that
// is an observation about one version; a Radarr (or a proxy) answering 200
// with an empty body would land here.
//
// The write must not be reported as done — nothing confirmed it — but it is
// also NOT the same event as a rejected write: the server accepted it, and
// the change may well have taken effect. errWriteUnverified is what carries
// that distinction to the caller (see runWritePass); at this level the
// contract is written=false, exactly one PUT, and an error naming the status
// the server actually returned.
func TestUnmonitorMovie_PutReturnsEmptyBodyOn2xx_IsUnconfirmedNotAWrite(t *testing.T) {
	var got []recordedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, recordedRequest{method: r.Method, path: r.URL.Path, body: body})
		if r.Method == http.MethodPut {
			// Accepted, and utterly silent about what happened.
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Write([]byte(writerTestMovieJSON))
	}))
	defer srv.Close()

	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := writerTestInstance(srv.URL)
	client := NewAPIClient(inst.URL, inst.APIKey)

	written, err := unmonitorMovie(context.Background(), logger, client, inst, 7, false)
	if err == nil {
		t.Fatal("unmonitorMovie returned nil error for a 2xx with an empty body: nothing confirmed the change")
	}
	if written {
		t.Error("written = true on an unconfirmed write: only the server's own echo may report a write as done")
	}
	if !errors.Is(err, errWriteUnverified) {
		t.Errorf("error %q is not classified as unverifiable; the caller cannot tell it apart from a rejected write", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(http.StatusAccepted)) {
		t.Errorf("error %q does not name the status the server returned", err)
	}
	if n := len(filterRequests(got, http.MethodPut)); n != 1 {
		t.Errorf("made %d PUT attempts, want exactly 1 (§2.6: never retried within a cycle)", n)
	}
}

// TestVerifyWriteEcho_SeparatesUnverifiableFromContradicted is the whole
// point of the split, stated at the unit: an echo that CONTRADICTS the write
// ("monitored": true) is proof the movie is still monitored — a failed write.
// An echo that merely fails to confirm it (empty, not an object, no key, not
// a boolean) is proof of nothing; the write may well have landed. Both are
// errors, but only the first may be reported as a write failure, so they must
// be distinguishable by errors.Is rather than by reading the message.
func TestVerifyWriteEcho_SeparatesUnverifiableFromContradicted(t *testing.T) {
	unverifiable := []struct{ name, echo string }{
		{"empty body", ""},
		{"not a JSON object", `[{"id": 7}]`},
		{"no monitored key", `{"id": 7, "title": "Silent"}`},
		{"monitored is not a boolean", `{"id": 7, "monitored": "false"}`},
	}
	for _, tc := range unverifiable {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyWriteEcho([]byte(tc.echo), 7, http.StatusOK)
			if err == nil {
				t.Fatal("verifyWriteEcho returned nil: an unconfirmed write must never pass")
			}
			if !errors.Is(err, errWriteUnverified) {
				t.Errorf("error %q is not errWriteUnverified; it would be reported as a failed write", err)
			}
		})
	}

	err := verifyWriteEcho([]byte(`{"id": 7, "monitored": true}`), 7, http.StatusOK)
	if err == nil {
		t.Fatal("verifyWriteEcho returned nil for an echo that still says monitored:true")
	}
	if errors.Is(err, errWriteUnverified) {
		t.Errorf("an echo that says the movie is STILL MONITORED is a failed write, not an unverifiable one: %q", err)
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

	// putEcho overrides what a successful PUT answers with, by id. Unset
	// means "behave like Radarr": echo the object that was sent. Set to ""
	// for a server that accepts the write and says nothing at all, which is
	// the response shape the write path can neither confirm nor call a
	// failure.
	putEcho map[int]string
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
		putEcho:      map[int]string{},
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
	// The catch-all is what makes "zero write requests of any method to any
	// path" an assertion rather than a hope. Without it, every request to a
	// path this fake does not stub would be answered by http.ServeMux's
	// built-in NotFound handler, which records nothing — so a POST to
	// /api/v3/command during a dry-run would 404 silently and leave every
	// zero-write test green. Recording happens first; the 404 (what a real
	// Radarr would return for an endpoint that does not exist) happens after.
	mux.HandleFunc("/", f.handle(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
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
		f.requests = append(f.requests, recordedRequest{method: r.Method, path: r.URL.Path, body: body, contentType: r.Header.Get("Content-Type")})
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
			return
		}
		if echo, overridden := f.putEcho[id]; overridden {
			w.Write([]byte(echo))
			return
		}
		// Radarr answers a successful PUT with the updated object. The
		// writer verifies that echo before it will report a write as
		// performed, so a fake that stayed silent here would be a fake of
		// something Radarr never does.
		body, _ := io.ReadAll(r.Body)
		w.Write(body)
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

// all returns every request the fake received, of any method, copied under
// the mutex so a test can read it without racing the server's goroutines.
func (f *radarrFake) all() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

func (f *radarrFake) puts() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return filterRequests(f.requests, http.MethodPut)
}

func (f *radarrFake) instance() Instance {
	return Instance{Name: "radarr-main", Type: "radarr", URL: f.srv.URL, APIKey: "key"}
}

// TestRadarrFake_RecordsRequestsToPathsItDoesNotStub is a test OF the test
// fake, and it earns its place: every zero-write assertion in this project
// is only as strong as the fake's recording. Registering handlers for the
// seven endpoints a run is expected to touch would leave http.ServeMux's
// built-in NotFound handler answering everything else — recording nothing —
// so a future change that fired POST /api/v3/command or a DELETE at some
// unstubbed path during a dry-run would 404 silently and the "ZERO write
// requests" assertions would stay green. The claim those tests make is about
// requests to endpoints nobody thought to stub, so those are exactly the
// ones that must be recorded.
func TestRadarrFake_RecordsRequestsToPathsItDoesNotStub(t *testing.T) {
	fake := newRadarrFake(t, "[]", nil)

	unstubbed := []struct{ method, path, body string }{
		{http.MethodPost, "/api/v3/command", `{"name":"MoviesSearch"}`},
		{http.MethodDelete, "/api/v3/queue/12", ""},
		{http.MethodPut, "/api/v3/config/naming", `{}`},
	}
	for _, u := range unstubbed {
		req, err := http.NewRequest(u.method, fake.srv.URL+u.path, strings.NewReader(u.body))
		if err != nil {
			t.Fatalf("building %s %s: %v", u.method, u.path, err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", u.method, u.path, err)
		}
		resp.Body.Close()
	}

	writes := fake.writes()
	if len(writes) != len(unstubbed) {
		t.Fatalf("the fake recorded %d of %d requests to unstubbed paths; anything it does not record cannot be asserted on: %+v", len(writes), len(unstubbed), writes)
	}
	for i, u := range unstubbed {
		if writes[i].method != u.method || writes[i].path != u.path {
			t.Errorf("recorded %s %s, want %s %s", writes[i].method, writes[i].path, u.method, u.path)
		}
	}
	if string(writes[0].body) != `{"name":"MoviesSearch"}` {
		t.Errorf("recorded body = %q, want the bytes that were actually sent", writes[0].body)
	}
}

// monitoredMovieDetail is a full-object fixture for GET /api/v3/movie/{id},
// carrying a field this codebase does not model so round-trip preservation
// is exercised through the engine too, not only in the unit tests. The
// overview carries "&" and "<": encoding/json escapes those by default, so
// their presence here is what makes the whole-run assertions capable of
// catching an escaping regression rather than only a dropped-key one.
func monitoredMovieDetail(id int, title string) string {
	return fmt.Sprintf(`{"id": %d, "title": %q, "monitored": true, "hasFile": true, "qualityProfileId": 1, "tags": [], "overview": "Cheech & Chong <uncut>", "someFutureField": {"keep": "me"}}`, id, title)
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
	// A healthy dry-run reports no write errors and no rehearsal errors. The
	// second counter is always present in dry-run so its absence can never
	// be mistaken for "no rehearsal failures happened".
	if !strings.Contains(out, "writeErrors=0") || !strings.Contains(out, "writeRehearsalErrors=0") {
		t.Errorf("expected writeErrors=0 and writeRehearsalErrors=0 in the dry-run summary:\n%s", out)
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
	// Asserted here too, not only at the unit: this is the exact request the
	// live acceptance criterion will send, and an untyped body is a 415 from
	// Radarr however correct its contents are.
	if writes[0].contentType != "application/json" {
		t.Errorf("PUT Content-Type = %q, want application/json (Radarr rejects an untyped body with 415)", writes[0].contentType)
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
	// Byte-level, not value-level: decoding the body (as the assertions
	// above do) cannot see HTML escaping, because the escaped and unescaped
	// forms decode to the same string.
	if !strings.Contains(string(writes[0].body), "Cheech & Chong <uncut>") {
		t.Errorf("the PUT body re-encoded characters the fetched object sent literally:\n%s", writes[0].body)
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

// TestRun_WriteMode_CrossCheckInconclusive_MakesZeroWriteRequests is the
// other half of the mandated scenario: "inconclusive" blocks the write pass
// exactly as hard as "failed" does, through the real main.go wiring. A
// cross-check that verified nothing is not a weaker pass — it is no evidence
// at all, and no evidence is not permission to write.
func TestRun_WriteMode_CrossCheckInconclusive_MakesZeroWriteRequests(t *testing.T) {
	// movieFile is present (so the movie is a cross-check candidate) but
	// carries no qualityCutoffNotMet, so there is nothing to compare the
	// wanted-set membership against and the cross-check can reach no verdict.
	unverifiable := `{"id": 1, "title": "Unverifiable Movie", "monitored": true, "hasFile": true, "qualityProfileId": 1, "tags": [], "movieFile": {"id": 1}}`
	fake := newRadarrFake(t, "["+unverifiable+"]", map[int]string{1: monitoredMovieDetail(1, "Unverifiable Movie")})

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", writeTestConfig(t, fake.srv.URL, false), "--once"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}

	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("an inconclusive cross-check must stop every write end to end, got %+v", writes)
	}
	out := stdout.String()
	if !strings.Contains(out, "crossCheck=inconclusive") {
		t.Errorf("expected the summary to state the cross-check was inconclusive:\n%s", out)
	}
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "withheld") {
		t.Errorf("expected a warning explaining writes were withheld:\n%s", out)
	}
	if !strings.Contains(out, "withheldWrites=1") {
		t.Errorf("expected the withheld warning to state how many writes it blocked:\n%s", out)
	}
	if !strings.Contains(out, "unmonitored=0") {
		t.Errorf("expected unmonitored=0 in the summary:\n%s", out)
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

// --- --only-id across multiple radarr instances ---------------------------
//
// Radarr movie ids are per-instance: each instance numbers its own library
// from 1, so id 2 in radarr-hd and id 2 in radarr-4k are two entirely
// different films. A paired HD + 4K setup is explicitly supported, which
// makes an unqualified --only-id ambiguous rather than precise — the exact
// opposite of what this phase's contract ("first write, single item,
// explicitly named") promises.

// writeTwoRadarrConfig writes a config with two radarr instances, the paired
// setup the plan mandates supporting.
func writeTwoRadarrConfig(t *testing.T, hdURL, fourKURL string, dryRun bool) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := fmt.Sprintf(`
dry_run: %t
instances:
  - name: radarr-hd
    type: radarr
    url: %s
    api_key: key1
  - name: radarr-4k
    type: radarr
    url: %s
    api_key: key2
`, dryRun, hdURL, fourKURL)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	return path
}

// TestRun_OnlyID_AmbiguousAcrossTwoRadarrInstances_IsFatalBeforeAnyRequest
// is the safety pin: with two radarr instances configured and no instance
// named, "--only-id 2" identifies two different movies, and the phase's
// promise is that exactly one explicitly named item is written. Guessing —
// or writing both — is not an option, so the run refuses before it opens a
// single connection.
func TestRun_OnlyID_AmbiguousAcrossTwoRadarrInstances_IsFatalBeforeAnyRequest(t *testing.T) {
	hd := newRadarrFake(t,
		"["+libraryMovie(2, "HD Movie", true, true)+"]",
		map[int]string{2: monitoredMovieDetail(2, "HD Movie")})
	fourK := newRadarrFake(t,
		"["+libraryMovie(2, "A Completely Different 4K Movie", true, true)+"]",
		map[int]string{2: monitoredMovieDetail(2, "A Completely Different 4K Movie")})

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", writeTwoRadarrConfig(t, hd.srv.URL, fourK.srv.URL, false), "--once", "--only-id", "2"}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2: an ambiguous --only-id must be a fatal flag error\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	if writes := hd.writes(); len(writes) != 0 {
		t.Errorf("radarr-hd was written despite the ambiguity: %+v", writes)
	}
	if writes := fourK.writes(); len(writes) != 0 {
		t.Errorf("radarr-4k was written despite the ambiguity: %+v", writes)
	}
	if len(hd.all()) != 0 || len(fourK.all()) != 0 {
		t.Errorf("the run contacted an instance before refusing: hd=%d 4k=%d requests", len(hd.all()), len(fourK.all()))
	}
	msg := stderr.String()
	if !strings.Contains(msg, "--only-id") || !strings.Contains(msg, "--instance") {
		t.Errorf("the error must name the flag that is ambiguous and the flag that resolves it:\n%s", msg)
	}
}

// TestRun_OnlyID_WithInstanceFlag_WritesOnlyTheNamedInstance is the resolved
// form: --instance names which library the id belongs to, and every other
// instance is left entirely alone — not merely unwritten, but untouched.
func TestRun_OnlyID_WithInstanceFlag_WritesOnlyTheNamedInstance(t *testing.T) {
	hd := newRadarrFake(t,
		"["+libraryMovie(2, "HD Movie", true, true)+"]",
		map[int]string{2: monitoredMovieDetail(2, "HD Movie")})
	fourK := newRadarrFake(t,
		"["+libraryMovie(2, "A Completely Different 4K Movie", true, true)+"]",
		map[int]string{2: monitoredMovieDetail(2, "A Completely Different 4K Movie")})

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", writeTwoRadarrConfig(t, hd.srv.URL, fourK.srv.URL, false), "--once", "--only-id", "2", "--instance", "radarr-4k"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}

	if got := hd.all(); len(got) != 0 {
		t.Errorf("radarr-hd received %d request(s); an instance the human did not name must not be contacted at all: %+v", len(got), got)
	}
	writes := fourK.writes()
	if len(writes) != 1 {
		t.Fatalf("expected exactly 1 write to the named instance, got %d: %+v", len(writes), writes)
	}
	if writes[0].method != http.MethodPut || writes[0].path != "/api/v3/movie/2" {
		t.Fatalf("write was %s %s, want PUT /api/v3/movie/2", writes[0].method, writes[0].path)
	}

	out := stdout.String()
	if !strings.Contains(out, `title="A Completely Different 4K Movie"`) {
		t.Errorf("expected the 4K movie to be the one written:\n%s", out)
	}
	if strings.Contains(out, "HD Movie") {
		t.Errorf("the unnamed instance's library must not appear anywhere in the report:\n%s", out)
	}
}

// TestRun_InstanceFlag_UnknownName_IsFatal covers the typo. Skipping every
// instance because none matched would be a silent no-op, and a run that
// quietly does nothing is indistinguishable from one that found nothing to
// do.
func TestRun_InstanceFlag_UnknownName_IsFatal(t *testing.T) {
	fake := newRadarrFake(t,
		"["+libraryMovie(1, "The Only Movie", true, true)+"]",
		map[int]string{1: monitoredMovieDetail(1, "The Only Movie")})

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", writeTestConfig(t, fake.srv.URL, false), "--once", "--instance", "radarr-typo"}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2 for an --instance name that is not configured\nstderr=%s", code, stderr.String())
	}
	if got := fake.all(); len(got) != 0 {
		t.Errorf("the run contacted an instance before refusing: %+v", got)
	}
	if !strings.Contains(stderr.String(), "radarr-typo") {
		t.Errorf("the error must name the instance that was not found:\n%s", stderr.String())
	}
}

// TestRun_OnlyID_SingleRadarrInstance_NeedsNoInstanceFlag is the regression
// guard on the common case: one radarr configured means --only-id is
// unambiguous on its own, and adding a required flag there would be a
// gratuitous break.
func TestRun_OnlyID_SingleRadarrInstance_NeedsNoInstanceFlag(t *testing.T) {
	fake := newRadarrFake(t,
		"["+libraryMovie(1, "The Only Movie", true, true)+"]",
		map[int]string{1: monitoredMovieDetail(1, "The Only Movie")})

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", writeTestConfig(t, fake.srv.URL, false), "--once", "--only-id", "1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if n := len(fake.puts()); n != 1 {
		t.Errorf("expected the single configured radarr to still be written without --instance, got %d PUTs", n)
	}
}

// TestRun_OnlyID_OneRadarrAlongsideSonarr_NeedsNoInstanceFlag pins that the
// ambiguity is about radarr instances specifically: --only-id is a radarr
// movie id in this phase, so a sonarr instance sharing the config does not
// make it ambiguous.
func TestRun_OnlyID_OneRadarrAlongsideSonarr_NeedsNoInstanceFlag(t *testing.T) {
	fake := newRadarrFake(t,
		"["+libraryMovie(1, "The Only Movie", true, true)+"]",
		map[int]string{1: monitoredMovieDetail(1, "The Only Movie")})

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
`, fake.srv.URL, fake.srv.URL)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path, "--once", "--only-id", "1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if n := len(fake.puts()); n != 1 {
		t.Errorf("expected exactly 1 PUT (one radarr in scope), got %d", n)
	}
}

// TestRun_WriteMode_SonarrInstanceIsNeverWritten pins the phase boundary:
// Sonarr writes arrive in Phase 7, so a sonarr instance must still be
// connectivity-only even with dry_run false — the library inspection, the
// decision engine, and the write pass are all radarr-only in this phase.
//
// This test used to pass --only-id alongside the sonarr config and assert
// nothing but exit 0 and zero writes, which quietly certified the very
// silence the review found: the flag named a movie id no instance in the run
// could act on, and the run said nothing about it. The id belongs to the
// scope tests above, which now assert the refusal; what is left here is the
// phase boundary itself, unscoped.
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
	code := run([]string{"--config", path, "--once"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}

	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("a sonarr instance must never be written in this phase, got %+v", writes)
	}
	// Connectivity-only means exactly that: the connectivity check's own two
	// endpoints (/system/status, /qualityprofile) and nothing further. The
	// movie endpoints a radarr run walks — including the write path's
	// /movie/{id} — must never be requested for a sonarr in this phase.
	for _, r := range fake.all() {
		if strings.HasPrefix(r.path, "/api/v3/movie") {
			t.Errorf("a sonarr instance is connectivity-only in this phase, but %s %s was requested", r.method, r.path)
		}
	}
}

// --- --only-id must never be silently ignored -------------------------------
//
// REVIEW FIX (phase-4 round 2, IMPORTANT): --only-id was dropped without a
// word whenever no radarr instance ended up in scope — an all-sonarr config,
// or "--only-id 5 --instance sonarr-main". radarrInstancesInScope returns
// empty, len(inScope) > 1 is false so the ambiguity guard stays silent, the
// loop does connectivity-only work, and run() exits 0 having said nothing
// whatsoever about the movie id the human explicitly named.
//
// That is the exact defect class this file already declares unacceptable
// twice over: an unknown --instance is fatal because "a typo would otherwise
// produce a run that silently does nothing, which is indistinguishable from a
// run that found nothing to do", and --only-id without --once warns for the
// same reason. A --only-id that cannot possibly name anything this run would
// process is now fatal too, and says which of the two causes it is.

// TestRun_OnlyID_NoRadarrConfigured_IsFatalNotSilent: every configured
// instance is a sonarr, so the movie id names nothing. Nothing is contacted
// at all — the refusal happens before any HTTP call, like the ambiguity
// guard next to it.
func TestRun_OnlyID_NoRadarrConfigured_IsFatalNotSilent(t *testing.T) {
	fake := newRadarrFake(t, "[]", nil)

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
	code := run([]string{"--config", path, "--once", "--only-id", "7"}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2: a --only-id no instance could act on must be a fatal flag error\nstdout=%s\nstderr=%s",
			code, stdout.String(), stderr.String())
	}
	msg := stderr.String()
	for _, want := range []string{"--only-id", "7", "radarr"} {
		if !strings.Contains(msg, want) {
			t.Errorf("stderr must name %q so the human knows which id was refused and why:\n%s", want, msg)
		}
	}
	if reqs := fake.all(); len(reqs) != 0 {
		t.Errorf("the refusal must happen before anything is contacted, got %d request(s): %+v", len(reqs), reqs)
	}
}

// TestRun_OnlyID_InstanceFlagNamesASonarr_IsFatalNotSilent is the other
// route to an empty scope: a radarr IS configured, but --instance points the
// run at the sonarr beside it, so both filters together leave nothing the id
// could apply to.
func TestRun_OnlyID_InstanceFlagNamesASonarr_IsFatalNotSilent(t *testing.T) {
	fake := newRadarrFake(t,
		"["+libraryMovie(5, "Untouched Movie", true, true)+"]",
		map[int]string{5: monitoredMovieDetail(5, "Untouched Movie")})

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
`, fake.srv.URL, fake.srv.URL)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path, "--once", "--only-id", "5", "--instance", "sonarr-main"}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2: --only-id scoped to a sonarr names nothing and must not run silently\nstdout=%s\nstderr=%s",
			code, stdout.String(), stderr.String())
	}
	msg := stderr.String()
	for _, want := range []string{"--only-id", "5", "sonarr-main"} {
		if !strings.Contains(msg, want) {
			t.Errorf("stderr must name %q:\n%s", want, msg)
		}
	}
	// Movie 5 exists in the radarr's library. Refusing rather than quietly
	// widening scope is the whole point: the run must not fall back to the
	// instance the human did not name.
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("a refused run must write nothing anywhere, got %+v", writes)
	}
}
