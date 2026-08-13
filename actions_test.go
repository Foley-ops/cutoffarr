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
