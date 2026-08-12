package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newRadarrTestLogger builds a slog.Logger backed by a buffer, at the given
// level, matching the TextHandler setup main.go uses in production (see
// newConnectivityTestLogger in connectivity_test.go for the identical
// pattern used by Phase 1's tests).
func newRadarrTestLogger(level slog.Level) (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level}))
	return logger, &buf
}

// --- parseSamples -----------------------------------------------------

func TestParseSamples_EmptyFlagReturnsNil(t *testing.T) {
	if got := parseSamples(""); got != nil {
		t.Errorf("parseSamples(\"\") = %#v, want nil", got)
	}
}

func TestParseSamples_SplitsTrimsAndDropsEmptyEntries(t *testing.T) {
	got := parseSamples(" Movie A ,Movie B,  , Movie C")
	want := []string{"Movie A", "Movie B", "Movie C"}
	if len(got) != len(want) {
		t.Fatalf("parseSamples = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("parseSamples[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// --- /movie fetch -------------------------------------------------------

// radarrMovieJSON is a 3-movie library used across several tests:
//   - id 1 "Movie In Cutoff": monitored, no file, movieFile absent entirely.
//   - id 2 "Movie Not In Cutoff": monitored, has a file with quality name
//     and custom format score, plus an extra unrecognized field
//     ("originalTitle") to prove the raw-JSON debug dump is the real
//     response body and not a re-marshaled reconstruction.
//   - id 3 "Unrelated Movie": unmonitored, no file; never referenced by
//     --samples in these tests, so it must never produce per-movie logging.
const radarrMovieJSON = `[
	{"id": 1, "title": "Movie In Cutoff", "monitored": true, "hasFile": false, "qualityProfileId": 1, "tags": [1, 2]},
	{"id": 2, "title": "Movie Not In Cutoff", "monitored": true, "hasFile": true, "qualityProfileId": 2, "tags": [], "originalTitle": "Le Film", "movieFile": {"quality": {"quality": {"name": "Bluray-1080p"}}, "customFormatScore": 50}},
	{"id": 3, "title": "Unrelated Movie", "monitored": false, "hasFile": false, "qualityProfileId": 1, "tags": []}
]`

// emptyWantedCutoffJSON is a single-page, zero-record /wanted/cutoff
// envelope, used by tests that only care about /movie behavior.
const emptyWantedCutoffJSON = `{"page": 1, "pageSize": 100, "totalRecords": 0, "records": []}`

// radarrTestServer wires a mux serving moviePath/moviesJSON at
// /api/v3/movie (with moviesStatus) and wantedHandler at
// /api/v3/wanted/cutoff, recording every request path and raw query into
// gotRequests.
func radarrTestServer(t *testing.T, moviesStatus int, moviesJSON string, wantedHandler http.HandlerFunc, gotRequests *[]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/movie", func(w http.ResponseWriter, r *http.Request) {
		*gotRequests = append(*gotRequests, r.URL.String())
		w.WriteHeader(moviesStatus)
		w.Write([]byte(moviesJSON))
	})
	mux.HandleFunc("/api/v3/wanted/cutoff", func(w http.ResponseWriter, r *http.Request) {
		*gotRequests = append(*gotRequests, r.URL.String())
		wantedHandler(w, r)
	})
	return httptest.NewServer(mux)
}

func staticWantedCutoffHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		w.Write([]byte(body))
	}
}

func TestInspectRadarrLibrary_LogsLibraryCounts(t *testing.T) {
	var gotRequests []string
	srv := radarrTestServer(t, http.StatusOK, radarrMovieJSON, staticWantedCutoffHandler(http.StatusOK, emptyWantedCutoffJSON), &gotRequests)
	defer srv.Close()

	logger, buf := newRadarrTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	inspectRadarrLibrary(context.Background(), logger, inst, nil)

	out := buf.String()
	for _, want := range []string{"total=3", "monitored=2", "hasFile=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("log output does not contain %q:\n%s", want, out)
		}
	}
}

// TestInspectRadarrLibrary_ReturnsFullDecodedMovieSliceAndWantedIDs pins
// refactor (b): fetchMovies/inspectRadarrLibrary must hand back every
// decoded movie (not just samples), plus the wanted/cutoff id set and
// ok=true, so a caller (the decision engine) can evaluate the whole
// library without a second /movie round trip.
func TestInspectRadarrLibrary_ReturnsFullDecodedMovieSliceAndWantedIDs(t *testing.T) {
	var gotRequests []string
	wantedJSON := `{"page": 1, "pageSize": 100, "totalRecords": 1, "records": [{"id": 2, "title": "Movie Not In Cutoff"}]}`
	srv := radarrTestServer(t, http.StatusOK, radarrMovieJSON, staticWantedCutoffHandler(http.StatusOK, wantedJSON), &gotRequests)
	defer srv.Close()

	logger, _ := newRadarrTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	movies, wantedIDs, ok := inspectRadarrLibrary(context.Background(), logger, inst, nil)

	if !ok {
		t.Fatal("inspectRadarrLibrary returned ok=false, want true")
	}
	if len(movies) != 3 {
		t.Fatalf("got %d decoded movies, want 3 (the full library, not just samples): %+v", len(movies), movies)
	}
	var sawUnrelated bool
	for _, m := range movies {
		if m.Title != nil && *m.Title == "Unrelated Movie" {
			sawUnrelated = true
			if m.Monitored == nil || *m.Monitored {
				t.Errorf("Unrelated Movie decoded monitored = %v, want false", derefOrAbsent(m.Monitored))
			}
		}
	}
	if !sawUnrelated {
		t.Errorf("expected the full decoded slice to include the non-sample movie 'Unrelated Movie': %+v", movies)
	}
	if len(wantedIDs) != 1 || !wantedIDs[2] {
		t.Errorf("wantedIDs = %v, want {2: true}", wantedIDs)
	}
}

// TestInspectRadarrLibrary_MovieFetchFailure_ReturnsNotOK pins that a
// failed /movie fetch is reflected in inspectRadarrLibrary's own ok return
// value (not just the log output), so a caller can gate further work on it
// without re-parsing logs.
func TestInspectRadarrLibrary_MovieFetchFailure_ReturnsNotOK(t *testing.T) {
	var gotRequests []string
	srv := radarrTestServer(t, http.StatusInternalServerError, radarrMovieJSON, staticWantedCutoffHandler(http.StatusOK, emptyWantedCutoffJSON), &gotRequests)
	defer srv.Close()

	logger, _ := newRadarrTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-broken", Type: "radarr", URL: srv.URL, APIKey: "key"}

	movies, wantedIDs, ok := inspectRadarrLibrary(context.Background(), logger, inst, nil)

	if ok {
		t.Error("inspectRadarrLibrary returned ok=true, want false when /movie fails")
	}
	if movies != nil || wantedIDs != nil {
		t.Errorf("expected nil movies/wantedIDs on failure, got movies=%v wantedIDs=%v", movies, wantedIDs)
	}
}

