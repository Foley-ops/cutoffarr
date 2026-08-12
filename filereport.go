package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// filereport.go is Phase 11 (v2b): the duplicate & orphan file report. It is
// the one part of this program that ever reads the filesystem, and per the
// PERMANENT rule stated first because it is structural, not a flag: it NEVER
// writes, deletes, moves, or renames a file, and it never gains an option to.
// §2's "never touch files" is extended here from an API restriction (no
// deleteFiles, no /command) to a literal filesystem restriction — see
// TestTree_BansFilesystemMutationAPIs (writer_test.go), which greps the whole
// non-test tree for os.Remove/os.Rename/os.Create/os.WriteFile/os.OpenFile/
// os.Mkdir* and refuses to let any of them exist outside an (empty, by
// design) allowlist.
//
// Method (binding controller resolutions, Phase 11 task brief): build the set
// of files the *arr actually TRACKS (Radarr: movie.path + movieFile.path;
// Sonarr: series.path + episodeFile.path, fetched per series since Sonarr
// never embeds a file's path anywhere but /episodefile), map every tracked
// arr-side path onto disk through media_root_map, then walk each mapped disk
// root and classify every video file it finds by LOCATION alone:
//
//   - the exact tracked path                                -> tracked
//   - anywhere else inside a tracked movie/series folder     -> duplicate
//   - anywhere else under the mapped root                    -> orphan
//   - wrong extension / trailer-sample suffix / extras dir   -> skipped-by-rule
//   - a season the forward engine already distrusted (file-  -> skipped-untrusted
//     count mismatch), or a movie/series whose own path
//     could not be read
//
// Classification never depends on a filename beyond the extension/suffix
// rules above: a Sonarr duplicate's "S01E05"-style label is parsed from the
// filename ONLY for the log line's display grouping, never to decide whether
// something is a duplicate at all (binding controller resolution 3).
//
// Scheduling (resolution 8) is a zero-value-off options struct exactly like
// reverseOptions (reverse.go): the pass runs on full-library cycles only —
// the startup scan, the reconciliation sweep, a full (non---only-id) --once
// run — and never for a webhook cycle or a --only-id scoped run, both of
// which get the zero value and therefore no file-report call at all. Unlike
// the reverse scan there is no acceptance-instrument exception: this pass
// never writes, so there is nothing for a scoped, human-watched run to
// rehearse.

// --- video extension / skip-rule vocabulary (binding controller resolution 6) -

// videoExtensions is the plan's exact recognized-extension set, lower-cased
// and dot-prefixed to match strings.ToLower(path.Ext(name)) directly.
var videoExtensions = map[string]bool{
	".mkv":  true,
	".mp4":  true,
	".avi":  true,
	".m2ts": true,
	".ts":   true,
	".wmv":  true,
}

// extrasDirNames are the Plex-convention "extras" subfolder names pruned from
// duplicate/orphan candidacy, matched case-insensitively against a directory's
// base name — the plan's exact list, verbatim.
var extrasDirNames = map[string]bool{
	"featurettes":       true,
	"behind the scenes": true,
	"trailers":          true,
	"extras":            true,
	"other":             true,
	"specials extras":   true,
	"deleted scenes":    true,
	"interviews":        true,
	"scenes":            true,
	"shorts":            true,
	"featurette":        true,
}

// File-report skip-reason vocabulary: the sub-classification printed under
// fileSkipReasons on the per-instance summary and used as the "reason" behind
// every skipped-by-rule / skipped-untrusted identity term. Named constants,
// not inline strings, for the same reason the forward engine's Reason*
// constants are (decision.go): one spelling, greppable, asserted by the
// exhaustive identity test below rather than typed at every call site.
const (
	FileSkipReasonExtension        = "unrecognized extension"
	FileSkipReasonTrailerOrSample  = "trailer/sample suffix"
	FileSkipReasonExtrasDir        = "extras subfolder"
	FileSkipReasonUntrackedPath    = "movie/series path unreadable; excluded from the tracked set"
	FileSkipReasonMismatchedSeason = "season excluded: episode file count mismatch"
)

// The five identity terms every video file the walk visits resolves into
// exactly one of (binding controller resolution 6). assertFileReportIdentity
// (filereport_test.go) is the exhaustive check that they always add up.
const (
	fileKindTracked          = "tracked"
	fileKindDuplicate        = "duplicate"
	fileKindOrphan           = "orphan"
	fileKindSkippedByRule    = "skipped-by-rule"
	fileKindSkippedUntrusted = "skipped-untrusted"
)

