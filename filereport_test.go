package main

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// newFileReportTestLogger mirrors newDecisionTestLogger (decision_test.go):
// same TextHandler setup, a second name so filereport_test.go's intent reads
// clearly at each call site.
func newFileReportTestLogger(level slog.Level) (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level}))
	return logger, &buf
}

// writeFixtureFiles creates root and every relative path under it (empty
// files; classification never reads file contents), returning the absolute
// disk paths in the same order.
func writeFixtureFiles(t *testing.T, root string, relPaths ...string) []string {
	t.Helper()
	abs := make([]string, len(relPaths))
	for i, rel := range relPaths {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		abs[i] = p
	}
	return abs
}

// assertFileReportIdentity is the exhaustive per-file check binding
// controller resolution 6 requires: every video-or-not file the walk visited
// resolves to EXACTLY one of the five identity terms, so nothing is silently
// double-counted or dropped. It is driven off the outcome's own bookkeeping
// counters rather than re-walking the disk, since the outcome IS the claim
// under test.
func assertFileReportIdentity(t *testing.T, totalFilesOnDisk int, o fileReportRootOutcome) {
	t.Helper()
	dup, orphan := 0, 0
	for _, f := range o.findings {
		switch f.kind {
		case fileKindDuplicate:
			dup++
		case fileKindOrphan:
			orphan++
		default:
			t.Fatalf("finding has unexpected kind %q", f.kind)
		}
	}
	sum := o.seenTracked + dup + orphan + o.seenSkippedByRule + o.seenSkippedUntrusted
	if sum != totalFilesOnDisk {
		t.Errorf("identity broken: tracked(%d)+duplicate(%d)+orphan(%d)+skipped-by-rule(%d)+skipped-untrusted(%d) = %d, want %d files seen on disk",
			o.seenTracked, dup, orphan, o.seenSkippedByRule, o.seenSkippedUntrusted, sum, totalFilesOnDisk)
	}
}

// --- evaluateFileReportRoot: the walk + classification, happy path ---------

func TestEvaluateFileReportRoot_ClassifiesEveryIdentityTerm(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFiles(t, dir,
		"Movie A (2020)/Movie A (2020).mkv",     // tracked
		"Movie A (2020)/Movie A (2020) (2).mkv", // duplicate
		"Movie A (2020)/poster.jpg",             // skipped-by-rule: extension
		"Movie A (2020)/Movie A-sample.mkv",     // skipped-by-rule: trailer/sample
		"Movie A (2020)/Trailers/clip.mkv",      // skipped-by-rule: extras
		"Stray Folder/Something.mkv",            // orphan
	)
	trackedFile := filepath.Join(dir, "Movie A (2020)/Movie A (2020).mkv")

	root := mediaRoot{arrPath: "/movies", diskPath: dir}
	set := instanceTrackedSet{
		files:   map[string]bool{trackedFile: true},
		folders: map[string]string{filepath.Join(dir, "Movie A (2020)"): "Movie A (2020)"},
	}

	logger, _ := newFileReportTestLogger(slog.LevelDebug)
	outcome := evaluateFileReportRoot(context.Background(), logger, slog.LevelInfo, Instance{Name: "radarr-main", Type: "radarr"}, root, set)

	if outcome.skipped {
		t.Fatalf("outcome unexpectedly skipped: %+v", outcome)
	}
	if outcome.seenTracked != 1 {
		t.Errorf("seenTracked = %d, want 1", outcome.seenTracked)
	}
	var dupPaths, orphanPaths []string
	for _, f := range outcome.findings {
		if f.kind == fileKindDuplicate {
			dupPaths = append(dupPaths, f.diskPath)
		} else {
			orphanPaths = append(orphanPaths, f.diskPath)
		}
	}
	if len(dupPaths) != 1 || !strings.Contains(dupPaths[0], "Movie A (2020) (2).mkv") {
		t.Errorf("duplicate findings = %v, want exactly the extra copy", dupPaths)
	}
	if len(orphanPaths) != 1 || !strings.Contains(orphanPaths[0], "Stray Folder") {
		t.Errorf("orphan findings = %v, want exactly the stray file", orphanPaths)
	}
	if outcome.seenSkippedByRule != 3 {
		t.Errorf("seenSkippedByRule = %d, want 3 (extension, trailer/sample, extras)", outcome.seenSkippedByRule)
	}
	if got := outcome.skipReasons[FileSkipReasonExtension]; got != 1 {
		t.Errorf("skipReasons[extension] = %d, want 1", got)
	}
	if got := outcome.skipReasons[FileSkipReasonTrailerOrSample]; got != 1 {
		t.Errorf("skipReasons[trailer/sample] = %d, want 1", got)
	}
	if got := outcome.skipReasons[FileSkipReasonExtrasDir]; got != 1 {
		t.Errorf("skipReasons[extras] = %d, want 1", got)
	}
	assertFileReportIdentity(t, 6, outcome)
}

// TestEvaluateFileReportRoot_FindingsCarryRootRelativeDisplayPath pins that
// the walk itself (not just fileReportSnapshotFrom downstream) sets
// displayPath on every finding it produces, using the exact `rel` fs.WalkDir
// already computed rather than re-deriving it a second time from diskPath.
func TestEvaluateFileReportRoot_FindingsCarryRootRelativeDisplayPath(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFiles(t, dir,
		"Movie A (2020)/Movie A (2020).mkv",     // tracked
		"Movie A (2020)/Movie A (2020) (2).mkv", // duplicate
		"Stray Folder/Something.mkv",            // orphan
	)
	trackedFile := filepath.Join(dir, "Movie A (2020)/Movie A (2020).mkv")

	root := mediaRoot{arrPath: "/movies", diskPath: dir}
	set := instanceTrackedSet{
		files:   map[string]bool{trackedFile: true},
		folders: map[string]string{filepath.Join(dir, "Movie A (2020)"): "Movie A (2020)"},
	}

	logger, _ := newFileReportTestLogger(slog.LevelDebug)
	outcome := evaluateFileReportRoot(context.Background(), logger, slog.LevelInfo, Instance{Name: "radarr-main", Type: "radarr"}, root, set)
	if outcome.skipped {
		t.Fatalf("outcome unexpectedly skipped: %+v", outcome)
	}

	rootName := filepath.Base(dir)
	wantDupDisplay := filepath.Join(rootName, "Movie A (2020)", "Movie A (2020) (2).mkv")
	wantOrphanDisplay := filepath.Join(rootName, "Stray Folder", "Something.mkv")
	for _, f := range outcome.findings {
		var want string
		switch f.kind {
		case fileKindDuplicate:
			want = wantDupDisplay
		case fileKindOrphan:
			want = wantOrphanDisplay
		}
		if f.displayPath != want {
			t.Errorf("finding kind=%s displayPath = %q, want %q", f.kind, f.displayPath, want)
		}
		if f.displayPath == f.diskPath {
			t.Errorf("finding kind=%s displayPath equals the full diskPath (%q) — it must be root-relative", f.kind, f.diskPath)
		}
	}
}

func TestEvaluateFileReportRoot_CleanRootProducesNoFindings(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFiles(t, dir, "Movie A (2020)/Movie A (2020).mkv")
	trackedFile := filepath.Join(dir, "Movie A (2020)/Movie A (2020).mkv")

	root := mediaRoot{arrPath: "/movies", diskPath: dir}
	set := instanceTrackedSet{
		files:   map[string]bool{trackedFile: true},
		folders: map[string]string{filepath.Join(dir, "Movie A (2020)"): "Movie A (2020)"},
	}
	logger, _ := newFileReportTestLogger(slog.LevelDebug)
	outcome := evaluateFileReportRoot(context.Background(), logger, slog.LevelInfo, Instance{Name: "radarr-main", Type: "radarr"}, root, set)
	if outcome.skipped {
		t.Fatalf("outcome unexpectedly skipped: %+v", outcome)
	}
	if len(outcome.findings) != 0 {
		t.Errorf("findings = %+v, want none for a clean library", outcome.findings)
	}
	assertFileReportIdentity(t, 1, outcome)
}

func TestEvaluateFileReportRoot_GroupCountReflectsSiblingDuplicates(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFiles(t, dir,
		"Movie A (2020)/Movie A (2020).mkv",
		"Movie A (2020)/Movie A (2020) (2).mkv",
		"Movie A (2020)/Movie A (2020) (3).mkv",
	)
	trackedFile := filepath.Join(dir, "Movie A (2020)/Movie A (2020).mkv")
	root := mediaRoot{arrPath: "/movies", diskPath: dir}
	set := instanceTrackedSet{
		files:   map[string]bool{trackedFile: true},
		folders: map[string]string{filepath.Join(dir, "Movie A (2020)"): "Movie A (2020)"},
	}
	logger, _ := newFileReportTestLogger(slog.LevelDebug)
	outcome := evaluateFileReportRoot(context.Background(), logger, slog.LevelInfo, Instance{Name: "radarr-main", Type: "radarr"}, root, set)
	if len(outcome.findings) != 2 {
		t.Fatalf("findings = %+v, want 2 duplicates", outcome.findings)
	}
	for _, f := range outcome.findings {
		if f.groupCount != 2 {
			t.Errorf("finding %+v groupCount = %d, want 2", f, f.groupCount)
		}
	}
}

// TestEvaluateFileReportRoot_GroupCountDoesNotCollideAcrossSameTitledMovies
// is the [IMPORTANT] fix for groupKey's title-collision bug: two DISTINCT
// movies sharing a display title (a common real-world case — same-titled
// remakes, e.g. two "Dune" entries) were keyed on title+group alone, so
// their extra files were counted as SIBLINGS of each other's group even
// though they belong to different tracked folders. groupCount is part of
// the plan's frozen vocabulary and the one number a human uses to judge how
// big a problem is — a wrong count actively misinforms.
func TestEvaluateFileReportRoot_GroupCountDoesNotCollideAcrossSameTitledMovies(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFiles(t, dir,
		"Dune (1984)/Dune (1984).mkv",
		"Dune (1984)/Dune (1984) (2).mkv", // duplicate, belongs to the 1984 folder only
		"Dune (2021)/Dune (2021).mkv",
		"Dune (2021)/Dune (2021) (2).mkv", // duplicate, belongs to the 2021 folder only
	)
	set := instanceTrackedSet{
		files: map[string]bool{
			filepath.Join(dir, "Dune (1984)/Dune (1984).mkv"): true,
			filepath.Join(dir, "Dune (2021)/Dune (2021).mkv"): true,
		},
		folders: map[string]string{
			// Both movies display under the SAME title, "Dune" — the
			// collision the bug hit.
			filepath.Join(dir, "Dune (1984)"): "Dune",
			filepath.Join(dir, "Dune (2021)"): "Dune",
		},
	}
	root := mediaRoot{arrPath: "/movies", diskPath: dir}
	logger, _ := newFileReportTestLogger(slog.LevelDebug)
	outcome := evaluateFileReportRoot(context.Background(), logger, slog.LevelInfo, Instance{Name: "radarr-main", Type: "radarr"}, root, set)
	if len(outcome.findings) != 2 {
		t.Fatalf("findings = %+v, want 2 duplicates", outcome.findings)
	}
	for _, f := range outcome.findings {
		if f.groupCount != 1 {
			t.Errorf("finding %+v groupCount = %d, want 1: two different movies sharing a title must never be counted as siblings of each other", f, f.groupCount)
		}
	}
}

// TestEvaluateFileReportRoot_GroupCountDoesNotCollideAcrossSameTitledSeries
// is the Sonarr twin, with the SxxEyy display label ALSO colliding (both
// series' extra files parse to "S01E01") so only folder-keying can tell them
// apart.
func TestEvaluateFileReportRoot_GroupCountDoesNotCollideAcrossSameTitledSeries(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFiles(t, dir,
		"Show A/Season 01/Show.S01E01.mkv",
		"Show A/Season 01/Show.S01E01 (2).mkv",
		"Show B/Season 01/Show.S01E01.mkv",
		"Show B/Season 01/Show.S01E01 (2).mkv",
	)
	set := instanceTrackedSet{
		files: map[string]bool{
			filepath.Join(dir, "Show A/Season 01/Show.S01E01.mkv"): true,
			filepath.Join(dir, "Show B/Season 01/Show.S01E01.mkv"): true,
		},
		folders: map[string]string{
			filepath.Join(dir, "Show A"): "Show", // shared title across two series
			filepath.Join(dir, "Show B"): "Show",
		},
		seriesFolder: map[string]bool{
			filepath.Join(dir, "Show A"): true,
			filepath.Join(dir, "Show B"): true,
		},
	}
	root := mediaRoot{arrPath: "/tv_shows", diskPath: dir}
	logger, _ := newFileReportTestLogger(slog.LevelDebug)
	outcome := evaluateFileReportRoot(context.Background(), logger, slog.LevelInfo, Instance{Name: "sonarr-main", Type: "sonarr"}, root, set)
	if len(outcome.findings) != 2 {
		t.Fatalf("findings = %+v, want 2 duplicates", outcome.findings)
	}
	for _, f := range outcome.findings {
		if f.groupCount != 1 {
			t.Errorf("finding %+v groupCount = %d, want 1: two different series sharing a title AND the same episode label must never be counted as siblings of each other", f, f.groupCount)
		}
	}
}

func TestEvaluateFileReportRoot_MismatchedSeasonWalkedFileIsSkippedUntrusted(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFiles(t, dir,
		"Show/Season 01/Show.S01E05.mkv",     // tracked
		"Show/Season 01/Show.S01E05 (2).mkv", // skipped-untrusted: season 1 is mismatched (by folder)
		"Show/Season 02/Show.S02E01.mkv",     // duplicate: season 2 is not mismatched (by folder)
	)
	root := mediaRoot{arrPath: "/tv_shows", diskPath: dir}
	set := instanceTrackedSet{
		files:             map[string]bool{filepath.Join(dir, "Show/Season 01/Show.S01E05.mkv"): true},
		folders:           map[string]string{filepath.Join(dir, "Show"): "Show"},
		seriesFolder:      map[string]bool{filepath.Join(dir, "Show"): true},
		mismatchedSeasons: map[string]map[int]bool{filepath.Join(dir, "Show"): {1: true}},
	}
	logger, _ := newFileReportTestLogger(slog.LevelDebug)
	outcome := evaluateFileReportRoot(context.Background(), logger, slog.LevelInfo, Instance{Name: "sonarr-main", Type: "sonarr"}, root, set)

	if outcome.skipped {
		t.Fatalf("outcome unexpectedly skipped: %+v", outcome)
	}
	if outcome.seenSkippedUntrusted != 1 {
		t.Errorf("seenSkippedUntrusted = %d, want 1", outcome.seenSkippedUntrusted)
	}
	if got := outcome.skipReasons[FileSkipReasonMismatchedSeason]; got != 1 {
		t.Errorf("skipReasons[mismatched season] = %d, want 1", got)
	}
	if len(outcome.findings) != 1 || outcome.findings[0].kind != fileKindDuplicate {
		t.Errorf("findings = %+v, want exactly one duplicate (season 2, not excluded)", outcome.findings)
	}
	assertFileReportIdentity(t, 3, outcome)
}

// TestEvaluateFileReportRoot_MismatchedSeasonExclusionIsByFolderNotFilename
// is the walk-level pin for binding controller resolution 5's "never
// guessed" requirement, covering both shapes the classify-level tests above
// pin individually: (1) a file physically inside the distrusted "Season 01"
// folder but misleadingly NAMED as season 2 must still be excluded, and (2) a
// season-folders-disabled series (files flat in the series folder) with any
// mismatched season must withhold ALL its extra files from duplicate
// candidacy, since none of their seasons can be determined from location.
func TestEvaluateFileReportRoot_MismatchedSeasonExclusionIsByFolderNotFilename(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFiles(t, dir,
		"Show A/Season 01/Show A.S01E05.mkv", // tracked
		"Show A/Season 01/Show A.S02E01.mkv", // skipped-untrusted: folder says season 1 (mismatched), filename lies
		"Flat Show/Flat Show.S01E01.mkv",     // tracked
		"Flat Show/Flat Show.S02E09.mkv",     // skipped-untrusted: season folders off, season 1 of this series is mismatched, cannot rule this file out
	)
	root := mediaRoot{arrPath: "/tv_shows", diskPath: dir}
	set := instanceTrackedSet{
		files: map[string]bool{
			filepath.Join(dir, "Show A/Season 01/Show A.S01E05.mkv"): true,
			filepath.Join(dir, "Flat Show/Flat Show.S01E01.mkv"):     true,
		},
		folders: map[string]string{
			filepath.Join(dir, "Show A"):    "Show A",
			filepath.Join(dir, "Flat Show"): "Flat Show",
		},
		seriesFolder: map[string]bool{
			filepath.Join(dir, "Show A"):    true,
			filepath.Join(dir, "Flat Show"): true,
		},
		mismatchedSeasons: map[string]map[int]bool{
			filepath.Join(dir, "Show A"):    {1: true},
			filepath.Join(dir, "Flat Show"): {1: true},
		},
	}
	logger, _ := newFileReportTestLogger(slog.LevelDebug)
	outcome := evaluateFileReportRoot(context.Background(), logger, slog.LevelInfo, Instance{Name: "sonarr-main", Type: "sonarr"}, root, set)

	if outcome.skipped {
		t.Fatalf("outcome unexpectedly skipped: %+v", outcome)
	}
	if len(outcome.findings) != 0 {
		t.Errorf("findings = %+v, want none: both extra files must be withheld as skipped-untrusted, never guessed as duplicates", outcome.findings)
	}
	if outcome.seenSkippedUntrusted != 2 {
		t.Errorf("seenSkippedUntrusted = %d, want 2", outcome.seenSkippedUntrusted)
	}
	assertFileReportIdentity(t, 4, outcome)
}

// --- sampleFileReportPaths: deterministic stride sampling -------------------

// TestSampleFileReportPaths_StridesAcrossTheSortedSliceNotJustTheHead is the
// [FIX] pin: sampleFileReportPaths used to head-slice the sorted tracked-path
// list, so heuristic (b)'s up-to-100-file sample was always the
// alphabetically-first cluster. A partially-mounted share does not fail
// randomly — a mergerfs branch going down, or a per-letter split mount,
// drops a contiguous ALPHABETICAL subtree — so a head-slice sample could
// pass heuristic (b) cleanly (every one of the first 100 alphabetically
// sorted paths present) while the rest of the library, past the sample
// window, is entirely missing. Stride sampling (the string twin of
// sampleEveryKth, decision.go) draws evenly from across the WHOLE sorted
// slice, so a share missing everything past some alphabetical point is
// caught instead of silently passing.
func TestSampleFileReportPaths_StridesAcrossTheSortedSliceNotJustTheHead(t *testing.T) {
	var sorted []string
	for i := 0; i < 500; i++ {
		sorted = append(sorted, fmt.Sprintf("/data/Movies/a%04d/movie.mkv", i))
	}
	for i := 0; i < 500; i++ {
		sorted = append(sorted, fmt.Sprintf("/data/Movies/z%04d/movie.mkv", i))
	}

	sample := sampleFileReportPaths(sorted, 100)
	if len(sample) != 100 {
		t.Fatalf("len(sample) = %d, want 100", len(sample))
	}
	sawA, sawZ := false, false
	for _, p := range sample {
		if strings.HasPrefix(p, "/data/Movies/a") {
			sawA = true
		}
		if strings.HasPrefix(p, "/data/Movies/z") {
			sawZ = true
		}
	}
	if !sawA || !sawZ {
		t.Errorf("sample drawn from a%%/z%% split = %v, want paths from BOTH halves of the sorted slice, not just the alphabetically-first cluster", sample)
	}
}

// TestSampleFileReportPaths_SliceAtOrBelowCapReturnedUnchanged pins the
// cap branch (fileReportMountSampleSize's "nothing to sample down" case) that
// was previously exercised only indirectly, by every evaluateFileReportRoot
// test's small fixture counts, never as a direct assertion on the function
// itself.
func TestSampleFileReportPaths_SliceAtOrBelowCapReturnedUnchanged(t *testing.T) {
	sorted := []string{"/a", "/b", "/c"}
	sample := sampleFileReportPaths(sorted, 100)
	if len(sample) != len(sorted) {
		t.Fatalf("len(sample) = %d, want %d: a slice at or below the cap must be returned unchanged", len(sample), len(sorted))
	}
	for i := range sorted {
		if sample[i] != sorted[i] {
			t.Errorf("sample[%d] = %q, want %q", i, sample[i], sorted[i])
		}
	}
}

// --- mount-problem heuristic (binding controller resolution 4) -------------

func TestEvaluateFileReportRoot_MissingRootAborts(t *testing.T) {
	root := mediaRoot{arrPath: "/movies", diskPath: filepath.Join(t.TempDir(), "does-not-exist")}
	set := instanceTrackedSet{files: map[string]bool{}, folders: map[string]string{}}
	logger, buf := newFileReportTestLogger(slog.LevelDebug)
	outcome := evaluateFileReportRoot(context.Background(), logger, slog.LevelInfo, Instance{Name: "radarr-main", Type: "radarr"}, root, set)
	if !outcome.skipped {
		t.Fatal("a missing root must abort (mount-problem heuristic a)")
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a WARN naming the root:\n%s", buf.String())
	}
}

func TestEvaluateFileReportRoot_UnreadableRootAborts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits behave differently on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permission bits")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	root := mediaRoot{arrPath: "/movies", diskPath: dir}
	set := instanceTrackedSet{files: map[string]bool{}, folders: map[string]string{}}
	logger, buf := newFileReportTestLogger(slog.LevelDebug)
	outcome := evaluateFileReportRoot(context.Background(), logger, slog.LevelInfo, Instance{Name: "radarr-main", Type: "radarr"}, root, set)
	if !outcome.skipped {
		t.Fatal("an unreadable root must abort (mount-problem heuristic a)")
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a WARN naming the root:\n%s", buf.String())
	}
}

