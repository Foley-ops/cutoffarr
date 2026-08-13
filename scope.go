package main

import (
	"context"
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

	// seasons narrows a SONARR scope one level further: for the series id it
	// keys, only these season numbers may be reported and written. It is the
	// plan's own granularity — "a webhook event evaluates only that movie
	// (Radarr) or that series' AFFECTED SEASON (Sonarr)" — derived from the
	// payload's episodes[].seasonNumber (binding controller resolution 2).
	//
	// A series id ABSENT from this map, or present with an empty list, means
	// every eligible season of that series, which is both --only-id's meaning
	// and the mandated behavior for an event whose episodes array is absent or
	// empty. Nil for every Radarr scope, since a movie has no seasons.
	//
	// It narrows the WRITE and REPORT set only. What is EVALUATED is unchanged
	// and still whole-library: the cross-check validates the data the decisions
	// rest on, not the target.
	seasons map[int][]int

	// origin names what narrowed the scope, for the two messages that have to
	// tell a human why an item they named produced nothing.
	origin string

	// itemLevel is the level of the per-item report lines (msg=skip,
	// msg=would-unmonitor). Never applied to summaries, warnings, or write
	// lines.
	itemLevel slog.Level

	// noForwardWrites is [v2.2] the one field in this struct that anything
	// branches on beyond item membership, and it is the structural half of the
	// re-monitor action's constitution rule 2 ("performs the one operation the
	// button named").
	//
	// A GUI action runs the whole decision engine scoped to the item a human
	// clicked, because rule 6 mandates driving the EXISTING gated write path
	// rather than a second copy of it. But the engine has TWO write halves, and
	// the button named only one of them: a re-monitor click must drive the
	// reverse pass and nothing else. Left unset, the FORWARD write pass ran
	// first over that same scoped item, and on a stale tab whose item had since
	// been re-monitored and upgraded it PUT monitored:false on it — the exact
	// inverse of the operation the button offered, reported afterwards as
	// "there was nothing to do" (round-2 review, CRITICAL).
	//
	// So this suppresses the forward write pass — composing no write call at
	// all, the same way reverseOptions.remonitor false composes none in the
	// other direction — while leaving the forward EVALUATION, the cross-check
	// it feeds, and the reverse gate that consults that cross-check exactly as
	// they were. Nothing an action does may rest on less evidence than a sweep
	// does; it may only do less.
	//
	// Zero value false: every sweep, webhook, --once and startup path keeps the
	// forward write pass it has always had, without naming this field.
	noForwardWrites bool
}

// suppressesForwardWrites reports whether this scope forbids the forward write
// pass outright. See the field.
func (s evalScope) suppressesForwardWrites() bool { return s.noForwardWrites }

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
//
// seasons is the Sonarr-only second level of narrowing, keyed by series id (nil
// for Radarr, and nil for any series whose events never named a season). See
// the field's own comment, and containsSeason for what an absent entry means.
func webhookScope(ids []int, seasons map[int][]int) evalScope {
	s := evalScope{ids: dedupeSortedIDs(ids), origin: scopeOriginWebhook, itemLevel: slog.LevelDebug}
	for id, within := range seasons {
		if len(within) == 0 {
			continue
		}
		if s.seasons == nil {
			s.seasons = make(map[int][]int, len(seasons))
		}
		s.seasons[id] = dedupeSortedIDs(within)
	}
	return s
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

// containsSeason reports whether season of itemID is in scope. It is
// contains() plus the Sonarr-only second level, and it answers the question
// "may this SEASON be reported and written", which is the granularity the plan
// text sets for a Sonarr webhook.
//
// The two "absent means everything" rules compose, and both are widening:
// an INACTIVE scope contains every item and therefore every season, and an
// item with no season entry contains every season of that item. Narrowing only
// ever happens because an event positively named the seasons it touched.
func (s evalScope) containsSeason(itemID, season int) bool {
	if !s.contains(itemID) {
		return false
	}
	within := s.seasons[itemID]
	if len(within) == 0 {
		return true
	}
	return containsIntID(within, season)
}

// narrowsSeasons reports whether this scope names specific seasons for itemID,
// as opposed to admitting every season of it.
func (s evalScope) narrowsSeasons(itemID int) bool { return len(s.seasons[itemID]) > 0 }

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
	var attrs []any
	switch {
	case !s.active():
		return nil
	case len(s.ids) == 1:
		attrs = []any{"onlyId", s.ids[0]}
	default:
		attrs = []any{"scopeIds", s.String()}
	}
	if seasons := s.seasonScopeString(); seasons != "" {
		attrs = append(attrs, "scopeSeasons", seasons)
	}
	return attrs
}

