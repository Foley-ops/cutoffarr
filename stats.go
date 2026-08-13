package main

import (
	"sync"
	"time"
)

// stats.go is Phase 12's (v2c) in-memory capture of what every cycle already
// computes and logs, kept as DATA so GET /api/stats (webui.go) has something
// to serve. It changes no decision, no write, and — this is the load-bearing
// property, per the Phase 11 branch review's binding "Next phase inputs" —
// no existing log line: every value captured here is read from a return
// value or a struct field the read-path/write-path code already produced for
// its own logging, appended alongside that logging rather than replacing any
// of it (see reverseCounts.movieFindings/seasonFindings/actions,
// fileReportCounts.findings, and runWritePass/runSonarrWritePass's optional
// actions sink, each documented at its own definition).
//
// There is no database (plan §8: out of scope) and no persistence: a restart
// starts this store empty, exactly as the debounce queue (webhook.go) does,
// and for the same reason — everything it would have held is re-derived by
// the next cycle that touches each instance.

// maxLastActions bounds instanceStatsView.LastActions to the plan's own
// number ("last 50 action lines"): a long-running daemon that has unmonitored
// or re-monitored thousands of items over its lifetime must not grow this
// slice without bound, and 50 is enough for a human glancing at the GUI to
// see what just happened without it becoming a second log file.
const maxLastActions = 50

// Action names actionRecord.Action carries. Exported for the same reason the
// decision engines' Reason* constants are (decision.go): a string literal
// repeated at every call site is a typo waiting to desync the value webui.go
// and stats_test.go both compare against.
const (
	ActionUnmonitor = "unmonitor"
	ActionRemonitor = "remonitor"
)

// buildVersion is the version string GET /api/stats reports. It is a package
// var rather than a const so a future release process can set it at link
// time (`-ldflags "-X main.buildVersion=vX.Y.Z"`); nothing in this repo does
// that yet (see .github/workflows/release.yml's own header — it tags and
// pushes an image but never stamps a version string into the binary), so
// every binary built today honestly reports "dev" rather than a fabricated
// version number.
var buildVersion = "dev"

// actionRecord is one confirmed write this project made — the forward
// pass's "unmonitor" or the reverse pass's "remonitor" — kept for
// instanceStatsView.LastActions. Season is nil for a Radarr movie (movies
// have no seasons) and set for a Sonarr season.
//
// Time is left zero by every write-path call site that builds one: it is
// filled in by statsStore.recordInstance, uniformly, from the SAME clock
// value (cycle.now()) already used for LastRun — see that method's own
// comment for why a per-item timestamp is not worth threading four call
// stacks deep for a value that exists to be glanced at, not audited to the
// millisecond.
type actionRecord struct {
	Time   time.Time `json:"time"`
	Action string    `json:"action"`
	ID     int       `json:"id"`
	Title  string    `json:"title"`
	Season *int      `json:"season,omitempty"`
	Reason string    `json:"reason"`
}

// reverseFinding is one reverse-scan finding, radarr or sonarr shaped. Only
// one half of the ID/Title vs SeriesID/Series pair is ever populated for a
// given entry, matching the API contract's `{id|seriesId, title|series,
// season?, reason, seriesMonitored?}` — a client tells which shape it got by
// which pair is present (Season and SeriesMonitored being present at all is
// itself already sufficient: a Radarr finding never carries either).
type reverseFinding struct {
	ID              int    `json:"id,omitempty"`
	SeriesID        int    `json:"seriesId,omitempty"`
	Title           string `json:"title,omitempty"`
	Series          string `json:"series,omitempty"`
	Season          *int   `json:"season,omitempty"`
	Reason          string `json:"reason"`
	SeriesMonitored *bool  `json:"seriesMonitored,omitempty"`
}

// caseCollisionNameRecord is [v2.1] one colliding name within a
// case-collision fileReportFindingRecord, matching the API contract's
// `{name, tracked}` — see fileReportFindingRecord.Names' own doc comment.
type caseCollisionNameRecord struct {
	Name    string `json:"name"`
	Tracked bool   `json:"tracked"`
}

