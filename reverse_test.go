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

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseScanOn())

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

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseScanOn())

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

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseScanOn())

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

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseScanOn())

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

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseScanOn())

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

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{7: true}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseScanOn())

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

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseScanOn())

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

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseOptions{})

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

	runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelDebug), true, reverseScanOn())

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
	findings := []string{ReasonQualityCutoffNotMet, ReasonCFCutoffNotMet}
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

	src, err := os.ReadFile("decision.go")
	if err != nil {
		t.Fatalf("reading decision.go: %v", err)
	}
	declared := regexp.MustCompile(`(?m)^\t(Reason[A-Za-z]+)\s+=\s+"`).FindAllStringSubmatch(string(src), -1)
	if len(declared) == 0 {
		t.Fatal("found no Reason* constants in decision.go; this audit's pattern has stopped matching the source it audits")
	}
	for _, m := range declared {
		if _, named := census[m[1]]; !named {
			t.Errorf("%s is not classified by this test: decide whether the reverse scan should report it as a finding, then add it to the census and to isReverseFinding", m[1])
		}
	}
	if len(declared) != len(census) {
		t.Errorf("decision.go declares %d Reason* constants but the census names %d; the census must be exhaustive", len(declared), len(census))
	}
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

	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseScanOn())

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

	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseScanOn())

	out := buf.String()
	line := reverseFindingLine(t, out)
	if !strings.Contains(line, "seriesMonitored=false") {
		t.Errorf("a finding under an unmonitored series must say so:\n%s", line)
	}
	if c := summaryCountersFor(t, out, "sonarr decision summary"); c["reverseFindings"] != 1 {
		t.Errorf("reverseFindings = %d, want 1:\n%s", c["reverseFindings"], out)
	}
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

	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{100: true}, wanted, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseScanOn())

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

	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseScanOn())

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

// TestRunSonarrDecisionEngine_ReverseScanDisabled_MakesNoUnmonitoredWantedFetch
// is the Sonarr twin of the Radarr scheduling pin.
func TestRunSonarrDecisionEngine_ReverseScanDisabled_MakesNoUnmonitoredWantedFetch(t *testing.T) {
	episodesJSON := "[" + episodeJSON(200, 2, 1, pastAirDate, 600) + "]"
	filesJSON := "[" + episodeFileJSON(600, 2, 200, true) + "]"
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	series := []seriesElement{testSeries(1, "Some Show", true, 1, []int{}, testSeason(2, false, 1, 1))}

	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseOptions{})

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
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{reverseFindingStatefulMovie(7, "Accidentally Unmonitored")})

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", writeReverseTestConfig(t, fake.srv.URL, false, false), "--once"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, `msg="reverse-scan finding"`) {
		t.Fatalf("this test proves nothing unless the finding really was made:\n%s", out)
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
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{reverseFindingStatefulMovie(7, "Accidentally Unmonitored")})

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
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{reverseFindingStatefulMovie(7, "Accidentally Unmonitored")})
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
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{reverseFindingStatefulMovie(7, "Excluded Finding")})
	fake.movie(7).tags = []int{42}
	fake.tagsJSON = `[{"id": 42, "label": "cutoffarr-exclude"}]`

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--config", writeReverseTestConfig(t, fake.srv.URL, false, true), "--once"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code; stderr=%s", stderr.String())
	}

	out := stdout.String()
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
		fullLibraryScope(slog.LevelInfo), false, reverseOptions{enabled: true, remonitor: true})

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
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{{
		id: 7, title: "Upgraded Meanwhile", monitored: false, hasFile: true, qualityProfileID: 1,
		tags: []int{}, movieFileID: 7, cfScore: 10, qualityCutoffNotMet: false, inWantedSet: false,
	}})
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
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{reverseFindingStatefulMovie(7, "Someone Beat Us To It")})
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
func assertReverseIdentity(t *testing.T, out string) {
	t.Helper()
	for _, msg := range []string{"radarr decision summary", "sonarr decision summary"} {
		if !strings.Contains(out, msg) {
			continue
		}
		c := summaryCountersFor(t, out, msg)
		if _, present := c["reverseFindings"]; !present {
			continue
		}
		accounted := c["remonitored"] + c["remonitorsRefused"] + c["reverseWithheld"] +
			c["reverseWriteErrors"] + c["reverseRehearsalErrors"] + c["reverseEchoUnverified"]
		if accounted != c["reverseFindings"] {
			t.Errorf("%s accounts for %d of %d findings; every finding must end in exactly one counted outcome:\n%s", msg, accounted, c["reverseFindings"], out)
		}
		for _, always := range []string{"remonitored=", "remonitorsRefused=", "reverseWithheld="} {
			if !strings.Contains(out, always) {
				t.Errorf("with the write switch on the summary must always carry %s, including as 0:\n%s", always, out)
			}
		}
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
	return newStatefulSonarrFake(t,
		[]*statefulSonarrSeries{
			{id: 1, title: "Accidentally Unmonitored", monitored: seriesMonitored, profileID: 1, tags: []int{},
				seasons: []statefulSonarrSeason{{number: 2, monitored: false, episodeFileCount: 1, totalEpisodeCount: 1}}},
		},
		[]*statefulSonarrEpisode{
			{id: 200, seriesID: 1, seasonNumber: 2, episodeNumber: 1, monitored: false, hasFile: true,
				airDateUtc: pastAirDate, episodeFileID: 600, inWantedSet: true},
		},
		[]*statefulSonarrEpisodeFile{{id: 600, seasonNumber: 2, customFormatScore: 200, qualityCutoffNotMet: true}},
	)
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
// half-completed re-monitor leaves the season unmonitored and therefore still a
// finding next cycle. That is the whole of why the reverse direction needs no
// recovery path.
func TestRun_ReverseScan_Sonarr_WriteMode_RemonitorsTheSeasonEpisodesFirst(t *testing.T) {
	fake := reverseFindingSonarrFake(t, true)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--config", writeReverseSonarrTestConfig(t, fake.srv.URL, false, true), "--once"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code; stderr=%s", stderr.String())
	}

	out := stdout.String()
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

	mark := h.mark()
	h.clock.Advance(time.Hour)
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
	time.Sleep(20 * time.Millisecond)
	h.stop()

	cycle := h.since(mark)
	if !strings.Contains(cycle, "radarr decision summary") {
		t.Fatalf("the webhook cycle must have run at all:\n%s", cycle)
	}
	assertNoReversePass(t, cycle)
}

