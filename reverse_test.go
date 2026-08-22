package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// reverse_test.go covers the Phase 10 reverse scan: the same decision function
// run over the unmonitored population, reporting the items that are
// unmonitored while still FAILING the criteria.
//
// The test fixtures here are deliberately built out of the SAME helpers the
// forward tests use, with monitored flipped: if the reverse scan needed its own
// fixture vocabulary, that would be evidence it had grown its own rules, which
// is the one thing this phase is not allowed to do.

// --- fixtures ---------------------------------------------------------------

// unmonitoredBelowCutoffMovie is the reverse scan's headline case: a movie
// somebody unmonitored even though its file is still below the quality cutoff
// (it is in the UNMONITORED wanted set). Its own movieFile.qualityCutoffNotMet
// agrees, which is what the forward cross-check would say about it too.
func unmonitoredBelowCutoffMovie(id int, title string) movieListElement {
	return movieListElement{
		ID: intPtr(id), Title: strPtr(title), Monitored: boolPtr(false), HasFile: boolPtr(true),
		QualityProfileID: intPtr(1), Tags: &noTags,
		MovieFile: &movieFileElement{ID: intPtr(1), QualityCutoffNotMet: boolPtr(true)},
	}
}

// unmonitoredMeetingCriteriaMovie is the ordinary, healthy state of an
// unmonitored movie: it meets the criteria, so cutoffarr itself would have
// unmonitored it, and the reverse scan must say nothing at all about it.
func unmonitoredMeetingCriteriaMovie(id int, title string) movieListElement {
	return movieListElement{
		ID: intPtr(id), Title: strPtr(title), Monitored: boolPtr(false), HasFile: boolPtr(true),
		QualityProfileID: intPtr(1), Tags: &noTags,
		MovieFile: &movieFileElement{ID: intPtr(1), QualityCutoffNotMet: boolPtr(false)},
	}
}

// reverseScanOn is the report-only reverse scan every full-library cycle runs:
// enabled, with the write switch off, which is the default configuration.
func reverseScanOn() reverseOptions { return reverseOptions{enabled: true} }

// reverseFindingLine returns the single reverse-scan finding line in out, or
// fails. reportLineWithMsg (sonarr_decision_test.go) cannot be reused here: it
// matches a whitespace-delimited msg= field, and slog quotes a message
// containing a space.
func reverseFindingLine(t *testing.T, out string) string {
	t.Helper()
	var found string
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, `msg="reverse-scan finding"`) {
			continue
		}
		if found != "" {
			t.Fatalf("expected exactly one reverse-scan finding line:\n%s", out)
		}
		found = line
	}
	if found == "" {
		t.Fatalf("expected a reverse-scan finding line:\n%s", out)
	}
	return found
}

// --- Radarr: what is and is not a finding -----------------------------------

// TestRunRadarrDecisionEngine_ReverseScan_ReportsUnmonitoredBelowCutoff is the
// plan's own acceptance shape: an unmonitored movie that is still below its
// cutoff is reported, with the reason it fails, and counted on the summary.
func TestRunRadarrDecisionEngine_ReverseScan_ReportsUnmonitoredBelowCutoff(t *testing.T) {
	fake := newRadarrFake(t, "", nil)
	// The UNMONITORED wanted set — a different response from the forward one,
	// exactly as the live endpoint gives (3 records against 131).
	fake.reverseWantedJSON = `{"page":1,"pageSize":100,"totalRecords":1,"records":[{"id":7,"title":"Accidentally Unmonitored"}]}`

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{unmonitoredBelowCutoffMovie(7, "Accidentally Unmonitored")}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseScanOn(), fileReportOptions{})

	out := buf.String()
	if !strings.Contains(out, `msg="reverse-scan finding"`) {
		t.Fatalf("expected a reverse-scan finding line:\n%s", out)
	}
	for _, want := range []string{"id=7", `title="Accidentally Unmonitored"`, `reason="` + ReasonQualityCutoffNotMet + `"`, "profile=HD-1080p", "instance=radarr-main"} {
		if !strings.Contains(out, want) {
			t.Errorf("the finding must carry %s:\n%s", want, out)
		}
	}
	if c := summaryCounters(t, out); c["reverseFindings"] != 1 {
		t.Errorf("reverseFindings = %d, want 1:\n%s", c["reverseFindings"], out)
	}
}

// TestRunRadarrDecisionEngine_ReverseScan_ReportsUnmonitoredBelowCFScore is the
// other half of "fails the criteria": absent from the unmonitored wanted set
// (so its quality cutoff IS met) but scoring below the profile's custom-format
// cutoff. The reason is the existing constant, not a reverse-specific one.
func TestRunRadarrDecisionEngine_ReverseScan_ReportsUnmonitoredBelowCFScore(t *testing.T) {
	fake := newRadarrFake(t, "", nil)
	fake.cfScore = 10 // the profile's cutoffFormatScore is 100

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{unmonitoredMeetingCriteriaMovie(7, "Low Score")}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseScanOn(), fileReportOptions{})

	out := buf.String()
	if !strings.Contains(out, `msg="reverse-scan finding"`) || !strings.Contains(out, `reason="`+ReasonCFCutoffNotMet+`"`) {
		t.Fatalf("expected a reverse-scan finding with reason %q:\n%s", ReasonCFCutoffNotMet, out)
	}
}

// TestRunRadarrDecisionEngine_ReverseScan_ItemMeetingCriteriaIsNotReported is
// the plan's explicit non-finding: "an unmonitored item that passes criteria is
// normal (that's Cutoffarr's own output) and is not reported". Every movie this
// project has ever unmonitored is in that state, so getting this wrong would
// report the whole history of the tool's own work as a problem, every cycle.
func TestRunRadarrDecisionEngine_ReverseScan_ItemMeetingCriteriaIsNotReported(t *testing.T) {
	fake := newRadarrFake(t, "", nil)

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{unmonitoredMeetingCriteriaMovie(7, "Correctly Unmonitored")}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseScanOn(), fileReportOptions{})

	out := buf.String()
	if strings.Contains(out, "reverse-scan finding") {
		t.Errorf("a movie that meets the criteria must not be reported:\n%s", out)
	}
	if c := summaryCounters(t, out); c["reverseFindings"] != 0 {
		t.Errorf("reverseFindings = %d, want 0:\n%s", c["reverseFindings"], out)
	}
}

// TestRunRadarrDecisionEngine_ReverseScan_NoFileMovieIsNotAFinding pins binding
// controller resolution 2: an unmonitored movie with no file is a deliberate
// user choice (that is what "unmonitored, not downloaded" means), so it is
// counted at debug and never reported as a finding.
func TestRunRadarrDecisionEngine_ReverseScan_NoFileMovieIsNotAFinding(t *testing.T) {
	fake := newRadarrFake(t, "", nil)

	logger, buf := newDecisionTestLogger(slog.LevelDebug)
	movies := []movieListElement{{
		ID: intPtr(7), Title: strPtr("Never Downloaded"), Monitored: boolPtr(false), HasFile: boolPtr(false),
		QualityProfileID: intPtr(1), Tags: &noTags,
	}}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseScanOn(), fileReportOptions{})

	out := buf.String()
	if strings.Contains(out, "reverse-scan finding") {
		t.Errorf("an unmonitored movie with no file must never be a finding:\n%s", out)
	}
	if c := summaryCounters(t, out); c["reverseFindings"] != 0 {
		t.Errorf("reverseFindings = %d, want 0:\n%s", c["reverseFindings"], out)
	}
	if !strings.Contains(out, "level=DEBUG") || !strings.Contains(out, "reverseNoFile=1") {
		t.Errorf("the pool statistic must still be visible at debug:\n%s", out)
	}
}

// TestRunRadarrDecisionEngine_ReverseScan_ExcludedByTagIsNeverAFinding pins
// §2.5 in the reverse direction, at its strongest reading (binding controller
// resolution 4): an excluded item is not merely never written, it is never even
// REPORTED. The tag means "leave this alone", and a line telling a human their
// excluded movie is below cutoff is not leaving it alone.
func TestRunRadarrDecisionEngine_ReverseScan_ExcludedByTagIsNeverAFinding(t *testing.T) {
	fake := newRadarrFake(t, "", nil)
	fake.tagsJSON = `[{"id": 42, "label": "cutoffarr-exclude"}]`
	fake.reverseWantedJSON = `{"page":1,"pageSize":100,"totalRecords":1,"records":[{"id":7,"title":"Excluded"}]}`

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	excludedTags := []int{42}
	movies := []movieListElement{{
		ID: intPtr(7), Title: strPtr("Excluded"), Monitored: boolPtr(false), HasFile: boolPtr(true),
		QualityProfileID: intPtr(1), Tags: &excludedTags,
		MovieFile: &movieFileElement{ID: intPtr(1), QualityCutoffNotMet: boolPtr(true)},
	}}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseScanOn(), fileReportOptions{})

	out := buf.String()
	if strings.Contains(out, "reverse-scan finding") || strings.Contains(out, "Excluded") {
		t.Errorf("an excluded movie must not appear in the reverse report at all:\n%s", out)
	}
}

// TestRunRadarrDecisionEngine_ReverseScan_MonitoredMoviesAreNeverFindings pins
// the population boundary from the other side. A monitored movie below its
// cutoff is the forward scan's ordinary skip; reporting it here would turn most
// of a healthy library into findings.
func TestRunRadarrDecisionEngine_ReverseScan_MonitoredMoviesAreNeverFindings(t *testing.T) {
	fake := newRadarrFake(t, "", nil)
	fake.reverseWantedJSON = `{"page":1,"pageSize":100,"totalRecords":1,"records":[{"id":7,"title":"Monitored Below Cutoff"}]}`

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{{
		ID: intPtr(7), Title: strPtr("Monitored Below Cutoff"), Monitored: boolPtr(true), HasFile: boolPtr(true),
		QualityProfileID: intPtr(1), Tags: &noTags,
		MovieFile: &movieFileElement{ID: intPtr(1), QualityCutoffNotMet: boolPtr(true)},
	}}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{7: true}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseScanOn(), fileReportOptions{})

	out := buf.String()
	if strings.Contains(out, "reverse-scan finding") {
		t.Errorf("a monitored movie must never be a reverse finding:\n%s", out)
	}
}

// TestRunRadarrDecisionEngine_ReverseScan_IncompleteWantedSet_SkipsOnlyTheReversePass
// is the completeness contract in the direction where it is most dangerous. An
// unmonitored wanted set short of what totalRecords claimed cannot say which
// movies are below cutoff, so every "absent from the set" conclusion drawn from
// it is a guess — one that manufactures FALSE FINDINGS (and, with the remonitor
// flag on, wrong writes). The reverse pass is skipped with a warning; the
// forward pass, which never touched that set, is untouched and still reports.
func TestRunRadarrDecisionEngine_ReverseScan_IncompleteWantedSet_SkipsOnlyTheReversePass(t *testing.T) {
	fake := newRadarrFake(t, "", nil)
	fake.reverseWantedJSON = `{"page":1,"pageSize":100,"totalRecords":50,"records":[]}` // claims 50, returns 0

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{
		wouldUnmonitorMovie(1, "Forward Candidate"),
		unmonitoredBelowCutoffMovie(7, "Reverse Candidate"),
	}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseScanOn(), fileReportOptions{})

	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "skipping the reverse scan for this instance") {
		t.Errorf("an incomplete unmonitored wanted set must warn and skip the reverse pass:\n%s", out)
	}
	if strings.Contains(out, "reverse-scan finding") {
		t.Errorf("no finding may be reported from an incomplete set:\n%s", out)
	}
	// The forward half is untouched: it still decided, reported and counted.
	if !strings.Contains(out, `msg=would-unmonitor`) {
		t.Errorf("the forward pass must be unaffected by a reverse-scan data failure:\n%s", out)
	}
	if c := summaryCounters(t, out); c["wouldUnmonitor"] != 1 {
		t.Errorf("wouldUnmonitor = %d, want 1:\n%s", c["wouldUnmonitor"], out)
	}
	// And the summary must not claim zero findings, which is a statement the
	// cycle is in no position to make.
	if strings.Contains(out, "reverseFindings=") {
		t.Errorf("a skipped reverse pass must not report a finding count:\n%s", out)
	}
	if !strings.Contains(out, "reverseScan=skipped") {
		t.Errorf("a skipped reverse pass must say so on the summary line:\n%s", out)
	}
}

// TestRunRadarrDecisionEngine_ReverseScanDisabled_MakesNoUnmonitoredWantedFetch
// pins the scheduling rule at the engine's own boundary: when the cycle did not
// ask for a reverse scan, the extra fetch does not happen and the summary says
// nothing about it. The scoped cycles that set this — webhooks and --only-id —
// are pinned end to end elsewhere.
func TestRunRadarrDecisionEngine_ReverseScanDisabled_MakesNoUnmonitoredWantedFetch(t *testing.T) {
	fake := newRadarrFake(t, "", nil)

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{unmonitoredBelowCutoffMovie(7, "Accidentally Unmonitored")}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseOptions{}, fileReportOptions{})

	if n := fake.countRequests("/api/v3/wanted/cutoff"); n != 0 {
		t.Errorf("the engine made %d wanted/cutoff request(s) with the reverse pass disabled, want 0", n)
	}
	out := buf.String()
	if strings.Contains(out, "reverse") {
		t.Errorf("a cycle that ran no reverse pass must say nothing about one:\n%s", out)
	}
}

// TestRunRadarrDecisionEngine_ReverseScan_FindingsFollowTheCycleReportLevel is
// the Phase 8 noise budget applied to the new lines (binding controller
// resolution 9). Findings repeat every cycle by design — they stay true until a
// human acts — so on a repeating cycle they are DEBUG, exactly like the
// would-unmonitor and skip lines beside them, and the summary is what remains
// at INFO.
func TestRunRadarrDecisionEngine_ReverseScan_FindingsFollowTheCycleReportLevel(t *testing.T) {
	fake := newRadarrFake(t, "", nil)
	fake.reverseWantedJSON = `{"page":1,"pageSize":100,"totalRecords":1,"records":[{"id":7,"title":"Accidentally Unmonitored"}]}`

	logger, buf := newDecisionTestLogger(slog.LevelDebug)
	movies := []movieListElement{unmonitoredBelowCutoffMovie(7, "Accidentally Unmonitored")}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelDebug), true, reverseScanOn(), fileReportOptions{})

	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "reverse-scan finding") && !strings.Contains(line, "level=DEBUG") {
			t.Errorf("on a repeating cycle a finding must be DEBUG:\n%s", line)
		}
	}
	if c := summaryCounters(t, buf.String()); c["reverseFindings"] != 1 {
		t.Errorf("the summary still counts it at INFO: reverseFindings = %d, want 1:\n%s", c["reverseFindings"], buf.String())
	}
}

// TestReverseFindingReasons_AreTheProjectsExistingConstants pins the report
// vocabulary itself. The reverse scan reports exactly the two ways an item can
// still FAIL the criteria, in the words the forward scan already uses for them;
// every other outcome is either healthy, a deliberate user choice, or untrusted
// input, and none of those is something to tell a human about.
func TestReverseFindingReasons_AreTheProjectsExistingConstants(t *testing.T) {
	findings := []string{ReasonQualityCutoffNotMet, ReasonCFCutoffNotMet, ReasonSeasonMonitorMismatch}
	notFindings := []string{
		ReasonCutoffMet, ReasonNoFile, ReasonExcludedByTag, ReasonTagsUnknown,
		ReasonUnknownProfile, ReasonUpgradesDisabled, ReasonCouldNotFetchCFScore,
		ReasonSeasonIncomplete, ReasonSeasonNotFullyAired, ReasonSeasonEpisodeDataInconsistent,
		ReasonSeasonFileCountMismatch, ReasonSeasonEpisodesUnavailable, "",
	}
	for _, r := range findings {
		if !isReverseFinding(r) {
			t.Errorf("%q must be a reverse finding: it is one of the two ways an item still fails the criteria", r)
		}
	}
	for _, r := range notFindings {
		if isReverseFinding(r) {
			t.Errorf("%q must not be a reverse finding", r)
		}
	}
}

// TestReverseScan_EveryReasonConstantIsDeliberatelyClassified is the guard on
// the list above, and it is a source audit for the same reason the write-verb
// count is one: the property has to hold for reasons nobody has written yet.
//
// isReverseFinding's default case answers "not a finding", which is the safe
// direction for every reason that exists today and the WRONG one for a future
// reason that means "still below its cutoff, in some new way" — that reason
// would silently stop being reported, with nothing failing anywhere. So every
// Reason* constant declared in decision.go must be named in the census below,
// and a new one fails this test until somebody decides which side it is on.
func TestReverseScan_EveryReasonConstantIsDeliberatelyClassified(t *testing.T) {
	census := map[string]bool{
		"ReasonQualityCutoffNotMet":           true, // finding
		"ReasonCFCutoffNotMet":                true, // finding
		"ReasonSeasonMonitorMismatch":         true, // finding (Sonarr only)
		"ReasonNoFile":                        false,
		"ReasonUnknownProfile":                false,
		"ReasonUpgradesDisabled":              false,
		"ReasonTagsUnknown":                   false,
		"ReasonExcludedByTag":                 false,
		"ReasonCouldNotFetchCFScore":          false,
		"ReasonCutoffMet":                     false,
		"ReasonSeasonIncomplete":              false,
		"ReasonSeasonNotFullyAired":           false,
		"ReasonSeasonEpisodeDataInconsistent": false,
		"ReasonSeasonFileCountMismatch":       false,
		"ReasonSeasonEpisodesUnavailable":     false,
	}

	declared := declaredReasonConstants(t)
	for _, name := range declared {
		if _, named := census[name]; !named {
			t.Errorf("%s is not classified by this test: decide whether the reverse scan should report it as a finding, then add it to the census and to isReverseFinding", name)
		}
	}
	if len(declared) != len(census) {
		t.Errorf("decision.go declares %d Reason* constants but the census names %d; the census must be exhaustive", len(declared), len(census))
	}
}

// declaredReasonConstants is the source audit both censuses read: every
// Reason* constant decision.go declares, by name. Shared so the two
// classifications the reverse scan makes of a reason — is it a finding, and is
// it untrusted input — are both held to the same exhaustiveness, against one
// pattern that can only stop matching in one place.
func declaredReasonConstants(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("decision.go")
	if err != nil {
		t.Fatalf("reading decision.go: %v", err)
	}
	matches := regexp.MustCompile(`(?m)^\t(Reason[A-Za-z]+)\s+=\s+"`).FindAllStringSubmatch(string(src), -1)
	if len(matches) == 0 {
		t.Fatal("found no Reason* constants in decision.go; this audit's pattern has stopped matching the source it audits")
	}
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, m[1])
	}
	return names
}

