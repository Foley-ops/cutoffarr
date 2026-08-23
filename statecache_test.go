package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// statecache_test.go covers [v0.2.0]'s warm-start display cache: the file
// written at the end of every full cycle (statecache.go), the loader that
// reads it back at startup, the stats store's warm-start entry point, and —
// the half the feature's whole safety argument rests on — the structural pins
// that keep that file out of every decision, evaluation and action path.
//
// Every filesystem fixture is a t.TempDir(). Nothing here writes anywhere a
// real deployment would, and no test in this file ever contacts a real *arr.

// --- fixtures ---------------------------------------------------------------

// warmStatsSnapshot is one recorded cycle's worth of stats, as statsStore
// would hand it to the cache writer.
func warmStatsSnapshot(t *testing.T, name, typ string, at time.Time) statsResponse {
	t.Helper()
	s := newStatsStore(true)
	s.recordInstance(cycleKindSweep, at, name, typ, cycleInstanceStats{
		total: 10, monitored: 6, unmonitored: 4, wouldUnmonitor: 2, decisionsRan: true,
		reverseRan: true,
		reverseFindings: []reverseFinding{
			{ID: 7, Title: "Wrongly Unmonitored", Reason: ReasonQualityCutoffNotMet},
		},
		fileReportRan: true,
		fileReport: fileReportSnapshot{
			Status: "ran", Duplicates: 1, ReclaimableBytes: 4096,
			Findings: []fileReportFindingRecord{
				{Kind: fileKindDuplicate, Path: "/movies/A/dup.mkv", Display: "A/dup.mkv", Count: 2, Size: 4096},
			},
		},
	})
	return s.snapshot()
}

// readStateCacheFile reads the cache file back as generic JSON, so a test can
// assert the WIRE spelling rather than whatever a Go struct round-trip would
// forgive.
func readStateCacheFile(t *testing.T, dir string) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, stateCacheFileName))
	if err != nil {
		t.Fatalf("reading the state cache: %v", err)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the state cache is not valid JSON: %v\n%s", err, raw)
	}
	return out
}

// dirEntryNames lists a directory's entries by name, for the "no litter left
// behind" assertions.
func dirEntryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// --- the writer -------------------------------------------------------------

// TestWriteStateCache_WritesTheWireSnapshotWithASchemaVersionAndAWriteTime is
// the whole write contract: the /api/stats body verbatim, plus the two fields
// that make a file on disk safe to trust later — the schema version the loader
// refuses to guess about, and the time the sweep behind it actually ran.
func TestWriteStateCache_WritesTheWireSnapshotWithASchemaVersionAndAWriteTime(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 8, 22, 9, 30, 0, 0, time.UTC)
	logger, buf := newActionTestLogger()

	writeStateCache(logger, dir, warmStatsSnapshot(t, "radarr-main", "radarr", at), at)

	doc := readStateCacheFile(t, dir)
	if got := string(doc["schemaVersion"]); got != "1" {
		t.Errorf("schemaVersion = %s, want 1", got)
	}
	if got := string(doc["writtenAt"]); got != `"2026-08-22T09:30:00Z"` {
		t.Errorf("writtenAt = %s, want the RFC3339 clock reading it was written with", got)
	}
	if _, ok := doc["instances"]; !ok {
		t.Fatalf("the cache carries no instances key at all: %v", doc)
	}
	var instances []instanceStatsView
	if err := json.Unmarshal(doc["instances"], &instances); err != nil {
		t.Fatalf("instances is not the /api/stats wire shape: %v", err)
	}
	if len(instances) != 1 || instances[0].Name != "radarr-main" || instances[0].Total != 10 {
		t.Fatalf("instances = %+v, want the recorded radarr-main snapshot", instances)
	}
	if len(instances[0].ReverseFindings) != 1 || len(instances[0].FileReport.Findings) != 1 {
		t.Errorf("the cached instance lost its findings: %+v", instances[0])
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("a successful cache write warned about something:\n%s", buf)
	}
}

// TestWriteStateCache_LeavesNoTemporaryFileBehind is the atomic-write
// contract's visible half: the cache appears under its final name, and the
// temp file it was written through does not survive the rename.
func TestWriteStateCache_LeavesNoTemporaryFileBehind(t *testing.T) {
	dir := t.TempDir()
	at := time.Now()
	logger, _ := newActionTestLogger()

	writeStateCache(logger, dir, warmStatsSnapshot(t, "radarr-main", "radarr", at), at)

	names := dirEntryNames(t, dir)
	if len(names) != 1 || names[0] != stateCacheFileName {
		t.Errorf("directory contains %v, want exactly [%s]: the write must go through a temp file and rename, leaving nothing else behind", names, stateCacheFileName)
	}
}

// TestWriteStateCache_ReusesOneTempNameSoAFailingDiskCannotAccumulateLitter is
// the reason the temp file has a FIXED name rather than a unique one. os.Remove
// is banned tree-wide, so nothing here can ever clean up after itself: a
// unique-per-write temp name would leave one orphan per failed write, forever,
// on exactly the failing disk that is least able to afford it. A fixed name is
// overwritten by the next attempt, so the litter is bounded at one file.
func TestWriteStateCache_ReusesOneTempNameSoAFailingDiskCannotAccumulateLitter(t *testing.T) {
	dir := t.TempDir()
	stray := filepath.Join(dir, stateCacheTempFileName)
	if err := os.WriteFile(stray, []byte("half a cache from a process that died mid-write"), 0o600); err != nil {
		t.Fatalf("planting a stray temp file: %v", err)
	}
	at := time.Now()
	logger, _ := newActionTestLogger()

	writeStateCache(logger, dir, warmStatsSnapshot(t, "radarr-main", "radarr", at), at)

	names := dirEntryNames(t, dir)
	if len(names) != 1 || names[0] != stateCacheFileName {
		t.Errorf("directory contains %v, want exactly [%s]: a stray temp file from an earlier crash must be reused by name, never left beside a second one", names, stateCacheFileName)
	}
	if _, _, ok := loadStateCache(logger, dir); !ok {
		t.Error("the cache written over a stray temp file is not loadable")
	}
}