// fileReportFindingRecord is one duplicate, orphan, or [v2.1] case-collision
// finding, matching the API contract's `{kind, group, path, display, count?,
// entryType?, names?}`. Group is empty for an orphan or case-collision
// (fileReportFinding never sets one for either — see filereport.go); Count
// is the duplicate group's size and is omitted (rather than printed as a
// misleading 0) for anything else, which has no group. Path is the full
// cutoffarr-side (disk) path — the same one logFileReportFinding already
// logs, and for a case-collision the CONTAINING DIRECTORY the collision was
// found in, never one of the colliding entries itself — kept verbatim for a
// client that wants it (e.g. a hover title); Display is that same path
// relative to its mapped root (rootRelativeDisplayPath, filereport.go),
// always present and always shorter, for a client that wants something
// screen-sized instead of a possibly long, host-specific mount prefix.
//
// EntryType ("dir"|"file", or "mixed" for a [final review round] cross-type
// twin — a directory colliding with a file, e.g. Show/show) and Names are
// case-collision-only (binding controller resolution 2: "all colliding
// names, whether each name contains/IS a tracked path ..., and entry
// type"), both omitted for a duplicate or orphan the same way Group/Count
// are omitted for anything that isn't a duplicate.
type fileReportFindingRecord struct {
	Kind      string                    `json:"kind"`
	Group     string                    `json:"group,omitempty"`
	Path      string                    `json:"path"`
	Display   string                    `json:"display"`
	Count     int                       `json:"count,omitempty"`
	EntryType string                    `json:"entryType,omitempty"`
	Names     []caseCollisionNameRecord `json:"names,omitempty"`
}

// fileReportSnapshot is one instance's Phase 11 file report, as of the last
// cycle that actually ran it (fileReportOptions.enabled — a full-library
// cycle only). Status is fileReportCounts.state()'s own three-way vocabulary
// (ran|skipped|off) verbatim, so the GUI and this struct can never drift
// from what logFileReportSummary already says about the same cycle.
// CaseCollisions is [v2.1]'s addition, following the exact same identity/
// token disciplines as Duplicates/Orphans: always present (including 0)
// whenever Status is ran or skipped, and — like them — left untouched by a
// cycle that never ran the file report (recordInstance's three-state
// fidelity rule).
type fileReportSnapshot struct {
	Status         string                    `json:"status"`
	Duplicates     int                       `json:"duplicates"`
	Orphans        int                       `json:"orphans"`
	CaseCollisions int                       `json:"caseCollisions"`
	Findings       []fileReportFindingRecord `json:"findings"`
}

