package main

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// --- the case-twin merge (rule 5) -------------------------------------------

// twinMergeVerdict is twinMergeEligibility's answer: whether this case-twin
// finding is one a button may be offered for at all, and — when it is not —
// the reason, in words the GUI puts on the row so a human can tell "cutoffarr
// judged this unsafe" apart from "cutoffarr forgot".
type twinMergeVerdict struct {
	ok        bool
	tracked   string
	untracked string
	reason    string
}

// twinMergeEligibility decides rule 5's "which shapes get a button".
//
// A merge is only ever offered for the one shape whose right answer is not a
// judgement call: TWO directory spellings, exactly one of which the *arr
// actually tracks. Then "merge into the tracked one" is not a preference, it
// is the only outcome that makes the library consistent with what the *arr
// already believes.
//
// Every other shape is report-only, with its reason shown:
//
//   - No tracked side. Nothing establishes which spelling is "the real one",
//     and picking would be cutoffarr inventing an opinion about a human's
//     library. (This is common and benign: two stray folders in an unmanaged
//     corner of a root.)
//   - Both tracked. The *arr has two entries pointing at two spellings, which
//     is a problem in the *arr's own database, not on disk; merging the
//     folders would leave one of those entries pointing at nothing.
//   - Mixed (a folder beside a file). There is no "contents" to merge, and
//     whatever the right fix is, it is a human's call.
//   - File twins. Same: a file has no contents to move into another file, and
//     "which copy wins" is exactly the question cutoffarr must not answer.
//   - More than two spellings. The pairwise "merge A into B" operation the
//     button names does not describe what would happen.
func twinMergeEligibility(f fileReportFindingRecord) twinMergeVerdict {
	if f.Kind != fileKindCaseCollision {
		return twinMergeVerdict{reason: "this finding is not a case-twin"}
	}
	switch f.EntryType {
	case fileReportEntryTypeMixed:
		return twinMergeVerdict{reason: "this twin is a folder and a file sharing one name — there are no folder contents to merge, so the fix is a human's call"}
	case fileReportEntryTypeFile:
		return twinMergeVerdict{reason: "these twins are files, not folders — merging would mean choosing which copy wins, which cutoffarr will not decide for you"}
	}
	if len(f.Names) != 2 {
		return twinMergeVerdict{reason: fmt.Sprintf("this twin has more than two spellings (%d) — the pairwise merge a button could name does not describe what would happen", len(f.Names))}
	}

	var tracked, untracked []string
	for _, n := range f.Names {
		if n.Tracked {
			tracked = append(tracked, n.Name)
		} else {
			untracked = append(untracked, n.Name)
		}
	}
	switch {
	case len(tracked) == 0:
		return twinMergeVerdict{reason: "neither spelling is tracked by this instance, so nothing establishes which one is the real folder — merging would be cutoffarr inventing an opinion about your library"}
	case len(tracked) == 2:
		return twinMergeVerdict{reason: "both spellings are tracked by this instance — that is a problem in the *arr's own database rather than on disk, and merging the folders would leave one of its entries pointing at nothing"}
	}
	return twinMergeVerdict{ok: true, tracked: tracked[0], untracked: untracked[0]}
}

// twinMergeResult is what one merge did, itemized. Every field is reported
// back to the page verbatim: rule 5 requires collisions to be itemized in the
// response, and a human who has just moved files needs to see exactly which
// ones went where without going to the log.
type twinMergeResult struct {
	// moved are the destination paths of files that changed spelling.
	moved []string
	// collided are one human-readable line per file whose destination was
	// already occupied — the source copy went to the trash instead, and the
	// line says so, naming both.
	collided []string
	// trashedDir is where the emptied untracked directory itself ended up.
	trashedDir string
}

