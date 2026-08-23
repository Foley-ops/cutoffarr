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
	"os/exec"
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

// TestStateCacheDirInsideAMediaRoot_IsTheHalfOfContainmentTheOtherTestCannotSee
// is a round-2 review fix, and it closes the gap the containment pin above left
// wide open.
//
// TestStateCachePaths_StayInsideTheConfigDirectory proves both paths are direct
// children of cfg.ConfigDir. Nothing proved cfg.ConfigDir is not ITSELF inside a
// configured media root — and it is just filepath.Dir(--config) (config.go), so
// `--config /data/media/Movies/config.yml` had cutoffarr writing a file into the
// user's library at the end of every full sweep. That is the exact thing the
// audit amendment's own argument ("statecache.go writes ONE file to the CONFIG
// directory") assumes cannot happen, and the brief asked for a pin that it
// cannot: "never under any media root; path containment asserted".
//
// Matching is on path SEGMENT boundaries (hasPathPrefix, the same helper the
// file report maps every *arr path with), so a config directory merely named
// like a media root — /data/media-config beside /data/media — is not refused.
func TestStateCacheDirInsideAMediaRoot_IsTheHalfOfContainmentTheOtherTestCannotSee(t *testing.T) {
	radarr := Instance{Name: "radarr-main", Type: "radarr", MediaRootMap: map[string]string{
		"/movies": "/data/media/Movies",
	}}
	sonarr := Instance{Name: "sonarr-main", Type: "sonarr", MediaRootMap: map[string]string{
		"/tv": "/data/media/TV",
	}}

	for _, tc := range []struct {
		name     string
		dir      string
		wantRoot string
	}{
		{"the media root itself", "/data/media/Movies", "/data/media/Movies"},
		{"a directory under it", "/data/media/Movies/cutoffarr", "/data/media/Movies"},
		{"a trailing slash does not smuggle it past", "/data/media/Movies/", "/data/media/Movies"},
		{"the SECOND instance's root counts too", "/data/media/TV/config", "/data/media/TV"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, inside := stateCacheDirInsideAMediaRoot(tc.dir, []Instance{radarr, sonarr})
			if !inside || root != tc.wantRoot {
				t.Errorf("stateCacheDirInsideAMediaRoot(%q) = %q, %v; want %q, true — a cache written there lands inside the user's library", tc.dir, root, inside, tc.wantRoot)
			}
		})
	}

	for _, tc := range []struct{ name, dir string }{
		{"an ordinary config directory", "/config"},
		{"a sibling that merely shares a prefix", "/data/media/Movies-config"},
		{"the media root's own parent", "/data/media"},
		{"no directory at all", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if root, inside := stateCacheDirInsideAMediaRoot(tc.dir, []Instance{radarr, sonarr}); inside {
				t.Errorf("stateCacheDirInsideAMediaRoot(%q) refused a directory outside every media root, naming %q; the warm start would be lost for no reason", tc.dir, root)
			}
		})
	}

	if _, inside := stateCacheDirInsideAMediaRoot("/data/media/Movies", []Instance{{Name: "radarr-main", Type: "radarr"}}); inside {
		t.Error("an instance with NO media_root_map produced a media root to be contained by; the file report is opt-in and an absent map is the OFF state")
	}

	// Round-3 review fix, and the bypass every case above is blind to: the
	// guard matched a possibly-RELATIVE directory against absolute media roots.
	// LoadConfig sets ConfigDir = filepath.Dir(--config) with no filepath.Abs,
	// so `cd /data/media/Movies && cutoffarr --config config.yml` produced
	// ConfigDir "." — which can never string-match any absolute root — and
	// state-cache.json was written into the user's media library at the end of
	// every full sweep, with no refusal and no WARN. Exactly what the brief's
	// "never under any media root; path containment asserted" clause, and this
	// whole function, exist to prevent.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("reading the working directory: %v", err)
	}
	here := Instance{Name: "radarr-here", Type: "radarr", MediaRootMap: map[string]string{
		"/movies": filepath.ToSlash(cwd),
	}}
	for _, tc := range []struct{ name, dir string }{
		{"the working directory itself, named relatively", "."},
		{"a child of it", "fixtures"},
		{"the same place by the long way round", "./fixtures/.."},
	} {
		t.Run("relative "+tc.name, func(t *testing.T) {
			root, inside := stateCacheDirInsideAMediaRoot(tc.dir, []Instance{here})
			if !inside {
				t.Fatalf("stateCacheDirInsideAMediaRoot(%q) allowed a relative directory whose absolute form is inside the media root %q: `cd <a media root> && cutoffarr --config config.yml` would drop %s into the user's library at the end of every sweep, unrefused and unlogged", tc.dir, filepath.ToSlash(cwd), stateCacheFileName)
			}
			if want := filepath.ToSlash(cwd); root != want {
				t.Errorf("stateCacheDirInsideAMediaRoot(%q) named %q as the containing root, want %q", tc.dir, root, want)
			}
		})
	}
	t.Run("a relative directory outside every root is still allowed", func(t *testing.T) {
		// The other half: absolutizing must not turn every relative path into a
		// refusal. This one resolves under the working directory, which the two
		// /data/media roots do not contain.
		if root, inside := stateCacheDirInsideAMediaRoot("config", []Instance{radarr, sonarr}); inside {
			t.Errorf("stateCacheDirInsideAMediaRoot(%q) refused a relative directory that resolves outside every media root, naming %q; the warm start would be lost for no reason", "config", root)
		}
	})
}

