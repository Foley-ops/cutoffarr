package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// --- case-twin merge (rule 5) -----------------------------------------------
//
// A NOTE ON WHY THESE FIXTURES ARE NOT SPELLED AS CASE TWINS. This project's
// development host has a case-INSENSITIVE filesystem (the same reason
// fileReportDirLister exists as an injectable seam in filereport.go: "case-twin
// names a real case-insensitive dev filesystem cannot hold at once"), so
// "Show" and "SHOW" are literally the same directory here and a fixture
// spelled that way silently tests nothing.
//
// That is not a compromise, because mergeCaseTwinDir's own contract is not
// about case at all: it moves the contents of one named directory into
// another named directory, never overwriting. WHICH two directories those are
// — and that they are a case twin with exactly one tracked side — is decided
// upstream, by twinMergeEligibility and by the executor's live re-derivation
// of the finding, both of which are tested separately and neither of which
// touches a filesystem. So the mechanics are exercised with two distinct
// names here, and the genuinely case-twinned spellings get their own
// end-to-end test below, which runs only where the filesystem can hold them.

// caseSensitiveFS reports whether dir's filesystem distinguishes "a" from "A".
func caseSensitiveFS(t *testing.T, dir string) bool {
	t.Helper()
	lower := filepath.Join(dir, "casecheck")
	if err := os.Mkdir(lower, 0o755); err != nil {
		t.Fatalf("probing filesystem case sensitivity: %v", err)
	}
	_, err := os.Lstat(filepath.Join(dir, "CASECHECK"))
	return err != nil
}

// TestMergeCaseTwinDir_MovesTheUntrackedSpellingsContentsIntoTheTrackedOne is
// rule 5's happy path. The *arr tracks exactly one spelling; everything that
// landed under the other one has to end up where the *arr is already looking,
// and the emptied wrong-cased directory has to stop existing as a directory
// entry — by being trashed, never deleted.
func TestMergeCaseTwinDir_MovesTheUntrackedSpellingsContentsIntoTheTrackedOne(t *testing.T) {
	root := t.TempDir()
	writeActionFile(t, root, "untracked-spelling/Season 01/a.mkv", "A")
	writeActionFile(t, root, "tracked-spelling/Season 01/b.mkv", "B")
	tracked := filepath.Join(root, "tracked-spelling")
	untracked := filepath.Join(root, "untracked-spelling")
	stamp := time.Date(2026, 8, 13, 9, 4, 5, 0, time.UTC)

	res, err := mergeCaseTwinDir(root, untracked, tracked, stamp)
	if err != nil {
		t.Fatalf("mergeCaseTwinDir returned error: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(tracked, "Season 01", "a.mkv"))
	if err != nil || string(body) != "A" {
		t.Errorf("a.mkv must land under the TRACKED spelling: contents=%q err=%v", body, err)
	}
	if body, err := os.ReadFile(filepath.Join(tracked, "Season 01", "b.mkv")); err != nil || string(body) != "B" {
		t.Errorf("the tracked spelling's own file must be left alone: contents=%q err=%v", body, err)
	}
	if _, err := os.Lstat(untracked); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the emptied untracked directory is still at %s (err=%v); it must be trash-moved", untracked, err)
	}
	if res.trashedDir == "" {
		t.Error("the result must name where the emptied untracked directory was trashed to")
	} else if _, err := os.Lstat(res.trashedDir); err != nil {
		t.Errorf("the emptied untracked directory is not in the trash at %s: %v", res.trashedDir, err)
	}
	if len(res.moved) != 1 || !strings.HasSuffix(res.moved[0], filepath.Join("Season 01", "a.mkv")) {
		t.Errorf("moved = %v, want exactly the one file that changed spelling", res.moved)
	}
	if len(res.collided) != 0 {
		t.Errorf("collided = %v, want none in the clean case", res.collided)
	}
}

// TestMergeCaseTwinDir_ADestinationCollisionTrashesTheSourceAndItemizesIt is
// rule 5's "never overwriting — a destination collision moves the source file
// to trash instead, itemized in the response". The two spellings holding a
// same-named file is the NORMAL shape of this bug (a re-download that landed
// under the wrong case), so this branch is the common one, not the corner.
func TestMergeCaseTwinDir_ADestinationCollisionTrashesTheSourceAndItemizesIt(t *testing.T) {
	root := t.TempDir()
	writeActionFile(t, root, "tracked-spelling/Season 01/ep.mkv", "the tracked copy")
	writeActionFile(t, root, "untracked-spelling/Season 01/ep.mkv", "the untracked copy")
	tracked := filepath.Join(root, "tracked-spelling")
	untracked := filepath.Join(root, "untracked-spelling")
	stamp := time.Date(2026, 8, 13, 9, 4, 5, 0, time.UTC)

	res, err := mergeCaseTwinDir(root, untracked, tracked, stamp)
	if err != nil {
		t.Fatalf("mergeCaseTwinDir returned error: %v", err)
	}

	kept, err := os.ReadFile(filepath.Join(tracked, "Season 01", "ep.mkv"))
	if err != nil || string(kept) != "the tracked copy" {
		t.Fatalf("the TRACKED spelling's file must never be overwritten: contents=%q err=%v", kept, err)
	}
	if len(res.collided) != 1 {
		t.Fatalf("collided = %v, want exactly one itemized collision", res.collided)
	}
	item := res.collided[0]
	if !strings.Contains(item, "ep.mkv") || !strings.Contains(item, trashDirName) {
		t.Errorf("the itemized collision must name the file and where its source copy went; got %q", item)
	}
	if len(res.moved) != 0 {
		t.Errorf("moved = %v, want none: the only file was a collision", res.moved)
	}
	// The trashed source copy must still be readable: "moved to trash instead"
	// is only true if the bytes actually survived somewhere.
	trashed := filepath.Join(root, trashDirName, "2026-08-13T09:04:05Z", "untracked-spelling", "Season 01", "ep.mkv")
	if body, err := os.ReadFile(trashed); err != nil || string(body) != "the untracked copy" {
		t.Errorf("the untracked copy must survive in the trash at %s: contents=%q err=%v", trashed, body, err)
	}
}

// TestMergeCaseTwinDir_GenuineCaseTwinSpellingsEndToEnd is the same merge with
// the fixtures spelled the way the real bug is spelled. It runs only where the
// filesystem can hold both spellings at once (Linux, which is what the
// container and the CI runner are); on a case-insensitive dev host the two
// names ARE one directory and there is nothing to merge, so it skips loudly
// rather than passing vacuously.
func TestMergeCaseTwinDir_GenuineCaseTwinSpellingsEndToEnd(t *testing.T) {
	root := t.TempDir()
	if !caseSensitiveFS(t, root) {
		t.Skip("this filesystem is case-insensitive, so the two twin spellings cannot both exist; the merge mechanics are covered by the distinct-name tests above")
	}
	writeActionFile(t, root, "Squid Game The Challenge/Season 01/a.mkv", "A")
	writeActionFile(t, root, "squid game the challenge/Season 01/b.mkv", "B")
	tracked := filepath.Join(root, "squid game the challenge")
	untracked := filepath.Join(root, "Squid Game The Challenge")

	res, err := mergeCaseTwinDir(root, untracked, tracked, time.Date(2026, 8, 13, 9, 4, 5, 0, time.UTC))
	if err != nil {
		t.Fatalf("mergeCaseTwinDir returned error: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(tracked, "Season 01", "a.mkv")); err != nil || string(body) != "A" {
		t.Errorf("a.mkv must land under the tracked spelling: contents=%q err=%v", body, err)
	}
	if _, err := os.Lstat(untracked); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the untracked spelling still exists at %s (err=%v)", untracked, err)
	}
	if len(res.moved) != 1 {
		t.Errorf("moved = %v, want exactly one file", res.moved)
	}
}

// TestMergeCaseTwinDir_RefusesWhenTheTrackedSpellingIsNotADirectory is the
// shape guard from the executor's side. mergeCaseTwinDir is only ever reached
// for a dir/dir twin with exactly one tracked side (the eligibility rules live
// in twinMergeEligibility), but the function that actually moves files must
// not depend on its caller having got that right.
func TestMergeCaseTwinDir_RefusesWhenTheTrackedSpellingIsNotADirectory(t *testing.T) {
	root := t.TempDir()
	writeActionFile(t, root, "a-directory/x.mkv", "x")
	writeActionFile(t, root, "a-plain-file", "not a directory")
	if _, err := mergeCaseTwinDir(root, filepath.Join(root, "a-directory"), filepath.Join(root, "a-plain-file"), time.Now()); err == nil {
		t.Fatal("mergeCaseTwinDir merged into a plain file; it must refuse")
	}
}

// TestMergeCaseTwinDir_APartialMergeReportsEverythingItAlreadyMoved is the one
// failure in this phase that leaves a library HALF-MOVED, and it had no test at
// any level. mergeCaseTwinDir deliberately does not roll back (see its own
// comment): what it does instead is return the result ALONGSIDE the error, and
// everything the operator is told — the "(N file(s) had already been moved and
// were left where they now are)" sentence, the 502, the ERROR line's moved=/
// collided= attrs — is built from that result. If this returned a bare error, or
// an empty result, the operator would be left with a half-moved library and a
// message that named none of it.
//
// The walk fails on the SECOND season because its destination parent is a plain
// file, which is a state no *arr creates but any hand-edit can: the point is a
// mid-walk failure after a completed move, not which syscall reports it.
func TestMergeCaseTwinDir_APartialMergeReportsEverythingItAlreadyMoved(t *testing.T) {
	root := t.TempDir()
	writeActionFile(t, root, "untracked-spelling/Season 01/a.mkv", "A")
	writeActionFile(t, root, "untracked-spelling/Season 02/b.mkv", "B")
	writeActionFile(t, root, "tracked-spelling/Season 01/keep.mkv", "keep")
	// The obstruction: Season 02 exists under the tracked spelling as a FILE.
	writeActionFile(t, root, "tracked-spelling/Season 02", "not a directory")
	tracked := filepath.Join(root, "tracked-spelling")
	untracked := filepath.Join(root, "untracked-spelling")

	res, err := mergeCaseTwinDir(root, untracked, tracked, time.Date(2026, 8, 13, 9, 4, 5, 0, time.UTC))
	if err == nil {
		t.Fatal("mergeCaseTwinDir reported success; the second season could not be merged")
	}
	if len(res.moved) != 1 || !strings.HasSuffix(res.moved[0], filepath.Join("Season 01", "a.mkv")) {
		t.Fatalf("moved = %v, want exactly the one file that DID move: a failure that reports nothing moved is how a half-moved library goes unnoticed", res.moved)
	}
	// "Left where they now are" has to be literally true — the caller's message
	// says so and nothing rolls it back.
	if body, err := os.ReadFile(filepath.Join(tracked, "Season 01", "a.mkv")); err != nil || string(body) != "A" {
		t.Errorf("the file that moved must still be under the tracked spelling: contents=%q err=%v", body, err)
	}
	if body, err := os.ReadFile(filepath.Join(untracked, "Season 02", "b.mkv")); err != nil || string(body) != "B" {
		t.Errorf("the file the walk failed on must be untouched: contents=%q err=%v", body, err)
	}
	if res.trashedDir != "" {
		t.Errorf("trashedDir = %q, want empty: a directory that still holds files is never trashed", res.trashedDir)
	}
	if _, err := os.Lstat(untracked); err != nil {
		t.Errorf("the untracked spelling must still be where it was (err=%v)", err)
	}
}

// TestDirIsEmptyOfFiles_TellsAnEmptyTreeFromOneThatStillHoldsSomething is the
// post-condition mergeCaseTwinDir checks before trashing the emptied source
// directory. It is a defensive re-check of a state the walk immediately above
// it just established, and it exists because the alternative — assuming — is
// how a "merge" quietly becomes "move a directory full of files somebody is
// still using into the trash".
//
// Empty SUBDIRECTORIES are tolerated deliberately: the merge moves files, not
// directory entries, so a source tree that held only files leaves its own
// skeleton of empty folders behind, and that skeleton carries nothing. Any
// non-directory entry at all — a regular file, a symlink, a device node —
// makes it non-empty.
func TestDirIsEmptyOfFiles_TellsAnEmptyTreeFromOneThatStillHoldsSomething(t *testing.T) {
	root := t.TempDir()

	skeleton := filepath.Join(root, "skeleton", "Season 01", "deeper")
	if err := os.MkdirAll(skeleton, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	empty, err := dirIsEmptyOfFiles(filepath.Join(root, "skeleton"))
	if err != nil {
		t.Fatalf("dirIsEmptyOfFiles returned error: %v", err)
	}
	if !empty {
		t.Error("a tree of empty subdirectories carries nothing and must read as empty")
	}

	writeActionFile(t, root, "holds-something/Season 01/left-behind.mkv", "x")
	empty, err = dirIsEmptyOfFiles(filepath.Join(root, "holds-something"))
	if err != nil {
		t.Fatalf("dirIsEmptyOfFiles returned error: %v", err)
	}
	if empty {
		t.Error("a tree still holding a regular file must never read as empty: trashing it would move a file nobody asked to move")
	}

	// A symlink is not a directory and not a regular file; it must still count.
	linkDir := filepath.Join(root, "holds-a-link")
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "holds-something"), filepath.Join(linkDir, "link")); err != nil {
		t.Skipf("this platform cannot create symlinks: %v", err)
	}
	empty, err = dirIsEmptyOfFiles(linkDir)
	if err != nil {
		t.Fatalf("dirIsEmptyOfFiles returned error: %v", err)
	}
	if empty {
		t.Error("a tree holding a symlink must not read as empty")
	}
}

// --- twin eligibility (rule 5's "button not offered" half) ------------------

// TestTwinMergeEligibility_TheShapesThatGetAButtonAndTheShapesThatDoNot is
// rule 5's second half: "Mixed/ambiguous shapes (no tracked side, both
// tracked) -> button not offered (report-only row, reason shown)". The reason
// is returned rather than the row being silently button-less, because a human
// looking at a finding with no button needs to know whether cutoffarr judged
// it unsafe or simply forgot.
func TestTwinMergeEligibility_TheShapesThatGetAButtonAndTheShapesThatDoNot(t *testing.T) {
	names := func(specs ...caseCollisionNameRecord) []caseCollisionNameRecord { return specs }
	tests := []struct {
		name       string
		entryType  string
		names      []caseCollisionNameRecord
		wantOK     bool
		wantReason string
	}{
		{
			name:      "one tracked, one untracked, both directories",
			entryType: fileReportEntryTypeDir,
			names:     names(caseCollisionNameRecord{Name: "Show", Tracked: false}, caseCollisionNameRecord{Name: "show", Tracked: true}),
			wantOK:    true,
		},
		{
			name:       "no tracked side",
			entryType:  fileReportEntryTypeDir,
			names:      names(caseCollisionNameRecord{Name: "Show"}, caseCollisionNameRecord{Name: "show"}),
			wantOK:     false,
			wantReason: "neither spelling",
		},
		{
			name:       "both tracked",
			entryType:  fileReportEntryTypeDir,
			names:      names(caseCollisionNameRecord{Name: "Show", Tracked: true}, caseCollisionNameRecord{Name: "show", Tracked: true}),
			wantOK:     false,
			wantReason: "both spellings",
		},
		{
			name:       "mixed entry type (a folder beside a file)",
			entryType:  fileReportEntryTypeMixed,
			names:      names(caseCollisionNameRecord{Name: "Show", Tracked: true}, caseCollisionNameRecord{Name: "show"}),
			wantOK:     false,
			wantReason: "a folder and a file",
		},
		{
			name:       "file twins have no contents to merge",
			entryType:  fileReportEntryTypeFile,
			names:      names(caseCollisionNameRecord{Name: "a.mkv", Tracked: true}, caseCollisionNameRecord{Name: "A.mkv"}),
			wantOK:     false,
			wantReason: "files, not folders",
		},
		{
			name:       "three spellings",
			entryType:  fileReportEntryTypeDir,
			names:      names(caseCollisionNameRecord{Name: "Show", Tracked: true}, caseCollisionNameRecord{Name: "show"}, caseCollisionNameRecord{Name: "SHOW"}),
			wantOK:     false,
			wantReason: "more than two",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			el := twinMergeEligibility(fileReportFindingRecord{Kind: fileKindCaseCollision, EntryType: tc.entryType, Names: tc.names})
			if el.ok != tc.wantOK {
				t.Fatalf("eligible = %v, want %v (reason %q)", el.ok, tc.wantOK, el.reason)
			}
			if tc.wantOK {
				if el.tracked != "show" && el.tracked != "a.mkv" {
					t.Errorf("tracked = %q, want the tracked spelling", el.tracked)
				}
				if el.untracked != "Show" {
					t.Errorf("untracked = %q, want the untracked spelling", el.untracked)
				}
				return
			}
			if !strings.Contains(el.reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to contain %q so the row can say WHY there is no button", el.reason, tc.wantReason)
			}
		})
	}
}

// --- the executors, the switches, and the endpoint --------------------------

// arrFake is one httptest *arr covering every endpoint the action executors
// can reach: the tag list (exclusion-tag resolution), the library, Sonarr's
// per-series episode files, the quality profiles, the wanted/cutoff pages in
// both directions, and — for the re-monitor path — the per-movie fetch and the
// one PUT this project is ever allowed to make.
//
// It records every write it received so a test can assert the strongest thing
// there is to assert about a rehearsal or a refusal: that NOTHING was written.
type arrFake struct {
	srv    *httptest.Server
	mu     sync.Mutex
	writes []string
	// writeBodies is writes' parallel slice, recorded only for the two Sonarr
	// season-write endpoints. The path alone cannot say WHICH SEASON was
	// written — both halves of a season write address the series — so a test
	// that a click on one season touched only that season has to read the body.
	writeBodies []string
	requests    int
	movies      string // JSON array for GET /api/v3/movie
	series      string
	// wanted maps the monitored filter value ("", "false") to a wanted/cutoff
	// page body.
	wanted       map[string]string
	episodeFiles map[string]string
	movieByID    map[string]string
	movieFiles   map[string]string

	// The Sonarr write path's own endpoints, keyed by series id as strings:
	// the per-series episode list the season write re-reads, and the
	// GET/PUT /api/v3/series/{id} pair. They exist so a Sonarr re-monitor can
	// be driven all the way through the decision engine and its gated write —
	// see sonarrRemonitorFixture, and the round-3 review finding that no test
	// reached that engine at all.
	episodes   map[string]string
	seriesByID map[string]string
	// tags is GET /api/v3/tag's body — "[]" by default (the exclusion tag is
	// simply not defined in this instance), and settable to something
	// undecodable so a test can reach the §2.6 warn-and-skip return the
	// engines take when the exclusion tag cannot be resolved at all.
	tags string
}

