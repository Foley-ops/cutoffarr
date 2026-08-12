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

	written, err := unmonitorMovie(context.Background(), logger, client, inst, 7, 0, false, true)
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

	written, err := unmonitorMovie(context.Background(), logger, client, inst, 7, 0, false, false)
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

	if _, err := unmonitorMovie(context.Background(), logger, client, inst, 7, 0, false, false); err != nil {
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

	if _, err := unmonitorMovie(context.Background(), logger, client, inst, 7, 0, false, false); err != nil {
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

// --- pre-write exclusion-tag re-check (Phase 5, mandated write-path safety
// fix) --------------------------------------------------------------------
//
// The scan that produced a would-unmonitor decision may be minutes old, and
// §2.5 says the exclusion tag always wins, in every mode. These tests pin
// the write path's own re-check of the FRESH pre-write fetch's tags,
// independent of whatever the scan-time decision believed.
//
// Every refusal branch below returns a non-nil error wrapping a sentinel
// (errExcludedAtWrite or errTagsUnverifiable, both declared in writer.go) —
// not (false, nil) — precisely so runWritePass can tell a pre-write tag
// refusal apart from every other reason a would-unmonitor decision produced
// no write, and count it (REVIEW FIX, Phase 5 round 2: the three refusals
// below used to be invisible in the "radarr decision summary" line; see
// TestRunRadarrDecisionEngine_ExclusionTagAddedBetweenScanAndWrite_RefusesTheWrite
// and the writesRefused tests in decision_test.go for the counted,
// end-to-end half of this fix).

// TestUnmonitorMovie_FreshPayloadCarriesExclusionTag_RefusesToWriteWithDistinctReason
// is the core case: the tag was added to this movie after the scan that
// produced the would-unmonitor decision, but before the write pass reached
// it. The write must be refused, and the reason must be distinguishable
// from "already unmonitored" (a different race, caught by a different
// check).
func TestUnmonitorMovie_FreshPayloadCarriesExclusionTag_RefusesToWriteWithDistinctReason(t *testing.T) {
	body := `{"id": 7, "title": "Newly Tagged Movie", "monitored": true, "tags": [3, 9]}`
	srv, got := writerTestServer(t, body, http.StatusOK)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := writerTestInstance(srv.URL)
	client := NewAPIClient(inst.URL, inst.APIKey)

	written, err := unmonitorMovie(context.Background(), logger, client, inst, 7, 9, true, false)
	if !errors.Is(err, errExcludedAtWrite) {
		t.Fatalf("unmonitorMovie error = %v, want errExcludedAtWrite so runWritePass can count this refusal:\n%s", err, buf.String())
	}
	if written {
		t.Error("written = true, want false: the exclusion tag must always win, even when discovered at write time")
	}
	if len(filterRequests(*got, http.MethodPut)) != 0 {
		t.Errorf("expected zero PUTs when the fresh payload carries the exclusion tag, got %+v", *got)
	}
	out := buf.String()
	if !strings.Contains(out, "level=INFO") || !strings.Contains(out, "exclusion tag") {
		t.Errorf("expected an info log explaining the write was refused due to the exclusion tag:\n%s", out)
	}
	if strings.Contains(out, "already unmonitored") {
		t.Errorf("the exclusion-tag skip must be distinguishable from the already-unmonitored skip:\n%s", out)
	}
	if !strings.Contains(out, `title="Newly Tagged Movie"`) {
		t.Errorf("expected the log to name the movie:\n%s", out)
	}
}

// TestUnmonitorMovie_FreshPayloadTagsAbsent_TagActive_RefusesToWriteWithWarn
// is the first untrusted-input case: "tags" entirely missing from the fresh
// pre-write fetch (as opposed to present-but-empty) means our assumed field
// name may not match this Radarr version, and — consistent with the
// decision-side rule 4 treatment of the same absence (decision.go FIX 1) —
// the write must be refused rather than risk writing over a movie the
// exclusion tag actually covers.
func TestUnmonitorMovie_FreshPayloadTagsAbsent_TagActive_RefusesToWriteWithWarn(t *testing.T) {
	body := `{"id": 7, "title": "No Tags Key", "monitored": true}`
	srv, got := writerTestServer(t, body, http.StatusOK)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := writerTestInstance(srv.URL)
	client := NewAPIClient(inst.URL, inst.APIKey)

	written, err := unmonitorMovie(context.Background(), logger, client, inst, 7, 9, true, false)
	if !errors.Is(err, errTagsUnverifiable) {
		t.Fatalf("unmonitorMovie error = %v, want errTagsUnverifiable so runWritePass can count this refusal:\n%s", err, buf.String())
	}
	if written {
		t.Error("written = true, want false")
	}
	if len(filterRequests(*got, http.MethodPut)) != 0 {
		t.Errorf("expected zero PUTs when tags cannot be verified, got %+v", *got)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "tags") {
		t.Errorf("expected a warning explaining the write was refused because tags could not be verified:\n%s", out)
	}
}

// TestUnmonitorMovie_FreshPayloadTagsNull_TagActive_RefusesToWriteWithWarn is
// the second untrusted-input case, and the one the review round found this
// re-check originally got wrong: "tags": null decodes successfully (nil
// error) into a nil []int with NO error, so a check that only asked "did the
// key decode without error" would treat null as "present and empty" and let
// the write proceed. The decision side treats the identical wire shape as
// untrusted input — movieListElement.Tags is *[]int (radarr.go), so "tags":
// null leaves m.Tags == nil and decision.go rule 4 returns ReasonTagsUnknown
// — so this re-check must refuse here too, exactly like the tags-absent case
// above, not silently fall through to "no exclusion tag found".
func TestUnmonitorMovie_FreshPayloadTagsNull_TagActive_RefusesToWriteWithWarn(t *testing.T) {
	body := `{"id": 7, "title": "Null Tags", "monitored": true, "tags": null}`
	srv, got := writerTestServer(t, body, http.StatusOK)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := writerTestInstance(srv.URL)
	client := NewAPIClient(inst.URL, inst.APIKey)

	written, err := unmonitorMovie(context.Background(), logger, client, inst, 7, 9, true, false)
	if !errors.Is(err, errTagsUnverifiable) {
		t.Fatalf("unmonitorMovie error = %v, want errTagsUnverifiable: a JSON null tags value must be treated the same as an absent tags key, not as \"present and empty\":\n%s", err, buf.String())
	}
	if written {
		t.Error("written = true, want false: a JSON null tags value must never authorize a write when the exclusion tag is active")
	}
	if len(filterRequests(*got, http.MethodPut)) != 0 {
		t.Errorf("expected zero PUTs when tags is JSON null, got %+v", *got)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "tags") {
		t.Errorf("expected a warning explaining the write was refused because tags could not be verified:\n%s", out)
	}
}

// TestUnmonitorMovie_FreshPayloadTagsMalformed_TagActive_RefusesToWriteWithWarn
// is the third untrusted-input case: "tags" present but not a JSON array of
// ids at all (e.g. the field renamed to an object in some future Radarr
// version). json.Unmarshal into []int fails outright here, unlike the null
// case, but the outcome must be the same refusal, for the same reason.
func TestUnmonitorMovie_FreshPayloadTagsMalformed_TagActive_RefusesToWriteWithWarn(t *testing.T) {
	body := `{"id": 7, "title": "Malformed Tags", "monitored": true, "tags": {"unexpected": "shape"}}`
	srv, got := writerTestServer(t, body, http.StatusOK)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := writerTestInstance(srv.URL)
	client := NewAPIClient(inst.URL, inst.APIKey)

	written, err := unmonitorMovie(context.Background(), logger, client, inst, 7, 9, true, false)
	if !errors.Is(err, errTagsUnverifiable) {
		t.Fatalf("unmonitorMovie error = %v, want errTagsUnverifiable:\n%s", err, buf.String())
	}
	if written {
		t.Error("written = true, want false")
	}
	if len(filterRequests(*got, http.MethodPut)) != 0 {
		t.Errorf("expected zero PUTs when tags is not a JSON array of ids, got %+v", *got)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "tags") {
		t.Errorf("expected a warning explaining the write was refused because tags could not be verified:\n%s", out)
	}
}

// TestUnmonitorMovie_FreshPayloadTagsContainNullElement_TagActive_RefusesToWriteWithWarn
// pins the null-ARRAY-ELEMENT corner (REVIEW FIX, Phase 6, carried forward
// from the phase-5 branch review): "tags": [3, null, 9] decodes cleanly into
// a plain []int with NO error at all — encoding/json leaves a non-pointer
// destination at its zero value when it meets JSON null — silently turning
// the null into tag id 0. Before decodeTagIDs (shared.go) existed, that
// meant a corrupted tags array could slip straight past both the
// "tagsPresent" and "tags == nil" guards and be compared to the exclusion
// tag id as if id 0 were a real, observed tag. Distinct from the
// already-covered "tags is not a JSON array at all" case just above: this
// array decodes structurally fine and only one element is the problem.
func TestUnmonitorMovie_FreshPayloadTagsContainNullElement_TagActive_RefusesToWriteWithWarn(t *testing.T) {
	body := `{"id": 7, "title": "Null Tag Element", "monitored": true, "tags": [3, null, 9]}`
	srv, got := writerTestServer(t, body, http.StatusOK)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := writerTestInstance(srv.URL)
	client := NewAPIClient(inst.URL, inst.APIKey)

	written, err := unmonitorMovie(context.Background(), logger, client, inst, 7, 9, true, false)
	if !errors.Is(err, errTagsUnverifiable) {
		t.Fatalf("unmonitorMovie error = %v, want errTagsUnverifiable:\n%s", err, buf.String())
	}
	if written {
		t.Error("written = true, want false")
	}
	if len(filterRequests(*got, http.MethodPut)) != 0 {
		t.Errorf("expected zero PUTs when tags contains a JSON null element, got %+v", *got)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "null") {
		t.Errorf("expected a warning explaining the write was refused because a tag element was JSON null:\n%s", out)
	}
}

// TestUnmonitorMovie_TagActive_TagsPresentButNotExcluded_WritesNormally is
// the mirror the review round found missing: every other tagActive=true test
// in this suite proves a refusal, and none proves the re-check PERMITTING a
// write. Without this, a bug that made the re-check refuse unconditionally
// whenever tagActive is true (e.g. containsIntID inverted, or the
// `if containsIntID(...)` block's early return escaping unconditionally)
// would sail through every test in this file undetected, because nothing
// here would ever expect a PUT while tagActive is true. This is the
// production-shaped case: the exclusion tag IS defined and active in the
// instance, the fresh payload DOES carry tags, and none of them is the
// exclusion tag id — so the write must proceed exactly as if the tag were
// inactive.
func TestUnmonitorMovie_TagActive_TagsPresentButNotExcluded_WritesNormally(t *testing.T) {
	body := `{"id": 7, "title": "Untagged", "monitored": true, "tags": [3]}`
	srv, got := writerTestServer(t, body, http.StatusOK)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := writerTestInstance(srv.URL)
	client := NewAPIClient(inst.URL, inst.APIKey)

	written, err := unmonitorMovie(context.Background(), logger, client, inst, 7, 9, true, false)
	if err != nil {
		t.Fatalf("unmonitorMovie returned error: %v\n%s", err, buf.String())
	}
	if !written {
		t.Errorf("written = false, want true: tags are present and verifiable and none of them is the exclusion tag, so the write must proceed:\n%s", buf.String())
	}
	puts := filterRequests(*got, http.MethodPut)
	if len(puts) != 1 {
		t.Fatalf("expected exactly 1 PUT, got %d: %+v", len(puts), *got)
	}
}

// TestUnmonitorMovie_ExclusionTagInactive_TagsIgnored_WritesNormally pins
// the other half: when the exclusion tag is not active in this instance
// (rule 4 is vacuous, per the tag-resolution rules), the pre-write re-check
// must not block anything, regardless of what tags the fresh payload
// carries.
func TestUnmonitorMovie_ExclusionTagInactive_TagsIgnored_WritesNormally(t *testing.T) {
	srv, _ := writerTestServer(t, `{"id": 7, "title": "Tagged But Inactive", "monitored": true, "tags": [9]}`, http.StatusOK)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := writerTestInstance(srv.URL)
	client := NewAPIClient(inst.URL, inst.APIKey)

	written, err := unmonitorMovie(context.Background(), logger, client, inst, 7, 9, false, false)
	if err != nil {
		t.Fatalf("unmonitorMovie returned error: %v\n%s", err, buf.String())
	}
	if !written {
		t.Errorf("written = false, want true: the exclusion tag is not active in this instance, so it must not block the write:\n%s", buf.String())
	}
}

// TestUnmonitorMovie_FreshGetShowsAlreadyUnmonitored_NoPut covers the
// scan-to-write race: the movie was monitored when the library was scanned
// but is monitored:false by the time the write pass reaches it. Changing
// nothing is then the correct action (§2.4's spirit: change exactly one
// thing, and only when it needs changing).
//
// REVIEW FIX (Phase 5 round 4): this race used to return (false, nil), the
// same shape runWritePass treats as "nothing to count" for the dry-run-
// withheld case, so it went completely uncounted in the summary. It now
// wraps errAlreadyUnmonitoredAtWrite so runWritePass can count it under its
// own name — exactly as errExcludedAtWrite/errTagsUnverifiable already do for
// the pre-write tag re-check's refusals — instead of a (false, nil) return
// that could not be told apart from a run that made no decision at all.
func TestUnmonitorMovie_FreshGetShowsAlreadyUnmonitored_NoPut(t *testing.T) {
	srv, got := writerTestServer(t, `{"id": 7, "title": "Already Done", "monitored": false}`, http.StatusOK)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := writerTestInstance(srv.URL)
	client := NewAPIClient(inst.URL, inst.APIKey)

	written, err := unmonitorMovie(context.Background(), logger, client, inst, 7, 0, false, false)
	if !errors.Is(err, errAlreadyUnmonitoredAtWrite) {
		t.Fatalf("unmonitorMovie error = %v, want errAlreadyUnmonitoredAtWrite so runWritePass can count this race:\n%s", err, buf.String())
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
//
// REVIEW FIX (Phase 5 final round): the refusal is now a WARN plus an error
// wrapping errMonitoredUnverifiable, exactly like the "tags" untrusted-input
// branches above it. It used to be a bare error, which runWritePass could only
// classify as a failed WRITE — an ERROR line reading "unmonitor write failed"
// about a movie no PUT was ever sent for, and a writeErrors count that says
// Radarr rejected something it never saw. It is a refusal, in the same class
// as an unverifiable "tags", and it is counted as one (writesRefused).
func TestUnmonitorMovie_MonitoredFieldAbsent_RefusesToWrite(t *testing.T) {
	srv, got := writerTestServer(t, `{"id": 7, "title": "No Monitored Key"}`, http.StatusOK)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := writerTestInstance(srv.URL)
	client := NewAPIClient(inst.URL, inst.APIKey)

	written, err := unmonitorMovie(context.Background(), logger, client, inst, 7, 0, false, false)
	if err == nil {
		t.Fatal("unmonitorMovie returned nil error, want an error when monitored is absent")
	}
	if !errors.Is(err, errMonitoredUnverifiable) {
		t.Fatalf("unmonitorMovie error = %v, want errMonitoredUnverifiable so runWritePass counts a refusal rather than a failed write:\n%s", err, buf.String())
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
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "monitored") {
		t.Errorf("expected a warning naming the field that could not be verified:\n%s", out)
	}
}

// TestUnmonitorMovie_FreshPayloadMonitoredNull_RefusesToWrite is the
// JSON-null trap on "monitored" itself — the one field the entire write
// path pivots on — mirroring
// TestUnmonitorMovie_FreshPayloadTagsNull_TagActive_RefusesToWriteWithWarn's
// coverage of the identical wire shape on "tags". A plain
// `var monitored bool; json.Unmarshal(rawMonitored, &monitored)` decodes the
// JSON literal null into monitored == false with NO decode error —
// indistinguishable from a genuine monitored:false — which would make the
// very next branch (the already-unmonitored skip) log "already unmonitored,
// skipping write" about a movie whose state was never actually observed, and
// leave it permanently unmonitorable on every future cycle too. §2.6: never
// guess — refuse the write instead, the same class of refusal as "monitored"
// being entirely absent from the fetch.
func TestUnmonitorMovie_FreshPayloadMonitoredNull_RefusesToWrite(t *testing.T) {
	srv, got := writerTestServer(t, `{"id": 7, "title": "Null Monitored", "monitored": null}`, http.StatusOK)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := writerTestInstance(srv.URL)
	client := NewAPIClient(inst.URL, inst.APIKey)

	written, err := unmonitorMovie(context.Background(), logger, client, inst, 7, 0, false, false)
	if err == nil {
		t.Fatal("unmonitorMovie returned nil error, want an error when monitored is JSON null: its true state cannot be trusted")
	}
	if !errors.Is(err, errMonitoredUnverifiable) {
		t.Fatalf("unmonitorMovie error = %v, want errMonitoredUnverifiable so runWritePass counts a refusal rather than a failed write:\n%s", err, buf.String())
	}
	if written {
		t.Error("written = true, want false")
	}
	if !strings.Contains(err.Error(), "monitored") || !strings.Contains(err.Error(), "null") {
		t.Errorf("error %q does not name the field and the null shape that made it unverifiable", err.Error())
	}
	if len(filterRequests(*got, http.MethodPut)) != 0 {
		t.Errorf("expected zero PUTs when monitored is JSON null, got %+v", *got)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "monitored") {
		t.Errorf("expected a warning naming the field that could not be verified:\n%s", out)
	}
	if strings.Contains(out, "already unmonitored") {
		t.Errorf("a null monitored value must never be reported as an observed already-unmonitored state:\n%s", out)
	}
}

// TestUnmonitorMovie_FreshPayloadMonitoredNotBoolean_RefusesToWrite is the
// third shape of the same untrusted-input class: "monitored" present but not
// a boolean at all (a string, an object, a number). The decode fails outright
// here, unlike the null case, but the outcome must be the same refusal — and,
// like its two siblings, one runWritePass can count as a refusal rather than
// misreport as a failed write.
func TestUnmonitorMovie_FreshPayloadMonitoredNotBoolean_RefusesToWrite(t *testing.T) {
	srv, got := writerTestServer(t, `{"id": 7, "title": "Stringly Monitored", "monitored": "true"}`, http.StatusOK)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := writerTestInstance(srv.URL)
	client := NewAPIClient(inst.URL, inst.APIKey)

	written, err := unmonitorMovie(context.Background(), logger, client, inst, 7, 0, false, false)
	if !errors.Is(err, errMonitoredUnverifiable) {
		t.Fatalf("unmonitorMovie error = %v, want errMonitoredUnverifiable:\n%s", err, buf.String())
	}
	if written {
		t.Error("written = true, want false")
	}
	if len(filterRequests(*got, http.MethodPut)) != 0 {
		t.Errorf("expected zero PUTs when monitored is not a boolean, got %+v", *got)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning explaining the refusal:\n%s", buf.String())
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

	written, err := unmonitorMovie(context.Background(), logger, client, inst, 7, 0, false, false)
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

	written, err := unmonitorMovie(context.Background(), logger, client, inst, 7, 0, false, false)
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

	if _, err := unmonitorMovie(context.Background(), logger, client, inst, 7, 0, false, false); err == nil {
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

	written, err := unmonitorMovie(context.Background(), logger, client, inst, 7, 0, false, false)
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

	written, err := unmonitorMovie(context.Background(), logger, client, inst, 7, 0, false, false)
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

	written, err := unmonitorMovie(context.Background(), logger, client, inst, 7, 0, false, false)
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

	written, err := unmonitorMovie(context.Background(), logger, client, inst, 7, 0, false, false)
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

	written, err := unmonitorMovie(context.Background(), logger, client, inst, 7, 0, false, false)
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
// The echo's own identity is part of that: an echo carrying a DIFFERENT
// movie's object (a proxy, redirect, or cache answering the write with
// something else) says nothing whatsoever about the movie we asked to change,
// so it belongs in the unverifiable class too — REVIEW FIX (Phase 5 final
// round). Without the check, `{"id": 99, "monitored": false}` came back from a
// PUT to /movie/7 and was accepted as movie 7's confirmed write: unmonitored++
// and msg=unmonitor id=7, on the strength of an object about movie 99. The
// pre-write GET has been guarded against exactly this since Phase 4
// (verifyMovieIdentity); the echo is the ONLY evidence this project accepts
// that a write landed, so it must clear the same bar.
func TestVerifyWriteEcho_SeparatesUnverifiableFromContradicted(t *testing.T) {
	unverifiable := []struct{ name, echo string }{
		{"empty body", ""},
		{"not a JSON object", `[{"id": 7}]`},
		{"no monitored key", `{"id": 7, "title": "Silent"}`},
		{"monitored is not a boolean", `{"id": 7, "monitored": "false"}`},
		{"monitored is JSON null", `{"id": 7, "monitored": null}`},
		{"echo is a different movie", `{"id": 99, "title": "Wrong Movie", "monitored": false}`},
		{"echo carries no id", `{"title": "Anonymous", "monitored": false}`},
		{"echo id is not a number", `{"id": "seven", "monitored": false}`},
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

	if _, err := unmonitorMovie(context.Background(), logger, client, inst, 7, 0, false, false); err != nil {
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

	// Everything above this line is equally true of a run that never reached
	// the write pass at all: zero writes, both report lines, all three
	// counters zero. So a fixture drifting (losing qualityCutoffNotMet, say)
	// would quietly downgrade the project's most important test to "a run
	// that never tried to write made no writes" — still green, and no longer
	// testing anything. The three assertions below are what make the claim
	// "the gate opened and the write path was walked" part of the test.
	if !strings.Contains(out, `crossCheck="passed`) {
		t.Errorf("the cross-check must PASS here, or the write pass was never entered and this test proves nothing:\n%s", out)
	}
	if strings.Contains(out, "writes withheld") {
		t.Errorf("the write gate blocked this run, so zero writes says nothing about dry-run:\n%s", out)
	}
	// The pre-write fetch is the write path's own first step, and it runs on
	// both sides of the dry-run gate (§2.1: the gate is immediately before
	// the PUT, not at the top of the function). Seeing one per would-unmonitor
	// movie is the evidence that dry-run rehearsed the write rather than
	// skipping it.
	for _, want := range []string{"/api/v3/movie/1", "/api/v3/movie/2"} {
		found := false
		for _, r := range fake.all() {
			if r.method == http.MethodGet && r.path == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no pre-write GET %s: the write pass did not reach the dry-run gate for this movie:\n%+v", want, fake.all())
		}
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

// TestRun_OnlyID_RadarrAlongsideSonarr_RequiresInstanceFlag pins the Phase 7
// widening of the ambiguity guard (binding controller resolution 4). While
// --only-id was a radarr movie id, a sonarr sharing the config made nothing
// ambiguous and this same invocation was expected to succeed. Now --only-id
// names a radarr MOVIE or a sonarr SERIES depending on which instance is in
// scope, so "--only-id 1" against one of each names two unrelated things and
// must be refused — before anything is contacted, and with the remedy stated.
func TestRun_OnlyID_RadarrAlongsideSonarr_RequiresInstanceFlag(t *testing.T) {
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
	if code != 2 {
		t.Fatalf("exit code = %d, want 2: one radarr and one sonarr in scope makes an id ambiguous\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	if reqs := fake.all(); len(reqs) != 0 {
		t.Errorf("the refusal must happen before anything is contacted, got %d request(s)", len(reqs))
	}
	if !strings.Contains(stderr.String(), "--instance") {
		t.Errorf("the refusal must name the flag that resolves it:\n%s", stderr.String())
	}
}

// TestRun_WriteMode_SonarrInstance_TouchesOnlyItsAllowlistedPaths is the
// phase boundary, restated where Phase 7 moved it.
//
// Through Phase 6 this test asserted a sonarr was never written at all, and
// did it by rejecting requests whose path began with "/api/v3/movie" — a loop
// that could not fail for a sonarr and quietly certified a claim
// ("connectivity-only") that had stopped being true two phases earlier. It was
// converted to an explicit ALLOWLIST so that Phase 7's first sonarr write
// would trip it deliberately rather than slip past it, and this is that
// deliberate update: the allowlist now names the two write endpoints as well,
// and nothing else may appear.
//
// The point is unchanged and is the reason to keep paying for it: the exact
// set of endpoints a write-mode sonarr run touches is a decision, not an
// accident, and any new one — a /command refresh, a /queue poll, a second
// write verb — has to be argued for here before it can happen anywhere.
func TestRun_WriteMode_SonarrInstance_TouchesOnlyItsAllowlistedPaths(t *testing.T) {
	fake := writableSonarrFake(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", writeSonarrTestConfig(t, fake.srv.URL, false, "info"), "--once"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}

	// Reads: connectivity (system/status, qualityprofile), the library scan
	// (series, wanted/cutoff), the decision engine (qualityprofile again, tag,
	// episode, episodefile), and the write path's own fresh series read.
	allowedGET := map[string]bool{
		"/api/v3/system/status":  true,
		"/api/v3/qualityprofile": true,
		"/api/v3/series":         true,
		"/api/v3/series/1":       true,
		"/api/v3/wanted/cutoff":  true,
		"/api/v3/tag":            true,
		"/api/v3/episode":        true,
		"/api/v3/episodefile":    true,
	}
	// Writes: exactly the two §2.2 permits for Sonarr, and no other method.
	allowedPUT := map[string]bool{
		"/api/v3/episode/monitor": true,
		"/api/v3/series/1":        true,
	}
	// fake.all(), not fake.requests: the slice is appended to by handler
	// goroutines under the fake's mutex, and this was the suite's one
	// unsynchronized read of it.
	for _, r := range fake.all() {
		switch r.method {
		case http.MethodGet:
			if !allowedGET[r.path] {
				t.Errorf("unexpected GET %s: a sonarr run's read surface is an allowlist, not a default", r.path)
			}
		case http.MethodPut:
			if !allowedPUT[r.path] {
				t.Errorf("unexpected PUT %s: §2.2 permits exactly two Sonarr writes", r.path)
			}
		default:
			t.Errorf("unexpected %s %s: no verb but GET and PUT may ever be sent", r.method, r.path)
		}
	}
}

// TestTree_HasExactlyThreeWriteVerbCallSites is the write-verb audit (binding
// controller resolution 7), pinned the way earlier phases pinned "exactly one
// PUT": by counting, so that adding a fourth write site anywhere in the tree
// is a decision someone has to come here and make.
//
// Two invariants, together, bound the whole program's write surface:
//
//   - DoJSON is the only client entry point that carries a request body, and
//     every write in this project goes through it. There are exactly three
//     call sites: the Radarr movie PUT, the Sonarr episode-monitor PUT, and the
//     Sonarr season PUT — each in a write-path file, each immediately behind
//     its own dry-run gate.
//   - A write could also be smuggled through the bodyless client.Do, so no
//     non-GET HTTP method constant may appear in ANY non-test file outside
//     those two write-path files. (client.go names no method at all; it takes
//     whichever its callers pass.)
func TestTree_HasExactlyThreeWriteVerbCallSites(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}

	wantCallSites := map[string]int{"writer.go": 1, "sonarr_writer.go": 2}
	writePathFiles := map[string]bool{"writer.go": true, "sonarr_writer.go": true}
	gotCallSites := map[string]int{}
	total := 0

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		body := string(src)

		// ".DoJSON(", not "client.DoJSON(": the guarantee is about the write
		// verb, not about what a variable happens to be named, and an innocent
		// receiver rename (c := client; c.DoJSON(...)) made a fourth write site
		// invisible to the count that is supposed to be the tree's write-surface
		// audit. The method name is the thing that cannot be renamed without
		// changing client.go.
		if n := strings.Count(body, ".DoJSON("); n > 0 {
			gotCallSites[name] = n
			total += n
		}
		if writePathFiles[name] {
			continue
		}
		for _, verb := range []string{"http.MethodPut", "http.MethodPost", "http.MethodDelete", "http.MethodPatch"} {
			if strings.Contains(body, verb) {
				t.Errorf("%s names %s: outside the write-path files, this project issues reads only", name, verb)
			}
		}
	}

	if total != 3 {
		t.Errorf("found %d write-verb call sites, want exactly 3: %v", total, gotCallSites)
	}
	for file, want := range wantCallSites {
		if gotCallSites[file] != want {
			t.Errorf("%s has %d write-verb call sites, want %d", file, gotCallSites[file], want)
		}
	}
	for file := range gotCallSites {
		if !writePathFiles[file] {
			t.Errorf("%s issues a write: every write call site must live in a write-path file", file)
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

// TestRun_OnlyID_SingleSonarrConfigured_NamesASeries is the other half of the
// same widening. An all-sonarr config with --only-id used to be FATAL ("the id
// names nothing this run could act on") because the flag was radarr-only.
// Phase 7 gives it a Sonarr meaning, so the very same invocation is now a
// perfectly ordinary scoped run — and must actually scope it: the named series
// is evaluated, reported and written, and nothing else is.
func TestRun_OnlyID_SingleSonarrConfigured_NamesASeries(t *testing.T) {
	fake := newStatefulSonarrFake(t,
		[]*statefulSonarrSeries{
			{id: 7, title: "Named Series", monitored: true, profileID: 1, tags: []int{},
				seasons: []statefulSonarrSeason{{number: 1, monitored: true, episodeFileCount: 1, totalEpisodeCount: 1}}},
			{id: 8, title: "Other Series", monitored: true, profileID: 1, tags: []int{},
				seasons: []statefulSonarrSeason{{number: 1, monitored: true, episodeFileCount: 1, totalEpisodeCount: 1}}},
		},
		[]*statefulSonarrEpisode{
			{id: 700, seriesID: 7, seasonNumber: 1, episodeNumber: 1, monitored: true, hasFile: true, airDateUtc: pastAirDate, episodeFileID: 70},
			{id: 800, seriesID: 8, seasonNumber: 1, episodeNumber: 1, monitored: true, hasFile: true, airDateUtc: pastAirDate, episodeFileID: 80},
		},
		[]*statefulSonarrEpisodeFile{
			{id: 70, seasonNumber: 1, customFormatScore: 200, qualityCutoffNotMet: false},
			{id: 80, seasonNumber: 1, customFormatScore: 200, qualityCutoffNotMet: false},
		},
	)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", writeSonarrTestConfig(t, fake.srv.URL, false, "info"), "--once", "--only-id", "7"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0: --only-id now names a sonarr series\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	if fake.seasonMonitored(7, 1) {
		t.Error("the named series' season should have been unmonitored")
	}
	if !fake.seasonMonitored(8, 1) {
		t.Error("a series the run did not name must never be written")
	}
	for _, w := range fake.writes() {
		if w.path == "/api/v3/series/8" {
			t.Errorf("a --only-id run wrote a series it does not name: %+v", w)
		}
	}
}

// TestRun_OnlyID_NoInstancesConfigured_IsFatalNotSilent is the one remaining
// route to an empty scope now that an unknown --instance is already fatal and
// both instance types honor the flag: a config with nothing in it at all. The
// id could not be applied to anything, and a run that silently does nothing is
// indistinguishable from a run that found nothing to do.
func TestRun_OnlyID_NoInstancesConfigured_IsFatalNotSilent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte("dry_run: false\ninstances: []\n"), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path, "--once", "--only-id", "7"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"--only-id", "7"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr must name %q so the human knows which id was refused and why:\n%s", want, stderr.String())
		}
	}
}

// TestRun_OnlyID_InstanceFlagNamesASonarr_ScopesTheSonarrSeries is the third
// invocation the widening reverses. "--only-id 7 --instance sonarr-main"
// beside a configured radarr used to be fatal, because the id could only ever
// mean a movie; it now names series 7 in that sonarr, and the radarr beside it
// must not be contacted at all — "--instance" has to mean the run touches that
// instance and nothing else, or it would be a weaker statement than it looks.
func TestRun_OnlyID_InstanceFlagNamesASonarr_ScopesTheSonarrSeries(t *testing.T) {
	radarr := newRadarrFake(t,
		"["+libraryMovie(7, "Untouched Movie", true, true)+"]",
		map[int]string{7: monitoredMovieDetail(7, "Untouched Movie")})
	sonarr := newStatefulSonarrFake(t,
		[]*statefulSonarrSeries{
			{id: 7, title: "Scoped Series", monitored: true, profileID: 1, tags: []int{},
				seasons: []statefulSonarrSeason{{number: 1, monitored: true, episodeFileCount: 1, totalEpisodeCount: 1}}},
		},
		[]*statefulSonarrEpisode{
			{id: 700, seriesID: 7, seasonNumber: 1, episodeNumber: 1, monitored: true, hasFile: true, airDateUtc: pastAirDate, episodeFileID: 70},
		},
		[]*statefulSonarrEpisodeFile{
			{id: 70, seasonNumber: 1, customFormatScore: 200, qualityCutoffNotMet: false},
		},
	)

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
	code := run([]string{"--config", path, "--once", "--only-id", "7", "--instance", "sonarr-main"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	if sonarr.seasonMonitored(7, 1) {
		t.Errorf("the named sonarr series' season should have been unmonitored:\n%s", stdout.String())
	}
	if reqs := radarr.all(); len(reqs) != 0 {
		t.Errorf("the radarr was contacted despite --instance sonarr-main: %+v", reqs)
	}
}

// --- per-instance write isolation, at run() level -------------------------
//
// §2.6's house rule is "skip that instance for the cycle": one instance's
// trouble must never cost another instance its writes. The structure supports
// it (runRadarrDecisionEngine returns nothing and main.go's instance loop
// never breaks), and connectivity isolation is pinned in main_test.go, but
// nothing proved it for the WRITE pass — the property Phase 8's daemon loop
// depends on, since a permanently sick instance would otherwise silently
// freeze every other library forever. Both realistic causes are covered: an
// instance whose writes fail at the server, and one whose write pass never
// opens because its own cross-check refused to authorize it.

// countSummaries counts "radarr decision summary" lines naming the given
// instance. Exactly one per processed instance is the contract (brief item 1),
// so a test that only checked "the summary mentions radarr-4k" could not tell
// one summary from three.
func countSummaries(output, instance string) int {
	n := 0
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "radarr decision summary") && strings.Contains(line, "instance="+instance) {
			n++
		}
	}
	return n
}

// TestRun_WriteMode_OneInstanceFailsItsWrites_TheOtherStillWrites: radarr-hd
// (first in the config, so a loop that aborted would take radarr-4k with it)
// answers every PUT with 500. radarr-4k must still make its own PUT, and both
// instances must report their own summary line with their own counters.
func TestRun_WriteMode_OneInstanceFailsItsWrites_TheOtherStillWrites(t *testing.T) {
	hd := newRadarrFake(t,
		"["+libraryMovie(1, "HD Movie", true, true)+"]",
		map[int]string{1: monitoredMovieDetail(1, "HD Movie")})
	hd.putStatus[1] = http.StatusInternalServerError
	fourK := newRadarrFake(t,
		"["+libraryMovie(1, "4K Movie", true, true)+"]",
		map[int]string{1: monitoredMovieDetail(1, "4K Movie")})

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", writeTwoRadarrConfig(t, hd.srv.URL, fourK.srv.URL, false), "--once"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0: a per-instance write failure is not a process failure; stderr=%s", code, stderr.String())
	}

	if n := len(hd.puts()); n != 1 {
		t.Errorf("radarr-hd: made %d PUT attempts, want exactly 1 (attempted once, never retried)", n)
	}
	writes := fourK.writes()
	if len(writes) != 1 || writes[0].method != http.MethodPut || writes[0].path != "/api/v3/movie/1" {
		t.Fatalf("radarr-4k must still write normally after radarr-hd's write failed, got %+v", writes)
	}

	out := stdout.String()
	for _, inst := range []string{"radarr-hd", "radarr-4k"} {
		if n := countSummaries(out, inst); n != 1 {
			t.Errorf("expected exactly 1 radarr decision summary for %s, got %d:\n%s", inst, n, out)
		}
	}
	// Each instance's counters must describe that instance and nothing else:
	// a shared counter would show the failure on both lines, or the write on
	// both.
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "radarr decision summary") {
			continue
		}
		switch {
		case strings.Contains(line, "instance=radarr-hd"):
			if !strings.Contains(line, "writeErrors=1") || !strings.Contains(line, "unmonitored=0") {
				t.Errorf("radarr-hd's summary must carry its own failure: %s", line)
			}
		case strings.Contains(line, "instance=radarr-4k"):
			if !strings.Contains(line, "writeErrors=0") || !strings.Contains(line, "unmonitored=1") {
				t.Errorf("radarr-4k's summary must carry its own confirmed write, unaffected by radarr-hd: %s", line)
			}
		}
	}
}

// TestRun_WriteMode_OneInstanceGateBlocked_TheOtherStillWrites is the second
// cause, and the one a daemon loop is likeliest to meet: radarr-hd's library
// carries a cross-check disagreement, so its write pass is blocked entirely.
// That verdict is per-instance evidence about per-instance data and must not
// travel: radarr-4k's cross-check passed, so radarr-4k writes.
func TestRun_WriteMode_OneInstanceGateBlocked_TheOtherStillWrites(t *testing.T) {
	// qualityCutoffNotMet: true while the (empty) wanted/cutoff set says the
	// movie is not below cutoff — two Radarr code paths disagreeing.
	disagreeing := `{"id": 1, "title": "Disagreeing HD Movie", "monitored": true, "hasFile": true, "qualityProfileId": 1, "tags": [], "movieFile": {"id": 1, "qualityCutoffNotMet": true}}`
	hd := newRadarrFake(t, "["+disagreeing+"]", map[int]string{1: monitoredMovieDetail(1, "Disagreeing HD Movie")})
	fourK := newRadarrFake(t,
		"["+libraryMovie(1, "4K Movie", true, true)+"]",
		map[int]string{1: monitoredMovieDetail(1, "4K Movie")})

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", writeTwoRadarrConfig(t, hd.srv.URL, fourK.srv.URL, false), "--once"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}

	if writes := hd.writes(); len(writes) != 0 {
		t.Fatalf("radarr-hd's cross-check FAILED, so it must write nothing: %+v", writes)
	}
	writes := fourK.writes()
	if len(writes) != 1 || writes[0].method != http.MethodPut {
		t.Fatalf("radarr-4k's own cross-check passed, so its write must proceed; got %+v", writes)
	}

	out := stdout.String()
	for _, inst := range []string{"radarr-hd", "radarr-4k"} {
		if n := countSummaries(out, inst); n != 1 {
			t.Errorf("expected exactly 1 radarr decision summary for %s, got %d:\n%s", inst, n, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "radarr decision summary") {
			continue
		}
		switch {
		case strings.Contains(line, "instance=radarr-hd"):
			if !strings.Contains(line, "crossCheck=FAILED") || !strings.Contains(line, "unmonitored=0") {
				t.Errorf("radarr-hd's summary must state its own blocked pass: %s", line)
			}
		case strings.Contains(line, "instance=radarr-4k"):
			if !strings.Contains(line, `crossCheck="passed`) || !strings.Contains(line, "unmonitored=1") {
				t.Errorf("radarr-4k's summary must state its own passed cross-check and confirmed write: %s", line)
			}
		}
	}
}

// --- statefulRadarrFake: the Phase 5 no-op contract, machine-verified -----
//
// Every fake above this point is stateless: newRadarrFake's GET /movie and
// GET /movie/{id} answer from fixed fixtures for the life of the server, so
// a PUT that changes a movie's monitored flag is never reflected back by a
// later GET against the same fake. That is fine for Phase 4's tests (each
// one runs the pipeline once), but Phase 5's core acceptance criterion is
// specifically about what a SECOND run sees after a first run wrote
// something — "already unmonitored" items skipped silently at info, logged
// at debug — and that can only be proven against a fake whose GET responses
// actually reflect its own PUT history. statefulRadarrFake is that fake:
// its /movie, /movie/{id}, /moviefile, and /wanted/cutoff endpoints are all
// generated from a single map of live per-movie state, and its one write
// operation (PUT /movie/{id}) mutates that same state before echoing it
// back, exactly as unmonitorMovie expects Radarr to behave.

// statefulRadarrMovie is one movie's mutable state in the fake's small
// world. hasFile/qualityProfileID/tags/movieFileID/cfScore are fixed for
// the life of the fake (nothing in this project ever writes them); monitored
// is the one field cutoffarr's write path ever changes, and inWantedSet /
// qualityCutoffNotMet are set once at construction to make the movie a
// clean would-unmonitor candidate whose cross-check agrees.
type statefulRadarrMovie struct {
	id                  int
	title               string
	monitored           bool
	hasFile             bool
	qualityProfileID    int
	tags                []int
	movieFileID         int
	cfScore             int
	qualityCutoffNotMet bool
	inWantedSet         bool
}

// wouldUnmonitorStatefulMovie builds a statefulRadarrMovie shaped to pass
// every rule of the STRICT decision rule against decisionEngineProfilesJSON
// (id 1, cutoffFormatScore 100, upgradeAllowed) and to be verified (not
// merely sampled) by the cross-check: monitored, hasFile, no tags, absent
// from the wanted set, cfScore above the profile's cutoff, and
// qualityCutoffNotMet agreeing with wanted-set absence (false).
func wouldUnmonitorStatefulMovie(id int, title string) *statefulRadarrMovie {
	return &statefulRadarrMovie{
		id: id, title: title, monitored: true, hasFile: true,
		qualityProfileID: 1, tags: []int{}, movieFileID: id, cfScore: 200,
		qualityCutoffNotMet: false, inWantedSet: false,
	}
}

// statefulRadarrProfilesJSON is decisionEngineProfilesJSON plus the "cutoff"
// field a real Radarr /api/v3/qualityprofile always returns (every other
// profile fixture in this suite carries it; the shared decision-engine one
// omits it because nothing that uses it looks at connectivity output).
// connectivity.go warns on the absence — correctly, and on both runs — but
// that WARN is a statement about this fake's fixture, not about the cycle, and
// run 2 of the no-op test asserts the steady-state cycle is warning-free.
// Making the fake faithful is the honest way to satisfy that: the decision
// engine itself never reads cutoff (it uses cutoffFormatScore), so this
// changes no decision, only the fidelity of what the fake answers with.
const statefulRadarrProfilesJSON = `[{"id": 1, "name": "HD-1080p", "upgradeAllowed": true, "cutoff": 7, "cutoffFormatScore": 100}]`

type statefulRadarrFake struct {
	srv *httptest.Server

	mu       sync.Mutex
	movies   map[int]*statefulRadarrMovie
	order    []int
	requests []recordedRequest

	profilesJSON string
	tagsJSON     string
}

// newStatefulRadarrFake starts a fake Radarr backed by movies. Order is
// preserved (the fake's /movie array and /wanted/cutoff records are built by
// iterating movies in the order given), which is what lets the no-op tests
// below make deterministic, line-for-line assertions about report ordering
// across repeated runs.
func newStatefulRadarrFake(t *testing.T, movies []*statefulRadarrMovie) *statefulRadarrFake {
	t.Helper()
	f := &statefulRadarrFake{
		movies:       make(map[int]*statefulRadarrMovie, len(movies)),
		profilesJSON: statefulRadarrProfilesJSON,
		tagsJSON:     decisionEngineNoTagsJSON,
	}
	for _, m := range movies {
		f.movies[m.id] = m
		f.order = append(f.order, m.id)
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
		w.Write([]byte(f.wantedJSON()))
	}))
	mux.HandleFunc("/api/v3/moviefile", f.handle(func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.Atoi(r.URL.Query().Get("movieId"))
		w.Write([]byte(f.moviefileJSON(id)))
	}))
	mux.HandleFunc("/api/v3/movie", f.handle(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(f.libraryJSON()))
	}))
	mux.HandleFunc("/api/v3/movie/", f.handle(f.serveMovieDetail))
	// Same rationale as radarrFake's catch-all: recording every request to
	// every path, stubbed or not, is what makes "zero PUTs on run 2" an
	// assertion about requests nobody thought to make, not just about the
	// ones this fake happens to answer.
	mux.HandleFunc("/", f.handle(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *statefulRadarrFake) handle(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{method: r.Method, path: r.URL.Path, body: body, contentType: r.Header.Get("Content-Type")})
		f.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		next(w, r)
	}
}

// libraryElementJSON renders one movie exactly as GET /api/v3/movie's array
// elements need to decode: id, title, monitored (the ONLY field this
// project's write path ever changes, and therefore the only one that can
// differ between a fake's first and second run), hasFile, qualityProfileId,
// tags, and the movieFile subset the decision engine and cross-check need.
func libraryElementJSON(m *statefulRadarrMovie) string {
	tagsJSON, _ := json.Marshal(m.tags)
	return fmt.Sprintf(`{"id":%d,"title":%q,"monitored":%t,"hasFile":%t,"qualityProfileId":%d,"tags":%s,"movieFile":{"id":%d,"qualityCutoffNotMet":%t}}`,
		m.id, m.title, m.monitored, m.hasFile, m.qualityProfileID, tagsJSON, m.movieFileID, m.qualityCutoffNotMet)
}

func (f *statefulRadarrFake) libraryJSON() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	elems := make([]string, 0, len(f.order))
	for _, id := range f.order {
		elems = append(elems, libraryElementJSON(f.movies[id]))
	}
	return "[" + strings.Join(elems, ",") + "]"
}

// detailJSON renders the same movie for GET /api/v3/movie/{id} — the
// pre-write fetch unmonitorMovie re-reads fresh on every write pass. It
// carries the same fields as the library element (this fake's movies have no
// fields beyond what the decision engine and write path use) plus
// customFormatScore, which only the /moviefile-equivalent detail needs.
func (f *statefulRadarrFake) detailJSON(id int) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, found := f.movies[id]
	if !found {
		return "", false
	}
	tagsJSON, _ := json.Marshal(m.tags)
	body := fmt.Sprintf(`{"id":%d,"title":%q,"monitored":%t,"hasFile":%t,"qualityProfileId":%d,"tags":%s,"movieFile":{"id":%d,"customFormatScore":%d,"qualityCutoffNotMet":%t}}`,
		m.id, m.title, m.monitored, m.hasFile, m.qualityProfileID, tagsJSON, m.movieFileID, m.cfScore, m.qualityCutoffNotMet)
	return body, true
}

