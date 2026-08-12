package main

import (
	"context"
	"fmt"
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
// time (episodeHasAired compares against time.Now()), so no clock injection
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

// episodeFileJSON renders one /api/v3/episodefile array element.
func episodeFileJSON(id, season, cfScore int, qualityCutoffNotMet bool) string {
	return fmt.Sprintf(`{"id": %d, "seasonNumber": %d, "customFormatScore": %d, "qualityCutoffNotMet": %t}`, id, season, cfScore, qualityCutoffNotMet)
}

// --- evaluateSeries: series-level checks ------------------------------------

func TestEvaluateSeries_SeriesLevelTagExcluded_AllMonitoredSeasonsSkipped(t *testing.T) {
	srv := sonarrEpisodeFileServer(t, `[]`, `[]`, nil, nil)
	defer srv.Close()
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	s := testSeries(1, "Excluded Show", true, 1, []int{9}, testSeason(1, true, 10, 10), testSeason(2, true, 5, 5))
	eval := evaluateSeries(context.Background(), logger, client, inst, s, sonarrDecisionTestProfiles, 9, true, map[seasonKey]bool{})

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
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "field=monitored") {
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

func TestEpisodeHasAired_TableDriven(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		e    episodeElement
		want bool
	}{
		{"past date", episodeElement{AirDateUtc: strPtr("2020-01-01T00:00:00Z")}, true},
		{"future date", episodeElement{AirDateUtc: strPtr("2030-01-01T00:00:00Z")}, false},
		{"absent", episodeElement{}, false},
		{"unparseable", episodeElement{AirDateUtc: strPtr("garbage")}, false},
		{"exactly now is not before now", episodeElement{AirDateUtc: strPtr("2025-01-01T00:00:00Z")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := episodeHasAired(tc.e, now); got != tc.want {
				t.Errorf("episodeHasAired(%+v) = %v, want %v", tc.e, got, tc.want)
			}
		})
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

// sonarrDecisionEngineTestServer wires a read-only fake serving
// /api/v3/qualityprofile, /api/v3/tag, /api/v3/episode, and
// /api/v3/episodefile, recording every /episode and /episodefile request's
// raw query into the two pointer slices. Mirrors decisionEngineTestServer's
// (Radarr) write-refusal guarantee: any non-GET method to any registered
// path is answered 405, and a catch-all records every request to any OTHER
// path too — the structural half of the zero-write-verb pin (item 1 of the
// controller's carried-forward notes): Sonarr's decision engine has no
// write path at all in this phase, and this fake makes "it never even
// tried" a checkable fact rather than an assumption.
type sonarrEngineFake struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []recordedRequest

	profilesJSON string
	tagsJSON     string
	episodeJSON  string
	fileJSON     string
}

func newSonarrEngineFake(t *testing.T, episodeJSON, fileJSON string) *sonarrEngineFake {
	t.Helper()
	f := &sonarrEngineFake{
		profilesJSON: sonarrDecisionProfilesJSON,
		tagsJSON:     sonarrDecisionNoTagsJSON,
		episodeJSON:  episodeJSON,
		fileJSON:     fileJSON,
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
		w.Write([]byte(f.episodeJSON))
	}))
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
		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{method: r.Method, path: r.URL.Path})
		f.mu.Unlock()
		next(w, r)
	}
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
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude")

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
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude")

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
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude")

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
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude")

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
	runSonarrDecisionEngine(context.Background(), logger, inst, series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude")

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
	runSonarrDecisionEngine(context.Background(), logger, inst, series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude")

	out := buf.String()
	if strings.Contains(out, "msg=would-unmonitor") || strings.Contains(out, "msg=skip") {
		t.Errorf("a failed tag fetch must skip the whole instance: no report lines at all:\n%s", out)
	}
}

func TestRunSonarrDecisionEngine_SeriesNotMonitored_ExcludedEntirelyFromReport(t *testing.T) {
	fake := newSonarrEngineFake(t, `[]`, `[]`)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	series := []seriesElement{testSeries(1, "Unmonitored Show", false, 1, []int{}, testSeason(1, true, 1, 1))}
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude")

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
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude")

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
	_ = buf
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

func TestRunSonarrDecisionEngine_CrossCheckDisagreement_SummaryStatesFailed(t *testing.T) {
	// Series 1 has an episode in the wanted set (rule 4 fails, skip
	// reason=quality cutoff not met) whose file claims qualityCutoffNotMet
	// is false — a genuine disagreement once this season becomes a
	// candidate. To reach the cross-check with crossCheckEpisodes populated,
	// the season must actually become a rule-7 candidate; use two seasons in
	// the same series so the second's cutoff-met candidacy triggers the
	// /episodefile fetch that also covers season 1's (already-decided) data.
	//
	// Simpler and sufficient: build the decisions directly and drive
	// runSonarrDecisionEngine's summary rendering via a small series/episode
	// set that reaches candidacy, then assert the rendered summary states
	// FAILED once a disagreement is deliberately engineered into the fixture.
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "]"
	filesJSON := "[" + episodeFileJSON(500, 1, 200, true) + "]" // qualityCutoffNotMet=true but NOT in wanted set: disagreement
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	series := []seriesElement{testSeries(1, "Disagreeing Show", true, 1, []int{}, testSeason(1, true, 1, 1))}
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude")

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
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, wantedEpisodeIDs, wantedSeasons, "cutoffarr-exclude")

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
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, wantedEpisodeIDs, wantedSeasons, "cutoffarr-exclude")

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
