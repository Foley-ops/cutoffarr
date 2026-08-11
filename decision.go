package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// profileElement decodes one /api/v3/qualityprofile array element as used
// by the decision engine (mandated refactor c). Distinct from
// connectivity.go's qualityProfileResponse (informational logging only, no
// id needed there): this one also decodes id, since movies reference
// profiles by qualityProfileId and the decision engine needs to build a map
// keyed by it.
type profileElement struct {
	ID                *int    `json:"id"`
	Name              *string `json:"name"`
	CutoffFormatScore *int    `json:"cutoffFormatScore"`
	UpgradeAllowed    *bool   `json:"upgradeAllowed"`
}

// qualityProfile is the minimal, fully-resolved view of a quality profile
// the decision engine needs for rules 3 and 6 of the STRICT decision rule.
type qualityProfile struct {
	Name              string
	CutoffFormatScore int
	UpgradeAllowed    bool
}

// fetchQualityProfiles fetches GET /api/v3/qualityprofile once for this
// instance's decision-engine pass and builds a map keyed by profile id.
// Per the mandated refactor, decisions depend on every one of a profile's
// id/name/cutoffFormatScore/upgradeAllowed fields being present: unlike
// checkInstanceConnectivity's identically-shaped but purely informational
// fetch of the same endpoint (whose absent-field warnings never skip the
// instance, since that fetch is diagnostic only), a profile missing any of
// these fields here means the decision engine cannot safely evaluate rules
// 3/6 against it, so per §2.6 the whole instance is skipped for the cycle.
func fetchQualityProfiles(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance) (map[int]qualityProfile, bool) {
	body, err := fetchBody(ctx, client, "/api/v3/qualityprofile", nil)
	if err != nil {
		logger.Warn("skipping instance: qualityprofile request failed",
			"instance", inst.Name, "type", inst.Type, "error", err)
		return nil, false
	}

	var raw []profileElement
	if err := json.Unmarshal(body, &raw); err != nil {
		logger.Warn("skipping instance: qualityprofile response is not valid JSON",
			"instance", inst.Name, "type", inst.Type, "error", err)
		return nil, false
	}

	profiles := make(map[int]qualityProfile, len(raw))
	for _, p := range raw {
		if p.ID == nil || p.Name == nil || p.CutoffFormatScore == nil || p.UpgradeAllowed == nil {
			logger.Warn("skipping instance: qualityprofile response missing a required field",
				"instance", inst.Name, "type", inst.Type,
				"id", derefOrAbsent(p.ID), "name", derefOrAbsent(p.Name),
				"cutoffFormatScore", derefOrAbsent(p.CutoffFormatScore), "upgradeAllowed", derefOrAbsent(p.UpgradeAllowed))
			return nil, false
		}
		profiles[*p.ID] = qualityProfile{
			Name:              *p.Name,
			CutoffFormatScore: *p.CutoffFormatScore,
			UpgradeAllowed:    *p.UpgradeAllowed,
		}
	}

	return profiles, true
}

// tagElement decodes one /api/v3/tag array element.
type tagElement struct {
	ID    *int    `json:"id"`
	Label *string `json:"label"`
}

// resolveExclusionTagID fetches GET /api/v3/tag and resolves label (the
// configured exclusion_tag) to its numeric id, matched case-insensitively
// per the binding tag-resolution rule. A request or decode failure is
// instance-fatal per §2.6 (ok=false): rule 4's exclusions cannot be safely
// evaluated without this data, so the whole instance is skipped for the
// cycle. A successful fetch that simply does not contain label is NOT a
// failure: found=false, ok=true, and rule 4 passes for every movie (logged
// at info, per the plan). An individual tag record missing its id or label
// is excluded from resolution with a warning but does not fail the whole
// fetch, mirroring how fetchWantedCutoff treats a record missing its id.
func resolveExclusionTagID(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance, label string) (id int, found bool, ok bool) {
	body, err := fetchBody(ctx, client, "/api/v3/tag", nil)
	if err != nil {
		logger.Warn("skipping instance: tag request failed",
			"instance", inst.Name, "type", inst.Type, "error", err)
		return 0, false, false
	}

	var tags []tagElement
	if err := json.Unmarshal(body, &tags); err != nil {
		logger.Warn("skipping instance: tag response is not valid JSON",
			"instance", inst.Name, "type", inst.Type, "error", err)
		return 0, false, false
	}

	for _, tg := range tags {
		if tg.ID == nil || tg.Label == nil {
			logger.Warn("tag record missing id or label field; excluded from exclusion-tag resolution",
				"instance", inst.Name, "type", inst.Type, "id", derefOrAbsent(tg.ID), "label", derefOrAbsent(tg.Label))
			continue
		}
		if strings.EqualFold(*tg.Label, label) {
			return *tg.ID, true, true
		}
	}

	logger.Info("exclusion tag not defined in this instance; no movies excluded",
		"instance", inst.Name, "type", inst.Type, "exclusionTag", label)
	return 0, false, true
}

// movieFileDetail decodes one element of the array GET
// /api/v3/moviefile?movieId=<id> returns — the only endpoint where
// customFormatScore actually exists (per the STRICT decision rule's live
// Phase 2 findings: it is absent from /movie and /movie/{id}).
type movieFileDetail struct {
	ID                *int `json:"id"`
	CustomFormatScore *int `json:"customFormatScore"`
}

// fetchMovieFileDetails fetches GET /api/v3/moviefile?movieId=<movieID> for
// a single movie. Used only for movies that have already passed rules 1-5
// of the STRICT decision rule (evaluateMovie fetches it lazily, last), to
// minimize API calls. A request or decode failure is per-movie, not
// instance-fatal (§2.6): the caller treats ok=false as "could not fetch
// custom format score" and skips just this movie, continuing to the rest
// of the library.
func fetchMovieFileDetails(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance, movieID int) ([]movieFileDetail, bool) {
	query := url.Values{"movieId": {strconv.Itoa(movieID)}}
	body, err := fetchBody(ctx, client, "/api/v3/moviefile", query)
	if err != nil {
		logger.Warn("moviefile request failed",
			"instance", inst.Name, "type", inst.Type, "movieId", movieID, "error", err)
		return nil, false
	}

	var files []movieFileDetail
	if err := json.Unmarshal(body, &files); err != nil {
		logger.Warn("moviefile response is not valid JSON",
			"instance", inst.Name, "type", inst.Type, "movieId", movieID, "error", err)
		return nil, false
	}

	return files, true
}

