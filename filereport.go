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
	FileSkipReasonExtension              = "unrecognized extension"
	FileSkipReasonTrailerOrSample        = "trailer/sample suffix"
	FileSkipReasonExtrasDir              = "extras subfolder"
	FileSkipReasonUntrackedPath          = "movie/series path unreadable; excluded from the tracked set"
	FileSkipReasonMismatchedSeason       = "season excluded: episode file count mismatch"
	FileSkipReasonExcludedByTag          = "movie/series excluded by exclusion_tag"
	FileSkipReasonOutsideConfiguredRoots = "tracked path outside every configured root"

	// FileSkipReasonCaseTwin is [v2.1] the case-collision skip reason: every
	// colliding entry NAME (both halves of a directory-name twin, or a
	// file-name twin) that caseCollisionsInDir excludes from descent/
	// classification is counted under this one string on the SAME
	// fileSkipReasons summary attr the other six reasons already share
	// ("case-twin names excluded=N", binding controller resolution 3) —
	// even though, unlike the other six, it can cover DIRECTORY entries as
	// well as file entries, never routed through classifyFileReportPath at
	// all. See caseCollisionsInDir's own doc comment for why this lives
	// outside the five-identity-term vocabulary below rather than as a
	// sixth skipped-by-rule/skipped-untrusted reason.
	FileSkipReasonCaseTwin = "case-twin names excluded"
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

// fileKindCaseCollision is [v2.1] a SIXTH finding kind, deliberately outside
// the five-identity-term vocabulary above: a case-collision finding is never
// the classification of one video FILE the walk visited (classifyFileReportPath
// never runs against a colliding entry at all — see caseCollisionsInDir's own
// doc comment), it is a finding about a whole GROUP of same-folded entry
// NAMES observed at listing time, before any per-file classification could
// even begin. assertFileReportIdentity (filereport_test.go) recognizes this
// kind explicitly and excludes it from the five-term sum for exactly that
// reason.
const fileKindCaseCollision = "case-collision"

// fileReportEntryTypeDir/fileReportEntryTypeFile are the two shapes a
// case-collision finding's own entryType field can take (binding controller
// resolution 2: "entry type (dir/file)") — a single finding's colliding
// names are always all the same type, since detectCaseCollisions is applied
// separately to a directory's subdirectory names and its file names (binding
// controller resolution 1), never to the two mixed together.
const (
	fileReportEntryTypeDir  = "dir"
	fileReportEntryTypeFile = "file"
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
	if root.arrPath == "/" {
		// hasPathPrefix's "/" special case matches p without ever requiring
		// "root.arrPath + /"; p (cleaned, absolute) already starts with its
		// own leading "/", so it IS the correct suffix as-is. The general
		// branch below would instead slice off that one leading byte
		// (root.arrPath being length 1) and silently strip the separator.
		return cleanArrPath(root.diskPath + p), true
	}
	// hasPathPrefix already guarantees p starts with root.arrPath + "/" here,
	// so the slice below always begins with the separating slash.
	suffix := p[len(root.arrPath):]
	return cleanArrPath(root.diskPath + suffix), true
}

// rootRelativeDisplayPath renders a file's ON-SCREEN path relative to its
// mapped root — the root's own last path segment, then rel (the file's path
// relative to root.diskPath, exactly as fs.WalkDir already handed
// evaluateFileReportRoot's walk function — reused here rather than
// re-derived a second time by trimming diskPath). The full disk path
// (fileReportFinding.diskPath) can carry a long, host-specific prefix — a
// scratch-directory mount on a dev laptop, a deeply nested share on the
// server — that a human operator glancing at the GUI does not need to see
// to know which file a finding is about; "Movies/Some Title/file.mkv" reads
// the same regardless of what that root happens to be mounted under. rel is
// always a slash-separated fs.FS path (io/fs's own contract, independent of
// host OS), so path.Join — not filepath.Join — is correct here, matching
// this file's own path.Clean-not-filepath.Clean convention above.
func rootRelativeDisplayPath(root mediaRoot, rel string) string {
	return path.Join(path.Base(root.diskPath), rel)
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

	// excludedFolders marks folders belonging to a movie/series that carries
	// the configured exclusion_tag (or, when the tag is active, whose tags
	// field could not be read at all — the same fail-safe direction
	// evaluateMovie/evaluateSeries use, decision.go). The README states the
	// tag "opts an item out of everything cutoffarr does, in every mode,
	// including dry-run reporting" — the file report honors that by still
	// protecting the item's own tracked file (it is never reported as an
	// orphan of itself) while withholding any EXTRA file inside its folder
	// from duplicate candidacy, rather than printing "kind=duplicate" lines
	// naming an item the user explicitly asked to be left alone.
	excludedFolders map[string]bool

	// distrustedFolders marks folders whose own tracked-FILE evidence is
	// untrustworthy: the item's own path could not be read while it claims
	// a file (Radarr movieFile.path / Sonarr episodeFile.path unreadable),
	// or a Sonarr series has no id at all (its episode files can never be
	// fetched). In both cases set.files was never told what to expect
	// inside this folder, so a real file sitting in it would otherwise fall
	// through to the duplicate branch and be printed as a confident
	// kind=duplicate about what may be a human's only copy — the exact
	// failure buildRadarrTrackedSet's own doc comment on independent
	// folder/file tracking exists to prevent for the item's OWN file, just
	// not (until this field existed) for its SIBLINGS.
	//
	// This is a DIFFERENT identity outcome from excludedFolders, deliberately:
	// excludedFolders is a human's explicit opt-out (exclusion_tag) —
	// skipped-by-rule, FileSkipReasonExcludedByTag. distrustedFolders is
	// untrusted *arr evidence, not a choice — skipped-untrusted,
	// FileSkipReasonUntrackedPath, the same identity term a mismatched
	// Sonarr season already uses for the same reason (the tracked-file
	// picture for this folder cannot be trusted, so nothing in it is ever
	// guessed as a confident duplicate).
	distrustedFolders map[string]bool
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

	// folder is the disk-side path of the containing tracked folder
	// (set.folders' key) a duplicate was matched against. It is the real
	// identity groupKey counts siblings by — see groupKey's doc comment for
	// why title+group alone is wrong: two distinct movies/series can share a
	// display title (same-titled remakes are common), but never a folder.
	folder string
}

// sxxeyyPattern extracts a season/episode label from a filename for DISPLAY
// grouping only — never for classification (binding controller resolution 3:
// "classification never depends on it").
var sxxeyyPattern = regexp.MustCompile(`(?i)S(\d{1,2})E\d{1,3}`)

// seasonFolderPattern recognizes a "Season NN" (optionally zero-padded)
// containing-directory name, the other display-grouping fallback the
// resolution allows.
var seasonFolderPattern = regexp.MustCompile(`(?i)^Season\s*0*(\d{1,2})$`)

// sonarrGroupLabel derives a display label ONLY for a Sonarr duplicate
// candidate (binding controller resolution 3: parsing SxxEyy from the
// filename, or falling back to the containing "Season NN" folder, is "for the
// label ONLY (display convenience; classification never depends on it)").
// The season number it also returns must NEVER be used to decide the
// season-file-count-mismatch exclusion (resolution 5: that decision is by
// LOCATION alone) — see seasonFromLocation for the classification-facing
// equivalent. Unparseable names group under the containing folder, never a
// guess.
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

