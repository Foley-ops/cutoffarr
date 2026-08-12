package main

import (
	"bytes"
	"context"
	"fmt"
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

func TestEvaluateFileReportRoot_TrackedFilesButZeroVideoFilesOnWalkAborts(t *testing.T) {
	dir := t.TempDir()
	// The tracked file itself is missing (so heuristic b already would catch a
	// LARGE tracked set) but here there is exactly one tracked file and NO
	// video files anywhere on disk at all — heuristic (c) specifically.
	writeFixtureFiles(t, dir, "Movie A/poster.jpg")
	files := map[string]bool{filepath.Join(dir, "Movie A", "Movie A.mkv"): true}
	root := mediaRoot{arrPath: "/movies", diskPath: dir}
	set := instanceTrackedSet{files: files, folders: map[string]string{filepath.Join(dir, "Movie A"): "Movie A"}}
	logger, buf := newFileReportTestLogger(slog.LevelDebug)
	outcome := evaluateFileReportRoot(context.Background(), logger, slog.LevelInfo, Instance{Name: "radarr-main", Type: "radarr"}, root, set)
	if !outcome.skipped {
		t.Fatal("a root tracking N>0 files with zero video files found on walk must abort (mount-problem heuristic c)")
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a WARN naming the root:\n%s", buf.String())
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
	c := classifyFileReportPath("/data/TV/Show/Show.S01E05.mkv", root, set)
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
	c := classifyFileReportPath("/data/TV/Show/Show.S02E05.mkv", root, set)
	if c.kind != fileKindDuplicate {
		t.Errorf("classify(season 2, mismatch only on season 1) = %+v, want duplicate", c)
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
	set := buildRadarrTrackedSet(logger, Instance{Name: "radarr-main", Type: "radarr"}, roots, movies)

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
	set := buildRadarrTrackedSet(logger, Instance{Name: "radarr-main", Type: "radarr"}, roots, movies)
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
	set := buildRadarrTrackedSet(logger, Instance{Name: "radarr-main", Type: "radarr"}, roots, movies)
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

func TestBuildRadarrTrackedSet_HasFileButAbsentMovieFilePathWarnsAndExcludes(t *testing.T) {
	roots := []mediaRoot{{arrPath: "/movies", diskPath: "/data/Movies"}}
	movies := []movieListElement{
		{ID: intPtr(1), Title: strPtr("Broken"), HasFile: boolPtr(true), Path: strPtr("/movies/Broken"), MovieFile: nil},
	}
	logger, buf := newFileReportTestLogger(slog.LevelDebug)
	set := buildRadarrTrackedSet(logger, Instance{Name: "radarr-main", Type: "radarr"}, roots, movies)
	if len(set.files) != 0 {
		t.Errorf("files = %v, want none: movieFile.path could not be read", set.files)
	}
	// The folder is still tracked (its own path WAS readable), so any extra
	// file sitting in it is correctly a duplicate, not a false orphan.
	if set.folders["/data/Movies/Broken"] != "Broken" {
		t.Errorf("folders = %v, want the folder still tracked", set.folders)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("hasFile=true with an unreadable movieFile.path must warn:\n%s", buf.String())
	}
}

func TestBuildRadarrTrackedSet_PathOutsideAnyRootIsIgnoredSilently(t *testing.T) {
	roots := []mediaRoot{{arrPath: "/movies", diskPath: "/data/Movies"}}
	movies := []movieListElement{
		{
			ID: intPtr(1), Title: strPtr("Elsewhere"), HasFile: boolPtr(true),
			Path:      strPtr("/other-library/Elsewhere"),
			MovieFile: &movieFileElement{Path: strPtr("/other-library/Elsewhere/file.mkv")},
		},
	}
	logger, buf := newFileReportTestLogger(slog.LevelDebug)
	set := buildRadarrTrackedSet(logger, Instance{Name: "radarr-main", Type: "radarr"}, roots, movies)
	if len(set.files) != 0 || len(set.folders) != 0 {
		t.Errorf("a movie entirely outside every configured root must contribute nothing: %+v", set)
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("a movie outside every configured root is not this instance's problem and must not warn:\n%s", buf.String())
	}
}

// --- runRadarrFileReport + the off/skipped/ran summary vocabulary ----------
//
// Binding controller resolution 1: media_root_map absent means OFF for that
// instance, distinguishable from skipped (configured but aborted) and from a
// clean ran (duplicates=0 orphans=0).

func TestRunRadarrFileReport_NoMediaRootMapIsOff(t *testing.T) {
	logger, _ := newFileReportTestLogger(slog.LevelDebug)
	c := runRadarrFileReport(context.Background(), logger, slog.LevelInfo, Instance{Name: "radarr-main", Type: "radarr"}, nil)
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
	c := runRadarrFileReport(context.Background(), logger, slog.LevelInfo, inst, movies)
	if c.state() != "ran" {
		t.Errorf("state() = %q, want ran", c.state())
	}
	if c.duplicates != 0 || c.orphans != 0 {
		t.Errorf("duplicates=%d orphans=%d, want 0/0 for a clean library", c.duplicates, c.orphans)
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
	c := runRadarrFileReport(context.Background(), logger, slog.LevelInfo, inst, movies)
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
	set, ok := buildSonarrTrackedSet(context.Background(), logger, client, inst, roots, series, nil)
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

func TestBuildSonarrTrackedSet_MismatchedSeasonsCarriedThroughByFolder(t *testing.T) {
	fake := newEpisodeFileFakeServer(t, map[string]string{"1": `[]`})
	client := NewAPIClient(fake.srv.URL, "key")
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "key"}
	roots := []mediaRoot{{arrPath: "/tv_shows", diskPath: "/data/TV"}}
	series := []seriesElement{{ID: intPtr(1), Title: strPtr("Show"), Path: strPtr("/tv_shows/Show")}}
	mismatched := map[int]map[int]bool{1: {2: true}}

	logger, _ := newFileReportTestLogger(slog.LevelDebug)
	set, ok := buildSonarrTrackedSet(context.Background(), logger, client, inst, roots, series, mismatched)
	if !ok {
		t.Fatal("buildSonarrTrackedSet returned ok=false")
	}
	if !set.mismatchedSeasons["/data/TV/Show"][2] {
		t.Errorf("mismatchedSeasons = %v, want season 2 excluded under the mapped folder", set.mismatchedSeasons)
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
	set, ok := buildSonarrTrackedSet(context.Background(), logger, client, inst, roots, series, nil)
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

func TestBuildSonarrTrackedSet_SeriesPathOutsideEveryRootSkipsTheFetchEntirely(t *testing.T) {
	fake := newEpisodeFileFakeServer(t, map[string]string{"1": `[]`})
	client := NewAPIClient(fake.srv.URL, "key")
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "key"}
	roots := []mediaRoot{{arrPath: "/tv_shows", diskPath: "/data/TV"}}
	series := []seriesElement{{ID: intPtr(1), Title: strPtr("Elsewhere"), Path: strPtr("/anime/Elsewhere")}}

	logger, buf := newFileReportTestLogger(slog.LevelDebug)
	_, ok := buildSonarrTrackedSet(context.Background(), logger, client, inst, roots, series, nil)
	if !ok {
		t.Fatal("buildSonarrTrackedSet returned ok=false")
	}
	if n := fake.requestCount(); n != 0 {
		t.Errorf("a series known to be outside every configured root must not cost an /episodefile fetch, got %d requests", n)
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("a series outside every configured root is not this instance's problem and must not warn:\n%s", buf.String())
	}
}

func TestBuildSonarrTrackedSet_EpisodeFileAbsentPathWarnsAndExcludesJustThatFile(t *testing.T) {
	fake := newEpisodeFileFakeServer(t, map[string]string{
		"1": `[{"id":100,"seriesId":1,"seasonNumber":1},
		       {"id":101,"seriesId":1,"seasonNumber":1,"path":"/tv_shows/Show/Show.S01E02.mkv"}]`,
	})
	client := NewAPIClient(fake.srv.URL, "key")
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "key"}
	roots := []mediaRoot{{arrPath: "/tv_shows", diskPath: "/data/TV"}}
	series := []seriesElement{{ID: intPtr(1), Title: strPtr("Show"), Path: strPtr("/tv_shows/Show")}}

	logger, buf := newFileReportTestLogger(slog.LevelDebug)
	set, ok := buildSonarrTrackedSet(context.Background(), logger, client, inst, roots, series, nil)
	if !ok {
		t.Fatal("buildSonarrTrackedSet returned ok=false")
	}
	if len(set.files) != 1 || !set.files["/data/TV/Show/Show.S01E02.mkv"] {
		t.Errorf("files = %v, want only the file with a readable path", set.files)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("an episode file with an absent path must warn:\n%s", buf.String())
	}
}

func TestBuildSonarrTrackedSet_EpisodeFileFetchFailureAbortsTheWholeInstance(t *testing.T) {
	fake := newEpisodeFileFakeServer(t, map[string]string{}) // every seriesId 500s
	client := NewAPIClient(fake.srv.URL, "key")
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "key"}
	roots := []mediaRoot{{arrPath: "/tv_shows", diskPath: "/data/TV"}}
	series := []seriesElement{{ID: intPtr(1), Title: strPtr("Show"), Path: strPtr("/tv_shows/Show")}}

	logger, buf := newFileReportTestLogger(slog.LevelDebug)
	_, ok := buildSonarrTrackedSet(context.Background(), logger, client, inst, roots, series, nil)
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
	_, ok := buildSonarrTrackedSet(ctx, logger, client, inst, roots, series, nil)
	if ok {
		t.Fatal("buildSonarrTrackedSet returned ok=true on a cancelled context, want false")
	}
}

// --- runSonarrFileReport -----------------------------------------------------

func TestRunSonarrFileReport_NoMediaRootMapIsOff(t *testing.T) {
	logger, _ := newFileReportTestLogger(slog.LevelDebug)
	client := NewAPIClient("http://unused.invalid", "key")
	c := runSonarrFileReport(context.Background(), logger, slog.LevelInfo, client, Instance{Name: "sonarr-main", Type: "sonarr"}, nil, nil)
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
	c := runSonarrFileReport(context.Background(), logger, slog.LevelInfo, client, inst, series, nil)
	if c.state() != "ran" {
		t.Fatalf("state() = %q, want ran", c.state())
	}
	if c.duplicates != 1 || c.orphans != 1 {
		t.Errorf("duplicates=%d orphans=%d, want 1/1", c.duplicates, c.orphans)
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
	c := runSonarrFileReport(context.Background(), logger, slog.LevelInfo, client, inst, series, nil)
	if c.state() != "skipped" {
		t.Errorf("state() = %q, want skipped when the tracked set could not be built", c.state())
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a WARN about the failed tracked-set build:\n%s", buf.String())
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

func TestDaemon_StartupScanAndReconciliationSweep_BothRunTheFileReport(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{wouldUnmonitorStatefulMovie(1, "Movie")})
	h := startDaemon(t, writeFileReportDaemonConfig(t, fake.srv.URL, t.TempDir(), "1h", "info"))
	h.waitReady()
	if !strings.Contains(h.out.String(), `msg="file report"`) {
		t.Fatalf("the startup scan must run the file report for a configured instance:\n%s", h.out.String())
	}

	mark := h.mark()
	h.clock.Advance(time.Hour)
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
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{wouldUnmonitorStatefulMovie(1, "Movie")})

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", writeFileReportOnceTestConfig(t, fake.srv.URL, t.TempDir()), "--once"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, `msg="file report"`) || !strings.Contains(out, "fileReport=ran") {
		t.Errorf("a full --once run against a configured instance must run the file report:\n%s", out)
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

	mark := h.mark()
	h.clock.Advance(time.Hour)
	h.awaitLogCount("reconciliation sweep complete", 1)
	cycle2 := h.since(mark)

	mark3 := h.mark()
	h.clock.Advance(time.Hour)
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