// cycleInstanceStats is what one call to runRadarrDecisionEngine or
// runSonarrDecisionEngine hands back about the cycle it just ran, for
// statsStore.recordInstance to fold in. It is the ENTIRE reason those two
// engines' signatures now end in a return type: every existing call site —
// daemon.go's three, main.go's one, and every Phase <=11 test's 122 — invokes
// them as a bare expression statement and therefore discards it, which Go
// permits without any change to the calling line at all (the same way
// fmt.Println's (int, error) return is routinely ignored). No test file
// needed to change for this specific addition.
//
// total/monitored/unmonitored/wouldUnmonitor are LIBRARY-WIDE, not narrowed
// to the cycle's scope. The read side of every cycle — even a webhook cycle
// scoped to one item — fetches the WHOLE library (full-evidence ruling,
// binding controller resolution 4), and both engines already run their
// decision function over every eligible item regardless of scope (only
// REPORTING and WRITING are scope-narrowed — see runRadarrDecisionEngine's
// own "reported" vs "decisions" split). So these four numbers describe the
// library as this cycle's own GET read it, refreshed by ANY cycle that
// reaches the engine — a webhook cycle included — not only a full-library
// one, which is exactly what makes the GUI's shelf bar a live, cheap-to-keep-
// current picture rather than something only a nightly sweep updates.
type cycleInstanceStats struct {
	total, monitored, unmonitored, wouldUnmonitor int

	// decisionsRan is true only once the evaluation loop that computes
	// wouldUnmonitor has run to completion — set at the same place in both
	// engines that wouldUnmonitor itself is (see each engine's own comment
	// at that line). §2.6's warn-and-skip paths (a scope miss, a quality
	// profile fetch failure, an exclusion-tag resolution failure) and the
	// shutdown-mid-evaluation boundary all bare-`return` BEFORE that point,
	// so on any of those cycles this stays false and wouldUnmonitor stays at
	// its zero value — a number this cycle never actually computed, not a
	// real "nothing would be unmonitored". recordInstance must leave the
	// PREVIOUS cycle's WouldUnmonitor (and LastRun/LastCycleKind, so a dead
	// cycle cannot look like "last swept just now" either) untouched when
	// this is false, the same never-overwrite-with-nothing-happened rule
	// reverseRan/fileReportRan already apply below. total/monitored/
	// unmonitored are NOT gated by this: they are read from the library
	// listing itself, which this function's own doc comment establishes is
	// already complete and trustworthy the instant it is in hand, before any
	// early return.
	decisionsRan bool

	// reverseRan/fileReportRan mirror this cycle's own reverse.enabled /
	// fileReport.enabled (scheduling, not per-instance config) AND — for
	// reverseRan — whether the pass, having run, could actually be trusted:
	// a webhook or --only-id cycle runs neither, and an unmonitored
	// wanted/cutoff set that could not be fetched completely (or a shutdown
	// mid-evaluation) leaves the reverse pass's own reverseCounts.skipped
	// true with an empty findings slice (reverse.go). Either way,
	// recordInstance must leave the PREVIOUS cycle's findings/fileReport
	// exactly as they were rather than overwrite them with "nothing
	// happened" — the three-state fidelity the Phase 11 branch review's
	// binding notes require (a pass that did not run, or ran but could not
	// be trusted, this cycle must never render as a pass that ran and found
	// nothing).
	reverseRan      bool
	reverseFindings []reverseFinding

	// reverseSkipped mirrors reverseRan's sibling half: true when this cycle
	// DID attempt the reverse pass (reverse.enabled) but the pass itself
	// could not be trusted (reverseCounts.skipped — an incomplete
	// unmonitored wanted/cutoff set, or a shutdown mid-evaluation), so
	// reverseFindings above was never populated. reverseRan and
	// reverseSkipped are never both true; a cycle that never scheduled the
	// reverse pass at all (a webhook cycle, or the feature globally off)
	// leaves both false, which recordInstance reads as "say nothing new" —
	// see instanceStatsView.ReverseStatus's own comment for why this third
	// case matters as much as the other two.
	reverseSkipped bool

	fileReportRan bool
	fileReport    fileReportSnapshot

	// actions is every write this cycle actually confirmed, forward
	// (unmonitor) and reverse (remonitor) alike, in the order performed. Both
	// are empty on a dry-run cycle: writeMovieMonitored/writeSeasonMonitored
	// return written=false at the §2.1 gate, so runWritePass/
	// runSonarrWritePass's success branch — the only place either sink is
	// appended to — is never reached, and reverseCounts.record's default
	// case is the same story for the reverse direction. A rehearsal is never
	// reported as an action taken.
	actions []actionRecord
}

