package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