// --- path mapping (binding controller resolution 2) -------------------------

// mediaRoot is one cleaned (arr-path, disk-path) pair from an instance's
// media_root_map, ready for prefix matching.
type mediaRoot struct {
	arrPath  string
	diskPath string
}

// cleanArrPath cleans a Linux-style absolute path per resolution 2: cleaned,
// trailing slash(es) stripped. path.Clean (not filepath.Clean) is used
// deliberately everywhere in this file: it is a purely lexical, forward-slash
// operation that touches no filesystem and behaves identically regardless of
// the host OS running cutoffarr's own build/test — which matters because
// symlinks are never resolved (lexical comparison only) and because this
// project's *arr and media filesystems are always Linux, never the build
// host's own path conventions.
func cleanArrPath(p string) string {
	return path.Clean(p)
}

// hasPathPrefix reports whether p is prefix itself, or a descendant of it,
// matched on path-SEGMENT boundaries only: "/movies" matches "/movies/X/y.mkv"
// but never "/movies-4k/...". Both arguments must already be cleaned
// (cleanArrPath). Comparison is byte-for-byte case-SENSITIVE (resolution 2:
// "server filesystems are Linux").
func hasPathPrefix(p, prefix string) bool {
	if prefix == "/" {
		// Every cleaned absolute path already starts with "/", so the root
		// root matches everything without a special "prefix+/" construction
		// (which would otherwise build "//").
		return true
	}
	return p == prefix || strings.HasPrefix(p, prefix+"/")
}

// mapArrPathToDisk maps an *arr-reported absolute path onto root's disk-side
// path, returning ok=false if p does not fall under root at all (checked on a
// path-segment boundary, never a bare string prefix).
func mapArrPathToDisk(p string, root mediaRoot) (string, bool) {
	p = cleanArrPath(p)
	if !hasPathPrefix(p, root.arrPath) {
		return "", false
	}
	if p == root.arrPath {
		return root.diskPath, true
	}
	// hasPathPrefix already guarantees p starts with root.arrPath + "/" here,
	// so the slice below always begins with the separating slash.
	suffix := p[len(root.arrPath):]
	return cleanArrPath(root.diskPath + suffix), true
}

// mapArrPathToAnyRoot maps p against every configured root, returning the
// disk-side path from whichever root matches. When more than one root's
// arr-side prefix matches (overlapping roots — not a configuration this
// project otherwise validates against), the LONGEST (most specific) prefix
// wins, the same tie-break url routers and this project's own
// containingTrackedFolder use.
func mapArrPathToAnyRoot(p string, roots []mediaRoot) (string, bool) {
	p = cleanArrPath(p)
	bestArrPath, bestDisk := "", ""
	matched := false
	for _, r := range roots {
		if disk, ok := mapArrPathToDisk(p, r); ok {
			if !matched || len(r.arrPath) > len(bestArrPath) {
				bestArrPath, bestDisk, matched = r.arrPath, disk, true
			}
		}
	}
	return bestDisk, matched
}

// mediaRootsFor cleans and returns inst.MediaRootMap as a slice of mediaRoot,
// sorted by arr-side path so iteration order (and therefore log/finding
// order) is reproducible across runs. A nil or empty map — media_root_map
// absent, or explicitly {} — returns nil, which is exactly the OFF state
// (binding controller resolution 1): every caller in this file treats a nil
// root slice as "nothing configured", not as an error.
func mediaRootsFor(inst Instance) []mediaRoot {
	if len(inst.MediaRootMap) == 0 {
		return nil
	}
	roots := make([]mediaRoot, 0, len(inst.MediaRootMap))
	for arrPath, diskPath := range inst.MediaRootMap {
		roots = append(roots, mediaRoot{arrPath: cleanArrPath(arrPath), diskPath: cleanArrPath(diskPath)})
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].arrPath < roots[j].arrPath })
	return roots
}

// --- tracked-set + classification -------------------------------------------

