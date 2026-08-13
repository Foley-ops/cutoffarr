package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// actions.go is v2.2: the human-clicked action system.
//
// THE ONE THING TO UNDERSTAND BEFORE READING ANY OF IT. This project's
// no-file-writes rule is permanent and it has not been weakened. What the
// owner ruled (plan §7, Phase 11, 2026-08-13) is that the rule binds the
// AUTOMATION: "forbidden for the automation to do, not for the human". A
// human who reads a button that states its exact operation, confirms it, and
// clicks it is the one acting; cutoffarr is the hand. So:
//
//	Sweeps, webhooks, reconciliation, the startup scan and every other
//	autonomous code path in this program remain filesystem-read-only and
//	*arr-write-limited exactly as they were, with gui_actions true or false.
//
// That is pinned structurally, not by intention: every executor in this file
// is called from exactly one place — actionRunner.run, which is called from
// exactly one place — the POST /api/action handler (webui.go). See
// TestTree_ExecutorsAreReachableOnlyFromTheActionEndpoint (actions_test.go),
// which greps the tree and fails if any other file so much as names one.
//
// THE CONSTITUTION OF AN ACTION, in the order the code applies it:
//
//  1. Only a human POST reaches here (above).
//  2. The button states its exact operation and the response echoes that same
//     operation string back, so the page can prove to the operator that what
//     was performed is what was described (actionResponse.Operation).
//  3. Fresh re-verification: every executor re-derives its finding from LIVE
//     data — re-running the file report against a fresh library read, or
//     re-listing the directory, or re-fetching the *arr object — and refuses
//     if the world no longer matches what the button promised. Refusals are
//     honest HTTP responses, never silent no-ops.
//  4. Trash, never delete. os.Remove and os.RemoveAll do not appear in this
//     file and are banned tree-wide, including here (see the amended
//     fsMutationAllowlist, filereport_test.go). Every removal is a
//     rename-move into <root>/.cutoffarr-trash/<stamp>/<original-rel-path>,
//     never overwriting anything already there, never auto-pruned, restored
//     by hand.
//  7. gui_actions and dry_run are both binding and are reported separately:
//     the page never has to guess which switch was missing.
//  8. File executors probe the affected root's writability at action time, by
//     attempting the trash-directory create itself, so a :ro mount produces a
//     refusal naming the mount and the fix rather than a bare EPERM.
//  9. Every action — rehearsed, performed or refused — logs one
//     msg=action source=gui line with the full paths/ids and the outcome.
//
// (5, 6 and 10 are the merge, re-monitor and audit rules; they live at their
// own functions below.)

// --- the trash --------------------------------------------------------------

// trashDirName is the single directory every trashed file lands under, at the
// top of the media root it came from. Dot-prefixed for two reasons that are
// both load-bearing: the *arrs' own scanners ignore dot-directories, and this
// project's file report already prunes dot-directories from its walk the same
// way it prunes the Plex extras folders, so trashed files can never come back
// as a second generation of duplicate/orphan findings.
//
// It is never auto-pruned. Nothing in this program ever deletes anything from
// it, on any schedule, under any flag: "restore by hand, empty it by hand" is
// the whole contract, and it is documented in the README's own "Acting on
// findings" section.
const trashDirName = ".cutoffarr-trash"

// trashStampLayout is the per-action subdirectory name: one RFC3339 UTC
// second-resolution stamp per action, so everything one click moved is
// together and a human reading the directory listing can see WHEN without
// reading a log. Second resolution is deliberate — it is a name a person has
// to type when restoring — and the collision it admits (two actions in the
// same second) is handled where it must be anyway: uniquifyTrashPath, which
// also covers the far more likely case of the same relative path being
// trashed twice.
const trashStampLayout = "2006-01-02T15:04:05Z"

// trashDestination computes where src lands when trashed out of root. It is
// pure: it touches no filesystem and creates nothing.
//
// Two refusals, both containment guards rather than conveniences:
//
//   - src must actually live under root. The endpoint takes a path from an
//     HTTP request, and although that path is separately required to match a
//     current finding, a mirrored destination built from a "../.."-laden
//     relative path would put the rename somewhere nobody asked for. This
//     function refuses to compute such a destination at all.
//   - src must not already be inside the trash. Trashing something out of the
//     trash would nest a second stamp directory inside the first and make the
//     mirrored restore path a lie.
func trashDestination(root, src string, stamp time.Time) (string, error) {
	root = filepath.Clean(root)
	src = filepath.Clean(src)

	rel, err := filepath.Rel(root, src)
	if err != nil {
		return "", fmt.Errorf("%s is not expressible relative to the media root %s: %w", src, root, err)
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is not inside the media root %s, so it has no place in that root's trash", src, root)
	}
	first, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
	if first == trashDirName {
		return "", fmt.Errorf("%s is already inside %s; the trash is never trashed into itself", src, trashDirName)
	}

	return filepath.Join(root, trashDirName, stamp.UTC().Format(trashStampLayout), rel), nil
}