func TestInspectRadarrLibrary_NoSamplesFlag_NoPerMovieLogging(t *testing.T) {
	var gotRequests []string
	srv := radarrTestServer(t, http.StatusOK, radarrMovieJSON, staticWantedCutoffHandler(http.StatusOK, emptyWantedCutoffJSON), &gotRequests)
	defer srv.Close()

	logger, buf := newRadarrTestLogger(slog.LevelDebug)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	inspectRadarrLibrary(context.Background(), logger, inst, nil)

	out := buf.String()
	if strings.Contains(out, "Movie In Cutoff") || strings.Contains(out, "Movie Not In Cutoff") || strings.Contains(out, "Unrelated Movie") {
		t.Errorf("no --samples given: expected no per-movie logging at all:\n%s", out)
	}
}

func TestInspectRadarrLibrary_SampleMatching_CaseInsensitiveTrimmedWhitespaceAndNotFoundWarns(t *testing.T) {
	var gotRequests []string
	srv := radarrTestServer(t, http.StatusOK, radarrMovieJSON, staticWantedCutoffHandler(http.StatusOK, emptyWantedCutoffJSON), &gotRequests)
	defer srv.Close()

	logger, buf := newRadarrTestLogger(slog.LevelDebug)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	// "  movie in cutoff  " deliberately mismatched in case and padded with
	// whitespace to exercise the case-insensitive, trimmed matching rule;
	// "Missing Movie" matches nothing in the library.
	samples := []string{"  movie in cutoff  ", "MOVIE NOT IN CUTOFF", "Missing Movie"}
	inspectRadarrLibrary(context.Background(), logger, inst, samples)

	out := buf.String()

	// Sample 1: matched, full detail logged at info. slog's TextHandler
	// quotes attribute values containing spaces, so the rendered tags
	// value is a quoted string.
	for _, want := range []string{"id=1", `title="Movie In Cutoff"`, "monitored=true", "hasFile=false", "qualityProfileId=1", `tags="[1 2]"`} {
		if !strings.Contains(out, want) {
			t.Errorf("expected sample 1 detail %q in log:\n%s", want, out)
		}
	}

	// Sample 2: matched, full detail plus movieFile quality/CF score.
	for _, want := range []string{"id=2", `title="Movie Not In Cutoff"`, "qualityName=Bluray-1080p", "customFormatScore=50"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected sample 2 detail %q in log:\n%s", want, out)
		}
	}

	// Sample 3: not found in the library, must warn naming it.
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected a warning for the not-found sample:\n%s", out)
	}
	if !strings.Contains(out, "Missing Movie") {
		t.Errorf("warning does not name the not-found sample:\n%s", out)
	}

	// Non-sample movie ("Unrelated Movie") must never appear per-movie.
	if strings.Contains(out, "Unrelated Movie") {
		t.Errorf("non-sample movie must not be logged per-movie:\n%s", out)
	}
}

func TestInspectRadarrLibrary_DebugDumpsSampleRawJSON(t *testing.T) {
	var gotRequests []string
	srv := radarrTestServer(t, http.StatusOK, radarrMovieJSON, staticWantedCutoffHandler(http.StatusOK, emptyWantedCutoffJSON), &gotRequests)
	defer srv.Close()

	logger, buf := newRadarrTestLogger(slog.LevelDebug)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	inspectRadarrLibrary(context.Background(), logger, inst, []string{"Movie Not In Cutoff"})

	out := buf.String()
	// "originalTitle" is a field cutoffarr's schema does not know about;
	// its presence in the debug dump proves the *raw* response bytes were
	// logged, not a re-marshaled reconstruction from the decoded struct.
	if !strings.Contains(out, "originalTitle") || !strings.Contains(out, "Le Film") {
		t.Errorf("expected the sample's raw JSON body (including unmodeled fields) at debug level:\n%s", out)
	}
}

func TestInspectRadarrLibrary_RawDumpNotLoggedAboveDebugLevel(t *testing.T) {
	var gotRequests []string
	srv := radarrTestServer(t, http.StatusOK, radarrMovieJSON, staticWantedCutoffHandler(http.StatusOK, emptyWantedCutoffJSON), &gotRequests)
	defer srv.Close()

	logger, buf := newRadarrTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	inspectRadarrLibrary(context.Background(), logger, inst, []string{"Movie Not In Cutoff"})

	out := buf.String()
	if strings.Contains(out, "level=DEBUG") || strings.Contains(out, "originalTitle") {
		t.Errorf("raw JSON dump should not appear at info level:\n%s", out)
	}
}

func TestInspectRadarrLibrary_MovieFileAbsent_NoWarnsAboutQualityOrCustomFormatFields(t *testing.T) {
	var gotRequests []string
	srv := radarrTestServer(t, http.StatusOK, radarrMovieJSON, staticWantedCutoffHandler(http.StatusOK, emptyWantedCutoffJSON), &gotRequests)
	defer srv.Close()

	logger, buf := newRadarrTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	// "Movie In Cutoff" has no movieFile object at all (no file yet).
	inspectRadarrLibrary(context.Background(), logger, inst, []string{"Movie In Cutoff"})

	out := buf.String()
	if strings.Contains(out, "level=WARN") {
		t.Errorf("movieFile legitimately absent (no file) must not warn about quality/customFormatScore fields:\n%s", out)
	}
	if strings.Contains(out, "qualityName") || strings.Contains(out, "customFormatScore") {
		t.Errorf("quality/customFormatScore must not be logged at all when movieFile is absent:\n%s", out)
	}
}

func TestInspectRadarrLibrary_AbsentQualityProfileIdField_WarnsNamingField(t *testing.T) {
	moviesJSON := `[{"id": 1, "title": "No Profile Movie", "monitored": true, "hasFile": false, "tags": []}]` // qualityProfileId key entirely absent
	var gotRequests []string
	srv := radarrTestServer(t, http.StatusOK, moviesJSON, staticWantedCutoffHandler(http.StatusOK, emptyWantedCutoffJSON), &gotRequests)
	defer srv.Close()

	logger, buf := newRadarrTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	inspectRadarrLibrary(context.Background(), logger, inst, []string{"No Profile Movie"})

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected a warning about the missing qualityProfileId field:\n%s", out)
	}
	if !strings.Contains(out, "field=qualityProfileId") {
		t.Errorf("warning does not name the missing field qualityProfileId:\n%s", out)
	}
	if !strings.Contains(out, "qualityProfileId=absent") {
		t.Errorf("expected the info line to show the missing field as absent:\n%s", out)
	}
}

func TestInspectRadarrLibrary_AbsentIdField_WarnsNamingField(t *testing.T) {
	// "id" is entirely absent. Title still matches so the sample is found;
	// per plan §5 "id" is a field of interest and must use the same
	// pointer-decode + warn-on-absent-key mechanism as qualityProfileId.
	moviesJSON := `[{"title": "No Id Movie", "monitored": true, "hasFile": false, "qualityProfileId": 1, "tags": []}]`
	var gotRequests []string
	srv := radarrTestServer(t, http.StatusOK, moviesJSON, staticWantedCutoffHandler(http.StatusOK, emptyWantedCutoffJSON), &gotRequests)
	defer srv.Close()

	logger, buf := newRadarrTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	inspectRadarrLibrary(context.Background(), logger, inst, []string{"No Id Movie"})

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected a warning about the missing id field:\n%s", out)
	}
	if !strings.Contains(out, "field=id") {
		t.Errorf("warning does not name the missing field id:\n%s", out)
	}
	if !strings.Contains(out, "id=absent") {
		t.Errorf("expected the info line to show the missing id as absent:\n%s", out)
	}
}