// mergeCaseTwinDir performs rule 5's merge: move every file under the
// UNTRACKED spelling into the same relative position under the TRACKED
// spelling, then trash-move the emptied untracked directory.
//
// Per-file rename-moves, never a directory rename of the whole tree: the
// tracked spelling already exists and already has contents (that is what
// makes it the tracked one), so a wholesale rename would either fail or
// clobber it. Walking gives every file its own collision decision, which is
// the only place the "never overwrite" rule can actually be applied.
//
// A destination collision NEVER overwrites. The source file goes to the trash
// instead and the collision is itemized. That choice is deliberate and it is
// the conservative one in both directions: the tracked copy is the one the
// *arr has scored, imported and is serving, so it stays; the untracked copy
// is not destroyed either, it is one directory away in .cutoffarr-trash if
// the human decides it was the better file.
//
// The emptied directory is trash-moved rather than removed for the reason
// this whole file exists: os.Remove does not appear in this codebase. Moving
// it also preserves anything the walk could not account for (an empty
// subdirectory tree, a non-regular entry) instead of silently discarding it.
func mergeCaseTwinDir(root, untrackedDir, trackedDir string, stamp time.Time) (twinMergeResult, error) {
	var res twinMergeResult

	if err := probeRootWritable(root); err != nil {
		return res, err
	}
	for _, d := range []string{untrackedDir, trackedDir} {
		info, err := os.Lstat(d)
		if err != nil {
			return res, fmt.Errorf("%s could not be read, so the merge this button described cannot be performed: %w", d, err)
		}
		if !info.IsDir() {
			return res, fmt.Errorf("%s is not a directory, so there is nothing to merge into or out of", d)
		}
	}

	walkErr := filepath.WalkDir(untrackedDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(untrackedDir, p)
		if err != nil {
			return fmt.Errorf("%s is not expressible relative to %s: %w", p, untrackedDir, err)
		}
		dest := filepath.Join(trackedDir, rel)

		if _, err := os.Lstat(dest); err == nil {
			// Occupied: the tracked spelling wins, and the source copy is
			// trashed rather than destroyed or silently left in place (leaving
			// it would make the "emptied directory" step below a lie).
			trashed, err := moveToTrash(root, p, stamp)
			if err != nil {
				return err
			}
			res.collided = append(res.collided, fmt.Sprintf("%s already existed under the tracked spelling, so the copy under the untracked one was moved to %s instead", rel, trashed))
			return nil
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("checking whether %s is already occupied: %w", dest, err)
		}

		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", filepath.Dir(dest), err)
		}
		if err := os.Rename(p, dest); err != nil {
			return fmt.Errorf("moving %s to %s: %w", p, dest, err)
		}
		res.moved = append(res.moved, dest)
		return nil
	})
	if walkErr != nil {
		// Deliberately NOT rolled back. Every move this walk completed was a
		// correct, individually-verified move into the tracked spelling; undoing
		// them would be a second batch of unverified moves made by a code path
		// that has just proved it is meeting conditions it did not expect. What
		// the caller does instead is report exactly what was done (res is
		// returned alongside the error) so the human sees the partial state
		// rather than discovering it later.
		return res, walkErr
	}

	// The emptied source directory. Checked rather than assumed: if anything
	// is still in there, something happened between the walk and now, and
	// trashing a directory that still holds files would move them out from
	// under the *arr as a side effect of an operation the button described as
	// a merge.
	empty, err := dirIsEmptyOfFiles(untrackedDir)
	if err != nil {
		return res, fmt.Errorf("re-checking that %s is empty before trashing it: %w", untrackedDir, err)
	}
	if !empty {
		return res, fmt.Errorf("%s still holds files after the merge, so it was left exactly where it is rather than being trashed with contents nobody asked to move", untrackedDir)
	}
	trashedDir, err := moveToTrash(root, untrackedDir, stamp)
	if err != nil {
		return res, err
	}
	res.trashedDir = trashedDir
	return res, nil
}

// --- the action vocabulary --------------------------------------------------

// Action kinds, as they appear on the wire and in the audit log's kind= attr.
// ActionRemonitor is deliberately NOT redefined here: it is the same
// "remonitor" the reverse pass already records in lastActions (stats.go), and
// a human-clicked re-monitor and a swept one are the same operation on the
// same object — giving them two names would make the one table that shows both
// harder to read for no gain.
const (
	ActionTrash         = "trash"
	ActionMergeCaseTwin = "merge-case-twin"
)

// Action outcomes. Four of them are the brief's own vocabulary; failed is the
// fifth and it is here for honesty, not convenience.
//
// "Refused" means cutoffarr DECLINED — the finding was stale, the switch
// combination did not authorize it, the mount is read-only. "Failed" means
// cutoffarr TRIED and something else broke: the *arr returned an error, a
// rename hit EXDEV. Collapsing the second into the first would tell an
// operator "we decided not to" about a case where the correct next step is to
// go and look at their server, which is exactly the class of lie the whole
// page is built to avoid.
const (
	actionOutcomePerformed = "performed"
	actionOutcomeRehearsed = "rehearsed"
	actionOutcomeRefused   = "refused"
	actionOutcomeDisabled  = "disabled"
	actionOutcomeFailed    = "failed"
)

// actionRequest is POST /api/action's body: the action kind, the human's
// explicit confirmation, and the identifying fields of the finding the button
// was rendered from.
//
// Confirm is a required, explicitly-true field rather than something inferred
// from the request existing at all. It costs one line and it means a
// hand-rolled curl, a mis-wired fetch, or a future GUI refactor that forgets
// the confirm step cannot perform an action by accident.
//
// The finding fields are deliberately the ones a human can see on screen, not
// an opaque server-side handle: the identifying fields ARE the description of
// the operation, so a request that names a different file than the button
// described is a request the executor's own re-derivation will refuse.
type actionRequest struct {
	Kind     string `json:"kind"`
	Confirm  bool   `json:"confirm"`
	Instance string `json:"instance"`

	// Trash: the finding's full cutoffarr-side disk path, its kind
	// (duplicate|orphan), and the size the button promised.
	Path    string `json:"path"`
	Finding string `json:"finding"`
	Size    int64  `json:"size"`

	// Merge: the containing directory is Path; these are the two spellings.
	Tracked   string `json:"tracked"`
	Untracked string `json:"untracked"`

	// Re-monitor: the item, and (Sonarr) the season.
	ID     int  `json:"id"`
	Season *int `json:"season"`
}

// actionResponse is what the page renders. Operation is echoed back verbatim
// so the row can show the operator that what happened is what the button said
// would happen; Reason carries every refusal, disablement and failure in the
// words the operator should read.
type actionResponse struct {
	Outcome   string   `json:"outcome"`
	Kind      string   `json:"kind"`
	Operation string   `json:"operation,omitempty"`
	Message   string   `json:"message"`
	Reason    string   `json:"reason,omitempty"`
	Items     []string `json:"items,omitempty"`
	Trash     string   `json:"trash,omitempty"`

	// status is the HTTP status this outcome answers with. Unexported so it
	// never reaches the wire twice (once as a status line, once as a field
	// that could disagree with it).
	status int
}