func TestEvaluateFileReportRoot_MostTrackedFilesMissingAborts(t *testing.T) {
	dir := t.TempDir()
	// Only one of ten tracked files actually exists on disk: a half-mounted
	// share, not a healthy library missing the occasional file.
	present := writeFixtureFiles(t, dir, "Movie 0/Movie 0.mkv")
	files := map[string]bool{present[0]: true}
	for i := 1; i < 10; i++ {
		files[filepath.Join(dir, "Movie X", fmt.Sprintf("missing%d.mkv", i))] = true
	}
	root := mediaRoot{arrPath: "/movies", diskPath: dir}
	set := instanceTrackedSet{files: files, folders: map[string]string{}}
	logger, buf := newFileReportTestLogger(slog.LevelDebug)
	outcome := evaluateFileReportRoot(context.Background(), logger, slog.LevelInfo, Instance{Name: "radarr-main", Type: "radarr"}, root, set)
	if !outcome.skipped {
		t.Fatal("a root where 90%+ of sampled tracked files are missing must abort (mount-problem heuristic b)")
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a WARN naming the root:\n%s", buf.String())
	}
}

func TestEvaluateFileReportRoot_MostlyPresentTrackedFilesDoNotAbort(t *testing.T) {
	dir := t.TempDir()
	var rel []string
	for i := 0; i < 10; i++ {
		rel = append(rel, filepath.Join("Movie", "f"+string(rune('0'+i))+".mkv"))
	}
	present := writeFixtureFiles(t, dir, rel...)
	files := map[string]bool{}
	for _, p := range present {
		files[p] = true
	}
	// One tracked file (the 11th) is legitimately missing — below the 10%
	// tolerance line, and must NOT trip the heuristic.
	files[filepath.Join(dir, "Movie", "missing-one.mkv")] = true

	root := mediaRoot{arrPath: "/movies", diskPath: dir}
	set := instanceTrackedSet{files: files, folders: map[string]string{filepath.Join(dir, "Movie"): "Movie"}}
	logger, _ := newFileReportTestLogger(slog.LevelDebug)
	outcome := evaluateFileReportRoot(context.Background(), logger, slog.LevelInfo, Instance{Name: "radarr-main", Type: "radarr"}, root, set)
	if outcome.skipped {
		t.Fatalf("one legitimately missing file out of eleven must not trip the heuristic: %+v", outcome)
	}
}

func TestEvaluateFileReportRoot_ZeroTrackedFilesSkipsHeuristicB(t *testing.T) {
	// A root the instance maps but currently tracks nothing under (e.g. a
	// brand-new empty library) must not be treated as a mount problem.
	dir := t.TempDir()
	root := mediaRoot{arrPath: "/movies", diskPath: dir}
	set := instanceTrackedSet{files: map[string]bool{}, folders: map[string]string{}}
	logger, _ := newFileReportTestLogger(slog.LevelDebug)
	outcome := evaluateFileReportRoot(context.Background(), logger, slog.LevelInfo, Instance{Name: "radarr-main", Type: "radarr"}, root, set)
	if outcome.skipped {
		t.Fatalf("zero tracked files must not itself trip the heuristic: %+v", outcome)
	}
}

// TestEvaluateFileReportRoot_TrackedFoldersButZeroTrackedFilesAborts pins the
// mount-problem heuristic's missing symmetric guard: heuristics (b) and (c)
// are both gated on len(tracked) > 0, so an EMPTY tracked-FILE set (e.g.
// every movieFile.path was absent or wrongly named — a systemic bug, not a
// mount problem) disables the whole safeguard while set.folders stays
// populated from movie.path. Without this guard, every real file in those
// folders matches its containing folder but no tracked file and is reported
// as kind=duplicate — the whole library, confidently. A root that tracks
// FOLDERS (movies/series are known to live under it) but has ZERO tracked
// FILES under it is exactly as untrustworthy as heuristic (b)'s missing-
// files case, so it must abort the same way, before ever walking.
func TestEvaluateFileReportRoot_TrackedFoldersButZeroTrackedFilesAborts(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFiles(t, dir, "Movie A/Movie A.mkv")
	root := mediaRoot{arrPath: "/movies", diskPath: dir}
	set := instanceTrackedSet{
		files:   map[string]bool{}, // every movieFile.path failed to map/read
		folders: map[string]string{filepath.Join(dir, "Movie A"): "Movie A"},
	}
	logger, buf := newFileReportTestLogger(slog.LevelDebug)
	outcome := evaluateFileReportRoot(context.Background(), logger, slog.LevelInfo, Instance{Name: "radarr-main", Type: "radarr"}, root, set)
	if !outcome.skipped {
		t.Fatal("a root tracking folders but zero files under it must abort rather than report every real file as a duplicate")
	}
	if len(outcome.findings) != 0 {
		t.Errorf("an aborted root must report nothing: %+v", outcome.findings)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a WARN naming the root:\n%s", buf.String())
	}
}

// TestEvaluateFileReportRoot_TrackedFilesButZeroVideoFilesOnWalkAborts must
// actually exercise heuristic (c), not (b): the tracked file has to EXIST on
// disk (so heuristic (b)'s "≥90% of sampled tracked files present" check
// legitimately passes and lets the walk run at all) but under an
// unrecognized extension, so classifyFileReportPath's extension check (which
// runs BEFORE the set.files lookup, by design — resolution 6) filters it out
// before it can ever be recognized as fileKindTracked, and the walk
// therefore finds zero files of any recognized video kind. This is the real
// scenario heuristic (c) exists for: a library whose real media files all
// carry an extension outside the frozen 6-item videoExtensions set
// (.m4v/.mov/.iso), not merely "the file happens to be missing" (which is
// heuristic (b)'s job, already covered by
// TestEvaluateFileReportRoot_MostTrackedFilesMissingAborts).
func TestEvaluateFileReportRoot_TrackedFilesButZeroVideoFilesOnWalkAborts(t *testing.T) {
	dir := t.TempDir()
	present := writeFixtureFiles(t, dir, "Movie A/Movie A.m4v")
	files := map[string]bool{present[0]: true}
	root := mediaRoot{arrPath: "/movies", diskPath: dir}
	set := instanceTrackedSet{files: files, folders: map[string]string{filepath.Join(dir, "Movie A"): "Movie A"}}
	logger, buf := newFileReportTestLogger(slog.LevelDebug)
	outcome := evaluateFileReportRoot(context.Background(), logger, slog.LevelInfo, Instance{Name: "radarr-main", Type: "radarr"}, root, set)
	if !outcome.skipped {
		t.Fatal("a root tracking N>0 files (present on disk) with zero RECOGNIZED video files found on walk must abort (mount-problem heuristic c)")
	}
	if !strings.Contains(buf.String(), "it tracks files but the walk found zero video files of any kind") {
		t.Errorf("expected heuristic (c)'s own WARN text, not just any WARN (a bare level=WARN match would not have caught this test previously tripping heuristic (b) instead):\n%s", buf.String())
	}
}

// TestEvaluateFileReportRoot_ZeroTrackedAnythingWithRealFilesAborts is the
// [IMPORTANT/plan-mandated] round-3 fix: heuristics (b), (b2) and (c) are
// ALL gated on this root having something tracked under it — but there was
// no guard for the inverse, a root under which THIS instance tracks
// NOTHING AT ALL (zero tracked files, zero tracked folders) despite the
// walk finding real video files there. That state is reachable a level
// below warnIfInstanceTrackedSetEntirelyUnmapped: a media_root_map KEY
// typo/case-mismatch on just ONE of several configured roots, or the same
// per-instance media_root_map block copy-pasted into two instances that
// each actually manage a different root — either way every real file under
// this root would otherwise fall through to fileKindOrphan and print as a
// confident kind=orphan, a flood of false orphans at INFO with no WARN
// anywhere, because every one of the instance's OTHER tracked paths mapped
// fine (so the whole-instance guard never fires).
func TestEvaluateFileReportRoot_ZeroTrackedAnythingWithRealFilesAborts(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFiles(t, dir, "Some Show/Some Show.mkv")
	root := mediaRoot{arrPath: "/anime", diskPath: dir}
	// This instance tracks something ELSEWHERE (a different root's folder
	// and file), but NOTHING at all maps under THIS root.
	set := instanceTrackedSet{
		files:   map[string]bool{"/data/Movies/Other/Other.mkv": true},
		folders: map[string]string{"/data/Movies/Other": "Other"},
	}
	logger, buf := newFileReportTestLogger(slog.LevelDebug)
	outcome := evaluateFileReportRoot(context.Background(), logger, slog.LevelInfo, Instance{Name: "radarr-main", Type: "radarr"}, root, set)
	if !outcome.skipped {
		t.Fatal("a root with zero tracked files AND zero tracked folders but real video files on the walk must abort, not report a flood of false orphans")
	}
	if len(outcome.findings) != 0 {
		t.Errorf("an aborted root must report nothing: %+v", outcome.findings)
	}
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "media_root_map") {
		t.Errorf("expected a WARN naming media_root_map:\n%s", buf.String())
	}
}

// --- partial-walk abort (completeness contract) -----------------------------

func TestEvaluateFileReportRoot_WalkErrorAbortsTheWholeRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits behave differently on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permission bits")
	}
	dir := t.TempDir()
	writeFixtureFiles(t, dir, "Movie A/Movie A.mkv", "Movie B/Movie B.mkv")
	unreadable := filepath.Join(dir, "Movie C")
	if err := os.MkdirAll(unreadable, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(unreadable, "x.mkv"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(unreadable, 0o755) })

	files := map[string]bool{
		filepath.Join(dir, "Movie A", "Movie A.mkv"): true,
		filepath.Join(dir, "Movie B", "Movie B.mkv"): true,
	}
	root := mediaRoot{arrPath: "/movies", diskPath: dir}
	set := instanceTrackedSet{files: files, folders: map[string]string{}}
	logger, buf := newFileReportTestLogger(slog.LevelDebug)
	outcome := evaluateFileReportRoot(context.Background(), logger, slog.LevelInfo, Instance{Name: "radarr-main", Type: "radarr"}, root, set)
	if !outcome.skipped {
		t.Fatal("a walk error partway through must abort the WHOLE root's report (completeness contract), not just the unreadable subtree")
	}
	if len(outcome.findings) != 0 {
		t.Errorf("an aborted root must report nothing it saw before the error: %+v", outcome.findings)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a WARN naming the root:\n%s", buf.String())
	}
}

// --- context cancellation mid-walk ------------------------------------------

func TestEvaluateFileReportRoot_ShutdownAbortsTheWalk(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFiles(t, dir, "Movie A/Movie A.mkv")
	root := mediaRoot{arrPath: "/movies", diskPath: dir}
	set := instanceTrackedSet{files: map[string]bool{}, folders: map[string]string{}}
	logger, _ := newFileReportTestLogger(slog.LevelDebug)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	outcome := evaluateFileReportRoot(ctx, logger, slog.LevelInfo, Instance{Name: "radarr-main", Type: "radarr"}, root, set)
	if !outcome.skipped {
		t.Fatal("a cancelled context must abort the walk rather than report a partial root")
	}
}

// --- symlinks (binding controller resolution 2: "symlinks NOT resolved
// (lexical comparison on mapped paths)") ------------------------------------
//
// Nothing in filereport.go ever calls filepath.EvalSymlinks or os.Readlink —
// comparison is purely lexical on the strings mapArrPathToDisk produces —
// so these tests pin the two consequences a live read-only mount will
// actually meet: a symlink pointing AT a tracked file is a different lexical
// path, so its own name is reported as a duplicate; and fs.WalkDir never
// follows a symlinked directory (a symlink's DirEntry.IsDir() is false
// regardless of what it points to), so nothing behind one is ever visited.

func TestEvaluateFileReportRoot_SymlinkToTrackedFileIsReportedAsDuplicate(t *testing.T) {
	dir := t.TempDir()
	real := writeFixtureFiles(t, dir, "Movie A/Movie A.mkv")[0]
	symlinkPath := filepath.Join(dir, "Movie A", "Movie A (2).mkv")
	if err := os.Symlink(real, symlinkPath); err != nil {
		t.Skipf("symlinks unavailable on this filesystem: %v", err)
	}

	root := mediaRoot{arrPath: "/movies", diskPath: dir}
	set := instanceTrackedSet{
		files:   map[string]bool{real: true},
		folders: map[string]string{filepath.Join(dir, "Movie A"): "Movie A"},
	}
	logger, _ := newFileReportTestLogger(slog.LevelDebug)
	outcome := evaluateFileReportRoot(context.Background(), logger, slog.LevelInfo, Instance{Name: "radarr-main", Type: "radarr"}, root, set)
	if outcome.skipped {
		t.Fatalf("outcome unexpectedly skipped: %+v", outcome)
	}
	if len(outcome.findings) != 1 || outcome.findings[0].kind != fileKindDuplicate || outcome.findings[0].diskPath != symlinkPath {
		t.Errorf("findings = %+v, want exactly one duplicate at the symlink's own (unresolved) path %q", outcome.findings, symlinkPath)
	}
}

func TestEvaluateFileReportRoot_SymlinkedSubdirectoryIsNotDescendedInto(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFiles(t, dir, "Movie A/Movie A.mkv")
	// A real directory OUTSIDE the walked root, containing a real video
	// file, symlinked in as a subdirectory of a tracked folder.
	outside := t.TempDir()
	writeFixtureFiles(t, outside, "hidden.mkv")
	symlinkDir := filepath.Join(dir, "Movie A", "Linked")
	if err := os.Symlink(outside, symlinkDir); err != nil {
		t.Skipf("symlinks unavailable on this filesystem: %v", err)
	}

	root := mediaRoot{arrPath: "/movies", diskPath: dir}
	set := instanceTrackedSet{
		files:   map[string]bool{filepath.Join(dir, "Movie A", "Movie A.mkv"): true},
		folders: map[string]string{filepath.Join(dir, "Movie A"): "Movie A"},
	}
	logger, _ := newFileReportTestLogger(slog.LevelDebug)
	outcome := evaluateFileReportRoot(context.Background(), logger, slog.LevelInfo, Instance{Name: "radarr-main", Type: "radarr"}, root, set)
	if outcome.skipped {
		t.Fatalf("outcome unexpectedly skipped: %+v", outcome)
	}
	for _, f := range outcome.findings {
		if strings.Contains(f.diskPath, "hidden.mkv") {
			t.Errorf("the walk must never descend into a symlinked directory: found %q", f.diskPath)
		}
	}
	// The symlink entry itself is visited (as a non-directory DirEntry, per
	// fs.WalkDir's own contract) but has no recognized video extension by
	// name ("Linked"), so it resolves as skipped-by-rule/extension — one
	// more file seen by the walk, not a directory descended into.
	assertFileReportIdentity(t, 2, outcome)
}

// --- logFileReportFinding: the frozen vocabulary (binding controller
// resolution 7) ---------------------------------------------------------

// TestLogFileReportFinding_EmitsTheFrozenVocabulary is the [plan-mandated]
// gap-closing pin: the only assertions ever made against an emitted finding
// line were kind=duplicate/orphan, group=SxxEyy (acceptance test), and a
// bare path substring — instance=, root=, path=, title=/series=, and
// groupCount= were asserted only as fileReportFinding STRUCT fields, never
// as the actual log attrs the controller's live gate greps for. Renaming
// path->file, dropping root/instance, or swapping the title/series attr
// would leave the rest of the suite green without this test. It pins the
// full attr set, attr-by-attr, for the three shapes the vocabulary ever
// produces: a Radarr duplicate, a Sonarr duplicate, and an orphan (which
// must carry NONE of the grouping attrs).
func TestLogFileReportFinding_EmitsTheFrozenVocabulary(t *testing.T) {
	root := mediaRoot{arrPath: "/movies", diskPath: "/data/Movies"}

	tests := []struct {
		name string
		inst Instance
		f    fileReportFinding
		want []string
	}{
		{
			name: "radarr duplicate",
			inst: Instance{Name: "radarr-main", Type: "radarr"},
			f: fileReportFinding{
				kind: fileKindDuplicate, diskPath: "/data/Movies/MovieA/MovieA(2).mkv",
				title: "MovieA", folder: "/data/Movies/MovieA", groupCount: 2,
			},
			want: []string{
				`msg="file-report finding"`, "kind=duplicate", "instance=radarr-main", "type=radarr",
				"root=/data/Movies", "path=/data/Movies/MovieA/MovieA(2).mkv",
				"title=MovieA", "groupCount=2",
			},
		},
		{
			name: "sonarr duplicate",
			inst: Instance{Name: "sonarr-main", Type: "sonarr"},
			f: fileReportFinding{
				kind: fileKindDuplicate, diskPath: "/data/Movies/ShowA/Season01/ShowA.S01E05(2).mkv",
				isSeries: true, title: "ShowA", group: "S01E05", folder: "/data/Movies/ShowA", groupCount: 3,
			},
			want: []string{
				`msg="file-report finding"`, "kind=duplicate", "instance=sonarr-main", "type=sonarr",
				"root=/data/Movies", "path=/data/Movies/ShowA/Season01/ShowA.S01E05(2).mkv",
				"series=ShowA", "group=S01E05", "groupCount=3",
			},
		},
		{
			name: "orphan",
			inst: Instance{Name: "radarr-main", Type: "radarr"},
			f: fileReportFinding{
				kind: fileKindOrphan, diskPath: "/data/Movies/Stray/stray.mkv",
			},
			want: []string{
				`msg="file-report finding"`, "kind=orphan", "instance=radarr-main", "type=radarr",
				"root=/data/Movies", "path=/data/Movies/Stray/stray.mkv",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, buf := newFileReportTestLogger(slog.LevelDebug)
			logFileReportFinding(context.Background(), logger, slog.LevelInfo, tt.inst, root, tt.f)
			out := buf.String()
			for _, want := range tt.want {
				if !strings.Contains(out, want) {
					t.Errorf("expected %q in the finding line:\n%s", want, out)
				}
			}
		})
	}

	// The orphan shape must carry NONE of the duplicate-only grouping
	// attrs — the identity-term boundary a human (or the controller's live
	// gate) uses to tell "an extra copy beside a tracked item" apart from
	// "a file nowhere near anything tracked".
	t.Run("orphan carries no grouping attrs", func(t *testing.T) {
		logger, buf := newFileReportTestLogger(slog.LevelDebug)
		logFileReportFinding(context.Background(), logger, slog.LevelInfo,
			Instance{Name: "radarr-main", Type: "radarr"}, root,
			fileReportFinding{kind: fileKindOrphan, diskPath: "/data/Movies/Stray/stray.mkv"})
		out := buf.String()
		for _, unwanted := range []string{"title=", "series=", "group=", "groupCount="} {
			if strings.Contains(out, unwanted) {
				t.Errorf("orphan finding must not carry %q:\n%s", unwanted, out)
			}
		}
	})
}

// --- path mapping (binding controller resolution 2) -------------------------
//
// Compare cleaned absolute paths (strip trailing slashes); exact-prefix match
// on path-SEGMENT boundaries only ("/movies" maps "/movies/X/y.mkv", never
// "/movies-4k/..."); case-SENSITIVE (server filesystems are Linux); symlinks
// NOT resolved (lexical comparison on mapped paths only).

