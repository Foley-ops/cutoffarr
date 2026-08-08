package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// newDecisionTestLogger builds a slog.Logger backed by a buffer, at the
// given level, matching the TextHandler setup main.go uses in production
// (see newRadarrTestLogger in radarr_test.go for the identical pattern).
func newDecisionTestLogger(level slog.Level) (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level}))
	return logger, &buf
}

// --- fetchQualityProfiles ------------------------------------------------

func TestFetchQualityProfiles_HappyPath_BuildsMapKeyedByID(t *testing.T) {
	profilesJSON := `[
		{"id": 1, "name": "HD-1080p", "upgradeAllowed": true, "cutoff": 7, "cutoffFormatScore": 10000},
		{"id": 2, "name": "Ultra-HD", "upgradeAllowed": false, "cutoff": 19, "cutoffFormatScore": 5000}
	]`
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/qualityprofile", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(profilesJSON))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	profiles, ok := fetchQualityProfiles(context.Background(), logger, client, inst)
	if !ok {
		t.Fatalf("fetchQualityProfiles returned ok=false, want true:\n%s", buf.String())
	}
	if len(profiles) != 2 {
		t.Fatalf("got %d profiles, want 2: %+v", len(profiles), profiles)
	}
	if profiles[1] != (qualityProfile{Name: "HD-1080p", CutoffFormatScore: 10000, UpgradeAllowed: true}) {
		t.Errorf("profiles[1] = %+v, want {HD-1080p 10000 true}", profiles[1])
	}
	if profiles[2] != (qualityProfile{Name: "Ultra-HD", CutoffFormatScore: 5000, UpgradeAllowed: false}) {
		t.Errorf("profiles[2] = %+v, want {Ultra-HD 5000 false}", profiles[2])
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("happy path should not warn:\n%s", buf.String())
	}
}

func TestFetchQualityProfiles_RequestFailure_SkipsInstance(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/qualityprofile", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-broken", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	_, ok := fetchQualityProfiles(context.Background(), logger, client, inst)
	if ok {
		t.Error("fetchQualityProfiles returned ok=true, want false when the request fails")
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "radarr-broken") {
		t.Errorf("expected a warning naming the instance:\n%s", out)
	}
}

// TestFetchQualityProfiles_MissingRequiredField_SkipsInstance pins refactor
// (c): decisions depend on every one of a profile's
// id/name/cutoffFormatScore/upgradeAllowed fields, unlike
// checkInstanceConnectivity's identically-shaped but informational-only
// fetch of the same endpoint (whose absent-field warnings never skip the
// instance). Here, a profile missing any required field must skip the
// whole instance for the cycle (§2.6), since the decision engine cannot
// safely evaluate rules 3/6 against an incomplete profile.
func TestFetchQualityProfiles_MissingRequiredField_SkipsInstance(t *testing.T) {
	profilesJSON := `[{"id": 1, "name": "HD-1080p", "upgradeAllowed": true, "cutoff": 7}]` // cutoffFormatScore absent
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/qualityprofile", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(profilesJSON))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-incomplete-profile", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	_, ok := fetchQualityProfiles(context.Background(), logger, client, inst)
	if ok {
		t.Error("fetchQualityProfiles returned ok=true, want false when a profile is missing a required field")
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "radarr-incomplete-profile") {
		t.Errorf("expected a warning naming the instance:\n%s", out)
	}
	if !strings.Contains(out, "cutoffFormatScore=absent") {
		t.Errorf("expected the warning to show the missing field as absent:\n%s", out)
	}
}

// --- resolveExclusionTagID ------------------------------------------------

func TestResolveExclusionTagID_LabelFound_CaseInsensitive(t *testing.T) {
	tagsJSON := `[{"id": 1, "label": "keep"}, {"id": 7, "label": "Cutoffarr-Exclude"}]`
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/tag", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(tagsJSON))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	id, found, ok := resolveExclusionTagID(context.Background(), logger, client, inst, "cutoffarr-exclude")
	if !ok {
		t.Fatalf("resolveExclusionTagID returned ok=false, want true:\n%s", buf.String())
	}
	if !found {
		t.Fatal("found = false, want true (label present with different case)")
	}
	if id != 7 {
		t.Errorf("id = %d, want 7", id)
	}
}

// TestResolveExclusionTagID_LabelNotDefined_PassesForAllMovies pins the
// binding rule: a successful /tag fetch that simply doesn't contain the
// configured label is NOT a failure. found=false, ok=true, and an info log
// explains that no movies will be excluded.
func TestResolveExclusionTagID_LabelNotDefined_PassesForAllMovies(t *testing.T) {
	tagsJSON := `[{"id": 1, "label": "keep"}]`
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/tag", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(tagsJSON))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	_, found, ok := resolveExclusionTagID(context.Background(), logger, client, inst, "cutoffarr-exclude")
	if !ok {
		t.Fatalf("resolveExclusionTagID returned ok=false, want true (missing label is not a failure):\n%s", buf.String())
	}
	if found {
		t.Error("found = true, want false: label is not present in this instance's tags")
	}
	out := buf.String()
	if !strings.Contains(out, "exclusion tag not defined in this instance; no movies excluded") {
		t.Errorf("expected the info log explaining no exclusions:\n%s", out)
	}
	if strings.Contains(out, "level=WARN") {
		t.Errorf("a simply-undefined label is not a failure and must not warn:\n%s", out)
	}
}

// TestResolveExclusionTagID_RequestFailure_SkipsInstance pins §2.6: we
// cannot safely evaluate rule 4's exclusions without this data, so a /tag
// request failure must skip the whole instance for the cycle.
func TestResolveExclusionTagID_RequestFailure_SkipsInstance(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/tag", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-broken", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	_, _, ok := resolveExclusionTagID(context.Background(), logger, client, inst, "cutoffarr-exclude")
	if ok {
		t.Error("resolveExclusionTagID returned ok=true, want false when /tag fails")
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "radarr-broken") {
		t.Errorf("expected a warning naming the instance:\n%s", out)
	}
}

func TestResolveExclusionTagID_MalformedJSON_SkipsInstance(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/tag", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-malformed", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	_, _, ok := resolveExclusionTagID(context.Background(), logger, client, inst, "cutoffarr-exclude")
	if ok {
		t.Error("resolveExclusionTagID returned ok=true, want false for malformed JSON")
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning:\n%s", buf.String())
	}
}

