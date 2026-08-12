package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
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

// TestResolveExclusionTagID_MalformedRecordAndNoMatch_SkipsInstance pins the
// REVIEW FIX (Phase 5 round 2) correction: an individual malformed tag
// record (missing id or label) is instance-fatal, exactly like the whole
// /tag request failing (§2.6), NOT merely excluded-and-skip-past — but only
// when the exclusion tag was not found among the records that ARE readable.
// A record with a nil label cannot be compared against the configured label,
// so when nothing else matched there is no way to tell whether the unreadable
// record is in fact the exclusion tag's own definition; falling through to
// "not defined" would risk asserting a false statement and silently disabling
// rule 4 for the whole instance. This intentionally diverges from
// fetchWantedCutoff's per-record skip for movies, where skipping one bad movie
// record cannot corrupt the evaluation of any other movie — the tag list has
// no such independence property when the very record in question might be the
// one that matters.
func TestResolveExclusionTagID_MalformedRecordAndNoMatch_SkipsInstance(t *testing.T) {
	tagsJSON := `[{"label": "no-id"}, {"id": 9, "label": "some-other-tag"}]` // first record missing id, and nothing matches
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
	if ok {
		t.Fatalf("resolveExclusionTagID returned ok=true, want false: with the label unfound and a record unreadable, its status is genuinely unknowable:\n%s", buf.String())
	}
	if found {
		t.Error("found = true, want false when the instance was skipped")
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "radarr-main") {
		t.Errorf("expected a warning naming the instance:\n%s", out)
	}
	if strings.Contains(out, "exclusion tag not defined in this instance") {
		t.Errorf("must not claim the tag is undefined — its status could not be verified, which is a different, stronger fact:\n%s", out)
	}
}

// TestResolveExclusionTagID_MalformedRecordButLabelMatches_ResolvesRegardlessOfOrder
// is the REVIEW FIX (Phase 5 final round) on top of that rule: fail-closed was
// right, failing EARLY was not. The round-3 implementation returned
// instance-fatal at the first malformed record, so identical tag data resolved
// or bricked the instance purely on array order — a malformed record listed
// before the exclusion tag's own record skipped the instance on every cycle,
// forever, while the same two records in the opposite order resolved fine and
// ran. Radarr tag labels are unique, so a positive label match on a
// well-formed record fully determines the id no matter what any other record
// says; the unknowable case is only "the label was not found AND something was
// unreadable". Both orderings are asserted here precisely because the defect
// was invisible to a test that only ever fed one of them.
func TestResolveExclusionTagID_MalformedRecordButLabelMatches_ResolvesRegardlessOfOrder(t *testing.T) {
	orderings := []struct{ name, tagsJSON string }{
		{"malformed record first", `[{"label": "no-id"}, {"id": 9, "label": "cutoffarr-exclude"}]`},
		{"malformed record last", `[{"id": 9, "label": "cutoffarr-exclude"}, {"label": "no-id"}]`},
		{"malformed records on both sides", `[{"id": 3}, {"id": 9, "label": "cutoffarr-exclude"}, {"label": "no-id"}]`},
	}
	for _, tc := range orderings {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/api/v3/tag", func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(tc.tagsJSON))
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			logger, buf := newDecisionTestLogger(slog.LevelInfo)
			inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}
			client := NewAPIClient(inst.URL, inst.APIKey)

			id, found, ok := resolveExclusionTagID(context.Background(), logger, client, inst, "cutoffarr-exclude")
			if !ok {
				t.Fatalf("resolveExclusionTagID returned ok=false: the label matched a well-formed record, so the answer is known regardless of unreadable neighbours:\n%s", buf.String())
			}
			if !found || id != 9 {
				t.Errorf("id/found = %d/%v, want 9/true", id, found)
			}
			// The unreadable record is still worth saying out loud: it may be
			// the first sign of a Radarr version whose /tag shape this code no
			// longer understands.
			out := buf.String()
			if !strings.Contains(out, "level=WARN") {
				t.Errorf("expected a warning about the record that could not be read:\n%s", out)
			}
			if strings.Contains(out, "skipping instance") {
				t.Errorf("the instance must not be skipped when the exclusion tag was positively identified:\n%s", out)
			}
		})
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
//
// It also serves GET /api/v3/movie/{id} (Phase 4). That is not decoration:
// the write pass re-fetches each would-unmonitor movie before the dry-run
// gate, so even a dry-run run touches this endpoint, and a fake that 404s
// it would make every passing-cross-check test fail on a write error that
// says nothing about what the test is actually checking. Tests that assert
// on write behaviour itself use radarrFake (writer_test.go), which records
// requests; this handler exists only so the read-only tests stay read-only
// in what they need to know about.
//
// GET, and ONLY GET. This fake records nothing, so a write that reached it
// would leave no trace: every one of its callers passes dryRun=true today,
// but a future engine test written against it with dryRun=false would issue
// real PUTs, get a cheerful 200 back, and pass — the precise failure this
// phase exists to make impossible. Answering 405 means such a test fails
// loudly and its author reaches for radarrFake, which is the fake that can
// actually see writes.
func decisionEngineTestServer(t *testing.T, profilesJSON, tagsJSON string, moviefileHandler http.HandlerFunc, gotMoviefileRequests *[]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/movie/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/v3/movie/")
		fmt.Fprintf(w, `{"id": %s, "monitored": true}`, id)
	})
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