func TestCleanArrPath_StripsTrailingSlash(t *testing.T) {
	cases := map[string]string{
		"/movies/":  "/movies",
		"/movies//": "/movies",
		"/movies":   "/movies",
		"/":         "/",
	}
	for in, want := range cases {
		if got := cleanArrPath(in); got != want {
			t.Errorf("cleanArrPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHasPathPrefix_SegmentBoundaryOnly(t *testing.T) {
	cases := []struct {
		p, prefix string
		want      bool
	}{
		{"/movies/X/y.mkv", "/movies", true},
		{"/movies", "/movies", true},
		{"/movies-4k/X/y.mkv", "/movies", false},
		{"/movies4k", "/movies", false},
		{"/other/X", "/movies", false},
	}
	for _, tc := range cases {
		if got := hasPathPrefix(tc.p, tc.prefix); got != tc.want {
			t.Errorf("hasPathPrefix(%q, %q) = %v, want %v", tc.p, tc.prefix, got, tc.want)
		}
	}
}

func TestHasPathPrefix_CaseSensitive(t *testing.T) {
	if hasPathPrefix("/Movies/X.mkv", "/movies") {
		t.Error("hasPathPrefix must be case-sensitive: Linux server filesystems distinguish /Movies from /movies")
	}
}

func TestMapArrPathToDisk_MapsUnderRoot(t *testing.T) {
	root := mediaRoot{arrPath: "/movies", diskPath: "/data/media/Movies"}
	got, ok := mapArrPathToDisk("/movies/Some Movie (2020)/file.mkv", root)
	if !ok {
		t.Fatal("expected a mapping")
	}
	want := "/data/media/Movies/Some Movie (2020)/file.mkv"
	if got != want {
		t.Errorf("mapArrPathToDisk = %q, want %q", got, want)
	}
}

func TestMapArrPathToDisk_ExactRootMaps(t *testing.T) {
	root := mediaRoot{arrPath: "/movies", diskPath: "/data/media/Movies"}
	got, ok := mapArrPathToDisk("/movies", root)
	if !ok || got != "/data/media/Movies" {
		t.Errorf("mapArrPathToDisk(root) = %q, %v, want /data/media/Movies, true", got, ok)
	}
}

func TestMapArrPathToDisk_SiblingRootDoesNotMatch(t *testing.T) {
	root := mediaRoot{arrPath: "/movies", diskPath: "/data/media/Movies"}
	if _, ok := mapArrPathToDisk("/movies-4k/X/y.mkv", root); ok {
		t.Error("a sibling root sharing a prefix must never match (path-segment boundary rule)")
	}
}

func TestMapArrPathToDisk_TrailingSlashesNormalized(t *testing.T) {
	root := mediaRoot{arrPath: "/movies", diskPath: "/data/media/Movies"}
	got, ok := mapArrPathToDisk("/movies/Title/", root)
	if !ok || got != "/data/media/Movies/Title" {
		t.Errorf("mapArrPathToDisk with trailing slash = %q, %v", got, ok)
	}
}

// TestMapArrPathToDisk_RootArrPathIsSlash pins a real config
// (media_root_map: {"/": "/data/media"}) that validateConfig accepts (both
// sides are absolute and non-empty). hasPathPrefix special-cases prefix=="/"
// to match everything without the usual "prefix+/" construction, so the
// mapping side must special-case it too, or the leading separator gets
// stripped from the suffix and every path silently mis-maps (e.g.
// "/movies/X/y.mkv" would become "/data/mediamovies/X/y.mkv" instead of
// "/data/media/movies/X/y.mkv") — corrupted mapped paths that fall outside
// root.diskPath entirely, disarming both mount-problem heuristics (they
// sample/require tracked files actually under the root) and reporting the
// whole library as orphans.
func TestMapArrPathToDisk_RootArrPathIsSlash(t *testing.T) {
	root := mediaRoot{arrPath: "/", diskPath: "/data/media"}
	got, ok := mapArrPathToDisk("/movies/X/y.mkv", root)
	if !ok {
		t.Fatal("expected a mapping")
	}
	want := "/data/media/movies/X/y.mkv"
	if got != want {
		t.Errorf("mapArrPathToDisk = %q, want %q", got, want)
	}
}

func TestMapArrPathToAnyRoot_PicksTheMatchingRoot(t *testing.T) {
	roots := []mediaRoot{
		{arrPath: "/tv_shows", diskPath: "/data/media/TV_Shows"},
		{arrPath: "/anime", diskPath: "/data/media/Anime"},
	}
	got, ok := mapArrPathToAnyRoot("/anime/Show/S01E01.mkv", roots)
	if !ok || got != "/data/media/Anime/Show/S01E01.mkv" {
		t.Errorf("mapArrPathToAnyRoot = %q, %v", got, ok)
	}
}

func TestMapArrPathToAnyRoot_NoMatch(t *testing.T) {
	roots := []mediaRoot{{arrPath: "/movies", diskPath: "/data/media/Movies"}}
	if _, ok := mapArrPathToAnyRoot("/tv_shows/Show/S01E01.mkv", roots); ok {
		t.Error("a path outside every configured root must not map")
	}
}

// TestRootRelativeDisplayPath_LastRootSegmentPlusRel pins the GUI fix's whole
// contract: a display path is the mapped root's own LAST path segment,
// joined to rel (the file's path relative to that root) — never the full
// disk path, which on a real deployment can carry a long, host-specific
// mount prefix (a scratch directory on a dev laptop, a deeply nested share
// on the server) that dwarfs anything an operator glancing at the GUI needs
// to see.
func TestRootRelativeDisplayPath_LastRootSegmentPlusRel(t *testing.T) {
	cases := []struct {
		name string
		root mediaRoot
		rel  string
		want string
	}{
		{
			name: "ordinary nested root",
			root: mediaRoot{arrPath: "/movies", diskPath: "/data/media/Movies"},
			rel:  "Harry Potter and the Chamber of Secrets (2002)/Harry.Potter.2002.2160p-ETRG/ETRG.mkv",
			want: "Movies/Harry Potter and the Chamber of Secrets (2002)/Harry.Potter.2002.2160p-ETRG/ETRG.mkv",
		},
		{
			name: "long host-specific scratch prefix collapses to its own last segment",
			root: mediaRoot{arrPath: "/movies", diskPath: "/private/tmp/claude/scratchpad/media-mnt/Movies"},
			rel:  "Some Film (2020)/Some Film (2020) (2).mkv",
			want: "Movies/Some Film (2020)/Some Film (2020) (2).mkv",
		},
		{
			name: "file directly at the root",
			root: mediaRoot{arrPath: "/movies", diskPath: "/data/media/Movies"},
			rel:  "loose.mkv",
			want: "Movies/loose.mkv",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rootRelativeDisplayPath(tc.root, tc.rel); got != tc.want {
				t.Errorf("rootRelativeDisplayPath(%+v, %q) = %q, want %q", tc.root, tc.rel, got, tc.want)
			}
		})
	}
}

func TestMediaRootsFor_AbsentMapReturnsNil(t *testing.T) {
	if roots := mediaRootsFor(Instance{}); roots != nil {
		t.Errorf("mediaRootsFor with no MediaRootMap = %v, want nil", roots)
	}
}

func TestMediaRootsFor_CleansBothSides(t *testing.T) {
	inst := Instance{MediaRootMap: map[string]string{"/movies/": "/data/media/Movies/"}}
	roots := mediaRootsFor(inst)
	if len(roots) != 1 || roots[0].arrPath != "/movies" || roots[0].diskPath != "/data/media/Movies" {
		t.Errorf("mediaRootsFor = %+v, want cleaned single root", roots)
	}
}

// --- video extension / trailer-sample / extras-dir classification rules -----

func TestVideoExtensions_RecognizesExactlyThePlanSet(t *testing.T) {
	want := []string{".mkv", ".mp4", ".avi", ".m2ts", ".ts", ".wmv"}
	for _, ext := range want {
		if !videoExtensions[ext] {
			t.Errorf("videoExtensions[%q] = false, want true", ext)
		}
	}
	notWanted := []string{".nfo", ".srt", ".jpg", ".mkvx", ".txt"}
	for _, ext := range notWanted {
		if videoExtensions[ext] {
			t.Errorf("videoExtensions[%q] = true, want false", ext)
		}
	}
}

func TestClassifyFileReportPath_UnrecognizedExtensionIsSkippedByRule(t *testing.T) {
	root := mediaRoot{arrPath: "/movies", diskPath: "/data/Movies"}
	set := instanceTrackedSet{files: map[string]bool{}, folders: map[string]string{}}
	c := classifyFileReportPath("/data/Movies/Title/poster.jpg", root, set)
	if c.kind != fileKindSkippedByRule || c.reason != FileSkipReasonExtension {
		t.Errorf("classify = %+v, want skipped-by-rule/extension", c)
	}
}

func TestClassifyFileReportPath_TrailerAndSampleSuffixesSkipped(t *testing.T) {
	root := mediaRoot{arrPath: "/movies", diskPath: "/data/Movies"}
	set := instanceTrackedSet{files: map[string]bool{}, folders: map[string]string{"/data/Movies/Title": "Title"}}
	for _, name := range []string{"Movie-trailer.mkv", "Movie-Trailer.mkv", "Movie-sample.mkv", "Movie-SAMPLE.mkv"} {
		c := classifyFileReportPath("/data/Movies/Title/"+name, root, set)
		if c.kind != fileKindSkippedByRule || c.reason != FileSkipReasonTrailerOrSample {
			t.Errorf("classify(%q) = %+v, want skipped-by-rule/trailer-sample", name, c)
		}
	}
}

func TestClassifyFileReportPath_NonTrailerSuffixIsNotSkipped(t *testing.T) {
	root := mediaRoot{arrPath: "/movies", diskPath: "/data/Movies"}
	set := instanceTrackedSet{files: map[string]bool{}, folders: map[string]string{"/data/Movies/Title": "Title"}}
	c := classifyFileReportPath("/data/Movies/Title/Movie-extended.mkv", root, set)
	if c.kind == fileKindSkippedByRule {
		t.Errorf("classify(Movie-extended.mkv) = %+v, must not be skipped as trailer/sample", c)
	}
}

func TestClassifyFileReportPath_ExtrasSubfolderSkipped(t *testing.T) {
	root := mediaRoot{arrPath: "/movies", diskPath: "/data/Movies"}
	set := instanceTrackedSet{files: map[string]bool{}, folders: map[string]string{"/data/Movies/Title": "Title"}}
	for _, dir := range []string{"Featurettes", "Behind The Scenes", "Trailers", "Extras", "Other",
		"Specials Extras", "Deleted Scenes", "Interviews", "Scenes", "Shorts", "Featurette", "TRAILERS"} {
		c := classifyFileReportPath("/data/Movies/Title/"+dir+"/extra.mkv", root, set)
		if c.kind != fileKindSkippedByRule || c.reason != FileSkipReasonExtrasDir {
			t.Errorf("classify under %q = %+v, want skipped-by-rule/extras", dir, c)
		}
	}
}

// TestClassifyFileReportPath_RootNamedLikeAnExtrasDirIsNotSelfExcluded is the
// [FIX] pin: underExtrasDir tested the climbed-to directory's own basename
// against extrasDirNames BEFORE checking whether that directory was the root
// itself, so a media root whose own basename happens to be one of the eleven
// Plex-convention extras names (e.g. /data/media/Shorts — a perfectly
// plausible real library name) matched extrasDirNames the moment the climb
// reached the root, for every file anywhere under it, no matter how deep.
// The whole root then silently reported skipped-by-rule/extras for
// everything: duplicates=0 orphans=0, indistinguishable from a clean pass.
func TestClassifyFileReportPath_RootNamedLikeAnExtrasDirIsNotSelfExcluded(t *testing.T) {
	root := mediaRoot{arrPath: "/anime", diskPath: "/data/media/Shorts"}
	set := instanceTrackedSet{
		files:   map[string]bool{"/data/media/Shorts/Title/movie.mkv": true},
		folders: map[string]string{"/data/media/Shorts/Title": "Title"},
	}
	c := classifyFileReportPath("/data/media/Shorts/Title/movie.mkv", root, set)
	if c.kind != fileKindTracked {
		t.Errorf("classify(tracked file under a root named like an extras dir) = %+v, want tracked: the root's own basename must never self-exclude everything beneath it", c)
	}

	// A stray file directly under such a root (outside every tracked folder)
	// must surface as a real orphan too, not skipped-by-rule/extras.
	c2 := classifyFileReportPath("/data/media/Shorts/stray.mkv", root, set)
	if c2.kind != fileKindOrphan {
		t.Errorf("classify(orphan directly under a root named like an extras dir) = %+v, want orphan", c2)
	}
}

func TestClassifyFileReportPath_TrackedFileIsTracked(t *testing.T) {
	root := mediaRoot{arrPath: "/movies", diskPath: "/data/Movies"}
	set := instanceTrackedSet{
		files:   map[string]bool{"/data/Movies/Title/movie.mkv": true},
		folders: map[string]string{"/data/Movies/Title": "Title"},
	}
	c := classifyFileReportPath("/data/Movies/Title/movie.mkv", root, set)
	if c.kind != fileKindTracked {
		t.Errorf("classify(tracked file) = %+v, want tracked", c)
	}
}

func TestClassifyFileReportPath_ExtraFileInTrackedFolderIsDuplicate(t *testing.T) {
	root := mediaRoot{arrPath: "/movies", diskPath: "/data/Movies"}
	set := instanceTrackedSet{
		files:   map[string]bool{"/data/Movies/Title/movie.mkv": true},
		folders: map[string]string{"/data/Movies/Title": "Title"},
	}
	c := classifyFileReportPath("/data/Movies/Title/movie (2).mkv", root, set)
	if c.kind != fileKindDuplicate || c.title != "Title" {
		t.Errorf("classify(extra file) = %+v, want duplicate grouped under Title", c)
	}
}

// TestClassifyFileReportPath_ExcludedFolderWithholdsExtraFilesFromDuplicates
// pins the exclusion_tag fix: README.md says the tag "opts an item out of
// everything cutoffarr does, in every mode, including dry-run reporting".
// An extra file inside an excluded item's folder must not be printed as
// kind=duplicate naming that item.
func TestClassifyFileReportPath_ExcludedFolderWithholdsExtraFilesFromDuplicates(t *testing.T) {
	root := mediaRoot{arrPath: "/movies", diskPath: "/data/Movies"}
	set := instanceTrackedSet{
		files:           map[string]bool{"/data/Movies/Title/movie.mkv": true},
		folders:         map[string]string{"/data/Movies/Title": "Title"},
		excludedFolders: map[string]bool{"/data/Movies/Title": true},
	}
	// The movie's own tracked file is still "tracked" — the exclusion tag
	// must never make it look like an orphan of itself.
	if c := classifyFileReportPath("/data/Movies/Title/movie.mkv", root, set); c.kind != fileKindTracked {
		t.Errorf("classify(the excluded item's own file) = %+v, want tracked", c)
	}
	// An extra file in that same folder must be withheld, not reported as a
	// duplicate.
	c := classifyFileReportPath("/data/Movies/Title/movie (2).mkv", root, set)
	if c.kind != fileKindSkippedByRule || c.reason != FileSkipReasonExcludedByTag {
		t.Errorf("classify(extra file in an excluded folder) = %+v, want skipped-by-rule/excluded-by-tag", c)
	}
}

// TestClassifyFileReportPath_DistrustedFolderWithholdsExtraFilesAsUntrusted
// is distrustedFolders' classify-level pin: a folder whose own tracked-file
// evidence is untrusted (own path unreadable, series id absent, etc.) must
// classify extras as skipped-untrusted with FileSkipReasonUntrackedPath,
// never as a confident duplicate — the excludedFolders mechanism produces
// skipped-by-rule/excluded-by-tag instead, a DIFFERENT identity term for a
// different reason (a deliberate opt-out, not untrusted evidence).
func TestClassifyFileReportPath_DistrustedFolderWithholdsExtraFilesAsUntrusted(t *testing.T) {
	root := mediaRoot{arrPath: "/movies", diskPath: "/data/Movies"}
	set := instanceTrackedSet{
		files:             map[string]bool{},
		folders:           map[string]string{"/data/Movies/Title": "Title"},
		distrustedFolders: map[string]bool{"/data/Movies/Title": true},
	}
	c := classifyFileReportPath("/data/Movies/Title/movie.mkv", root, set)
	if c.kind != fileKindSkippedUntrusted || c.reason != FileSkipReasonUntrackedPath {
		t.Errorf("classify(file in a distrusted folder) = %+v, want skipped-untrusted/untracked-path", c)
	}
}

func TestClassifyFileReportPath_FileOutsideAnyTrackedFolderIsOrphan(t *testing.T) {
	root := mediaRoot{arrPath: "/movies", diskPath: "/data/Movies"}
	set := instanceTrackedSet{files: map[string]bool{}, folders: map[string]string{"/data/Movies/Title": "Title"}}
	c := classifyFileReportPath("/data/Movies/Untracked Folder/movie.mkv", root, set)
	if c.kind != fileKindOrphan {
		t.Errorf("classify(untracked location) = %+v, want orphan", c)
	}
}

func TestClassifyFileReportPath_SonarrSeasonMismatchExcludesFromDuplicateCandidacy(t *testing.T) {
	root := mediaRoot{arrPath: "/tv_shows", diskPath: "/data/TV"}
	set := instanceTrackedSet{
		files:             map[string]bool{},
		folders:           map[string]string{"/data/TV/Show": "Show"},
		seriesFolder:      map[string]bool{"/data/TV/Show": true},
		mismatchedSeasons: map[string]map[int]bool{"/data/TV/Show": {1: true}},
	}
	c := classifyFileReportPath("/data/TV/Show/Season 01/Show.S01E05.mkv", root, set)
	if c.kind != fileKindSkippedUntrusted || c.reason != FileSkipReasonMismatchedSeason {
		t.Errorf("classify(mismatched season extra) = %+v, want skipped-untrusted/mismatched-season", c)
	}
}

func TestClassifyFileReportPath_SonarrNonMismatchedSeasonIsDuplicate(t *testing.T) {
	root := mediaRoot{arrPath: "/tv_shows", diskPath: "/data/TV"}
	set := instanceTrackedSet{
		files:             map[string]bool{},
		folders:           map[string]string{"/data/TV/Show": "Show"},
		seriesFolder:      map[string]bool{"/data/TV/Show": true},
		mismatchedSeasons: map[string]map[int]bool{"/data/TV/Show": {1: true}},
	}
	c := classifyFileReportPath("/data/TV/Show/Season 02/Show.S02E05.mkv", root, set)
	if c.kind != fileKindDuplicate {
		t.Errorf("classify(season 2, mismatch only on season 1) = %+v, want duplicate", c)
	}
}

// TestClassifyFileReportPath_SeasonDeterminedByFolderNotMisleadingFilename
// pins binding controller resolution 5's "never guessed" requirement against
// its most direct violation: a file physically living inside a DISTRUSTED
// "Season 01" folder but named as though it belongs to season 2. The
// filename must never override the folder it actually sits in.
func TestClassifyFileReportPath_SeasonDeterminedByFolderNotMisleadingFilename(t *testing.T) {
	root := mediaRoot{arrPath: "/tv_shows", diskPath: "/data/TV"}
	set := instanceTrackedSet{
		files:             map[string]bool{},
		folders:           map[string]string{"/data/TV/Show": "Show"},
		seriesFolder:      map[string]bool{"/data/TV/Show": true},
		mismatchedSeasons: map[string]map[int]bool{"/data/TV/Show": {1: true}},
	}
	// Filename claims season 2 (not mismatched); the containing folder says
	// season 1 (mismatched). Location must win.
	c := classifyFileReportPath("/data/TV/Show/Season 01/Show.S02E01.mkv", root, set)
	if c.kind != fileKindSkippedUntrusted || c.reason != FileSkipReasonMismatchedSeason {
		t.Errorf("classify(misleading filename inside a mismatched season folder) = %+v, want skipped-untrusted/mismatched-season", c)
	}
}

// TestClassifyFileReportPath_FlatLayoutSeriesWithAnyMismatchWithheldFromDuplicates
// pins the second shape resolution 5 forbids guessing on: Sonarr's
// season-folder option is off (or the file otherwise sits directly in the
// series folder), so there is no folder name to derive a season from at all.
// With at least one season of this series already distrusted, the file's
// season cannot be ruled safe, so it must NOT be reported as a confident
// duplicate — regardless of what its filename claims.
func TestClassifyFileReportPath_FlatLayoutSeriesWithAnyMismatchWithheldFromDuplicates(t *testing.T) {
	root := mediaRoot{arrPath: "/tv_shows", diskPath: "/data/TV"}
	set := instanceTrackedSet{
		files:             map[string]bool{},
		folders:           map[string]string{"/data/TV/Show": "Show"},
		seriesFolder:      map[string]bool{"/data/TV/Show": true},
		mismatchedSeasons: map[string]map[int]bool{"/data/TV/Show": {1: true}},
	}
	// Filename claims season 2; there is no "Season NN" folder anywhere
	// between this file and the series folder (season folders disabled).
	c := classifyFileReportPath("/data/TV/Show/Show.S02E05.mkv", root, set)
	if c.kind != fileKindSkippedUntrusted || c.reason != FileSkipReasonMismatchedSeason {
		t.Errorf("classify(flat layout, season undeterminable, series has a mismatch) = %+v, want skipped-untrusted/mismatched-season, not a guessed duplicate", c)
	}
}

// --- Sonarr grouping label (display convenience only — resolution 3) --------

func TestSonarrGroupLabel_ParsesSxxEyyFromFilename(t *testing.T) {
	season, label := sonarrGroupLabel("/data/TV/Show/Show.S01E05.mkv", "/data/TV/Show")
	if season == nil || *season != 1 || label != "S01E05" {
		t.Errorf("sonarrGroupLabel = season=%v label=%q, want season=1 label=S01E05", season, label)
	}
}

func TestSonarrGroupLabel_FallsBackToSeasonFolder(t *testing.T) {
	season, label := sonarrGroupLabel("/data/TV/Show/Season 03/extra.mkv", "/data/TV/Show")
	if season == nil || *season != 3 || label != "Season 03" {
		t.Errorf("sonarrGroupLabel = season=%v label=%q, want season=3 label=%q", season, label, "Season 03")
	}
}

func TestSonarrGroupLabel_UnparseableGroupsUnderContainingFolder(t *testing.T) {
	season, label := sonarrGroupLabel("/data/TV/Show/randomfile.mkv", "/data/TV/Show")
	if season != nil {
		t.Errorf("sonarrGroupLabel season = %v, want nil (unparseable, never guessed)", *season)
	}
	if label == "" {
		t.Error("sonarrGroupLabel must still produce a display label for the containing folder")
	}
}

// --- buildRadarrTrackedSet ---------------------------------------------------

func TestBuildRadarrTrackedSet_MapsFolderAndFile(t *testing.T) {
	roots := []mediaRoot{{arrPath: "/movies", diskPath: "/data/Movies"}}
	movies := []movieListElement{
		{
			ID: intPtr(1), Title: strPtr("Movie A"), HasFile: boolPtr(true),
			Path:      strPtr("/movies/Movie A (2020)"),
			MovieFile: &movieFileElement{Path: strPtr("/movies/Movie A (2020)/Movie A (2020).mkv")},
		},
	}
	logger, _ := newFileReportTestLogger(slog.LevelDebug)
	set, _ := buildRadarrTrackedSet(logger, Instance{Name: "radarr-main", Type: "radarr"}, roots, movies, 0, false)

	if set.folders["/data/Movies/Movie A (2020)"] != "Movie A" {
		t.Errorf("folders = %v, want the mapped folder keyed to the title", set.folders)
	}
	if !set.files["/data/Movies/Movie A (2020)/Movie A (2020).mkv"] {
		t.Errorf("files = %v, want the mapped tracked file", set.files)
	}
}

func TestBuildRadarrTrackedSet_NoFileMeansOnlyFolderTracked(t *testing.T) {
	roots := []mediaRoot{{arrPath: "/movies", diskPath: "/data/Movies"}}
	movies := []movieListElement{
		{ID: intPtr(1), Title: strPtr("Not Downloaded"), HasFile: boolPtr(false), Path: strPtr("/movies/Not Downloaded")},
	}
	logger, _ := newFileReportTestLogger(slog.LevelDebug)
	set, _ := buildRadarrTrackedSet(logger, Instance{Name: "radarr-main", Type: "radarr"}, roots, movies, 0, false)
	if set.folders["/data/Movies/Not Downloaded"] != "Not Downloaded" {
		t.Errorf("folders = %v, want the folder tracked even with no file", set.folders)
	}
	if len(set.files) != 0 {
		t.Errorf("files = %v, want none: hasFile is false", set.files)
	}
}

// TestBuildRadarrTrackedSet_AbsentMoviePathStillTracksItsOwnFile pins the
// design decision documented in classifyFileReportPath's callers: a movie
// whose OWN folder path could not be read still has its exact tracked FILE
// path protected (from movieFile.path alone), even though nothing protects
// the surrounding folder from false-orphan classification for its siblings.
// A warning is required rather than silence, per §2.6.
func TestBuildRadarrTrackedSet_AbsentMoviePathStillTracksItsOwnFile(t *testing.T) {
	roots := []mediaRoot{{arrPath: "/movies", diskPath: "/data/Movies"}}
	movies := []movieListElement{
		{
			ID: intPtr(1), Title: strPtr("Mystery Path"), HasFile: boolPtr(true),
			Path:      nil,
			MovieFile: &movieFileElement{Path: strPtr("/movies/Mystery Path/file.mkv")},
		},
	}
	logger, buf := newFileReportTestLogger(slog.LevelDebug)
	set, _ := buildRadarrTrackedSet(logger, Instance{Name: "radarr-main", Type: "radarr"}, roots, movies, 0, false)
	if !set.files["/data/Movies/Mystery Path/file.mkv"] {
		t.Errorf("files = %v, want the tracked file protected even without a known folder", set.files)
	}
	if len(set.folders) != 0 {
		t.Errorf("folders = %v, want none: the movie's own path was never observed", set.folders)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("an absent movie path must warn:\n%s", buf.String())
	}
}

// TestBuildRadarrTrackedSet_HasFileNilButMovieFilePathReadableStillTracksFile
// pins the fix for the false-orphan-of-your-own-file bug: HasFile absent
// (untrusted input — could mean "not yet imported" OR could mean the field
// simply wasn't parsed) must never be treated as "this movie's file cannot
// be readable," because a perfectly readable movieFile.path is independent
// evidence the movie file protection should ride on. Gating the tracked-FILE
// half on HasFile==true (decision.go's forward-engine idiom, safe there
// because absent-as-false only means "do not unmonitor") inverts to unsafe
// here: it would silently drop the movie's own real file from set.files,
// after which the SAME file gets classified as a duplicate/orphan of itself
// by classifyFileReportPath.
func TestBuildRadarrTrackedSet_HasFileNilButMovieFilePathReadableStillTracksFile(t *testing.T) {
	roots := []mediaRoot{{arrPath: "/movies", diskPath: "/data/Movies"}}
	movies := []movieListElement{
		{
			ID: intPtr(1), Title: strPtr("Untrusted HasFile"), HasFile: nil,
			Path:      strPtr("/movies/Untrusted HasFile"),
			MovieFile: &movieFileElement{Path: strPtr("/movies/Untrusted HasFile/file.mkv")},
		},
	}
	logger, buf := newFileReportTestLogger(slog.LevelDebug)
	set, _ := buildRadarrTrackedSet(logger, Instance{Name: "radarr-main", Type: "radarr"}, roots, movies, 0, false)
	if !set.files["/data/Movies/Untrusted HasFile/file.mkv"] {
		t.Errorf("files = %v, want the movie's own readable file tracked even though hasFile is absent", set.files)
	}
	// hasFile absent with a readable path is not itself a contradiction (no
	// warning demanded): the warning exists for hasFile=true with an
	// UNREADABLE path (the genuinely suspicious combination), pinned by
	// TestBuildRadarrTrackedSet_HasFileButAbsentMovieFilePathWarnsAndExcludes
	// below.
	_ = buf
}

// TestBuildRadarrTrackedSet_HasFileButAbsentMovieFilePathDistrustsTheFolder
// is the [IMPORTANT/plan-mandated] fix: a movie whose folder maps but whose
// OWN tracked-file path is unreadable used to leave the folder in
// set.folders with nothing in set.files for it — meaning the movie's own,
// only real file on disk would fall through to the duplicate branch and be
// printed as a confident kind=duplicate about a human's only copy. The
// folder must stay registered (containment still needs it, so extras are
// never misreported as ORPHANS either) but be marked distrusted so extras
// classify as skipped-untrusted, not a confident duplicate.
func TestBuildRadarrTrackedSet_HasFileButAbsentMovieFilePathDistrustsTheFolder(t *testing.T) {
	roots := []mediaRoot{{arrPath: "/movies", diskPath: "/data/Movies"}}
	movies := []movieListElement{
		{ID: intPtr(1), Title: strPtr("Broken"), HasFile: boolPtr(true), Path: strPtr("/movies/Broken"), MovieFile: nil},
	}
	logger, buf := newFileReportTestLogger(slog.LevelDebug)
	set, _ := buildRadarrTrackedSet(logger, Instance{Name: "radarr-main", Type: "radarr"}, roots, movies, 0, false)
	if len(set.files) != 0 {
		t.Errorf("files = %v, want none: movieFile.path could not be read", set.files)
	}
	// The folder is still registered (containment must still find it), but
	// now marked distrusted rather than left as a confident duplicate
	// generator.
	if set.folders["/data/Movies/Broken"] != "Broken" {
		t.Errorf("folders = %v, want the folder still registered", set.folders)
	}
	if !set.distrustedFolders["/data/Movies/Broken"] {
		t.Errorf("distrustedFolders = %v, want the folder marked distrusted: an extra file in it must never be reported as a confident duplicate", set.distrustedFolders)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("hasFile=true with an unreadable movieFile.path must warn:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "orphan") {
		t.Errorf("the warning must name the real risk (duplicate), not orphan:\n%s", buf.String())
	}
}

// TestBuildRadarrTrackedSet_HasFileNilAndAbsentMovieFilePathDistrustsTheFolder
// is the [IMPORTANT/plan-mandated] round-3 fix, build-time twin of
// TestRunRadarrFileReport_HasFileAbsentDoesNotMisreportOwnFileAsDuplicate:
// the distrust branch used to require hasFile explicitly true before
// marking a folder distrusted, so hasFile=nil with an unreadable
// movieFile.path left the folder registered but NOT distrusted — the exact
// contradiction the function's own doc comment already calls out for the
// tracked-FILE half, just not (until now) drawn to its symmetric
// conclusion here.
func TestBuildRadarrTrackedSet_HasFileNilAndAbsentMovieFilePathDistrustsTheFolder(t *testing.T) {
	roots := []mediaRoot{{arrPath: "/movies", diskPath: "/data/Movies"}}
	movies := []movieListElement{
		{ID: intPtr(1), Title: strPtr("Broken"), HasFile: nil, Path: strPtr("/movies/Broken"), MovieFile: nil},
	}
	logger, buf := newFileReportTestLogger(slog.LevelDebug)
	set, _ := buildRadarrTrackedSet(logger, Instance{Name: "radarr-main", Type: "radarr"}, roots, movies, 0, false)
	if len(set.files) != 0 {
		t.Errorf("files = %v, want none: movieFile.path could not be read", set.files)
	}
	if set.folders["/data/Movies/Broken"] != "Broken" {
		t.Errorf("folders = %v, want the folder still registered", set.folders)
	}
	if !set.distrustedFolders["/data/Movies/Broken"] {
		t.Errorf("distrustedFolders = %v, want the folder marked distrusted: hasFile absent is untrusted input, not confirmation the movie has no file", set.distrustedFolders)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("hasFile absent with an unreadable movieFile.path must warn:\n%s", buf.String())
	}
}

// TestBuildRadarrTrackedSet_PathOutsideAnyRootIsCountedNotSilentlyDropped
// pins the [plan-mandated] fix: the Phase 10 binding "Next phase inputs"
// require "a tracked path that matches NO mapping is a skip+warn counted
// under a skip reason, never an orphan". A movie's path/movieFile.path
// mapping to none of the configured roots is still not worth a per-item
// WARN (that would flood the log for every legitimately out-of-scope movie
// on a multi-root instance), but it MUST be counted so a media_root_map typo
// that silently maps nothing is distinguishable, on the always-INFO summary,
// from a genuinely clean library.
func TestBuildRadarrTrackedSet_PathOutsideAnyRootIsCountedNotSilentlyDropped(t *testing.T) {
	roots := []mediaRoot{{arrPath: "/movies", diskPath: "/data/Movies"}}
	movies := []movieListElement{
		{
			ID: intPtr(1), Title: strPtr("Elsewhere"), HasFile: boolPtr(true),
			Path:      strPtr("/other-library/Elsewhere"),
			MovieFile: &movieFileElement{Path: strPtr("/other-library/Elsewhere/file.mkv")},
		},
	}
	logger, buf := newFileReportTestLogger(slog.LevelDebug)
	set, skipCounts := buildRadarrTrackedSet(logger, Instance{Name: "radarr-main", Type: "radarr"}, roots, movies, 0, false)
	if len(set.files) != 0 || len(set.folders) != 0 {
		t.Errorf("a movie entirely outside every configured root must contribute nothing to the tracked set: %+v", set)
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("a movie outside every configured root is not this instance's problem and must not warn per-item:\n%s", buf.String())
	}
	// Both the folder (movie.path) and the file (movieFile.path) are outside
	// every configured root, so both must be counted.
	if got := skipCounts[FileSkipReasonOutsideConfiguredRoots]; got != 2 {
		t.Errorf("skipCounts[outside configured roots] = %d, want 2 (folder + file)", got)
	}
}

// TestBuildRadarrTrackedSet_ExclusionTagMarksTheFolderExcluded pins the
// exclusion_tag fix: a tagged movie's folder is still tracked (so its own
// file is protected) but is also recorded in excludedFolders, and a movie
// WITHOUT the tag is not.
func TestBuildRadarrTrackedSet_ExclusionTagMarksTheFolderExcluded(t *testing.T) {
	const exclusionTagID = 9
	roots := []mediaRoot{{arrPath: "/movies", diskPath: "/data/Movies"}}
	movies := []movieListElement{
		{ID: intPtr(1), Title: strPtr("Tagged"), HasFile: boolPtr(true), Tags: &[]int{exclusionTagID},
			Path: strPtr("/movies/Tagged"), MovieFile: &movieFileElement{Path: strPtr("/movies/Tagged/Tagged.mkv")}},
		{ID: intPtr(2), Title: strPtr("Untagged"), HasFile: boolPtr(true), Tags: &[]int{},
			Path: strPtr("/movies/Untagged"), MovieFile: &movieFileElement{Path: strPtr("/movies/Untagged/Untagged.mkv")}},
	}
	logger, _ := newFileReportTestLogger(slog.LevelDebug)
	set, _ := buildRadarrTrackedSet(logger, Instance{Name: "radarr-main", Type: "radarr"}, roots, movies, exclusionTagID, true)
	if !set.excludedFolders["/data/Movies/Tagged"] {
		t.Errorf("excludedFolders = %v, want the tagged movie's folder excluded", set.excludedFolders)
	}
	if set.excludedFolders["/data/Movies/Untagged"] {
		t.Errorf("excludedFolders = %v, want the untagged movie's folder NOT excluded", set.excludedFolders)
	}
	// The tag must never affect whether tagActive is false: with the feature
	// inactive, nothing is ever excluded.
	setInactive, _ := buildRadarrTrackedSet(logger, Instance{Name: "radarr-main", Type: "radarr"}, roots, movies, exclusionTagID, false)
	if len(setInactive.excludedFolders) != 0 {
		t.Errorf("excludedFolders = %v, want none when tagActive is false", setInactive.excludedFolders)
	}
}

// --- runRadarrFileReport + the off/skipped/ran summary vocabulary ----------
//
// Binding controller resolution 1: media_root_map absent means OFF for that
// instance, distinguishable from skipped (configured but aborted) and from a
// clean ran (duplicates=0 orphans=0).

func TestRunRadarrFileReport_NoMediaRootMapIsOff(t *testing.T) {
	logger, _ := newFileReportTestLogger(slog.LevelDebug)
	c := runRadarrFileReport(context.Background(), logger, slog.LevelInfo, Instance{Name: "radarr-main", Type: "radarr"}, nil, 0, false)
	if c.state() != "off" {
		t.Errorf("state() = %q, want off when media_root_map is absent", c.state())
	}
}

func TestRunRadarrFileReport_CleanLibraryRansWithZeroFindings(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFiles(t, dir, "Movie A/Movie A.mkv")
	inst := Instance{Name: "radarr-main", Type: "radarr", MediaRootMap: map[string]string{"/movies": dir}}
	movies := []movieListElement{
		{ID: intPtr(1), Title: strPtr("Movie A"), HasFile: boolPtr(true),
			Path: strPtr("/movies/Movie A"), MovieFile: &movieFileElement{Path: strPtr("/movies/Movie A/Movie A.mkv")}},
	}
	logger, _ := newFileReportTestLogger(slog.LevelDebug)
	c := runRadarrFileReport(context.Background(), logger, slog.LevelInfo, inst, movies, 0, false)
	if c.state() != "ran" {
		t.Errorf("state() = %q, want ran", c.state())
	}
	if c.duplicates != 0 || c.orphans != 0 {
		t.Errorf("duplicates=%d orphans=%d, want 0/0 for a clean library", c.duplicates, c.orphans)
	}
}

// TestRunRadarrFileReport_ManyUntrackedPathsWarnOnceNotOncePerItem is the
// [FIX] pin for the per-item WARN flood: buildRadarrTrackedSet used to log
// its own logger.Warn call for EVERY FileSkipReasonUntrackedPath occurrence,
// during tracked-set build — which runs BEFORE
// warnIfInstanceTrackedSetEntirelyUnmapped ever gets a chance to abort. If a
// speculatively-added *arr field name turns out wrong at the live gate (the
// implementer's own stated risk), every movie in the library would warn,
// potentially thousands of lines per sweep, drowning the one abort WARN that
// actually matters. This pins that a whole instance's worth of
// untracked-path items produces exactly ONE aggregated WARN carrying the
// count — mirroring warnIfAnyTrackedPathUnmapped's existing aggregation for
// the sibling FileSkipReasonOutsideConfiguredRoots reason — while the
// per-reason count on the always-INFO summary stays exact.
func TestRunRadarrFileReport_ManyUntrackedPathsWarnOnceNotOncePerItem(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFiles(t, dir, "Healthy/Healthy.mkv")
	inst := Instance{Name: "radarr-main", Type: "radarr", MediaRootMap: map[string]string{"/movies": dir}}

	var movies []movieListElement
	for i := 0; i < 25; i++ {
		// Path maps fine (folder tracked, no warn from that half) but
		// MovieFile is nil and HasFile is nil: the untrustworthy-file half
		// fires exactly once per movie.
		movies = append(movies, movieListElement{
			ID: intPtr(100 + i), Title: strPtr(fmt.Sprintf("Broken %d", i)), HasFile: nil,
			Path: strPtr(fmt.Sprintf("/movies/Broken %d", i)), MovieFile: nil,
		})
	}
	// A healthy movie with a real tracked file so neither the instance-wide
	// guard nor the per-root mount heuristics fire — this test is about the
	// per-item flood, a separate guard from either.
	movies = append(movies, movieListElement{
		ID: intPtr(1), Title: strPtr("Healthy"), HasFile: boolPtr(true),
		Path: strPtr("/movies/Healthy"), MovieFile: &movieFileElement{Path: strPtr("/movies/Healthy/Healthy.mkv")},
	})

	logger, buf := newFileReportTestLogger(slog.LevelInfo)
	c := runRadarrFileReport(context.Background(), logger, slog.LevelInfo, inst, movies, 0, false)
	if c.state() != "ran" {
		t.Fatalf("state() = %q, want ran:\n%s", c.state(), buf.String())
	}
	if got := c.skipReasons[FileSkipReasonUntrackedPath]; got != 25 {
		t.Fatalf("skipReasons[untracked path] = %d, want 25: the count must stay exact even when the WARN is aggregated", got)
	}
	if got := strings.Count(buf.String(), "level=WARN"); got != 1 {
		t.Errorf("WARN lines = %d, want exactly 1: 25 untracked-path movies must aggregate into ONE WARN, not flood the log one per item:\n%s", got, buf.String())
	}
	if !strings.Contains(buf.String(), "count=25") {
		t.Errorf("expected the aggregated WARN to carry count=25:\n%s", buf.String())
	}
}

// TestRunRadarrFileReport_OwnFilePathUnreadableDoesNotMisreportAsDuplicate
// is the [plan-mandated] walk-level pin: the two build-time tests that touch
// this shape asserted only the tracked-set build, never what the WALK then
// does with it — and the walk is where the actual bug lives. hasFile=true
// with an unreadable movieFile.path used to leave the folder tracked but
// contribute nothing to set.files, so the movie's own, only real file on
// disk fell through to the duplicate branch and was printed as
// kind=duplicate about a human's only copy.
func TestRunRadarrFileReport_OwnFilePathUnreadableDoesNotMisreportAsDuplicate(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFiles(t, dir, "Broken/Broken.mkv", "Healthy/Healthy.mkv")
	inst := Instance{Name: "radarr-main", Type: "radarr", MediaRootMap: map[string]string{"/movies": dir}}
	movies := []movieListElement{
		// hasFile=true but movieFile is nil (path unreadable): the ONLY real
		// file on disk, Broken/Broken.mkv, must never be reported.
		{ID: intPtr(1), Title: strPtr("Broken"), HasFile: boolPtr(true), Path: strPtr("/movies/Broken"), MovieFile: nil},
		// A second, fully-healthy movie so the root's own tracked-file
		// count is not itself zero — this test is about ONE item's own
		// untrustworthy evidence, not the whole-root heuristic (b2)
		// (finding 5, prior round), which is a deliberately separate guard.
		{ID: intPtr(2), Title: strPtr("Healthy"), HasFile: boolPtr(true),
			Path: strPtr("/movies/Healthy"), MovieFile: &movieFileElement{Path: strPtr("/movies/Healthy/Healthy.mkv")}},
	}
	logger, buf := newFileReportTestLogger(slog.LevelInfo)
	c := runRadarrFileReport(context.Background(), logger, slog.LevelInfo, inst, movies, 0, false)
	if c.state() != "ran" {
		t.Fatalf("state() = %q, want ran", c.state())
	}
	if c.duplicates != 0 || c.orphans != 0 {
		t.Errorf("duplicates=%d orphans=%d, want 0/0: the movie's only real file must never be misreported as a duplicate of itself", c.duplicates, c.orphans)
	}
	if got := c.skipReasons[FileSkipReasonUntrackedPath]; got == 0 {
		t.Errorf("skipReasons[untracked path] = %d, want > 0: the reason must be counted", got)
	}
	if strings.Contains(buf.String(), "kind=duplicate") {
		t.Errorf("no duplicate finding may be logged for this movie's folder:\n%s", buf.String())
	}
}

// TestRunRadarrFileReport_HasFileAbsentDoesNotMisreportOwnFileAsDuplicate is
// the [IMPORTANT/plan-mandated] round-3 fix: the walk-level twin of
// TestRunRadarrFileReport_OwnFilePathUnreadableDoesNotMisreportAsDuplicate
// above, but through the OTHER untrustworthy door — hasFile ABSENT (nil)
// rather than hasFile=true. buildRadarrTrackedSet's distrust branch used to
// require hasFile to be explicitly true before marking a folder distrusted,
// so a movie with hasFile=nil and movieFile=nil left its folder registered
// with nothing in set.files and nothing in distrustedFolders — its own real
// file on disk then fell through to the duplicate branch and printed as a
// confident kind=duplicate naming a human's only copy, even though the
// function's own doc comment already treats hasFile-absent as untrusted
// input everywhere else.
func TestRunRadarrFileReport_HasFileAbsentDoesNotMisreportOwnFileAsDuplicate(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFiles(t, dir, "Broken/Broken.mkv", "Healthy/Healthy.mkv")
	inst := Instance{Name: "radarr-main", Type: "radarr", MediaRootMap: map[string]string{"/movies": dir}}
	movies := []movieListElement{
		// hasFile is ABSENT (nil), and movieFile is nil too: the ONLY real
		// file on disk, Broken/Broken.mkv, must never be reported.
		{ID: intPtr(1), Title: strPtr("Broken"), HasFile: nil, Path: strPtr("/movies/Broken"), MovieFile: nil},
		// A second, fully-healthy movie so the root's own tracked-file
		// count is not itself zero — this test is about ONE item's own
		// untrustworthy evidence, not the whole-root heuristic (b2).
		{ID: intPtr(2), Title: strPtr("Healthy"), HasFile: boolPtr(true),
			Path: strPtr("/movies/Healthy"), MovieFile: &movieFileElement{Path: strPtr("/movies/Healthy/Healthy.mkv")}},
	}
	logger, buf := newFileReportTestLogger(slog.LevelInfo)
	c := runRadarrFileReport(context.Background(), logger, slog.LevelInfo, inst, movies, 0, false)
	if c.state() != "ran" {
		t.Fatalf("state() = %q, want ran", c.state())
	}
	if c.duplicates != 0 || c.orphans != 0 {
		t.Errorf("duplicates=%d orphans=%d, want 0/0: a movie whose hasFile status is unknown must never have its real file misreported as a duplicate of itself", c.duplicates, c.orphans)
	}
	if got := c.skipReasons[FileSkipReasonUntrackedPath]; got == 0 {
		t.Errorf("skipReasons[untracked path] = %d, want > 0: the reason must be counted", got)
	}
	if strings.Contains(buf.String(), "kind=duplicate") {
		t.Errorf("no duplicate finding may be logged for this movie's folder:\n%s", buf.String())
	}
}

// TestRunRadarrFileReport_ExcludedMoviesExtraFilesAreWithheldEndToEnd is the
// end-to-end pin for honoring exclusion_tag in the file report: README.md
// states the tag "opts an item out of everything cutoffarr does, in every
// mode, including dry-run reporting" — an extra file inside a tagged
// movie's own folder must never be printed as kind=duplicate.
func TestRunRadarrFileReport_ExcludedMoviesExtraFilesAreWithheldEndToEnd(t *testing.T) {
	const exclusionTagID = 9
	dir := t.TempDir()
	writeFixtureFiles(t, dir, "Tagged/Tagged.mkv", "Tagged/Tagged (2).mkv")
	inst := Instance{Name: "radarr-main", Type: "radarr", MediaRootMap: map[string]string{"/movies": dir}}
	movies := []movieListElement{
		{ID: intPtr(1), Title: strPtr("Tagged"), HasFile: boolPtr(true), Tags: &[]int{exclusionTagID},
			Path: strPtr("/movies/Tagged"), MovieFile: &movieFileElement{Path: strPtr("/movies/Tagged/Tagged.mkv")}},
	}
	logger, buf := newFileReportTestLogger(slog.LevelDebug)
	c := runRadarrFileReport(context.Background(), logger, slog.LevelInfo, inst, movies, exclusionTagID, true)
	if c.state() != "ran" {
		t.Fatalf("state() = %q, want ran", c.state())
	}
	if c.duplicates != 0 {
		t.Errorf("duplicates=%d, want 0: the extra file belongs to an excluded movie and must never be reported", c.duplicates)
	}
	if got := c.skipReasons[FileSkipReasonExcludedByTag]; got != 1 {
		t.Errorf("skipReasons[excluded by tag] = %d, want 1", got)
	}
	if strings.Contains(buf.String(), `kind=duplicate`) {
		t.Errorf("no duplicate finding may be logged for an excluded movie's folder:\n%s", buf.String())
	}
}

func TestRunRadarrFileReport_FindingsAreCountedAndAbortedRootIsSkipped(t *testing.T) {
	cleanDir := t.TempDir()
	writeFixtureFiles(t, cleanDir, "Movie A/Movie A.mkv", "Movie A/Movie A (2).mkv", "Stray/orphan.mkv")

	inst := Instance{Name: "radarr-main", Type: "radarr", MediaRootMap: map[string]string{
		"/movies":    cleanDir,
		"/movies-4k": filepath.Join(t.TempDir(), "does-not-exist"),
	}}
	movies := []movieListElement{
		{ID: intPtr(1), Title: strPtr("Movie A"), HasFile: boolPtr(true),
			Path: strPtr("/movies/Movie A"), MovieFile: &movieFileElement{Path: strPtr("/movies/Movie A/Movie A.mkv")}},
	}
	logger, buf := newFileReportTestLogger(slog.LevelDebug)
	c := runRadarrFileReport(context.Background(), logger, slog.LevelInfo, inst, movies, 0, false)
	if c.state() != "skipped" {
		t.Errorf("state() = %q, want skipped: one of two roots aborted", c.state())
	}
	// The other, healthy root's findings must still be reported — one root
	// aborting never affects other roots (binding controller resolution 4).
	if c.duplicates != 1 || c.orphans != 1 {
		t.Errorf("duplicates=%d orphans=%d, want 1/1 from the healthy root despite the other root's abort", c.duplicates, c.orphans)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("the aborted root must still warn:\n%s", buf.String())
	}
}

// TestRunRadarrFileReport_RootWithNothingTrackedButRealFilesAbortsOnlyThatRoot
// is the [IMPORTANT/plan-mandated] round-3 end-to-end pin for the per-root
// twin of warnIfInstanceTrackedSetEntirelyUnmapped: "/movies" is mapped
// fine and clean, but "/anime" is a real, readable, media-filled root this
// instance's library never references at all — the realistic shape of a
// media_root_map key typo on one of several roots (or the same map block
// copy-pasted into another instance that actually manages that root). Only
// the untracked root may abort; the healthy root's own clean result must
// still stand, and nothing under the untracked root may be printed as a
// flood of false orphans.
func TestRunRadarrFileReport_RootWithNothingTrackedButRealFilesAbortsOnlyThatRoot(t *testing.T) {
	trackedDir := t.TempDir()
	writeFixtureFiles(t, trackedDir, "Movie A/Movie A.mkv")
	untrackedDir := t.TempDir()
	writeFixtureFiles(t, untrackedDir, "Some Show/Some Show.mkv")

	inst := Instance{Name: "radarr-main", Type: "radarr", MediaRootMap: map[string]string{
		"/movies": trackedDir,
		"/anime":  untrackedDir,
	}}
	movies := []movieListElement{
		{ID: intPtr(1), Title: strPtr("Movie A"), HasFile: boolPtr(true),
			Path: strPtr("/movies/Movie A"), MovieFile: &movieFileElement{Path: strPtr("/movies/Movie A/Movie A.mkv")}},
	}
	logger, buf := newFileReportTestLogger(slog.LevelDebug)
	c := runRadarrFileReport(context.Background(), logger, slog.LevelInfo, inst, movies, 0, false)
	if c.state() != "skipped" {
		t.Fatalf("state() = %q, want skipped: the /anime root tracks nothing but has real video files", c.state())
	}
	if c.duplicates != 0 || c.orphans != 0 {
		t.Errorf("duplicates=%d orphans=%d, want 0/0: /movies is clean and /anime's real files must never print as a flood of false orphans", c.duplicates, c.orphans)
	}
	if strings.Contains(buf.String(), "kind=orphan") {
		t.Errorf("no orphan finding may be logged for the untracked root:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "media_root_map") {
		t.Errorf("expected a WARN naming media_root_map:\n%s", buf.String())
	}
}

// TestRunRadarrFileReport_UnmappedTrackedPathSurfacesOnTheSummary is the
// [plan-mandated] end-to-end pin: the Phase 10 binding "Next phase inputs"
// require an unmapped tracked path be "counted under a skip reason" so a
// media_root_map typo — configuring "/Movies" while Radarr reports
// "/movies" (comparison is deliberately case-SENSITIVE) — is distinguishable
// on the always-INFO summary from a genuinely clean, fully-mapped library:
// both would otherwise log fileReport=ran duplicates=0 orphans=0
// fileSkipReasons="none".
//
// [CRITICAL FIX] this used to run over an EMPTY t.TempDir with only ONE
// movie in the whole library, whose path was the mismapped one — meaning the
// tracked set came back COMPLETELY empty (set.files AND set.folders both
// empty) and this test never exercised what that state does over a
// populated root: every real file would be walked and reported as a
// confident kind=orphan. That state is now its own, stronger abort case —
// see TestRunRadarrFileReport_AllTrackedPathsUnmappedAbortsTheWholeInstance
// below — so this test now uses a SECOND, correctly-mapped movie to pin the
// original, weaker claim on its own: an instance that is NOT entirely
// unmapped still counts (and warns about) the one tracked path that failed
// to map, without aborting the whole instance over it.
func TestRunRadarrFileReport_UnmappedTrackedPathSurfacesOnTheSummary(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFiles(t, dir, "Movie B/Movie B.mkv")
	inst := Instance{Name: "radarr-main", Type: "radarr", MediaRootMap: map[string]string{"/movies": dir}}
	movies := []movieListElement{
		// The configured root is "/movies"; this movie's arr-side path is
		// "/Movies" (case mismatch) — a config typo that maps NOTHING.
		{ID: intPtr(1), Title: strPtr("Movie A"), HasFile: boolPtr(true),
			Path: strPtr("/Movies/Movie A"), MovieFile: &movieFileElement{Path: strPtr("/Movies/Movie A/Movie A.mkv")}},
		// A second, correctly-mapped movie, so the tracked set is not
		// ENTIRELY empty — only this ONE movie's own path failed to map.
		{ID: intPtr(2), Title: strPtr("Movie B"), HasFile: boolPtr(true),
			Path: strPtr("/movies/Movie B"), MovieFile: &movieFileElement{Path: strPtr("/movies/Movie B/Movie B.mkv")}},
	}
	logger, buf := newFileReportTestLogger(slog.LevelInfo)
	c := runRadarrFileReport(context.Background(), logger, slog.LevelInfo, inst, movies, 0, false)
	if c.state() != "ran" {
		t.Fatalf("state() = %q, want ran (Movie B mapped fine; only Movie A's own path failed to map)", c.state())
	}
	if got := c.skipReasons[FileSkipReasonOutsideConfiguredRoots]; got != 2 {
		t.Errorf("skipReasons[outside configured roots] = %d, want 2 (folder + file)", got)
	}
	logFileReportSummary(logger, inst, c)
	out := buf.String()
	if !strings.Contains(out, FileSkipReasonOutsideConfiguredRoots) {
		t.Errorf("the summary line must name the reason, so this is never indistinguishable from a clean library:\n%s", out)
	}
}

// TestRunRadarrFileReport_UnmappedTrackedPathsWarnOnceAggregatedNotPerItem
// pins the binding "skip+warn counted under a skip reason" requirement's
// WARN half: unmapped tracked paths were only ever counted (summary-only,
// always INFO) with no WARN at all, so a media_root_map typo was invisible
// unless a human happened to be reading the always-INFO summary line. The
// WARN must be ONE aggregated line per instance, not one per unmapped item —
// the exact per-item flood the original fix (finding 2, prior round)
// deliberately avoided.
func TestRunRadarrFileReport_UnmappedTrackedPathsWarnOnceAggregatedNotPerItem(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFiles(t, dir, "Movie C/Movie C.mkv")
	inst := Instance{Name: "radarr-main", Type: "radarr", MediaRootMap: map[string]string{"/movies": dir}}
	movies := []movieListElement{
		{ID: intPtr(1), Title: strPtr("Movie A"), HasFile: boolPtr(true),
			Path: strPtr("/Movies/Movie A"), MovieFile: &movieFileElement{Path: strPtr("/Movies/Movie A/Movie A.mkv")}},
		{ID: intPtr(2), Title: strPtr("Movie B"), HasFile: boolPtr(true),
			Path: strPtr("/Movies/Movie B"), MovieFile: &movieFileElement{Path: strPtr("/Movies/Movie B/Movie B.mkv")}},
		{ID: intPtr(3), Title: strPtr("Movie C"), HasFile: boolPtr(true),
			Path: strPtr("/movies/Movie C"), MovieFile: &movieFileElement{Path: strPtr("/movies/Movie C/Movie C.mkv")}},
	}
	logger, buf := newFileReportTestLogger(slog.LevelInfo)
	c := runRadarrFileReport(context.Background(), logger, slog.LevelInfo, inst, movies, 0, false)
	if c.state() != "ran" {
		t.Fatalf("state() = %q, want ran", c.state())
	}
	if got := c.skipReasons[FileSkipReasonOutsideConfiguredRoots]; got != 4 {
		t.Fatalf("skipReasons[outside configured roots] = %d, want 4 (2 movies x folder+file)", got)
	}
	out := buf.String()
	if n := strings.Count(out, "media_root_map"); n != 1 {
		t.Errorf("expected exactly one aggregated WARN naming media_root_map, got %d occurrences:\n%s", n, out)
	}
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected a WARN:\n%s", out)
	}
}

// TestRunRadarrFileReport_AllTrackedPathsUnmappedAbortsTheWholeInstance is
// the [CRITICAL] pin: when NOTHING in the library maps to any configured
// root (a media_root_map key typo — comparison is deliberately
// case-SENSITIVE — or a wrong live-*arr field name), the tracked set comes
// back COMPLETELY empty (set.files AND set.folders both empty), which
// disarms every per-root mount-problem guard: heuristic (b) and (c) are
// gated on len(tracked) > 0, (b2) on len(trackedFolders) > 0, and all three
// are trivially zero. Without an instance-wide guard, the walk runs anyway
// and every real, healthy, tracked file on disk gets reported as a
// confident kind=orphan — a healthy library reported as a flood of false
// orphans, exactly what the mount-problem heuristic exists to prevent, just
// upstream of where any per-root heuristic ever gets a chance to run.
func TestRunRadarrFileReport_AllTrackedPathsUnmappedAbortsTheWholeInstance(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFiles(t, dir, "Movie A/Movie A.mkv", "Movie B/Movie B.mkv")
	inst := Instance{Name: "radarr-main", Type: "radarr", MediaRootMap: map[string]string{"/movies": dir}}
	movies := []movieListElement{
		// Every movie's arr-side path uses the wrong case ("/Movies" vs the
		// configured "/movies") — nothing maps, instance-wide.
		{ID: intPtr(1), Title: strPtr("Movie A"), HasFile: boolPtr(true),
			Path: strPtr("/Movies/Movie A"), MovieFile: &movieFileElement{Path: strPtr("/Movies/Movie A/Movie A.mkv")}},
		{ID: intPtr(2), Title: strPtr("Movie B"), HasFile: boolPtr(true),
			Path: strPtr("/Movies/Movie B"), MovieFile: &movieFileElement{Path: strPtr("/Movies/Movie B/Movie B.mkv")}},
	}
	logger, buf := newFileReportTestLogger(slog.LevelInfo)
	c := runRadarrFileReport(context.Background(), logger, slog.LevelInfo, inst, movies, 0, false)
	if c.state() != "skipped" {
		t.Fatalf("state() = %q, want skipped: the whole library failed to map, so nothing may be walked", c.state())
	}
	if c.duplicates != 0 || c.orphans != 0 {
		t.Errorf("duplicates=%d orphans=%d, want 0/0: a healthy, fully-unmapped library must never be walked and reported as orphans", c.duplicates, c.orphans)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "media_root_map") {
		t.Errorf("expected a WARN naming media_root_map:\n%s", out)
	}
}

// TestRunRadarrFileReport_UntrackedPathIsAlsoCountedOnTheSummary is
// UnmappedTrackedPathSurfacesOnTheSummary's twin for the OTHER build-time
// skip reason: a path absent/unreadable altogether (FileSkipReasonUntrackedPath)
// already warned per-item, but was NOT folded into the summary's
// fileSkipReasons before this fix — so a cycle whose tracked set was
// knowingly incomplete could still summarize as fileSkipReasons="none".
func TestRunRadarrFileReport_UntrackedPathIsAlsoCountedOnTheSummary(t *testing.T) {
	dir := t.TempDir()
	inst := Instance{Name: "radarr-main", Type: "radarr", MediaRootMap: map[string]string{"/movies": dir}}
	movies := []movieListElement{
		// No Path at all: movie.path absent.
		{ID: intPtr(1), Title: strPtr("No Path"), HasFile: boolPtr(false)},
	}
	logger, _ := newFileReportTestLogger(slog.LevelInfo)
	c := runRadarrFileReport(context.Background(), logger, slog.LevelInfo, inst, movies, 0, false)
	if got := c.skipReasons[FileSkipReasonUntrackedPath]; got != 1 {
		t.Errorf("skipReasons[untracked path] = %d, want 1", got)
	}
	var buf bytes.Buffer
	summaryLogger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logFileReportSummary(summaryLogger, inst, c)
	if !strings.Contains(buf.String(), FileSkipReasonUntrackedPath) {
		t.Errorf("the summary line must name the reason:\n%s", buf.String())
	}
}

// TestRunRadarrFileReport_ShutdownBetweenRootsAbandonsRemainingRoots pins the
// same shutdown boundary daemon.go's runScanCycle draws BETWEEN instances,
// applied here between a multi-root instance's own roots: a cancelled
// context stops the loop before touching a root it had not yet reached,
// rather than reporting a partial picture as though it were complete.
func TestRunRadarrFileReport_ShutdownBetweenRootsAbandonsRemainingRoots(t *testing.T) {
	dir := t.TempDir()
	inst := Instance{Name: "radarr-main", Type: "radarr", MediaRootMap: map[string]string{"/movies": dir}}
	movies := []movieListElement{
		{ID: intPtr(1), Title: strPtr("Movie A"), HasFile: boolPtr(false), Path: strPtr("/movies/Movie A")},
	}
	logger, _ := newFileReportTestLogger(slog.LevelDebug)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := runRadarrFileReport(ctx, logger, slog.LevelInfo, inst, movies, 0, false)
	if c.state() != "skipped" {
		t.Errorf("state() = %q, want skipped: a cancelled context must abandon the remaining roots rather than report a partial picture", c.state())
	}
}

func TestFileReportCounts_State(t *testing.T) {
	cases := []struct {
		name string
		c    fileReportCounts
		want string
	}{
		{"unconfigured", fileReportCounts{}, "off"},
		{"configured and clean", fileReportCounts{configured: true}, "ran"},
		{"configured with a skipped root", fileReportCounts{configured: true, anySkipped: true}, "skipped"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.c.state(); got != tc.want {
				t.Errorf("state() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLogFileReportSummary_OffIsDebugAndCarriesNoDuplicateOrphanCounts(t *testing.T) {
	logger, buf := newFileReportTestLogger(slog.LevelDebug)
	logFileReportSummary(logger, Instance{Name: "radarr-main", Type: "radarr"}, fileReportCounts{})
	out := buf.String()
	if !strings.Contains(out, "level=DEBUG") {
		t.Errorf("off must log at debug:\n%s", out)
	}
	if !strings.Contains(out, `fileReport=off`) {
		t.Errorf("off must say so explicitly:\n%s", out)
	}
	if strings.Contains(out, "duplicates=") || strings.Contains(out, "orphans=") {
		t.Errorf("off must not carry duplicate/orphan counts, which would look like a completed report:\n%s", out)
	}
}

func TestLogFileReportSummary_OffIsInvisibleAtInfoLevel(t *testing.T) {
	// The everyday case: a user who never configured media_root_map for an
	// instance must see NOTHING about the file report at the default log
	// level, exactly as if Phase 11 did not exist for them.
	logger, buf := newFileReportTestLogger(slog.LevelInfo)
	logFileReportSummary(logger, Instance{Name: "radarr-main", Type: "radarr"}, fileReportCounts{})
	if buf.String() != "" {
		t.Errorf("off must be silent at info level:\n%s", buf.String())
	}
}

func TestLogFileReportSummary_RanIsInfoWithCounts(t *testing.T) {
	logger, buf := newFileReportTestLogger(slog.LevelInfo)
	logFileReportSummary(logger, Instance{Name: "radarr-main", Type: "radarr"},
		fileReportCounts{configured: true, duplicates: 2, orphans: 1, skipReasons: map[string]int{FileSkipReasonExtension: 3}})
	out := buf.String()
	for _, want := range []string{"level=INFO", `fileReport=ran`, "duplicates=2", "orphans=1", "fileSkipReasons="} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in:\n%s", want, out)
		}
	}
}

func TestLogFileReportSummary_SkippedIsInfoAndDistinctFromCleanRan(t *testing.T) {
	logger, buf := newFileReportTestLogger(slog.LevelInfo)
	logFileReportSummary(logger, Instance{Name: "radarr-main", Type: "radarr"},
		fileReportCounts{configured: true, anySkipped: true, skipReasons: map[string]int{}})
	out := buf.String()
	if !strings.Contains(out, "level=INFO") || !strings.Contains(out, `fileReport=skipped`) {
		t.Errorf("expected an INFO line saying fileReport=skipped:\n%s", out)
	}
}

// --- seasonsWithFileCountMismatch --------------------------------------------

func TestSeasonsWithFileCountMismatch_FewerFilesThanStatisticsIsFlagged(t *testing.T) {
	seasons := &[]seriesSeasonElement{
		{SeasonNumber: intPtr(1), Statistics: &seasonStatisticsElement{EpisodeFileCount: intPtr(3), TotalEpisodeCount: intPtr(3)}},
		{SeasonNumber: intPtr(2), Statistics: &seasonStatisticsElement{EpisodeFileCount: intPtr(2), TotalEpisodeCount: intPtr(2)}},
	}
	fileCounts := map[int]int{1: 2, 2: 2} // season 1 under-reported (2 < 3), season 2 matches
	got := seasonsWithFileCountMismatch(seasons, fileCounts)
	if !got[1] {
		t.Errorf("got %v, want season 1 flagged", got)
	}
	if got[2] {
		t.Errorf("got %v, want season 2 NOT flagged (counts agree)", got)
	}
}

func TestSeasonsWithFileCountMismatch_NilSeasonsReturnsEmpty(t *testing.T) {
	if got := seasonsWithFileCountMismatch(nil, map[int]int{1: 5}); len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

func TestSeasonsWithFileCountMismatch_MissingStatisticsSkipped(t *testing.T) {
	seasons := &[]seriesSeasonElement{{SeasonNumber: intPtr(1), Statistics: nil}}
	if got := seasonsWithFileCountMismatch(seasons, map[int]int{}); len(got) != 0 {
		t.Errorf("got %v, want empty: no statistics means no comparison is possible", got)
	}
}

func TestSeasonsWithFileCountMismatch_MoreFilesThanStatisticsIsNotFlagged(t *testing.T) {
	// More files fetched than statistics claims is not itself untrustworthy
	// in the direction this guard cares about (an UNDER-report is the
	// dangerous one — see decision.go's identical "<" comparison).
	seasons := &[]seriesSeasonElement{
		{SeasonNumber: intPtr(1), Statistics: &seasonStatisticsElement{EpisodeFileCount: intPtr(1), TotalEpisodeCount: intPtr(1)}},
	}
	if got := seasonsWithFileCountMismatch(seasons, map[int]int{1: 2}); got[1] {
		t.Errorf("got %v, want season 1 NOT flagged", got)
	}
}

// --- buildSonarrTrackedSet ---------------------------------------------------

// episodeFileFakeServer answers GET /api/v3/episodefile?seriesId=N with a
// per-series canned response, and records every request path it saw.
type episodeFileFakeServer struct {
	srv       *httptest.Server
	mu        sync.Mutex
	responses map[string]string // seriesId (string) -> JSON body
	requests  []string
}

func newEpisodeFileFakeServer(t *testing.T, responses map[string]string) *episodeFileFakeServer {
	t.Helper()
	f := &episodeFileFakeServer{responses: responses}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/episodefile", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("seriesId")
		f.mu.Lock()
		f.requests = append(f.requests, id)
		body, ok := f.responses[id]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte(body))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *episodeFileFakeServer) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func TestBuildSonarrTrackedSet_HappyPath_TracksFolderAndFiles(t *testing.T) {
	fake := newEpisodeFileFakeServer(t, map[string]string{
		"1": `[{"id":100,"seriesId":1,"seasonNumber":1,"path":"/tv_shows/Show/Show.S01E01.mkv"},
		       {"id":101,"seriesId":1,"seasonNumber":1,"path":"/tv_shows/Show/Show.S01E02.mkv"}]`,
	})
	client := NewAPIClient(fake.srv.URL, "key")
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "key"}
	roots := []mediaRoot{{arrPath: "/tv_shows", diskPath: "/data/TV"}}
	series := []seriesElement{{ID: intPtr(1), Title: strPtr("Show"), Path: strPtr("/tv_shows/Show")}}

	logger, _ := newFileReportTestLogger(slog.LevelDebug)
	set, _, ok := buildSonarrTrackedSet(context.Background(), logger, client, inst, roots, series, nil, 0, false)
	if !ok {
		t.Fatal("buildSonarrTrackedSet returned ok=false")
	}
	if set.folders["/data/TV/Show"] != "Show" || !set.seriesFolder["/data/TV/Show"] {
		t.Errorf("folders/seriesFolder = %v/%v, want the series folder tracked", set.folders, set.seriesFolder)
	}
	if !set.files["/data/TV/Show/Show.S01E01.mkv"] || !set.files["/data/TV/Show/Show.S01E02.mkv"] {
		t.Errorf("files = %v, want both episode files tracked", set.files)
	}
}

// TestBuildSonarrTrackedSet_ManyIdAbsentSeriesWarnOnceNotOncePerItem is the
// [FIX] pin for the per-item WARN flood, Sonarr's side: buildSonarrTrackedSet
// used to log its own logger.Warn call for EVERY series/episode-file that hit
// a FileSkipReasonUntrackedPath condition (id absent, path absent, or an
// episode file with an absent path) — a systemic field-name break at the
// live gate (series.path/episodeFile.path both being untrusted input this
// project only ever speculatively named) would flood the log one line per
// series, drowning the one abort WARN that actually matters. This pins that
// many id-absent series aggregate into exactly ONE WARN carrying the count.
func TestBuildSonarrTrackedSet_ManyIdAbsentSeriesWarnOnceNotOncePerItem(t *testing.T) {
	fake := newEpisodeFileFakeServer(t, map[string]string{
		"1": `[{"id":100,"seriesId":1,"seasonNumber":1,"path":"/tv_shows/Healthy/Healthy.S01E01.mkv"}]`,
	})
	client := NewAPIClient(fake.srv.URL, "key")
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "key"}
	roots := []mediaRoot{{arrPath: "/tv_shows", diskPath: "/data/TV"}}

	var series []seriesElement
	for i := 0; i < 25; i++ {
		// Path maps fine (folder tracked, no warn from that half) but ID is
		// nil: the id-absent half fires exactly once per series.
		series = append(series, seriesElement{
			ID: nil, Title: strPtr(fmt.Sprintf("Broken %d", i)), Path: strPtr(fmt.Sprintf("/tv_shows/Broken %d", i)),
		})
	}
	// A healthy series with a real tracked file so the instance-wide guard
	// never fires — this test is about the per-item flood, a separate guard.
	series = append(series, seriesElement{ID: intPtr(1), Title: strPtr("Healthy"), Path: strPtr("/tv_shows/Healthy")})

	logger, buf := newFileReportTestLogger(slog.LevelInfo)
	_, skipCounts, ok := buildSonarrTrackedSet(context.Background(), logger, client, inst, roots, series, nil, 0, false)
	if !ok {
		t.Fatal("buildSonarrTrackedSet returned ok=false")
	}
	if got := skipCounts[FileSkipReasonUntrackedPath]; got != 25 {
		t.Fatalf("skipCounts[untracked path] = %d, want 25: the count must stay exact even when the WARN is aggregated", got)
	}
	if got := strings.Count(buf.String(), "level=WARN"); got != 1 {
		t.Errorf("WARN lines = %d, want exactly 1: 25 id-absent series must aggregate into ONE WARN, not flood the log one per item:\n%s", got, buf.String())
	}
	if !strings.Contains(buf.String(), "count=25") {
		t.Errorf("expected the aggregated WARN to carry count=25:\n%s", buf.String())
	}
}

func TestBuildSonarrTrackedSet_MismatchedSeasonsCarriedThroughByFolder(t *testing.T) {
	fake := newEpisodeFileFakeServer(t, map[string]string{"1": `[]`})
	client := NewAPIClient(fake.srv.URL, "key")
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "key"}
	roots := []mediaRoot{{arrPath: "/tv_shows", diskPath: "/data/TV"}}
	series := []seriesElement{{ID: intPtr(1), Title: strPtr("Show"), Path: strPtr("/tv_shows/Show")}}
	mismatched := map[int]map[int]bool{1: {2: true}}

	logger, _ := newFileReportTestLogger(slog.LevelDebug)
	set, _, ok := buildSonarrTrackedSet(context.Background(), logger, client, inst, roots, series, mismatched, 0, false)
	if !ok {
		t.Fatal("buildSonarrTrackedSet returned ok=false")
	}
	if !set.mismatchedSeasons["/data/TV/Show"][2] {
		t.Errorf("mismatchedSeasons = %v, want season 2 excluded under the mapped folder", set.mismatchedSeasons)
	}
}

// TestBuildSonarrTrackedSet_ComputesOwnFileCountMismatchFromSeriesStatistics
// is the [IMPORTANT] fix: buildMismatchedSeasonsIndex only ever covers a
// series the FORWARD engine evaluated (rule 1: monitored, with a rule-1-6
// candidate season) — an unmonitored series (most of a mature library, by
// this project's own design) never appears in allDecisions at all, so its
// under-reporting /episodefile response here had NO mismatch protection.
// Fixed by comparing THIS fetch's own per-season file count against
// statistics.episodeFileCount already on the /series response — no forward
// index passed at all (nil), proving detection works standalone.
func TestBuildSonarrTrackedSet_ComputesOwnFileCountMismatchFromSeriesStatistics(t *testing.T) {
	fake := newEpisodeFileFakeServer(t, map[string]string{
		// statistics.episodeFileCount (below) claims season 1 has 3 files;
		// this fetch returns only 2.
		"1": `[{"id":100,"seriesId":1,"seasonNumber":1,"path":"/tv_shows/Show/Show.S01E01.mkv"},
		       {"id":101,"seriesId":1,"seasonNumber":1,"path":"/tv_shows/Show/Show.S01E02.mkv"}]`,
	})
	client := NewAPIClient(fake.srv.URL, "key")
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "key"}
	roots := []mediaRoot{{arrPath: "/tv_shows", diskPath: "/data/TV"}}
	series := []seriesElement{{
		ID: intPtr(1), Title: strPtr("Show"), Path: strPtr("/tv_shows/Show"),
		Seasons: &[]seriesSeasonElement{
			{SeasonNumber: intPtr(1), Statistics: &seasonStatisticsElement{EpisodeFileCount: intPtr(3), TotalEpisodeCount: intPtr(3)}},
		},
	}}
	logger, _ := newFileReportTestLogger(slog.LevelDebug)
	set, _, ok := buildSonarrTrackedSet(context.Background(), logger, client, inst, roots, series, nil, 0, false)
	if !ok {
		t.Fatal("buildSonarrTrackedSet returned ok=false")
	}
	if !set.mismatchedSeasons["/data/TV/Show"][1] {
		t.Errorf("mismatchedSeasons = %v, want season 1 excluded: this fetch (2 files) under-reports vs statistics.episodeFileCount (3)", set.mismatchedSeasons)
	}
}

// TestBuildSonarrTrackedSet_NoSelfMismatchWhenFileCountsAgree is the
// negative twin: statistics and this fetch agree, so nothing is flagged.
func TestBuildSonarrTrackedSet_NoSelfMismatchWhenFileCountsAgree(t *testing.T) {
	fake := newEpisodeFileFakeServer(t, map[string]string{
		"1": `[{"id":100,"seriesId":1,"seasonNumber":1,"path":"/tv_shows/Show/Show.S01E01.mkv"}]`,
	})
	client := NewAPIClient(fake.srv.URL, "key")
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "key"}
	roots := []mediaRoot{{arrPath: "/tv_shows", diskPath: "/data/TV"}}
	series := []seriesElement{{
		ID: intPtr(1), Title: strPtr("Show"), Path: strPtr("/tv_shows/Show"),
		Seasons: &[]seriesSeasonElement{
			{SeasonNumber: intPtr(1), Statistics: &seasonStatisticsElement{EpisodeFileCount: intPtr(1), TotalEpisodeCount: intPtr(1)}},
		},
	}}
	logger, _ := newFileReportTestLogger(slog.LevelDebug)
	set, _, ok := buildSonarrTrackedSet(context.Background(), logger, client, inst, roots, series, nil, 0, false)
	if !ok {
		t.Fatal("buildSonarrTrackedSet returned ok=false")
	}
	if len(set.mismatchedSeasons["/data/TV/Show"]) != 0 {
		t.Errorf("mismatchedSeasons = %v, want none: statistics and this fetch agree", set.mismatchedSeasons)
	}
}