// maxTrashUniquifyAttempts bounds uniquifyTrashPath's search. A trash
// directory holding a thousand copies of one relative path is not a case to
// keep grinding on: it is a signal something is very wrong, and an honest
// error beats an unbounded loop against a filesystem.
const maxTrashUniquifyAttempts = 1000

// uniquifyTrashPath returns dest if nothing occupies it, or the first
// "name (2).ext", "name (3).ext" … variant that is free.
//
// This is rule 4's "never overwrite in trash" in executable form, and it is
// the single most important line of defense in this file: os.Rename on Unix
// SILENTLY replaces an existing destination file. Without this, two trashes
// of the same relative path within the same stamp second would destroy the
// first file permanently — in the one directory whose entire purpose is that
// nothing is ever destroyed.
//
// KNOWN AND ACCEPTED RACE, stated rather than papered over: this is a
// check-then-rename, so a *different process* creating the chosen destination
// in the window between the Lstat and the os.Rename would still be
// overwritten. It is not closeable with the primitives this file is allowed
// to use (os.Link + os.Remove would close it, and os.Remove is banned here
// for a stronger reason than this race is worth). It is bounded to nothing in
// practice: actions are single-flight (actionRunner.mu), the destination is
// under a per-action timestamped directory nothing else writes to, and the
// only other writer would be a human manually creating files inside
// .cutoffarr-trash while clicking a trash button.
func uniquifyTrashPath(dest string) (string, error) {
	if _, err := os.Lstat(dest); err != nil {
		if os.IsNotExist(err) {
			return dest, nil
		}
		return "", fmt.Errorf("checking whether the trash destination %s is free: %w", dest, err)
	}

	ext := filepath.Ext(dest)
	stem := strings.TrimSuffix(dest, ext)
	for n := 2; n < maxTrashUniquifyAttempts; n++ {
		candidate := fmt.Sprintf("%s (%d)%s", stem, n, ext)
		if _, err := os.Lstat(candidate); err != nil {
			if os.IsNotExist(err) {
				return candidate, nil
			}
			return "", fmt.Errorf("checking whether the trash destination %s is free: %w", candidate, err)
		}
	}
	return "", fmt.Errorf("%s and %d numbered variants of it are all already in the trash; refusing to guess further", dest, maxTrashUniquifyAttempts-2)
}

// probeRootWritable is rule 8: the read-only-filesystem self-detection, run at
// ACTION time against the root the action would touch.
//
// The probe IS the real operation's first step — creating the trash directory
// — rather than a separate, weaker test (a temp file elsewhere, a stat of the
// mount table) that could succeed where the actual work would fail. If this
// succeeds, the trash directory now exists and the move that follows has one
// less thing that can go wrong; if it fails, nothing has been touched.
//
// The error names the root and states the fix, because on the DEFAULT deploy
// this is not an exotic failure: docker-compose.example.yml mounts media :ro
// deliberately, so this refusal is what every file button answers until an
// operator makes the informed choice to remount rw (README, "Acting on
// findings"). A bare "permission denied" from six frames down would leave
// them guessing at a decision the deployment made on purpose.
func probeRootWritable(root string) error {
	trashRoot := filepath.Join(filepath.Clean(root), trashDirName)
	if err := os.MkdirAll(trashRoot, 0o755); err != nil {
		return fmt.Errorf("the media root %s is not writable by cutoffarr (could not create %s: %v) — this is what a read-only :ro mount looks like, and it is the deploy default: either remount that volume read-write for cutoffarr, or perform this operation on the server yourself", root, trashRoot, err)
	}
	return nil
}

// moveToTrash performs one trash move: probe the root, compute the mirrored
// destination, uniquify it, create its parents, rename. It returns the path
// the file now lives at, which the caller logs and hands back to the page so
// a human can find it again.
//
// os.Rename, not a copy-then-delete: a rename is atomic within a filesystem,
// leaves no half-copied file behind if it fails, and — the reason it matters
// most here — never touches the file's contents at all. Its one limitation is
// that it cannot cross filesystems (EXDEV), which is exactly right: the trash
// lives under the same media root as the file, so a cross-device error means
// the root itself is a mount point stack this action should not be silently
// working around.
func moveToTrash(root, src string, stamp time.Time) (string, error) {
	if err := probeRootWritable(root); err != nil {
		return "", err
	}
	dest, err := trashDestination(root, src, stamp)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", fmt.Errorf("creating the trash directory %s: %w", filepath.Dir(dest), err)
	}
	dest, err = uniquifyTrashPath(dest)
	if err != nil {
		return "", err
	}
	if err := os.Rename(src, dest); err != nil {
		return "", fmt.Errorf("moving %s to %s: %w", src, dest, err)
	}
	return dest, nil
}

// dirIsEmptyOfFiles reports whether p contains no regular files anywhere
// beneath it — the post-condition a case-twin merge has to establish before
// the emptied source directory is itself trashed. Empty SUBDIRECTORIES are
// tolerated (they carry nothing), which is why this is not a bare ReadDir
// length check.
func dirIsEmptyOfFiles(p string) (bool, error) {
	empty := true
	err := filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			empty = false
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return empty, nil
}
