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

// noTags is a shared present-but-empty tags slice, used by evaluateMovie
// fixtures that need to pass rule 4 cleanly (as opposed to Tags left nil,
// which after FIX 1 is untrusted input when the exclusion tag is active —
// see TestEvaluateMovie_TagActiveAndTagsNil_SkipsWithReasonAndWarns).
// Read-only; safe to share a single backing slice across tests.
var noTags = []int{}

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
	if d.reason != ReasonNoFile {
		t.Errorf("reason = %q, want %q", d.reason, ReasonNoFile)
	}
	// FIX 5 (controller-mandated correction): the profile display name is
	// resolved via a map read only (no evaluation-order or reason change)
	// before rule 2, so a "no file" report line still carries a usable
	// profile attr instead of being blank.
	if d.profileName != "HD-1080p" {
		t.Errorf("profileName = %q, want %q (resolved even though rule 2 failed first)", d.profileName, "HD-1080p")
	}
	if len(gotRequests) != 0 {
		t.Errorf("expected zero /moviefile requests for a no-file movie, got %d: %v", len(gotRequests), gotRequests)
	}
}

// TestEvaluateMovie_NoFile_UnknownProfileID_ProfileNameStaysEmptyNotUnknown
// pins FIX 5's boundary: "unknown" is reserved specifically for a movie
// that actually reaches and fails rule 3 for that reason. A "no file" (rule
// 2) skip with an unresolvable profile id must not be eagerly mislabeled
// "unknown" — that would misleadingly suggest rule 3 was evaluated (and
// failed) when it was never reached.
func TestEvaluateMovie_NoFile_UnknownProfileID_ProfileNameStaysEmptyNotUnknown(t *testing.T) {
	var gotRequests []string
	srv := moviefileServer(t, http.StatusOK, `[]`, &gotRequests)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	m := movieListElement{ID: intPtr(1), Title: strPtr("No File Unknown Profile Movie"), HasFile: boolPtr(false), QualityProfileID: intPtr(999)}
	d := evaluateMovie(context.Background(), logger, client, inst, m, evaluateTestProfiles, 99, true, map[int]bool{})

	if d.reason != ReasonNoFile {
		t.Errorf("reason = %q, want %q", d.reason, ReasonNoFile)
	}
	if d.profileName != "" {
		t.Errorf("profileName = %q, want empty (rule 3 was never reached, so it must not read \"unknown\")", d.profileName)
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

	if d.reason != ReasonUnknownProfile {
		t.Errorf("reason = %q, want %q", d.reason, ReasonUnknownProfile)
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
	if d.reason != ReasonUpgradesDisabled {
		t.Errorf("reason = %q, want exact %q", d.reason, ReasonUpgradesDisabled)
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

	if d.reason != ReasonExcludedByTag {
		t.Errorf("reason = %q, want %q", d.reason, ReasonExcludedByTag)
	}
	if len(gotRequests) != 0 {
		t.Errorf("expected zero /moviefile requests, got %d", len(gotRequests))
	}
}

// TestEvaluateMovie_ExcludedByTagAndInWantedSet_TagReasonWins is FIX 4's
// rule-precedence pin: a movie that fails BOTH rule 4 (excluded by tag) and
// rule 5 (in the wanted/cutoff set, i.e. would also fail on quality cutoff)
// must report rule 4's reason, since rules are evaluated in order and the
// first failing rule wins. If rule 5 were consulted first (or the id set
// weren't consulted at all because the code fell through past rule 4
// incorrectly), this would report "quality cutoff not met" instead.
func TestEvaluateMovie_ExcludedByTagAndInWantedSet_TagReasonWins(t *testing.T) {
	var gotRequests []string
	srv := moviefileServer(t, http.StatusOK, `[]`, &gotRequests)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	tags := []int{99} // 99 is the exclusion tag id used throughout this file's fixtures
	m := movieListElement{ID: intPtr(1), Title: strPtr("Excluded And In Cutoff Movie"), HasFile: boolPtr(true), QualityProfileID: intPtr(1), Tags: &tags}
	wantedIDs := map[int]bool{1: true} // also fails rule 5, if it were reached
	d := evaluateMovie(context.Background(), logger, client, inst, m, evaluateTestProfiles, 99, true, wantedIDs)

	if d.reason != ReasonExcludedByTag {
		t.Errorf("reason = %q, want %q (rule 4 must win over rule 5 per the mandated evaluation order)", d.reason, ReasonExcludedByTag)
	}
	if len(gotRequests) != 0 {
		t.Errorf("expected zero /moviefile requests, got %d", len(gotRequests))
	}
}

// TestEvaluateMovie_UpgradesDisabledAndInWantedSet_UpgradesDisabledReasonWins
// is FIX 4's other rule-precedence pin: a movie whose profile has upgrades
// disabled (rule 3) that would ALSO fail rule 5 (in the wanted/cutoff set)
// must report rule 3's reason — the earlier rule in evaluation order — not
// rule 5's, and must never reach /moviefile.
func TestEvaluateMovie_UpgradesDisabledAndInWantedSet_UpgradesDisabledReasonWins(t *testing.T) {
	var gotRequests []string
	srv := moviefileServer(t, http.StatusOK, `[]`, &gotRequests)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	m := movieListElement{ID: intPtr(1), Title: strPtr("Locked And In Cutoff Movie"), HasFile: boolPtr(true), QualityProfileID: intPtr(2)} // profile 2 has upgrades disabled
	wantedIDs := map[int]bool{1: true}                                                                                                     // also fails rule 5, if it were reached
	d := evaluateMovie(context.Background(), logger, client, inst, m, evaluateTestProfiles, 99, true, wantedIDs)

	if d.reason != ReasonUpgradesDisabled {
		t.Errorf("reason = %q, want %q (rule 3 must win over rule 5 per the mandated evaluation order)", d.reason, ReasonUpgradesDisabled)
	}
	if len(gotRequests) != 0 {
		t.Errorf("expected zero /moviefile requests, got %d", len(gotRequests))
	}
}

// TestEvaluateMovie_TagActiveAndTagsNil_SkipsWithReasonAndWarns is FIX 1
// (controller-mandated correction after the whole-branch review): "tags"
// entirely absent from the movie's JSON (m.Tags == nil) is untrusted
// input the same way "monitored" absent is (FIX 6) — containsTag(nil, id)
// returns false, so without this fix a movie whose tags we simply
// couldn't observe would silently pass rule 4 and could reach
// would-unmonitor, the one remaining untrusted-input route to that
// outcome. When the exclusion tag is actually active in this instance,
// that uncertainty must not be allowed to let the movie through: it is
// warned about (house warnIfFieldAbsent convention) and skipped with a
// dedicated reason.
func TestEvaluateMovie_TagActiveAndTagsNil_SkipsWithReasonAndWarns(t *testing.T) {
	var gotRequests []string
	srv := moviefileServer(t, http.StatusOK, `[]`, &gotRequests)
	defer srv.Close()
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	m := movieListElement{ID: intPtr(1), Title: strPtr("No Tags Movie"), HasFile: boolPtr(true), QualityProfileID: intPtr(1)} // Tags left nil
	d := evaluateMovie(context.Background(), logger, client, inst, m, evaluateTestProfiles, 99, true, map[int]bool{})

	if d.reason != ReasonTagsUnknown {
		t.Errorf("reason = %q, want %q", d.reason, ReasonTagsUnknown)
	}
	if d.wouldUnmonitor {
		t.Error("wouldUnmonitor = true, want false")
	}
	if len(gotRequests) != 0 {
		t.Errorf("expected zero /moviefile requests, got %d", len(gotRequests))
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected a warning about the missing tags field:\n%s", out)
	}
	if !strings.Contains(out, "field=tags") {
		t.Errorf("warning does not name the missing field tags:\n%s", out)
	}
}

// TestEvaluateMovie_TagActiveAndTagsPresentEmpty_PassesRuleFourNoWarn pins
// the other half of FIX 1's present-vs-absent distinction: tags present
// but an empty list is a legitimate, common value (a movie with no tags at
// all) and must pass rule 4 normally, with no warning.
func TestEvaluateMovie_TagActiveAndTagsPresentEmpty_PassesRuleFourNoWarn(t *testing.T) {
	var gotRequests []string
	srv := moviefileServer(t, http.StatusOK, `[{"id": 1, "customFormatScore": 200}]`, &gotRequests)
	defer srv.Close()
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	m := movieListElement{ID: intPtr(1), Title: strPtr("Empty Tags Movie"), HasFile: boolPtr(true), QualityProfileID: intPtr(1), Tags: &noTags,
		MovieFile: &movieFileElement{ID: intPtr(1)}}
	d := evaluateMovie(context.Background(), logger, client, inst, m, evaluateTestProfiles, 99, true, map[int]bool{})

	if !d.wouldUnmonitor {
		t.Errorf("wouldUnmonitor = false, reason=%q; want true (present-but-empty tags must pass rule 4 cleanly)", d.reason)
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("present-but-empty tags must not warn:\n%s", buf.String())
	}
}

// TestEvaluateMovie_TagNotActiveAndTagsNil_WarnsButProceeds pins the third
// case FIX 1 specifies: when no exclusion tag is defined in this instance
// at all (tagActive=false), rule 4 is vacuous regardless of tags, so nil
// tags is harmless to the decision and evaluation must proceed normally —
// but a warning still fires for gate visibility, since an absent field may
// still indicate the assumed field name is wrong.
func TestEvaluateMovie_TagNotActiveAndTagsNil_WarnsButProceeds(t *testing.T) {
	var gotRequests []string
	srv := moviefileServer(t, http.StatusOK, `[{"id": 1, "customFormatScore": 200}]`, &gotRequests)
	defer srv.Close()
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	m := movieListElement{ID: intPtr(1), Title: strPtr("No Tags Movie Inactive Tag"), HasFile: boolPtr(true), QualityProfileID: intPtr(1),
		MovieFile: &movieFileElement{ID: intPtr(1)}} // Tags left nil
	d := evaluateMovie(context.Background(), logger, client, inst, m, evaluateTestProfiles, 99, false, map[int]bool{})

	if !d.wouldUnmonitor {
		t.Errorf("wouldUnmonitor = false, reason=%q; want true (rule 4 is vacuous when the tag isn't active, so absent tags is harmless to the decision)", d.reason)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected a warning about the missing tags field even though the tag isn't active (gate visibility):\n%s", out)
	}
	if !strings.Contains(out, "field=tags") {
		t.Errorf("warning does not name the missing field tags:\n%s", out)
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

	m := movieListElement{ID: intPtr(1), Title: strPtr("Cutoff Not Met Movie"), HasFile: boolPtr(true), QualityProfileID: intPtr(1), Tags: &noTags}
	d := evaluateMovie(context.Background(), logger, client, inst, m, evaluateTestProfiles, 99, true, map[int]bool{1: true})

	if d.reason != ReasonQualityCutoffNotMet {
		t.Errorf("reason = %q, want %q", d.reason, ReasonQualityCutoffNotMet)
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

	m := movieListElement{ID: intPtr(1), Title: strPtr("Fetch Fails Movie"), HasFile: boolPtr(true), QualityProfileID: intPtr(1), Tags: &noTags}
	d := evaluateMovie(context.Background(), logger, client, inst, m, evaluateTestProfiles, 99, true, map[int]bool{})

	// Exact string mandated by the brief.
	if d.reason != ReasonCouldNotFetchCFScore {
		t.Errorf("reason = %q, want exact %q", d.reason, ReasonCouldNotFetchCFScore)
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

	m := movieListElement{ID: intPtr(1), Title: strPtr("Missing Score Movie"), HasFile: boolPtr(true), QualityProfileID: intPtr(1), Tags: &noTags,
		MovieFile: &movieFileElement{ID: intPtr(1)}}
	d := evaluateMovie(context.Background(), logger, client, inst, m, evaluateTestProfiles, 99, true, map[int]bool{})

	if d.reason != ReasonCouldNotFetchCFScore {
		t.Errorf("reason = %q, want %q", d.reason, ReasonCouldNotFetchCFScore)
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

	m := movieListElement{ID: intPtr(1), Title: strPtr("Below Threshold Movie"), HasFile: boolPtr(true), QualityProfileID: intPtr(1), Tags: &noTags,
		MovieFile: &movieFileElement{ID: intPtr(1)}}
	d := evaluateMovie(context.Background(), logger, client, inst, m, evaluateTestProfiles, 99, true, map[int]bool{})

	if d.wouldUnmonitor {
		t.Error("wouldUnmonitor = true, want false")
	}
	if d.reason != ReasonCFCutoffNotMet {
		t.Errorf("reason = %q, want %q", d.reason, ReasonCFCutoffNotMet)
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

	m := movieListElement{ID: intPtr(1), Title: strPtr("Passes All Movie"), HasFile: boolPtr(true), QualityProfileID: intPtr(1), Tags: &noTags,
		MovieFile: &movieFileElement{ID: intPtr(1)}}
	d := evaluateMovie(context.Background(), logger, client, inst, m, evaluateTestProfiles, 99, true, map[int]bool{})

	if !d.wouldUnmonitor {
		t.Fatalf("wouldUnmonitor = false, reason=%q; want true", d.reason)
	}
	// Exact string mandated by the brief's report-line example.
	if d.reason != ReasonCutoffMet {
		t.Errorf("reason = %q, want exact %q", d.reason, ReasonCutoffMet)
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

	m := movieListElement{ID: intPtr(1), Title: strPtr("Exactly At Threshold Movie"), HasFile: boolPtr(true), QualityProfileID: intPtr(1), Tags: &noTags,
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

// TestRunRadarrDecisionEngine_MonitoredFieldAbsent_WarnsButStillExcluded is
// FIX 6 (controller-mandated correction after the initial Phase 3 review):
// a movie whose "monitored" key is entirely absent from /movie (as opposed
// to present with monitored: false, a legitimate value) was previously
// silently dropped with no signal at all — inconsistent with the house
// warn-on-absent-key convention (warnIfFieldAbsent) used everywhere else a
// field this important is missing. The movie must still be excluded from
// the report (monitored can't be assumed true; excluding is the safe
// direction), but now with a warning naming the field, matching
// connectivity.go/radarr.go's existing pattern exactly.
func TestRunRadarrDecisionEngine_MonitoredFieldAbsent_WarnsButStillExcluded(t *testing.T) {
	var gotMoviefileRequests []string
	srv := decisionEngineTestServer(t, decisionEngineProfilesJSON, decisionEngineNoTagsJSON, staticMoviefileHandler(200), &gotMoviefileRequests)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	movies := []movieListElement{
		{ID: intPtr(1), Title: strPtr("No Monitored Field Movie"), HasFile: boolPtr(true), QualityProfileID: intPtr(1)}, // Monitored left nil
	}
	runRadarrDecisionEngine(context.Background(), logger, inst, movies, map[int]bool{}, "cutoffarr-exclude")

	out := buf.String()
	if strings.Contains(out, "msg=would-unmonitor") || strings.Contains(out, "msg=skip") {
		t.Errorf("a movie with monitored absent must still be excluded from the report entirely (safe direction):\n%s", out)
	}
	if !strings.Contains(out, "totalMonitored=0") {
		t.Errorf("expected the movie to not be counted:\n%s", out)
	}
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected a warning about the missing monitored field (house warnIfFieldAbsent convention), not silence:\n%s", out)
	}
	if !strings.Contains(out, `field="monitored"`) && !strings.Contains(out, "field=monitored") {
		t.Errorf("warning does not name the missing field monitored:\n%s", out)
	}
	if len(gotMoviefileRequests) != 0 {
		t.Errorf("expected zero /moviefile requests, got %d", len(gotMoviefileRequests))
	}
}

// TestRunRadarrDecisionEngine_MonitoredFalse_PresentValue_NoWarn pins the
// other half of the present-vs-absent distinction FIX 6 relies on:
// monitored: false is a legitimate, common real value (an intentionally
// unmonitored movie) and must never warn, only monitored being entirely
// absent should.
func TestRunRadarrDecisionEngine_MonitoredFalse_PresentValue_NoWarn(t *testing.T) {
	var gotMoviefileRequests []string
	srv := decisionEngineTestServer(t, decisionEngineProfilesJSON, decisionEngineNoTagsJSON, staticMoviefileHandler(200), &gotMoviefileRequests)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	movies := []movieListElement{
		{ID: intPtr(1), Title: strPtr("Explicitly Unmonitored Movie"), Monitored: boolPtr(false), HasFile: boolPtr(true), QualityProfileID: intPtr(1)},
	}
	runRadarrDecisionEngine(context.Background(), logger, inst, movies, map[int]bool{}, "cutoffarr-exclude")

	out := buf.String()
	if strings.Contains(out, "level=WARN") {
		t.Errorf("monitored: false is a legitimate present value and must not warn:\n%s", out)
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
//
// FIX 1 (controller-mandated correction, applied after the initial Phase 3
// review): runCrossCheck's return signature changed from (passed bool,
// checked int) to (status string, verified int, unverifiable int). The old
// shape let an item with qualityCutoffNotMet == nil count toward "checked"
// while being excluded from the agree/disagree comparison, so the summary
// could read e.g. "passed (20 items)" when zero items were actually
// verified — dishonest on any future Radarr response shape where the field
// is absent (confirmed live on Radarr 6.3.0, 2026-08-07, that the field IS
// present today: 3 samples read false, 146 true library-wide — this fix
// guards against a future/different version, not today's observed
// behavior). status is one of crossCheckStatusPassed/Failed/Inconclusive;
// "inconclusive" fires when every sampled item was unverifiable, so an
// all-unverifiable run can never silently read as a pass.

func agreeTrue() *bool  { return boolPtr(true) }
func agreeFalse() *bool { return boolPtr(false) }

func TestRunCrossCheck_ZeroCandidates_PassesWithZeroItems(t *testing.T) {
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr"}

	status, verified, unverifiable := runCrossCheck(logger, inst, nil, map[int]bool{})
	if status != crossCheckStatusPassed || verified != 0 || unverifiable != 0 {
		t.Errorf("runCrossCheck = (%q, %d, %d), want (%q, 0, 0) for zero candidates", status, verified, unverifiable, crossCheckStatusPassed)
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
		{id: 2, title: "Skip In Cutoff Movie", wouldUnmonitor: false, hasFile: true, reason: ReasonQualityCutoffNotMet, qualityCutoffNotMet: agreeTrue()},
	}
	wantedIDs := map[int]bool{2: true} // id 1 not in set (agrees with false); id 2 in set (agrees with true)

	status, verified, unverifiable := runCrossCheck(logger, inst, decisions, wantedIDs)
	if status != crossCheckStatusPassed {
		t.Errorf("status = %q, want %q:\n%s", status, crossCheckStatusPassed, buf.String())
	}
	if verified != 2 || unverifiable != 0 {
		t.Errorf("verified/unverifiable = %d/%d, want 2/0", verified, unverifiable)
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
		{id: 1, title: "Disagreeing Movie", wouldUnmonitor: false, hasFile: true, reason: ReasonQualityCutoffNotMet, qualityCutoffNotMet: agreeFalse()},
	}
	wantedIDs := map[int]bool{1: true}

	status, verified, unverifiable := runCrossCheck(logger, inst, decisions, wantedIDs)
	if status != crossCheckStatusFailed {
		t.Errorf("status = %q, want %q for a disagreement", status, crossCheckStatusFailed)
	}
	// The disagreeing item was compared (it has a non-nil qualityCutoffNotMet),
	// so it counts as verified, not unverifiable.
	if verified != 1 || unverifiable != 0 {
		t.Errorf("verified/unverifiable = %d/%d, want 1/0", verified, unverifiable)
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
// 15 skip candidates (all agreeing) must be capped at 10 + 10 = 20 verified
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

	status, verified, unverifiable := runCrossCheck(logger, inst, decisions, wantedIDs)
	if status != crossCheckStatusPassed {
		t.Errorf("status = %q, want %q (all candidates agree)", status, crossCheckStatusPassed)
	}
	if verified != 20 || unverifiable != 0 {
		t.Errorf("verified/unverifiable = %d/%d, want 20/0 (10 would-unmonitor + 10 skip)", verified, unverifiable)
	}
}

// TestRunCrossCheck_AllUnverifiable_ReturnsInconclusiveAndWarns pins FIX 1's
// core honesty requirement: if movieFile.qualityCutoffNotMet is entirely
// absent from the /movie data for every sampled candidate (e.g. an
// older/different Radarr response shape than the one confirmed live on
// 6.3.0), the result must NOT read as a pass — status is "inconclusive",
// verified=0, and a warning fires, distinct from a genuine "passed" outcome
// which requires at least one item to have actually been compared.
func TestRunCrossCheck_AllUnverifiable_ReturnsInconclusiveAndWarns(t *testing.T) {
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr"}

	decisions := []movieDecision{
		{id: 1, title: "Missing Field Movie", wouldUnmonitor: true, hasFile: true, qualityCutoffNotMet: nil, cfScore: intPtr(200), cfThreshold: 100},
	}
	status, verified, unverifiable := runCrossCheck(logger, inst, decisions, map[int]bool{})
	if status != crossCheckStatusInconclusive {
		t.Errorf("status = %q, want %q: an all-unverifiable sample must not read as passed", status, crossCheckStatusInconclusive)
	}
	if verified != 0 || unverifiable != 1 {
		t.Errorf("verified/unverifiable = %d/%d, want 0/1", verified, unverifiable)
	}
	out := buf.String()
	if strings.Contains(out, "level=ERROR") {
		t.Errorf("an absent field must warn, not error:\n%s", out)
	}
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "Missing Field Movie") {
		t.Errorf("expected a per-item warning naming the movie with the absent field:\n%s", out)
	}
}

// TestRunCrossCheck_MixedVerifiedAndUnverifiable_PassesWithBothCountsSeparate
// is FIX 1's other mandated case: a mix of verified-agreeing and
// unverifiable candidates must report both counts honestly and still pass
// overall (at least one item was actually verified and none disagreed).
func TestRunCrossCheck_MixedVerifiedAndUnverifiable_PassesWithBothCountsSeparate(t *testing.T) {
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr"}

	decisions := []movieDecision{
		{id: 1, title: "Verified Movie A", wouldUnmonitor: true, hasFile: true, qualityCutoffNotMet: agreeFalse()},
		{id: 2, title: "Verified Movie B", wouldUnmonitor: true, hasFile: true, qualityCutoffNotMet: agreeFalse()},
		{id: 3, title: "Unverifiable Movie", wouldUnmonitor: true, hasFile: true, qualityCutoffNotMet: nil},
	}
	status, verified, unverifiable := runCrossCheck(logger, inst, decisions, map[int]bool{})
	if status != crossCheckStatusPassed {
		t.Errorf("status = %q, want %q:\n%s", status, crossCheckStatusPassed, buf.String())
	}
	if verified != 2 || unverifiable != 1 {
		t.Errorf("verified/unverifiable = %d/%d, want 2/1", verified, unverifiable)
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
		{id: 1, title: "No File Movie", wouldUnmonitor: false, hasFile: false, reason: ReasonNoFile},
	}
	_, verified, unverifiable := runCrossCheck(logger, inst, decisions, map[int]bool{})
	if verified != 0 || unverifiable != 0 {
		t.Errorf("verified/unverifiable = %d/%d, want 0/0 (hasFile=false skip must not be a cross-check candidate)", verified, unverifiable)
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
	if !strings.Contains(out, `crossCheck="passed (1 verified, 0 unverifiable)"`) {
		t.Errorf("expected the summary to state the cross-check passed with verified/unverifiable counts:\n%s", out)
	}
}

// TestRunRadarrDecisionEngine_CrossCheckAllUnverifiable_SummaryStatesInconclusive
// is FIX 1's end-to-end pin: when the /movie data for every sampled
// candidate is missing movieFile.qualityCutoffNotMet, the summary must read
// "inconclusive", never "passed" — through the real orchestrator, not just
// the runCrossCheck unit.
func TestRunRadarrDecisionEngine_CrossCheckAllUnverifiable_SummaryStatesInconclusive(t *testing.T) {
	var gotMoviefileRequests []string
	moviefileHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id": 1, "customFormatScore": 200}]`))
	}
	srv := decisionEngineTestServer(t, decisionEngineProfilesJSON, decisionEngineNoTagsJSON, moviefileHandler, &gotMoviefileRequests)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	// QualityCutoffNotMet left nil: movieFile present, but this field absent.
	movies := []movieListElement{
		{ID: intPtr(1), Title: strPtr("Would Unmonitor Movie"), Monitored: boolPtr(true), HasFile: boolPtr(true), QualityProfileID: intPtr(1),
			MovieFile: &movieFileElement{ID: intPtr(1)}},
	}
	runRadarrDecisionEngine(context.Background(), logger, inst, movies, map[int]bool{}, "cutoffarr-exclude")

	out := buf.String()
	if strings.Contains(out, "level=ERROR") {
		t.Errorf("an unverifiable-only cross-check must not error:\n%s", out)
	}
	if !strings.Contains(out, `crossCheck="inconclusive (0 verified, 1 unverifiable)"`) {
		t.Errorf("expected the summary to state the cross-check inconclusive, not passed:\n%s", out)
	}
	if strings.Contains(out, `crossCheck="passed`) {
		t.Errorf("an all-unverifiable cross-check must never render as passed:\n%s", out)
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