// Decision reason strings (FIX 3, controller-mandated correction after the
// initial Phase 3 review): the single authoritative source for every
// would-unmonitor/skip reason evaluateMovie can produce, so the exact
// wording exists in exactly one place instead of being duplicated across
// evaluateMovie's branches (ReasonCouldNotFetchCFScore previously appeared
// as two separate literals) and repeated as free-standing string literals
// in tests. ReasonNoFile through ReasonCutoffMet are listed in the STRICT
// decision rule's evaluation order (rules 2-6); ReasonCutoffMet is the
// would-unmonitor reason, not a skip reason. ReasonUpgradesDisabled,
// ReasonCouldNotFetchCFScore, and ReasonCutoffMet are exact strings
// mandated by the brief; the others are this implementation's own
// consistent choice of wording where the brief left it unspecified.
const (
	ReasonNoFile               = "no file"
	ReasonUnknownProfile       = "unknown quality profile"
	ReasonUpgradesDisabled     = "profile has upgrades disabled"
	ReasonTagsUnknown          = "could not verify exclusion tags"
	ReasonExcludedByTag        = "excluded by tag"
	ReasonQualityCutoffNotMet  = "quality cutoff not met"
	ReasonCouldNotFetchCFScore = "could not fetch custom format score"
	ReasonCFCutoffNotMet       = "custom format cutoff not met"
	ReasonCutoffMet            = "cutoff met"
)

// movieDecision is the outcome of evaluating one monitored movie against
// the STRICT decision rule.
type movieDecision struct {
	id             int
	title          string
	hasFile        bool
	wouldUnmonitor bool
	reason         string
	profileName    string

	// qualityCutoffNotMet is copied from the movie's own
	// movieFile.qualityCutoffNotMet (nil if movieFile was absent), used
	// only by the cross-check to independently verify the wanted/cutoff-
	// derived id set against data computed by a different Radarr code
	// path. It is never consulted by the decision rules themselves (rule 5
	// is wanted/cutoff set membership only, per the STRICT rule).
	qualityCutoffNotMet *bool

	// cfScore and cfThreshold are populated once rule 6 is reached (rules
	// 1-5 passed): cfScore only if /moviefile was fetched successfully and
	// yielded a usable score; cfThreshold as soon as the profile is
	// resolved (rule 3 passed), so it is available even for a rule 6
	// CF-below-threshold skip. Both are used by the cross-check to restate
	// what was used, without a second /moviefile round trip.
	cfScore     *int
	cfThreshold int
}

// titleOrAbsent returns *t, or the string "absent" if t is nil, mirroring
// derefOrAbsent (connectivity.go) but returning a plain string rather than
// an `any`, since movieDecision.title is used both as a struct field and as
// a %q-formatted log attribute value.
func titleOrAbsent(t *string) string {
	if t == nil {
		return "absent"
	}
	return *t
}

// containsTag reports whether id is present in tags. A nil tags pointer
// (the "tags" key entirely absent from the movie's JSON) is treated as not
// containing anything, same as a present-but-empty tags list.
func containsTag(tags *[]int, id int) bool {
	if tags == nil {
		return false
	}
	for _, t := range *tags {
		if t == id {
			return true
		}
	}
	return false
}