// seasonScopeString renders the season narrowing as "1:2,3 7:all" — one entry
// per in-scope series, in id order — or "" when no series is narrowed at all.
//
// Every in-scope id appears once ANY of them is narrowed, including the ones
// that are not (as "all"). A line that listed only the narrowed series would
// leave a human to infer what the others meant, and "which seasons is this
// cycle allowed to write" is exactly the question the attribute exists to
// answer.
func (s evalScope) seasonScopeString() string {
	if len(s.seasons) == 0 {
		return ""
	}
	parts := make([]string, 0, len(s.ids))
	for _, id := range s.ids {
		within := "all"
		if s.narrowsSeasons(id) {
			within = joinInts(s.seasons[id])
		}
		parts = append(parts, strconv.Itoa(id)+":"+within)
	}
	return strings.Join(parts, " ")
}

// --- the idle-cycle noise budget -------------------------------------------

// demoteInfoTo returns a logger that emits INFO records at level instead, and
// leaves every other level exactly where it was. When level is LevelInfo — the
// startup scan, a --once run — it returns logger untouched, so the common path
// carries no wrapper at all.
//
// It exists because the daemon repeats the whole reporting pipeline forever
// (binding controller note 6). Almost every INFO line the read path emits is
// news exactly once: the *arr's version, its quality profiles, the library
// totals, the exclusion tag resolution, each cross-check sample. Repeated every
// reconciliation sweep and every webhook cycle, they bury the lines that are
// genuinely news — a write, a warning, an error, the summary that says the
// cycle happened at all.
//
// A LEVEL-REWRITING HANDLER rather than a level parameter threaded through
// twelve read helpers is a deliberate choice, and the rule it encodes is narrow
// enough to state in one sentence: on a repeating cycle, informational records
// are debug; nothing else moves. Warnings and errors are untouched — a daemon
// that quieted those would be hiding exactly what it exists to surface — and
// the lines that must stay at INFO on every cycle (the per-instance summaries,
// each confirmed write) are emitted through the UNWRAPPED logger by the engines
// themselves, which is why they are unaffected by anything here.
func demoteInfoTo(logger *slog.Logger, level slog.Level) *slog.Logger {
	if level == slog.LevelInfo {
		return logger
	}
	return slog.New(demotingHandler{inner: logger.Handler(), to: level})
}

// demotingHandler is demoteInfoTo's implementation.
type demotingHandler struct {
	inner slog.Handler
	to    slog.Level
}

// Enabled maps the INFO question onto the level INFO records will actually be
// emitted at. Without this, slog would build and hand over an INFO record that
// Handle then rewrites to DEBUG — meaning a demoted line would print at
// log_level=info, which is the one thing this whole mechanism exists to stop.
func (h demotingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if level == slog.LevelInfo {
		return h.inner.Enabled(ctx, h.to)
	}
	return h.inner.Enabled(ctx, level)
}

func (h demotingHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level == slog.LevelInfo {
		r.Level = h.to
	}
	return h.inner.Handle(ctx, r)
}

func (h demotingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return demotingHandler{inner: h.inner.WithAttrs(attrs), to: h.to}
}

func (h demotingHandler) WithGroup(name string) slog.Handler {
	return demotingHandler{inner: h.inner.WithGroup(name), to: h.to}
}

// scopeOriginGUIAction is [v2.2] the origin of a scope narrowed by a human
// clicking one finding's button. It joins --only-id and webhook in the same
// message-text-only role: nothing branches on it, it exists so the two
// messages that have to tell a human why the item they named produced nothing
// can say WHERE the narrowing came from. (What DOES branch for a GUI action is
// noForwardWrites, a field of its own, precisely so a reworded origin string
// can never change a write decision.)
const scopeOriginGUIAction = "gui action"

// actionScope is the scope one GUI re-monitor action runs under: exactly the
// item the button named, reported at INFO because a human is watching this one
// run — the same reasoning onlyIDScope uses, for the same situation — and with
// the forward write pass suppressed outright.
//
// season is nil for a Radarr movie and set for a Sonarr season, and it narrows
// the write set one level further exactly as a webhook's own season list does:
// clicking one season's button must never re-monitor that series' other
// seasons, even though the evaluation still reads the whole library (the
// full-evidence rule is unchanged — only reporting and writing are narrowed).
//
// noForwardWrites is set here and only here, and it is what makes a
// "Re-monitor" click structurally incapable of unmonitoring the item it names.
// See the field's own doc comment for the failure it closes.
func actionScope(id int, season *int) evalScope {
	s := evalScope{ids: []int{id}, origin: scopeOriginGUIAction, itemLevel: slog.LevelInfo, noForwardWrites: true}
	if season != nil {
		s.seasons = map[int][]int{id: {*season}}
	}
	return s
}