// instanceStatsView is one instance's entry in GET /api/stats's `instances`
// array, matching the API contract field-for-field. It is also the type
// statsStore stores internally (behind s.mu), so recordInstance mutates it in
// place and snapshot() must never hand back the live pointer or a value that
// shares a slice with it (see cloneInstanceStatsView).
type instanceStatsView struct {
	Name           string `json:"name"`
	Type           string `json:"type"`
	Total          int    `json:"total"`
	Monitored      int    `json:"monitored"`
	Unmonitored    int    `json:"unmonitored"`
	WouldUnmonitor int    `json:"wouldUnmonitor"`

	// LastRun/LastCycleKind are pointers so the API contract's "RFC3339 or
	// null" / "startup|sweep|webhook|once|null" is representable literally.
	// Both stay nil until the FIRST cycle whose cycleInstanceStats.decisionsRan
	// is true — an entry can be created by a cycle that aborted before ever
	// completing an evaluation (e.g. a quality-profile fetch failure on this
	// instance's very first cycle), and such a cycle never ran a decision to
	// report the time of, so recordInstance leaves both nil rather than
	// stamping a "last run" that produced no WouldUnmonitor at all.
	LastRun       *time.Time `json:"lastRun"`
	LastCycleKind *string    `json:"lastCycleKind"`

	// ReverseStatus is the reverse pass's own ran|skipped|off vocabulary —
	// fileReport.Status's exact twin, added for the same reason (Phase 12
	// review round 3): reverseFindings being empty means three different
	// things ("never ran", "ran but could not be trusted", "ran and found
	// nothing"), and only this field lets a client (the GUI, a script) tell
	// them apart instead of rendering a pass that never completed as a clean
	// all-clear. "off" is the default at instance creation and stays that
	// way until the first full cycle actually attempts the pass — a webhook
	// cycle (which never schedules reverse at all) leaves it exactly as the
	// last full cycle set it, never reverting to "off" in between, mirroring
	// FileReport.Status's own persistence rule below.
	ReverseStatus string `json:"reverseStatus"`

	// ReverseAsOf is the clock reading (cycle.now()) of the most recent
	// cycle whose reverse pass actually completed trustworthily — the
	// timestamp of the findings ReverseFindings is CURRENTLY holding, not of
	// "now" or of the most recent cycle generally. It stays nil until the
	// first cycle that sets ReverseStatus to "ran", and — the reason it
	// exists (controller ruling, Phase 12 final round: "the skipped-reverse-
	// pass overwrite is the highest-severity item ... last-known-good
	// preservation + a staleness indicator") — it is left UNTOUCHED on a
	// cycle that sets ReverseStatus to "skipped", exactly like
	// ReverseFindings itself: last-known-good preservation without a
	// timestamp still leaves a human unable to tell "found nothing five
	// minutes ago" from "found nothing five weeks ago", so the GUI (round-4
	// review fix) uses this to render "showing last complete sweep from
	// <time>" whenever it displays preserved-but-now-unconfirmed findings.
	ReverseAsOf *time.Time `json:"reverseAsOf"`

	ReverseFindings []reverseFinding   `json:"reverseFindings"`
	FileReport      fileReportSnapshot `json:"fileReport"`
	LastActions     []actionRecord     `json:"lastActions"`

	// LastCycleStatus is this instance's outcome on the MOST RECENT cycle
	// that named it at all — including a cycle that never reached the
	// decision engine because §2.6's connectivity-gate or library-read
	// warn-and-skip path fired (statsStore.recordUnreachable), AND
	// (round-4 review fix) a cycle that DID reach the engine but aborted
	// inside it, before the evaluation loop that computes wouldUnmonitor
	// ever ran to completion (a quality-profile fetch failure, an
	// exclusion-tag resolution failure — cycleInstanceStats.decisionsRan
	// stays false on both). Unlike every other field on this struct, it is
	// NEVER carried forward from a previous cycle when this one fails: a
	// daemon that has been unable to complete an evaluation for a week must
	// show that fact on THIS cycle, not silently keep repeating "ok" from
	// the last time it could — see statsStore.recordUnreachable and
	// recordInstance's own unconditional write to this field, gated only on
	// WHICH value to write (ok vs skipped), never on whether to write at
	// all.
	LastCycleStatus cycleStatusView `json:"lastCycleStatus"`
}