func newArrFake(t *testing.T) *arrFake {
	t.Helper()
	f := &arrFake{
		movies: "[]", series: "[]", tags: "[]",
		wanted:       map[string]string{},
		episodeFiles: map[string]string{},
		movieByID:    map[string]string{},
		movieFiles:   map[string]string{},
		episodes:     map[string]string{},
		seriesByID:   map[string]string{},
	}
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
	// Every request, of any method, so a test can assert that a refusal
	// happened WITHOUT the instance being contacted at all.
	countAll := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			f.requests++
			f.mu.Unlock()
			next(w, r)
		}
	}
	mux.HandleFunc("/api/v3/system/status", countAll(func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"version":"5.0.0.0"}`)
	}))
	mux.HandleFunc("/api/v3/qualityprofile", countAll(func(w http.ResponseWriter, r *http.Request) {
		write(w, `[{"id":1,"name":"HD","cutoff":2,"items":[{"quality":{"id":1,"name":"720p"},"allowed":true},{"quality":{"id":2,"name":"1080p"},"allowed":true}],"upgradeAllowed":true,"cutoffFormatScore":0}]`)
	}))
	mux.HandleFunc("/api/v3/tag", countAll(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		body := f.tags
		f.mu.Unlock()
		write(w, body)
	}))
	mux.HandleFunc("/api/v3/movie", countAll(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		body := f.movies
		f.mu.Unlock()
		write(w, body)
	}))
	mux.HandleFunc("/api/v3/movie/", countAll(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/v3/movie/")
		if r.Method != http.MethodGet {
			f.mu.Lock()
			f.writes = append(f.writes, r.Method+" "+r.URL.Path)
			body := f.movieByID[id]
			f.mu.Unlock()
			write(w, strings.Replace(body, `"monitored":false`, `"monitored":true`, 1))
			return
		}
		f.mu.Lock()
		body, ok := f.movieByID[id]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		write(w, body)
	}))
	mux.HandleFunc("/api/v3/moviefile", countAll(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		body, ok := f.movieFiles[r.URL.Query().Get("movieId")]
		f.mu.Unlock()
		if !ok {
			body = `[]`
		}
		write(w, body)
	}))
	mux.HandleFunc("/api/v3/wanted/cutoff", countAll(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		body, ok := f.wanted[r.URL.Query().Get("monitored")]
		f.mu.Unlock()
		if !ok {
			body = `{"page":1,"pageSize":1000,"totalRecords":0,"records":[]}`
		}
		write(w, body)
	}))
	mux.HandleFunc("/api/v3/series", countAll(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		body := f.series
		f.mu.Unlock()
		write(w, body)
	}))
	mux.HandleFunc("/api/v3/episodefile", countAll(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		body, ok := f.episodeFiles[r.URL.Query().Get("seriesId")]
		f.mu.Unlock()
		if !ok {
			body = `[]`
		}
		write(w, body)
	}))
	mux.HandleFunc("/api/v3/episode", countAll(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		body, ok := f.episodes[r.URL.Query().Get("seriesId")]
		f.mu.Unlock()
		if !ok {
			body = `[]`
		}
		write(w, body)
	}))
	// The season write's first half. Sonarr answers with the updated episode
	// resources, and the writer verifies that echo, so a fake that answered
	// anything else would exercise a failure path instead of the happy one.
	mux.HandleFunc("/api/v3/episode/monitor", countAll(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			EpisodeIDs []int `json:"episodeIds"`
			Monitored  bool  `json:"monitored"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.writes = append(f.writes, r.Method+" "+r.URL.Path)
		f.writeBodies = append(f.writeBodies, string(body))
		f.mu.Unlock()
		var elems []string
		for _, id := range req.EpisodeIDs {
			elems = append(elems, fmt.Sprintf(`{"id":%d,"monitored":%t}`, id, req.Monitored))
		}
		write(w, "["+strings.Join(elems, ",")+"]")
	}))
	// The season write's second half, and the pre-write fetch that precedes it.
	mux.HandleFunc("/api/v3/series/", countAll(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/v3/series/")
		if r.Method != http.MethodGet {
			// Sonarr echoes the object it was sent; the writer verifies that
			// echo names this series, this season and the value it asked for.
			echo, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.writes = append(f.writes, r.Method+" "+r.URL.Path)
			f.writeBodies = append(f.writeBodies, string(echo))
			f.mu.Unlock()
			write(w, string(echo))
			return
		}
		f.mu.Lock()
		body, ok := f.seriesByID[id]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		write(w, body)
	}))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			f.mu.Lock()
			f.writes = append(f.writes, r.Method+" "+r.URL.Path)
			f.mu.Unlock()
		}
		w.WriteHeader(http.StatusNotFound)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *arrFake) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

func (f *arrFake) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

// radarrFileFixture builds a media root on disk plus the matching Radarr
// library JSON: one tracked movie file and one duplicate beside it.
func radarrFileFixture(t *testing.T, fake *arrFake) (root string, dupPath string, inst Instance) {
	t.Helper()
	root = t.TempDir()
	writeActionFile(t, root, "Some Movie (2019)/Some Movie (2019).mkv", "tracked")
	dupPath = writeActionFile(t, root, "Some Movie (2019)/ETRG.Sample.mkv", "0123456789")
	fake.movies = `[{"id":7,"title":"Some Movie","monitored":true,"hasFile":true,"qualityProfileId":1,"tags":[],
	  "path":"/movies/Some Movie (2019)",
	  "movieFile":{"id":70,"path":"/movies/Some Movie (2019)/Some Movie (2019).mkv","quality":{"quality":{"id":2,"name":"1080p"}},"qualityCutoffNotMet":false}}]`
	inst = Instance{Name: "radarr-main", Type: "radarr", URL: fake.srv.URL, APIKey: "k",
		MediaRootMap: map[string]string{"/movies": root}}
	return root, dupPath, inst
}

// newActionFixture wires a runner and an endpoint over one instance, with the
// stats store already holding the finding a button would have been rendered
// from — which is what makes the request in these tests a realistic one.
func newActionFixture(t *testing.T, cfg Config, inst Instance) (*actionRunner, *httptest.Server, *statsStore, *strings.Builder) {
	t.Helper()
	logger, buf := newActionTestLogger()
	store := newStatsStore(cfg.DryRun)
	cfg.Instances = []Instance{inst}
	runner := newActionRunner(cfg, logger, store)
	ts := httptest.NewServer(newWebUIHandler(&webUIServer{stats: store, scan: newScanCoordinator(), logger: logger, actions: runner}))
	t.Cleanup(ts.Close)
	return runner, ts, store, buf
}

// seedFileFinding puts one duplicate/orphan finding into the store, exactly as
// a completed sweep would have.
func seedFileFinding(store *statsStore, inst Instance, rec fileReportFindingRecord) {
	store.recordInstance(cycleKindSweep, time.Now(), inst.Name, inst.Type, cycleInstanceStats{
		decisionsRan:  true,
		fileReportRan: true,
		fileReport: fileReportSnapshot{
			Status: "ran", Duplicates: 1, Findings: []fileReportFindingRecord{rec},
		},
	})
}

func postAction(t *testing.T, ts *httptest.Server, body string) (int, actionResponse) {
	t.Helper()
	resp, err := http.Post(ts.URL+"/api/action", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/action: %v", err)
	}
	defer resp.Body.Close()
	var out actionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding the action response: %v", err)
	}
	return resp.StatusCode, out
}

// --- the switch matrix ------------------------------------------------------

