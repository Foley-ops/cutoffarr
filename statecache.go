package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// statecache.go is [v0.2.0]'s warm-start display cache, and the FIRST thing to
// understand about it is what it is not.
//
// THE STATELESSNESS CONSTITUTION IS UNCHANGED. Every decision this program
// makes, every write it composes, and every re-verification an action performs
// still derives ONLY from live data re-fetched within that cycle (binding
// controller note 3: "no caching of decisions, payloads, or cross-check
// verdicts"). This file caches NONE of that. What it holds is one copy of the
// bytes GET /api/stats last served — the numbers a human GLANCES at — so that a
// container restart shows the last sweep's dashboard instead of a blank page
// for the minutes a full sweep takes.
//
// The property is structural, not a promise in a comment:
//
//   - loadStateCache and writeStateCache are the whole published surface, and
//     the ONLY file permitted to name either is daemon.go — startup wiring for
//     the load, end-of-full-cycle wiring for the write. No engine, evaluator,
//     executor, cross-check or action path can reach them at all
//     (TestTree_TheStateCacheIsReachableOnlyFromTheDaemonsStartupWiring,
//     statecache_test.go, which parses the tree the same way the executor
//     reachability audit does).
//   - what the loader hands back goes to exactly one consumer,
//     statsStore.warmStart (stats.go), which marks every instance it loads
//     stale: true so nothing on the page can present it as this process's own
//     work.
//   - an action clicked against a warm-stale row is authorized against that row
//     and then re-derived from LIVE data exactly as it always was — the cache
//     buys the row's EXISTENCE, never its authority
//     (TestAction_OnAWarmStaleFinding_StillReVerifiesLiveAndRefusesWhenLiveDisagrees).
//
// THE FILESYSTEM-MUTATION AUDIT AMENDMENT. Phase 11 banned every filesystem
// mutation API tree-wide and v2.2 admitted exactly one file (actions.go, for
// the trash move) to a narrow allowlist. This file is the second and it may
// make exactly two calls — os.WriteFile and os.Rename — both inside
// writeStateCache and both confined to the config directory by construction
// (stateCachePaths). os.Remove and os.RemoveAll remain banned here as
// everywhere; see stateCacheTempFileName for what that costs and why the cost
// is bounded at one file.

// stateCacheSchemaVersion is the on-disk format's version. The loader refuses
// any other value outright rather than guessing at a field's meaning: a cache
// is a convenience, and a convenience that has to be interpreted is worth less
// than a cold start.
//
// It is bumped whenever a field's MEANING changes — never for an added field,
// which an old loader ignores and a new loader reads as absent, both of which
// are already handled.
const stateCacheSchemaVersion = 1

// stateCacheFileName is the cache's name inside the config directory. It sits
// beside config.yml deliberately: that directory is the one persistent,
// writable, container-mounted place this program already knows about, it is
// never a media root, and an operator who deletes this file loses nothing but
// one restart's warm start.
const stateCacheFileName = "state-cache.json"

// stateCacheTempFileName is the file every write goes through before being
// renamed into place. The name is FIXED rather than unique-per-write
// (os.CreateTemp's own shape), and that is a deliberate trade:
//
//   - os.Remove is banned in this codebase, here included, so nothing can ever
//     clean up a temp file that was created and then not renamed (a process
//     killed mid-write, a disk that filled up between the write and the
//     rename).
//   - with a unique name, every such failure leaves one orphan behind forever,
//     and the disk most likely to keep failing is the one least able to afford
//     an unbounded pile of them.
//   - with this fixed name, the next write simply overwrites it. The litter is
//     bounded at exactly one file, whose name says what it is, and which is
//     safe to delete by hand at any time.
//
// The rename itself is what makes a reader's view atomic: a torn or partial
// temp file never carries the final name, so a loader either sees the previous
// complete cache or the new complete one.
const stateCacheTempFileName = "state-cache.json.tmp"

