package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// scanprogress_test.go covers [v0.2.0]'s live scan progress: the mutex-guarded
// progress surface in the stats store, the coarse stages the daemon and both
// engines publish into it, and the two properties that keep it from ever
// becoming a second, weaker source of truth — it is write-only from the cycle's
// side, and no partial finding it describes ever reaches the data fields.

// progressRecorder collects every publish the store makes, in order, so a test
// can assert the SEQUENCE of stages a cycle goes through rather than only
// whichever one it happened to end on.
type progressRecorder struct {
	mu   sync.Mutex
	seen []scanProgressView
	by   map[string][]scanProgressView
}

func newProgressRecorder(s *statsStore) *progressRecorder {
	r := &progressRecorder{by: map[string][]scanProgressView{}}
	s.observeProgress = func(instance string, v scanProgressView) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.seen = append(r.seen, v)
		r.by[instance] = append(r.by[instance], v)
	}
	return r
}

// stages returns the distinct stage names in first-published order.
func (r *progressRecorder) stages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	seen := map[string]bool{}
	for _, v := range r.seen {
		if seen[v.Stage] {
			continue
		}
		seen[v.Stage] = true
		out = append(out, v.Stage)
	}
	return out
}

func (r *progressRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seen)
}

// viewsFor returns every publish made for one stage, in order, so a test can
// assert what a stage CLAIMED (its done/total) rather than only that it was
// announced at all.
func (r *progressRecorder) viewsFor(stage string) []scanProgressView {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []scanProgressView
	for _, v := range r.seen {
		if v.Stage == stage {
			out = append(out, v)
		}
	}
	return out
}

// --- the surface itself -----------------------------------------------------

// TestStatsStore_ScanProgress_WireSpelling pins the exact JSON the page reads
// literally, in TestWebUIHandler_Stats_CaseCollisionWireFieldsRawJSONSpelling's
// own style: every other test decodes the response back into the same Go struct
// it was encoded from, so a typo'd or `,omitempty`'d tag would round-trip fine
// and break only the dashboard.
func TestStatsStore_ScanProgress_WireSpelling(t *testing.T) {
	s := newStatsStore(true)
	s.beginScan(cycleKindSweep)
	p := s.progressFor("radarr-main")
	p.stage(scanStageEvaluating, 996)
	p.count(scanStageEvaluating, 100, 996)
	ts, _ := newTestWebUIServer(t, s)

	resp, err := http.Get(ts.URL + "/api/stats")
	if err != nil {
		t.Fatalf("GET /api/stats: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	for _, want := range []string{
		`"scan":{`, `"inProgress":true`, `"cycleKind":"sweep"`,
		`"instances":{"radarr-main":{"stage":"evaluating","done":100,"total":996}}`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("raw JSON is missing %s:\n%s", want, raw)
		}
	}
}

// TestStatsStore_ScanProgress_IdleBetweenCyclesIsAnHonestEmptySurface: between
// cycles the page must be able to tell "nothing is running" from "something is
// running and has not reported a stage yet", and an absent key can never say
// either. inProgress is always present, and instances is always an object.
func TestStatsStore_ScanProgress_IdleBetweenCyclesIsAnHonestEmptySurface(t *testing.T) {
	s := newStatsStore(true)
	ts, _ := newTestWebUIServer(t, s)

	resp, err := http.Get(ts.URL + "/api/stats")
	if err != nil {
		t.Fatalf("GET /api/stats: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	for _, want := range []string{`"scan":{`, `"inProgress":false`, `"instances":{}`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("an idle daemon's raw JSON is missing %s:\n%s", want, raw)
		}
	}
	if strings.Contains(string(raw), `"cycleKind"`) {
		t.Errorf("an idle daemon named a cycle kind; there is no cycle to name:\n%s", raw)
	}
}

// TestStatsStore_ScanProgress_ClearsWhenTheCycleEnds is the completion signal:
// the data snapshot has landed, so the strip must go away by itself rather than
// leaving a bar on screen that nothing will ever advance again.
func TestStatsStore_ScanProgress_ClearsWhenTheCycleEnds(t *testing.T) {
	s := newStatsStore(true)
	s.beginScan(cycleKindStartup)
	s.progressFor("radarr-main").stage(scanStageEvaluating, 10)

	s.endScan()

	scan := s.snapshot().Scan
	if scan.InProgress || scan.CycleKind != "" || len(scan.Instances) != 0 {
		t.Errorf("scan = %+v after the cycle ended, want an empty, not-in-progress surface", scan)
	}
}

// TestStatsStore_ScanProgress_ANewCycleNeverInheritsTheLastOnesStages: each
// cycle's surface is its own. An instance the previous cycle reached but this
// one has not yet must not still be showing the previous cycle's stage.
func TestStatsStore_ScanProgress_ANewCycleNeverInheritsTheLastOnesStages(t *testing.T) {
	s := newStatsStore(true)
	s.beginScan(cycleKindStartup)
	s.progressFor("radarr-main").stage(scanStageFileWalk, 0)
	s.endScan()

	s.beginScan(cycleKindSweep)
	s.progressFor("sonarr-main").stage(scanStageConnectivity, 0)

	scan := s.snapshot().Scan
	if _, stale := scan.Instances["radarr-main"]; stale {
		t.Errorf("scan.instances = %+v, want only the instance THIS cycle has reached", scan.Instances)
	}
	if scan.CycleKind != cycleKindSweep {
		t.Errorf("cycleKind = %q, want %q", scan.CycleKind, cycleKindSweep)
	}
}

