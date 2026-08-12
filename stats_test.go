package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

// stats_test.go covers Phase 12's in-memory capture: the statsStore itself
// (recordInstance/snapshot semantics — three-state fidelity, isolation,
// lastActions capping and ordering), the pure conversion helpers
// (fileReportSnapshotFrom, radarrReverseFindings, sonarrReverseFindings), and
// — the part that actually proves the plumbing wired into decision.go,
// reverse.go, filereport.go and the two write passes works — the decision
// engines' new cycleInstanceStats return value, exercised end to end against
// the SAME fakes the rest of the suite already trusts.

// --- statsStore --------------------------------------------------------

// TestStatsStore_SnapshotBeforeAnyCycle_IsEmptyInstancesNotAnError pins the
// API contract's own words: "before the first cycle completes: valid JSON
// with empty instances state, not an error".
func TestStatsStore_SnapshotBeforeAnyCycle_IsEmptyInstancesNotAnError(t *testing.T) {
	s := newStatsStore(true)
	snap := s.snapshot()
	if snap.Instances == nil {
		t.Fatal("Instances is nil; want a non-nil empty slice so it encodes as [] rather than null")
	}
	if len(snap.Instances) != 0 {
		t.Errorf("Instances = %+v, want empty before any cycle has completed", snap.Instances)
	}
	if !snap.DryRun {
		t.Error("DryRun must reflect the value the store was created with")
	}
	if snap.Version == "" {
		t.Error("Version must never be empty")
	}
}

// TestStatsStore_RecordInstance_CapturesFieldsAndPreservesConfigOrder pins
// the basic capture and the insertion-order guarantee snapshot's own doc
// comment promises.
func TestStatsStore_RecordInstance_CapturesFieldsAndPreservesConfigOrder(t *testing.T) {
	s := newStatsStore(false)
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	s.recordInstance(cycleKindStartup, at, "sonarr-main", "sonarr", cycleInstanceStats{total: 10, monitored: 3, unmonitored: 7, wouldUnmonitor: 1})
	s.recordInstance(cycleKindStartup, at, "radarr-main", "radarr", cycleInstanceStats{total: 20, monitored: 5, unmonitored: 15})

	snap := s.snapshot()
	if len(snap.Instances) != 2 {
		t.Fatalf("Instances = %+v, want 2 entries", snap.Instances)
	}
	if snap.Instances[0].Name != "sonarr-main" || snap.Instances[1].Name != "radarr-main" {
		t.Fatalf("Instances order = [%s, %s], want config/first-seen order [sonarr-main, radarr-main]", snap.Instances[0].Name, snap.Instances[1].Name)
	}
	got := snap.Instances[0]
	if got.Type != "sonarr" || got.Total != 10 || got.Monitored != 3 || got.Unmonitored != 7 || got.WouldUnmonitor != 1 {
		t.Errorf("sonarr-main entry = %+v, fields do not match what was recorded", got)
	}
	if got.LastRun == nil || !got.LastRun.Equal(at) {
		t.Errorf("LastRun = %v, want %v", got.LastRun, at)
	}
	if got.LastCycleKind == nil || *got.LastCycleKind != cycleKindStartup {
		t.Errorf("LastCycleKind = %v, want %q", got.LastCycleKind, cycleKindStartup)
	}
}

// TestStatsStore_RecordInstance_LaterCycleOverwritesTotalsForTheSameInstance
// pins that a second cycle's totals REPLACE the first's (this is a live
// snapshot of the library, not an accumulator).
func TestStatsStore_RecordInstance_LaterCycleOverwritesTotalsForTheSameInstance(t *testing.T) {
	s := newStatsStore(false)
	s.recordInstance(cycleKindStartup, time.Now(), "radarr-main", "radarr", cycleInstanceStats{total: 10, monitored: 10})
	s.recordInstance(cycleKindSweep, time.Now(), "radarr-main", "radarr", cycleInstanceStats{total: 10, monitored: 4, unmonitored: 6})

	snap := s.snapshot()
	if len(snap.Instances) != 1 {
		t.Fatalf("want exactly one instance entry across two cycles of the same instance, got %d", len(snap.Instances))
	}
	got := snap.Instances[0]
	if got.Monitored != 4 || got.Unmonitored != 6 {
		t.Errorf("got Monitored=%d Unmonitored=%d, want the SECOND cycle's numbers (4/6), not the first's", got.Monitored, got.Unmonitored)
	}
	if *got.LastCycleKind != cycleKindSweep {
		t.Errorf("LastCycleKind = %q, want the most recent cycle's kind %q", *got.LastCycleKind, cycleKindSweep)
	}
}