func (r actionResponse) withStatus(code int) actionResponse { r.status = code; return r }

// actionRunner owns everything the executors need and the one lock that keeps
// them from ever running at the same time as each other.
type actionRunner struct {
	// mu is the single-flight executor (endpoint contract): actions are
	// serialized against each other, always. Two file moves interleaving over
	// the same tree — a merge and a trash of something inside it — produce a
	// state neither operation described, and no amount of per-executor
	// re-verification helps, because each one verified a world the other was
	// already changing.
	//
	// It deliberately does NOT serialize against the daemon's own sweeps.
	// Those are read-only on the filesystem, forever, so a sweep can only ever
	// observe a half-finished action, never participate in one; and the
	// re-monitor executor's *arr writes take the same pre-write re-verification
	// discipline the reverse pass's own writes take, which is what makes them
	// safe next to a concurrent read.
	mu sync.Mutex

	cfg    Config
	logger *slog.Logger
	stats  *statsStore
	now    func() time.Time

	// observe is a test seam called inside the lock, used to prove the
	// serialization above actually holds. Nil in production.
	observe func()
}

func newActionRunner(cfg Config, logger *slog.Logger, stats *statsStore) *actionRunner {
	if logger == nil {
		// Self-review finding: a nil logger reached resolveExclusionTagID (and
		// every other read-path helper an executor calls) and panicked on the
		// first posted action. Production always passes a real one, but a test
		// helper or a future embedding constructing a runner without one must
		// get a quiet action rather than a crashed daemon — and the audit line
		// going nowhere is a lesser failure than the process dying, which is
		// why this discards rather than refusing to construct.
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &actionRunner{cfg: cfg, logger: logger, stats: stats, now: time.Now}
}

// actionKinds is the closed set. A kind outside it is a 400 naming the value,
// never a silent no-op and never a best-effort guess.
var actionKinds = map[string]bool{
	ActionTrash:         true,
	ActionMergeCaseTwin: true,
	ActionRemonitor:     true,
}

// switchRefusal answers rule 7's first question: do the config switches
// authorize this kind of action at all? It returns "" for yes, or the reason
// in the operator's own vocabulary — always NAMING the switch, because "the
// page never lies about which switch is missing" and an operator who has
// already set dry_run: false needs to be told it is the other one.
//
// dry_run is not answered here: it does not forbid an action, it turns it into
// a rehearsal, which is a different (and fully verified) outcome.
func (a *actionRunner) switchRefusal(kind string) string {
	if !a.cfg.GUIActions {
		return "gui_actions is false in this cutoffarr's config, so it will not act on findings for you. Set gui_actions: true to enable these buttons; nothing else about this deployment changes."
	}
	if kind == ActionRemonitor && !a.cfg.ReverseScanRemonitor {
		return "reverse_scan_remonitor is false in this cutoffarr's config, so no re-monitor may be written by any path, button or sweep. Set reverse_scan_remonitor: true as well as gui_actions: true to enable this button."
	}
	return ""
}

// instanceByName finds the configured instance a request names.
func (a *actionRunner) instanceByName(name string) (Instance, bool) {
	for _, inst := range a.cfg.Instances {
		if inst.Name == name {
			return inst, true
		}
	}
	return Instance{}, false
}

// run is the ONE entry point to every executor in this file, and the only
// function in it that anything outside this file may call (see this file's
// header and TestTree_ExecutorsAreReachableOnlyFromTheActionEndpoint). Its own
// only caller is the POST /api/action handler.
func (a *actionRunner) run(ctx context.Context, req actionRequest) actionResponse {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.observe != nil {
		a.observe()
	}

	if !actionKinds[req.Kind] {
		return a.refuse(req, actionOutcomeRefused, "", fmt.Sprintf("%q is not an action this cutoffarr knows how to perform", req.Kind)).withStatus(http.StatusBadRequest)
	}
	if !req.Confirm {
		return a.refuse(req, actionOutcomeRefused, "", "this request did not carry confirm: true, and cutoffarr never performs an action a human has not explicitly confirmed").withStatus(http.StatusBadRequest)
	}
	inst, ok := a.instanceByName(req.Instance)
	if !ok {
		return a.refuse(req, actionOutcomeRefused, "", fmt.Sprintf("no instance named %q is configured in this cutoffarr", req.Instance)).withStatus(http.StatusBadRequest)
	}
	if reason := a.switchRefusal(req.Kind); reason != "" {
		return a.refuse(req, actionOutcomeDisabled, "", reason).withStatus(http.StatusForbidden)
	}

	switch req.Kind {
	case ActionTrash:
		return a.trash(ctx, inst, req)
	case ActionMergeCaseTwin:
		return a.mergeTwin(ctx, inst, req)
	default:
		return a.remonitor(ctx, inst, req)
	}
}

// refuse builds a non-success response AND logs its audit line, so no exit
// from an executor can be silent (rule 9). Every caller returns its result
// directly, which is what makes "one refusal, one line" structural rather than
// a convention someone has to remember.
func (a *actionRunner) refuse(req actionRequest, outcome, operation, reason string) actionResponse {
	a.audit(req, outcome, operation, reason)
	return actionResponse{
		Outcome: outcome, Kind: req.Kind, Operation: operation,
		Message: reason, Reason: reason, status: http.StatusConflict,
	}
}

// audit is rule 9: every action — rehearsed, performed or refused — produces
// one line, at INFO, carrying the source, the kind, the outcome, and the full
// paths/ids involved. INFO and not DEBUG because these are the only lines in
// this program that describe something a HUMAN did through cutoffarr, and a
// deployment that has actions switched on wants them in the same place it
// reads its writes.
func (a *actionRunner) audit(req actionRequest, outcome, operation, detail string) {
	if a.logger == nil {
		return
	}
	attrs := []any{
		"source", "gui",
		"kind", req.Kind,
		"outcome", outcome,
		"instance", req.Instance,
	}
	if req.Path != "" {
		attrs = append(attrs, "path", req.Path)
	}
	if req.ID != 0 {
		attrs = append(attrs, "id", req.ID)
	}
	if req.Season != nil {
		attrs = append(attrs, "season", *req.Season)
	}
	if req.Tracked != "" || req.Untracked != "" {
		attrs = append(attrs, "tracked", req.Tracked, "untracked", req.Untracked)
	}
	if operation != "" {
		attrs = append(attrs, "operation", operation)
	}
	if detail != "" {
		attrs = append(attrs, "detail", detail)
	}
	a.logger.Info("action", attrs...)
}

// recordAction folds a performed GUI action into the stats store's
// lastActions, so the same table that shows the daemon's own writes shows what
// a human did through it. Rehearsals and refusals are deliberately NOT
// recorded: lastActions has always meant "writes that actually landed"
// (cycleInstanceStats.actions' own rule), and a rehearsal that appeared there
// would make the one list an operator trusts to say what changed start
// listing things that did not.
func (a *actionRunner) recordAction(inst Instance, rec actionRecord) {
	if a.stats == nil {
		return
	}
	a.stats.recordGUIAction(inst.Name, inst.Type, a.now(), rec)
}

// --- live re-derivation (rule 3, the file half) -----------------------------

// liveFileReport re-runs THIS instance's file report against a fresh library
// read and returns the findings it produces now — not the ones the last sweep
// produced. It is the "re-derives the finding from live data" half of rule 3
// for both file actions.
//
// It reuses runRadarrFileReport / runSonarrFileReport whole rather than
// re-implementing a one-path classification, for the same reason the reverse
// scan reuses the decision function: "is this still a finding" and "is this a
// finding" must be the same question asked of the same code. It also inherits
// every mount-problem heuristic for free, which matters more here than
// anywhere else — acting on a finding produced while a share was half-mounted
// is the one mistake this system cannot take back.
//
// SONARR'S mismatchedSeasons IS PASSED NIL, and that is a real difference from
// a sweep worth stating. That parameter carries the FORWARD engine's own
// episode-file-count-mismatch index, which only a full decision cycle
// computes. buildSonarrTrackedSet independently computes the same mismatch
// from its OWN fresh /episodefile fetch (seasonsWithFileCountMismatch), so the
// protection is not absent here, only single-sourced. The gap that leaves is
// closed from the other side: an action is only ever accepted for a finding a
// COMPLETED sweep already reported (see trash/mergeTwin's snapshot gate), and
// that sweep ran with both sources.
func (a *actionRunner) liveFileReport(ctx context.Context, inst Instance) (fileReportSnapshot, error) {
	client := NewAPIClient(inst.URL, inst.APIKey)
	exclusionTagID, tagActive, ok := resolveExclusionTagID(ctx, a.logger, client, inst, a.cfg.ExclusionTag)
	if !ok {
		return fileReportSnapshot{}, fmt.Errorf("%s's exclusion tag could not be resolved, so its findings cannot be re-derived and nothing will be acted on", inst.Name)
	}

	var counts fileReportCounts
	switch inst.Type {
	case "radarr":
		_, movies, _, movieOK := fetchMovies(ctx, a.logger, client, inst, nil)
		if !movieOK {
			return fileReportSnapshot{}, fmt.Errorf("%s's library could not be read, so its findings cannot be re-derived and nothing will be acted on", inst.Name)
		}
		counts = runRadarrFileReport(ctx, a.logger, slog.LevelDebug, inst, movies, exclusionTagID, tagActive)
	case "sonarr":
		series, seriesOK := fetchSeriesLibrary(ctx, a.logger, client, inst)
		if !seriesOK {
			return fileReportSnapshot{}, fmt.Errorf("%s's library could not be read, so its findings cannot be re-derived and nothing will be acted on", inst.Name)
		}
		counts = runSonarrFileReport(ctx, a.logger, slog.LevelDebug, client, inst, series, nil, exclusionTagID, tagActive)
	default:
		return fileReportSnapshot{}, fmt.Errorf("instance %s has type %q, which has no file report", inst.Name, inst.Type)
	}

	snap := fileReportSnapshotFrom(counts)
	if snap.Status != "ran" {
		return snap, fmt.Errorf("%s's file report did not complete this time (status %s) — a half-mounted share or an unreadable root is exactly when acting on a finding is most dangerous, so nothing was touched", inst.Name, snap.Status)
	}
	return snap, nil
}

// reportedFinding looks one finding up in the LAST COMPLETED SWEEP's snapshot.
//
// This is the authorization gate, and it is what bounds this endpoint's power:
// "path" arrives as a string in an HTTP body, and without this it would be an
// arbitrary filesystem argument handed to a rename by anything that can reach
// the LAN port. With it, the only paths any button can ever name are the ones
// a completed, mount-verified sweep of THIS instance classified as clutter.
func reportedFinding(snap fileReportSnapshot, kind, path string) (fileReportFindingRecord, bool) {
	if snap.Status != "ran" {
		return fileReportFindingRecord{}, false
	}
	for _, f := range snap.Findings {
		if f.Kind == kind && f.Path == path {
			return f, true
		}
	}
	return fileReportFindingRecord{}, false
}

// storedSnapshotFor returns what the stats store currently holds for one
// instance's file report.
func (a *actionRunner) storedSnapshotFor(name string) fileReportSnapshot {
	if a.stats == nil {
		return fileReportSnapshot{}
	}
	for _, v := range a.stats.snapshot().Instances {
		if v.Name == name {
			return v.FileReport
		}
	}
	return fileReportSnapshot{}
}

// rootForPath returns the configured media root p falls under, so a file
// action knows which .cutoffarr-trash it belongs in.
func rootForPath(inst Instance, p string) (string, bool) {
	best := ""
	for _, r := range mediaRootsFor(inst) {
		if hasPathPrefix(cleanArrPath(p), r.diskPath) && len(r.diskPath) > len(best) {
			best = r.diskPath
		}
	}
	return best, best != ""
}

// humanSize renders a byte count the way a button has to say it: short, and
// never more precise than the decision it informs.
//
// math.Round, not fmt's own %.0f, and that is not a style choice. The PAGE
// builds the label the operator reads in the confirm dialog (webui.html's own
// humanSize, using JavaScript's Math.round, which rounds halves AWAY FROM
// ZERO); this builds the operation string the server echoes back into the
// row. fmt's %.0f rounds halves to EVEN, so 2560 bytes read as "3 KB" in the
// dialog and "2 KB" in the answer — the row quietly claiming the server acted
// on a different file than the one confirmed. Same rounding on both sides, or
// the two disagree about the same bytes.
func humanSize(n int64) string {
	switch {
	case n <= 0:
		return "size unknown"
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%d KB", int64(math.Round(float64(n)/1024)))
	case n < 1024*1024*1024:
		return fmt.Sprintf("%d MB", int64(math.Round(float64(n)/(1024*1024))))
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/(1024*1024*1024))
	}
}