// TestDaemon_StateCacheDir_RefusesToWriteInsideAMediaRootAndSaysSoOnce is the
// wiring half: one WARN per process naming the root, and the cache disabled in
// BOTH directions — a cache that may not be written must not be read either, or
// a stale file already sitting in a media root would keep warm-starting the
// dashboard forever.
func TestDaemon_StateCacheDir_RefusesToWriteInsideAMediaRootAndSaysSoOnce(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "cutoffarr")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("creating the config directory: %v", err)
	}
	logger, buf := newActionTestLogger()
	d := &daemon{
		logger: logger,
		clock:  newFakeClock(),
		stats:  newStatsStore(true),
		cfg: Config{
			ConfigDir: configDir,
			Instances: []Instance{{Name: "radarr-main", Type: "radarr", MediaRootMap: map[string]string{"/movies": root}}},
		},
	}

	if got := d.stateCacheDir(); got != "" {
		t.Errorf("stateCacheDir() = %q, want \"\": the config directory is inside a configured media root, and nothing this program writes may land in a media library", got)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, root) {
		t.Errorf("the refusal never warned, or never named the media root it refused for:\n%s", out)
	}

	// Once per process, not once per sweep: the reconciliation loop calls this
	// at the end of every full cycle, and a line repeated daily forever is how
	// operators learn to filter warnings out.
	d.stateCacheDir()
	d.saveStateCache()
	if n := strings.Count(buf.String(), "sits inside a configured media root"); n != 1 {
		t.Errorf("the refusal was logged %d times, want exactly 1 per process:\n%s", n, buf)
	}

	// And the write really did not happen.
	d.stats.recordInstance(cycleKindSweep, time.Now(), "radarr-main", "radarr", cycleInstanceStats{total: 1, decisionsRan: true})
	d.saveStateCache()
	if names := dirEntryNames(t, configDir); len(names) != 0 {
		t.Errorf("the config directory contains %v after a save; nothing may be written inside a media root", names)
	}

	// Nor the read: a cache left there by an earlier, unguarded version must not
	// warm-start anything either.
	seedStateCache(t, filepath.Join(configDir, "config.yml"), time.Date(2026, 8, 22, 9, 30, 0, 0, time.UTC),
		instanceStatsView{Name: "radarr-main", Type: "radarr", Total: 996})
	fresh := newStatsStore(true)
	d.stats = fresh
	d.warmStartFromStateCache()
	if got := fresh.snapshot().Instances; len(got) != 0 {
		t.Errorf("instances = %+v, want none: a cache sitting inside a media root is refused in both directions", got)
	}
}

