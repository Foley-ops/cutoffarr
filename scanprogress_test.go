package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
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