// TestRun_OnlyID_RunsNoReversePass is the same rule for the scoped one-shot run.
func TestRun_OnlyID_RunsNoReversePass(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{
		wouldUnmonitorStatefulMovie(1, "Named"),
		reverseFindingStatefulMovie(7, "Accidentally Unmonitored"),
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", writeReverseTestConfig(t, fake.srv.URL, true, true), "--once", "--only-id", "1"}, &stdout, &stderr)
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

	mark := h.mark()
	h.clock.Advance(time.Hour)
	h.awaitLogCount("reconciliation sweep complete", 1)
	cycle2 := h.since(mark)

	mark3 := h.mark()
	h.clock.Advance(time.Hour)
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
			movies := []movieListElement{unmonitoredBelowCutoffMovie(findingID, "Accounted Movie")}
			runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude",
				fullLibraryScope(slog.LevelInfo), tc.dryRun, reverseOptions{enabled: true, remonitor: true})

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
		fullLibraryScope(slog.LevelInfo), false, reverseOptions{enabled: true, remonitor: true})

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
		fullLibraryScope(slog.LevelInfo), true, reverseScanOn())

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
		fullLibraryScope(slog.LevelInfo), true, reverseScanOn())

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

// TestRun_ReverseScan_Sonarr_WriteMode_SeasonFlagOnly_NeedsNoEpisodeWrite is the
// shape where only the season flag is wrong: every episode of the season is
// already monitored, so there is nothing for the episode call to name and one
// write is enough.
//
// It is worth its own test because it is the reverse mirror of the FORWARD
// path's recovery shape, and must not be reported as one: recovery means
// "finishing something a previous cycle left half-done", which is a claim about
// the forward direction only. Here it is simply an ordinary re-monitor of an
// ordinary season.
func TestRun_ReverseScan_Sonarr_WriteMode_SeasonFlagOnly_NeedsNoEpisodeWrite(t *testing.T) {
	// A CF-score finding: the episode is MONITORED (so it is absent from the
	// unmonitored wanted set and its quality cutoff counts as met) while the
	// SEASON is not, and its file scores below the profile's cutoff of 100.
	fake := newStatefulSonarrFake(t,
		[]*statefulSonarrSeries{
			{id: 1, title: "Season Flag Only", monitored: true, profileID: 1, tags: []int{},
				seasons: []statefulSonarrSeason{{number: 2, monitored: false, episodeFileCount: 1, totalEpisodeCount: 1}}},
		},
		[]*statefulSonarrEpisode{
			{id: 200, seriesID: 1, seasonNumber: 2, episodeNumber: 1, monitored: true, hasFile: true,
				airDateUtc: pastAirDate, episodeFileID: 600, inWantedSet: false},
		},
		[]*statefulSonarrEpisodeFile{{id: 600, seasonNumber: 2, customFormatScore: 10, qualityCutoffNotMet: false}},
	)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--config", writeReverseSonarrTestConfig(t, fake.srv.URL, false, true), "--once"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code; stderr=%s", stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, `reason="`+ReasonCFCutoffNotMet+`"`) {
		t.Fatalf("this test proves nothing unless the CF-score finding was made:\n%s", out)
	}
	writes := fake.writes()
	if len(writes) != 1 || writes[0].path != "/api/v3/series/1" {
		t.Fatalf("want exactly one write, the season PUT, got %+v", writes)
	}
	if !fake.seasonMonitored(1, 2) {
		t.Error("season 2 must be monitored after the write")
	}
	c := summaryCountersFor(t, out, "sonarr decision summary")
	if c["remonitored"] != 1 {
		t.Errorf("remonitored = %d, want 1:\n%s", c["remonitored"], out)
	}
	if c["recoveredWrites"] != 0 || strings.Contains(out, "completing a previously partial season unmonitor") {
		t.Errorf("a reverse write must never be reported as a forward recovery:\n%s", out)
	}
	assertReverseIdentity(t, out)
}