// --- Sonarr -----------------------------------------------------------------

// TestRunSonarrDecisionEngine_ReverseScan_ReportsUnmonitoredSeasonBelowCutoff is
// the Sonarr headline: an unmonitored season of a MONITORED series, complete on
// disk and fully aired, still below its quality cutoff. The finding names the
// season the way every other Sonarr line does, and says what state its series
// is in — which is the fact that decides whether it may ever be written.
func TestRunSonarrDecisionEngine_ReverseScan_ReportsUnmonitoredSeasonBelowCutoff(t *testing.T) {
	episodesJSON := "[" + episodeJSON(200, 2, 1, pastAirDate, 600) + "]"
	filesJSON := "[" + episodeFileJSON(600, 2, 200, true) + "]"
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	fake.reverseWantedJSON = `{"page":1,"pageSize":100,"totalRecords":1,"records":[{"id":200,"seriesId":1,"seasonNumber":2}]}`

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	series := []seriesElement{testSeries(1, "Accidentally Unmonitored Season", true, 1, []int{}, testSeason(2, false, 1, 1))}

	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseScanOn(), fileReportOptions{})

	out := buf.String()
	line := reverseFindingLine(t, out)
	for _, want := range []string{"instance=sonarr-main", "seriesId=1", `series="Accidentally Unmonitored Season"`, "season=2",
		`reason="` + ReasonQualityCutoffNotMet + `"`, "profile=HD-1080p", "seriesMonitored=true"} {
		if !strings.Contains(line, want) {
			t.Errorf("the finding must carry %s:\n%s", want, line)
		}
	}
	if strings.Contains(line, " id=") || strings.Contains(line, " title=") {
		t.Errorf("a season finding must not carry movie-style id/title attrs:\n%s", line)
	}
	if c := summaryCountersFor(t, out, "sonarr decision summary"); c["reverseFindings"] != 1 {
		t.Errorf("reverseFindings = %d, want 1:\n%s", c["reverseFindings"], out)
	}
}

// TestRunSonarrDecisionEngine_ReverseScan_UnmonitoredSeriesIsReportedWithTheAttr
// pins binding controller resolution 3's reporting half: a season under an
// UNMONITORED series is still reported — the human retired the show, and a
// below-cutoff season inside it is still a fact worth knowing — and it carries
// seriesMonitored=false, which is the attr the write half keys on.
func TestRunSonarrDecisionEngine_ReverseScan_UnmonitoredSeriesIsReportedWithTheAttr(t *testing.T) {
	episodesJSON := "[" + episodeJSON(200, 2, 1, pastAirDate, 600) + "]"
	filesJSON := "[" + episodeFileJSON(600, 2, 200, true) + "]"
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	fake.reverseWantedJSON = `{"page":1,"pageSize":100,"totalRecords":1,"records":[{"id":200,"seriesId":1,"seasonNumber":2}]}`

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	series := []seriesElement{testSeries(1, "Retired Show", false, 1, []int{}, testSeason(2, false, 1, 1))}

	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseScanOn(), fileReportOptions{})

	out := buf.String()
	line := reverseFindingLine(t, out)
	if !strings.Contains(line, "seriesMonitored=false") {
		t.Errorf("a finding under an unmonitored series must say so:\n%s", line)
	}
	if c := summaryCountersFor(t, out, "sonarr decision summary"); c["reverseFindings"] != 1 {
		t.Errorf("reverseFindings = %d, want 1:\n%s", c["reverseFindings"], out)
	}
}

// TestRunSonarrDecisionEngine_ReverseScan_MonitorMismatch_IsReported is the
// third way a Sonarr season can be a finding, and the only one with no Radarr
// twin: the season's own flag says unmonitored while an episode inside it says
// monitored.
//
// It is reported because it is the one state nothing else in this program can
// see. The forward pass excludes the season at rule 1, so those monitored
// episodes are searched and upgraded by Sonarr forever with no cutoff
// enforcement — and before this fix the reverse pass called the season "cutoff
// met" and said nothing at all, at any level.
//
// The negative case is in the same test on purpose: a season this project
// unmonitored properly (flag AND episodes) meets the criteria and must stay
// silent, because that describes most of a mature library.
func TestRunSonarrDecisionEngine_ReverseScan_MonitorMismatch_IsReported(t *testing.T) {
	// Complete, fully aired, absent from the unmonitored wanted set and scoring
	// 200 against the profile's 100: the season meets every criterion, so the
	// episode's own monitored flag is the only thing this test varies.
	filesJSON := "[" + episodeFileJSON(600, 2, 200, false) + "]"

	t.Run("an episode is still monitored inside it", func(t *testing.T) {
		fake := newSonarrEngineFake(t, "["+episodeJSON(200, 2, 1, pastAirDate, 600)+"]", filesJSON)

		logger, buf := newDecisionTestLogger(slog.LevelInfo)
		series := []seriesElement{testSeries(1, "Half Written Show", true, 1, []int{}, testSeason(2, false, 1, 1))}
		runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{},
			"cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseScanOn(), fileReportOptions{})

		out := buf.String()
		line := reverseFindingLine(t, out)
		for _, want := range []string{"seriesId=1", "season=2", `reason="` + ReasonSeasonMonitorMismatch + `"`} {
			if !strings.Contains(line, want) {
				t.Errorf("the finding must carry %s:\n%s", want, line)
			}
		}
		if c := summaryCountersFor(t, out, "sonarr decision summary"); c["reverseFindings"] != 1 {
			t.Errorf("reverseFindings = %d, want 1:\n%s", c["reverseFindings"], out)
		}
	})

	t.Run("its episodes are unmonitored too, which is this project's own output", func(t *testing.T) {
		fake := newSonarrEngineFake(t, "["+episodeJSONWithMonitored(200, 2, 1, pastAirDate, 600, false)+"]", filesJSON)

		logger, buf := newDecisionTestLogger(slog.LevelInfo)
		series := []seriesElement{testSeries(1, "Correctly Unmonitored Show", true, 1, []int{}, testSeason(2, false, 1, 1))}
		runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{},
			"cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseScanOn(), fileReportOptions{})

		out := buf.String()
		if strings.Contains(out, "reverse-scan finding") {
			t.Errorf("a season whose flag and episodes agree meets the criteria and must not be reported:\n%s", out)
		}
		if c := summaryCountersFor(t, out, "sonarr decision summary"); c["reverseFindings"] != 0 {
			t.Errorf("reverseFindings = %d, want 0:\n%s", c["reverseFindings"], out)
		}
	})
}

// TestRunSonarrDecisionEngine_ReverseScan_MonitoredSeasonsAreNeverFindings is
// the population boundary: the forward pass owns monitored seasons, and a
// monitored season below its cutoff is its ordinary skip.
func TestRunSonarrDecisionEngine_ReverseScan_MonitoredSeasonsAreNeverFindings(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "]"
	filesJSON := "[" + episodeFileJSON(500, 1, 200, true) + "]"
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	fake.reverseWantedJSON = `{"page":1,"pageSize":100,"totalRecords":1,"records":[{"id":100,"seriesId":1,"seasonNumber":1}]}`

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	series := []seriesElement{testSeries(1, "Ordinary Show", true, 1, []int{}, testSeason(1, true, 1, 1))}
	wanted := map[seasonKey]bool{{seriesID: 1, seasonNumber: 1}: true}

	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{100: true}, wanted, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseScanOn(), fileReportOptions{})

	out := buf.String()
	if strings.Contains(out, "reverse-scan finding") {
		t.Errorf("a monitored season must never be a reverse finding:\n%s", out)
	}
}

// TestRunSonarrDecisionEngine_ReverseScan_IncompleteWantedSet_SkipsOnlyTheReversePass
// is the Sonarr half of the completeness contract, with the same consequence:
// no findings from a set that cannot be trusted, and a forward pass that
// carries on untouched.
func TestRunSonarrDecisionEngine_ReverseScan_IncompleteWantedSet_SkipsOnlyTheReversePass(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "," + episodeJSON(200, 2, 1, pastAirDate, 600) + "]"
	filesJSON := "[" + episodeFileJSON(500, 1, 200, false) + "," + episodeFileJSON(600, 2, 200, true) + "]"
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	fake.reverseWantedJSON = `{"page":1,"pageSize":100,"totalRecords":50,"records":[]}`

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	series := []seriesElement{testSeries(1, "Mixed Show", true, 1, []int{}, testSeason(1, true, 1, 1), testSeason(2, false, 1, 1))}

	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseScanOn(), fileReportOptions{})

	out := buf.String()
	if !strings.Contains(out, "skipping the reverse scan for this instance") {
		t.Errorf("an incomplete unmonitored wanted set must warn and skip the reverse pass:\n%s", out)
	}
	if strings.Contains(out, "reverse-scan finding") {
		t.Errorf("no finding may be reported from an incomplete set:\n%s", out)
	}
	if c := summaryCountersFor(t, out, "sonarr decision summary"); c["wouldUnmonitor"] != 1 {
		t.Errorf("the forward pass must be unaffected: wouldUnmonitor = %d, want 1:\n%s", c["wouldUnmonitor"], out)
	}
	if !strings.Contains(out, "reverseScan=skipped") {
		t.Errorf("a skipped reverse pass must say so on the summary line:\n%s", out)
	}
}

// TestRunSonarrDecisionEngine_ReverseScan_FindingsFollowTheCycleReportLevel is
// the Sonarr twin of the Radarr level pin above (binding controller resolution
// 9), and it needs to exist separately because the Sonarr finding is a SEPARATE
// LOG STATEMENT: the Radarr pin — and the daemon noise-budget pin beside it —
// both run Radarr only, so nothing anywhere failed when this call site printed
// at INFO.
//
// The cost of that regression is not symmetric between the two engines. The live
// Sonarr's unmonitored wanted set holds 1213 records against Radarr's 3, so an
// INFO finding line here means one line per accidental unmonitor, per season, on
// every sweep, forever — precisely what the noise budget exists to prevent, on
// the larger of the two libraries.
func TestRunSonarrDecisionEngine_ReverseScan_FindingsFollowTheCycleReportLevel(t *testing.T) {
	episodesJSON := "[" + episodeJSON(200, 2, 1, pastAirDate, 600) + "]"
	filesJSON := "[" + episodeFileJSON(600, 2, 200, true) + "]"
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	fake.reverseWantedJSON = `{"page":1,"pageSize":100,"totalRecords":1,"records":[{"id":200,"seriesId":1,"seasonNumber":2}]}`

	logger, buf := newDecisionTestLogger(slog.LevelDebug)
	series := []seriesElement{testSeries(1, "Accidentally Unmonitored Season", true, 1, []int{}, testSeason(2, false, 1, 1))}

	// A repeating cycle: everything that scales with the library is demoted.
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{},
		"cutoffarr-exclude", fullLibraryScope(slog.LevelDebug), true, reverseScanOn(), fileReportOptions{})

	out := buf.String()
	found := false
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "reverse-scan finding") {
			continue
		}
		found = true
		if !strings.Contains(line, "level=DEBUG") {
			t.Errorf("on a repeating cycle a season finding must be DEBUG:\n%s", line)
		}
	}
	if !found {
		t.Fatalf("this test proves nothing unless a finding was made:\n%s", out)
	}
	if c := summaryCountersFor(t, out, "sonarr decision summary"); c["reverseFindings"] != 1 {
		t.Errorf("the summary still counts it at INFO: reverseFindings = %d, want 1:\n%s", c["reverseFindings"], out)
	}
}

// TestRunSonarrDecisionEngine_ReverseScanDisabled_MakesNoUnmonitoredWantedFetch
// is the Sonarr twin of the Radarr scheduling pin.
func TestRunSonarrDecisionEngine_ReverseScanDisabled_MakesNoUnmonitoredWantedFetch(t *testing.T) {
	episodesJSON := "[" + episodeJSON(200, 2, 1, pastAirDate, 600) + "]"
	filesJSON := "[" + episodeFileJSON(600, 2, 200, true) + "]"
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	series := []seriesElement{testSeries(1, "Some Show", true, 1, []int{}, testSeason(2, false, 1, 1))}

	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseOptions{}, fileReportOptions{})

	if n := fake.countRequests("/api/v3/wanted/cutoff"); n != 0 {
		t.Errorf("the engine made %d wanted/cutoff request(s) with the reverse pass disabled, want 0", n)
	}
	if strings.Contains(buf.String(), "reverse") {
		t.Errorf("a cycle that ran no reverse pass must say nothing about one:\n%s", buf.String())
	}
}

// --- the remonitor write path (Phase 10) ------------------------------------
//
// Everything below is about the flag. Report-only is the default and the
// headline pin is that a write-mode config with a qualifying finding composes
// no write at all with the flag off — not "sends no PUT", but never enters the
// write path in the first place, which is checkable because the write path's
// own fresh pre-write GET would show up in the fake's request log.

// writeReverseTestConfig writes a radarr config with dry_run and
// reverse_scan_remonitor set explicitly.
func writeReverseTestConfig(t *testing.T, url string, dryRun, remonitor bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	content := fmt.Sprintf(`
dry_run: %t
reverse_scan_remonitor: %t
instances:
  - name: radarr-main
    type: radarr
    url: %s
    api_key: key1
`, dryRun, remonitor, url)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	return path
}

// writeReverseSonarrTestConfig is its Sonarr twin.
func writeReverseSonarrTestConfig(t *testing.T, url string, dryRun, remonitor bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	content := fmt.Sprintf(`
dry_run: %t
reverse_scan_remonitor: %t
instances:
  - name: sonarr-main
    type: sonarr
    url: %s
    api_key: key1
`, dryRun, remonitor, url)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	return path
}

// TestRun_ReverseScan_RemonitorFlagOff_ComposesNoWriteAtAll is THE pin of this
// phase, and it is deliberately the historically strongest shape this project
// has: a run in WRITE MODE (dry_run: false — no dry-run gate anywhere in the
// picture) against a movie that really is a qualifying finding, with the flag
// left at its default. Zero requests of any method other than GET, and zero
// pre-write fetches: the write path is never entered, so there is nothing to
// gate.
func TestRun_ReverseScan_RemonitorFlagOff_ComposesNoWriteAtAll(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{
		// The witness makes the flag the ONLY thing standing between this
		// finding and a write: the cycle's cross-check verifies a real forward
		// sample, so the write gate would have opened. Without it, "nothing was
		// written" would have a second possible explanation.
		crossCheckWitnessStatefulMovie(5, "Ordinary Monitored"),
		reverseFindingStatefulMovie(7, "Accidentally Unmonitored"),
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", writeReverseTestConfig(t, fake.srv.URL, false, false), "--once"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, `msg="reverse-scan finding"`) {
		t.Fatalf("this test proves nothing unless the finding really was made:\n%s", out)
	}
	if !strings.Contains(out, `crossCheck="passed (1 verified`) {
		t.Fatalf("nor unless the write gate would have opened, leaving the flag as the only explanation:\n%s", out)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("report-only must compose no write of any method: got %+v", writes)
	}
	if n := fake.countRequests("/api/v3/movie/7"); n != 0 {
		t.Errorf("the write path's own pre-write GET was issued %d time(s); with the flag off the write path must never be entered at all", n)
	}
	if m := fake.movie(7); m.monitored {
		t.Errorf("movie 7 is monitored: report-only must change nothing")
	}
	// The off state must be visually distinguishable from the on state.
	if strings.Contains(out, "remonitored=") || strings.Contains(out, "reverseWithheld=") {
		t.Errorf("with the flag off the summary must carry no write counters at all:\n%s", out)
	}
	if !strings.Contains(out, "reverseFindings=1") {
		t.Errorf("the finding is still counted with the flag off:\n%s", out)
	}
}

// TestRun_ReverseScan_RemonitorFlagOn_DryRun_RehearsesAndWritesNothing is §2.1
// in the new direction: the whole write path runs — fresh GET, identity check,
// tag re-check, the fresh re-evaluation — and stops at the gate immediately
// before the PUT, which is what makes a dry-run a rehearsal rather than a
// different code path.
func TestRun_ReverseScan_RemonitorFlagOn_DryRun_RehearsesAndWritesNothing(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{
		crossCheckWitnessStatefulMovie(5, "Ordinary Monitored"),
		reverseFindingStatefulMovie(7, "Accidentally Unmonitored"),
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", writeReverseTestConfig(t, fake.srv.URL, true, true), "--once"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}

	out := stdout.String()
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("dry-run must write nothing: got %+v", writes)
	}
	if n := fake.countRequests("/api/v3/movie/7"); n == 0 {
		t.Errorf("the dry-run must still REHEARSE the write path, including its fresh pre-write fetch:\n%s", out)
	}
	c := summaryCounters(t, out)
	if c["reverseFindings"] != 1 || c["reverseWithheld"] != 1 || c["remonitored"] != 0 {
		t.Errorf("want reverseFindings=1 reverseWithheld=1 remonitored=0, got %v:\n%s", c, out)
	}
	assertReverseIdentity(t, out)
}