// TestBuildSonarrTrackedSet_UnionsForwardIndexWithSelfComputedMismatch
// proves the two sources of mismatch evidence are UNIONED, not one
// overriding the other: season 1 is flagged by this function's own
// statistics comparison, season 2 is flagged only by the forward engine's
// index (passed in), and BOTH must end up excluded.
func TestBuildSonarrTrackedSet_UnionsForwardIndexWithSelfComputedMismatch(t *testing.T) {
	fake := newEpisodeFileFakeServer(t, map[string]string{
		"1": `[{"id":100,"seriesId":1,"seasonNumber":1,"path":"/tv_shows/Show/Show.S01E01.mkv"},
		       {"id":200,"seriesId":1,"seasonNumber":2,"path":"/tv_shows/Show/Show.S02E01.mkv"}]`,
	})
	client := NewAPIClient(fake.srv.URL, "key")
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "key"}
	roots := []mediaRoot{{arrPath: "/tv_shows", diskPath: "/data/TV"}}
	series := []seriesElement{{
		ID: intPtr(1), Title: strPtr("Show"), Path: strPtr("/tv_shows/Show"),
		Seasons: &[]seriesSeasonElement{
			// Season 1: statistics says 2 files, this fetch returned 1 — self-mismatch.
			{SeasonNumber: intPtr(1), Statistics: &seasonStatisticsElement{EpisodeFileCount: intPtr(2), TotalEpisodeCount: intPtr(2)}},
			// Season 2: statistics and this fetch agree (1 file each) — only
			// the forward engine's own index flags it.
			{SeasonNumber: intPtr(2), Statistics: &seasonStatisticsElement{EpisodeFileCount: intPtr(1), TotalEpisodeCount: intPtr(1)}},
		},
	}}
	forwardIndex := map[int]map[int]bool{1: {2: true}}
	logger, _ := newFileReportTestLogger(slog.LevelDebug)
	set, _, ok := buildSonarrTrackedSet(context.Background(), logger, client, inst, roots, series, forwardIndex, 0, false)
	if !ok {
		t.Fatal("buildSonarrTrackedSet returned ok=false")
	}
	if !set.mismatchedSeasons["/data/TV/Show"][1] {
		t.Error("season 1 must be flagged: this fetch itself under-reported vs statistics")
	}
	if !set.mismatchedSeasons["/data/TV/Show"][2] {
		t.Error("season 2 must be flagged: carried through from the forward engine's own index")
	}
}