// TestInspectRadarrLibrary_AbsentTagsField_WarnsNamingField and
// TestInspectRadarrLibrary_PresentEmptyTagsField_NoWarn together pin the
// same present-vs-absent distinction already proven for the other movie
// fields, now extended to "tags": decoding into a *[]int (rather than a
// bare []int) makes an entirely-missing "tags" key (nil pointer)
// distinguishable from a present-but-empty "tags": [] (non-nil pointer to
// a zero-length slice), exactly like every other pointer-decoded field.
func TestInspectRadarrLibrary_AbsentTagsField_WarnsNamingField(t *testing.T) {
	moviesJSON := `[{"id": 1, "title": "No Tags Movie", "monitored": true, "hasFile": false, "qualityProfileId": 1}]` // tags key entirely absent
	var gotRequests []string
	srv := radarrTestServer(t, http.StatusOK, moviesJSON, staticWantedCutoffHandler(http.StatusOK, emptyWantedCutoffJSON), &gotRequests)
	defer srv.Close()

	logger, buf := newRadarrTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	inspectRadarrLibrary(context.Background(), logger, inst, []string{"No Tags Movie"})

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected a warning about the missing tags field:\n%s", out)
	}
	if !strings.Contains(out, "field=tags") {
		t.Errorf("warning does not name the missing field tags:\n%s", out)
	}
	if !strings.Contains(out, "tags=absent") {
		t.Errorf("expected the info line to show the missing tags as absent:\n%s", out)
	}
}

func TestInspectRadarrLibrary_PresentEmptyTagsField_NoWarn(t *testing.T) {
	moviesJSON := `[{"id": 1, "title": "Empty Tags Movie", "monitored": true, "hasFile": false, "qualityProfileId": 1, "tags": []}]`
	var gotRequests []string
	srv := radarrTestServer(t, http.StatusOK, moviesJSON, staticWantedCutoffHandler(http.StatusOK, emptyWantedCutoffJSON), &gotRequests)
	defer srv.Close()

	logger, buf := newRadarrTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	inspectRadarrLibrary(context.Background(), logger, inst, []string{"Empty Tags Movie"})

	out := buf.String()
	if strings.Contains(out, "level=WARN") {
		t.Errorf("present-but-empty tags must not warn:\n%s", out)
	}
	if !strings.Contains(out, "tags=[]") {
		t.Errorf("expected the present empty tags value to still be logged normally:\n%s", out)
	}
}

// TestInspectRadarrLibrary_TagsArrayContainsNullElement_NormalizedToAbsent
// pins the null-ARRAY-ELEMENT corner (REVIEW FIX, Phase 6, carried forward
// from the phase-5 branch review): "tags": [3, null, 9] decodes cleanly
// through movieListElement's *[]int field with NO error at all — a plain
// []int destination silently turns a null element into tag id 0 — so
// without an explicit check, a movie whose tags array is corrupted this way
// would look exactly like a real "tags": [3, 0, 9]. Rather than aborting the
// whole /movie fetch (the existing behavior for a movie element that fails
// to decode at all), a detected null element normalizes THIS movie's Tags
// back to nil so it flows into the already-correct, already-tested "tags
// absent" untrusted-input handling (decision.go rule 4) instead of silently
// treating the corrupted array as trustworthy.
func TestInspectRadarrLibrary_TagsArrayContainsNullElement_NormalizedToAbsent(t *testing.T) {
	moviesJSON := `[{"id": 1, "title": "Corrupted Tags Movie", "monitored": true, "hasFile": true, "qualityProfileId": 1, "tags": [3, null, 9]}]`
	var gotRequests []string
	srv := radarrTestServer(t, http.StatusOK, moviesJSON, staticWantedCutoffHandler(http.StatusOK, emptyWantedCutoffJSON), &gotRequests)
	defer srv.Close()

	logger, buf := newRadarrTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	movies, _, ok := inspectRadarrLibrary(context.Background(), logger, inst, nil)
	if !ok {
		t.Fatalf("inspectRadarrLibrary returned ok=false, want true (a corrupted tags array is per-movie, not instance-fatal):\n%s", buf.String())
	}
	if len(movies) != 1 {
		t.Fatalf("expected 1 movie, got %d", len(movies))
	}
	if movies[0].Tags != nil {
		t.Errorf("expected Tags to be normalized to nil after a null array element was detected, got %v", *movies[0].Tags)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "Corrupted Tags Movie") {
		t.Errorf("expected a warning naming the movie whose tags array contained a null element:\n%s", out)
	}
}

func TestInspectRadarrLibrary_MovieRequestNonTwoxx_SkipsInstanceWithWarning(t *testing.T) {
	var gotRequests []string
	srv := radarrTestServer(t, http.StatusInternalServerError, radarrMovieJSON, staticWantedCutoffHandler(http.StatusOK, emptyWantedCutoffJSON), &gotRequests)
	defer srv.Close()

	logger, buf := newRadarrTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-broken", Type: "radarr", URL: srv.URL, APIKey: "key"}

	inspectRadarrLibrary(context.Background(), logger, inst, []string{"Movie In Cutoff"})

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected a warning to be logged:\n%s", out)
	}
	if !strings.Contains(out, "radarr-broken") {
		t.Errorf("warning does not mention instance name:\n%s", out)
	}
	if len(gotRequests) != 1 {
		t.Fatalf("expected /wanted/cutoff to never be called after /movie failure, got requests: %v", gotRequests)
	}
}

func TestInspectRadarrLibrary_MovieResponseMalformedJSON_SkipsInstanceWithWarning(t *testing.T) {
	var gotRequests []string
	srv := radarrTestServer(t, http.StatusOK, "not json", staticWantedCutoffHandler(http.StatusOK, emptyWantedCutoffJSON), &gotRequests)
	defer srv.Close()

	logger, buf := newRadarrTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-malformed", Type: "radarr", URL: srv.URL, APIKey: "key"}

	inspectRadarrLibrary(context.Background(), logger, inst, nil)

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected a warning to be logged for malformed /movie JSON:\n%s", out)
	}
	if !strings.Contains(out, "radarr-malformed") {
		t.Errorf("warning does not mention instance name:\n%s", out)
	}
	if len(gotRequests) != 1 {
		t.Fatalf("expected /wanted/cutoff to never be called after malformed /movie body, got requests: %v", gotRequests)
	}
}

