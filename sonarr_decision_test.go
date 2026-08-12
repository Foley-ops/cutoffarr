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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- fixtures --------------------------------------------------------------

const sonarrDecisionProfilesJSON = `[{"id": 1, "name": "HD-1080p", "upgradeAllowed": true, "cutoffFormatScore": 100}]`
const sonarrDecisionNoTagsJSON = `[]`

var sonarrDecisionTestProfiles = map[int]qualityProfile{
	1: {Name: "HD-1080p", CutoffFormatScore: 100, UpgradeAllowed: true},
	2: {Name: "Locked-Profile", CutoffFormatScore: 100, UpgradeAllowed: false},
}

// pastAirDate / futureAirDate are safely distant from any real test-run
// time (rule 3 resolves episodeAiringStatus against time.Now()), so no clock injection
// is needed: 2015 is always in the past and 2099 is always in the future for
// any foreseeable run of this suite.
const pastAirDate = "2015-01-01T00:00:00Z"
const futureAirDate = "2099-01-01T00:00:00Z"

func testSeason(number int, monitored bool, fileCount, totalCount int) seriesSeasonElement {
	return seriesSeasonElement{
		SeasonNumber: intPtr(number),
		Monitored:    boolPtr(monitored),
		Statistics:   &seasonStatisticsElement{EpisodeFileCount: intPtr(fileCount), TotalEpisodeCount: intPtr(totalCount)},
	}
}

func testSeries(id int, title string, monitored bool, profileID int, tags []int, seasons ...seriesSeasonElement) seriesElement {
	s := seriesElement{
		ID: intPtr(id), Title: strPtr(title), Monitored: boolPtr(monitored),
		QualityProfileID: intPtr(profileID), Seasons: &seasons,
	}
	if tags != nil {
		s.Tags = &tags
	}
	return s
}

// sonarrEpisodeFileServer serves episodeJSON at /api/v3/episode and
// episodefileJSON at /api/v3/episodefile, recording every request's raw
// query string into the two pointer slices — the request-count assertions
// the laziness contract (binding evaluation-order resolution) depends on.
func sonarrEpisodeFileServer(t *testing.T, episodeJSON, episodefileJSON string, gotEpisodeRequests, gotEpisodefileRequests *[]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/episode", func(w http.ResponseWriter, r *http.Request) {
		if gotEpisodeRequests != nil {
			*gotEpisodeRequests = append(*gotEpisodeRequests, r.URL.RawQuery)
		}
		w.Write([]byte(episodeJSON))
	})
	mux.HandleFunc("/api/v3/episodefile", func(w http.ResponseWriter, r *http.Request) {
		if gotEpisodefileRequests != nil {
			*gotEpisodefileRequests = append(*gotEpisodefileRequests, r.URL.RawQuery)
		}
		w.Write([]byte(episodefileJSON))
	})
	return httptest.NewServer(mux)
}

// episodeJSON renders one /api/v3/episode array element.
func episodeJSON(id, season, episodeNum int, airDate string, episodeFileID int) string {
	airDateJSON := "null"
	if airDate != "" {
		airDateJSON = strconv.Quote(airDate)
	}
	return fmt.Sprintf(`{"id": %d, "seasonNumber": %d, "episodeNumber": %d, "monitored": true, "hasFile": true, "airDateUtc": %s, "episodeFileId": %d}`,
		id, season, episodeNum, airDateJSON, episodeFileID)
}

// stampSeriesID adds "seriesId": seriesID to every element of an /api/v3/episode
// fixture that does not already state one, leaving an element that DOES state
// one exactly as written (so a test can still hand back foreign or absent
// provenance deliberately, by spelling a different id — an absent one has to be
// built without this helper).
//
// It exists because episodeJSON's fixtures are shared across a fake's whole
// library while the field is per-series: baking an id into the text would be
// wrong for every series but one, and omitting it entirely made the write path
// refuse (Phase 8: an episode of the target season must prove it belongs to the
// series before its id can be named in a write).
func stampSeriesID(t *testing.T, episodesJSON string, seriesID int) string {
	t.Helper()
	var elems []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(episodesJSON), &elems); err != nil {
		t.Fatalf("episode fixture is not a JSON array of objects: %v\n%s", err, episodesJSON)
	}
	for _, e := range elems {
		if _, present := e["seriesId"]; !present {
			e["seriesId"] = json.RawMessage(strconv.Itoa(seriesID))
		}
	}
	out, err := json.Marshal(elems)
	if err != nil {
		t.Fatalf("re-encoding the episode fixture: %v", err)
	}
	return string(out)
}

// episodeFileJSON renders one /api/v3/episodefile array element.
func episodeFileJSON(id, season, cfScore int, qualityCutoffNotMet bool) string {
	return fmt.Sprintf(`{"id": %d, "seasonNumber": %d, "customFormatScore": %d, "qualityCutoffNotMet": %t}`, id, season, cfScore, qualityCutoffNotMet)
}

// --- evaluateSeries: series-level checks ------------------------------------

func TestEvaluateSeries_SeriesLevelTagExcluded_AllMonitoredSeasonsSkipped(t *testing.T) {
	var gotEp, gotEf []string
	srv := sonarrEpisodeFileServer(t, `[]`, `[]`, &gotEp, &gotEf)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Excluded Show", true, 1, []int{9}, testSeason(1, true, 10, 10), testSeason(2, true, 5, 5))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, true, map[seasonKey]bool{})

	// The binding evaluation-order resolution's whole point: a series-level
	// failure costs ZERO per-series fetches. The /episodefile half was already
	// pinned here; the /episode half was not, so a hoisted or eagerly-issued
	// episode fetch would have multiplied this project's API call count
	// against a large library with nothing in the suite noticing.
	if len(gotEp) != 0 {
		t.Errorf("expected zero /episode requests for a series excluded at the series level, got %v", gotEp)
	}
	if len(gotEf) != 0 {
		t.Errorf("expected zero /episodefile requests, got %v", gotEf)
	}
	if len(eval.decisions) != 2 {
		t.Fatalf("expected 2 season decisions, got %d", len(eval.decisions))
	}
	for _, d := range eval.decisions {
		if d.reason != ReasonExcludedByTag {
			t.Errorf("season %d reason = %q, want %q", d.season, d.reason, ReasonExcludedByTag)
		}
		if d.wouldUnmonitor {
			t.Errorf("season %d wouldUnmonitor = true, want false", d.season)
		}
	}
}

func TestEvaluateSeries_SeriesLevelTagsAbsent_TagActive_SkippedWithReason(t *testing.T) {
	srv := sonarrEpisodeFileServer(t, `[]`, `[]`, nil, nil)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Untagged Show", true, 1, nil, testSeason(1, true, 10, 10))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, true, map[seasonKey]bool{})

	if len(eval.decisions) != 1 || eval.decisions[0].reason != ReasonTagsUnknown {
		t.Fatalf("decisions = %+v, want one with reason %q", eval.decisions, ReasonTagsUnknown)
	}
}

func TestEvaluateSeries_TagsAbsent_TagNotActive_ProceedsNormally(t *testing.T) {
	var gotEp, gotEf []string
	srv := sonarrEpisodeFileServer(t,
		"["+episodeJSON(100, 1, 1, pastAirDate, 500)+"]",
		"["+episodeFileJSON(500, 1, 200, false)+"]",
		&gotEp, &gotEf)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Untagged Show", true, 1, nil, testSeason(1, true, 1, 1))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if len(eval.decisions) != 1 || !eval.decisions[0].wouldUnmonitor {
		t.Fatalf("decisions = %+v, want a single would-unmonitor decision (tag inactive, so absent tags is vacuous)", eval.decisions)
	}
}

func TestEvaluateSeries_UnknownProfile_SkippedWithReason(t *testing.T) {
	srv := sonarrEpisodeFileServer(t, `[]`, `[]`, nil, nil)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Unknown Profile Show", true, 999, []int{}, testSeason(1, true, 10, 10))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if len(eval.decisions) != 1 || eval.decisions[0].reason != ReasonUnknownProfile || eval.decisions[0].profileName != "unknown" {
		t.Fatalf("decisions = %+v, want reason %q profileName \"unknown\"", eval.decisions, ReasonUnknownProfile)
	}
}

func TestEvaluateSeries_UpgradesDisabled_SkippedWithReason(t *testing.T) {
	srv := sonarrEpisodeFileServer(t, `[]`, `[]`, nil, nil)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Locked Show", true, 2, []int{}, testSeason(1, true, 10, 10))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if len(eval.decisions) != 1 || eval.decisions[0].reason != ReasonUpgradesDisabled {
		t.Fatalf("decisions = %+v, want reason %q", eval.decisions, ReasonUpgradesDisabled)
	}
}

// TestEvaluateSeries_TagAndProfileFail_ExcludedByTagWinsOverProfile pins the
// binding evaluation order: tags are checked before profile existence /
// upgradeAllowed, so a series that fails both is reported for the tag
// reason, never the profile one.
func TestEvaluateSeries_TagAndProfileFail_ExcludedByTagWinsOverProfile(t *testing.T) {
	srv := sonarrEpisodeFileServer(t, `[]`, `[]`, nil, nil)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Both Fail Show", true, 999, []int{9}, testSeason(1, true, 10, 10))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, true, map[seasonKey]bool{})

	if len(eval.decisions) != 1 || eval.decisions[0].reason != ReasonExcludedByTag {
		t.Fatalf("decisions = %+v, want reason %q (tags checked before profile)", eval.decisions, ReasonExcludedByTag)
	}
}

// --- evaluateSeries: season monitored / zero-monitored-seasons -------------

func TestEvaluateSeries_SeasonMonitoredFalse_ExcludedAndCounted(t *testing.T) {
	srv := sonarrEpisodeFileServer(t,
		"["+episodeJSON(100, 1, 1, pastAirDate, 500)+"]",
		"["+episodeFileJSON(500, 1, 200, false)+"]",
		nil, nil)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Mixed Show", true, 1, []int{}, testSeason(1, true, 1, 1), testSeason(2, false, 0, 10))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if eval.alreadyUnmonitored != 1 {
		t.Errorf("alreadyUnmonitored = %d, want 1", eval.alreadyUnmonitored)
	}
	if len(eval.decisions) != 1 {
		t.Fatalf("expected 1 season decision (season 2 excluded), got %d: %+v", len(eval.decisions), eval.decisions)
	}
	if eval.decisions[0].season != 1 {
		t.Errorf("expected the surviving decision to be for season 1, got season %d", eval.decisions[0].season)
	}
}

func TestEvaluateSeries_SeasonMonitoredAbsent_ExcludedNotCountedWarns(t *testing.T) {
	srv := sonarrEpisodeFileServer(t, `[]`, `[]`, nil, nil)
	defer srv.Close()
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	untrusted := seriesSeasonElement{SeasonNumber: intPtr(1), Statistics: &seasonStatisticsElement{EpisodeFileCount: intPtr(1), TotalEpisodeCount: intPtr(1)}} // Monitored absent
	s := testSeries(1, "Untrusted Season Show", true, 1, []int{}, untrusted)
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if eval.alreadyUnmonitored != 0 {
		t.Errorf("alreadyUnmonitored = %d, want 0 (absent is untrusted, not a counted state)", eval.alreadyUnmonitored)
	}
	if len(eval.decisions) != 0 {
		t.Errorf("expected zero decisions (series has no monitored seasons), got %+v", eval.decisions)
	}
	// The generic warnIfFieldAbsent line ("field=monitored") was replaced by
	// an explicit, traceable warn naming the series — see
	// TestEvaluateSeries_SeasonMonitoredAbsent_WarnNamesTheSeries.
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "skipping season: missing monitored field") {
		t.Errorf("expected a warning about the absent season monitored field:\n%s", buf.String())
	}
}

func TestEvaluateSeries_ZeroMonitoredSeasons_SingleDebugLineNoPerSeasonSpam(t *testing.T) {
	srv := sonarrEpisodeFileServer(t, `[]`, `[]`, nil, nil)
	defer srv.Close()
	logger, buf := newDecisionTestLogger(slog.LevelDebug) // slog.LevelDebug
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Fully Unmonitored Show", true, 1, []int{},
		testSeason(1, false, 10, 10), testSeason(2, false, 10, 10), testSeason(3, false, 10, 10))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if eval.alreadyUnmonitored != 3 {
		t.Errorf("alreadyUnmonitored = %d, want 3 (still counted even though not individually logged)", eval.alreadyUnmonitored)
	}
	if len(eval.decisions) != 0 {
		t.Errorf("expected zero decisions, got %+v", eval.decisions)
	}
	out := buf.String()
	if strings.Count(out, `msg="season already unmonitored"`) != 0 {
		t.Errorf("expected NO per-season debug spam when the whole series has zero monitored seasons:\n%s", out)
	}
	if !strings.Contains(out, `msg="series has no monitored seasons"`) {
		t.Errorf("expected a single bulk debug line naming the series:\n%s", out)
	}
	if !strings.Contains(out, "seasons=3") {
		t.Errorf("expected the bulk debug line to state the count:\n%s", out)
	}
}

