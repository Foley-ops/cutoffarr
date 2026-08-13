package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

	s.recordInstance(cycleKindStartup, at, "sonarr-main", "sonarr", cycleInstanceStats{total: 10, monitored: 3, unmonitored: 7, wouldUnmonitor: 1, decisionsRan: true})
	s.recordInstance(cycleKindStartup, at, "radarr-main", "radarr", cycleInstanceStats{total: 20, monitored: 5, unmonitored: 15, decisionsRan: true})

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
	s.recordInstance(cycleKindStartup, time.Now(), "radarr-main", "radarr", cycleInstanceStats{total: 10, monitored: 10, decisionsRan: true})
	s.recordInstance(cycleKindSweep, time.Now(), "radarr-main", "radarr", cycleInstanceStats{total: 10, monitored: 4, unmonitored: 6, decisionsRan: true})

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
		total: 5, decisionsRan: true, reverseRan: true, reverseFindings: []reverseFinding{{ID: 7, Title: "Accidental", Reason: ReasonQualityCutoffNotMet}},
	})
	// A webhook cycle: reverseRan is false (the zero value), exactly as a
	// real webhook cycle's cycleInstanceStats would be — but decisionsRan is
	// still true, since a webhook cycle DOES run the evaluation loop (only
	// the reverse/file-report passes are scheduling-gated off it).
	s.recordInstance(cycleKindWebhook, time.Now(), "radarr-main", "radarr", cycleInstanceStats{total: 5, decisionsRan: true, wouldUnmonitor: 2})

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

// TestStatsStore_RecordInstance_ReverseAsOf_StaysAtLastTrustworthyPass is the
// controller ruling's own binding requirement (Phase 12 final round): "the
// skipped-reverse-pass overwrite is the highest-severity item — last-known-
// good preservation + a staleness indicator on the page". Preservation alone
// (the test above) is not enough for a human to tell "found nothing five
// minutes ago" from "found nothing five weeks ago"; ReverseAsOf must hold
// the timestamp of the findings CURRENTLY being shown, frozen at the last
// cycle whose reverse pass actually completed trustworthily, and must not
// silently advance just because a later cycle merely attempted (and lost
// trust in) the pass.
func TestStatsStore_RecordInstance_ReverseAsOf_StaysAtLastTrustworthyPass(t *testing.T) {
	s := newStatsStore(false)
	goodAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	skippedAt := goodAt.Add(24 * time.Hour)

	s.recordInstance(cycleKindStartup, goodAt, "radarr-main", "radarr", cycleInstanceStats{
		total: 5, decisionsRan: true, reverseRan: true,
		reverseFindings: []reverseFinding{{ID: 7, Title: "Accidental", Reason: ReasonQualityCutoffNotMet}},
	})
	got := s.snapshot().Instances[0]
	if got.ReverseAsOf == nil || !got.ReverseAsOf.Equal(goodAt) {
		t.Fatalf("ReverseAsOf = %v, want %v (the cycle that actually ran the pass)", got.ReverseAsOf, goodAt)
	}

	// A day later, the reverse pass could not be trusted this cycle (an
	// incomplete unmonitored wanted/cutoff set) — findings are preserved,
	// and ReverseAsOf must STILL say the findings are from goodAt, not
	// silently jump to skippedAt (which would claim a pass that never
	// actually completed).
	s.recordInstance(cycleKindSweep, skippedAt, "radarr-main", "radarr", cycleInstanceStats{
		total: 5, decisionsRan: true, reverseSkipped: true,
	})
	got = s.snapshot().Instances[0]
	if len(got.ReverseFindings) != 1 || got.ReverseFindings[0].ID != 7 {
		t.Fatalf("ReverseFindings = %+v, want the previous trustworthy pass's finding preserved", got.ReverseFindings)
	}
	if got.ReverseStatus != "skipped" {
		t.Fatalf("ReverseStatus = %q, want %q", got.ReverseStatus, "skipped")
	}
	if got.ReverseAsOf == nil || !got.ReverseAsOf.Equal(goodAt) {
		t.Errorf("ReverseAsOf = %v, want it to STILL say %v (the last cycle that actually completed the pass), not the skipped cycle's own time", got.ReverseAsOf, goodAt)
	}
}