// evaluateMovie applies the STRICT decision rule (rules 2-6; rule 1 —
// monitored == true — is the caller's responsibility, since a monitored
// == false movie is excluded from the report entirely rather than
// producing a skip line) to a single movie, in order, so the reported
// reason is always the first failing rule and GET /api/v3/moviefile is
// requested only once rules 2-5 have all passed (minimizing API calls, per
// the plan).
func evaluateMovie(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance, m movieListElement, profiles map[int]qualityProfile, exclusionTagID int, tagActive bool, wantedIDs map[int]bool) movieDecision {
	id := *m.ID // caller guarantees non-nil (see runRadarrDecisionEngine)
	hasFile := m.HasFile != nil && *m.HasFile

	d := movieDecision{id: id, title: titleOrAbsent(m.Title), hasFile: hasFile}
	if m.MovieFile != nil {
		d.qualityCutoffNotMet = m.MovieFile.QualityCutoffNotMet
	}

	// Profile display name resolution (FIX 5, controller-mandated
	// correction after the initial Phase 3 review): a map read only, done
	// eagerly so every report line — even one that fails as early as rule
	// 2 — carries a usable profile attr where one can actually be
	// resolved. This does NOT evaluate or reorder rule 3: profileFound is
	// only consulted by the rule 3 check below, unchanged. d.profileName
	// is set here only on a successful lookup; "unknown" is reserved for
	// the rule 3 not-found branch itself, so a movie that fails earlier
	// (e.g. rule 2) with an unresolvable profile id is left with an empty
	// profileName rather than being eagerly mislabeled "unknown" before
	// rule 3 was ever reached.
	var profile qualityProfile
	profileFound := false
	if m.QualityProfileID != nil {
		profile, profileFound = profiles[*m.QualityProfileID]
		if profileFound {
			d.profileName = profile.Name
		}
	}

	// Rule 2: hasFile == true.
	if !hasFile {
		d.reason = ReasonNoFile
		return d
	}

	// Rule 3: quality profile exists in the fetched set AND upgradeAllowed.
	if !profileFound {
		logger.Warn("skipping movie: unknown quality profile",
			"instance", inst.Name, "type", inst.Type, "title", d.title, "qualityProfileId", derefOrAbsent(m.QualityProfileID))
		d.reason = ReasonUnknownProfile
		d.profileName = "unknown"
		return d
	}
	d.cfThreshold = profile.CutoffFormatScore
	if !profile.UpgradeAllowed {
		d.reason = ReasonUpgradesDisabled
		return d
	}

	// Rule 4: tags do not include the exclusion tag id (only when the tag
	// is actually defined in this instance; otherwise rule 4 always
	// passes, per the tag-resolution rules).
	//
	// FIX 1 (controller-mandated correction after the whole-branch
	// review): "tags" entirely absent from the movie's JSON (m.Tags ==
	// nil, as opposed to present-but-empty) is untrusted input the same
	// way "monitored" absent is (FIX 6) — containsTag(nil, id) reports
	// false, so without this guard a movie whose tags we simply couldn't
	// observe would silently pass rule 4 and could reach would-unmonitor:
	// the one remaining untrusted-input route to that outcome, violating
	// §2.6. When the exclusion tag is actually active, that uncertainty
	// must not let the movie through: warn (house warnIfFieldAbsent
	// convention) and skip with a dedicated reason. When the tag is not
	// active in this instance, rule 4 is vacuous regardless of tags — nil
	// tags is harmless to the decision — so evaluation still proceeds, but
	// the warn still fires for gate visibility (an absent field may still
	// mean our assumed field name is wrong).
	warnIfFieldAbsent(logger, inst, "movie", "tags", m.Tags == nil)
	if tagActive && m.Tags == nil {
		d.reason = ReasonTagsUnknown
		return d
	}
	if tagActive && containsTag(m.Tags, exclusionTagID) {
		d.reason = ReasonExcludedByTag
		return d
	}

	// Rule 5: movie id is NOT in the complete /wanted/cutoff id set.
	if wantedIDs[id] {
		d.reason = ReasonQualityCutoffNotMet
		return d
	}

	// Rule 6: the file's customFormatScore >= the profile's
	// cutoffFormatScore. Only rules 1-5 having passed reaches this point,
	// so /moviefile is fetched lazily, exactly once, right here.
	files, ok := fetchMovieFileDetails(ctx, logger, client, inst, id)
	if !ok {
		d.reason = ReasonCouldNotFetchCFScore
		return d
	}
	var wantFileID *int
	if m.MovieFile != nil {
		wantFileID = m.MovieFile.ID
	}
	mf, found := selectMovieFile(files, wantFileID)
	if !found || mf.CustomFormatScore == nil {
		logger.Warn("skipping movie: could not determine custom format score from moviefile response",
			"instance", inst.Name, "type", inst.Type, "title", d.title, "id", id)
		d.reason = ReasonCouldNotFetchCFScore
		return d
	}
	score := *mf.CustomFormatScore
	d.cfScore = &score
	if score < profile.CutoffFormatScore {
		d.reason = ReasonCFCutoffNotMet
		return d
	}

	d.wouldUnmonitor = true
	d.reason = ReasonCutoffMet
	return d
}