// TestEvaluateSeries_SeasonsFieldAbsent_WarnsAndProducesNoDecisions is the
// IMPORTANT review fix's regression proof (PROBE C): a monitored series
// whose "seasons" key is entirely absent from the response must not
// silently vanish into zero decisions and zero log lines of any level —
// Seasons is now a pointer (sonarr.go) specifically so this can be told
// apart from a genuinely empty list, and warned on twice over: once via
// warnIfFieldAbsent (the field-name-drift signal every other load-bearing
// field gets) and once via the "produced no season decisions" warn (the
// series-level "this monitored series did nothing" signal).
func TestEvaluateSeries_SeasonsFieldAbsent_WarnsAndProducesNoDecisions(t *testing.T) {
	srv := sonarrEpisodeFileServer(t, `[]`, `[]`, nil, nil)
	defer srv.Close()
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := seriesElement{
		ID: intPtr(1), Title: strPtr("No Seasons Key Show"), Monitored: boolPtr(true),
		QualityProfileID: intPtr(1), Tags: &[]int{},
		// Seasons deliberately left nil: the "seasons" key never decoded.
	}
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if len(eval.decisions) != 0 {
		t.Errorf("expected zero decisions, got %+v", eval.decisions)
	}
	if eval.alreadyUnmonitored != 0 {
		t.Errorf("alreadyUnmonitored = %d, want 0", eval.alreadyUnmonitored)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("expected at least one warning; a monitored series must never vanish silently:\n%s", out)
	}
	if !strings.Contains(out, `field=seasons`) {
		t.Errorf("expected a field-absent warning naming \"seasons\":\n%s", out)
	}
	if !strings.Contains(out, "no season decisions") {
		t.Errorf("expected a warning stating the series produced no season decisions:\n%s", out)
	}
}

// --- evaluateSeries: rule 2 (complete on disk) ------------------------------

func TestEvaluateSeries_IncompleteOnDisk_SkippedWithReason(t *testing.T) {
	srv := sonarrEpisodeFileServer(t, `[]`, `[]`, nil, nil)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Incomplete Show", true, 1, []int{}, testSeason(1, true, 5, 10))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if len(eval.decisions) != 1 || eval.decisions[0].reason != ReasonSeasonIncomplete {
		t.Fatalf("decisions = %+v, want reason %q", eval.decisions, ReasonSeasonIncomplete)
	}
	if eval.decisions[0].completeOnDisk {
		t.Error("completeOnDisk = true, want false: this season failed rule 2")
	}
}

func TestEvaluateSeries_ZeroTotalEpisodeCount_SkippedIncomplete(t *testing.T) {
	srv := sonarrEpisodeFileServer(t, `[]`, `[]`, nil, nil)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	// 0 == 0 would satisfy a naive equality check; rule 2 also requires
	// totalEpisodeCount > 0.
	s := testSeries(1, "Empty Show", true, 1, []int{}, testSeason(1, true, 0, 0))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if len(eval.decisions) != 1 || eval.decisions[0].reason != ReasonSeasonIncomplete {
		t.Fatalf("decisions = %+v, want reason %q (totalEpisodeCount must be > 0)", eval.decisions, ReasonSeasonIncomplete)
	}
}

func TestEvaluateSeries_StatisticsMissing_SkippedAsInconsistent(t *testing.T) {
	srv := sonarrEpisodeFileServer(t, `[]`, `[]`, nil, nil)
	defer srv.Close()
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	noStats := seriesSeasonElement{SeasonNumber: intPtr(1), Monitored: boolPtr(true)} // Statistics absent
	s := testSeries(1, "No Stats Show", true, 1, []int{}, noStats)
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if len(eval.decisions) != 1 || eval.decisions[0].reason != ReasonSeasonEpisodeDataInconsistent {
		t.Fatalf("decisions = %+v, want reason %q", eval.decisions, ReasonSeasonEpisodeDataInconsistent)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning:\n%s", buf.String())
	}
}

func TestEvaluateSeries_StatsClaimCompleteButNoEpisodesReturned_SkippedAsInconsistent(t *testing.T) {
	// /episode returns nothing for season 1, but statistics claim it is complete.
	srv := sonarrEpisodeFileServer(t, `[]`, `[]`, nil, nil)
	defer srv.Close()
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Inconsistent Show", true, 1, []int{}, testSeason(1, true, 10, 10))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if len(eval.decisions) != 1 || eval.decisions[0].reason != ReasonSeasonEpisodeDataInconsistent {
		t.Fatalf("decisions = %+v, want reason %q", eval.decisions, ReasonSeasonEpisodeDataInconsistent)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning:\n%s", buf.String())
	}
}

// TestEvaluateSeries_PartialEpisodeList_ShortOfStatistics_SkippedAsInconsistent
// is the CRITICAL review fix's own regression proof (the reviewer's PROBE):
// statistics claim episodeFileCount=totalEpisodeCount=3 (rule 2 passes),
// but /episode returns only the 2 already-aired episodes — the third,
// still-to-air one is silently absent. Before the fix, the airing guard ran
// over only the 2 episodes it was given, found them both aired, and the
// season reached rule 7 and became would-unmonitor while genuinely still
// airing — the exact outcome the airing guard exists to prevent. The
// episode files (all in-threshold) are supplied to prove the season would
// otherwise have sailed through to would-unmonitor were it not caught here.
func TestEvaluateSeries_PartialEpisodeList_ShortOfStatistics_SkippedAsInconsistent(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "," + episodeJSON(101, 1, 2, pastAirDate, 501) + "]" // only 2 of 3
	filesJSON := "[" + episodeFileJSON(500, 1, 200, false) + "," + episodeFileJSON(501, 1, 200, false) + "," + episodeFileJSON(502, 1, 200, false) + "]"
	srv := sonarrEpisodeFileServer(t, episodesJSON, filesJSON, nil, nil)
	defer srv.Close()
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Partial Episode List Show", true, 1, []int{}, testSeason(1, true, 3, 3))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if len(eval.decisions) != 1 {
		t.Fatalf("expected 1 decision, got %d: %+v", len(eval.decisions), eval.decisions)
	}
	d := eval.decisions[0]
	if d.wouldUnmonitor {
		t.Fatalf("wouldUnmonitor = true, want false: /episode returned fewer episodes than statistics.totalEpisodeCount claims, so airing status for the missing one is unknown — must never be guessed at as would-unmonitor")
	}
	if d.reason != ReasonSeasonEpisodeDataInconsistent {
		t.Errorf("reason = %q, want %q", d.reason, ReasonSeasonEpisodeDataInconsistent)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning:\n%s", buf.String())
	}
}

// TestEvaluateSeries_UnairedEpisodeDroppedForMissingSeasonNumber_SkippedAsInconsistent
// is the CRITICAL review fix's second regression proof (PROBE B): a season
// with 2 total episodes where one episode IS present in /episode but is
// missing its own seasonNumber (already dropped, with its own separate
// warning, before this season's episode list is even built) and is the
// season's only NOT-yet-aired episode. Before the fix, the dropped episode
// left seasonEpisodes with only 1 (aired) entry, the airing guard passed on
// that partial view, and the season reached would-unmonitor despite
// genuinely having an unaired episode. The episode-count-vs-statistics
// guard (added by the same fix) catches this too, since it is a general
// completeness check, not merely a length-zero special case.
func TestEvaluateSeries_UnairedEpisodeDroppedForMissingSeasonNumber_SkippedAsInconsistent(t *testing.T) {
	// Episode 101 has NO seasonNumber field at all — dropped during
	// episodesBySeason construction, before rule 3 ever sees it. Its
	// airDateUtc is set to a future date to prove that, if it HAD been
	// counted, the airing guard would have caught it a different way; the
	// point of this test is that dropping it must not silently remove that
	// protection.
	episodesJSON := `[` + episodeJSON(100, 1, 1, pastAirDate, 500) +
		`,{"id": 101, "episodeNumber": 2, "monitored": true, "hasFile": true, "airDateUtc": "` + futureAirDate + `", "episodeFileId": 501}]`
	filesJSON := "[" + episodeFileJSON(500, 1, 200, false) + "," + episodeFileJSON(501, 1, 200, false) + "]"
	srv := sonarrEpisodeFileServer(t, episodesJSON, filesJSON, nil, nil)
	defer srv.Close()
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Dropped Episode Show", true, 1, []int{}, testSeason(1, true, 2, 2))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if len(eval.decisions) != 1 {
		t.Fatalf("expected 1 decision, got %d: %+v", len(eval.decisions), eval.decisions)
	}
	d := eval.decisions[0]
	if d.wouldUnmonitor {
		t.Fatalf("wouldUnmonitor = true, want false: the season's only unaired episode was dropped for missing seasonNumber, leaving a short (and therefore untrusted) episode list")
	}
	if d.reason != ReasonSeasonEpisodeDataInconsistent {
		t.Errorf("reason = %q, want %q", d.reason, ReasonSeasonEpisodeDataInconsistent)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning:\n%s", buf.String())
	}
}

// --- evaluateSeries: rule 3 (airing guard) — the crown jewel ---------------

// TestEvaluateSeries_AiringSeason_NeverWouldUnmonitor_EvenWhenCompleteAndCutoffMet
// is the single most important test in this file: a season with one future-
// dated episode must NEVER be reported would-unmonitor, even though every
// other rule (complete on disk, not in wanted set, CF score comfortably
// above threshold) would otherwise pass cleanly. Unmonitoring an airing
// season would stop its future episodes from ever being grabbed.
func TestEvaluateSeries_AiringSeason_NeverWouldUnmonitor_EvenWhenCompleteAndCutoffMet(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "," + episodeJSON(101, 1, 2, futureAirDate, 501) + "]"
	filesJSON := "[" + episodeFileJSON(500, 1, 200, false) + "," + episodeFileJSON(501, 1, 200, false) + "]"
	srv := sonarrEpisodeFileServer(t, episodesJSON, filesJSON, nil, nil)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Airing Show", true, 1, []int{}, testSeason(1, true, 2, 2))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if len(eval.decisions) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(eval.decisions))
	}
	d := eval.decisions[0]
	if d.wouldUnmonitor {
		t.Fatalf("wouldUnmonitor = true, want false: this season has a future-dated episode and must never be a candidate for unmonitoring")
	}
	if d.reason != ReasonSeasonNotFullyAired {
		t.Errorf("reason = %q, want %q", d.reason, ReasonSeasonNotFullyAired)
	}
}

// TestEvaluateSeries_AbsentAirDateUtc_TreatedAsNotAired also pins the
// IMPORTANT review fix: an absent airDateUtc is an untrusted shape, not a
// healthy "still airing" skip, and must WARN — distinguishably, in the log
// stream, from TestEvaluateSeries_AiringSeason_NeverWouldUnmonitor...'s
// genuinely future-dated episode, which must NOT warn.
func TestEvaluateSeries_AbsentAirDateUtc_TreatedAsNotAired(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, "", 500) + "]" // airDateUtc absent (null)
	srv := sonarrEpisodeFileServer(t, episodesJSON, "[]", nil, nil)
	defer srv.Close()
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Undated Show", true, 1, []int{}, testSeason(1, true, 1, 1))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if len(eval.decisions) != 1 || eval.decisions[0].reason != ReasonSeasonNotFullyAired {
		t.Fatalf("decisions = %+v, want reason %q for an absent airDateUtc", eval.decisions, ReasonSeasonNotFullyAired)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning distinguishing an absent (untrusted) airDateUtc from a genuinely airing season:\n%s", buf.String())
	}
}

// TestEvaluateSeries_UnparseableAirDateUtc_TreatedAsNotAired also pins the
// IMPORTANT review fix: same rationale as the absent-airDateUtc test above,
// for the "present but not a valid RFC3339 timestamp" untrusted shape.
func TestEvaluateSeries_UnparseableAirDateUtc_TreatedAsNotAired(t *testing.T) {
	episodesJSON := `[{"id": 100, "seasonNumber": 1, "episodeNumber": 1, "monitored": true, "hasFile": true, "airDateUtc": "not-a-date", "episodeFileId": 500}]`
	srv := sonarrEpisodeFileServer(t, episodesJSON, "[]", nil, nil)
	defer srv.Close()
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Malformed Date Show", true, 1, []int{}, testSeason(1, true, 1, 1))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if len(eval.decisions) != 1 || eval.decisions[0].reason != ReasonSeasonNotFullyAired {
		t.Fatalf("decisions = %+v, want reason %q for an unparseable airDateUtc (fail safe)", eval.decisions, ReasonSeasonNotFullyAired)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning distinguishing an unparseable (untrusted) airDateUtc from a genuinely airing season:\n%s", buf.String())
	}
}

// TestEvaluateSeries_AiringSeason_ValidFutureDate_NoWarnLogged is the
// negative half of the IMPORTANT review fix: a genuinely future-dated,
// well-formed airDateUtc is a healthy, expected "still airing" skip, not a
// data problem — it must NOT produce a WARN, or the fix above would just
// make every airing season noisy instead of fixing the actual gap.
func TestEvaluateSeries_AiringSeason_ValidFutureDate_NoWarnLogged(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, futureAirDate, 500) + "]"
	srv := sonarrEpisodeFileServer(t, episodesJSON, "[]", nil, nil)
	defer srv.Close()
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Genuinely Airing Show", true, 1, []int{}, testSeason(1, true, 1, 1))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if len(eval.decisions) != 1 || eval.decisions[0].reason != ReasonSeasonNotFullyAired {
		t.Fatalf("decisions = %+v, want reason %q for a future-dated episode", eval.decisions, ReasonSeasonNotFullyAired)
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("a genuinely future-dated (valid, parseable) airDateUtc must never warn:\n%s", buf.String())
	}
}

// --- evaluateSeries: rule 4 (wanted set) + precedence pin -------------------