// TestBuildSonarrTrackedSet_ExclusionTagMarksTheFolderExcluded is the Sonarr
// twin of TestBuildRadarrTrackedSet_ExclusionTagMarksTheFolderExcluded.
func TestBuildSonarrTrackedSet_ExclusionTagMarksTheFolderExcluded(t *testing.T) {
	const exclusionTagID = 9
	fake := newEpisodeFileFakeServer(t, map[string]string{"1": `[]`})
	client := NewAPIClient(fake.srv.URL, "key")
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "key"}
	roots := []mediaRoot{{arrPath: "/tv_shows", diskPath: "/data/TV"}}
	series := []seriesElement{{ID: intPtr(1), Title: strPtr("Show"), Path: strPtr("/tv_shows/Show"), Tags: &[]int{exclusionTagID}}}

	logger, _ := newFileReportTestLogger(slog.LevelDebug)
	set, _, ok := buildSonarrTrackedSet(context.Background(), logger, client, inst, roots, series, nil, exclusionTagID, true)
	if !ok {
		t.Fatal("buildSonarrTrackedSet returned ok=false")
	}
	if !set.excludedFolders["/data/TV/Show"] {
		t.Errorf("excludedFolders = %v, want the tagged series' folder excluded", set.excludedFolders)
	}
}