// TestResolveExclusionTagID_RecordMissingIdOrLabel_SkipsRecordNotInstance
// pins that an individual malformed tag record (missing id or label) is
// excluded from resolution with a warning but is NOT instance-fatal — only
// the whole /tag request failing is (§2.6), consistent with how
// fetchWantedCutoff treats a wanted/cutoff record missing its id.
func TestResolveExclusionTagID_RecordMissingIdOrLabel_SkipsRecordNotInstance(t *testing.T) {
	tagsJSON := `[{"label": "no-id"}, {"id": 9, "label": "cutoffarr-exclude"}]` // first record missing id
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/tag", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(tagsJSON))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	id, found, ok := resolveExclusionTagID(context.Background(), logger, client, inst, "cutoffarr-exclude")
	if !ok {
		t.Fatalf("resolveExclusionTagID returned ok=false, want true:\n%s", buf.String())
	}
	if !found || id != 9 {
		t.Errorf("expected the second, well-formed record to still resolve: found=%v id=%d", found, id)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning about the malformed record:\n%s", buf.String())
	}
}

// --- fetchMovieFileDetails / selectMovieFile ------------------------------

func TestFetchMovieFileDetails_HappyPath_PassesMovieIdAsQueryParam(t *testing.T) {
	var gotQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/moviefile", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Write([]byte(`[{"id": 42, "customFormatScore": 12000}]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	files, ok := fetchMovieFileDetails(context.Background(), logger, client, inst, 5)
	if !ok {
		t.Fatalf("fetchMovieFileDetails returned ok=false, want true:\n%s", buf.String())
	}
	if gotQuery != "movieId=5" {
		t.Errorf("query = %q, want movieId=5", gotQuery)
	}
	if len(files) != 1 || files[0].ID == nil || *files[0].ID != 42 || files[0].CustomFormatScore == nil || *files[0].CustomFormatScore != 12000 {
		t.Errorf("files = %+v, want one element {id:42 customFormatScore:12000}", files)
	}
}

func TestFetchMovieFileDetails_RequestFailure_ReturnsNotOK(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/moviefile", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-broken", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	_, ok := fetchMovieFileDetails(context.Background(), logger, client, inst, 5)
	if ok {
		t.Error("fetchMovieFileDetails returned ok=true, want false when the request fails")
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning:\n%s", buf.String())
	}
}

func TestFetchMovieFileDetails_MalformedJSON_ReturnsNotOK(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/moviefile", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-malformed", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	_, ok := fetchMovieFileDetails(context.Background(), logger, client, inst, 5)
	if ok {
		t.Error("fetchMovieFileDetails returned ok=true, want false for malformed JSON")
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning:\n%s", buf.String())
	}
}

func TestSelectMovieFile_SingleElement_TakenRegardlessOfID(t *testing.T) {
	files := []movieFileDetail{{ID: intPtr(999), CustomFormatScore: intPtr(1)}}
	got, found := selectMovieFile(files, intPtr(1)) // wantID deliberately mismatched
	if !found || got.CustomFormatScore == nil || *got.CustomFormatScore != 1 {
		t.Errorf("selectMovieFile = %+v, %v; want the single element regardless of id mismatch", got, found)
	}
}

func TestSelectMovieFile_MultipleElements_MatchesByID(t *testing.T) {
	files := []movieFileDetail{
		{ID: intPtr(1), CustomFormatScore: intPtr(10)},
		{ID: intPtr(2), CustomFormatScore: intPtr(20)},
	}
	got, found := selectMovieFile(files, intPtr(2))
	if !found || got.CustomFormatScore == nil || *got.CustomFormatScore != 20 {
		t.Errorf("selectMovieFile = %+v, %v; want the element matching id=2", got, found)
	}
}

func TestSelectMovieFile_MultipleElementsNoMatch_NotFound(t *testing.T) {
	files := []movieFileDetail{
		{ID: intPtr(1), CustomFormatScore: intPtr(10)},
		{ID: intPtr(2), CustomFormatScore: intPtr(20)},
	}
	_, found := selectMovieFile(files, intPtr(99))
	if found {
		t.Error("selectMovieFile found = true, want false when no element matches the wanted id")
	}
}

func TestSelectMovieFile_Empty_NotFound(t *testing.T) {
	_, found := selectMovieFile(nil, intPtr(1))
	if found {
		t.Error("selectMovieFile found = true, want false for an empty file list")
	}
}

func intPtr(i int) *int { return &i }

// --- evaluateMovie: STRICT decision rule ----------------------------------

// evaluateTestProfiles is a small fixed profile set reused across
// evaluateMovie tests: profile 1 allows upgrades with a CF cutoff of 100;
// profile 2 has upgrades disabled.
var evaluateTestProfiles = map[int]qualityProfile{
	1: {Name: "HD-1080p", CutoffFormatScore: 100, UpgradeAllowed: true},
	2: {Name: "Locked-Profile", CutoffFormatScore: 100, UpgradeAllowed: false},
}

// moviefileServer wires a mux serving moviefileJSON at /api/v3/moviefile
// for any movieId, recording every request into gotRequests.
func moviefileServer(t *testing.T, status int, moviefileJSON string, gotRequests *[]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/moviefile", func(w http.ResponseWriter, r *http.Request) {
		*gotRequests = append(*gotRequests, r.URL.RawQuery)
		w.WriteHeader(status)
		w.Write([]byte(moviefileJSON))
	})
	return httptest.NewServer(mux)
}

func boolPtr(b bool) *bool { return &b }

func TestEvaluateMovie_NoFile_SkipsWithReasonBeforeAnyMoviefileCall(t *testing.T) {
	var gotRequests []string
	srv := moviefileServer(t, http.StatusOK, `[]`, &gotRequests)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	m := movieListElement{ID: intPtr(1), Title: strPtr("No File Movie"), HasFile: boolPtr(false), QualityProfileID: intPtr(1)}
	d := evaluateMovie(context.Background(), logger, client, inst, m, evaluateTestProfiles, 99, true, map[int]bool{})

	if d.wouldUnmonitor {
		t.Error("wouldUnmonitor = true, want false")
	}
	if d.reason != "no file" {
		t.Errorf("reason = %q, want %q", d.reason, "no file")
	}
	if len(gotRequests) != 0 {
		t.Errorf("expected zero /moviefile requests for a no-file movie, got %d: %v", len(gotRequests), gotRequests)
	}
}

