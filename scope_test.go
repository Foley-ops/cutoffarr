package main

import (
	"log/slog"
	"strings"
	"testing"
)

// TestEvalScope_ZeroValueIsTheWholeLibraryAtInfo pins the property every call
// site depends on: evalScope{} must mean what an unflagged --once run means.
// slog.LevelInfo is itself the zero slog.Level, which is what makes that true
// — and is exactly the kind of thing that silently stops being true.
func TestEvalScope_ZeroValueIsTheWholeLibraryAtInfo(t *testing.T) {
	var s evalScope
	if s.active() {
		t.Error("the zero scope must not narrow anything")
	}
	if !s.contains(1) || !s.contains(999999) {
		t.Error("an inactive scope contains every item")
	}
	if s.itemLevel != slog.LevelInfo {
		t.Errorf("itemLevel = %v, want %v", s.itemLevel, slog.LevelInfo)
	}
	if attrs := s.summaryAttrs(); attrs != nil {
		t.Errorf("summaryAttrs = %v, want nil: nothing was narrowed, so nothing should be claimed", attrs)
	}
}

func TestOnlyIDScope_NarrowsToOneItemAndRendersAsOnlyId(t *testing.T) {
	s := onlyIDScope(42)
	if !s.active() || !s.contains(42) || s.contains(43) {
		t.Errorf("onlyIDScope(42) must contain 42 and nothing else: %+v", s)
	}
	if s.itemLevel != slog.LevelInfo {
		t.Error("a --only-id run is watched by a human; its per-item lines stay at INFO")
	}
	attrs := s.summaryAttrs()
	if len(attrs) != 2 || attrs[0] != "onlyId" || attrs[1] != 42 {
		t.Errorf("summaryAttrs = %v, want [onlyId 42] — the greppable token every earlier phase printed", attrs)
	}
}

// TestWebhookScope_CoalescesDeduplicatesAndSorts is the mandated optimization's
// data half (binding controller note 2): several expired keys for the same
// instance become ONE evaluation whose write scope is their union.
func TestWebhookScope_CoalescesDeduplicatesAndSorts(t *testing.T) {
	s := webhookScope([]int{77, 42, 77, 103, 42}, nil)
	if got := s.String(); got != "42,77,103" {
		t.Errorf("String() = %q, want %q (sorted, deduplicated — the rendering must not depend on event arrival order)", got, "42,77,103")
	}
	for _, id := range []int{42, 77, 103} {
		if !s.contains(id) {
			t.Errorf("scope must contain %d", id)
		}
	}
	if s.contains(43) {
		t.Error("scope must not contain an id nothing asked for")
	}
	if s.itemLevel != slog.LevelDebug {
		t.Error("webhook cycles fire unattended and repeatedly; their per-item report lines are demoted to DEBUG")
	}
	attrs := s.summaryAttrs()
	if len(attrs) != 2 || attrs[0] != "scopeIds" || attrs[1] != "42,77,103" {
		t.Errorf("summaryAttrs = %v, want [scopeIds 42,77,103]: a multi-id scope must not be mistaken for the --only-id flag", attrs)
	}
}

// TestWebhookScope_SingleID_StillRendersAsOnlyId: the common webhook case is
// one item, and "onlyId" describes it exactly.
func TestWebhookScope_SingleID_StillRendersAsOnlyId(t *testing.T) {
	attrs := webhookScope([]int{5}, nil).summaryAttrs()
	if len(attrs) != 2 || attrs[0] != "onlyId" || attrs[1] != 5 {
		t.Errorf("summaryAttrs = %v, want [onlyId 5]", attrs)
	}
}

// --- the season narrowing ---------------------------------------------------
//
// The plan's granularity for a Sonarr webhook is "that series' AFFECTED
// SEASON", and every rule below is a widening one: narrowing happens only
// because an event positively named the seasons it touched. The alternative —
// treating an absent season list as "no seasons" — would silently stop writing
// anything at all, which is the failure mode this project is least able to see.