// runRadarrDecisionEngine is the entry point for a single radarr instance.
// It fetches the two additional data sources the STRICT decision rule needs
// beyond what inspectRadarrLibrary (Phase 2) already gathered — quality
// profiles (refactor c) and the exclusion tag id (tag-resolution rules) —
// and then runs three distinct passes, in order:
//
//  1. EVALUATE every monitored movie in movies, in library order, logging a
//     "would-unmonitor" or "skip" report line for each (msg="would-unmonitor"
//     / msg=skip, per the plan). No writes, no decisions acted on.
//  2. CROSS-CHECK the resulting decisions against Radarr's own
//     qualityCutoffNotMet data (plan §6).
//  3. WRITE (Phase 4) — but only if the cross-check explicitly PASSED.
//
// Keeping the write pass separate from evaluation is deliberate rather than
// stylistic: a write may only happen once the cross-check has had the
// complete picture and approved it, which is impossible if writes are
// interleaved with the evaluation that produces the cross-check's input.
//
// onlyID (the --only-id flag, 0 when absent) narrows what is REPORTED and
// WRITTEN to the single movie with that id. It deliberately does not narrow
// what is evaluated: the cross-check validates the data the decision rests
// on, not the target, so it still samples the whole library (see
// runWritePass and the plan's Phase 4 acceptance criteria).
//
// It is called only for a radarr instance whose connectivity check and
// inspectRadarrLibrary fetch both already succeeded (main.go's
// responsibility); it never returns anything because, like
// checkInstanceConnectivity and inspectRadarrLibrary, the binding
// error-handling rule (§2.6) is "skip that instance for the cycle and log a
// warning" with no further work for a caller to gate on.
func runRadarrDecisionEngine(ctx context.Context, logger *slog.Logger, inst Instance, movies []movieListElement, wantedIDs map[int]bool, exclusionTagLabel string, onlyID int, dryRun bool) {
	client := NewAPIClient(inst.URL, inst.APIKey)

	// An --only-id naming a movie this instance's library does not contain
	// is checked before anything else is fetched: there is nothing to decide
	// or write, so there is no reason to make further API calls. A mistyped
	// id is the ordinary cause, and it is a warning rather than an error
	// because this function has no standing to fail the process — §2.6's
	// house rule for an instance that cannot be processed is "warn and skip
	// the instance for the cycle".
	//
	// This is NOT the guard against --only-id hitting the wrong instance.
	// Radarr movie ids are per-instance (each library is numbered from 1),
	// so id 2 exists in radarr-hd AND radarr-4k as two different films, and
	// the wrong-instance case is a MATCH here, not a miss — nothing in this
	// function could detect it. run() refuses ambiguous --only-id runs up
	// front, before any instance is contacted, by requiring --instance when
	// more than one radarr is in scope.
	if onlyID != 0 && !libraryContainsID(movies, onlyID) {
		logger.Warn("--only-id movie not found in this instance's library; no decisions for this instance",
			"instance", inst.Name, "type", inst.Type, "onlyId", onlyID)
		return
	}

	profiles, ok := fetchQualityProfiles(ctx, logger, client, inst)
	if !ok {
		return
	}

	exclusionTagID, tagActive, ok := resolveExclusionTagID(ctx, logger, client, inst, exclusionTagLabel)
	if !ok {
		return
	}

	// decisions holds every evaluated movie and feeds the cross-check;
	// reported holds the subset that is in scope for the report, the
	// summary counts, and the write pass. The two are the same slice unless
	// --only-id narrowed the scope.
	var decisions []movieDecision
	var reported []movieDecision
	totalMonitored := 0
	wouldUnmonitorCount := 0
	alreadyUnmonitoredCount := 0
	skipCounts := make(map[string]int)

	for _, m := range movies {
		// FIX 6 (controller-mandated correction after the initial Phase 3
		// review): "monitored" entirely absent from the JSON (as opposed
		// to present with monitored: false, a legitimate common value) is
		// the same present-vs-absent distinction warnIfFieldAbsent already
		// makes everywhere else a field this important is missing — a
		// movie this important silently dropped with no signal at all was
		// an inconsistency with that house convention. Still excluded
		// either way (monitored can't be assumed true; excluding is the
		// safe direction), just no longer silent about why.
		warnIfFieldAbsent(logger, inst, "movie", "monitored", m.Monitored == nil)
		if m.Monitored == nil || !*m.Monitored {
			// Rule 1: excluded from the report entirely, per the plan. A
			// movie whose monitored field is genuinely present and false
			// (as opposed to absent — a different, already-warned-about
			// case) is also Phase 5's no-op contract in miniature: this is
			// exactly what a movie this project unmonitored on an earlier
			// cycle looks like on every cycle after. The plan says such
			// movies are "skipped silently at info, logged at debug" — so
			// at info they stay as invisible as before, but the debug line
			// and the counter make a second, no-op run of this project
			// provable rather than merely quiet.
			//
			// REVIEW FIX (Phase 5 round 2): the onlyID scope test mirrors
			// the one the --only-id narrowing block below applies to every
			// other counter (totalMonitored, wouldUnmonitor, unmonitored).
			// Without it, this counter and its debug line were populated
			// library-wide even during a --only-id run — the summary's one
			// remaining number not scoped to the movie the human named —
			// and a targeted run against a large library would additionally
			// emit one debug line per already-unmonitored movie in the
			// entire library, not just the target.
			if m.Monitored != nil && !*m.Monitored && (onlyID == 0 || (m.ID != nil && *m.ID == onlyID)) {
				alreadyUnmonitoredCount++
				logger.Debug("already unmonitored",
					"id", derefOrAbsent(m.ID), "title", titleOrAbsent(m.Title), "instance", inst.Name)
			}
			continue
		}
		if m.ID == nil {
			// Defensive: without an id this movie can't be checked against
			// the wanted/cutoff set or queried at /moviefile, and can't be
			// safely reported. Real Radarr responses always include id;
			// this mirrors how fetchWantedCutoff treats a record missing
			// its id (warn, exclude, continue).
			logger.Warn("skipping movie: missing id field",
				"instance", inst.Name, "type", inst.Type, "title", titleOrAbsent(m.Title))
			continue
		}
		d := evaluateMovie(ctx, logger, client, inst, m, profiles, exclusionTagID, tagActive, wantedIDs)
		decisions = append(decisions, d)

		// --only-id scoping happens here and nowhere else: every movie is
		// still evaluated above (the cross-check needs the full candidate
		// pools), but only the named movie is reported, counted, or written.
		if onlyID != 0 && d.id != onlyID {
			continue
		}
		reported = append(reported, d)
		totalMonitored++

		if d.wouldUnmonitor {
			wouldUnmonitorCount++
			logger.Info("would-unmonitor",
				"id", d.id, "title", d.title, "reason", d.reason, "profile", d.profileName, "instance", inst.Name)
		} else {
			skipCounts[d.reason]++
			logger.Info("skip",
				"id", d.id, "title", d.title, "reason", d.reason, "profile", d.profileName, "instance", inst.Name)
		}
	}

	// The movie is in the library — that was established before any of this
	// ran, and matching it there required a non-nil id — so the only thing
	// that can have kept it out of the report is rule 1: monitored is false,
	// or the key was absent entirely (warned about separately above). Naming
	// any other cause here would send the reader hunting for a data problem
	// that cannot be the explanation. Without this line, --only-id on such a
	// movie would say nothing whatsoever about the one movie the human
	// explicitly named.
	if onlyID != 0 && len(reported) == 0 {
		logger.Info("--only-id movie produced no decision: it is not monitored, so rule 1 excludes it from the report",
			"instance", inst.Name, "type", inst.Type, "onlyId", onlyID)
	}

	cc := runCrossCheck(logger, inst, decisions, wantedIDs)
	crossCheckSummary := renderCrossCheckSummary(cc.status, cc.verified, cc.unverifiable)

	unmonitoredCount, writeErrorCount, echoUnverifiedCount, writesRefusedCount := runWritePass(ctx, logger, client, inst, reported, cc, exclusionTagID, tagActive, dryRun)

	attrs := []any{"instance", inst.Name, "type", inst.Type}
	if onlyID != 0 {
		attrs = append(attrs, "onlyId", onlyID)
	}
	attrs = append(attrs, "totalMonitored", totalMonitored, "wouldUnmonitor", wouldUnmonitorCount, "unmonitored", unmonitoredCount, "alreadyUnmonitored", alreadyUnmonitoredCount)

	// writeErrors counts failed WRITES, so in dry-run it is unconditionally
	// 0: no write was attempted, so none can have failed. That is not the
	// same as saying nothing went wrong. The write pass runs in dry-run too
	// (that is what makes it a rehearsal rather than a different code path),
	// and its pre-write GET, decode, and identity check can all fail. Those
	// failures are real, are logged at ERROR, and are counted — under their
	// own name, so that a report the human reads as "nothing was attempted"
	// never carries a number that reads as "N writes failed". Phase 5's
	// no-op contract reads unmonitored and writeErrors; neither is ever
	// inflated by a dry-run.
	//
	// writeEchoUnverified is the third outcome (see runWritePass): PUTs the
	// server accepted without confirming. It is always present in write mode,
	// including as 0, so its absence can never be read as "none happened";
	// it cannot occur in dry-run, where no PUT is ever sent, so printing it
	// there would be an invitation to wonder what it means. unmonitored +
	// writeEchoUnverified is the number of writes the server took;
	// unmonitored alone is the number it confirmed.
	if dryRun {
		attrs = append(attrs, "writeErrors", 0, "writeRehearsalErrors", writeErrorCount)
	} else {
		attrs = append(attrs, "writeErrors", writeErrorCount, "writeEchoUnverified", echoUnverifiedCount)
	}

	// writesRefused counts a fourth, mode-independent outcome (see
	// runWritePass's doc comment): would-unmonitor decisions the pre-write
	// exclusion-tag re-check refused before any request could fail or
	// succeed. Unlike writeErrors/writeEchoUnverified it is not split by
	// dryRun — the re-check runs identically in both modes, well before the
	// dry-run gate, so "this write would be refused" is exactly as true and
	// exactly as worth reporting during a rehearsal as during a real write.
	// Always present, including as 0, for the same reason writeEchoUnverified
	// is always present in write mode: its absence must never be mistaken for
	// "nothing to refuse".
	attrs = append(attrs, "writesRefused", writesRefusedCount)

	attrs = append(attrs, "skipReasons", formatSkipCounts(skipCounts), "crossCheck", crossCheckSummary)
	logger.Info("radarr decision summary", attrs...)
}