func TestEvaluateMovie_UnknownProfile_SkipsWithReason(t *testing.T) {
	var gotRequests []string
	srv := moviefileServer(t, http.StatusOK, `[]`, &gotRequests)
	defer srv.Close()
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	m := movieListElement{ID: intPtr(1), Title: strPtr("Unknown Profile Movie"), HasFile: boolPtr(true), QualityProfileID: intPtr(999)}
	d := evaluateMovie(context.Background(), logger, client, inst, m, evaluateTestProfiles, 99, true, map[int]bool{})

	if d.reason != "unknown quality profile" {
		t.Errorf("reason = %q, want %q", d.reason, "unknown quality profile")
	}
	if len(gotRequests) != 0 {
		t.Errorf("expected zero /moviefile requests, got %d", len(gotRequests))
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning about the unknown profile:\n%s", buf.String())
	}
}

func TestEvaluateMovie_UpgradesDisabled_SkipsWithExactReasonString(t *testing.T) {
	var gotRequests []string
	srv := moviefileServer(t, http.StatusOK, `[]`, &gotRequests)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	m := movieListElement{ID: intPtr(1), Title: strPtr("Locked Movie"), HasFile: boolPtr(true), QualityProfileID: intPtr(2)}
	d := evaluateMovie(context.Background(), logger, client, inst, m, evaluateTestProfiles, 99, true, map[int]bool{})

	// Exact string mandated by the brief.
	if d.reason != "profile has upgrades disabled" {
		t.Errorf("reason = %q, want exact %q", d.reason, "profile has upgrades disabled")
	}
	if d.profileName != "Locked-Profile" {
		t.Errorf("profileName = %q, want %q", d.profileName, "Locked-Profile")
	}
	if len(gotRequests) != 0 {
		t.Errorf("expected zero /moviefile requests, got %d", len(gotRequests))
	}
}

func TestEvaluateMovie_ExcludedByTag_SkipsWithReason(t *testing.T) {
	var gotRequests []string
	srv := moviefileServer(t, http.StatusOK, `[]`, &gotRequests)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	tags := []int{5, 99}
	m := movieListElement{ID: intPtr(1), Title: strPtr("Excluded Movie"), HasFile: boolPtr(true), QualityProfileID: intPtr(1), Tags: &tags}
	d := evaluateMovie(context.Background(), logger, client, inst, m, evaluateTestProfiles, 99, true, map[int]bool{})

	if d.reason != "excluded by tag" {
		t.Errorf("reason = %q, want %q", d.reason, "excluded by tag")
	}
	if len(gotRequests) != 0 {
		t.Errorf("expected zero /moviefile requests, got %d", len(gotRequests))
	}
}

// TestEvaluateMovie_TagNotActive_RuleFourAlwaysPasses pins that when the
// exclusion tag is not defined in this instance (tagActive=false), rule 4
// passes for every movie regardless of its actual tags — the movie below
// carries tag id 99 (which would exclude it if active) but must still
// proceed past rule 4 since tagActive is false.
func TestEvaluateMovie_TagNotActive_RuleFourAlwaysPasses(t *testing.T) {
	var gotRequests []string
	srv := moviefileServer(t, http.StatusOK, `[{"id": 1, "customFormatScore": 200}]`, &gotRequests)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	tags := []int{99}
	m := movieListElement{ID: intPtr(1), Title: strPtr("Movie"), HasFile: boolPtr(true), QualityProfileID: intPtr(1), Tags: &tags,
		MovieFile: &movieFileElement{ID: intPtr(1)}}
	d := evaluateMovie(context.Background(), logger, client, inst, m, evaluateTestProfiles, 99, false, map[int]bool{})

	if !d.wouldUnmonitor {
		t.Errorf("wouldUnmonitor = false, reason=%q; want true (rule 4 must not apply when tag is not active)", d.reason)
	}
}

func TestEvaluateMovie_InWantedCutoffSet_SkipsWithReason(t *testing.T) {
	var gotRequests []string
	srv := moviefileServer(t, http.StatusOK, `[]`, &gotRequests)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	m := movieListElement{ID: intPtr(1), Title: strPtr("Cutoff Not Met Movie"), HasFile: boolPtr(true), QualityProfileID: intPtr(1)}
	d := evaluateMovie(context.Background(), logger, client, inst, m, evaluateTestProfiles, 99, true, map[int]bool{1: true})

	if d.reason != "quality cutoff not met" {
		t.Errorf("reason = %q, want %q", d.reason, "quality cutoff not met")
	}
	if len(gotRequests) != 0 {
		t.Errorf("expected zero /moviefile requests for a movie in the wanted/cutoff set, got %d", len(gotRequests))
	}
}

func TestEvaluateMovie_MoviefileFetchFails_SkipsWithExactReasonString(t *testing.T) {
	var gotRequests []string
	srv := moviefileServer(t, http.StatusInternalServerError, `[]`, &gotRequests)
	defer srv.Close()
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	m := movieListElement{ID: intPtr(1), Title: strPtr("Fetch Fails Movie"), HasFile: boolPtr(true), QualityProfileID: intPtr(1)}
	d := evaluateMovie(context.Background(), logger, client, inst, m, evaluateTestProfiles, 99, true, map[int]bool{})

	// Exact string mandated by the brief.
	if d.reason != "could not fetch custom format score" {
		t.Errorf("reason = %q, want exact %q", d.reason, "could not fetch custom format score")
	}
	if len(gotRequests) != 1 {
		t.Errorf("expected exactly 1 /moviefile request, got %d", len(gotRequests))
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning:\n%s", buf.String())
	}
}