// seasonFromLocation determines a Sonarr file's season purely from the
// containing "Season NN" ancestor directory, walking up from filePath to (and
// including) seriesFolder — never from the filename. This is the ONLY season
// signal the season-file-count-mismatch exclusion may use (binding controller
// resolution 5: "FOLDERS excluded from duplicate candidacy, never guessed"):
// a file physically inside a distrusted "Season 01" folder but misleadingly
// named "...S02E01.mkv" must still be excluded as season 1, and
// sonarrGroupLabel's filename-first parse must never be allowed to override
// that. Returns nil when no ancestor up to seriesFolder is a recognizable
// season folder — e.g. season folders are disabled for this series and every
// episode file sits directly in the series folder — in which case the caller
// cannot rule out ANY season and must fail toward distrust, not toward a
// guessed duplicate.
func seasonFromLocation(filePath, seriesFolder string) *int {
	dir := path.Dir(filePath)
	for hasPathPrefix(dir, seriesFolder) {
		if m := seasonFolderPattern.FindStringSubmatch(path.Base(dir)); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				return &n
			}
		}
		if dir == seriesFolder {
			return nil
		}
		dir = path.Dir(dir)
	}
	return nil
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
		// [FIX] dir==root must be checked BEFORE testing dir's own basename
		// against extrasDirNames: a mapped root's basename is not a
		// sub-folder name a file sits inside, it is the boundary of the
		// walk itself. Testing the name first meant a root whose own
		// basename happens to match one of the eleven Plex-convention
		// extras names (e.g. /data/media/Shorts, /data/media/Other — all
		// plausible real library names) matched the moment the climb
		// reached the root, self-excluding every file under it, at any
		// depth, as skipped-by-rule/extras — a whole root silently
		// reporting duplicates=0 orphans=0, indistinguishable from clean.
		if dir == root {
			return false
		}
		if extrasDirNames[strings.ToLower(path.Base(dir))] {
			return true
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

	if set.distrustedFolders[folder] {
		// This folder's own tracked-FILE evidence is untrustworthy (own path
		// unreadable while a file is claimed, or — Sonarr — no series id at
		// all to ever fetch episode files with). set.files was never told
		// what belongs in here, so a real file could be either the item's
		// own (misclassified) file or a genuine extra — indistinguishable
		// from here, and never guessed either way. See
		// instanceTrackedSet.distrustedFolders' doc comment for why this is
		// skipped-untrusted rather than excludedFolders' skipped-by-rule.
		return fileReportClassification{kind: fileKindSkippedUntrusted, reason: FileSkipReasonUntrackedPath}
	}

	if set.excludedFolders[folder] {
		// The exclusion_tag opts this movie/series out of everything
		// cutoffarr does, "in every mode, including dry-run reporting"
		// (README) — its own tracked file was already returned as
		// fileKindTracked above (set.files is checked first, unconditionally),
		// so this only withholds EXTRA files in its folder from being
		// printed as a duplicate finding.
		return fileReportClassification{kind: fileKindSkippedByRule, reason: FileSkipReasonExcludedByTag}
	}

	title := set.folders[folder]
	isSeries := set.seriesFolder[folder]
	group := ""
	if isSeries {
		// The display label is filename-first (resolution 3: display
		// convenience only). The exclusion decision below is LOCATION-only
		// (resolution 5) and must never consult it.
		_, sub := sonarrGroupLabel(filePath, folder)
		group = sub

		if bySeason := set.mismatchedSeasons[folder]; len(bySeason) > 0 {
			// This series has at least one season the forward engine
			// already distrusted for a file-count mismatch. Determine
			// THIS file's season from its containing "Season NN" folder
			// alone — never the filename, which could be wrong or
			// deliberately misleading about which season a file actually
			// lives in. When the season folder can't be identified at all
			// (season folders disabled; the file sits directly in the
			// series folder) we cannot rule out that it belongs to the
			// distrusted season, and resolution 5 forbids guessing either
			// way, so the whole series folder is treated as untrusted
			// rather than any file in it being reported as a confident
			// duplicate.
			season := seasonFromLocation(filePath, folder)
			if season == nil || bySeason[*season] {
				return fileReportClassification{kind: fileKindSkippedUntrusted, reason: FileSkipReasonMismatchedSeason}
			}
		}
	}
	return fileReportClassification{kind: fileKindDuplicate, isSeries: isSeries, title: title, group: group, folder: folder}
}

// --- case-collision detection (v2.1, user-requested) ------------------------
//
// Case-twins are real library defects (the *arr tracks at most one
// spelling; the other is shadow content) AND they break directory
// addressing on case-insensitive views (macOS SMB), where they previously
// caused the whole root to abort with "could not be checked". Detection
// from the LISTING works on every platform — enumeration returns both names
// even where opening one of them ambiguously is not safe — so this whole
// feature is built on that: a directory's own entry names, nothing more.

// detectCaseCollisions groups names by Unicode case-folding equality
// (strings.EqualFold semantics — not a precomputed strings.ToLower key,
// which can diverge from EqualFold for pairs like the Kelvin sign and
// ASCII "k") and returns every group of two or more names that fold equal
// to each other but are not ALL identical byte-for-byte — real case-twins,
// the library defect this whole feature exists to surface (binding
// controller resolution 1). A directory's own real entries can never repeat
// a byte-identical name twice (the filesystem itself forbids it), so the
// "not all identical" guard only ever matters against a synthesized/test
// name list — but it costs nothing to keep correct there too: two spellings
// that already match exactly have nothing to merge or rename.
//
// Pure and side-effect free: no I/O, callable directly from a synthesized
// name list on every platform — see this file's own "case-collision
// detection" section header, and filereport_test.go's matching header, for
// why that matters (t.TempDir on macOS/APFS cannot hold two real case-twin
// entries at once).
//
// O(n^2) in the size of names (every name pairwise-compared against every
// other): a real directory listing is never large enough, in any library
// this project targets, for that to matter, and pairwise strings.EqualFold
// is the simplest way to guarantee the result matches EqualFold's own
// semantics exactly rather than an approximation of them.
//
// Order is deterministic and entirely a function of the input's own order:
// groups are returned in the order their first member appears in names, and
// each group lists its members in the order they appear in names — no
// internal re-sorting, so a caller in a fixed listing order (os.ReadDir's
// own alphabetical guarantee, or a test's own fixed slice) gets a fixed,
// reproducible result.
func detectCaseCollisions(names []string) [][]string {
	var groups [][]string
	assigned := make([]bool, len(names))
	for i, a := range names {
		if assigned[i] {
			continue
		}
		group := []string{a}
		distinct := map[string]bool{a: true}
		for j := i + 1; j < len(names); j++ {
			if assigned[j] {
				continue
			}
			if strings.EqualFold(a, names[j]) {
				group = append(group, names[j])
				distinct[names[j]] = true
				assigned[j] = true
			}
		}
		if len(group) >= 2 && len(distinct) >= 2 {
			groups = append(groups, group)
		}
	}
	return groups
}

// fileReportDirLister lists one directory's own entries for
// caseCollisionsInDir's pre-scan. A package-level var — the same seam
// fileReportMountSampleSize already established for a different value this
// file needs test control over — rather than a new parameter threaded
// through evaluateFileReportRoot and its many existing call sites/tests:
// production always uses the real os.ReadDir; a test can substitute a fake
// lister returning SYNTHESIZED fs.DirEntry values (case-twin names a real
// case-insensitive dev filesystem cannot hold at once) without touching
// disk at all. Every test in this package runs sequentially (no
// t.Parallel), so a test overriding this var and restoring it on cleanup is
// safe, including under -race.
var fileReportDirLister = os.ReadDir

// caseCollisionEntry is one name within a case-collision finding: the
// colliding entry's own name (not a full path — the containing directory is
// already carried by the finding's diskPath/displayPath), and whether it
// correlates against the tracked set (binding controller resolution 2:
// "whether each name contains/IS a tracked path (correlate against the
// tracked set where determinable)"). The tracked set is always fully built
// by the time the walk runs, so "determinable" here always means "checked",
// never "unknown" — a name outside the tracked set entirely (a stray
// directory in an unmanaged corner of a root) simply correlates false,
// exactly as orphan classification already treats an untracked location.
type caseCollisionEntry struct {
	name    string
	tracked bool
}

// pathIsOrContainsTracked reports whether p (one colliding entry's own full
// disk path) IS a tracked file or folder itself, or CONTAINS one — some
// tracked file or folder lives at or under p. This is a case-collision-only
// correlation (binding controller resolution 2) computed for a human's
// benefit — it tells which of two colliding spellings looks like "the real
// one" — and is never an input to classifyFileReportPath's own duplicate/
// orphan decision, which stays exactly as it always was.
func pathIsOrContainsTracked(p string, set instanceTrackedSet) bool {
	if set.files[p] {
		return true
	}
	if _, ok := set.folders[p]; ok {
		return true
	}
	for f := range set.files {
		if hasPathPrefix(f, p) {
			return true
		}
	}
	for f := range set.folders {
		if hasPathPrefix(f, p) {
			return true
		}
	}
	return false
}