// stateCacheDocument is the on-disk shape: the GET /api/stats body verbatim,
// plus the two fields that make a file on disk safe to trust — the schema
// version, and the clock reading of the cycle that produced it.
//
// Every load-bearing field is a POINTER, following this project's
// pointer-decode convention (see systemStatusResponse, connectivity.go): a key
// that is entirely ABSENT from the JSON (nil) is a file that is not a cutoffarr
// state cache — or is one truncated before that key — and must be refused,
// while a key present at its zero value is a legitimate value. For
// schemaVersion the difference is "not a cache" versus "version 0"; for
// instances it is "no such key" versus a real cache from a daemon whose cycle
// recorded nothing yet.
//
// DryRun/GUIActions/ReverseScanRemonitor/Version are written so the file is
// exactly what the endpoint served — a human or a script reading it gets the
// whole picture — and are NEVER read back into the running process. They are
// live facts about THIS process's config and build, and a cache that could
// change what the dry-run badge says would be a cache that changes behavior.
// TestLoadStateCache_NeverRestoresTheProcessesOwnSwitchesFromDisk pins that.
type stateCacheDocument struct {
	SchemaVersion *int       `json:"schemaVersion"`
	WrittenAt     *time.Time `json:"writtenAt"`

	Instances *[]instanceStatsView `json:"instances"`

	DryRun               bool   `json:"dryRun"`
	GUIActions           bool   `json:"guiActions"`
	ReverseScanRemonitor bool   `json:"reverseScanRemonitor"`
	Version              string `json:"version"`
}

// stateCacheProbe is the second decode pass over the SAME bytes, and it exists
// because instanceStatsView is a wire OUTPUT type whose Name/Type are plain
// strings: decoded into it, "the key was absent" and "the key was the empty
// string" are the same thing. An instance with no name at all cannot key the
// stats store and must be refused rather than silently stored under "", so the
// one field that decides that is decoded again here as a pointer.
type stateCacheProbe struct {
	Instances *[]struct {
		Name *string `json:"name"`
		Type *string `json:"type"`
	} `json:"instances"`
}

// stateCachePaths returns the cache's final and temporary paths inside dir,
// and whether there is a directory to use at all.
//
// It is the containment guard the audit amendment owes: both names are bare
// constants with no separator in them, so filepath.Join can only ever produce a
// direct child of dir — the config directory — and never a path that a media
// root, a finding, or anything else derived from an *arr could steer. An empty
// dir (a Config built in memory rather than loaded from a file) returns ok
// false: a program that does not know where its config lives must not guess a
// directory to write into.
func stateCachePaths(dir string) (final, tmp string, ok bool) {
	if dir == "" {
		return "", "", false
	}
	clean := filepath.Clean(dir)
	return filepath.Join(clean, stateCacheFileName), filepath.Join(clean, stateCacheTempFileName), true
}

// stateCacheDirInsideAMediaRoot reports the disk path of the first configured
// media root that CONTAINS dir, if any.
//
// It is the other half of the containment the audit amendment owes, and the
// half stateCachePaths cannot supply. stateCachePaths proves the cache's two
// paths are children of the config directory; this proves the config directory
// is not itself inside a media library. ConfigDir is only filepath.Dir of
// whatever --config was pointed at (config.go), so nothing but this stops
// `--config /data/media/Movies/config.yml` from dropping a file into the user's
// library at the end of every sweep — the precise scenario the amendment's own
// argument ("actions.go writes to a MEDIA root … statecache.go writes ONE file
// to the CONFIG directory") assumes away.
//
// Matching reuses hasPathPrefix (filereport.go), so it is lexical, case
// sensitive and on path-SEGMENT boundaries: /data/media-config is not inside
// /data/media. An instance with no media_root_map contributes no roots at all —
// that map is the file report's opt-in switch and an absent one is the OFF
// state (binding controller resolution 1).
func stateCacheDirInsideAMediaRoot(dir string, instances []Instance) (string, bool) {
	if dir == "" {
		return "", false
	}
	// Resolved against the working directory FIRST (round-3 review fix). The
	// media roots are absolute by validation, and ConfigDir is only
	// filepath.Dir of whatever --config was pointed at — so
	// `cd /data/media/Movies && cutoffarr --config config.yml` gave this
	// function "." to compare against "/data/media/Movies", which can never
	// match. The guard did not refuse, did not warn, and the cache landed in
	// the user's library at the end of every sweep: the whole failure this
	// function exists to prevent, reachable by typing a relative path.
	//
	// Absolutizing HERE rather than only at the one place that fills ConfigDir
	// in is deliberate: this is the function whose answer is load-bearing, and
	// it must be right for any caller — a Config assembled in memory, a future
	// second source of directories — not only for LoadConfig's output. A
	// filepath.Abs that fails (an unreadable working directory) falls back to
	// the raw string, which is the strictly more careful of the two available
	// wrong answers: a relative path still matches a relative root, and
	// nothing here can invent containment that is not there.
	resolved := dir
	if abs, err := filepath.Abs(dir); err == nil {
		resolved = abs
	}
	clean := cleanArrPath(filepath.ToSlash(resolved))
	for _, inst := range instances {
		for _, root := range mediaRootsFor(inst) {
			if hasPathPrefix(clean, root.diskPath) {
				return root.diskPath, true
			}
		}
	}
	return "", false
}