// TestStatsStore_ScanProgress_CounterUpdatesAreBounded is the "never per-item
// lock churn" rule made observable. The call site may call count() per item —
// that is what keeps the engines readable — but the store must only be written
// to on a stride boundary and at the end, so a 100k-item library costs about a
// thousand lock acquisitions rather than a hundred thousand.
func TestStatsStore_ScanProgress_CounterUpdatesAreBounded(t *testing.T) {
	s := newStatsStore(true)
	rec := newProgressRecorder(s)
	s.beginScan(cycleKindSweep)
	p := s.progressFor("radarr-main")

	p.stage(scanStageEvaluating, 1000)
	for i := 1; i <= 150; i++ {
		p.count(scanStageEvaluating, i, 1000)
	}

	if got := s.snapshot().Scan.Instances["radarr-main"].Done; got != 100 {
		t.Errorf("done = %d after 150 items, want 100: the surface advances on stride boundaries, not per item", got)
	}
	// One stage transition + one stride boundary. Anything more is per-item
	// churn; anything less means the counter never moved at all.
	if n := rec.count(); n != 2 {
		t.Errorf("the store was written to %d times for 150 items, want 2 (the stage transition and one stride boundary)", n)
	}

	for i := 151; i <= 1000; i++ {
		p.count(scanStageEvaluating, i, 1000)
	}
	if got := s.snapshot().Scan.Instances["radarr-main"]; got.Done != 1000 || got.Total != 1000 {
		t.Errorf("done/total = %d/%d at the end of the pass, want 1000/1000: the LAST item must always publish, whatever the stride left over", got.Done, got.Total)
	}
}

// TestStatsStore_ScanProgress_AStageTransitionAlwaysPublishesImmediately: a
// stage is the coarse thing an operator reads, and holding one back until a
// stride boundary would leave "evaluating" on screen through the whole file
// walk.
func TestStatsStore_ScanProgress_AStageTransitionAlwaysPublishesImmediately(t *testing.T) {
	s := newStatsStore(true)
	s.beginScan(cycleKindSweep)
	p := s.progressFor("radarr-main")

	p.stage(scanStageEvaluating, 1000)
	p.count(scanStageEvaluating, 1, 1000)
	p.stage(scanStageCrossCheck, 0)

	got := s.snapshot().Scan.Instances["radarr-main"]
	if got.Stage != scanStageCrossCheck || got.Done != 0 || got.Total != 0 {
		t.Errorf("progress = %+v, want the new stage with its counters reset", got)
	}
}

// TestStatsStore_ScanProgress_ANilHandleIsANoOp: every call site that has no
// store — a --once run, and every engine test written before this feature
// existed — passes nothing, and publishing must be free and silent there rather
// than a nil dereference in the middle of a sweep.
func TestStatsStore_ScanProgress_ANilHandleIsANoOp(t *testing.T) {
	var p *scanProgress
	p.stage(scanStageEvaluating, 10)
	p.count(scanStageEvaluating, 1, 10)

	var s *statsStore
	if got := s.progressFor("radarr-main"); got != nil {
		t.Errorf("progressFor on a nil store = %+v, want nil", got)
	}
}

// TestStatsStore_ScanProgress_NeverCreatesAnInstanceInTheDataFields is the
// contract's hardest line: "NO partial findings ever appear in the data fields
// mid-cycle". Progress is a SEPARATE surface, and an instance that has only
// been progressed — not recorded — must not appear in `instances` at all, where
// it would render as a shelf card full of zeroes.
func TestStatsStore_ScanProgress_NeverCreatesAnInstanceInTheDataFields(t *testing.T) {
	s := newStatsStore(true)
	s.beginScan(cycleKindStartup)
	s.progressFor("radarr-main").stage(scanStageEvaluating, 996)
	s.progressFor("radarr-main").count(scanStageEvaluating, 100, 996)

	snap := s.snapshot()
	if len(snap.Instances) != 0 {
		t.Errorf("instances = %+v, want none: progress must never manufacture a data row", snap.Instances)
	}
	if len(snap.Scan.Instances) != 1 {
		t.Errorf("scan.instances = %+v, want the one instance being scanned", snap.Scan.Instances)
	}
}

// TestStatsStore_ScanProgress_SnapshotsAreIndependentOfTheLiveSurface is the
// aliasing guard every other part of this store already applies
// (cloneInstanceStatsView): the map handed to an HTTP handler must not be the
// one a running cycle keeps writing into, or the encoder and the cycle would
// race over it.
func TestStatsStore_ScanProgress_SnapshotsAreIndependentOfTheLiveSurface(t *testing.T) {
	s := newStatsStore(true)
	s.beginScan(cycleKindSweep)
	s.progressFor("radarr-main").stage(scanStageLibrary, 0)

	snap := s.snapshot()
	s.progressFor("radarr-main").stage(scanStageEvaluating, 10)
	s.progressFor("sonarr-main").stage(scanStageConnectivity, 0)

	if got := snap.Scan.Instances["radarr-main"].Stage; got != scanStageLibrary {
		t.Errorf("an already-taken snapshot changed under the caller: stage = %q, want %q", got, scanStageLibrary)
	}
	if _, leaked := snap.Scan.Instances["sonarr-main"]; leaked {
		t.Error("an instance added after the snapshot appeared inside it")
	}
}