// TestTree_TheDaemonFeedsTheCacheOnlyThroughItsOwnGuardedDirectory pins the
// mechanism the two tests above rest on. The media-root refusal lives in ONE
// place — (*daemon).stateCacheDir — and it is worth nothing if a future call
// site passes d.cfg.ConfigDir straight to the loader or the writer instead.
//
// So: every call to loadStateCache/writeStateCache in daemon.go must take its
// directory from a stateCacheDir() call written INLINE at the call site. Inline
// deliberately — a local variable assigned three lines up would be one more
// thing to trace, for a reader and for this test, on the one call in this
// program that can put a file inside someone's media library.
func TestTree_TheDaemonFeedsTheCacheOnlyThroughItsOwnGuardedDirectory(t *testing.T) {
	const file = "daemon.go"
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}

	entryPoints := map[string]bool{"loadStateCache": true, "writeStateCache": true}
	sites := 0
	ast.Inspect(parsed, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || !entryPoints[id.Name] {
			return true
		}
		sites++
		guarded := false
		for _, arg := range call.Args {
			inner, ok := arg.(*ast.CallExpr)
			if !ok {
				continue
			}
			if sel, ok := inner.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "stateCacheDir" {
				guarded = true
			}
		}
		if !guarded {
			t.Errorf("%s:%d passes %s a directory that did not come from d.stateCacheDir(): the media-root refusal is the only thing keeping this program from writing a file into the user's library, and it is bypassed by any call site that reads d.cfg.ConfigDir itself",
				file, fset.Position(call.Pos()).Line, id.Name)
		}
		return true
	})
	// Vacuity guard, the twin of the mutation audit's own.
	if sites != 2 {
		t.Fatalf("found %d loadStateCache/writeStateCache call site(s) in %s, want exactly 2 (the startup load and the end-of-cycle write): this audit is vacuous unless it is actually finding them", sites, file)
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

// TestLoadStateCache_AnAbsentCacheWarnsAndColdStarts.
//
// This was TestLoadStateCache_AnAbsentCacheIsNotAWarning, asserting INFO, and
// the argument for INFO was a good one: every fresh deployment's first start has
// no cache, and a warning that fires on a healthy install is how operators learn
// to ignore warnings. The brief settled it the other way and named this case
// FIRST — "Invalid in ANY way (missing, unparseable, wrong schemaVersion,
// pointer-decode misses on load-bearing fields) → WARN + ignore + cold start" —
// and the deviation was not ratified in the controller's round-2 rulings. So it
// is a WARN, and it says in the same breath that a first start is exactly when
// this is expected.
//
// The line must still SAY the three things every refusal in this file says:
// which file, what is wrong with it, and what happens instead.
func TestLoadStateCache_AnAbsentCacheWarnsAndColdStarts(t *testing.T) {
	dir := t.TempDir()
	logger, buf := newActionTestLogger()

	if _, _, ok := loadStateCache(logger, dir); ok {
		t.Fatal("loadStateCache reported success with no cache file present")
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("an absent cache did not warn; the brief lists `missing` first among the shapes that must WARN and cold-start:\n%s", out)
	}
	for _, want := range []string{filepath.Join(dir, stateCacheFileName), "cold", "first start"} {
		if !strings.Contains(out, want) {
			t.Errorf("the absent-cache warning never mentions %q — a refusal here says which file, what is wrong, and what happens instead:\n%s", want, out)
		}
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

// TestStatsStore_ACycleThatAbortedInsideTheEngineLeavesItWarmAndStale is the
// third outcome, and it is the one that has no other marker at all — a
// round-5 review fix.
//
// runScanCycle reaches recordInstance whenever the LIBRARY read succeeded
// (dataOK), which is strictly weaker than the engine having completed an
// evaluation: §2.6's warn-and-skip paths INSIDE the engine (a quality-profile
// fetch failure at decision.go's fetchQualityProfiles, an exclusion-tag
// resolution failure) bare-return with decisionsRan still false. Every field
// such a cycle could not recompute is deliberately left alone thirty lines
// below — WouldUnmonitor, ReverseFindings, FileReport — so what remains on the
// dashboard is a previous PROCESS's cached findings. Clearing Stale/AsOf here
// removed the only two markers that said so: the amber banner and the per-card
// "as of …" note both key on stale, and the file-clutter panel carries no
// timestamp of its own. That is R3-3's defect ("a previous process's findings
// presented as current, indefinitely, with live trash and merge buttons beside
// them") reached through the abort-inside-the-engine door, and it compounds:
// the next saveStateCache would write the entry with stale:false, so the
// following restart's warmStart would stamp those same cached findings as of
// the cache's own write time.
//
// The brief's rule is "the FIRST COMPLETED fresh cycle clears stale/asOf". A
// cycle that aborted before completing an evaluation is not one.
func TestStatsStore_ACycleThatAbortedInsideTheEngineLeavesItWarmAndStale(t *testing.T) {
	writtenAt := time.Now().Add(-3 * time.Hour)
	store := newStatsStore(true)
	store.warmStart([]instanceStatsView{{
		Name: "radarr-main", Type: "radarr", Total: 10, Unmonitored: 4, WouldUnmonitor: 6,
		FileReport: fileReportSnapshot{Status: "ran", Duplicates: 3,
			Findings: []fileReportFindingRecord{{Kind: fileKindDuplicate, Path: "/movies/Movie A/Movie A (2).mkv", Display: "Movie A (2).mkv"}}},
	}}, writtenAt)

	// dataOK was true (the library read landed, so total/monitored/unmonitored
	// below are this process's own), but the engine bare-returned before the
	// evaluation loop: decisionsRan is false.
	store.recordInstance(cycleKindStartup, time.Now(), "radarr-main", "radarr", cycleInstanceStats{
		total: 12, monitored: 7, unmonitored: 5,
	})

	inst := store.snapshot().Instances[0]
	if !inst.Stale || inst.AsOf == nil || !inst.AsOf.Equal(writtenAt) {
		t.Errorf("instance = %+v (asOf %v), want still stale as of %s: this cycle never completed an evaluation, so wouldUnmonitor and the file-clutter findings on screen are still the cache's",
			inst, inst.AsOf, writtenAt)
	}
	// The reason the markers must stay: these are the numbers they mark.
	if inst.WouldUnmonitor != 6 || inst.FileReport.Duplicates != 3 || len(inst.FileReport.Findings) != 1 {
		t.Errorf("instance = %+v, want the cached wouldUnmonitor and file-clutter findings left exactly as they were", inst)
	}
	if inst.LastCycleStatus.Status != cycleStatusSkipped {
		t.Errorf("LastCycleStatus = %+v, want skipped: this cycle's own outcome is never stale", inst.LastCycleStatus)
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
// stateCacheDirInsideAMediaRoot is the third name, added in round 2 of review,
// and it is admitted on the opposite grounds from the other two: it is a
// REFUSAL, not an access. It is a pure predicate over a path and the configured
// media roots, it opens nothing, reads nothing and returns no cached value —
// (*daemon).stateCacheDir is its only caller, and all it can do is decide the
// cache is disabled for this process. A file that could reach it still could not
// reach one byte of the cache.
// stateCacheFileName is the fourth, added in round 5 with the audit that can
// finally see it, and it is admitted on the narrowest grounds of all: daemon.go
// names it three times and every one of them is inside a log line's attrs
// (`"file", stateCacheFileName`), telling the operator which file the sentence
// they are reading is about. A const holding a file NAME opens nothing; what
// made it worth banning in the first place is that a name plus os.ReadFile is
// how a cache becomes a decision input, and daemon.go is the one file already
// trusted with the loader itself. Any OTHER file naming it now fails this
// audit, which is the change.
var stateCacheEntryPointsAllowedOutside = map[string]map[string]bool{
	"daemon.go": {"loadStateCache": true, "writeStateCache": true, "stateCacheDirInsideAMediaRoot": true, "stateCacheFileName": true},
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

	// Round-5 review fix, and the bypass that was invisible to BOTH audits: an
	// engine that never calls a single function of statecache.go and reads the
	// cache anyway. The filename and the document type are all it needs —
	// os.ReadFile is not a filesystem MUTATION, so the v2.2 audit does not look
	// at it, and derived-from-FuncDecl left every const and type this file
	// declares nameable from anywhere in the tree. Two lines in decision.go and
	// the cache is a decision input, which is the one thing the structural pin
	// exists to make impossible.
	sneaking := writeActionFile(t, dir, "reverse.go", `package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

func lastNightsNumbers(cfg Config) []instanceStatsView {
	raw, err := os.ReadFile(filepath.Join(cfg.ConfigDir, stateCacheFileName))
	if err != nil {
		return nil
	}
	var doc stateCacheDocument
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Instances == nil {
		return nil
	}
	return *doc.Instances
}
`)
	got = stateCacheEntryPointViolations(t, source, []string{sneaking})
	if len(got) != 2 {
		t.Fatalf("violations = %v, want two (the file name and the document type): a file that opens the cache by name and decodes it into this file's own type never touches a function of it, so a banned set derived only from func declarations cannot see it at all", got)
	}
	for _, want := range []string{"stateCacheFileName", "stateCacheDocument"} {
		found := false
		for _, v := range got {
			if strings.Contains(v, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("violations = %v, want one naming %s: every top-level name this file declares — consts and types included — is part of the surface an engine must not be able to reach", got, want)
		}
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
	bannedNames, bannedMethods := map[string]bool{}, map[string]bool{}
	for _, decl := range parsed.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv != nil {
				bannedMethods[d.Name.Name] = true
				continue
			}
			bannedNames[d.Name.Name] = true
		case *ast.GenDecl:
			// Round-5 review fix: the CONSTS and TYPES too, not only the
			// functions. Derived-from-FuncDecl left stateCacheFileName,
			// stateCacheSchemaVersion and stateCacheDocument nameable from any
			// file in the tree, which is a two-line bypass of this whole audit —
			// os.ReadFile(filepath.Join(cfg.ConfigDir, stateCacheFileName)) plus a
			// json.Unmarshal into stateCacheDocument reaches every byte of the
			// cache without calling one function of statecache.go, and reads are
			// not filesystem MUTATIONS so the v2.2 audit never looks at them
			// either. The surface an engine must not be able to reach is every
			// top-level name this file declares, so that is what is derived.
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.ValueSpec:
					for _, name := range s.Names {
						bannedNames[name.Name] = true
					}
				case *ast.TypeSpec:
					bannedNames[s.Name.Name] = true
				}
			}
		}
	}
	// Vacuity guard: a rename, a moved file or a parse that silently produced
	// nothing would leave this test green while checking an empty set. One name
	// of each KIND, because the derivation now has three branches and a broken
	// one would otherwise be silent.
	for _, must := range []string{"loadStateCache", "writeStateCache", "stateCachePaths", "coldStart", "stateCacheFileName", "stateCacheSchemaVersion", "stateCacheDocument"} {
		if !bannedNames[must] {
			t.Fatalf("%s no longer declares %s, so this audit is not looking at the whole state cache: the banned set is every top-level name the file declares — funcs, consts and types alike", source, must)
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
			case !selectors[id.Pos()] && bannedNames[id.Name]:
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

// writeDaemonConfigWithMediaRoot is writeDaemonConfig plus a media_root_map,
// which is the file report's own opt-in switch (binding controller resolution
// 1: an absent map is the OFF state). It exists for the pair of warm-start
// tests that turn on exactly that difference — whether the instance whose
// cached file report is being restored can still produce one at all.
func writeDaemonConfigWithMediaRoot(t *testing.T, url, arrPath, diskPath string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := fmt.Sprintf(`
dry_run: true
log_level: info
poll_interval: 0
webhook_debounce: 45s
instances:
  - name: radarr-main
    type: radarr
    url: %s
    api_key: key1
    media_root_map:
      %q: %q
`, url, arrPath, diskPath)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing the daemon test config: %v", err)
	}
	return path
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

// TestDaemon_ACachedFileReportIsDroppedWhenTheInstanceHasNoMediaRootsLeft is
// the third case of the same containment rule, and the one the name/type filter
// cannot see: the instance is still configured, still the same *arr, but its
// media_root_map has been REMOVED from config.yml since the cache was written.
//
// The file report is opt-in per instance and an absent map is the OFF state, so
// no cycle of this process will ever run the pass — and recordInstance only
// overwrites FileReport when the pass actually ran (cs.fileReportRan). So the
// previous process's clutter findings would be restored from disk, survive
// every subsequent cycle untouched, and, once the first cycle clears Stale, be
// served with stale:false and no banner: a previous process's findings
// presented as current, indefinitely, with live trash and merge buttons beside
// them. fileReportSnapshot carries no timestamp of its own (unlike ReverseAsOf,
// which is exactly why the reverse half self-heals and is age-marked), so
// nothing on the page could have said otherwise. Round-3 review fix. Pre-v0.2.0
// this was impossible: with no media_root_map the snapshot stayed zero-value
// and the panel read "off".
func TestDaemon_ACachedFileReportIsDroppedWhenTheInstanceHasNoMediaRootsLeft(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{wouldUnmonitorStatefulMovie(1, "Movie")})
	reached, release := holdTheLibraryRead(t, fake)
	// writeDaemonConfig writes no media_root_map at all, which is the "the
	// operator removed it" state this test is about.
	configPath := writeDaemonConfig(t, "radarr", fake.srv.URL, true, "info", "0", "45s")
	seedStateCache(t, configPath, time.Date(2026, 8, 22, 9, 30, 0, 0, time.UTC),
		instanceStatsView{
			Name: "radarr-main", Type: "radarr", Total: 996, Monitored: 540, Unmonitored: 456,
			FileReport: fileReportSnapshot{
				Status: "ran", Duplicates: 1, ReclaimableBytes: 4096,
				Findings: []fileReportFindingRecord{{
					Kind: fileKindDuplicate, Path: "/data/media/Movies/Old/dupe.mkv",
					Display: "dupe.mkv", Size: 4096,
				}},
			},
		},
	)

	h := startDaemon(t, configPath)
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatalf("the startup scan never reached its library read:\n%s", h.out.String())
	}

	warm := getStats(t, h)
	if len(warm.Instances) != 1 {
		t.Fatalf("instances = %+v, want the cached instance warm-started (only its file report is in question)", warm.Instances)
	}
	if warm.Instances[0].Total != 996 {
		t.Errorf("the instance's own numbers were dropped along with the file report: %+v — only the report is unrestorable", warm.Instances[0])
	}
	if got := warm.Instances[0].FileReport; got.Status != "off" || len(got.Findings) != 0 || got.Duplicates != 0 {
		t.Errorf("fileReport = %+v, want the off state: this instance has no configured media roots, so no cycle will ever refresh or correct these findings", got)
	}
	// Said out loud, in coldStart's own vocabulary — which file, what was
	// dropped, what happens instead — the same treatment the retired-instance
	// and changed-type cases already get.
	out := h.out.String()
	for _, want := range []string{"level=WARN", stateCacheFileName, "radarr-main", "media_root_map"} {
		if !strings.Contains(out, want) {
			t.Errorf("dropping a cached file report never mentioned %q in the log; a panel that silently loses its findings is the silent-skip shape §2.6 forbids:\n%s", want, out)
		}
	}

	release()
	h.waitReady()

	// And it stays dropped: the first completed cycle does not run the pass
	// either, so nothing would have corrected a restored snapshot.
	after := getStats(t, h)
	if len(after.Instances) != 1 {
		t.Fatalf("instances = %+v after the cycle, want one", after.Instances)
	}
	if got := after.Instances[0]; got.Stale || got.FileReport.Status != "off" || len(got.FileReport.Findings) != 0 {
		t.Errorf("after the first fresh cycle: stale=%v fileReport=%+v — the cycle clears stale unconditionally, so a restored report would be served as this process's own work from here on", got.Stale, got.FileReport)
	}
}

// TestDaemon_ACachedFileReportSurvivesWhenTheInstanceStillHasMediaRoots is the
// negative half: the drop above is about an instance that can no longer produce
// a report, not about file reports in general. An instance whose media_root_map
// is still configured keeps its cached findings — that is the warm start doing
// its job — and nothing is warned about.
func TestDaemon_ACachedFileReportSurvivesWhenTheInstanceStillHasMediaRoots(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{wouldUnmonitorStatefulMovie(1, "Movie")})
	reached, release := holdTheLibraryRead(t, fake)
	root := t.TempDir()
	configPath := writeDaemonConfigWithMediaRoot(t, fake.srv.URL, "/movies", root)
	seedStateCache(t, configPath, time.Date(2026, 8, 22, 9, 30, 0, 0, time.UTC),
		instanceStatsView{
			Name: "radarr-main", Type: "radarr", Total: 996,
			FileReport: fileReportSnapshot{
				Status: "ran", Duplicates: 1, ReclaimableBytes: 4096,
				Findings: []fileReportFindingRecord{{
					Kind: fileKindDuplicate, Path: "/movies/Old/dupe.mkv", Display: "dupe.mkv", Size: 4096,
				}},
			},
		},
	)

	h := startDaemon(t, configPath)
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatalf("the startup scan never reached its library read:\n%s", h.out.String())
	}

	warm := getStats(t, h)
	if len(warm.Instances) != 1 {
		t.Fatalf("instances = %+v, want the cached instance", warm.Instances)
	}
	if got := warm.Instances[0].FileReport; got.Status != "ran" || len(got.Findings) != 1 {
		t.Errorf("fileReport = %+v, want the cached findings kept: this instance is still configured to produce them, and the warm start's whole job is showing the last sweep", got)
	}
	if strings.Contains(h.out.String(), "media_root_map") {
		t.Errorf("the daemon warned about media roots for an instance that has them:\n%s", h.out.String())
	}

	release()
	h.waitReady()
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

	// Round-2 review fix. Dropping the whole cache used to be TOTALLY silent:
	// warmStartFromStateCache returned on `loaded == 0` with no line at any
	// level, so an operator who renamed an instance, changed its type, or
	// removed the last one restarted into a blank dashboard with nothing in the
	// log about the cache at all — not the INFO that says one was found, not a
	// WARN that says why it was dropped. That is the silent-skip shape §2.6
	// exists to forbid, and this file already owns the right vocabulary for it
	// (coldStart: which file, what is wrong, what happens instead).
	out := h.out.String()
	for _, want := range []string{"level=WARN", stateCacheFileName, "matches this config", "cached=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("the daemon read a cache, dropped every entry in it, and its log never said %q — a warm start that silently becomes a cold one is exactly the silent skip this project forbids:\n%s", want, out)
		}
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
	// The one line this adds to a first start, and the brief asked for it at
	// WARN (see TestLoadStateCache_AnAbsentCacheWarnsAndColdStarts). What
	// matters here is that it is the ONLY thing a healthy cold start warns
	// about: the daemon's noise budget is not open for anything else.
	out := h.out.String()
	if !strings.Contains(out, stateCacheFileName) {
		t.Errorf("a first-ever start never mentioned the cache it has not written yet, so nothing explains the empty dashboard:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "level=WARN") && !strings.Contains(line, stateCacheFileName) {
			t.Errorf("a healthy cold start warned about something other than the absent cache:\n%s", line)
		}
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

// --- the page ---------------------------------------------------------------

// TestWebUIPage_WarmStartBannerSaysWhichSweepIsOnScreenAndThatItIsRefreshing is
// the display half of the warm start. Showing a previous run's numbers without
// saying so would be precisely the "glance and trust these numbers" failure the
// UNREACHABLE badge already exists to prevent — so the page carries a prominent
// amber banner naming the sweep's age, and it goes away by itself when the
// first fresh cycle lands.
func TestWebUIPage_WarmStartBannerSaysWhichSweepIsOnScreenAndThatItIsRefreshing(t *testing.T) {
	page := string(webUIPage)
	for _, want := range []string{`id="staleBanner"`, "stale-banner", "showing last sweep from", "refreshing now"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page is missing %q: a dashboard showing a cache must say so, in words, above the numbers", want)
		}
	}

	body := pageFunctionBody(t, "renderStaleBanner")
	if !strings.Contains(body, ".stale") {
		t.Errorf("renderStaleBanner never reads an instance's stale flag:\n%s", body)
	}
	if !strings.Contains(body, "asOf") || !strings.Contains(body, "fmtRelative") {
		t.Errorf("renderStaleBanner does not render asOf through the page's own relative-time formatter, the way every other timestamp on this page is rendered:\n%s", body)
	}
	if !strings.Contains(body, "hidden") {
		t.Errorf("renderStaleBanner never hides the banner, so it would stay on screen after the first fresh cycle:\n%s", body)
	}

	if !strings.Contains(pageFunctionBody(t, "render"), "renderStaleBanner(instances,") {
		t.Error("render() does not update the stale banner from each snapshot")
	}

	// Round-3 review fix, and it is exactly the defect round 2 closed for
	// schedulePoll and renderScanButton, unfixed on this feature's headline UI
	// element: everything above is VOCABULARY. Inverting `if (stale === 0)` —
	// so the banner is hidden precisely when the dashboard is warm and shown
	// when every card is fresh — left the whole suite green, and so did
	// deleting `banner.hidden = false`. Which STATE carries which behavior is
	// the contract, so that is what is pinned.
	fresh := jsBracedBlockAfter(t, body, "if (stale === 0)")
	if !strings.Contains(fresh, "hidden = true") {
		t.Errorf("the `stale === 0` branch is not the one that HIDES the banner; a dashboard with no cached numbers must carry no banner at all:\n%s", fresh)
	}
	if strings.Contains(fresh, "showing last sweep from") {
		t.Errorf("the `stale === 0` branch writes the warm-start copy; a fully fresh dashboard must never claim it is showing a previous sweep:\n%s", fresh)
	}
	tail := body[strings.Index(body, fresh)+len(fresh):]
	if !strings.Contains(tail, "hidden = false") || !strings.Contains(tail, "showing last sweep from") {
		t.Errorf("the warm path (at least one stale instance) does not both show the banner and name the sweep on screen:\n%s", tail)
	}
}

// TestWebUIPage_TheWarmStartBannerOnlyClaimsARefreshWhenOneIsRunning is the
// banner's other honesty rule, and the round-3 review fix for two states in
// which it was asserting something untrue.
//
// "— refreshing now … each shelf updates as the running scan reaches it" is a
// claim about the DAEMON, not about the cache, and there are two ordinary ways
// for it to be false while stale rows are on screen:
//
//   - the daemon is gone. showDisconnected already tears the scan strip down,
//     with a comment saying a progress indicator left up on a dead backend
//     "would be the same lie the 'first sweep in progress' placeholder was
//     fixed for" — but it never touched #staleBanner, so a page that
//     warm-started and then lost its daemon read "disconnected — showing data
//     from 4m ago" in the badge and "refreshing now" directly beneath it.
//   - nothing is running. recordUnreachable deliberately does not clear Stale,
//     so a permanently unreachable instance leaves the banner up — and, with
//     the clause unconditional, leaves "refreshing now" on screen for the whole
//     24h gap between sweeps.
//
// "showing last sweep from <t>" alone is true in every state, so the clause is
// what moves, not the banner.
func TestWebUIPage_TheWarmStartBannerOnlyClaimsARefreshWhenOneIsRunning(t *testing.T) {
	body := pageFunctionBody(t, "renderStaleBanner")
	if !strings.Contains(body, "scanning") {
		t.Fatalf("renderStaleBanner never consults whether a scan is running, so its \"refreshing now\" clause cannot be conditional on one:\n%s", body)
	}
	refreshing := jsBracedBlockAfter(t, body, "if (scanning)")
	if !strings.Contains(refreshing, "refreshing now") {
		t.Errorf("the scanning branch is not the one that claims a refresh is under way:\n%s", refreshing)
	}
	idle := body[strings.Index(body, refreshing)+len(refreshing):]
	if strings.Contains(idle, "refreshing now") {
		t.Errorf("the idle path still claims the numbers are refreshing; with no cycle running that is simply false, and on a permanently unreachable instance it stays false for the whole 24h between sweeps:\n%s", idle)
	}
	if !strings.Contains(idle, "showing last sweep from") {
		t.Errorf("the idle path drops the banner's true half along with its false one; \"showing last sweep from <t>\" is accurate in every state:\n%s", idle)
	}

	// The caller has to hand it the scan state for any of that to mean
	// anything.
	if !strings.Contains(pageFunctionBody(t, "render"), "renderStaleBanner(instances, scanInProgress)") {
		t.Error("render() does not pass the snapshot's scan state to renderStaleBanner, so the banner cannot tell a running sweep from an idle daemon")
	}

	// And the disconnected path, where nothing at all is refreshing.
	dis := pageFunctionBody(t, "showDisconnected")
	if !strings.Contains(dis, "staleBanner") {
		t.Errorf("showDisconnected tears the scan strip down but leaves the warm-start banner claiming a refresh on a backend it just failed to reach:\n%s", dis)
	}
	if !strings.Contains(dis, "staleBanner.hidden = true") {
		t.Errorf("showDisconnected names #staleBanner but never hides it; the badge would read \"disconnected — showing data from 4m ago\" with \"refreshing now\" directly beneath it:\n%s", dis)
	}
}

// TestWebUIPage_TheWarmStartBannerNamesTheOldestNumbersUnderIt is the banner's
// third honesty rule, and the one that only shows up on the ORDINARY mixed
// page: two instances, one restored a few minutes ago and one carrying numbers
// a week-long outage froze (recordUnreachable rewrites the cache entry without
// ever refreshing the figures, so the two ages in one file routinely differ by
// days).
//
// The banner is a SUMMARY of everything beneath it, so it must be as old as the
// oldest thing beneath it. Picking the newest asOf — which is what it did — put
// "showing last sweep from 4m ago" above a card whose own numbers were nine
// days old: the same overstatement stats.go's warmStart fix exists to prevent
// one level down, and a direct violation of this codebase's own rule, "the age
// of the numbers, never of the attempt to refresh them". The per-card
// `.shelf-stale` notes still carry each row's real age, so nothing is hidden by
// this line — but the summary must never read fresher than the worst number it
// summarises.
//
// The mapping is pinned twice. The substring assertions are the half that
// survives a machine with no node on it; the node evaluation below is the half
// that actually RUNS the function, because "the source mentions oldest" and
// "the oldest one is what reaches the screen" are different claims and only the
// second is the contract.
func TestWebUIPage_TheWarmStartBannerNamesTheOldestNumbersUnderIt(t *testing.T) {
	body := pageFunctionBody(t, "renderStaleBanner")
	if strings.Contains(body, "newest") {
		t.Errorf("renderStaleBanner still tracks the NEWEST stale asOf; the page-level banner summarises every stale row beneath it and must name the oldest of them:\n%s", body)
	}
	if !strings.Contains(body, "oldest") {
		t.Errorf("renderStaleBanner never tracks an oldest stale asOf:\n%s", body)
	}

	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; the structural half of this pin ran, the behavioural half needs an interpreter")
	}

	// Three rows, and the ages are chosen to land in three different buckets of
	// fmtRelative's own vocabulary so the assertions cannot confuse them: the
	// freshly restored one ("3m ago"), the one whose *arr has been unreachable
	// since yesterday ("1d ago"), and a row that is NOT stale at all whose
	// timestamp must be ignored outright ("400d ago").
	now := time.Now().UTC()
	instances, err := json.Marshal([]map[string]any{
		{"name": "radarr-main", "stale": true, "asOf": now.Add(-3 * time.Minute).Format(time.RFC3339)},
		{"name": "sonarr-main", "stale": true, "asOf": now.Add(-26 * time.Hour).Format(time.RFC3339)},
		{"name": "radarr-4k", "stale": false, "asOf": now.Add(-400 * 24 * time.Hour).Format(time.RFC3339)},
	})
	if err != nil {
		t.Fatalf("encoding the fixture instances: %v", err)
	}

	out := runPageFunctionUnderNode(t, nodePath, `
var banner = { textContent: "", hidden: null };
var document = {
  getElementById: function (id) {
    if (id !== "staleBanner") throw new Error("renderStaleBanner asked for an unexpected element: " + id);
    return banner;
  }
};
`+pageFunctionSource(t, "text")+pageFunctionSource(t, "fmtRelative")+pageFunctionSource(t, "renderStaleBanner")+`
renderStaleBanner(`+string(instances)+`, true);
console.log(JSON.stringify({ text: banner.textContent, hidden: banner.hidden }));
`)

	var got struct {
		Text   string `json:"text"`
		Hidden any    `json:"hidden"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("could not read what the page rendered (%v):\n%s", err, out)
	}
	if got.Hidden != false {
		t.Errorf("hidden = %v with two stale rows on screen, want false", got.Hidden)
	}
	if !strings.Contains(got.Text, "1d ago") {
		t.Errorf("the banner reads %q, want it to name the OLDEST stale numbers on the page (1d ago): a summary line that is newer than the worst row under it makes the data look fresher than it is", got.Text)
	}
	if strings.Contains(got.Text, "3m ago") {
		t.Errorf("the banner reads %q, which is the age of the FRESHEST stale row; the card beside it is a day older and the banner is the line the operator reads first", got.Text)
	}
	if strings.Contains(got.Text, "400d ago") {
		t.Errorf("the banner reads %q, so it took its age from a row that is not stale at all; only cached rows have an age to report", got.Text)
	}
}

// pageFunctionSource returns one page function as EXECUTABLE source.
// pageFunctionBody starts one byte into the declaration (its own boundary
// search needs the offset), which is invisible to a substring assertion and
// fatal to an interpreter, so the missing `f` goes back on here.
func pageFunctionSource(t *testing.T, name string) string {
	t.Helper()
	return "\n f" + pageFunctionBody(t, name) + "\n"
}

// runPageFunctionUnderNode runs script under node and returns its stdout. It is
// the only way this project can assert what the embedded page DOES rather than
// what it says — there is no browser in this test suite and no build step to
// add one — so the script it is given stubs the two globals a page function
// touches (document, and whatever the function calls) and prints its result as
// JSON.
func runPageFunctionUnderNode(t *testing.T, nodePath, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "page-fragment.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatalf("writing the extracted page fragment: %v", err)
	}
	out, err := exec.Command(nodePath, path).CombinedOutput()
	if err != nil {
		t.Fatalf("node could not run the extracted page fragment (%v):\n%s\n--- script ---\n%s", err, out, script)
	}
	return string(out)
}

// TestWebUIPage_AWarmStaleShelfCardSaysSoOnTheCardItself is the per-instance
// half, following ReverseAsOf's own convention (the reverse panel's "showing
// last complete sweep from <time>" notice): the page-level banner says the
// dashboard as a whole is warm, and the card says which numbers are the cached
// ones — they are not always the same set, since the first fresh cycle clears
// instances one at a time as it reaches them.
func TestWebUIPage_AWarmStaleShelfCardSaysSoOnTheCardItself(t *testing.T) {
	page := string(webUIPage)
	if !strings.Contains(page, "shelf-stale") {
		t.Error("the page has no per-card stale marker class")
	}
	body := pageFunctionBody(t, "updateShelfCard")
	if !strings.Contains(body, "inst.stale") {
		t.Errorf("updateShelfCard never reads the instance's own stale flag:\n%s", body)
	}
	if !strings.Contains(body, "inst.asOf") {
		t.Errorf("updateShelfCard never renders the cached numbers' own age:\n%s", body)
	}
	// The bar itself must keep showing the cached numbers: blanking it would
	// throw away the very data the warm start exists to show.
	if !strings.Contains(body, "els.rest.style.width = pctStr;") {
		t.Errorf("updateShelfCard no longer sets the bar's width unconditionally; a stale card must still show its numbers:\n%s", body)
	}
}

// TestStatsStore_WarmStart_KeepsTheCachedNumbersOwnAgeAcrossRepeatedRestarts is
// a self-review finding, and it is an honesty bug rather than a crash.
//
// The cache is rewritten at the end of every full cycle, INCLUDING one that
// could not reach an instance — whose numbers are therefore still the ones some
// earlier run recorded, and are written out again carrying stale: true and their
// original asOf. Taking the file's own writtenAt for those would move their
// apparent age forward at every restart, so a daemon that has been unable to
// reach an instance for a week would say "showing last sweep from just now"
// over week-old numbers. The instance's OWN asOf wins whenever the cache
// already knew it was stale — the same rule ReverseAsOf follows: the age of the
// numbers, never of the attempt to refresh them.
func TestStatsStore_WarmStart_KeepsTheCachedNumbersOwnAgeAcrossRepeatedRestarts(t *testing.T) {
	swept := time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC)
	rewritten := time.Date(2026, 8, 22, 17, 0, 0, 0, time.UTC)
	store := newStatsStore(true)

	store.warmStart([]instanceStatsView{
		// Already stale in the cache: written out again by a later cycle that
		// never reached this instance.
		{Name: "radarr-unreachable", Type: "radarr", Total: 996, Stale: true, AsOf: &swept},
		// Recorded fresh by the cycle that wrote this cache, and never
		// recorded again since: the *arr went offline at 09:00 and every sweep
		// after it took recordUnreachable's path, which by design touches
		// neither Stale nor the numbers — so the entry keeps being rewritten
		// with stale:false over 09:00's figures for as long as the outage
		// lasts. This is the ORDINARY shape of the same lie the row above
		// covers, and the file's writtenAt is the wrong answer for it too:
		// LastRun is the only field that says when these numbers were
		// produced (recordInstance sets it under decisionsRan, beside them).
		{Name: "radarr-offline-since", Type: "radarr", Total: 996, LastRun: &swept},
		// Recorded fresh by the cycle that wrote this cache, by a cache old
		// enough to predate LastRun being written out at all: nothing states
		// these numbers' own age, so the file's is the closest truth left.
		{Name: "radarr-main", Type: "radarr", Total: 42},
	}, rewritten)

	got := map[string]*time.Time{}
	for _, inst := range store.snapshot().Instances {
		if !inst.Stale {
			t.Fatalf("%s came back not stale; every warm-started instance is", inst.Name)
		}
		got[inst.Name] = inst.AsOf
	}
	if got["radarr-unreachable"] == nil || !got["radarr-unreachable"].Equal(swept) {
		t.Errorf("asOf = %v for numbers the cache itself already knew were stale, want their own age %s — not the file's rewrite time", got["radarr-unreachable"], swept)
	}
	if got["radarr-offline-since"] == nil || !got["radarr-offline-since"].Equal(swept) {
		t.Errorf("asOf = %v for numbers last actually recorded at %s, want that — the banner would otherwise read \"showing last sweep from just now\" over figures a week-long outage has frozen, which is the age of the ATTEMPT to refresh them, never of the numbers",
			got["radarr-offline-since"], swept)
	}
	if got["radarr-main"] == nil || !got["radarr-main"].Equal(rewritten) {
		t.Errorf("asOf = %v for numbers this cache recorded fresh with no LastRun of their own, want the cache's own write time %s", got["radarr-main"], rewritten)
	}
}

// TestActionScope_CarriesNoProgressHandle is the separation the live-progress
// surface owes the action system. A human-clicked action drives the SAME
// decision engine, scoped to one item — and it is not a cycle: it must not
// appear on the dashboard's scan strip, where it would read as a sweep nobody
// started and would replace whatever a real running cycle was reporting.
//
// Two independent things make that true, which is why both are asserted: the
// scope an action builds carries no handle at all, and the store drops any
// publish that arrives while no cycle is in progress.
func TestActionScope_CarriesNoProgressHandle(t *testing.T) {
	if scope := actionScope(7, nil); scope.progress != nil {
		t.Error("a GUI action's scope carries a progress handle: a human clicking one finding would publish into the live scan strip as though a cycle were running")
	}

	store := newStatsStore(true)
	store.progressFor("radarr-main").stage(scanStageEvaluating, 10)
	if scan := store.snapshot().Scan; scan.InProgress || len(scan.Instances) != 0 {
		t.Errorf("scan = %+v, want empty: a publish arriving while no cycle is running must be dropped, never resurrect the surface", scan)
	}
}

// TestDaemon_TheCacheIsWrittenBeforeTheCycleSaysItIsComplete is a self-review
// finding with teeth: it was found by an intermittently failing -race run, not
// by reading the code.
//
// The write used to happen AFTER "reconciliation sweep complete", which made
// the same idle cycle print different output run to run — the write lands on
// the loop goroutine while whatever is reading the log has already moved past
// the completion line — and broke
// TestDaemon_IdleCycleWithAWebUIPoller_StaysByteIdenticalToNoPoller roughly one
// run in five. It is also just wrong: that line is this daemon's statement that
// everything belonging to the cycle is done.
func TestDaemon_TheCacheIsWrittenBeforeTheCycleSaysItIsComplete(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{wouldUnmonitorStatefulMovie(1, "Movie")})
	h := startDaemon(t, writeDaemonConfig(t, "radarr", fake.srv.URL, true, "debug", "1h", "45s"))
	h.waitReady()
	mark := h.mark()

	h.clock.Advance(time.Hour)
	h.awaitLogCount("reconciliation sweep complete", 1)
	cycle := h.since(mark)

	wrote := strings.Index(cycle, "warm-start cache written")
	complete := strings.Index(cycle, "reconciliation sweep complete")
	if wrote == -1 {
		t.Fatalf("the sweep logged no cache write at all:\n%s", cycle)
	}
	if complete == -1 || wrote > complete {
		t.Errorf("the cache write is logged after the sweep's completion line (write at %d, complete at %d); everything belonging to a cycle must land before the line that says the cycle is over, or the same cycle prints different output run to run:\n%s", wrote, complete, cycle)
	}
}
