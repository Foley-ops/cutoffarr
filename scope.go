package main

import (
	"log/slog"
	"sort"
	"strconv"
	"strings"
)

// scope.go generalizes what used to be a single `onlyID int` parameter into a
// small value that says three things about one instance evaluation: WHICH items
// of that instance's library the run may report and write, WHERE that narrowing
// came from (so the refusal messages can name it), and AT WHAT LEVEL the
// per-item report lines are logged.
//
// The generalization from one id to a SET is mandated by the Phase 8 webhook
// design (binding controller note 2). A webhook event triggers the same
// full-evidence evaluation the --only-id path performs — the whole library is
// still READ, because that is what the cross-check samples — with writes scoped
// to the triggering item. When several debounce keys for the SAME instance
// expire together (a season-pack import that also finished a second show), they
// coalesce into ONE evaluation whose write scope is the union of their ids,
// rather than N full library scans.
//
// The report LEVEL rides along for a related reason: the daemon repeats this
// cycle forever, and the per-item skip/would-unmonitor lines that are the point
// of a --once run are pure repetition on the hundredth idle reconciliation
// sweep. Startup and --once report at INFO; reconciliation and webhook cycles
// demote those lines to DEBUG and keep summaries, warnings, and write lines at
// INFO (binding controller note 6).

// Origins a scope can come from. They are log/message text, never control flow:
// nothing branches on them, so a reworded origin can never change a decision.
const (
	scopeOriginOnlyID  = "--only-id"
	scopeOriginWebhook = "webhook"
)

// evalScope is that value. Its ZERO value is meaningful and is the common case:
// no ids means the whole library, and slog.LevelInfo is itself the zero
// slog.Level, so evalScope{} is "report everything, at INFO" — exactly what
// a --once run with no flags does.
type evalScope struct {
	// ids are the item ids (radarr movie ids, sonarr series ids) this
	// evaluation may report and write. Empty means the whole library. Kept
	// sorted and deduplicated by the constructors so the log rendering is
	// deterministic regardless of the order events arrived in.
	ids []int

	// origin names what narrowed the scope, for the two messages that have to
	// tell a human why an item they named produced nothing.
	origin string

	// itemLevel is the level of the per-item report lines (msg=skip,
	// msg=would-unmonitor). Never applied to summaries, warnings, or write
	// lines.
	itemLevel slog.Level
}

// fullLibraryScope is an unnarrowed evaluation reporting at level.
func fullLibraryScope(level slog.Level) evalScope {
	return evalScope{itemLevel: level}
}

// onlyIDScope is the --once --only-id scope: exactly one named item, reported
// at INFO because a human is watching that one run.
func onlyIDScope(id int) evalScope {
	return evalScope{ids: []int{id}, origin: scopeOriginOnlyID, itemLevel: slog.LevelInfo}
}

// webhookScope is a debounced webhook cycle's scope: the coalesced set of ids
// whose timers expired for this instance, reported at DEBUG because these
// cycles fire unattended and repeatedly.
func webhookScope(ids []int) evalScope {
	return evalScope{ids: dedupeSortedIDs(ids), origin: scopeOriginWebhook, itemLevel: slog.LevelDebug}
}

// dedupeSortedIDs returns ids sorted ascending with duplicates removed, leaving
// the caller's slice untouched.
func dedupeSortedIDs(ids []int) []int {
	if len(ids) == 0 {
		return nil
	}
	sorted := append([]int(nil), ids...)
	sort.Ints(sorted)
	out := sorted[:1]
	for _, id := range sorted[1:] {
		if id != out[len(out)-1] {
			out = append(out, id)
		}
	}
	return out
}

// active reports whether the scope narrows anything at all.
func (s evalScope) active() bool { return len(s.ids) > 0 }

// contains reports whether id is in scope. An INACTIVE scope contains
// everything, which is what makes `!scope.active() || scope.contains(id)`
// unnecessary at call sites.
func (s evalScope) contains(id int) bool {
	if !s.active() {
		return true
	}
	return containsIntID(s.ids, id)
}

// missing returns the scoped ids that inLibrary says this instance's library
// does not have, in scope order. It is how both engines tell "the id names
// something this instance does not have" (a warning) apart from "the id names
// something the decision rules filtered out" (a different message entirely).
func (s evalScope) missing(inLibrary func(int) bool) []int {
	var out []int
	for _, id := range s.ids {
		if !inLibrary(id) {
			out = append(out, id)
		}
	}
	return out
}

// String renders the scoped ids for a log attribute: "42", or "42,77,103".
func (s evalScope) String() string {
	parts := make([]string, 0, len(s.ids))
	for _, id := range s.ids {
		parts = append(parts, strconv.Itoa(id))
	}
	return strings.Join(parts, ",")
}

// summaryAttrs are the log attributes naming this scope on a summary or
// refusal line, or nil when nothing is narrowed.
//
// A ONE-id scope renders as onlyId=N — the exact attribute every earlier phase
// printed, kept because it is a greppable token in this project's logs and
// because "onlyId" says precisely what a one-item scope means, whether the
// single id came from the flag or from a webhook. More than one renders as
// scopeIds="a,b,c", a shape that can only occur on a coalesced webhook cycle
// and that must not be mistaken for the flag.
func (s evalScope) summaryAttrs() []any {
	switch {
	case !s.active():
		return nil
	case len(s.ids) == 1:
		return []any{"onlyId", s.ids[0]}
	default:
		return []any{"scopeIds", s.String()}
	}
}