// TestStatsStore_ScanProgress_ConcurrentPublishingAndPollingIsRaceFree is the
// -race gate for the one genuinely concurrent thing this feature adds: a cycle
// publishing progress on the daemon's loop goroutine while the HTTP handler
// serializes a snapshot on another.
func TestStatsStore_ScanProgress_ConcurrentPublishingAndPollingIsRaceFree(t *testing.T) {
	s := newStatsStore(true)
	done := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		s.beginScan(cycleKindSweep)
		p := s.progressFor("radarr-main")
		for i := 1; i <= 5000; i++ {
			p.count(scanStageEvaluating, i, 5000)
		}
		s.recordInstance(cycleKindSweep, time.Now(), "radarr-main", "radarr", cycleInstanceStats{total: 5000, decisionsRan: true})
		s.endScan()
		close(done)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			snap := s.snapshot()
			_, _ = json.Marshal(snap)
		}
	}()

	wg.Wait()
	if s.snapshot().Scan.InProgress {
		t.Error("the surface is still in progress after the cycle ended")
	}
}

// --- what the daemon and the engines actually publish ------------------------

// TestRunRadarrDecisionEngine_PublishesEveryStageItReaches walks one Radarr
// engine run — evaluation, cross-check, the write pass in write mode, the
// reverse scan with its own wanted-set fetch, and the file walk — and requires
// each stage to be announced, in order. Without this the vocabulary would be a
// list in a comment: a stage nothing publishes is a bar that never moves.
func TestRunRadarrDecisionEngine_PublishesEveryStageItReaches(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFiles(t, dir,
		filepath.Join("Movie A", "Movie A.mkv"),
		filepath.Join("Movie A", "Movie A (2).mkv"),
	)
	fake := newRadarrFake(t, "", nil)
	fake.reverseWantedJSON = `{"page":1,"pageSize":100,"totalRecords":1,"records":[{"id":7,"title":"Accidentally Unmonitored"}]}`
	inst := fake.instance()
	inst.MediaRootMap = map[string]string{"/movies": dir}
	movies := []movieListElement{
		crossCheckWitnessMovie(5, "Ordinary Monitored"),
		unmonitoredBelowCutoffMovie(7, "Accidentally Unmonitored"),
	}

	store := newStatsStore(false)
	rec := newProgressRecorder(store)
	store.beginScan(cycleKindSweep)
	scope := fullLibraryScope(slog.LevelInfo)
	scope.progress = store.progressFor(inst.Name)

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	runRadarrDecisionEngine(context.Background(), logger, inst, movies, map[int]bool{5: true}, "cutoffarr-exclude",
		scope, false, reverseOptions{enabled: true}, fileReportOptions{enabled: true})

	want := []string{scanStageEvaluating, scanStageCrossCheck, scanStageWriting, scanStageReverseScan, scanStageWantedSet, scanStageFileWalk}
	got := rec.stages()
	for _, stage := range want {
		if !containsString(got, stage) {
			t.Errorf("the engine never published stage %q; published: %v\n%s", stage, got, buf.String())
		}
	}
	if len(got) == 0 || got[0] != scanStageEvaluating {
		t.Errorf("published stages = %v, want the evaluation first", got)
	}
}

// TestRunSonarrDecisionEngine_PublishesEveryStageItReaches is the Sonarr twin.
// The two engines are separate implementations of the same shape, and a stage
// published by only one of them is a progress strip that stalls on half a
// deployment.
func TestRunSonarrDecisionEngine_PublishesEveryStageItReaches(t *testing.T) {
	episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "]"
	filesJSON := "[" + episodeFileJSON(500, 1, 200, false) + "]"
	fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
	series := []seriesElement{testSeries(1, "Show", true, 1, []int{}, testSeason(1, true, 1, 1))}

	store := newStatsStore(false)
	rec := newProgressRecorder(store)
	store.beginScan(cycleKindSweep)
	scope := fullLibraryScope(slog.LevelInfo)
	scope.progress = store.progressFor("sonarr-main")

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude",
		scope, false, reverseOptions{enabled: true}, fileReportOptions{})

	for _, stage := range []string{scanStageEvaluating, scanStageCrossCheck, scanStageWriting, scanStageReverseScan, scanStageWantedSet} {
		if !containsString(rec.stages(), stage) {
			t.Errorf("the sonarr engine never published stage %q; published: %v\n%s", stage, rec.stages(), buf.String())
		}
	}
}