func (f *statefulRadarrFake) moviefileJSON(id int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, found := f.movies[id]
	if !found {
		return "[]"
	}
	return fmt.Sprintf(`[{"id": %d, "customFormatScore": %d}]`, m.movieFileID, m.cfScore)
}

// wantedJSON renders the whole /api/v3/wanted/cutoff envelope on a single
// page: every configured movie here has inWantedSet=false (a deliberate
// choice — see wouldUnmonitorStatefulMovie), and fetchWantedCutoff stops
// paging as soon as one page accounts for the full totalRecords, so a
// single response is a faithful, complete fake regardless of query params.
func (f *statefulRadarrFake) wantedJSON() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var recs []string
	for _, id := range f.order {
		if m := f.movies[id]; m.inWantedSet {
			recs = append(recs, fmt.Sprintf(`{"id":%d,"title":%q}`, m.id, m.title))
		}
	}
	return fmt.Sprintf(`{"page":1,"pageSize":100,"totalRecords":%d,"records":[%s]}`, len(recs), strings.Join(recs, ","))
}

// serveMovieDetail is GET/PUT /api/v3/movie/{id}. The PUT handler is the
// one place in this fake that mutates state: it decodes the body's
// "monitored" key and stores it, then echoes the exact bytes it received —
// same as real Radarr, and same as radarrFake's default PUT behavior — so
// the movie's monitored field really is whatever the write path last sent,
// for every GET (library, detail, or otherwise) from this point on.
func (f *statefulRadarrFake) serveMovieDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/api/v3/movie/"))
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		body, found := f.detailJSON(id)
		if !found {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(body))
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(body, &payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var monitored bool
		if raw, ok := payload["monitored"]; ok {
			json.Unmarshal(raw, &monitored)
		}
		f.mu.Lock()
		if m, found := f.movies[id]; found {
			m.monitored = monitored
		}
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *statefulRadarrFake) writes() []recordedRequest {
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

func (f *statefulRadarrFake) puts() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return filterRequests(f.requests, http.MethodPut)
}