// TestRun_ReverseScan_RemonitorFlagOn_WriteMode_RemonitorsExactlyTheFinding is
// the plan's own acceptance criterion, machine-verified: with the flag on and
// dry_run false, the one qualifying movie is re-monitored — the PUT really
// carries monitored:true, the server's echo confirms it, and the fake's state
// changes.
func TestRun_ReverseScan_RemonitorFlagOn_WriteMode_RemonitorsExactlyTheFinding(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{
		// The cycle's evidence: an ordinary monitored movie for the forward
		// cross-check to verify, without which the reverse gate stays shut.
		crossCheckWitnessStatefulMovie(5, "Ordinary Monitored"),
		reverseFindingStatefulMovie(7, "Accidentally Unmonitored"),
		// A correctly unmonitored movie, which must be left alone.
		{id: 8, title: "Correctly Unmonitored", monitored: false, hasFile: true, qualityProfileID: 1,
			tags: []int{}, movieFileID: 8, cfScore: 200, qualityCutoffNotMet: false, inWantedSet: false},
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", writeReverseTestConfig(t, fake.srv.URL, false, true), "--once"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}

	out := stdout.String()
	// The write gate opened on real evidence, not on an empty sample pool:
	// stated here because it is a precondition of everything below it.
	if !strings.Contains(out, `crossCheck="passed (1 verified`) {
		t.Fatalf("the cycle must have verified something for its reverse writes to be authorized:\n%s", out)
	}
	puts := fake.puts()
	if len(puts) != 1 {
		t.Fatalf("want exactly 1 PUT (the one finding), got %d: %+v", len(puts), puts)
	}
	if puts[0].path != "/api/v3/movie/7" {
		t.Errorf("PUT path = %s, want /api/v3/movie/7", puts[0].path)
	}
	if !strings.Contains(string(puts[0].body), `"monitored":true`) {
		t.Errorf("the reverse write must set monitored true:\n%s", puts[0].body)
	}
	if !fake.movie(7).monitored {
		t.Errorf("movie 7 must be monitored after a confirmed remonitor write")
	}
	if fake.movie(8).monitored {
		t.Errorf("movie 8 meets the criteria and must not be touched")
	}
	if !strings.Contains(out, "msg=remonitor") {
		t.Errorf("a confirmed re-monitor must log msg=remonitor:\n%s", out)
	}
	c := summaryCounters(t, out)
	if c["reverseFindings"] != 1 || c["remonitored"] != 1 || c["reverseWithheld"] != 0 {
		t.Errorf("want reverseFindings=1 remonitored=1 reverseWithheld=0, got %v:\n%s", c, out)
	}
	assertReverseIdentity(t, out)
}

// TestRun_ReverseScan_WriteMode_SecondRunIsANoOp is the property that makes the
// write direction safe to leave switched on: once a finding has been
// re-monitored it is no longer a finding (it is monitored), and the FORWARD
// pass must not immediately unmonitor it again. A pair of passes that disagreed
// would flap the same movie on every cycle forever.
func TestRun_ReverseScan_WriteMode_SecondRunIsANoOp(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{
		crossCheckWitnessStatefulMovie(5, "Ordinary Monitored"),
		reverseFindingStatefulMovie(7, "Accidentally Unmonitored"),
	})
	cfg := writeReverseTestConfig(t, fake.srv.URL, false, true)

	var run1, stderr bytes.Buffer
	if code := run([]string{"--config", cfg, "--once"}, &run1, &stderr); code != 0 {
		t.Fatalf("run 1 exit code = %d; stderr=%s", code, stderr.String())
	}
	if len(fake.puts()) != 1 {
		t.Fatalf("run 1 must re-monitor the finding: %+v", fake.puts())
	}

	before := len(fake.puts())
	var run2 bytes.Buffer
	if code := run([]string{"--config", cfg, "--once"}, &run2, &stderr); code != 0 {
		t.Fatalf("run 2 exit code = %d; stderr=%s", code, stderr.String())
	}
	if len(fake.puts()) != before {
		t.Errorf("run 2 wrote again (%d -> %d PUTs); the two directions must not flap the same movie", before, len(fake.puts()))
	}
	out := run2.String()
	c := summaryCounters(t, out)
	if c["reverseFindings"] != 0 {
		t.Errorf("run 2 must find nothing: reverseFindings = %d:\n%s", c["reverseFindings"], out)
	}
	if c["unmonitored"] != 0 || c["wouldUnmonitor"] != 0 {
		t.Errorf("run 2's forward pass must not unmonitor what run 1 re-monitored: %v\n%s", c, out)
	}
}

// TestRun_ReverseScan_WriteMode_ExcludedMovieIsNeitherReportedNorWritten pins
// §2.5 end to end, in write mode, with the flag on: the strongest form of the
// exclusion tag's promise.
func TestRun_ReverseScan_WriteMode_ExcludedMovieIsNeitherReportedNorWritten(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{
		// As on the flag-off pin: with the gate provably open, §2.5 is the only
		// thing that can account for the silence.
		crossCheckWitnessStatefulMovie(5, "Ordinary Monitored"),
		reverseFindingStatefulMovie(7, "Excluded Finding"),
	})
	fake.movie(7).tags = []int{42}
	fake.tagsJSON = `[{"id": 42, "label": "cutoffarr-exclude"}]`

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--config", writeReverseTestConfig(t, fake.srv.URL, false, true), "--once"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code; stderr=%s", stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, `crossCheck="passed (1 verified`) {
		t.Fatalf("this test proves nothing unless the write gate would have opened:\n%s", out)
	}
	if strings.Contains(out, "reverse-scan finding") {
		t.Errorf("an excluded movie must never be reported:\n%s", out)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("an excluded movie must never be written: %+v", writes)
	}
	if c := summaryCounters(t, out); c["reverseFindings"] != 0 {
		t.Errorf("reverseFindings = %d, want 0:\n%s", c["reverseFindings"], out)
	}
}

// TestRunRadarrDecisionEngine_ReverseScan_CrossCheckNotPassed_WithholdsEveryWrite
// pins binding controller resolution 6. The reverse pass has no cross-check of
// its own — it has no second signal to compare against — so what authorizes its
// writes is the FORWARD cross-check's verdict, read as a data-layer health
// signal: if this instance's data disagreed with itself this cycle, nothing
// derived from it may be written in either direction.
func TestRunRadarrDecisionEngine_ReverseScan_CrossCheckNotPassed_WithholdsEveryWrite(t *testing.T) {
	fake := newRadarrFake(t, "", nil)
	fake.reverseWantedJSON = `{"page":1,"pageSize":100,"totalRecords":1,"records":[{"id":7,"title":"Finding"}]}`

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{
		// A monitored movie whose own qualityCutoffNotMet contradicts its
		// absence from the forward wanted set: the cross-check FAILS.
		{ID: intPtr(1), Title: strPtr("Contradictory"), Monitored: boolPtr(true), HasFile: boolPtr(true),
			QualityProfileID: intPtr(1), Tags: &noTags,
			MovieFile: &movieFileElement{ID: intPtr(1), QualityCutoffNotMet: boolPtr(true)}},
		unmonitoredBelowCutoffMovie(7, "Finding"),
	}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude",
		fullLibraryScope(slog.LevelInfo), false, reverseOptions{enabled: true, remonitor: true}, fileReportOptions{})

	out := buf.String()
	if !strings.Contains(out, "crossCheck=FAILED") {
		t.Fatalf("this test is only meaningful if the cross-check really failed:\n%s", out)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("a failed cross-check must block every reverse write: %+v", writes)
	}
	c := summaryCounters(t, out)
	if c["reverseFindings"] != 1 || c["reverseWithheld"] != 1 || c["remonitored"] != 0 {
		t.Errorf("want reverseFindings=1 reverseWithheld=1 remonitored=0, got %v:\n%s", c, out)
	}
	if !strings.Contains(out, "reverse-scan writes withheld for this instance") {
		t.Errorf("a blocked reverse pass must say so: 'nothing was written' and 'nothing needed writing' are different facts:\n%s", out)
	}
	assertReverseIdentity(t, out)
}

// TestReverseWriteGateBlockReason_OpensOnlyForAnEvidencedPass is the reverse
// write gate, tested directly and exhaustively, because the two engine tests
// that exercise it can only ever reach one status each.
//
// Binding controller resolution 6 names three blocking statuses (FAILED,
// inconclusive, unrecognized) and one that admits a write, and the project's own
// forward precedent — writeGateBlockReason, hardened in the phase-4 round-2
// review — established that the PASSED status alone is far weaker evidence than
// it reads: "passed" is awarded whenever nothing disagreed, including when
// nothing was compared at all. Both holes are checked here, in one table, so
// that rewriting the switch as `if failed { block }` (inconclusive is the COMMON
// degraded state) or dropping the evidence conditions fails immediately.
func TestReverseWriteGateBlockReason_OpensOnlyForAnEvidencedPass(t *testing.T) {
	cases := []struct {
		name     string
		cc       crossCheckResult
		wantOpen bool
	}{
		{
			name:     "passed, on a sample that verified something",
			cc:       crossCheckResult{status: crossCheckStatusPassed, verified: 1},
			wantOpen: true,
		},
		{
			name:     "passed, most of the sample verified",
			cc:       crossCheckResult{status: crossCheckStatusPassed, verified: 6, unverifiable: 4},
			wantOpen: true,
		},
		{
			// The pseudo-pass: no would-unmonitor decision and no
			// sample-eligible skip existed, so nothing was compared and
			// nothing was verified — yet the status reads "passed".
			name: "passed, but nothing at all was sampled",
			cc:   crossCheckResult{status: crossCheckStatusPassed},
		},
		{
			name: "passed on one verified item out of twenty",
			cc:   crossCheckResult{status: crossCheckStatusPassed, verified: 1, unverifiable: 19},
		},
		{
			name: "failed",
			cc:   crossCheckResult{status: crossCheckStatusFailed, verified: 10},
		},
		{
			name: "inconclusive",
			cc:   crossCheckResult{status: crossCheckStatusInconclusive, unverifiable: 10},
		},
		{
			name: "a status nobody has written yet",
			cc:   crossCheckResult{status: "some-future-status", verified: 10},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason := reverseWriteGateBlockReason(tc.cc)
			if tc.wantOpen && reason != "" {
				t.Errorf("the gate must open for %+v, but it blocked: %s", tc.cc, reason)
			}
			if !tc.wantOpen && reason == "" {
				t.Errorf("the gate must block %+v, and say why", tc.cc)
			}
		})
	}
}

// crossCheckWitnessMovie is an ORDINARY monitored movie that is still below its
// quality cutoff: it is in the FORWARD wanted set and its own
// movieFile.qualityCutoffNotMet agrees with that. Its forward decision is a
// plain skip, so it is never written in either direction — what it contributes
// is evidence: it is what the cycle's cross-check samples, compares, and
// verifies, which is what a reverse write pass now needs before its gate will
// open (see reverseWriteGateBlockReason).
//
// Every reverse write test needs one, and that is the point: a library made
// only of unmonitored movies gives the cross-check nothing to sample, and a
// cycle that verified nothing may not write.
func crossCheckWitnessMovie(id int, title string) movieListElement {
	return movieListElement{
		ID: intPtr(id), Title: strPtr(title), Monitored: boolPtr(true), HasFile: boolPtr(true),
		QualityProfileID: intPtr(1), Tags: &noTags,
		MovieFile: &movieFileElement{ID: intPtr(1), QualityCutoffNotMet: boolPtr(true)},
	}
}

// TestRunRadarrDecisionEngine_ReverseWriteGate_NeedsAVerifiedForwardSample is
// the "nothing sampled" pseudo-pass, end to end through the engine, in the one
// shape the reverse direction makes easy to build by accident: a library whose
// only interesting items are UNMONITORED produces no forward decision to sample,
// so the cross-check compares nothing, reports `passed (nothing sampled: ...)`,
// and — before this fix — authorized re-monitor PUTs on the strength of it.
//
// The second case is the same cycle with one ordinary monitored movie added, and
// it is the only thing that differs: the cross-check now has something real to
// verify, so the gate opens and the finding is written.
func TestRunRadarrDecisionEngine_ReverseWriteGate_NeedsAVerifiedForwardSample(t *testing.T) {
	const findingID = 7
	reverseWanted := `{"page":1,"pageSize":100,"totalRecords":1,"records":[{"id":7,"title":"Accidentally Unmonitored"}]}`
	detail := map[int]string{findingID: `{"id": 7, "title": "Accidentally Unmonitored", "monitored": false, "hasFile": true, "qualityProfileId": 1, "tags": []}`}
	finding := unmonitoredBelowCutoffMovie(findingID, "Accidentally Unmonitored")

	t.Run("nothing was sampled, so every write is withheld", func(t *testing.T) {
		fake := newRadarrFake(t, "", detail)
		fake.reverseWantedJSON = reverseWanted

		logger, buf := newDecisionTestLogger(slog.LevelInfo)
		runRadarrDecisionEngine(context.Background(), logger, fake.instance(), []movieListElement{finding}, map[int]bool{},
			"cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), false, reverseOptions{enabled: true, remonitor: true}, fileReportOptions{})

		out := buf.String()
		if !strings.Contains(out, `crossCheck="passed (nothing sampled`) {
			t.Fatalf("this test is only meaningful if the cross-check really sampled nothing:\n%s", out)
		}
		if writes := fake.writes(); len(writes) != 0 {
			t.Errorf("a cycle that verified nothing must write nothing: %+v", writes)
		}
		if n := fake.countRequests("/api/v3/movie/7"); n != 0 {
			t.Errorf("a blocked pass must not even make its pre-write fetch, got %d", n)
		}
		if !strings.Contains(out, "reverse-scan writes withheld for this instance") || !strings.Contains(out, "sampled nothing") {
			t.Errorf("the block must name the nothing-sampled pass as its reason:\n%s", out)
		}
		c := summaryCounters(t, out)
		if c["reverseFindings"] != 1 || c["reverseWithheld"] != 1 || c["remonitored"] != 0 {
			t.Errorf("want reverseFindings=1 reverseWithheld=1 remonitored=0, got %v:\n%s", c, out)
		}
		assertReverseIdentity(t, out)
	})

	t.Run("a real forward sample was verified, so the gate opens", func(t *testing.T) {
		fake := newRadarrFake(t, "", detail)
		fake.reverseWantedJSON = reverseWanted

		logger, buf := newDecisionTestLogger(slog.LevelInfo)
		movies := []movieListElement{crossCheckWitnessMovie(5, "Ordinary Monitored"), finding}
		runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{5: true},
			"cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), false, reverseOptions{enabled: true, remonitor: true}, fileReportOptions{})

		out := buf.String()
		if !strings.Contains(out, `crossCheck="passed (1 verified`) {
			t.Fatalf("this test is only meaningful if the cross-check really verified something:\n%s", out)
		}
		puts := fake.puts()
		if len(puts) != 1 || puts[0].path != "/api/v3/movie/7" {
			t.Fatalf("want exactly the finding's PUT, got %+v", puts)
		}
		c := summaryCounters(t, out)
		if c["reverseFindings"] != 1 || c["remonitored"] != 1 || c["reverseWithheld"] != 0 {
			t.Errorf("want reverseFindings=1 remonitored=1 reverseWithheld=0, got %v:\n%s", c, out)
		}
		assertReverseIdentity(t, out)
	})
}

// TestRun_ReverseScan_WriteMode_ItemThatNowMeetsCriteria_IsRefused is the
// fresh-GET re-verification, mirrored for the reverse direction. The decision
// was correct when it was made; by the time the write pass reaches it the
// movie's file has been upgraded and now scores above the profile's cutoff, so
// re-monitoring it would undo cutoffarr's own correct work — and the next
// forward pass would unmonitor it again, forever.
//
// The re-verification is evaluateMovie itself, re-run against the write path's
// own fresh fetches, which is why it sees the new score at all: rule 6 goes
// back to /moviefile rather than trusting anything the scan remembered.
func TestRun_ReverseScan_WriteMode_ItemThatNowMeetsCriteria_IsRefused(t *testing.T) {
	// A CF-score finding: absent from the unmonitored wanted set (its quality
	// cutoff is met) but scoring below the profile's cutoffFormatScore of 100.
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{
		crossCheckWitnessStatefulMovie(5, "Ordinary Monitored"),
		{
			id: 7, title: "Upgraded Meanwhile", monitored: false, hasFile: true, qualityProfileID: 1,
			tags: []int{}, movieFileID: 7, cfScore: 10, qualityCutoffNotMet: false, inWantedSet: false,
		},
	})
	// The world changes between the scan and the write pass: the write path's
	// own fresh pre-write fetch is the moment this movie stops being a finding.
	fake.onRequest = func(method, path string) {
		if method == http.MethodGet && path == "/api/v3/movie/7" {
			fake.setCFScore(7, 200)
		}
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--config", writeReverseTestConfig(t, fake.srv.URL, false, true), "--once"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code; stderr=%s", stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, `msg="reverse-scan finding"`) {
		t.Fatalf("this test proves nothing unless the finding was made first:\n%s", out)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("a movie that now meets the criteria must not be re-monitored: %+v", writes)
	}
	c := summaryCounters(t, out)
	if c["reverseFindings"] != 1 || c["remonitorsRefused"] != 1 || c["remonitored"] != 0 {
		t.Errorf("want reverseFindings=1 remonitorsRefused=1 remonitored=0, got %v:\n%s", c, out)
	}
	if !strings.Contains(out, "no longer fails the criteria") {
		t.Errorf("the refusal must say why:\n%s", out)
	}
	assertReverseIdentity(t, out)
}

// TestRun_ReverseScan_WriteMode_AlreadyMonitoredAtWrite_IsRefused is the
// scan-to-write race: something (a human, another tool) re-monitored the movie
// between the scan and the write pass. Nothing needs doing, and the refusal is
// counted rather than vanishing into an unexplained gap in the summary.
func TestRun_ReverseScan_WriteMode_AlreadyMonitoredAtWrite_IsRefused(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{
		crossCheckWitnessStatefulMovie(5, "Ordinary Monitored"),
		reverseFindingStatefulMovie(7, "Someone Beat Us To It"),
	})
	fake.onRequest = func(method, path string) {
		if method == http.MethodGet && path == "/api/v3/movie/7" {
			fake.setMonitored(7, true)
		}
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--config", writeReverseTestConfig(t, fake.srv.URL, false, true), "--once"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code; stderr=%s", stderr.String())
	}

	out := stdout.String()
	if puts := fake.puts(); len(puts) != 0 {
		t.Errorf("an already-monitored movie needs no write: %+v", puts)
	}
	c := summaryCounters(t, out)
	if c["remonitorsRefused"] != 1 {
		t.Errorf("remonitorsRefused = %d, want 1:\n%s", c["remonitorsRefused"], out)
	}
	assertReverseIdentity(t, out)
}