// instanceTrackedSet is the whole of what this instance's file report knows
// about its library, built ONCE per cycle and shared across every configured
// root (a movie/series maps under at most one root in practice, so nothing is
// lost by not re-deriving this per root — and every warning about an
// unreadable path is thereby logged exactly once per item, not once per root).
type instanceTrackedSet struct {
	// files are the exact disk-side paths of every tracked movieFile/
	// episodeFile this instance's library claims, mapped through
	// media_root_map. A video file at one of these paths is "tracked".
	files map[string]bool

	// folders are the exact disk-side paths of every tracked movie/series
	// folder, keyed to the display title used for duplicate grouping. A video
	// file anywhere under one of these (that is not itself in files) is a
	// "duplicate" — unless mismatchedSeasons excludes it.
	folders map[string]string

	// seriesFolder marks which folders keys are Sonarr series (as opposed to
	// Radarr movies), so classifyFileReportPath knows whether to attempt the
	// season/episode display-grouping heuristic at all. Nil/absent means
	// "Radarr instance" for every entry (the zero value, false, is correct
	// there without this map needing to exist).
	seriesFolder map[string]bool

	// mismatchedSeasons excludes specific (series folder, season number)
	// pairs from duplicate candidacy: binding controller resolution 5. Seasons
	// the forward engine already flagged ReasonSeasonFileCountMismatch this
	// cycle have an /episodefile response that disagreed with statistics, so
	// this file report has no trustworthy tracked-file list for them — an
	// "extra" file there is exactly as likely to be Sonarr's OWN untracked
	// file as a genuine duplicate, and it is warn-counted rather than guessed
	// either way.
	mismatchedSeasons map[string]map[int]bool
}

// fileReportClassification is classifyFileReportPath's whole answer: which of
// the five identity terms a file resolves to, its skip reason when
// applicable, and (duplicates only) the display grouping.
type fileReportClassification struct {
	kind   string
	reason string // skipped-by-rule / skipped-untrusted only

	// Duplicate grouping only (resolution 3, display convenience — never an
	// input to kind itself, which is decided purely by folder containment
	// above). isSeries picks which log attr name a finding uses (title for a
	// Radarr movie, series+group for a Sonarr episode/season).
	isSeries bool
	title    string
	group    string
}

// sxxeyyPattern extracts a season/episode label from a filename for DISPLAY
// grouping only — never for classification (binding controller resolution 3:
// "classification never depends on it").
var sxxeyyPattern = regexp.MustCompile(`(?i)S(\d{1,2})E\d{1,3}`)

// seasonFolderPattern recognizes a "Season NN" (optionally zero-padded)
// containing-directory name, the other display-grouping fallback the
// resolution allows.
var seasonFolderPattern = regexp.MustCompile(`(?i)^Season\s*0*(\d{1,2})$`)

// sonarrGroupLabel derives a season number (used ONLY to check the
// season-file-count-mismatch exclusion list) and a display label for a Sonarr
// duplicate candidate. Unparseable names group under the containing folder,
// never a guess (binding controller resolution 3, verbatim).
func sonarrGroupLabel(filePath, seriesFolder string) (season *int, label string) {
	base := path.Base(filePath)
	if m := sxxeyyPattern.FindStringSubmatch(base); m != nil {
		n, err := strconv.Atoi(m[1])
		if err == nil {
			return &n, strings.ToUpper(m[0])
		}
	}
	dir := path.Dir(filePath)
	if m := seasonFolderPattern.FindStringSubmatch(path.Base(dir)); m != nil {
		n, err := strconv.Atoi(m[1])
		if err == nil {
			return &n, path.Base(dir)
		}
	}
	// Unparseable: group under the containing folder, relative to the
	// series' own folder so a deeply-nested stray file's label still says
	// something useful, falling back to the series folder's own name when the
	// file sits directly inside it.
	if dir == seriesFolder {
		return nil, path.Base(seriesFolder)
	}
	rel := strings.TrimPrefix(dir, seriesFolder+"/")
	return nil, rel
}

// containingTrackedFolder reports whether path falls under any folder key in
// folders (path-segment boundary, resolution 2), returning the most specific
// (longest) match when more than one contains it — folders never legitimately
// nest for this project's movies/series, but the tie-break is defined rather
// than left to map iteration order regardless.
func containingTrackedFolder(filePath string, folders map[string]string) (string, bool) {
	best := ""
	matched := false
	for folder := range folders {
		if hasPathPrefix(filePath, folder) {
			if !matched || len(folder) > len(best) {
				best, matched = folder, true
			}
		}
	}
	return best, matched
}