func TestEvalScope_ContainsSeason_AbsentNarrowingMeansEverySeason(t *testing.T) {
	// An inactive scope: every item, every season.
	var whole evalScope
	if !whole.containsSeason(1, 1) || !whole.containsSeason(999, 42) {
		t.Error("an inactive scope contains every season of every item")
	}
	if whole.narrowsSeasons(1) {
		t.Error("an inactive scope narrows no seasons")
	}

	// A scope naming a series but no seasons — --only-id, and the mandated
	// behavior for a payload whose episodes array was absent or empty.
	s := webhookScope([]int{7}, nil)
	if !s.containsSeason(7, 1) || !s.containsSeason(7, 99) {
		t.Error("a series in scope with no season narrowing contains every season of it")
	}
	if s.containsSeason(8, 1) {
		t.Error("a series OUT of scope contains no season, whatever the season narrowing says")
	}

	// An empty list is the same claim as no list at all, and must never be
	// read as "no seasons are in scope".
	empty := webhookScope([]int{7}, map[int][]int{7: {}})
	if !empty.containsSeason(7, 3) || empty.narrowsSeasons(7) {
		t.Error("an EMPTY season list must widen to every season, never collapse to none")
	}
}

func TestEvalScope_ContainsSeason_NarrowsToTheSeasonsTheEventNamed(t *testing.T) {
	s := webhookScope([]int{7, 9}, map[int][]int{7: {3, 2, 3}})

	if !s.containsSeason(7, 2) || !s.containsSeason(7, 3) {
		t.Error("the named seasons must be in scope")
	}
	if s.containsSeason(7, 1) || s.containsSeason(7, 4) {
		t.Error("a season the event did not name must NOT be in scope: an import of season 3 does not authorize a write to season 1")
	}
	// The other series of the same coalesced cycle is untouched by 7's
	// narrowing.
	if !s.containsSeason(9, 1) || !s.containsSeason(9, 77) {
		t.Error("one series' season narrowing must not narrow another series in the same cycle")
	}
	if !s.narrowsSeasons(7) || s.narrowsSeasons(9) {
		t.Error("narrowsSeasons must be per series")
	}
}

// TestEvalScope_SummaryAttrs_SaysWhichSeasonsACycleMayWrite: an unattended
// cycle whose per-item lines are demoted to DEBUG leaves the summary as the
// only INFO record describing it, so the summary is where the write scope has
// to be legible.
func TestEvalScope_SummaryAttrs_SaysWhichSeasonsACycleMayWrite(t *testing.T) {
	one := webhookScope([]int{7}, map[int][]int{7: {2, 3}}).summaryAttrs()
	if len(one) != 4 || one[0] != "onlyId" || one[1] != 7 || one[2] != "scopeSeasons" || one[3] != "7:2,3" {
		t.Errorf("summaryAttrs = %v, want [onlyId 7 scopeSeasons 7:2,3]", one)
	}

	// A coalesced cycle where only ONE of the two series was narrowed: both
	// appear, because "which seasons may this cycle write" must not be left to
	// inference.
	two := webhookScope([]int{9, 7}, map[int][]int{7: {2}}).summaryAttrs()
	if len(two) != 4 || two[0] != "scopeIds" || two[1] != "7,9" || two[2] != "scopeSeasons" || two[3] != "7:2 9:all" {
		t.Errorf("summaryAttrs = %v, want [scopeIds 7,9 scopeSeasons \"7:2 9:all\"]", two)
	}

	// No narrowing anywhere: the attribute is absent entirely rather than
	// present and empty, so a Radarr cycle's summary is byte-unchanged.
	if attrs := webhookScope([]int{7}, nil).summaryAttrs(); len(attrs) != 2 {
		t.Errorf("summaryAttrs = %v, want just [onlyId 7]: an unnarrowed scope claims no season scope", attrs)
	}
}