// assertReverseIdentity is the reverse pass's accounting identity, the same
// discipline the two forward passes are held to: every finding must end in
// exactly one counted outcome, so a future path that silently drops a promised
// write cannot hide.
//
// Every caller runs with the write switch ON, so it also enforces that switch's
// half of the frozen report vocabulary (binding controller resolution 7) in
// BOTH states a flag-on cycle can end in — the pass ran, or the pass was skipped
// — because "always present including as 0" is what stops an absent number from
// reading as "none happened".
func assertReverseIdentity(t *testing.T, out string) {
	t.Helper()
	for _, msg := range []string{"radarr decision summary", "sonarr decision summary"} {
		if !strings.Contains(out, msg) {
			continue
		}
		line := summaryLineFor(t, out, msg)
		c := summaryCountersFor(t, out, msg)
		_, ran := c["reverseFindings"]
		skipped := strings.Contains(line, "reverseScan=skipped")
		if !ran && !skipped {
			// This cycle ran no reverse pass at all (a webhook or --only-id
			// scope): the whole vocabulary is deliberately absent and there is
			// no identity to check.
			continue
		}
		for _, always := range []string{"remonitored", "remonitorsRefused", "reverseWithheld"} {
			if _, present := c[always]; !present {
				t.Errorf("with the write switch on the summary must always carry %s=, including as 0:\n%s", always, line)
			}
		}
		if skipped {
			// A skipped pass has no finding count to balance — that is the
			// point of the state — but the three counters it does print are
			// facts, and every path that sets skipped returns before the write
			// half is reached, so each of them must read 0.
			if ran {
				t.Errorf("a skipped reverse pass must print no finding count, which cannot be trusted:\n%s", line)
			}
			for _, zero := range []string{"remonitored", "remonitorsRefused", "reverseWithheld"} {
				if c[zero] != 0 {
					t.Errorf("a skipped reverse pass never reached the write half, so %s must be 0, got %d:\n%s", zero, c[zero], line)
				}
			}
			continue
		}
		accounted := c["remonitored"] + c["remonitorsRefused"] + c["reverseWithheld"] +
			c["reverseWriteErrors"] + c["reverseRehearsalErrors"] + c["reverseEchoUnverified"]
		if accounted != c["reverseFindings"] {
			t.Errorf("%s accounts for %d of %d findings; every finding must end in exactly one counted outcome:\n%s", msg, accounted, c["reverseFindings"], out)
		}
	}
}

// TestRunRadarrDecisionEngine_ReverseScan_SkippedWithTheFlagOn_StillCarriesTheWriteCounters
// pins both halves of binding controller resolution 7's frozen vocabulary on
// the one cycle where they are hardest to keep: a reverse pass that ran but
// could not be trusted.
//
// "remonitored, remonitorsRefused and reverseWithheld are always present incl. 0
// when the flag is on; ABSENT entirely when the flag is off — the off state must
// be visually distinguishable" has to hold here too, and it is here that it
// matters most: something went wrong, nothing was re-monitored, and a human is
// reading this line to find out which mode the daemon is even in. A skipped
// flag-ON cycle that printed the same bytes as a skipped flag-OFF cycle would
// answer that question wrongly.
func TestRunRadarrDecisionEngine_ReverseScan_SkippedWithTheFlagOn_StillCarriesTheWriteCounters(t *testing.T) {
	cycleWith := func(t *testing.T, remonitor bool) string {
		t.Helper()
		fake := newRadarrFake(t, "", nil)
		// Claims 50 records and returns none: the completeness contract skips
		// the reverse pass for this instance.
		fake.reverseWantedJSON = `{"page":1,"pageSize":100,"totalRecords":50,"records":[]}`

		logger, buf := newDecisionTestLogger(slog.LevelInfo)
		movies := []movieListElement{unmonitoredBelowCutoffMovie(7, "Never Evaluated")}
		// Write mode, so nothing but the flag itself distinguishes the two runs.
		runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude",
			fullLibraryScope(slog.LevelInfo), false, reverseOptions{enabled: true, remonitor: remonitor}, fileReportOptions{})
		return buf.String()
	}

	onOut, offOut := cycleWith(t, true), cycleWith(t, false)
	on := summaryLineFor(t, onOut, "radarr decision summary")
	off := summaryLineFor(t, offOut, "radarr decision summary")
	// The accounting rules for the skipped state itself: three counters, all 0,
	// and no finding count to balance them against.
	assertReverseIdentity(t, onOut)

	for _, line := range []string{on, off} {
		if !strings.Contains(line, "reverseScan=skipped") {
			t.Fatalf("this test proves nothing unless the pass really was skipped:\n%s", line)
		}
		if strings.Contains(line, "reverseFindings=") {
			t.Errorf("a skipped pass must never print a finding count:\n%s", line)
		}
	}
	for _, want := range []string{"remonitored=0", "remonitorsRefused=0", "reverseWithheld=0"} {
		if !strings.Contains(on, want) {
			t.Errorf("with the flag on the summary must carry %s even on a skipped pass:\n%s", want, on)
		}
	}
	for _, token := range []string{"remonitored=", "remonitorsRefused=", "reverseWithheld="} {
		if strings.Contains(off, token) {
			t.Errorf("with the flag off the summary must carry no write counter at all, but it carries %s:\n%s", token, off)
		}
	}
	if stripTimeAttr(on) == stripTimeAttr(off) {
		t.Errorf("a flag-on skipped cycle and a flag-off one must not be byte-identical:\n%s", on)
	}
}

// --- the remonitor write path: Sonarr ---------------------------------------

// reverseFindingSonarrFake is one series with one UNMONITORED season that is
// complete on disk, fully aired, and still below its quality cutoff — the
// Sonarr shape of an accidental unmonitor. seriesMonitored says whether the
// SERIES itself is monitored, which is the fact that decides whether the
// finding may ever be written.
func reverseFindingSonarrFake(t *testing.T, seriesMonitored bool) *statefulSonarrFake {
	t.Helper()
	series, episodes, files := crossCheckWitnessSonarrFixtures()
	return newStatefulSonarrFake(t,
		append(series,
			&statefulSonarrSeries{id: 1, title: "Accidentally Unmonitored", monitored: seriesMonitored, profileID: 1, tags: []int{},
				seasons: []statefulSonarrSeason{{number: 2, monitored: false, episodeFileCount: 1, totalEpisodeCount: 1}}}),
		append(episodes,
			&statefulSonarrEpisode{id: 200, seriesID: 1, seasonNumber: 2, episodeNumber: 1, monitored: false, hasFile: true,
				airDateUtc: pastAirDate, episodeFileID: 600, inWantedSet: true}),
		append(files, &statefulSonarrEpisodeFile{id: 600, seasonNumber: 2, customFormatScore: 200, qualityCutoffNotMet: true}),
	)
}

// crossCheckWitnessSonarrFixtures is crossCheckWitnessStatefulMovie's Sonarr
// twin: an ORDINARY monitored series with one monitored season that is complete,
// fully aired, in the forward wanted set, and whose episode file agrees that its
// cutoff is not met.
//
// The forward pass skips that season at rule 4, so it is never written; what it
// contributes is the one thing every reverse write test needs and none of them
// had — a forward decision for the cross-check to sample, compare and VERIFY.
// Without it the cross-check has an empty pool, reports "passed (nothing
// sampled)", and the reverse write gate refuses to open on a health signal that
// does not exist.
//
// It is a separate SERIES rather than an extra season of the series under test
// because one of the tests retires that series (series-level monitored false),
// which would take the witness out of the forward pass with it.
func crossCheckWitnessSonarrFixtures() ([]*statefulSonarrSeries, []*statefulSonarrEpisode, []*statefulSonarrEpisodeFile) {
	return []*statefulSonarrSeries{
			{id: 9, title: "Ordinary Monitored Show", monitored: true, profileID: 1, tags: []int{},
				seasons: []statefulSonarrSeason{{number: 1, monitored: true, episodeFileCount: 1, totalEpisodeCount: 1}}},
		},
		[]*statefulSonarrEpisode{
			{id: 900, seriesID: 9, seasonNumber: 1, episodeNumber: 1, monitored: true, hasFile: true,
				airDateUtc: pastAirDate, episodeFileID: 9000, inWantedSet: true},
		},
		[]*statefulSonarrEpisodeFile{{id: 9000, seasonNumber: 1, customFormatScore: 200, qualityCutoffNotMet: true}}
}

// TestRun_ReverseScan_Sonarr_RemonitorFlagOff_ComposesNoWriteAtAll is the
// headline pin's Sonarr half: write mode, a real finding, flag off, nothing
// written and the write path never entered.
func TestRun_ReverseScan_Sonarr_RemonitorFlagOff_ComposesNoWriteAtAll(t *testing.T) {
	fake := reverseFindingSonarrFake(t, true)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--config", writeReverseSonarrTestConfig(t, fake.srv.URL, false, false), "--once"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code; stderr=%s", stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, `msg="reverse-scan finding"`) {
		t.Fatalf("this test proves nothing unless the finding really was made:\n%s", out)
	}
	if !strings.Contains(out, `crossCheck="passed (1 verified`) {
		t.Fatalf("nor unless the write gate would have opened, leaving the flag as the only explanation:\n%s", out)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("report-only must compose no write of any method: got %+v", writes)
	}
	if n := fake.countRequests("/api/v3/series/1"); n != 0 {
		t.Errorf("the write path's own pre-write GET was issued %d time(s); with the flag off it must never be entered", n)
	}
	if strings.Contains(out, "remonitored=") {
		t.Errorf("with the flag off the summary must carry no write counters:\n%s", out)
	}
}

// TestRun_ReverseScan_Sonarr_WriteMode_RemonitorsTheSeasonEpisodesFirst is the
// Sonarr acceptance shape. Both writes go out, in the binding order — episodes
// first, then the season — and both carry monitored:true.
//
// The order matters in this direction for its own reason: the season flag is
// what makes a season visible to the reverse pass, so writing it LAST means a
// half-completed re-monitor leaves the season unmonitored — and therefore still
// a finding next cycle, though not the same one, since the episodes that landed
// take it out of the wanted set. See
// TestRun_ReverseScan_Sonarr_WriteMode_HalfDoneWrite_IsReportedAndRetried for
// the state that really produces and the reason that reports it; between the two
// of them, that is why the reverse direction needs no recovery path.
func TestRun_ReverseScan_Sonarr_WriteMode_RemonitorsTheSeasonEpisodesFirst(t *testing.T) {
	fake := reverseFindingSonarrFake(t, true)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--config", writeReverseSonarrTestConfig(t, fake.srv.URL, false, true), "--once"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code; stderr=%s", stderr.String())
	}

	out := stdout.String()
	// As on the Radarr twin: the gate opened because the cross-check verified a
	// real forward sample, which is a precondition of every assertion below.
	if !strings.Contains(out, `crossCheck="passed (1 verified`) {
		t.Fatalf("the cycle must have verified something for its reverse writes to be authorized:\n%s", out)
	}
	writes := fake.writes()
	if len(writes) != 2 {
		t.Fatalf("want exactly 2 writes (episodes then season), got %d: %+v", len(writes), writes)
	}
	if writes[0].path != "/api/v3/episode/monitor" || writes[1].path != "/api/v3/series/1" {
		t.Fatalf("write order = %s then %s, want /api/v3/episode/monitor then /api/v3/series/1", writes[0].path, writes[1].path)
	}
	if !strings.Contains(string(writes[0].body), `"monitored":true`) || !strings.Contains(string(writes[0].body), `"episodeIds":[200]`) {
		t.Errorf("the episode write must name the season's unmonitored episodes and set them monitored:\n%s", writes[0].body)
	}
	if !fake.episodeMonitored(200) {
		t.Error("episode 200 must be monitored after the write")
	}
	if !fake.seasonMonitored(1, 2) {
		t.Error("season 2 must be monitored after the write")
	}
	if !fake.seriesMonitored(1) {
		t.Error("the series-level flag must be untouched and still monitored")
	}
	if !strings.Contains(out, "msg=remonitor") {
		t.Errorf("a confirmed re-monitor must log msg=remonitor:\n%s", out)
	}
	c := summaryCountersFor(t, out, "sonarr decision summary")
	if c["reverseFindings"] != 1 || c["remonitored"] != 1 {
		t.Errorf("want reverseFindings=1 remonitored=1, got %v:\n%s", c, out)
	}
	assertReverseIdentity(t, out)
}

// TestRun_ReverseScan_Sonarr_WriteMode_HalfDoneWrite_IsReportedForever is what
// happens to a season this pass left half-written, and it is deliberately NOT an
// automatic repair (binding controller ruling R2, Phase 10 round 5).
//
// A season is TWO writes: the episodes first, then the season flag. When the
// second one fails, the season is left unmonitored with MONITORED episodes
// inside it. Before round 3 that state was invisible — the episodes that landed
// had left the monitored=false wanted set, so rule 4 stopped firing, the season
// evaluated as "cutoff met", which is not a finding, and the forward pass
// excludes it at rule 1 on its flag; nothing would ever have looked at it again
// while Sonarr went on upgrading its episodes. Round 3 made it a REPORTED
// finding (ReasonSeasonMonitorMismatch), and round 4 let the write finish the
// flag.
//
// R2 takes the write back and keeps the report. A season with monitored episodes
// already inside it is a mixed monitored state, and this pass cannot tell OUR
// half-done write from a human who monitored an episode by hand — the two are
// byte-identical in the API — so it re-monitors neither and says so every cycle
// until a human settles it. The cost is one line per cycle on a state a click
// fixes; the alternative is guessing about somebody's deliberate choice.
//
// Run 1 produces the state against a server that rejects the season PUT. Run 2
// is the next cycle, with the server healthy.
func TestRun_ReverseScan_Sonarr_WriteMode_HalfDoneWrite_IsReportedForever(t *testing.T) {
	fake := reverseFindingSonarrFake(t, true)
	fake.setSeriesPutStatus(1, http.StatusInternalServerError)
	cfg := writeReverseSonarrTestConfig(t, fake.srv.URL, false, true)

	var run1, stderr bytes.Buffer
	if code := run([]string{"--config", cfg, "--once"}, &run1, &stderr); code != 0 {
		t.Fatalf("run 1 exit code; stderr=%s", stderr.String())
	}

	out1 := run1.String()
	// The half-done state really exists: the episode write landed, the season
	// flag did not. Everything below is about what happens to it next.
	if !fake.episodeMonitored(200) {
		t.Fatalf("run 1 must land the episode write, or this test is about nothing:\n%s", out1)
	}
	if fake.seasonMonitored(1, 2) {
		t.Fatalf("run 1's season PUT was supposed to fail:\n%s", out1)
	}
	if !strings.Contains(out1, "left unmonitored with monitored episodes") {
		t.Errorf("the failure that creates this state must name it, at WARN, rather than being one more writeErrors tally:\n%s", out1)
	}
	c1 := summaryCountersFor(t, out1, "sonarr decision summary")
	if c1["reverseFindings"] != 1 || c1["reverseWriteErrors"] != 1 {
		t.Errorf("run 1: want reverseFindings=1 reverseWriteErrors=1, got %v:\n%s", c1, out1)
	}
	assertReverseIdentity(t, out1)

	// The next cycle, against a healthy server.
	fake.setSeriesPutStatus(1, 0)
	before := len(fake.writes())
	var run2 bytes.Buffer
	if code := run([]string{"--config", cfg, "--once"}, &run2, &stderr); code != 0 {
		t.Fatalf("run 2 exit code; stderr=%s", stderr.String())
	}

	out2 := run2.String()
	line := reverseFindingLine(t, out2)
	if !strings.Contains(line, `reason="`+ReasonSeasonMonitorMismatch+`"`) {
		t.Errorf("run 2 must report the stranded season, with the reason that describes it:\n%s", line)
	}
	if writes := fake.writes()[before:]; len(writes) != 0 {
		t.Errorf("run 2 must write nothing: a season with monitored episodes inside it is reported, never repaired by guesswork, got %+v", writes)
	}
	if fake.seasonMonitored(1, 2) {
		t.Errorf("the season flag must be left exactly as the half-done write left it:\n%s", out2)
	}
	if !strings.Contains(out2, "already has monitored episodes inside it") {
		t.Errorf("the refusal must say what stopped it, on the cycle a human is reading to find out:\n%s", out2)
	}
	c2 := summaryCountersFor(t, out2, "sonarr decision summary")
	if c2["reverseFindings"] != 1 || c2["remonitorsRefused"] != 1 || c2["remonitored"] != 0 {
		t.Errorf("run 2: want reverseFindings=1 remonitorsRefused=1 remonitored=0, got %v:\n%s", c2, out2)
	}
	assertReverseIdentity(t, out2)
}

// TestRun_ReverseScan_Sonarr_UnmonitoredSeries_IsReportedButNeverWritten pins
// binding controller resolution 3's write half. A series-level unmonitor is a
// human deliberately retiring a show; re-monitoring its seasons would be this
// program arguing with that decision, so the finding is reported (with its
// seriesMonitored=false attr and a reason) and withheld — even in write mode,
// even with the flag on, even with a passing cross-check.
func TestRun_ReverseScan_Sonarr_UnmonitoredSeries_IsReportedButNeverWritten(t *testing.T) {
	fake := reverseFindingSonarrFake(t, false)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--config", writeReverseSonarrTestConfig(t, fake.srv.URL, false, true), "--once"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code; stderr=%s", stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "seriesMonitored=false") {
		t.Fatalf("the finding must be reported with its series state:\n%s", out)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("a season under an unmonitored series must never be written: %+v", writes)
	}
	if n := fake.countRequests("/api/v3/series/1"); n != 0 {
		t.Errorf("it must not even be fetched for a write: %d pre-write GET(s)", n)
	}
	if !strings.Contains(out, "its series is not monitored") {
		t.Errorf("the withheld finding must say why, or a human cannot tell it from a gate block:\n%s", out)
	}
	c := summaryCountersFor(t, out, "sonarr decision summary")
	if c["reverseFindings"] != 1 || c["reverseWithheld"] != 1 || c["remonitored"] != 0 {
		t.Errorf("want reverseFindings=1 reverseWithheld=1 remonitored=0, got %v:\n%s", c, out)
	}
	assertReverseIdentity(t, out)
}