// TestDecisionEngineTestServer_RefusesWrites is a test OF the shared
// read-only fake, and it earns its place for the same reason
// TestRadarrFake_RecordsRequestsToPathsItDoesNotStub does: this fake records
// nothing, so any write that it answers with a 200 is a write no assertion
// in this file can see. Every current caller passes dryRun=true, which is
// what makes the hole invisible — and temporary. The guard has to be pinned,
// or removing it silently restores a fake that green-lights real PUTs.
func TestDecisionEngineTestServer_RefusesWrites(t *testing.T) {
	var gotMoviefileRequests []string
	srv := decisionEngineTestServer(t, decisionEngineProfilesJSON, decisionEngineNoTagsJSON, staticMoviefileHandler(200), &gotMoviefileRequests)
	defer srv.Close()

	for _, method := range []string{http.MethodPut, http.MethodPost, http.MethodDelete, http.MethodPatch} {
		req, err := http.NewRequest(method, srv.URL+"/api/v3/movie/1", strings.NewReader(`{"id": 1, "monitored": false}`))
		if err != nil {
			t.Fatalf("building %s request: %v", method, err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s /api/v3/movie/1: %v", method, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/v3/movie/1 answered %d; a fake that records nothing must never accept a write, or a dryRun=false test written against it would pass while writing",
				method, resp.StatusCode)
		}
	}

	// The read it does exist for must still work.
	resp, err := http.Get(srv.URL + "/api/v3/movie/1")
	if err != nil {
		t.Fatalf("GET /api/v3/movie/1: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /api/v3/movie/1 answered %d, want 200: the write pass's pre-write fetch runs in dry-run too", resp.StatusCode)
	}
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
	runRadarrDecisionEngine(context.Background(), logger, inst, movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

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
	// Phase 5: rule 1's exclusions must still be countable, or a write-mode
	// summary reading wouldUnmonitor=0 unmonitored=0 (a no-op cycle) is
	// indistinguishable from a run that fetched nothing at all.
	if !strings.Contains(out, "alreadyUnmonitored=1") {
		t.Errorf("expected alreadyUnmonitored=1 in the summary:\n%s", out)
	}
}

// TestRunRadarrDecisionEngine_UnmonitoredMovies_DebugLineNotInfo is the
// Phase 5 no-op contract's visibility half (plan: "already unmonitored"
// items are skipped silently at info, logged at debug"). At info level rule
// 1's exclusions remain completely invisible per-movie (only the summary
// counter says how many); at debug a line names each one so a human running
// with -v can see exactly which movies rule 1 excluded and why.
func TestRunRadarrDecisionEngine_UnmonitoredMovies_DebugLineNotInfo(t *testing.T) {
	var gotMoviefileRequests []string
	srv := decisionEngineTestServer(t, decisionEngineProfilesJSON, decisionEngineNoTagsJSON, staticMoviefileHandler(200), &gotMoviefileRequests)
	defer srv.Close()
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}
	movies := []movieListElement{
		{ID: intPtr(9), Title: strPtr("Quietly Unmonitored"), Monitored: boolPtr(false), HasFile: boolPtr(true), QualityProfileID: intPtr(1)},
	}

	t.Run("invisible at info", func(t *testing.T) {
		logger, buf := newDecisionTestLogger(slog.LevelInfo)
		runRadarrDecisionEngine(context.Background(), logger, inst, movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)
		out := buf.String()
		if strings.Contains(out, `msg="already unmonitored"`) {
			t.Errorf("the already-unmonitored line must stay invisible at info level:\n%s", out)
		}
	})

	t.Run("visible at debug with id, title, instance", func(t *testing.T) {
		logger, buf := newDecisionTestLogger(slog.LevelDebug)
		runRadarrDecisionEngine(context.Background(), logger, inst, movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)
		out := buf.String()
		if !strings.Contains(out, `msg="already unmonitored"`) {
			t.Errorf("expected msg=\"already unmonitored\" at debug level:\n%s", out)
		}
		for _, want := range []string{"id=9", `title="Quietly Unmonitored"`, "instance=radarr-main"} {
			if !strings.Contains(out, want) {
				t.Errorf("expected the debug line to contain %q:\n%s", want, out)
			}
		}
	})
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
	runRadarrDecisionEngine(context.Background(), logger, inst, movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

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
	runRadarrDecisionEngine(context.Background(), logger, inst, movies, wantedIDs, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

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
	runRadarrDecisionEngine(context.Background(), logger, inst, movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

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
	runRadarrDecisionEngine(context.Background(), logger, inst, movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

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
	runRadarrDecisionEngine(context.Background(), logger, inst, movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

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
	// REVIEW FIX (Phase 6, carried forward from the phase-5 branch review):
	// writesRefused=0 was never pinned by name in any existing summary
	// assertion — every always-present-as-0 counter needs at least one test
	// proving it actually appears as a token (not merely that the
	// reconciliation identity holds arithmetically), or a bug that dropped
	// the attr entirely from the summary line could go unnoticed.
	if !strings.Contains(out, "writesRefused=0") {
		t.Errorf("expected writesRefused=0 in the summary (nothing here carries the exclusion tag):\n%s", out)
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
	runRadarrDecisionEngine(context.Background(), logger, inst, movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

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
	runRadarrDecisionEngine(context.Background(), logger, inst, movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

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
	runRadarrDecisionEngine(context.Background(), logger, inst, movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

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
	// REVIEW FIX (Phase 5 round 4): monitored ABSENT is untrusted input (the
	// warning above), not evidence the movie is already unmonitored — it must
	// never be counted by, or logged through, the alreadyUnmonitored
	// no-op-visibility machinery that legitimately covers monitored: false.
	// The `m.Monitored != nil` half of decision.go's guard
	// (`m.Monitored != nil && !*m.Monitored && ...`) is what keeps the two
	// apart; on its own it reads as dead redundancy (the outer branch is
	// already `m.Monitored == nil || !*m.Monitored`), which is exactly what a
	// future "simplification" would delete. Pinning alreadyUnmonitored=0 here
	// fails the whole suite the moment that guard stops doing its job.
	if !strings.Contains(out, "alreadyUnmonitored=0") {
		t.Errorf("a monitored-absent movie must not be counted as already unmonitored — that would report a fact about its state the code explicitly refused to trust:\n%s", out)
	}
}

// TestRunRadarrDecisionEngine_MonitoredFieldAbsent_NoAlreadyUnmonitoredDebugLine
// is the debug-level half of the same pin: even with the debug line visible,
// a monitored-absent movie must never be reported through it. Without this,
// a library whose /movie payloads lack "monitored" entirely (an older Radarr
// version, a proxy stripping fields) would converge to a report that reads
// totalMonitored=0 wouldUnmonitor=0 alreadyUnmonitored=N plus N debug lines
// each claiming a movie is "already unmonitored" — state the code explicitly
// refused to trust, presented as an observed fact.
func TestRunRadarrDecisionEngine_MonitoredFieldAbsent_NoAlreadyUnmonitoredDebugLine(t *testing.T) {
	var gotMoviefileRequests []string
	srv := decisionEngineTestServer(t, decisionEngineProfilesJSON, decisionEngineNoTagsJSON, staticMoviefileHandler(200), &gotMoviefileRequests)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelDebug)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}

	movies := []movieListElement{
		{ID: intPtr(1), Title: strPtr("No Monitored Field Movie"), HasFile: boolPtr(true), QualityProfileID: intPtr(1)}, // Monitored left nil
	}
	runRadarrDecisionEngine(context.Background(), logger, inst, movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

	out := buf.String()
	if strings.Contains(out, `msg="already unmonitored"`) {
		t.Errorf("a monitored-absent movie must never be reported through the already-unmonitored debug line; its state was never observed:\n%s", out)
	}
	if !strings.Contains(out, "alreadyUnmonitored=0") {
		t.Errorf("expected alreadyUnmonitored=0:\n%s", out)
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
	runRadarrDecisionEngine(context.Background(), logger, inst, movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

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

	cc := runCrossCheck(logger, inst, nil, map[int]bool{})
	status, verified, unverifiable := cc.status, cc.verified, cc.unverifiable
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

	cc := runCrossCheck(logger, inst, decisions, wantedIDs)
	status, verified, unverifiable := cc.status, cc.verified, cc.unverifiable
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

	cc := runCrossCheck(logger, inst, decisions, wantedIDs)
	status, verified, unverifiable := cc.status, cc.verified, cc.unverifiable
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

	cc := runCrossCheck(logger, inst, decisions, wantedIDs)
	status, verified, unverifiable := cc.status, cc.verified, cc.unverifiable
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
	cc := runCrossCheck(logger, inst, decisions, map[int]bool{})
	status, verified, unverifiable := cc.status, cc.verified, cc.unverifiable
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
	cc := runCrossCheck(logger, inst, decisions, map[int]bool{})
	status, verified, unverifiable := cc.status, cc.verified, cc.unverifiable
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
	cc := runCrossCheck(logger, inst, decisions, map[int]bool{})
	verified, unverifiable := cc.verified, cc.unverifiable
	if verified != 0 || unverifiable != 0 {
		t.Errorf("verified/unverifiable = %d/%d, want 0/0 (hasFile=false skip must not be a cross-check candidate)", verified, unverifiable)
	}
}

// --- renderCrossCheckSummary -----------------------------------------------
//
// FIX 2 (Minor, controller-mandated correction after the whole-branch
// review): the crossCheck summary rendering previously fell through to a
// bare `default:` case that rendered the "passed" format — meaning any
// future new/unrecognized status value (a typo in a new status constant, a
// forgotten case after adding a fourth state, etc.) would silently render
// as a pass instead of failing loudly. renderCrossCheckSummary now makes
// crossCheckStatusPassed its own explicit case, and default renders the
// raw, unrecognized status string plus counts — something that can never
// be mistaken for "passed (...)" — so a truly unrecognized status is loud
// and debuggable rather than falsely reassuring.

func TestRenderCrossCheckSummary_Passed(t *testing.T) {
	got := renderCrossCheckSummary(crossCheckStatusPassed, 5, 1)
	if got != "passed (5 verified, 1 unverifiable)" {
		t.Errorf("renderCrossCheckSummary(passed) = %q, want %q", got, "passed (5 verified, 1 unverifiable)")
	}
}

func TestRenderCrossCheckSummary_Failed(t *testing.T) {
	got := renderCrossCheckSummary(crossCheckStatusFailed, 5, 1)
	if got != "FAILED" {
		t.Errorf("renderCrossCheckSummary(failed) = %q, want %q", got, "FAILED")
	}
}

func TestRenderCrossCheckSummary_Inconclusive(t *testing.T) {
	got := renderCrossCheckSummary(crossCheckStatusInconclusive, 0, 3)
	if got != "inconclusive (0 verified, 3 unverifiable)" {
		t.Errorf("renderCrossCheckSummary(inconclusive) = %q, want %q", got, "inconclusive (0 verified, 3 unverifiable)")
	}
}

// TestRenderCrossCheckSummary_UnrecognizedStatus_NeverReadsAsPassed is
// FIX 2's core pin: feed an unrecognized status through the renderer and
// assert the output does not contain "passed" — the exact hazard the old
// bare `default:` case created.
func TestRenderCrossCheckSummary_UnrecognizedStatus_NeverReadsAsPassed(t *testing.T) {
	got := renderCrossCheckSummary("some-future-status-nobody-added-a-case-for", 5, 1)
	if strings.Contains(got, "passed") {
		t.Errorf("renderCrossCheckSummary(unrecognized) = %q, must never contain \"passed\"", got)
	}
	if !strings.Contains(got, "some-future-status-nobody-added-a-case-for") {
		t.Errorf("renderCrossCheckSummary(unrecognized) = %q, expected it to surface the raw unrecognized status", got)
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
	runRadarrDecisionEngine(context.Background(), logger, inst, movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

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
	runRadarrDecisionEngine(context.Background(), logger, inst, movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

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
	runRadarrDecisionEngine(context.Background(), logger, inst, movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

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

// --- the write pass (Phase 4) ---------------------------------------------
//
// Writes are a THIRD pass, deliberately separated from evaluation: evaluate
// every movie, run the cross-check, and only then — and only if the
// cross-check explicitly PASSED — act on the retained decisions. The gate
// is the wouldUnmonitor bool, never the reason text.

// wouldUnmonitorMovie builds a library element that passes every rule of
// the STRICT decision rule (monitored, hasFile, known profile with upgrades
// allowed, no tags, absent from the wanted set, CF score above cutoff) and
// whose movieFile.qualityCutoffNotMet agrees with that, so the cross-check
// verifies it and passes.
func wouldUnmonitorMovie(id int, title string) movieListElement {
	return movieListElement{
		ID: intPtr(id), Title: strPtr(title), Monitored: boolPtr(true), HasFile: boolPtr(true),
		QualityProfileID: intPtr(1), Tags: &noTags,
		MovieFile: &movieFileElement{ID: intPtr(1), QualityCutoffNotMet: boolPtr(false)},
	}
}

// skippedMovie builds a library element that fails rule 2 (no file), so it
// is reported as a skip and must never be written.
func skippedMovie(id int, title string) movieListElement {
	return movieListElement{
		ID: intPtr(id), Title: strPtr(title), Monitored: boolPtr(true), HasFile: boolPtr(false),
		QualityProfileID: intPtr(1), Tags: &noTags,
	}
}

// TestRunRadarrDecisionEngine_DryRun_WouldUnmonitorItemsProduceZeroWrites is
// the most important test in this project: a run where the decision engine
// really does fire (a would-unmonitor item exists, the cross-check passes,
// so the write pass is entered and walks the entire write path) must still
// send ZERO write requests of any method to any endpoint, while the
// would-unmonitor report line is unchanged.
func TestRunRadarrDecisionEngine_DryRun_WouldUnmonitorItemsProduceZeroWrites(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{1: monitoredMovieDetail(1, "Would Unmonitor Movie")})

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{wouldUnmonitorMovie(1, "Would Unmonitor Movie")}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("dry-run made %d write request(s), want ZERO: %+v", len(writes), writes)
	}
	out := buf.String()
	if !strings.Contains(out, "msg=would-unmonitor") || !strings.Contains(out, `title="Would Unmonitor Movie"`) {
		t.Errorf("the would-unmonitor report line must still appear in dry-run:\n%s", out)
	}
	if strings.Contains(out, "msg=unmonitor ") {
		t.Errorf("dry-run must never log a completed unmonitor:\n%s", out)
	}
	if !strings.Contains(out, "unmonitored=0") {
		t.Errorf("expected unmonitored=0 in the dry-run summary:\n%s", out)
	}
	if !strings.Contains(out, "writeErrors=0") {
		t.Errorf("expected writeErrors=0 in the dry-run summary:\n%s", out)
	}

	// Every assertion above also holds for a run whose write pass was never
	// entered — a blocked gate produces zero writes, an unchanged report, and
	// zero counters too. These three are what distinguish "dry-run walked the
	// write path and stopped at the gate" from "nothing ever tried to write".
	if !strings.Contains(out, `crossCheck="passed`) {
		t.Errorf("the cross-check must PASS here, or the write pass was never entered and this test proves nothing:\n%s", out)
	}
	if strings.Contains(out, "writes withheld") {
		t.Errorf("the write gate blocked this run, so zero writes says nothing about dry-run:\n%s", out)
	}
	sawPreWriteGet := false
	for _, r := range fake.all() {
		if r.method == http.MethodGet && r.path == "/api/v3/movie/1" {
			sawPreWriteGet = true
			break
		}
	}
	if !sawPreWriteGet {
		t.Errorf("no pre-write GET /api/v3/movie/1: the write path was not walked up to the dry-run gate:\n%+v", fake.all())
	}
}

// TestRunRadarrDecisionEngine_WriteMode_UnmonitorsOnlyWouldUnmonitorItems is
// the write-mode counterpart: exactly one PUT, for the one item whose
// wouldUnmonitor bool is set, with the plan §3 "unmonitor" vocabulary and
// every mandated attr on the log line.
func TestRunRadarrDecisionEngine_WriteMode_UnmonitorsOnlyWouldUnmonitorItems(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{
		1: monitoredMovieDetail(1, "Would Unmonitor Movie"),
		2: monitoredMovieDetail(2, "No File Movie"),
	})

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{
		wouldUnmonitorMovie(1, "Would Unmonitor Movie"),
		skippedMovie(2, "No File Movie"),
	}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), false)

	puts := fake.puts()
	if len(puts) != 1 {
		t.Fatalf("expected exactly 1 PUT, got %d: %+v", len(puts), puts)
	}
	if puts[0].path != "/api/v3/movie/1" {
		t.Errorf("PUT went to %q, want /api/v3/movie/1 (the skipped movie must never be written)", puts[0].path)
	}

	out := buf.String()
	for _, want := range []string{"msg=unmonitor ", "id=1", `title="Would Unmonitor Movie"`, `reason="cutoff met"`, "profile=HD-1080p", "instance=radarr-main"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected the unmonitor log line to contain %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "unmonitored=1") {
		t.Errorf("expected unmonitored=1 in the summary:\n%s", out)
	}
	if !strings.Contains(out, "writeErrors=0") {
		t.Errorf("expected writeErrors=0 in the summary:\n%s", out)
	}
}

// TestRunRadarrDecisionEngine_CrossCheckFailed_WritePassBlocked pins the
// binding architecture mandate: a FAILED cross-check blocks the write pass
// entirely, with a warning that says writes were withheld and why. The
// report itself is unaffected — the human gate still gets the full report.
func TestRunRadarrDecisionEngine_CrossCheckFailed_WritePassBlocked(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{1: monitoredMovieDetail(1, "Disagreeing Movie")})

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	// wouldUnmonitor (absent from the wanted set) but the movie's own
	// qualityCutoffNotMet says true: a cross-check disagreement.
	movies := []movieListElement{{
		ID: intPtr(1), Title: strPtr("Disagreeing Movie"), Monitored: boolPtr(true), HasFile: boolPtr(true),
		QualityProfileID: intPtr(1), Tags: &noTags,
		MovieFile: &movieFileElement{ID: intPtr(1), QualityCutoffNotMet: boolPtr(true)},
	}}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), false)

	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("a FAILED cross-check must block every write, got %d: %+v", len(writes), writes)
	}
	out := buf.String()
	if !strings.Contains(out, "crossCheck=FAILED") {
		t.Errorf("expected the summary to state the cross-check FAILED:\n%s", out)
	}
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "withheld") {
		t.Errorf("expected a warning explaining that writes were withheld:\n%s", out)
	}
	// The count makes the warning unambiguous: "the gate blocked 1 write"
	// reads very differently from "the gate blocked, but there was nothing
	// to write anyway", and the human deciding whether to investigate needs
	// to know which one happened.
	if !strings.Contains(out, "withheldWrites=1") {
		t.Errorf("expected the withheld warning to state how many writes it blocked:\n%s", out)
	}
	if !strings.Contains(out, "unmonitored=0") {
		t.Errorf("expected unmonitored=0:\n%s", out)
	}
	if !strings.Contains(out, "msg=would-unmonitor") {
		t.Errorf("blocking writes must not suppress the report the human gate reads:\n%s", out)
	}
}

// TestRunRadarrDecisionEngine_CrossCheckInconclusive_WritePassBlocked pins
// that "inconclusive" blocks writes exactly as hard as "failed" does: a
// cross-check that verified nothing is not permission to write.
func TestRunRadarrDecisionEngine_CrossCheckInconclusive_WritePassBlocked(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{1: monitoredMovieDetail(1, "Unverifiable Movie")})

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	// movieFile present but qualityCutoffNotMet absent: nothing to compare,
	// so the cross-check is inconclusive.
	movies := []movieListElement{{
		ID: intPtr(1), Title: strPtr("Unverifiable Movie"), Monitored: boolPtr(true), HasFile: boolPtr(true),
		QualityProfileID: intPtr(1), Tags: &noTags,
		MovieFile: &movieFileElement{ID: intPtr(1)},
	}}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), false)

	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("an inconclusive cross-check must block every write, got %d: %+v", len(writes), writes)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "withheld") {
		t.Errorf("expected a warning explaining that writes were withheld:\n%s", out)
	}
	if !strings.Contains(out, "unmonitored=0") {
		t.Errorf("expected unmonitored=0:\n%s", out)
	}
}

// TestRunWritePass_UnrecognizedCrossCheckStatus_WritePassBlocked pins the
// half of binding mandate 1 that the "failed" and "inconclusive" tests
// cannot reach: "(and any other status)". Those two tests are equally green
// against the correct gate (crossCheckStatus != crossCheckStatusPassed) and
// against a narrowed one that enumerates the two known blocking values, so
// on their own they permit a future fourth status to fall THROUGH the gate
// and write. That is not a hypothetical defect class in this repo — it is
// the same one phase-3 FIX 2 had to correct in renderCrossCheckSummary,
// where a bare `default:` rendered an unrecognized status as "passed" (see
// the comment above that function).
//
// The gate is therefore stated positively: only the exact string
// crossCheckStatusPassed opens it, and every other value — a typo in a new
// constant, a status a later phase adds and forgets to wire in here, the
// zero value of an unset field — blocks the pass and says so. This calls
// runWritePass directly because that is the only way to feed it a status
// runCrossCheck cannot currently produce.
func TestRunWritePass_UnrecognizedCrossCheckStatus_WritePassBlocked(t *testing.T) {
	statuses := []struct{ name, status string }{
		{"future fourth state", "partially-verified"},
		{"typo or case drift in a new constant", "Passed"},
		{"zero value of an unset status field", ""},
	}
	for _, tc := range statuses {
		t.Run(tc.name, func(t *testing.T) {
			// The detail fixture matters: movie 1 is fully serveable, so a
			// gate that let this status through would complete a real PUT
			// and be recorded, rather than failing the fresh GET and
			// passing this test for the wrong reason.
			fake := newRadarrFake(t, "", map[int]string{1: monitoredMovieDetail(1, "Ungated Movie")})
			client := NewAPIClient(fake.instance().URL, fake.instance().APIKey)
			logger, buf := newDecisionTestLogger(slog.LevelInfo)

			decisions := []movieDecision{{id: 1, title: "Ungated Movie", wouldUnmonitor: true, reason: ReasonCutoffMet, profileName: "HD-1080p"}}
			// Everything except the status is what a healthy cross-check
			// looks like — the would-unmonitor item was sampled and verified
			// — so the status really is the only thing keeping this pass
			// shut, and a gate that stopped consulting it would write.
			cc := crossCheckResult{status: tc.status, verified: 1, writeVerified: 1}
			unmonitored, writeErrors, echoUnverified, writesRefused, _ := runWritePass(context.Background(), logger, client, fake.instance(), decisions, cc, 0, false, false)

			if writes := fake.writes(); len(writes) != 0 {
				t.Fatalf("cross-check status %q is not %q and must block every write, got %d: %+v",
					tc.status, crossCheckStatusPassed, len(writes), writes)
			}
			if unmonitored != 0 || writeErrors != 0 || echoUnverified != 0 || writesRefused != 0 {
				t.Errorf("a blocked pass wrote nothing, failed at nothing, and left nothing unconfirmed or refused; got unmonitored=%d writeErrors=%d echoUnverified=%d writesRefused=%d", unmonitored, writeErrors, echoUnverified, writesRefused)
			}

			out := buf.String()
			// Blocking must be loud, and it must carry the count: an
			// unrecognized status silently withholding writes looks exactly
			// like a run with nothing to write.
			if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "withheld") {
				t.Errorf("expected a warning explaining that writes were withheld:\n%s", out)
			}
			if !strings.Contains(out, "withheldWrites=1") {
				t.Errorf("expected the withheld warning to state how many writes it blocked:\n%s", out)
			}
			// The unrecognized status itself has to reach the log, or the
			// operator has no way to tell which value blocked the pass.
			if !strings.Contains(out, "crossCheck="+strconv.Quote(tc.status)) && !strings.Contains(out, "crossCheck="+tc.status) {
				t.Errorf("expected the warning to name the status %q that blocked the pass:\n%s", tc.status, out)
			}
			if strings.Contains(out, "msg=unmonitor ") {
				t.Errorf("a blocked pass must never log a completed unmonitor:\n%s", out)
			}
		})
	}
}