// trashOperationText is the exact operation a trash button states — and the
// string the response echoes back so the row can prove they match. Rule 2:
// never a bare "Fix".
func trashOperationText(display string, size int64) string {
	return fmt.Sprintf("Move to trash — %s (%s)", display, humanSize(size))
}

// --- the trash executor -----------------------------------------------------

func (a *actionRunner) trash(ctx context.Context, inst Instance, req actionRequest) actionResponse {
	if req.Finding != fileKindDuplicate && req.Finding != fileKindOrphan {
		return a.refuse(req, actionOutcomeRefused, "", fmt.Sprintf("only duplicate and orphan findings can be trashed; this request named %q", req.Finding)).withStatus(http.StatusBadRequest)
	}

	// Gate 1 — authorization: a completed sweep must have reported exactly
	// this path, for this instance, as this kind of finding.
	reported, ok := reportedFinding(a.storedSnapshotFor(inst.Name), req.Finding, req.Path)
	if !ok {
		return a.refuse(req, actionOutcomeRefused, "",
			fmt.Sprintf("%s is not a %s that %s's last completed sweep reported, so cutoffarr will not move it. Re-run the scan and try again from a fresh row.", req.Path, req.Finding, inst.Name))
	}
	operation := trashOperationText(reported.Display, reported.Size)

	root, ok := rootForPath(inst, req.Path)
	if !ok {
		return a.refuse(req, actionOutcomeRefused, operation, fmt.Sprintf("%s does not fall under any media root configured for %s, so there is no trash for it to go to", req.Path, inst.Name))
	}

	// Rule 8, at action time — and in a rehearsal too. A rehearsal that says
	// "would move" on a read-only mount is a lie, and writability cannot be
	// established without attempting it. The one filesystem effect a rehearsal
	// therefore has is creating an EMPTY .cutoffarr-trash directory: nothing
	// is moved, nothing is destroyed, and the operator gets a true answer
	// instead of a hopeful one. (That directory is dot-prefixed, so the file
	// report's own walk prunes it and it can never become a finding.)
	//
	// It runs HERE — after the in-memory authorization gate, before the
	// expensive live re-derivation — and the order is a deliberate,
	// self-review correction. A read-only media mount is the DEFAULT
	// deployment, so "this button refuses" is the ordinary steady state, and
	// paying for a full library read (a per-series /episodefile fetch across
	// the whole library, on Sonarr) to reach an answer a directory create
	// already settles is a cost per click for nothing. Gate 1 above still
	// runs first, so an arbitrary path can never so much as cause a probe.
	if err := probeRootWritable(root); err != nil {
		return a.refuse(req, actionOutcomeRefused, operation, err.Error())
	}

	// Gate 2 — fresh re-derivation: the finding must still be produced by a
	// file report run against live data, right now.
	live, err := a.liveFileReport(ctx, inst)
	if err != nil {
		return a.refuse(req, actionOutcomeRefused, operation, err.Error())
	}
	fresh, ok := reportedFinding(live, req.Finding, req.Path)
	if !ok {
		return a.refuse(req, actionOutcomeRefused, operation,
			fmt.Sprintf("%s is no longer a %s as of a fresh check of %s — the *arr may have imported it, or it may already be gone. Nothing was moved.", req.Path, req.Finding, inst.Name))
	}

	// Gate 3 — the promise the button made. A file REPLACED at the same path
	// passes every existence check there is; the size is what catches it.
	if req.Size > 0 && fresh.Size != req.Size {
		return a.refuse(req, actionOutcomeRefused, operation,
			fmt.Sprintf("the file at %s is now %s, not the %s the button described — its size changed, so it is not the file you confirmed. Nothing was moved.", req.Path, humanSize(fresh.Size), humanSize(req.Size)))
	}

	info, err := os.Lstat(req.Path)
	if err != nil {
		return a.refuse(req, actionOutcomeRefused, operation, fmt.Sprintf("%s could not be read just now, so nothing was moved: %v", req.Path, err))
	}
	if !info.Mode().IsRegular() {
		return a.refuse(req, actionOutcomeRefused, operation, fmt.Sprintf("%s is not a regular file, so cutoffarr will not move it", req.Path))
	}

	stamp := a.now()
	if a.cfg.DryRun {
		wouldGo, err := trashDestination(root, req.Path, stamp)
		if err != nil {
			return a.refuse(req, actionOutcomeRefused, operation, err.Error())
		}
		a.audit(req, actionOutcomeRehearsed, operation, "dry_run is on: verified and withheld")
		return actionResponse{
			Outcome: actionOutcomeRehearsed, Kind: req.Kind, Operation: operation,
			Message: fmt.Sprintf("Rehearsed only — dry_run is on. Everything checked out; with dry_run: false this would move the file to %s.", wouldGo),
			Trash:   wouldGo, status: http.StatusOK,
		}
	}

	dest, err := moveToTrash(root, req.Path, stamp)
	if err != nil {
		a.audit(req, actionOutcomeFailed, operation, err.Error())
		return actionResponse{
			Outcome: actionOutcomeFailed, Kind: req.Kind, Operation: operation,
			Message: err.Error(), Reason: err.Error(), status: http.StatusBadGateway,
		}
	}
	a.audit(req, actionOutcomePerformed, operation, "moved to "+dest)
	a.recordAction(inst, actionRecord{Action: ActionTrash, Title: reported.Display, Reason: "moved to " + dest})
	return actionResponse{
		Outcome: actionOutcomePerformed, Kind: req.Kind, Operation: operation,
		Message: "Moved to " + dest + " — nothing was deleted; move it back by hand if this was wrong.",
		Trash:   dest, status: http.StatusOK,
	}
}