// TestRunSonarrDecisionEngine_ReverseScan_CrossCheckFailed_WithholdsEverySeasonWrite
// is binding controller resolution 6 on the SONARR engine, whose gate block is a
// second, independent copy of the check (remonitorSeasons has its own) that no
// test exercised: every Sonarr reverse test ran under a passing cross-check, so
// deleting that block would have written two PUTs per season on data the cycle
// had declared unsound, with reverseWithheld=0 and no warning, and the whole
// suite would still have passed.
//
// The failure is built the way a real one arrives: season 1 is monitored,
// complete, fully aired and ABSENT from the forward wanted set, while its own
// episodeFile.qualityCutoffNotMet says the cutoff is not met — the two signals
// Sonarr computes separately, disagreeing.
func TestRunSonarrDecisionEngine_ReverseScan_CrossCheckFailed_WithholdsEverySeasonWrite(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "," + episodeJSON(200, 2, 1, pastAirDate, 600) + "]"
	// File 500 scores below the profile's cutoff of 100, so season 1 is an
	// ordinary skip rather than a would-unmonitor decision: this test is about
	// the REVERSE gate, and a forward write candidate would only muddy it.
	filesJSON := "[" + episodeFileJSON(500, 1, 10, true) + "," + episodeFileJSON(600, 2, 200, true) + "]"
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	fake.reverseWantedJSON = `{"page":1,"pageSize":100,"totalRecords":1,"records":[{"id":200,"seriesId":1,"seasonNumber":2}]}`

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	series := []seriesElement{testSeries(1, "Contradictory Show", true, 1, []int{},
		testSeason(1, true, 1, 1), testSeason(2, false, 1, 1))}

	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{},
		"cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), false, reverseOptions{enabled: true, remonitor: true}, fileReportOptions{})

	out := buf.String()
	if !strings.Contains(out, "crossCheck=FAILED") {
		t.Fatalf("this test is only meaningful if the cross-check really failed:\n%s", out)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("a failed cross-check must block every reverse write, in both of the season path's two calls: %+v", writes)
	}
	if n := fake.countRequests("/api/v3/series/1"); n != 0 {
		t.Errorf("a blocked pass must not even make its pre-write fetch, got %d", n)
	}
	if !strings.Contains(out, "reverse-scan writes withheld for this instance") {
		t.Errorf("a blocked reverse pass must say so: 'nothing was written' and 'nothing needed writing' are different facts:\n%s", out)
	}
	c := summaryCountersFor(t, out, "sonarr decision summary")
	if c["reverseFindings"] != 1 || c["reverseWithheld"] != 1 || c["remonitored"] != 0 {
		t.Errorf("want reverseFindings=1 reverseWithheld=1 remonitored=0, got %v:\n%s", c, out)
	}
	assertReverseIdentity(t, out)
}

// sonarrReverseWriteFixtures is the smallest library a Sonarr reverse WRITE test
// can be built from: the series under test (1), plus the cross-check witness (9)
// without which the gate never opens and every such test would prove only that a
// blocked pass writes nothing.
//
// The two series share the fake's single /episode and /episodefile fixture — the
// fake stamps the requested seriesId onto every episode — so each season number
// is used by exactly one of them, which is what keeps the shared fixture honest.
// Season 1 belongs to the witness; season 2 to the series under test.
func sonarrReverseWriteFixtures(seasonTwoEpisodeMonitored bool, seasonTwoScore int, seasonTwoCutoffNotMet bool) (episodesJSON, filesJSON string, series []seriesElement, wantedEpisodes map[int]bool, wantedSeasons map[seasonKey]bool) {
	episodesJSON = "[" + episodeJSON(900, 1, 1, pastAirDate, 9000) + "," +
		episodeJSONWithMonitored(200, 2, 1, pastAirDate, 600, seasonTwoEpisodeMonitored) + "]"
	filesJSON = "[" + episodeFileJSON(9000, 1, 200, true) + "," + episodeFileJSON(600, 2, seasonTwoScore, seasonTwoCutoffNotMet) + "]"
	series = []seriesElement{
		// The witness: monitored, complete, aired, in the FORWARD wanted set with
		// its own file agreeing. A plain forward skip that is never written in
		// either direction — its only job is to give the cross-check something
		// real to verify.
		testSeries(9, "Ordinary Monitored Show", true, 1, []int{}, testSeason(1, true, 1, 1)),
		testSeries(1, "Reverse Write Candidate", true, 1, []int{}, testSeason(2, false, 1, 1)),
	}
	return episodesJSON, filesJSON, series, map[int]bool{900: true}, map[seasonKey]bool{{seriesID: 9, seasonNumber: 1}: true}
}

// TestRunSonarrDecisionEngine_ReverseScan_SeriesRetiredBeforeTheWrite_IsRefused
// is binding controller resolution 3's SECOND enforcement point: the reverse
// half of the write path's own series-monitored guard.
//
// The first enforcement — remonitorSeasons withholding a season whose series the
// SCAN saw unmonitored — is pinned elsewhere, and that test asserts the write
// path is never entered at all, so it cannot reach this line. What is only
// checkable here is the window between the two: a human retires the show after
// the scan has already decided, and the pre-write fetch is the first thing to
// see it. Re-monitoring seasons of a deliberately retired show is the single
// behaviour resolution 3 exists to forbid.
func TestRunSonarrDecisionEngine_ReverseScan_SeriesRetiredBeforeTheWrite_IsRefused(t *testing.T) {
	episodesJSON, filesJSON, series, wantedEpisodes, wantedSeasons := sonarrReverseWriteFixtures(false, 200, true)
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	fake.reverseWantedJSON = `{"page":1,"pageSize":100,"totalRecords":1,"records":[{"id":200,"seriesId":1,"seasonNumber":2}]}`
	// The pre-write fetch, and the only thing that differs from the scan: the
	// series itself is no longer monitored.
	fake.seriesDetail[1] = `{"id":1,"title":"Reverse Write Candidate","monitored":false,"qualityProfileId":1,"tags":[],` +
		`"seasons":[{"seasonNumber":2,"monitored":false,"statistics":{"episodeFileCount":1,"totalEpisodeCount":1}}]}`

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, wantedEpisodes, wantedSeasons,
		"cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), false, reverseOptions{enabled: true, remonitor: true}, fileReportOptions{})

	out := buf.String()
	if !strings.Contains(out, `crossCheck="passed (1 verified`) {
		t.Fatalf("this test proves nothing unless the write gate really opened:\n%s", out)
	}
	// The distinction this test exists for: the write path was ENTERED — it made
	// its own pre-write fetch — and stopped at its own guard, rather than being
	// withheld before any of that (which is the other test's subject).
	if n := fake.countRequests("/api/v3/series/1"); n == 0 {
		t.Fatalf("the write path was never entered, so this test says nothing about its guard:\n%s", out)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("a season whose series is no longer monitored must never be written: %+v", writes)
	}
	if !strings.Contains(out, "series is no longer monitored as of the pre-write fetch") {
		t.Errorf("the refusal must say which guard stopped it:\n%s", out)
	}
	c := summaryCountersFor(t, out, "sonarr decision summary")
	if c["reverseFindings"] != 1 || c["remonitorsRefused"] != 1 || c["remonitored"] != 0 {
		t.Errorf("want reverseFindings=1 remonitorsRefused=1 remonitored=0, got %v:\n%s", c, out)
	}
	assertReverseIdentity(t, out)
}

// TestRunSonarrDecisionEngine_ReverseScan_SeasonThatNowMeetsCriteria_IsRefused
// is the season path's own fresh-GET re-verification (binding controller
// resolution 5: "still FAILS criteria on fresh data"), which had no
// discriminating test — only the happy path's PASS through it, so the whole
// check could be deleted with nothing failing.
//
// It is the Sonarr twin of the Radarr pin that stages a CF-score rise, and it
// has to be staged that way here too: the airing and completeness levers cannot
// reach this code, because verifySeasonStillWritable refuses first with a
// different sentinel. The season is below its custom-format cutoff when it is
// found, and above it by the time the write pass re-reads /episodefile — so
// re-monitoring it would undo this project's own correct work, after which the
// next forward cycle would unmonitor it again, forever.
func TestRunSonarrDecisionEngine_ReverseScan_SeasonThatNowMeetsCriteria_IsRefused(t *testing.T) {
	// Absent from the unmonitored wanted set (its quality cutoff is met) and
	// scoring 10 against the profile's 100: a custom-format finding.
	episodesJSON, filesJSON, series, wantedEpisodes, wantedSeasons := sonarrReverseWriteFixtures(false, 10, false)
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	fake.seriesDetail[1] = sonarrWriteEngineSeriesDetail(1, "Reverse Write Candidate", 2, 1, false)
	// The world changes between the scan and the write: the file was upgraded and
	// now scores above the cutoff.
	fake.writeTimeFileJSON = "[" + episodeFileJSON(9000, 1, 200, true) + "," + episodeFileJSON(600, 2, 200, false) + "]"

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, wantedEpisodes, wantedSeasons,
		"cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), false, reverseOptions{enabled: true, remonitor: true}, fileReportOptions{})

	out := buf.String()
	if !strings.Contains(out, `reason="`+ReasonCFCutoffNotMet+`"`) {
		t.Fatalf("this test proves nothing unless the CF-score finding was made first:\n%s", out)
	}
	if !strings.Contains(out, `crossCheck="passed (1 verified`) {
		t.Fatalf("nor unless the write gate really opened:\n%s", out)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("a season that now meets the criteria must not be re-monitored: %+v", writes)
	}
	if !strings.Contains(out, "no longer fails the criteria") {
		t.Errorf("the refusal must say why:\n%s", out)
	}
	c := summaryCountersFor(t, out, "sonarr decision summary")
	if c["reverseFindings"] != 1 || c["remonitorsRefused"] != 1 || c["remonitored"] != 0 {
		t.Errorf("want reverseFindings=1 remonitorsRefused=1 remonitored=0, got %v:\n%s", c, out)
	}
	assertReverseIdentity(t, out)
}

// TestRunSonarrDecisionEngine_ReverseScan_MismatchSeasonWithUnmonitoredEpisodes_IsRefused
// is the human whose deliberate state the clean-season mandate protects (binding
// controller ruling R2): somebody monitored episode 1 of an otherwise-finished
// unmonitored season by hand.
//
// A mismatch season MEETS every criterion — rules 4, 5 and 7 all passed — and is
// reported for one reason only: an episode inside it is monitored while its own
// flag is not. Re-monitoring it the ordinary way writes EVERY episode that is
// currently false, so episode 2 would be dragged along, and the next forward
// cycle would then unmonitor the whole season, including the episode the human
// chose — a state rule 1 used to protect. The plan's write mandate does not
// reach that far either: re-monitoring is scoped to "an unmonitored item that
// FAILS criteria", and this season fails none.
func TestRunSonarrDecisionEngine_ReverseScan_MismatchSeasonWithUnmonitoredEpisodes_IsRefused(t *testing.T) {
	// Season 2 is complete, fully aired, absent from the unmonitored wanted set
	// and scoring 200 against the profile's 100. Episode 200 is monitored by
	// hand; episode 201 is not, and is the one an ordinary write would touch.
	// Season 1 belongs to the cross-check witness, without which the gate never
	// opens (see sonarrReverseWriteFixtures).
	episodesJSON := "[" + episodeJSON(900, 1, 1, pastAirDate, 9000) + "," +
		episodeJSONWithMonitored(200, 2, 1, pastAirDate, 600, true) + "," +
		episodeJSONWithMonitored(201, 2, 2, pastAirDate, 601, false) + "]"
	filesJSON := "[" + episodeFileJSON(9000, 1, 200, true) + "," +
		episodeFileJSON(600, 2, 200, false) + "," + episodeFileJSON(601, 2, 200, false) + "]"
	series := []seriesElement{
		testSeries(9, "Ordinary Monitored Show", true, 1, []int{}, testSeason(1, true, 1, 1)),
		testSeries(1, "Hand Monitored Episode", true, 1, []int{}, testSeason(2, false, 2, 2)),
	}
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	fake.seriesDetail[1] = sonarrWriteEngineSeriesDetail(1, "Hand Monitored Episode", 2, 2, false)

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series,
		map[int]bool{900: true}, map[seasonKey]bool{{seriesID: 9, seasonNumber: 1}: true},
		"cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), false, reverseOptions{enabled: true, remonitor: true}, fileReportOptions{})

	out := buf.String()
	line := reverseFindingLine(t, out)
	if !strings.Contains(line, `reason="`+ReasonSeasonMonitorMismatch+`"`) {
		t.Fatalf("this test proves nothing unless the mismatch was reported first:\n%s", line)
	}
	if !strings.Contains(out, `crossCheck="passed (1 verified`) {
		t.Fatalf("nor unless the write gate really opened:\n%s", out)
	}
	// The distinction from a withheld finding: the write path was ENTERED, made
	// its fresh reads, and stopped at its own predicate.
	if n := fake.countRequests("/api/v3/series/1"); n == 0 {
		t.Fatalf("the write path was never entered, so this test says nothing about its predicate:\n%s", out)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("a mismatch season may not be written at all — neither the episodes a human left alone nor the flag: %+v", writes)
	}
	if !strings.Contains(out, "already has monitored episodes inside it") {
		t.Errorf("the refusal must say what stopped it:\n%s", out)
	}
	c := summaryCountersFor(t, out, "sonarr decision summary")
	if c["reverseFindings"] != 1 || c["remonitorsRefused"] != 1 || c["remonitored"] != 0 {
		t.Errorf("want reverseFindings=1 remonitorsRefused=1 remonitored=0, got %v:\n%s", c, out)
	}
	assertReverseIdentity(t, out)
}

// TestRun_ReverseScan_Sonarr_DryRun_RehearsesAndWritesNothing is §2.1 for the
// season path's TWO write calls: both gates hold, and the rehearsal still runs
// every read and every check in front of them.
func TestRun_ReverseScan_Sonarr_DryRun_RehearsesAndWritesNothing(t *testing.T) {
	fake := reverseFindingSonarrFake(t, true)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--config", writeReverseSonarrTestConfig(t, fake.srv.URL, true, true), "--once"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code; stderr=%s", stderr.String())
	}

	out := stdout.String()
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("dry-run must write nothing: %+v", writes)
	}
	if n := fake.countRequests("/api/v3/series/1"); n == 0 {
		t.Errorf("the dry-run must still rehearse the write path, including its fresh pre-write fetch:\n%s", out)
	}
	c := summaryCountersFor(t, out, "sonarr decision summary")
	if c["reverseFindings"] != 1 || c["reverseWithheld"] != 1 || c["remonitored"] != 0 {
		t.Errorf("want reverseFindings=1 reverseWithheld=1 remonitored=0, got %v:\n%s", c, out)
	}
	assertReverseIdentity(t, out)
}

// --- scheduling: which cycles run a reverse scan at all ---------------------
//
// Binding controller resolution 8. The reverse scan belongs to full-library
// cycles and to nothing else. A webhook cycle is about the one item somebody
// just imported; an --only-id run is about the one item a human named. Running
// a whole-library reverse scan inside either would answer a question nobody
// asked, at the cost of a second pass over the library — in the webhook case,
// on every import, which is precisely the cost the debounce exists to bound.

// writeReverseDaemonConfig is writeDaemonConfig plus the reverse write switch.
func writeReverseDaemonConfig(t *testing.T, url string, remonitor bool, pollInterval, logLevel string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	content := fmt.Sprintf(`
dry_run: true
log_level: %s
poll_interval: %s
webhook_debounce: 45s
reverse_scan_remonitor: %t
instances:
  - name: radarr-main
    type: radarr
    url: %s
    api_key: key1
`, logLevel, pollInterval, remonitor, url)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing the daemon test config: %v", err)
	}
	return path
}

// TestDaemon_StartupScanAndReconciliationSweep_BothRunTheReverseScan is the
// positive half of the scheduling rule.
func TestDaemon_StartupScanAndReconciliationSweep_BothRunTheReverseScan(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{reverseFindingStatefulMovie(7, "Accidentally Unmonitored")})
	h := startDaemon(t, writeReverseDaemonConfig(t, fake.srv.URL, false, "1h", "info"))
	h.waitReady()

	startup := h.out.String()
	if !strings.Contains(startup, `msg="reverse-scan finding"`) || !strings.Contains(startup, "reverseFindings=1") {
		t.Fatalf("the startup scan must run the reverse scan and report it in full:\n%s", startup)
	}
	// FLAKE FIX, round 2: the loop computes its first sweep deadline from the
	// clock as it enters, asynchronously, AFTER the startup scan returns, and
	// this eventually() waits for the ONE log line that announces it —
	// written once, at the very top of loop(), before the for-loop's first
	// iteration even begins. That proves nextReconcile has been COMPUTED, but
	// NOT that d.clock.NewTimer(wait) has actually been CALLED: that happens
	// several lines later, on that same first iteration, after the loop's own
	// queue/scan checks. A single unretried Advance can still land in that
	// remaining sliver — before the timer is armed — computing its target
	// from a now the clock has already moved past, so the sweep silently
	// never fires within this test's own timeout (this is the CI failure
	// this round fixes: "timed out waiting for 1 occurrence(s) of
	// 'reconciliation sweep complete'" at the 5s ceiling; reproduced locally
	// with a 50ms artificial delay inserted between daemon.go's
	// d.clock.Now() read and its d.clock.NewTimer(wait) call — the test
	// failed identically, at the same 5.01s mark). This is the exact race
	// documented on advanceClockUntilReconciliationSweepFires
	// (filereport_test.go) and the SAME rework applies here: retry Advance
	// (safe — it is idempotent) until the sweep it triggers actually
	// completes, rather than racing a single Advance against a wall-clock
	// timeout.
	eventually(t, "the reconciliation schedule to be announced", func() bool {
		return strings.Contains(h.out.String(), "reconciliation sweep scheduled")
	})

	mark := h.mark()
	advanceClockUntilReconciliationSweepFires(t, h, mark, time.Hour)
	h.awaitLogCount("reconciliation sweep complete", 1)
	sweep := h.since(mark)
	h.stop()

	if !strings.Contains(sweep, "reverseFindings=1") {
		t.Errorf("the reconciliation sweep must run the reverse scan too:\n%s", sweep)
	}
}

// TestDaemon_WebhookCycle_RunsNoReversePass is the negative half, and the one
// that costs something if it regresses: a busy library imports all day.
func TestDaemon_WebhookCycle_RunsNoReversePass(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{
		wouldUnmonitorStatefulMovie(1, "Imported"),
		reverseFindingStatefulMovie(7, "Accidentally Unmonitored"),
	})
	h := startDaemon(t, writeReverseDaemonConfig(t, fake.srv.URL, false, "0", "debug"))
	h.waitReady()
	if !strings.Contains(h.out.String(), "reverseFindings=") {
		t.Fatalf("this test proves nothing unless the startup scan really did run a reverse pass:\n%s", h.out.String())
	}

	mark := h.mark()
	h.post("radarr-main", downloadMoviePayload)
	eventually(t, "the event to be queued", func() bool {
		return strings.Contains(h.out.String(), "webhook queued")
	})
	h.clock.Advance(45 * time.Second)
	h.awaitLogCount("webhook debounce expired; evaluating", 1)
	// FLAKE FIX: this was a fixed 20ms sleep, the pattern the older webhook
	// tests use for "let the cycle finish". Every assertion below is about what
	// that cycle logged, and under load 20ms is not always enough for it to log
	// anything — observed once as the fatal below, on a machine running six
	// suites at once. The summary line is the cycle's own last word, so waiting
	// for it is the synchronization point that was missing.
	eventually(t, "the webhook cycle to finish", func() bool {
		return strings.Contains(h.since(mark), "radarr decision summary")
	})
	h.stop()

	cycle := h.since(mark)
	if !strings.Contains(cycle, "radarr decision summary") {
		t.Fatalf("the webhook cycle must have run at all:\n%s", cycle)
	}
	assertNoReversePass(t, cycle)
}