func TestEvaluateMovie_MoviefileMissingCustomFormatScore_SkipsWithReasonAndWarns(t *testing.T) {
	var gotRequests []string
	srv := moviefileServer(t, http.StatusOK, `[{"id": 1}]`, &gotRequests) // customFormatScore key absent
	defer srv.Close()
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	m := movieListElement{ID: intPtr(1), Title: strPtr("Missing Score Movie"), HasFile: boolPtr(true), QualityProfileID: intPtr(1),
		MovieFile: &movieFileElement{ID: intPtr(1)}}
	d := evaluateMovie(context.Background(), logger, client, inst, m, evaluateTestProfiles, 99, true, map[int]bool{})

	if d.reason != "could not fetch custom format score" {
		t.Errorf("reason = %q, want %q", d.reason, "could not fetch custom format score")
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning:\n%s", buf.String())
	}
}

func TestEvaluateMovie_CFBelowThreshold_SkipsWithReason(t *testing.T) {
	var gotRequests []string
	srv := moviefileServer(t, http.StatusOK, `[{"id": 1, "customFormatScore": 50}]`, &gotRequests) // threshold is 100
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	m := movieListElement{ID: intPtr(1), Title: strPtr("Below Threshold Movie"), HasFile: boolPtr(true), QualityProfileID: intPtr(1),
		MovieFile: &movieFileElement{ID: intPtr(1)}}
	d := evaluateMovie(context.Background(), logger, client, inst, m, evaluateTestProfiles, 99, true, map[int]bool{})

	if d.wouldUnmonitor {
		t.Error("wouldUnmonitor = true, want false")
	}
	if d.reason != "custom format cutoff not met" {
		t.Errorf("reason = %q, want %q", d.reason, "custom format cutoff not met")
	}
	if d.cfScore == nil || *d.cfScore != 50 {
		t.Errorf("cfScore = %v, want 50", d.cfScore)
	}
}

// TestEvaluateMovie_AllRulesPass_WouldUnmonitorWithExactReasonString pins
// the plan's verbatim would-unmonitor reason text: "cutoff met".
func TestEvaluateMovie_AllRulesPass_WouldUnmonitorWithExactReasonString(t *testing.T) {
	var gotRequests []string
	srv := moviefileServer(t, http.StatusOK, `[{"id": 1, "customFormatScore": 150}]`, &gotRequests) // threshold is 100
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	m := movieListElement{ID: intPtr(1), Title: strPtr("Passes All Movie"), HasFile: boolPtr(true), QualityProfileID: intPtr(1),
		MovieFile: &movieFileElement{ID: intPtr(1)}}
	d := evaluateMovie(context.Background(), logger, client, inst, m, evaluateTestProfiles, 99, true, map[int]bool{})

	if !d.wouldUnmonitor {
		t.Fatalf("wouldUnmonitor = false, reason=%q; want true", d.reason)
	}
	// Exact string mandated by the brief's report-line example.
	if d.reason != "cutoff met" {
		t.Errorf("reason = %q, want exact %q", d.reason, "cutoff met")
	}
	if d.profileName != "HD-1080p" {
		t.Errorf("profileName = %q, want %q", d.profileName, "HD-1080p")
	}
	if d.cfScore == nil || *d.cfScore != 150 {
		t.Errorf("cfScore = %v, want 150", d.cfScore)
	}
}

// TestEvaluateMovie_CFEqualToThreshold_PassesRuleSix pins the ">=" boundary
// from the STRICT rule ("customFormatScore >= the profile's
// cutoffFormatScore").
func TestEvaluateMovie_CFEqualToThreshold_PassesRuleSix(t *testing.T) {
	var gotRequests []string
	srv := moviefileServer(t, http.StatusOK, `[{"id": 1, "customFormatScore": 100}]`, &gotRequests) // exactly the threshold
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	m := movieListElement{ID: intPtr(1), Title: strPtr("Exactly At Threshold Movie"), HasFile: boolPtr(true), QualityProfileID: intPtr(1),
		MovieFile: &movieFileElement{ID: intPtr(1)}}
	d := evaluateMovie(context.Background(), logger, client, inst, m, evaluateTestProfiles, 99, true, map[int]bool{})

	if !d.wouldUnmonitor {
		t.Errorf("wouldUnmonitor = false, reason=%q; want true (score equal to threshold must pass rule 6)", d.reason)
	}
}

func strPtr(s string) *string { return &s }

// --- runRadarrDecisionEngine: orchestrator --------------------------------

// decisionEngineTestServer wires a mux serving profilesJSON at
// /api/v3/qualityprofile, tagsJSON at /api/v3/tag, and moviefileHandler at
// /api/v3/moviefile, recording every /api/v3/moviefile request's raw query
// into gotMoviefileRequests.
func decisionEngineTestServer(t *testing.T, profilesJSON, tagsJSON string, moviefileHandler http.HandlerFunc, gotMoviefileRequests *[]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/qualityprofile", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(profilesJSON))
	})
	mux.HandleFunc("/api/v3/tag", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(tagsJSON))
	})
	mux.HandleFunc("/api/v3/moviefile", func(w http.ResponseWriter, r *http.Request) {
		*gotMoviefileRequests = append(*gotMoviefileRequests, r.URL.RawQuery)
		moviefileHandler(w, r)
	})
	return httptest.NewServer(mux)
}

const decisionEngineProfilesJSON = `[{"id": 1, "name": "HD-1080p", "upgradeAllowed": true, "cutoffFormatScore": 100}]`
const decisionEngineNoTagsJSON = `[]`

func staticMoviefileHandler(customFormatScore int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id": 1, "customFormatScore": ` + strconv.Itoa(customFormatScore) + `}]`))
	}
}

func TestRunRadarrDecisionEngine_UnmonitoredMovies_ExcludedFromReportEntirely(t *testing.T) {
	var gotMoviefileRequests []string
	srv := decisionEngineTestServer(t, decisionEngineProfilesJSON, decisionEngineNoTagsJSON, staticMoviefileHandler(200), &gotMoviefileRequests)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	movies := []movieListElement{
		{ID: intPtr(1), Title: strPtr("Unmonitored Movie"), Monitored: boolPtr(false), HasFile: boolPtr(true), QualityProfileID: intPtr(1)},
	}
	runRadarrDecisionEngine(context.Background(), logger, inst, movies, map[int]bool{}, "cutoffarr-exclude")

	out := buf.String()
	if strings.Contains(out, "Unmonitored Movie") {
		t.Errorf("unmonitored movie must produce no report line at all:\n%s", out)
	}
	if len(gotMoviefileRequests) != 0 {
		t.Errorf("expected zero /moviefile requests for an unmonitored movie, got %d", len(gotMoviefileRequests))
	}
	if !strings.Contains(out, "totalMonitored=0") {
		t.Errorf("expected totalMonitored=0 in the summary:\n%s", out)
	}
}