// cycleStatusView is LastCycleStatus's shape: whether the most recent cycle
// that named this instance actually completed an evaluation. Status is "ok"
// (checkInstanceConnectivity succeeded, the library read succeeded, AND the
// engine's own evaluation loop ran to completion — cs.decisionsRan) or
// "skipped" (any one of those three gates failed; Reason names which).
// Reason is empty for "ok".
type cycleStatusView struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

const (
	cycleStatusOK      = "ok"
	cycleStatusSkipped = "skipped"

	// abortedEvaluationReason is recordInstance's own LastCycleStatus.Reason
	// (round-4 review fix) for the third gate — distinct from
	// unreachableReasonConnectivity/unreachableReasonLibraryRead (daemon.go),
	// which name the two gates BEFORE the engine is ever called: this cycle
	// DID reach the decision engine (connectivity and the library read both
	// succeeded), but the engine itself bare-returned from one of §2.6's
	// warn-and-skip paths (a quality-profile fetch failure, an
	// exclusion-tag resolution failure) before its evaluation loop finished.
	abortedEvaluationReason = "the cycle reached this instance but aborted before completing an evaluation"
)

// statsResponse is GET /api/stats's whole body.
type statsResponse struct {
	Instances []instanceStatsView `json:"instances"`
	DryRun    bool                `json:"dryRun"`
	Version   string              `json:"version"`
}

// statsStore is the mutex-guarded in-memory state GET /api/stats serves from.
// It is updated at the end of every cycle (daemon.go's runScanCycle) and read
// by the HTTP handler (webui.go); neither side ever blocks the other for
// longer than a struct copy, and neither blocks a running cycle — the whole
// point of copying out a snapshot rather than serializing straight from the
// live map.
type statsStore struct {
	mu     sync.Mutex
	dryRun bool

	// order is insertion order — the order runScanCycle first successfully
	// captured each instance in, which on an ordinary daemon start is
	// cfg.Instances' own order (the startup scan is always the first cycle to
	// run and it covers every configured instance). It exists because Go map
	// iteration order is randomized and the JSON array's element order should
	// not be, for the same reason every other listing in this project sorts
	// or preserves config order rather than handing back whatever the runtime
	// felt like.
	order  []string
	byName map[string]*instanceStatsView
}

// newStatsStore creates an empty store. dryRun is fixed for the process's
// lifetime (cfg.DryRun after --dry-run's force-on override, exactly the value
// every cycle already carries) and is never learned from a cycle, so it is
// set once here rather than threaded through recordInstance.
func newStatsStore(dryRun bool) *statsStore {
	return &statsStore{dryRun: dryRun, byName: make(map[string]*instanceStatsView)}
}

