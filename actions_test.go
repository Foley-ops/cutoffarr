package main

import (
	"context"
	"encoding/json"
	"errors"
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
	movies string // JSON array for GET /api/v3/movie
	series string
	// wanted maps the monitored filter value ("", "false") to a wanted/cutoff
	// page body.
	wanted       map[string]string
	episodeFiles map[string]string
	movieByID    map[string]string
	movieFiles   map[string]string
}

func newArrFake(t *testing.T) *arrFake {
	t.Helper()
	f := &arrFake{
		movies: "[]", series: "[]",
		wanted:       map[string]string{},
		episodeFiles: map[string]string{},
		movieByID:    map[string]string{},
		movieFiles:   map[string]string{},
	}
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
	mux.HandleFunc("/api/v3/system/status", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"version":"5.0.0.0"}`)
	})
	mux.HandleFunc("/api/v3/qualityprofile", func(w http.ResponseWriter, r *http.Request) {
		write(w, `[{"id":1,"name":"HD","cutoff":2,"items":[{"quality":{"id":1,"name":"720p"},"allowed":true},{"quality":{"id":2,"name":"1080p"},"allowed":true}],"upgradeAllowed":true,"cutoffFormatScore":0}]`)
	})
	mux.HandleFunc("/api/v3/tag", func(w http.ResponseWriter, r *http.Request) { write(w, `[]`) })
	mux.HandleFunc("/api/v3/movie", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		body := f.movies
		f.mu.Unlock()
		write(w, body)
	})
	mux.HandleFunc("/api/v3/movie/", func(w http.ResponseWriter, r *http.Request) {
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
	})
	mux.HandleFunc("/api/v3/moviefile", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		body, ok := f.movieFiles[r.URL.Query().Get("movieId")]
		f.mu.Unlock()
		if !ok {
			body = `[]`
		}
		write(w, body)
	})
	mux.HandleFunc("/api/v3/wanted/cutoff", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		body, ok := f.wanted[r.URL.Query().Get("monitored")]
		f.mu.Unlock()
		if !ok {
			body = `{"page":1,"pageSize":1000,"totalRecords":0,"records":[]}`
		}
		write(w, body)
	})
	mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		body := f.series
		f.mu.Unlock()
		write(w, body)
	})
	mux.HandleFunc("/api/v3/episodefile", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		body, ok := f.episodeFiles[r.URL.Query().Get("seriesId")]
		f.mu.Unlock()
		if !ok {
			body = `[]`
		}
		write(w, body)
	})
	mux.HandleFunc("/api/v3/episode", func(w http.ResponseWriter, r *http.Request) { write(w, `[]`) })
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
	_, ts, store, _ := newActionFixture(t, cfg, inst)
	seedFileFinding(store, inst, fileReportFindingRecord{Kind: fileKindDuplicate, Path: dupPath, Display: "d", Size: 10})

	status, out := postAction(t, ts, `{"kind":"trash","confirm":true,"instance":"radarr-main","path":`+jsonString(dupPath)+`,"finding":"duplicate","size":10}`)
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
	_, ts, store, _ := newActionFixture(t, cfg, inst)
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

// TestTree_ExecutorsAreReachableOnlyFromTheActionEndpoint is the pin that keeps
// the owner's ruling true no matter what any later phase does: the ruling
// permits a HUMAN to act, and it is only honest if no autonomous code path can
// reach an executor at all.
//
// The check is structural rather than aspirational: the executor entry points
// are named, and no non-test file other than actions.go itself and the one HTTP
// handler file may so much as mention them.
func TestTree_ExecutorsAreReachableOnlyFromTheActionEndpoint(t *testing.T) {
	executors := []string{"moveToTrash(", "mergeCaseTwinDir(", "probeRootWritable(", ".run(", "newActionRunner("}
	allowed := map[string]map[string]bool{
		// actions.go declares them all, and calls them from its own executors.
		"actions.go": {"moveToTrash(": true, "mergeCaseTwinDir(": true, "probeRootWritable(": true, ".run(": true, "newActionRunner(": true},
		// webui.go holds the ONE handler, and it may call the runner and
		// nothing else. daemon.go may construct it (wiring is not acting).
		"webui.go":  {".run(": true},
		"daemon.go": {"newActionRunner(": true},
	}
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
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		src, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		body := string(src)
		for _, ex := range executors {
			if strings.Contains(body, ex) && !allowed[name][ex] {
				t.Errorf("%s names %s: the action executors must stay reachable ONLY from the action endpoint handler — a sweep, webhook, reconciliation or startup path that can reach one would make the owner's human-acts ruling false", name, ex)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}
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