// TestRun_OnlyID_FlagOff_RunsNoReversePass is the same rule for the scoped
// one-shot run, in the configuration that is the product: report-only.
//
// A scoped run is about the one item a human named. With re-monitoring switched
// off there is nothing the reverse pass could do about that item, so running a
// second pass over the unmonitored half of the library would answer a question
// nobody asked at the cost of a full extra scan. (With the switch ON it becomes
// the acceptance instrument — see
// TestRun_OnlyID_WithRemonitorFlag_RunsBothDirectionsOnThatItemAlone.)
func TestRun_OnlyID_FlagOff_RunsNoReversePass(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{
		wouldUnmonitorStatefulMovie(1, "Named"),
		reverseFindingStatefulMovie(7, "Accidentally Unmonitored"),
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", writeReverseTestConfig(t, fake.srv.URL, true, false), "--once", "--only-id", "1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d; stderr=%s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "onlyId=1") {
		t.Fatalf("this test proves nothing unless the run really was scoped:\n%s", out)
	}
	assertNoReversePass(t, out)
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("a dry-run must write nothing at all: %+v", writes)
	}
}

// --- the scoped acceptance instrument (controller ruling R7) ----------------
//
// v1's forward writes have never run against a live library, and are not
// authorized to. That leaves the reverse direction's first real write with
// nothing to learn from, and "switch the daemon on and watch" is not an
// acceptance procedure for a program whose whole claim is that it writes one
// field to items it is certain about.
//
// So `--once --only-id N --instance X` WITH reverse_scan_remonitor:true becomes
// the instrument: it runs BOTH directions, each scoped to that one id — the
// forward evaluation of that item exactly as before, and a reverse pass that
// evaluates that item and nothing else, skipping it unless it is a current
// finding on fresh data. Every gate, refusal and echo rule applies unchanged;
// the only thing narrowed is which items exist as far as the pass is concerned.
//
// The scoping guarantee is therefore held to the same standard as this
// project's zero-write pins: exactly one item, either direction, nothing else
// ever. Without the flag, scoped runs stay forward-only (above).

// TestRun_OnlyID_WithRemonitorFlag_RunsBothDirectionsOnThatItemAlone is the
// instrument itself, staged as the acceptance run would be: a library holding
// the named item, a second identical reverse finding, a forward would-unmonitor
// candidate, and the cross-check witness that makes the gate's verdict real.
// Exactly one write leaves the process, to exactly the named movie.
func TestRun_OnlyID_WithRemonitorFlag_RunsBothDirectionsOnThatItemAlone(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{
		crossCheckWitnessStatefulMovie(5, "Ordinary Monitored"),
		// The forward direction's own candidate: monitored and finished, so a
		// full write-mode cycle would unmonitor it. Nothing may touch it here.
		wouldUnmonitorStatefulMovie(1, "Would Be Unmonitored"),
		reverseFindingStatefulMovie(7, "The Named Item"),
		reverseFindingStatefulMovie(8, "The Other Accidental Unmonitor"),
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", writeReverseTestConfig(t, fake.srv.URL, false, true), "--once", "--only-id", "7"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d; stderr=%s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "onlyId=7") {
		t.Fatalf("this test proves nothing unless the run really was scoped:\n%s", out)
	}
	// The gate's evidence comes from the FORWARD pass, which is library-wide
	// even on a scoped run (the cross-check validates the data, not the target),
	// so both monitored movies are sampled and verified here.
	if !strings.Contains(out, `crossCheck="passed (2 verified`) {
		t.Fatalf("nor unless the write gate really opened on a verified sample:\n%s", out)
	}

	puts := fake.puts()
	if len(puts) != 1 || puts[0].path != "/api/v3/movie/7" {
		t.Fatalf("a scoped run may write exactly the item it names, in either direction, got %+v", puts)
	}
	if !fake.movie(7).monitored {
		t.Error("the named movie must be re-monitored")
	}
	if fake.movie(8).monitored {
		t.Error("movie 8 must be left exactly as it was found: unmonitored")
	}
	if !fake.movie(1).monitored {
		t.Error("the forward candidate must be left monitored: a scoped run writes nothing it did not name")
	}
	// Not merely unwritten: never even looked at by the write path, and never
	// reported. A finding line for another item on a scoped run would send a
	// human to act on something this run was not asked about.
	for _, id := range []int{1, 8} {
		if n := fake.countRequests("/api/v3/movie/" + strconv.Itoa(id)); n != 0 {
			t.Errorf("movie %d got %d pre-write fetch(es); a scoped run must not enter any write path but the named one", id, n)
		}
	}
	if strings.Contains(out, "The Other Accidental Unmonitor") {
		t.Errorf("a scoped reverse pass must report only the item it was given:\n%s", out)
	}
	if n := strings.Count(out, `msg="reverse-scan finding"`); n != 1 {
		t.Errorf("want exactly one reverse-scan finding, got %d:\n%s", n, out)
	}
	c := summaryCounters(t, out)
	if c["reverseFindings"] != 1 || c["remonitored"] != 1 {
		t.Errorf("want reverseFindings=1 remonitored=1, got %v:\n%s", c, out)
	}
	if c["unmonitored"] != 0 || c["wouldUnmonitor"] != 0 {
		t.Errorf("the forward half of a scoped run must be about the named item only, got %v:\n%s", c, out)
	}
	assertReverseIdentity(t, out)
}

// TestRun_OnlyID_WithRemonitorFlag_ItemThatIsNotAFinding_WritesNothing is the
// other half of the instrument: it is a QUESTION, not an instruction. Naming an
// item that is unmonitored and meets the criteria — cutoffarr's own ordinary
// output — must produce a reverse pass that ran, found nothing, and wrote
// nothing, rather than a re-monitor on the strength of having been asked.
func TestRun_OnlyID_WithRemonitorFlag_ItemThatIsNotAFinding_WritesNothing(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{
		crossCheckWitnessStatefulMovie(5, "Ordinary Monitored"),
		// Unmonitored, with a file, and meeting every criterion: exactly what
		// this program leaves behind, and never a finding.
		{id: 7, title: "Finished And Unmonitored", monitored: false, hasFile: true,
			qualityProfileID: 1, tags: []int{}, movieFileID: 7, cfScore: 200,
			qualityCutoffNotMet: false, inWantedSet: false},
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", writeReverseTestConfig(t, fake.srv.URL, false, true), "--once", "--only-id", "7"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d; stderr=%s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "reverseFindings=0") {
		t.Fatalf("the pass must have run and reported nothing:\n%s", out)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("naming an item is not an instruction to write it: %+v", writes)
	}
	if fake.movie(7).monitored {
		t.Error("the named movie must be left unmonitored")
	}
	assertReverseIdentity(t, out)
}

// TestRun_OnlyID_WithRemonitorFlag_Sonarr_WritesOnlyTheNamedSeries is the
// instrument on the other *arr, where the scope names a SERIES and the reverse
// pass has a second level of narrowing under it. Two series, each with an
// unmonitored qualifying season; one of them is named.
func TestRun_OnlyID_WithRemonitorFlag_Sonarr_WritesOnlyTheNamedSeries(t *testing.T) {
	witnessSeries, witnessEpisodes, witnessFiles := crossCheckWitnessSonarrFixtures()
	fake := newStatefulSonarrFake(t,
		append(witnessSeries,
			&statefulSonarrSeries{id: 1, title: "The Named Show", monitored: true, profileID: 1, tags: []int{},
				seasons: []statefulSonarrSeason{{number: 2, monitored: false, episodeFileCount: 1, totalEpisodeCount: 1}}},
			&statefulSonarrSeries{id: 2, title: "The Other Show", monitored: true, profileID: 1, tags: []int{},
				seasons: []statefulSonarrSeason{{number: 3, monitored: false, episodeFileCount: 1, totalEpisodeCount: 1}}}),
		append(witnessEpisodes,
			&statefulSonarrEpisode{id: 200, seriesID: 1, seasonNumber: 2, episodeNumber: 1, monitored: false, hasFile: true,
				airDateUtc: pastAirDate, episodeFileID: 600, inWantedSet: true},
			&statefulSonarrEpisode{id: 300, seriesID: 2, seasonNumber: 3, episodeNumber: 1, monitored: false, hasFile: true,
				airDateUtc: pastAirDate, episodeFileID: 700, inWantedSet: true}),
		append(witnessFiles,
			&statefulSonarrEpisodeFile{id: 600, seasonNumber: 2, customFormatScore: 200, qualityCutoffNotMet: true},
			&statefulSonarrEpisodeFile{id: 700, seasonNumber: 3, customFormatScore: 200, qualityCutoffNotMet: true}),
	)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", writeReverseSonarrTestConfig(t, fake.srv.URL, false, true), "--once", "--only-id", "1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d; stderr=%s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "onlyId=1") || !strings.Contains(out, `crossCheck="passed (1 verified`) {
		t.Fatalf("this test needs a scoped run whose gate really opened:\n%s", out)
	}
	for _, w := range fake.writes() {
		if w.path != "/api/v3/series/1" && w.path != "/api/v3/episode/monitor" {
			t.Errorf("a scoped run wrote to %s %s", w.method, w.path)
		}
	}
	if !fake.seasonMonitored(1, 2) {
		t.Errorf("the named series' season must be re-monitored:\n%s", out)
	}
	if fake.seasonMonitored(2, 3) {
		t.Error("the other series must be untouched")
	}
	if n := fake.countRequests("/api/v3/series/2"); n != 0 {
		t.Errorf("the other series' write path was entered %d time(s)", n)
	}
	if strings.Contains(out, "The Other Show") {
		t.Errorf("a scoped reverse pass must report only the series it was given:\n%s", out)
	}
	c := summaryCountersFor(t, out, "sonarr decision summary")
	if c["reverseFindings"] != 1 || c["remonitored"] != 1 {
		t.Errorf("want reverseFindings=1 remonitored=1, got %v:\n%s", c, out)
	}
	assertReverseIdentity(t, out)
}

// TestDaemon_WebhookCycle_WithRemonitorFlag_StillRunsNoReversePass keeps the
// instrument from widening into the cycle it must never reach. A webhook cycle
// is scoped too, and R7 gives scoped runs a reverse pass when the write switch
// is on — but only the --once ones a human is watching. A webhook fires
// unattended on every import, and a reverse pass there would be both a
// per-import full scan and a write nobody asked for at that moment.
func TestDaemon_WebhookCycle_WithRemonitorFlag_StillRunsNoReversePass(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{
		wouldUnmonitorStatefulMovie(1, "Imported"),
		reverseFindingStatefulMovie(7, "Accidentally Unmonitored"),
	})
	h := startDaemon(t, writeReverseDaemonConfig(t, fake.srv.URL, true, "0", "debug"))
	h.waitReady()
	if !strings.Contains(h.out.String(), "remonitored=") {
		t.Fatalf("this test proves nothing unless the startup scan really did run a reverse pass with the switch on:\n%s", h.out.String())
	}

	mark := h.mark()
	h.post("radarr-main", downloadMoviePayload)
	eventually(t, "the event to be queued", func() bool {
		return strings.Contains(h.out.String(), "webhook queued")
	})
	h.clock.Advance(45 * time.Second)
	h.awaitLogCount("webhook debounce expired; evaluating", 1)
	eventually(t, "the webhook cycle to finish", func() bool {
		return strings.Contains(h.since(mark), "radarr decision summary")
	})
	h.stop()

	assertNoReversePass(t, h.since(mark))
}

// TestDaemon_IdleCycleWithReverseFindings_StaysWithinTheNoiseBudget is the
// Phase 8 noise budget extended to the new lines (binding controller resolution
// 9), and the reason it matters is that findings REPEAT: they stay true until a
// human acts on them, so a reverse scan that reported at INFO on every sweep
// would print the same lines every poll interval forever and bury the writes and
// warnings the log exists for.
//
// The rule is the existing one, unchanged: on a repeating cycle nothing that
// scales with the size of the library may be INFO. The per-instance summary
// carries the count, and the findings themselves are there at DEBUG for anyone
// who turns it on — which the startup scan proves, since it printed all of them.
func TestDaemon_IdleCycleWithReverseFindings_StaysWithinTheNoiseBudget(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{
		reverseFindingStatefulMovie(7, "Accidentally Unmonitored A"),
		reverseFindingStatefulMovie(8, "Accidentally Unmonitored B"),
	})
	h := startDaemon(t, writeReverseDaemonConfig(t, fake.srv.URL, false, "1h", "info"))
	h.waitReady()

	startup := h.out.String()
	if n := strings.Count(startup, `msg="reverse-scan finding"`); n != 2 {
		t.Fatalf("the startup scan must report both findings in full, got %d:\n%s", n, startup)
	}
	// The loop announces its schedule once, asynchronously, after the startup
	// scan returns; waiting for it keeps that one-off line out of the cycles
	// being compared (the same wait the existing idle-cycle tests make).
	eventually(t, "the reconciliation schedule to be announced", func() bool {
		return strings.Contains(h.out.String(), "reconciliation sweep scheduled")
	})

	// See advanceClockUntilReconciliationSweepFires' own doc comment
	// (filereport_test.go) for why a single, unretried Advance here can
	// still race the daemon's own timer registration — for BOTH sweeps
	// below, not only the first.
	mark := h.mark()
	advanceClockUntilReconciliationSweepFires(t, h, mark, time.Hour)
	h.awaitLogCount("reconciliation sweep complete", 1)
	cycle2 := h.since(mark)

	mark3 := h.mark()
	advanceClockUntilReconciliationSweepFires(t, h, mark3, time.Hour)
	h.awaitLogCount("reconciliation sweep complete", 2)
	cycle3 := h.since(mark3)
	h.stop()

	for i, cycle := range []string{cycle2, cycle3} {
		// The existing budget, unchanged and now applied to a cycle that has
		// reverse findings in it: every INFO line must be on the allowlist, so a
		// per-item finding line at INFO fails here rather than merely being
		// noticed by a human six months from now.
		if !assertIdleCycleInfoIsWithinTheBudget(t, cycle) {
			t.Errorf("cycle %d printed no per-instance decision summary:\n%s", i+2, cycle)
		}
		if !strings.Contains(cycle, "reverseFindings=2") {
			t.Errorf("cycle %d must still COUNT the findings on its summary — the count is what stays visible:\n%s", i+2, cycle)
		}
	}
	// Two idle cycles must say the same thing, which is the strongest form of
	// "nothing changed, so nothing new was said". nextSweep legitimately differs
	// (it names a later time), so the sweep-complete line is the one exception.
	if strip2, strip3 := withoutTimestamps(dropNextSweep(cycle2)), withoutTimestamps(dropNextSweep(cycle3)); strip2 != strip3 {
		t.Errorf("two idle cycles with nothing changed must say the same thing:\ncycle2:\n%s\ncycle3:\n%s", strip2, strip3)
	}
}

// assertNoReversePass fails if a cycle shows any sign of having run one. It
// matches the reverse scan's own tokens rather than the bare word "reverse",
// which also appears in the startup config printout (reverse_scan_remonitor=).
func assertNoReversePass(t *testing.T, cycle string) {
	t.Helper()
	for _, token := range []string{"reverse-scan finding", "reverseFindings=", "reverseScan=", "remonitored=", `wantedFilter="monitored=false"`} {
		if strings.Contains(cycle, token) {
			t.Errorf("this cycle must run no reverse pass, but its log carries %q:\n%s", token, cycle)
		}
	}
}