func TestEvaluateSeries_InWantedSet_SkippedWithReason(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "]"
	srv := sonarrEpisodeFileServer(t, episodesJSON, "[]", nil, nil)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Wanted Show", true, 1, []int{}, testSeason(1, true, 1, 1))
	wantedSeasons := map[seasonKey]bool{{seriesID: 1, seasonNumber: 1}: true}
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, wantedSeasons)

	if len(eval.decisions) != 1 || eval.decisions[0].reason != ReasonQualityCutoffNotMet {
		t.Fatalf("decisions = %+v, want reason %q", eval.decisions, ReasonQualityCutoffNotMet)
	}
	if !eval.decisions[0].completeOnDisk {
		t.Error("completeOnDisk = false, want true: this season passed rule 2 before failing rule 4")
	}
}

// TestEvaluateSeries_AiringAndInWantedSet_AiringReasonWins pins the binding
// precedence pin explicitly: a season that fails BOTH the airing guard and
// the wanted-set check is reported with the airing reason, since rule 3 is
// checked before rule 4.
func TestEvaluateSeries_AiringAndInWantedSet_AiringReasonWins(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, futureAirDate, 500) + "]"
	srv := sonarrEpisodeFileServer(t, episodesJSON, "[]", nil, nil)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Airing And Wanted Show", true, 1, []int{}, testSeason(1, true, 1, 1))
	wantedSeasons := map[seasonKey]bool{{seriesID: 1, seasonNumber: 1}: true}
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, wantedSeasons)

	if len(eval.decisions) != 1 || eval.decisions[0].reason != ReasonSeasonNotFullyAired {
		t.Fatalf("decisions = %+v, want reason %q (airing checked before wanted-set)", eval.decisions, ReasonSeasonNotFullyAired)
	}
}

// --- evaluateSeries: /episode fetch failure ---------------------------------

func TestEvaluateSeries_EpisodeFetchFailure_AllMonitoredSeasonsSkipped(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/episode", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-broken", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Broken Episode Fetch Show", true, 1, []int{}, testSeason(1, true, 10, 10), testSeason(2, true, 5, 5))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if len(eval.decisions) != 2 {
		t.Fatalf("expected 2 decisions (per-series failure, not instance-fatal), got %d", len(eval.decisions))
	}
	for _, d := range eval.decisions {
		if d.reason != ReasonSeasonEpisodesUnavailable {
			t.Errorf("season %d reason = %q, want %q", d.season, d.reason, ReasonSeasonEpisodesUnavailable)
		}
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning:\n%s", buf.String())
	}
}

// --- evaluateSeries: rule 7 (STRICT) + laziness -----------------------------

func TestEvaluateSeries_AllRulesPass_WouldUnmonitor(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "]"
	filesJSON := "[" + episodeFileJSON(500, 1, 200, false) + "]"
	srv := sonarrEpisodeFileServer(t, episodesJSON, filesJSON, nil, nil)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Complete Show", true, 1, []int{}, testSeason(1, true, 1, 1))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if len(eval.decisions) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(eval.decisions))
	}
	d := eval.decisions[0]
	if !d.wouldUnmonitor || d.reason != ReasonCutoffMet {
		t.Fatalf("decision = %+v, want wouldUnmonitor=true reason=%q", d, ReasonCutoffMet)
	}
	if d.profileName != "HD-1080p" {
		t.Errorf("profileName = %q, want HD-1080p", d.profileName)
	}
	if len(d.crossCheckEpisodes) != 1 || d.crossCheckEpisodes[0].episodeID != 100 {
		t.Errorf("crossCheckEpisodes = %+v, want one entry for episode 100", d.crossCheckEpisodes)
	}
}

func TestEvaluateSeries_CFBelowThreshold_SkippedWithReason(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "]"
	filesJSON := "[" + episodeFileJSON(500, 1, 50, false) + "]" // 50 < threshold 100
	srv := sonarrEpisodeFileServer(t, episodesJSON, filesJSON, nil, nil)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Low Score Show", true, 1, []int{}, testSeason(1, true, 1, 1))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if len(eval.decisions) != 1 || eval.decisions[0].reason != ReasonCFCutoffNotMet {
		t.Fatalf("decisions = %+v, want reason %q", eval.decisions, ReasonCFCutoffNotMet)
	}
}

func TestEvaluateSeries_CFScoreEqualToThreshold_Passes(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "]"
	filesJSON := "[" + episodeFileJSON(500, 1, 100, false) + "]" // exactly the threshold
	srv := sonarrEpisodeFileServer(t, episodesJSON, filesJSON, nil, nil)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Exact Score Show", true, 1, []int{}, testSeason(1, true, 1, 1))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if len(eval.decisions) != 1 || !eval.decisions[0].wouldUnmonitor {
		t.Fatalf("decisions = %+v, want wouldUnmonitor=true (score equal to threshold passes)", eval.decisions)
	}
}

func TestEvaluateSeries_MultiEpisodeSeason_OneLowScoringEpisode_SkipsWholeSeason(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "," + episodeJSON(101, 1, 2, pastAirDate, 501) + "]"
	filesJSON := "[" + episodeFileJSON(500, 1, 200, false) + "," + episodeFileJSON(501, 1, 50, false) + "]"
	srv := sonarrEpisodeFileServer(t, episodesJSON, filesJSON, nil, nil)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Mixed Score Show", true, 1, []int{}, testSeason(1, true, 2, 2))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if len(eval.decisions) != 1 || eval.decisions[0].reason != ReasonCFCutoffNotMet {
		t.Fatalf("decisions = %+v, want reason %q: ANY episode below threshold fails the whole season", eval.decisions, ReasonCFCutoffNotMet)
	}
}

func TestEvaluateSeries_MissingCustomFormatScore_SkippedWithReason(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "]"
	filesJSON := `[{"id": 500, "seasonNumber": 1, "qualityCutoffNotMet": false}]` // customFormatScore absent
	srv := sonarrEpisodeFileServer(t, episodesJSON, filesJSON, nil, nil)
	defer srv.Close()
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "No Score Show", true, 1, []int{}, testSeason(1, true, 1, 1))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if len(eval.decisions) != 1 || eval.decisions[0].reason != ReasonCouldNotFetchCFScore {
		t.Fatalf("decisions = %+v, want reason %q", eval.decisions, ReasonCouldNotFetchCFScore)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning:\n%s", buf.String())
	}
}

func TestEvaluateSeries_EpisodeFileFetchFailure_CandidatesSkippedWithReason(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "]"
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/episode", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(episodesJSON))
	})
	mux.HandleFunc("/api/v3/episodefile", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-broken", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Broken Episodefile Show", true, 1, []int{}, testSeason(1, true, 1, 1))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if len(eval.decisions) != 1 || eval.decisions[0].reason != ReasonCouldNotFetchCFScore {
		t.Fatalf("decisions = %+v, want reason %q", eval.decisions, ReasonCouldNotFetchCFScore)
	}
}

func TestEvaluateSeries_FileCountMismatch_SkippedWithReason(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "," + episodeJSON(101, 1, 2, pastAirDate, 501) + "]"
	// statistics claims 2 files (testSeason fileCount=2), but only 1 is returned.
	filesJSON := "[" + episodeFileJSON(500, 1, 200, false) + "]"
	srv := sonarrEpisodeFileServer(t, episodesJSON, filesJSON, nil, nil)
	defer srv.Close()
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "File Count Mismatch Show", true, 1, []int{}, testSeason(1, true, 2, 2))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if len(eval.decisions) != 1 || eval.decisions[0].reason != ReasonSeasonFileCountMismatch {
		t.Fatalf("decisions = %+v, want reason %q", eval.decisions, ReasonSeasonFileCountMismatch)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning:\n%s", buf.String())
	}
}

// TestEvaluateSeries_EpisodefileLaziness_NotFetchedForSeriesFailingRulesOneToSix
// is the request-count assertion the binding evaluation-order resolution
// depends on: a series whose only season fails an earlier rule (here, rule
// 2 — incomplete) must never trigger the /episodefile fetch at all.
func TestEvaluateSeries_EpisodefileLaziness_NotFetchedForSeriesFailingRulesOneToSix(t *testing.T) {
	var gotEpisodefileRequests []string
	srv := sonarrEpisodeFileServer(t, `[]`, `[]`, nil, &gotEpisodefileRequests)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Incomplete Show", true, 1, []int{}, testSeason(1, true, 5, 10))
	evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if len(gotEpisodefileRequests) != 0 {
		t.Errorf("expected zero /episodefile requests for a series with no rule-1-6-passing season, got %d", len(gotEpisodefileRequests))
	}
}

// TestEvaluateSeries_MultipleCandidateSeasons_SingleEpisodefileFetchCoversAll
// pins the "at most once per series" half of the laziness contract: two
// candidate seasons in the same series must share exactly one /episodefile
// fetch, not one each.
func TestEvaluateSeries_MultipleCandidateSeasons_SingleEpisodefileFetchCoversAll(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "," + episodeJSON(200, 2, 1, pastAirDate, 600) + "]"
	filesJSON := "[" + episodeFileJSON(500, 1, 200, false) + "," + episodeFileJSON(600, 2, 200, false) + "]"
	var gotEp, gotEf []string
	srv := sonarrEpisodeFileServer(t, episodesJSON, filesJSON, &gotEp, &gotEf)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Two Season Show", true, 1, []int{}, testSeason(1, true, 1, 1), testSeason(2, true, 1, 1))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if len(gotEp) != 1 {
		t.Errorf("expected exactly 1 /episode request, got %d", len(gotEp))
	}
	if len(gotEf) != 1 {
		t.Errorf("expected exactly 1 /episodefile request (shared across both candidate seasons), got %d", len(gotEf))
	}
	if len(eval.decisions) != 2 {
		t.Fatalf("expected 2 decisions, got %d", len(eval.decisions))
	}
	for _, d := range eval.decisions {
		if !d.wouldUnmonitor {
			t.Errorf("season %d wouldUnmonitor = false, want true", d.season)
		}
	}
}

// TestEvaluateSeries_SeasonZeroSpecials_NoSpecialCasing pins the binding
// resolution: season 0 (specials) is evaluated exactly like any other
// season, with no special-casing.
func TestEvaluateSeries_SeasonZeroSpecials_NoSpecialCasing(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 0, 1, pastAirDate, 500) + "]"
	filesJSON := "[" + episodeFileJSON(500, 0, 200, false) + "]"
	srv := sonarrEpisodeFileServer(t, episodesJSON, filesJSON, nil, nil)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Show With Specials", true, 1, []int{}, testSeason(0, true, 1, 1))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if len(eval.decisions) != 1 || !eval.decisions[0].wouldUnmonitor || eval.decisions[0].season != 0 {
		t.Fatalf("decisions = %+v, want season 0 evaluated normally and would-unmonitor", eval.decisions)
	}
}

// --- runSonarrDecisionEngine: instance-level orchestration ------------------

// sonarrEngineFake wires a fake serving every endpoint a full
// runSonarrDecisionEngine pass touches: /api/v3/qualityprofile, /api/v3/tag,
// /api/v3/episode, /api/v3/episodefile, and (Phase 7) the write path's own
// GET/PUT /api/v3/series/{id} plus PUT /api/v3/episode/monitor. Every request
// to every path is recorded, stubbed or not, so a "zero writes" claim is
// about requests nobody thought to make rather than only the ones this fake
// happens to answer.
//
// A read endpoint answers 405 to any non-GET method, which is what made the
// Phase 6 zero-write-verb pin structural; the two write endpoints are the
// exact, and only, additions Phase 7 makes to that surface.
type sonarrEngineFake struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []recordedRequest

	profilesJSON string
	tagsJSON     string
	episodeJSON  string
	fileJSON     string

	// Phase 7 write-path fixtures. seriesDetail holds the GET
	// /api/v3/series/{id} body the write path re-fetches, by id — an id with
	// no entry answers 404, modeling a series that vanished between the scan
	// and the write. seriesPutStatus/seriesPutEcho override what a PUT
	// answers with (an unset echo means "behave like Sonarr": echo the object
	// that was sent); episodeMonitorStatus does the same for the episode
	// half.
	seriesDetail         map[int]string
	seriesPutStatus      map[int]int
	seriesPutEcho        map[int]string
	episodeMonitorStatus int

	// writeTimeEpisodeJSON, when set, is what /api/v3/episode answers with
	// once the write pass has begun (detected by its own fresh GET of
	// /api/v3/series/{id}, which always precedes its episode re-read). It is
	// how a test models the world changing between the decision and the
	// write — a season that started airing, an episode that was unmonitored
	// by something else — which is the entire subject of the pre-write
	// re-verification.
	writeTimeEpisodeJSON string
	writePassStarted     bool
}