// TestStatsStore_RecordInstance_PreservesReverseFindingsWhenACycleDidNotRunReverse
// is the three-state fidelity binding note: a webhook or --only-id cycle
// never runs the reverse pass, and must not overwrite a previous full
// cycle's findings with an empty slice — that would misreport "did not
// look" as "looked, found nothing".
func TestStatsStore_RecordInstance_PreservesReverseFindingsWhenACycleDidNotRunReverse(t *testing.T) {
	s := newStatsStore(false)
	s.recordInstance(cycleKindStartup, time.Now(), "radarr-main", "radarr", cycleInstanceStats{
		total: 5, reverseRan: true, reverseFindings: []reverseFinding{{ID: 7, Title: "Accidental", Reason: ReasonQualityCutoffNotMet}},
	})
	// A webhook cycle: reverseRan is false (the zero value), exactly as a
	// real webhook cycle's cycleInstanceStats would be.
	s.recordInstance(cycleKindWebhook, time.Now(), "radarr-main", "radarr", cycleInstanceStats{total: 5, wouldUnmonitor: 2})

	snap := s.snapshot()
	got := snap.Instances[0]
	if len(got.ReverseFindings) != 1 || got.ReverseFindings[0].ID != 7 {
		t.Errorf("ReverseFindings = %+v, want the PREVIOUS cycle's one finding preserved, not cleared by a cycle that never ran the pass", got.ReverseFindings)
	}
	// The fields the webhook cycle DID report must still update.
	if got.WouldUnmonitor != 2 {
		t.Errorf("WouldUnmonitor = %d, want 2 (this cycle's own value)", got.WouldUnmonitor)
	}
}

// TestStatsStore_RecordInstance_PreservesFileReportWhenACycleDidNotRunIt is
// the file-report half of the same rule.
func TestStatsStore_RecordInstance_PreservesFileReportWhenACycleDidNotRunIt(t *testing.T) {
	s := newStatsStore(false)
	s.recordInstance(cycleKindStartup, time.Now(), "radarr-main", "radarr", cycleInstanceStats{
		total: 5, fileReportRan: true,
		fileReport: fileReportSnapshot{Status: "ran", Duplicates: 2, Orphans: 1, Findings: []fileReportFindingRecord{{Kind: "duplicate", Path: "/x"}}},
	})
	s.recordInstance(cycleKindWebhook, time.Now(), "radarr-main", "radarr", cycleInstanceStats{total: 5})

	got := s.snapshot().Instances[0]
	if got.FileReport.Status != "ran" || got.FileReport.Duplicates != 2 || got.FileReport.Orphans != 1 {
		t.Errorf("FileReport = %+v, want the previous full cycle's file report preserved", got.FileReport)
	}
}

// TestStatsStore_RecordInstance_NewInstanceDefaultsFileReportToOff pins the
// pre-any-full-cycle default: an instance recorded for the first time by a
// cycle that never ran the file report (e.g. a webhook cycle firing before
// any full sweep has completed for it) must show "off", never a bare
// duplicates=0/orphans=0 that would misread as "ran clean".
func TestStatsStore_RecordInstance_NewInstanceDefaultsFileReportToOff(t *testing.T) {
	s := newStatsStore(false)
	s.recordInstance(cycleKindWebhook, time.Now(), "radarr-main", "radarr", cycleInstanceStats{total: 5})

	got := s.snapshot().Instances[0]
	if got.FileReport.Status != "off" {
		t.Errorf("FileReport.Status = %q, want %q before any full cycle has run it for this instance", got.FileReport.Status, "off")
	}
	if got.FileReport.Findings == nil || len(got.FileReport.Findings) != 0 {
		t.Errorf("FileReport.Findings = %+v, want a non-nil empty slice", got.FileReport.Findings)
	}
	if got.ReverseFindings == nil || len(got.ReverseFindings) != 0 {
		t.Errorf("ReverseFindings = %+v, want a non-nil empty slice", got.ReverseFindings)
	}
}