// --- the case-twin merge executor -------------------------------------------

// twinOperationText is the exact operation a merge button states (rule 2:
// "Rename 'Squid Game The Challenge' → merge into 'squid game the challenge'
// [tracked]").
func twinOperationText(untracked, tracked string) string {
	return fmt.Sprintf("Merge %q into %q [tracked]", untracked, tracked)
}

func (a *actionRunner) mergeTwin(ctx context.Context, inst Instance, req actionRequest) actionResponse {
	operation := twinOperationText(req.Untracked, req.Tracked)

	// Gate 1 — authorization: the containing directory must be a case-twin
	// finding a completed sweep reported for this instance.
	reported, ok := reportedFinding(a.storedSnapshotFor(inst.Name), fileKindCaseCollision, req.Path)
	if !ok {
		return a.refuse(req, actionOutcomeRefused, operation,
			fmt.Sprintf("%s is not a case-twin that %s's last completed sweep reported, so cutoffarr will not merge anything there. Re-run the scan and try again from a fresh row.", req.Path, inst.Name))
	}
	if el := twinMergeEligibility(reported); !el.ok {
		return a.refuse(req, actionOutcomeRefused, operation, el.reason)
	}

	root, ok := rootForPath(inst, req.Path)
	if !ok {
		return a.refuse(req, actionOutcomeRefused, operation, fmt.Sprintf("%s does not fall under any media root configured for %s, so there is no trash for the emptied folder to go to", req.Path, inst.Name))
	}
	// Rule 8, before the expensive re-derivation — see the trash executor's
	// own comment at the same point for why the order is what it is.
	if err := probeRootWritable(root); err != nil {
		return a.refuse(req, actionOutcomeRefused, operation, err.Error())
	}

	// Gate 2 — fresh re-derivation, and it is the one that catches the case
	// this action most needs to catch: a twin somebody already merged by hand
	// is simply no longer a finding, so it refuses here rather than "merging"
	// a directory that is now the only spelling there is.
	live, err := a.liveFileReport(ctx, inst)
	if err != nil {
		return a.refuse(req, actionOutcomeRefused, operation, err.Error())
	}
	fresh, ok := reportedFinding(live, fileKindCaseCollision, req.Path)
	if !ok {
		return a.refuse(req, actionOutcomeRefused, operation,
			fmt.Sprintf("%s no longer holds a case-twin as of a fresh check of %s — it may already have been merged or renamed. Nothing was moved.", req.Path, inst.Name))
	}
	el := twinMergeEligibility(fresh)
	if !el.ok {
		return a.refuse(req, actionOutcomeRefused, operation, el.reason)
	}

	// Gate 3 — the promise the button made: the two spellings, and which of
	// them is the tracked one, must still be exactly what the operator read.
	// If the *arr has switched which spelling it tracks since the sweep, this
	// merge would move files AWAY from where it is now looking.
	if el.tracked != req.Tracked || el.untracked != req.Untracked {
		return a.refuse(req, actionOutcomeRefused, operation,
			fmt.Sprintf("the twin at %s is no longer the one the button described: %s now tracks %q and not %q. Nothing was moved.", req.Path, inst.Name, el.tracked, req.Tracked))
	}

	untrackedDir := filepath.Join(req.Path, el.untracked)
	trackedDir := filepath.Join(req.Path, el.tracked)

	if a.cfg.DryRun {
		// Rehearsal: everything above ran, and the one thing left — the moves
		// themselves — is exactly what is withheld. The count is stated rather
		// than the file list, because listing files a rehearsal did not
		// examine one by one would be claiming more than was checked.
		a.audit(req, actionOutcomeRehearsed, operation, "dry_run is on: verified and withheld")
		return actionResponse{
			Outcome: actionOutcomeRehearsed, Kind: req.Kind, Operation: operation,
			Message: fmt.Sprintf("Rehearsed only — dry_run is on. Everything checked out; with dry_run: false this would move the contents of %s into %s and then move the emptied folder to the trash.", untrackedDir, trackedDir),
			status:  http.StatusOK,
		}
	}

	res, err := mergeCaseTwinDir(root, untrackedDir, trackedDir, a.now())
	if err != nil {
		detail := err.Error()
		if len(res.moved) > 0 || len(res.collided) > 0 {
			// A partial merge is reported as a partial merge. Rolling back
			// would be a second batch of unverified moves by a code path that
			// has just proved it is meeting conditions it did not expect.
			detail = fmt.Sprintf("%v (%d file(s) had already been moved and were left where they now are)", err, len(res.moved))
		}
		a.audit(req, actionOutcomeFailed, operation, detail)
		return actionResponse{
			Outcome: actionOutcomeFailed, Kind: req.Kind, Operation: operation,
			Message: detail, Reason: detail, Items: res.collided, status: http.StatusBadGateway,
		}
	}

	msg := fmt.Sprintf("Merged %d file(s) into %s; the emptied folder is at %s.", len(res.moved), trackedDir, res.trashedDir)
	if len(res.collided) > 0 {
		msg += fmt.Sprintf(" %d file(s) already existed under the tracked spelling and were moved to the trash instead — listed below.", len(res.collided))
	}
	a.audit(req, actionOutcomePerformed, operation, msg)
	a.recordAction(inst, actionRecord{Action: ActionMergeCaseTwin, Title: reported.Display, Reason: msg})
	return actionResponse{
		Outcome: actionOutcomePerformed, Kind: req.Kind, Operation: operation,
		Message: msg, Items: res.collided, Trash: res.trashedDir, status: http.StatusOK,
	}
}