// libraryContainsID reports whether any movie in the library carries id.
// Used only to tell "--only-id names a movie this instance does not have"
// (a warning, not an error) apart from a target that exists but is filtered
// out by the decision rules.
func libraryContainsID(movies []movieListElement, id int) bool {
	for _, m := range movies {
		if m.ID != nil && *m.ID == id {
			return true
		}
	}
	return false
}

// runWritePass is the third and final pass: it acts on the decisions the
// first two passes produced. It returns the number of movies actually
// written (always 0 in dry-run, since no PUT is ever issued there) and the
// number of failures.
//
// In write mode a failure is a failed write. In dry-run it can only be a
// failed REHEARSAL of the write — the fresh GET, the decode, or the identity
// check, all of which run before the dry-run gate — because no write is ever
// attempted. The caller reports the two under different names for that
// reason; see the summary in runRadarrDecisionEngine.
//
// Two gates stand in front of every write:
//
//   - The cross-check must have authorized writes: an explicit PASS, and
//     evidence that the pass says something about the decisions this pass is
//     about to act on (see writeGateBlockReason). "failed" and
//     "inconclusive" — and any status value a future change might add —
//     block the entire pass, because the cross-check is what establishes
//     that the data the decisions rest on is trustworthy at all. An
//     inconclusive result is not a weaker pass; it means nothing was
//     verified, which is no basis for a write. Blocking is loud (warn), not
//     silent: "nothing was written" and "nothing needed writing" must never
//     look the same in the log.
//   - The decision's wouldUnmonitor bool. Never its reason text: reason is
//     a human-facing string, and gating a write on string comparison would
//     make a copy-edit to a message a functional change to the write path.
//
// Per §2.6 a failed write is logged, counted, and abandoned for the cycle —
// the pass moves to the next item and never retries.
//
// Four outcomes are counted, not two, because a PUT has four meaningfully
// different endings and only two of them are the ones people expect:
//
//   - confirmed (unmonitored): the server accepted the write AND its echo
//     says the movie is now unmonitored. The only outcome that logs
//     msg=unmonitor and the only one Phase 5's no-op contract counts.
//   - failed (writeErrors): the server rejected the write, or its echo says
//     the movie is still monitored. The change did not happen.
//   - accepted but unconfirmed (echoUnverified): a 2xx whose body cannot
//     settle the question — empty, not an object, no "monitored" key. The
//     change probably DID happen and cannot be proven, which is neither of
//     the above and must not be reported as either.
//   - refused before any request was sent (writesRefused): unmonitorMovie's
//     own pre-write exclusion-tag re-check refused to proceed — either
//     because the fresh payload confirms the exclusion tag is now present
//     (errExcludedAtWrite), or because its tags could not be trusted at all
//     (errTagsUnverifiable). REVIEW FIX (Phase 5 round 2): these three
//     refusal branches used to return (false, nil) and go completely
//     uncounted, so an instance where the exclusion tag is active and every
//     fresh fetch is refused could report wouldUnmonitor=N against
//     unmonitored=0, writeErrors=0, writeEchoUnverified=0 — every counter on
//     the summary line zero, with nothing explaining where N promised writes
//     went. It is neither a failed write (no PUT was ever sent, so nothing
//     Radarr did can have failed) nor a no-op (a no-op is "nothing needed
//     doing"; this is "something needed doing and the safety check said no")
//     and so gets its own name rather than being folded into either.
//
// exclusionTagID and tagActive are threaded straight through to
// unmonitorMovie's own pre-write re-check (Phase 5, mandated write-path
// safety fix): the scan that produced these decisions may be minutes old,
// and §2.5 says the exclusion tag always wins, in every mode — including a
// tag added to a movie after it was evaluated but before the write pass
// reached it.
func runWritePass(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance, decisions []movieDecision, cc crossCheckResult, exclusionTagID int, tagActive bool, dryRun bool) (unmonitored, writeErrors, echoUnverified, writesRefused int) {
	// The count is what makes the warnings below actionable: "the gate
	// blocked 12 writes" and "the gate blocked, but there was nothing to
	// write anyway" are very different situations, and without a number they
	// produce identical log lines. It is also what tells the sample-quality
	// rules whether they have anything to protect.
	pending := 0
	for _, d := range decisions {
		if d.wouldUnmonitor {
			pending++
		}
	}

	if reason := writeGateBlockReason(cc, pending); reason != "" {
		attrs := []any{"instance", inst.Name, "type", inst.Type, "crossCheck", cc.status}
		attrs = append(attrs, cc.logAttrs()...)
		attrs = append(attrs, "withheldWrites", pending, "dryRun", dryRun)
		msg := "writes withheld for this instance: " + reason
		// Phase 5 noise-budget fix (mandated): blocking a pass that had
		// nothing to write anyway is a health signal, not an alarm — WARN
		// every cycle a benign, nothing-pending instance's cross-check comes
		// back inconclusive trains a human running this in a daemon loop to
		// ignore the WARN that actually matters (one that blocked real
		// writes). Only a pass that actually withheld something stays WARN.
		if pending > 0 {
			logger.Warn(msg, attrs...)
		} else {
			logger.Info(msg, attrs...)
		}
		return 0, 0, 0, 0
	}

	// The gate opened, but "opened" is not the same as "the sample was
	// clean". Anything the cross-check could not verify is named here, at the
	// moment of consequence, with the ratio: the per-item warnings are
	// scattered through the report and the summary's crossCheck string is
	// read after the fact, so neither tells the human that writes went ahead
	// on partially verified data. Only fires when there is actually something
	// to write — a report-only run has no consequence to warn about.
	if pending > 0 && cc.unverifiable > 0 {
		attrs := []any{"instance", inst.Name, "type", inst.Type}
		attrs = append(attrs, cc.logAttrs()...)
		attrs = append(attrs, "pendingWrites", pending, "dryRun", dryRun)
		// Phase 5 noise-budget fix (mandated): this same code path runs in
		// dry-run too (the write pass is a rehearsal all the way up to the
		// per-item dry-run gate), so the old wording — "writes proceeding" —
		// was stated as fact in a mode where no write is ever sent. The
		// dryRun=true attr was already there, but a human scanning log text
		// should not have to notice an attr to avoid reading a rehearsal as a
		// write.
		msg := "writes proceeding on a partially verified cross-check: some sampled movies could not be verified"
		if dryRun {
			msg = "dry-run: write rehearsal proceeding on a partially verified cross-check: some sampled movies could not be verified (no write is sent; this is a rehearsal)"
		}
		logger.Warn(msg, attrs...)
	}

	for _, d := range decisions {
		if !d.wouldUnmonitor {
			continue
		}

		written, err := unmonitorMovie(ctx, logger, client, inst, d.id, exclusionTagID, tagActive, dryRun)
		if errors.Is(err, errExcludedAtWrite) || errors.Is(err, errTagsUnverifiable) {
			// unmonitorMovie already logged the specific reason (the
			// exclusion tag is now present, or its tags could not be
			// trusted) at the point it refused; nothing more to log here.
			// This is not a write failure (no PUT was ever sent — the
			// re-check runs before that, in both modes) and not a no-op
			// (unlike the already-unmonitored and dry-run-withheld cases,
			// something DID need doing here), so it gets counted under its
			// own name instead of falling into writeErrors or vanishing
			// through the `!written` no-op branch below.
			writesRefused++
			continue
		}
		if errors.Is(err, errWriteUnverified) {
			// The server took the write and told us nothing useful about it.
			// Calling this a failed write would state two things that are
			// probably both false — that the write path is broken, and that
			// the movie is still monitored — so it gets its own message, its
			// own counter, and a WARN rather than an ERROR. It is not
			// retried: §2.6 bars retries within a cycle, and a retry here
			// would be a second write against a movie that has most likely
			// already been changed. The next cycle settles it for free —
			// either the movie reads unmonitored (nothing to do, rule 1
			// excludes it) or it is a candidate again and gets written then.
			echoUnverified++
			logger.Warn("unmonitor write accepted but the response was unverifiable; treat it as applied and let the next cycle reconcile it",
				"instance", inst.Name, "type", inst.Type, "id", d.id, "title", d.title, "error", err)
			continue
		}
		if err != nil {
			writeErrors++
			// The wording is mode-specific because the two failures are
			// different events. In write mode a PUT was attempted and did
			// not take. In dry-run nothing was ever sent, so the only thing
			// that can have failed is the rehearsal of the write path (its
			// fresh GET, decode, or identity check) — still worth an ERROR,
			// since a broken write path must be visible in the one mode
			// allowed to exercise it, but "write failed" would be a false
			// statement about a run that wrote nothing.
			msg := "unmonitor write failed; skipping this movie for the cycle"
			if dryRun {
				msg = "unmonitor write rehearsal failed; no write was attempted (dry-run), and this movie is skipped for the cycle"
			}
			logger.Error(msg,
				"instance", inst.Name, "type", inst.Type, "id", d.id, "title", d.title, "error", err)
			continue
		}
		if !written {
			// Dry-run, or the movie was already unmonitored by the time the
			// write pass reached it; unmonitorMovie has already accounted
			// for both. Nothing was changed, so nothing is counted.
			continue
		}

		unmonitored++
		logger.Info("unmonitor",
			"id", d.id, "title", d.title, "reason", d.reason, "profile", d.profileName, "instance", inst.Name)
	}

	return unmonitored, writeErrors, echoUnverified, writesRefused
}

