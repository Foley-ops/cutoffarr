package main

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// actions_test.go covers v2.2's human-clicked action system (actions.go).
//
// Two disciplines run through every test here, and they are the reason this
// file exists separately from webui_test.go:
//
//   - Nothing in this suite ever touches a real media mount. File behavior is
//     exercised against t.TempDir() fixtures; *arr behavior against httptest
//     fakes. The one test that needs a read-only filesystem makes one, with
//     chmod, in a temp dir it owns.
//   - Every switch combination is tested for its OWN outcome, not merely for
//     "nothing happened": the entire point of the rehearsal semantics is that
//     a dry-run action does the full re-verification and then says what it
//     would have done, which is a different observable result from a refusal
//     and from a disabled switch.

// --- fixtures ---------------------------------------------------------------

func newActionTestLogger() (*slog.Logger, *strings.Builder) {
	buf := &strings.Builder{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// writeActionFile creates dir/rel with the given contents, making parents.
func writeActionFile(t *testing.T, root, rel, contents string) string {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("creating fixture parent for %s: %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		t.Fatalf("writing fixture %s: %v", rel, err)
	}
	return full
}

// --- trash path construction ------------------------------------------------

// TestTrashDestination_MirrorsTheOriginalPathUnderAStampedTrashDir pins rule 4
// of the action constitution: a removal is a rename-move into
// <root>/.cutoffarr-trash/<RFC3339-stamp>/<original-relative-path>. The
// relative path is MIRRORED, not flattened, because the whole restore story
// ("move it back") depends on a human being able to read where it came from
// off the trash path itself.
func TestTrashDestination_MirrorsTheOriginalPathUnderAStampedTrashDir(t *testing.T) {
	root := "/media/tv"
	src := "/media/tv/Show/Season 01/Show.S01E05.mkv"
	stamp := time.Date(2026, 8, 13, 9, 4, 5, 0, time.UTC)

	got, err := trashDestination(root, src, stamp)
	if err != nil {
		t.Fatalf("trashDestination returned error: %v", err)
	}
	want := filepath.Join(root, trashDirName, "2026-08-13T09:04:05Z", "Show", "Season 01", "Show.S01E05.mkv")
	if got != want {
		t.Errorf("trashDestination = %q, want %q", got, want)
	}
}

// TestTrashDestination_RefusesAPathOutsideItsRoot is the containment guard: a
// destination is only computable for a file that actually lives under the root
// it is being trashed into. Without this, a request naming "/etc/passwd" with
// root "/media/tv" would produce a destination built from a "../.."-laden
// relative path and the rename would land somewhere nobody asked for.
func TestTrashDestination_RefusesAPathOutsideItsRoot(t *testing.T) {
	if _, err := trashDestination("/media/tv", "/etc/passwd", time.Now()); err == nil {
		t.Fatal("trashDestination accepted a path outside its root; it must refuse")
	}
}

// TestTrashDestination_RefusesAPathAlreadyInsideTheTrash stops the one loop
// this design can form: trashing something out of the trash would nest a
// second stamp directory inside the first and make the restore path a lie.
func TestTrashDestination_RefusesAPathAlreadyInsideTheTrash(t *testing.T) {
	src := filepath.Join("/media/tv", trashDirName, "2026-08-13T09:04:05Z", "Show", "x.mkv")
	if _, err := trashDestination("/media/tv", src, time.Now()); err == nil {
		t.Fatal("trashDestination accepted a path already inside the trash; it must refuse")
	}
}

// TestUniquifyTrashPath_NeverOverwritesAnExistingTrashEntry is rule 4's
// "never overwrite in trash" half. os.Rename silently clobbers an existing
// destination file on Unix, so the ONLY thing standing between two trashes of
// the same relative path in the same second and a permanently lost file is
// this function.
func TestUniquifyTrashPath_NeverOverwritesAnExistingTrashEntry(t *testing.T) {
	dir := t.TempDir()
	occupied := writeActionFile(t, dir, "Show/x.mkv", "first")

	got, err := uniquifyTrashPath(occupied)
	if err != nil {
		t.Fatalf("uniquifyTrashPath returned error: %v", err)
	}
	if got == occupied {
		t.Fatalf("uniquifyTrashPath returned the occupied path %q unchanged; the existing trash entry would be overwritten", got)
	}
	if !strings.HasSuffix(got, ".mkv") {
		t.Errorf("uniquifyTrashPath = %q, want the original extension preserved", got)
	}

	// And again, with the first alternative also occupied.
	writeActionFile(t, dir, filepath.ToSlash(mustRel(t, dir, got)), "second")
	third, err := uniquifyTrashPath(occupied)
	if err != nil {
		t.Fatalf("uniquifyTrashPath returned error on the second collision: %v", err)
	}
	if third == occupied || third == got {
		t.Errorf("uniquifyTrashPath = %q, want a third distinct path (both %q and %q are occupied)", third, occupied, got)
	}
}

func mustRel(t *testing.T, base, target string) string {
	t.Helper()
	rel, err := filepath.Rel(base, target)
	if err != nil {
		t.Fatalf("filepath.Rel(%q, %q): %v", base, target, err)
	}
	return rel
}

// TestUniquifyTrashPath_LeavesAFreePathAlone keeps the common case boring.
func TestUniquifyTrashPath_LeavesAFreePathAlone(t *testing.T) {
	dir := t.TempDir()
	free := filepath.Join(dir, "nothing-here.mkv")
	got, err := uniquifyTrashPath(free)
	if err != nil {
		t.Fatalf("uniquifyTrashPath returned error: %v", err)
	}
	if got != free {
		t.Errorf("uniquifyTrashPath = %q, want the unoccupied path %q unchanged", got, free)
	}
}

// --- the read-only mount probe ----------------------------------------------

// TestProbeRootWritable_ReadOnlyMountIsAnHonestRefusalNamingTheRoot is rule 8.
// The deploy compose keeps the media mount :ro by default, so this is not an
// exotic failure — it is the DEFAULT deployment's answer to every file button,
// and it has to name the mount and the fix rather than surfacing a bare
// "permission denied" from six frames down.
func TestProbeRootWritable_ReadOnlyMountIsAnHonestRefusalNamingTheRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if os.Geteuid() == 0 {
		t.Skip("running as root: a 0555 directory is still writable, so this probe cannot be exercised")
	}

	err := probeRootWritable(dir)
	if err == nil {
		t.Fatal("probeRootWritable returned nil on a read-only directory")
	}
	msg := err.Error()
	if !strings.Contains(msg, dir) {
		t.Errorf("the refusal must name the mount it could not write to; got %q", msg)
	}
	for _, want := range []string{"read-only", "mount"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal must state the cause and the fix (%q missing); got %q", want, msg)
		}
	}
}