// underExtrasDir reports whether any ancestor directory of filePath, up to
// (and including) root, is named one of extrasDirNames (case-insensitive).
//
// This walks the path lexically (strings only, no filesystem access) rather
// than using fs.WalkDir's SkipDir to prune real directory traversal: SkipDir
// would make every file inside an extras folder invisible to the walk
// entirely, which would make them impossible to COUNT under
// FileSkipReasonExtrasDir for the summary's fileSkipReasons — and per
// resolution 6, "extras" is one of the named skipped-by-rule reasons every
// file walked must be able to resolve to, not a class of file this report
// pretends it never saw. Extras folders are small in every real library this
// matters for, so the (already-open) walk visiting them costs nothing
// meaningful in return for that accounting.
func underExtrasDir(filePath, root string) bool {
	dir := path.Dir(filePath)
	for hasPathPrefix(dir, root) {
		if extrasDirNames[strings.ToLower(path.Base(dir))] {
			return true
		}
		if dir == root {
			return false
		}
		dir = path.Dir(dir)
	}
	return false
}

// classifyFileReportPath is the whole of Phase 11's classification rule,
// applied to one already-mapped disk-side file path. It is a pure function —
// no I/O, no logging — so every branch is unit-testable directly.
//
// Order matters and is deliberate: extension, extras-dir and trailer/sample
// are checked BEFORE folder containment, because none of the three needs to
// know anything about tracked folders to answer, and because a poster.jpg or
// a Trailers/ clip sitting beside a tracked movie must never be reported as a
// duplicate just because it happens to share the folder.
func classifyFileReportPath(filePath string, root mediaRoot, set instanceTrackedSet) fileReportClassification {
	base := path.Base(filePath)
	ext := strings.ToLower(path.Ext(base))
	if !videoExtensions[ext] {
		return fileReportClassification{kind: fileKindSkippedByRule, reason: FileSkipReasonExtension}
	}
	if underExtrasDir(filePath, root.diskPath) {
		return fileReportClassification{kind: fileKindSkippedByRule, reason: FileSkipReasonExtrasDir}
	}
	stem := strings.TrimSuffix(base, ext)
	lowerStem := strings.ToLower(stem)
	if strings.HasSuffix(lowerStem, "-trailer") || strings.HasSuffix(lowerStem, "-sample") {
		return fileReportClassification{kind: fileKindSkippedByRule, reason: FileSkipReasonTrailerOrSample}
	}
	if set.files[filePath] {
		return fileReportClassification{kind: fileKindTracked}
	}

	folder, ok := containingTrackedFolder(filePath, set.folders)
	if !ok {
		return fileReportClassification{kind: fileKindOrphan}
	}

	title := set.folders[folder]
	isSeries := set.seriesFolder[folder]
	group := ""
	if isSeries {
		season, sub := sonarrGroupLabel(filePath, folder)
		group = sub
		if season != nil {
			if bySeason, ok := set.mismatchedSeasons[folder]; ok && bySeason[*season] {
				return fileReportClassification{kind: fileKindSkippedUntrusted, reason: FileSkipReasonMismatchedSeason}
			}
		}
	}
	return fileReportClassification{kind: fileKindDuplicate, isSeries: isSeries, title: title, group: group}
}

// --- mount-problem heuristic + walk (binding controller resolution 4) ------

// fileReportMountSampleSize bounds how many tracked files under one root are
// stat'd for heuristic (b). "Up to 100" per the binding resolution — a var,
// not a const, purely so a test could lower it if a future change needed to
// exercise the cap directly (no current test does; kept a var for the same
// reason maxWantedCutoffPages is, radarr.go).
var fileReportMountSampleSize = 100

// fileReportMountHealthyFraction is heuristic (b)'s threshold: at least this
// fraction of the sampled tracked files must exist on disk, or the root is
// treated as a mount problem rather than a library with a few missing files.
const fileReportMountHealthyFraction = 0.9

// fileReportFinding is one duplicate or orphan file, ready to be logged.
// groupCount (duplicates only) is filled in once the whole root has been
// walked, since it counts SIBLINGS within the same display group.
type fileReportFinding struct {
	kind     string // fileKindDuplicate or fileKindOrphan
	diskPath string

	isSeries   bool
	title      string
	group      string
	groupCount int
}

// fileReportRootOutcome is one mapped root's walk result: either skipped (the
// mount-problem heuristic aborted it, or a walk error made it incomplete —
// same completeness-contract reasoning as fetchWantedCutoffPages, radarr.go:
// an incomplete result is never returned as if it were the whole truth), or
// the classification of every file it found.
//
// seenTracked/seenSkippedByRule/seenSkippedUntrusted plus
// len(findings-by-kind) are the five identity-term counters
// assertFileReportIdentity (filereport_test.go) checks always add up to the
// number of files the walk actually visited.
type fileReportRootOutcome struct {
	skipped bool

	findings    []fileReportFinding
	skipReasons map[string]int

	seenTracked          int
	seenSkippedByRule    int
	seenSkippedUntrusted int
}