// TestInspectRadarrLibrary_MovieResponseTruncatedBetweenElements_SkipsInstanceWithWarning
// pins the fix for a gap in the streaming decoder: dec.More() returns false
// both when the array closes cleanly (a genuine "]") and when the
// underlying reader hits EOF or the sanity limit mid-stream — it swallows
// the read error rather than surfacing it. A body with two syntactically
// complete elements but no closing "]" (simulating a server dying or a
// proxy cutting the connection exactly on an element boundary) must be
// treated as malformed per §2.6, not as "the library happened to end
// after 2 movies".
func TestInspectRadarrLibrary_MovieResponseTruncatedBetweenElements_SkipsInstanceWithWarning(t *testing.T) {
	truncated := `[{"id":1,"title":"A","monitored":true,"hasFile":false},{"id":2,"title":"B","monitored":true,"hasFile":true}` // no closing "]"
	var gotRequests []string
	srv := radarrTestServer(t, http.StatusOK, truncated, staticWantedCutoffHandler(http.StatusOK, emptyWantedCutoffJSON), &gotRequests)
	defer srv.Close()

	logger, buf := newRadarrTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-truncated", Type: "radarr", URL: srv.URL, APIKey: "key"}

	inspectRadarrLibrary(context.Background(), logger, inst, nil)

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected a warning for the between-element-truncated /movie body:\n%s", out)
	}
	if !strings.Contains(out, "radarr-truncated") {
		t.Errorf("warning does not mention instance name:\n%s", out)
	}
	if strings.Contains(out, "total=2") {
		t.Errorf("truncated body must not report partial counts as if the library had ended normally:\n%s", out)
	}
	if len(gotRequests) != 1 {
		t.Fatalf("expected /wanted/cutoff to never be called after a truncated /movie body, got requests: %v", gotRequests)
	}
}

// --- /wanted/cutoff paging -----------------------------------------------

// wantedCutoffMultiPageHandler serves a 250-record /wanted/cutoff listing
// across 3 pages (100, 100, 50) with pageSize=100, recording each page's
// query string into gotQueries.
func wantedCutoffMultiPageHandler(t *testing.T, gotQueries *[]string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		*gotQueries = append(*gotQueries, r.URL.RawQuery)
		page := r.URL.Query().Get("page")

		var start, count int
		switch page {
		case "1":
			start, count = 1, 100
		case "2":
			start, count = 101, 100
		case "3":
			start, count = 201, 50
		default:
			t.Fatalf("unexpected /wanted/cutoff page requested: %q", page)
		}

		var records []string
		for i := start; i < start+count; i++ {
			records = append(records, fmt.Sprintf(`{"id": %d, "title": "Record %d"}`, i, i))
		}
		body := fmt.Sprintf(`{"page": %s, "pageSize": 100, "totalRecords": 250, "records": [%s]}`, page, strings.Join(records, ","))
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(body))
	}
}

func TestInspectRadarrLibrary_WantedCutoffPaging_AccumulatesAcrossPagesWithCorrectQueryParams(t *testing.T) {
	// Movie 1 (id 1) falls inside the 1..250 wanted/cutoff id range: in
	// cutoff. Movie 2 (id 2, "Movie Not In Cutoff") also falls in range in
	// radarrMovieJSON, so use a library with an id clearly outside the
	// range for the "not in cutoff" case.
	moviesJSON := `[
		{"id": 1, "title": "Below Cutoff Movie", "monitored": true, "hasFile": false, "qualityProfileId": 1, "tags": []},
		{"id": 9001, "title": "Above Cutoff Movie", "monitored": true, "hasFile": true, "qualityProfileId": 1, "tags": []}
	]`

	var gotRequests, gotQueries []string
	srv := radarrTestServer(t, http.StatusOK, moviesJSON, wantedCutoffMultiPageHandler(t, &gotQueries), &gotRequests)
	defer srv.Close()

	logger, buf := newRadarrTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	inspectRadarrLibrary(context.Background(), logger, inst, []string{"Below Cutoff Movie", "Above Cutoff Movie"})

	if len(gotQueries) != 3 {
		t.Fatalf("expected 3 /wanted/cutoff page requests, got %d: %v", len(gotQueries), gotQueries)
	}
	for i, want := range []string{"page=1", "page=2", "page=3"} {
		if !strings.Contains(gotQueries[i], want) {
			t.Errorf("page %d query = %q, want it to contain %q", i+1, gotQueries[i], want)
		}
		if !strings.Contains(gotQueries[i], "pageSize=100") {
			t.Errorf("page %d query = %q, want it to contain pageSize=100", i+1, gotQueries[i])
		}
	}

	out := buf.String()
	if !strings.Contains(out, "totalRecords=250") {
		t.Errorf("expected totalRecords=250 logged:\n%s", out)
	}
	if !strings.Contains(out, "fetched=250") {
		t.Errorf("expected fetched=250 logged:\n%s", out)
	}
	if !strings.Contains(out, "sample=\"Below Cutoff Movie\"") || !strings.Contains(out, "inWantedCutoff=true") {
		t.Errorf("expected Below Cutoff Movie (id 1) to be reported in wanted/cutoff:\n%s", out)
	}
	if !strings.Contains(out, "sample=\"Above Cutoff Movie\"") || !strings.Contains(out, "inWantedCutoff=false") {
		t.Errorf("expected Above Cutoff Movie (id 9001) to be reported NOT in wanted/cutoff:\n%s", out)
	}
}

// TestInspectRadarrLibrary_WantedCutoffEmptyPageBeforeTotalReached_WarnsAndSkipsInstance
// pins refactor (a)'s completeness contract: an id set fetched from fewer
// records than totalRecords claimed is a partial set. Absence-from-set
// means "would-unmonitor" to the decision engine, so a partial set produces
// false positives in the dangerous direction — it must never be treated as
// usable, so fetchWantedCutoff must report ok=false (instance skipped for
// the cycle), not merely warn-and-continue with what was fetched.
func TestInspectRadarrLibrary_WantedCutoffEmptyPageBeforeTotalReached_WarnsAndSkipsInstance(t *testing.T) {
	var gotRequests, gotQueries []string
	// totalRecords claims 50, but the very first page returns 0 records.
	handler := staticWantedCutoffHandler(http.StatusOK, `{"page": 1, "pageSize": 100, "totalRecords": 50, "records": []}`)
	wrapped := func(w http.ResponseWriter, r *http.Request) {
		gotQueries = append(gotQueries, r.URL.RawQuery)
		handler(w, r)
	}
	srv := radarrTestServer(t, http.StatusOK, radarrMovieJSON, wrapped, &gotRequests)
	defer srv.Close()

	logger, buf := newRadarrTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	inspectRadarrLibrary(context.Background(), logger, inst, []string{"Movie In Cutoff"})

	if len(gotQueries) != 1 {
		t.Fatalf("expected paging to stop after a single empty page (no infinite loop), got %d requests: %v", len(gotQueries), gotQueries)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected a warning about the empty page before totalRecords was reached:\n%s", out)
	}
	if strings.Contains(out, "inWantedCutoff") {
		t.Errorf("a partial wanted/cutoff id set must skip the instance: no in/out determination should be logged:\n%s", out)
	}
}