// TestRunWritePass_UnrecognizedCrossCheckStatus_NothingPending_StillWarns
// pins the REVIEW FIX (Phase 6, carried forward from the phase-5 branch
// review) closing a gap the noise-budget fix left open: the "nothing pending
// means downgrade WARN to INFO" rule (see the sibling test
// TestRunRadarrDecisionEngine_CrossCheckBlockedWithNothingToWrite_IsInfoNotWarn)
// is only safe for a status this code actually understands — "inconclusive"
// or "failed" with nothing to write really is a benign, ignorable cycle. An
// UNRECOGNIZED status (a typo in a future status constant, a zero-value
// crossCheckResult that never got its status set) is a sign something is
// actually broken, and silencing it just because no write happened to be
// pending this cycle would let that bug hide behind the exact noise-budget
// mechanism meant to protect a human running this in a daemon loop from
// missing the warning that matters.
func TestRunWritePass_UnrecognizedCrossCheckStatus_NothingPending_StillWarns(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{})
	client := NewAPIClient(fake.instance().URL, fake.instance().APIKey)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	// No would-unmonitor decisions at all: pending == 0, the case the
	// noise-budget fix downgrades to INFO for a KNOWN status.
	decisions := []movieDecision{{id: 1, title: "Skipped Movie", wouldUnmonitor: false, reason: ReasonNoFile}}
	cc := crossCheckResult{status: "partially-verified"} // not one of the three known constants
	unmonitored, writeErrors, echoUnverified, writesRefused, withheld := runWritePass(context.Background(), logger, client, fake.instance(), decisions, cc, 0, false, false)

	if unmonitored != 0 || writeErrors != 0 || echoUnverified != 0 || writesRefused != 0 || withheld != 0 {
		t.Errorf("unmonitored/writeErrors/echoUnverified/writesRefused/withheld = %d/%d/%d/%d/%d, want all 0", unmonitored, writeErrors, echoUnverified, writesRefused, withheld)
	}
	out := buf.String()
	var withheldLine string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "withheld") {
			withheldLine = line
			break
		}
	}
	if withheldLine == "" {
		t.Fatalf("expected a line explaining writes were withheld:\n%s", out)
	}
	if !strings.Contains(withheldLine, "level=WARN") {
		t.Errorf("an unrecognized cross-check status must stay WARN even with nothing pending (it is a sign of a bug, not a benign cycle): %s", withheldLine)
	}
}

// TestRunRadarrDecisionEngine_WriteFailure_CountedAndRunContinues pins §2.6
// at the pass level: a rejected PUT is logged at error level, counted, and
// never retried, and the next item is still attempted.
func TestRunRadarrDecisionEngine_WriteFailure_CountedAndRunContinues(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{
		1: monitoredMovieDetail(1, "Rejected Movie"),
		2: monitoredMovieDetail(2, "Accepted Movie"),
	})
	fake.putStatus[1] = http.StatusInternalServerError

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{
		wouldUnmonitorMovie(1, "Rejected Movie"),
		wouldUnmonitorMovie(2, "Accepted Movie"),
	}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), false)

	puts := fake.puts()
	if len(puts) != 2 {
		t.Fatalf("expected 2 PUT attempts (one per item, never retried), got %d: %+v", len(puts), puts)
	}
	out := buf.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "Rejected Movie") {
		t.Errorf("expected an error-level log naming the failed write:\n%s", out)
	}
	if !strings.Contains(out, "writeErrors=1") {
		t.Errorf("expected writeErrors=1 in the summary:\n%s", out)
	}
	if !strings.Contains(out, "unmonitored=1") {
		t.Errorf("expected the later item to still be written (unmonitored=1):\n%s", out)
	}
}

// TestRunRadarrDecisionEngine_WriteAcceptedButUnverifiable_IsNotAWriteFailure
// pins the split the review demanded: a PUT the server ACCEPTED but did not
// confirm is not the same event as a PUT the server rejected, and the run may
// not report it with the same words or the same counter.
//
// The stakes are concrete. Radarr answers PUT /api/v3/movie/{id} with the
// updated resource, but that is an observation about one version; if the
// first live write ever comes back 202 with an empty body, the old wording
// ("unmonitor write failed; skipping this movie for the cycle") would tell
// the human two things at once, both of them probably false: that the write
// path is broken, and that the movie is still monitored. The truth is that
// the write was accepted and almost certainly applied — we simply cannot
// prove it from the response.
func TestRunRadarrDecisionEngine_WriteAcceptedButUnverifiable_IsNotAWriteFailure(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{1: monitoredMovieDetail(1, "Silently Accepted Movie")})
	// Accepted, and utterly silent about what it did.
	fake.putStatus[1] = http.StatusAccepted
	fake.putEcho[1] = ""

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{wouldUnmonitorMovie(1, "Silently Accepted Movie")}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), false)

	// §2.6: reported once, never retried, even though the outcome is unknown.
	if puts := fake.puts(); len(puts) != 1 {
		t.Fatalf("expected exactly 1 PUT attempt (an unconfirmed write is never retried), got %d: %+v", len(puts), puts)
	}

	out := buf.String()
	if strings.Contains(out, "unmonitor write failed") {
		t.Errorf("an accepted-but-unconfirmed write must not be reported as a failed write:\n%s", out)
	}
	for _, want := range []string{"level=WARN", "accepted", "unverifiable", "reconcile"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected the unverified-write warning to contain %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, `title="Silently Accepted Movie"`) || !strings.Contains(out, "id=1") {
		t.Errorf("expected the warning to name the movie it concerns:\n%s", out)
	}
	// The counters are the machine-readable half of the same statement, and
	// Phase 5's no-op contract reads writeErrors: an unverifiable write must
	// not inflate it.
	if !strings.Contains(out, "writeErrors=0") {
		t.Errorf("expected writeErrors=0: nothing was rejected:\n%s", out)
	}
	if !strings.Contains(out, "writeEchoUnverified=1") {
		t.Errorf("expected writeEchoUnverified=1 in the summary:\n%s", out)
	}
	// unmonitored counts CONFIRMED writes and nothing else; the movie is
	// treated as written only in the sense that it is not retried and not
	// called a failure.
	if !strings.Contains(out, "unmonitored=0") {
		t.Errorf("expected unmonitored=0: no write was confirmed:\n%s", out)
	}
	if strings.Contains(out, "msg=unmonitor ") {
		t.Errorf("an unconfirmed write must not log the completed-unmonitor line:\n%s", out)
	}
}