// TestBothEngines_TheWritingStageNeverClaimsACountNothingWillAdvance is a
// round-2 review fix, and the rule it pins is the one the wanted-set stage was
// split out of the reverse pass to honor (reverse.go): a strip that sits on
// "0 / 340" through the whole of a multi-minute pass looks WEDGED, and looking
// wedged during the write pass — the one phase where the daemon is mutating the
// user's *arrs — is the worst place in the program to look wedged.
//
// `writing` announced len(reported) as its total and then nothing ever counted
// against it: runWritePass takes no scope and no progress handle, so there was
// no call site that could. total > 0 puts the page on its DETERMINATE branch
// (updateScanRow), which renders a 0%-wide bar and the literal text "0 / 340".
//
// Every other uncountable stage in this program declares total 0 and pulses
// honestly. This one now does too. The assertion is on the TOTAL rather than on
// the presence of the stage, because "the stage is published" was already true
// while the bar was lying.
func TestBothEngines_TheWritingStageNeverClaimsACountNothingWillAdvance(t *testing.T) {
	t.Run("radarr", func(t *testing.T) {
		fake := newRadarrFake(t, "", nil)
		movies := []movieListElement{
			crossCheckWitnessMovie(5, "Ordinary Monitored"),
			unmonitoredBelowCutoffMovie(7, "Accidentally Unmonitored"),
		}
		store := newStatsStore(false)
		rec := newProgressRecorder(store)
		store.beginScan(cycleKindSweep)
		scope := fullLibraryScope(slog.LevelInfo)
		scope.progress = store.progressFor("radarr-main")

		logger, buf := newDecisionTestLogger(slog.LevelInfo)
		runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{5: true}, "cutoffarr-exclude",
			scope, false, reverseOptions{}, fileReportOptions{})

		assertWritingStageIsHonestlyUncountable(t, rec, buf.String())
	})

	t.Run("sonarr", func(t *testing.T) {
		episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "]"
		filesJSON := "[" + episodeFileJSON(500, 1, 200, false) + "]"
		fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
		series := []seriesElement{testSeries(1, "Show", true, 1, []int{}, testSeason(1, true, 1, 1))}
		store := newStatsStore(false)
		rec := newProgressRecorder(store)
		store.beginScan(cycleKindSweep)
		scope := fullLibraryScope(slog.LevelInfo)
		scope.progress = store.progressFor("sonarr-main")

		logger, buf := newDecisionTestLogger(slog.LevelInfo)
		runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude",
			scope, false, reverseOptions{}, fileReportOptions{})

		assertWritingStageIsHonestlyUncountable(t, rec, buf.String())
	})
}

// assertWritingStageIsHonestlyUncountable is the shared half of the two cases
// above: the stage must be announced (write mode reached the pass) and every
// publish it makes must carry total 0, so the page pulses instead of showing a
// bar that cannot move.
func assertWritingStageIsHonestlyUncountable(t *testing.T, rec *progressRecorder, log string) {
	t.Helper()
	views := rec.viewsFor(scanStageWriting)
	if len(views) == 0 {
		t.Fatalf("the engine never published the writing stage at all, so this test proves nothing about what it claims:\n%s", log)
	}
	for _, v := range views {
		if v.Total != 0 || v.Done != 0 {
			t.Errorf("the writing stage published %+v: a total nothing ever counts against renders as a 0%%-wide bar reading \"0 / %d\" for the whole write pass, which is the 'looks wedged' failure this project splits stages to avoid. An uncountable stage declares total 0 and pulses", v, v.Total)
		}
	}
}

// TestBothEngines_ARehearsalNeverPublishesTheWritingStage is the mutation guard
// for the `if !dryRun` that wraps each of those two announcements. Without it,
// deleting either guard — or flipping it to `if dryRun` — leaves the whole
// suite green while a REHEARSING daemon's strip reads "writing", which is the
// one claim this project is most careful never to make falsely.
//
// The two positive assertions matter as much as the negative one: without them
// the test would pass just as well against an engine that published nothing at
// all (a nil handle, a scope wired up wrong, a store that dropped the publishes
// because no cycle was in progress).
func TestBothEngines_ARehearsalNeverPublishesTheWritingStage(t *testing.T) {
	t.Run("radarr", func(t *testing.T) {
		fake := newRadarrFake(t, "", nil)
		movies := []movieListElement{
			crossCheckWitnessMovie(5, "Ordinary Monitored"),
			unmonitoredBelowCutoffMovie(7, "Accidentally Unmonitored"),
		}
		store := newStatsStore(true)
		rec := newProgressRecorder(store)
		store.beginScan(cycleKindSweep)
		scope := fullLibraryScope(slog.LevelInfo)
		scope.progress = store.progressFor("radarr-main")

		logger, buf := newDecisionTestLogger(slog.LevelInfo)
		runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{5: true}, "cutoffarr-exclude",
			scope, true, reverseOptions{}, fileReportOptions{})

		assertRehearsalPublishesEverythingButWriting(t, rec, buf.String())
	})

	t.Run("sonarr", func(t *testing.T) {
		episodesJSON := "[" + episodeJSON(100, 1, 1, pastAirDate, 500) + "]"
		filesJSON := "[" + episodeFileJSON(500, 1, 200, false) + "]"
		fake := newSonarrEngineFake(t, episodesJSON, filesJSON)
		series := []seriesElement{testSeries(1, "Show", true, 1, []int{}, testSeason(1, true, 1, 1))}
		store := newStatsStore(true)
		rec := newProgressRecorder(store)
		store.beginScan(cycleKindSweep)
		scope := fullLibraryScope(slog.LevelInfo)
		scope.progress = store.progressFor("sonarr-main")

		logger, buf := newDecisionTestLogger(slog.LevelInfo)
		runSonarrDecisionEngine(context.Background(), logger, fake.instance(), series, map[int]bool{}, map[seasonKey]bool{}, "cutoffarr-exclude",
			scope, true, reverseOptions{}, fileReportOptions{})

		assertRehearsalPublishesEverythingButWriting(t, rec, buf.String())
	})
}