func TestRunRadarrDecisionEngine_LogsWouldUnmonitorAndSkipLinesWithMandatedAttrs(t *testing.T) {
	var gotMoviefileRequests []string
	srv := decisionEngineTestServer(t, decisionEngineProfilesJSON, decisionEngineNoTagsJSON, staticMoviefileHandler(200), &gotMoviefileRequests)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	movies := []movieListElement{
		{ID: intPtr(1), Title: strPtr("Would Unmonitor Movie"), Monitored: boolPtr(true), HasFile: boolPtr(true), QualityProfileID: intPtr(1),
			MovieFile: &movieFileElement{ID: intPtr(1)}},
		{ID: intPtr(2), Title: strPtr("No File Movie"), Monitored: boolPtr(true), HasFile: boolPtr(false), QualityProfileID: intPtr(1)},
	}
	runRadarrDecisionEngine(context.Background(), logger, inst, movies, map[int]bool{}, "cutoffarr-exclude")

	out := buf.String()
	if !strings.Contains(out, `msg=would-unmonitor`) {
		t.Errorf("expected a would-unmonitor log line:\n%s", out)
	}
	for _, want := range []string{`title="Would Unmonitor Movie"`, `reason="cutoff met"`, `profile=HD-1080p`, "instance=radarr-main"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected would-unmonitor line to contain %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, `msg=skip`) {
		t.Errorf("expected a skip log line:\n%s", out)
	}
	for _, want := range []string{`title="No File Movie"`, `reason="no file"`, "instance=radarr-main"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected skip line to contain %q:\n%s", want, out)
		}
	}
}

// TestRunRadarrDecisionEngine_MoviefileFetchedOnlyForMoviesPassingRulesOneThroughFive
// is the self-review-mandated request-count assertion at the orchestrator
// level: across a mix of movies failing at every earlier rule plus one that
// reaches rule 6, exactly one /moviefile request must be made.
func TestRunRadarrDecisionEngine_MoviefileFetchedOnlyForMoviesPassingRulesOneThroughFive(t *testing.T) {
	var gotMoviefileRequests []string
	srv := decisionEngineTestServer(t, decisionEngineProfilesJSON, decisionEngineNoTagsJSON, staticMoviefileHandler(200), &gotMoviefileRequests)
	defer srv.Close()

	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	movies := []movieListElement{
		{ID: intPtr(1), Title: strPtr("Unmonitored"), Monitored: boolPtr(false), HasFile: boolPtr(true), QualityProfileID: intPtr(1)},
		{ID: intPtr(2), Title: strPtr("No File"), Monitored: boolPtr(true), HasFile: boolPtr(false), QualityProfileID: intPtr(1)},
		{ID: intPtr(3), Title: strPtr("Unknown Profile"), Monitored: boolPtr(true), HasFile: boolPtr(true), QualityProfileID: intPtr(999)},
		{ID: intPtr(4), Title: strPtr("In Wanted Set"), Monitored: boolPtr(true), HasFile: boolPtr(true), QualityProfileID: intPtr(1)},
		{ID: intPtr(5), Title: strPtr("Reaches Rule Six"), Monitored: boolPtr(true), HasFile: boolPtr(true), QualityProfileID: intPtr(1),
			MovieFile: &movieFileElement{ID: intPtr(1)}},
	}
	wantedIDs := map[int]bool{4: true}
	runRadarrDecisionEngine(context.Background(), logger, inst, movies, wantedIDs, "cutoffarr-exclude")

	if len(gotMoviefileRequests) != 1 {
		t.Errorf("expected exactly 1 /moviefile request, got %d: %v", len(gotMoviefileRequests), gotMoviefileRequests)
	}
	if len(gotMoviefileRequests) == 1 && gotMoviefileRequests[0] != "movieId=5" {
		t.Errorf("the single /moviefile request = %q, want movieId=5", gotMoviefileRequests[0])
	}
}

func TestRunRadarrDecisionEngine_ProfileFetchFailure_NoReportLinesAtAll(t *testing.T) {
	var gotMoviefileRequests []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/qualityprofile", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-broken", Type: "radarr", URL: srv.URL, APIKey: "key"}

	movies := []movieListElement{
		{ID: intPtr(1), Title: strPtr("Some Movie"), Monitored: boolPtr(true), HasFile: boolPtr(true), QualityProfileID: intPtr(1)},
	}
	runRadarrDecisionEngine(context.Background(), logger, inst, movies, map[int]bool{}, "cutoffarr-exclude")

	out := buf.String()
	if strings.Contains(out, "msg=would-unmonitor") || strings.Contains(out, "msg=skip") {
		t.Errorf("a failed profile fetch must skip the whole instance: no report lines at all:\n%s", out)
	}
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected a warning:\n%s", out)
	}
	if len(gotMoviefileRequests) != 0 {
		t.Errorf("expected zero /moviefile requests, got %d", len(gotMoviefileRequests))
	}
}

func TestRunRadarrDecisionEngine_TagFetchFailure_NoReportLinesAtAll(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/qualityprofile", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(decisionEngineProfilesJSON))
	})
	mux.HandleFunc("/api/v3/tag", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-broken", Type: "radarr", URL: srv.URL, APIKey: "key"}

	movies := []movieListElement{
		{ID: intPtr(1), Title: strPtr("Some Movie"), Monitored: boolPtr(true), HasFile: boolPtr(true), QualityProfileID: intPtr(1)},
	}
	runRadarrDecisionEngine(context.Background(), logger, inst, movies, map[int]bool{}, "cutoffarr-exclude")

	out := buf.String()
	if strings.Contains(out, "msg=would-unmonitor") || strings.Contains(out, "msg=skip") {
		t.Errorf("a failed tag fetch must skip the whole instance: no report lines at all:\n%s", out)
	}
}