// TestRunRadarrDecisionEngine_EchoSaysStillMonitored_IsAWriteFailure is the
// other side of that line, and the reason it is drawn by errors.Is rather
// than by "the PUT returned 2xx". An echo that says "monitored": true is not
// a failure to confirm — it is a confirmed failure. The server told us the
// movie is still monitored, so this stays an ERROR and stays in writeErrors.
func TestRunRadarrDecisionEngine_EchoSaysStillMonitored_IsAWriteFailure(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{1: monitoredMovieDetail(1, "Stubborn Movie")})
	fake.putEcho[1] = `{"id": 1, "title": "Stubborn Movie", "monitored": true}`

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{wouldUnmonitorMovie(1, "Stubborn Movie")}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), false)

	out := buf.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "unmonitor write failed") {
		t.Errorf("an echo stating the movie is STILL monitored is a failed write:\n%s", out)
	}
	if !strings.Contains(out, "writeErrors=1") {
		t.Errorf("expected writeErrors=1:\n%s", out)
	}
	if !strings.Contains(out, "writeEchoUnverified=0") {
		t.Errorf("expected writeEchoUnverified=0: the response was perfectly verifiable, it just said no:\n%s", out)
	}
	if !strings.Contains(out, "unmonitored=0") {
		t.Errorf("expected unmonitored=0:\n%s", out)
	}
}

// TestRunRadarrDecisionEngine_AlreadyUnmonitoredAtWriteTime_NoPut covers the
// scan-to-write race through the pass: the item was monitored during the
// scan but is already unmonitored by the time the write pass reaches it, so
// nothing is written.
//
// REVIEW FIX (Phase 5 round 4): this race used to leave EVERY counter on the
// summary line at zero — wouldUnmonitor=1 unmonitored=0 writeErrors=0
// writesRefused=0 alreadyUnmonitored=0 — with nothing at all explaining
// where the one promised write went; an instance where all N candidates hit
// this race would report the identical, fully-zeroed line regardless of N.
//
// REVIEW FIX (Phase 5 final round): it is counted as writesRefused, the single
// counter for every would-unmonitor decision the write pass declined to act on
// (see runWritePass's reconciliation identity). The round-4 fix gave the race
// its own attr, alreadyUnmonitoredAtWrite, which solved the accounting but
// added a sixth number to a line that already carries five, one letter-for-
// letter confusable with the unrelated scan-time alreadyUnmonitored beside it.
// The causes stay distinguishable where a human actually reads them — each
// refusal logs its own message — while the summary keeps ONE number for "a
// promised write that did not happen and was not a failure". alreadyUnmonitored
// (scan-time, rule-1 exclusions) must still read 0 here: this movie was
// monitored:true when the library was scanned, so rule 1 never touched it.
func TestRunRadarrDecisionEngine_AlreadyUnmonitoredAtWriteTime_NoPut(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{
		1: `{"id": 1, "title": "Raced Movie", "monitored": false}`,
	})

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{wouldUnmonitorMovie(1, "Raced Movie")}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), false)

	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("expected zero write requests of any method for an already-unmonitored movie, got %+v", writes)
	}
	out := buf.String()
	if !strings.Contains(out, "already unmonitored") {
		t.Errorf("expected an info log explaining the write was unnecessary:\n%s", out)
	}
	for _, want := range []string{"wouldUnmonitor=1", "unmonitored=0", "writeErrors=0", "writesRefused=1", "alreadyUnmonitored=0"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in the summary — writesRefused is the only number that accounts for the 1 promised write when every other counter (including the unrelated scan-time alreadyUnmonitored) is 0:\n%s", want, out)
		}
	}
	if strings.Contains(out, "alreadyUnmonitoredAtWrite") {
		t.Errorf("the summary must not carry a second already-unmonitored counter one character away from the scan-time one; the race is counted as a refusal:\n%s", out)
	}
}

// TestRunRadarrDecisionEngine_ExclusionTagAddedBetweenScanAndWrite_RefusesTheWrite
// is the end-to-end pin for the pre-write tag re-check (Phase 5, mandated
// write-path safety fix): the scan evaluated this movie before it was
// tagged, so the decision engine's own would-unmonitor line is produced as
// normal — the decision was correct when it was made — but by the time the
// write pass re-fetches the movie fresh, Radarr already shows the exclusion
// tag, and §2.5 says that tag always wins, in every mode. This proves the
// wiring from resolveExclusionTagID through runWritePass into
// unmonitorMovie, not just the unit behavior (pinned separately in
// writer_test.go).
func TestRunRadarrDecisionEngine_ExclusionTagAddedBetweenScanAndWrite_RefusesTheWrite(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{
		1: `{"id": 1, "title": "Tagged Since Scan", "monitored": true, "hasFile": true, "qualityProfileId": 1, "tags": [42]}`,
	})
	fake.tagsJSON = `[{"id": 42, "label": "cutoffarr-exclude"}]`

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{wouldUnmonitorMovie(1, "Tagged Since Scan")}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), false)

	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("expected zero write requests of any method: the fresh fetch shows the exclusion tag, which always wins, got %+v", writes)
	}
	out := buf.String()
	if !strings.Contains(out, "msg=would-unmonitor") {
		t.Errorf("the scan-time report must be unaffected; the decision was correct when it was made:\n%s", out)
	}
	if !strings.Contains(out, "exclusion tag") {
		t.Errorf("expected the write refusal to name the exclusion tag as the reason:\n%s", out)
	}
	if !strings.Contains(out, "unmonitored=0") {
		t.Errorf("expected unmonitored=0:\n%s", out)
	}
	if !strings.Contains(out, "writeErrors=0") {
		t.Errorf("a tag-excluded skip is not a write failure:\n%s", out)
	}
	// REVIEW FIX (Phase 5 round 2): the refusal must be counted somewhere on
	// the summary line, or a report reading wouldUnmonitor=1 unmonitored=0
	// writeErrors=0 leaves no number that explains where the promised write
	// went.
	if !strings.Contains(out, "writesRefused=1") {
		t.Errorf("expected writesRefused=1: the pre-write tag re-check refused this write, and that must be counted:\n%s", out)
	}
}

// TestRunRadarrDecisionEngine_DryRun_ExclusionTagAddedBetweenScanAndWrite_RefusesTheWrite
// is the dry-run twin of
// TestRunRadarrDecisionEngine_ExclusionTagAddedBetweenScanAndWrite_RefusesTheWrite
// (REVIEW FIX, Phase 5 round 2): the pre-write exclusion-tag re-check must
// run identically "in every mode" (§2.5, brief item 3) — writer.go's own
// doc comment says the re-check sits above the dry-run gate at
// writer.go:209, and decision.go deliberately emits writesRefused
// unconditionally rather than inside the write-mode branch, on exactly that
// claim. Before this test, every scenario that made tagActive true through
// the real pipeline was write-mode only, so both claims were unpinned: a
// future edit could move the re-check below the dry-run gate, or fold
// writesRefused into the write-mode branch, and the whole suite would stay
// green while a dry-run report promised writes a real run would refuse.
func TestRunRadarrDecisionEngine_DryRun_ExclusionTagAddedBetweenScanAndWrite_RefusesTheWrite(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{
		1: `{"id": 1, "title": "Tagged Since Scan", "monitored": true, "hasFile": true, "qualityProfileId": 1, "tags": [42]}`,
	})
	fake.tagsJSON = `[{"id": 42, "label": "cutoffarr-exclude"}]`

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{wouldUnmonitorMovie(1, "Tagged Since Scan")}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("expected zero write requests of any method even in dry-run: the fresh fetch shows the exclusion tag, which always wins, got %+v", writes)
	}
	out := buf.String()
	if !strings.Contains(out, "msg=would-unmonitor") {
		t.Errorf("the scan-time report must be unaffected; the decision was correct when it was made:\n%s", out)
	}
	if !strings.Contains(out, "exclusion tag") {
		t.Errorf("expected the write refusal to name the exclusion tag as the reason, in dry-run just as in write mode:\n%s", out)
	}
	if !strings.Contains(out, "unmonitored=0") {
		t.Errorf("expected unmonitored=0:\n%s", out)
	}
	if !strings.Contains(out, "writesRefused=1") {
		t.Errorf("expected writesRefused=1: the pre-write tag re-check runs identically in dry-run, before the dry-run gate, so the refusal must be counted here too:\n%s", out)
	}
}

// TestRunRadarrDecisionEngine_AllPreWriteTagChecksRefused_SummaryAccountsForEveryPendingWrite
// is the scenario the review round found genuinely unaccounted for: an
// instance where the exclusion tag is active and the fresh pre-write fetch
// for every single would-unmonitor candidate is refused (here: "tags" is
// absent from the detail fixture, the untrusted-input case). Before this
// fix, such a run reported wouldUnmonitor=2 against unmonitored=0,
// writeErrors=0, writeEchoUnverified=0, alreadyUnmonitored=0 — every counter
// on the line zero, with nothing at all explaining where the two promised
// writes went. writesRefused is the number that closes that gap.
func TestRunRadarrDecisionEngine_AllPreWriteTagChecksRefused_SummaryAccountsForEveryPendingWrite(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{
		1: `{"id": 1, "title": "No Tags Key A", "monitored": true, "hasFile": true, "qualityProfileId": 1}`,
		2: `{"id": 2, "title": "No Tags Key B", "monitored": true, "hasFile": true, "qualityProfileId": 1}`,
	})
	fake.tagsJSON = `[{"id": 42, "label": "cutoffarr-exclude"}]`

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{
		wouldUnmonitorMovie(1, "No Tags Key A"),
		wouldUnmonitorMovie(2, "No Tags Key B"),
	}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), false)

	if puts := fake.puts(); len(puts) != 0 {
		t.Fatalf("expected zero PUTs: every fresh fetch's tags could not be verified, got %+v", puts)
	}
	out := buf.String()
	for _, want := range []string{"wouldUnmonitor=2", "unmonitored=0", "writeErrors=0", "writeEchoUnverified=0", "alreadyUnmonitored=0", "writesRefused=2"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in the summary — writesRefused is the only number that accounts for the 2 pending writes when every other counter is 0:\n%s", want, out)
		}
	}
}

// --- every promised write is accounted for ---------------------------------
//
// REVIEW FIX (Phase 5 final round), and the reason the individual counters
// above exist at all: the summary line's job is to leave no unexplained gap
// between what the report promised (wouldUnmonitor=N) and what happened. Two
// review rounds each found a different silent (false, nil) return that made N
// writes evaporate with every counter reading zero, and each was closed by
// adding a counter. The pattern is the bug: a THIRD such path would be found
// the same way, by a human noticing numbers that do not add up, which is only
// possible if they are supposed to add up in the first place.
//
// So the identity is now stated, and pinned:
//
//	wouldUnmonitor == unmonitored + writeEchoUnverified + writeErrors
//	                  + writeRehearsalErrors + writesRefused + withheldWrites
//
// (writeErrors and writeRehearsalErrors are the same counter under two names —
// only one of them is ever printed, decided by dry-run — so summing both is
// exact in either mode.) Any future write outcome that does not increment one
// of these terms fails this test on the first scenario that reaches it.

// summaryCounters parses every integer attr off the run's single "radarr
// decision summary" line. Non-integer attrs (level, msg, crossCheck,
// skipReasons) are ignored rather than parsed, since nothing here asks about
// them; splitting on whitespace is safe for exactly that reason — the values
// that contain spaces are the ones this never looks up.
func summaryCounters(t *testing.T, output string) map[string]int {
	t.Helper()
	return summaryCountersFor(t, output, "radarr decision summary")
}

// summaryCountersFor is summaryCounters generalized over which engine's
// summary line to parse, so the Sonarr write pass's own accounting-identity
// test (sonarr_writer_test.go) reads its counters through exactly the same
// parser rather than a second copy that could drift from it.
func summaryCountersFor(t *testing.T, output, msg string) map[string]int {
	t.Helper()
	var line string
	for _, l := range strings.Split(output, "\n") {
		if !strings.Contains(l, msg) {
			continue
		}
		if line != "" {
			t.Fatalf("expected exactly one summary line per instance:\n%s", output)
		}
		line = l
	}
	if line == "" {
		t.Fatalf("no %q line in the output:\n%s", msg, output)
	}
	counters := make(map[string]int)
	for _, field := range strings.Fields(line) {
		key, value, found := strings.Cut(field, "=")
		if !found {
			continue
		}
		if n, err := strconv.Atoi(value); err == nil {
			counters[key] = n
		}
	}
	return counters
}