// --- the re-monitor executor ------------------------------------------------

// remonitorOperationText is the exact operation a re-monitor button states
// (rule 2: "Re-monitor — Fever Pitch (movie 152)").
func remonitorOperationText(title string, id int, season *int, typ string) string {
	noun := "movie"
	if typ == "sonarr" {
		noun = "series"
	}
	if season != nil {
		return fmt.Sprintf("Re-monitor — %s season %d (%s %d)", title, *season, noun, id)
	}
	return fmt.Sprintf("Re-monitor — %s (%s %d)", title, noun, id)
}

// remonitor drives the EXISTING gated reverse write path, scoped to the one
// item the button named. Rule 6, and the emphasis on "existing" is the whole
// design: not one line of new write logic is added here.
//
// What that buys, in the order the reverse path applies it: a fresh library
// read; the forward cross-check computed over that read, and the reverse
// write gate that consults it; the pre-write GET of this exact object; the
// exclusion-tag re-check against those fresh bytes; the already-monitored
// refusal; the re-run of the decision function itself, which refuses the
// write unless the item STILL fails the criteria; and §2.1's dry-run gate
// immediately before the PUT. §2.2's limit — the only write is a monitored
// flag — is structural: this path reaches the same single PUT call site the
// write-verb audit has counted since Phase 8, and adds no new one.
//
// The scope is narrowed to this one id (and, for Sonarr, this one season)
// exactly as --only-id narrows it, so the reverse pass evaluates and writes
// that item and no other. The FORWARD pass of the same cycle is scoped
// identically and is, in practice, a no-op on a re-monitor target: the item is
// unmonitored (that is what made it a finding), and the forward write path
// refuses an already-unmonitored item at its own pre-write check.
//
// The cross-check gate is deliberately NOT bypassed for a human click. It asks
// "did this instance's data agree with itself this cycle", which is a question
// about whether the evidence under the decision can be trusted at all — and a
// human clicking a button has confirmed WHICH item to act on, not that the
// instance's data is sound. When it blocks, the reason is handed back verbatim
// (reverseCounts.withheldReason) so the page states it rather than leaving the
// operator with a button that appears to do nothing.
func (a *actionRunner) remonitor(ctx context.Context, inst Instance, req actionRequest) actionResponse {
	if req.ID == 0 {
		return a.refuse(req, actionOutcomeRefused, "", "this request named no item id, so there is nothing to re-monitor").withStatus(http.StatusBadRequest)
	}
	if (inst.Type == "sonarr") != (req.Season != nil) {
		return a.refuse(req, actionOutcomeRefused, "",
			fmt.Sprintf("%s is a %s instance, so this request's season field is the wrong shape for it (a Radarr movie has no season; a Sonarr finding always names one)", inst.Name, inst.Type)).withStatus(http.StatusBadRequest)
	}

	// The title is for the operation text only. It is looked up from the
	// stored findings rather than trusted from the request, so the sentence
	// the response echoes can never be one the client made up.
	title := a.reverseFindingTitle(inst, req)
	operation := remonitorOperationText(title, req.ID, req.Season, inst.Type)

	scope := actionScope(req.ID, req.Season)
	reverse := reverseOptions{enabled: true, remonitor: true}
	var result cycleInstanceStats

	switch inst.Type {
	case "radarr":
		movies, wantedIDs, dataOK := inspectRadarrLibrary(ctx, a.logger, inst, nil)
		if !dataOK {
			return a.refuse(req, actionOutcomeRefused, operation, fmt.Sprintf("%s's library could not be read just now, so nothing was written", inst.Name))
		}
		result = runRadarrDecisionEngine(ctx, a.logger, inst, movies, wantedIDs, a.cfg.ExclusionTag, scope, a.cfg.DryRun, reverse, fileReportOptions{})
	case "sonarr":
		series, wantedEpisodeIDs, wantedSeasons, dataOK := inspectSonarrLibrary(ctx, a.logger, inst)
		if !dataOK {
			return a.refuse(req, actionOutcomeRefused, operation, fmt.Sprintf("%s's library could not be read just now, so nothing was written", inst.Name))
		}
		result = runSonarrDecisionEngine(ctx, a.logger, inst, series, wantedEpisodeIDs, wantedSeasons, a.cfg.ExclusionTag, scope, a.cfg.DryRun, reverse, fileReportOptions{})
	default:
		return a.refuse(req, actionOutcomeRefused, operation, fmt.Sprintf("instance %s has type %q, which cannot be re-monitored", inst.Name, inst.Type)).withStatus(http.StatusBadRequest)
	}

	return a.remonitorOutcome(inst, req, operation, result)
}