// TestProbeRootWritable_WritableRootPassesAndLeavesTheTrashDirBehind pins that
// the probe IS the trash-directory create (rule 8: "attempt the trash-dir
// create as the probe, at action time") rather than a separate, weaker test
// that could succeed where the real operation would fail.
func TestProbeRootWritable_WritableRootPassesAndLeavesTheTrashDirBehind(t *testing.T) {
	dir := t.TempDir()
	if err := probeRootWritable(dir); err != nil {
		t.Fatalf("probeRootWritable returned error on a writable dir: %v", err)
	}
	if info, err := os.Stat(filepath.Join(dir, trashDirName)); err != nil || !info.IsDir() {
		t.Errorf("the probe must be the trash-dir create itself; %s/%s: err=%v", dir, trashDirName, err)
	}
}

// --- moveToTrash ------------------------------------------------------------

func TestMoveToTrash_MovesTheFileAndMirrorsItsRelativePath(t *testing.T) {
	root := t.TempDir()
	src := writeActionFile(t, root, "Show/Season 01/dupe.mkv", "payload")
	stamp := time.Date(2026, 8, 13, 9, 4, 5, 0, time.UTC)

	dest, err := moveToTrash(root, src, stamp)
	if err != nil {
		t.Fatalf("moveToTrash returned error: %v", err)
	}
	if _, err := os.Lstat(src); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the original file is still at %s after the trash move (err=%v)", src, err)
	}
	body, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("reading the trashed file at %s: %v", dest, err)
	}
	if string(body) != "payload" {
		t.Errorf("trashed file contents = %q, want %q: a trash move must move, never re-create", body, "payload")
	}
	want := filepath.Join(root, trashDirName, "2026-08-13T09:04:05Z", "Show", "Season 01", "dupe.mkv")
	if dest != want {
		t.Errorf("moveToTrash landed at %q, want %q", dest, want)
	}
}

// TestMoveToTrash_TwoTrashesOfTheSamePathInTheSameSecondKeepBothFiles is the
// end-to-end statement of "never overwrite in trash": the stamp directory is
// second-resolution, so this is the collision that actually happens, and the
// first file must survive it.
func TestMoveToTrash_TwoTrashesOfTheSamePathInTheSameSecondKeepBothFiles(t *testing.T) {
	root := t.TempDir()
	stamp := time.Date(2026, 8, 13, 9, 4, 5, 0, time.UTC)

	src := writeActionFile(t, root, "Show/dupe.mkv", "first")
	firstDest, err := moveToTrash(root, src, stamp)
	if err != nil {
		t.Fatalf("first moveToTrash: %v", err)
	}
	src = writeActionFile(t, root, "Show/dupe.mkv", "second")
	secondDest, err := moveToTrash(root, src, stamp)
	if err != nil {
		t.Fatalf("second moveToTrash: %v", err)
	}
	if firstDest == secondDest {
		t.Fatalf("both trash moves landed on %q; the first file was overwritten", firstDest)
	}
	first, err := os.ReadFile(firstDest)
	if err != nil || string(first) != "first" {
		t.Errorf("the FIRST trashed file must survive the second trash of the same path: contents=%q err=%v", first, err)
	}
	second, err := os.ReadFile(secondDest)
	if err != nil || string(second) != "second" {
		t.Errorf("second trashed file contents=%q err=%v", second, err)
	}
}