// writeStateCache writes snap to dir/state-cache.json, atomically, at the end
// of one full cycle.
//
// EVERY failure here is a WARN and a return. Not an error return, not a retry,
// not a fatal: the cache is display-only, the cycle that produced it has
// already done its real work, and a daemon that stopped reconciling because a
// convenience file could not be written would be trading the thing that matters
// for the thing that does not. The next full cycle tries again on its own.
func writeStateCache(logger *slog.Logger, dir string, snap statsResponse, at time.Time) {
	final, tmp, ok := stateCachePaths(dir)
	if !ok {
		// Not a warning: a Config with no directory behind it is a legitimate
		// state (an in-memory config, a --once run), and it means only that
		// there is no warm start to save.
		return
	}

	doc := stateCacheDocument{
		SchemaVersion:        intPtrValue(stateCacheSchemaVersion),
		WrittenAt:            timePtrValue(at.UTC()),
		Instances:            &snap.Instances,
		DryRun:               snap.DryRun,
		GUIActions:           snap.GUIActions,
		ReverseScanRemonitor: snap.ReverseScanRemonitor,
		Version:              snap.Version,
	}
	// Indented, because this file is meant to be readable by the human whose
	// dashboard it backs: "safe to delete anytime" is only a real invitation if
	// they can see what they are deleting.
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		logger.Warn("the dashboard's warm-start cache could not be encoded, so this cycle's snapshot was not saved; the dashboard still shows live data and the next full cycle will try again",
			"error", err, "path", final)
		return
	}
	body = append(body, '\n')

	// 0o600: this file mirrors what an unauthenticated LAN endpoint already
	// serves, so it carries no secret — but it sits in the same directory as
	// config.yml, which does, and matching that directory's own posture costs
	// nothing.
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		logger.Warn("the dashboard's warm-start cache could not be written, so this cycle's snapshot was not saved; the dashboard still shows live data and the next full cycle will try again",
			"error", err, "path", tmp)
		return
	}
	if err := os.Rename(tmp, final); err != nil {
		logger.Warn("the dashboard's warm-start cache could not be moved into place, so this cycle's snapshot was not saved; a stale temporary file may remain beside the config and is safe to delete",
			"error", err, "from", tmp, "to", final)
		return
	}
	// DEBUG, not INFO: this happens at the end of every full cycle forever, and
	// the daemon's noise budget is reserved for lines that say something a
	// human needs (idleCycleAllowedInfo, daemon_test.go, is that budget written
	// down). The failures above are WARNs precisely because they do.
	//
	// No path attr, deliberately, and it must stay that way: this line is
	// printed inside every reconciliation sweep, and
	// TestDaemon_IdleCycleWithAWebUIPoller_StaysByteIdenticalToNoPoller compares
	// two whole sweeps' output line for line — an absolute path (which differs
	// between any two deployments, and between any two test runs) makes that
	// comparison impossible. The path is on every WARN above, which is where
	// anyone needs it.
	logger.Debug("warm-start cache written", "instances", len(snap.Instances), "writtenAt", at.UTC().Format(time.RFC3339))
}