func newSonarrEngineFake(t *testing.T, episodeJSON, fileJSON string) *sonarrEngineFake {
	t.Helper()
	f := &sonarrEngineFake{
		profilesJSON:         sonarrDecisionProfilesJSON,
		tagsJSON:             sonarrDecisionNoTagsJSON,
		episodeJSON:          episodeJSON,
		fileJSON:             fileJSON,
		seriesDetail:         map[int]string{},
		seriesPutStatus:      map[int]int{},
		seriesPutEcho:        map[int]string{},
		episodeMonitorStatus: http.StatusOK,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/qualityprofile", f.handle(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Write([]byte(f.profilesJSON))
	}))
	mux.HandleFunc("/api/v3/tag", f.handle(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Write([]byte(f.tagsJSON))
	}))
	mux.HandleFunc("/api/v3/episode", f.handle(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		// stampSeriesID: a real Sonarr always states each episode's own
		// seriesId, and since Phase 8 the WRITE path requires it (an episode
		// of the target season that cannot prove it belongs to this series is
		// refused rather than named in PUT /episode/monitor — see
		// episodesOfSeason). This fake's shared per-test fixture is served for
		// every ?seriesId=N, so the id cannot be baked into the fixture text
		// for a multi-series test; stamping it here is what makes the fake
		// faithful instead of exercising a refusal no live instance produces.
		seriesID, _ := strconv.Atoi(r.URL.Query().Get("seriesId"))
		w.Write([]byte(stampSeriesID(t, f.episodesFor(), seriesID)))
	}))
	mux.HandleFunc("/api/v3/episode/monitor", f.handle(f.serveEpisodeMonitor))
	mux.HandleFunc("/api/v3/series/", f.handle(f.serveSeriesDetail))
	mux.HandleFunc("/api/v3/episodefile", f.handle(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Write([]byte(f.fileJSON))
	}))
	// Catch-all: records every request to any path this fake does not
	// explicitly stub — including /api/v3/series/{id}, /api/v3/command, or
	// anything else a future bug might reach for — and answers 404. Same
	// rationale as radarrFake's catch-all (writer_test.go): a claim about
	// requests that were never made is only as strong as the fake's
	// recording of paths nobody thought to stub.
	mux.HandleFunc("/", f.handle(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *sonarrEngineFake) handle(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{method: r.Method, path: r.URL.Path, body: body, contentType: r.Header.Get("Content-Type")})
		f.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		next(w, r)
	}
}

// episodesFor answers /api/v3/episode with the decision-time fixture until
// the write pass starts, then with writeTimeEpisodeJSON if a test set one.
// See the field's doc comment for why.
func (f *sonarrEngineFake) episodesFor() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writePassStarted && f.writeTimeEpisodeJSON != "" {
		return f.writeTimeEpisodeJSON
	}
	return f.episodeJSON
}

// serveSeriesDetail is GET/PUT /api/v3/series/{id}: the write path's fresh
// pre-write read and the season write itself.
func (f *sonarrEngineFake) serveSeriesDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/api/v3/series/"))
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		f.mu.Lock()
		f.writePassStarted = true
		body, found := f.seriesDetail[id]
		f.mu.Unlock()
		if !found {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(body))
	case http.MethodPut:
		status := http.StatusOK
		if s, found := f.seriesPutStatus[id]; found {
			status = s
		}
		w.WriteHeader(status)
		if status >= 400 {
			w.Write([]byte(`{"message":"write rejected by fake"}`))
			return
		}
		if echo, overridden := f.seriesPutEcho[id]; overridden {
			w.Write([]byte(echo))
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Write(body)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// serveEpisodeMonitor answers PUT /api/v3/episode/monitor with the updated
// episode resources, as Sonarr does.
func (f *sonarrEngineFake) serveEpisodeMonitor(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if f.episodeMonitorStatus >= 400 {
		w.WriteHeader(f.episodeMonitorStatus)
		w.Write([]byte(`{"message":"episode monitor write rejected by fake"}`))
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		EpisodeIDs []int `json:"episodeIds"`
		Monitored  bool  `json:"monitored"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		// Never silently discard a decode failure the way an earlier fake
		// did: a malformed write body is a defect in the code under test, and
		// a fake that shrugs at it hides exactly the bug it exists to catch.
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"message":"episode monitor body is not valid JSON"}`))
		return
	}
	var elems []string
	for _, id := range req.EpisodeIDs {
		elems = append(elems, fmt.Sprintf(`{"id":%d,"monitored":%t}`, id, req.Monitored))
	}
	w.WriteHeader(f.episodeMonitorStatus)
	w.Write([]byte("[" + strings.Join(elems, ",") + "]"))
}

// writes returns every non-GET request the fake received — the "zero write
// requests" assertion is about requests of any method to any path, not
// merely the absence of a PUT to some expected write endpoint.
func (f *sonarrEngineFake) writes() []recordedRequest {
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

func (f *sonarrEngineFake) instance() Instance {
	return Instance{Name: "sonarr-main", Type: "sonarr", URL: f.srv.URL, APIKey: "key"}
}

// all returns every request the fake received, of any method — used by the
// cross-check's on-demand-fetch tests to prove a specific extra GET really
// happened (or, just as importantly, that no more than the expected number
// did).
func (f *sonarrEngineFake) all() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

// reportLineWithMsg isolates the single log line whose msg attr is an EXACT
// match for want (not merely a substring of the whole buffer) so callers can
// assert the mandated per-line attrs against ONLY that line. IMPORTANT
// REVIEW FIX: asserting against the whole buffer is not sufficient here —
// the cross-check's own "cross-check" log line (decision.go) also carries
// instance/seriesId/series/season for the very same season a would-unmonitor
// or skip report line describes, so a season-attr assertion made against the
// whole buffer can pass even when the report line itself dropped the attr
// entirely.
func reportLineWithMsg(t *testing.T, out, want string) string {
	t.Helper()
	token := "msg=" + want
	for _, line := range strings.Split(out, "\n") {
		for _, field := range strings.Fields(line) {
			if field == token {
				return line
			}
		}
	}
	t.Fatalf("expected a log line with %s:\n%s", token, out)
	return ""
}

func TestRunSonarrDecisionEngine_LogsWouldUnmonitorAndSkipLinesWithMandatedAttrs(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "]"
	filesJSON := "[" + episodeFileJSON(500, 1, 200, false) + "]"
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	series := []seriesElement{
		testSeries(1, "Would Unmonitor Show", true, 1, []int{}, testSeason(1, true, 1, 1)),
		testSeries(2, "No File Show", true, 1, []int{}, testSeason(1, true, 0, 5)),
	}
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

	out := buf.String()

	wouldUnmonitorLine := reportLineWithMsg(t, out, "would-unmonitor")
	for _, want := range []string{"instance=sonarr-main", "seriesId=1", `series="Would Unmonitor Show"`, "season=1", `reason="cutoff met"`, "profile=HD-1080p"} {
		if !strings.Contains(wouldUnmonitorLine, want) {
			t.Errorf("expected would-unmonitor line to contain %q:\n%s", want, wouldUnmonitorLine)
		}
	}

	skipLine := reportLineWithMsg(t, out, "skip")
	for _, want := range []string{"instance=sonarr-main", "seriesId=2", `series="No File Show"`, "season=1", `reason="season incomplete on disk"`, "profile=HD-1080p"} {
		if !strings.Contains(skipLine, want) {
			t.Errorf("expected skip line to contain %q:\n%s", want, skipLine)
		}
	}
}

// TestRunSonarrDecisionEngine_SeasonLinesNeverCarryMovieStyleAttrs pins the
// binding non-collision requirement: season report lines must never carry
// Radarr's movie-line attrs (id, title), since a season line's identity is
// seriesId+series+season.
func TestRunSonarrDecisionEngine_SeasonLinesNeverCarryMovieStyleAttrs(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "]"
	filesJSON := "[" + episodeFileJSON(500, 1, 200, false) + "]"
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	series := []seriesElement{testSeries(1, "Some Show", true, 1, []int{}, testSeason(1, true, 1, 1))}
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

	out := buf.String()
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "msg=would-unmonitor") && !strings.Contains(line, "msg=skip") {
			continue
		}
		if strings.Contains(line, " id=") || strings.Contains(line, " title=") {
			t.Errorf("season report line must not carry movie-style id/title attrs: %s", line)
		}
	}
}

func TestRunSonarrDecisionEngine_SummaryCountsCorrect(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "]"
	filesJSON := "[" + episodeFileJSON(500, 1, 200, false) + "]"
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	series := []seriesElement{
		testSeries(1, "Would Unmonitor Show", true, 1, []int{}, testSeason(1, true, 1, 1)),
		testSeries(2, "Incomplete A", true, 1, []int{}, testSeason(1, true, 0, 5)),
		testSeries(3, "Incomplete B", true, 1, []int{}, testSeason(1, true, 0, 5)),
		testSeries(4, "Not Monitored Show", false, 1, []int{}, testSeason(1, true, 1, 1)),
	}
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

	out := buf.String()
	if !strings.Contains(out, "totalSeriesMonitored=3") {
		t.Errorf("expected totalSeriesMonitored=3 (excludes the not-monitored series):\n%s", out)
	}
	if !strings.Contains(out, "seasonsEvaluated=3") {
		t.Errorf("expected seasonsEvaluated=3:\n%s", out)
	}
	if !strings.Contains(out, "wouldUnmonitor=1") {
		t.Errorf("expected wouldUnmonitor=1:\n%s", out)
	}
	if !strings.Contains(out, "season incomplete on disk=2") {
		t.Errorf("expected the skip-reason count 'season incomplete on disk=2':\n%s", out)
	}
	if !strings.Contains(out, `msg="sonarr decision summary"`) {
		t.Errorf("expected exactly the sonarr decision summary message:\n%s", out)
	}
}

func TestRunSonarrDecisionEngine_AlreadyUnmonitoredSeasonsCounted(t *testing.T) {
	fake := newSonarrEngineFake(t, `[]`, `[]`)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	series := []seriesElement{
		testSeries(1, "Mixed Show", true, 1, []int{}, testSeason(1, true, 0, 1), testSeason(2, false, 5, 5)),
	}
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

	out := buf.String()
	if !strings.Contains(out, "alreadyUnmonitored=1") {
		t.Errorf("expected alreadyUnmonitored=1:\n%s", out)
	}
}

func TestRunSonarrDecisionEngine_ProfileFetchFailure_NoReportLinesAtAll(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/qualityprofile", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-broken", Type: "sonarr", URL: srv.URL, APIKey: "key"}

	series := []seriesElement{testSeries(1, "Some Show", true, 1, []int{}, testSeason(1, true, 1, 1))}
	runSonarrDecisionEngine(context.Background(), logger, inst, series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

	out := buf.String()
	if strings.Contains(out, "msg=would-unmonitor") || strings.Contains(out, "msg=skip") {
		t.Errorf("a failed profile fetch must skip the whole instance: no report lines at all:\n%s", out)
	}
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected a warning:\n%s", out)
	}
}

func TestRunSonarrDecisionEngine_TagFetchFailure_NoReportLinesAtAll(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/qualityprofile", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(sonarrDecisionProfilesJSON))
	})
	mux.HandleFunc("/api/v3/tag", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-broken", Type: "sonarr", URL: srv.URL, APIKey: "key"}

	series := []seriesElement{testSeries(1, "Some Show", true, 1, []int{}, testSeason(1, true, 1, 1))}
	runSonarrDecisionEngine(context.Background(), logger, inst, series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

	out := buf.String()
	if strings.Contains(out, "msg=would-unmonitor") || strings.Contains(out, "msg=skip") {
		t.Errorf("a failed tag fetch must skip the whole instance: no report lines at all:\n%s", out)
	}
}

func TestRunSonarrDecisionEngine_SeriesNotMonitored_ExcludedEntirelyFromReport(t *testing.T) {
	fake := newSonarrEngineFake(t, `[]`, `[]`)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	series := []seriesElement{testSeries(1, "Unmonitored Show", false, 1, []int{}, testSeason(1, true, 1, 1))}
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

	out := buf.String()
	if strings.Contains(out, "Unmonitored Show") {
		t.Errorf("a not-monitored series must produce no report line at all:\n%s", out)
	}
	if !strings.Contains(out, "totalSeriesMonitored=0") {
		t.Errorf("expected totalSeriesMonitored=0:\n%s", out)
	}
}

// TestRunSonarrDecisionEngine_NeverMakesAWriteRequest is the structural
// zero-write-verb pin (item 1 of the carried-forward controller notes) at
// the decision-engine level: even when the engine produces would-unmonitor
// decisions, it must never issue a single non-GET request to anything.
func TestRunSonarrDecisionEngine_NeverMakesAWriteRequest(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "]"
	filesJSON := "[" + episodeFileJSON(500, 1, 200, false) + "]"
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	series := []seriesElement{testSeries(1, "Would Unmonitor Show", true, 1, []int{}, testSeason(1, true, 1, 1))}
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

	if !strings.Contains(buf.String(), "would-unmonitor") {
		t.Fatalf("test setup did not actually produce a would-unmonitor decision:\n%s", buf.String())
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("runSonarrDecisionEngine must never issue a write request, got %+v", writes)
	}
}

// --- runSonarrCrossCheck ----------------------------------------------------

func sonarrCandidateDecision(seriesID, season int, wouldUnmonitor bool, episodes []seasonCrossCheckEpisode) seasonDecision {
	return seasonDecision{
		seriesID: seriesID, series: fmt.Sprintf("Series %d", seriesID), season: season,
		wouldUnmonitor: wouldUnmonitor, completeOnDisk: true, crossCheckEpisodes: episodes, cfThreshold: 100,
		// The episode set a real evaluation hands the write pass is complete
		// unless an episode of the season had no id at all (see
		// buildSeasonCrossCheckEpisodes); the incomplete case has its own
		// dedicated tests rather than being every fixture's default.
		crossCheckEpisodesComplete: true,
	}
}

func TestRunSonarrCrossCheck_ZeroCandidates_PassesWithZeroItems(t *testing.T) {
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr"}

	cc := runSonarrCrossCheck(context.Background(), logger, nil, inst, nil, map[int]bool{})
	if cc.status != crossCheckStatusPassed {
		t.Errorf("status = %q, want %q", cc.status, crossCheckStatusPassed)
	}
	if cc.verified != 0 || cc.unverifiable != 0 {
		t.Errorf("verified/unverifiable = %d/%d, want 0/0", cc.verified, cc.unverifiable)
	}
	// Nothing was sampled, so nothing can have been unverifiable: a
	// zero-candidate cross-check must be silent rather than warning about a
	// sample it never took.
	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("a zero-candidate cross-check must produce no warnings:\n%s", buf.String())
	}
}