// dropNextSweep removes the nextSweep attr, the one thing about an idle
// reconciliation cycle that legitimately differs from the last one.
func dropNextSweep(cycle string) string {
	var out []string
	for _, line := range strings.Split(cycle, "\n") {
		if i := strings.Index(line, " nextSweep="); i >= 0 {
			line = line[:i]
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// withoutTimestamps strips the leading time= attr from every line, so two
// cycles that said the same thing compare equal. stripTimeAttr (writer_test.go)
// does one line; this does a whole cycle.
func withoutTimestamps(cycle string) string {
	var out []string
	for _, line := range strings.Split(cycle, "\n") {
		out = append(out, stripTimeAttr(line))
	}
	return strings.Join(out, "\n")
}

// TestRunRadarrDecisionEngine_EveryReverseFindingIsAccountedForInTheSummary is
// the reverse pass's accounting identity, walked ending by ending — the same
// test the forward write pass has, for the same reason.
//
// The forward path grew this test because two separate review rounds each found
// a different silent path where a promised write evaporated with every counter
// reading zero. The reverse path is new code with the same shape and the same
// five outcomes, so it gets the same discipline from the start rather than
// after the same two rounds:
//
//	reverseFindings == remonitored + remonitorsRefused + reverseWithheld
//	                   + reverseWriteErrors + reverseRehearsalErrors
//	                   + reverseEchoUnverified
//
// (reverseWriteErrors and reverseRehearsalErrors are the same counter under two
// names — only one is ever printed, decided by dry-run — so summing both is
// exact in either mode.)
func TestRunRadarrDecisionEngine_EveryReverseFindingIsAccountedForInTheSummary(t *testing.T) {
	// The finding: unmonitored, with a file, and in the UNMONITORED wanted set.
	const findingID = 1
	reverseWanted := `{"page":1,"pageSize":100,"totalRecords":1,"records":[{"id":1,"title":"Accounted Movie"}]}`
	unmonitoredDetail := `{"id": 1, "title": "Accounted Movie", "monitored": false, "hasFile": true, "qualityProfileId": 1, "tags": []}`

	cases := []struct {
		name        string
		detail      map[int]string
		tagsJSON    string
		putStatus   map[int]int
		putEcho     map[string]string
		dryRun      bool
		wantCounter string
	}{
		{
			name:        "confirmed write",
			detail:      map[int]string{findingID: unmonitoredDetail},
			wantCounter: "remonitored",
		},
		{
			name:        "the server rejected the PUT",
			detail:      map[int]string{findingID: unmonitoredDetail},
			putStatus:   map[int]int{findingID: http.StatusInternalServerError},
			wantCounter: "reverseWriteErrors",
		},
		{
			name:        "the PUT was accepted but the echo confirms nothing",
			detail:      map[int]string{findingID: unmonitoredDetail},
			putEcho:     map[string]string{"1": ""},
			wantCounter: "reverseEchoUnverified",
		},
		{
			name:        "the movie vanished before the pre-write fetch",
			detail:      map[int]string{},
			wantCounter: "reverseWriteErrors",
		},
		{
			name:        "the exclusion tag was added between scan and write",
			detail:      map[int]string{findingID: `{"id": 1, "title": "Accounted Movie", "monitored": false, "hasFile": true, "qualityProfileId": 1, "tags": [42]}`},
			tagsJSON:    `[{"id": 42, "label": "cutoffarr-exclude"}]`,
			wantCounter: "remonitorsRefused",
		},
		{
			name:        "something else re-monitored it first",
			detail:      map[int]string{findingID: `{"id": 1, "title": "Accounted Movie", "monitored": true, "hasFile": true, "qualityProfileId": 1, "tags": []}`},
			wantCounter: "remonitorsRefused",
		},
		{
			name:        "the fresh payload's monitored is JSON null",
			detail:      map[int]string{findingID: `{"id": 1, "title": "Accounted Movie", "monitored": null, "hasFile": true, "qualityProfileId": 1, "tags": []}`},
			wantCounter: "remonitorsRefused",
		},
		{
			name:        "it no longer fails the criteria (its file is gone)",
			detail:      map[int]string{findingID: `{"id": 1, "title": "Accounted Movie", "monitored": false, "hasFile": false, "qualityProfileId": 1, "tags": []}`},
			wantCounter: "remonitorsRefused",
		},
		{
			name:        "dry-run withheld the write at the gate",
			detail:      map[int]string{findingID: unmonitoredDetail},
			dryRun:      true,
			wantCounter: "reverseWithheld",
		},
		{
			name:        "dry-run rehearsal failed before the gate",
			detail:      map[int]string{},
			dryRun:      true,
			wantCounter: "reverseRehearsalErrors",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newRadarrFake(t, "", tc.detail)
			fake.reverseWantedJSON = reverseWanted
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
			// The witness is what opens the write gate: a cycle whose
			// cross-check sampled nothing withholds every reverse write, and
			// then every case below would end in the same counter and prove
			// nothing about the others.
			movies := []movieListElement{crossCheckWitnessMovie(5, "Ordinary Monitored"), unmonitoredBelowCutoffMovie(findingID, "Accounted Movie")}
			runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{5: true}, "cutoffarr-exclude",
				fullLibraryScope(slog.LevelInfo), tc.dryRun, reverseOptions{enabled: true, remonitor: true}, fileReportOptions{})

			out := buf.String()
			c := summaryCounters(t, out)
			if c["reverseFindings"] != 1 {
				t.Fatalf("reverseFindings = %d, want 1: this case must actually reach the write pass or it proves nothing:\n%s", c["reverseFindings"], out)
			}
			if c[tc.wantCounter] != 1 {
				t.Errorf("%s = %d, want 1: this outcome must be counted under that name:\n%s", tc.wantCounter, c[tc.wantCounter], out)
			}
			assertReverseIdentity(t, out)
		})
	}
}

// TestRunRadarrDecisionEngine_ReverseGateBlocked_WithheldAccountsForThePass is
// the identity's remaining term, which no case above can reach: when the
// cross-check refuses to authorize the pass, every finding is withheld before
// remonitorMovie is called even once.
func TestRunRadarrDecisionEngine_ReverseGateBlocked_WithheldAccountsForThePass(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{})
	fake.reverseWantedJSON = `{"page":1,"pageSize":100,"totalRecords":2,"records":[{"id":7,"title":"A"},{"id":8,"title":"B"}]}`

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{
		// The forward cross-check fails on this one.
		{ID: intPtr(1), Title: strPtr("Contradictory"), Monitored: boolPtr(true), HasFile: boolPtr(true),
			QualityProfileID: intPtr(1), Tags: &noTags,
			MovieFile: &movieFileElement{ID: intPtr(1), QualityCutoffNotMet: boolPtr(true)}},
		unmonitoredBelowCutoffMovie(7, "A"),
		unmonitoredBelowCutoffMovie(8, "B"),
	}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude",
		fullLibraryScope(slog.LevelInfo), false, reverseOptions{enabled: true, remonitor: true}, fileReportOptions{})

	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("a blocked pass must write nothing, got %+v", writes)
	}
	out := buf.String()
	c := summaryCounters(t, out)
	if c["reverseFindings"] != 2 || c["reverseWithheld"] != 2 {
		t.Errorf("want reverseFindings=2 reverseWithheld=2, got %v:\n%s", c, out)
	}
	assertReverseIdentity(t, out)
}

// TestRunRadarrDecisionEngine_ReverseScan_ShutdownMidEvaluation_ReportsNoFindingCount
// is the shutdown boundary applied to the new pass. A cycle cut short has an
// incomplete picture, and the reverse scan's output is a COUNT of things that
// are wrong — reporting a partial one as though it were the whole library is
// exactly the false all-clear the skipped state exists to prevent.
func TestRunRadarrDecisionEngine_ReverseScan_ShutdownMidEvaluation_ReportsNoFindingCount(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{
		// Evaluated first, and the only movie whose evaluation makes a request
		// of its own (rule 6's /moviefile, reached because its quality cutoff IS
		// met): the hook below turns that request into the shutdown.
		{id: 1, title: "Evaluated First", monitored: false, hasFile: true, qualityProfileID: 1,
			tags: []int{}, movieFileID: 1, cfScore: 10, qualityCutoffNotMet: false, inWantedSet: false},
		reverseFindingStatefulMovie(2, "Never Reached"),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake.onRequest = func(method, path string) {
		if path == "/api/v3/moviefile" {
			cancel()
		}
	}

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{
		{ID: intPtr(1), Title: strPtr("Evaluated First"), Monitored: boolPtr(false), HasFile: boolPtr(true),
			QualityProfileID: intPtr(1), Tags: &noTags,
			MovieFile: &movieFileElement{ID: intPtr(1), QualityCutoffNotMet: boolPtr(false)}},
		unmonitoredBelowCutoffMovie(2, "Never Reached"),
	}

	runRadarrDecisionEngine(ctx, logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude",
		fullLibraryScope(slog.LevelInfo), true, reverseScanOn(), fileReportOptions{})

	out := buf.String()
	if !strings.Contains(out, "abandoning this instance's reverse scan mid-evaluation") {
		t.Errorf("an interrupted reverse scan must say so:\n%s", out)
	}
	if !strings.Contains(out, "reverseScan=skipped") {
		t.Errorf("an interrupted reverse scan must not report a finding count:\n%s", out)
	}
	if strings.Contains(out, "reverseFindings=") {
		t.Errorf("a partial reverse scan must never print a count that reads as the whole library:\n%s", out)
	}
	// REVIEW FIX (Phase 10 round 5, binding controller ruling R4): the one
	// number the abandonment line does carry is how far it got, and it used to
	// be findings+noFile — neither of which this movie produced, so a pass that
	// really did evaluate a movie reported evaluated=0. On a real library that is
	// hundreds of movies reported as none, on the single line explaining what
	// happened to the pass.
	if !strings.Contains(out, "evaluated=1") || !strings.Contains(out, "libraryTotal=2") {
		t.Errorf("the abandonment line must say how far the pass got, counting every movie it evaluated whatever the outcome:\n%s", out)
	}
}

// TestRunRadarrDecisionEngine_ReverseFindings_NeverEnterTheForwardCrossCheck
// pins the separation the two passes depend on. The cross-check samples the
// forward decisions and nothing else; a reverse finding that leaked into its
// pools would be sampled as though it were a would-unmonitor decision, and — via
// writeGateBlockReason, which reads exactly those counts — could authorize or
// block FORWARD writes on the strength of an item the forward pass never
// considered.
func TestRunRadarrDecisionEngine_ReverseFindings_NeverEnterTheForwardCrossCheck(t *testing.T) {
	fake := newRadarrFake(t, "", nil)
	fake.reverseWantedJSON = `{"page":1,"pageSize":100,"totalRecords":2,"records":[{"id":7,"title":"A"},{"id":8,"title":"B"}]}`

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{unmonitoredBelowCutoffMovie(7, "A"), unmonitoredBelowCutoffMovie(8, "B")}

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude",
		fullLibraryScope(slog.LevelInfo), true, reverseScanOn(), fileReportOptions{})

	out := buf.String()
	if !strings.Contains(out, "reverseFindings=2") {
		t.Fatalf("this test proves nothing unless both findings were made:\n%s", out)
	}
	if strings.Contains(out, "msg=cross-check ") {
		t.Errorf("no reverse finding may be sampled by the forward cross-check:\n%s", out)
	}
	if !strings.Contains(out, `crossCheck="passed (nothing sampled`) {
		t.Errorf("the forward cross-check had nothing to sample this cycle and must say so:\n%s", out)
	}
}

// TestRun_ReverseScan_Sonarr_WriteMode_SeasonWhoseEpisodesAreAllMonitored_IsRefused
// is the other half of the clean-season mandate (binding controller ruling R2),
// and the shape that makes it a rule about STATE rather than about one reason
// string: this season is a plain custom-format finding — it really does fail the
// criteria — and it is still not written, because every episode inside it is
// already monitored.
//
// That shape is the reverse mirror of the FORWARD path's recovery case, and the
// mirror does not hold. Going forward, "every episode is already unmonitored"
// means a previous cycle of OURS was interrupted, and the season flag is the
// only thing left of a write this project decided on. Coming back, the identical
// shape is produced just as easily by a human monitoring the episodes of a
// season they left unmonitored, and nothing in the API distinguishes the two —
// so the flag is not finished on a guess about which one happened.
func TestRun_ReverseScan_Sonarr_WriteMode_SeasonWhoseEpisodesAreAllMonitored_IsRefused(t *testing.T) {
	// A CF-score finding: the episode is MONITORED (so it is absent from the
	// unmonitored wanted set and its quality cutoff counts as met) while the
	// SEASON is not, and its file scores below the profile's cutoff of 100.
	witnessSeries, witnessEpisodes, witnessFiles := crossCheckWitnessSonarrFixtures()
	fake := newStatefulSonarrFake(t,
		append(witnessSeries,
			&statefulSonarrSeries{id: 1, title: "Season Flag Only", monitored: true, profileID: 1, tags: []int{},
				seasons: []statefulSonarrSeason{{number: 2, monitored: false, episodeFileCount: 1, totalEpisodeCount: 1}}}),
		append(witnessEpisodes,
			&statefulSonarrEpisode{id: 200, seriesID: 1, seasonNumber: 2, episodeNumber: 1, monitored: true, hasFile: true,
				airDateUtc: pastAirDate, episodeFileID: 600, inWantedSet: false}),
		append(witnessFiles, &statefulSonarrEpisodeFile{id: 600, seasonNumber: 2, customFormatScore: 10, qualityCutoffNotMet: false}),
	)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--config", writeReverseSonarrTestConfig(t, fake.srv.URL, false, true), "--once"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code; stderr=%s", stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, `reason="`+ReasonCFCutoffNotMet+`"`) {
		t.Fatalf("this test proves nothing unless the CF-score finding was made:\n%s", out)
	}
	if !strings.Contains(out, `crossCheck="passed (1 verified`) {
		t.Fatalf("nor unless the write gate really opened, leaving the mandate as the only explanation:\n%s", out)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("a season whose episodes are already monitored must not be written: %+v", writes)
	}
	if fake.seasonMonitored(1, 2) {
		t.Error("season 2 must be left exactly as it was found")
	}
	if !strings.Contains(out, "already has monitored episodes inside it") {
		t.Errorf("the refusal must say what stopped it:\n%s", out)
	}
	c := summaryCountersFor(t, out, "sonarr decision summary")
	if c["reverseFindings"] != 1 || c["remonitorsRefused"] != 1 || c["remonitored"] != 0 {
		t.Errorf("want reverseFindings=1 remonitorsRefused=1 remonitored=0, got %v:\n%s", c, out)
	}
	if c["recoveredWrites"] != 0 || strings.Contains(out, "completing a previously partial season unmonitor") {
		t.Errorf("nothing here is a forward recovery:\n%s", out)
	}
	assertReverseIdentity(t, out)
}

// TestRunSonarrDecisionEngine_ReverseScan_BelowCutoffSeasonWithOneMonitoredEpisode_IsRefused
// is the widest case the clean-season mandate covers, and the one that could not
// be reached through the mismatch reason at all: this season is genuinely below
// its quality cutoff (it is in the unmonitored wanted set, so rule 4 fires and
// rule 7 never runs), which is the plan's own "an unmonitored item that FAILS
// criteria" — and one episode inside it is monitored anyway.
//
// Writing it would re-monitor the season and every other episode of it. The one
// somebody monitored by hand tells this pass that the season's state is somebody
// else's business; a finding that says "and it is also below cutoff" does not
// make that any less true.
func TestRunSonarrDecisionEngine_ReverseScan_BelowCutoffSeasonWithOneMonitoredEpisode_IsRefused(t *testing.T) {
	// Both of season 2's files score 200 against the profile's 100, so the CF
	// rule cannot be what reports this season: the unmonitored wanted set is.
	episodesJSON := "[" + episodeJSON(900, 1, 1, pastAirDate, 9000) + "," +
		episodeJSONWithMonitored(200, 2, 1, pastAirDate, 600, true) + "," +
		episodeJSONWithMonitored(201, 2, 2, pastAirDate, 601, false) + "]"
	filesJSON := "[" + episodeFileJSON(9000, 1, 200, true) + "," +
		episodeFileJSON(600, 2, 200, true) + "," + episodeFileJSON(601, 2, 200, true) + "]"
	series := []seriesElement{
		testSeries(9, "Ordinary Monitored Show", true, 1, []int{}, testSeason(1, true, 1, 1)),
		testSeries(1, "Below Cutoff, Hand Touched", true, 1, []int{}, testSeason(2, false, 2, 2)),
	}
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	fake.seriesDetail[1] = sonarrWriteEngineSeriesDetail(1, "Below Cutoff, Hand Touched", 2, 2, false)
	fake.reverseWantedJSON = `{"page":1,"pageSize":100,"totalRecords":1,"records":[{"id":201,"seriesId":1,"seasonNumber":2}]}`

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series,
		map[int]bool{900: true}, map[seasonKey]bool{{seriesID: 9, seasonNumber: 1}: true},
		"cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), false, reverseOptions{enabled: true, remonitor: true}, fileReportOptions{})

	out := buf.String()
	line := reverseFindingLine(t, out)
	if !strings.Contains(line, `reason="`+ReasonQualityCutoffNotMet+`"`) {
		t.Fatalf("this test is only about the widest case if the season really was reported as below its cutoff:\n%s", line)
	}
	if !strings.Contains(out, `crossCheck="passed (1 verified`) {
		t.Fatalf("nor unless the write gate really opened:\n%s", out)
	}
	if n := fake.countRequests("/api/v3/series/1"); n == 0 {
		t.Fatalf("the write path was never entered, so this test says nothing about its predicate:\n%s", out)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("a season with a hand-monitored episode inside it is reported, not written: %+v", writes)
	}
	if !strings.Contains(out, "already has monitored episodes inside it") {
		t.Errorf("the refusal must say what stopped it:\n%s", out)
	}
	c := summaryCountersFor(t, out, "sonarr decision summary")
	if c["reverseFindings"] != 1 || c["remonitorsRefused"] != 1 || c["remonitored"] != 0 {
		t.Errorf("want reverseFindings=1 remonitorsRefused=1 remonitored=0, got %v:\n%s", c, out)
	}
	assertReverseIdentity(t, out)
}

// --- round 5: what the shared write path SAYS, and what its numbers mean -----
//
// Everything below this line is about the same class of defect, found once the
// reverse direction had been threaded through the shared Sonarr write path: a
// message or a number written when only one direction existed, still stating
// that direction's fact on a cycle that did the opposite. None of them changes
// what is written; every one of them changes what a human reading the log
// believes happened, which is the only evidence anybody has that this daemon is
// safe.

// reverseSeasonWriteEngineFake is the smallest engine fixture a Sonarr reverse
// WRITE test needs: the series under test with one unmonitored, qualifying
// season, the cross-check witness that opens the gate, and the pre-write series
// payload the write path re-fetches.
func reverseSeasonWriteEngineFake(t *testing.T) (*sonarrEngineFake, []seriesElement, map[int]bool, map[seasonKey]bool) {
	t.Helper()
	episodesJSON, filesJSON, series, wantedEpisodes, wantedSeasons := sonarrReverseWriteFixtures(false, 200, true)
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	fake.reverseWantedJSON = `{"page":1,"pageSize":100,"totalRecords":1,"records":[{"id":200,"seriesId":1,"seasonNumber":2}]}`
	fake.seriesDetail[1] = sonarrWriteEngineSeriesDetail(1, "Reverse Write Candidate", 2, 1, false)
	return fake, series, wantedEpisodes, wantedSeasons
}

// TestSeasonEpisodeMonitorWrite_TransportFailure_NamesTheDirectionItAttempted
// is the episode half's transport error, which wrapped its cause with the word
// "unmonitoring" in BOTH directions (REVIEW FIX, Phase 10 round 5).
//
// That wrap is not an internal detail: it is surfaced verbatim by
// reverseCounts.record's ERROR line, so a failed re-monitor told a human
// `msg="remonitor write failed" error="...unmonitoring the season's episodes..."`
// — the opposite of what was attempted, on the one line they have to diagnose it
// from.
func TestSeasonEpisodeMonitorWrite_TransportFailure_NamesTheDirectionItAttempted(t *testing.T) {
	t.Run("forward", func(t *testing.T) {
		fake := newSonarrWriterFake(t, sonarrWriterSeriesJSON, sonarrWriterEpisodesJSON)
		fake.episodeMonitorStatus = http.StatusInternalServerError
		logger, _ := newDecisionTestLogger(slog.LevelInfo)

		written, _, err := unmonitorSeason(context.Background(), logger, fake.client(), fake.instance(), 3, 1, 0, false, false, false)
		if written || err == nil {
			t.Fatalf("written = %t, err = %v; want no write and an error", written, err)
		}
		if !strings.Contains(err.Error(), "unmonitoring the season's episodes") {
			t.Errorf("the forward direction must still name its own verb: %v", err)
		}
	})

	t.Run("reverse", func(t *testing.T) {
		fake, series, wantedEpisodes, wantedSeasons := reverseSeasonWriteEngineFake(t)
		fake.episodeMonitorStatus = http.StatusInternalServerError
		logger, buf := newDecisionTestLogger(slog.LevelInfo)

		runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, wantedEpisodes, wantedSeasons,
			"cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), false, reverseOptions{enabled: true, remonitor: true}, fileReportOptions{})

		out := buf.String()
		if !strings.Contains(out, `msg="remonitor write failed`) {
			t.Fatalf("this test proves nothing unless the re-monitor really failed:\n%s", out)
		}
		if strings.Contains(out, "unmonitoring the season's episodes") {
			t.Errorf("a failed RE-monitor must never report itself as an unmonitor:\n%s", out)
		}
		if !strings.Contains(out, "re-monitoring the season's episodes") {
			t.Errorf("the error must name the direction it attempted:\n%s", out)
		}
		c := summaryCountersFor(t, out, "sonarr decision summary")
		if c["reverseWriteErrors"] != 1 {
			t.Errorf("reverseWriteErrors = %d, want 1:\n%s", c["reverseWriteErrors"], out)
		}
		assertReverseIdentity(t, out)
	})
}