// TestStatsStore_LastActions_CapsAt50AndOrdersNewestFirst pins the plan's own
// number ("last 50 action lines") and the display order the GUI relies on to
// need no client-side sort.
func TestStatsStore_LastActions_CapsAt50AndOrdersNewestFirst(t *testing.T) {
	s := newStatsStore(false)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for cycle := 0; cycle < 55; cycle++ {
		s.recordInstance(cycleKindSweep, base.Add(time.Duration(cycle)*time.Minute), "radarr-main", "radarr", cycleInstanceStats{
			total:   1,
			actions: []actionRecord{{Action: ActionUnmonitor, ID: cycle, Title: "movie", Reason: ReasonCutoffMet}},
		})
	}

	got := s.snapshot().Instances[0].LastActions
	if len(got) != maxLastActions {
		t.Fatalf("len(LastActions) = %d, want %d", len(got), maxLastActions)
	}
	// Newest first: the last cycle recorded (id 54) must be at index 0, and
	// the oldest surviving one (55 cycles - 50 = the 5th, id 5) at the end.
	if got[0].ID != 54 {
		t.Errorf("LastActions[0].ID = %d, want 54 (the most recent action first)", got[0].ID)
	}
	if got[len(got)-1].ID != 5 {
		t.Errorf("LastActions[last].ID = %d, want 5 (the oldest surviving action)", got[len(got)-1].ID)
	}
	if !got[0].Time.Equal(base.Add(54 * time.Minute)) {
		t.Errorf("LastActions[0].Time = %v, want the recording cycle's own clock reading", got[0].Time)
	}
}

// TestStatsStore_Snapshot_IsolatedFromLaterMutation is the explicit isolation
// requirement: a snapshot handed to an HTTP handler must never change
// underneath it because a later cycle recorded more data.
func TestStatsStore_Snapshot_IsolatedFromLaterMutation(t *testing.T) {
	s := newStatsStore(false)
	s.recordInstance(cycleKindStartup, time.Now(), "radarr-main", "radarr", cycleInstanceStats{
		total: 1, reverseRan: true, reverseFindings: []reverseFinding{{ID: 1, Title: "A", Reason: ReasonCutoffMet}},
		fileReportRan: true, fileReport: fileReportSnapshot{Status: "ran", Findings: []fileReportFindingRecord{{Kind: "orphan", Path: "/a"}}},
		actions: []actionRecord{{Action: ActionUnmonitor, ID: 1, Title: "A"}},
	})

	first := s.snapshot()
	firstJSON := len(first.Instances[0].ReverseFindings)
	firstActions := len(first.Instances[0].LastActions)
	firstFiles := len(first.Instances[0].FileReport.Findings)
	firstLastRun := *first.Instances[0].LastRun

	// Mutate the store extensively AFTER taking the snapshot above.
	s.recordInstance(cycleKindSweep, time.Now().Add(time.Hour), "radarr-main", "radarr", cycleInstanceStats{
		total: 1, reverseRan: true, reverseFindings: []reverseFinding{{ID: 2}, {ID: 3}},
		fileReportRan: true, fileReport: fileReportSnapshot{Status: "ran", Findings: []fileReportFindingRecord{{Kind: "duplicate"}, {Kind: "orphan"}}},
		actions: []actionRecord{{Action: ActionUnmonitor, ID: 9}},
	})
	s.recordInstance(cycleKindSweep, time.Now().Add(2*time.Hour), "radarr-2", "radarr", cycleInstanceStats{total: 99})

	if len(first.Instances) != 1 {
		t.Errorf("the FIRST snapshot grew a second instance after a later recordInstance call; snapshots must be independent copies")
	}
	if len(first.Instances[0].ReverseFindings) != firstJSON {
		t.Errorf("first snapshot's ReverseFindings changed after a later cycle recorded more (len %d -> %d)", firstJSON, len(first.Instances[0].ReverseFindings))
	}
	if len(first.Instances[0].LastActions) != firstActions {
		t.Errorf("first snapshot's LastActions changed after a later cycle recorded more (len %d -> %d)", firstActions, len(first.Instances[0].LastActions))
	}
	if len(first.Instances[0].FileReport.Findings) != firstFiles {
		t.Errorf("first snapshot's FileReport.Findings changed after a later cycle recorded more (len %d -> %d)", firstFiles, len(first.Instances[0].FileReport.Findings))
	}
	if !first.Instances[0].LastRun.Equal(firstLastRun) {
		t.Errorf("first snapshot's LastRun changed after a later cycle (%v -> %v)", firstLastRun, *first.Instances[0].LastRun)
	}

	second := s.snapshot()
	if len(second.Instances) != 2 {
		t.Fatalf("a fresh snapshot taken NOW must see both instances, got %d", len(second.Instances))
	}
}