// errFileReportShutdown is fs.WalkDir's abort signal for a context
// cancellation observed between files: it is never a real filesystem error,
// but returning it from the walk function is the only way to stop
// fs.WalkDir early, and evaluateFileReportRoot treats it exactly like any
// other walk error — abort this root's report, per the completeness
// contract, rather than return what was seen before the cut.
var errFileReportShutdown = errors.New("file report: shutdown requested mid-walk")

// filesUnderRoot returns the subset of files whose disk path falls under
// root (path-segment boundary), for heuristic (b)'s per-root sample.
func filesUnderRoot(files map[string]bool, root string) []string {
	var out []string
	for f := range files {
		if hasPathPrefix(f, root) {
			out = append(out, f)
		}
	}
	// Sorted so the sample taken below is deterministic across runs — the
	// same reason sampleEveryKth (decision.go) sorts before sampling, rather
	// than depending on Go's randomized map iteration order.
	sort.Strings(out)
	return out
}

// sampleFileReportPaths deterministically samples up to n paths: the first n
// of a slice the caller has already sorted. Simpler than
// crossCheckSampleSize's stride sampling (decision.go) because heuristic (b)
// only needs SOME evidence the root is healthy, not a representative spread —
// any 100 tracked files existing (or not) answers the same question.
func sampleFileReportPaths(sorted []string, n int) []string {
	if len(sorted) <= n {
		return sorted
	}
	return sorted[:n]
}