// recordInstance folds one cycle's result for one instance into the store.
// kind is the cycle's own vocabulary (startup|sweep|webhook|once) — see
// scanCycle.kind's doc comment (daemon.go) for why a manual "Scan now" cycle
// reports itself as "sweep" rather than adding a fifth value the API contract
// never named. at is the cycle's own clock reading (cycle.now()), used
// uniformly as both LastRun and every action's Time: threading a
// finer-grained timestamp four call stacks into the write paths would buy
// precision nothing here reads to the second, since this is a value a human
// glances at ("2m ago"), not one anything in this project's decision or
// cross-check logic ever consults.
func (s *statsStore) recordInstance(kind string, at time.Time, name, typ string, cs cycleInstanceStats) {
	s.mu.Lock()
	defer s.mu.Unlock()

	v, ok := s.byName[name]
	if !ok {
		v = &instanceStatsView{
			Name:            name,
			Type:            typ,
			ReverseStatus:   "off",
			ReverseFindings: []reverseFinding{},
			FileReport:      fileReportSnapshot{Status: "off", Findings: []fileReportFindingRecord{}},
			LastActions:     []actionRecord{},
		}
		s.byName[name] = v
		s.order = append(s.order, name)
	}

	v.Type = typ
	v.Total = cs.total
	v.Monitored = cs.monitored
	v.Unmonitored = cs.unmonitored

	// This cycle reached the decision engine at all (runScanCycle only calls
	// recordInstance from inside its dataOK branch) — a strictly stronger
	// statement than the connectivity/library-read gates recordUnreachable
	// exists for — but reaching the engine is not the same as the engine
	// having actually COMPLETED an evaluation: §2.6's warn-and-skip paths
	// INSIDE the engine (a quality-profile fetch failure, an exclusion-tag
	// resolution failure) bare-return before cs.decisionsRan is ever set
	// (round-4 review fix). Before this gate, such a cycle was recorded as
	// {status:"ok"} unconditionally — indistinguishable on the dashboard
	// from a cycle that actually swept the library, with no badge and no
	// per-card timestamp to betray it, the same false-all-clear class
	// already closed for reverseRan/fileReportRan/WouldUnmonitor. Either
	// branch always overwrites whatever LastCycleStatus previously said
	// (unlike every other field below): this cycle's own outcome, good or
	// bad, is never stale.
	if cs.decisionsRan {
		v.LastCycleStatus = cycleStatusView{Status: cycleStatusOK}
	} else {
		v.LastCycleStatus = cycleStatusView{Status: cycleStatusSkipped, Reason: abortedEvaluationReason}
	}

	// decisionsRan gates WouldUnmonitor and LastRun/LastCycleKind together
	// (see cycleInstanceStats.decisionsRan's own comment): a cycle that
	// aborted before the evaluation loop finished must not overwrite the
	// last real WouldUnmonitor with the zero value it never actually
	// computed, and must not claim "last swept just now" for a sweep that
	// never produced a decision. total/monitored/unmonitored above are
	// unconditional because they come from the library read, which already
	// succeeded (daemon.go only calls the engine after dataOK) regardless of
	// what happened after.
	if cs.decisionsRan {
		v.WouldUnmonitor = cs.wouldUnmonitor
		atCopy := at
		v.LastRun = &atCopy
		kindCopy := kind
		v.LastCycleKind = &kindCopy
	}

	// Three-state fidelity (see cycleInstanceStats.reverseRan's own comment):
	// a cycle that did not run the reverse pass leaves the previous findings
	// untouched rather than overwriting them with an empty slice, which would
	// misreport "did not look" as "looked, found nothing".
	if cs.reverseRan {
		v.ReverseStatus = "ran"
		findings := make([]reverseFinding, len(cs.reverseFindings))
		copy(findings, cs.reverseFindings)
		v.ReverseFindings = findings
		// ReverseAsOf tracks the findings' own freshness, not the cycle's:
		// stamped here, alongside them, and left alone everywhere else (see
		// ReverseAsOf's own doc comment) — the controller-ruling staleness
		// indicator needs the LAST time these findings were confirmed, not
		// the last time any cycle merely attempted the pass.
		atCopy := at
		v.ReverseAsOf = &atCopy
	} else if cs.reverseSkipped {
		// The pass ran but could not be trusted this cycle: say so, but —
		// same three-state fidelity rule as the findings themselves — leave
		// the PREVIOUS trustworthy findings in place rather than clearing
		// them, since this cycle produced no new evidence either way.
		v.ReverseStatus = "skipped"
	}
	if cs.fileReportRan {
		findings := make([]fileReportFindingRecord, len(cs.fileReport.Findings))
		copy(findings, cs.fileReport.Findings)
		v.FileReport = fileReportSnapshot{
			Status:         cs.fileReport.Status,
			Duplicates:     cs.fileReport.Duplicates,
			Orphans:        cs.fileReport.Orphans,
			CaseCollisions: cs.fileReport.CaseCollisions,
			Findings:       findings,
		}
	}

	if len(cs.actions) > 0 {
		// Newest first: prepend this cycle's actions ahead of whatever was
		// already there, so the GUI table needs no client-side sort to show
		// the most recent action on top. cs.actions is in PERFORMED order
		// (oldest first) — walking it in REVERSE (round-4 review fix) is
		// what makes the newest action of THIS cycle land at merged[0], not
		// just the newest action across cycles. Without the reverse walk, a
		// single cycle producing more than maxLastActions actions (a live
		// sweep's first pass over a mature library, unmonitoring hundreds of
		// movies at once) would have `merged[:maxLastActions]` below keep
		// that cycle's FIRST maxLastActions actions and silently discard its
		// most recent ones — the exact opposite of "last 50 action lines".
		merged := make([]actionRecord, 0, len(cs.actions)+len(v.LastActions))
		for i := len(cs.actions) - 1; i >= 0; i-- {
			a := cs.actions[i]
			a.Time = at
			merged = append(merged, a)
		}
		merged = append(merged, v.LastActions...)
		if len(merged) > maxLastActions {
			merged = merged[:maxLastActions]
		}
		v.LastActions = merged
	}
}