// --- conversion helpers --------------------------------------------------

func TestFileReportSnapshotFrom_ConvertsCountsAndFindings(t *testing.T) {
	c := fileReportCounts{
		configured: true, duplicates: 1, orphans: 1,
		findings: []fileReportFinding{
			{kind: fileKindDuplicate, diskPath: "/movies/A/A (2).mkv", group: "A", groupCount: 2},
			{kind: fileKindOrphan, diskPath: "/movies/Untracked/stray.mkv"},
		},
	}
	snap := fileReportSnapshotFrom(c)
	if snap.Status != "ran" {
		t.Errorf("Status = %q, want %q", snap.Status, "ran")
	}
	if snap.Duplicates != 1 || snap.Orphans != 1 {
		t.Errorf("Duplicates/Orphans = %d/%d, want 1/1", snap.Duplicates, snap.Orphans)
	}
	if len(snap.Findings) != 2 {
		t.Fatalf("Findings = %+v, want 2 entries", snap.Findings)
	}
	dup := snap.Findings[0]
	if dup.Kind != "duplicate" || dup.Group != "A" || dup.Count != 2 || dup.Path != "/movies/A/A (2).mkv" {
		t.Errorf("duplicate finding = %+v, fields do not match the source fileReportFinding", dup)
	}
	orphan := snap.Findings[1]
	if orphan.Kind != "orphan" || orphan.Group != "" || orphan.Count != 0 || orphan.Path != "/movies/Untracked/stray.mkv" {
		t.Errorf("orphan finding = %+v, want an empty group and zero count (omitted in JSON via omitempty)", orphan)
	}
}

func TestRadarrReverseFindings_ConvertsMovieDecisions(t *testing.T) {
	out := radarrReverseFindings([]movieDecision{{id: 7, title: "Accidental", reason: ReasonQualityCutoffNotMet}})
	if len(out) != 1 {
		t.Fatalf("want 1 finding, got %+v", out)
	}
	f := out[0]
	if f.ID != 7 || f.Title != "Accidental" || f.Reason != ReasonQualityCutoffNotMet {
		t.Errorf("got %+v, fields do not match the source movieDecision", f)
	}
	if f.SeriesID != 0 || f.Series != "" || f.Season != nil || f.SeriesMonitored != nil {
		t.Errorf("a Radarr finding must carry none of the Sonarr-only fields, got %+v", f)
	}
}

func TestSonarrReverseFindings_ConvertsSeasonFindingsWithSeriesMonitored(t *testing.T) {
	out := sonarrReverseFindings([]reverseSeasonFinding{
		{decision: seasonDecision{seriesID: 1, series: "Show", season: 2, reason: ReasonSeasonMonitorMismatch}, seriesMonitored: true},
	})
	if len(out) != 1 {
		t.Fatalf("want 1 finding, got %+v", out)
	}
	f := out[0]
	if f.SeriesID != 1 || f.Series != "Show" || f.Reason != ReasonSeasonMonitorMismatch {
		t.Errorf("got %+v, fields do not match the source reverseSeasonFinding", f)
	}
	if f.Season == nil || *f.Season != 2 {
		t.Fatalf("Season = %v, want 2", f.Season)
	}
	if f.SeriesMonitored == nil || !*f.SeriesMonitored {
		t.Fatalf("SeriesMonitored = %v, want true", f.SeriesMonitored)
	}
	if f.ID != 0 || f.Title != "" {
		t.Errorf("a Sonarr finding must carry none of the Radarr-only fields, got %+v", f)
	}
}

// --- decision-engine integration: the plumbing actually wired in ---------