func TestRunRadarrDecisionEngine_EveryWouldUnmonitorDecisionIsAccountedForInTheSummary(t *testing.T) {
	// Each case makes its single would-unmonitor decision end a different
	// way. The named counter is the one that must absorb it; the identity
	// check is what proves nothing fell between the counters.
	cases := []struct {
		name        string
		detail      map[int]string
		tagsJSON    string
		putStatus   map[int]int
		putEcho     map[string]string // by id, as a string key so the literal stays readable
		dryRun      bool
		wantCounter string
	}{
		{
			name:        "confirmed write",
			detail:      map[int]string{1: monitoredMovieDetail(1, "Accounted Movie")},
			wantCounter: "unmonitored",
		},
		{
			name:        "the server rejected the PUT",
			detail:      map[int]string{1: monitoredMovieDetail(1, "Accounted Movie")},
			putStatus:   map[int]int{1: http.StatusInternalServerError},
			wantCounter: "writeErrors",
		},
		{
			name:        "the PUT was accepted but the echo confirms nothing",
			detail:      map[int]string{1: monitoredMovieDetail(1, "Accounted Movie")},
			putEcho:     map[string]string{"1": ""},
			wantCounter: "writeEchoUnverified",
		},
		{
			name:        "the movie vanished before the pre-write fetch",
			detail:      map[int]string{},
			wantCounter: "writeErrors",
		},
		{
			name:        "the exclusion tag was added between scan and write",
			detail:      map[int]string{1: `{"id": 1, "title": "Accounted Movie", "monitored": true, "tags": [42]}`},
			tagsJSON:    `[{"id": 42, "label": "cutoffarr-exclude"}]`,
			wantCounter: "writesRefused",
		},
		{
			name:        "the fresh payload's tags cannot be verified",
			detail:      map[int]string{1: `{"id": 1, "title": "Accounted Movie", "monitored": true}`},
			tagsJSON:    `[{"id": 42, "label": "cutoffarr-exclude"}]`,
			wantCounter: "writesRefused",
		},
		{
			name:        "something else unmonitored it first",
			detail:      map[int]string{1: `{"id": 1, "title": "Accounted Movie", "monitored": false}`},
			wantCounter: "writesRefused",
		},
		{
			name:        "the fresh payload's monitored is JSON null",
			detail:      map[int]string{1: `{"id": 1, "title": "Accounted Movie", "monitored": null}`},
			wantCounter: "writesRefused",
		},
		{
			name:        "the fresh payload has no monitored key",
			detail:      map[int]string{1: `{"id": 1, "title": "Accounted Movie"}`},
			wantCounter: "writesRefused",
		},
		{
			name:        "dry-run withheld the write at the gate",
			detail:      map[int]string{1: monitoredMovieDetail(1, "Accounted Movie")},
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
			fake := newRadarrFake(t, "", tc.detail)
			if tc.tagsJSON != "" {
				fake.tagsJSON = tc.tagsJSON
			}
			for id, status := range tc.putStatus {
				fake.putStatus[id] = status
			}
			for id, echo := range tc.putEcho {
				n, err := strconv.Atoi(id)
				if err != nil {
					t.Fatalf("bad putEcho key %q: %v", id, err)
				}
				fake.putEcho[n] = echo
			}

			logger, buf := newDecisionTestLogger(slog.LevelInfo)
			movies := []movieListElement{wouldUnmonitorMovie(1, "Accounted Movie")}
			runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), tc.dryRun)

			out := buf.String()
			c := summaryCounters(t, out)
			if c["wouldUnmonitor"] != 1 {
				t.Fatalf("wouldUnmonitor = %d, want 1: this case must actually reach the write pass or it proves nothing:\n%s", c["wouldUnmonitor"], out)
			}
			if c[tc.wantCounter] != 1 {
				t.Errorf("%s = %d, want 1: this outcome must be counted under that name:\n%s", tc.wantCounter, c[tc.wantCounter], out)
			}
			accounted := c["unmonitored"] + c["writeEchoUnverified"] + c["writeErrors"] + c["writeRehearsalErrors"] + c["writesRefused"] + c["withheldWrites"]
			if accounted != c["wouldUnmonitor"] {
				t.Errorf("the summary accounts for %d of %d promised writes; every would-unmonitor decision must end in exactly one counted outcome:\n%s", accounted, c["wouldUnmonitor"], out)
			}
			// Both mode-independent counters are always printed, including as
			// 0: a reader must never have to wonder whether an absent number
			// means "none" or "not reported".
			for _, always := range []string{"writesRefused=", "withheldWrites="} {
				if !strings.Contains(out, always) {
					t.Errorf("the summary must always carry %s, including as 0:\n%s", always, out)
				}
			}
		})
	}
}

// TestRunRadarrDecisionEngine_GateBlocked_WithheldWritesAccountsForThePass is
// the identity's remaining term, which no case above can reach: when the
// cross-check refuses to authorize the pass, every pending write is withheld
// before unmonitorMovie is called even once, so nothing else can count them.
func TestRunRadarrDecisionEngine_GateBlocked_WithheldWritesAccountsForThePass(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{
		1: monitoredMovieDetail(1, "Blocked Movie A"),
		2: monitoredMovieDetail(2, "Blocked Movie B"),
	})

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	// Both movies are would-unmonitor decisions whose own qualityCutoffNotMet
	// disagrees with their absence from the wanted set: the cross-check FAILS
	// and the gate blocks the whole pass.
	movies := []movieListElement{
		{ID: intPtr(1), Title: strPtr("Blocked Movie A"), Monitored: boolPtr(true), HasFile: boolPtr(true),
			QualityProfileID: intPtr(1), Tags: &noTags, MovieFile: &movieFileElement{ID: intPtr(1), QualityCutoffNotMet: boolPtr(true)}},
		{ID: intPtr(2), Title: strPtr("Blocked Movie B"), Monitored: boolPtr(true), HasFile: boolPtr(true),
			QualityProfileID: intPtr(1), Tags: &noTags, MovieFile: &movieFileElement{ID: intPtr(1), QualityCutoffNotMet: boolPtr(true)}},
	}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), false)

	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("a blocked pass must write nothing, got %+v", writes)
	}
	out := buf.String()
	c := summaryCounters(t, out)
	if c["wouldUnmonitor"] != 2 || c["withheldWrites"] != 2 {
		t.Errorf("wouldUnmonitor=%d withheldWrites=%d, want 2 and 2: the summary itself must account for a blocked pass, not only the warning above it:\n%s",
			c["wouldUnmonitor"], c["withheldWrites"], out)
	}
	accounted := c["unmonitored"] + c["writeEchoUnverified"] + c["writeErrors"] + c["writeRehearsalErrors"] + c["writesRefused"] + c["withheldWrites"]
	if accounted != c["wouldUnmonitor"] {
		t.Errorf("the summary accounts for %d of %d promised writes:\n%s", accounted, c["wouldUnmonitor"], out)
	}
}

// TestRunRadarrDecisionEngine_DryRun_PreWriteFetchFailure_IsARehearsalFailureNotAWriteError
// covers the one path that can fail during a dry-run, and the reason it
// needs its own name in the report.
//
// Because the write pass runs in dry-run too (that is what makes a dry-run a
// real rehearsal), every would-unmonitor movie still gets a fresh GET
// /api/v3/movie/{id}. That GET can fail — a movie deleted between the
// library scan and the write pass 404s, a restarting Radarr 502s — and the
// failure is real and worth an ERROR line. What it is not is a write error:
// no write was attempted, and none could have been. Counting it as
// writeErrors=N would put a number the human reads as "N writes failed" in
// the report of a run during which nothing was ever written, so the two are
// reported under separate names.
func TestRunRadarrDecisionEngine_DryRun_PreWriteFetchFailure_IsARehearsalFailureNotAWriteError(t *testing.T) {
	// The detail map has no entry for movie 1, so the fake answers the
	// pre-write GET with a 404: the movie vanished between the scan and the
	// write pass.
	fake := newRadarrFake(t, "", map[int]string{})

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{wouldUnmonitorMovie(1, "Vanished Movie")}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("a dry-run must make zero write requests even when the pre-write fetch fails, got %+v", writes)
	}
	out := buf.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "Vanished Movie") {
		t.Errorf("a failed pre-write fetch must still be reported at error level, naming the movie:\n%s", out)
	}
	if !strings.Contains(out, "rehearsal") {
		t.Errorf("the dry-run failure must not be worded as a failed write; no write was attempted:\n%s", out)
	}
	if !strings.Contains(out, "writeRehearsalErrors=1") {
		t.Errorf("expected writeRehearsalErrors=1 in the dry-run summary:\n%s", out)
	}
	if !strings.Contains(out, "writeErrors=0") {
		t.Errorf("writeErrors must stay 0 in dry-run: no write was attempted, so none can have failed:\n%s", out)
	}
	if !strings.Contains(out, "unmonitored=0") {
		t.Errorf("expected unmonitored=0:\n%s", out)
	}
}

// TestRunRadarrDecisionEngine_WriteMode_HasNoRehearsalCounter pins the other
// half: outside dry-run there is no rehearsal, so a failure is a write
// failure and is reported as one, with no second counter to read past.
func TestRunRadarrDecisionEngine_WriteMode_HasNoRehearsalCounter(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{})

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{wouldUnmonitorMovie(1, "Vanished Movie")}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), false)

	out := buf.String()
	if !strings.Contains(out, "writeErrors=1") {
		t.Errorf("expected writeErrors=1 in the write-mode summary:\n%s", out)
	}
	if strings.Contains(out, "writeRehearsalErrors") {
		t.Errorf("writeRehearsalErrors has no meaning outside dry-run and must not appear:\n%s", out)
	}
	if strings.Contains(out, "rehearsal") {
		t.Errorf("a failure in write mode is a write failure, not a rehearsal failure:\n%s", out)
	}
}

// TestRunRadarrDecisionEngine_ReportLinesCarryMovieId pins the mandated new
// attr: the human picks --only-id values straight out of these lines, so
// both would-unmonitor and skip lines must carry the movie id.
func TestRunRadarrDecisionEngine_ReportLinesCarryMovieId(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{1: monitoredMovieDetail(1, "Would Unmonitor Movie")})

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{
		wouldUnmonitorMovie(1, "Would Unmonitor Movie"),
		skippedMovie(2, "No File Movie"),
	}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

	out := buf.String()
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "msg=would-unmonitor") && !strings.Contains(line, "msg=skip") {
			continue
		}
		if !strings.Contains(line, "id=") {
			t.Errorf("report line carries no id attr: %s", line)
		}
	}
	if !strings.Contains(out, "msg=would-unmonitor id=1") {
		t.Errorf("expected the would-unmonitor line to lead with id=1:\n%s", out)
	}
	if !strings.Contains(out, "msg=skip id=2") {
		t.Errorf("expected the skip line to lead with id=2:\n%s", out)
	}
}

// --- --only-id scoping ------------------------------------------------------

// TestRunRadarrDecisionEngine_OnlyID_ReportsAndWritesOnlyTheTarget pins the
// flag's core contract: one report line, one write, and no report lines at
// all for any other movie.
func TestRunRadarrDecisionEngine_OnlyID_ReportsAndWritesOnlyTheTarget(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{
		1: monitoredMovieDetail(1, "Other Movie"),
		2: monitoredMovieDetail(2, "Target Movie"),
		3: monitoredMovieDetail(3, "Another Movie"),
	})

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{
		wouldUnmonitorMovie(1, "Other Movie"),
		wouldUnmonitorMovie(2, "Target Movie"),
		wouldUnmonitorMovie(3, "Another Movie"),
	}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", onlyIDScope(2), false)

	puts := fake.puts()
	if len(puts) != 1 {
		t.Fatalf("expected exactly 1 PUT in --only-id mode, got %d: %+v", len(puts), puts)
	}
	if puts[0].path != "/api/v3/movie/2" {
		t.Errorf("PUT went to %q, want /api/v3/movie/2", puts[0].path)
	}

	out := buf.String()
	if !strings.Contains(out, `title="Target Movie"`) {
		t.Errorf("expected a report line for the target movie:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "msg=would-unmonitor") && !strings.Contains(line, "msg=skip") {
			continue
		}
		if !strings.Contains(line, "Target Movie") {
			t.Errorf("--only-id mode produced a report line for a non-target movie: %s", line)
		}
	}
	if !strings.Contains(out, "unmonitored=1") {
		t.Errorf("expected unmonitored=1:\n%s", out)
	}
}