func assertRehearsalPublishesEverythingButWriting(t *testing.T, rec *progressRecorder, log string) {
	t.Helper()
	got := rec.stages()
	for _, stage := range []string{scanStageEvaluating, scanStageCrossCheck} {
		if !containsString(got, stage) {
			t.Fatalf("a rehearsal published no %q stage at all; published: %v — the negative assertion below would pass for the wrong reason\n%s", stage, got, log)
		}
	}
	if containsString(got, scanStageWriting) {
		t.Errorf("a DRY RUN published the %q stage; published: %v. A rehearsal composes no write at all (§2.1's gate sits immediately before each PUT), so a strip reading \"writing\" during one is a lie about the one thing this program is most careful never to lie about\n%s", scanStageWriting, got, log)
	}
}

// TestRunRadarrDecisionEngine_WithNoProgressHandleBehavesExactlyAsBefore is the
// compatibility pin for every call site that predates this feature (a --once
// run, and 122 engine tests): the zero-value scope carries no handle, and the
// engine must be indifferent to that.
func TestRunRadarrDecisionEngine_WithNoProgressHandleBehavesExactlyAsBefore(t *testing.T) {
	fake := newRadarrFake(t, "", nil)
	movies := []movieListElement{crossCheckWitnessMovie(5, "Ordinary Monitored")}

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	result := runRadarrDecisionEngine(context.Background(), logger, fake.instance(), movies, map[int]bool{5: true}, "cutoffarr-exclude",
		fullLibraryScope(slog.LevelInfo), true, reverseOptions{}, fileReportOptions{})

	if !result.decisionsRan || result.total != 1 {
		t.Errorf("result = %+v, want a completed evaluation of one movie:\n%s", result, buf.String())
	}
}

// TestDaemon_ScanProgress_IsVisibleWhileACycleRunsAndGoneWhenItEnds is the
// user-reported gap's other half, end to end: a sweep used to be entirely
// invisible from the page — the same numbers, unchanged, until it finished.
// Here the startup scan is held at its library read and the page can see
// exactly which instance is being scanned and what it is doing.
func TestDaemon_ScanProgress_IsVisibleWhileACycleRunsAndGoneWhenItEnds(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{wouldUnmonitorStatefulMovie(1, "Movie")})
	reached, release := holdTheLibraryRead(t, fake)
	h := startDaemon(t, writeDaemonConfig(t, "radarr", fake.srv.URL, true, "info", "0", "45s"))
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatalf("the startup scan never reached its library read:\n%s", h.out.String())
	}

	mid := getStats(t, h)
	if !mid.Scan.InProgress {
		t.Fatalf("scan.inProgress = false while the startup scan is held inside its library read: %+v", mid.Scan)
	}
	if mid.Scan.CycleKind != cycleKindStartup {
		t.Errorf("scan.cycleKind = %q, want %q", mid.Scan.CycleKind, cycleKindStartup)
	}
	p, ok := mid.Scan.Instances["radarr-main"]
	if !ok {
		t.Fatalf("scan.instances = %+v, want the instance being scanned", mid.Scan.Instances)
	}
	if p.Stage != scanStageLibrary {
		t.Errorf("stage = %q, want %q: the cycle is blocked inside GET /api/v3/movie", p.Stage, scanStageLibrary)
	}
	if len(mid.Instances) != 0 {
		t.Errorf("instances = %+v mid-cycle, want none: no partial finding may ever reach the data fields", mid.Instances)
	}

	release()
	h.waitReady()

	after := getStats(t, h)
	if after.Scan.InProgress || len(after.Scan.Instances) != 0 {
		t.Errorf("scan = %+v after the cycle completed, want cleared", after.Scan)
	}
	if len(after.Instances) != 1 {
		t.Errorf("instances = %+v, want the completed cycle's data", after.Instances)
	}
}

// --- the page ---------------------------------------------------------------

// pageFunctionBody returns the source of one top-level function in the embedded
// page's script, from its `function name(` to the next top-level `\n  function
// `, so a test can assert what a PARTICULAR function does rather than what the
// 2000-line page mentions somewhere.
func pageFunctionBody(t *testing.T, name string) string {
	t.Helper()
	page := string(webUIPage)
	start := strings.Index(page, "function "+name+"(")
	if start == -1 {
		t.Fatalf("the page has no %s function", name)
	}
	rest := page[start+1:]
	// The nearest following top-level boundary, so a function that happens to
	// be the LAST one in the script is bounded by the bootstrap lines rather
	// than by the whole rest of the file — a body that ran to the end of the
	// page would make every substring assertion below meaningless.
	end := -1
	for _, boundary := range []string{"\n  function ", "\n  document.addEventListener(", "\n  scanBtn.addEventListener(", "\n  refresh().then("} {
		if at := strings.Index(rest, boundary); at != -1 && (end == -1 || at < end) {
			end = at
		}
	}
	if end == -1 {
		t.Fatalf("could not find the end of %s (no following top-level statement)", name)
	}
	return rest[:end]
}