func TestRunRadarrDecisionEngine_SummaryCountsCorrect(t *testing.T) {
	var gotMoviefileRequests []string
	srv := decisionEngineTestServer(t, decisionEngineProfilesJSON, decisionEngineNoTagsJSON, staticMoviefileHandler(200), &gotMoviefileRequests)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	movies := []movieListElement{
		{ID: intPtr(1), Title: strPtr("Unmonitored"), Monitored: boolPtr(false), HasFile: boolPtr(true), QualityProfileID: intPtr(1)},
		{ID: intPtr(2), Title: strPtr("No File A"), Monitored: boolPtr(true), HasFile: boolPtr(false), QualityProfileID: intPtr(1)},
		{ID: intPtr(3), Title: strPtr("No File B"), Monitored: boolPtr(true), HasFile: boolPtr(false), QualityProfileID: intPtr(1)},
		{ID: intPtr(4), Title: strPtr("Would Unmonitor"), Monitored: boolPtr(true), HasFile: boolPtr(true), QualityProfileID: intPtr(1),
			MovieFile: &movieFileElement{ID: intPtr(1)}},
	}
	runRadarrDecisionEngine(context.Background(), logger, inst, movies, map[int]bool{}, "cutoffarr-exclude")

	out := buf.String()
	if !strings.Contains(out, "totalMonitored=3") {
		t.Errorf("expected totalMonitored=3 (excludes the unmonitored movie):\n%s", out)
	}
	if !strings.Contains(out, "wouldUnmonitor=1") {
		t.Errorf("expected wouldUnmonitor=1:\n%s", out)
	}
	if !strings.Contains(out, "no file=2") {
		t.Errorf("expected the skip-reason count 'no file=2' in the summary:\n%s", out)
	}
}

// TestRunRadarrDecisionEngine_PerMovieMoviefileFailure_DoesNotStopOtherMovies
// pins the plan's binding per-movie-not-instance-skip rule explicitly at
// the orchestrator level: when /moviefile fails for one movie partway
// through the library, the loop must still evaluate and report every other
// movie, not abort the rest of the instance.
func TestRunRadarrDecisionEngine_PerMovieMoviefileFailure_DoesNotStopOtherMovies(t *testing.T) {
	var gotMoviefileRequests []string
	// id 1 fails (500); id 3 succeeds. Movie order is 1, 2 (no file, never
	// reaches moviefile), 3, so the failure happens before the last movie
	// is evaluated.
	moviefileHandler := func(w http.ResponseWriter, r *http.Request) {
		gotMoviefileRequests = append(gotMoviefileRequests, r.URL.RawQuery)
		if r.URL.Query().Get("movieId") == "1" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`[{"id": 1, "customFormatScore": 200}]`))
	}
	srv := decisionEngineTestServer(t, decisionEngineProfilesJSON, decisionEngineNoTagsJSON, moviefileHandler, &gotMoviefileRequests)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	movies := []movieListElement{
		{ID: intPtr(1), Title: strPtr("Fails Movie"), Monitored: boolPtr(true), HasFile: boolPtr(true), QualityProfileID: intPtr(1),
			MovieFile: &movieFileElement{ID: intPtr(1)}},
		{ID: intPtr(2), Title: strPtr("No File Movie"), Monitored: boolPtr(true), HasFile: boolPtr(false), QualityProfileID: intPtr(1)},
		{ID: intPtr(3), Title: strPtr("Succeeds Movie"), Monitored: boolPtr(true), HasFile: boolPtr(true), QualityProfileID: intPtr(1),
			MovieFile: &movieFileElement{ID: intPtr(1)}},
	}
	runRadarrDecisionEngine(context.Background(), logger, inst, movies, map[int]bool{}, "cutoffarr-exclude")

	out := buf.String()
	if !strings.Contains(out, `title="Fails Movie"`) || !strings.Contains(out, `reason="could not fetch custom format score"`) {
		t.Errorf("expected the failing movie's skip line:\n%s", out)
	}
	if !strings.Contains(out, `title="No File Movie"`) || !strings.Contains(out, `reason="no file"`) {
		t.Errorf("expected the no-file movie to still be reported (not skipped due to the earlier failure):\n%s", out)
	}
	if !strings.Contains(out, `title="Succeeds Movie"`) || !strings.Contains(out, "msg=would-unmonitor") {
		t.Errorf("expected the later movie to still be evaluated and reported would-unmonitor:\n%s", out)
	}
	if !strings.Contains(out, "totalMonitored=3") {
		t.Errorf("expected all 3 monitored movies counted:\n%s", out)
	}
}

// TestRunRadarrDecisionEngine_MovieMissingID_WarnsAndExcludedFromReport is a
// defensive-coding test: a monitored movie missing its id field cannot be
// evaluated against rules 5/6 (no id to look up) or safely reported, so it
// is warned about and excluded from the report/counts rather than crashing
// or producing a misleading line.
func TestRunRadarrDecisionEngine_MovieMissingID_WarnsAndExcludedFromReport(t *testing.T) {
	var gotMoviefileRequests []string
	srv := decisionEngineTestServer(t, decisionEngineProfilesJSON, decisionEngineNoTagsJSON, staticMoviefileHandler(200), &gotMoviefileRequests)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	movies := []movieListElement{
		{Title: strPtr("No Id Movie"), Monitored: boolPtr(true), HasFile: boolPtr(true), QualityProfileID: intPtr(1)},
	}
	runRadarrDecisionEngine(context.Background(), logger, inst, movies, map[int]bool{}, "cutoffarr-exclude")

	out := buf.String()
	if strings.Contains(out, "msg=would-unmonitor") || strings.Contains(out, "msg=skip") {
		t.Errorf("a movie missing its id must not produce a decision report line:\n%s", out)
	}
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "No Id Movie") {
		t.Errorf("expected a warning naming the movie missing its id:\n%s", out)
	}
	if !strings.Contains(out, "totalMonitored=0") {
		t.Errorf("expected the movie missing id to not be counted:\n%s", out)
	}
}

// --- sampleEveryKth --------------------------------------------------------