func TestBuildSonarrTrackedSet_AbsentSeriesPathStillFetchesAndTracksFiles(t *testing.T) {
	fake := newEpisodeFileFakeServer(t, map[string]string{
		"1": `[{"id":100,"seriesId":1,"seasonNumber":1,"path":"/tv_shows/Show/Show.S01E01.mkv"}]`,
	})
	client := NewAPIClient(fake.srv.URL, "key")
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "key"}
	roots := []mediaRoot{{arrPath: "/tv_shows", diskPath: "/data/TV"}}
	series := []seriesElement{{ID: intPtr(1), Title: strPtr("Show"), Path: nil}}

	logger, buf := newFileReportTestLogger(slog.LevelDebug)
	set, _, ok := buildSonarrTrackedSet(context.Background(), logger, client, inst, roots, series, nil, 0, false)
	if !ok {
		t.Fatal("buildSonarrTrackedSet returned ok=false")
	}
	if len(set.folders) != 0 {
		t.Errorf("folders = %v, want none: the series' own path was never observed", set.folders)
	}
	if !set.files["/data/TV/Show/Show.S01E01.mkv"] {
		t.Errorf("files = %v, want the episode file still tracked from episodeFile.path alone", set.files)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("an absent series path must warn:\n%s", buf.String())
	}
}

// TestBuildSonarrTrackedSet_SeriesPathOutsideEveryRootSkipsTheFetchEntirely
// is also the [plan-mandated] pin for the Sonarr twin of
// TestBuildRadarrTrackedSet_PathOutsideAnyRootIsCountedNotSilentlyDropped:
// no per-item warn (still avoids flooding a multi-root instance), but the
// occurrence IS counted so it surfaces on the summary.
func TestBuildSonarrTrackedSet_SeriesPathOutsideEveryRootSkipsTheFetchEntirely(t *testing.T) {
	fake := newEpisodeFileFakeServer(t, map[string]string{"1": `[]`})
	client := NewAPIClient(fake.srv.URL, "key")
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "key"}
	roots := []mediaRoot{{arrPath: "/tv_shows", diskPath: "/data/TV"}}
	series := []seriesElement{{ID: intPtr(1), Title: strPtr("Elsewhere"), Path: strPtr("/anime/Elsewhere")}}

	logger, buf := newFileReportTestLogger(slog.LevelDebug)
	_, skipCounts, ok := buildSonarrTrackedSet(context.Background(), logger, client, inst, roots, series, nil, 0, false)
	if !ok {
		t.Fatal("buildSonarrTrackedSet returned ok=false")
	}
	if got := skipCounts[FileSkipReasonOutsideConfiguredRoots]; got != 1 {
		t.Errorf("skipCounts[outside configured roots] = %d, want 1", got)
	}
	if n := fake.requestCount(); n != 0 {
		t.Errorf("a series known to be outside every configured root must not cost an /episodefile fetch, got %d requests", n)
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("a series outside every configured root is not this instance's problem and must not warn:\n%s", buf.String())
	}
}

// TestBuildSonarrTrackedSet_MissingSeriesIDDoesNotTrackTheFolder pins the
// fix for a full-series false-duplicate flood: a series with a readable,
// in-root path but an absent id can never have its episode files fetched (no
// way to call /episodefile), so leaving its folder in set.folders would make
// EVERY real episode on disk classify as a duplicate of a series report has
// zero tracked files for — the exact "reads as delete this" outcome §2.6
// exists to prevent. The fix must warn and exclude the folder entirely,
// mirroring this project's established "missing id field" convention
// (decision.go).
// TestBuildSonarrTrackedSet_MissingSeriesIDDistrustsTheFolderRatherThanOmittingIt
// is the [IMPORTANT] fix for a round-1 relabelled flood: not registering the
// folder at all (the prior fix) left containingTrackedFolder unable to find
// it, so EVERY real, tracked episode file under it fell through to
// fileKindOrphan — one false ORPHAN per real episode, which is the outcome
// §2.6 treats as the worse direction. The fix registers the folder (so
// containment finds it) AND marks it distrusted, so its files resolve to
// skipped-untrusted — neither duplicate nor orphan.
func TestBuildSonarrTrackedSet_MissingSeriesIDDistrustsTheFolderRatherThanOmittingIt(t *testing.T) {
	fake := newEpisodeFileFakeServer(t, map[string]string{})
	client := NewAPIClient(fake.srv.URL, "key")
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "key"}
	roots := []mediaRoot{{arrPath: "/tv_shows", diskPath: "/data/TV"}}
	series := []seriesElement{{ID: nil, Title: strPtr("No ID"), Path: strPtr("/tv_shows/No ID")}}

	logger, buf := newFileReportTestLogger(slog.LevelDebug)
	set, _, ok := buildSonarrTrackedSet(context.Background(), logger, client, inst, roots, series, nil, 0, false)
	if !ok {
		t.Fatal("buildSonarrTrackedSet returned ok=false")
	}
	if set.folders["/data/TV/No ID"] != "No ID" {
		t.Errorf("folders = %v, want the folder registered (so containment can find it and route to skipped-untrusted, not orphan)", set.folders)
	}
	if !set.distrustedFolders["/data/TV/No ID"] {
		t.Errorf("distrustedFolders = %v, want the folder marked distrusted: a series with no id can never have its episode files fetched", set.distrustedFolders)
	}
	if n := fake.requestCount(); n != 0 {
		t.Errorf("a series with no id must never attempt an /episodefile fetch, got %d requests", n)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("a series with no id must warn:\n%s", buf.String())
	}
}

// TestBuildSonarrTrackedSet_EpisodeFileAbsentPathDistrustsTheFolder is the
// [IMPORTANT/plan-mandated] fix: an episode file record with no readable
// path means this fetch cannot say WHICH file on disk it corresponds to, so
// the real file sitting in the series folder for it would otherwise fall
// through to the duplicate branch. Since we cannot know which disk file is
// the untracked one, the whole series folder is marked distrusted (the same
// conservative direction TestBuildSonarrTrackedSet_MissingSeriesIDDistrusts...
// takes) rather than guessing that only "that one file" is affected.
func TestBuildSonarrTrackedSet_EpisodeFileAbsentPathDistrustsTheFolder(t *testing.T) {
	fake := newEpisodeFileFakeServer(t, map[string]string{
		"1": `[{"id":100,"seriesId":1,"seasonNumber":1},
		       {"id":101,"seriesId":1,"seasonNumber":1,"path":"/tv_shows/Show/Show.S01E02.mkv"}]`,
	})
	client := NewAPIClient(fake.srv.URL, "key")
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "key"}
	roots := []mediaRoot{{arrPath: "/tv_shows", diskPath: "/data/TV"}}
	series := []seriesElement{{ID: intPtr(1), Title: strPtr("Show"), Path: strPtr("/tv_shows/Show")}}

	logger, buf := newFileReportTestLogger(slog.LevelDebug)
	set, _, ok := buildSonarrTrackedSet(context.Background(), logger, client, inst, roots, series, nil, 0, false)
	if !ok {
		t.Fatal("buildSonarrTrackedSet returned ok=false")
	}
	// The one file WITH a readable path is still tracked+protected regardless
	// of the folder's distrust.
	if len(set.files) != 1 || !set.files["/data/TV/Show/Show.S01E02.mkv"] {
		t.Errorf("files = %v, want only the file with a readable path", set.files)
	}
	if !set.distrustedFolders["/data/TV/Show"] {
		t.Errorf("distrustedFolders = %v, want the series folder marked distrusted", set.distrustedFolders)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("an episode file with an absent path must warn:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "orphan") {
		t.Errorf("the warning must name the real risk (duplicate), not orphan:\n%s", buf.String())
	}
}

