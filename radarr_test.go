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

func TestInspectRadarrLibrary_WantedCutoffEmptyPageBeforeTotalReached_WarnsAndStopsWithoutInfiniteLoop(t *testing.T) {
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

	inspectRadarrLibrary(context.Background(), logger, inst, nil)

	if len(gotQueries) != 1 {
		t.Fatalf("expected paging to stop after a single empty page (no infinite loop), got %d requests: %v", len(gotQueries), gotQueries)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected a warning about the empty page before totalRecords was reached:\n%s", out)
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

// TestInspectRadarrLibrary_WantedCutoffPageCap_WarnsWhenHitWithoutCompleting
// pins the hard page-cap defense. maxWantedCutoffPages is temporarily
// lowered so the test doesn't need to make 1000 real HTTP round trips.
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

	inspectRadarrLibrary(context.Background(), logger, inst, nil)

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