// writeGateBlockReason decides whether the cross-check authorizes this
// instance's write pass, returning "" to open the gate or the reason it
// stays shut. It is separate from runWritePass so the whole authorization
// rule is one readable expression rather than three early returns tangled
// with logging.
//
// REVIEW FIX (phase-4 round 2): the gate used to be `status ==
// crossCheckStatusPassed` and nothing more, which turned out to be far
// weaker than it reads. "passed" is awarded whenever at least one sampled
// item was verified and nothing disagreed — so:
//
//   - A sample of 20 in which 19 items lacked movieFile.qualityCutoffNotMet
//     still passed, and authorized writes across the entire library on the
//     strength of one verified movie. Nothing at the gate even mentioned the
//     19; the aggregate warning inside runCrossCheck fires only when the
//     verified count is zero.
//   - Worse, the cross-check samples TWO pools — would-unmonitor decisions
//     and monitored+hasFile skip decisions — and "passed" pooled them. A run
//     could earn its pass entirely from skip items, verifying not one of the
//     would-unmonitor decisions that are the only thing a write pass ever
//     acts on, and the gate could not tell the difference.
//
// So the rule is now about the evidence, not the verdict. Both extra
// conditions are scoped to the would-unmonitor pool: verification of skip
// decisions is worth having in the report, but it is not evidence about the
// data behind a write, and treating it as such is exactly the conflation
// above.
//
// The conditions apply only when the pass actually has writes to make.
// pendingWrites == 0 means the would-unmonitor pool is empty, so it was
// never sampled and "no would-unmonitor item was verified" is trivially true
// — blocking a pass with nothing to block would be pure noise. The status
// check is deliberately outside that guard: an unrecognized or failed status
// is worth saying out loud either way.
func writeGateBlockReason(cc crossCheckResult, pendingWrites int) string {
	if cc.status != crossCheckStatusPassed {
		return "the cross-check did not pass"
	}
	if pendingWrites == 0 {
		return ""
	}
	if cc.writeVerified == 0 {
		return "the cross-check verified no would-unmonitor decision, so nothing establishes that the data behind these writes is sound"
	}
	if cc.writeUnverifiable > cc.writeVerified {
		return "most sampled would-unmonitor decisions could not be verified, so the data behind these writes is only partly trustworthy"
	}
	return ""
}