func TestInspectRadarrLibrary_WantedCutoffMissingTotalRecords_WarnsAndSkipsInstance(t *testing.T) {
	var gotRequests, gotQueries []string
	handler := staticWantedCutoffHandler(http.StatusOK, `{"page": 1, "pageSize": 100, "records": [{"id": 1, "title": "Movie In Cutoff"}]}`) // totalRecords key entirely absent
	wrapped := func(w http.ResponseWriter, r *http.Request) {
		gotQueries = append(gotQueries, r.URL.RawQuery)
		handler(w, r)
	}
	srv := radarrTestServer(t, http.StatusOK, radarrMovieJSON, wrapped, &gotRequests)
	defer srv.Close()

	logger, buf := newRadarrTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	inspectRadarrLibrary(context.Background(), logger, inst, []string{"Movie In Cutoff"})

	if len(gotQueries) != 1 {
		t.Fatalf("expected paging to stop after the first page when totalRecords is missing, got %d requests: %v", len(gotQueries), gotQueries)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected a warning about the missing totalRecords field:\n%s", out)
	}
	if strings.Contains(out, "inWantedCutoff") {
		t.Errorf("instance should be skipped for wanted/cutoff purposes: no in/out determination should be logged:\n%s", out)
	}
}

// TestInspectRadarrLibrary_WantedCutoffMissingRecordsKey_WarnsAndSkipsInstance
// pins the pointer-decode of wantedCutoffPage.Records: an entirely absent
// "records" key (as opposed to a present-but-empty "records": []) is
// malformed per §2.6 (can't tell whether the server omitted the field or
// really meant "no records"), so it must warn and skip the instance rather
// than being treated like the normal empty-page-ends-paging case.
func TestInspectRadarrLibrary_WantedCutoffMissingRecordsKey_WarnsAndSkipsInstance(t *testing.T) {
	var gotRequests, gotQueries []string
	handler := staticWantedCutoffHandler(http.StatusOK, `{"page": 1, "pageSize": 100, "totalRecords": 5}`) // records key entirely absent
	wrapped := func(w http.ResponseWriter, r *http.Request) {
		gotQueries = append(gotQueries, r.URL.RawQuery)
		handler(w, r)
	}
	srv := radarrTestServer(t, http.StatusOK, radarrMovieJSON, wrapped, &gotRequests)
	defer srv.Close()

	logger, buf := newRadarrTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	// A sample that matched a library movie is required to make this test
	// discriminate correctly: an absent "records" key wrongly treated as
	// "present but empty" would still be ok=true and would proceed to log
	// "sample cutoff status ... inWantedCutoff=false" for this sample —
	// that line's presence is exactly the bug this test catches.
	inspectRadarrLibrary(context.Background(), logger, inst, []string{"Movie In Cutoff"})

	if len(gotQueries) != 1 {
		t.Fatalf("expected paging to stop after the first page when records is missing, got %d requests: %v", len(gotQueries), gotQueries)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected a warning about the missing records field:\n%s", out)
	}
	if strings.Contains(out, "inWantedCutoff") {
		t.Errorf("instance should be skipped for wanted/cutoff purposes (records key entirely absent is malformed, not an empty page): no in/out determination should be logged:\n%s", out)
	}
}

// TestInspectRadarrLibrary_WantedCutoffRecordMissingId_WarnsAndSkipsInstance
// pins wantedCutoffRecord decoding "title" (plan §5: "decode records
// minimally: id, title") and using it at the one place an individual
// record is referenced: a record with no usable id can't be added to the
// cutoff set, and title is the only other identifying information
// available for the warning that reports the loss.
//
// FIX 2 (controller-mandated correction, applied after the initial Phase 3
// review): a record missing its id was previously warned-about-but-
// skipped, while the rest of the page's (and any later pages') ids were
// still folded into the returned set and the fetch still reported ok=true.
// That made the returned set silently non-authoritative: a movie's true
// membership could never be reconstructed from an id-less record, so the
// set could under-report the true cutoff-not-met population without any
// indication that had happened — the same "partial set masquerading as
// complete" hazard refactor (a) closes for the empty-page-early and
// page-cap cases. It must be treated identically: warn and return
// (nil, false), consistent with those other two partial cases.
func TestInspectRadarrLibrary_WantedCutoffRecordMissingId_WarnsAndSkipsInstance(t *testing.T) {
	var gotRequests, gotQueries []string
	handler := staticWantedCutoffHandler(http.StatusOK, `{"totalRecords": 1, "records": [{"title": "Untitled Cutoff Record"}]}`) // id absent
	wrapped := func(w http.ResponseWriter, r *http.Request) {
		gotQueries = append(gotQueries, r.URL.RawQuery)
		handler(w, r)
	}
	srv := radarrTestServer(t, http.StatusOK, radarrMovieJSON, wrapped, &gotRequests)
	defer srv.Close()

	logger, buf := newRadarrTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	// A sample that matches a library movie is required so this test can
	// discriminate correctly: if the id-less record were wrongly treated as
	// merely "excluded from the set" (ok=true), this sample would still
	// produce a "sample cutoff status ... inWantedCutoff=false" line —
	// that line's presence is exactly the bug this test catches.
	inspectRadarrLibrary(context.Background(), logger, inst, []string{"Movie In Cutoff"})

	if len(gotQueries) != 1 {
		t.Fatalf("expected a single page request, got %d: %v", len(gotQueries), gotQueries)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected a warning about the record missing its id field:\n%s", out)
	}
	if !strings.Contains(out, "Untitled Cutoff Record") {
		t.Errorf("expected the warning to include the record's title for context:\n%s", out)
	}
	if strings.Contains(out, "inWantedCutoff") {
		t.Errorf("a record missing its id makes the whole set non-authoritative: instance must be skipped, no in/out determination logged:\n%s", out)
	}
}

// TestInspectRadarrLibrary_WantedCutoffPageCap_WarnsWhenHitWithoutCompleting
// pins the hard page-cap defense. maxWantedCutoffPages is temporarily
// lowered so the test doesn't need to make 1000 real HTTP round trips. Per
// refactor (a)'s completeness contract, hitting the cap without completing
// also means the id set is partial, so the instance must be skipped (no
// in/out determination logged), not merely warned-and-continued.
func TestInspectRadarrLibrary_WantedCutoffPageCap_WarnsWhenHitWithoutCompleting(t *testing.T) {
	original := maxWantedCutoffPages
	maxWantedCutoffPages = 3
	defer func() { maxWantedCutoffPages = original }()

	var gotRequests, gotQueries []string
	// Every page reports a huge totalRecords and always returns a full
	// page of 100 records, so fetched never reaches totalRecords and the
	// page is never empty: the only way paging stops is the cap.
	handler := func(w http.ResponseWriter, r *http.Request) {
		gotQueries = append(gotQueries, r.URL.RawQuery)
		page := r.URL.Query().Get("page")
		var records []string
		for i := 0; i < 100; i++ {
			records = append(records, fmt.Sprintf(`{"id": %d, "title": "Record"}`, i))
		}
		body := fmt.Sprintf(`{"page": %s, "pageSize": 100, "totalRecords": 999999, "records": [%s]}`, page, strings.Join(records, ","))
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(body))
	}
	srv := radarrTestServer(t, http.StatusOK, radarrMovieJSON, handler, &gotRequests)
	defer srv.Close()

	logger, buf := newRadarrTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	inspectRadarrLibrary(context.Background(), logger, inst, []string{"Movie In Cutoff"})

	if len(gotQueries) != 3 {
		t.Fatalf("expected paging to stop exactly at the (lowered) cap of 3 pages, got %d requests: %v", len(gotQueries), gotQueries)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected a warning when the page cap is hit without completing:\n%s", out)
	}
	if !strings.Contains(out, "page cap") && !strings.Contains(out, "pageCap") {
		t.Errorf("expected the page-cap warning to mention the cap:\n%s", out)
	}
	if strings.Contains(out, "inWantedCutoff") {
		t.Errorf("a partial wanted/cutoff id set (page cap hit) must skip the instance: no in/out determination should be logged:\n%s", out)
	}
}