func TestEvalScope_Missing_NamesOnlyWhatTheLibraryLacks(t *testing.T) {
	s := webhookScope([]int{1, 2, 3}, nil)
	library := map[int]bool{1: true, 3: true}
	got := s.missing(func(id int) bool { return library[id] })
	if len(got) != 1 || got[0] != 2 {
		t.Errorf("missing = %v, want [2]", got)
	}
	if none := fullLibraryScope(slog.LevelInfo).missing(func(int) bool { return false }); none != nil {
		t.Errorf("an inactive scope names no missing ids, got %v", none)
	}
}

// --- the demoting handler ---------------------------------------------------

func TestDemoteInfoTo_LevelInfo_ReturnsTheLoggerUntouched(t *testing.T) {
	logger, _ := newDecisionTestLogger(slog.LevelInfo)
	if got := demoteInfoTo(logger, slog.LevelInfo); got != logger {
		t.Error("demoting INFO to INFO must hand the logger straight back: the common path carries no wrapper")
	}
}

// TestDemoteInfoTo_InfoRecordsBecomeDebug_AndNothingElseMoves is the whole
// contract. The demotion must be invisible to warnings and errors — a daemon
// that quieted those would be hiding exactly what it exists to surface.
func TestDemoteInfoTo_InfoRecordsBecomeDebug_AndNothingElseMoves(t *testing.T) {
	logger, buf := newDecisionTestLogger(slog.LevelDebug)
	demoted := demoteInfoTo(logger, slog.LevelDebug)

	demoted.Info("informational", "k", "v")
	demoted.Warn("a warning")
	demoted.Error("an error")
	demoted.Debug("already debug")

	out := buf.String()
	if !strings.Contains(out, "level=DEBUG msg=informational") {
		t.Errorf("an INFO record must be emitted at DEBUG:\n%s", out)
	}
	if !strings.Contains(out, "k=v") {
		t.Errorf("the record's attributes must survive the rewrite:\n%s", out)
	}
	if !strings.Contains(out, `level=WARN msg="a warning"`) {
		t.Errorf("a WARN must be untouched:\n%s", out)
	}
	if !strings.Contains(out, `level=ERROR msg="an error"`) {
		t.Errorf("an ERROR must be untouched:\n%s", out)
	}
}

// TestDemoteInfoTo_AtLogLevelInfo_TheDemotedRecordDisappears is the property
// that makes this a noise budget rather than a relabelling. slog asks the
// handler whether a level is enabled BEFORE building the record, so without the
// Enabled override a demoted line would still print at log_level=info — with a
// DEBUG label, which is worse than not demoting at all.
func TestDemoteInfoTo_AtLogLevelInfo_TheDemotedRecordDisappears(t *testing.T) {
	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	demoted := demoteInfoTo(logger, slog.LevelDebug)

	demoted.Info("should not appear")
	demoted.Warn("should appear")

	out := buf.String()
	if strings.Contains(out, "should not appear") {
		t.Errorf("a demoted record must be filtered out entirely at log_level=info:\n%s", out)
	}
	if !strings.Contains(out, "should appear") {
		t.Errorf("a WARN must still get through:\n%s", out)
	}
}

// TestDemoteInfoTo_WithAttrs_KeepsDemoting: slog.Logger.With returns a new
// logger from Handler.WithAttrs, and a handler that forgot to re-wrap there
// would silently stop demoting for every caller that used With.
func TestDemoteInfoTo_WithAttrs_KeepsDemoting(t *testing.T) {
	logger, buf := newDecisionTestLogger(slog.LevelDebug)
	demoted := demoteInfoTo(logger, slog.LevelDebug).With("instance", "radarr-main")

	demoted.Info("still demoted")

	out := buf.String()
	if !strings.Contains(out, "level=DEBUG msg=\"still demoted\"") || !strings.Contains(out, "instance=radarr-main") {
		t.Errorf("With() must preserve the demotion and the attribute:\n%s", out)
	}
}
