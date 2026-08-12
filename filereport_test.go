package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