// recordUnreachable folds in the OTHER outcome runScanCycle's per-instance
// loop can have: this cycle never even reached the decision engine for name,
// because checkInstanceConnectivity or the library read (inspectRadarrLibrary
// / inspectSonarrLibrary) hit one of §2.6's warn-and-skip paths — a
// quality-profile fetch failure, an unreachable *arr mid-restart, a malformed
// response, and so on.
//
// It creates name's entry if this is the very first cycle that has ever
// named it, which is the whole point: before this method existed, an
// instance that had NEVER once been reached was simply absent from
// `instances` — indistinguishable from "not configured in this daemon at
// all" — and an instance that had been reached before just kept showing
// last cycle's numbers with no marker that anything was wrong. Either way
// the log's own WARN was the only place the failure was visible, and the
// GUI's entire job is "glance and trust these numbers" without reading the
// log.
//
// It touches ONLY LastCycleStatus — never Total/Monitored/Unmonitored/
// WouldUnmonitor/LastRun/LastCycleKind/ReverseFindings/FileReport/
// LastActions, all of which stay exactly what the last cycle that actually
// reached the engine left them as (or their zero value, if none ever has):
// this cycle produced no new evidence about any of those, and overwriting a
// trusted number with a guess would be worse than leaving it stale and
// flagged.
func (s *statsStore) recordUnreachable(name, typ, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	v, ok := s.byName[name]
	if !ok {
		v = &instanceStatsView{
			Name:            name,
			Type:            typ,
			ReverseStatus:   "off",
			ReverseFindings: []reverseFinding{},
			FileReport:      fileReportSnapshot{Status: "off", Findings: []fileReportFindingRecord{}},
			LastActions:     []actionRecord{},
		}
		s.byName[name] = v
		s.order = append(s.order, name)
	}
	v.Type = typ
	v.LastCycleStatus = cycleStatusView{Status: cycleStatusSkipped, Reason: reason}
}

// snapshot copies out the current state for JSON serialization. It is safe to
// call while a cycle is running: it holds the lock only long enough to copy,
// never for the duration of a request, and every slice/pointer in the result
// is independent of the store's own — see cloneInstanceStatsView.
func (s *statsStore) snapshot() statsResponse {
	s.mu.Lock()
	defer s.mu.Unlock()

	resp := statsResponse{DryRun: s.dryRun, Version: buildVersion, Instances: []instanceStatsView{}}
	for _, name := range s.order {
		resp.Instances = append(resp.Instances, cloneInstanceStatsView(*s.byName[name]))
	}
	return resp
}