// TestRunRadarrDecisionEngine_OnlyID_CrossCheckStillRunsOnFullCandidatePools
// pins the deliberate exception to that scoping: the cross-check validates
// the DATA, not the target, so it still samples the whole library. Narrowing
// it to the single target would reduce the project's first write to a
// one-item safety gate.
func TestRunRadarrDecisionEngine_OnlyID_CrossCheckStillRunsOnFullCandidatePools(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{2: monitoredMovieDetail(2, "Target Movie")})

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{
		wouldUnmonitorMovie(1, "Other Movie"),
		wouldUnmonitorMovie(2, "Target Movie"),
		wouldUnmonitorMovie(3, "Another Movie"),
	}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", onlyIDScope(2), true)

	out := buf.String()
	if !strings.Contains(out, `crossCheck="passed (3 verified, 0 unverifiable)"`) {
		t.Errorf("expected the cross-check to have verified all 3 library candidates, not just the --only-id target:\n%s", out)
	}
}

// TestRunRadarrDecisionEngine_OnlyID_AlreadyUnmonitoredScopedToTarget is the
// review-mandated correction to rule 1's "already unmonitored" counter and
// debug line: every OTHER counter and attr on the same "radarr decision
// summary" line (totalMonitored, wouldUnmonitor, unmonitored) is scoped to
// the --only-id target, because they are only ever populated from `reported`,
// which the --only-id narrowing at the bottom of the movies loop builds.
// alreadyUnmonitoredCount/the debug line, by contrast, used to be incremented
// unconditionally inside the rule-1 branch, BEFORE that narrowing block is
// ever reached — so a targeted run against a large library would report an
// alreadyUnmonitored count (and emit a debug line per movie) for the entire
// library, silently mixing a library-wide population into a summary line
// whose every neighbouring number is about one movie. This pins the fix: the
// counter and the debug line apply the exact same scope test the narrowing
// block already uses.
func TestRunRadarrDecisionEngine_OnlyID_AlreadyUnmonitoredScopedToTarget(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{})

	logger, buf := newDecisionTestLogger(slog.LevelDebug)
	movies := []movieListElement{
		{ID: intPtr(1), Title: strPtr("Other Already Unmonitored"), Monitored: boolPtr(false), HasFile: boolPtr(true), QualityProfileID: intPtr(1)},
		{ID: intPtr(2), Title: strPtr("Target Already Unmonitored"), Monitored: boolPtr(false), HasFile: boolPtr(true), QualityProfileID: intPtr(1)},
	}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", onlyIDScope(2), true)

	out := buf.String()
	if strings.Contains(out, "Other Already Unmonitored") {
		t.Errorf("--only-id must not count or log the already-unmonitored line for a movie other than the target:\n%s", out)
	}
	if !strings.Contains(out, `msg="already unmonitored" id=2 title="Target Already Unmonitored" instance=radarr-main`) {
		t.Errorf("expected the target's own already-unmonitored debug line:\n%s", out)
	}
	if !strings.Contains(out, "alreadyUnmonitored=1") {
		t.Errorf("expected alreadyUnmonitored=1, scoped to the --only-id target only (not the whole library):\n%s", out)
	}
}

// TestRunRadarrDecisionEngine_OnlyID_CrossCheckDisagreementElsewhereBlocksTheWrite
// is the consequence of that decision, and the reason it matters: bad data
// anywhere in the library withholds the write even when the target itself
// looks perfect.
func TestRunRadarrDecisionEngine_OnlyID_CrossCheckDisagreementElsewhereBlocksTheWrite(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{2: monitoredMovieDetail(2, "Target Movie")})

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{
		{ID: intPtr(1), Title: strPtr("Disagreeing Movie"), Monitored: boolPtr(true), HasFile: boolPtr(true),
			QualityProfileID: intPtr(1), Tags: &noTags,
			MovieFile: &movieFileElement{ID: intPtr(1), QualityCutoffNotMet: boolPtr(true)}},
		wouldUnmonitorMovie(2, "Target Movie"),
	}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", onlyIDScope(2), false)

	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("a disagreement elsewhere in the library must still withhold the --only-id write, got %+v", writes)
	}
	if !strings.Contains(buf.String(), "crossCheck=FAILED") {
		t.Errorf("expected the cross-check to have failed on the other movie:\n%s", buf.String())
	}
}

// TestRunRadarrDecisionEngine_OnlyID_UnknownId_WarnsAndMakesNoDecisions pins
// the not-found case: warn naming the id, produce no decisions, and touch
// nothing.
func TestRunRadarrDecisionEngine_OnlyID_UnknownId_WarnsAndMakesNoDecisions(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{1: monitoredMovieDetail(1, "Only Movie")})

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{wouldUnmonitorMovie(1, "Only Movie")}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", onlyIDScope(4242), false)

	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("an unknown --only-id must write nothing, got %+v", writes)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "4242") {
		t.Errorf("expected a warning naming the id that was not found:\n%s", out)
	}
	if strings.Contains(out, "msg=would-unmonitor") || strings.Contains(out, "msg=skip") {
		t.Errorf("an unknown --only-id must produce no decisions at all:\n%s", out)
	}
}

// TestRunRadarrDecisionEngine_OnlyID_TargetIsSkipped_NoWriteButSkipLineLogged
// pins that --only-id never bypasses the decision rules: a target that fails
// a rule is reported as a skip and is not written.
func TestRunRadarrDecisionEngine_OnlyID_TargetIsSkipped_NoWriteButSkipLineLogged(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{2: monitoredMovieDetail(2, "Target No File")})

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{
		wouldUnmonitorMovie(1, "Other Movie"),
		skippedMovie(2, "Target No File"),
	}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", onlyIDScope(2), false)

	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("a skipped target must never be written, got %+v", writes)
	}
	out := buf.String()
	if !strings.Contains(out, "msg=skip") || !strings.Contains(out, `title="Target No File"`) || !strings.Contains(out, `reason="no file"`) {
		t.Errorf("expected the target's skip line with its reason:\n%s", out)
	}
	if !strings.Contains(out, "unmonitored=0") {
		t.Errorf("expected unmonitored=0:\n%s", out)
	}
}

// TestRunRadarrDecisionEngine_OnlyID_TargetNotMonitored_ExplainsTheNoOp
// guards against a silent no-op: rule 1 excludes an unmonitored movie from
// the report entirely, which in --only-id mode would otherwise mean a run
// that says nothing whatsoever about the movie the human explicitly named.
func TestRunRadarrDecisionEngine_OnlyID_TargetNotMonitored_ExplainsTheNoOp(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{2: monitoredMovieDetail(2, "Unmonitored Target")})

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{
		{ID: intPtr(2), Title: strPtr("Unmonitored Target"), Monitored: boolPtr(false), HasFile: boolPtr(true),
			QualityProfileID: intPtr(1), Tags: &noTags},
	}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", onlyIDScope(2), false)

	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("an unmonitored target must never be written, got %+v", writes)
	}
	if !strings.Contains(buf.String(), "produced no decision") {
		t.Errorf("expected an explanation of why the named movie produced no report line:\n%s", buf.String())
	}
}

// TestRunRadarrDecisionEngine_OnlyID_SummaryNamesTheScope keeps the summary
// honest: totalMonitored counts what was reported (the target alone), so
// the line must say it was scoped rather than appear to describe a full run.
func TestRunRadarrDecisionEngine_OnlyID_SummaryNamesTheScope(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{2: monitoredMovieDetail(2, "Target Movie")})

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{
		wouldUnmonitorMovie(1, "Other Movie"),
		wouldUnmonitorMovie(2, "Target Movie"),
	}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", onlyIDScope(2), true)

	out := buf.String()
	if !strings.Contains(out, "onlyId=2") {
		t.Errorf("expected the summary to name the --only-id scope:\n%s", out)
	}
	if !strings.Contains(out, "totalMonitored=1") {
		t.Errorf("expected totalMonitored=1 (only the target was reported):\n%s", out)
	}
}

// TestRunRadarrDecisionEngine_CrossCheckBlockedWithNothingToWrite_SaysSoPlainly
// is the other half of that: when the gate blocks a pass that had no
// candidates anyway, the warning must say zero rather than leave the reader
// to assume writes were lost.
func TestRunRadarrDecisionEngine_CrossCheckBlockedWithNothingToWrite_SaysSoPlainly(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{})

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	// A skip decision whose qualityCutoffNotMet is absent: it is a
	// cross-check candidate (monitored + hasFile), so the cross-check is
	// inconclusive, but it is not a would-unmonitor item.
	movies := []movieListElement{{
		ID: intPtr(1), Title: strPtr("In Wanted Set Movie"), Monitored: boolPtr(true), HasFile: boolPtr(true),
		QualityProfileID: intPtr(1), Tags: &noTags,
		MovieFile: &movieFileElement{ID: intPtr(1)},
	}}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{1: true}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), false)

	out := buf.String()
	if !strings.Contains(out, "withheldWrites=0") {
		t.Errorf("expected the withheld line to state that it blocked zero writes:\n%s", out)
	}
}

// TestRunRadarrDecisionEngine_CrossCheckBlockedWithNothingToWrite_IsInfoNotWarn
// is the Phase 5 noise-budget fix (mandated): the gate used to WARN every
// cycle a benign, nothing-to-write instance's cross-check came back
// inconclusive — noise that trains a human running this in a daemon loop to
// ignore the warning that matters. Blocking a pass that had zero writes to
// make is a health signal worth reporting, but not an alarm, so it is now
// INFO; blocking a pass that actually had writes pending (the sibling tests
// throughout this file, all with withheldWrites>=1) stays WARN.
func TestRunRadarrDecisionEngine_CrossCheckBlockedWithNothingToWrite_IsInfoNotWarn(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{})

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{{
		ID: intPtr(1), Title: strPtr("In Wanted Set Movie"), Monitored: boolPtr(true), HasFile: boolPtr(true),
		QualityProfileID: intPtr(1), Tags: &noTags,
		MovieFile: &movieFileElement{ID: intPtr(1)},
	}}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{1: true}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), false)

	out := buf.String()
	var withheldLine string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "withheld") {
			withheldLine = line
			break
		}
	}
	if withheldLine == "" {
		t.Fatalf("expected a line explaining writes were withheld:\n%s", out)
	}
	if strings.Contains(withheldLine, "level=WARN") {
		t.Errorf("a withheld-writes line that blocked ZERO writes must not be WARN (noise budget): %s", withheldLine)
	}
	if !strings.Contains(withheldLine, "level=INFO") {
		t.Errorf("expected the zero-withheld line to be INFO: %s", withheldLine)
	}
}

// TestRunRadarrDecisionEngine_OnlyID_TargetNotMonitored_MessageNamesTheActualCause
// keeps the no-op explanation accurate: reaching it means the target was
// found in the library, so the only thing that can have excluded it is rule
// 1. Naming a cause that cannot apply would send the reader looking for a
// data problem that is not there.
func TestRunRadarrDecisionEngine_OnlyID_TargetNotMonitored_MessageNamesTheActualCause(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{})

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{
		{ID: intPtr(2), Title: strPtr("Unmonitored Target"), Monitored: boolPtr(false), HasFile: boolPtr(true),
			QualityProfileID: intPtr(1), Tags: &noTags},
	}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", onlyIDScope(2), false)

	out := buf.String()
	if !strings.Contains(out, "not monitored") {
		t.Errorf("expected the no-op explanation to name rule 1 as the cause:\n%s", out)
	}
	if strings.Contains(out, "id field was absent") {
		t.Errorf("the message names a cause that cannot apply here (the target was found by id):\n%s", out)
	}
}

// --- the write gate needs more than a bare "passed" -------------------------
//
// REVIEW FIX (phase-4 round 2, IMPORTANT): the write gate consumed
// runCrossCheck's status and nothing else, and that status is awarded on the
// weakest possible evidence — "at least one sampled item was verified and
// nothing disagreed". Two consequences, both of which end with real PUTs
// authorized by data nobody checked:
//
//   - The two sampled pools are conflated. The cross-check samples up to 10
//     would-unmonitor decisions AND up to 10 monitored+hasFile skip
//     decisions, and a single verified SKIP item was enough to earn "passed"
//     — opening the write pass even though not one would-unmonitor decision,
//     the only kind that is ever written, had been verified at all.
//   - The unverifiable count never reached the gate. 19 of 20 samples
//     missing movieFile.qualityCutoffNotMet still rendered "passed (1
//     verified, 19 unverifiable)" and authorized the whole library, with no
//     gate-level warning: the aggregate warning fires only at verified == 0.
//
// The gate now requires evidence about the decisions it is actually about to
// act on, and says out loud when it proceeds on a partially verified sample.