func (f *statefulRadarrFake) instance() Instance {
	return Instance{Name: "radarr-main", Type: "radarr", URL: f.srv.URL, APIKey: "key"}
}

// writeNoOpTestConfig writes a config with the given dry_run and log_level,
// pointed at url. log_level is explicit (rather than reusing writeTestConfig,
// which always defaults to info) because the no-op test needs debug to
// observe the mandated "already unmonitored" debug line.
func writeNoOpTestConfig(t *testing.T, url string, dryRun bool, logLevel string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := fmt.Sprintf(`
dry_run: %t
log_level: %s
instances:
  - name: radarr-main
    type: radarr
    url: %s
    api_key: key1
`, dryRun, logLevel, url)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	return path
}

// TestRun_WriteMode_SecondRun_IsANoOp is the plan's Phase 5 acceptance
// criterion, machine-verified: "second run is a no-op ('already unmonitored'
// items are skipped silently at info, logged at debug)". Run 1 against a
// stateful fake really does write N movies (N PUTs, unmonitored=N). Run 2,
// against the SAME fake — now reflecting run 1's writes — must make ZERO
// PUTs and report wouldUnmonitor=0 unmonitored=0 alreadyUnmonitored=N, with
// the N movies visible only as debug-level "already unmonitored" lines.
func TestRun_WriteMode_SecondRun_IsANoOp(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{
		wouldUnmonitorStatefulMovie(1, "Would Unmonitor A"),
		wouldUnmonitorStatefulMovie(2, "Would Unmonitor B"),
		wouldUnmonitorStatefulMovie(3, "Would Unmonitor C"),
	})
	// REVIEW FIX (Phase 5 round 2): newStatefulRadarrFake defaults tagsJSON
	// to decisionEngineNoTagsJSON ("[]"), which makes resolveExclusionTagID
	// return tagActive=false — the mandated pre-write exclusion-tag re-check
	// (writer.go's `if tagActive` block) is then dead code as far as this,
	// the phase's own centerpiece acceptance test, is concerned. Defining
	// the exclusion tag here (matching the config default this test relies
	// on — writeNoOpTestConfig sets no exclusion_tag, so config.go defaults
	// it to "cutoffarr-exclude") makes tagActive=true for real, exercising
	// the production-shaped configuration. The 3 writes below must still
	// happen: every movie's tags (empty, from wouldUnmonitorStatefulMovie)
	// legitimately does not carry the exclusion tag.
	fake.tagsJSON = `[{"id": 42, "label": "cutoffarr-exclude"}]`
	configPath := writeNoOpTestConfig(t, fake.srv.URL, false, "debug")

	// --- Run 1: the first cycle actually writes the three candidates. ---
	var stdout1, stderr1 bytes.Buffer
	code := run([]string{"--config", configPath, "--once"}, &stdout1, &stderr1)
	if code != 0 {
		t.Fatalf("run 1: exit code = %d, want 0; stderr=%s", code, stderr1.String())
	}
	puts1 := fake.puts()
	if len(puts1) != 3 {
		t.Fatalf("run 1: expected 3 PUTs, got %d: %+v", len(puts1), puts1)
	}
	// The run-1 baseline for the no-op assertion below is every non-GET
	// request of any method to any path, not just the PUTs this run expected.
	writes1 := len(fake.writes())
	out1 := stdout1.String()
	if !strings.Contains(out1, "wouldUnmonitor=3") || !strings.Contains(out1, "unmonitored=3") {
		t.Errorf("run 1: expected wouldUnmonitor=3 unmonitored=3 in the summary:\n%s", out1)
	}
	if !strings.Contains(out1, "alreadyUnmonitored=0") {
		t.Errorf("run 1: expected alreadyUnmonitored=0 (nothing was already unmonitored yet):\n%s", out1)
	}

	// --- Run 2: same fake, now mutated by run 1's writes. ---
	var stdout2, stderr2 bytes.Buffer
	code = run([]string{"--config", configPath, "--once"}, &stdout2, &stderr2)
	if code != 0 {
		t.Fatalf("run 2: exit code = %d, want 0; stderr=%s", code, stderr2.String())
	}

	// The no-op's central claim: NOT ONE additional write request of any
	// method, to any path, was made — same standard as the dry-run
	// guarantee, applied to a second write-mode run that has nothing left to
	// do. REVIEW FIX (Phase 5 final round): this compared fake.puts() (PUTs
	// only) against the run-1 baseline while claiming "of any method, to any
	// path" — a second-cycle POST /api/v3/command, or a DELETE at some path
	// nobody stubbed, would have passed it. The fake's catch-all handler and
	// fake.writes() exist for exactly this claim, and the dry-run twin below
	// already used them.
	if writes2 := fake.writes(); len(writes2) != writes1 {
		t.Fatalf("run 2: made %d additional write request(s) of any method to any path, want ZERO (no-op): %+v", len(writes2)-writes1, writes2[writes1:])
	}

	out2 := stdout2.String()
	// "no errors" is half the plan's Phase 5 acceptance sentence, and exit
	// code 0 does not cover it: a warning exits 0, and so does a counted write
	// error. A steady-state cycle that has nothing to do must be quiet, which
	// is the whole point of this phase's mandated noise-budget fixes — the
	// withheld-writes line is INFO here precisely because run 2 blocks a pass
	// with nothing pending.
	if strings.Contains(out2, "level=WARN") || strings.Contains(out2, "level=ERROR") {
		t.Errorf("run 2: a no-op cycle must produce no WARN or ERROR at all; a daemon loop repeats this cycle forever:\n%s", out2)
	}
	if strings.Contains(out2, "msg=would-unmonitor") {
		t.Errorf("run 2: a no-op cycle must produce no would-unmonitor lines at all:\n%s", out2)
	}
	if !strings.Contains(out2, "wouldUnmonitor=0") {
		t.Errorf("run 2: expected wouldUnmonitor=0:\n%s", out2)
	}
	if !strings.Contains(out2, "unmonitored=0") {
		t.Errorf("run 2: expected unmonitored=0:\n%s", out2)
	}
	if !strings.Contains(out2, "alreadyUnmonitored=3") {
		t.Errorf("run 2: expected alreadyUnmonitored=3 (all three movies run 1 wrote are now already unmonitored):\n%s", out2)
	}
	// Skipped silently at info, logged at debug (plan's exact words): the
	// three movies must be individually visible at debug, each with the
	// mandated id/title/instance attrs, and nowhere at all in the report at
	// a level above it.
	for _, want := range []string{
		`msg="already unmonitored" id=1 title="Would Unmonitor A" instance=radarr-main`,
		`msg="already unmonitored" id=2 title="Would Unmonitor B" instance=radarr-main`,
		`msg="already unmonitored" id=3 title="Would Unmonitor C" instance=radarr-main`,
	} {
		if !strings.Contains(out2, want) {
			t.Errorf("run 2: expected the debug line %q:\n%s", want, out2)
		}
	}
}