// remonitorOutcome maps the scoped pass's own counters onto the response. The
// pass ran over exactly one item, so exactly one counter moved, and each of
// them already means something precise — which is why the outcome is read from
// them rather than re-derived from the log or guessed from the findings slice.
func (a *actionRunner) remonitorOutcome(inst Instance, req actionRequest, operation string, result cycleInstanceStats) actionResponse {
	rev := result.reverse

	switch {
	case rev.remonitored > 0:
		msg := fmt.Sprintf("Re-monitored in %s. The next sweep will treat it like any other monitored item.", inst.Name)
		a.audit(req, actionOutcomePerformed, operation, msg)
		for _, rec := range rev.actions {
			a.recordAction(inst, rec)
		}
		return actionResponse{Outcome: actionOutcomePerformed, Kind: req.Kind, Operation: operation, Message: msg, status: http.StatusOK}

	case rev.echoUnverified > 0:
		msg := fmt.Sprintf("%s accepted the write but did not confirm it. Treat it as applied; if it did not land, the next sweep reports the item again.", inst.Name)
		a.audit(req, actionOutcomePerformed, operation, msg)
		return actionResponse{Outcome: actionOutcomePerformed, Kind: req.Kind, Operation: operation, Message: msg, Reason: msg, status: http.StatusOK}

	case rev.writeErrors > 0:
		msg := fmt.Sprintf("The re-monitor write to %s failed. Nothing was changed; the log line for this action names the error.", inst.Name)
		a.audit(req, actionOutcomeFailed, operation, msg)
		return actionResponse{Outcome: actionOutcomeFailed, Kind: req.Kind, Operation: operation, Message: msg, Reason: msg, status: http.StatusBadGateway}

	case rev.refused > 0:
		msg := fmt.Sprintf("%s refused this write after re-checking the item against fresh data: it is already monitored, or it no longer fails the criteria that made it a finding. Nothing was changed.", inst.Name)
		a.audit(req, actionOutcomeRefused, operation, msg)
		return actionResponse{Outcome: actionOutcomeRefused, Kind: req.Kind, Operation: operation, Message: msg, Reason: msg, status: http.StatusConflict}

	case rev.withheld > 0 && a.cfg.DryRun:
		msg := "Rehearsed only — dry_run is on. Everything was re-verified against fresh data and the write was withheld at the last moment; with dry_run: false this would re-monitor the item."
		a.audit(req, actionOutcomeRehearsed, operation, msg)
		return actionResponse{Outcome: actionOutcomeRehearsed, Kind: req.Kind, Operation: operation, Message: msg, status: http.StatusOK}

	case rev.withheld > 0:
		reason := rev.withheldReason
		if reason == "" {
			reason = "the reverse write pass withheld this item"
		}
		return a.refuse(req, actionOutcomeRefused, operation, "Nothing was written: "+reason)

	default:
		// The pass produced no outcome for this item at all, which means it
		// never became a finding under a fresh evaluation — the commonest
		// reason being that the *arr, or a human, already re-monitored it.
		return a.refuse(req, actionOutcomeRefused, operation,
			fmt.Sprintf("%s no longer reports this item as wrongly unmonitored, so there was nothing to do. Re-run the scan to see the current state.", inst.Name))
	}
}

// reverseFindingTitle looks the item's title up from the store's own reverse
// findings, so the operation sentence the response echoes is built from what
// cutoffarr reported rather than from what the request claimed.
func (a *actionRunner) reverseFindingTitle(inst Instance, req actionRequest) string {
	if a.stats == nil {
		return "this item"
	}
	for _, v := range a.stats.snapshot().Instances {
		if v.Name != inst.Name {
			continue
		}
		for _, f := range v.ReverseFindings {
			if f.ID == req.ID && f.Title != "" && req.Season == nil {
				return f.Title
			}
			if f.SeriesID == req.ID && f.Season != nil && req.Season != nil && *f.Season == *req.Season {
				return f.Series
			}
		}
	}
	return "this item"
}