// TestAction_GUIActionsFalse_Is403AndNamesTheSwitchWithoutTouchingAnything is
// rule 7's default state, and the one every deployment starts in. The refusal
// must NAME the switch: "the page never lies about which switch is missing",
// and an operator who has set dry_run: false and still sees nothing happen
// needs to be told it is the other flag.
func TestAction_GUIActionsFalse_Is403AndNamesTheSwitchWithoutTouchingAnything(t *testing.T) {
	fake := newArrFake(t)
	root, dupPath, inst := radarrFileFixture(t, fake)
	cfg := Config{DryRun: false, GUIActions: false, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, buf := newActionFixture(t, cfg, inst)
	seedFileFinding(store, inst, fileReportFindingRecord{Kind: fileKindDuplicate, Path: dupPath, Display: "d", Size: 10})

	status, out := postAction(t, ts, `{"kind":"trash","confirm":true,"instance":"radarr-main","path":`+jsonString(dupPath)+`,"finding":"duplicate","size":10}`)
	assertActionAudited(t, buf, "trash", actionOutcomeDisabled, dupPath)
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want 403", status)
	}
	if out.Outcome != actionOutcomeDisabled {
		t.Errorf("outcome = %q, want %q", out.Outcome, actionOutcomeDisabled)
	}
	if !strings.Contains(out.Reason, "gui_actions") {
		t.Errorf("the refusal must name the switch that is off; got %q", out.Reason)
	}
	if _, err := os.Lstat(dupPath); err != nil {
		t.Errorf("the file must not have been touched: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, trashDirName)); err == nil {
		t.Error("a disabled action must not even create the trash directory")
	}
}

// TestAction_DryRun_RehearsesTheTrashWithFullReVerificationAndMovesNothing is
// rule 7's rehearsal semantics. The distinction that matters: a rehearsal is
// not "the switch is off", it is the action having done ALL of its
// verification and stopped at the last possible moment, so the response can
// say what WOULD happen — including the destination path.
func TestAction_DryRun_RehearsesTheTrashWithFullReVerificationAndMovesNothing(t *testing.T) {
	fake := newArrFake(t)
	_, dupPath, inst := radarrFileFixture(t, fake)
	cfg := Config{DryRun: true, GUIActions: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, buf := newActionFixture(t, cfg, inst)
	seedFileFinding(store, inst, fileReportFindingRecord{Kind: fileKindDuplicate, Path: dupPath, Display: "d", Size: 10})

	status, out := postAction(t, ts, `{"kind":"trash","confirm":true,"instance":"radarr-main","path":`+jsonString(dupPath)+`,"finding":"duplicate","size":10}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %+v", status, out)
	}
	if out.Outcome != actionOutcomeRehearsed {
		t.Fatalf("outcome = %q, want %q (reason %q)", out.Outcome, actionOutcomeRehearsed, out.Reason)
	}
	if out.Trash == "" || !strings.Contains(out.Trash, trashDirName) {
		t.Errorf("a rehearsal must say where the file WOULD go; trash=%q", out.Trash)
	}
	if _, err := os.Lstat(dupPath); err != nil {
		t.Errorf("a rehearsal must move nothing: %v", err)
	}
	if !strings.Contains(buf.String(), "source=gui") || !strings.Contains(buf.String(), "outcome=rehearsed") {
		t.Errorf("a rehearsal must still produce the loud audit line; log was:\n%s", buf.String())
	}
	if fake.writeCount() != 0 {
		t.Errorf("a file action must never write to the *arr; writes=%v", fake.writes)
	}
}

// TestAction_Trash_PerformsTheMoveLogsItAndRecordsItInLastActions is the whole
// point of the phase, end to end.
func TestAction_Trash_PerformsTheMoveLogsItAndRecordsItInLastActions(t *testing.T) {
	fake := newArrFake(t)
	root, dupPath, inst := radarrFileFixture(t, fake)
	cfg := Config{DryRun: false, GUIActions: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, buf := newActionFixture(t, cfg, inst)
	seedFileFinding(store, inst, fileReportFindingRecord{Kind: fileKindDuplicate, Path: dupPath, Display: "Movies/Some Movie (2019)/ETRG.Sample.mkv", Size: 10})

	status, out := postAction(t, ts, `{"kind":"trash","confirm":true,"instance":"radarr-main","path":`+jsonString(dupPath)+`,"finding":"duplicate","size":10}`)
	if status != http.StatusOK || out.Outcome != actionOutcomePerformed {
		t.Fatalf("status=%d outcome=%q reason=%q, want 200/performed", status, out.Outcome, out.Reason)
	}
	if _, err := os.Lstat(dupPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the file is still at its original path (err=%v)", err)
	}
	body, err := os.ReadFile(out.Trash)
	if err != nil || string(body) != "0123456789" {
		t.Errorf("the file must be readable in the trash at %s: contents=%q err=%v", out.Trash, body, err)
	}
	if !strings.HasPrefix(out.Trash, filepath.Join(root, trashDirName)) {
		t.Errorf("trash path %q is not under the root's own trash directory", out.Trash)
	}
	// The operation the response echoes must be the operation the button
	// stated, so the page can prove they match.
	if !strings.Contains(out.Operation, "Move to trash") || !strings.Contains(out.Operation, "ETRG.Sample.mkv") {
		t.Errorf("operation = %q, want it to state the exact operation and name the file", out.Operation)
	}
	log := buf.String()
	for _, want := range []string{"msg=action", "source=gui", "kind=trash", "outcome=performed", dupPath} {
		if !strings.Contains(log, want) {
			t.Errorf("audit line is missing %q; log was:\n%s", want, log)
		}
	}
	snap := store.snapshot()
	if len(snap.Instances) != 1 || len(snap.Instances[0].LastActions) == 0 {
		t.Fatalf("the action must appear in lastActions; got %+v", snap.Instances)
	}
	if got := snap.Instances[0].LastActions[0].Action; got != ActionTrash {
		t.Errorf("lastActions[0].Action = %q, want %q", got, ActionTrash)
	}
}

// TestAction_EveryActionAppearsInLastActionsCarryingItsOutcome is brief item 9
// read literally, which is the controller's ruling on the round-3 escalation:
// "every action (rehearsed, performed, refused) logs msg=action ... AND appears
// in lastActions".
//
// The reason the literal reading is the right one is the operator's question.
// lastActions is the one list a human reads to answer "what did I do through
// this dashboard"; if it holds only what landed, then every rehearsal and every
// refusal exists solely in stdout, and the audit trail for actions is split
// across two places with the GUI-visible half silently incomplete. What makes
// that safe is that each record now CARRIES its outcome, so a listed rehearsal
// can never be mistaken for a write: the list says what happened, in the same
// vocabulary the response and the log line use.
//
// Every outcome is asserted in both places at once — the log line and the
// record — because the whole point is that they cannot diverge.
func TestAction_EveryActionAppearsInLastActionsCarryingItsOutcome(t *testing.T) {
	// A rehearsal: every check runs, nothing is written, and the record says so.
	t.Run("a rehearsed action is audited and listed as rehearsed", func(t *testing.T) {
		fake := newArrFake(t)
		_, dupPath, inst := radarrFileFixture(t, fake)
		cfg := Config{DryRun: true, GUIActions: true, ExclusionTag: "cutoffarr-exclude"}
		_, ts, store, buf := newActionFixture(t, cfg, inst)
		seedFileFinding(store, inst, fileReportFindingRecord{Kind: fileKindDuplicate, Path: dupPath, Display: "Movies/x/d.mkv", Size: 10})

		status, out := postAction(t, ts, `{"kind":"trash","confirm":true,"instance":"radarr-main","path":`+jsonString(dupPath)+`,"finding":"duplicate","size":10}`)
		if status != http.StatusOK || out.Outcome != actionOutcomeRehearsed {
			t.Fatalf("status=%d outcome=%q reason=%q, want 200/rehearsed", status, out.Outcome, out.Reason)
		}
		assertActionAudited(t, buf, "trash", actionOutcomeRehearsed, dupPath)
		rec := assertOneListedAction(t, store, ActionTrash, actionOutcomeRehearsed)
		if rec.Title != "Movies/x/d.mkv" {
			t.Errorf("lastActions[0].Title = %q, want the finding's own display path", rec.Title)
		}
		// The file is still where it was: a listed rehearsal must never be a
		// performed one.
		if body, err := os.ReadFile(dupPath); err != nil || string(body) != "0123456789" {
			t.Errorf("the file must be untouched by a rehearsal: contents=%q err=%v", body, err)
		}
	})

	// A refusal: the finding is stale, so nothing happened — and the list says
	// that too, rather than being silent about the click.
	t.Run("a refused action is audited and listed as refused", func(t *testing.T) {
		fake := newArrFake(t)
		_, dupPath, inst := radarrFileFixture(t, fake)
		cfg := Config{DryRun: false, GUIActions: true, ExclusionTag: "cutoffarr-exclude"}
		_, ts, store, buf := newActionFixture(t, cfg, inst)
		seedFileFinding(store, inst, fileReportFindingRecord{Kind: fileKindDuplicate, Path: dupPath, Display: "d", Size: 10})
		// The size the button promised no longer matches the file on disk.
		if err := os.WriteFile(dupPath, []byte("a much larger sample file than before"), 0o644); err != nil {
			t.Fatalf("replacing the file: %v", err)
		}

		status, out := postAction(t, ts, `{"kind":"trash","confirm":true,"instance":"radarr-main","path":`+jsonString(dupPath)+`,"finding":"duplicate","size":10}`)
		if status != http.StatusConflict || out.Outcome != actionOutcomeRefused {
			t.Fatalf("status=%d outcome=%q, want 409/refused", status, out.Outcome)
		}
		assertActionAudited(t, buf, "trash", actionOutcomeRefused, dupPath)
		rec := assertOneListedAction(t, store, ActionTrash, actionOutcomeRefused)
		if !strings.Contains(rec.Reason, "not the file you confirmed") {
			t.Errorf("lastActions[0].Reason = %q, want the same sentence the operator was answered with", rec.Reason)
		}
	})

	// A disabled switch is an answer too, and it is the one an operator is most
	// likely to be looking for an explanation of.
	t.Run("a disabled action is audited and listed as disabled", func(t *testing.T) {
		fake := newArrFake(t)
		_, dupPath, inst := radarrFileFixture(t, fake)
		cfg := Config{DryRun: false, GUIActions: false, ExclusionTag: "cutoffarr-exclude"}
		_, ts, store, buf := newActionFixture(t, cfg, inst)
		seedFileFinding(store, inst, fileReportFindingRecord{Kind: fileKindDuplicate, Path: dupPath, Display: "d", Size: 10})

		status, out := postAction(t, ts, `{"kind":"trash","confirm":true,"instance":"radarr-main","path":`+jsonString(dupPath)+`,"finding":"duplicate","size":10}`)
		if status != http.StatusForbidden || out.Outcome != actionOutcomeDisabled {
			t.Fatalf("status=%d outcome=%q, want 403/disabled", status, out.Outcome)
		}
		assertActionAudited(t, buf, "trash", actionOutcomeDisabled, dupPath)
		assertOneListedAction(t, store, ActionTrash, actionOutcomeDisabled)
	})

	// The control the two non-performed cases need: an implementation that
	// recorded everything with a constant outcome would pass them and fail here.
	t.Run("a performed action is listed as performed", func(t *testing.T) {
		fake := newArrFake(t)
		_, dupPath, inst := radarrFileFixture(t, fake)
		cfg := Config{DryRun: false, GUIActions: true, ExclusionTag: "cutoffarr-exclude"}
		_, ts, store, _ := newActionFixture(t, cfg, inst)
		seedFileFinding(store, inst, fileReportFindingRecord{Kind: fileKindDuplicate, Path: dupPath, Display: "d", Size: 10})

		if status, out := postAction(t, ts, `{"kind":"trash","confirm":true,"instance":"radarr-main","path":`+jsonString(dupPath)+`,"finding":"duplicate","size":10}`); status != http.StatusOK || out.Outcome != actionOutcomePerformed {
			t.Fatalf("status=%d outcome=%q reason=%q, want 200/performed", status, out.Outcome, out.Reason)
		}
		assertOneListedAction(t, store, ActionTrash, actionOutcomePerformed)
	})
}

// TestAction_ARequestThatNamesNothingRealIsNeverListed is the other half of the
// literal reading, and the reason it is safe to record refusals at all: an
// action is a thing a human did to a finding on a configured instance. A request
// naming an instance this daemon has never heard of, or a kind it does not
// implement, is not an action — recording it would let anything that can reach
// the port invent instances in the operator's own stats table, and would put a
// value outside the documented four-token vocabulary into `action`.
//
// The log still carries both, which is where an unrecognized request belongs.
func TestAction_ARequestThatNamesNothingRealIsNeverListed(t *testing.T) {
	fake := newArrFake(t)
	_, dupPath, inst := radarrFileFixture(t, fake)
	cfg := Config{DryRun: false, GUIActions: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, buf := newActionFixture(t, cfg, inst)
	seedFileFinding(store, inst, fileReportFindingRecord{Kind: fileKindDuplicate, Path: dupPath, Display: "d", Size: 10})

	for _, body := range []string{
		`{"kind":"trash","confirm":true,"instance":"a-radarr-nobody-configured","path":` + jsonString(dupPath) + `,"finding":"duplicate","size":10}`,
		`{"kind":"delete-everything","confirm":true,"instance":"radarr-main","path":` + jsonString(dupPath) + `,"finding":"duplicate","size":10}`,
	} {
		if status, out := postAction(t, ts, body); status != http.StatusBadRequest {
			t.Fatalf("status=%d outcome=%q, want 400 for %s", status, out.Outcome, body)
		}
	}
	snap := store.snapshot()
	if len(snap.Instances) != 1 {
		t.Fatalf("instances = %+v, want only the one configured instance: no request may invent one", snap.Instances)
	}
	if acts := snap.Instances[0].LastActions; len(acts) != 0 {
		t.Errorf("lastActions = %+v, want empty: neither request named an action on a finding", acts)
	}
	if !strings.Contains(buf.String(), "outcome=refused") {
		t.Errorf("both requests must still be audited; log:\n%s", buf.String())
	}
}

// assertOneListedAction asserts that the store holds exactly one GUI action for
// its one instance, with the kind and outcome given, and returns it.
func assertOneListedAction(t *testing.T, store *statsStore, action, outcome string) actionRecord {
	t.Helper()
	snap := store.snapshot()
	if len(snap.Instances) != 1 {
		t.Fatalf("instances = %+v, want exactly one", snap.Instances)
	}
	acts := snap.Instances[0].LastActions
	if len(acts) != 1 {
		t.Fatalf("lastActions = %+v, want exactly one record: every action, whatever its outcome, is one line in the list an operator reads to answer \"what did I do here\"", acts)
	}
	if acts[0].Action != action || acts[0].Outcome != outcome {
		t.Fatalf("lastActions[0] = %+v, want action %q outcome %q", acts[0], action, outcome)
	}
	if acts[0].Time.IsZero() {
		t.Errorf("lastActions[0].Time is zero; every record is stamped by the store")
	}
	return acts[0]
}

// --- staleness refusals -----------------------------------------------------

// TestAction_Trash_RefusesAPathNoSweepEverReportedAsAFinding is the guard that
// bounds this endpoint's power to exactly what a completed sweep classified as
// clutter. Without it, "path" is an arbitrary filesystem argument handed to a
// rename by anything that can reach the LAN port.
func TestAction_Trash_RefusesAPathNoSweepEverReportedAsAFinding(t *testing.T) {
	fake := newArrFake(t)
	root, dupPath, inst := radarrFileFixture(t, fake)
	cfg := Config{DryRun: false, GUIActions: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, _ := newActionFixture(t, cfg, inst)
	seedFileFinding(store, inst, fileReportFindingRecord{Kind: fileKindDuplicate, Path: dupPath, Display: "d", Size: 10})

	tracked := filepath.Join(root, "Some Movie (2019)", "Some Movie (2019).mkv")
	status, out := postAction(t, ts, `{"kind":"trash","confirm":true,"instance":"radarr-main","path":`+jsonString(tracked)+`,"finding":"duplicate","size":7}`)
	if status != http.StatusConflict || out.Outcome != actionOutcomeRefused {
		t.Fatalf("status=%d outcome=%q, want 409/refused for a path that is not a reported finding", status, out.Outcome)
	}
	if _, err := os.Lstat(tracked); err != nil {
		t.Errorf("the tracked file must be untouched: %v", err)
	}
}

// TestAction_Trash_RefusesWhenTheFileIsAlreadyGone is rule 3's plainest case: a
// human clicking a button on a dashboard that has been open since before they
// cleaned up by hand.
func TestAction_Trash_RefusesWhenTheFileIsAlreadyGone(t *testing.T) {
	fake := newArrFake(t)
	_, dupPath, inst := radarrFileFixture(t, fake)
	cfg := Config{DryRun: false, GUIActions: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, _ := newActionFixture(t, cfg, inst)
	seedFileFinding(store, inst, fileReportFindingRecord{Kind: fileKindDuplicate, Path: dupPath, Display: "d", Size: 10})
	if err := os.Rename(dupPath, dupPath+".moved-by-a-human"); err != nil {
		t.Fatalf("simulating a hand cleanup: %v", err)
	}

	status, out := postAction(t, ts, `{"kind":"trash","confirm":true,"instance":"radarr-main","path":`+jsonString(dupPath)+`,"finding":"duplicate","size":10}`)
	if status != http.StatusConflict || out.Outcome != actionOutcomeRefused {
		t.Fatalf("status=%d outcome=%q reason=%q, want 409/refused", status, out.Outcome, out.Reason)
	}
}

// TestAction_Trash_RefusesWhenTheFileIsNoLongerTheSizeTheButtonPromised is why
// fileReportFinding.size exists. A file REPLACED at the same path passes every
// existence check there is; the size is the cheapest fact that catches it.
func TestAction_Trash_RefusesWhenTheFileIsNoLongerTheSizeTheButtonPromised(t *testing.T) {
	fake := newArrFake(t)
	_, dupPath, inst := radarrFileFixture(t, fake)
	cfg := Config{DryRun: false, GUIActions: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, _ := newActionFixture(t, cfg, inst)
	seedFileFinding(store, inst, fileReportFindingRecord{Kind: fileKindDuplicate, Path: dupPath, Display: "d", Size: 10})

	// The button was rendered against a 10-byte file; the request says so.
	// What is on disk now is a different, larger file at the same path.
	if err := os.WriteFile(dupPath, []byte("a much larger replacement file"), 0o644); err != nil {
		t.Fatalf("replacing the fixture: %v", err)
	}
	status, out := postAction(t, ts, `{"kind":"trash","confirm":true,"instance":"radarr-main","path":`+jsonString(dupPath)+`,"finding":"duplicate","size":10}`)
	if status != http.StatusConflict || out.Outcome != actionOutcomeRefused {
		t.Fatalf("status=%d outcome=%q reason=%q, want 409/refused", status, out.Outcome, out.Reason)
	}
	if !strings.Contains(out.Reason, "size") {
		t.Errorf("the refusal must say the size no longer matches; got %q", out.Reason)
	}
	if _, err := os.Lstat(dupPath); err != nil {
		t.Errorf("the replacement file must be untouched: %v", err)
	}
}

// TestAction_Trash_ReadOnlyRootRefusesNamingTheMountAndTheFix is rule 8 through
// the endpoint: the DEFAULT deployment's answer, because the compose example
// mounts media :ro on purpose.
func TestAction_Trash_ReadOnlyRootRefusesNamingTheMountAndTheFix(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a 0555 directory is still writable")
	}
	fake := newArrFake(t)
	root, dupPath, inst := radarrFileFixture(t, fake)
	cfg := Config{DryRun: false, GUIActions: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, buf := newActionFixture(t, cfg, inst)
	seedFileFinding(store, inst, fileReportFindingRecord{Kind: fileKindDuplicate, Path: dupPath, Display: "d", Size: 10})

	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	status, out := postAction(t, ts, `{"kind":"trash","confirm":true,"instance":"radarr-main","path":`+jsonString(dupPath)+`,"finding":"duplicate","size":10}`)
	if status != http.StatusConflict || out.Outcome != actionOutcomeRefused {
		t.Fatalf("status=%d outcome=%q, want 409/refused on a read-only root", status, out.Outcome)
	}
	if !strings.Contains(out.Reason, root) || !strings.Contains(out.Reason, "read-only") {
		t.Errorf("the refusal must name the mount and the cause; got %q", out.Reason)
	}
	// Rule 9 names refusals explicitly, and on the shipped :ro deployment a
	// refusal is what EVERY file button produces — so this line is the only
	// record that a human tried to act at all.
	assertActionAudited(t, buf, "trash", actionOutcomeRefused, dupPath)
}

// assertActionAudited pins rule 9's line for the outcomes nothing else records:
// a refusal and a disablement produce no write, no stats entry and no other log
// line, so if this line is missing the attempt left no trace anywhere. Deleting
// the a.audit call inside refuse() used to keep the whole suite green.
func assertActionAudited(t *testing.T, buf *strings.Builder, kind, outcome, path string) {
	t.Helper()
	log := buf.String()
	for _, want := range []string{"msg=action", "source=gui", "kind=" + kind, "outcome=" + outcome, path} {
		if !strings.Contains(log, want) {
			t.Errorf("the audit line for this %s action is missing %q; log:\n%s", outcome, want, log)
		}
	}
}

// --- endpoint validation ----------------------------------------------------

func TestActionEndpoint_ValidationAndMethodConventions(t *testing.T) {
	fake := newArrFake(t)
	_, dupPath, inst := radarrFileFixture(t, fake)
	cfg := Config{DryRun: false, GUIActions: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, _ := newActionFixture(t, cfg, inst)
	seedFileFinding(store, inst, fileReportFindingRecord{Kind: fileKindDuplicate, Path: dupPath, Display: "d", Size: 10})

	t.Run("GET is 405", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/api/action")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", resp.StatusCode)
		}
	})

	for _, tc := range []struct{ name, body, wantIn string }{
		{"malformed JSON", `{not json`, "could not be read"},
		{"unknown kind", `{"kind":"delete-everything","confirm":true,"instance":"radarr-main"}`, "delete-everything"},
		{"missing confirm", `{"kind":"trash","instance":"radarr-main","path":"/x","finding":"duplicate"}`, "confirm"},
		{"unknown instance", `{"kind":"trash","confirm":true,"instance":"nope","path":"/x","finding":"duplicate"}`, "nope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, out := postAction(t, ts, tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (outcome %q reason %q)", status, out.Outcome, out.Reason)
			}
			if !strings.Contains(out.Reason, tc.wantIn) {
				t.Errorf("reason = %q, want it to contain %q", out.Reason, tc.wantIn)
			}
		})
	}
}

// postActionWith is postAction with control over the two headers the
// cross-site guard reads. An empty contentType sends none at all.
func postActionWith(t *testing.T, ts *httptest.Server, body, contentType, secFetchSite string) (int, actionResponse) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/action", strings.NewReader(body))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if secFetchSite != "" {
		req.Header.Set("Sec-Fetch-Site", secFetchSite)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/action: %v", err)
	}
	defer resp.Body.Close()
	var out actionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding the action response: %v", err)
	}
	return resp.StatusCode, out
}

// TestActionEndpoint_RefusesACrossSiteDriveByPost is the review finding this
// endpoint had no answer to: the page has no authentication (LAN trust model,
// README), and that model covers LAN PEERS — it does not cover a random web
// page the operator opens in a browser that is already on that LAN. Such a page
// can POST here with no CORS involvement at all, because a form post is a
// "simple request": no preflight, and the response body it cannot read is not
// the point when the request itself moved a file.
//
// `confirm: true` does not stop it (a form field can say confirm), and neither
// does DisallowUnknownFields: a form whose INPUT NAME is
//
//	{"kind":"trash","confirm":true,...,"path":"
//
// and whose value is `"}` posts the body `{"kind":"trash",...,"path":"="}` under
// enctype=text/plain — every field known, valid JSON, no header the attacking
// page had to set.
//
// The two things such a request cannot do are the guard: it cannot set
// Content-Type to application/json (the three enctypes a form can send are the
// only ones available without a preflight), and it cannot forge Sec-Fetch-Site,
// which the browser itself stamps. The page's own fetch already sends the
// former, so the guard costs the real client nothing — the third case here is
// what proves that.
func TestActionEndpoint_RefusesACrossSiteDriveByPost(t *testing.T) {
	fake := newArrFake(t)
	_, dupPath, inst := radarrFileFixture(t, fake)
	cfg := Config{DryRun: false, GUIActions: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, buf := newActionFixture(t, cfg, inst)
	seedFileFinding(store, inst, fileReportFindingRecord{Kind: fileKindDuplicate, Path: dupPath, Display: "d", Size: 10})

	body := `{"kind":"trash","confirm":true,"instance":"radarr-main","path":` + jsonString(dupPath) + `,"finding":"duplicate","size":10}`

	// The drive-by form post: the enctype a cross-site form can actually send,
	// carrying a body that is valid JSON with every field known.
	t.Run("a form-post content type is refused", func(t *testing.T) {
		status, out := postActionWith(t, ts, body, "text/plain;charset=UTF-8", "")
		if status != http.StatusBadRequest || out.Outcome != actionOutcomeRefused {
			t.Fatalf("status=%d outcome=%q, want 400/refused: a text/plain POST is a cross-site form post, not this page's fetch", status, out.Outcome)
		}
		if !strings.Contains(out.Reason, "application/json") {
			t.Errorf("reason = %q, want it to name the content type an action must arrive as", out.Reason)
		}
		if _, err := os.Stat(dupPath); err != nil {
			t.Fatalf("the file was moved by a refused cross-site request: %v", err)
		}
	})

	// A page that fetches with the right content type still cannot forge the
	// header the browser stamps for it.
	t.Run("a cross-site fetch is refused", func(t *testing.T) {
		status, out := postActionWith(t, ts, body, "application/json", "cross-site")
		if status != http.StatusForbidden || out.Outcome != actionOutcomeRefused {
			t.Fatalf("status=%d outcome=%q, want 403/refused", status, out.Outcome)
		}
		if !strings.Contains(out.Reason, "another site") {
			t.Errorf("reason = %q, want it to say plainly where the request came from", out.Reason)
		}
		if _, err := os.Stat(dupPath); err != nil {
			t.Fatalf("the file was moved by a refused cross-site request: %v", err)
		}
	})

	// Both refusals must leave the trace rule 9 requires; neither reaches the
	// runner, so this handler owes the line itself.
	assertActionAudited(t, buf, "trash", actionOutcomeRefused, dupPath)

	// The control, and the whole reason the guard is these two headers and not
	// something stricter: the page's own request is unaffected.
	t.Run("the page's own fetch still works", func(t *testing.T) {
		status, out := postActionWith(t, ts, body, "application/json", "same-origin")
		if status != http.StatusOK || out.Outcome != actionOutcomePerformed {
			t.Fatalf("status=%d outcome=%q reason=%q, want 200/performed — the guard must not break the page it protects\nlog:\n%s", status, out.Outcome, out.Reason, buf.String())
		}
	})
}

// --- single-flight ----------------------------------------------------------

// TestActionRunner_SerializesActions is the "never concurrent with each other"
// half of the endpoint contract. Two file moves interleaving is how a merge
// and a trash of the same tree produce a state neither one described.
func TestActionRunner_SerializesActions(t *testing.T) {
	fake := newArrFake(t)
	_, dupPath, inst := radarrFileFixture(t, fake)
	cfg := Config{DryRun: true, GUIActions: true, ExclusionTag: "cutoffarr-exclude"}
	runner, _, store, _ := newActionFixture(t, cfg, inst)
	seedFileFinding(store, inst, fileReportFindingRecord{Kind: fileKindDuplicate, Path: dupPath, Display: "d", Size: 10})

	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	runner.observe = func() {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		time.Sleep(2 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
	}

	req := actionRequest{Kind: ActionTrash, Confirm: true, Instance: inst.Name, Path: dupPath, Finding: fileKindDuplicate, Size: 10}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runner.run(context.Background(), req)
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if maxInFlight != 1 {
		t.Errorf("maxInFlight = %d, want 1: actions must be serialized through a single-flight executor", maxInFlight)
	}
}

// --- the structural pin the whole ruling rests on ---------------------------

// actionsFileEntryPointsAllowedOutside is the entire published surface of
// actions.go: the two names another file may legitimately mention, and the file
// each one is allowed in.
//
//   - webui.go holds the ONE handler. It may hand a request to the runner
//     (run) and it may ask the runner to vet the request's headers before doing
//     so (crossSiteRefusal). Both are gates; neither is an executor.
//   - daemon.go may CONSTRUCT the runner, because wiring is not acting.
//
// Anything else actions.go declares — every executor, every re-derivation,
// every operation-text helper — is unreachable from the rest of the program by
// construction, which is what the ruling rests on.
var actionsFileEntryPointsAllowedOutside = map[string]map[string]bool{
	"webui.go":  {"run": true, "crossSiteRefusal": true},
	"daemon.go": {"newActionRunner": true},
}

// TestTree_ExecutorsAreReachableOnlyFromTheActionEndpoint is the pin the whole
// v2.2 ruling rests on: the ruling permits a HUMAN to act, and it is only
// honest if no autonomous code path can reach an executor at all.
//
// It DERIVES the banned set rather than listing it, and that is the round-3
// review fix. The previous version named five symbols, which pinned today's
// names instead of the property: a new entry point added to actions.go — say a
// startTrashJanitor() that called moveToTrash — and invoked from daemon.go's
// sweep loop passed this audit (daemon.go named none of the five strings) AND
// passed TestActionsFile_EveryMutationIsATrashMoveOrAMergeMove (the mutations
// still sat inside the approved moveToTrash), so the owner's ruling that no
// autonomous path can ever touch a file would have become false with every
// audit green.
//
// So: parse actions.go, collect EVERY top-level func and method it declares,
// and fail if any other non-test file references one of them, save the two
// published entry points above.
//
// It is an identifier check rather than a substring scan (go/ast, the same
// machinery TestActionsFile_EveryMutationIsATrashMoveOrAMergeMove uses), and it
// tells the two kinds of reference apart, which is what keeps it from crying
// wolf on names this package legitimately reuses:
//
//   - a PLAIN FUNCTION of actions.go can only be reached by naming it bare, so
//     only bare identifiers are checked. main.go's own top-level `run` is
//     therefore not a hit against actionRunner.run, which is a method.
//   - a METHOD can only be reached through a selector, so only `x.name` is
//     checked — EXCEPT where THE FILE BEING SCANNED declares a field or method
//     of that name itself, which makes the selector genuinely ambiguous to an
//     audit working without type information (reverseOptions.remonitor, read as
//     opts.remonitor all over reverse.go, is the live example). That exemption
//     is derived from the tree, not hand-listed, so it cannot quietly grow to
//     cover a real caller: a file would have to declare a field named `run` of
//     its own before it could call the runner's.
//
// THE EXEMPTION IS PER-FILE, and that is the round-4 review fix. It used to be
// one global set built from every other non-test file at once, which meant
// reverse.go's `remonitor` field exempted the name `remonitor` EVERYWHERE: the
// re-monitor executor — the one that composes an *arr write — could be called
// as `r.remonitor(ctx, inst, req)` from daemon.go's sweep loop (which is already
// allowed to name newActionRunner) with this audit staying green. Exactly the
// hole the round-3 rewrite was written to close, left open for one name. Scoped
// to the declaring file, reverse.go keeps its exemption and every other file
// inherits nothing.
func TestTree_ExecutorsAreReachableOnlyFromTheActionEndpoint(t *testing.T) {
	const source = "actions.go"
	for _, v := range actionEntryPointViolations(t, source, nonTestGoFilesExcept(t, source)) {
		t.Error(v)
	}
}

// TestTree_ExecutorReachabilityAuditCatchesASweepThatCallsTheRemonitorExecutor
// is the mutation check for the audit above, run against SYNTHETIC files rather
// than by editing daemon.go, so the property is proved on every run instead of
// once by hand in a review round.
//
// The first case is the hole the round-4 review found, and it is scanned
// ALONGSIDE THE REAL TREE, because the tree is where the ambiguity comes from: a
// synthetic daemon.go on its own declares no `remonitor` and would be caught by
// either version of this audit. It is the real reverse.go, in the same scan,
// that used to exempt the name everywhere.
//
// The second case is that exemption, which must keep working: reverse.go's own
// reverseOptions.remonitor is read as opts.remonitor in four places and is not a
// call to anything in actions.go.
func TestTree_ExecutorReachabilityAuditCatchesASweepThatCallsTheRemonitorExecutor(t *testing.T) {
	const source = "actions.go"
	dir := t.TempDir()

	// A daemon.go that wires the runner (allowed) and then drives the re-monitor
	// executor from its sweep loop (never allowed).
	sweeping := writeActionFile(t, dir, "daemon.go", `package main

import "context"

func (d *daemon) sweepInstance(ctx context.Context, inst Instance, req actionRequest) {
	a := newActionRunner(d.cfg, d.logger, d.stats)
	a.remonitor(ctx, inst, req)
}
`)
	got := actionEntryPointViolations(t, source, append(nonTestGoFilesExcept(t, source), sweeping))
	if len(got) != 1 || !strings.Contains(got[0], "remonitor") {
		t.Errorf("violations = %v, want exactly one naming remonitor: a sweep that reaches the re-monitor executor — the executor that composes an *arr write — is precisely what this audit exists to catch", got)
	}

	// The legitimate ambiguity, in the file that owns it.
	declaring := writeActionFile(t, dir, "reverse.go", `package main

type reverseOptions struct {
	enabled   bool
	remonitor bool
}

func wouldWrite(opts reverseOptions) bool {
	return opts.enabled && opts.remonitor
}
`)
	if got := actionEntryPointViolations(t, source, []string{declaring}); len(got) != 0 {
		t.Errorf("violations = %v, want none: a file reading a field IT declares is not calling anything in %s", got, source)
	}
}

// actionEntryPointViolations is the audit body, returning one message per
// violation instead of failing, so the same code can be run against the real
// tree and against a synthetic file whose violation is the point.
//
// The allowlist is consulted by BASE NAME so a synthetic "daemon.go" carries
// exactly the permissions the real one has — every .go file in this package
// lives in the package directory, so base names are already unique across the
// scanned set.
func actionEntryPointViolations(t *testing.T, source string, others []string) []string {
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
	// Vacuity guard, the twin of the mutation audit's own: a rename, a moved
	// file or a parse that silently produced nothing would leave this test green
	// while checking an empty set.
	for _, must := range []string{"moveToTrash", "mergeCaseTwinDir", "probeRootWritable", "newActionRunner"} {
		if !bannedFuncs[must] {
			t.Fatalf("%s no longer declares func %s, so this audit is not looking at the action system at all", source, must)
		}
	}
	for _, must := range []string{"run", "trash", "mergeTwin", "remonitor"} {
		if !bannedMethods[must] {
			t.Fatalf("%s no longer declares a %s method, so this audit is not looking at the action system at all", source, must)
		}
	}

	var violations []string
	for _, path := range others {
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			t.Fatalf("parsing %s: %v", path, parseErr)
		}
		// Per-file: only the names THIS file declares can make a selector in it
		// ambiguous. See the test's own comment for why this is not global.
		ambiguous := selectorNamesDeclaredIn(t, fset, []string{path})
		allowed := actionsFileEntryPointsAllowedOutside[filepath.Base(path)]
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
			violations = append(violations, fmt.Sprintf("%s:%d references %s, which %s declares: the action system must stay reachable ONLY from the action endpoint handler in webui.go, and a sweep, webhook, reconciliation or startup path that can reach any part of it would make the owner's human-acts ruling false",
				path, fset.Position(id.Pos()).Line, id.Name, source))
			return true
		})
	}
	return violations
}

// nonTestGoFilesExcept lists this package's non-test .go files, minus the one
// named, in walk order.
func nonTestGoFilesExcept(t *testing.T, except string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != "." && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		name := filepath.ToSlash(p)
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == except {
			return nil
		}
		out = append(out, name)
		return nil
	})
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}
	if len(out) < minScannedNonTestGoFiles {
		t.Fatalf("found only %d non-test .go files, want at least %d: this audit is vacuous unless it is actually reading the tree", len(out), minScannedNonTestGoFiles)
	}
	return out
}

// selectorNamesDeclaredIn collects every name these files declare that can be
// read through a selector — struct/interface field and method names, plus the
// methods they define. Those are the names on which `x.name` cannot be
// attributed to actions.go without type information. See the test's own comment
// for why the exemption is derived instead of listed.
func selectorNamesDeclaredIn(t *testing.T, fset *token.FileSet, paths []string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, p := range paths {
		file, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", p, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl:
				if node.Recv != nil {
					out[node.Name.Name] = true
				}
			case *ast.StructType:
				for _, f := range node.Fields.List {
					for _, name := range f.Names {
						out[name.Name] = true
					}
				}
			case *ast.InterfaceType:
				for _, f := range node.Methods.List {
					for _, name := range f.Names {
						out[name.Name] = true
					}
				}
			}
			return true
		})
	}
	return out
}

