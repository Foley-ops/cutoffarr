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
	s := webhookScope([]int{77, 42, 77, 103, 42})
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
	attrs := webhookScope([]int{5}).summaryAttrs()
	if len(attrs) != 2 || attrs[0] != "onlyId" || attrs[1] != 5 {
		t.Errorf("summaryAttrs = %v, want [onlyId 5]", attrs)
	}
}

func TestEvalScope_Missing_NamesOnlyWhatTheLibraryLacks(t *testing.T) {
	s := webhookScope([]int{1, 2, 3})
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