// --- sonarr scope ---------------------------------------------------------

// TestRun_SonarrInstance_NeverCallsMovieOrWantedCutoff proves the wiring in
// main.go: Radarr library inspection must never run for a sonarr instance.
// It uses run() end-to-end (same pattern as main_test.go) because the guard
// under test lives in main.go's --once loop, not in radarr.go itself.
func TestRun_SonarrInstance_NeverCallsMovieOrWantedCutoff(t *testing.T) {
	var gotPaths []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/system/status", func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		w.Write([]byte(`{"appName": "Sonarr", "version": "4.0.9.2244"}`))
	})
	mux.HandleFunc("/api/v3/qualityprofile", func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		w.Write([]byte(`[]`))
	})
	mux.HandleFunc("/api/v3/movie", func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		t.Errorf("unexpected /api/v3/movie request against a sonarr instance")
	})
	mux.HandleFunc("/api/v3/wanted/cutoff", func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		t.Errorf("unexpected /api/v3/wanted/cutoff request against a sonarr instance")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := `
instances:
  - name: sonarr-main
    type: sonarr
    url: ` + srv.URL + `
    api_key: key1
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path, "--once"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	for _, p := range gotPaths {
		if strings.Contains(p, "movie") || strings.Contains(p, "wanted") {
			t.Errorf("sonarr instance triggered a radarr-only endpoint: %s", p)
		}
	}
}

// TestRun_SamplesFlag_WiresIntoRadarrInspection proves --samples is parsed
// by run() and threaded through to the radarr inspection for a radarr
// instance, end to end.
func TestRun_SamplesFlag_WiresIntoRadarrInspection(t *testing.T) {
	var gotRequests []string
	srv := radarrTestServer(t, http.StatusOK, radarrMovieJSON, staticWantedCutoffHandler(http.StatusOK, emptyWantedCutoffJSON), &gotRequests)
	defer srv.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/system/status", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"appName": "Radarr", "version": "5.14.0.9383"}`))
	})
	mux.HandleFunc("/api/v3/qualityprofile", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	})
	mux.HandleFunc("/api/v3/movie", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(radarrMovieJSON))
	})
	mux.HandleFunc("/api/v3/wanted/cutoff", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(emptyWantedCutoffJSON))
	})
	srv2 := httptest.NewServer(mux)
	defer srv2.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := `
log_level: debug
instances:
  - name: radarr-main
    type: radarr
    url: ` + srv2.URL + `
    api_key: key1
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path, "--once", "--samples", "Movie In Cutoff"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, `title="Movie In Cutoff"`) {
		t.Errorf("expected --samples to be threaded through to the radarr inspection:\n%s", out)
	}
}

// TestRun_ConnectivityFailedRadarrInstance_NeverCallsMovieOrWantedCutoff
// pins the fix for a gap in main.go's wiring: checkInstanceConnectivity
// logs "skipping instance..." when either connectivity endpoint fails, but
// previously returned nothing, so main.go still went ahead and called
// inspectRadarrLibrary on an instance already declared skipped for the
// cycle. checkInstanceConnectivity now returns ok, and main.go must gate
// the radarr library inspection on it. A later, healthy instance in the
// same config must still be checked — one broken instance must not stop
// the rest.
func TestRun_ConnectivityFailedRadarrInstance_NeverCallsMovieOrWantedCutoff(t *testing.T) {
	var brokenHitMovieOrCutoff bool
	brokenMux := http.NewServeMux()
	brokenMux.HandleFunc("/api/v3/system/status", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // connectivity fails here
	})
	brokenMux.HandleFunc("/api/v3/qualityprofile", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	})
	brokenMux.HandleFunc("/api/v3/movie", func(w http.ResponseWriter, r *http.Request) {
		brokenHitMovieOrCutoff = true
	})
	brokenMux.HandleFunc("/api/v3/wanted/cutoff", func(w http.ResponseWriter, r *http.Request) {
		brokenHitMovieOrCutoff = true
	})
	brokenSrv := httptest.NewServer(brokenMux)
	defer brokenSrv.Close()

	var gotHealthyMoviePaths []string
	healthyMux := http.NewServeMux()
	healthyMux.HandleFunc("/api/v3/system/status", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"appName": "Radarr", "version": "5.14.0.9383"}`))
	})
	healthyMux.HandleFunc("/api/v3/qualityprofile", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	})
	healthyMux.HandleFunc("/api/v3/movie", func(w http.ResponseWriter, r *http.Request) {
		gotHealthyMoviePaths = append(gotHealthyMoviePaths, r.URL.Path)
		w.Write([]byte(radarrMovieJSON))
	})
	healthyMux.HandleFunc("/api/v3/wanted/cutoff", func(w http.ResponseWriter, r *http.Request) {
		gotHealthyMoviePaths = append(gotHealthyMoviePaths, r.URL.Path)
		w.Write([]byte(emptyWantedCutoffJSON))
	})
	healthySrv := httptest.NewServer(healthyMux)
	defer healthySrv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := `
instances:
  - name: radarr-broken
    type: radarr
    url: ` + brokenSrv.URL + `
    api_key: key1
  - name: radarr-healthy
    type: radarr
    url: ` + healthySrv.URL + `
    api_key: key2
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path, "--once"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if brokenHitMovieOrCutoff {
		t.Errorf("connectivity-failed instance must not receive /movie or /wanted/cutoff requests")
	}
	if len(gotHealthyMoviePaths) != 2 || gotHealthyMoviePaths[0] != "/api/v3/movie" || gotHealthyMoviePaths[1] != "/api/v3/wanted/cutoff" {
		t.Errorf("expected the later healthy instance to still receive /movie then /wanted/cutoff requests, got: %v", gotHealthyMoviePaths)
	}
	out := stdout.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "radarr-broken") {
		t.Errorf("expected a warning naming the broken instance:\n%s", out)
	}
	if !strings.Contains(out, "radarr-healthy") || !strings.Contains(out, "total=3") {
		t.Errorf("expected the healthy instance's movie library to still be inspected:\n%s", out)
	}
}

// --- Phase 3: decision engine wiring into main.go's run() -----------------

// fullRadarrPipelineMux wires a mux serving every endpoint a full
// connectivity + library inspection + decision engine pass touches:
// system/status, qualityprofile (hit twice: once by checkInstanceConnectivity,
// once by the decision engine's own fetchQualityProfiles), movie,
// wanted/cutoff, tag, and moviefile.
func fullRadarrPipelineMux(moviesJSON, wantedCutoffJSON, tagsJSON string, moviefileHandler http.HandlerFunc) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/system/status", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"appName": "Radarr", "version": "5.14.0.9383"}`))
	})
	mux.HandleFunc("/api/v3/qualityprofile", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id": 1, "name": "HD-1080p", "upgradeAllowed": true, "cutoff": 7, "cutoffFormatScore": 100}]`))
	})
	mux.HandleFunc("/api/v3/movie", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(moviesJSON))
	})
	mux.HandleFunc("/api/v3/wanted/cutoff", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(wantedCutoffJSON))
	})
	mux.HandleFunc("/api/v3/tag", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(tagsJSON))
	})
	mux.HandleFunc("/api/v3/moviefile", moviefileHandler)
	return mux
}