// identRoles splits one file's identifiers into the two roles this audit cares
// about: the ones being read through a selector (`x.name`), and the ones that
// are DECLARING a name rather than referencing one. Everything else is a bare
// reference.
func identRoles(file *ast.File) (selectors, declarations map[token.Pos]bool) {
	selectors, declarations = map[token.Pos]bool{}, map[token.Pos]bool{}
	mark := func(m map[token.Pos]bool, ids ...*ast.Ident) {
		for _, id := range ids {
			if id != nil {
				m[id.Pos()] = true
			}
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.SelectorExpr:
			mark(selectors, node.Sel)
		case *ast.FuncDecl:
			mark(declarations, node.Name)
		case *ast.TypeSpec:
			mark(declarations, node.Name)
		case *ast.ValueSpec:
			mark(declarations, node.Names...)
		case *ast.Field:
			mark(declarations, node.Names...)
		case *ast.ImportSpec:
			mark(declarations, node.Name)
		case *ast.LabeledStmt:
			mark(declarations, node.Label)
		case *ast.AssignStmt:
			if node.Tok != token.DEFINE {
				return true
			}
			for _, lhs := range node.Lhs {
				if id, ok := lhs.(*ast.Ident); ok {
					mark(declarations, id)
				}
			}
		}
		return true
	})
	return selectors, declarations
}

func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// --- the re-monitor action, end to end --------------------------------------

// remonitorFixture builds a Radarr library with exactly the two movies this
// action needs to be exercised honestly:
//
//   - movie 1: monitored, has a file, cutoff MET. It is the cross-check's
//     sample — an item whose wanted-set membership can actually be verified —
//     and without at least one of those, reverseWriteGateBlockReason blocks
//     every reverse write by design ("the cross-check sampled nothing this
//     cycle"). A fixture without it would test the gate, not the action.
//   - movie 152: UNMONITORED, has a file, still below cutoff. That is the
//     reverse-scan finding a human clicks the button on.
func remonitorFixture(t *testing.T, fake *arrFake) Instance {
	t.Helper()
	fake.movies = `[
	  {"id":1,"title":"Already Fine","monitored":true,"hasFile":true,"qualityProfileId":1,"tags":[],
	   "movieFile":{"id":10,"path":"/movies/Already Fine/f.mkv","quality":{"quality":{"id":2,"name":"1080p"}},"qualityCutoffNotMet":false}},
	  {"id":152,"title":"Fever Pitch","monitored":false,"hasFile":true,"qualityProfileId":1,"tags":[],
	   "movieFile":{"id":20,"path":"/movies/Fever Pitch/f.mkv","quality":{"quality":{"id":1,"name":"720p"}},"qualityCutoffNotMet":true}}
	]`
	fake.wanted[""] = `{"page":1,"pageSize":1000,"totalRecords":0,"records":[]}`
	fake.wanted["false"] = `{"page":1,"pageSize":1000,"totalRecords":1,"records":[{"id":152}]}`
	fake.movieByID["152"] = `{"id":152,"title":"Fever Pitch","monitored":false,"hasFile":true,"qualityProfileId":1,"tags":[],
	   "movieFile":{"id":20,"path":"/movies/Fever Pitch/f.mkv","quality":{"quality":{"id":1,"name":"720p"}},"qualityCutoffNotMet":true}}`
	fake.movieFiles["152"] = `[{"id":20,"movieId":152,"customFormatScore":0}]`
	return Instance{Name: "radarr-main", Type: "radarr", URL: fake.srv.URL, APIKey: "k"}
}

// seedReverseFinding puts the reverse-scan finding into the store, as a
// completed sweep would, so the operation sentence is built from what
// cutoffarr reported rather than from what the request claimed.
func seedReverseFinding(store *statsStore, inst Instance, f reverseFinding) {
	store.recordInstance(cycleKindSweep, time.Now(), inst.Name, inst.Type, cycleInstanceStats{
		decisionsRan: true, reverseRan: true, reverseFindings: []reverseFinding{f},
	})
}