// loadStateCache reads dir/state-cache.json back.
//
// It is the ONE loader (this file's header explains why that matters), and it
// is deliberately unforgiving: a cache that is missing, unreadable,
// unparseable, of another schema version, or missing any field the warm start
// is built out of produces ok=false and a cold start — exactly the behavior
// every version before this one had. The alternative, patching a half-readable
// cache into something usable, would put invented numbers on the one surface
// whose entire job is "glance and trust these numbers".
//
// EVERY refusal is a WARN except one: an ABSENT file is an INFO. The brief's
// rule ("Invalid in ANY way (missing, unparseable, wrong schemaVersion,
// pointer-decode misses on load-bearing fields) → WARN + ignore + cold start")
// names `missing` first, and this file followed it to the letter for two review
// rounds — but the controller has now ratified the deviation on noise-discipline
// grounds: a warning that fires on every fresh deployment's healthy first start
// is how operators learn to ignore warnings, and this one fires on a state that
// is not a fault at all. Nothing has gone wrong when a program that has never
// completed a cycle has not yet written the file it writes at the end of a
// cycle. Every OTHER shape stays a WARN, because every other shape means a file
// exists and could not be used — which is a thing someone may need to fix.
//
// The line still says the three things every refusal here says: which file, what
// is wrong with it, and what happens instead.
func loadStateCache(logger *slog.Logger, dir string) ([]instanceStatsView, time.Time, bool) {
	final, _, ok := stateCachePaths(dir)
	if !ok {
		return nil, time.Time{}, false
	}

	raw, err := os.ReadFile(final)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			logger.Info("there is no warm-start cache to read, so this start is a cold one and the dashboard stays empty until the first sweep completes. On a first start that is expected; the next completed full cycle writes the file",
				"path", final)
			return nil, time.Time{}, false
		}
		logger.Warn("the dashboard's warm-start cache could not be read, so this start is a cold one; the first completed sweep fills the dashboard in",
			"error", err, "path", final)
		return nil, time.Time{}, false
	}

	var doc stateCacheDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, time.Time{}, coldStart(logger, final, fmt.Sprintf("it is not valid JSON (%v)", err))
	}
	var probe stateCacheProbe
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, time.Time{}, coldStart(logger, final, fmt.Sprintf("its instances array is not the shape this loader can read (%v)", err))
	}

	switch {
	case doc.SchemaVersion == nil:
		return nil, time.Time{}, coldStart(logger, final, "it carries no schemaVersion, so it is not a cutoffarr state cache this version can read")
	case *doc.SchemaVersion != stateCacheSchemaVersion:
		return nil, time.Time{}, coldStart(logger, final,
			fmt.Sprintf("it was written with schemaVersion %d and this cutoffarr reads %d", *doc.SchemaVersion, stateCacheSchemaVersion))
	case doc.WrittenAt == nil:
		return nil, time.Time{}, coldStart(logger, final, "it carries no writtenAt, so nothing could say how old the numbers in it are")
	case doc.WrittenAt.IsZero():
		return nil, time.Time{}, coldStart(logger, final, "its writtenAt is the zero time, so nothing could say how old the numbers in it are")
	case doc.Instances == nil:
		return nil, time.Time{}, coldStart(logger, final, "it carries no instances key at all")
	case probe.Instances == nil || len(*probe.Instances) != len(*doc.Instances):
		return nil, time.Time{}, coldStart(logger, final, "its instances array could not be read consistently twice, which means it is not the shape it claims to be")
	}

	instances := *doc.Instances
	seen := make(map[string]bool, len(instances))
	for i, p := range *probe.Instances {
		switch {
		case p.Name == nil || *p.Name == "":
			return nil, time.Time{}, coldStart(logger, final, fmt.Sprintf("its instances[%d] has no name, and an instance with no name cannot be shown or refreshed", i))
		case p.Type == nil || *p.Type == "":
			return nil, time.Time{}, coldStart(logger, final, fmt.Sprintf("its instances[%d] (%s) has no type", i, *p.Name))
		case seen[*p.Name]:
			return nil, time.Time{}, coldStart(logger, final, fmt.Sprintf("it names the instance %q twice, so there is no single set of numbers to show for it", *p.Name))
		}
		seen[*p.Name] = true
	}

	return instances, doc.WrittenAt.UTC(), true
}

// coldStart is loadStateCache's one refusal shape: say which file, say exactly
// what is wrong with it, say what happens instead, and always return false so
// the call site reads as a single expression.
func coldStart(logger *slog.Logger, path, why string) bool {
	logger.Warn("the dashboard's warm-start cache was ignored and this start is a cold one; the first completed sweep fills the dashboard in. The file is safe to delete",
		"path", path, "reason", why)
	return false
}

// intPtrValue/timePtrValue give the document's pointer fields something to
// point at without a temporary variable at each call site. They exist for the
// WRITE side only: the read side's pointers come from the decoder.
func intPtrValue(v int) *int { return &v }

func timePtrValue(v time.Time) *time.Time { return &v }