func TestRunSonarrCrossCheck_AgreeingSamples_Passes(t *testing.T) {
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr"}

	// Episode 100 is in the wanted set and its file agrees (qualityCutoffNotMet=true).
	wanted := map[int]bool{100: true}
	decisions := []seasonDecision{
		sonarrCandidateDecision(1, 1, false, []seasonCrossCheckEpisode{{episodeID: 100, monitored: boolPtr(true), qualityCutoffNotMet: boolPtr(true)}}),
		// Episode 200 is NOT in the wanted set and its file agrees (qualityCutoffNotMet=false).
		sonarrCandidateDecision(2, 1, true, []seasonCrossCheckEpisode{{episodeID: 200, monitored: boolPtr(true), qualityCutoffNotMet: boolPtr(false)}}),
	}
	cc := runSonarrCrossCheck(context.Background(), logger, nil, inst, decisions, wanted)

	if cc.status != crossCheckStatusPassed {
		t.Fatalf("status = %q, want %q:\n%s", cc.status, crossCheckStatusPassed, buf.String())
	}
	if cc.verified != 2 {
		t.Errorf("verified = %d, want 2", cc.verified)
	}
	if cc.writeVerified != 1 {
		t.Errorf("writeVerified = %d, want 1 (only the would-unmonitor sample)", cc.writeVerified)
	}
}

// TestRunSonarrCrossCheck_Disagreement_FailsWithErrorLog uses episode 100
// NOT in the wanted set while its file says qualityCutoffNotMet=true: a
// genuine disagreement. (The opposite direction — IN the wanted set with
// qualityCutoffNotMet=false — is deliberately NOT used here: that is the
// CF-only ambiguous shape (see runSonarrCrossCheck's doc comment, shape
// (b)) and must be treated as unverifiable, not flagged, per the review fix
// that added TestRunSonarrCrossCheck_CFOnlyWantedReason_TreatedAsUnverifiable
// below.)
func TestRunSonarrCrossCheck_Disagreement_FailsWithErrorLog(t *testing.T) {
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr"}

	wanted := map[int]bool{}
	decisions := []seasonDecision{
		sonarrCandidateDecision(1, 1, false, []seasonCrossCheckEpisode{{episodeID: 100, monitored: boolPtr(true), qualityCutoffNotMet: boolPtr(true)}}),
	}
	cc := runSonarrCrossCheck(context.Background(), logger, nil, inst, decisions, wanted)

	if cc.status != crossCheckStatusFailed {
		t.Fatalf("status = %q, want %q", cc.status, crossCheckStatusFailed)
	}
	if !strings.Contains(buf.String(), "level=ERROR") || !strings.Contains(buf.String(), "disagreement") {
		t.Errorf("expected an error log naming the disagreement:\n%s", buf.String())
	}
}

// TestRunSonarrCrossCheck_WouldUnmonitorSeasonHasWantedEpisode_Disagreement
// pins the IMPORTANT review fix: rule 4 (decision.go) decides would-unmonitor
// via the SEASON key (seriesId, seasonNumber) against wantedSeasons, while
// this cross-check independently consults wantedEpisodeIDs at the EPISODE
// level for the very same season. If those two views of the same
// /wanted/cutoff fetch ever disagree — a mis-built or transposed key on
// either side, or drift between the two fetches — a would-unmonitor season
// containing an episode that IS in the wanted set is a direct contradiction
// of the rule that produced the decision, and must fail the cross-check
// regardless of qualityCutoffNotMet. This is deliberately the WORST case:
// qualityCutoffNotMet=true here reads as an ordinary verified agreement
// under the old per-field comparison alone (inWantedSet == qualityCutoffNotMet,
// both true) — the exact shape the finding says could count as "evidence of
// correctness" instead of the problem it actually is.
func TestRunSonarrCrossCheck_WouldUnmonitorSeasonHasWantedEpisode_Disagreement(t *testing.T) {
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr"}

	wanted := map[int]bool{100: true}
	decisions := []seasonDecision{
		sonarrCandidateDecision(1, 1, true, []seasonCrossCheckEpisode{{episodeID: 100, monitored: boolPtr(true), qualityCutoffNotMet: boolPtr(true)}}),
	}
	cc := runSonarrCrossCheck(context.Background(), logger, nil, inst, decisions, wanted)

	if cc.status != crossCheckStatusFailed {
		t.Fatalf("status = %q, want %q (would-unmonitor season contains a wanted episode):\n%s", cc.status, crossCheckStatusFailed, buf.String())
	}
	if !strings.Contains(buf.String(), "level=ERROR") || !strings.Contains(buf.String(), "wanted/cutoff set") {
		t.Errorf("expected an error log naming the would-unmonitor/wanted-set contradiction:\n%s", buf.String())
	}
}

// TestRunSonarrCrossCheck_CFOnlyWantedReason_TreatedAsUnverifiable pins the
// IMPORTANT review fix, shape (b): an episode IN the wanted set (because
// its custom-format score is below the profile's cutoff) whose file's
// qualityCutoffNotMet is false (because the QUALITY cutoff, the only thing
// that field reflects, IS met) must not be flagged as a disagreement — that
// mismatch is fully explained by a legitimate CF-only wanted reason.
func TestRunSonarrCrossCheck_CFOnlyWantedReason_TreatedAsUnverifiable(t *testing.T) {
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr"}

	wanted := map[int]bool{100: true}
	decisions := []seasonDecision{
		sonarrCandidateDecision(1, 1, false, []seasonCrossCheckEpisode{{episodeID: 100, monitored: boolPtr(true), qualityCutoffNotMet: boolPtr(false)}}),
	}
	cc := runSonarrCrossCheck(context.Background(), logger, nil, inst, decisions, wanted)

	if cc.status != crossCheckStatusInconclusive {
		t.Fatalf("status = %q, want %q (CF-only shape must not read as a disagreement):\n%s", cc.status, crossCheckStatusInconclusive, buf.String())
	}
	if cc.unverifiable != 1 || cc.verified != 0 {
		t.Errorf("verified/unverifiable = %d/%d, want 0/1", cc.verified, cc.unverifiable)
	}
	if strings.Contains(buf.String(), "level=ERROR") {
		t.Errorf("the CF-only shape must never produce a disagreement error:\n%s", buf.String())
	}
}

// TestRunSonarrCrossCheck_UnmonitoredEpisode_ExcludedFromComparison pins the
// IMPORTANT review fix, shape (a): an unmonitored episode inside an
// otherwise-monitored, complete season can never appear in /wanted/cutoff
// (Sonarr's own filter), so inWantedSet=false alongside
// qualityCutoffNotMet=true for that one episode must not be flagged.
func TestRunSonarrCrossCheck_UnmonitoredEpisode_ExcludedFromComparison(t *testing.T) {
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr"}

	decisions := []seasonDecision{
		sonarrCandidateDecision(1, 1, false, []seasonCrossCheckEpisode{{episodeID: 100, monitored: boolPtr(false), qualityCutoffNotMet: boolPtr(true)}}),
	}
	cc := runSonarrCrossCheck(context.Background(), logger, nil, inst, decisions, map[int]bool{})

	if cc.status != crossCheckStatusInconclusive {
		t.Fatalf("status = %q, want %q (unmonitored episode excluded, nothing left to verify):\n%s", cc.status, crossCheckStatusInconclusive, buf.String())
	}
	if cc.verified != 0 || cc.unverifiable != 1 {
		t.Errorf("verified/unverifiable = %d/%d, want 0/1", cc.verified, cc.unverifiable)
	}
	if strings.Contains(buf.String(), "level=ERROR") {
		t.Errorf("an unmonitored episode must never produce a disagreement error:\n%s", buf.String())
	}
}

func TestRunSonarrCrossCheck_AllUnverifiable_ReturnsInconclusive(t *testing.T) {
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr"}

	decisions := []seasonDecision{
		sonarrCandidateDecision(1, 1, true, []seasonCrossCheckEpisode{{episodeID: 100, monitored: boolPtr(true), qualityCutoffNotMet: nil}}),
	}
	cc := runSonarrCrossCheck(context.Background(), logger, nil, inst, decisions, map[int]bool{})

	if cc.status != crossCheckStatusInconclusive {
		t.Fatalf("status = %q, want %q:\n%s", cc.status, crossCheckStatusInconclusive, buf.String())
	}
	if cc.unverifiable != 1 || cc.verified != 0 {
		t.Errorf("verified/unverifiable = %d/%d, want 0/1", cc.verified, cc.unverifiable)
	}
	// The write gate (writeGateBlockReason) reads the would-unmonitor pool's
	// own counts, not the aggregate: a sampled would-unmonitor season that
	// could not be verified must land in writeUnverifiable, or the gate would
	// be asked to authorize writes on evidence nobody counted.
	if cc.writeUnverifiable != 1 || cc.writeVerified != 0 {
		t.Errorf("writeVerified/writeUnverifiable = %d/%d, want 0/1 — the write gate reads these", cc.writeVerified, cc.writeUnverifiable)
	}
}

// TestRunSonarrCrossCheck_NoEpisodeFileDataAtAll_TreatedAsUnverifiable pins
// the "series never fetched /episodefile, and no raw episode data to fetch
// it on demand either" case: crossCheckEpisodes and rawEpisodesForCrossCheck
// are both nil, and the cross-check must treat the whole season as
// unverifiable rather than crashing or silently skipping it out of the
// sample entirely.
func TestRunSonarrCrossCheck_NoEpisodeFileDataAtAll_TreatedAsUnverifiable(t *testing.T) {
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr"}

	decisions := []seasonDecision{
		{seriesID: 1, series: "No File Data Show", season: 1, wouldUnmonitor: false, completeOnDisk: true, crossCheckEpisodes: nil, reason: ReasonSeasonNotFullyAired},
	}
	cc := runSonarrCrossCheck(context.Background(), logger, nil, inst, decisions, map[int]bool{})

	if cc.status != crossCheckStatusInconclusive {
		t.Fatalf("status = %q, want %q:\n%s", cc.status, crossCheckStatusInconclusive, buf.String())
	}
	if cc.unverifiable != 1 {
		t.Errorf("unverifiable = %d, want 1", cc.unverifiable)
	}
}

// TestRunSonarrCrossCheck_SkipPoolExcludesIncompleteSeasons pins the
// skip-side pool's eligibility rule: only completeOnDisk seasons are
// eligible skip-side candidates, mirroring Radarr's "monitored && hasFile"
// eligibility for its own skip pool.
func TestRunSonarrCrossCheck_SkipPoolExcludesIncompleteSeasons(t *testing.T) {
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr"}

	decisions := []seasonDecision{
		{seriesID: 1, series: "Incomplete Show", season: 1, wouldUnmonitor: false, completeOnDisk: false, reason: ReasonSeasonIncomplete},
	}
	cc := runSonarrCrossCheck(context.Background(), logger, nil, inst, decisions, map[int]bool{})

	// Nothing sampled at all (not complete on disk, not would-unmonitor), so
	// zero items -> a trivial pass, not an inconclusive result.
	if cc.status != crossCheckStatusPassed {
		t.Errorf("status = %q, want %q (an incomplete-on-disk skip is not sample-eligible at all)", cc.status, crossCheckStatusPassed)
	}
	if cc.verified != 0 || cc.unverifiable != 0 {
		t.Errorf("verified/unverifiable = %d/%d, want 0/0", cc.verified, cc.unverifiable)
	}
}