// TestAction_Remonitor_RemonitorFlagOff_Is403AndNamesThatSecondSwitch is rule
// 6: gui_actions alone is not enough for a re-monitor, and the refusal must
// name the OTHER flag or an operator who has already turned gui_actions on is
// left staring at a button that does nothing.
func TestAction_Remonitor_RemonitorFlagOff_Is403AndNamesThatSecondSwitch(t *testing.T) {
	fake := newArrFake(t)
	inst := remonitorFixture(t, fake)
	cfg := Config{DryRun: false, GUIActions: true, ReverseScanRemonitor: false, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, _ := newActionFixture(t, cfg, inst)
	seedReverseFinding(store, inst, reverseFinding{ID: 152, Title: "Fever Pitch", Reason: ReasonQualityCutoffNotMet})

	status, out := postAction(t, ts, `{"kind":"remonitor","confirm":true,"instance":"radarr-main","id":152}`)
	if status != http.StatusForbidden || out.Outcome != actionOutcomeDisabled {
		t.Fatalf("status=%d outcome=%q, want 403/disabled", status, out.Outcome)
	}
	if !strings.Contains(out.Reason, "reverse_scan_remonitor") {
		t.Errorf("the refusal must name the second switch; got %q", out.Reason)
	}
	if fake.writeCount() != 0 {
		t.Errorf("a disabled re-monitor must send no write; writes=%v", fake.writes)
	}
}

// TestAction_Remonitor_DryRun_RehearsesAgainstFreshDataAndWritesNothing is the
// rehearsal semantics on the *arr side: every fetch and every re-verification
// happens, and the write is withheld at §2.1's gate immediately before the PUT.
func TestAction_Remonitor_DryRun_RehearsesAgainstFreshDataAndWritesNothing(t *testing.T) {
	fake := newArrFake(t)
	inst := remonitorFixture(t, fake)
	cfg := Config{DryRun: true, GUIActions: true, ReverseScanRemonitor: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, buf := newActionFixture(t, cfg, inst)
	seedReverseFinding(store, inst, reverseFinding{ID: 152, Title: "Fever Pitch", Reason: ReasonQualityCutoffNotMet})

	status, out := postAction(t, ts, `{"kind":"remonitor","confirm":true,"instance":"radarr-main","id":152}`)
	if status != http.StatusOK || out.Outcome != actionOutcomeRehearsed {
		t.Fatalf("status=%d outcome=%q reason=%q, want 200/rehearsed", status, out.Outcome, out.Reason)
	}
	if !strings.Contains(out.Operation, "Fever Pitch") || !strings.Contains(out.Operation, "152") {
		t.Errorf("operation = %q, want it to state the item and its id", out.Operation)
	}
	if fake.writeCount() != 0 {
		t.Errorf("a rehearsal must send no write; writes=%v", fake.writes)
	}
	if !strings.Contains(buf.String(), "outcome=rehearsed") {
		t.Errorf("the rehearsal must be audited; log:\n%s", buf.String())
	}
}

// TestAction_Remonitor_PerformsExactlyOneWriteForExactlyTheItemClicked is rule
// 6's whole promise: the existing gated reverse path, scoped to one item. One
// PUT, for movie 152 and nothing else — the other movie in the library is
// monitored and correct, and must not be touched in either direction.
func TestAction_Remonitor_PerformsExactlyOneWriteForExactlyTheItemClicked(t *testing.T) {
	fake := newArrFake(t)
	inst := remonitorFixture(t, fake)
	cfg := Config{DryRun: false, GUIActions: true, ReverseScanRemonitor: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, buf := newActionFixture(t, cfg, inst)
	seedReverseFinding(store, inst, reverseFinding{ID: 152, Title: "Fever Pitch", Reason: ReasonQualityCutoffNotMet})

	status, out := postAction(t, ts, `{"kind":"remonitor","confirm":true,"instance":"radarr-main","id":152}`)
	if status != http.StatusOK || out.Outcome != actionOutcomePerformed {
		t.Fatalf("status=%d outcome=%q reason=%q, want 200/performed\nlog:\n%s", status, out.Outcome, out.Reason, buf.String())
	}
	fake.mu.Lock()
	writes := append([]string(nil), fake.writes...)
	fake.mu.Unlock()
	if len(writes) != 1 {
		t.Fatalf("writes = %v, want exactly one", writes)
	}
	if !strings.HasSuffix(writes[0], "/api/v3/movie/152") {
		t.Errorf("the one write was %q, want it to name movie 152 and nothing else", writes[0])
	}
	log := buf.String()
	for _, want := range []string{"msg=action", "source=gui", "kind=remonitor", "outcome=performed"} {
		if !strings.Contains(log, want) {
			t.Errorf("audit line is missing %q; log:\n%s", want, log)
		}
	}
	snap := store.snapshot()
	if len(snap.Instances[0].LastActions) == 0 || snap.Instances[0].LastActions[0].Action != ActionRemonitor {
		t.Errorf("the re-monitor must appear in lastActions; got %+v", snap.Instances[0].LastActions)
	}
}

// TestAction_Remonitor_RefusesAnItemThatIsAlreadyMonitored is rule 3 on the
// *arr side — the exact staleness the brief names ("movie already monitored").
func TestAction_Remonitor_RefusesAnItemThatIsAlreadyMonitored(t *testing.T) {
	fake := newArrFake(t)
	inst := remonitorFixture(t, fake)
	// Somebody re-monitored it in Radarr between the sweep and the click.
	fake.movies = strings.Replace(fake.movies, `"id":152,"title":"Fever Pitch","monitored":false`, `"id":152,"title":"Fever Pitch","monitored":true`, 1)
	fake.wanted["false"] = `{"page":1,"pageSize":1000,"totalRecords":0,"records":[]}`

	cfg := Config{DryRun: false, GUIActions: true, ReverseScanRemonitor: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, _ := newActionFixture(t, cfg, inst)
	seedReverseFinding(store, inst, reverseFinding{ID: 152, Title: "Fever Pitch", Reason: ReasonQualityCutoffNotMet})

	status, out := postAction(t, ts, `{"kind":"remonitor","confirm":true,"instance":"radarr-main","id":152}`)
	if status != http.StatusConflict || out.Outcome != actionOutcomeRefused {
		t.Fatalf("status=%d outcome=%q reason=%q, want 409/refused", status, out.Outcome, out.Reason)
	}
	if fake.writeCount() != 0 {
		t.Errorf("a refused re-monitor must send no write; writes=%v", fake.writes)
	}
}

// TestAction_Remonitor_SeasonShapeMustMatchTheInstanceType keeps a Radarr
// request carrying a season (or a Sonarr one carrying none) from reaching a
// write path built for the other shape.
func TestAction_Remonitor_SeasonShapeMustMatchTheInstanceType(t *testing.T) {
	fake := newArrFake(t)
	inst := remonitorFixture(t, fake)
	cfg := Config{DryRun: false, GUIActions: true, ReverseScanRemonitor: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, _, _ := newActionFixture(t, cfg, inst)

	status, out := postAction(t, ts, `{"kind":"remonitor","confirm":true,"instance":"radarr-main","id":152,"season":3}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (outcome %q reason %q)", status, out.Outcome, out.Reason)
	}
	if fake.writeCount() != 0 {
		t.Errorf("writes = %v, want none", fake.writes)
	}
}

// --- the merge action, end to end -------------------------------------------

// TestAction_MergeCaseTwin_EndToEndOnACaseSensitiveFilesystem is the merge
// through the endpoint, with the twin spelled the way the real bug is spelled.
// It runs only where the filesystem can hold both spellings (see the note at
// the top of the merge section); the merge mechanics themselves, and every
// eligibility and refusal rule, are covered on every platform above.
func TestAction_MergeCaseTwin_EndToEndOnACaseSensitiveFilesystem(t *testing.T) {
	root := t.TempDir()
	if !caseSensitiveFS(t, root) {
		t.Skip("this filesystem is case-insensitive, so a real case-twin cannot exist to be merged")
	}
	fake := newArrFake(t)
	writeActionFile(t, root, "show/Season 01/tracked.mkv", "tracked")
	writeActionFile(t, root, "SHOW/Season 01/stray.mkv", "stray")
	fake.series = `[{"id":3,"title":"Show","monitored":true,"tags":[],"path":"/tv/show","seasons":[]}]`
	fake.episodeFiles["3"] = `[{"id":30,"seriesId":3,"seasonNumber":1,"path":"/tv/show/Season 01/tracked.mkv"}]`
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "k",
		MediaRootMap: map[string]string{"/tv": root}}

	cfg := Config{DryRun: false, GUIActions: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, buf := newActionFixture(t, cfg, inst)
	store.recordInstance(cycleKindSweep, time.Now(), inst.Name, inst.Type, cycleInstanceStats{
		decisionsRan: true, fileReportRan: true,
		fileReport: fileReportSnapshot{Status: "ran", CaseCollisions: 1, Findings: []fileReportFindingRecord{{
			Kind: fileKindCaseCollision, Path: root, Display: filepath.Base(root),
			EntryType: fileReportEntryTypeDir,
			Names: []caseCollisionNameRecord{
				{Name: "SHOW", Tracked: false},
				{Name: "show", Tracked: true},
			},
		}}},
	})

	status, out := postAction(t, ts,
		`{"kind":"merge-case-twin","confirm":true,"instance":"sonarr-main","path":`+jsonString(root)+`,"tracked":"show","untracked":"SHOW"}`)
	if status != http.StatusOK || out.Outcome != actionOutcomePerformed {
		t.Fatalf("status=%d outcome=%q reason=%q, want 200/performed\nlog:\n%s", status, out.Outcome, out.Reason, buf.String())
	}
	if body, err := os.ReadFile(filepath.Join(root, "show", "Season 01", "stray.mkv")); err != nil || string(body) != "stray" {
		t.Errorf("the stray file must land under the tracked spelling: contents=%q err=%v", body, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "SHOW")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the untracked spelling must be trash-moved (err=%v)", err)
	}
	if !strings.Contains(out.Operation, "SHOW") || !strings.Contains(out.Operation, "show") {
		t.Errorf("operation = %q, want it to name both spellings", out.Operation)
	}
}

// TestAction_MergeCaseTwin_APartialFailureIsA502LoggedAtErrorWithItsCounts is
// the endpoint half of the half-moved-library case, and the loudest answer this
// phase can give. It runs where a real case twin can exist, for the same reason
// the test above does; the merge mechanics of a partial failure are covered on
// every platform by TestMergeCaseTwinDir_APartialMergeReportsEverythingItAlreadyMoved.
//
// Three files, three fates, one click: stray.mkv moves, tracked.mkv collides
// (its copy goes to the trash, and it is itemized on the wire), and the second
// season cannot be merged at all because its destination parent is a plain file.
// Everything an operator would need in that moment has to be in the answer and
// in the log: the outcome is `failed` and not `refused` (cutoffarr TRIED —
// nothing here is a decision it made), the status is 502, the message says how
// many files are already on the other side, and the line carries moved= and
// collided= at ERROR, because a library left half-moved must not be a line an
// INFO-level deployment never sees.
func TestAction_MergeCaseTwin_APartialFailureIsA502LoggedAtErrorWithItsCounts(t *testing.T) {
	root := t.TempDir()
	if !caseSensitiveFS(t, root) {
		t.Skip("this filesystem is case-insensitive, so a real case-twin cannot exist to be half-merged")
	}
	fake := newArrFake(t)
	writeActionFile(t, root, "show/Season 01/tracked.mkv", "the tracked copy")
	writeActionFile(t, root, "SHOW/Season 01/tracked.mkv", "the untracked copy")
	writeActionFile(t, root, "SHOW/Season 01/stray.mkv", "stray")
	writeActionFile(t, root, "SHOW/Season 02/second.mkv", "second")
	// The obstruction, mid-walk and after a completed move.
	writeActionFile(t, root, "show/Season 02", "not a directory")
	fake.series = `[{"id":3,"title":"Show","monitored":true,"tags":[],"path":"/tv/show","seasons":[]}]`
	fake.episodeFiles["3"] = `[{"id":30,"seriesId":3,"seasonNumber":1,"path":"/tv/show/Season 01/tracked.mkv"}]`
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "k",
		MediaRootMap: map[string]string{"/tv": root}}

	cfg := Config{DryRun: false, GUIActions: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, buf := newActionFixture(t, cfg, inst)
	seedTwinFindings(store, inst,
		twinFinding(root, caseCollisionNameRecord{Name: "SHOW"}, caseCollisionNameRecord{Name: "show", Tracked: true}))

	status, out := postAction(t, ts,
		`{"kind":"merge-case-twin","confirm":true,"instance":"sonarr-main","path":`+jsonString(root)+`,"tracked":"show","untracked":"SHOW"}`)
	if status != http.StatusBadGateway || out.Outcome != actionOutcomeFailed {
		t.Fatalf("status=%d outcome=%q reason=%q, want 502/failed\nlog:\n%s", status, out.Outcome, out.Reason, buf.String())
	}
	if !strings.Contains(out.Message, "1 file(s) had already been moved and were left where they now are") {
		t.Errorf("message = %q, want it to say how much of the library is already on the other side", out.Message)
	}
	if len(out.Items) != 1 || !strings.Contains(out.Items[0], "tracked.mkv") {
		t.Errorf("items = %v, want the one collision itemized: a failure does not excuse the response from saying which file went to the trash", out.Items)
	}
	log := buf.String()
	if !strings.Contains(log, "level=ERROR msg=action") {
		t.Errorf("a half-moved library must be greppable at ERROR; log:\n%s", log)
	}
	for _, want := range []string{"source=gui", "kind=merge-case-twin", "outcome=failed", "moved=1", "collided=1"} {
		if !strings.Contains(log, want) {
			t.Errorf("the failure line is missing %q; log:\n%s", want, log)
		}
	}
	// The partial state itself: what moved stayed moved, what did not is still
	// where it was, and nothing was trashed to tidy up after the failure.
	if body, err := os.ReadFile(filepath.Join(root, "show", "Season 01", "stray.mkv")); err != nil || string(body) != "stray" {
		t.Errorf("the file that moved must still be under the tracked spelling: contents=%q err=%v", body, err)
	}
	if body, err := os.ReadFile(filepath.Join(root, "show", "Season 01", "tracked.mkv")); err != nil || string(body) != "the tracked copy" {
		t.Errorf("the tracked copy must never be overwritten, failure or not: contents=%q err=%v", body, err)
	}
	if body, err := os.ReadFile(filepath.Join(root, "SHOW", "Season 02", "second.mkv")); err != nil || string(body) != "second" {
		t.Errorf("the file the walk failed on must be untouched: contents=%q err=%v", body, err)
	}
	// And the operator's own list says a click happened and what came of it.
	rec := assertOneListedAction(t, store, ActionMergeCaseTwin, actionOutcomeFailed)
	if !strings.Contains(rec.Reason, "already been moved") {
		t.Errorf("lastActions[0].Reason = %q, want the same answer the operator got", rec.Reason)
	}
}

// TestAction_MergeCaseTwin_IneligibleShapeIsRefusedWithItsReason is rule 5's
// report-only half reaching the operator through the endpoint rather than
// being silently button-less.
func TestAction_MergeCaseTwin_IneligibleShapeIsRefusedWithItsReason(t *testing.T) {
	root := t.TempDir()
	fake := newArrFake(t)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "k",
		MediaRootMap: map[string]string{"/tv": root}}
	cfg := Config{DryRun: false, GUIActions: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, _ := newActionFixture(t, cfg, inst)
	store.recordInstance(cycleKindSweep, time.Now(), inst.Name, inst.Type, cycleInstanceStats{
		decisionsRan: true, fileReportRan: true,
		fileReport: fileReportSnapshot{Status: "ran", CaseCollisions: 1, Findings: []fileReportFindingRecord{{
			Kind: fileKindCaseCollision, Path: root, Display: "d", EntryType: fileReportEntryTypeDir,
			Names: []caseCollisionNameRecord{{Name: "SHOW"}, {Name: "show"}}, // neither tracked
		}}},
	})

	status, out := postAction(t, ts,
		`{"kind":"merge-case-twin","confirm":true,"instance":"sonarr-main","path":`+jsonString(root)+`,"tracked":"show","untracked":"SHOW"}`)
	if status != http.StatusConflict || out.Outcome != actionOutcomeRefused {
		t.Fatalf("status=%d outcome=%q, want 409/refused", status, out.Outcome)
	}
	if !strings.Contains(out.Reason, "neither spelling") {
		t.Errorf("reason = %q, want the eligibility reason a row would show", out.Reason)
	}
}

// --- self-review findings, pinned ------------------------------------------

// TestActionEndpoint_ExtendsItsOwnDeadlineBeyondTheServersWriteTimeout is a
// defect found in self-review and fixed with this test.
//
// The daemon's *http.Server sets WriteTimeout: 30s, which is generous for
// every endpoint that existed before v2.2 — they answer from memory. An
// action does not: rule 3 makes it re-derive its finding from LIVE data, and
// for a Sonarr instance that is a per-series /episodefile fetch across the
// whole library plus a full walk of every mapped root. On a real library that
// is minutes, not seconds.
//
// Left alone, the connection would be torn down mid-action: the operator sees
// a network error and has no idea whether the file moved, which is the exact
// "never lies about what happened" failure this phase exists to avoid. The
// handler therefore extends its own deadline (http.ResponseController), and
// bounds the work with a matching context deadline so a genuinely stuck
// action cannot run forever either.
func TestActionEndpoint_ExtendsItsOwnDeadlineBeyondTheServersWriteTimeout(t *testing.T) {
	page := readRepoFileForActions(t, "webui.go")
	if !strings.Contains(page, "NewResponseController") {
		t.Error("handleAction never extends its write deadline; a live re-derivation that outlasts the server's 30s WriteTimeout would tear the connection down mid-action")
	}
	if !strings.Contains(page, "actionDeadline") {
		t.Error("the action's own deadline is not a named constant, so the write deadline and the context deadline can drift apart")
	}
	if !strings.Contains(page, "context.WithTimeout") {
		t.Error("the action's context is unbounded: a stuck *arr would hold the single-flight lock forever and every later click would block behind it")
	}
}

// TestHumanSize_MatchesThePagesOwnRounding is a second self-review finding.
// The button's label is built by the PAGE and the operation the server echoes
// back is built by the SERVER; if the two round differently, a 1.5 KB file
// reads as "2 KB" in the confirm dialog and "1 KB" in the answer, and the row
// is quietly claiming the server did something other than what was confirmed.
func TestHumanSize_MatchesThePagesOwnRounding(t *testing.T) {
	// The page uses Math.round, which rounds halves AWAY from zero. Go's
	// %.0f rounds halves to EVEN, so these two cases diverged before the fix.
	for _, tc := range []struct {
		n    int64
		want string
	}{
		{0, "size unknown"},
		{-1, "size unknown"},
		{512, "512 B"},
		{1536, "2 KB"},         // 1.5 -> away from zero
		{2560, "3 KB"},         // 2.5 -> away from zero (Go's %.0f gave "2")
		{2621440, "3 MB"},      // 2.5 MB, same trap one unit up
		{1610612736, "1.5 GB"}, // GB keeps one decimal, both sides
	} {
		if got := humanSize(tc.n); got != tc.want {
			t.Errorf("humanSize(%d) = %q, want %q — the server's label must match the page's, or the confirm dialog and the answer disagree about the same file", tc.n, got, tc.want)
		}
	}
}

// TestActionRunner_NilLoggerNeverPanics is the third: a runner constructed
// without a logger (every pre-v2.2 test helper builds a webUIServer that way,
// and so could a future embedding) used to reach resolveExclusionTagID with a
// nil *slog.Logger the moment anyone posted an action.
func TestActionRunner_NilLoggerNeverPanics(t *testing.T) {
	fake := newArrFake(t)
	_, dupPath, inst := radarrFileFixture(t, fake)
	cfg := Config{DryRun: true, GUIActions: true, ExclusionTag: "cutoffarr-exclude", Instances: []Instance{inst}}
	store := newStatsStore(true)
	seedFileFinding(store, inst, fileReportFindingRecord{Kind: fileKindDuplicate, Path: dupPath, Display: "d", Size: 10})

	runner := newActionRunner(cfg, nil, store)
	resp := runner.run(context.Background(), actionRequest{
		Kind: ActionTrash, Confirm: true, Instance: inst.Name, Path: dupPath, Finding: fileKindDuplicate, Size: 10,
	})
	if resp.Outcome != actionOutcomeRehearsed {
		t.Errorf("outcome = %q (%q), want a normal rehearsal even with no logger wired", resp.Outcome, resp.Reason)
	}
}

// TestAction_Rehearsal_CreatesOnlyTheEmptyTrashDirectory pins the one
// filesystem effect a rehearsal has, as a DECISION rather than an accident.
//
// Rule 8 says the writability probe is the trash-directory create, at action
// time. A rehearsal that skipped it would answer "this would work" on a
// read-only mount, which is a rehearsal that lies — and the default deploy
// mounts media :ro, so that is the COMMON case, not a corner. So the
// rehearsal probes, and its entire footprint is one empty dot-directory that
// the *arrs ignore and the file report prunes.
func TestAction_Rehearsal_CreatesOnlyTheEmptyTrashDirectory(t *testing.T) {
	fake := newArrFake(t)
	root, dupPath, inst := radarrFileFixture(t, fake)
	cfg := Config{DryRun: true, GUIActions: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, _ := newActionFixture(t, cfg, inst)
	seedFileFinding(store, inst, fileReportFindingRecord{Kind: fileKindDuplicate, Path: dupPath, Display: "d", Size: 10})

	status, out := postAction(t, ts, `{"kind":"trash","confirm":true,"instance":"radarr-main","path":`+jsonString(dupPath)+`,"finding":"duplicate","size":10}`)
	if status != http.StatusOK || out.Outcome != actionOutcomeRehearsed {
		t.Fatalf("status=%d outcome=%q reason=%q, want 200/rehearsed", status, out.Outcome, out.Reason)
	}

	trash := filepath.Join(root, trashDirName)
	info, err := os.Stat(trash)
	if err != nil || !info.IsDir() {
		t.Fatalf("the rehearsal must have probed writability by creating %s: err=%v", trash, err)
	}
	entries, err := os.ReadDir(trash)
	if err != nil {
		t.Fatalf("reading the trash: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the trash holds %d entry/entries after a REHEARSAL; its whole footprint must be one empty directory", len(entries))
	}
	if _, err := os.Lstat(dupPath); err != nil {
		t.Errorf("the file itself must be untouched: %v", err)
	}
}

func readRepoFileForActions(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(b)
}

// --- the Sonarr shape -------------------------------------------------------

// TestActionScope_NarrowsToTheOneItemAndSeason is what stops a season button
// from re-monitoring a whole series. The scope's `seasons` map is the second
// level of narrowing (the same one a webhook uses), and without it a click on
// "Show season 2" would hand the reverse pass every eligible season of that
// series.
func TestActionScope_NarrowsToTheOneItemAndSeason(t *testing.T) {
	movie := actionScope(152, nil)
	if len(movie.ids) != 1 || movie.ids[0] != 152 {
		t.Errorf("ids = %v, want exactly [152]", movie.ids)
	}
	if movie.seasons != nil {
		t.Errorf("seasons = %v, want nil for a Radarr movie", movie.seasons)
	}
	if movie.origin != scopeOriginGUIAction {
		t.Errorf("origin = %q, want %q", movie.origin, scopeOriginGUIAction)
	}
	if movie.itemLevel != slog.LevelInfo {
		t.Errorf("itemLevel = %v, want Info: a human is watching this one run", movie.itemLevel)
	}

	season := 2
	got := actionScope(9, &season)
	if !got.containsSeason(9, 2) {
		t.Error("the scope must contain the season the button named")
	}
	for _, other := range []int{1, 3} {
		if got.containsSeason(9, other) {
			t.Errorf("the scope contains season %d as well; a click on one season must never re-monitor the rest of the series", other)
		}
	}
}

// TestAction_Remonitor_SonarrDispatchesTheSeasonPathAndWritesNothingWhenStale
// exercises the Sonarr branch end to end through the endpoint. The series here
// has no unmonitored-and-failing season, so the correct outcome is a refusal
// with no write at all — which is also the strongest thing to assert about a
// path whose failure mode would be writing to the wrong shape.
func TestAction_Remonitor_SonarrDispatchesTheSeasonPathAndWritesNothingWhenStale(t *testing.T) {
	fake := newArrFake(t)
	fake.series = `[{"id":9,"title":"Show","monitored":true,"tags":[],"path":"/tv/Show",
	  "seasons":[{"seasonNumber":1,"monitored":true,"statistics":{"episodeFileCount":1,"totalEpisodeCount":1}}]}]`
	fake.episodeFiles["9"] = `[{"id":90,"seriesId":9,"seasonNumber":1,"path":"/tv/Show/Show.S01E01.mkv"}]`
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "k"}

	cfg := Config{DryRun: false, GUIActions: true, ReverseScanRemonitor: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, _ := newActionFixture(t, cfg, inst)
	season := 1
	seedReverseFinding(store, inst, reverseFinding{SeriesID: 9, Series: "Show", Season: &season, Reason: ReasonQualityCutoffNotMet})

	status, out := postAction(t, ts, `{"kind":"remonitor","confirm":true,"instance":"sonarr-main","id":9,"season":1}`)
	if status != http.StatusConflict || out.Outcome != actionOutcomeRefused {
		t.Fatalf("status=%d outcome=%q reason=%q, want 409/refused: this season is monitored and complete, so it is not a finding", status, out.Outcome, out.Reason)
	}
	if !strings.Contains(out.Operation, "season 1") || !strings.Contains(out.Operation, "series 9") {
		t.Errorf("operation = %q, want it to name the season and the series id", out.Operation)
	}
	if fake.writeCount() != 0 {
		t.Errorf("a refused Sonarr re-monitor must send no write; writes=%v", fake.writes)
	}
}

// sonarrRemonitorFixture is remonitorFixture's Sonarr twin, and it exists
// because of a round-3 review finding: once the pre-check
// (sonarrRemonitorTargetRefusal) landed, EVERY Sonarr re-monitor test stopped
// before runSonarrDecisionEngine was entered, so rule 6's Sonarr half — drive
// the existing gated reverse path scoped to ONE season — had no end-to-end
// coverage at all. A wrong argument, a scope that failed to narrow to the
// season, or a broken remonitorOutcome mapping would have been invisible.
//
// Two series, for the same reason the Radarr fixture has two movies:
//
//   - series 7 is the CROSS-CHECK WITNESS. Monitored, complete, aired, its own
//     file agreeing that its cutoff is met. Without at least one verifiable
//     sample, reverseWriteGateBlockReason withholds every reverse write by
//     design and the test would prove only that a blocked pass writes nothing.
//     It is also the control for the scope: nothing may ever be written to it,
//     in either direction, however eligible the forward pass finds it.
//   - series 9 holds TWO equally eligible findings: season 1 (the one clicked)
//     and season 2. Both are UNMONITORED, complete, aired and — when
//     belowCutoff — both sit in the unmonitored wanted set. Season 2 is what
//     makes "scoped to the one item" checkable instead of asserted: a scope
//     that narrowed to the series and not to the season would write both, and
//     both halves of a season write address the SERIES, so only the request
//     bodies can tell the difference.
//
// belowCutoff false is the SAME library with the one difference that makes the
// seasons no longer findings — Sonarr no longer reports their episodes as below
// cutoff — which is what drives remonitorOutcome's default branch.
func sonarrRemonitorFixture(t *testing.T, fake *arrFake, belowCutoff bool) Instance {
	t.Helper()
	fake.series = `[
	  {"id":7,"title":"Ordinary Monitored Show","monitored":true,"qualityProfileId":1,"tags":[],"path":"/tv/Ordinary",
	   "seasons":[{"seasonNumber":1,"monitored":true,"statistics":{"episodeFileCount":1,"totalEpisodeCount":1}}]},
	  {"id":9,"title":"Show","monitored":true,"qualityProfileId":1,"tags":[],"path":"/tv/Show",
	   "seasons":[{"seasonNumber":1,"monitored":false,"statistics":{"episodeFileCount":1,"totalEpisodeCount":1}},
	              {"seasonNumber":2,"monitored":false,"statistics":{"episodeFileCount":1,"totalEpisodeCount":1}}]}
	]`
	fake.episodes["7"] = `[{"id":700,"seriesId":7,"seasonNumber":1,"episodeNumber":1,"monitored":true,"hasFile":true,"airDateUtc":"` + pastAirDate + `","episodeFileId":7000}]`
	fake.episodeFiles["7"] = `[{"id":7000,"seriesId":7,"seasonNumber":1,"customFormatScore":200,"qualityCutoffNotMet":false}]`
	// The clicked season's episodes are unmonitored too: a season this project
	// itself unmonitored, episodes and all, which is the shape the reverse pass
	// is allowed to write (a season with monitored episodes inside it is
	// refused as a mixed state — binding controller ruling R2).
	fake.episodes["9"] = `[{"id":900,"seriesId":9,"seasonNumber":1,"episodeNumber":1,"monitored":false,"hasFile":true,"airDateUtc":"` + pastAirDate + `","episodeFileId":9000},
	  {"id":901,"seriesId":9,"seasonNumber":2,"episodeNumber":1,"monitored":false,"hasFile":true,"airDateUtc":"` + pastAirDate + `","episodeFileId":9001}]`
	fake.episodeFiles["9"] = fmt.Sprintf(`[{"id":9000,"seriesId":9,"seasonNumber":1,"customFormatScore":200,"qualityCutoffNotMet":%t},
	  {"id":9001,"seriesId":9,"seasonNumber":2,"customFormatScore":200,"qualityCutoffNotMet":%t}]`, belowCutoff, belowCutoff)

	emptyWanted := `{"page":1,"pageSize":1000,"totalRecords":0,"records":[]}`
	fake.wanted[""] = emptyWanted
	fake.wanted["false"] = emptyWanted
	if belowCutoff {
		fake.wanted["false"] = `{"page":1,"pageSize":1000,"totalRecords":2,"records":[
		  {"id":900,"seriesId":9,"seasonNumber":1},{"id":901,"seriesId":9,"seasonNumber":2}]}`
	}

	// The write path's own pre-write fetch, which must agree with the library
	// read: the series monitored, both seasons not.
	fake.seriesByID["9"] = `{"id":9,"title":"Show","monitored":true,"qualityProfileId":1,"tags":[],"path":"/tv/Show",
	  "seasons":[{"seasonNumber":1,"monitored":false,"statistics":{"episodeFileCount":1,"totalEpisodeCount":1}},
	             {"seasonNumber":2,"monitored":false,"statistics":{"episodeFileCount":1,"totalEpisodeCount":1}}]}`
	fake.seriesByID["7"] = `{"id":7,"title":"Ordinary Monitored Show","monitored":true,"qualityProfileId":1,"tags":[],"path":"/tv/Ordinary",
	  "seasons":[{"seasonNumber":1,"monitored":true,"statistics":{"episodeFileCount":1,"totalEpisodeCount":1}}]}`
	return Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "k"}
}

// TestAction_Remonitor_SonarrPerformsExactlyOneSeasonWrite is rule 6's Sonarr
// half, end to end through the endpoint and all the way through
// runSonarrDecisionEngine's gated reverse write pass.
//
// What it pins that no Radarr test can: that the season really is the unit. The
// library holds a second, entirely eligible series and a second monitored
// season, and the click must reach neither — not through the reverse pass (the
// scope's ids), not through the forward pass (the scope's
// noForwardWrites suppression), not through anything.
func TestAction_Remonitor_SonarrPerformsExactlyOneSeasonWrite(t *testing.T) {
	fake := newArrFake(t)
	inst := sonarrRemonitorFixture(t, fake, true)
	cfg := Config{DryRun: false, GUIActions: true, ReverseScanRemonitor: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, buf := newActionFixture(t, cfg, inst)
	season := 1
	seedReverseFinding(store, inst, reverseFinding{SeriesID: 9, Series: "Show", Season: &season, Reason: ReasonQualityCutoffNotMet})

	status, out := postAction(t, ts, `{"kind":"remonitor","confirm":true,"instance":"sonarr-main","id":9,"season":1}`)
	if status != http.StatusOK || out.Outcome != actionOutcomePerformed {
		t.Fatalf("status=%d outcome=%q reason=%q, want 200/performed\nlog:\n%s", status, out.Outcome, out.Reason, buf.String())
	}
	if !strings.Contains(out.Operation, "season 1") || !strings.Contains(out.Operation, "series 9") {
		t.Errorf("operation = %q, want it to name the season and the series id", out.Operation)
	}

	fake.mu.Lock()
	writes := append([]string(nil), fake.writes...)
	bodies := append([]string(nil), fake.writeBodies...)
	fake.mu.Unlock()
	// A season is TWO writes by construction (writeSeasonMonitored: the
	// episodes, then the season flag), and both must name series 9.
	want := []string{"PUT /api/v3/episode/monitor", "PUT /api/v3/series/9"}
	if len(writes) != len(want) {
		t.Fatalf("writes = %v, want exactly %v — one season, one pair of writes\nlog:\n%s", writes, want, buf.String())
	}
	for i, w := range want {
		if writes[i] != w {
			t.Errorf("writes[%d] = %q, want %q", i, writes[i], w)
		}
	}
	if strings.Contains(strings.Join(writes, " "), "/api/v3/series/7") {
		t.Errorf("the click on series 9 wrote to series 7 as well: %v", writes)
	}

	// The season narrowing, read from the bodies because the paths cannot show
	// it: season 2 of this same series is an equally eligible finding, and a
	// scope that stopped at the series would have taken it too.
	if !strings.Contains(bodies[0], `"episodeIds":[900]`) {
		t.Errorf("the episode write named %s, want exactly episode 900 — season 1's only episode; 901 belongs to the season nobody clicked", bodies[0])
	}
	var written struct {
		ID      int `json:"id"`
		Seasons []struct {
			SeasonNumber int  `json:"seasonNumber"`
			Monitored    bool `json:"monitored"`
		} `json:"seasons"`
	}
	if err := json.Unmarshal([]byte(bodies[1]), &written); err != nil {
		t.Fatalf("the season write body is not a series object: %v\n%s", err, bodies[1])
	}
	if written.ID != 9 || len(written.Seasons) != 2 {
		t.Fatalf("the season write named series %d with %d seasons, want series 9 with both of its seasons intact", written.ID, len(written.Seasons))
	}
	for _, s := range written.Seasons {
		if want := s.SeasonNumber == 1; s.Monitored != want {
			t.Errorf("the write set season %d monitored=%t, want %t: a click on season 1 must change season 1 and nothing else", s.SeasonNumber, s.Monitored, want)
		}
	}

	log := buf.String()
	for _, wantAttr := range []string{"msg=action", "source=gui", "kind=remonitor", "outcome=performed", "season=1", "id=9"} {
		if !strings.Contains(log, wantAttr) {
			t.Errorf("audit line is missing %q; log:\n%s", wantAttr, log)
		}
	}
	// The write is only real if it is also the one the operator's table shows.
	acts := store.snapshot().Instances[0].LastActions
	if len(acts) != 1 || acts[0].Action != ActionRemonitor {
		t.Fatalf("lastActions = %+v, want exactly one %s record", acts, ActionRemonitor)
	}
	if acts[0].Season == nil || *acts[0].Season != 1 || acts[0].ID != 9 {
		t.Errorf("lastActions[0] = %+v, want it to name series 9 season 1", acts[0])
	}
}

// TestAction_Remonitor_SonarrSeasonThatIsNoLongerAFindingIsAnsweredHonestly
// drives remonitorOutcome's DEFAULT branch on the Sonarr shape: the season is
// still unmonitored — so the pre-check passes and the engine really runs — but
// Sonarr no longer reports it as below cutoff, so the reverse pass finds
// nothing and every counter stays at zero.
//
// The honest answer to that is "this instance no longer reports the item as
// wrongly unmonitored", and it must be told apart from the ambiguous zero the
// round-2 fix separated out (a pass that never completed). Nothing may be
// written; the refusal is listed as a refusal (item 9), which is a different
// claim from a write and must not read as one.
func TestAction_Remonitor_SonarrSeasonThatIsNoLongerAFindingIsAnsweredHonestly(t *testing.T) {
	fake := newArrFake(t)
	inst := sonarrRemonitorFixture(t, fake, false)
	cfg := Config{DryRun: false, GUIActions: true, ReverseScanRemonitor: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, buf := newActionFixture(t, cfg, inst)
	season := 1
	seedReverseFinding(store, inst, reverseFinding{SeriesID: 9, Series: "Show", Season: &season, Reason: ReasonQualityCutoffNotMet})

	status, out := postAction(t, ts, `{"kind":"remonitor","confirm":true,"instance":"sonarr-main","id":9,"season":1}`)
	if status != http.StatusConflict || out.Outcome != actionOutcomeRefused {
		t.Fatalf("status=%d outcome=%q reason=%q, want 409/refused\nlog:\n%s", status, out.Outcome, out.Reason, buf.String())
	}
	if !strings.Contains(out.Reason, "no longer reports") {
		t.Errorf("reason = %q, want the default branch's own sentence — not the could-not-complete one, which means something entirely different", out.Reason)
	}
	// The proof that the ENGINE is what answered, rather than a pre-check: the
	// executor paid for the whole live re-derivation, including this series'
	// own episode fetch.
	if fake.requestCount() < 4 {
		t.Errorf("only %d requests were made; this answer must come from a completed live evaluation, not from a short-circuit", fake.requestCount())
	}
	if fake.writeCount() != 0 {
		t.Errorf("a season that is no longer a finding must never be written: %v", fake.writes)
	}
	rec := assertOneListedAction(t, store, ActionRemonitor, actionOutcomeRefused)
	if rec.ID != 9 || rec.Season == nil || *rec.Season != 1 {
		t.Errorf("lastActions[0] = %+v, want it to name series 9 season 1 — the item the button named", rec)
	}
	if !strings.Contains(rec.Reason, "no longer reports") {
		t.Errorf("lastActions[0].Reason = %q, want the same answer the operator got", rec.Reason)
	}
}

// TestActionScope_SuppressesTheSonarrForwardWritePassStructurally is the Sonarr
// twin of the Radarr pin of the same name, and it closes the gap a round-3
// review found: the Sonarr half of the round-2 CRITICAL ("a Re-monitor click
// can never perform an unmonitor") was guarded by nothing but a source-text
// grep, which matches the guard's own words within a window and would pass on
// an INVERTED guard.
//
// The scenario is the Sonarr stale tab: the operator re-monitored season 1
// themselves and an upgrade landed, so the season now MEETS the forward
// criteria — monitored, complete, aired, at cutoff — and a scoped forward write
// pass would PUT monitored:false on exactly the season the button offered to
// re-monitor.
//
// The engine is driven directly, deliberately: the executor's own pre-check
// (sonarrRemonitorTargetRefusal) refuses this state before the engine is
// reached, so an end-to-end click can only ever prove the pre-check. This is
// the only place the second, independent guard is observable at all.
func TestActionScope_SuppressesTheSonarrForwardWritePassStructurally(t *testing.T) {
	fake := newArrFake(t)
	inst := sonarrRemonitorFixture(t, fake, false)
	// The stale tab: season 1 is monitored again and its episode is at cutoff.
	fake.series = strings.Replace(fake.series,
		`{"seasonNumber":1,"monitored":false,"statistics":{"episodeFileCount":1,"totalEpisodeCount":1}},
	              {"seasonNumber":2,`,
		`{"seasonNumber":1,"monitored":true,"statistics":{"episodeFileCount":1,"totalEpisodeCount":1}},
	              {"seasonNumber":2,`, 1)
	fake.episodes["9"] = strings.Replace(fake.episodes["9"], `"seasonNumber":1,"episodeNumber":1,"monitored":false`, `"seasonNumber":1,"episodeNumber":1,"monitored":true`, 1)
	fake.seriesByID["9"] = strings.Replace(fake.seriesByID["9"], `{"seasonNumber":1,"monitored":false`, `{"seasonNumber":1,"monitored":true`, 1)

	logger, buf := newActionTestLogger()
	ctx := context.Background()
	series, wantedEpisodeIDs, wantedSeasons, ok := inspectSonarrLibrary(ctx, logger, inst)
	if !ok {
		t.Fatalf("the fixture library could not be read; log:\n%s", buf.String())
	}
	season := 1
	stats := runSonarrDecisionEngine(ctx, logger, inst, series, wantedEpisodeIDs, wantedSeasons, "cutoffarr-exclude",
		actionScope(9, &season), false, reverseOptions{enabled: true, remonitor: true}, fileReportOptions{})

	if fake.writeCount() != 0 {
		t.Fatalf("an engine run under a GUI action scope wrote %v — a human-clicked action drives the reverse half only, and the one write available here is the INVERSE of the operation the button named; log:\n%s", fake.writes, buf.String())
	}
	if stats.reverse.remonitored != 0 {
		t.Errorf("reverse remonitored = %d, want 0: this season is already monitored", stats.reverse.remonitored)
	}
	// Without this the test could pass on a fixture the forward pass never
	// found anything in, which would prove nothing about the suppression.
	if !strings.Contains(buf.String(), `msg=would-unmonitor instance=sonarr-main seriesId=9 series=Show season=1`) {
		t.Errorf("the forward pass never decided season 1 of series 9 was at cutoff, so there was no forward write for the scope to suppress and this test proves nothing; log:\n%s", buf.String())
	}
}

// TestAction_Remonitor_SonarrWithoutASeasonIsRejectedBeforeAnyFetch is the
// mirror of the Radarr shape guard: a Sonarr finding always names a season,
// so a request without one is malformed rather than "the whole series".
func TestAction_Remonitor_SonarrWithoutASeasonIsRejectedBeforeAnyFetch(t *testing.T) {
	fake := newArrFake(t)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "k"}
	cfg := Config{DryRun: false, GUIActions: true, ReverseScanRemonitor: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, _, _ := newActionFixture(t, cfg, inst)

	status, _ := postAction(t, ts, `{"kind":"remonitor","confirm":true,"instance":"sonarr-main","id":9}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if fake.writeCount() != 0 {
		t.Errorf("writes = %v, want none", fake.writes)
	}
}

// TestAction_TrashAndMerge_NeverWriteToTheArr is the boundary between the two
// halves of this phase, asserted directly: a file action re-READS the *arr to
// re-derive its finding, and must never write to it. The re-derivation is a
// full library read, so "it made no requests" would be the wrong assertion —
// "it made no non-GET requests" is the right one.
func TestAction_TrashAndMerge_NeverWriteToTheArr(t *testing.T) {
	fake := newArrFake(t)
	_, dupPath, inst := radarrFileFixture(t, fake)
	cfg := Config{DryRun: false, GUIActions: true, ReverseScanRemonitor: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, _ := newActionFixture(t, cfg, inst)
	seedFileFinding(store, inst, fileReportFindingRecord{Kind: fileKindDuplicate, Path: dupPath, Display: "d", Size: 10})

	if status, out := postAction(t, ts, `{"kind":"trash","confirm":true,"instance":"radarr-main","path":`+jsonString(dupPath)+`,"finding":"duplicate","size":10}`); status != http.StatusOK {
		t.Fatalf("status=%d outcome=%q reason=%q", status, out.Outcome, out.Reason)
	}
	if fake.writeCount() != 0 {
		t.Errorf("a trash action wrote to the *arr: %v — file actions are §2.2-silent by construction", fake.writes)
	}
}

// TestAction_Trash_ReadOnlyRootRefusesBeforeContactingTheArrAtAll is a
// self-review finding about ORDER, not about correctness of the refusal.
//
// A read-only media mount is the DEFAULT deployment, so "every file button
// refuses" is the ordinary steady state, not an edge case. Re-deriving the
// finding first meant every one of those refusals first paid for a full
// library read — a per-series /episodefile fetch across the whole library, for
// Sonarr — to arrive at an answer that was already knowable from a directory
// create.
//
// The order is therefore: the in-memory authorization gate (which is what
// stops an arbitrary path from even causing a probe), then the writability
// probe, then the expensive live re-derivation. Nothing is weakened — every
// gate still runs before anything moves — and the common refusal became free.
func TestAction_Trash_ReadOnlyRootRefusesBeforeContactingTheArrAtAll(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a 0555 directory is still writable")
	}
	fake := newArrFake(t)
	root, dupPath, inst := radarrFileFixture(t, fake)
	cfg := Config{DryRun: false, GUIActions: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, _ := newActionFixture(t, cfg, inst)
	seedFileFinding(store, inst, fileReportFindingRecord{Kind: fileKindDuplicate, Path: dupPath, Display: "d", Size: 10})

	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	status, out := postAction(t, ts, `{"kind":"trash","confirm":true,"instance":"radarr-main","path":`+jsonString(dupPath)+`,"finding":"duplicate","size":10}`)
	if status != http.StatusConflict || out.Outcome != actionOutcomeRefused {
		t.Fatalf("status=%d outcome=%q, want 409/refused", status, out.Outcome)
	}
	if n := fake.requestCount(); n != 0 {
		t.Errorf("the *arr was contacted %d time(s) before a refusal that a directory create already settled; on the DEFAULT :ro deployment that is a full library read per click", n)
	}
}

// --- round-2 review fixes ---------------------------------------------------

// remonitorStaleTabFixture is remonitorFixture after the ordinary follow-up to
// reading a re-monitor finding: the operator re-monitored movie 152 in Radarr
// themselves and an upgrade landed, so the movie is now MONITORED and its file
// is at the profile's cutoff. The click that arrives a week later, from a stale
// tab, still says "Re-monitor — Fever Pitch (movie 152)".
//
// In that state the FORWARD engine's own criteria are met — monitored, has a
// file, cutoff met — so a scoped forward write pass would PUT monitored:false
// on exactly the item the button offered to re-monitor. Movie 1 is left as the
// cross-check's verifiable sample so the write gate is open, which is what
// makes this fixture a test of the action and not of the gate.
func remonitorStaleTabFixture(t *testing.T, fake *arrFake) Instance {
	t.Helper()
	inst := remonitorFixture(t, fake)
	fake.movies = `[
	  {"id":1,"title":"Already Fine","monitored":true,"hasFile":true,"qualityProfileId":1,"tags":[],
	   "movieFile":{"id":10,"path":"/movies/Already Fine/f.mkv","quality":{"quality":{"id":2,"name":"1080p"}},"qualityCutoffNotMet":false}},
	  {"id":152,"title":"Fever Pitch","monitored":true,"hasFile":true,"qualityProfileId":1,"tags":[],
	   "movieFile":{"id":20,"path":"/movies/Fever Pitch/f.mkv","quality":{"quality":{"id":2,"name":"1080p"}},"qualityCutoffNotMet":false}}
	]`
	fake.movieByID["152"] = `{"id":152,"title":"Fever Pitch","monitored":true,"hasFile":true,"qualityProfileId":1,"tags":[],
	   "movieFile":{"id":20,"path":"/movies/Fever Pitch/f.mkv","quality":{"quality":{"id":2,"name":"1080p"}},"qualityCutoffNotMet":false}}`
	// Nothing is below cutoff any more, in either wanted set.
	fake.wanted["false"] = `{"page":1,"pageSize":1000,"totalRecords":0,"records":[]}`
	return inst
}

// TestAction_Remonitor_NeverPerformsTheForwardUnmonitorWrite is the round-2
// CRITICAL: a "Re-monitor" click must never perform the INVERSE operation.
//
// The re-monitor executor drives the decision engine scoped to the clicked
// item, and that engine's FORWARD write pass used to run unconditionally
// before the reverse pass was ever reached. On a stale tab whose item is now
// monitored and at cutoff, that pass unmonitored the very item the button
// offered to re-monitor — and the response then said there was nothing to do.
func TestAction_Remonitor_NeverPerformsTheForwardUnmonitorWrite(t *testing.T) {
	fake := newArrFake(t)
	inst := remonitorStaleTabFixture(t, fake)
	cfg := Config{DryRun: false, GUIActions: true, ReverseScanRemonitor: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, buf := newActionFixture(t, cfg, inst)
	seedReverseFinding(store, inst, reverseFinding{ID: 152, Title: "Fever Pitch", Reason: ReasonQualityCutoffNotMet})

	status, out := postAction(t, ts, `{"kind":"remonitor","confirm":true,"instance":"radarr-main","id":152}`)
	if fake.writeCount() != 0 {
		t.Fatalf("a re-monitor click performed %v — the inverse of the operation the button named; log:\n%s", fake.writes, buf.String())
	}
	if status != http.StatusConflict || out.Outcome != actionOutcomeRefused {
		t.Fatalf("status=%d outcome=%q reason=%q, want 409/refused", status, out.Outcome, out.Reason)
	}
	if !strings.Contains(out.Reason, "already monitored") {
		t.Errorf("reason = %q, want it to state that the item is already monitored", out.Reason)
	}
}

// TestActionScope_SuppressesTheForwardWritePassStructurally pins the fix one
// level below the executor: an engine run under a GUI action's scope composes
// no forward write at all, whatever state the named item is in. Without this,
// the endpoint test above would pass on a pre-check alone and the forward
// write would still be one refactor away from firing.
func TestActionScope_SuppressesTheForwardWritePassStructurally(t *testing.T) {
	fake := newArrFake(t)
	inst := remonitorStaleTabFixture(t, fake)
	logger, buf := newActionTestLogger()

	ctx := context.Background()
	movies, wantedIDs, ok := inspectRadarrLibrary(ctx, logger, inst, nil)
	if !ok {
		t.Fatalf("the fixture library could not be read; log:\n%s", buf.String())
	}
	stats := runRadarrDecisionEngine(ctx, logger, inst, movies, wantedIDs, "cutoffarr-exclude",
		actionScope(152, nil), false, reverseOptions{enabled: true, remonitor: true}, fileReportOptions{})

	if fake.writeCount() != 0 {
		t.Fatalf("an engine run under a GUI action scope wrote %v; a human-clicked action drives the reverse half only", fake.writes)
	}
	if stats.reverse.remonitored != 0 {
		t.Errorf("reverse remonitored = %d, want 0 for an already-monitored item", stats.reverse.remonitored)
	}
}

// gateBlockedRemonitorFixture is remonitorFixture with the cross-check's one
// verifiable sample REMOVED, which is the ordinary state of any library the
// cross-check cannot sample: nothing establishes that the instance's data is
// sound, so reverseWriteGateBlockReason blocks every reverse write.
//
// The point of the fixture is that turning dry_run off does NOT overcome this.
// It is the case the rehearsal branch used to swallow.
func gateBlockedRemonitorFixture(t *testing.T, fake *arrFake) Instance {
	t.Helper()
	inst := remonitorFixture(t, fake)
	fake.movies = `[
	  {"id":152,"title":"Fever Pitch","monitored":false,"hasFile":true,"qualityProfileId":1,"tags":[],
	   "movieFile":{"id":20,"path":"/movies/Fever Pitch/f.mkv","quality":{"quality":{"id":1,"name":"720p"}},"qualityCutoffNotMet":true}}
	]`
	return inst
}

// TestAction_Remonitor_DryRunDoesNotDisguiseAGateBlockAsARehearsal is the
// round-2 IMPORTANT: `withheld > 0 && DryRun` used to be tested BEFORE the
// withheldReason branch, so on the DEFAULT deployment (dry_run: true) every
// non-dry-run withholding — a blocked cross-check gate, a Sonarr season under
// an unmonitored series, a shutdown — was reported as a successful rehearsal
// promising a write that turning dry_run off would not produce.
//
// The two halves are asserted together, because the lie is only visible as a
// contradiction: the same fixture must not answer "this would work" under
// dry_run and "refused, here is the gate" without it.
func TestAction_Remonitor_DryRunDoesNotDisguiseAGateBlockAsARehearsal(t *testing.T) {
	post := func(t *testing.T, dryRun bool) (int, actionResponse, *arrFake) {
		t.Helper()
		fake := newArrFake(t)
		inst := gateBlockedRemonitorFixture(t, fake)
		cfg := Config{DryRun: dryRun, GUIActions: true, ReverseScanRemonitor: true, ExclusionTag: "cutoffarr-exclude"}
		_, ts, store, _ := newActionFixture(t, cfg, inst)
		seedReverseFinding(store, inst, reverseFinding{ID: 152, Title: "Fever Pitch", Reason: ReasonQualityCutoffNotMet})
		status, out := postAction(t, ts, `{"kind":"remonitor","confirm":true,"instance":"radarr-main","id":152}`)
		return status, out, fake
	}

	liveStatus, live, _ := post(t, false)
	if liveStatus != http.StatusConflict || live.Outcome != actionOutcomeRefused {
		t.Fatalf("dry_run:false status=%d outcome=%q reason=%q, want 409/refused quoting the gate", liveStatus, live.Outcome, live.Reason)
	}
	if !strings.Contains(live.Reason, "cross-check") {
		t.Fatalf("dry_run:false reason = %q, want the gate's own reason", live.Reason)
	}

	status, out, fake := post(t, true)
	if status != http.StatusConflict || out.Outcome != actionOutcomeRefused {
		t.Fatalf("dry_run:true status=%d outcome=%q reason=%q, want the SAME 409/refused: dry_run is not what is stopping this write",
			status, out.Outcome, out.Reason)
	}
	if !strings.Contains(out.Reason, "cross-check") {
		t.Errorf("dry_run:true reason = %q, want the gate's own reason rather than a rehearsal that promises a write dry_run:false would not make", out.Reason)
	}
	if fake.writeCount() != 0 {
		t.Errorf("writes = %v, want none", fake.writes)
	}
}

// TestAction_Remonitor_DryRunStillRehearsesWhenDryRunIsTheOnlyThingStopping
// It is the other half of the branch above: when the §2.1 dry-run gate is
// genuinely the only thing withholding the write, the answer must still be a
// rehearsal. Fixing the ordering must not turn every rehearsal into a refusal.
func TestAction_Remonitor_DryRunStillRehearsesWhenDryRunIsTheOnlyThingStopping(t *testing.T) {
	fake := newArrFake(t)
	inst := remonitorFixture(t, fake)
	cfg := Config{DryRun: true, GUIActions: true, ReverseScanRemonitor: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, _ := newActionFixture(t, cfg, inst)
	seedReverseFinding(store, inst, reverseFinding{ID: 152, Title: "Fever Pitch", Reason: ReasonQualityCutoffNotMet})

	status, out := postAction(t, ts, `{"kind":"remonitor","confirm":true,"instance":"radarr-main","id":152}`)
	if status != http.StatusOK || out.Outcome != actionOutcomeRehearsed {
		t.Fatalf("status=%d outcome=%q reason=%q, want 200/rehearsed", status, out.Outcome, out.Reason)
	}
	if fake.writeCount() != 0 {
		t.Errorf("writes = %v, want none", fake.writes)
	}
}

// TestAction_Remonitor_ASkippedReversePassIsNeverReportedAsNothingToDo is the
// round-2 IMPORTANT on the default branch: "no counters moved" was answered
// with the positive claim that the instance no longer reports the item as
// wrongly unmonitored. That branch is also reached when the evaluation never
// happened at all — here, because the reverse pass's own unmonitored
// wanted/cutoff set could not be fetched — and answering "there was nothing to
// do" then is guessing library state from a pass that did not run (§2.6).
func TestAction_Remonitor_ASkippedReversePassIsNeverReportedAsNothingToDo(t *testing.T) {
	fake := newArrFake(t)
	inst := remonitorFixture(t, fake)
	fake.wanted["false"] = `{"page":1,"pageSize":1000,"totalRecords":`
	cfg := Config{DryRun: false, GUIActions: true, ReverseScanRemonitor: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, buf := newActionFixture(t, cfg, inst)
	seedReverseFinding(store, inst, reverseFinding{ID: 152, Title: "Fever Pitch", Reason: ReasonQualityCutoffNotMet})

	status, out := postAction(t, ts, `{"kind":"remonitor","confirm":true,"instance":"radarr-main","id":152}`)
	if status != http.StatusConflict || out.Outcome != actionOutcomeRefused {
		t.Fatalf("status=%d outcome=%q, want 409/refused", status, out.Outcome)
	}
	if strings.Contains(out.Reason, "nothing to do") {
		t.Errorf("reason = %q, but the pass never ran — see the WARN in:\n%s", out.Reason, buf.String())
	}
	if !strings.Contains(out.Reason, "could not be completed") {
		t.Errorf("reason = %q, want it to say the evaluation could not be completed or trusted and point at the log", out.Reason)
	}
	if fake.writeCount() != 0 {
		t.Errorf("writes = %v, want none", fake.writes)
	}
}

// TestAction_Remonitor_AnEngineThatNeverDecidedIsNeverReportedAsNothingToDo is
// the same rule one step earlier: §2.6's warn-and-skip bare returns (here, the
// exclusion tag could not be resolved) leave every counter zero WITHOUT the
// engine having decided anything at all.
func TestAction_Remonitor_AnEngineThatNeverDecidedIsNeverReportedAsNothingToDo(t *testing.T) {
	fake := newArrFake(t)
	inst := remonitorFixture(t, fake)
	fake.tags = `[{"id":`
	cfg := Config{DryRun: false, GUIActions: true, ReverseScanRemonitor: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, buf := newActionFixture(t, cfg, inst)
	seedReverseFinding(store, inst, reverseFinding{ID: 152, Title: "Fever Pitch", Reason: ReasonQualityCutoffNotMet})

	status, out := postAction(t, ts, `{"kind":"remonitor","confirm":true,"instance":"radarr-main","id":152}`)
	if status != http.StatusConflict || out.Outcome != actionOutcomeRefused {
		t.Fatalf("status=%d outcome=%q, want 409/refused", status, out.Outcome)
	}
	if strings.Contains(out.Reason, "nothing to do") {
		t.Errorf("reason = %q, but the engine returned before deciding anything; log:\n%s", out.Reason, buf.String())
	}
	if !strings.Contains(out.Reason, "could not be completed") {
		t.Errorf("reason = %q, want the honest could-not-complete answer", out.Reason)
	}
}

// TestAction_Remonitor_SonarrRefusesASeasonThatIsAlreadyMonitored is the
// Sonarr half of rule 3's named must-refuse, with the refusal saying which
// state it found rather than the generic "nothing to do".
func TestAction_Remonitor_SonarrRefusesASeasonThatIsAlreadyMonitored(t *testing.T) {
	fake := newArrFake(t)
	fake.series = `[{"id":9,"title":"Show","monitored":true,"tags":[],"path":"/tv/Show",
	  "seasons":[{"seasonNumber":1,"monitored":true,"statistics":{"episodeFileCount":1,"totalEpisodeCount":1}}]}]`
	fake.episodeFiles["9"] = `[{"id":90,"seriesId":9,"seasonNumber":1,"path":"/tv/Show/Show.S01E01.mkv"}]`
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "k"}
	cfg := Config{DryRun: false, GUIActions: true, ReverseScanRemonitor: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, _ := newActionFixture(t, cfg, inst)
	season := 1
	seedReverseFinding(store, inst, reverseFinding{SeriesID: 9, Series: "Show", Season: &season, Reason: ReasonQualityCutoffNotMet})

	status, out := postAction(t, ts, `{"kind":"remonitor","confirm":true,"instance":"sonarr-main","id":9,"season":1}`)
	if status != http.StatusConflict || out.Outcome != actionOutcomeRefused {
		t.Fatalf("status=%d outcome=%q, want 409/refused", status, out.Outcome)
	}
	if !strings.Contains(out.Reason, "already monitored") {
		t.Errorf("reason = %q, want it to name the state it found", out.Reason)
	}
	if fake.writeCount() != 0 {
		t.Errorf("writes = %v, want none", fake.writes)
	}
}

// TestAction_Trash_SizeGateHoldsWhenTheRequestOmitsTheSize is the round-2
// IMPORTANT on gate 3: the promise the button made was only enforced `if
// req.Size > 0`, so an untrusted request disabled the whole check by omitting
// one field — and the response still echoed the operation text built from the
// SWEPT size, "proving" an operation on a file that no longer existed.
//
// The authoritative number was already in hand: reported.Size, from the last
// completed sweep, which is what builds that operation text in the first place.
func TestAction_Trash_SizeGateHoldsWhenTheRequestOmitsTheSize(t *testing.T) {
	fake := newArrFake(t)
	root, dupPath, inst := radarrFileFixture(t, fake)
	cfg := Config{DryRun: false, GUIActions: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, _ := newActionFixture(t, cfg, inst)
	seedFileFinding(store, inst, fileReportFindingRecord{Kind: fileKindDuplicate, Path: dupPath, Display: "Some Movie (2019)/ETRG.Sample.mkv", Size: 10})

	// The file at that path was replaced between the sweep and the click.
	if err := os.WriteFile(dupPath, []byte("a much larger sample file than before"), 0o644); err != nil {
		t.Fatalf("replacing the file: %v", err)
	}

	status, out := postAction(t, ts, `{"kind":"trash","confirm":true,"instance":"radarr-main","path":`+jsonString(dupPath)+`,"finding":"duplicate"}`)
	if status != http.StatusConflict || out.Outcome != actionOutcomeRefused {
		t.Fatalf("status=%d outcome=%q reason=%q, want 409/refused: the file at that path is not the one the button described", status, out.Outcome, out.Reason)
	}
	if _, err := os.Lstat(dupPath); err != nil {
		t.Errorf("the replaced file must still be where it was: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, trashDirName)); err == nil {
		entries, _ := os.ReadDir(filepath.Join(root, trashDirName))
		for _, e := range entries {
			t.Errorf("nothing may have been trashed; found %s", e.Name())
		}
	}
}

// TestAction_Trash_RefusesAFindingWhoseSweptSizeIsUnknown closes the other half
// of the same gate: a finding the sweep recorded with no size at all cannot be
// compared to anything, and "cannot compare" must refuse rather than skip the
// comparison — skipping is what made the gate optional in the first place.
func TestAction_Trash_RefusesAFindingWhoseSweptSizeIsUnknown(t *testing.T) {
	fake := newArrFake(t)
	_, dupPath, inst := radarrFileFixture(t, fake)
	cfg := Config{DryRun: false, GUIActions: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, _ := newActionFixture(t, cfg, inst)
	seedFileFinding(store, inst, fileReportFindingRecord{Kind: fileKindDuplicate, Path: dupPath, Display: "d", Size: 0})

	status, out := postAction(t, ts, `{"kind":"trash","confirm":true,"instance":"radarr-main","path":`+jsonString(dupPath)+`,"finding":"duplicate"}`)
	if status != http.StatusConflict || out.Outcome != actionOutcomeRefused {
		t.Fatalf("status=%d outcome=%q reason=%q, want 409/refused", status, out.Outcome, out.Reason)
	}
	if _, err := os.Lstat(dupPath); err != nil {
		t.Errorf("nothing may have moved: %v", err)
	}
}

// TestAction_Trash_AFailedFileActionIsLoggedAtError is the round-2 IMPORTANT
// on rule 9's level. `failed` exists to mean "cutoffarr tried and something
// outside it broke — go look at your server", and for FILE actions the audit
// line is the ONLY log the failure produces. At INFO it is invisible on a
// deployment whose operator watches WARN and above — including the worst state
// this phase can reach, a case-twin merge that renamed N files and then errored
// and is deliberately not rolled back.
func TestAction_Trash_AFailedFileActionIsLoggedAtError(t *testing.T) {
	fake := newArrFake(t)
	root, dupPath, inst := radarrFileFixture(t, fake)
	cfg := Config{DryRun: false, GUIActions: true, ExclusionTag: "cutoffarr-exclude"}
	runner, ts, store, buf := newActionFixture(t, cfg, inst)
	seedFileFinding(store, inst, fileReportFindingRecord{Kind: fileKindDuplicate, Path: dupPath, Display: "d", Size: 10})

	// A real failure AFTER every gate has passed: the stamp directory is
	// pinned, and a plain FILE is put where the mirrored destination's parent
	// directory has to be created, so probeRootWritable still succeeds (it only
	// creates .cutoffarr-trash itself) and the move fails on ENOTDIR.
	stamp := time.Date(2026, 8, 13, 4, 5, 6, 0, time.UTC)
	runner.now = func() time.Time { return stamp }
	stampDir := filepath.Join(root, trashDirName, stamp.Format(trashStampLayout))
	if err := os.MkdirAll(stampDir, 0o755); err != nil {
		t.Fatalf("seeding the obstruction: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stampDir, "Some Movie (2019)"), []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("seeding the obstruction: %v", err)
	}

	status, out := postAction(t, ts, `{"kind":"trash","confirm":true,"instance":"radarr-main","path":`+jsonString(dupPath)+`,"finding":"duplicate","size":10}`)
	if out.Outcome != actionOutcomeFailed {
		t.Fatalf("status=%d outcome=%q reason=%q, want failed: the mirrored destination's parent cannot be created", status, out.Outcome, out.Reason)
	}
	log := buf.String()
	if !strings.Contains(log, "level=ERROR msg=action") {
		t.Errorf("a failed action must be greppable at ERROR, the level operators actually watch; log:\n%s", log)
	}
	for _, want := range []string{"source=gui", "kind=trash", "outcome=failed"} {
		if !strings.Contains(log, want) {
			t.Errorf("the failure line lost %q from the single msg=action attr vocabulary; log:\n%s", want, log)
		}
	}
}

// --- the merge action's live-data gates -------------------------------------
//
// These use the fileReportDirLister seam (filereport.go) rather than real twin
// directories, so they run on this project's case-INSENSITIVE dev host as well
// as on the case-sensitive CI runner. Only the TRACKED spelling needs to exist
// on disk — the file report's own mount heuristic requires the tracked files it
// knows about to be findable — and none of these tests reaches a move.

// twinListerFixture builds a Sonarr instance whose root holds the real,
// lowercase, tracked spelling of each named show, with the differently-cased
// twin supplied by a synthesized directory listing. names is the listing the
// root reports, in order; tracked names the shows the *arr actually tracks.
func twinListerFixture(t *testing.T, fake *arrFake, names []string, tracked []string) (root string, inst Instance) {
	t.Helper()
	root = t.TempDir()

	var series, files []string
	for i, show := range tracked {
		writeActionFile(t, root, show+"/Season 01/tracked.mkv", "tracked "+show)
		id := 3 + i
		series = append(series, fmt.Sprintf(`{"id":%d,"title":%q,"monitored":true,"tags":[],"path":"/tv/%s","seasons":[]}`, id, show, show))
		files = append(files, fmt.Sprintf(`{"id":%d,"seriesId":%d,"seasonNumber":1,"path":"/tv/%s/Season 01/tracked.mkv"}`, 30+i, id, show))
		fake.episodeFiles[fmt.Sprint(id)] = "[" + files[i] + "]"
	}
	fake.series = "[" + strings.Join(series, ",") + "]"

	entries := make([]fs.DirEntry, 0, len(names))
	for _, n := range names {
		entries = append(entries, fakeDir(n))
	}
	withFakeDirLister(t, map[string][]fs.DirEntry{root: entries})

	inst = Instance{Name: "sonarr-main", Type: "sonarr", URL: fake.srv.URL, APIKey: "k",
		MediaRootMap: map[string]string{"/tv": root}}
	return root, inst
}

func twinFinding(path string, names ...caseCollisionNameRecord) fileReportFindingRecord {
	return fileReportFindingRecord{
		Kind: fileKindCaseCollision, Path: path, Display: filepath.Base(path),
		EntryType: fileReportEntryTypeDir, Names: names,
	}
}

func seedTwinFindings(store *statsStore, inst Instance, findings ...fileReportFindingRecord) {
	store.recordInstance(cycleKindSweep, time.Now(), inst.Name, inst.Type, cycleInstanceStats{
		decisionsRan: true, fileReportRan: true,
		fileReport: fileReportSnapshot{Status: "ran", CaseCollisions: len(findings), Findings: findings},
	})
}

// TestAction_MergeCaseTwin_ADirectoryWithTwoTwinPairsActsOnTheOneTheButtonNamed
// is the round-2 IMPORTANT: a case-twin finding was looked up by its CONTAINING
// DIRECTORY path alone, but caseCollisionsInDir emits one finding per collision
// GROUP, all sharing that same directory path. A TV root holding two twin pairs
// — every series folder is a sibling in one root, so this is the ordinary case,
// not an exotic one — therefore resolved both gates to whichever group the walk
// listed first, and the second twin's button could never do anything but refuse
// at gate 3 with "sonarr-main now tracks X and not Y": the operator told the
// world changed when it had not, and that twin unmergeable forever.
//
// Dry run, so the assertion is about which twin the executor RESOLVED rather
// than about a filesystem a case-insensitive host cannot hold.
func TestAction_MergeCaseTwin_ADirectoryWithTwoTwinPairsActsOnTheOneTheButtonNamed(t *testing.T) {
	fake := newArrFake(t)
	root, inst := twinListerFixture(t, fake, []string{"SHOW", "show", "Foo", "foo"}, []string{"show", "foo"})
	cfg := Config{DryRun: true, GUIActions: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, buf := newActionFixture(t, cfg, inst)
	seedTwinFindings(store, inst,
		twinFinding(root, caseCollisionNameRecord{Name: "SHOW"}, caseCollisionNameRecord{Name: "show", Tracked: true}),
		twinFinding(root, caseCollisionNameRecord{Name: "Foo"}, caseCollisionNameRecord{Name: "foo", Tracked: true}),
	)

	status, out := postAction(t, ts,
		`{"kind":"merge-case-twin","confirm":true,"instance":"sonarr-main","path":`+jsonString(root)+`,"tracked":"foo","untracked":"Foo"}`)
	if status != http.StatusOK || out.Outcome != actionOutcomeRehearsed {
		t.Fatalf("status=%d outcome=%q reason=%q, want 200/rehearsed for the SECOND twin in this directory\nlog:\n%s",
			status, out.Outcome, out.Reason, buf.String())
	}
	if !strings.Contains(out.Message, filepath.Join(root, "Foo")) || !strings.Contains(out.Message, filepath.Join(root, "foo")) {
		t.Errorf("message = %q, want it to name the twin the button described", out.Message)
	}
	if strings.Contains(out.Message, "SHOW") || strings.Contains(out.Message, "show") {
		t.Errorf("message = %q, want nothing about the OTHER twin pair in the same directory", out.Message)
	}
}

// TestAction_MergeCaseTwin_TwinAlreadyMergedByHandIsRefusedAtTheLiveGate is
// gate 2, which the brief names verbatim ("twin already merged") and which no
// test reached: the stored snapshot still carries the finding, a fresh
// re-derivation no longer produces one, and the merge must refuse rather than
// "merge" a directory that is now the only spelling there is.
func TestAction_MergeCaseTwin_TwinAlreadyMergedByHandIsRefusedAtTheLiveGate(t *testing.T) {
	fake := newArrFake(t)
	// The live listing holds ONE spelling: somebody merged the twin by hand.
	root, inst := twinListerFixture(t, fake, []string{"show"}, []string{"show"})
	cfg := Config{DryRun: false, GUIActions: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, buf := newActionFixture(t, cfg, inst)
	seedTwinFindings(store, inst,
		twinFinding(root, caseCollisionNameRecord{Name: "SHOW"}, caseCollisionNameRecord{Name: "show", Tracked: true}))

	status, out := postAction(t, ts,
		`{"kind":"merge-case-twin","confirm":true,"instance":"sonarr-main","path":`+jsonString(root)+`,"tracked":"show","untracked":"SHOW"}`)
	if status != http.StatusConflict || out.Outcome != actionOutcomeRefused {
		t.Fatalf("status=%d outcome=%q reason=%q, want 409/refused\nlog:\n%s", status, out.Outcome, out.Reason, buf.String())
	}
	if !strings.Contains(out.Reason, "no longer holds a case-twin") {
		t.Errorf("reason = %q, want gate 2's own reason", out.Reason)
	}
	if body, err := os.ReadFile(filepath.Join(root, "show", "Season 01", "tracked.mkv")); err != nil || string(body) != "tracked show" {
		t.Errorf("nothing may have moved: contents=%q err=%v", body, err)
	}
	assertTrashHoldsNothing(t, root)
}

// TestAction_MergeCaseTwin_ATrackedSideSwapIsRefusedQuotingBothSpellings is
// gate 3, and it is the one thing standing between a stale dashboard tab and a
// merge that moves the library AWAY from the spelling the *arr is now looking
// at — the largest blast radius in this phase, and it had no assertion behind
// it. Here the sweep saw "show" tracked; the *arr now tracks "SHOW".
func TestAction_MergeCaseTwin_ATrackedSideSwapIsRefusedQuotingBothSpellings(t *testing.T) {
	fake := newArrFake(t)
	root, inst := twinListerFixture(t, fake, []string{"SHOW", "show"}, []string{"SHOW"})
	cfg := Config{DryRun: false, GUIActions: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, buf := newActionFixture(t, cfg, inst)
	// What the sweep reported, and what the button therefore promised.
	seedTwinFindings(store, inst,
		twinFinding(root, caseCollisionNameRecord{Name: "SHOW"}, caseCollisionNameRecord{Name: "show", Tracked: true}))

	status, out := postAction(t, ts,
		`{"kind":"merge-case-twin","confirm":true,"instance":"sonarr-main","path":`+jsonString(root)+`,"tracked":"show","untracked":"SHOW"}`)
	if status != http.StatusConflict || out.Outcome != actionOutcomeRefused {
		t.Fatalf("status=%d outcome=%q reason=%q, want 409/refused\nlog:\n%s", status, out.Outcome, out.Reason, buf.String())
	}
	if !strings.Contains(out.Reason, `"SHOW"`) || !strings.Contains(out.Reason, `"show"`) {
		t.Errorf("reason = %q, want both spellings quoted so the operator can see which way round the *arr now has it", out.Reason)
	}
	if body, err := os.ReadFile(filepath.Join(root, "SHOW", "Season 01", "tracked.mkv")); err != nil || string(body) != "tracked SHOW" {
		t.Errorf("nothing may have moved AWAY from where the *arr is now looking: contents=%q err=%v", body, err)
	}
	assertTrashHoldsNothing(t, root)
}

// TestAction_MergeCaseTwin_DryRunRehearsesAndMovesNothing is the merge's
// rehearsal branch, which had no test at all: both merge posts in the suite ran
// with DryRun:false. It is the one rehearsal whose regression is destructive —
// drop or invert the `if a.cfg.DryRun` guard and a rehearsal performs a real
// multi-file merge plus two trash-moves while answering "Rehearsed only", on a
// deployment whose operator believes nothing can move.
func TestAction_MergeCaseTwin_DryRunRehearsesAndMovesNothing(t *testing.T) {
	fake := newArrFake(t)
	root, inst := twinListerFixture(t, fake, []string{"SHOW", "show"}, []string{"show"})
	writeActionFile(t, root, "show/Season 01/second.mkv", "second")
	cfg := Config{DryRun: true, GUIActions: true, ExclusionTag: "cutoffarr-exclude"}
	_, ts, store, buf := newActionFixture(t, cfg, inst)
	seedTwinFindings(store, inst,
		twinFinding(root, caseCollisionNameRecord{Name: "SHOW"}, caseCollisionNameRecord{Name: "show", Tracked: true}))

	status, out := postAction(t, ts,
		`{"kind":"merge-case-twin","confirm":true,"instance":"sonarr-main","path":`+jsonString(root)+`,"tracked":"show","untracked":"SHOW"}`)
	if status != http.StatusOK || out.Outcome != actionOutcomeRehearsed {
		t.Fatalf("status=%d outcome=%q reason=%q, want 200/rehearsed\nlog:\n%s", status, out.Outcome, out.Reason, buf.String())
	}
	for name, want := range map[string]string{"tracked.mkv": "tracked show", "second.mkv": "second"} {
		if body, err := os.ReadFile(filepath.Join(root, "show", "Season 01", name)); err != nil || string(body) != want {
			t.Errorf("%s must be untouched by a rehearsal: contents=%q err=%v", name, body, err)
		}
	}
	assertTrashHoldsNothing(t, root)
	if !strings.Contains(buf.String(), "outcome=rehearsed") {
		t.Errorf("the rehearsal must be audited; log:\n%s", buf.String())
	}
}

// assertTrashHoldsNothing asserts that the only thing under root's trash is the
// empty directory the writability probe itself creates — the one filesystem
// effect a rehearsal or a refusal is allowed to have (see probeRootWritable).
func assertTrashHoldsNothing(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, trashDirName))
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatalf("reading the trash: %v", err)
	}
	for _, e := range entries {
		t.Errorf("the trash holds %q; nothing may have been moved", e.Name())
	}
}

// TestTree_BothEnginesGuardTheirForwardWritePassWithTheScopeSuppression is the
// Sonarr half of the CRITICAL fix, pinned at the source rather than through a
// fixture: the Radarr behaviour is asserted end to end above, and this is what
// stops the same defect being reintroduced on the season side, where composing
// the equivalent "monitored, complete on disk, cutoff met" library takes more
// fixture than the guarantee is legible in.
//
// The guarantee is one sentence: neither engine may compose a forward write on
// a scope that forbids them. Both write passes are called exactly once each,
// and each call must sit inside that guard.
func TestTree_BothEnginesGuardTheirForwardWritePassWithTheScopeSuppression(t *testing.T) {
	if !actionScope(1, nil).suppressesForwardWrites() {
		t.Fatal("a GUI action's scope does not suppress forward writes: a Re-monitor click can perform the inverse of the operation it names")
	}
	src := readRepoFileForActions(t, "decision.go")
	for _, call := range []string{"= runWritePass(ctx,", "= runSonarrWritePass(ctx,"} {
		if n := strings.Count(src, call); n != 1 {
			t.Fatalf("%q is called %d times in decision.go; this pin assumes exactly one call site per engine", call, n)
		}
		before := src[:strings.Index(src, call)]
		if len(before) > 1200 {
			before = before[len(before)-1200:]
		}
		if !strings.Contains(before, "scope.suppressesForwardWrites()") {
			t.Errorf("the %q call site is not guarded by scope.suppressesForwardWrites(): a human-clicked action would run the FORWARD write pass over the item it was asked to re-monitor", call)
		}
	}
}