func TestBuildSonarrTrackedSet_EpisodeFileFetchFailureAbortsTheWholeInstance(t *testing.T) {
	fake := newEpisodeFileFakeServer(t, map[string]string{}) // every seriesId 500s
	client := NewAPIClient(fake.srv.URL, "key")
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "key"}
	roots := []mediaRoot{{arrPath: "/tv_shows", diskPath: "/data/TV"}}
	series := []seriesElement{{ID: intPtr(1), Title: strPtr("Show"), Path: strPtr("/tv_shows/Show")}}

	logger, buf := newFileReportTestLogger(slog.LevelDebug)
	_, _, ok := buildSonarrTrackedSet(context.Background(), logger, client, inst, roots, series, nil, 0, false)
	if ok {
		t.Fatal("buildSonarrTrackedSet returned ok=true, want false: a partial tracked set must never be reported as complete")
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning about the fetch failure:\n%s", buf.String())
	}
}

func TestBuildSonarrTrackedSet_ShutdownAborts(t *testing.T) {
	fake := newEpisodeFileFakeServer(t, map[string]string{"1": `[]`, "2": `[]`})
	client := NewAPIClient(fake.srv.URL, "key")
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "key"}
	roots := []mediaRoot{{arrPath: "/tv_shows", diskPath: "/data/TV"}}
	series := []seriesElement{
		{ID: intPtr(1), Title: strPtr("Show 1"), Path: strPtr("/tv_shows/Show 1")},
		{ID: intPtr(2), Title: strPtr("Show 2"), Path: strPtr("/tv_shows/Show 2")},
	}

	logger, _ := newFileReportTestLogger(slog.LevelDebug)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, ok := buildSonarrTrackedSet(ctx, logger, client, inst, roots, series, nil, 0, false)
	if ok {
		t.Fatal("buildSonarrTrackedSet returned ok=true on a cancelled context, want false")
	}
}

// --- buildMismatchedSeasonsIndex ---------------------------------------------
//
// buildMismatchedSeasonsIndex had no test of any kind before this: it is the
// one piece that derives binding controller resolution 5's exclusion index
// from THIS CYCLE's real forward decisions, and a wrong field, a wrong
// reason constant, or an empty return would leave the rest of the suite
// green while every extra file in an untrusted season is confidently
// reported as a duplicate.

func TestBuildMismatchedSeasonsIndex_TableOfReasonsAndSeries(t *testing.T) {
	decisions := []seasonDecision{
		{seriesID: 1, series: "Show A", season: 2, reason: ReasonSeasonFileCountMismatch},
		// Same series, a DIFFERENT reason on another season: must not be
		// indexed (reverse.go treats several reasons as alike for OTHER
		// purposes, but this index is specifically about the file-count
		// mismatch reason and no other).
		{seriesID: 1, series: "Show A", season: 3, reason: ReasonSeasonEpisodesUnavailable},
		// A different series entirely, also file-count-mismatched: must be
		// indexed under its OWN seriesID, never merged with series 1.
		{seriesID: 2, series: "Show B", season: 1, reason: ReasonSeasonFileCountMismatch},
		// A season that passed cleanly: never indexed.
		{seriesID: 1, series: "Show A", season: 1, reason: ""},
	}
	got := buildMismatchedSeasonsIndex(decisions)

	want := map[int]map[int]bool{
		1: {2: true},
		2: {1: true},
	}
	if len(got) != len(want) {
		t.Fatalf("buildMismatchedSeasonsIndex = %v, want %v", got, want)
	}
	for seriesID, seasons := range want {
		if len(got[seriesID]) != len(seasons) {
			t.Errorf("index[%d] = %v, want %v", seriesID, got[seriesID], seasons)
			continue
		}
		for season := range seasons {
			if !got[seriesID][season] {
				t.Errorf("index[%d][%d] = false, want true", seriesID, season)
			}
		}
	}
	if got[1][3] {
		t.Error("a season skipped for a DIFFERENT reason (ReasonSeasonEpisodesUnavailable) must not be indexed as a file-count mismatch")
	}
	if got[1][1] {
		t.Error("a cleanly-passing season must not be indexed")
	}
}

func TestBuildMismatchedSeasonsIndex_NoMismatchesReturnsEmptyIndex(t *testing.T) {
	decisions := []seasonDecision{
		{seriesID: 1, series: "Show A", season: 1, reason: ReasonSeasonEpisodesUnavailable},
	}
	if got := buildMismatchedSeasonsIndex(decisions); len(got) != 0 {
		t.Errorf("buildMismatchedSeasonsIndex = %v, want an empty index", got)
	}
}

// --- runSonarrFileReport -----------------------------------------------------

func TestRunSonarrFileReport_NoMediaRootMapIsOff(t *testing.T) {
	logger, _ := newFileReportTestLogger(slog.LevelDebug)
	client := NewAPIClient("http://unused.invalid", "key")
	c := runSonarrFileReport(context.Background(), logger, slog.LevelInfo, client, Instance{Name: "sonarr-main", Type: "sonarr"}, nil, nil, 0, false)
	if c.state() != "off" {
		t.Errorf("state() = %q, want off", c.state())
	}
}

func TestRunSonarrFileReport_FindingsAreCounted(t *testing.T) {
	fake := newEpisodeFileFakeServer(t, map[string]string{
		"1": `[{"id":100,"seriesId":1,"seasonNumber":1,"path":"/tv_shows/Show/Show.S01E01.mkv"}]`,
	})
	client := NewAPIClient(fake.srv.URL, "key")
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "key",
		MediaRootMap: map[string]string{"/tv_shows": ""}}

	dir := t.TempDir()
	writeFixtureFiles(t, dir, "Show/Show.S01E01.mkv", "Show/Show.S01E01 (2).mkv", "Stray/orphan.mkv")
	inst.MediaRootMap["/tv_shows"] = dir

	series := []seriesElement{{ID: intPtr(1), Title: strPtr("Show"), Path: strPtr("/tv_shows/Show")}}

	logger, _ := newFileReportTestLogger(slog.LevelDebug)
	c := runSonarrFileReport(context.Background(), logger, slog.LevelInfo, client, inst, series, nil, 0, false)
	if c.state() != "ran" {
		t.Fatalf("state() = %q, want ran", c.state())
	}
	if c.duplicates != 1 || c.orphans != 1 {
		t.Errorf("duplicates=%d orphans=%d, want 1/1", c.duplicates, c.orphans)
	}
}

// TestRunSonarrFileReport_MissingSeriesIDDoesNotMisreportRealEpisodesAsOrphans
// is the [IMPORTANT] walk-level pin: round 1's fix for the no-id
// false-duplicate flood relabelled it into a false-ORPHAN flood instead (not
// registering the folder at all left containment unable to find it). This
// proves the corrected behavior end to end: a real episode file physically
// on disk under a no-id series' folder must resolve to neither duplicate
// nor orphan.
func TestRunSonarrFileReport_MissingSeriesIDDoesNotMisreportRealEpisodesAsOrphans(t *testing.T) {
	fake := newEpisodeFileFakeServer(t, map[string]string{
		"2": `[{"id":200,"seriesId":2,"seasonNumber":1,"path":"/tv_shows/Healthy/Healthy.S01E01.mkv"}]`,
	})
	client := NewAPIClient(fake.srv.URL, "key")
	dir := t.TempDir()
	writeFixtureFiles(t, dir, "No ID/No ID.S01E01.mkv", "Healthy/Healthy.S01E01.mkv")
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "key",
		MediaRootMap: map[string]string{"/tv_shows": dir}}
	series := []seriesElement{
		{ID: nil, Title: strPtr("No ID"), Path: strPtr("/tv_shows/No ID")},
		// A second, fully-healthy series so the root's own tracked-file
		// count is not itself zero — this test is about ONE series' own
		// untrustworthy evidence, not the whole-root heuristic (b2)
		// (finding 5, prior round), which is a deliberately separate guard.
		{ID: intPtr(2), Title: strPtr("Healthy"), Path: strPtr("/tv_shows/Healthy")},
	}

	logger, buf := newFileReportTestLogger(slog.LevelInfo)
	c := runSonarrFileReport(context.Background(), logger, slog.LevelInfo, client, inst, series, nil, 0, false)
	if c.state() != "ran" {
		t.Fatalf("state() = %q, want ran", c.state())
	}
	if c.duplicates != 0 || c.orphans != 0 {
		t.Errorf("duplicates=%d orphans=%d, want 0/0: the real episode file under a no-id series must never be reported as either", c.duplicates, c.orphans)
	}
	if got := c.skipReasons[FileSkipReasonUntrackedPath]; got == 0 {
		t.Errorf("skipReasons[untracked path] = %d, want > 0", got)
	}
	if strings.Contains(buf.String(), "kind=duplicate") || strings.Contains(buf.String(), "kind=orphan") {
		t.Errorf("no finding may be logged for a no-id series' folder:\n%s", buf.String())
	}
}

// TestRunSonarrFileReport_OwnEpisodeFilePathUnreadableDoesNotMisreportAsDuplicate
// is the Sonarr twin of the Radarr own-file-path-unreadable walk-level pin.
func TestRunSonarrFileReport_OwnEpisodeFilePathUnreadableDoesNotMisreportAsDuplicate(t *testing.T) {
	fake := newEpisodeFileFakeServer(t, map[string]string{
		// The episode file record has no path at all: this fetch cannot say
		// which real file on disk it corresponds to.
		"1": `[{"id":100,"seriesId":1,"seasonNumber":1}]`,
		"2": `[{"id":200,"seriesId":2,"seasonNumber":1,"path":"/tv_shows/Healthy/Healthy.S01E01.mkv"}]`,
	})
	client := NewAPIClient(fake.srv.URL, "key")
	dir := t.TempDir()
	writeFixtureFiles(t, dir, "Show/Show.S01E01.mkv", "Healthy/Healthy.S01E01.mkv")
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "key",
		MediaRootMap: map[string]string{"/tv_shows": dir}}
	series := []seriesElement{
		{ID: intPtr(1), Title: strPtr("Show"), Path: strPtr("/tv_shows/Show")},
		// A second, fully-healthy series so the root's own tracked-file
		// count is not itself zero — see the sibling test's identical note.
		{ID: intPtr(2), Title: strPtr("Healthy"), Path: strPtr("/tv_shows/Healthy")},
	}

	logger, buf := newFileReportTestLogger(slog.LevelInfo)
	c := runSonarrFileReport(context.Background(), logger, slog.LevelInfo, client, inst, series, nil, 0, false)
	if c.state() != "ran" {
		t.Fatalf("state() = %q, want ran", c.state())
	}
	if c.duplicates != 0 || c.orphans != 0 {
		t.Errorf("duplicates=%d orphans=%d, want 0/0: the only real episode file must never be misreported as a duplicate of itself", c.duplicates, c.orphans)
	}
	if strings.Contains(buf.String(), "kind=duplicate") {
		t.Errorf("no duplicate finding may be logged for this series' folder:\n%s", buf.String())
	}
}

func TestRunSonarrFileReport_TrackedSetBuildFailureIsSkipped(t *testing.T) {
	fake := newEpisodeFileFakeServer(t, map[string]string{}) // 500s every request
	client := NewAPIClient(fake.srv.URL, "key")
	dir := t.TempDir()
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "key",
		MediaRootMap: map[string]string{"/tv_shows": dir}}
	series := []seriesElement{{ID: intPtr(1), Title: strPtr("Show"), Path: strPtr("/tv_shows/Show")}}

	logger, buf := newFileReportTestLogger(slog.LevelDebug)
	c := runSonarrFileReport(context.Background(), logger, slog.LevelInfo, client, inst, series, nil, 0, false)
	if c.state() != "skipped" {
		t.Errorf("state() = %q, want skipped when the tracked set could not be built", c.state())
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a WARN about the failed tracked-set build:\n%s", buf.String())
	}
}

// TestRunSonarrFileReport_AllTrackedPathsUnmappedAbortsTheWholeInstance is
// the Sonarr twin of the Radarr [CRITICAL] fix: every series' path maps to
// none of the configured roots (a case-mismatch typo), so
// buildSonarrTrackedSet skips the /episodefile fetch for all of them
// (relevant=false) and the tracked set comes back completely empty. Without
// the instance-wide guard, the healthy fixture file below would be walked
// and reported as a false orphan.
func TestRunSonarrFileReport_AllTrackedPathsUnmappedAbortsTheWholeInstance(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFiles(t, dir, "Show A/Show A.S01E01.mkv")
	client := NewAPIClient("http://unused.invalid", "key")
	inst := Instance{Name: "sonarr-main", Type: "sonarr", MediaRootMap: map[string]string{"/tv_shows": dir}}
	series := []seriesElement{
		// Configured root is "/tv_shows"; this series reports "/TV_Shows"
		// (case mismatch) — maps nothing.
		{ID: intPtr(1), Title: strPtr("Show A"), Path: strPtr("/TV_Shows/Show A")},
	}
	logger, buf := newFileReportTestLogger(slog.LevelInfo)
	c := runSonarrFileReport(context.Background(), logger, slog.LevelInfo, client, inst, series, nil, 0, false)
	if c.state() != "skipped" {
		t.Fatalf("state() = %q, want skipped: the whole library failed to map, so nothing may be walked", c.state())
	}
	if c.duplicates != 0 || c.orphans != 0 {
		t.Errorf("duplicates=%d orphans=%d, want 0/0: a healthy, fully-unmapped library must never be walked and reported as orphans", c.duplicates, c.orphans)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "media_root_map") {
		t.Errorf("expected a WARN naming media_root_map:\n%s", out)
	}
}

// TestRunSonarrFileReport_UnmappedTrackedPathsWarnOnceAggregatedNotPerItem
// is the Sonarr twin of the Radarr aggregated-WARN pin.
func TestRunSonarrFileReport_UnmappedTrackedPathsWarnOnceAggregatedNotPerItem(t *testing.T) {
	fake := newEpisodeFileFakeServer(t, map[string]string{
		"3": `[{"id":300,"seriesId":3,"seasonNumber":1,"path":"/tv_shows/Show C/Show C.S01E01.mkv"}]`,
	})
	client := NewAPIClient(fake.srv.URL, "key")
	dir := t.TempDir()
	writeFixtureFiles(t, dir, "Show C/Show C.S01E01.mkv")
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "key",
		MediaRootMap: map[string]string{"/tv_shows": dir}}
	series := []seriesElement{
		{ID: intPtr(1), Title: strPtr("Show A"), Path: strPtr("/TV_Shows/Show A")},
		{ID: intPtr(2), Title: strPtr("Show B"), Path: strPtr("/TV_Shows/Show B")},
		{ID: intPtr(3), Title: strPtr("Show C"), Path: strPtr("/tv_shows/Show C")},
	}
	logger, buf := newFileReportTestLogger(slog.LevelInfo)
	c := runSonarrFileReport(context.Background(), logger, slog.LevelInfo, client, inst, series, nil, 0, false)
	if c.state() != "ran" {
		t.Fatalf("state() = %q, want ran", c.state())
	}
	if got := c.skipReasons[FileSkipReasonOutsideConfiguredRoots]; got != 2 {
		t.Fatalf("skipReasons[outside configured roots] = %d, want 2 (Show A + Show B folders)", got)
	}
	out := buf.String()
	if n := strings.Count(out, "media_root_map"); n != 1 {
		t.Errorf("expected exactly one aggregated WARN naming media_root_map, got %d occurrences:\n%s", n, out)
	}
}

// --- scheduling (binding controller resolution 8) ---------------------------
//
// "Reconciliation sweeps and --once full runs only; never webhook-scoped,
// never --only-id." Pinned with the mirrored no-pass tests reverse.go's own
// scheduling rule uses (TestDaemon_WebhookCycle_RunsNoReversePass,
// TestRun_OnlyID_FlagOff_RunsNoReversePass), plus the positive half proving a
// full cycle really does run it.

// assertNoFileReport fails if cycle shows any sign of the file report having
// run. Mirrors assertNoReversePass (reverse_test.go) exactly: matches the
// pass's own frozen tokens, not a bare substring that could also appear
// elsewhere (e.g. in a config dump or an unrelated log line).
func assertNoFileReport(t *testing.T, cycle string) {
	t.Helper()
	for _, token := range []string{`msg="file report"`, `msg="file-report finding"`, "fileReport=", "duplicates=", "orphans=", "fileSkipReasons="} {
		if strings.Contains(cycle, token) {
			t.Errorf("this cycle must run no file report, but its log carries %q:\n%s", token, cycle)
		}
	}
}

// writeFileReportDaemonConfig is writeReverseDaemonConfig's twin: a radarr
// daemon config with media_root_map configured so the file report is
// eligible to run whenever a cycle schedules it at all.
func writeFileReportDaemonConfig(t *testing.T, url, mediaRoot, pollInterval, logLevel string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	content := fmt.Sprintf(`
dry_run: true
log_level: %s
poll_interval: %s
webhook_debounce: 45s
instances:
  - name: radarr-main
    type: radarr
    url: %s
    api_key: key1
    media_root_map:
      /movies: %s
`, logLevel, pollInterval, url, mediaRoot)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing the daemon test config: %v", err)
	}
	return path
}

// writeFileReportOnceTestConfig is the --once twin, for the scoped-run tests
// below.
func writeFileReportOnceTestConfig(t *testing.T, url, mediaRoot string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	content := fmt.Sprintf(`
dry_run: true
instances:
  - name: radarr-main
    type: radarr
    url: %s
    api_key: key1
    media_root_map:
      /movies: %s
`, url, mediaRoot)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	return path
}

// advanceClockUntilReconciliationSweepFires is the [FIX] shared helper this
// project's own root-cause analysis (originally documented inline here)
// already called for: waitReady()/awaitLogCount("reconciliation sweep
// scheduled") close most, but not all, of the race window between the
// startup scan finishing and d.loop()'s first iteration actually reaching
// its own clock.NewTimer(wait) call (daemon.go). A single unretried
// h.clock.Advance can land in that last sliver — before the timer is
// armed — and the sweep it was meant to trigger then silently never fires,
// because nextReconcile ends up computed from the ALREADY-advanced clock
// value, unreachable by that one Advance call.
//
// Retrying Advance is safe: it is idempotent (it only ever moves the fake
// clock further forward and fires whatever is already due), so whichever
// attempt lands after loop() has actually armed its timer wins. This was
// previously written out only in
// TestDaemon_StartupScanAndReconciliationSweep_BothRunTheFileReport, while
// its sibling TestDaemon_IdleCycleWithFileReportFindings_StaysWithinTheNoiseBudget
// used a single, unretried Advance for both of its sweeps — the exact gap
// this helper closes by giving every reconciliation-sweep test the same
// hardening.
func advanceClockUntilReconciliationSweepFires(t *testing.T, h *daemonHarness, mark int, step time.Duration) {
	t.Helper()
	for attempt := 0; attempt < 5; attempt++ {
		h.clock.Advance(step)
		deadline := time.Now().Add(200 * time.Millisecond)
		for time.Now().Before(deadline) {
			if strings.Contains(h.since(mark), "reconciliation sweep complete") {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
}

func TestDaemon_StartupScanAndReconciliationSweep_BothRunTheFileReport(t *testing.T) {
	// The movie's own path/movieFile.path are set (and mapped, with a real
	// fixture file on disk) so every cycle's file report is a genuinely
	// CLEAN, fully-mapped pass — this test is about scheduling (does the
	// file report run on both the startup scan AND the reconciliation
	// sweep), not about the mapping guard, which has its own dedicated
	// coverage (TestRunRadarrFileReport_AllTrackedPathsUnmappedAbortsTheWholeInstance).
	movie := wouldUnmonitorStatefulMovie(1, "Movie")
	movie.path = "/movies/Movie"
	movie.movieFilePath = "/movies/Movie/Movie.mkv"
	dir := t.TempDir()
	writeFixtureFiles(t, dir, "Movie/Movie.mkv")
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{movie})
	h := startDaemon(t, writeFileReportDaemonConfig(t, fake.srv.URL, dir, "1h", "info"))
	h.waitReady()
	if !strings.Contains(h.out.String(), `msg="file report"`) {
		t.Fatalf("the startup scan must run the file report for a configured instance:\n%s", h.out.String())
	}

	// See advanceClockUntilReconciliationSweepFires's doc comment for why a
	// single Advance would race.
	h.awaitLogCount("reconciliation sweep scheduled", 1)

	mark := h.mark()
	advanceClockUntilReconciliationSweepFires(t, h, mark, time.Hour)
	h.awaitLogCount("reconciliation sweep complete", 1)
	h.stop()

	if !strings.Contains(h.since(mark), `msg="file report"`) {
		t.Errorf("the reconciliation sweep must run the file report too:\n%s", h.since(mark))
	}
}

func TestDaemon_WebhookCycle_RunsNoFileReport(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{wouldUnmonitorStatefulMovie(1, "Imported")})
	h := startDaemon(t, writeFileReportDaemonConfig(t, fake.srv.URL, t.TempDir(), "0", "debug"))
	h.waitReady()
	if !strings.Contains(h.out.String(), `msg="file report"`) {
		t.Fatalf("this test proves nothing unless the startup scan really did run the file report:\n%s", h.out.String())
	}

	mark := h.mark()
	h.post("radarr-main", downloadMoviePayload)
	eventually(t, "the event to be queued", func() bool {
		return strings.Contains(h.out.String(), "webhook queued")
	})
	h.clock.Advance(45 * time.Second)
	h.awaitLogCount("webhook debounce expired; evaluating", 1)
	eventually(t, "the webhook cycle to finish", func() bool {
		return strings.Contains(h.since(mark), "radarr decision summary")
	})
	h.stop()

	cycle := h.since(mark)
	if !strings.Contains(cycle, "radarr decision summary") {
		t.Fatalf("the webhook cycle must have run at all:\n%s", cycle)
	}
	assertNoFileReport(t, cycle)
}

func TestRun_OnlyID_RunsNoFileReport(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{wouldUnmonitorStatefulMovie(1, "Named")})

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", writeFileReportOnceTestConfig(t, fake.srv.URL, t.TempDir()), "--once", "--only-id", "1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d; stderr=%s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "onlyId=1") {
		t.Fatalf("this test proves nothing unless the run really was scoped:\n%s", out)
	}
	assertNoFileReport(t, out)
}