// TestWebUIPage_ScanStripRendersEveryInstancesStageAndProgress pins the strip
// itself: one row per instance the scan surface names, its stage in the page's
// own 11px letter-spaced label style, and a bar driven by done/total.
func TestWebUIPage_ScanStripRendersEveryInstancesStageAndProgress(t *testing.T) {
	page := string(webUIPage)
	for _, want := range []string{"scanStrip", "scan-strip", "scan-strip-stage", "scan-strip-bar", "scan-strip-fill"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page is missing %q: the live progress strip needs its own element and class names to be styleable and testable", want)
		}
	}

	// The two halves of the renderer: the one that decides WHICH rows exist,
	// and the one that fills a row in. Both are checked, because the property
	// is that the strip is driven by the scan surface and by nothing else.
	body := pageFunctionBody(t, "renderScanStrip") + pageFunctionBody(t, "updateScanRow")
	for _, want := range []string{"inProgress", "data.scan", "stage", "done", "total"} {
		if !strings.Contains(body, want) {
			t.Errorf("the strip renderer never reads %q from the scan surface:\n%s", want, body)
		}
	}
	// A determinate bar when there is something to count, an indeterminate
	// pulse when there is not: "0 of 0" and "0 of 996" are different statements
	// and must not render alike.
	if !strings.Contains(body, "total > 0") {
		t.Error("the strip renderer does not distinguish a countable stage from one with no total; an indeterminate stage must pulse rather than show a 0% bar")
	}
	if !strings.Contains(body, "scan-strip-indeterminate") {
		t.Error("the strip renderer never applies the indeterminate class for a stage with no total")
	}
}

// TestWebUIPage_ScanStripLivesOutsideTheShelvesSoDataIsNeverBlanked is the
// binding "scanning must never blank the page" rule, pinned structurally. The
// strip is a SIBLING of #instances, never a child: renderInstances clears
// #instances outright on its empty-state path, and a strip living inside it
// would be destroyed by the very poll that is meant to be showing progress.
func TestWebUIPage_ScanStripLivesOutsideTheShelvesSoDataIsNeverBlanked(t *testing.T) {
	page := string(webUIPage)
	stripAt := strings.Index(page, `id="scanStrip"`)
	mainOpen := strings.Index(page, `<main id="instances"`)
	mainClose := strings.Index(page, "</main>")
	if stripAt == -1 || mainOpen == -1 || mainClose == -1 {
		t.Fatal("the page is missing either the scan strip or the instances main element")
	}
	if stripAt > mainOpen && stripAt < mainClose {
		t.Error("the scan strip is inside <main id=\"instances\">, which renderInstances clears wholesale on its empty-state path — the strip must be a sibling so a running scan can never blank what is on screen")
	}
	if stripAt > mainOpen {
		t.Error("the scan strip renders below the shelves; the brief places the live progress strip above them")
	}

	// render() must not stop rendering data because a scan is running: the
	// three renderers are called unconditionally.
	body := pageFunctionBody(t, "render")
	for _, call := range []string{"renderInstances(instances)", "renderReverse(instances)", "renderFileReport(instances)", "renderScanStrip(data)"} {
		if !strings.Contains(body, call) {
			t.Errorf("render() no longer calls %s; the existing data must stay fully visible beneath a running scan:\n%s", call, body)
		}
	}
}

// TestWebUIPage_PollTightensWhileScanningAndReturnsAfterwards is the poll
// contract: 2s while a scan is running, 30s when it is not, and the
// document.hidden pause preserved through both.
func TestWebUIPage_PollTightensWhileScanningAndReturnsAfterwards(t *testing.T) {
	page := string(webUIPage)
	if !strings.Contains(page, "POLL_MS = 30000") {
		t.Error("the idle poll interval is no longer pinned at 30s")
	}
	if !strings.Contains(page, "POLL_SCANNING_MS = 2000") {
		t.Error("the page does not define a 2s poll interval for while a scan is in progress")
	}

	body := pageFunctionBody(t, "schedulePoll")
	if !strings.Contains(body, "document.hidden") {
		t.Errorf("schedulePoll no longer pauses on document.hidden:\n%s", body)
	}
	if !strings.Contains(body, "scanInProgress") {
		t.Errorf("schedulePoll does not consult the last snapshot's scan state to pick its interval:\n%s", body)
	}
	// Round-2 review fix: this used to assert only that both identifiers were
	// PRESENT, which is vocabulary rather than behavior. Inverting the ternary
	// to `? POLL_MS : POLL_SCANNING_MS` — every idle dashboard polling every 2s
	// forever, and a live scan updating once every 30s — passed it unchanged.
	// The MAPPING is the contract, so the mapping is what is pinned.
	if !strings.Contains(body, "? POLL_SCANNING_MS : POLL_MS") {
		t.Errorf("schedulePoll does not map the scanning state to the FAST interval and the idle state to the slow one (want the literal `? POLL_SCANNING_MS : POLL_MS`):\n%s", body)
	}
}