// renderCrossCheckSummary formats the "radarr decision summary" line's
// crossCheck attr from runCrossCheck's result.
//
// FIX 2 (controller-mandated correction after the whole-branch review):
// crossCheckStatusPassed is its own explicit case here, not the switch's
// default — a bare default rendering the "passed" format meant any future
// or unrecognized status value (a typo in a new status constant, a
// forgotten case after adding a fourth state) would have silently rendered
// as a pass. default now renders the raw, unrecognized status string plus
// counts instead: something that can never be mistaken for "passed (...)",
// so an unrecognized status is loud and debuggable rather than falsely
// reassuring.
func renderCrossCheckSummary(status string, verified, unverifiable int) string {
	switch status {
	case crossCheckStatusPassed:
		return fmt.Sprintf("passed (%d verified, %d unverifiable)", verified, unverifiable)
	case crossCheckStatusFailed:
		// Deliberately no counts here: this is the human-gate signal (plan:
		// "disagreement must stop the project before writes"), kept as the
		// single unmistakable token it always was.
		return "FAILED"
	case crossCheckStatusInconclusive:
		return fmt.Sprintf("inconclusive (%d verified, %d unverifiable)", verified, unverifiable)
	default:
		return fmt.Sprintf("UNRECOGNIZED CROSS-CHECK STATUS %q (%d verified, %d unverifiable)", status, verified, unverifiable)
	}
}