// TestRunRadarrDecisionEngine_Stats_LibraryTotalsWouldUnmonitorAndConfirmedActions
// is the load-bearing proof that runRadarrDecisionEngine's new return value
// reflects a REAL cycle: two would-unmonitor movies, cross-check passing,
// write mode on — total/monitored/unmonitored are library-wide, wouldUnmonitor
// counts both candidates, and actions carries exactly the two confirmed
// unmonitor writes with the reason and title the write pass itself used.
func TestRunRadarrDecisionEngine_Stats_LibraryTotalsWouldUnmonitorAndConfirmedActions(t *testing.T) {
	fake := newRadarrFake(t, "", map[int]string{
		1: monitoredMovieDetail(1, "Would Unmonitor A"),
		2: monitoredMovieDetail(2, "Would Unmonitor B"),
	})
	movies := []movieListElement{
		wouldUnmonitorMovie(1, "Would Unmonitor A"),
		wouldUnmonitorMovie(2, "Would Unmonitor B"),
	}

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	result := runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude",
		fullLibraryScope(slog.LevelInfo), false, reverseOptions{}, fileReportOptions{})

	if result.total != 2 || result.monitored != 2 || result.unmonitored != 0 {
		t.Fatalf("total/monitored/unmonitored = %d/%d/%d, want 2/2/0:\n%s", result.total, result.monitored, result.unmonitored, buf.String())
	}
	if result.wouldUnmonitor != 2 {
		t.Fatalf("wouldUnmonitor = %d, want 2:\n%s", result.wouldUnmonitor, buf.String())
	}
	if len(result.actions) != 2 {
		t.Fatalf("actions = %+v, want exactly 2 confirmed unmonitor actions:\n%s", result.actions, buf.String())
	}
	byTitle := map[string]actionRecord{}
	for _, a := range result.actions {
		byTitle[a.Title] = a
	}
	for _, title := range []string{"Would Unmonitor A", "Would Unmonitor B"} {
		a, ok := byTitle[title]
		if !ok {
			t.Fatalf("no action recorded for %q; got %+v", title, result.actions)
		}
		if a.Action != ActionUnmonitor || a.Reason != ReasonCutoffMet || a.Season != nil {
			t.Errorf("action for %q = %+v, want Action=unmonitor Reason=%q Season=nil", title, a, ReasonCutoffMet)
		}
	}
	if result.reverseRan || result.fileReportRan {
		t.Errorf("neither pass was enabled this cycle; reverseRan/fileReportRan must both be false, got %+v", result)
	}
	if len(fake.puts()) != 2 {
		t.Fatalf("the fake must have actually received 2 PUTs for this test to prove anything, got %d", len(fake.puts()))
	}
}

// TestRunRadarrDecisionEngine_Stats_ReverseFindingsAndRemonitorAction proves
// the reverse-scan half: a confirmed re-monitor write is captured both as a
// reverseFinding (report) and as an actions entry (the write itself),
// reusing the exact fixture TestRunRadarrDecisionEngine_
// EveryReverseFindingIsAccountedForInTheSummary's "confirmed write" case
// already trusts to produce one.
func TestRunRadarrDecisionEngine_Stats_ReverseFindingsAndRemonitorAction(t *testing.T) {
	const findingID = 1
	reverseWanted := `{"page":1,"pageSize":100,"totalRecords":1,"records":[{"id":1,"title":"Accounted Movie"}]}`
	unmonitoredDetail := `{"id": 1, "title": "Accounted Movie", "monitored": false, "hasFile": true, "qualityProfileId": 1, "tags": []}`

	fake := newRadarrFake(t, "", map[int]string{findingID: unmonitoredDetail})
	fake.reverseWantedJSON = reverseWanted
	movies := []movieListElement{crossCheckWitnessMovie(5, "Ordinary Monitored"), unmonitoredBelowCutoffMovie(findingID, "Accounted Movie")}

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	result := runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{5: true}, "cutoffarr-exclude",
		fullLibraryScope(slog.LevelInfo), false, reverseOptions{enabled: true, remonitor: true}, fileReportOptions{})

	if !result.reverseRan {
		t.Fatalf("reverseRan = false, want true:\n%s", buf.String())
	}
	if len(result.reverseFindings) != 1 {
		t.Fatalf("reverseFindings = %+v, want exactly 1:\n%s", result.reverseFindings, buf.String())
	}
	rf := result.reverseFindings[0]
	if rf.ID != findingID || rf.Title != "Accounted Movie" || rf.Reason != ReasonQualityCutoffNotMet {
		t.Errorf("reverseFindings[0] = %+v, want id=%d title=%q reason=%q", rf, findingID, "Accounted Movie", ReasonQualityCutoffNotMet)
	}

	var remonitors []actionRecord
	for _, a := range result.actions {
		if a.Action == ActionRemonitor {
			remonitors = append(remonitors, a)
		}
	}
	if len(remonitors) != 1 {
		t.Fatalf("actions = %+v, want exactly 1 remonitor action:\n%s", result.actions, buf.String())
	}
	if remonitors[0].ID != findingID || remonitors[0].Title != "Accounted Movie" {
		t.Errorf("remonitor action = %+v, want id=%d title=%q", remonitors[0], findingID, "Accounted Movie")
	}
}