// TestRun_DryRun_TwoRuns_NeverConverges is the dry-run regression the phase
// mandates alongside the no-op test: against the SAME stateful fake, two
// dry-run passes must both make zero write requests and must produce
// IDENTICAL would-unmonitor reports — dry-run changes nothing, so nothing
// ever converges to a no-op the way write mode's run 2 does. Any drift
// between the two reports (a movie disappearing, a count changing) would
// mean the fake's state was mutated somewhere it should not have been.
func TestRun_DryRun_TwoRuns_NeverConverges(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{
		wouldUnmonitorStatefulMovie(1, "Would Unmonitor A"),
		wouldUnmonitorStatefulMovie(2, "Would Unmonitor B"),
	})
	configPath := writeNoOpTestConfig(t, fake.srv.URL, true, "info")

	var stdout1, stderr1 bytes.Buffer
	code := run([]string{"--config", configPath, "--once"}, &stdout1, &stderr1)
	if code != 0 {
		t.Fatalf("run 1: exit code = %d, want 0; stderr=%s", code, stderr1.String())
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("run 1: dry-run made %d write request(s), want ZERO: %+v", len(writes), writes)
	}

	var stdout2, stderr2 bytes.Buffer
	code = run([]string{"--config", configPath, "--once"}, &stdout2, &stderr2)
	if code != 0 {
		t.Fatalf("run 2: exit code = %d, want 0; stderr=%s", code, stderr2.String())
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("run 2: dry-run made %d write request(s) across both runs, want ZERO: %+v", len(writes), writes)
	}

	report1 := reportLines(stdout1.String())
	report2 := reportLines(stdout2.String())
	if strings.Join(report1, "\n") != strings.Join(report2, "\n") {
		t.Errorf("dry-run reports differ between run 1 and run 2 (dry-run must never converge):\nrun1:\n%s\n\nrun2:\n%s",
			strings.Join(report1, "\n"), strings.Join(report2, "\n"))
	}
	// A regression that made both runs converge to an EMPTY report would
	// still pass a raw string-equality check; guard against that separately.
	if len(report1) == 0 {
		t.Fatal("expected a non-empty report to compare; the test proves nothing otherwise")
	}
	if !strings.Contains(strings.Join(report1, "\n"), "msg=would-unmonitor") {
		t.Errorf("expected would-unmonitor lines in the report:\n%s", strings.Join(report1, "\n"))
	}
}

// reportLines extracts the would-unmonitor/skip/summary lines from a run's
// stdout, with the leading time= attribute stripped (slog's TextHandler
// puts it first on every line): two runs of an unchanged fake produce
// identical report content but necessarily different timestamps, so the
// timestamp must be removed before two reports can be compared for equality.
func reportLines(output string) []string {
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, "msg=would-unmonitor") && !strings.Contains(line, "msg=skip") && !strings.Contains(line, "radarr decision summary") {
			continue
		}
		lines = append(lines, stripTimeAttr(line))
	}
	return lines
}

func stripTimeAttr(line string) string {
	fields := strings.Fields(line)
	if len(fields) > 0 && strings.HasPrefix(fields[0], "time=") {
		fields = fields[1:]
	}
	return strings.Join(fields, " ")
}