// fileReportSnapshotFrom converts one cycle's fileReportCounts (filereport.go
// — the struct runRadarrFileReport/runSonarrFileReport already return and
// logFileReportSummary already logs from) into the JSON-facing shape GET
// /api/stats serves. state() is the same three-way vocabulary
// logFileReportSummary itself renders, so the two can never say different
// things about the same cycle.
func fileReportSnapshotFrom(c fileReportCounts) fileReportSnapshot {
	findings := make([]fileReportFindingRecord, 0, len(c.findings))
	for _, f := range c.findings {
		rec := fileReportFindingRecord{
			Kind: f.kind, Group: f.group, Path: f.diskPath, Display: f.displayPath, Count: f.groupCount,
		}
		if f.kind == fileKindCaseCollision {
			// [v2.1] entryType/names are case-collision-only — see
			// fileReportFindingRecord's own doc comment. Group/Count above
			// are already f.group/f.groupCount's zero values for this kind
			// (classifyFileReportPath/caseCollisionsInDir never set either
			// on a case-collision fileReportFinding), so nothing needs
			// clearing here.
			rec.EntryType = f.entryType
			rec.Names = make([]caseCollisionNameRecord, len(f.names))
			for i, n := range f.names {
				rec.Names[i] = caseCollisionNameRecord{Name: n.name, Tracked: n.tracked}
			}
		}
		findings = append(findings, rec)
	}
	return fileReportSnapshot{
		Status: c.state(), Duplicates: c.duplicates, Orphans: c.orphans, CaseCollisions: c.caseCollisions,
		Findings: findings,
	}
}

// radarrReverseFindings converts a Radarr reverse pass's findings
// (reverseCounts.movieFindings) into the API's shared reverseFinding shape.
func radarrReverseFindings(ds []movieDecision) []reverseFinding {
	out := make([]reverseFinding, 0, len(ds))
	for _, d := range ds {
		out = append(out, reverseFinding{ID: d.id, Title: d.title, Reason: d.reason})
	}
	return out
}

// sonarrReverseFindings converts a Sonarr reverse pass's findings
// (reverseCounts.seasonFindings) into the API's shared reverseFinding shape.
func sonarrReverseFindings(fs []reverseSeasonFinding) []reverseFinding {
	out := make([]reverseFinding, 0, len(fs))
	for _, f := range fs {
		season := f.decision.season
		monitored := f.seriesMonitored
		out = append(out, reverseFinding{
			SeriesID: f.decision.seriesID, Series: f.decision.series, Season: &season,
			Reason: f.decision.reason, SeriesMonitored: &monitored,
		})
	}
	return out
}

// cloneInstanceStatsView deep-copies everything reference-typed in v, so a
// snapshot handed to an HTTP handler cannot be mutated by a later
// recordInstance call — not through slice aliasing (append growing v's own
// slice in place would otherwise be invisible here but could still corrupt a
// snapshot that shared the same backing array) and not through the LastRun/
// LastCycleKind/ReverseAsOf pointers (recordInstance always allocates a
// fresh one, but this function does not rely on that staying true).
func cloneInstanceStatsView(v instanceStatsView) instanceStatsView {
	out := v

	if v.LastRun != nil {
		t := *v.LastRun
		out.LastRun = &t
	}
	if v.LastCycleKind != nil {
		k := *v.LastCycleKind
		out.LastCycleKind = &k
	}
	if v.ReverseAsOf != nil {
		t := *v.ReverseAsOf
		out.ReverseAsOf = &t
	}

	out.ReverseFindings = make([]reverseFinding, len(v.ReverseFindings))
	copy(out.ReverseFindings, v.ReverseFindings)

	out.LastActions = make([]actionRecord, len(v.LastActions))
	copy(out.LastActions, v.LastActions)

	out.FileReport.Findings = make([]fileReportFindingRecord, len(v.FileReport.Findings))
	copy(out.FileReport.Findings, v.FileReport.Findings)
	// [v2.1] The copy above is a shallow, per-element struct copy: each
	// fileReportFindingRecord's own Names slice still shares its backing
	// array with v's (and therefore the store's) until each element is
	// given its own — see TestCloneInstanceStatsView_DeepCopiesCaseCollisionNames.
	for i, f := range out.FileReport.Findings {
		if len(f.Names) == 0 {
			continue
		}
		names := make([]caseCollisionNameRecord, len(f.Names))
		copy(names, f.Names)
		out.FileReport.Findings[i].Names = names
	}

	return out
}