func TestRun_FullOnceRun_RunsTheFileReport(t *testing.T) {
	// The movie's own path/movieFile.path are set (and mapped, with a real
	// fixture file on disk) so this is a genuinely CLEAN, fully-mapped pass:
	// proving the FULL --once run wires up the file report at all, distinct
	// from the (separately pinned) case where the tracked set fails to map —
	// see TestRunRadarrFileReport_AllTrackedPathsUnmappedAbortsTheWholeInstance.
	movie := wouldUnmonitorStatefulMovie(1, "Movie")
	movie.path = "/movies/Movie"
	movie.movieFilePath = "/movies/Movie/Movie.mkv"
	dir := t.TempDir()
	writeFixtureFiles(t, dir, "Movie/Movie.mkv")
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{movie})

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", writeFileReportOnceTestConfig(t, fake.srv.URL, dir), "--once"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, `msg="file report"`) || !strings.Contains(out, "fileReport=ran") {
		t.Errorf("a full --once run against a configured, fully-mapped instance must run the file report cleanly:\n%s", out)
	}
}

// TestRunSonarrDecisionEngine_FileReportWiring is the Sonarr twin proving the
// engine-level wiring end to end (scanCycle's gating itself is engine-
// agnostic and already proven above via Radarr); it exercises
// runSonarrDecisionEngine directly rather than the full daemon harness.
func TestRunSonarrDecisionEngine_FileReportWiring(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/qualityprofile", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":1,"name":"HD","upgradeAllowed":true,"cutoffFormatScore":100}]`))
	})
	mux.HandleFunc("/api/v3/tag", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`[]`)) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key",
		MediaRootMap: map[string]string{"/tv_shows": t.TempDir()}}
	var series []seriesElement

	t.Run("disabled: no file report log at all", func(t *testing.T) {
		logger, buf := newFileReportTestLogger(slog.LevelDebug)
		runSonarrDecisionEngine(context.Background(), logger, inst, series, map[int]bool{}, map[seasonKey]bool{},
			"cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseOptions{}, fileReportOptions{})
		assertNoFileReport(t, buf.String())
	})

	t.Run("enabled: file report runs and reports ran", func(t *testing.T) {
		logger, buf := newFileReportTestLogger(slog.LevelDebug)
		runSonarrDecisionEngine(context.Background(), logger, inst, series, map[int]bool{}, map[seasonKey]bool{},
			"cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseOptions{}, fullScanFileReportOptions())
		out := buf.String()
		if !strings.Contains(out, `msg="file report"`) || !strings.Contains(out, "fileReport=ran") {
			t.Errorf("expected the file report to have run for a configured sonarr instance:\n%s", out)
		}
	})
}

// TestRunSonarrDecisionEngine_FileCountMismatchExcludesSeasonFromDuplicates
// is the end-to-end pin buildMismatchedSeasonsIndex's own tests can't reach
// on their own: it drives THIS CYCLE's real forward decision for a
// file-count-mismatched season all the way through
// buildMismatchedSeasonsIndex, buildSonarrTrackedSet and the walk, proving
// an extra file in that season lands in fileSkipReasons under
// FileSkipReasonMismatchedSeason rather than being counted as a duplicate —
// while a non-mismatched season's own extra file IS still reported as an
// ordinary duplicate, so this is a discriminating test, not a vacuous one.
func TestRunSonarrDecisionEngine_FileCountMismatchExcludesSeasonFromDuplicates(t *testing.T) {
	episodesJSON := "[" +
		episodeJSON(100, 1, 1, pastAirDate, 500) + "," +
		episodeJSON(101, 1, 2, pastAirDate, 501) + "," +
		episodeJSON(200, 2, 1, pastAirDate, 600) +
		"]"
	// Season 1's statistics (below) claim 2 files, but only ONE episodefile
	// record actually comes back for it — the file-count mismatch. Season
	// 2's statistics claim 1, and exactly 1 comes back — clean. Both
	// entries carry "path" (which forward-engine decoding simply ignores)
	// so buildSonarrTrackedSet's OWN /episodefile fetch — the same
	// endpoint, reused here for both purposes — can track real files.
	filesJSON := `[` +
		`{"id":500,"seasonNumber":1,"customFormatScore":200,"qualityCutoffNotMet":false,"seriesId":1,"path":"/tv_shows/Mismatch Show/Season 01/Show.S01E01.mkv"},` +
		`{"id":600,"seasonNumber":2,"customFormatScore":200,"qualityCutoffNotMet":false,"seriesId":1,"path":"/tv_shows/Mismatch Show/Season 02/Show.S02E01.mkv"}` +
		`]`

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/qualityprofile", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":1,"name":"HD","upgradeAllowed":true,"cutoffFormatScore":100}]`))
	})
	mux.HandleFunc("/api/v3/tag", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`[]`)) })
	mux.HandleFunc("/api/v3/episode", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(episodesJSON)) })
	mux.HandleFunc("/api/v3/episodefile", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(filesJSON)) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	writeFixtureFiles(t, dir,
		"Mismatch Show/Season 01/Show.S01E01.mkv", // tracked
		"Mismatch Show/Season 01/Show.S01E02.mkv", // extra, in the MISMATCHED season 1 -> must be withheld
		"Mismatch Show/Season 02/Show.S02E01.mkv", // tracked
		"Mismatch Show/Season 02/Show.S02E02.mkv", // extra, in clean season 2 -> ordinary duplicate
	)

	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key",
		MediaRootMap: map[string]string{"/tv_shows": dir}}
	s := testSeries(1, "Mismatch Show", true, 1, []int{}, testSeason(1, true, 2, 2), testSeason(2, true, 1, 1))
	s.Path = strPtr("/tv_shows/Mismatch Show")
	series := []seriesElement{s}
	// Season 2 is placed in the wanted set so rule 4 skips it (ReasonQualityCutoffNotMet)
	// before ever reaching the forward engine's own rule-7 /episodefile
	// fetch — keeping this fixture's forward-pass shape minimal (no
	// would-unmonitor decision on either season, so the write pass has
	// nothing to do). buildSonarrTrackedSet fetches /episodefile
	// independently regardless of what the forward pass decided, so season
	// 2's own file is still tracked and its extra file is still classified.
	wantedSeasons := map[seasonKey]bool{{seriesID: 1, seasonNumber: 2}: true}

	logger, buf := newFileReportTestLogger(slog.LevelDebug)
	runSonarrDecisionEngine(context.Background(), logger, inst, series, map[int]bool{}, wantedSeasons,
		"cutoffarr-exclude", fullLibraryScope(slog.LevelInfo), true, reverseOptions{}, fullScanFileReportOptions())

	out := buf.String()
	if !strings.Contains(out, "fileReport=ran") {
		t.Fatalf("the file report must have run:\n%s", out)
	}
	if !strings.Contains(out, `kind=duplicate`) || !strings.Contains(out, `group=S02E02`) {
		t.Errorf("season 2's extra file must still be reported as an ordinary duplicate:\n%s", out)
	}
	if strings.Contains(out, `kind=duplicate`) && strings.Contains(out, `S01E02`) {
		t.Errorf("season 1's extra file must NEVER be reported as a duplicate (file-count mismatch season):\n%s", out)
	}
	if !strings.Contains(out, "duplicates=1") {
		t.Errorf("want exactly 1 duplicate finding (season 2's extra only):\n%s", out)
	}
	if !strings.Contains(out, FileSkipReasonMismatchedSeason) {
		t.Errorf("want the mismatched-season skip reason on the summary:\n%s", out)
	}
}

// --- noise budget: the two-cycle idle test, extended (binding controller
// resolution 7) --------------------------------------------------------------

// newFileReportOnlyRadarrFake serves the minimum Radarr API this test needs,
// with one movie whose path and movieFile.path are set — unlike
// statefulRadarrFake (writer_test.go), which never emits either field. The
// movie is unmonitored so no forward decision, cross-check sample, or write
// pass has anything to do: this test's only subject is the file-report
// summary's own noise-budget compliance across repeated idle sweeps.
func newFileReportOnlyRadarrFake(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/system/status", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"appName":"Radarr","version":"5.14.0.9383"}`))
	})
	mux.HandleFunc("/api/v3/qualityprofile", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":1,"name":"HD","upgradeAllowed":true,"cutoff":7,"cutoffFormatScore":100}]`))
	})
	mux.HandleFunc("/api/v3/tag", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`[]`)) })
	mux.HandleFunc("/api/v3/wanted/cutoff", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"page":1,"totalRecords":0,"records":[]}`))
	})
	mux.HandleFunc("/api/v3/movie", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":1,"title":"Movie A","monitored":false,"hasFile":true,"qualityProfileId":1,"tags":[],
			"path":"/movies/Movie A","movieFile":{"id":1,"path":"/movies/Movie A/Movie A.mkv"}}]`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestDaemon_IdleCycleWithFileReportFindings_StaysWithinTheNoiseBudget(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFiles(t, dir, "Movie A/Movie A.mkv", "Movie A/Movie A (2).mkv", "Stray/orphan.mkv")
	srv := newFileReportOnlyRadarrFake(t)

	h := startDaemon(t, writeFileReportDaemonConfig(t, srv.URL, dir, "1h", "info"))
	h.waitReady()

	startup := h.out.String()
	if !strings.Contains(startup, `msg="file-report finding"`) {
		t.Fatalf("the startup scan must report the findings in full:\n%s", startup)
	}
	if n := strings.Count(startup, `msg="file-report finding"`); n != 2 {
		t.Fatalf("want exactly 2 file-report findings (1 duplicate + 1 orphan) at startup, got %d:\n%s", n, startup)
	}
	eventually(t, "the reconciliation schedule to be announced", func() bool {
		return strings.Contains(h.out.String(), "reconciliation sweep scheduled")
	})

	// See advanceClockUntilReconciliationSweepFires's doc comment for why a
	// single, unretried Advance races the daemon's own timer arming.
	mark := h.mark()
	advanceClockUntilReconciliationSweepFires(t, h, mark, time.Hour)
	h.awaitLogCount("reconciliation sweep complete", 1)
	cycle2 := h.since(mark)

	mark3 := h.mark()
	advanceClockUntilReconciliationSweepFires(t, h, mark3, time.Hour)
	h.awaitLogCount("reconciliation sweep complete", 2)
	cycle3 := h.since(mark3)
	h.stop()

	for i, cycle := range []string{cycle2, cycle3} {
		if !assertIdleCycleInfoIsWithinTheBudget(t, cycle) {
			t.Errorf("cycle %d printed no per-instance decision summary:\n%s", i+2, cycle)
		}
		// Per-finding lines are demoted to debug on a sweep (scope.itemLevel),
		// so they must NOT appear at all with log_level: info — only the
		// summary's counts stay visible, exactly like the reverse scan's own
		// idle-cycle pin.
		if strings.Contains(cycle, `msg="file-report finding"`) {
			t.Errorf("cycle %d: per-finding lines must be demoted to debug on a repeating sweep:\n%s", i+2, cycle)
		}
		if !strings.Contains(cycle, "duplicates=1") || !strings.Contains(cycle, "orphans=1") {
			t.Errorf("cycle %d must still COUNT the findings on its summary:\n%s", i+2, cycle)
		}
	}
	if strip2, strip3 := withoutTimestamps(dropNextSweep(cycle2)), withoutTimestamps(dropNextSweep(cycle3)); strip2 != strip3 {
		t.Errorf("two idle cycles with nothing changed must say the same thing:\ncycle2:\n%s\ncycle3:\n%s", strip2, strip3)
	}
}

// --- the filesystem-mutation audit (Phase 10 branch review, carried forward
// as a binding Phase 11 requirement) -----------------------------------------
//
// The permanent rule stated first because it is structural, not a flag: this
// phase (and every phase after it) never writes, deletes, moves, or renames a
// file. TestTree_HasExactlyThreeWriteVerbCallSites (writer_test.go) has
// audited this project's HTTP write surface since Phase 8; this is that
// audit's filesystem twin, extended to cover the one new class of mutation
// Phase 11 introduces the ability to make a mistake about: os package calls
// that create, delete, rename, or truncate a file or directory.
//
// It is an ALLOWLIST-of-nothing rather than a forbidden-substring test for
// the same reason idleCycleAllowedInfo (daemon_test.go) is an allowlist: the
// failure mode this guards against is someone adding a legitimate-looking
// os.WriteFile "just for a cache file" three phases from now, and a bare
// substring ban is exactly as effective at stopping that as an allowlist that
// starts empty and has to be argued into non-empty.

// fsMutationAllowlist names files permitted to call a filesystem-mutation API
// outside a test file. It is empty: nothing in this project needs to create,
// delete, rename, or truncate anything on disk, and this phase's own
// filereport.go — the one file with any reason to go near the media
// filesystem at all — is walk-and-stat only, appearing here would be exactly
// the drift the binding Phase 10 branch review ordered this test to prevent.
var fsMutationAllowlist = map[string]bool{}

// fsMutationAPIs are the calls that mutate the filesystem: create, delete,
// rename, truncate, or open for writing. Named individually (not a single
// "Remove|Rename|..." substring) so the failure message for each is
// unambiguous about which one fired.
//
// [FIX] Originally os-package-only, extended to close the two likeliest
// drift routes around it: shelling out (os/exec — a subprocess can mutate
// the filesystem in ways no static scan of THIS tree could ever catch, so
// the import itself is banned, not just one constructor call on it) and the
// raw syscall package, which every os-package mutator above is a thin
// wrapper over and could be called directly to bypass this exact list.
// syscall.* is intentionally NOT banned as a bare prefix: daemon.go
// legitimately calls signal.Notify(..., syscall.SIGTERM, syscall.SIGINT)
// for graceful-shutdown signal handling, which has nothing to do with the
// filesystem — a bare "syscall." ban would false-positive on that
// pre-existing, correct code. Only the specific raw mutation calls (the
// syscall-level twins of the os-package functions already listed) are
// named.
var fsMutationAPIs = []string{
	"os.Remove(",
	"os.RemoveAll(",
	"os.Rename(",
	"os.Create(",
	"os.CreateTemp(",
	"os.WriteFile(",
	"os.OpenFile(",
	"os.Mkdir(",
	"os.MkdirAll(",
	"os.MkdirTemp(",
	"os.Truncate(",
	"os.Symlink(",
	"os.Link(",
	"os.Chmod(",
	"os.Chown(",
	"os.Chtimes(",
	"os.Lchown(",
	`"os/exec"`,
	"exec.Command(",
	"syscall.Unlink(",
	"syscall.Rmdir(",
	"syscall.Rename(",
	"syscall.Mkdir(",
	"syscall.Truncate(",
	"syscall.Chmod(",
	"syscall.Chown(",
	"syscall.Symlink(",
	"syscall.Link(",
}

// minScannedNonTestGoFiles is [FIX] the vacuity-guard floor for
// TestTree_BansFilesystemMutationAPIsEverywhere: comfortably below this
// project's actual non-test .go file count (15 as of this writing) so
// ordinary file churn never trips it, but high enough that a walk finding
// close to nothing — the working directory changing out from under the
// test, a filter bug excluding everything real, WalkDir silently erroring
// out on the first entry — cannot pass silently.
const minScannedNonTestGoFiles = 10

func TestTree_BansFilesystemMutationAPIsEverywhere(t *testing.T) {
	scanned := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != "." && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		name := filepath.ToSlash(path)
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		scanned++
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		body := string(src)

		for _, api := range fsMutationAPIs {
			if strings.Contains(body, api) && !fsMutationAllowlist[name] {
				t.Errorf("%s calls %s: this project's filesystem access must stay read-only outside the (empty) allowlist — the permanent no-file-writes rule extends from the *arr APIs to the disk itself", name, api)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}

	// [FIX] vacuity guard: without this, a walk that (for whatever reason)
	// finds none of the project's real source files passes this audit
	// forever, having verified nothing — an audit that can never see a
	// real violation is worse than no audit at all, because it still looks
	// green. TestTree_HasExactlyThreeWriteVerbCallSites (writer_test.go)
	// gets this for free from its exact-count ("!= 3") assertion; this
	// test's assertions are all per-file t.Errorf calls with no equivalent
	// built-in floor, so the floor is made explicit here instead.
	if scanned < minScannedNonTestGoFiles {
		t.Fatalf("scanned only %d non-test .go file(s), want at least %d: this audit is vacuous unless the walk is actually finding the project's source", scanned, minScannedNonTestGoFiles)
	}
}

// --- the plan's verbatim acceptance scenario (Phase 11, HUMAN GATE) --------
//
// "drop a spare .mkv copy next to one tracked episode and one stray .mkv in
// an untracked location under a root -> sweep reports exactly one duplicate
// (grouped) and one orphan; delete nothing; unmount test triggers the
// mount-problem abort."
//
// Run as a Sonarr fixture (an "episode" is the plan's own noun, and Sonarr's
// grouping-by-S01E05 display convenience is the more demanding of the two
// engines to get right), through runSonarrFileReport end to end: tracked-set
// build, mount-problem heuristic, walk, classification, and grouping all in
// one pass, exactly as a real reconciliation sweep would run it.
func TestAcceptance_Phase11_DuplicateAndOrphanScenario(t *testing.T) {
	dir := t.TempDir()
	trackedRel := filepath.Join("Show", "Show.S01E05.mkv")
	duplicateRel := filepath.Join("Show", "Show.S01E05 (2).mkv")
	orphanRel := filepath.Join("Untracked Location", "stray.mkv")
	written := writeFixtureFiles(t, dir, trackedRel, duplicateRel, orphanRel)

	fake := newEpisodeFileFakeServer(t, map[string]string{
		"1": fmt.Sprintf(`[{"id":500,"seriesId":1,"seasonNumber":1,"path":"/tv_shows/Show/Show.S01E05.mkv"}]`),
	})
	client := NewAPIClient(fake.srv.URL, "key")
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "key",
		MediaRootMap: map[string]string{"/tv_shows": dir}}
	series := []seriesElement{{ID: intPtr(1), Title: strPtr("Show"), Path: strPtr("/tv_shows/Show")}}

	logger, buf := newFileReportTestLogger(slog.LevelInfo)
	c := runSonarrFileReport(context.Background(), logger, slog.LevelInfo, client, inst, series, nil, 0, false)

	if c.state() != "ran" {
		t.Fatalf("state() = %q, want ran:\n%s", c.state(), buf.String())
	}
	if c.duplicates != 1 {
		t.Errorf("duplicates = %d, want exactly 1", c.duplicates)
	}
	if c.orphans != 1 {
		t.Errorf("orphans = %d, want exactly 1", c.orphans)
	}

	out := buf.String()
	if !strings.Contains(out, `kind=duplicate`) || !strings.Contains(out, `group=S01E05`) {
		t.Errorf("expected a grouped duplicate finding naming S01E05:\n%s", out)
	}
	if !strings.Contains(out, `kind=orphan`) || !strings.Contains(out, "Untracked Location") {
		t.Errorf("expected an orphan finding naming the untracked location:\n%s", out)
	}

	// "delete nothing": every fixture file must still be exactly where it was
	// written — the strongest form of "read-only" this test can check.
	for _, p := range written {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("file report must never delete anything, but %s: %v", p, err)
		}
	}
}

// TestAcceptance_Phase11_UnmountTriggersTheMountProblemAbort is the
// scenario's second half: pointing media_root_map at a path that does not
// exist — the file-report analogue of an unmounted share — must abort that
// root's report rather than report the tracked episode (and everything else
// that would have been under it) as a flood of false orphans.
func TestAcceptance_Phase11_UnmountTriggersTheMountProblemAbort(t *testing.T) {
	unmounted := filepath.Join(t.TempDir(), "not-actually-mounted")

	fake := newEpisodeFileFakeServer(t, map[string]string{
		"1": `[{"id":500,"seriesId":1,"seasonNumber":1,"path":"/tv_shows/Show/Show.S01E05.mkv"}]`,
	})
	client := NewAPIClient(fake.srv.URL, "key")
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "key",
		MediaRootMap: map[string]string{"/tv_shows": unmounted}}
	series := []seriesElement{{ID: intPtr(1), Title: strPtr("Show"), Path: strPtr("/tv_shows/Show")}}

	logger, buf := newFileReportTestLogger(slog.LevelInfo)
	c := runSonarrFileReport(context.Background(), logger, slog.LevelInfo, client, inst, series, nil, 0, false)

	if c.state() != "skipped" {
		t.Fatalf("state() = %q, want skipped: the mount-problem heuristic must abort an unmounted root", c.state())
	}
	if c.duplicates != 0 || c.orphans != 0 {
		t.Errorf("an aborted root must report zero findings, got duplicates=%d orphans=%d — never a flood of false orphans from an unmounted share", c.duplicates, c.orphans)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a WARN naming the aborted root:\n%s", buf.String())
	}
}