func TestSampleEveryKth_FewerThanN_ReturnsAllSorted(t *testing.T) {
	got := sampleEveryKth([]int{5, 1, 3}, 10)
	want := []int{1, 3, 5}
	if len(got) != len(want) {
		t.Fatalf("sampleEveryKth = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sampleEveryKth[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}

func TestSampleEveryKth_Empty_ReturnsEmpty(t *testing.T) {
	if got := sampleEveryKth(nil, 10); len(got) != 0 {
		t.Errorf("sampleEveryKth(nil, 10) = %v, want empty", got)
	}
}

// TestSampleEveryKth_MoreThanN_ReturnsExactlyNDeterministically pins the
// binding requirement: deterministic randomness seeded from movie-id
// ordering (sort candidates by id, take every k-th), not math/rand.
// Calling twice with the same input must yield the same result.
func TestSampleEveryKth_MoreThanN_ReturnsExactlyNDeterministically(t *testing.T) {
	ids := make([]int, 25)
	for i := range ids {
		ids[i] = 100 - i // deliberately unsorted, descending
	}
	got1 := sampleEveryKth(ids, 10)
	got2 := sampleEveryKth(ids, 10)
	if len(got1) != 10 {
		t.Fatalf("got %d items, want exactly 10: %v", len(got1), got1)
	}
	for i := range got1 {
		if got1[i] != got2[i] {
			t.Fatalf("non-deterministic: first call %v, second call %v", got1, got2)
		}
	}
	for i := 1; i < len(got1); i++ {
		if got1[i] <= got1[i-1] {
			t.Errorf("sample not sorted ascending: %v", got1)
		}
	}
}

// --- runCrossCheck -----------------------------------------------------

func agreeTrue() *bool  { return boolPtr(true) }
func agreeFalse() *bool { return boolPtr(false) }

func TestRunCrossCheck_ZeroCandidates_PassesWithZeroItems(t *testing.T) {
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr"}

	passed, checked := runCrossCheck(logger, inst, nil, map[int]bool{})
	if !passed || checked != 0 {
		t.Errorf("runCrossCheck = (%v, %d), want (true, 0) for zero candidates", passed, checked)
	}
	if strings.Contains(buf.String(), "level=ERROR") {
		t.Errorf("no candidates should never error:\n%s", buf.String())
	}
}

// TestRunCrossCheck_AgreeingSamples_PassesAndLogsBothCategories covers both
// candidate categories: a would-unmonitor decision (not in wanted set,
// qualityCutoffNotMet=false: agree) restating its CF score/threshold, and a
// monitored+hasFile skip decision (in wanted set, qualityCutoffNotMet=true:
// agree).
func TestRunCrossCheck_AgreeingSamples_PassesAndLogsBothCategories(t *testing.T) {
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr"}

	score := 150
	decisions := []movieDecision{
		{id: 1, title: "Would Unmonitor Movie", wouldUnmonitor: true, hasFile: true, qualityCutoffNotMet: agreeFalse(), cfScore: &score, cfThreshold: 100},
		{id: 2, title: "Skip In Cutoff Movie", wouldUnmonitor: false, hasFile: true, reason: "quality cutoff not met", qualityCutoffNotMet: agreeTrue()},
	}
	wantedIDs := map[int]bool{2: true} // id 1 not in set (agrees with false); id 2 in set (agrees with true)

	passed, checked := runCrossCheck(logger, inst, decisions, wantedIDs)
	if !passed {
		t.Errorf("passed = false, want true:\n%s", buf.String())
	}
	if checked != 2 {
		t.Errorf("checked = %d, want 2", checked)
	}
	out := buf.String()
	if strings.Contains(out, "level=ERROR") {
		t.Errorf("agreeing samples must not error:\n%s", out)
	}
	if !strings.Contains(out, "msg=cross-check") {
		t.Errorf("expected cross-check log lines:\n%s", out)
	}
	for _, want := range []string{`title="Would Unmonitor Movie"`, "cfScore=150", "cfThreshold=100"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected would-unmonitor cross-check line to contain %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, `title="Skip In Cutoff Movie"`) {
		t.Errorf("expected the skip-category candidate to also be cross-checked:\n%s", out)
	}
}

// TestRunCrossCheck_Disagreement_FailsWithErrorLog pins the binding rule:
// any disagreement between wanted-set membership and
// movieFile.qualityCutoffNotMet is an error-level log naming the movie and
// both values, and the caller's summary must read "FAILED" — the human
// gate is meant to catch this before any write path exists.
func TestRunCrossCheck_Disagreement_FailsWithErrorLog(t *testing.T) {
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr"}

	decisions := []movieDecision{
		// In the wanted set (inWantedSet=true) but qualityCutoffNotMet
		// says false: disagreement.
		{id: 1, title: "Disagreeing Movie", wouldUnmonitor: false, hasFile: true, reason: "quality cutoff not met", qualityCutoffNotMet: agreeFalse()},
	}
	wantedIDs := map[int]bool{1: true}

	passed, checked := runCrossCheck(logger, inst, decisions, wantedIDs)
	if passed {
		t.Error("passed = true, want false for a disagreement")
	}
	if checked != 1 {
		t.Errorf("checked = %d, want 1", checked)
	}
	out := buf.String()
	if !strings.Contains(out, "level=ERROR") {
		t.Errorf("expected an error-level log for the disagreement:\n%s", out)
	}
	if !strings.Contains(out, "Disagreeing Movie") {
		t.Errorf("expected the error to name the movie:\n%s", out)
	}
	if !strings.Contains(out, "inWantedSet=true") || !strings.Contains(out, "qualityCutoffNotMet=false") {
		t.Errorf("expected the error to state both values:\n%s", out)
	}
}

// TestRunCrossCheck_SamplesUpToTenPerCategory pins the up-to-10-per-
// category sampling cap: 15 would-unmonitor candidates (all agreeing) plus
// 15 skip candidates (all agreeing) must be capped at 10 + 10 = 20 checked
// items, not 30.
func TestRunCrossCheck_SamplesUpToTenPerCategory(t *testing.T) {
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr"}

	var decisions []movieDecision
	wantedIDs := map[int]bool{}
	for i := 1; i <= 15; i++ {
		decisions = append(decisions, movieDecision{id: i, title: "WU", wouldUnmonitor: true, hasFile: true, qualityCutoffNotMet: agreeFalse()})
	}
	for i := 101; i <= 115; i++ {
		decisions = append(decisions, movieDecision{id: i, title: "Skip", wouldUnmonitor: false, hasFile: true, qualityCutoffNotMet: agreeFalse()})
		wantedIDs[i] = false
	}

	passed, checked := runCrossCheck(logger, inst, decisions, wantedIDs)
	if !passed {
		t.Error("passed = false, want true (all candidates agree)")
	}
	if checked != 20 {
		t.Errorf("checked = %d, want 20 (10 would-unmonitor + 10 skip)", checked)
	}
}

// TestRunCrossCheck_QualityCutoffNotMetAbsent_WarnsButDoesNotFailTheCheck
// pins a defensive edge case: if movieFile.qualityCutoffNotMet is entirely
// absent from the /movie data for a sampled candidate (e.g. an older Radarr
// response shape), silently treating that as "false" could mask a genuine
// disagreement. It must instead warn distinctly and not, on its own,
// contribute to a FAILED verdict.
func TestRunCrossCheck_QualityCutoffNotMetAbsent_WarnsButDoesNotFailTheCheck(t *testing.T) {
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr"}

	decisions := []movieDecision{
		{id: 1, title: "Missing Field Movie", wouldUnmonitor: true, hasFile: true, qualityCutoffNotMet: nil, cfScore: intPtr(200), cfThreshold: 100},
	}
	passed, checked := runCrossCheck(logger, inst, decisions, map[int]bool{})
	if !passed {
		t.Error("passed = false, want true: an absent field alone must not fail the check")
	}
	if checked != 1 {
		t.Errorf("checked = %d, want 1", checked)
	}
	out := buf.String()
	if strings.Contains(out, "level=ERROR") {
		t.Errorf("an absent field must warn, not error:\n%s", out)
	}
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "Missing Field Movie") {
		t.Errorf("expected a warning naming the movie with the absent field:\n%s", out)
	}
}

// TestRunCrossCheck_HasFileFalseSkip_ExcludedFromCandidates pins the
// candidate filter: a skip decision for a movie with hasFile==false (e.g.
// "no file") is not a "monitored+hasFile skip item" and must not be
// sampled at all.
func TestRunCrossCheck_HasFileFalseSkip_ExcludedFromCandidates(t *testing.T) {
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr"}

	decisions := []movieDecision{
		{id: 1, title: "No File Movie", wouldUnmonitor: false, hasFile: false, reason: "no file"},
	}
	_, checked := runCrossCheck(logger, inst, decisions, map[int]bool{})
	if checked != 0 {
		t.Errorf("checked = %d, want 0 (hasFile=false skip must not be a cross-check candidate)", checked)
	}
}

// TestRunRadarrDecisionEngine_CrossCheckPassed_SummaryStatesPassedWithCount
// and TestRunRadarrDecisionEngine_CrossCheckDisagreement_SummaryStatesFailed
// pin the full pipeline's cross-check integration end to end: the
// orchestrator threads its own decisions and wantedIDs into runCrossCheck
// and reflects the result in the end-of-instance summary line, per the
// plan's binding requirement that a disagreement's FAILED state is visible
// in exactly the log the human gate reads.
func TestRunRadarrDecisionEngine_CrossCheckPassed_SummaryStatesPassedWithCount(t *testing.T) {
	var gotMoviefileRequests []string
	// movieFile.qualityCutoffNotMet=false agrees with "not in wanted set".
	moviefileHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id": 1, "customFormatScore": 200}]`))
	}
	srv := decisionEngineTestServer(t, decisionEngineProfilesJSON, decisionEngineNoTagsJSON, moviefileHandler, &gotMoviefileRequests)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	movies := []movieListElement{
		{ID: intPtr(1), Title: strPtr("Would Unmonitor Movie"), Monitored: boolPtr(true), HasFile: boolPtr(true), QualityProfileID: intPtr(1),
			MovieFile: &movieFileElement{ID: intPtr(1), QualityCutoffNotMet: boolPtr(false)}},
	}
	runRadarrDecisionEngine(context.Background(), logger, inst, movies, map[int]bool{}, "cutoffarr-exclude")

	out := buf.String()
	if strings.Contains(out, "level=ERROR") {
		t.Errorf("agreeing cross-check must not error:\n%s", out)
	}
	if !strings.Contains(out, `crossCheck="passed (1 items)"`) {
		t.Errorf("expected the summary to state the cross-check passed with its item count:\n%s", out)
	}
}

func TestRunRadarrDecisionEngine_CrossCheckDisagreement_SummaryStatesFailed(t *testing.T) {
	var gotMoviefileRequests []string
	moviefileHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id": 1, "customFormatScore": 200}]`))
	}
	srv := decisionEngineTestServer(t, decisionEngineProfilesJSON, decisionEngineNoTagsJSON, moviefileHandler, &gotMoviefileRequests)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	// wouldUnmonitor (not in wantedIDs, score 200 >= threshold 100), but
	// movieFile.qualityCutoffNotMet=true disagrees with "not in set".
	movies := []movieListElement{
		{ID: intPtr(1), Title: strPtr("Disagreeing Movie"), Monitored: boolPtr(true), HasFile: boolPtr(true), QualityProfileID: intPtr(1),
			MovieFile: &movieFileElement{ID: intPtr(1), QualityCutoffNotMet: boolPtr(true)}},
	}
	runRadarrDecisionEngine(context.Background(), logger, inst, movies, map[int]bool{}, "cutoffarr-exclude")

	out := buf.String()
	if !strings.Contains(out, "level=ERROR") {
		t.Errorf("expected an error-level log for the cross-check disagreement:\n%s", out)
	}
	if !strings.Contains(out, "crossCheck=FAILED") {
		t.Errorf("expected the summary to state the cross-check FAILED:\n%s", out)
	}
}

func TestFetchQualityProfiles_MalformedJSON_SkipsInstance(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/qualityprofile", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-malformed", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	_, ok := fetchQualityProfiles(context.Background(), logger, client, inst)
	if ok {
		t.Error("fetchQualityProfiles returned ok=true, want false for malformed JSON")
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning:\n%s", buf.String())
	}
}
