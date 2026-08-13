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

// fileReportFindingRecord is one duplicate or orphan finding, matching the
// API contract's `{kind, group, path, count?}`. Group is empty for an
// orphan (fileReportFinding never sets one for fileKindOrphan — see
// filereport.go); Count is the duplicate group's size and is omitted
// (rather than printed as a misleading 0) for an orphan, which has no group.
type fileReportFindingRecord struct {
	Kind  string `json:"kind"`
	Group string `json:"group,omitempty"`
	Path  string `json:"path"`
	Count int    `json:"count,omitempty"`
}

// fileReportSnapshot is one instance's Phase 11 file report, as of the last
// cycle that actually ran it (fileReportOptions.enabled — a full-library
// cycle only). Status is fileReportCounts.state()'s own three-way vocabulary
// (ran|skipped|off) verbatim, so the GUI and this struct can never drift
// from what logFileReportSummary already says about the same cycle.
type fileReportSnapshot struct {
	Status     string                    `json:"status"`
	Duplicates int                       `json:"duplicates"`
	Orphans    int                       `json:"orphans"`
	Findings   []fileReportFindingRecord `json:"findings"`
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

	ReverseFindings []reverseFinding   `json:"reverseFindings"`
	FileReport      fileReportSnapshot `json:"fileReport"`
	LastActions     []actionRecord     `json:"lastActions"`
}

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
		findings := make([]reverseFinding, len(cs.reverseFindings))
		copy(findings, cs.reverseFindings)
		v.ReverseFindings = findings
	}
	if cs.fileReportRan {
		findings := make([]fileReportFindingRecord, len(cs.fileReport.Findings))
		copy(findings, cs.fileReport.Findings)
		v.FileReport = fileReportSnapshot{
			Status:     cs.fileReport.Status,
			Duplicates: cs.fileReport.Duplicates,
			Orphans:    cs.fileReport.Orphans,
			Findings:   findings,
		}
	}

	if len(cs.actions) > 0 {
		// Newest first: prepend this cycle's actions (in the order they were
		// performed) ahead of whatever was already there, so the GUI table
		// needs no client-side sort to show the most recent action on top.
		merged := make([]actionRecord, 0, len(cs.actions)+len(v.LastActions))
		for _, a := range cs.actions {
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
		findings = append(findings, fileReportFindingRecord{
			Kind: f.kind, Group: f.group, Path: f.diskPath, Count: f.groupCount,
		})
	}
	return fileReportSnapshot{Status: c.state(), Duplicates: c.duplicates, Orphans: c.orphans, Findings: findings}
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
// LastCycleKind pointers (recordInstance always allocates a fresh one, but
// this function does not rely on that staying true).
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

	out.ReverseFindings = make([]reverseFinding, len(v.ReverseFindings))
	copy(out.ReverseFindings, v.ReverseFindings)

	out.LastActions = make([]actionRecord, len(v.LastActions))
	copy(out.LastActions, v.LastActions)

	out.FileReport.Findings = make([]fileReportFindingRecord, len(v.FileReport.Findings))
	copy(out.FileReport.Findings, v.FileReport.Findings)

	return out
}