// TestRunSonarrDecisionEngine_ReverseScan_UnreadableEpisodeAtWrite_GivesTheReverseRationale
// is the same class on the two episode-loop refusals. The refusals themselves
// are right in both directions — an episode this write cannot name, or whose
// state it cannot read, is untrusted input on a load-bearing field — but their
// stated RATIONALE is the forward one: "unmonitoring the season would strand
// it", meaning rule 1 would hide the episode from every future cycle. Coming
// back, nothing is stranded by anything, and the true reason is the opposite
// one: the episode would be left UNMONITORED inside a season this write is
// making monitored, so the write cannot do what it says it does.
func TestRunSonarrDecisionEngine_ReverseScan_UnreadableEpisodeAtWrite_GivesTheReverseRationale(t *testing.T) {
	cases := []struct {
		name          string
		seasonEpisode string
		wantInWarn    string
	}{
		{
			name:          "the episode cannot be named",
			seasonEpisode: `{"seasonNumber":2,"episodeNumber":1,"monitored":false,"hasFile":true,"airDateUtc":"2015-01-01T00:00:00Z","episodeFileId":600}`,
			wantInWarn:    "could not be named in the episode monitor write",
		},
		{
			name:          "the episode's own monitored value cannot be read",
			seasonEpisode: `{"id":200,"seasonNumber":2,"episodeNumber":1,"monitored":null,"hasFile":true,"airDateUtc":"2015-01-01T00:00:00Z","episodeFileId":600}`,
			wantInWarn:    "unknown state",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake, series, wantedEpisodes, wantedSeasons := reverseSeasonWriteEngineFake(t)
			// The world as the WRITE path finds it: the scan saw a clean season
			// (that is what made the finding), and the pre-write episode re-read
			// is where the unreadable field appears.
			fake.writeTimeEpisodeJSON = "[" + episodeJSON(900, 1, 1, pastAirDate, 9000) + "," + tc.seasonEpisode + "]"
			logger, buf := newDecisionTestLogger(slog.LevelInfo)

			runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, wantedEpisodes, wantedSeasons,
				"cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), false, reverseOptions{enabled: true, remonitor: true}, fileReportOptions{})

			out := buf.String()
			if writes := fake.writes(); len(writes) != 0 {
				t.Errorf("an unreadable episode must stop both halves of the write: %+v", writes)
			}
			if strings.Contains(out, "strand") {
				t.Errorf("a re-monitor strands nothing; the forward rationale must not be printed on a reverse write:\n%s", out)
			}
			if !strings.Contains(out, tc.wantInWarn) || !strings.Contains(out, "re-monitoring the season") {
				t.Errorf("the refusal must state the reverse direction's own rationale:\n%s", out)
			}
			c := summaryCountersFor(t, out, "sonarr decision summary")
			if c["reverseFindings"] != 1 || c["remonitorsRefused"] != 1 || c["remonitored"] != 0 {
				t.Errorf("want reverseFindings=1 remonitorsRefused=1 remonitored=0, got %v:\n%s", c, out)
			}
			assertReverseIdentity(t, out)
		})
	}
}

// TestRunSonarrDecisionEngine_ReverseScan_ShutdownMidEvaluation_CountsWhatItEvaluated
// is the Sonarr half of the abandonment line's own honesty (REVIEW FIX, Phase 10
// round 5, binding controller ruling R4), and the mirror of the forward pass's
// TestRunSonarrDecisionEngine_ShutdownMidEvaluation_CountsSeriesEvaluatedNotSeriesInScope.
//
// The line exists to say how far the abandoned pass got. It used to print
// findings=, which is the count of things that were WRONG — zero on a healthy
// library — so a reverse scan cut short after hundreds of series reported 0 on
// the one line explaining what happened to it.
func TestRunSonarrDecisionEngine_ReverseScan_ShutdownMidEvaluation_CountsWhatItEvaluated(t *testing.T) {
	// Two ordinary series, each with an unmonitored season that MEETS the
	// criteria: nothing here is a finding, which is the whole point — the count
	// must reflect work done, not findings made.
	fake := newStatefulSonarrFake(t,
		[]*statefulSonarrSeries{
			{id: 1, title: "First Show", monitored: true, profileID: 1, tags: []int{},
				seasons: []statefulSonarrSeason{{number: 1, monitored: false, episodeFileCount: 1, totalEpisodeCount: 1}}},
			{id: 2, title: "Second Show", monitored: true, profileID: 1, tags: []int{},
				seasons: []statefulSonarrSeason{{number: 2, monitored: false, episodeFileCount: 1, totalEpisodeCount: 1}}},
		},
		[]*statefulSonarrEpisode{
			{id: 100, seriesID: 1, seasonNumber: 1, episodeNumber: 1, monitored: false, hasFile: true,
				airDateUtc: pastAirDate, episodeFileID: 500, inWantedSet: false},
			{id: 200, seriesID: 2, seasonNumber: 2, episodeNumber: 1, monitored: false, hasFile: true,
				airDateUtc: pastAirDate, episodeFileID: 600, inWantedSet: false},
		},
		[]*statefulSonarrEpisodeFile{
			{id: 500, seasonNumber: 1, customFormatScore: 200, qualityCutoffNotMet: false},
			{id: 600, seasonNumber: 2, customFormatScore: 200, qualityCutoffNotMet: false},
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The forward pass's own wanted set is handed to the engine as an argument,
	// so the ONLY /wanted/cutoff request of this cycle is the reverse pass's:
	// it is what arms the cancellation, which then lands partway through the
	// first series of the REVERSE evaluation rather than the forward one.
	var armed atomic.Bool
	var once sync.Once
	fake.onRequest = func(method, path string) {
		switch {
		case path == "/api/v3/wanted/cutoff":
			armed.Store(true)
		case armed.Load() && path == "/api/v3/episode":
			once.Do(cancel)
		}
	}

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	series := []seriesElement{
		testSeries(1, "First Show", true, 1, []int{}, testSeason(1, false, 1, 1)),
		testSeries(2, "Second Show", true, 1, []int{}, testSeason(2, false, 1, 1)),
	}
	runSonarrDecisionEngine(ctx, logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{},
		"cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseScanOn(), fileReportOptions{})

	out := buf.String()
	if !strings.Contains(out, "abandoning this instance's reverse scan mid-evaluation") {
		t.Fatalf("this test proves nothing unless the reverse scan really was interrupted:\n%s", out)
	}
	if strings.Contains(out, "findings=0") {
		t.Errorf("the abandonment line must not report a finding count at all — it is a number this cycle is in no position to state:\n%s", out)
	}
	if !strings.Contains(out, "seriesEvaluated=1") || !strings.Contains(out, "libraryTotal=2") {
		t.Errorf("the line must say how far the abandoned pass got, in the same words the forward twin uses:\n%s", out)
	}
}

// TestReverseScan_UntrustedPreWriteData_IsNeverReportedAsNowMeetingTheCriteria
// is R5's second half. The pre-write re-verification refuses on EVERY reason
// that is not a finding, which is correct — but it announced all of them at INFO
// as "no longer fails the criteria", including the ones that mean "this could
// not be checked at all" (an unknown profile, unreadable tags, a file score that
// could not be fetched). That is the never-observed-claim class this project has
// fixed repeatedly: "we could not read it" is not evidence that it now passes,
// and the reason attr on the same line contradicted the message.
func TestReverseScan_UntrustedPreWriteData_IsNeverReportedAsNowMeetingTheCriteria(t *testing.T) {
	t.Run("radarr", func(t *testing.T) {
		// The pre-write fetch names a quality profile this instance does not
		// have, so rule 3 cannot evaluate the movie at all.
		detail := map[int]string{7: `{"id":7,"title":"Accidentally Unmonitored","monitored":false,"hasFile":true,"qualityProfileId":99,"tags":[]}`}
		fake := newRadarrFake(t, "", detail)
		fake.reverseWantedJSON = `{"page":1,"pageSize":100,"totalRecords":1,"records":[{"id":7,"title":"Accidentally Unmonitored"}]}`

		logger, buf := newDecisionTestLogger(slog.LevelInfo)
		movies := []movieListElement{crossCheckWitnessMovie(5, "Ordinary Monitored"), unmonitoredBelowCutoffMovie(7, "Accidentally Unmonitored")}
		runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{5: true},
			"cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), false, reverseOptions{enabled: true, remonitor: true}, fileReportOptions{})

		out := buf.String()
		if !strings.Contains(out, `crossCheck="passed (1 verified`) {
			t.Fatalf("this test proves nothing unless the write gate really opened:\n%s", out)
		}
		if puts := fake.puts(); len(puts) != 0 {
			t.Errorf("untrusted input must never be written through: %+v", puts)
		}
		assertUntrustedReVerification(t, out, ReasonUnknownProfile)
		c := summaryCounters(t, out)
		if c["reverseFindings"] != 1 || c["remonitorsRefused"] != 1 || c["remonitored"] != 0 {
			t.Errorf("want reverseFindings=1 remonitorsRefused=1 remonitored=0, got %v:\n%s", c, out)
		}
		assertReverseIdentity(t, out)
	})

	t.Run("sonarr", func(t *testing.T) {
		fake, series, wantedEpisodes, wantedSeasons := reverseSeasonWriteEngineFake(t)
		fake.seriesDetail[1] = `{"id":1,"title":"Reverse Write Candidate","monitored":true,"qualityProfileId":99,"tags":[],` +
			`"seasons":[{"seasonNumber":2,"monitored":false,"statistics":{"episodeFileCount":1,"totalEpisodeCount":1}}]}`

		logger, buf := newDecisionTestLogger(slog.LevelInfo)
		runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, wantedEpisodes, wantedSeasons,
			"cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), false, reverseOptions{enabled: true, remonitor: true}, fileReportOptions{})

		out := buf.String()
		if !strings.Contains(out, `crossCheck="passed (1 verified`) {
			t.Fatalf("this test proves nothing unless the write gate really opened:\n%s", out)
		}
		if writes := fake.writes(); len(writes) != 0 {
			t.Errorf("untrusted input must never be written through: %+v", writes)
		}
		assertUntrustedReVerification(t, out, ReasonUnknownProfile)
		c := summaryCountersFor(t, out, "sonarr decision summary")
		if c["reverseFindings"] != 1 || c["remonitorsRefused"] != 1 || c["remonitored"] != 0 {
			t.Errorf("want reverseFindings=1 remonitorsRefused=1 remonitored=0, got %v:\n%s", c, out)
		}
		assertReverseIdentity(t, out)
	})
}

// assertUntrustedReVerification is the shared shape of both halves above: the
// refusal is a WARNING that names the untrusted reason and does not claim the
// item now meets the criteria.
func assertUntrustedReVerification(t *testing.T, out, reason string) {
	t.Helper()
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "could not be re-established from the pre-write fetch") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatalf("the refusal must say the finding could not be re-established:\n%s", out)
	}
	if !strings.Contains(line, "level=WARN") {
		t.Errorf("an untrusted-input refusal is a warning, not an informational note:\n%s", line)
	}
	if !strings.Contains(line, `reason="`+reason+`"`) {
		t.Errorf("the refusal must name the untrusted reason:\n%s", line)
	}
	if strings.Contains(out, "no longer fails the criteria") {
		t.Errorf("nothing established that this item now meets the criteria; the log must not say it did:\n%s", out)
	}
}

// TestReverseFindingReasons_UntrustedInputIsClassifiedExhaustively is the
// census guard for the classification the two tests above rest on. Same
// rationale as isReverseFinding's own audit: a reason nobody has written yet
// must not silently default into "this is a real observation about the item",
// which is what would let a future "we could not read X" reason be announced as
// "it now meets the criteria".
func TestReverseFindingReasons_UntrustedInputIsClassifiedExhaustively(t *testing.T) {
	untrusted := []string{
		ReasonUnknownProfile, ReasonTagsUnknown, ReasonCouldNotFetchCFScore,
		ReasonSeasonEpisodesUnavailable, ReasonSeasonEpisodeDataInconsistent, ReasonSeasonFileCountMismatch,
	}
	trusted := []string{
		ReasonCutoffMet, ReasonNoFile, ReasonExcludedByTag, ReasonUpgradesDisabled,
		ReasonSeasonIncomplete, ReasonSeasonNotFullyAired,
		ReasonQualityCutoffNotMet, ReasonCFCutoffNotMet, ReasonSeasonMonitorMismatch, "",
	}
	for _, r := range untrusted {
		if !isUntrustedInputReason(r) {
			t.Errorf("%q means the item could not be checked, and must never be reported as an observation about it", r)
		}
	}
	for _, r := range trusted {
		if isUntrustedInputReason(r) {
			t.Errorf("%q is a real observation about the item, not untrusted input", r)
		}
	}
	// -1 for the empty string, which is not a declared constant but is the
	// value a decision carries before any rule has spoken.
	if named, declared := len(untrusted)+len(trusted)-1, len(declaredReasonConstants(t)); named != declared {
		t.Errorf("this census names %d reason constants; decision.go declares %d, and every one of them must be classified as trusted or not",
			named, declared)
	}
}

// TestRunSonarrDecisionEngine_ReverseScan_SeasonMonitoredBeforeTheWrite_IsRefused
// is the reverse direction's scan-to-write race on the SONARR path: something
// else — a human in the UI, another tool, a list sync — re-monitored the season
// between the scan that found it and the write pass that would have.
//
// The Radarr twin has been pinned since this branch opened
// (TestRun_ReverseScan_WriteMode_AlreadyMonitoredAtWrite_IsRefused); this guard
// had nothing but the happy path passing THROUGH it, so deleting it left the
// suite green. It stayed green because a later guard catches the same world in a
// different way — the pre-write re-verification finds no unmonitored season and
// refuses for want of a decision — which is why this test asserts WHICH guard
// spoke. A refusal is not the property; refusing for the reason that is actually
// true is.
func TestRunSonarrDecisionEngine_ReverseScan_SeasonMonitoredBeforeTheWrite_IsRefused(t *testing.T) {
	fake, series, wantedEpisodes, wantedSeasons := reverseSeasonWriteEngineFake(t)
	// The pre-write fetch, and the only thing that differs from the scan: the
	// season is monitored again.
	fake.seriesDetail[1] = sonarrWriteEngineSeriesDetail(1, "Reverse Write Candidate", 2, 1, true)

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, wantedEpisodes, wantedSeasons,
		"cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), false, reverseOptions{enabled: true, remonitor: true}, fileReportOptions{})

	out := buf.String()
	if !strings.Contains(out, `crossCheck="passed (1 verified`) {
		t.Fatalf("this test proves nothing unless the write gate really opened:\n%s", out)
	}
	if n := fake.countRequests("/api/v3/series/1"); n == 0 {
		t.Fatalf("the write path was never entered, so it says nothing about this guard:\n%s", out)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("there is nothing to re-monitor, so nothing may be written: %+v", writes)
	}
	if !strings.Contains(out, "season already monitored as of the pre-write fetch") {
		t.Errorf("the refusal must be the race's own, naming what the fresh fetch found:\n%s", out)
	}
	c := summaryCountersFor(t, out, "sonarr decision summary")
	if c["reverseFindings"] != 1 || c["remonitorsRefused"] != 1 || c["remonitored"] != 0 {
		t.Errorf("want reverseFindings=1 remonitorsRefused=1 remonitored=0, got %v:\n%s", c, out)
	}
	assertReverseIdentity(t, out)
}

// TestRunSonarrDecisionEngine_ReverseScan_SeasonNarrowedScope_ReportsOnlyThatSeason
// pins the scope's SECOND level in the reverse direction. An evalScope can name
// seasons under a series id, and the reverse pass applies that narrowing exactly
// as the forward one does.
//
// No cycle this daemon builds today combines the two — season narrowing comes
// only from webhook events, and webhook cycles run no reverse pass — so this is
// deliberately constructed at the engine boundary rather than through run(). The
// alternative was leaving the narrowing unapplied on the reverse side, which
// would mean a scope that says "season 2 of series 1" quietly meaning "every
// season of series 1" the first time the two are combined. A narrowing that
// exists must mean the same thing in both directions.
func TestRunSonarrDecisionEngine_ReverseScan_SeasonNarrowedScope_ReportsOnlyThatSeason(t *testing.T) {
	episodesJSON := "[" + episodeJSONWithMonitored(200, 2, 1, pastAirDate, 600, false) + "," +
		episodeJSONWithMonitored(300, 3, 1, pastAirDate, 700, false) + "]"
	filesJSON := "[" + episodeFileJSON(600, 2, 200, true) + "," + episodeFileJSON(700, 3, 200, true) + "]"
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	fake.reverseWantedJSON = `{"page":1,"pageSize":100,"totalRecords":2,"records":[` +
		`{"id":200,"seriesId":1,"seasonNumber":2},{"id":300,"seriesId":1,"seasonNumber":3}]}`

	logger, buf := newDecisionTestLogger(slog.LevelDebug)
	series := []seriesElement{
		testSeries(1, "Two Unmonitored Seasons", true, 1, []int{}, testSeason(2, false, 1, 1), testSeason(3, false, 1, 1)),
	}
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{},
		"cutoffarr-exclude", webhookScope([]int{1}, map[int][]int{1: {2}}), true, reverseScanOn(), fileReportOptions{})

	out := buf.String()
	line := reverseFindingLine(t, out)
	if !strings.Contains(line, "season=2") {
		t.Errorf("the reported finding must be the season the scope names:\n%s", line)
	}
	if strings.Contains(out, "season=3") {
		t.Errorf("a season the scope excludes must not be reported at all:\n%s", out)
	}
	if !strings.Contains(out, "reverseFindings=1") {
		t.Errorf("and the count must agree with what was reported:\n%s", out)
	}
}