// TestWriteStateCache_AFailedWriteWarnsAndContinues is the rule that keeps this
// feature display-only in the strongest sense: a cache that cannot be written
// is a WARN and nothing else. It never fails a cycle, never panics, never
// retries.
func TestWriteStateCache_AFailedWriteWarnsAndContinues(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-config-dir")
	at := time.Now()
	logger, buf := newActionTestLogger()

	writeStateCache(logger, missing, warmStatsSnapshot(t, "radarr-main", "radarr", at), at)

	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("a cache write into a directory that does not exist said nothing:\n%s", buf)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("the failed write created %s; it must never create the directory it was pointed at", missing)
	}
}

// TestWriteStateCache_WithNoConfigDirectoryWritesNothingAtAll is the guard for
// every Config built in memory rather than loaded from a file (every test that
// constructs one, and any future embedding): a program that does not know where
// its config lives must not guess a directory to write into.
func TestWriteStateCache_WithNoConfigDirectoryWritesNothingAtAll(t *testing.T) {
	logger, buf := newActionTestLogger()
	at := time.Now()

	writeStateCache(logger, "", warmStatsSnapshot(t, "radarr-main", "radarr", at), at)

	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("an unconfigured cache directory warned; it is a normal state, not a fault:\n%s", buf)
	}
}

// TestStateCachePaths_StayInsideTheConfigDirectory is the containment pin the
// filesystem-mutation audit amendment owes: both paths this file can ever write
// are the config directory's own children, by construction — never a media
// root, never anywhere a media path could steer them.
func TestStateCachePaths_StayInsideTheConfigDirectory(t *testing.T) {
	for _, dir := range []string{"/config", "/etc/cutoffarr", "/config/"} {
		final, tmp, ok := stateCachePaths(dir)
		if !ok {
			t.Fatalf("stateCachePaths(%q) refused a real directory", dir)
		}
		clean := filepath.Clean(dir)
		for _, p := range []string{final, tmp} {
			if filepath.Dir(p) != clean {
				t.Errorf("stateCachePaths(%q) produced %q, which is not a direct child of the config directory", dir, p)
			}
		}
		if final == tmp {
			t.Errorf("stateCachePaths(%q) returned the same path twice (%q); the rename would be a no-op over the file it is meant to replace", dir, final)
		}
	}
	if _, _, ok := stateCachePaths(""); ok {
		t.Error("stateCachePaths(\"\") produced a path; with no config directory known there is nowhere to write and nothing to read")
	}
	for _, name := range []string{stateCacheFileName, stateCacheTempFileName} {
		if strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
			t.Errorf("state cache file name %q contains a path separator; the containment above is only structural while these are bare names", name)
		}
	}
}

// --- the loader -------------------------------------------------------------

// TestLoadStateCache_RoundTripsACompletedSweep is the whole point of the file:
// what one process wrote at the end of a cycle is what the next process reads
// back before its first one.
func TestLoadStateCache_RoundTripsACompletedSweep(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 8, 22, 9, 30, 0, 0, time.UTC)
	logger, buf := newActionTestLogger()
	writeStateCache(logger, dir, warmStatsSnapshot(t, "sonarr-main", "sonarr", at), at)

	instances, writtenAt, ok := loadStateCache(logger, dir)
	if !ok {
		t.Fatalf("loadStateCache refused a cache it had just written:\n%s", buf)
	}
	if !writtenAt.Equal(at) {
		t.Errorf("writtenAt = %s, want %s", writtenAt, at)
	}
	if len(instances) != 1 {
		t.Fatalf("instances = %+v, want exactly the one that was written", instances)
	}
	got := instances[0]
	if got.Name != "sonarr-main" || got.Type != "sonarr" || got.Total != 10 || got.WouldUnmonitor != 2 {
		t.Errorf("loaded instance = %+v, want the written numbers verbatim", got)
	}
	if len(got.ReverseFindings) != 1 || got.ReverseFindings[0].ID != 7 {
		t.Errorf("loaded reverse findings = %+v, want the one that was written", got.ReverseFindings)
	}
	if got.FileReport.Status != "ran" || len(got.FileReport.Findings) != 1 || got.FileReport.Findings[0].Size != 4096 {
		t.Errorf("loaded file report = %+v, want the one that was written", got.FileReport)
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("a clean round trip warned about something:\n%s", buf)
	}
}