// TestStatsStore_RecordInstance_ReverseAsOf_NilUntilAPassActuallyRuns pins
// the zero state: an instance that has never once had a trustworthy reverse
// pass (reverse globally off, or only ever reached by a webhook cycle) must
// report ReverseAsOf as null, not a zero time.Time that would render as some
// implausible epoch date.
func TestStatsStore_RecordInstance_ReverseAsOf_NilUntilAPassActuallyRuns(t *testing.T) {
	s := newStatsStore(false)
	s.recordInstance(cycleKindWebhook, time.Now(), "radarr-main", "radarr", cycleInstanceStats{total: 5, decisionsRan: true})

	got := s.snapshot().Instances[0]
	if got.ReverseAsOf != nil {
		t.Errorf("ReverseAsOf = %v, want nil: no cycle has ever completed a trustworthy reverse pass for this instance", got.ReverseAsOf)
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

// TestStatsStore_RecordInstance_CapturesCaseCollisionsAndPreservesThemAcrossACycleThatDidNotRunTheFileReport
// is [v2.1]'s addition to the file-report three-state fidelity rule above:
// CaseCollisions and a case-collision finding's own Names/EntryType must
// survive a later cycle that never ran the file report at all, exactly like
// Duplicates/Orphans/Findings already do.
func TestStatsStore_RecordInstance_CapturesCaseCollisionsAndPreservesThemAcrossACycleThatDidNotRunTheFileReport(t *testing.T) {
	s := newStatsStore(false)
	s.recordInstance(cycleKindStartup, time.Now(), "radarr-main", "radarr", cycleInstanceStats{
		total: 5, fileReportRan: true,
		fileReport: fileReportSnapshot{
			Status: "ran", Duplicates: 1, Orphans: 0, CaseCollisions: 1,
			Findings: []fileReportFindingRecord{{
				Kind: "case-collision", Path: "/movies/Show", Display: "Movies/Show", EntryType: "dir",
				Names: []caseCollisionNameRecord{{Name: "Show", Tracked: true}, {Name: "show", Tracked: false}},
			}},
		},
	})
	s.recordInstance(cycleKindWebhook, time.Now(), "radarr-main", "radarr", cycleInstanceStats{total: 5})

	got := s.snapshot().Instances[0]
	if got.FileReport.CaseCollisions != 1 {
		t.Errorf("CaseCollisions = %d, want 1 preserved from the previous full cycle", got.FileReport.CaseCollisions)
	}
	if len(got.FileReport.Findings) != 1 || got.FileReport.Findings[0].EntryType != "dir" {
		t.Fatalf("Findings = %+v, want the preserved case-collision finding", got.FileReport.Findings)
	}
	names := got.FileReport.Findings[0].Names
	if len(names) != 2 || names[0].Name != "Show" || !names[0].Tracked || names[1].Name != "show" || names[1].Tracked {
		t.Errorf("Names = %+v, want both colliding names with their tracked correlation preserved", names)
	}
}

// TestCloneInstanceStatsView_DeepCopiesCaseCollisionNames is the [v2.1]
// isolation check TestStatsStore_Snapshot_IsolatedFromLaterMutation already
// runs for ReverseFindings/LastActions/FileReport.Findings, extended one
// level deeper: a fileReportFindingRecord's own Names slice must not alias
// the store's backing array either, or writing through a handed-out
// snapshot's Names element would silently corrupt the store.
func TestCloneInstanceStatsView_DeepCopiesCaseCollisionNames(t *testing.T) {
	s := newStatsStore(false)
	s.recordInstance(cycleKindStartup, time.Now(), "radarr-main", "radarr", cycleInstanceStats{
		total: 1, fileReportRan: true,
		fileReport: fileReportSnapshot{
			Status: "ran", CaseCollisions: 1,
			Findings: []fileReportFindingRecord{{
				Kind: "case-collision", Path: "/a", EntryType: "dir",
				Names: []caseCollisionNameRecord{{Name: "A", Tracked: true}, {Name: "a", Tracked: false}},
			}},
		},
	})
	first := s.snapshot()
	first.Instances[0].FileReport.Findings[0].Names[0].Tracked = false
	first.Instances[0].FileReport.Findings[0].Names[0].Name = "MUTATED"

	verify := s.snapshot().Instances[0]
	if verify.FileReport.Findings[0].Names[0].Name != "A" || !verify.FileReport.Findings[0].Names[0].Tracked {
		t.Errorf("writing through the first snapshot's Names element corrupted the store: a fresh snapshot now reads %+v, want the untouched original {A true}", verify.FileReport.Findings[0].Names[0])
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
	if got.ReverseStatus != "off" {
		t.Errorf("ReverseStatus = %q, want %q before any full cycle has run it for this instance", got.ReverseStatus, "off")
	}
}

// TestStatsStore_RecordInstance_ReverseStatusTracksRanVsSkipped is round-3's
// reverse-scan twin of the file-report three-state coverage above: a cycle
// that ran the pass and trusted it reports "ran"; a LATER cycle that
// attempted the pass but could not trust it (reverseSkipped, e.g. an
// incomplete unmonitored wanted set) must report "skipped" — never silently
// keep reporting "ran" from the earlier cycle — while still preserving the
// earlier cycle's actual findings rather than clearing them (the existing
// three-state fidelity rule for ReverseFindings itself, unchanged here).
func TestStatsStore_RecordInstance_ReverseStatusTracksRanVsSkipped(t *testing.T) {
	s := newStatsStore(false)
	s.recordInstance(cycleKindStartup, time.Now(), "radarr-main", "radarr", cycleInstanceStats{
		total: 5, decisionsRan: true, reverseRan: true,
		reverseFindings: []reverseFinding{{ID: 7, Title: "Real finding", Reason: ReasonQualityCutoffNotMet}},
	})
	if got := s.snapshot().Instances[0].ReverseStatus; got != "ran" {
		t.Fatalf("ReverseStatus after a trustworthy reverse pass = %q, want %q", got, "ran")
	}

	s.recordInstance(cycleKindSweep, time.Now(), "radarr-main", "radarr", cycleInstanceStats{
		total: 5, decisionsRan: true, reverseSkipped: true,
	})
	got := s.snapshot().Instances[0]
	if got.ReverseStatus != "skipped" {
		t.Errorf("ReverseStatus after a cycle that attempted but could not trust the reverse pass = %q, want %q", got.ReverseStatus, "skipped")
	}
	if len(got.ReverseFindings) != 1 || got.ReverseFindings[0].ID != 7 {
		t.Errorf("ReverseFindings after a skipped reverse pass = %+v, want the PREVIOUS trustworthy finding preserved", got.ReverseFindings)
	}

	// A webhook cycle (neither reverseRan nor reverseSkipped — the pass was
	// never even scheduled) must leave the "skipped" status exactly as it
	// was, never silently reverting to "off" or back to "ran".
	s.recordInstance(cycleKindWebhook, time.Now(), "radarr-main", "radarr", cycleInstanceStats{total: 5, decisionsRan: true})
	if got := s.snapshot().Instances[0].ReverseStatus; got != "skipped" {
		t.Errorf("ReverseStatus after an unrelated webhook cycle = %q, want the previous %q preserved", got, "skipped")
	}
}

// TestStatsStore_RecordInstance_SetsLastCycleStatusOK pins that a cycle
// which reaches recordInstance AND completes an evaluation (decisionsRan) is
// marked ok, overwriting whatever a PREVIOUS failed cycle may have left,
// unlike every other field on instanceStatsView. It also pins the round-4
// review fix's own case in the same place: recordInstance is called (the
// connectivity/library-read gates recordUnreachable exists for both
// succeeded) but decisionsRan is false (an engine-internal warn-and-skip
// abort) must still land on "skipped", not "ok" — reaching the engine is a
// weaker statement than completing an evaluation.
func TestStatsStore_RecordInstance_SetsLastCycleStatusOK(t *testing.T) {
	s := newStatsStore(false)
	s.recordUnreachable("radarr-main", "radarr", unreachableReasonConnectivity)
	if got := s.snapshot().Instances[0].LastCycleStatus; got.Status != cycleStatusSkipped {
		t.Fatalf("LastCycleStatus after recordUnreachable = %+v, want status=%q", got, cycleStatusSkipped)
	}

	s.recordInstance(cycleKindSweep, time.Now(), "radarr-main", "radarr", cycleInstanceStats{total: 5, decisionsRan: true})
	got := s.snapshot().Instances[0].LastCycleStatus
	if got.Status != cycleStatusOK || got.Reason != "" {
		t.Errorf("LastCycleStatus after a cycle that reached the engine and completed an evaluation = %+v, want {status: %q, reason: \"\"}, overwriting the previous skipped status", got, cycleStatusOK)
	}

	// A cycle that reaches recordInstance but never completes an evaluation
	// (decisionsRan false) must not fall back to "ok" just because it beat
	// the connectivity/library-read gates.
	s.recordInstance(cycleKindSweep, time.Now(), "radarr-main", "radarr", cycleInstanceStats{total: 5, decisionsRan: false})
	got2 := s.snapshot().Instances[0].LastCycleStatus
	if got2.Status != cycleStatusSkipped {
		t.Errorf("LastCycleStatus after a cycle that reached the engine but never completed an evaluation = %+v, want status=%q, overwriting the previous ok status", got2, cycleStatusSkipped)
	}
	if got2.Reason == "" {
		t.Error("LastCycleStatus.Reason is empty for a cycle that reached the engine but aborted mid-evaluation")
	}
}

// TestStatsStore_RecordUnreachable_NeverReachedInstance_CreatesAVisibleEntry
// is the fix's whole point: before it, an instance that had NEVER once been
// reached was simply absent from `instances`, indistinguishable from "not
// configured in this daemon at all". Now such an instance appears with its
// zero-value totals and an explicit skipped LastCycleStatus.
func TestStatsStore_RecordUnreachable_NeverReachedInstance_CreatesAVisibleEntry(t *testing.T) {
	s := newStatsStore(false)
	s.recordUnreachable("radarr-main", "radarr", unreachableReasonLibraryRead)

	snap := s.snapshot()
	if len(snap.Instances) != 1 {
		t.Fatalf("Instances = %+v, want exactly 1 (the unreachable instance must be visible, not absent)", snap.Instances)
	}
	got := snap.Instances[0]
	if got.Name != "radarr-main" || got.Type != "radarr" {
		t.Errorf("instance = %+v, want name=radarr-main type=radarr", got)
	}
	if got.LastCycleStatus.Status != cycleStatusSkipped || got.LastCycleStatus.Reason != unreachableReasonLibraryRead {
		t.Errorf("LastCycleStatus = %+v, want {status: %q, reason: %q}", got.LastCycleStatus, cycleStatusSkipped, unreachableReasonLibraryRead)
	}
	if got.Total != 0 || got.Monitored != 0 || got.Unmonitored != 0 || got.WouldUnmonitor != 0 {
		t.Errorf("an instance that has NEVER been reached must show zero-value totals, got %+v", got)
	}
	if got.LastRun != nil {
		t.Errorf("LastRun = %v, want nil: this cycle never reached the decision engine, so it never produced a run to timestamp", got.LastRun)
	}
	if got.FileReport.Status != "off" || got.ReverseStatus != "off" {
		t.Errorf("FileReport.Status/ReverseStatus = %q/%q, want off/off for a never-reached instance", got.FileReport.Status, got.ReverseStatus)
	}
}

// TestStatsStore_RecordUnreachable_PreviouslyGoodInstance_LeavesTrustedFieldsAlone
// is the OTHER half: an instance the daemon has reached before, going
// unreachable on a LATER cycle, must keep showing its last-known-good
// totals/findings/actions (never clobbered with zeroes or an empty slice)
// while LastCycleStatus alone reports the new failure.
func TestStatsStore_RecordUnreachable_PreviouslyGoodInstance_LeavesTrustedFieldsAlone(t *testing.T) {
	s := newStatsStore(false)
	s.recordInstance(cycleKindStartup, time.Now(), "radarr-main", "radarr", cycleInstanceStats{
		total: 100, monitored: 40, unmonitored: 60, wouldUnmonitor: 3, decisionsRan: true,
		reverseRan:      true,
		reverseFindings: []reverseFinding{{ID: 7, Title: "Real finding", Reason: ReasonQualityCutoffNotMet}},
		fileReportRan:   true,
		fileReport:      fileReportSnapshot{Status: "ran", Duplicates: 1, Orphans: 2, Findings: []fileReportFindingRecord{{Kind: "duplicate", Path: "/x"}}},
		actions:         []actionRecord{{Action: ActionUnmonitor, ID: 1, Title: "Movie", Reason: ReasonCutoffMet}},
	})
	before := s.snapshot().Instances[0]

	s.recordUnreachable("radarr-main", "radarr", unreachableReasonConnectivity)

	got := s.snapshot().Instances[0]
	if got.Total != before.Total || got.Monitored != before.Monitored || got.Unmonitored != before.Unmonitored || got.WouldUnmonitor != before.WouldUnmonitor {
		t.Errorf("totals changed after recordUnreachable: before=%+v after=%+v", before, got)
	}
	if len(got.ReverseFindings) != 1 || got.ReverseFindings[0].ID != 7 {
		t.Errorf("ReverseFindings changed after recordUnreachable: got %+v", got.ReverseFindings)
	}
	if got.FileReport.Status != "ran" || got.FileReport.Duplicates != 1 {
		t.Errorf("FileReport changed after recordUnreachable: got %+v", got.FileReport)
	}
	if len(got.LastActions) != 1 {
		t.Errorf("LastActions changed after recordUnreachable: got %+v", got.LastActions)
	}
	if got.LastRun == nil || !got.LastRun.Equal(*before.LastRun) {
		t.Errorf("LastRun changed after recordUnreachable: before=%v after=%v", before.LastRun, got.LastRun)
	}
	if got.LastCycleStatus.Status != cycleStatusSkipped || got.LastCycleStatus.Reason != unreachableReasonConnectivity {
		t.Errorf("LastCycleStatus = %+v, want {status: %q, reason: %q}", got.LastCycleStatus, cycleStatusSkipped, unreachableReasonConnectivity)
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

// TestStatsStore_LastActions_WithinOneCycleKeepsTheNewestNotTheFirst is
// round-4 review's finding: the existing cap/order test above only ever
// records ONE action per cycle across 55 cycles, so it cannot see that
// WITHIN a single cycle recordInstance used to append cs.actions in
// PERFORMED order (oldest first) ahead of the previous list — newest-first
// ACROSS cycles, but oldest-first inside one. A cycle that alone produces
// more than maxLastActions actions (a live sweep's first pass over a mature
// library, unmonitoring hundreds of movies at once) then had
// `merged[:maxLastActions]` keep that cycle's FIRST 50 actions and discard
// its most recent ones — the opposite of the plan's "last 50 action lines".
func TestStatsStore_LastActions_WithinOneCycleKeepsTheNewestNotTheFirst(t *testing.T) {
	s := newStatsStore(false)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// One cycle, 60 actions, recorded in the order they were actually
	// performed (ID 0 first, ID 59 last).
	actions := make([]actionRecord, 60)
	for i := 0; i < 60; i++ {
		actions[i] = actionRecord{Action: ActionUnmonitor, ID: i, Title: "movie", Reason: ReasonCutoffMet}
	}
	s.recordInstance(cycleKindSweep, at, "radarr-main", "radarr", cycleInstanceStats{
		total: 60, actions: actions,
	})

	got := s.snapshot().Instances[0].LastActions
	if len(got) != maxLastActions {
		t.Fatalf("len(LastActions) = %d, want %d", len(got), maxLastActions)
	}
	// The LAST action this cycle performed (ID 59) must survive at index 0 —
	// not the first one it performed (ID 0), which the pre-fix code kept.
	if got[0].ID != 59 {
		t.Errorf("LastActions[0].ID = %d, want 59 (the most recently PERFORMED action within this cycle, not the first)", got[0].ID)
	}
	// The 50 survivors must be the 50 MOST RECENT of the 60 (IDs 10..59),
	// newest first, so the oldest surviving one is ID 10 — not ID 49, which
	// is what "keep the first 50, oldest-first within the cycle" would leave
	// at the tail.
	if got[len(got)-1].ID != 10 {
		t.Errorf("LastActions[last].ID = %d, want 10 (the oldest of the 50 MOST RECENTLY performed actions)", got[len(got)-1].ID)
	}
}

// TestStatsStore_Snapshot_IsolatedFromLaterMutation is the explicit isolation
// requirement: a snapshot handed to an HTTP handler must never change
// underneath it because a later cycle recorded more data, AND — the part a
// length-only check cannot catch — must not share backing storage with the
// store at all: writing THROUGH the handed-out snapshot's own elements must
// never reach the store's live state either. A shallow struct copy (or a
// snapshot loop that dropped cloneInstanceStatsView) would pass every
// length/count assertion here while still aliasing the same slice backing
// arrays and the same *time.Time, which is exactly the hazard this test
// exists to catch: a future recordInstance that appends into spare capacity,
// or any code that mutates an element in place, would silently corrupt a
// snapshot a handler is in the middle of serializing.
func TestStatsStore_Snapshot_IsolatedFromLaterMutation(t *testing.T) {
	s := newStatsStore(false)
	s.recordInstance(cycleKindStartup, time.Now(), "radarr-main", "radarr", cycleInstanceStats{
		total: 1, decisionsRan: true,
		reverseRan: true, reverseFindings: []reverseFinding{{ID: 1, Title: "A", Reason: ReasonCutoffMet}},
		fileReportRan: true, fileReport: fileReportSnapshot{Status: "ran", Findings: []fileReportFindingRecord{{Kind: "orphan", Path: "/a"}}},
		actions: []actionRecord{{Action: ActionUnmonitor, ID: 1, Title: "A"}},
	})

	first := s.snapshot()
	firstJSON := len(first.Instances[0].ReverseFindings)
	firstActions := len(first.Instances[0].LastActions)
	firstFiles := len(first.Instances[0].FileReport.Findings)

	// Write THROUGH the handed-out snapshot's own elements — not append,
	// mutate what is already there. If cloneInstanceStatsView were deleted
	// or weakened to a shallow copy, every one of these writes would land
	// in the store's own backing arrays/pointee, and the "fresh snapshot"
	// assertions below would see the corruption.
	first.Instances[0].ReverseFindings[0].ID = -1
	first.Instances[0].ReverseFindings[0].Title = "MUTATED"
	first.Instances[0].LastActions[0].Title = "MUTATED"
	first.Instances[0].FileReport.Findings[0].Path = "MUTATED"
	*first.Instances[0].LastRun = time.Unix(0, 0)

	verify := s.snapshot()
	vGot := verify.Instances[0]
	if vGot.ReverseFindings[0].ID != 1 || vGot.ReverseFindings[0].Title != "A" {
		t.Errorf("writing through the first snapshot's ReverseFindings element corrupted the store: a fresh snapshot now reads %+v, want the untouched original {ID:1 Title:A}", vGot.ReverseFindings[0])
	}
	if vGot.LastActions[0].Title != "A" {
		t.Errorf("writing through the first snapshot's LastActions element corrupted the store: a fresh snapshot now reads Title=%q, want the untouched original %q", vGot.LastActions[0].Title, "A")
	}
	if vGot.FileReport.Findings[0].Path != "/a" {
		t.Errorf("writing through the first snapshot's FileReport.Findings element corrupted the store: a fresh snapshot now reads Path=%q, want the untouched original %q", vGot.FileReport.Findings[0].Path, "/a")
	}
	if vGot.LastRun.Equal(time.Unix(0, 0)) {
		t.Errorf("writing through *first.Instances[0].LastRun corrupted the store: a fresh snapshot's LastRun now reads the mutated value; recordInstance/cloneInstanceStatsView must hand out a pointer to its OWN copy, never the store's")
	}

	// Mutate the store extensively AFTER taking the snapshot above (a real
	// later cycle, not a write-through) — the store-side half of isolation.
	s.recordInstance(cycleKindSweep, time.Now().Add(time.Hour), "radarr-main", "radarr", cycleInstanceStats{
		total: 1, decisionsRan: true,
		reverseRan: true, reverseFindings: []reverseFinding{{ID: 2}, {ID: 3}},
		fileReportRan: true, fileReport: fileReportSnapshot{Status: "ran", Findings: []fileReportFindingRecord{{Kind: "duplicate"}, {Kind: "orphan"}}},
		actions: []actionRecord{{Action: ActionUnmonitor, ID: 9}},
	})
	s.recordInstance(cycleKindSweep, time.Now().Add(2*time.Hour), "radarr-2", "radarr", cycleInstanceStats{total: 99, decisionsRan: true})

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
			{kind: fileKindDuplicate, diskPath: "/movies/A/A (2).mkv", displayPath: "Movies/A/A (2).mkv", group: "A", groupCount: 2},
			{kind: fileKindOrphan, diskPath: "/movies/Untracked/stray.mkv", displayPath: "Movies/Untracked/stray.mkv"},
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
	if dup.Display != "Movies/A/A (2).mkv" {
		t.Errorf("duplicate finding Display = %q, want the source fileReportFinding.displayPath carried through verbatim", dup.Display)
	}
	orphan := snap.Findings[1]
	if orphan.Kind != "orphan" || orphan.Group != "" || orphan.Count != 0 || orphan.Path != "/movies/Untracked/stray.mkv" {
		t.Errorf("orphan finding = %+v, want an empty group and zero count (omitted in JSON via omitempty)", orphan)
	}
	if orphan.Display != "Movies/Untracked/stray.mkv" {
		t.Errorf("orphan finding Display = %q, want the source fileReportFinding.displayPath carried through verbatim", orphan.Display)
	}
}

// TestFileReportSnapshotFrom_ConvertsCaseCollisionFindingWithNamesAndEntryType
// is [v2.1]'s addition to TestFileReportSnapshotFrom_ConvertsCountsAndFindings
// above: a case-collision finding's own entryType/names must survive the
// filereport.go -> stats.go conversion, and it carries no duplicate-only
// group/count (a case-collision finding has no sibling group).
func TestFileReportSnapshotFrom_ConvertsCaseCollisionFindingWithNamesAndEntryType(t *testing.T) {
	c := fileReportCounts{
		configured: true, caseCollisions: 1,
		findings: []fileReportFinding{
			{
				kind: fileKindCaseCollision, diskPath: "/movies/Show", displayPath: "Movies/Show",
				entryType: fileReportEntryTypeDir,
				names:     []caseCollisionEntry{{name: "Show", tracked: true}, {name: "show", tracked: false}},
			},
		},
	}
	snap := fileReportSnapshotFrom(c)
	if snap.CaseCollisions != 1 {
		t.Errorf("CaseCollisions = %d, want 1", snap.CaseCollisions)
	}
	if len(snap.Findings) != 1 {
		t.Fatalf("Findings = %+v, want 1", snap.Findings)
	}
	f := snap.Findings[0]
	if f.Kind != "case-collision" || f.EntryType != "dir" || f.Path != "/movies/Show" || f.Display != "Movies/Show" {
		t.Errorf("finding = %+v, fields do not match the source fileReportFinding", f)
	}
	if f.Group != "" || f.Count != 0 {
		t.Errorf("finding = %+v, want an empty group and zero count: a case-collision finding has no duplicate grouping", f)
	}
	wantNames := []caseCollisionNameRecord{{Name: "Show", Tracked: true}, {Name: "show", Tracked: false}}
	if len(f.Names) != 2 || f.Names[0] != wantNames[0] || f.Names[1] != wantNames[1] {
		t.Errorf("Names = %+v, want %+v carried through with their tracked correlation", f.Names, wantNames)
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

// TestRunRadarrDecisionEngine_Stats_UnmonitoredExcludesAbsentMonitoredField is
// round-4's final-round finding: unmonitored used to be computed as
// total-monitored, which folds a movie whose "monitored" key is entirely
// ABSENT (untrusted input — rule 1 excludes it and warnIfFieldAbsent warns
// about it, see this function's own loop a few lines below the totals) into
// the same bucket as a movie the *arr genuinely reports monitored=false. The
// shelf's headline "N at rest" is built directly from Unmonitored, so an
// untrusted movie used to silently count as "at rest" — the same
// absence-is-untrusted-not-a-state posture this file already enforces
// everywhere else, now pinned for the totals themselves.
func TestRunRadarrDecisionEngine_Stats_UnmonitoredExcludesAbsentMonitoredField(t *testing.T) {
	fake := newRadarrFake(t, "", nil)
	movies := []movieListElement{
		// monitored=true, still hunting (quality cutoff not met) — never a
		// write candidate, so the fake needs no detail/PUT fixture for it.
		crossCheckWitnessMovie(1, "Still Hunting"),
		// monitored=false (trusted, already at rest).
		unmonitoredBelowCutoffMovie(2, "Already At Rest"),
		// monitored ENTIRELY ABSENT from the payload — untrusted, not a state.
		{
			ID: intPtr(3), Title: strPtr("Untrusted"), HasFile: boolPtr(true),
			QualityProfileID: intPtr(1), Tags: &noTags,
			MovieFile: &movieFileElement{ID: intPtr(1), QualityCutoffNotMet: boolPtr(true)},
		},
	}

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	result := runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude",
		fullLibraryScope(slog.LevelInfo), false, reverseOptions{}, fileReportOptions{})

	if result.total != 3 {
		t.Fatalf("total = %d, want 3 (every library element, trusted or not):\n%s", result.total, buf.String())
	}
	if result.monitored != 1 {
		t.Errorf("monitored = %d, want 1:\n%s", result.monitored, buf.String())
	}
	if result.unmonitored != 1 {
		t.Errorf("unmonitored = %d, want 1 — a movie whose \"monitored\" field is entirely absent must land in NEITHER bucket, not be folded into \"at rest\" by a total-monitored subtraction:\n%s", result.unmonitored, buf.String())
	}
	if result.monitored+result.unmonitored == result.total {
		t.Errorf("monitored(%d)+unmonitored(%d) == total(%d): the untrusted movie was folded into one of the two trusted buckets", result.monitored, result.unmonitored, result.total)
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

// TestRunRadarrDecisionEngine_Stats_DryRun_ReverseFindingsButNoRemonitorAction
// is TestRunRadarrDecisionEngine_Stats_ReverseFindingsAndRemonitorAction's
// dry-run twin — the SAME fixture, dryRun=true instead of false — proving the
// REVERSE direction honors "a rehearsal is never reported as an action
// taken" exactly like the forward direction already does above:
// reverseFindings must still report the finding (the pass ran and is
// trustworthy regardless of dry-run), but actions must stay empty, since
// writeMovieMonitored never confirms a write in dry-run (the §2.1 gate). Round-3
// review fix: before this test, only the FORWARD direction's dry-run
// guarantee was pinned by any test; the reverse direction's was true in the
// code (decision.go, reverse.go's record()) but unverified.
func TestRunRadarrDecisionEngine_Stats_DryRun_ReverseFindingsButNoRemonitorAction(t *testing.T) {
	const findingID = 1
	reverseWanted := `{"page":1,"pageSize":100,"totalRecords":1,"records":[{"id":1,"title":"Accounted Movie"}]}`
	unmonitoredDetail := `{"id": 1, "title": "Accounted Movie", "monitored": false, "hasFile": true, "qualityProfileId": 1, "tags": []}`

	fake := newRadarrFake(t, "", map[int]string{findingID: unmonitoredDetail})
	fake.reverseWantedJSON = reverseWanted
	movies := []movieListElement{crossCheckWitnessMovie(5, "Ordinary Monitored"), unmonitoredBelowCutoffMovie(findingID, "Accounted Movie")}

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	result := runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{5: true}, "cutoffarr-exclude",
		fullLibraryScope(slog.LevelInfo), true, reverseOptions{enabled: true, remonitor: true}, fileReportOptions{})

	if !result.reverseRan {
		t.Fatalf("reverseRan = false, want true: a trustworthy reverse pass still ran in dry-run:\n%s", buf.String())
	}
	if len(result.reverseFindings) != 1 || result.reverseFindings[0].ID != findingID {
		t.Fatalf("reverseFindings = %+v, want exactly the one finding, dry-run or not:\n%s", result.reverseFindings, buf.String())
	}
	if len(result.actions) != 0 {
		t.Fatalf("actions = %+v, want empty: dry-run must never record a confirmed remonitor action:\n%s", result.actions, buf.String())
	}
	if len(fake.puts()) != 0 {
		t.Fatalf("the fake must have received ZERO PUTs in dry-run for this test to prove anything, got %d", len(fake.puts()))
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

	// Path-display GUI fix: each finding's Display must be the root-relative
	// path (the mapped root's own last segment, then the remainder under
	// it) — never the full disk path, which on a real deployment can carry
	// a long, host-specific mount prefix (dir's own t.TempDir() value
	// stands in for that here).
	wantDupDisplay := filepath.Join(filepath.Base(dir), "Movie A", "Movie A (2).mkv")
	wantOrphanDisplay := filepath.Join(filepath.Base(dir), "Untracked", "stray.mkv")
	for _, f := range result.fileReport.Findings {
		var want string
		switch f.Kind {
		case "duplicate":
			want = wantDupDisplay
		case "orphan":
			want = wantOrphanDisplay
		default:
			t.Fatalf("finding has unexpected kind %q", f.Kind)
		}
		if f.Display != want {
			t.Errorf("%s finding Display = %q, want %q", f.Kind, f.Display, want)
		}
		if f.Display == f.Path {
			t.Errorf("%s finding Display equals the full disk Path (%q) — Display must be root-relative, not the long host-specific path this field exists to shorten", f.Kind, f.Path)
		}
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

// TestRunSonarrDecisionEngine_Stats_UnmonitoredExcludesAbsentMonitoredSeason
// is the Sonarr twin of the Radarr test above: a season whose "monitored"
// field is entirely absent (evaluateSeries's own "skipping season: missing
// monitored field" warn-and-exclude path, decision.go) must land in neither
// the monitored nor the unmonitored season count, not be folded into "at
// rest" by a total-monitored subtraction.
func TestRunSonarrDecisionEngine_Stats_UnmonitoredExcludesAbsentMonitoredSeason(t *testing.T) {
	fake := newSonarrEngineFake(t, "[]", "[]")
	series := []seriesElement{
		testSeries(1, "Show", true, 1, nil,
			testSeason(1, true, 0, 0),
			testSeason(2, false, 0, 0),
			seriesSeasonElement{SeasonNumber: intPtr(3), Monitored: nil, Statistics: &seasonStatisticsElement{EpisodeFileCount: intPtr(0), TotalEpisodeCount: intPtr(0)}},
		),
	}

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	result := runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude",
		fullLibraryScope(slog.LevelInfo), true, reverseOptions{}, fileReportOptions{})

	if result.total != 3 {
		t.Fatalf("total = %d, want 3 (every season, trusted or not):\n%s", result.total, buf.String())
	}
	if result.monitored != 1 {
		t.Errorf("monitored = %d, want 1:\n%s", result.monitored, buf.String())
	}
	if result.unmonitored != 1 {
		t.Errorf("unmonitored = %d, want 1 — a season whose \"monitored\" field is entirely absent must land in NEITHER bucket:\n%s", result.unmonitored, buf.String())
	}
	if result.monitored+result.unmonitored == result.total {
		t.Errorf("monitored(%d)+unmonitored(%d) == total(%d): the untrusted season was folded into one of the two trusted buckets", result.monitored, result.unmonitored, result.total)
	}
}

// --- review-fix regression coverage: reverseRan/decisionsRan gating ------
//
// The four tests below pin two review fixes together: a reverse pass that
// RAN but could not be trusted (reverseCounts.skipped) must not be captured
// as reverseRan=true with an empty findings slice, and a cycle that ABORTED
// before its evaluation loop ran to completion must not be captured as
// decisionsRan=true with wouldUnmonitor at its zero value. Both are the same
// shape of bug — "this cycle is in no position to state that number" (the
// wording reverseCounts.summaryAttrs' own doc comment uses) captured as
// though it were an observed fact — and each is proven at two levels: the
// engine's own return value, and the store-level consequence (a previous
// real cycle's data must survive being folded in behind the untrustworthy
// one).

// TestRunRadarrDecisionEngine_Stats_ReverseScanSkipped_ReverseRanStaysFalse
// reuses TestRunRadarrDecisionEngine_ReverseScan_IncompleteWantedSet_
// SkipsOnlyTheReversePass's exact fixture (reverse_test.go): an unmonitored
// wanted/cutoff set that claims more records than it returns, which sets
// reverseCounts.skipped and returns before ever populating movieFindings.
func TestRunRadarrDecisionEngine_Stats_ReverseScanSkipped_ReverseRanStaysFalse(t *testing.T) {
	fake := newRadarrFake(t, "", nil)
	fake.reverseWantedJSON = `{"page":1,"pageSize":100,"totalRecords":50,"records":[]}` // claims 50, returns 0

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	movies := []movieListElement{
		wouldUnmonitorMovie(1, "Forward Candidate"),
		unmonitoredBelowCutoffMovie(7, "Reverse Candidate"),
	}

	result := runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{}, "cutoffarr-exclude",
		fullLibraryScope(slog.LevelInfo), true, reverseScanOn(), fileReportOptions{})

	if result.reverseRan {
		t.Fatalf("reverseRan = true, want false: the reverse pass was skipped (an incomplete unmonitored wanted set), so it must not be captured as a pass that ran and found nothing:\n%s", buf.String())
	}
	if !result.reverseSkipped {
		t.Errorf("reverseSkipped = false, want true: the pass DID attempt to run this cycle (reverse.enabled) but could not be trusted, which is a distinct state from \"never scheduled\" — see instanceStatsView.ReverseStatus's own comment")
	}
	if len(result.reverseFindings) != 0 {
		t.Errorf("reverseFindings = %+v, want empty/unset alongside reverseRan=false", result.reverseFindings)
	}
	// This fixture drives the engine with dryRun=true: a rehearsal must never
	// report a confirmed action, forward or reverse — round-3 review fix,
	// pinned here because this fixture already produces a forward
	// would-unmonitor candidate ("Forward Candidate") that a regression
	// recording INTENDED rather than CONFIRMED writes would surface.
	if len(result.actions) != 0 {
		t.Fatalf("actions = %+v, want empty: dry-run must never record a confirmed action:\n%s", result.actions, buf.String())
	}

	// Store-level consequence: a previous cycle's real finding must survive
	// being folded in behind this skipped one.
	s := newStatsStore(false)
	prevAt := time.Now()
	s.recordInstance(cycleKindStartup, prevAt, "radarr-main", "radarr", cycleInstanceStats{
		total: 2, decisionsRan: true, reverseRan: true,
		reverseFindings: []reverseFinding{{ID: 99, Title: "Real finding from a trustworthy cycle", Reason: ReasonQualityCutoffNotMet}},
	})
	s.recordInstance(cycleKindSweep, prevAt.Add(time.Hour), "radarr-main", "radarr", result)

	got := s.snapshot().Instances[0]
	if len(got.ReverseFindings) != 1 || got.ReverseFindings[0].ID != 99 {
		t.Errorf("ReverseFindings after a skipped reverse pass = %+v, want the PREVIOUS cycle's real finding preserved, not cleared", got.ReverseFindings)
	}
}

// TestRunSonarrDecisionEngine_Stats_ReverseScanSkipped_ReverseRanStaysFalse
// is the Sonarr twin, reusing TestRunSonarrDecisionEngine_ReverseScan_
// IncompleteWantedSet_SkipsOnlyTheReversePass's fixture.
func TestRunSonarrDecisionEngine_Stats_ReverseScanSkipped_ReverseRanStaysFalse(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "," + episodeJSON(200, 2, 1, pastAirDate, 600) + "]"
	filesJSON := "[" + episodeFileJSON(500, 1, 200, false) + "," + episodeFileJSON(600, 2, 200, true) + "]"
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	fake.reverseWantedJSON = `{"page":1,"pageSize":100,"totalRecords":50,"records":[]}`

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	series := []seriesElement{testSeries(1, "Mixed Show", true, 1, []int{}, testSeason(1, true, 1, 1), testSeason(2, false, 1, 1))}

	result := runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{},
		"cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseScanOn(), fileReportOptions{})

	if result.reverseRan {
		t.Fatalf("reverseRan = true, want false: the reverse pass was skipped:\n%s", buf.String())
	}
	if !result.reverseSkipped {
		t.Errorf("reverseSkipped = false, want true: the pass DID attempt to run this cycle but could not be trusted")
	}
	if len(result.reverseFindings) != 0 {
		t.Errorf("reverseFindings = %+v, want empty/unset alongside reverseRan=false", result.reverseFindings)
	}
	// dryRun=true here too (see the Radarr twin's identical assertion) — this
	// fixture's "Mixed Show" already produces a forward would-unmonitor
	// candidate (season 1).
	if len(result.actions) != 0 {
		t.Fatalf("actions = %+v, want empty: dry-run must never record a confirmed action:\n%s", result.actions, buf.String())
	}

	s := newStatsStore(false)
	prevAt := time.Now()
	season := 3
	s.recordInstance(cycleKindStartup, prevAt, "sonarr-main", "sonarr", cycleInstanceStats{
		total: 2, decisionsRan: true, reverseRan: true,
		reverseFindings: []reverseFinding{{SeriesID: 1, Series: "Real finding", Season: &season, Reason: ReasonQualityCutoffNotMet}},
	})
	s.recordInstance(cycleKindSweep, prevAt.Add(time.Hour), "sonarr-main", "sonarr", result)

	got := s.snapshot().Instances[0]
	if len(got.ReverseFindings) != 1 || got.ReverseFindings[0].SeriesID != 1 {
		t.Errorf("ReverseFindings after a skipped reverse pass = %+v, want the PREVIOUS cycle's real finding preserved, not cleared", got.ReverseFindings)
	}
}

// TestRunRadarrDecisionEngine_Stats_AbortedCycle_PreservesPreviousWouldUnmonitorAndLastRun
// reuses TestRunRadarrDecisionEngine_ProfileFetchFailure_NoReportLinesAtAll's
// exact fixture (decision_test.go): a quality-profile fetch failure aborts
// the cycle at one of §2.6's warn-and-skip paths, before the evaluation loop
// that computes wouldUnmonitor ever runs.
func TestRunRadarrDecisionEngine_Stats_AbortedCycle_PreservesPreviousWouldUnmonitorAndLastRun(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/qualityprofile", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "key"}
	movies := []movieListElement{
		{ID: intPtr(1), Title: strPtr("Some Movie"), Monitored: boolPtr(true), HasFile: boolPtr(true), QualityProfileID: intPtr(1)},
	}

	result := runRadarrDecisionEngine(context.Background(), logger, inst, movies, map[int]bool{}, "cutoffarr-exclude",
		fullLibraryScope(slog.LevelInfo), true, reverseOptions{}, fileReportOptions{})

	if result.decisionsRan {
		t.Fatalf("decisionsRan = true, want false: the cycle aborted at the quality-profile fetch, before the evaluation loop ever ran:\n%s", buf.String())
	}
	if result.wouldUnmonitor != 0 {
		t.Errorf("wouldUnmonitor = %d, want 0 (the zero value; this aborted cycle never computed a real one)", result.wouldUnmonitor)
	}
	// total is trustworthy even on this path: it comes from the library
	// listing, already a complete read by the time this function was called.
	if result.total != 1 {
		t.Errorf("total = %d, want 1 (the library read succeeded; only the decision loop was aborted)", result.total)
	}

	s := newStatsStore(false)
	prevAt := time.Now()
	s.recordInstance(cycleKindStartup, prevAt, "radarr-main", "radarr", cycleInstanceStats{
		total: 1, monitored: 1, decisionsRan: true, wouldUnmonitor: 37,
	})
	s.recordInstance(cycleKindSweep, prevAt.Add(time.Hour), "radarr-main", "radarr", result)

	got := s.snapshot().Instances[0]
	if got.WouldUnmonitor != 37 {
		t.Errorf("WouldUnmonitor after an aborted cycle = %d, want the PREVIOUS real cycle's 37 preserved, not overwritten with 0", got.WouldUnmonitor)
	}
	if got.LastRun == nil || !got.LastRun.Equal(prevAt) {
		t.Errorf("LastRun after an aborted cycle = %v, want the PREVIOUS real cycle's timestamp %v preserved (an aborted cycle must not claim \"last swept just now\")", got.LastRun, prevAt)
	}
	if got.LastCycleKind == nil || *got.LastCycleKind != cycleKindStartup {
		t.Errorf("LastCycleKind after an aborted cycle = %v, want the PREVIOUS real cycle's %q preserved", got.LastCycleKind, cycleKindStartup)
	}
	// Total, unlike WouldUnmonitor, DOES update even on an aborted cycle:
	// recordInstance treats it unconditionally, since it comes from a
	// library read that already succeeded regardless of what happened after.
	if got.Total != 1 {
		t.Errorf("Total after an aborted cycle = %d, want 1 (this cycle's own library read, trustworthy even though the decision loop aborted)", got.Total)
	}
	// Round-4 review fix: an aborted cycle must NOT render as {status:"ok"}
	// just because it reached the engine — the connectivity/library-read
	// gates succeeded, but the evaluation loop itself never finished, and
	// that must be visible on THIS cycle's LastCycleStatus (not a stale "ok"
	// preserved from the earlier real cycle either — the status field is
	// never carried forward, unlike WouldUnmonitor/LastRun above).
	if got.LastCycleStatus.Status != cycleStatusSkipped {
		t.Errorf("LastCycleStatus.Status after an aborted cycle = %q, want %q (reaching the engine is not the same as completing an evaluation)", got.LastCycleStatus.Status, cycleStatusSkipped)
	}
	if got.LastCycleStatus.Reason == "" {
		t.Error("LastCycleStatus.Reason after an aborted cycle is empty; the operator has no idea the evaluation never completed")
	}
}

// TestRunSonarrDecisionEngine_Stats_AbortedCycle_PreservesPreviousWouldUnmonitorAndLastRun
// is the Sonarr twin, reusing TestRunSonarrDecisionEngine_
// ProfileFetchFailure_NoReportLinesAtAll's fixture.
func TestRunSonarrDecisionEngine_Stats_AbortedCycle_PreservesPreviousWouldUnmonitorAndLastRun(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/qualityprofile", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	series := []seriesElement{testSeries(1, "Some Show", true, 1, []int{}, testSeason(1, true, 1, 1))}

	result := runSonarrDecisionEngine(context.Background(), logger, inst, series, map[int]bool{}, map[seasonKey]bool{},
		"cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseOptions{}, fileReportOptions{})

	if result.decisionsRan {
		t.Fatalf("decisionsRan = true, want false: the cycle aborted at the quality-profile fetch, before the evaluation loop ever ran:\n%s", buf.String())
	}
	if result.wouldUnmonitor != 0 {
		t.Errorf("wouldUnmonitor = %d, want 0 (the zero value; this aborted cycle never computed a real one)", result.wouldUnmonitor)
	}
	if result.total != 1 {
		t.Errorf("total = %d, want 1 (the library read succeeded; only the decision loop was aborted)", result.total)
	}

	s := newStatsStore(false)
	prevAt := time.Now()
	s.recordInstance(cycleKindStartup, prevAt, "sonarr-main", "sonarr", cycleInstanceStats{
		total: 1, monitored: 1, decisionsRan: true, wouldUnmonitor: 12,
	})
	s.recordInstance(cycleKindSweep, prevAt.Add(time.Hour), "sonarr-main", "sonarr", result)

	got := s.snapshot().Instances[0]
	if got.WouldUnmonitor != 12 {
		t.Errorf("WouldUnmonitor after an aborted cycle = %d, want the PREVIOUS real cycle's 12 preserved, not overwritten with 0", got.WouldUnmonitor)
	}
	if got.LastRun == nil || !got.LastRun.Equal(prevAt) {
		t.Errorf("LastRun after an aborted cycle = %v, want the PREVIOUS real cycle's timestamp %v preserved", got.LastRun, prevAt)
	}
	if got.LastCycleKind == nil || *got.LastCycleKind != cycleKindStartup {
		t.Errorf("LastCycleKind after an aborted cycle = %v, want the PREVIOUS real cycle's %q preserved", got.LastCycleKind, cycleKindStartup)
	}
	// Round-4 review fix: the Sonarr twin of the same LastCycleStatus
	// assertion the Radarr test above pins.
	if got.LastCycleStatus.Status != cycleStatusSkipped {
		t.Errorf("LastCycleStatus.Status after an aborted cycle = %q, want %q (reaching the engine is not the same as completing an evaluation)", got.LastCycleStatus.Status, cycleStatusSkipped)
	}
	if got.LastCycleStatus.Reason == "" {
		t.Error("LastCycleStatus.Reason after an aborted cycle is empty; the operator has no idea the evaluation never completed")
	}
}