// TestRunSonarrCrossCheck_PerEpisodeLineDemotedToDebug_SeasonAggregateAtInfo
// pins F3 (Phase 6 final review round): the cross-check used to log one INFO
// "cross-check" line per COMPARED EPISODE — for up to 20 sampled seasons of
// ordinary 12-24-episode seasons, that is 200-500 INFO lines per instance
// per cycle, burying the ERROR disagreement and WARN unverifiable lines the
// cross-check exists to surface (the same noise-budget hazard this project
// has already fixed twice by mandate elsewhere). The per-episode line must
// drop to DEBUG, and a single INFO aggregate ("cross-check season": verdict
// + compared/agreed/unverifiable counts) must appear once per sampled
// season instead. This season has 3 compared episodes (one disagreeing) and
// 1 unverifiable (nil qualityCutoffNotMet) to prove the aggregate counts
// are real, not placeholders.
func TestRunSonarrCrossCheck_PerEpisodeLineDemotedToDebug_SeasonAggregateAtInfo(t *testing.T) {
	wanted := map[int]bool{} // none of these episodes are in the wanted set
	episodes := []seasonCrossCheckEpisode{
		{episodeID: 100, monitored: boolPtr(true), qualityCutoffNotMet: boolPtr(false)}, // agrees: inWantedSet=false == qualityCutoffNotMet=false
		{episodeID: 101, monitored: boolPtr(true), qualityCutoffNotMet: boolPtr(false)}, // agrees
		{episodeID: 102, monitored: boolPtr(true), qualityCutoffNotMet: boolPtr(true)},  // disagrees: inWantedSet=false != qualityCutoffNotMet=true
		{episodeID: 103, monitored: boolPtr(true), qualityCutoffNotMet: nil},            // unverifiable
	}
	decisions := []seasonDecision{sonarrCandidateDecision(1, 1, false, episodes)}

	infoLogger, infoBuf := newDecisionTestLogger(slog.LevelInfo)
	runSonarrCrossCheck(context.Background(), infoLogger, nil, Instance{Name: "sonarr-main", Type: "sonarr"}, decisions, wanted)
	infoOut := infoBuf.String()

	if strings.Contains(infoOut, "msg=cross-check ") || strings.HasSuffix(strings.TrimRight(infoOut, "\n"), "msg=cross-check") {
		t.Errorf("the per-episode msg=cross-check line must no longer appear at INFO level:\n%s", infoOut)
	}
	for _, line := range strings.Split(infoOut, "\n") {
		if strings.Contains(line, "level=INFO") && strings.Contains(line, "msg=cross-check") && !strings.Contains(line, `msg="cross-check season"`) {
			t.Errorf("found a per-episode cross-check line at INFO level, want it demoted to DEBUG: %q", line)
		}
	}

	aggregateLine := ""
	aggregateCount := 0
	for _, line := range strings.Split(infoOut, "\n") {
		if strings.Contains(line, `msg="cross-check season"`) {
			aggregateCount++
			aggregateLine = line
		}
	}
	if aggregateCount != 1 {
		t.Fatalf(`expected exactly 1 msg="cross-check season" aggregate line for this one sampled season, got %d:%s`, aggregateCount, infoOut)
	}
	if !strings.Contains(aggregateLine, "seriesId=1") || !strings.Contains(aggregateLine, "season=1") {
		t.Errorf("aggregate line missing seriesId/season identifying attrs: %q", aggregateLine)
	}
	if !strings.Contains(aggregateLine, "compared=3") {
		t.Errorf("aggregate line = %q, want compared=3 (3 episodes had a comparable qualityCutoffNotMet value)", aggregateLine)
	}
	if !strings.Contains(aggregateLine, "agreed=2") {
		t.Errorf("aggregate line = %q, want agreed=2 (2 of the 3 compared episodes agreed)", aggregateLine)
	}
	if !strings.Contains(aggregateLine, "unverifiable=1") {
		t.Errorf("aggregate line = %q, want unverifiable=1 (episode 103's nil qualityCutoffNotMet)", aggregateLine)
	}

	// The per-episode detail must still exist, just gated behind DEBUG — a
	// human who actually needs to see every episode can still get it.
	debugLogger, debugBuf := newDecisionTestLogger(slog.LevelDebug)
	runSonarrCrossCheck(context.Background(), debugLogger, nil, Instance{Name: "sonarr-main", Type: "sonarr"}, decisions, wanted)
	debugOut := debugBuf.String()
	perEpisodeDebugCount := 0
	for _, line := range strings.Split(debugOut, "\n") {
		if strings.Contains(line, "level=DEBUG") && strings.Contains(line, "msg=cross-check") && !strings.Contains(line, `msg="cross-check season"`) {
			perEpisodeDebugCount++
		}
	}
	if perEpisodeDebugCount != 4 {
		t.Errorf("expected 4 per-episode msg=cross-check lines at DEBUG (one per sampled episode), got %d:\n%s", perEpisodeDebugCount, debugOut)
	}
	if !strings.Contains(debugOut, `msg="cross-check season"`) {
		t.Errorf("the season aggregate line must still appear at DEBUG level too (INFO is a floor, not a ceiling):\n%s", debugOut)
	}
}

func TestRunSonarrDecisionEngine_CrossCheckDisagreement_SummaryStatesFailed(t *testing.T) {
	// The season passes every rule and becomes a would-unmonitor candidate,
	// so the /episodefile fetch happens and crossCheckEpisodes is populated —
	// but the file it returns claims qualityCutoffNotMet=true while the
	// (empty) wanted set says the episode is not below cutoff. Two Sonarr
	// code paths disagreeing is exactly what the cross-check exists to catch,
	// and the summary must render it as FAILED.
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "]"
	filesJSON := "[" + episodeFileJSON(500, 1, 200, true) + "]" // qualityCutoffNotMet=true but NOT in wanted set: disagreement
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	series := []seriesElement{testSeries(1, "Disagreeing Show", true, 1, []int{}, testSeason(1, true, 1, 1))}
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

	out := buf.String()
	if !strings.Contains(out, "crossCheck=FAILED") {
		t.Errorf("expected crossCheck=FAILED in the summary:\n%s", out)
	}
}

// TestRunSonarrDecisionEngine_WouldUnmonitorSeasonHasWantedEpisode_CrossCheckFails
// is the IMPORTANT review fix's end-to-end regression proof, driven through
// the full engine: wantedEpisodeIDs contains episode 100, but wantedSeasons
// deliberately OMITS that episode's (seriesId, seasonNumber) key — modeling
// the exact drift/mis-keyed shape the finding describes between the two
// views the same /wanted/cutoff fetch produces. Rule 4, which consults only
// wantedSeasons, lets the season through as would-unmonitor; the
// independent, episode-keyed cross-check must catch the contradiction and
// fail the whole cross-check rather than count it as verified agreement.
func TestRunSonarrDecisionEngine_WouldUnmonitorSeasonHasWantedEpisode_CrossCheckFails(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "]"
	filesJSON := "[" + episodeFileJSON(500, 1, 200, true) + "]" // qualityCutoffNotMet=true: reads as ordinary agreement without the fix
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	series := []seriesElement{testSeries(1, "Contradiction Show", true, 1, []int{}, testSeason(1, true, 1, 1))}
	wantedEpisodeIDs := map[int]bool{100: true} // episode 100 IS in the wanted set...
	wantedSeasons := map[seasonKey]bool{}       // ...but its season key is absent, so rule 4 lets the season through
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, wantedEpisodeIDs, wantedSeasons, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

	out := buf.String()
	if !strings.Contains(out, "msg=would-unmonitor") {
		t.Fatalf("test setup did not actually produce a would-unmonitor season:\n%s", out)
	}
	if !strings.Contains(out, "crossCheck=FAILED") {
		t.Errorf("expected crossCheck=FAILED: a would-unmonitor season contained an episode in the wanted set:\n%s", out)
	}
}

// TestRunSonarrDecisionEngine_SkipSideCrossCheck_RuleFourSkip_BecomesVerifiable
// is the IMPORTANT review fix's own regression proof, driven through the
// full engine (not a hand-built seasonDecision): a season that fails rule 4
// (in the wanted set) never reaches rule 7, so evaluateSeries itself never
// fetches /episodefile for it — before the fix, this season's
// crossCheckEpisodes would be nil and the cross-check would count it
// unverifiable with a WARN, forever, on every real skip of this shape.
// After the fix, the cross-check makes its own bounded, on-demand
// /episodefile fetch for this sampled skip-side season and actually
// verifies it.
func TestRunSonarrDecisionEngine_SkipSideCrossCheck_RuleFourSkip_BecomesVerifiable(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "]"
	filesJSON := "[" + episodeFileJSON(500, 1, 200, true) + "]" // qualityCutoffNotMet=true, agrees with inWantedSet=true below
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	series := []seriesElement{testSeries(1, "Skip Side Show", true, 1, []int{}, testSeason(1, true, 1, 1))}
	wantedEpisodeIDs := map[int]bool{100: true}
	wantedSeasons := map[seasonKey]bool{{seriesID: 1, seasonNumber: 1}: true} // rule 4 fails: never reaches rule 7
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, wantedEpisodeIDs, wantedSeasons, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

	out := buf.String()
	if !strings.Contains(out, `reason="quality cutoff not met"`) {
		t.Fatalf("test setup did not actually produce a rule-4 skip:\n%s", out)
	}
	if !strings.Contains(out, `crossCheck="passed (1 verified, 0 unverifiable)"`) {
		t.Errorf(`expected crossCheck="passed (1 verified, 0 unverifiable)" (the rule-4 skip made genuinely verifiable via the on-demand fetch), got:%s`, out)
	}

	// Prove the verification is real, not incidental: exactly one
	// /episodefile request happened, beyond what evaluateSeries itself
	// would have made (which is none — this season never reaches rule 7).
	episodefileRequests := 0
	for _, r := range fake.all() {
		if r.path == "/api/v3/episodefile" {
			episodefileRequests++
		}
	}
	if episodefileRequests != 1 {
		t.Errorf("expected exactly 1 /episodefile request (the cross-check's on-demand fetch), got %d:\n%+v", episodefileRequests, fake.all())
	}
}

// ============================================================================
// Phase 7: Phase 6 branch-review carry-over minors (binding)
// ============================================================================

// TestEvaluateSeries_EpisodeFileMissingSeasonNumber_WarnsNamingTheFile pins
// the FIX-IN-PASSING at decision.go's filesBySeason build: an /episodefile
// record with an absent seasonNumber is dropped from the ONLY input rule 7
// has, with no log line of any level — while the structurally identical
// episode drop three hundred lines above it warns. The file-count guard
// usually catches the resulting shortfall, but it names the symptom
// ("fewer episode files returned than statistics claims") rather than the
// cause, and a season carrying an extra correctly-labelled file absorbs the
// drop entirely.
func TestEvaluateSeries_EpisodeFileMissingSeasonNumber_WarnsNamingTheFile(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "]"
	// File 501 has no seasonNumber at all: silently dropped from filesBySeason.
	filesJSON := `[` + episodeFileJSON(500, 1, 200, false) + `,{"id": 501, "customFormatScore": 200, "qualityCutoffNotMet": false}]`
	srv := sonarrEpisodeFileServer(t, episodesJSON, filesJSON, nil, nil)
	defer srv.Close()
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Dropped File Show", true, 1, []int{}, testSeason(1, true, 1, 1))
	evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	out := buf.String()
	if !strings.Contains(out, "episode file missing seasonNumber") {
		t.Errorf("expected a WARN naming the dropped episode file, got:\n%s", out)
	}
	if !strings.Contains(out, "fileId=501") {
		t.Errorf("the warn must name the file id so the cause is traceable:\n%s", out)
	}
}

// TestEvaluateSeries_DuplicateSeasonNumber_WarnsAndExcludesTheDuplicate pins
// the FIX-IN-PASSING at decision.go's candidateIndex/statsFileCountFor/
// seasonEpisodesFor maps: they are keyed on the bare season number, so a
// duplicated seasonNumber among a series' monitored seasons leaves the FIRST
// decision's reason empty — reported as msg=skip reason="" at INFO with no
// warn at all, and counted under an empty-string key in skipReasons. A skip
// with no reason, produced from untrusted input, is the exact shape §2.6
// forbids.
func TestEvaluateSeries_DuplicateSeasonNumber_WarnsAndExcludesTheDuplicate(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "]"
	filesJSON := "[" + episodeFileJSON(500, 1, 200, false) + "]"
	srv := sonarrEpisodeFileServer(t, episodesJSON, filesJSON, nil, nil)
	defer srv.Close()
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	// Two monitored seasons both claiming to be season 1.
	s := testSeries(1, "Duplicate Season Show", true, 1, []int{}, testSeason(1, true, 1, 1), testSeason(1, true, 1, 1))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if len(eval.decisions) != 1 {
		t.Fatalf("expected exactly 1 decision (the duplicate excluded), got %d: %+v", len(eval.decisions), eval.decisions)
	}
	if eval.decisions[0].reason == "" {
		t.Errorf("a decision must never carry an empty reason: %+v", eval.decisions[0])
	}
	if !strings.Contains(buf.String(), "duplicate seasonNumber") {
		t.Errorf("expected a WARN naming the duplicate season number:\n%s", buf.String())
	}
}

// TestEvaluateSeries_EpisodeCountMismatch_NotSkipPoolEligible pins the third
// evidence-touching Phase 6 minor: completeOnDisk was set before the episode-
// count guard and never cleared, so an "episode data inconsistent" season
// entered the cross-check's skip pool carrying NEITHER crossCheckEpisodes NOR
// rawEpisodesForCrossCheck — unverifiable by construction, consuming one of
// only 10 skip-side sample slots, always adding a second WARN, and pushing
// the verdict toward inconclusive. The write gate reads that verdict, so this
// must be closed before the gate goes live.
func TestEvaluateSeries_EpisodeCountMismatch_NotSkipPoolEligible(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "]" // 1 of 3
	filesJSON := "[" + episodeFileJSON(500, 1, 200, false) + "]"
	srv := sonarrEpisodeFileServer(t, episodesJSON, filesJSON, nil, nil)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Inconsistent Show", true, 1, []int{}, testSeason(1, true, 3, 3))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if len(eval.decisions) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(eval.decisions))
	}
	if eval.decisions[0].completeOnDisk {
		t.Errorf("a season whose episode data is inconsistent must not be skip-pool eligible: it can never be verified, so it would consume a sample slot and produce nothing but a WARN")
	}
}