// TestRun_RadarrInstance_DecisionEngineProducesReportLines proves the
// full pipeline is wired end to end through run(): connectivity succeeds,
// inspectRadarrLibrary's returned movies/wantedIDs are handed to
// runRadarrDecisionEngine, which fetches profiles/tag/moviefile and emits
// would-unmonitor/skip report lines for every monitored movie.
func TestRun_RadarrInstance_DecisionEngineProducesReportLines(t *testing.T) {
	moviesJSON := `[
		{"id": 1, "title": "Would Unmonitor Movie", "monitored": true, "hasFile": true, "qualityProfileId": 1, "tags": [], "movieFile": {"id": 1}},
		{"id": 2, "title": "No File Movie", "monitored": true, "hasFile": false, "qualityProfileId": 1, "tags": []}
	]`
	mux := fullRadarrPipelineMux(moviesJSON, emptyWantedCutoffJSON, decisionEngineNoTagsJSON, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id": 1, "customFormatScore": 200}]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := `
instances:
  - name: radarr-main
    type: radarr
    url: ` + srv.URL + `
    api_key: key1
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path, "--once"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "msg=would-unmonitor") || !strings.Contains(out, `title="Would Unmonitor Movie"`) {
		t.Errorf("expected a would-unmonitor line for the passing movie:\n%s", out)
	}
	if !strings.Contains(out, "msg=skip") || !strings.Contains(out, `title="No File Movie"`) || !strings.Contains(out, `reason="no file"`) {
		t.Errorf("expected a skip line for the no-file movie:\n%s", out)
	}
	if !strings.Contains(out, "msg=\"radarr decision summary\"") {
		t.Errorf("expected the end-of-instance decision summary:\n%s", out)
	}
}

// TestRun_RadarrInstance_IncompleteWantedCutoff_DecisionEngineNeverRuns is
// the self-review-mandated dangerous-direction guard: refactor (a) makes
// fetchWantedCutoff return ok=false for a partial id set, and this proves
// that failure actually prevents the decision engine from running at all
// through the real main.go wiring — no would-unmonitor/skip lines, and no
// requests to the decision engine's own endpoints (qualityprofile hit only
// once, by connectivity; tag and moviefile never hit).
func TestRun_RadarrInstance_IncompleteWantedCutoff_DecisionEngineNeverRuns(t *testing.T) {
	moviesJSON := `[{"id": 1, "title": "Some Movie", "monitored": true, "hasFile": true, "qualityProfileId": 1, "tags": []}]`
	// totalRecords claims 50 but the first page returns 0 records: partial set.
	incompleteWantedCutoffJSON := `{"page": 1, "pageSize": 100, "totalRecords": 50, "records": []}`

	var gotTagRequests, gotMoviefileRequests, gotProfileRequests []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/system/status", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"appName": "Radarr", "version": "5.14.0.9383"}`))
	})
	mux.HandleFunc("/api/v3/qualityprofile", func(w http.ResponseWriter, r *http.Request) {
		gotProfileRequests = append(gotProfileRequests, r.URL.Path)
		w.Write([]byte(`[{"id": 1, "name": "HD-1080p", "upgradeAllowed": true, "cutoff": 7, "cutoffFormatScore": 100}]`))
	})
	mux.HandleFunc("/api/v3/movie", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(moviesJSON))
	})
	mux.HandleFunc("/api/v3/wanted/cutoff", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(incompleteWantedCutoffJSON))
	})
	mux.HandleFunc("/api/v3/tag", func(w http.ResponseWriter, r *http.Request) {
		gotTagRequests = append(gotTagRequests, r.URL.Path)
		w.Write([]byte(`[]`))
	})
	mux.HandleFunc("/api/v3/moviefile", func(w http.ResponseWriter, r *http.Request) {
		gotMoviefileRequests = append(gotMoviefileRequests, r.URL.Path)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := `
instances:
  - name: radarr-main
    type: radarr
    url: ` + srv.URL + `
    api_key: key1
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path, "--once"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}

	out := stdout.String()
	if strings.Contains(out, "msg=would-unmonitor") || strings.Contains(out, "msg=skip") {
		t.Errorf("a partial wanted/cutoff id set must never produce decision lines:\n%s", out)
	}
	if len(gotProfileRequests) != 1 {
		t.Errorf("expected qualityprofile to be hit exactly once (by connectivity only, never by the decision engine), got %d", len(gotProfileRequests))
	}
	if len(gotTagRequests) != 0 {
		t.Errorf("expected /tag to never be requested, got %d", len(gotTagRequests))
	}
	if len(gotMoviefileRequests) != 0 {
		t.Errorf("expected /moviefile to never be requested, got %d", len(gotMoviefileRequests))
	}
}

// TestRun_RadarrInstance_ExclusionTagConfigThreadedToDecisionEngine proves
// cfg.ExclusionTag reaches runRadarrDecisionEngine: a non-default
// exclusion_tag configured in the YAML must be the label resolveExclusionTagID
// looks up (visible in the "exclusion tag not defined" info log naming it,
// since this instance's /tag response does not contain it).
func TestRun_RadarrInstance_ExclusionTagConfigThreadedToDecisionEngine(t *testing.T) {
	moviesJSON := `[{"id": 1, "title": "Some Movie", "monitored": true, "hasFile": false, "qualityProfileId": 1, "tags": []}]`
	mux := fullRadarrPipelineMux(moviesJSON, emptyWantedCutoffJSON, `[{"id": 1, "label": "unrelated-tag"}]`, func(w http.ResponseWriter, r *http.Request) {})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := `
exclusion_tag: my-custom-exclusion-label
instances:
  - name: radarr-main
    type: radarr
    url: ` + srv.URL + `
    api_key: key1
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path, "--once"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "exclusionTag=my-custom-exclusion-label") {
		t.Errorf("expected the configured exclusion_tag label to reach the decision engine's tag resolution:\n%s", out)
	}
}

// TestInspectRadarrLibrary_TagsKeyDifferentlyCased_NullElementStillCaught is
// the Radarr half of the case-sensitivity hole (see the Sonarr twin in
// sonarr_test.go): encoding/json matches "Tags" to the movieListElement.Tags
// field case-insensitively, while the raw re-check looked the key up exactly,
// so a differently-cased key silently skipped the null-element check and the
// corrupted array [3, null, 9] was trusted as [3, 0, 9].
func TestInspectRadarrLibrary_TagsKeyDifferentlyCased_NullElementStillCaught(t *testing.T) {
	moviesJSON := `[{"id": 1, "title": "Oddly Cased Tags Movie", "monitored": true, "hasFile": true, "qualityProfileId": 1, "Tags": [3, null, 9]}]`
	var gotRequests []string
	srv := radarrTestServer(t, http.StatusOK, moviesJSON, staticWantedCutoffHandler(http.StatusOK, emptyWantedCutoffJSON), &gotRequests)
	defer srv.Close()

	logger, buf := newRadarrTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	movies, _, ok := inspectRadarrLibrary(context.Background(), logger, inst, nil)
	if !ok {
		t.Fatalf("inspectRadarrLibrary returned ok=false, want true:\n%s", buf.String())
	}
	if len(movies) != 1 {
		t.Fatalf("expected 1 movie, got %d", len(movies))
	}
	if movies[0].Tags != nil {
		t.Errorf("a corrupted tags array must be normalized to nil regardless of how the JSON key is cased, got %v", *movies[0].Tags)
	}
}