// jsBracedBlockAfter returns the `{ … }` block that follows header inside src,
// header included, by counting braces. It lets a test assert what ONE branch of
// a function does rather than what the whole function mentions somewhere.
func jsBracedBlockAfter(t *testing.T, src, header string) string {
	t.Helper()
	at := strings.Index(src, header)
	if at == -1 {
		t.Fatalf("no %q in:\n%s", header, src)
	}
	open := strings.Index(src[at:], "{")
	if open == -1 {
		t.Fatalf("%q is not followed by a block:\n%s", header, src)
	}
	depth := 0
	for i := at + open; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[at : i+1]
			}
		}
	}
	t.Fatalf("the block after %q is never closed:\n%s", header, src)
	return ""
}

// TestWebUIPage_ScanNowButtonReflectsTheScanSurfaceNotJustItsOwnPost is the
// user-visible half of the same fact: the button used to guess its state from
// the POST response alone, so a sweep started by the TIMER (or by another tab)
// left it reading "Scan now" while a scan was plainly running.
func TestWebUIPage_ScanNowButtonReflectsTheScanSurfaceNotJustItsOwnPost(t *testing.T) {
	body := pageFunctionBody(t, "renderScanButton")
	if !strings.Contains(body, "scanInProgress") {
		t.Errorf("renderScanButton does not read the scan surface:\n%s", body)
	}
	if !strings.Contains(body, "scanBtn.disabled") {
		t.Errorf("renderScanButton never sets the button's disabled state:\n%s", body)
	}
	if !strings.Contains(body, "Scan now") {
		t.Errorf("renderScanButton never restores the idle label:\n%s", body)
	}

	// Round-2 review fix, the same one schedulePoll's test needed: the three
	// assertions above are vocabulary. Swapping the branches — so a running
	// sweep leaves the button reading "Scan now" and enabled, which is EXACTLY
	// the bug Feature B item 4 exists to fix — passed all three unchanged. What
	// follows pins which state carries which label.
	scanning := jsBracedBlockAfter(t, body, "if (scanInProgress)")
	if !strings.Contains(scanning, "Scanning…") || !strings.Contains(scanning, "scanBtn.disabled = true") {
		t.Errorf("the scanInProgress branch is not the one that disables the button and calls it \"Scanning…\":\n%s", scanning)
	}
	if strings.Contains(scanning, "Scan now") {
		t.Errorf("the scanInProgress branch restores the idle label; a sweep the server says is running must never leave the button reading \"Scan now\":\n%s", scanning)
	}
	// The idle tail: whatever is left after both early-returning branches.
	queued := jsBracedBlockAfter(t, body, "if (scanQueuedRecently())")
	tail := body[strings.Index(body, queued)+len(queued):]
	if !strings.Contains(tail, "Scan now") || !strings.Contains(tail, "scanBtn.disabled = false") {
		t.Errorf("the idle path (neither scanning nor queued) does not re-enable the button and restore \"Scan now\":\n%s", tail)
	}

	// And the refresh path must route through it rather than resetting the
	// button behind its back — the bug being fixed.
	refresh := pageFunctionBody(t, "refresh")
	if !strings.Contains(refresh, "renderScanButton()") {
		t.Errorf("refresh() no longer routes the button's state through renderScanButton, so a timer-started sweep would leave it reading \"Scan now\":\n%s", refresh)
	}
}

// TestWebUIPage_ScanStripPulseIsDisabledUnderReducedMotion is the motion rule
// this feature owes. The existing reduced-motion test
// (TestWebUIPage_ReducedMotionDisablesEveryAnimatedProperty) covers every
// `transition:`; a keyframe ANIMATION is a second kind of motion it cannot see,
// and the indeterminate pulse is exactly that.
func TestWebUIPage_ScanStripPulseIsDisabledUnderReducedMotion(t *testing.T) {
	page := string(webUIPage)
	if !strings.Contains(page, "@keyframes") {
		t.Fatal("the page defines no keyframes; the indeterminate strip has no pulse to disable")
	}
	start := strings.Index(page, "@media (prefers-reduced-motion: reduce)")
	if start == -1 {
		t.Fatal("the page has no prefers-reduced-motion block")
	}
	block := page[start:]
	if end := strings.Index(block, "\n  }"); end != -1 {
		block = block[:end]
	}
	if !strings.Contains(block, "animation: none") {
		t.Errorf("the prefers-reduced-motion block never disables an animation, so the indeterminate pulse keeps running for a reader who asked for no motion:\n%s", block)
	}
	if !strings.Contains(block, "scan-strip-indeterminate") {
		t.Errorf("the prefers-reduced-motion block does not name the indeterminate strip's own selector:\n%s", block)
	}
}