// TestEvaluateSeries_EpisodefileFetchFailure_NotSkipPoolEligible is the third
// and last shape of the same evidence-touching defect class, and the binding
// Phase 6 branch note ordered all three closed BEFORE the write gate went
// live: when the per-series /episodefile fetch fails, every candidate season
// of that series is stamped "could not fetch custom format score" — but
// completeOnDisk was set by rule 2 and never cleared, so each one entered the
// cross-check's skip pool carrying NEITHER crossCheckEpisodes NOR
// rawEpisodesForCrossCheck: unverifiable by construction.
//
// The cost is not theoretical. Such a season consumes one of only 10 skip-side
// sample slots, always adds a WARN, and pushes the verdict toward inconclusive
// — and the write gate reads that verdict for the WHOLE instance, so a broken
// /episodefile endpoint on one series could withhold every write on every
// other one. A season whose file data could not be read has nothing to
// contribute as evidence, so it is not sample-eligible at all.
func TestEvaluateSeries_EpisodefileFetchFailure_NotSkipPoolEligible(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "]"
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/episode", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(episodesJSON))
	})
	mux.HandleFunc("/api/v3/episodefile", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-broken", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Broken Episodefile Show", true, 1, []int{}, testSeason(1, true, 1, 1))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if len(eval.decisions) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(eval.decisions))
	}
	d := eval.decisions[0]
	if d.reason != ReasonCouldNotFetchCFScore {
		t.Fatalf("reason = %q, want %q", d.reason, ReasonCouldNotFetchCFScore)
	}
	if len(d.crossCheckEpisodes) != 0 || len(d.rawEpisodesForCrossCheck) != 0 {
		t.Fatalf("this test only proves something while the season really has no cross-check data: episodes=%d raw=%d",
			len(d.crossCheckEpisodes), len(d.rawEpisodesForCrossCheck))
	}
	if d.completeOnDisk {
		t.Errorf("a season whose /episodefile fetch failed must not be skip-pool eligible: it can never be verified, so it would consume one of ten sample slots, add a WARN, and push the instance's cross-check — which the write gate reads — toward inconclusive")
	}
}

// TestRunSonarrCrossCheck_EpisodefileFetchFailureSeason_IsNeverSampled is the
// consequence the test above exists for, asserted where it actually bites: the
// skip-side pool is built from completeOnDisk seasons, so clearing the flag is
// what keeps an unverifiable-by-construction season out of the sample and off
// the verdict.
func TestRunSonarrCrossCheck_EpisodefileFetchFailureSeason_IsNeverSampled(t *testing.T) {
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr"}

	// One verifiable would-unmonitor season, plus the shape evaluateSeries now
	// produces for a failed /episodefile fetch: no cross-check data of any
	// kind, and therefore not skip-pool eligible.
	decisions := []seasonDecision{
		sonarrCandidateDecision(1, 1, true, []seasonCrossCheckEpisode{
			{episodeID: 100, monitored: boolPtr(true), qualityCutoffNotMet: boolPtr(false)},
		}),
		{seriesID: 2, series: "Broken Episodefile Show", season: 1, reason: ReasonCouldNotFetchCFScore, completeOnDisk: false},
	}
	cc := runSonarrCrossCheck(context.Background(), logger, nil, inst, decisions, map[int]bool{})

	if cc.unverifiable != 0 {
		t.Errorf("unverifiable = %d, want 0: the fetch-failure season must not have been sampled at all:\n%s", cc.unverifiable, buf.String())
	}
	if cc.status != crossCheckStatusPassed || cc.verified != 1 {
		t.Errorf("status/verified = %q/%d, want %q/1: one unverifiable-by-construction season must not drag the instance's verdict down:\n%s",
			cc.status, cc.verified, crossCheckStatusPassed, buf.String())
	}
	if strings.Contains(buf.String(), "seriesId=2") {
		t.Errorf("nothing may be logged about a season that was never sampled:\n%s", buf.String())
	}
}

// TestEvaluateSeries_FileCountMismatch_StillCarriesCrossCheckEpisodes is the
// same minor's other half: a file-count-mismatch season DOES have a validated
// episode list and a fetched /episodefile map in scope, so it can be made
// verifiable at zero additional API cost rather than being left unverifiable
// by construction in the skip pool.
func TestEvaluateSeries_FileCountMismatch_StillCarriesCrossCheckEpisodes(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "," + episodeJSON(101, 1, 2, pastAirDate, 501) + "]"
	filesJSON := "[" + episodeFileJSON(500, 1, 200, false) + "]" // statistics claims 2
	srv := sonarrEpisodeFileServer(t, episodesJSON, filesJSON, nil, nil)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "File Count Mismatch Show", true, 1, []int{}, testSeason(1, true, 2, 2))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if len(eval.decisions) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(eval.decisions))
	}
	d := eval.decisions[0]
	if d.reason != ReasonSeasonFileCountMismatch {
		t.Fatalf("reason = %q, want %q", d.reason, ReasonSeasonFileCountMismatch)
	}
	if len(d.crossCheckEpisodes) == 0 {
		t.Errorf("a file-count-mismatch season already has both halves of the join in scope; leaving it with no cross-check data makes it unverifiable by construction in the skip pool")
	}
}

// TestEvaluateSeries_UntrustedAirDateAfterAFutureOne_StillWarns pins the
// FIX-IN-PASSING at rule 3's loop: it breaks on the FIRST not-aired episode,
// so the untrusted-airDateUtc WARN fires only when the untrusted episode
// happens to precede every validly-future one. A genuinely airing season that
// also carries one TBA/undated episode therefore skips silently at INFO —
// exactly the ambiguity the round-1 fix was meant to remove.
func TestEvaluateSeries_UntrustedAirDateAfterAFutureOne_StillWarns(t *testing.T) {
	// Episode 100 is validly future-dated (healthy "still airing"); episode
	// 101 is undated (untrusted). Order matters: 100 is seen first and breaks
	// the pre-fix loop before 101 is ever examined.
	episodesJSON := "[" + episodeJSON(100, 1, 1, futureAirDate, 500) + "," + episodeJSON(101, 1, 2, "", 501) + "]"
	srv := sonarrEpisodeFileServer(t, episodesJSON, "[]", nil, nil)
	defer srv.Close()
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Mixed Airing Show", true, 1, []int{}, testSeason(1, true, 2, 2))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	if len(eval.decisions) != 1 || eval.decisions[0].reason != ReasonSeasonNotFullyAired {
		t.Fatalf("decisions = %+v, want one airing skip", eval.decisions)
	}
	out := buf.String()
	if !strings.Contains(out, "airDateUtc is absent or unparseable") {
		t.Errorf("the undated episode must still be warned about even though a validly-future episode was seen first:\n%s", out)
	}
	if !strings.Contains(out, "episodeId=101") {
		t.Errorf("the warn must name the untrusted episode:\n%s", out)
	}
}

// TestEpisodeAiringStatus_TableDriven replaces the old
// TestEpisodeHasAired_TableDriven: episodeHasAired had zero production
// callers (rule 3 uses episodeAiringStatus) and survived only via that test,
// so it is deleted and the table is retargeted at the function production
// actually calls — asserting BOTH return values, which pins the untrusted bit
// at unit level for the first time.
func TestEpisodeAiringStatus_TableDriven(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name          string
		e             episodeElement
		wantAired     bool
		wantUntrusted bool
	}{
		{"past date", episodeElement{AirDateUtc: strPtr("2020-01-01T00:00:00Z")}, true, false},
		{"future date", episodeElement{AirDateUtc: strPtr("2030-01-01T00:00:00Z")}, false, false},
		{"absent", episodeElement{}, false, true},
		{"unparseable", episodeElement{AirDateUtc: strPtr("garbage")}, false, true},
		{"exactly now is not before now", episodeElement{AirDateUtc: strPtr("2025-01-01T00:00:00Z")}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			aired, untrusted := episodeAiringStatus(tc.e, now)
			if aired != tc.wantAired || untrusted != tc.wantUntrusted {
				t.Errorf("episodeAiringStatus(%+v) = (%v, %v), want (%v, %v)", tc.e, aired, untrusted, tc.wantAired, tc.wantUntrusted)
			}
		})
	}
}

// TestRunSonarrCrossCheck_EpisodeMonitoredNil_WarnsAndCountsUnverifiable pins
// the Phase 6 minor at the cross-check's per-episode monitored filter:
// ep.monitored == nil (a shape never observed live) was folded into the same
// silent exclusion as monitored == false, so an untrusted shape was discarded
// with no warn and no unverifiable count while the season could still render
// "verified" on its remaining episodes.
func TestRunSonarrCrossCheck_EpisodeMonitoredNil_WarnsAndCountsUnverifiable(t *testing.T) {
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr"}

	decisions := []seasonDecision{
		sonarrCandidateDecision(1, 1, false, []seasonCrossCheckEpisode{
			{episodeID: 100, monitored: nil, qualityCutoffNotMet: boolPtr(false)},
			{episodeID: 101, monitored: boolPtr(true), qualityCutoffNotMet: boolPtr(false)},
		}),
	}
	runSonarrCrossCheck(context.Background(), logger, nil, inst, decisions, map[int]bool{})

	out := buf.String()
	if !strings.Contains(out, "monitored is absent") {
		t.Errorf("an episode whose own monitored field could not be read is untrusted input and must warn, not vanish:\n%s", out)
	}
	if !strings.Contains(out, "unverifiable=1") {
		t.Errorf("the season aggregate must count the untrusted episode as unverifiable:\n%s", out)
	}
}

// TestRunSonarrCrossCheck_WantedButUnmonitoredEpisode_InWouldUnmonitorSeason_IsADisagreement
// pins the hoist: the would-unmonitor/wanted-set contradiction check sat
// AFTER the monitored filter, so the one shape it could not see was a
// wanted-set episode that /episode reports unmonitored — precisely two-fetch
// drift. Wanted-set membership contradicts rule 4 regardless of the episode's
// own monitored flag.
func TestRunSonarrCrossCheck_WantedButUnmonitoredEpisode_InWouldUnmonitorSeason_IsADisagreement(t *testing.T) {
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr"}

	decisions := []seasonDecision{
		sonarrCandidateDecision(1, 1, true, []seasonCrossCheckEpisode{
			{episodeID: 100, monitored: boolPtr(false), qualityCutoffNotMet: boolPtr(true)},
			{episodeID: 101, monitored: boolPtr(true), qualityCutoffNotMet: boolPtr(false)},
		}),
	}
	cc := runSonarrCrossCheck(context.Background(), logger, nil, inst, decisions, map[int]bool{100: true})

	if cc.status != crossCheckStatusFailed {
		t.Fatalf("status = %q, want %q: an episode of a would-unmonitor season being in the wanted set flatly contradicts rule 4:\n%s", cc.status, crossCheckStatusFailed, buf.String())
	}
	if !strings.Contains(buf.String(), "contains an episode in the wanted/cutoff set") {
		t.Errorf("expected the contradiction ERROR line:\n%s", buf.String())
	}
}

// TestEvaluateSeries_SeasonMonitoredAbsent_WarnNamesTheSeries pins the
// traceability minor: the season-level warnIfFieldAbsent emitted only
// instance/type/endpoint/field, so the warning could not be traced to a
// series at all — unlike the explicit missing-seasonNumber warn three lines
// below it, which names seriesId and series.
func TestEvaluateSeries_SeasonMonitoredAbsent_WarnNamesTheSeries(t *testing.T) {
	srv := sonarrEpisodeFileServer(t, `[]`, `[]`, nil, nil)
	defer srv.Close()
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Untraceable Warn Show", true, 1, []int{},
		seriesSeasonElement{SeasonNumber: intPtr(1), Statistics: &seasonStatisticsElement{EpisodeFileCount: intPtr(1), TotalEpisodeCount: intPtr(1)}})
	evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, false, map[seasonKey]bool{})

	// Isolate the season-monitored warn itself. The buffer also carries the
	// "monitored series produced no season decisions" warn, which DOES name
	// the series — asserting on the whole buffer would pass vacuously on that
	// line while the traceability hole stayed wide open.
	line := ""
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, "level=WARN") && strings.Contains(l, "season") && strings.Contains(l, "monitored") &&
			!strings.Contains(l, "produced no season decisions") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatalf("expected a WARN about the season's absent monitored field:\n%s", buf.String())
	}
	if !strings.Contains(line, "seriesId=1") || !strings.Contains(line, `series="Untraceable Warn Show"`) {
		t.Errorf("the season-level monitored-absent warn must name the series it belongs to, got %q", line)
	}
}

// TestRunSonarrCrossCheck_UnmonitoredEpisodeAboveCutoff_IsNotEvidence is the
// review-round reversal of a Phase 7 narrowing of shape (a), and the reason
// this file's shape (a) is whole again.
//
// Phase 7 narrowed shape (a) to "exclude only unmonitored episodes whose file
// says qualityCutoffNotMet == true", letting an unmonitored episode with
// qualityCutoffNotMet == false fall through to the final comparison. That
// comparison cannot fail: an unmonitored episode is filtered out of Sonarr's
// /wanted/cutoff by construction, so inWantedSet is false, and the comparison
// is `false != false` — an agreement that could never have been a
// disagreement. It still incremented writeVerified, which is exactly what the
// write gate reads to authorize writes for the WHOLE instance.
//
// A sample that verified nothing must never look like a sample that verified
// everything: this shape is counted in its own bucket, never as evidence.
// (Post-partial-write convergence is preserved by the write pass's separately
// gated RECOVERY path instead — see
// TestRunSonarrWritePass_RecoveryUnderInconclusiveGate_CompletesWhileTheRestIsWithheld.)
func TestRunSonarrCrossCheck_UnmonitoredEpisodeAboveCutoff_IsNotEvidence(t *testing.T) {
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr"}

	decisions := []seasonDecision{
		sonarrCandidateDecision(1, 1, true, []seasonCrossCheckEpisode{
			{episodeID: 100, monitored: boolPtr(false), qualityCutoffNotMet: boolPtr(false)},
		}),
	}
	cc := runSonarrCrossCheck(context.Background(), logger, nil, inst, decisions, map[int]bool{})

	if cc.writeVerified != 0 {
		t.Errorf("writeVerified = %d, want 0: a comparison that is structurally incapable of disagreeing is not evidence, and the write gate reads this number:\n%s", cc.writeVerified, buf.String())
	}
	if cc.verified != 0 || cc.unverifiable != 1 {
		t.Errorf("verified/unverifiable = %d/%d, want 0/1", cc.verified, cc.unverifiable)
	}
	if cc.status != crossCheckStatusInconclusive {
		t.Errorf("status = %q, want %q (nothing was verified):\n%s", cc.status, crossCheckStatusInconclusive, buf.String())
	}
	if strings.Contains(buf.String(), "level=ERROR") {
		t.Errorf("no disagreement may be manufactured here:\n%s", buf.String())
	}
	// The own bucket: a human reading the season line must be able to see WHY
	// nothing was comparable, not merely that nothing was.
	if !strings.Contains(buf.String(), "alreadyUnmonitoredEpisodes=1") {
		t.Errorf("the season aggregate must count the excluded unmonitored episodes in their own bucket:\n%s", buf.String())
	}
}