// TestLoadStateCache_EveryInvalidShapeColdStartsAndSaysWhy is the load
// contract's other half, and the one that keeps a bad file from ever reaching
// the dashboard: invalid in ANY way means ignore it, say so, and start exactly
// as cold as every version before this one did.
func TestLoadStateCache_EveryInvalidShapeColdStartsAndSaysWhy(t *testing.T) {
	valid := `{"schemaVersion":1,"writtenAt":"2026-08-22T09:30:00Z","instances":[{"name":"radarr-main","type":"radarr"}]}`
	cases := []struct {
		name, body string
	}{
		{"empty file", ``},
		{"not json at all", `this is a config file, not a cache`},
		{"truncated json", `{"schemaVersion":1,"instances":[{"name":`},
		{"json but not an object", `[1,2,3]`},
		{"no schema version", `{"writtenAt":"2026-08-22T09:30:00Z","instances":[]}`},
		{"a schema version from the future", `{"schemaVersion":99,"writtenAt":"2026-08-22T09:30:00Z","instances":[]}`},
		{"a schema version from before this one", `{"schemaVersion":0,"writtenAt":"2026-08-22T09:30:00Z","instances":[]}`},
		{"no written-at", `{"schemaVersion":1,"instances":[]}`},
		{"a null written-at", `{"schemaVersion":1,"writtenAt":null,"instances":[]}`},
		{"a zero written-at", `{"schemaVersion":1,"writtenAt":"0001-01-01T00:00:00Z","instances":[]}`},
		{"no instances key", `{"schemaVersion":1,"writtenAt":"2026-08-22T09:30:00Z"}`},
		{"a null instances key", `{"schemaVersion":1,"writtenAt":"2026-08-22T09:30:00Z","instances":null}`},
		{"an instance with no name", `{"schemaVersion":1,"writtenAt":"2026-08-22T09:30:00Z","instances":[{"type":"radarr"}]}`},
		{"an instance with an empty name", `{"schemaVersion":1,"writtenAt":"2026-08-22T09:30:00Z","instances":[{"name":"","type":"radarr"}]}`},
		{"an instance with no type", `{"schemaVersion":1,"writtenAt":"2026-08-22T09:30:00Z","instances":[{"name":"radarr-main"}]}`},
		{"the same instance twice", `{"schemaVersion":1,"writtenAt":"2026-08-22T09:30:00Z","instances":[{"name":"a","type":"radarr"},{"name":"a","type":"radarr"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, stateCacheFileName), []byte(tc.body), 0o600); err != nil {
				t.Fatalf("writing the fixture cache: %v", err)
			}
			logger, buf := newActionTestLogger()

			instances, writtenAt, ok := loadStateCache(logger, dir)

			if ok {
				t.Fatalf("loadStateCache accepted %s: %+v", tc.name, instances)
			}
			if instances != nil || !writtenAt.IsZero() {
				t.Errorf("a refused cache handed back %+v / %s; it must hand back nothing at all", instances, writtenAt)
			}
			if !strings.Contains(buf.String(), "level=WARN") {
				t.Errorf("a cache refused for %s said nothing; an operator with a broken cache must be told which file and why:\n%s", tc.name, buf)
			}
		})
	}

	// The control: the same shape, valid, is accepted — without which every
	// case above could be passing for the wrong reason.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, stateCacheFileName), []byte(valid), 0o600); err != nil {
		t.Fatalf("writing the control cache: %v", err)
	}
	logger, buf := newActionTestLogger()
	if _, _, ok := loadStateCache(logger, dir); !ok {
		t.Fatalf("the control cache was refused, so the cases above prove nothing:\n%s", buf)
	}
}

// TestLoadStateCache_AnAbsentCacheIsNotAWarning is the one non-fault among the
// refusals: every fresh deployment's first start has no cache, and a warning
// that fires on a healthy install is how operators learn to ignore warnings. It
// still cold-starts; it just does not cry wolf.
func TestLoadStateCache_AnAbsentCacheIsNotAWarning(t *testing.T) {
	logger, buf := newActionTestLogger()

	if _, _, ok := loadStateCache(logger, t.TempDir()); ok {
		t.Fatal("loadStateCache reported success with no cache file present")
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("a first-ever start warned about the cache it has not written yet:\n%s", buf)
	}
	if !strings.Contains(buf.String(), "no warm-start cache yet") {
		t.Errorf("a first-ever start said nothing at all about why the dashboard is empty:\n%s", buf)
	}
}

// TestLoadStateCache_WithNoConfigDirectoryLoadsNothingQuietly mirrors the
// writer's own guard: no directory means no cache, and that is a state, not a
// fault.
func TestLoadStateCache_WithNoConfigDirectoryLoadsNothingQuietly(t *testing.T) {
	logger, buf := newActionTestLogger()

	if _, _, ok := loadStateCache(logger, ""); ok {
		t.Fatal("loadStateCache produced a cache from nowhere")
	}
	if buf.Len() != 0 {
		t.Errorf("an unconfigured cache directory logged something; there is nothing to say:\n%s", buf)
	}
}

// TestLoadStateCache_NeverRestoresTheProcessesOwnSwitchesFromDisk is the pin
// that keeps this file from ever changing behavior. dry_run, gui_actions and
// reverse_scan_remonitor are live facts about THIS process's config; the
// version is a fact about this binary. The cache carries them so a human
// reading the file sees what the endpoint served, and the loader hands back
// none of them: a file claiming dryRun false must never be able to make a
// rehearsing daemon's badge read "live".
func TestLoadStateCache_NeverRestoresTheProcessesOwnSwitchesFromDisk(t *testing.T) {
	dir := t.TempDir()
	body := `{"schemaVersion":1,"writtenAt":"2026-08-22T09:30:00Z","dryRun":false,"guiActions":true,
	          "reverseScanRemonitor":true,"version":"v9.9.9",
	          "instances":[{"name":"radarr-main","type":"radarr","total":10}]}`
	if err := os.WriteFile(filepath.Join(dir, stateCacheFileName), []byte(body), 0o600); err != nil {
		t.Fatalf("writing the fixture cache: %v", err)
	}
	logger, _ := newActionTestLogger()

	instances, _, ok := loadStateCache(logger, dir)
	if !ok {
		t.Fatal("the fixture cache was refused")
	}

	// The loader's signature is the proof: it hands back instances and a
	// timestamp, and there is nowhere for a switch to travel. A warm-started
	// store built from it reports the switches THIS process was constructed
	// with, never the file's.
	store := newStatsStore(true)
	store.warmStart(instances, time.Now())
	if snap := store.snapshot(); !snap.DryRun || snap.GUIActions || snap.ReverseScanRemonitor || snap.Version != buildVersion {
		t.Errorf("a warm-started store reported %+v; every switch and the version must come from this process, never from the cache file", snap)
	}
}

// --- the warm start ---------------------------------------------------------

// TestStatsStore_WarmStart_MarksEveryInstanceStaleAsOfTheCachesWriteTime is the
// display contract: the numbers come back in full, and every one of them is
// labelled as the last sweep's rather than this process's. A dashboard that
// showed cached numbers without saying so would be the exact "glance and trust
// these numbers" failure the unreachable badge already exists to prevent.
func TestStatsStore_WarmStart_MarksEveryInstanceStaleAsOfTheCachesWriteTime(t *testing.T) {
	writtenAt := time.Date(2026, 8, 22, 9, 30, 0, 0, time.UTC)
	store := newStatsStore(true)

	loaded := store.warmStart([]instanceStatsView{
		{Name: "radarr-main", Type: "radarr", Total: 10, Unmonitored: 4},
		{Name: "sonarr-main", Type: "sonarr", Total: 20, Unmonitored: 9},
	}, writtenAt)

	if loaded != 2 {
		t.Errorf("warmStart loaded %d instances, want 2", loaded)
	}
	snap := store.snapshot()
	if len(snap.Instances) != 2 {
		t.Fatalf("instances = %+v, want both, in the order they were handed over", snap.Instances)
	}
	if snap.Instances[0].Name != "radarr-main" || snap.Instances[1].Name != "sonarr-main" {
		t.Errorf("instance order = %s, %s: warm start must preserve the order it was given (config order)", snap.Instances[0].Name, snap.Instances[1].Name)
	}
	for _, inst := range snap.Instances {
		if !inst.Stale {
			t.Errorf("%s came back from the cache without stale set; nothing on the page could tell it apart from a fresh sweep", inst.Name)
		}
		if inst.AsOf == nil || !inst.AsOf.Equal(writtenAt) {
			t.Errorf("%s asOf = %v, want the cache's own writtenAt %s", inst.Name, inst.AsOf, writtenAt)
		}
		if inst.Total == 0 {
			t.Errorf("%s came back empty; the whole point is that the numbers are there", inst.Name)
		}
	}
}

// TestStatsStore_WarmStart_NeverOverwritesWhatACycleAlreadyRecorded is the
// ordering guard. Warm start belongs strictly before the first cycle; if it
// ever ran later (a refactor, a second call site), it would replace live
// numbers with older ones and mark them stale — a dashboard going BACKWARDS.
func TestStatsStore_WarmStart_NeverOverwritesWhatACycleAlreadyRecorded(t *testing.T) {
	store := newStatsStore(true)
	store.recordInstance(cycleKindStartup, time.Now(), "radarr-main", "radarr", cycleInstanceStats{
		total: 99, monitored: 50, unmonitored: 49, decisionsRan: true,
	})

	loaded := store.warmStart([]instanceStatsView{{Name: "radarr-main", Type: "radarr", Total: 10}}, time.Now())

	if loaded != 0 {
		t.Errorf("warmStart loaded %d instances into a store a cycle had already written to, want 0", loaded)
	}
	inst := store.snapshot().Instances[0]
	if inst.Total != 99 || inst.Stale {
		t.Errorf("instance = %+v, want the live numbers untouched and not marked stale", inst)
	}
}

// TestStatsStore_TheFirstFreshCycleClearsStaleAndAsOf: the moment a cycle
// records this instance from a live read, the numbers are this process's own
// and the banner must go away by itself.
func TestStatsStore_TheFirstFreshCycleClearsStaleAndAsOf(t *testing.T) {
	store := newStatsStore(true)
	store.warmStart([]instanceStatsView{{Name: "radarr-main", Type: "radarr", Total: 10, Unmonitored: 4}}, time.Now().Add(-time.Hour))

	store.recordInstance(cycleKindStartup, time.Now(), "radarr-main", "radarr", cycleInstanceStats{
		total: 12, monitored: 7, unmonitored: 5, decisionsRan: true,
	})

	inst := store.snapshot().Instances[0]
	if inst.Stale {
		t.Error("an instance a cycle has just recorded is still marked stale")
	}
	if inst.AsOf != nil {
		t.Errorf("asOf = %v, want null once the numbers are this process's own", inst.AsOf)
	}
	if inst.Total != 12 {
		t.Errorf("Total = %d, want the fresh cycle's own number", inst.Total)
	}
}

// TestStatsStore_ACycleThatCouldNotReachAnInstanceLeavesItWarmAndStale is the
// other half, and the honest one: recordUnreachable produced no new numbers, so
// the cached ones are still what is on screen and must still say so. Clearing
// stale here would claim a freshness this cycle never established.
func TestStatsStore_ACycleThatCouldNotReachAnInstanceLeavesItWarmAndStale(t *testing.T) {
	writtenAt := time.Now().Add(-2 * time.Hour)
	store := newStatsStore(true)
	store.warmStart([]instanceStatsView{{Name: "radarr-main", Type: "radarr", Total: 10, Unmonitored: 4}}, writtenAt)

	store.recordUnreachable("radarr-main", "radarr", unreachableReasonConnectivity)

	inst := store.snapshot().Instances[0]
	if !inst.Stale || inst.AsOf == nil || !inst.AsOf.Equal(writtenAt) {
		t.Errorf("instance = %+v (asOf %v), want still stale as of %s: a cycle that could not reach it produced no fresher numbers", inst, inst.AsOf, writtenAt)
	}
	if inst.Total != 10 {
		t.Errorf("Total = %d, want the cached number left exactly as it was", inst.Total)
	}
	if inst.LastCycleStatus.Status != cycleStatusSkipped {
		t.Errorf("LastCycleStatus = %+v, want skipped: this cycle's own outcome is never stale", inst.LastCycleStatus)
	}
}

// TestStatsStore_WarmStart_RestoresTheAlwaysPresentWireShape: a hand-edited or
// older cache can omit the slices and statuses the API contract promises are
// always present. The store's own invariants — never a null findings array,
// never an empty status string — are restored on the way in, so no consumer of
// GET /api/stats can tell a warm-started instance from a cold one except by the
// two fields that are meant to say so.
func TestStatsStore_WarmStart_RestoresTheAlwaysPresentWireShape(t *testing.T) {
	store := newStatsStore(true)

	store.warmStart([]instanceStatsView{{Name: "radarr-main", Type: "radarr"}}, time.Now())

	inst := store.snapshot().Instances[0]
	if inst.ReverseFindings == nil || inst.LastActions == nil || inst.FileReport.Findings == nil {
		t.Errorf("instance = %+v, want empty (never nil) findings/action slices", inst)
	}
	if inst.ReverseStatus != "off" || inst.FileReport.Status != "off" {
		t.Errorf("reverseStatus/fileReport.status = %q/%q, want the store's own \"off\" default rather than an empty string", inst.ReverseStatus, inst.FileReport.Status)
	}
}

// TestStatsStore_WarmStart_CopiesEverythingItIsHandedIn is the aliasing guard
// the store already applies everywhere else (cloneInstanceStatsView): the slice
// the loader hands over is decoded JSON the caller still holds, and a later
// recordInstance appending to the store's copy must not be able to reach back
// into it.
func TestStatsStore_WarmStart_CopiesEverythingItIsHandedIn(t *testing.T) {
	findings := []reverseFinding{{ID: 1, Title: "One", Reason: ReasonQualityCutoffNotMet}}
	loaded := []instanceStatsView{{Name: "radarr-main", Type: "radarr", ReverseFindings: findings}}
	store := newStatsStore(true)
	store.warmStart(loaded, time.Now())

	findings[0].Title = "mutated after the warm start"
	loaded[0].Name = "renamed after the warm start"

	inst := store.snapshot().Instances[0]
	if inst.Name != "radarr-main" || inst.ReverseFindings[0].Title != "One" {
		t.Errorf("instance = %+v: the store still shares memory with what the loader handed it", inst)
	}
}

// TestWebUIHandler_Stats_StaleAndAsOfRawJSONSpelling pins the two new wire
// keys' exact spelling and their always-present-ness, in
// TestWebUIHandler_Stats_CaseCollisionWireFieldsRawJSONSpelling's own style:
// every other test decodes the response back into the same Go struct it was
// encoded from, so a typo'd or `,omitempty`'d tag would round-trip fine and
// break only the page.
func TestWebUIHandler_Stats_StaleAndAsOfRawJSONSpelling(t *testing.T) {
	writtenAt := time.Date(2026, 8, 22, 9, 30, 0, 0, time.UTC)
	warm := newStatsStore(true)
	warm.warmStart([]instanceStatsView{{Name: "radarr-main", Type: "radarr", Total: 10}}, writtenAt)
	cold := newStatsStore(true)
	cold.recordInstance(cycleKindSweep, writtenAt, "radarr-main", "radarr", cycleInstanceStats{total: 10, decisionsRan: true})

	for _, tc := range []struct {
		name  string
		store *statsStore
		want  []string
	}{
		{"warm", warm, []string{`"stale":true`, `"asOf":"2026-08-22T09:30:00Z"`}},
		{"fresh", cold, []string{`"stale":false`, `"asOf":null`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, _ := newTestWebUIServer(t, tc.store)
			resp, err := http.Get(ts.URL + "/api/stats")
			if err != nil {
				t.Fatalf("GET /api/stats: %v", err)
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			for _, want := range tc.want {
				if !strings.Contains(string(raw), want) {
					t.Errorf("raw JSON is missing %s — the page reads these two keys literally:\n%s", want, raw)
				}
			}
		})
	}
}

// --- the structural pins ----------------------------------------------------

// stateCacheMutationApprovedFunctions is the v0.2.0 amendment's second half,
// exactly as fsMutationApprovedFunctions (filereport_test.go) is v2.2's: naming
// a FILE that may write and rename says nothing about WHAT it writes. Only this
// function's body may contain such a call, and it is the atomic snapshot write
// by construction.
var stateCacheMutationApprovedFunctions = map[string]bool{"writeStateCache": true}

// TestStateCacheFile_EveryMutationIsTheCacheWriteConfinedToTheConfigDirectory
// is the pin the filesystem-mutation audit's amendment owes. It parses
// statecache.go (go/ast, so it can tell WHICH function a call sits in, unlike a
// substring scan) and requires every os.WriteFile/os.Rename call site to be
// inside writeStateCache — and requires that function to take its paths from
// stateCachePaths, the containment guard, rather than composing one itself.
//
// A future "pruneOldCaches" or "writeFindingsToDisk" would compile, would sit
// in an allowlisted file, and would be caught here, which is the point.
func TestStateCacheFile_EveryMutationIsTheCacheWriteConfinedToTheConfigDirectory(t *testing.T) {
	const file = "statecache.go"
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}

	watched := map[string]bool{"WriteFile": true, "Rename": true}
	sites := 0
	pathsFrom := map[string]bool{}
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "stateCachePaths" {
				pathsFrom[fn.Name.Name] = true
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "os" || !watched[sel.Sel.Name] {
				return true
			}
			sites++
			if !stateCacheMutationApprovedFunctions[fn.Name.Name] {
				t.Errorf("%s: os.%s is called inside %s, which is not the approved cache-write function — the v0.2.0 allowlist grants this file the ability to write ONE snapshot into the config directory, nothing else",
					file, sel.Sel.Name, fn.Name.Name)
			}
			return true
		})
	}

	// Vacuity guard, the twin of the action audit's own: a rename, a moved file
	// or a parse that silently produced nothing would leave this green while
	// checking an empty set.
	if sites != 2 {
		t.Fatalf("found %d os.WriteFile/os.Rename call site(s) in %s, want exactly 2 (the temp write and the rename): this audit is vacuous unless it is actually finding them", sites, file)
	}
	if !pathsFrom["writeStateCache"] {
		t.Error("writeStateCache no longer takes its paths from stateCachePaths; the containment guard is the only thing keeping this file's two mutations inside the config directory")
	}
}

// stateCacheEntryPointsAllowedOutside is the entire published surface of
// statecache.go: the two names another file may legitimately mention, and the
// file each is allowed in.
//
// daemon.go is the whole list, and both entries are WIRING rather than
// evaluation: the load happens once, in runDaemon, before the first cycle; the
// write happens at the end of a completed full cycle, from the same file that
// schedules those cycles. Nothing in the decision engines, the cross-check, the
// write paths, the file report, the reverse scan, the action system or the HTTP
// handlers may name any part of this file at all — which is what makes "the
// cache is never an input to anything that decides or acts" a property of the
// code rather than a promise in a comment.
var stateCacheEntryPointsAllowedOutside = map[string]map[string]bool{
	"daemon.go": {"loadStateCache": true, "writeStateCache": true},
}

// TestTree_TheStateCacheIsReachableOnlyFromTheDaemonsStartupWiring is the pin
// the statelessness constitution owes this feature, and it is deliberately
// modelled on TestTree_ExecutorsAreReachableOnlyFromTheActionEndpoint
// (actions_test.go) — same machinery, same derived-rather-than-listed banned
// set, same per-file selector-ambiguity exemption.
//
// It DERIVES the banned set: every top-level func and method statecache.go
// declares. So a helper added there later and called from decision.go — the
// ordinary way a cache becomes a decision input — fails here on the day it is
// written, rather than the day someone notices the daemon acting on numbers it
// read off a disk.
func TestTree_TheStateCacheIsReachableOnlyFromTheDaemonsStartupWiring(t *testing.T) {
	const source = "statecache.go"
	for _, v := range stateCacheEntryPointViolations(t, source, nonTestGoFilesExcept(t, source)) {
		t.Error(v)
	}
}

// TestTree_StateCacheReachabilityAuditCatchesAnEngineThatReadsTheCache is the
// mutation check for the audit above, run against SYNTHETIC files rather than
// by breaking the real tree, so the property is proved on every run instead of
// once by hand in a review round.
//
// Both cases are the failure that matters: a file that decides things reaching
// for the cache. One reads it through the published loader; the other reaches
// for an internal helper, which is why the banned set is derived from the whole
// file rather than listed.
func TestTree_StateCacheReachabilityAuditCatchesAnEngineThatReadsTheCache(t *testing.T) {
	const source = "statecache.go"
	dir := t.TempDir()

	deciding := writeActionFile(t, dir, "decision.go", `package main

import "log/slog"

func decideFromWhateverWasOnDisk(logger *slog.Logger, dir string) bool {
	instances, _, ok := loadStateCache(logger, dir)
	return ok && len(instances) > 0
}
`)
	got := stateCacheEntryPointViolations(t, source, []string{deciding})
	if len(got) != 1 || !strings.Contains(got[0], "loadStateCache") {
		t.Errorf("violations = %v, want exactly one naming loadStateCache: an engine that reads the display cache is precisely what this audit exists to catch", got)
	}

	acting := writeActionFile(t, dir, "actions.go", `package main

func (a *actionRunner) authorizeFromDisk(dir string) bool {
	_, _, ok := stateCachePaths(dir)
	return ok
}
`)
	got = stateCacheEntryPointViolations(t, source, []string{acting})
	if len(got) != 1 || !strings.Contains(got[0], "stateCachePaths") {
		t.Errorf("violations = %v, want exactly one naming stateCachePaths: the internal helpers are as off-limits as the loader, which is why the banned set is derived rather than listed", got)
	}
}

// stateCacheEntryPointViolations is the audit body, returning one message per
// violation instead of failing, so the same code runs against the real tree and
// against a synthetic file whose violation is the point.
//
// It reuses actions_test.go's own machinery verbatim (nonTestGoFilesExcept,
// selectorNamesDeclaredIn, identRoles) rather than forking a second copy of it:
// the two audits ask the same question about two different files, and a drifted
// copy of the harder half — the selector/declaration role split — would be a
// second thing to keep correct for no benefit.
func stateCacheEntryPointViolations(t *testing.T, source string, others []string) []string {
	t.Helper()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, source, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", source, err)
	}
	bannedFuncs, bannedMethods := map[string]bool{}, map[string]bool{}
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Recv != nil {
			bannedMethods[fn.Name.Name] = true
			continue
		}
		bannedFuncs[fn.Name.Name] = true
	}
	// Vacuity guard: a rename, a moved file or a parse that silently produced
	// nothing would leave this test green while checking an empty set.
	for _, must := range []string{"loadStateCache", "writeStateCache", "stateCachePaths", "coldStart"} {
		if !bannedFuncs[must] {
			t.Fatalf("%s no longer declares func %s, so this audit is not looking at the state cache at all", source, must)
		}
	}

	var violations []string
	for _, path := range others {
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			t.Fatalf("parsing %s: %v", path, parseErr)
		}
		ambiguous := selectorNamesDeclaredIn(t, fset, []string{path})
		allowed := stateCacheEntryPointsAllowedOutside[filepath.Base(path)]
		selectors, declarations := identRoles(file)
		ast.Inspect(file, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok || declarations[id.Pos()] || allowed[id.Name] {
				return true
			}
			switch {
			case selectors[id.Pos()] && bannedMethods[id.Name] && !ambiguous[id.Name]:
			case !selectors[id.Pos()] && bannedFuncs[id.Name]:
			default:
				return true
			}
			violations = append(violations, fmt.Sprintf("%s:%d references %s, which %s declares: the warm-start display cache must stay reachable ONLY from daemon.go's startup and end-of-cycle wiring, and an engine, evaluator, executor or handler that can reach it would make the statelessness constitution false",
				path, fset.Position(id.Pos()).Line, id.Name, source))
			return true
		})
	}
	return violations
}

// --- the daemon wiring, end to end ------------------------------------------

// seedStateCache writes a cache into a config file's own directory, as a
// previous run of the daemon would have left it there.
func seedStateCache(t *testing.T, configPath string, at time.Time, instances ...instanceStatsView) {
	t.Helper()
	logger, _ := newActionTestLogger()
	writeStateCache(logger, filepath.Dir(configPath), statsResponse{Instances: instances, DryRun: true, Version: "previous"}, at)
	if _, err := os.Stat(filepath.Join(filepath.Dir(configPath), stateCacheFileName)); err != nil {
		t.Fatalf("the seeded cache is not where the daemon will look for it: %v", err)
	}
}

// holdTheLibraryRead makes a stateful Radarr fake block its GET /api/v3/movie
// until the test lets it go, so a test can observe what the dashboard serves
// WHILE the first cycle is still running. The release func is registered as
// cleanup as well as returned: a test that fails an assertion before releasing
// would otherwise leave the daemon's own goroutine parked inside the fake's
// handler, and the harness's shutdown would wait for it until the whole package
// timed out.
func holdTheLibraryRead(t *testing.T, fake *statefulRadarrFake) (reached <-chan struct{}, release func()) {
	t.Helper()
	got := make(chan struct{}, 1)
	gate := make(chan struct{})
	release = sync.OnceFunc(func() { close(gate) })
	t.Cleanup(release)
	fake.onRequest = func(method, path string) {
		if method == http.MethodGet && path == "/api/v3/movie" {
			select {
			case got <- struct{}{}:
			default:
			}
			<-gate
		}
	}
	return got, release
}

// TestDaemon_WarmStart_ShowsTheLastSweepWhileTheFirstCycleIsStillRunning is the
// user-reported gap, closed: a restarted container used to serve an EMPTY
// instances array — a blank dashboard — for as long as the first full sweep
// took. Here the startup scan is held at its library read while the page asks
// for stats, and the answer is the previous run's numbers, labelled stale.
func TestDaemon_WarmStart_ShowsTheLastSweepWhileTheFirstCycleIsStillRunning(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{wouldUnmonitorStatefulMovie(1, "Movie")})
	reached, release := holdTheLibraryRead(t, fake)
	configPath := writeDaemonConfig(t, "radarr", fake.srv.URL, true, "info", "0", "45s")
	writtenAt := time.Date(2026, 8, 22, 9, 30, 0, 0, time.UTC)
	seedStateCache(t, configPath, writtenAt, instanceStatsView{
		Name: "radarr-main", Type: "radarr", Total: 996, Monitored: 540, Unmonitored: 456, WouldUnmonitor: 3,
	})

	h := startDaemon(t, configPath)
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatalf("the startup scan never reached its library read:\n%s", h.out.String())
	}

	warm := getStats(t, h)
	if len(warm.Instances) != 1 {
		t.Fatalf("instances = %+v, want the cached one served while the first cycle is still running", warm.Instances)
	}
	inst := warm.Instances[0]
	if inst.Total != 996 || inst.Unmonitored != 456 {
		t.Errorf("warm instance = %+v, want the cached numbers in full", inst)
	}
	if !inst.Stale || inst.AsOf == nil || !inst.AsOf.Equal(writtenAt) {
		t.Errorf("warm instance stale/asOf = %t/%v, want true and the cache's own write time %s: the page must never present cached numbers as this process's own", inst.Stale, inst.AsOf, writtenAt)
	}
	if !warm.DryRun {
		t.Error("the warm-started response reported dryRun false; every switch must come from this process's config, never from the cache")
	}

	release()
	h.waitReady()

	fresh := getStats(t, h)
	if len(fresh.Instances) != 1 {
		t.Fatalf("instances = %+v after the startup scan, want exactly one", fresh.Instances)
	}
	if fresh.Instances[0].Stale || fresh.Instances[0].AsOf != nil {
		t.Errorf("instance = %+v, want stale cleared once the first fresh cycle recorded it", fresh.Instances[0])
	}
	if fresh.Instances[0].Total != 1 {
		t.Errorf("Total = %d, want the live library's own number (1) rather than the cache's 996", fresh.Instances[0].Total)
	}
}

// TestDaemon_WritesTheCacheAtTheEndOfEveryFullCycleButNeverForAWebhookOne is
// the write half of the wiring. A full cycle describes the whole library, so it
// is a snapshot worth restoring; a webhook cycle is scoped to one item, runs no
// reverse pass and no file report, and saving after one would overwrite a
// complete picture with a partial one.
func TestDaemon_WritesTheCacheAtTheEndOfEveryFullCycleButNeverForAWebhookOne(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{wouldUnmonitorStatefulMovie(1, "Movie")})
	configPath := writeDaemonConfig(t, "radarr", fake.srv.URL, true, "debug", "0", "45s")
	dir := filepath.Dir(configPath)
	h := startDaemon(t, configPath)
	h.waitReady()

	logger, _ := newActionTestLogger()
	instances, afterStartup, ok := loadStateCache(logger, dir)
	if !ok {
		t.Fatalf("the startup scan wrote no cache:\n%s", h.out.String())
	}
	if len(instances) != 1 || instances[0].Name != "radarr-main" || instances[0].Total != 1 {
		t.Errorf("cached instances = %+v, want the startup scan's own numbers", instances)
	}

	// A webhook cycle: it runs, it records, and it must leave the cache exactly
	// as the last full cycle left it.
	h.post("radarr-main", downloadMoviePayload)
	eventually(t, "the webhook to be queued", func() bool {
		return strings.Contains(h.out.String(), "webhook queued")
	})
	h.clock.Advance(45 * time.Second)
	h.awaitLogCount("webhook debounce expired; evaluating", 1)
	eventually(t, "the webhook cycle's own summary", func() bool {
		return strings.Count(h.out.String(), "radarr decision summary") == 2
	})
	if _, afterWebhook, ok := loadStateCache(logger, dir); !ok || !afterWebhook.Equal(afterStartup) {
		t.Errorf("the cache's writtenAt moved from %s to %s across a webhook cycle; a scoped cycle must never overwrite a full sweep's snapshot", afterStartup, afterWebhook)
	}

	// A manual scan IS a full cycle, so it must.
	h.clock.Advance(time.Hour)
	if got := postScanTo(t, h.url); got.status != http.StatusAccepted {
		t.Fatalf("POST /api/scan = %+v", got)
	}
	h.awaitLogCount("manual scan complete", 1)
	eventually(t, "the manual scan's own cache write", func() bool {
		_, at, ok := loadStateCache(logger, dir)
		return ok && at.After(afterStartup)
	})
}

// TestDaemon_AnInstanceRemovedFromTheConfigIsNotResurrectedFromTheCache is the
// warm start's own containment rule. Nothing ever removes an instance from the
// stats store, so a cached instance the config no longer names would reappear
// on the dashboard at every restart, be written back out at the end of the
// cycle, and outlive the deployment that created it.
func TestDaemon_AnInstanceRemovedFromTheConfigIsNotResurrectedFromTheCache(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{wouldUnmonitorStatefulMovie(1, "Movie")})
	configPath := writeDaemonConfig(t, "radarr", fake.srv.URL, true, "info", "0", "45s")
	seedStateCache(t, configPath, time.Date(2026, 8, 22, 9, 30, 0, 0, time.UTC),
		instanceStatsView{Name: "radarr-main", Type: "radarr", Total: 996},
		instanceStatsView{Name: "radarr-retired", Type: "radarr", Total: 42},
	)

	h := startDaemon(t, configPath)
	h.waitReady()

	for _, inst := range getStats(t, h).Instances {
		if inst.Name == "radarr-retired" {
			t.Errorf("an instance the config no longer names came back from the cache: %+v", inst)
		}
	}
	if !strings.Contains(h.out.String(), "warm start") {
		t.Errorf("the daemon warm-started without saying so; a dashboard showing a previous run's numbers must be explained in the log too:\n%s", h.out.String())
	}
}

// TestDaemon_ACachedInstanceWhoseTypeChangedIsNotWarmStarted is the same
// containment rule one step subtler: the config still names this instance, but
// it names it as a different *arr. The numbers under that name describe the old
// thing — a movie count against a series library — so the entry is dropped and
// the card stays empty until this process's own first cycle fills it in.
func TestDaemon_ACachedInstanceWhoseTypeChangedIsNotWarmStarted(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{wouldUnmonitorStatefulMovie(1, "Movie")})
	reached, release := holdTheLibraryRead(t, fake)
	configPath := writeDaemonConfig(t, "radarr", fake.srv.URL, true, "info", "0", "45s")
	seedStateCache(t, configPath, time.Date(2026, 8, 22, 9, 30, 0, 0, time.UTC),
		instanceStatsView{Name: "radarr-main", Type: "sonarr", Total: 996},
	)

	h := startDaemon(t, configPath)
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatalf("the startup scan never reached its library read:\n%s", h.out.String())
	}

	if got := getStats(t, h); len(got.Instances) != 0 {
		t.Errorf("instances = %+v, want none: the cached entry is a sonarr's numbers under a name this config now gives to a radarr", got.Instances)
	}

	release()
	h.waitReady()
}

// TestDaemon_AColdStartWithNoCacheIsExactlyWhatItAlwaysWas: the feature adds a
// warm path, it does not change the cold one. With no cache present the
// instances array is empty until the first cycle records something, exactly as
// every version before this one behaved.
func TestDaemon_AColdStartWithNoCacheIsExactlyWhatItAlwaysWas(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{wouldUnmonitorStatefulMovie(1, "Movie")})
	reached, release := holdTheLibraryRead(t, fake)
	h := startDaemon(t, writeDaemonConfig(t, "radarr", fake.srv.URL, true, "info", "0", "45s"))
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatalf("the startup scan never reached its library read:\n%s", h.out.String())
	}

	if got := getStats(t, h); len(got.Instances) != 0 {
		t.Errorf("instances = %+v on a cold start's first cycle, want the empty array every version before this one served", got.Instances)
	}
	if strings.Contains(h.out.String(), "level=WARN") {
		t.Errorf("a first-ever start warned about the cache it has not written yet:\n%s", h.out.String())
	}

	release()
	h.waitReady()
}

// --- the cache's authority, or rather its complete lack of one --------------

// TestAction_OnAWarmStaleFinding_StillReVerifiesLiveAndRefusesWhenLiveDisagrees
// is the constitutional pin the warm start owes the action system.
//
// The row a human clicks after a restart was rendered from the on-disk cache:
// nothing about it was observed by THIS process. Rule 3 of the action
// constitution says every executor re-derives its finding from live data and
// refuses if the world no longer matches what the button promised — and that
// rule must hold identically whether the row came from a sweep this process ran
// or from a file it read at startup. The cache buys the row's EXISTENCE, never
// its authority.
//
// Every switch is on (gui_actions true, dry_run false) so nothing but the live
// re-verification can be what stops this.
func TestAction_OnAWarmStaleFinding_StillReVerifiesLiveAndRefusesWhenLiveDisagrees(t *testing.T) {
	fake := newArrFake(t)
	_, dupPath, inst := radarrFileFixture(t, fake)
	cfg := Config{DryRun: false, GUIActions: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, buf := newActionFixture(t, cfg, inst)

	// The row comes from the CACHE — statsStore.warmStart, exactly as
	// daemon.go's startup wiring seeds it — not from any cycle this process
	// ran. It is marked stale, as the page shows it.
	store.warmStart([]instanceStatsView{{
		Name: inst.Name, Type: inst.Type,
		FileReport: fileReportSnapshot{Status: "ran", Duplicates: 1, Findings: []fileReportFindingRecord{
			{Kind: fileKindDuplicate, Path: dupPath, Display: "Some Movie (2019)/ETRG.Sample.mkv", Size: 10},
		}},
	}}, time.Now().Add(-6*time.Hour))
	if !store.snapshot().Instances[0].Stale {
		t.Fatal("the fixture is not actually a warm-stale row, so this test would prove nothing")
	}
	before := fake.requestCount()

	// The world has moved on since that cache was written: a different, larger
	// file now sits at the same path.
	if err := os.WriteFile(dupPath, []byte("a much larger replacement file than the cache ever saw"), 0o644); err != nil {
		t.Fatalf("replacing the fixture: %v", err)
	}

	status, out := postAction(t, ts, `{"kind":"trash","confirm":true,"instance":"radarr-main","path":`+jsonString(dupPath)+`,"finding":"duplicate","size":10}`)

	if status != http.StatusConflict || out.Outcome != actionOutcomeRefused {
		t.Fatalf("status=%d outcome=%q reason=%q, want 409/refused: a cached row must never be able to authorize a move on its own", status, out.Outcome, out.Reason)
	}
	if !strings.Contains(out.Reason, "size") {
		t.Errorf("the refusal must say the file is not the one the button described; got %q", out.Reason)
	}
	if fake.requestCount() <= before {
		t.Error("the action never contacted the instance: rule 3's live re-derivation must run for a warm-stale row exactly as for a fresh one, not be short-circuited by what was on disk")
	}
	if _, err := os.Lstat(dupPath); err != nil {
		t.Errorf("the replacement file must be untouched: %v", err)
	}
	if !strings.Contains(buf.String(), "msg=action") {
		t.Errorf("the refusal was not audited:\n%s", buf)
	}
}