// TestRunRadarrDecisionEngine_CrossCheckVerifiedNoWouldUnmonitorItem_WritePassBlocked
// is the end-to-end pin, and it is the reviewer's exact scenario in
// miniature: the one verified sample is a SKIP item, every would-unmonitor
// item is unverifiable, the cross-check therefore still reads "passed" — and
// the write pass must nevertheless write nothing.
func TestRunRadarrDecisionEngine_CrossCheckVerifiedNoWouldUnmonitorItem_WritePassBlocked(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{1: monitoredMovieDetail(1, "Unverifiable Would-Unmonitor Movie")})

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{
		// Would-unmonitor (not in the wanted set, CF score above the
		// threshold) but its own movieFile.qualityCutoffNotMet is absent, so
		// the cross-check cannot verify the decision that would be written.
		{ID: intPtr(1), Title: strPtr("Unverifiable Would-Unmonitor Movie"), Monitored: boolPtr(true), HasFile: boolPtr(true),
			QualityProfileID: intPtr(1), Tags: &noTags, MovieFile: &movieFileElement{ID: intPtr(1)}},
		// A skip item that IS verifiable: in the wanted set, and its own
		// qualityCutoffNotMet agrees. This is the single verified sample that
		// used to be enough to open the gate for the movie above.
		{ID: intPtr(2), Title: strPtr("Verified Skip Movie"), Monitored: boolPtr(true), HasFile: boolPtr(true),
			QualityProfileID: intPtr(1), Tags: &noTags, MovieFile: &movieFileElement{ID: intPtr(1), QualityCutoffNotMet: boolPtr(true)}},
	}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{2: true}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), false)

	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("a cross-check that verified only a skip item must authorize no write, got %d: %+v", len(writes), writes)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "withheld") {
		t.Errorf("expected a warning explaining that writes were withheld:\n%s", out)
	}
	if !strings.Contains(out, "withheldWrites=1") {
		t.Errorf("expected the withheld warning to state how many writes it blocked:\n%s", out)
	}
	if !strings.Contains(out, "unmonitored=0") {
		t.Errorf("expected unmonitored=0:\n%s", out)
	}
	// The report is untouched: blocking writes never suppresses what the
	// human gate reads.
	if !strings.Contains(out, "msg=would-unmonitor") {
		t.Errorf("blocking writes must not suppress the report:\n%s", out)
	}
}

// TestRunCrossCheck_CountsTheWouldUnmonitorPoolSeparately is the unit under
// the fix: the aggregate counts say the sample was half verified, and only
// the per-pool counts reveal that the verified half was entirely skip items.
func TestRunCrossCheck_CountsTheWouldUnmonitorPoolSeparately(t *testing.T) {
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr"}

	decisions := []movieDecision{
		{id: 1, title: "Unverifiable Would-Unmonitor", wouldUnmonitor: true, hasFile: true, qualityCutoffNotMet: nil},
		{id: 2, title: "Verified Skip", wouldUnmonitor: false, hasFile: true, reason: ReasonQualityCutoffNotMet, qualityCutoffNotMet: agreeTrue()},
	}

	cc := runCrossCheck(logger, inst, decisions, map[int]bool{2: true})

	if cc.status != crossCheckStatusPassed {
		t.Fatalf("status = %q, want %q — this test is only meaningful while the aggregate verdict still reads as a pass", cc.status, crossCheckStatusPassed)
	}
	if cc.verified != 1 || cc.unverifiable != 1 {
		t.Errorf("aggregate verified/unverifiable = %d/%d, want 1/1", cc.verified, cc.unverifiable)
	}
	if cc.writeVerified != 0 || cc.writeUnverifiable != 1 {
		t.Errorf("would-unmonitor verified/unverifiable = %d/%d, want 0/1: a verified SKIP item is not evidence about a write",
			cc.writeVerified, cc.writeUnverifiable)
	}
}

// TestRunCrossCheck_MostlyUnverifiableSample_StillPassesButSaysSo is the
// reviewer's headline arithmetic, at the unit: 20 sampled items, 19 of them
// missing movieFile.qualityCutoffNotMet, one verified. The status is still
// "passed" (that is what "at least one verified, nothing disagreed" means,
// and changing the status vocabulary is not this fix), so the counts are the
// only thing standing between that verdict and ~146 authorized PUTs — which
// is why the gate now reads them.
func TestRunCrossCheck_MostlyUnverifiableSample_StillPassesButSaysSo(t *testing.T) {
	logger, _ := newDecisionTestLogger(slog.LevelWarn)
	inst := Instance{Name: "radarr-main", Type: "radarr"}

	var decisions []movieDecision
	// One verifiable would-unmonitor item, nine unverifiable ones.
	decisions = append(decisions, movieDecision{id: 1, title: "The One Verified Movie", wouldUnmonitor: true, hasFile: true, qualityCutoffNotMet: agreeFalse()})
	for i := 2; i <= 10; i++ {
		decisions = append(decisions, movieDecision{id: i, title: "Unverifiable WU", wouldUnmonitor: true, hasFile: true})
	}
	// Ten unverifiable skip items.
	for i := 101; i <= 110; i++ {
		decisions = append(decisions, movieDecision{id: i, title: "Unverifiable Skip", wouldUnmonitor: false, hasFile: true, reason: ReasonQualityCutoffNotMet})
	}

	cc := runCrossCheck(logger, inst, decisions, map[int]bool{})

	if cc.status != crossCheckStatusPassed {
		t.Fatalf("status = %q, want %q", cc.status, crossCheckStatusPassed)
	}
	if cc.verified != 1 || cc.unverifiable != 19 {
		t.Errorf("aggregate verified/unverifiable = %d/%d, want 1/19", cc.verified, cc.unverifiable)
	}
	if cc.writeVerified != 1 || cc.writeUnverifiable != 9 {
		t.Errorf("would-unmonitor verified/unverifiable = %d/%d, want 1/9", cc.writeVerified, cc.writeUnverifiable)
	}
	if reason := writeGateBlockReason(cc, 146); reason == "" {
		t.Errorf("the gate opened on a sample that verified 1 of 20 items; it must not")
	}
}

// --- the write gate reads the evidence, not just the verdict ----------------

// gateTestDecision is a single would-unmonitor decision for movie 1, matched
// by the fake's detail fixture below: everything about the pass is healthy
// except the cross-check evidence each case supplies, so a gate that let the
// case through would complete a real, recorded PUT rather than failing for
// some incidental reason.
func gateTestDecision() []movieDecision {
	return []movieDecision{{id: 1, title: "Gated Movie", wouldUnmonitor: true, reason: ReasonCutoffMet, profileName: "HD-1080p"}}
}

func TestRunWritePass_PassedButNoWouldUnmonitorItemVerified_BlocksAndNamesTheRatio(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{1: monitoredMovieDetail(1, "Gated Movie")})
	client := NewAPIClient(fake.instance().URL, fake.instance().APIKey)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	// The pass the old gate saw: "passed", one item verified. That item was
	// a skip.
	cc := crossCheckResult{status: crossCheckStatusPassed, verified: 1, unverifiable: 1, writeVerified: 0, writeUnverifiable: 1}
	unmonitored, writeErrors, echoUnverified, writesRefused, _ := runWritePass(context.Background(), logger, client, fake.instance(), gateTestDecision(), cc, 0, false, false)

	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("a pass that verified no would-unmonitor decision must authorize no write, got %+v", writes)
	}
	if unmonitored != 0 || writeErrors != 0 || echoUnverified != 0 || writesRefused != 0 {
		t.Errorf("a blocked pass wrote nothing, failed at nothing, and left nothing unconfirmed or refused; got unmonitored=%d writeErrors=%d echoUnverified=%d writesRefused=%d", unmonitored, writeErrors, echoUnverified, writesRefused)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "withheld") {
		t.Errorf("expected a warning explaining that writes were withheld:\n%s", out)
	}
	// The gate must publish the numbers it judged on. A warning that says
	// "the cross-check did not authorize this" without them leaves the human
	// unable to tell a data problem from a bug in this rule.
	for _, want := range []string{"wouldUnmonitorVerified=0", "wouldUnmonitorUnverifiable=1", "verified=1", "unverifiable=1", "withheldWrites=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected the withheld warning to state %q:\n%s", want, out)
		}
	}
}

func TestRunWritePass_MostWouldUnmonitorSamplesUnverifiable_Blocks(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{1: monitoredMovieDetail(1, "Gated Movie")})
	client := NewAPIClient(fake.instance().URL, fake.instance().APIKey)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	// One verified would-unmonitor item out of ten sampled: verification
	// happened, but not enough of it to speak for the pool.
	cc := crossCheckResult{status: crossCheckStatusPassed, verified: 1, unverifiable: 19, writeVerified: 1, writeUnverifiable: 9}
	runWritePass(context.Background(), logger, client, fake.instance(), gateTestDecision(), cc, 0, false, false)

	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("a sample where 9 of 10 would-unmonitor items were unverifiable must authorize no write, got %+v", writes)
	}
	if !strings.Contains(buf.String(), "withheld") {
		t.Errorf("expected a warning explaining that writes were withheld:\n%s", buf.String())
	}
}

// TestRunWritePass_MinorityUnverifiable_WritesButWarnsAtTheGate: verification
// that covers most of the pool is still permission to write — the fix is
// about evidence, not about refusing every imperfect sample — but the human
// hears about the gap at the moment writes happen, not only in a summary
// string read afterwards.
func TestRunWritePass_MinorityUnverifiable_WritesButWarnsAtTheGate(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{1: monitoredMovieDetail(1, "Gated Movie")})
	client := NewAPIClient(fake.instance().URL, fake.instance().APIKey)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	cc := crossCheckResult{status: crossCheckStatusPassed, verified: 9, unverifiable: 1, writeVerified: 9, writeUnverifiable: 1}
	unmonitored, writeErrors, echoUnverified, writesRefused, _ := runWritePass(context.Background(), logger, client, fake.instance(), gateTestDecision(), cc, 0, false, false)

	if n := len(fake.puts()); n != 1 {
		t.Fatalf("expected the write to proceed on a well-verified sample, got %d PUT(s)", n)
	}
	if unmonitored != 1 || writeErrors != 0 || echoUnverified != 0 || writesRefused != 0 {
		t.Errorf("unmonitored/writeErrors/echoUnverified/writesRefused = %d/%d/%d/%d, want 1/0/0/0", unmonitored, writeErrors, echoUnverified, writesRefused)
	}
	out := buf.String()
	if !strings.Contains(out, "partially verified") {
		t.Errorf("expected a gate-level warning that writes proceeded on a partially verified cross-check:\n%s", out)
	}
	if !strings.Contains(out, "unverifiable=1") || !strings.Contains(out, "verified=9") {
		t.Errorf("expected that warning to name the ratio:\n%s", out)
	}
	if strings.Contains(out, "rehearsal") {
		t.Errorf("write-mode wording must not claim a rehearsal:\n%s", out)
	}
}