// formatSkipCounts renders a skip-reason -> count map as a single
// deterministic (alphabetically sorted, so log output and tests are stable
// across runs) "reason=count, reason=count" string for the summary line.
func formatSkipCounts(counts map[string]int) string {
	if len(counts) == 0 {
		return "none"
	}
	reasons := make([]string, 0, len(counts))
	for r := range counts {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	parts := make([]string, 0, len(reasons))
	for _, r := range reasons {
		parts = append(parts, fmt.Sprintf("%s=%d", r, counts[r]))
	}
	return strings.Join(parts, ", ")
}

// crossCheckSampleSize is "up to 10" from the plan, applied independently
// to each of the two candidate categories (would-unmonitor and
// monitored+hasFile skip), so up to 20 items are checked in total.
const crossCheckSampleSize = 10

// sampleEveryKth deterministically samples up to n items from ids: sorted
// ascending, then every k-th item is taken where k = len(ids)/n (at least
// 1), per the binding requirement ("seed from movie-id ordering... do NOT
// use math/rand without a seed argument or time-based seeds"). Calling it
// twice with the same input always yields the same output.
func sampleEveryKth(ids []int, n int) []int {
	if len(ids) == 0 {
		return nil
	}
	sorted := append([]int(nil), ids...)
	sort.Ints(sorted)
	if len(sorted) <= n {
		return sorted
	}
	step := len(sorted) / n
	if step < 1 {
		step = 1
	}
	sampled := make([]int, 0, n)
	for i := 0; i < len(sorted) && len(sampled) < n; i += step {
		sampled = append(sampled, sorted[i])
	}
	return sampled
}

// Cross-check result states. "inconclusive" exists specifically so that a
// sample containing zero actually-comparable items can never render as a
// pass (FIX 1, controller-mandated correction after the initial Phase 3
// review): a naive "checked N items, no disagreements found" summary is
// indistinguishable from "verified nothing, found nothing wrong (because
// there was nothing to compare)" unless verified and unverifiable counts
// are tracked and surfaced separately.
const (
	crossCheckStatusPassed       = "passed"
	crossCheckStatusFailed       = "failed"
	crossCheckStatusInconclusive = "inconclusive"
)

// crossCheckResult is everything the cross-check learned, not just its
// verdict. It is a struct rather than a status string plus two counts
// because the write gate needs the evidence behind the verdict and not only
// the verdict itself (see writeGateBlockReason): a status alone cannot say
// whether anything relevant to a write was ever verified, and the review
// that found that hole showed how far apart those two things can be.
//
// The would-unmonitor counts are tracked separately from the aggregate for
// exactly that reason. The cross-check samples two pools with different
// meanings — decisions that would be WRITTEN, and skip decisions that would
// not — and a count that adds them together answers a question nobody is
// asking at the moment of a write.
type crossCheckResult struct {
	status string

	// Aggregate across both sampled pools: what the summary line reports and
	// what the "partially verified" warning quantifies.
	verified     int
	unverifiable int

	// The would-unmonitor pool alone — the only decisions the write pass
	// ever acts on. writeVerified counts sampled would-unmonitor decisions
	// whose movieFile.qualityCutoffNotMet was present and could therefore be
	// compared; writeUnverifiable counts those where it was absent.
	writeVerified     int
	writeUnverifiable int
}

// logAttrs renders the result as slog key/value pairs, so every place that
// reports the gate's reasoning names the same numbers under the same keys.
// The would-unmonitor pool is spelled out in full rather than summarized,
// because "1 of 10 verified" and "1 of 1 verified" are the difference
// between a run to investigate and a run to ignore.
func (cc crossCheckResult) logAttrs() []any {
	return []any{
		"verified", cc.verified,
		"unverifiable", cc.unverifiable,
		"wouldUnmonitorVerified", cc.writeVerified,
		"wouldUnmonitorUnverifiable", cc.writeUnverifiable,
	}
}

// runCrossCheck implements plan §6's cross-check, adapted to the STRICT
// rule (runs after decisions, read-only, no additional API calls: it
// re-uses data already collected during evaluateMovie plus the same
// wantedIDs set the decision engine consulted for rule 5). For up to 10
// deterministically sampled would-unmonitor decisions and up to 10
// deterministically sampled monitored+hasFile skip decisions (fewer if
// fewer exist), it independently verifies that wanted-set membership
// (wantedIDs[id], the same source the decision used for rule 5) agrees
// with the movie's own movieFile.qualityCutoffNotMet from the
// already-fetched /movie data — the two are computed by different Radarr
// code paths and should never disagree. For would-unmonitor items it also
// re-states the CF score and threshold that were used.
//
// verified counts items that were actually compared (qualityCutoffNotMet
// present); unverifiable counts items sampled but skipped because that
// field was absent (confirmed present on live Radarr 6.3.0 — 3 samples
// read false, 146 true library-wide — so this guards a future/different
// version, not today's observed behavior). Any disagreement among the
// verified items is logged at error level naming the movie and both
// values, and status is crossCheckStatusFailed — the caller's summary must
// then read "FAILED" (the human gate reads this, per the plan, to stop the
// project before any write path exists). If every sampled item was
// unverifiable, status is crossCheckStatusInconclusive (with its own
// warning) rather than Passed: a sample that verified nothing must never
// be indistinguishable from one that verified everything and found no
// problems.
//
// The same two counts are also kept for the would-unmonitor pool on its own
// (crossCheckResult.writeVerified / writeUnverifiable). The status is a
// statement about the sample as a whole; the write gate has to ask a
// narrower question — "was anything verified about the decisions I am about
// to act on?" — and only a per-pool count can answer it. See
// writeGateBlockReason.
func runCrossCheck(logger *slog.Logger, inst Instance, decisions []movieDecision, wantedIDs map[int]bool) crossCheckResult {
	byID := make(map[int]movieDecision, len(decisions))
	var wouldUnmonitorIDs, skipIDs []int
	for _, d := range decisions {
		byID[d.id] = d
		if d.wouldUnmonitor {
			wouldUnmonitorIDs = append(wouldUnmonitorIDs, d.id)
		} else if d.hasFile {
			skipIDs = append(skipIDs, d.id)
		}
	}

	sampled := append(sampleEveryKth(wouldUnmonitorIDs, crossCheckSampleSize), sampleEveryKth(skipIDs, crossCheckSampleSize)...)

	var result crossCheckResult
	disagreementFound := false
	for _, id := range sampled {
		d := byID[id]
		inWantedSet := wantedIDs[id]

		attrs := []any{
			"instance", inst.Name, "id", id, "title", d.title,
			"inWantedSet", inWantedSet, "qualityCutoffNotMet", derefOrAbsent(d.qualityCutoffNotMet),
		}
		if d.wouldUnmonitor {
			attrs = append(attrs, "cfScore", derefOrAbsent(d.cfScore), "cfThreshold", d.cfThreshold)
		}
		logger.Info("cross-check", attrs...)

		if d.qualityCutoffNotMet == nil {
			// Data-quality issue distinct from an actual disagreement: we
			// cannot verify agreement without a value to compare against.
			// Silently treating "absent" as "false" here could mask a real
			// disagreement (inWantedSet=false would then trivially "agree"
			// with a value we never actually observed), so this is counted
			// separately and called out on its own.
			//
			// d.wouldUnmonitor is what put this id in one sampled pool
			// rather than the other, so it is also the exact test for which
			// pool's count this belongs to.
			result.unverifiable++
			if d.wouldUnmonitor {
				result.writeUnverifiable++
			}
			logger.Warn("cross-check: movieFile.qualityCutoffNotMet missing from /movie data; cannot verify wanted-set agreement for this movie",
				"instance", inst.Name, "id", id, "title", d.title)
			continue
		}

		result.verified++
		if d.wouldUnmonitor {
			result.writeVerified++
		}
		if inWantedSet != *d.qualityCutoffNotMet {
			disagreementFound = true
			logger.Error("cross-check disagreement: wanted-set membership does not match movieFile.qualityCutoffNotMet",
				"instance", inst.Name, "id", id, "title", d.title,
				"inWantedSet", inWantedSet, "qualityCutoffNotMet", *d.qualityCutoffNotMet)
		}
	}

	switch {
	case disagreementFound:
		result.status = crossCheckStatusFailed
	case len(sampled) > 0 && result.verified == 0:
		result.status = crossCheckStatusInconclusive
		logger.Warn("cross-check: every sampled item was unverifiable (movieFile.qualityCutoffNotMet missing); cannot determine pass or fail",
			"instance", inst.Name, "unverifiable", result.unverifiable)
	default:
		result.status = crossCheckStatusPassed
	}

	return result
}

// selectMovieFile picks the movieFileDetail this movie's file actually
// corresponds to out of the array /api/v3/moviefile?movieId=<id> returns,
// per the STRICT decision rule: "take the element whose id matches the
// movie's movieFile.id, or the single element". In practice a movie has at
// most one file, so the single-element case is the norm; matching by id is
// a defensive fallback for the (unexpected) case of multiple elements.
func selectMovieFile(files []movieFileDetail, wantID *int) (movieFileDetail, bool) {
	if len(files) == 1 {
		return files[0], true
	}
	if wantID != nil {
		for _, f := range files {
			if f.ID != nil && *f.ID == *wantID {
				return f, true
			}
		}
	}
	return movieFileDetail{}, false
}