// caseCollisionsInDir lists dirPath's own entries (fileReportDirLister — see
// its own doc comment on why this is an injectable seam) and applies
// detectCaseCollisions separately to its subdirectory names and its file
// names (binding controller resolution 1: "applies to BOTH ... at every
// level of the walk") — unfiltered by video extension or any other
// classification rule, since the collision check runs BEFORE any of that
// (binding controller resolution 3). For every group found it returns one
// case-collision finding, carrying the containing directory (diskPath/
// displayPath, root-relative per the existing convention) plus every
// colliding name and its tracked correlation, and records every colliding
// entry's own full disk path in excludeDirs/excludeFiles so
// evaluateFileReportRoot's walk can exclude it from descent (a directory)
// or classification (a file) entirely — see that function's own walkFn
// closure for how the two maps are consumed. excludedCount is the total
// number of colliding NAMES across every group found here, both entry
// types combined — the new skip reason's own tally, FileSkipReasonCaseTwin
// ("case-twin names excluded=N", binding controller resolution 3).
//
// [Known simplicity/perf tradeoff, documented rather than engineered
// around] This is a SECOND, independent read of dirPath's own entries:
// fs.WalkDir does its own internal ReadDir on every directory it does not
// SkipDir, and this function's own fileReportDirLister call happens
// earlier, from inside walkFn, for the SAME directory. A custom,
// from-scratch recursive walk could read each directory exactly once, but
// would have to reimplement fs.WalkDir's own symlink-non-following,
// sorted-order and error-propagation behavior byte for byte — all of it
// already correct and already pinned by this file's own symlink/walk-error
// tests. A doubled, cheap (ReadDir never touches file contents) directory
// listing was judged the smaller risk.
func caseCollisionsInDir(dirPath, rel string, root mediaRoot, set instanceTrackedSet) (findings []fileReportFinding, excludeDirs, excludeFiles map[string]bool, excludedCount int, err error) {
	entries, err := fileReportDirLister(dirPath)
	if err != nil {
		return nil, nil, nil, 0, err
	}
	var dirNames, fileNames []string
	for _, e := range entries {
		if e.IsDir() {
			dirNames = append(dirNames, e.Name())
		} else {
			fileNames = append(fileNames, e.Name())
		}
	}

	excludeDirs = map[string]bool{}
	excludeFiles = map[string]bool{}
	display := rootRelativeDisplayPath(root, rel)

	addFindingsFor := func(groups [][]string, entryType string, exclude map[string]bool) {
		for _, group := range groups {
			names := make([]caseCollisionEntry, len(group))
			for i, n := range group {
				full := cleanArrPath(path.Join(dirPath, n))
				exclude[full] = true
				names[i] = caseCollisionEntry{name: n, tracked: pathIsOrContainsTracked(full, set)}
			}
			excludedCount += len(group)
			findings = append(findings, fileReportFinding{
				kind: fileKindCaseCollision, diskPath: dirPath, displayPath: display,
				entryType: entryType, names: names,
			})
		}
	}
	addFindingsFor(detectCaseCollisions(dirNames), fileReportEntryTypeDir, excludeDirs)
	addFindingsFor(detectCaseCollisions(fileNames), fileReportEntryTypeFile, excludeFiles)

	return findings, excludeDirs, excludeFiles, excludedCount, nil
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

// fileReportFinding is one duplicate, orphan, or [v2.1] case-collision
// finding, ready to be logged. groupCount (duplicates only) is filled in
// once the whole root has been walked, since it counts SIBLINGS within the
// same display group.
type fileReportFinding struct {
	kind     string // fileKindDuplicate, fileKindOrphan, or fileKindCaseCollision
	diskPath string

	// displayPath is diskPath rendered relative to its mapped root — see
	// rootRelativeDisplayPath. diskPath is still what gets logged (binding
	// controller resolution 7: "root and path, both CUTOFFARR-side / disk
	// paths") and still travels to the API's own `path` field; displayPath
	// exists only so a client (the GUI) has something short enough to put
	// on screen without a scratch-directory-length prefix, without having
	// to re-derive it itself from `path` and a root it doesn't have.
	displayPath string

	isSeries   bool
	title      string
	group      string
	folder     string // groupKey's real key (duplicates only) — see its doc comment.
	groupCount int

	// entryType/names are [v2.1] case-collision-only: which kind of entry
	// collided (fileReportEntryTypeDir/fileReportEntryTypeFile) and every
	// colliding name plus its tracked-set correlation (binding controller
	// resolution 2). For this kind, diskPath/displayPath name the
	// CONTAINING DIRECTORY the collision was found in, not any one of the
	// colliding entries themselves — see caseCollisionsInDir's own doc
	// comment.
	entryType string
	names     []caseCollisionEntry
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

// foldersUnderRoot returns the subset of folder keys whose disk path falls
// under root (path-segment boundary) — the symmetric-guard twin of
// filesUnderRoot, used to detect a root that tracks movie/series FOLDERS
// but has zero tracked FILES under it (see evaluateFileReportRoot's
// heuristic (b2)).
func foldersUnderRoot(folders map[string]string, root string) []string {
	var out []string
	for f := range folders {
		if hasPathPrefix(f, root) {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// sampleFileReportPaths deterministically samples up to n paths, evenly
// spread across the whole ALREADY-SORTED slice the caller passes in — the
// string twin of sampleEveryKth (decision.go).
//
// [FIX] This used to head-slice (return sorted[:n]), on the reasoning that
// heuristic (b) only needs SOME evidence the root is healthy, not a
// representative spread. That reasoning missed how a real partial mount
// fails: a mergerfs branch going down, or a per-letter split mount, drops a
// contiguous ALPHABETICAL subtree of the library, not a random scatter of
// files. A head-slice sample of the sorted tracked-path list is always the
// alphabetically-first cluster, so such a share could pass heuristic (b)
// cleanly — every one of the first n sorted paths present — while
// everything past the sample window is entirely missing, and heuristic (c)
// only catches TOTAL absence, not a partial one. Striding across the whole
// slice means a subtree dropped anywhere in the alphabet shows up in the
// sample.
func sampleFileReportPaths(sorted []string, n int) []string {
	if len(sorted) <= n {
		return sorted
	}
	step := len(sorted) / n
	if step < 1 {
		step = 1
	}
	sampled := make([]string, 0, n)
	for i := 0; i < len(sorted) && len(sampled) < n; i += step {
		sampled = append(sampled, sorted[i])
	}
	return sampled
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

	// Heuristic (b2), symmetric to (b): a root that tracks movie/series
	// FOLDERS but has ZERO tracked FILES under it is exactly as
	// untrustworthy as (b)'s "most sampled tracked files are missing"
	// case, but (b) itself can never catch it — (b) only samples FILES,
	// and an EMPTY tracked-file set trivially has nothing to sample, so
	// gating heuristic (b) on len(tracked) > 0 leaves this direction
	// completely unguarded. The failure mode: every movieFile.path/
	// episodeFile.path under this root was absent, unmapped, or the field
	// name is simply wrong (a systemic bug, not a mount problem), so
	// set.files ends up empty while set.folders is still populated from
	// movie.path/series.path. Every real file the walk finds would then
	// match its containing folder but no tracked file and be reported as
	// kind=duplicate — the WHOLE root, confidently. A root with no tracked
	// folders under it either (nothing this instance manages lives here at
	// all) is unaffected: that is a legitimate empty-or-unmanaged root, the
	// exact case the file report exists to surface as orphans, not a
	// mount problem.
	trackedFolders := foldersUnderRoot(set.folders, root.diskPath)
	if len(trackedFolders) > 0 && len(tracked) == 0 {
		return warnAbort("file report: mount-problem heuristic aborted this root: it tracks movie/series folders but has zero tracked files under it — every real file in those folders would misreport as a duplicate rather than being recognized as already tracked",
			"trackedFoldersUnderRoot", len(trackedFolders))
	}

	var outcome fileReportRootOutcome
	outcome.skipReasons = map[string]int{}
	videoFilesSeen := 0

	// excludeDirs/excludeFiles are [v2.1] populated by caseCollisionsInDir's
	// pre-scan of a directory's own children the moment walkFn is called for
	// THAT DIRECTORY itself — strictly before fs.WalkDir ever calls walkFn
	// again for any one of those children (fs.WalkDir's own pre-order
	// guarantee: a directory's walkFn call always precedes every descendant's).
	// So by the time walkFn is invoked for a given child path, that child's
	// own exclusion status (if any) was already decided by its PARENT's
	// callback, one level up — see the d.IsDir() branch below for how each
	// half is consumed.
	excludeDirs := map[string]bool{}
	excludeFiles := map[string]bool{}

	walkErr := fs.WalkDir(os.DirFS(root.diskPath), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return errFileReportShutdown
		}

		filePath := cleanArrPath(path.Join(root.diskPath, rel))

		if d.IsDir() {
			if excludeDirs[filePath] {
				// [v2.1] This directory's own name collided with a
				// sibling's, decided at its PARENT's pre-scan below (never
				// this directory's own — a directory never pre-scans
				// itself). fs.SkipDir is fs.WalkDir's only mechanism to
				// omit one specific child's whole subtree while leaving
				// every sibling's traversal untouched (binding controller
				// resolution 3: "excluded from descent"): nothing inside is
				// ever visited, or individually counted, this cycle.
				return fs.SkipDir
			}
			findings, subDirs, subFiles, excluded, err := caseCollisionsInDir(filePath, rel, root, set)
			if err != nil {
				// A pre-scan read failure is exactly as fatal to this
				// root's completeness contract as any other walk error —
				// see this function's own warnAbort-on-walkErr handling
				// below, which treats whatever this closure returns
				// uniformly.
				return err
			}
			outcome.findings = append(outcome.findings, findings...)
			if excluded > 0 {
				outcome.skipReasons[FileSkipReasonCaseTwin] += excluded
				// [FIX, v2.1] Counted here, ONCE per directory's pre-scan,
				// as proof toward heuristic (c) ("this root is not simply
				// empty/unmounted") — not per individual file visit below,
				// since a colliding SUBDIRECTORY is never individually
				// visited at all (fs.SkipDir prevents fs.WalkDir from ever
				// reaching anything inside it), so there is no later
				// per-file moment to count it at. Without this, a root
				// whose only tracked file happens to sit inside a
				// case-colliding folder — or IS itself a case-colliding
				// file — would see videoFilesSeen stay at 0 while
				// len(tracked) > 0, tripping heuristic (c)'s
				// "zero video files of any kind" abort and turning binding
				// controller resolution 3's own required outcome
				// ("a root whose only irregularities are detected
				// collisions completes with fileReport=ran") into a false
				// "skipped". Successfully LISTING a directory and finding
				// 2+ real, distinctly-named entries in it is at least as
				// strong evidence the root is live and properly mounted as
				// finding one video file would be — arguably stronger,
				// since a half-mounted share typically fails the listing
				// itself (heuristic (a)) rather than returning specific
				// named entries that then happen to collide.
				videoFilesSeen += excluded
			}
			for k := range subDirs {
				excludeDirs[k] = true
			}
			for k := range subFiles {
				excludeFiles[k] = true
			}
			return nil
		}

		if excludeFiles[filePath] {
			// [v2.1] Excluded at this file's own containing directory's
			// pre-scan, above — already counted under FileSkipReasonCaseTwin
			// AND toward videoFilesSeen there (see the pre-scan's own
			// comment on why), and already represented by the
			// case-collision finding that pre-scan produced. Never
			// classified as tracked/duplicate/orphan/skipped-* (binding
			// controller resolution 3: "excluded from ... duplicate/orphan
			// classification") — this second visit, from fs.WalkDir's own
			// real traversal, contributes nothing further.
			return nil
		}

		c := classifyFileReportPath(filePath, root, set)
		switch c.kind {
		case fileKindTracked:
			outcome.seenTracked++
			videoFilesSeen++
		case fileKindDuplicate:
			videoFilesSeen++
			outcome.findings = append(outcome.findings, fileReportFinding{
				kind: fileKindDuplicate, diskPath: filePath, displayPath: rootRelativeDisplayPath(root, rel),
				isSeries: c.isSeries, title: c.title, group: c.group, folder: c.folder,
			})
		case fileKindOrphan:
			videoFilesSeen++
			outcome.findings = append(outcome.findings, fileReportFinding{kind: fileKindOrphan, diskPath: filePath, displayPath: rootRelativeDisplayPath(root, rel)})
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

	// Heuristic (d), the per-ROOT twin of
	// warnIfInstanceTrackedSetEntirelyUnmapped: a root under which THIS
	// instance tracks NOTHING AT ALL — zero tracked files (tracked, above)
	// AND zero tracked folders (trackedFolders, above) — but the walk found
	// real video files anyway is not evidence of a legitimate empty or
	// unmanaged root (a genuinely empty/unmanaged root has nothing to walk
	// in the first place, which is exactly why this is gated on
	// videoFilesSeen > 0). It is evidence THIS SPECIFIC root has a
	// media_root_map problem the instance-wide guard cannot see, because
	// every one of the instance's OTHER tracked paths mapped fine: a key
	// typo/case-mismatch on just this one root, or the same per-instance
	// media_root_map block copy-pasted into two instances that each
	// actually manage a different root (README/compose examples show one
	// map per instance and multiple roots like /tv_shows + /anime as
	// normal). Without this guard every real file here falls through
	// classifyFileReportPath's containment check to fileKindOrphan and
	// prints as a confident kind=orphan — a healthy library (this
	// instance's or another instance's) reported as a flood of false
	// orphans, at INFO, with no WARN anywhere: the same failure
	// warnIfInstanceTrackedSetEntirelyUnmapped exists to prevent, one root
	// down from where that guard can see.
	if len(tracked) == 0 && len(trackedFolders) == 0 && videoFilesSeen > 0 {
		return warnAbort("file report: mount-problem heuristic aborted this root: this instance tracks nothing at all under it (zero tracked files, zero tracked folders) but the walk found real video files here — check media_root_map for a key typo/case-mismatch on this root, or the same map copy-pasted into another instance that actually manages it",
			"videoFilesSeen", videoFilesSeen)
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
// count siblings by: the containing tracked FOLDER (unique per movie/series
// by construction — two items never share a folder) plus, for Sonarr, the
// sub-label (a series can have several groups, one per parseable
// season/episode or containing folder).
//
// [IMPORTANT FIX] this used to key on f.title+f.group, which collapses two
// DISTINCT movies/series that happen to share a display title — same-titled
// remakes are common (e.g. two "Dune" entries) — into one group, giving a
// wrong groupCount on the one number a human uses to judge how big a
// problem is. folder is set by classifyFileReportPath from the exact
// set.folders key the file matched, so it can never collide across two
// different tracked items even when their titles (and, for Sonarr, their
// parsed episode labels) do.
func groupKey(f fileReportFinding) string {
	return f.folder + "\x00" + f.group
}

// logFileReportFinding emits the frozen vocabulary line (binding controller
// resolution 7): msg="file-report finding", kind=duplicate|orphan|
// case-collision, instance, root and path (both CUTOFFARR-side / disk
// paths), plus duplicate-only grouping attrs or [v2.1] case-collision-only
// entryType/names attrs. Logged at itemLevel — the same demotion the
// forward and reverse passes give their own per-item lines (full at
// startup/--once, debug on repeating sweeps) — never at a level of its own.
func logFileReportFinding(ctx context.Context, logger *slog.Logger, itemLevel slog.Level, inst Instance, root mediaRoot, f fileReportFinding) {
	attrs := []any{"kind", f.kind, "instance", inst.Name, "type", inst.Type, "root", root.diskPath, "path", f.diskPath}
	switch f.kind {
	case fileKindDuplicate:
		if f.isSeries {
			attrs = append(attrs, "series", f.title, "group", f.group)
		} else {
			attrs = append(attrs, "title", f.title)
		}
		attrs = append(attrs, "groupCount", f.groupCount)
	case fileKindCaseCollision:
		names := make([]string, len(f.names))
		for i, n := range f.names {
			names[i] = n.name
		}
		attrs = append(attrs, "entryType", f.entryType, "names", strings.Join(names, ", "))
	}
	logger.Log(ctx, itemLevel, "file-report finding", attrs...)
}

// --- Radarr tracked-set -------------------------------------------------

// buildRadarrTrackedSet builds this instance's whole tracked-file/folder
// picture from the ALREADY-FETCHED movie library (binding controller
// resolution 8: "reuse fetched movies... do not refetch what the cycle
// already holds") — Radarr embeds both movie.path and movieFile.path
// directly on /api/v3/movie, so this makes zero additional API calls.
//
// A movie's folder and its file are tracked INDEPENDENTLY of each other
// (deliberately, see the design note on FileSkipReasonUntrackedPath's uses
// below): each is added to the set if its own path was readable, regardless
// of whether the other one was. This bounds the damage an untrusted field on
// one movie can do — a movie with a known file path but an unknown folder
// path still has that exact file protected from being reported as its OWN
// orphan, even though its folder cannot be protected from a false-duplicate
// (really: false-orphan-of-a-sibling) misclassification. The alternative —
// refusing to track anything about a movie unless BOTH paths are known — is
// safer in one narrow sense and strictly worse in the sense that matters
// most (§2.6's "never manufacture a false orphan"): it would make the movie's
// OWN tracked file report itself as an orphan, which a human reading the
// report would reasonably read as "delete this", the one class of mistake
// this whole feature exists to never make.
//
// exclusionTagID/tagActive are honored exactly like every write-path
// function that already takes them (writer.go, decision.go): the README
// states the exclusion tag opts an item out of "everything cutoffarr does,
// in every mode, including dry-run reporting", so a tagged movie's folder is
// still tracked (its OWN file must never be misreported as its own orphan)
// but is also recorded in excludedFolders so classifyFileReportPath
// withholds any EXTRA file inside it from duplicate candidacy. A movie whose
// tags could not be read at all, while the tag is active, is treated the
// same fail-safe way evaluateMovie treats it (decision.go: tagActive &&
// m.Tags == nil) — excluded rather than risking a false "duplicate" finding
// against untrusted input.
//
// The second return value is this call's build-time skip-reason counts
// (FileSkipReasonUntrackedPath for a path that could not be read at all,
// FileSkipReasonOutsideConfiguredRoots for a path that WAS read but maps to
// none of this instance's configured roots) — the caller folds them into the
// per-instance summary's fileSkipReasons so a systematically broken/renamed
// field, or a media_root_map typo, is never invisible on an always-INFO
// line simply because every individual occurrence was itself silent or
// already logged once elsewhere.
func buildRadarrTrackedSet(logger *slog.Logger, inst Instance, roots []mediaRoot, movies []movieListElement, exclusionTagID int, tagActive bool) (instanceTrackedSet, map[string]int) {
	set := instanceTrackedSet{
		files: map[string]bool{}, folders: map[string]string{},
		excludedFolders: map[string]bool{}, distrustedFolders: map[string]bool{},
	}
	skipCounts := map[string]int{}

	// [FIX] untrackedPathExample remembers only the FIRST item that ever
	// triggers a FileSkipReasonUntrackedPath skip below (either half: an
	// absent movie.path, or hasFile true/unknown with an unreadable
	// movieFile.path) — see the deferred aggregate WARN at the bottom of
	// this function for why. This is a build-time count/example only; the
	// per-item detail is never lost, since skipCounts (and the
	// always-INFO fileSkipReasons summary it feeds) still tallies every
	// occurrence exactly.
	untrackedPathExample := ""

	for _, m := range movies {
		title := titleOrAbsent(m.Title)
		excluded := tagActive && (m.Tags == nil || containsTag(m.Tags, exclusionTagID))

		var diskFolder string
		folderKnown := false
		if m.Path != nil && strings.TrimSpace(*m.Path) != "" {
			if df, ok := mapArrPathToAnyRoot(*m.Path, roots); ok {
				diskFolder = df
				folderKnown = true
				set.folders[diskFolder] = title
				if excluded {
					set.excludedFolders[diskFolder] = true
				}
			} else {
				skipCounts[FileSkipReasonOutsideConfiguredRoots]++
			}
		} else {
			if untrackedPathExample == "" {
				untrackedPathExample = fmt.Sprintf("title=%q id=%v", title, derefOrAbsent(m.ID))
			}
			skipCounts[FileSkipReasonUntrackedPath]++
		}

		// The tracked-FILE half is keyed on movieFile.path being READABLE,
		// never on hasFile. hasFile absent is untrusted input — it could
		// mean "not yet imported" or simply mean the field wasn't parsed —
		// and gating on it the way decision.go's forward engine safely does
		// (hasFile absent -> treated as false -> "do not unmonitor") inverts
		// to UNSAFE here: "do not protect a file I can see" is the exact
		// false-orphan-of-your-own-file mistake this file's own doc comment
		// exists to prevent.
		//
		// [IMPORTANT/plan-mandated FIX, round 3] The distrust branch below
		// draws the SYMMETRIC conclusion: only an EXPLICIT hasFile=false is
		// trustworthy evidence that "no readable movieFile.path" means the
		// movie genuinely has no file. hasFile ABSENT (nil) is exactly as
		// untrustworthy here as it is above — it must never be read as "no
		// file exists" any more than it may be read as "a file exists".
		// Gating the distrust branch on hasFile==true alone (as it used to)
		// let the nil case fall all the way through with the folder
		// registered and set.files untouched and NOT distrusted: a real file
		// on disk under that folder then matched the containment branch and
		// printed as a confident kind=duplicate naming itself — the
		// identical mistake fixed below for hasFile=true, just reached
		// through the nil door instead. Against a real Radarr (which always
		// emits hasFile) this changes nothing; it only matters against the
		// untrusted-input case this whole file exists to defend against.
		if m.MovieFile != nil && m.MovieFile.Path != nil && strings.TrimSpace(*m.MovieFile.Path) != "" {
			if diskFile, ok := mapArrPathToAnyRoot(*m.MovieFile.Path, roots); ok {
				set.files[diskFile] = true
			} else {
				skipCounts[FileSkipReasonOutsideConfiguredRoots]++
			}
		} else if m.HasFile == nil || *m.HasFile {
			// [IMPORTANT/plan-mandated FIX] hasFile=true (or unknown) with an
			// unreadable path means set.files was never told about this
			// movie's own file — so, without distrustedFolders, the movie's
			// own real file on disk falls through classifyFileReportPath's
			// folder-containment branch and prints as a confident
			// kind=duplicate (its groupCount=1 "sibling" is itself). The
			// folder stays registered above (containment must still find it,
			// so this never becomes a false ORPHAN either) but is now marked
			// distrusted so extras — including the movie's own
			// misclassified file — resolve to skipped-untrusted instead.
			if untrackedPathExample == "" {
				untrackedPathExample = fmt.Sprintf("title=%q id=%v", title, derefOrAbsent(m.ID))
			}
			skipCounts[FileSkipReasonUntrackedPath]++
			if folderKnown {
				set.distrustedFolders[diskFolder] = true
			}
		}
	}

	warnAggregatedUntrackedPaths(logger, inst, skipCounts[FileSkipReasonUntrackedPath], untrackedPathExample)
	return set, skipCounts
}

// warnAggregatedUntrackedPaths is the [FIX] per-item-WARN-flood fix: every
// FileSkipReasonUntrackedPath occurrence used to log its own logger.Warn
// call DURING tracked-set build — before either
// warnIfInstanceTrackedSetEntirelyUnmapped or the per-root mount heuristics
// ever get a chance to abort. If a speculatively-added *arr field name
// (movie.path/movieFile.path/series.path/episodeFile.path) turns out wrong
// at the live gate — the risk this whole untrusted-input posture exists
// for — every item in the library would warn, potentially thousands of
// lines per sweep, drowning the one abort WARN that actually matters. This
// is ONE aggregated WARN per instance per build call, mirroring
// warnIfAnyTrackedPathUnmapped's existing aggregation for the sibling
// FileSkipReasonOutsideConfiguredRoots reason: the exact count is never
// lost (skipCounts, and the always-INFO fileSkipReasons summary it feeds,
// still tally every occurrence), only the WARN volume is bounded.
func warnAggregatedUntrackedPaths(logger *slog.Logger, inst Instance, count int, example string) {
	if count == 0 {
		return
	}
	logger.Warn("file report: some tracked paths were unreadable and could not be fully protected from misclassification — see the per-instance summary's fileSkipReasons count for the exact total",
		"instance", inst.Name, "type", inst.Type, "count", count, "example", example)
}

// --- scheduling + the off/skipped/ran summary vocabulary --------------------

// fileReportOptions is what a CYCLE says about the file report: whether to
// run it at all. It is the same zero-value-off shape as reverseOptions
// (reverse.go), for the same reason (binding controller resolution 8): the
// zero value is "do not run", which is what a webhook cycle or a --only-id
// scoped run gets simply by never setting this field, and what a caller that
// forgot about this phase entirely gets too.
//
// Unlike reverseOptions there is no second field for a write switch: this
// pass never writes anything, ever, so there is nothing for a cycle to
// authorize beyond running it at all.
type fileReportOptions struct {
	enabled bool
}

// fullScanFileReportOptions is what a FULL-LIBRARY cycle asks for: the
// startup scan, the reconciliation sweep, and a full (non---only-id) --once
// run. It takes no config, unlike fullScanReverseOptions: per-instance
// opt-in lives entirely in Instance.MediaRootMap, discovered fresh every
// cycle from cfg.Instances, so there is nothing else here to gate on.
func fullScanFileReportOptions() fileReportOptions {
	return fileReportOptions{enabled: true}
}

// fileReportCounts is one instance's Phase 11 accounting for one cycle.
//
// configured records whether media_root_map was present for this instance at
// all — the "off" half of resolution 1's three-way state. anySkipped records
// whether ANY configured root aborted (the mount-problem heuristic, or a
// walk error) — resolution 4's "one root aborting never affects other
// roots/instances" means duplicates/orphans still reflect whatever roots DID
// complete, even on a partially-skipped instance; anySkipped exists so the
// human reading the summary knows the totals are not the WHOLE story.
type fileReportCounts struct {
	configured bool
	anySkipped bool

	duplicates  int
	orphans     int
	skipReasons map[string]int

	// caseCollisions is [v2.1]: the number of case-collision FINDINGS (one
	// per colliding GROUP, binding controller resolution 2 — never one per
	// colliding NAME, which is what FileSkipReasonCaseTwin's own count under
	// skipReasons already tallies instead). Counted the same way duplicates/
	// orphans are, in mergeFileReportOutcome, by kind.
	caseCollisions int

	// findings is Phase 12's addition: the SAME findings duplicates/orphans
	// (and, since v2.1, caseCollisions) already tally, kept as data.
	// Appended alongside the existing counting in mergeFileReportOutcome,
	// never in place of it, so the frozen "file report" summary line
	// (logFileReportSummary) is untouched.
	findings []fileReportFinding
}

// state renders the three-way distinction the binding resolution requires to
// be tellable apart at a glance: "off" (never configured, so this cycle
// looked at nothing), "skipped" (configured, but at least one root could not
// be trusted this cycle), and "ran" (every configured root completed,
// whether or not it found anything — a clean duplicates=0 orphans=0 line IS
// evidence the report ran, not silence).
func (c fileReportCounts) state() string {
	if !c.configured {
		return "off"
	}
	if c.anySkipped {
		return "skipped"
	}
	return "ran"
}

// logFileReportSummary logs the per-instance summary line, msg="file
// report", per binding controller resolution 7's frozen vocabulary
// (fileReport=ran|skipped|off, duplicates=N, orphans=N, fileSkipReasons=).
//
// The off state is deliberately a SEPARATE, lower-severity log call rather
// than the same INFO line with different attrs (binding controller
// resolution 1: "off is logged once per full cycle at debug ... distinct
// from skipped ... and from duplicates=0 orphans=0"): an instance that never
// configured media_root_map should never see a line about a feature it did
// not opt into at its everyday log level, and — because this is a single
// call per instance per cycle, never a per-item loop — "once per full cycle"
// falls out for free rather than needing its own dedup bookkeeping. ran and
// skipped are both INFO (Phase 8's noise budget: "summary always INFO"):
// skipped's own per-root cause was already WARNed about by
// evaluateFileReportRoot the moment it happened, so the summary line itself
// only needs to be visible, not alarming twice.
func logFileReportSummary(logger *slog.Logger, inst Instance, c fileReportCounts) {
	state := c.state()
	attrs := []any{"instance", inst.Name, "type", inst.Type, "fileReport", state}
	if state == "off" {
		logger.Debug("file report", attrs...)
		return
	}
	attrs = append(attrs, "duplicates", c.duplicates, "orphans", c.orphans, "caseCollisions", c.caseCollisions, "fileSkipReasons", formatSkipCounts(c.skipReasons))
	logger.Info("file report", attrs...)
}

// runRadarrFileReport is the Radarr entry point for one instance's file
// report: build the tracked set from the already-fetched library (no extra
// API calls), then evaluate every configured root in turn. One root's
// mount-problem abort never stops the others (resolution 4) — the loop
// simply keeps going and folds the aborted root's contribution to
// c.anySkipped, while every OTHER root's duplicates/orphans/skipReasons are
// still added to the total.
func runRadarrFileReport(ctx context.Context, logger *slog.Logger, itemLevel slog.Level, inst Instance, movies []movieListElement, exclusionTagID int, tagActive bool) fileReportCounts {
	roots := mediaRootsFor(inst)
	if len(roots) == 0 {
		return fileReportCounts{}
	}

	set, buildSkipCounts := buildRadarrTrackedSet(logger, inst, roots, movies, exclusionTagID, tagActive)

	c := fileReportCounts{configured: true, skipReasons: map[string]int{}}
	for reason, n := range buildSkipCounts {
		c.skipReasons[reason] += n
	}

	// [CRITICAL FIX] Instance-wide symmetric guard, checked before any root
	// is walked — see warnIfInstanceTrackedSetEntirelyUnmapped's doc comment.
	if warnIfInstanceTrackedSetEntirelyUnmapped(logger, inst, len(movies), set) {
		c.anySkipped = true
		return c
	}
	warnIfAnyTrackedPathUnmapped(logger, inst, buildSkipCounts)

	for _, root := range roots {
		if ctx.Err() != nil {
			logger.Info("shutdown requested: abandoning this instance's file report; the remaining roots are not evaluated this cycle",
				"instance", inst.Name, "type", inst.Type)
			c.anySkipped = true
			break
		}
		outcome := evaluateFileReportRoot(ctx, logger, itemLevel, inst, root, set)
		mergeFileReportOutcome(&c, outcome)
	}
	return c
}

// warnIfInstanceTrackedSetEntirelyUnmapped is the instance-wide, upstream
// twin of heuristic (b2) (evaluateFileReportRoot): (b2) catches one ROOT
// whose tracked FILES are all missing while its tracked FOLDERS stay
// populated, but every per-root guard — (b), (b2), and (c) alike — is gated
// on the tracked set having SOMETHING in it for that root, so none of them
// can ever fire when the tracked set built for the WHOLE INSTANCE is
// completely empty (set.files AND set.folders both empty) despite the
// library itself being non-empty.
//
// [CRITICAL FIX] That state is reachable in the real world exactly the way
// the per-root guards are — a media_root_map key typo (case mismatch:
// resolution 2 makes the comparison deliberately case-SENSITIVE, so
// "/Movies" configured against a live Radarr reporting "/movies" maps
// NOTHING), or a live-*arr field name this project only added speculatively
// (movie.path / movieFile.path / episodeFile.path) turning out wrong. Either
// way, every real, healthy, tracked file on disk then gets walked, matches
// nothing tracked, and prints as a confident kind=orphan — the WHOLE
// library, at INFO, with no WARN anywhere: precisely the "healthy library
// reported as thousands of orphans" the mount-problem heuristic exists to
// prevent (Phase 10 binding inputs), just upstream of where any per-root
// heuristic ever gets a chance to run.
//
// Aborts the WHOLE instance (every root, not just one) with a WARN naming
// media_root_map, mirroring evaluateFileReportRoot's own warnAbort shape —
// callers must treat a true return exactly like any other aborted-instance
// case (anySkipped=true, no roots evaluated).
func warnIfInstanceTrackedSetEntirelyUnmapped(logger *slog.Logger, inst Instance, libraryCount int, set instanceTrackedSet) bool {
	if libraryCount == 0 || len(set.files) > 0 || len(set.folders) > 0 {
		return false
	}
	logger.Warn("file report: aborting this instance: the library is non-empty but not a single tracked path mapped to any configured root — check media_root_map for a typo or case mismatch (comparison is deliberately case-sensitive)",
		"instance", inst.Name, "type", inst.Type, "libraryCount", libraryCount)
	return true
}

// warnIfAnyTrackedPathUnmapped is the binding "skip+warn counted under a
// skip reason" requirement's WARN half, for the case
// warnIfInstanceTrackedSetEntirelyUnmapped does NOT already cover: at least
// one, but not every, tracked path failed to map. buildRadarrTrackedSet/
// buildSonarrTrackedSet already COUNT this (FileSkipReasonOutsideConfiguredRoots,
// surfaced on the always-INFO summary), but counting alone was never itself
// a WARN, so a media_root_map typo affecting a MINORITY of the library was
// invisible unless a human happened to be reading the summary's
// fileSkipReasons. This is ONE aggregated WARN per instance — never one per
// unmapped item, which would flood the log for every legitimately
// out-of-scope item on a multi-root instance (the same reasoning the
// original, count-only fix already applied to the per-item case).
func warnIfAnyTrackedPathUnmapped(logger *slog.Logger, inst Instance, buildSkipCounts map[string]int) {
	n := buildSkipCounts[FileSkipReasonOutsideConfiguredRoots]
	if n == 0 {
		return
	}
	logger.Warn("file report: some tracked paths did not map to any configured root — check media_root_map for a typo or case mismatch (comparison is deliberately case-sensitive)",
		"instance", inst.Name, "type", inst.Type, "count", n)
}

// mergeFileReportOutcome folds one root's outcome into the instance-wide
// accounting, shared by both the Radarr and Sonarr entry points so the two
// cannot drift on how a skipped root, or a duplicate/orphan/[v2.1]
// case-collision split, is counted.
//
// [FIX, v2.1] This used to be an if/else ("duplicate" vs "else -> orphan"),
// which silently miscounted every case-collision finding as an orphan the
// moment this kind was added to the SAME outcome.findings slice — an
// exhaustive switch, with every recognized kind named explicitly, is what
// TestMergeFileReportOutcome_CountsCaseCollisionsSeparatelyFromOrphans
// exists to pin against regressing back to.
func mergeFileReportOutcome(c *fileReportCounts, outcome fileReportRootOutcome) {
	if outcome.skipped {
		c.anySkipped = true
		return
	}
	for _, f := range outcome.findings {
		switch f.kind {
		case fileKindDuplicate:
			c.duplicates++
		case fileKindOrphan:
			c.orphans++
		case fileKindCaseCollision:
			c.caseCollisions++
		}
	}
	// Phase 12: the findings themselves, carried alongside the counts above
	// — see fileReportCounts.findings' own doc comment.
	c.findings = append(c.findings, outcome.findings...)
	for reason, n := range outcome.skipReasons {
		c.skipReasons[reason] += n
	}
}

// --- Sonarr tracked-set -------------------------------------------------

// buildSonarrTrackedSet builds this instance's tracked-file/folder picture
// from the already-fetched series library PLUS one fresh /api/v3/episodefile
// fetch per relevant series — unlike Radarr, Sonarr never embeds a file's
// path anywhere but that endpoint (see episodeFileElement's doc comment,
// sonarr.go), and the forward decision engine's own /episodefile fetch is
// lazy and conditional (only series with a rule-1-6-passing candidate
// season), so it cannot be reused here: this report needs a COMPLETE
// tracked-file picture across the whole library, monitored or not, which the
// forward engine's optimization never builds.
//
// "Relevant" is decided per series BEFORE spending a fetch on it: a series
// whose own path is KNOWN and maps to none of the configured roots
// contributes nothing regardless of what its episode files say, so the
// fetch is skipped entirely (bounding the added API cost to series that
// could plausibly matter). A series whose path is unknown is fetched
// defensively — see buildRadarrTrackedSet's identical reasoning for why an
// unreadable folder path must never silently drop the file-level protection
// that only needs the SERIES ID, not the folder, to obtain.
//
// mismatchedSeasons is keyed by seriesID -> season number (built by the
// caller from this cycle's own forward decisions, binding controller
// resolution 5); this function re-keys it onto the series' DISK folder path,
// which is the key classifyFileReportPath actually looks up by.
//
// ok=false means the tracked set could not be completely built — an
// /episodefile fetch failed, or a shutdown interrupted the loop — and the
// caller must not report anything for this instance this cycle: the same
// completeness contract fetchWantedCutoffPages (radarr.go) applies to a
// partial wanted set applies here to a partial tracked set, and for the same
// reason (a partial tracked set manufactures false orphans).
//
// exclusionTagID/tagActive and the returned skip-count map mirror
// buildRadarrTrackedSet's identical additions — see its doc comment.
func buildSonarrTrackedSet(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance, roots []mediaRoot, series []seriesElement, mismatchedSeasons map[int]map[int]bool, exclusionTagID int, tagActive bool) (instanceTrackedSet, map[string]int, bool) {
	set := instanceTrackedSet{
		files:             map[string]bool{},
		folders:           map[string]string{},
		seriesFolder:      map[string]bool{},
		mismatchedSeasons: map[string]map[int]bool{},
		excludedFolders:   map[string]bool{},
		distrustedFolders: map[string]bool{},
	}
	skipCounts := map[string]int{}

	// [FIX] untrackedPathExample, and the deferred warnAggregatedUntrackedPaths
	// call below, are buildRadarrTrackedSet's per-item-WARN-flood fix
	// (see its doc comment) applied here: series id absent, series path
	// absent, and an episode file with an absent path all used to log
	// their own logger.Warn call, per occurrence. Deferred so it still
	// fires — with whatever count/example was accumulated so far — on
	// every return path (shutdown, an episodefile fetch failure, or a
	// normal completion), not just the happy one.
	untrackedPathExample := ""
	defer func() {
		warnAggregatedUntrackedPaths(logger, inst, skipCounts[FileSkipReasonUntrackedPath], untrackedPathExample)
	}()

	for _, s := range series {
		if ctx.Err() != nil {
			logger.Info("shutdown requested: abandoning this instance's file-report tracked-set build",
				"instance", inst.Name, "type", inst.Type)
			return set, skipCounts, false
		}

		title := titleOrAbsent(s.Title)
		excluded := tagActive && (s.Tags == nil || containsTag(s.Tags, exclusionTagID))

		// relevant defaults true (fetch defensively) and is narrowed to false
		// ONLY when the series' path is both known and confirmed to map to
		// none of this instance's configured roots.
		relevant := true
		var diskFolder string
		folderKnown := false
		if s.Path != nil && strings.TrimSpace(*s.Path) != "" {
			df, ok := mapArrPathToAnyRoot(*s.Path, roots)
			relevant = ok
			if ok {
				diskFolder = df
				folderKnown = true
				set.folders[diskFolder] = title
				set.seriesFolder[diskFolder] = true
				if excluded {
					set.excludedFolders[diskFolder] = true
				}
				if s.ID == nil {
					// [IMPORTANT FIX] A series id is required to ever fetch
					// its episode files (the only Sonarr endpoint that
					// carries a file's path). The prior fix left the folder
					// entirely UNREGISTERED here to avoid a false-duplicate
					// flood — but that just relabelled the flood: with the
					// folder absent from set.folders, containingTrackedFolder
					// can never find it, so EVERY real episode on disk fell
					// through to fileKindOrphan instead — a false-ORPHAN
					// flood, the outcome §2.6 treats as the worse direction.
					// The folder now stays registered (so containment finds
					// it) AND is marked distrusted, routing its files to
					// skipped-untrusted — neither duplicate nor orphan.
					if untrackedPathExample == "" {
						untrackedPathExample = fmt.Sprintf("title=%q seriesId=absent", title)
					}
					skipCounts[FileSkipReasonUntrackedPath]++
					set.distrustedFolders[diskFolder] = true
				}
			} else {
				skipCounts[FileSkipReasonOutsideConfiguredRoots]++
			}
		} else {
			if untrackedPathExample == "" {
				untrackedPathExample = fmt.Sprintf("title=%q seriesId=%v", title, derefOrAbsent(s.ID))
			}
			skipCounts[FileSkipReasonUntrackedPath]++
		}

		if s.ID == nil || !relevant {
			continue
		}

		files, ok := fetchEpisodeFiles(ctx, logger, client, inst, *s.ID)
		if !ok {
			logger.Warn("file report: skipping this instance: an episodefile fetch failed, and a partial tracked set would manufacture false orphans",
				"instance", inst.Name, "type", inst.Type, "series", title, "seriesId", *s.ID)
			return set, skipCounts, false
		}

		// seasonFileCounts feeds seasonsWithFileCountMismatch below: this
		// fetch's own per-season file count, compared against
		// statistics.episodeFileCount already on the /series response.
		seasonFileCounts := map[int]int{}
		for _, f := range files {
			if f.Path == nil || strings.TrimSpace(*f.Path) == "" {
				// [IMPORTANT/plan-mandated FIX] An episode file record with
				// no readable path means this fetch cannot say WHICH real
				// file on disk it corresponds to — unlike Radarr (at most
				// one file per movie), there is no way to scope the
				// distrust to "just that file" without knowing its
				// location, so the whole series folder is marked
				// distrusted rather than guessing that only this one
				// record is affected.
				if untrackedPathExample == "" {
					untrackedPathExample = fmt.Sprintf("series=%q seriesId=%v fileId=%v", title, *s.ID, derefOrAbsent(f.ID))
				}
				skipCounts[FileSkipReasonUntrackedPath]++
				if folderKnown {
					set.distrustedFolders[diskFolder] = true
				}
				continue
			}
			if f.SeasonNumber != nil {
				seasonFileCounts[*f.SeasonNumber]++
			}
			if diskFile, ok := mapArrPathToAnyRoot(*f.Path, roots); ok {
				set.files[diskFile] = true
			} else {
				skipCounts[FileSkipReasonOutsideConfiguredRoots]++
			}
		}

		// [IMPORTANT FIX] Union the forward engine's own per-cycle mismatch
		// index (mismatchedSeasons — built from allDecisions, which only
		// ever contains a MONITORED series' rule-1-6 candidate seasons)
		// with a mismatch this function can detect ITSELF, from data
		// already in hand: comparing THIS fetch's own per-season file
		// count against statistics.episodeFileCount already on the
		// already-fetched /series response. The forward index alone
		// systematically misses every unmonitored series — decision.go's
		// rule 1 (series half) `continue`s before evaluateSeries ever runs
		// for one, so it never appears in allDecisions — which is most of
		// a mature library by this project's own design (its purpose is
		// to unmonitor finished series). Either source finding a mismatch
		// is enough to withhold the season's folder from duplicate
		// candidacy.
		if folderKnown {
			bySeason := map[int]bool{}
			if s.ID != nil {
				for sn := range mismatchedSeasons[*s.ID] {
					bySeason[sn] = true
				}
			}
			for sn := range seasonsWithFileCountMismatch(s.Seasons, seasonFileCounts) {
				bySeason[sn] = true
			}
			if len(bySeason) > 0 {
				set.mismatchedSeasons[diskFolder] = bySeason
			}
		}
	}

	return set, skipCounts, true
}

// seasonsWithFileCountMismatch compares one series' OWN /episodefile fetch
// (grouped by season number, seasonFileCounts) against the season
// statistics already on the /series response (season.Statistics.
// EpisodeFileCount) — the same "<" comparison decision.go's forward engine
// uses for ReasonSeasonFileCountMismatch (statsFileCountFor[sn] vs
// len(seasonFiles)), applied here to buildSonarrTrackedSet's OWN fetch
// rather than the forward engine's. See buildSonarrTrackedSet's doc comment
// on why the forward engine's own per-cycle index cannot be the only
// source. A season with no statistics (or an unparseable one) is left
// alone — absence is not evidence of a mismatch, only the inability to
// compare — and MORE files than statistics claims is not flagged either:
// only an UNDER-report is the dangerous direction (statistics promising
// more than this fetch actually returned).
func seasonsWithFileCountMismatch(seasons *[]seriesSeasonElement, seasonFileCounts map[int]int) map[int]bool {
	mismatched := map[int]bool{}
	if seasons == nil {
		return mismatched
	}
	for _, season := range *seasons {
		if season.SeasonNumber == nil || season.Statistics == nil || season.Statistics.EpisodeFileCount == nil {
			continue
		}
		if seasonFileCounts[*season.SeasonNumber] < *season.Statistics.EpisodeFileCount {
			mismatched[*season.SeasonNumber] = true
		}
	}
	return mismatched
}

// runSonarrFileReport is the Sonarr entry point for one instance's file
// report: build the tracked set (the one part of this pass that costs extra
// API calls — see buildSonarrTrackedSet), then evaluate every configured
// root exactly like the Radarr entry point does.
func runSonarrFileReport(ctx context.Context, logger *slog.Logger, itemLevel slog.Level, client *APIClient, inst Instance, series []seriesElement, mismatchedSeasons map[int]map[int]bool, exclusionTagID int, tagActive bool) fileReportCounts {
	roots := mediaRootsFor(inst)
	if len(roots) == 0 {
		return fileReportCounts{}
	}

	set, buildSkipCounts, ok := buildSonarrTrackedSet(ctx, logger, client, inst, roots, series, mismatchedSeasons, exclusionTagID, tagActive)
	c := fileReportCounts{configured: true, skipReasons: map[string]int{}}
	for reason, n := range buildSkipCounts {
		c.skipReasons[reason] += n
	}
	if !ok {
		c.anySkipped = true
		return c
	}

	// [CRITICAL FIX] Instance-wide symmetric guard, checked before any root
	// is walked — see warnIfInstanceTrackedSetEntirelyUnmapped's doc comment.
	if warnIfInstanceTrackedSetEntirelyUnmapped(logger, inst, len(series), set) {
		c.anySkipped = true
		return c
	}
	warnIfAnyTrackedPathUnmapped(logger, inst, buildSkipCounts)

	for _, root := range roots {
		if ctx.Err() != nil {
			logger.Info("shutdown requested: abandoning this instance's file report; the remaining roots are not evaluated this cycle",
				"instance", inst.Name, "type", inst.Type)
			c.anySkipped = true
			break
		}
		outcome := evaluateFileReportRoot(ctx, logger, itemLevel, inst, root, set)
		mergeFileReportOutcome(&c, outcome)
	}
	return c
}

// buildMismatchedSeasonsIndex folds this cycle's forward Sonarr decisions
// into the seriesID -> season-number index buildSonarrTrackedSet needs
// (binding controller resolution 5): seasons the forward engine already
// distrusted for an episode-file-count mismatch are excluded from duplicate
// candidacy, never guessed either way.
func buildMismatchedSeasonsIndex(decisions []seasonDecision) map[int]map[int]bool {
	index := map[int]map[int]bool{}
	for _, d := range decisions {
		if d.reason != ReasonSeasonFileCountMismatch {
			continue
		}
		if index[d.seriesID] == nil {
			index[d.seriesID] = map[int]bool{}
		}
		index[d.seriesID][d.season] = true
	}
	return index
}