// TestInspectRadarrLibrary_TagsKeyPresentTwiceInDifferentCases_TreatedAsUnverifiable
// is the Radarr twin of the Sonarr fixture in sonarr_test.go, and the reason it
// had to exist: radarr.go's found==false branch (the one that normalizes Tags
// back to nil) had no fixture that could reach it at all, so the branch was
// asserted by reading rather than by running. encoding/json decodes an object
// key by key, so with BOTH "tags" and "Tags" present the LAST one wins for the
// struct field while rawObjectField reports found=false — the field's
// provenance is ambiguous, which on the exclusion tag's own input is untrusted
// input.
func TestInspectRadarrLibrary_TagsKeyPresentTwiceInDifferentCases_TreatedAsUnverifiable(t *testing.T) {
	moviesJSON := `[{"id": 1, "title": "Two Tags Keys Movie", "monitored": true, "hasFile": true, "qualityProfileId": 1, "tags": [1, 2], "Tags": [3, null]}]`
	var gotRequests []string
	srv := radarrTestServer(t, http.StatusOK, moviesJSON, staticWantedCutoffHandler(http.StatusOK, emptyWantedCutoffJSON), &gotRequests)
	defer srv.Close()

	logger, buf := newRadarrTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	movies, _, ok := inspectRadarrLibrary(context.Background(), logger, inst, nil)
	if !ok {
		t.Fatalf("inspectRadarrLibrary returned ok=false, want true:\n%s", buf.String())
	}
	if len(movies) != 1 {
		t.Fatalf("expected 1 movie, got %d", len(movies))
	}
	if movies[0].Tags != nil {
		t.Errorf("two keys could each have populated tags; ambiguous provenance must be treated as unverifiable, got %v", *movies[0].Tags)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning naming the unverifiable tags:\n%s", buf.String())
	}
}

// --- the reverse scan's wanted set (Phase 10) ------------------------------
//
// The reverse scan's quality-cutoff signal is the SAME /wanted/cutoff endpoint
// the forward scan reads, narrowed to unmonitored records with an explicit
// monitored=false parameter (verified against both live instances before this
// code was written: Radarr returns only monitored=false records, 3 of them
// against 131 for the default, and default/monitored=true are identical).
// Reusing the paging machinery means it inherits the completeness contract
// unchanged, which matters more here than on the forward side: a partial
// unmonitored set manufactures FALSE FINDINGS, and — with the remonitor flag
// on — wrong writes.

func TestFetchWantedCutoff_UnmonitoredFilter_SendsMonitoredFalseOnEveryPage(t *testing.T) {
	var gotQueries []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/wanted/cutoff", func(w http.ResponseWriter, r *http.Request) {
		gotQueries = append(gotQueries, r.URL.RawQuery)
		page := r.URL.Query().Get("page")
		var records []string
		start := 1
		if page == "2" {
			start = 101
		}
		count := 100
		if page == "2" {
			count = 50
		}
		for i := start; i < start+count; i++ {
			records = append(records, fmt.Sprintf(`{"id": %d, "title": "Record %d"}`, i, i))
		}
		fmt.Fprintf(w, `{"page": %s, "pageSize": 100, "totalRecords": 150, "records": [%s]}`, page, strings.Join(records, ","))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newRadarrTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	ids, ok := fetchWantedCutoff(context.Background(), logger, client, inst, unmonitoredWantedFilter())
	if !ok {
		t.Fatalf("fetchWantedCutoff returned ok=false, want true:\n%s", buf.String())
	}
	if len(ids) != 150 || !ids[1] || !ids[150] {
		t.Errorf("got %d ids, want the full 150-record set", len(ids))
	}
	if len(gotQueries) != 2 {
		t.Fatalf("expected 2 page requests, got %d: %v", len(gotQueries), gotQueries)
	}
	for i, q := range gotQueries {
		if !strings.Contains(q, "monitored=false") {
			t.Errorf("page %d query = %q, want it to carry monitored=false: the filter must be sent on EVERY page, not only the first", i+1, q)
		}
		if !strings.Contains(q, "pageSize=100") || !strings.Contains(q, fmt.Sprintf("page=%d", i+1)) {
			t.Errorf("page %d query = %q lost the paging parameters", i+1, q)
		}
	}
	if !strings.Contains(buf.String(), `wantedFilter="monitored=false"`) {
		t.Errorf("the wanted/cutoff line must say WHICH set it counted, or the forward and reverse fetches are two indistinguishable lines:\n%s", buf.String())
	}
}

// TestFetchWantedCutoff_ForwardFetch_SendsNoMonitoredFilter pins the other half:
// the forward fetch's request is unchanged by this phase. The live probe showed
// default and monitored=true return identical sets, so the forward path keeps
// sending neither — adding a parameter it never sent would be a behavior change
// smuggled in beside a new feature.
func TestFetchWantedCutoff_ForwardFetch_SendsNoMonitoredFilter(t *testing.T) {
	var gotQueries []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/wanted/cutoff", func(w http.ResponseWriter, r *http.Request) {
		gotQueries = append(gotQueries, r.URL.RawQuery)
		w.Write([]byte(emptyWantedCutoffJSON))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, _ := newRadarrTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	if _, ok := fetchWantedCutoff(context.Background(), logger, client, inst, nil); !ok {
		t.Fatal("fetchWantedCutoff returned ok=false on an empty but well-formed set")
	}
	if len(gotQueries) != 1 {
		t.Fatalf("expected 1 page request, got %v", gotQueries)
	}
	if strings.Contains(gotQueries[0], "monitored") {
		t.Errorf("the forward fetch must send no monitored filter at all, got %q", gotQueries[0])
	}
}

// TestFetchWantedCutoff_UnmonitoredFilter_PartialSetIsRefused pins that the
// filtered fetch inherits the completeness contract rather than getting a
// weaker one. The dangerous direction is reversed here — a missing record means
// a movie the reverse scan will NOT report, and a set short of what
// totalRecords claimed cannot say which — so a partial answer is refused
// exactly as the forward one is.
func TestFetchWantedCutoff_UnmonitoredFilter_PartialSetIsRefused(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/wanted/cutoff", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"page": 1, "pageSize": 100, "totalRecords": 50, "records": []}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newRadarrTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-partial", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	if _, ok := fetchWantedCutoff(context.Background(), logger, client, inst, unmonitoredWantedFilter()); ok {
		t.Fatal("fetchWantedCutoff returned ok=true on a partial set; the completeness contract must apply to the filtered fetch too")
	}
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "radarr-partial") {
		t.Errorf("expected a warning naming the instance:\n%s", buf.String())
	}
}