// TestRunSonarrDecisionEngine_SkipSideCrossCheck_RuleThreeSkip_BecomesVerifiable
// is the rule-4 test's missing twin (carried-forward test debt): rule 3's
// skip side sets rawEpisodesForCrossCheck for the same reason rule 4's does —
// an airing season never reaches rule 7, so evaluateSeries never fetches
// /episodefile for it — but only the rule-4 path was ever exercised, so the
// rule-3 assignment could have been deleted with the suite staying green.
func TestRunSonarrDecisionEngine_SkipSideCrossCheck_RuleThreeSkip_BecomesVerifiable(t *testing.T) {
	// Two episodes, one still to air: rule 3 skips the season, and the season
	// is still complete on disk so it IS skip-pool eligible.
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "," + episodeJSON(101, 1, 2, futureAirDate, 501) + "]"
	filesJSON := "[" + episodeFileJSON(500, 1, 200, false) + "," + episodeFileJSON(501, 1, 200, false) + "]"
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	series := []seriesElement{testSeries(1, "Airing Skip Show", true, 1, []int{}, testSeason(1, true, 2, 2))}
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

	out := buf.String()
	if !strings.Contains(out, `reason="unaired or undated episodes"`) {
		t.Fatalf("test setup did not actually produce a rule-3 skip:\n%s", out)
	}
	if !strings.Contains(out, `crossCheck="passed (1 verified, 0 unverifiable)"`) {
		t.Errorf("an airing skip must be made verifiable by the cross-check's own on-demand fetch, got:\n%s", out)
	}
	episodefileRequests := 0
	for _, r := range fake.all() {
		if r.path == "/api/v3/episodefile" {
			episodefileRequests++
		}
	}
	if episodefileRequests != 1 {
		t.Errorf("expected exactly 1 /episodefile request (the cross-check's on-demand fetch), got %d", episodefileRequests)
	}
}

// TestRunSonarrCrossCheck_OnDemandFetch_DeduplicatedPerSeries pins
// fetchedFilesBySeries: two sampled skip-side seasons of the SAME series must
// cost exactly one /episodefile request, not one each. Without the cache the
// bound on this cross-check's extra API calls would be "up to 10 fetches per
// instance per cycle" instead of "up to 10 SERIES".
func TestRunSonarrCrossCheck_OnDemandFetch_DeduplicatedPerSeries(t *testing.T) {
	filesJSON := "[" + episodeFileJSON(500, 1, 200, true) + "," + episodeFileJSON(600, 2, 200, true) + "]"
	fake := newSonarrEngineFake(t, "[]", filesJSON)
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	client := NewAPIClient(fake.srv.URL, "key")

	decisions := []seasonDecision{
		{seriesID: 1, series: "One Series", season: 1, completeOnDisk: true, reason: ReasonQualityCutoffNotMet,
			rawEpisodesForCrossCheck: []episodeElement{{ID: intPtr(100), Monitored: boolPtr(true), EpisodeFileID: intPtr(500)}}},
		{seriesID: 1, series: "One Series", season: 2, completeOnDisk: true, reason: ReasonQualityCutoffNotMet,
			rawEpisodesForCrossCheck: []episodeElement{{ID: intPtr(200), Monitored: boolPtr(true), EpisodeFileID: intPtr(600)}}},
	}
	cc := runSonarrCrossCheck(context.Background(), logger, client, fake.instance(), decisions, map[int]bool{100: true, 200: true})

	if cc.verified != 2 {
		t.Errorf("verified = %d, want 2 (both sampled seasons made verifiable by one fetch)", cc.verified)
	}
	requests := 0
	for _, r := range fake.all() {
		if r.path == "/api/v3/episodefile" {
			requests++
		}
	}
	if requests != 1 {
		t.Errorf("expected exactly 1 /episodefile request for two seasons of one series, got %d", requests)
	}
}

// TestRunSonarrCrossCheck_OnDemandFetchFails_TriedOnceAndInconclusive pins the
// other half of the same cache: a nil map value records "already tried and
// failed", so a failing series is not re-fetched once per sampled season, and
// the seasons it covers stay unverifiable rather than silently verified.
func TestRunSonarrCrossCheck_OnDemandFetchFails_TriedOnceAndInconclusive(t *testing.T) {
	mux := http.NewServeMux()
	requests := 0
	mux.HandleFunc("/api/v3/episodefile", func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(srv.URL, "key")

	decisions := []seasonDecision{
		{seriesID: 1, series: "Broken Files Show", season: 1, completeOnDisk: true, reason: ReasonQualityCutoffNotMet,
			rawEpisodesForCrossCheck: []episodeElement{{ID: intPtr(100), Monitored: boolPtr(true), EpisodeFileID: intPtr(500)}}},
		{seriesID: 1, series: "Broken Files Show", season: 2, completeOnDisk: true, reason: ReasonQualityCutoffNotMet,
			rawEpisodesForCrossCheck: []episodeElement{{ID: intPtr(200), Monitored: boolPtr(true), EpisodeFileID: intPtr(600)}}},
	}
	cc := runSonarrCrossCheck(context.Background(), logger, client, inst, decisions, map[int]bool{})

	if requests != 1 {
		t.Errorf("expected exactly 1 attempt for a failing series, got %d (a failed fetch must be cached, not retried per season)", requests)
	}
	if cc.status != crossCheckStatusInconclusive {
		t.Errorf("status = %q, want %q: nothing could be verified:\n%s", cc.status, crossCheckStatusInconclusive, buf.String())
	}
	if cc.unverifiable != 2 {
		t.Errorf("unverifiable = %d, want 2", cc.unverifiable)
	}
}

// TestBuildSeasonCrossCheckEpisodes_JoinsEachEpisodeToItsOwnFile exercises the
// join with two DISTINGUISHABLE files in one season — the case every existing
// test avoided by using a single file, so a join that returned "the first
// file" or "any file" would have passed the whole suite.
func TestBuildSeasonCrossCheckEpisodes_JoinsEachEpisodeToItsOwnFile(t *testing.T) {
	episodes := []episodeElement{
		{ID: intPtr(100), Monitored: boolPtr(true), EpisodeFileID: intPtr(500)},
		{ID: intPtr(101), Monitored: boolPtr(true), EpisodeFileID: intPtr(501)},
		{ID: intPtr(102), Monitored: boolPtr(true), EpisodeFileID: intPtr(999)}, // no such file
		{ID: intPtr(103), Monitored: boolPtr(true)},                             // no file id at all
	}
	filesByID := map[int]episodeFileElement{
		500: {ID: intPtr(500), QualityCutoffNotMet: boolPtr(false)},
		501: {ID: intPtr(501), QualityCutoffNotMet: boolPtr(true)},
	}

	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: "http://sonarr.invalid", APIKey: "key"}
	got, complete := buildSeasonCrossCheckEpisodes(logger, inst, 7, 1, episodes, filesByID)
	if !complete {
		t.Error("complete = false, want true: every episode here is nameable, so nothing was dropped")
	}
	if len(got) != 4 {
		t.Fatalf("expected 4 episodes, got %d: %+v", len(got), got)
	}
	if got[0].qualityCutoffNotMet == nil || *got[0].qualityCutoffNotMet {
		t.Errorf("episode 100 must carry file 500's qualityCutoffNotMet=false, got %v", got[0].qualityCutoffNotMet)
	}
	if got[1].qualityCutoffNotMet == nil || !*got[1].qualityCutoffNotMet {
		t.Errorf("episode 101 must carry file 501's qualityCutoffNotMet=true, got %v", got[1].qualityCutoffNotMet)
	}
	if got[2].qualityCutoffNotMet != nil {
		t.Errorf("an episode whose file id matches nothing must be unverifiable, got %v", got[2].qualityCutoffNotMet)
	}
	if got[3].qualityCutoffNotMet != nil {
		t.Errorf("an episode with no file id at all must be unverifiable, got %v", got[3].qualityCutoffNotMet)
	}
}

// TestRunSonarrDecisionEngine_UntrustedSeriesShapes_WarnAndSkipWithoutPanicking
// pins the engine's three nil guards at once (carried-forward test debt):
// deleting any of them turns a warn-and-skip into a nil dereference, and none
// of the three had a test.
func TestRunSonarrDecisionEngine_UntrustedSeriesShapes_WarnAndSkipWithoutPanicking(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "]"
	filesJSON := "[" + episodeFileJSON(500, 1, 200, false) + "]"
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	monitoredAbsent := seriesElement{ID: intPtr(1), Title: strPtr("No Monitored Field"), QualityProfileID: intPtr(1), Tags: &[]int{}, Seasons: &[]seriesSeasonElement{}}
	idAbsent := seriesElement{Title: strPtr("No Id"), Monitored: boolPtr(true), QualityProfileID: intPtr(1), Tags: &[]int{}, Seasons: &[]seriesSeasonElement{}}
	seasonNumberAbsent := seriesElement{ID: intPtr(3), Title: strPtr("Season Without Number"), Monitored: boolPtr(true), QualityProfileID: intPtr(1), Tags: &[]int{},
		Seasons: &[]seriesSeasonElement{{Monitored: boolPtr(true), Statistics: &seasonStatisticsElement{EpisodeFileCount: intPtr(1), TotalEpisodeCount: intPtr(1)}}}}

	runSonarrDecisionEngine(context.Background(), logger, fake.instance(),
		[]seriesElement{monitoredAbsent, idAbsent, seasonNumberAbsent},
		map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true)

	out := buf.String()
	for _, want := range []string{
		"endpoint=series field=monitored",
		"skipping series: missing id field",
		"skipping season: missing seasonNumber field",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected a warning containing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "msg=would-unmonitor") {
		t.Errorf("no untrusted-shaped series may reach a would-unmonitor decision:\n%s", out)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("nothing may be written for untrusted input, got %+v", writes)
	}
}

// TestBuildSeasonCrossCheckEpisodes_EpisodeWithoutID_WarnsAndReportsIncomplete
// clears DEFERRED DEBT from the Phase 7 branch review. An episode with no id
// was dropped silently, and the resulting set was then handed to the recovery
// gate (recoveryCandidate), which asks "is EVERY episode of this season already
// unmonitored?" — a question no incomplete set can answer. The drop is now
// warned about, exactly as every other structurally identical drop in this
// engine is, and the set says whether it is complete so its one dangerous
// consumer can refuse it.
func TestBuildSeasonCrossCheckEpisodes_EpisodeWithoutID_WarnsAndReportsIncomplete(t *testing.T) {
	episodes := []episodeElement{
		{ID: intPtr(100), Monitored: boolPtr(false), EpisodeFileID: intPtr(500)},
		{Monitored: boolPtr(true), EpisodeFileID: intPtr(501)}, // no id: unnameable
	}
	filesByID := map[int]episodeFileElement{500: {ID: intPtr(500), QualityCutoffNotMet: boolPtr(false)}}
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: "http://sonarr.invalid", APIKey: "key"}

	got, complete := buildSeasonCrossCheckEpisodes(logger, inst, 7, 1, episodes, filesByID)
	if len(got) != 1 {
		t.Fatalf("expected the one nameable episode, got %d: %+v", len(got), got)
	}
	if complete {
		t.Error("complete = true, want false: an episode of this season was dropped, so the set does not describe the whole season")
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "season=1") {
		t.Errorf("the drop must be warned about and name its season:\n%s", out)
	}
}

// TestBuildSeasonCrossCheckEpisodes_AllEpisodesNameable_ReportsComplete is the
// complement: nothing was dropped, so the set really does describe the season.
func TestBuildSeasonCrossCheckEpisodes_AllEpisodesNameable_ReportsComplete(t *testing.T) {
	episodes := []episodeElement{
		{ID: intPtr(100), Monitored: boolPtr(false), EpisodeFileID: intPtr(500)},
		{ID: intPtr(101), Monitored: boolPtr(false), EpisodeFileID: intPtr(501)},
	}
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: "http://sonarr.invalid", APIKey: "key"}

	got, complete := buildSeasonCrossCheckEpisodes(logger, inst, 7, 1, episodes, map[int]episodeFileElement{})
	if len(got) != 2 {
		t.Fatalf("expected 2 episodes, got %d", len(got))
	}
	if !complete {
		t.Errorf("complete = false, want true: no episode was dropped\n%s", buf.String())
	}
}