// evaluateFileReportRoot applies the mount-problem heuristic to root and,
// only if root passes it, walks every file under it and classifies each one.
// It never returns an error: like every other read-path function in this
// project (§2.6), the caller-visible outcome is skipped=true plus a WARN
// already logged naming the root — "one root aborting never affects other
// roots/instances" (binding controller resolution 4) is enforced by the
// CALLER simply moving on to the next root, not by anything in here.
func evaluateFileReportRoot(ctx context.Context, logger *slog.Logger, itemLevel slog.Level, inst Instance, root mediaRoot, set instanceTrackedSet) fileReportRootOutcome {
	warnAbort := func(msg string, args ...any) fileReportRootOutcome {
		attrs := []any{"instance", inst.Name, "type", inst.Type, "root", root.diskPath}
		logger.Warn(msg, append(attrs, args...)...)
		return fileReportRootOutcome{skipped: true}
	}

	// Heuristic (a): the mapped root must exist and be a readable directory.
	info, err := os.Stat(root.diskPath)
	if err != nil {
		return warnAbort("file report: mount-problem heuristic aborted this root: it could not be statted (missing mount?)", "error", err)
	}
	if !info.IsDir() {
		return warnAbort("file report: mount-problem heuristic aborted this root: it is not a directory")
	}
	if _, err := os.ReadDir(root.diskPath); err != nil {
		return warnAbort("file report: mount-problem heuristic aborted this root: it could not be read", "error", err)
	}

	// Heuristic (b): of a sample of up to fileReportMountSampleSize tracked
	// files under this root, at least fileReportMountHealthyFraction must
	// exist on disk. A root with zero tracked files skips this check
	// entirely (there is nothing to sample, and that is not itself evidence
	// of a problem — an empty or brand-new library looks exactly like this).
	tracked := filesUnderRoot(set.files, root.diskPath)
	if len(tracked) > 0 {
		sample := sampleFileReportPaths(tracked, fileReportMountSampleSize)
		existing := 0
		for _, p := range sample {
			if _, err := os.Stat(p); err == nil {
				existing++
			}
		}
		if float64(existing) < float64(len(sample))*fileReportMountHealthyFraction {
			return warnAbort("file report: mount-problem heuristic aborted this root: most of a sample of its own tracked files are missing from disk — this looks like an unmounted or half-mounted share, not a healthy library missing a few files",
				"sampled", len(sample), "existingOnDisk", existing, "trackedUnderRoot", len(tracked))
		}
	}

	var outcome fileReportRootOutcome
	outcome.skipReasons = map[string]int{}
	videoFilesSeen := 0

	walkErr := fs.WalkDir(os.DirFS(root.diskPath), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return errFileReportShutdown
		}
		if d.IsDir() {
			return nil
		}

		filePath := cleanArrPath(path.Join(root.diskPath, rel))
		c := classifyFileReportPath(filePath, root, set)
		switch c.kind {
		case fileKindTracked:
			outcome.seenTracked++
			videoFilesSeen++
		case fileKindDuplicate:
			videoFilesSeen++
			outcome.findings = append(outcome.findings, fileReportFinding{
				kind: fileKindDuplicate, diskPath: filePath, isSeries: c.isSeries, title: c.title, group: c.group,
			})
		case fileKindOrphan:
			videoFilesSeen++
			outcome.findings = append(outcome.findings, fileReportFinding{kind: fileKindOrphan, diskPath: filePath})
		case fileKindSkippedByRule:
			outcome.skipReasons[c.reason]++
			outcome.seenSkippedByRule++
			// Every skipped-by-rule reason except "wrong extension" is, by
			// construction, still a real video file — just excluded from
			// candidacy by a naming rule. It counts as evidence toward
			// heuristic (c) ("this root is not simply empty") the same as a
			// tracked or duplicate file does; a wrong-extension file (a
			// poster, an nfo) proves nothing about whether real media exists
			// here.
			if c.reason != FileSkipReasonExtension {
				videoFilesSeen++
			}
		case fileKindSkippedUntrusted:
			outcome.skipReasons[c.reason]++
			outcome.seenSkippedUntrusted++
			videoFilesSeen++
		default:
			return fmt.Errorf("file report: internal error: unclassified file kind %q", c.kind)
		}
		return nil
	})
	if walkErr != nil {
		if errors.Is(walkErr, errFileReportShutdown) {
			logger.Log(ctx, slog.LevelInfo, "shutdown requested: abandoning this root's file report mid-walk; a partial walk is never reported as findings",
				"instance", inst.Name, "type", inst.Type, "root", root.diskPath)
			return fileReportRootOutcome{skipped: true}
		}
		return warnAbort("file report: aborting this root: an error occurred while walking it, so its report would be incomplete", "error", walkErr)
	}

	// Heuristic (c): a root tracking N>0 files that contains ZERO video files
	// anywhere on the walk is the same "this looks unmounted" signal as (b),
	// caught here for the case where the specific tracked files sampled by
	// (b) happen not to be the ones missing (a small sample, or files under a
	// subtree heuristic (b) did not walk).
	if len(tracked) > 0 && videoFilesSeen == 0 {
		return warnAbort("file report: mount-problem heuristic aborted this root: it tracks files but the walk found zero video files of any kind",
			"trackedUnderRoot", len(tracked))
	}

	// Group counts, computed now that every file under the root has been
	// seen: a duplicate finding's "N extra files for <group>" number is a
	// fact about the WHOLE root, not about any one file in isolation.
	groupCounts := map[string]int{}
	for _, f := range outcome.findings {
		if f.kind == fileKindDuplicate {
			groupCounts[groupKey(f)]++
		}
	}
	for i, f := range outcome.findings {
		if f.kind == fileKindDuplicate {
			outcome.findings[i].groupCount = groupCounts[groupKey(f)]
		}
	}

	for _, f := range outcome.findings {
		logFileReportFinding(ctx, logger, itemLevel, inst, root, f)
	}

	return outcome
}

// groupKey identifies a duplicate finding's display group uniquely enough to
// count siblings by: title alone for Radarr (one movie, one group), title+
// sub-label for Sonarr (a series can have several groups, one per parseable
// season/episode or containing folder).
func groupKey(f fileReportFinding) string {
	return f.title + "\x00" + f.group
}

// logFileReportFinding emits the frozen vocabulary line (binding controller
// resolution 7): msg="file-report finding", kind=duplicate|orphan, instance,
// root and path (both CUTOFFARR-side / disk paths), plus duplicate-only
// grouping attrs. Logged at itemLevel — the same demotion the forward and
// reverse passes give their own per-item lines (full at startup/--once,
// debug on repeating sweeps) — never at a level of its own.
func logFileReportFinding(ctx context.Context, logger *slog.Logger, itemLevel slog.Level, inst Instance, root mediaRoot, f fileReportFinding) {
	attrs := []any{"kind", f.kind, "instance", inst.Name, "type", inst.Type, "root", root.diskPath, "path", f.diskPath}
	if f.kind == fileKindDuplicate {
		if f.isSeries {
			attrs = append(attrs, "series", f.title, "group", f.group)
		} else {
			attrs = append(attrs, "title", f.title)
		}
		attrs = append(attrs, "groupCount", f.groupCount)
	}
	logger.Log(ctx, itemLevel, "file-report finding", attrs...)
}