// TestWebUIPage_TheReducedMotionBlockIsTheLastWordOnMotion is round-2's fix for
// the exact failure the review named: "the GUI pins assert source text, not
// behavior".
//
// Both reduced-motion tests above check that a SELECTOR is mentioned inside the
// media block. Neither can see the cascade — and the cascade was wrong. A media
// query adds NO specificity, `.scan-strip-fill` inside one has exactly the
// specificity of `.scan-strip-fill` outside one, and there is no `!important`
// anywhere in this page (asserted below, because that is what makes document
// order the deciding rule). So an override written ABOVE the rule it overrides
// is simply lost: a reader who asked for no motion still got the 1.4s infinite
// pulse, and — pre-existing, since round 3 — the disclosure caret's rotate too.
//
// The property that actually holds the behavior up is positional: the
// prefers-reduced-motion block must come AFTER every rule that turns motion on.
// This test asserts exactly that, over the whole stylesheet, so any future
// animated rule appended below the block fails here on the day it is written.
func TestWebUIPage_TheReducedMotionBlockIsTheLastWordOnMotion(t *testing.T) {
	page := string(webUIPage)
	styleStart := strings.Index(page, "<style>")
	styleEnd := strings.Index(page, "</style>")
	if styleStart == -1 || styleEnd == -1 || styleEnd < styleStart {
		t.Fatal("page has no <style>...</style> block")
	}
	// Comments are stripped first, for the reason webui_test.go's own parser
	// documents: this file's prose names `transition:` and `animation:` inside
	// CSS comments, and a substring scan that counts those is measuring the
	// commentary rather than the stylesheet.
	css := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(page[styleStart+len("<style>"):styleEnd], "")

	if strings.Contains(css, "!important") {
		t.Error("the stylesheet now uses !important; this test's positional argument (identical specificity, so document order decides) no longer describes how the reduced-motion block wins, and the block's placement must be re-reasoned rather than assumed")
	}

	const marker = "@media (prefers-reduced-motion: reduce)"
	at := strings.Index(css, marker)
	if at == -1 {
		t.Fatal("the page has no @media (prefers-reduced-motion: reduce) block")
	}
	if again := strings.Index(css[at+len(marker):], marker); again != -1 {
		t.Error("the page has more than one prefers-reduced-motion block; motion is turned off in exactly one place, and two of them make 'which one wins' a question again")
	}
	// The end of the block: brace depth back to zero from its own opening one.
	depth, end := 0, -1
	for i := at; i < len(css); i++ {
		switch css[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				end = i + 1
			}
		}
		if end != -1 {
			break
		}
	}
	if end == -1 {
		t.Fatal("the prefers-reduced-motion block is never closed; this test cannot locate what follows it")
	}

	// Vacuity guard: the assertion below is only meaningful because motion IS
	// declared somewhere above the block.
	before := css[:at]
	if !strings.Contains(before, "transition:") && !strings.Contains(before, "animation:") {
		t.Fatal("no motion at all is declared before the reduced-motion block; this test would pass against a stylesheet it never read correctly")
	}

	for _, line := range strings.Split(css[end:], "\n") {
		decl := strings.TrimSpace(line)
		for _, prop := range []string{"transition:", "animation:"} {
			if !strings.HasPrefix(decl, prop) || strings.HasPrefix(decl, prop+" none") {
				continue
			}
			t.Errorf("%q is declared AFTER the prefers-reduced-motion block, so it overrides the block's own %s none for a reader who asked for no motion (identical selector specificity, later rule wins). Move the block below every rule that turns motion on.", decl, prop)
		}
	}
}

// TestWebUIPage_ScanStripReusesItsRowsAcrossPolls is a self-review finding, and
// it is the same rule the shelf cards already live by
// (TestWebUIPage_ReusesShelfCardsAcrossRefreshesForTransitions): a CSS
// transition needs a prior computed value on an already-rendered element to
// animate from, and a keyframe animation restarts from frame one every time its
// element is replaced.
//
// The strip is redrawn every 2s while a scan runs. Rebuilt from scratch, its
// determinate bar would jump rather than glide and — worse — its indeterminate
// pulse would visibly stutter back to the start fifteen times a minute. So the
// per-instance row is built once, keyed by instance name, and updated in place.
func TestWebUIPage_ScanStripReusesItsRowsAcrossPolls(t *testing.T) {
	page := string(webUIPage)
	for _, want := range []string{"scanRows", "buildScanRow", "updateScanRow"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page is missing %q: the strip's rows must be built once and updated in place, not recreated on every 2s poll", want)
		}
	}

	body := pageFunctionBody(t, "renderScanStrip")
	if !strings.Contains(body, "scanRows[") {
		t.Errorf("renderScanStrip does not key its rows by instance name:\n%s", body)
	}
	if !strings.Contains(body, "updateScanRow(") {
		t.Errorf("renderScanStrip never updates an existing row in place:\n%s", body)
	}

	update := pageFunctionBody(t, "updateScanRow")
	// Switching a row from a countable stage to an uncountable one must clear
	// the inline width, or it would override the indeterminate class's own and
	// pin the pulse at whatever fraction the last countable stage ended on.
	if !strings.Contains(update, `els.fill.style.width = "";`) {
		t.Errorf("updateScanRow never clears the inline width when a stage stops being countable, so an indeterminate pulse would inherit the previous stage's fraction:\n%s", update)
	}
	if !strings.Contains(update, "scan-strip-indeterminate") || !strings.Contains(update, "total > 0") {
		t.Errorf("updateScanRow does not switch between the determinate bar and the indeterminate pulse:\n%s", update)
	}
}