// TestRunWritePass_MinorityUnverifiable_DryRun_WordingDoesNotClaimWritesHappened
// is the Phase 5 noise-budget fix (mandated): runWritePass walks the exact
// same code up to the dry-run gate in both modes, so the partially-verified
// warning fires in dry-run too — but "writes proceeding" is false in
// dry-run: nothing is ever sent. The wording must say so, not merely attach
// a dryRun=true attr a reader has to notice.
func TestRunWritePass_MinorityUnverifiable_DryRun_WordingDoesNotClaimWritesHappened(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{1: monitoredMovieDetail(1, "Gated Movie")})
	client := NewAPIClient(fake.instance().URL, fake.instance().APIKey)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	cc := crossCheckResult{status: crossCheckStatusPassed, verified: 9, unverifiable: 1, writeVerified: 9, writeUnverifiable: 1}
	unmonitored, writeErrors, echoUnverified, writesRefused, _ := runWritePass(context.Background(), logger, client, fake.instance(), gateTestDecision(), cc, 0, false, true)

	if n := len(fake.writes()); n != 0 {
		t.Fatalf("dry-run must make zero write requests, got %d: %+v", n, fake.writes())
	}
	if unmonitored != 0 || writeErrors != 0 || echoUnverified != 0 || writesRefused != 0 {
		t.Errorf("dry-run counters must all be zero, got %d/%d/%d/%d", unmonitored, writeErrors, echoUnverified, writesRefused)
	}
	out := buf.String()
	if strings.Contains(out, "writes proceeding") {
		t.Errorf("a dry-run partially-verified warning must not read as a write-mode event (\"writes proceeding\"):\n%s", out)
	}
	if !strings.Contains(out, "partially verified") {
		t.Errorf("expected the dry-run warning to still explain the sample was partially verified:\n%s", out)
	}
	if !strings.Contains(out, "unverifiable=1") || !strings.Contains(out, "verified=9") {
		t.Errorf("expected that warning to name the ratio:\n%s", out)
	}
	if !strings.Contains(out, "dryRun=true") {
		t.Errorf("expected the warning to carry dryRun=true:\n%s", out)
	}
}

// TestRunWritePass_FullyVerified_WritesWithNoPartialVerificationWarning is
// the noise guard on the warning above: a clean sample must not produce a
// warning at all, or the warning stops meaning anything.
func TestRunWritePass_FullyVerified_WritesWithNoPartialVerificationWarning(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{1: monitoredMovieDetail(1, "Gated Movie")})
	client := NewAPIClient(fake.instance().URL, fake.instance().APIKey)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	cc := crossCheckResult{status: crossCheckStatusPassed, verified: 4, unverifiable: 0, writeVerified: 4, writeUnverifiable: 0}
	runWritePass(context.Background(), logger, client, fake.instance(), gateTestDecision(), cc, 0, false, false)

	if n := len(fake.puts()); n != 1 {
		t.Fatalf("expected exactly 1 PUT on a fully verified sample, got %d", n)
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("a fully verified cross-check must produce no gate warning at all:\n%s", buf.String())
	}
}

// TestRunWritePass_NothingToWrite_UnverifiedPoolIsNotAnAlarm: with no
// would-unmonitor decisions the would-unmonitor pool was never sampled, so
// "nothing was verified about it" is trivially true and means nothing. A
// gate that warned here would cry wolf on every clean report-only run.
func TestRunWritePass_NothingToWrite_UnverifiedPoolIsNotAnAlarm(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{})
	client := NewAPIClient(fake.instance().URL, fake.instance().APIKey)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	decisions := []movieDecision{{id: 1, title: "Skipped Movie", wouldUnmonitor: false, reason: ReasonNoFile}}
	cc := crossCheckResult{status: crossCheckStatusPassed, verified: 3, unverifiable: 1, writeVerified: 0, writeUnverifiable: 0}
	unmonitored, writeErrors, echoUnverified, writesRefused, _ := runWritePass(context.Background(), logger, client, fake.instance(), decisions, cc, 0, false, false)

	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("a pass with no would-unmonitor decisions must write nothing, got %+v", writes)
	}
	if unmonitored != 0 || writeErrors != 0 || echoUnverified != 0 || writesRefused != 0 {
		t.Errorf("unmonitored/writeErrors/echoUnverified/writesRefused = %d/%d/%d/%d, want 0/0/0/0", unmonitored, writeErrors, echoUnverified, writesRefused)
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("nothing to write is not a reason to warn:\n%s", buf.String())
	}
}

// --- Phase 8: the scope is a SET, and it carries the report level ----------

// TestRunRadarrDecisionEngine_WebhookScope_CoalescedIDs_ReportsAndWritesEachOfThem
// is the mandated coalescing optimization at the engine level (binding
// controller note 2): when several debounce keys for the SAME instance expire
// together, ONE evaluation runs whose write scope is the union of their ids —
// not one full instance scan per event, and not a widening to the whole
// library either.
func TestRunRadarrDecisionEngine_WebhookScope_CoalescedIDs_ReportsAndWritesEachOfThem(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{
		1: monitoredMovieDetail(1, "First Import"),
		2: monitoredMovieDetail(2, "Untouched Movie"),
		3: monitoredMovieDetail(3, "Second Import"),
	})
	logger, buf := newDecisionTestLogger(slog.LevelDebug)
	movies := []movieListElement{
		wouldUnmonitorMovie(1, "First Import"),
		wouldUnmonitorMovie(2, "Untouched Movie"),
		wouldUnmonitorMovie(3, "Second Import"),
	}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", webhookScope([]int{3, 1}), false)

	out := buf.String()
	if !strings.Contains(out, `scopeIds=1,3`) {
		t.Errorf("the summary must name the coalesced write scope:\n%s", out)
	}
	if !strings.Contains(out, "totalMonitored=2") || !strings.Contains(out, "unmonitored=2") {
		t.Errorf("expected exactly the two named movies to be reported and written:\n%s", out)
	}
	var wrote []string
	for _, w := range fake.puts() {
		wrote = append(wrote, w.path)
	}
	for _, want := range []string{"/api/v3/movie/1", "/api/v3/movie/3"} {
		if !containsString(wrote, want) {
			t.Errorf("expected a PUT to %s, got %v", want, wrote)
		}
	}
	if containsString(wrote, "/api/v3/movie/2") {
		t.Errorf("movie 2 was never named by any webhook event and must not be written: %v", wrote)
	}
}

// TestRunRadarrDecisionEngine_WebhookScope_DemotesPerItemLinesButNotTheSummary
// is the daemon's idle-cycle noise budget (binding controller note 6) at the
// level it is enforced. The daemon repeats this cycle forever; per-item report
// lines are the point of a --once run and pure repetition on the hundredth
// sweep, so they drop to DEBUG while summaries, warnings and WRITE lines stay
// where a human can see them.
func TestRunRadarrDecisionEngine_WebhookScope_DemotesPerItemLinesButNotTheSummary(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{1: monitoredMovieDetail(1, "Imported Movie")})
	logger, buf := newDecisionTestLogger(slog.LevelInfo) // INFO: the demoted lines must vanish entirely
	movies := []movieListElement{
		wouldUnmonitorMovie(1, "Imported Movie"),
		skippedMovie(2, "Skipped Movie"),
	}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", webhookScope([]int{1}), false)

	out := buf.String()
	if strings.Contains(out, "msg=would-unmonitor") || strings.Contains(out, "msg=skip") {
		t.Errorf("per-item report lines must be demoted below INFO on a webhook cycle:\n%s", out)
	}
	if !strings.Contains(out, "msg=unmonitor ") {
		t.Errorf("an actual WRITE is never demoted — it is the one thing a daemon cycle must always announce:\n%s", out)
	}
	if !strings.Contains(out, "radarr decision summary") {
		t.Errorf("the summary is never demoted:\n%s", out)
	}
}

// TestRunRadarrDecisionEngine_ScopedIDMissing_NamesItButStillProcessesTheRest:
// one webhook event can name a movie that has since been deleted from the
// library while another names a real one. Refusing the whole cycle would let a
// stale event suppress real work; saying nothing would hide the stale event.
func TestRunRadarrDecisionEngine_ScopedIDMissing_NamesItButStillProcessesTheRest(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{1: monitoredMovieDetail(1, "Present Movie")})
	logger, buf := newDecisionTestLogger(slog.LevelDebug)
	movies := []movieListElement{wouldUnmonitorMovie(1, "Present Movie")}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", webhookScope([]int{1, 4242}), false)

	out := buf.String()
	if !strings.Contains(out, "movieId=4242") {
		t.Errorf("the id that names nothing must be reported:\n%s", out)
	}
	if !strings.Contains(out, "unmonitored=1") {
		t.Errorf("the id that names something real must still be acted on:\n%s", out)
	}
}

// containsString is a local helper: the suite has no generic slice-contains
// and containsIntID (shared.go) is deliberately int-only.
func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// TestRunRadarrDecisionEngine_ShutdownMidEvaluation_AbandonsTheCycleWithoutWriting
// is the other half of the Phase 8 shutdown boundary. The write pass withholds
// and accounts for what it did not do; an interrupted EVALUATION cannot do even
// that, because its picture of the library is a fraction of one — so the cycle
// is abandoned before the cross-check ever samples that truncated pool, and no
// summary is printed that would describe part of a library as if it were the
// whole thing.
func TestRunRadarrDecisionEngine_ShutdownMidEvaluation_AbandonsTheCycleWithoutWriting(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{
		wouldUnmonitorStatefulMovie(1, "First"),
		wouldUnmonitorStatefulMovie(2, "Second"),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var once sync.Once
	fake.onRequest = func(method, path string) {
		if method == http.MethodGet && path == "/api/v3/moviefile" {
			once.Do(cancel) // partway through the FIRST movie's evaluation
		}
	}
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{
		wouldUnmonitorMovie(1, "First"),
		wouldUnmonitorMovie(2, "Second"),
	}

	runRadarrDecisionEngine(ctx, logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), false)

	out := buf.String()
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("a partial evaluation must never reach the write pass, got %+v", writes)
	}
	if strings.Contains(out, "radarr decision summary") {
		t.Errorf("no summary may be printed for a cycle that saw part of the library:\n%s", out)
	}
	if !strings.Contains(out, "abandoning this instance's cycle mid-evaluation") {
		t.Errorf("the abandonment must be stated, or the silence reads as a clean no-op cycle:\n%s", out)
	}
}

// TestRunRadarrDecisionEngine_AlreadyUnmonitoredMovieWithoutAnID_IsStillCounted
// is a self-review regression pin from the Phase 8 scope refactor. The
// already-unmonitored branch used to read `onlyID == 0 || (m.ID != nil && *m.ID
// == onlyID)`, and became `m.ID != nil && scope.contains(*m.ID)` — which is the
// same thing for a scoped run and NOT the same thing for an unscoped one: a
// movie with monitored:false and no id silently stopped being counted at all.
//
// It is a corner (real Radarr always sends id) but it is exactly the corner
// this project refuses to let vanish: the summary's alreadyUnmonitored is one
// of the numbers a human reads to decide a no-op cycle really was a no-op.
func TestRunRadarrDecisionEngine_AlreadyUnmonitoredMovieWithoutAnID_IsStillCounted(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{})
	logger, buf := newDecisionTestLogger(slog.LevelDebug)
	movies := []movieListElement{
		{Title: strPtr("No Id, Not Monitored"), Monitored: boolPtr(false), HasFile: boolPtr(true), QualityProfileID: intPtr(1), Tags: &noTags},
	}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

	out := buf.String()
	if !strings.Contains(out, "alreadyUnmonitored=1") {
		t.Errorf("an unscoped run counts every already-unmonitored movie, id or no id:\n%s", out)
	}
	if !strings.Contains(out, `msg="already unmonitored"`) || !strings.Contains(out, "id=absent") {
		t.Errorf("and says so at debug, naming what it could not name:\n%s", out)
	}
}

// TestRunRadarrDecisionEngine_ScopedRun_AlreadyUnmonitoredMovieWithoutAnID_IsNotCounted
// is the other side: a movie whose id was never observed cannot be shown to be
// the one the scope names, so a scoped run must not count it. Together the two
// tests pin the exact expression, which is what makes it safe to simplify later.
func TestRunRadarrDecisionEngine_ScopedRun_AlreadyUnmonitoredMovieWithoutAnID_IsNotCounted(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{2: monitoredMovieDetail(2, "Target")})
	logger, buf := newDecisionTestLogger(slog.LevelDebug)
	movies := []movieListElement{
		{Title: strPtr("No Id, Not Monitored"), Monitored: boolPtr(false), HasFile: boolPtr(true), QualityProfileID: intPtr(1), Tags: &noTags},
		wouldUnmonitorMovie(2, "Target"),
	}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", onlyIDScope(2), true)

	if out := buf.String(); !strings.Contains(out, "alreadyUnmonitored=0") {
		t.Errorf("a scoped run counts only the named movie; an unidentifiable one is not it:\n%s", out)
	}
}