// TestRunRadarrDecisionEngine_Stats_FileReportCapturesRealDuplicateAndOrphan
// exercises the file report half through the real filesystem walk
// (runRadarrFileReport, unchanged) and fileReportSnapshotFrom together,
// mirroring the Phase 11 acceptance fixture shape.
func TestRunRadarrDecisionEngine_Stats_FileReportCapturesRealDuplicateAndOrphan(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFiles(t, dir,
		filepath.Join("Movie A", "Movie A.mkv"),
		filepath.Join("Movie A", "Movie A (2).mkv"),
		filepath.Join("Untracked", "stray.mkv"),
	)

	fake := newRadarrFake(t, "", nil)
	movies := []movieListElement{{
		ID: intPtr(1), Title: strPtr("Movie A"), Monitored: boolPtr(false), HasFile: boolPtr(true),
		Path: strPtr("/movies/Movie A"), MovieFile: &movieFileElement{Path: strPtr("/movies/Movie A/Movie A.mkv")},
	}}
	inst := fake.instance()
	inst.MediaRootMap = map[string]string{"/movies": dir}

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	result := runRadarrDecisionEngine(context.Background(), logger, inst, movies, map[int]bool{}, "cutoffarr-exclude",
		fullLibraryScope(slog.LevelInfo), false, reverseOptions{}, fileReportOptions{enabled: true})

	if !result.fileReportRan {
		t.Fatalf("fileReportRan = false, want true:\n%s", buf.String())
	}
	if result.fileReport.Status != "ran" {
		t.Fatalf("fileReport.Status = %q, want %q:\n%s", result.fileReport.Status, "ran", buf.String())
	}
	if result.fileReport.Duplicates != 1 || result.fileReport.Orphans != 1 {
		t.Fatalf("Duplicates/Orphans = %d/%d, want 1/1:\n%s", result.fileReport.Duplicates, result.fileReport.Orphans, buf.String())
	}
	if len(result.fileReport.Findings) != 2 {
		t.Fatalf("Findings = %+v, want 2 entries", result.fileReport.Findings)
	}
}

// TestRunSonarrDecisionEngine_Stats_SeasonTotalsAndConfirmedAction is the
// Sonarr twin, driven through the real read path (inspectSonarrLibrary) so
// the series/season data runSonarrDecisionEngine sees is exactly what a real
// cycle would fetch, not a hand-built approximation of it.
func TestRunSonarrDecisionEngine_Stats_SeasonTotalsAndConfirmedAction(t *testing.T) {
	fake := writableSonarrFake(t)
	logger, buf := newDecisionTestLogger(slog.LevelInfo)

	series, wantedEpisodeIDs, wantedSeasons, dataOK := inspectSonarrLibrary(context.Background(), logger, fake.instance())
	if !dataOK {
		t.Fatalf("inspectSonarrLibrary reported dataOK=false:\n%s", buf.String())
	}

	result := runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, wantedEpisodeIDs, wantedSeasons, "cutoffarr-exclude",
		fullLibraryScope(slog.LevelInfo), false, reverseOptions{}, fileReportOptions{})

	// writableSonarrFake's one series has exactly 2 seasons, both monitored.
	if result.total != 2 || result.monitored != 2 || result.unmonitored != 0 {
		t.Fatalf("total/monitored/unmonitored = %d/%d/%d, want 2/2/0:\n%s", result.total, result.monitored, result.unmonitored, buf.String())
	}
	if result.wouldUnmonitor < 1 {
		t.Fatalf("wouldUnmonitor = %d, want at least 1 (season 1 is eligible):\n%s", result.wouldUnmonitor, buf.String())
	}

	var unmonitors []actionRecord
	for _, a := range result.actions {
		if a.Action == ActionUnmonitor {
			unmonitors = append(unmonitors, a)
		}
	}
	if len(unmonitors) != 1 {
		t.Fatalf("actions = %+v, want exactly 1 confirmed unmonitor action (season 1):\n%s", result.actions, buf.String())
	}
	a := unmonitors[0]
	if a.ID != 1 || a.Title != "Write Me" || a.Season == nil || *a.Season != 1 {
		t.Errorf("unmonitor action = %+v, want seriesId=1 title=%q season=1", a, "Write Me")
	}
}
