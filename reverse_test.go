package main

import (
	"context"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"
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
