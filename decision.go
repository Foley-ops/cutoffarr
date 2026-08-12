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
	"time"
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
// at info, per the plan).
//
// A tag record missing its id or label makes the instance fatal ONLY when the
// configured label was not found among the records that could be read (REVIEW
// FIX, Phase 5 round 2, corrected in the final round). Unlike a movie record
// missing its id (fetchWantedCutoff's precedent, where skipping just that one
// movie is safe because every other movie is still evaluated correctly), a
// malformed tag record cannot simply be skipped past: when its label is nil
// the record cannot be compared against the configured label at all, so
// falling through to "not defined" would assert a false statement — the tag
// may in fact be defined, just unreadably — and would silently disable rule 4
// for every movie in the instance (tagActive=false), which is exactly the
// untrusted-input route to an unmonitor action §2.6 forbids.
//
// But that reasoning only holds while the answer is actually unknown. Radarr
// tag labels are unique, so a positive, case-insensitive match against a
// well-formed record fully determines the id no matter what any other record
// says — which is why this scans EVERY record before deciding, rather than
// returning at the first unreadable one. Returning early made the outcome
// depend on array order: a malformed record listed before the exclusion tag's
// own record bricked the instance on every cycle forever, while the identical
// data in the opposite order resolved and ran. Unreadable records are always
// warned about (they may be the first sign of a /tag shape this code no longer
// understands); they are fatal only in combination with a label that was not
// found, which is the one case where the honest answer is "unknowable".
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

	matchedID, matched, sawUnresolvable := 0, false, false
	for _, tg := range tags {
		if tg.ID == nil || tg.Label == nil {
			sawUnresolvable = true
			logger.Warn("a tag record could not be resolved; it will be reported as fatal only if the exclusion tag is not found elsewhere in the list",
				"instance", inst.Name, "type", inst.Type, "id", derefOrAbsent(tg.ID), "label", derefOrAbsent(tg.Label))
			continue
		}
		if !matched && strings.EqualFold(*tg.Label, label) {
			matchedID, matched = *tg.ID, true
		}
	}

	if matched {
		return matchedID, true, true
	}
	if sawUnresolvable {
		logger.Warn("skipping instance: the exclusion tag was not found among the readable tag records and at least one record could not be resolved, so whether the tag exists cannot be determined",
			"instance", inst.Name, "type", inst.Type, "exclusionTag", label)
		return 0, false, false
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

// Sonarr-specific season decision reasons (Phase 6). Six of the Sonarr
// season rule's seven rules map onto concepts Radarr's decision rule
// already has a reason for — an unknown/upgrades-disabled profile, an
// untrusted or excluded tag, wanted/cutoff set membership, and both the
// missing-score and below-threshold custom-format outcomes mean exactly the
// same thing for a season's episode files as they do for a movie's file —
// so evaluateSeries reuses ReasonUnknownProfile, ReasonUpgradesDisabled,
// ReasonTagsUnknown, ReasonExcludedByTag, ReasonQualityCutoffNotMet,
// ReasonCouldNotFetchCFScore, ReasonCFCutoffNotMet, and ReasonCutoffMet
// directly rather than duplicating them under new names: same greppable
// token, same meaning, one instance type or the other. Only what a season
// has that a movie does not gets a new constant here: completeness on disk
// is a season-wide statistic a movie has no equivalent of, the airing guard
// has no Radarr analog at all, and a season's episode data can be
// internally inconsistent or unfetchable in ways a single movie file cannot.
//
// ReasonSeasonNotFullyAired's exact string is mandated by the binding
// controller resolution ("skip the season, reason 'unaired or undated
// episodes'"); the others are this implementation's own consistent choice
// of wording where the brief left it unspecified, following the same
// concise, lowercase, no-trailing-punctuation style as the Radarr reasons
// above.
const (
	ReasonSeasonIncomplete              = "season incomplete on disk"
	ReasonSeasonNotFullyAired           = "unaired or undated episodes"
	ReasonSeasonEpisodeDataInconsistent = "episode data inconsistent with statistics"
	ReasonSeasonFileCountMismatch       = "episode file count mismatch"
	ReasonSeasonEpisodesUnavailable     = "could not fetch episode data"
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
		//
		// Rule 1 therefore excludes two different populations, and they are
		// two branches rather than one because they are two different facts
		// about the library — REVIEW FIX (Phase 5 final round). They used to
		// share a branch (`m.Monitored == nil || !*m.Monitored`) whose body
		// then re-tested `m.Monitored != nil` to tell them apart, which reads
		// as dead redundancy and is exactly what a future simplification
		// deletes; deleting it would have reported a movie whose state was
		// never observed as "already unmonitored", both in the counter and in
		// a debug line stating it as an observed fact. Split like this, the
		// distinction is load-bearing rather than defensive: the second branch
		// dereferences the pointer the first one just ruled out.
		warnIfFieldAbsent(logger, inst, "movie", "monitored", m.Monitored == nil)
		if m.Monitored == nil {
			// "monitored" entirely absent from the JSON: untrusted input, not
			// a state. It is excluded (monitored cannot be assumed true, and
			// excluding is the safe direction) and it is warned about, just
			// above — but it is deliberately NOT counted as already
			// unmonitored, because nobody knows whether it is. A library whose
			// /movie payloads lack the field — an unexpected Radarr version, a
			// proxy stripping fields — would otherwise converge on a report
			// reading totalMonitored=0 alreadyUnmonitored=N: a confident
			// no-op, asserted over exactly the data §2.6 says never to guess
			// from.
			continue
		}
		if !*m.Monitored {
			// monitored is present and false. This is Phase 5's no-op contract
			// in miniature: it is exactly what a movie this project
			// unmonitored on an earlier cycle looks like on every cycle after.
			// The plan says such movies are "skipped silently at info, logged
			// at debug" — so at info they stay as invisible as before, but the
			// debug line and the counter make a second, no-op run of this
			// project provable rather than merely quiet.
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
			if onlyID == 0 || (m.ID != nil && *m.ID == onlyID) {
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

	unmonitoredCount, writeErrorCount, echoUnverifiedCount, writesRefusedCount, withheldWriteCount := runWritePass(ctx, logger, client, inst, reported, cc, exclusionTagID, tagActive, dryRun)

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

	// The last two outcomes, both mode-independent (see runWritePass's doc
	// comment for all five and for the identity they satisfy):
	//
	//   - writesRefused: would-unmonitor decisions the write path itself
	//     declined — the exclusion tag reappeared, the fresh payload's tags or
	//     "monitored" could not be read, or something else unmonitored the
	//     movie between the scan and the write pass. Unlike
	//     writeErrors/writeEchoUnverified this is not split by dryRun: every
	//     refusal happens well before the dry-run gate, so "this write would be
	//     refused" is exactly as true and exactly as worth reporting during a
	//     rehearsal as during a real write.
	//   - withheldWrites: writes that were never attempted at all, because the
	//     cross-check gate blocked the pass or because a dry-run stopped at the
	//     gate immediately before the PUT. This is what makes a dry-run summary
	//     add up (wouldUnmonitor=N withheldWrites=N is the shape of a healthy
	//     rehearsal) and what stops a gate-blocked pass from reporting N
	//     promised writes against a line of zeroes. The same attr name and
	//     meaning as the gate's own warning line, which quantifies just its
	//     own half.
	//
	// Both are always present, including as 0, for the same reason
	// writeEchoUnverified always is in write mode: an absent number must never
	// be readable as "none happened".
	attrs = append(attrs, "writesRefused", writesRefusedCount, "withheldWrites", withheldWriteCount)

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
// Five outcomes are counted, not two, because a would-unmonitor decision has
// five meaningfully different endings and only two of them are the ones people
// expect:
//
//   - confirmed (unmonitored): the server accepted the write AND its echo
//     says the movie is now unmonitored. The only outcome that logs
//     msg=unmonitor and the only one Phase 5's no-op contract counts.
//   - failed (writeErrors): the server rejected the write, its echo says the
//     movie is still monitored, or the pre-write fetch itself failed. The
//     change did not happen. In dry-run the same counter is reported under
//     the name writeRehearsalErrors, since no write was attempted and only
//     the rehearsal of one can have failed.
//   - accepted but unconfirmed (echoUnverified): a 2xx whose body cannot
//     settle the question — empty, not an object, about a different movie, no
//     "monitored" key, or "monitored" as JSON null. The change probably DID
//     happen and cannot be proven, which is neither of the above and must not
//     be reported as either.
//   - refused before any request was sent (writesRefused): unmonitorMovie
//     declined to proceed. Four causes, each logging its own reason at the
//     moment it refuses: the fresh payload confirms the exclusion tag is now
//     present (errExcludedAtWrite), its tags could not be trusted at all
//     (errTagsUnverifiable), its "monitored" could not be read at all
//     (errMonitoredUnverifiable), or the movie is already unmonitored —
//     something else changed it between the scan and this pass reaching it
//     (errAlreadyUnmonitoredAtWrite). None is a failed write (no PUT was ever
//     sent, so nothing Radarr did can have failed) and none is a no-op (a
//     no-op is "nothing needed doing"; these are "something needed doing and
//     the write path said no"), so they are neither of those counters.
//   - withheld (withheld): no attempt was made at all — either the
//     cross-check gate blocked the entire pass, or this is a dry-run and
//     unmonitorMovie stopped at the gate immediately before the PUT (§2.1).
//     The one outcome that is a genuine non-event.
//
// Those five are exhaustive by construction, and that is the point. Two review
// rounds each found a different silent (false, nil) path that made promised
// writes evaporate with every counter reading zero, and each was closed by
// adding a counter — a pattern that only stops when the numbers are required
// to add up:
//
//	wouldUnmonitor == unmonitored + echoUnverified + writeErrors
//	                  + writesRefused + withheld
//
// (In the summary line, writeErrors is printed as writeRehearsalErrors during
// a dry-run; the identity is over the same value either way.) The identity is
// pinned by
// TestRunRadarrDecisionEngine_EveryWouldUnmonitorDecisionIsAccountedForInTheSummary,
// which walks every ending a decision can have and fails if one of them lands
// nowhere. Any future outcome must increment one of these five terms, or that
// test says so.
//
// exclusionTagID and tagActive are threaded straight through to
// unmonitorMovie's own pre-write re-check (Phase 5, mandated write-path
// safety fix): the scan that produced these decisions may be minutes old,
// and §2.5 says the exclusion tag always wins, in every mode — including a
// tag added to a movie after it was evaluated but before the write pass
// reached it.
func runWritePass(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance, decisions []movieDecision, cc crossCheckResult, exclusionTagID int, tagActive bool, dryRun bool) (unmonitored, writeErrors, echoUnverified, writesRefused, withheld int) {
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
		//
		// REVIEW FIX (Phase 6, carried forward from the phase-5 branch
		// review): that downgrade is only safe when cc.status is a status
		// this code actually recognizes — "failed" or "inconclusive" with
		// nothing pending really is benign. A status outside the three known
		// constants (a typo in a future status constant, a zero-value
		// crossCheckResult) is itself a sign something is broken, and
		// pending == 0 must not be allowed to silence that: the pass still
		// downgrades on a known status, but stays WARN whenever pending > 0
		// OR the status itself is unrecognized.
		if pending > 0 || !isKnownCrossCheckStatus(cc.status) {
			logger.Warn(msg, attrs...)
		} else {
			logger.Info(msg, attrs...)
		}
		// Every pending write is withheld, and says so on the summary line as
		// well as here: the gate's own warning scrolls past in a daemon loop,
		// while the summary is the line a human greps, and a blocked pass that
		// left wouldUnmonitor=N against five zeroes was exactly the
		// unexplained gap the reconciliation identity above exists to forbid.
		return 0, 0, 0, 0, pending
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
		if isWriteRefusal(err) {
			// unmonitorMovie already logged the specific reason — the
			// exclusion tag is now present, its tags could not be trusted, its
			// "monitored" could not be read, or something else unmonitored the
			// movie first — at the point it refused, so nothing more is logged
			// here and no cause is lost by sharing a counter. None of them is
			// a write failure (no PUT was ever sent: every refusal happens
			// before that point, in both modes) and none is a no-op (something
			// DID need doing), so they are counted here rather than falling
			// into writeErrors or vanishing through the `!written` branch
			// below.
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
			// Dry-run: unmonitorMovie withheld the write immediately before
			// the PUT (§2.1) and has already logged it at debug. This is the
			// ONLY remaining (false, nil) case — the already-unmonitored race
			// and the tag/monitored refusals all moved to sentinel-wrapped
			// errors, counted above, precisely so nothing real falls silently
			// through this branch. Nothing was written, so it is counted with
			// the gate-blocked writes rather than left to vanish: "withheld"
			// is what both mean, and the reconciliation identity needs every
			// pending write to end in exactly one counter.
			withheld++
			continue
		}

		unmonitored++
		logger.Info("unmonitor",
			"id", d.id, "title", d.title, "reason", d.reason, "profile", d.profileName, "instance", inst.Name)
	}

	return unmonitored, writeErrors, echoUnverified, writesRefused, withheld
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

// isKnownCrossCheckStatus reports whether status is one of the three
// constants above. Used by runWritePass's noise-budget downgrade (a blocked
// pass with nothing pending drops to INFO) to scope that downgrade to
// statuses this code actually understands — an unrecognized status is a bug
// signal that must stay WARN regardless of how much was pending.
func isKnownCrossCheckStatus(status string) bool {
	switch status {
	case crossCheckStatusPassed, crossCheckStatusFailed, crossCheckStatusInconclusive:
		return true
	default:
		return false
	}
}

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

// ============================================================================
// Sonarr season decision engine (Phase 6)
// ============================================================================
//
// Repeats the shape of the Radarr decision engine above at SEASON
// granularity rather than movie granularity, against the seven-rule Sonarr
// season rule (plan §6, plus the strict amendment) — see the task brief for
// the binding controller resolutions this implementation follows:
//
//  1. Series monitored == true and season monitored == true.
//  2. Season is complete on disk: episodeFileCount == totalEpisodeCount &&
//     totalEpisodeCount > 0.
//  3. Every episode of the season has aired (airDateUtc in the past) — the
//     single most important Sonarr guard, since unmonitoring an airing
//     season would stop future episodes from grabbing.
//  4. No episode of the season appears in the paged /wanted/cutoff results.
//  5. Series tags do not include the exclusion tag.
//  6. Profile upgradeAllowed == true.
//  7. STRICT: every episode file of the season has customFormatScore >=
//     profile.cutoffFormatScore.
//
// Evaluation order deliberately does NOT follow that numbering: the binding
// evaluation-order resolution puts the series-level cheap checks (monitored,
// tags, profile+upgradeAllowed — rules 1a/5/6) before rules 1b-4, and before
// the one /episode fetch per surviving series they need, so a series that
// fails a series-level check never causes that fetch at all. Only after
// rules 1-6 all pass for a season does the one /episodefile fetch per
// series (shared across every candidate season of that series) happen, for
// rule 7. The reported "reason" for a season is always the first rule that
// actually failed IN THIS EVALUATION ORDER — mirroring evaluateMovie's own
// "first failing rule wins" contract, just over a different, efficiency-
// driven sequence than the plan's rule numbering.
//
// Never at the series level: unmonitoring only ever happens to a season (and
// implicitly its episodes); no code path in this engine composes a
// series-level monitored write, and there is no Sonarr write path in this
// phase at all (Phase 7).

// seasonDecision is the outcome of evaluating one monitored season against
// the Sonarr season decision rule.
type seasonDecision struct {
	seriesID       int
	series         string
	season         int
	wouldUnmonitor bool
	reason         string
	profileName    string

	// completeOnDisk is true once the season has passed rule 2, regardless
	// of what happens afterward. It is what makes a season eligible for the
	// cross-check's skip-side sampling pool (runSonarrCrossCheck) — the
	// Sonarr analogue of Radarr's "monitored && hasFile" skip-pool
	// eligibility, since qualityCutoffNotMet is only a meaningful signal for
	// a season that actually has files on disk.
	completeOnDisk bool

	// crossCheckEpisodes carries, for a season whose /episodefile data was
	// actually fetched during evaluation (every candidate that reached rule
	// 7, whichever way it was decided), the per-episode data
	// runSonarrCrossCheck needs to independently verify wanted-set
	// membership against episodeFile.qualityCutoffNotMet. It is nil for
	// every season whose series never fetched /episodefile during
	// evaluation (no season of that series ever reached rule 7) — most
	// notably every rule-3 (airing) and rule-4 (in wanted set) skip, since
	// evaluateSeries never has a reason to fetch /episodefile for those. See
	// rawEpisodesForCrossCheck immediately below for how the cross-check
	// still makes those seasons comparable when they're sampled, rather
	// than leaving them permanently unverifiable by construction.
	crossCheckEpisodes []seasonCrossCheckEpisode

	// rawEpisodesForCrossCheck is set (IMPORTANT REVIEW FIX) for a season
	// that failed rule 3 or rule 4 specifically — the two skip reasons whose
	// episode data evaluateSeries already fetched and validated (rule 2's
	// completeness check passed) but whose /episodefile fetch never
	// happened, because those seasons never reach rule 7. It carries just
	// enough (the episode list itself) for runSonarrCrossCheck to make its
	// OWN bounded, read-only, on-demand /episodefile fetch if — and only
	// if — this season is actually sampled into the skip-side pool, so
	// that pool can produce real evidence for exactly the skip reason
	// (rule 4: "in the wanted set") whose independent signal is most worth
	// checking, instead of being unverifiable by construction for every
	// season that reaches it. Always nil when crossCheckEpisodes is
	// already populated (rule 7 was reached) — the two are never both
	// needed for the same season.
	rawEpisodesForCrossCheck []episodeElement

	// cfThreshold is populated once the series' profile is resolved (the
	// series-level profile check passed, or failed only on upgradeAllowed —
	// mirrors evaluateMovie's own asymmetry, where cfThreshold is set before
	// the upgradeAllowed check but a wholly unknown profile leaves it 0).
	cfThreshold int
}

// seasonCrossCheckEpisode is the minimal per-episode data
// runSonarrCrossCheck needs: the episode's own id (to test membership in
// the wanted/cutoff-derived episode id set), whether the episode itself is
// monitored, and its file's qualityCutoffNotMet, joined via the episode's
// own episodeFileId — nil when no matching file was found (or the episode
// has no file id at all), which the cross-check treats as unverifiable for
// that one episode, the same way it treats a whole season with no
// episode-file data at all.
//
// monitored exists (IMPORTANT REVIEW FIX) because /wanted/cutoff is
// filtered to monitored episodes of monitored series (fetchSonarrWantedCutoff
// only ever sends page/pageSize — confirmed live behavior), while this
// engine decides at SEASON granularity: an unmonitored episode inside an
// otherwise-monitored, complete season can never appear in the wanted set
// regardless of its own file's qualityCutoffNotMet, so comparing it would
// manufacture a disagreement the wanted-set semantics never claimed to
// make. runSonarrCrossCheck excludes any episode whose monitored is nil or
// false from the comparison entirely.
type seasonCrossCheckEpisode struct {
	episodeID           int
	monitored           *bool
	qualityCutoffNotMet *bool
}

// buildSeasonCrossCheckEpisodes joins episodes to their episode files via
// each episode's own episodeFileId — the one join runSonarrCrossCheck's
// per-episode comparison needs. Shared (not forked) between evaluateSeries'
// own rule-7 candidate path, whose /episodefile fetch already happened
// during evaluation, and runSonarrCrossCheck's on-demand fetch for sampled
// skip-side seasons that never reached rule 7 at all.
func buildSeasonCrossCheckEpisodes(episodes []episodeElement, filesByID map[int]episodeFileElement) []seasonCrossCheckEpisode {
	var out []seasonCrossCheckEpisode
	for _, e := range episodes {
		if e.ID == nil {
			continue
		}
		var qcnm *bool
		if e.EpisodeFileID != nil {
			if f, found := filesByID[*e.EpisodeFileID]; found {
				qcnm = f.QualityCutoffNotMet
			}
		}
		out = append(out, seasonCrossCheckEpisode{episodeID: *e.ID, monitored: e.Monitored, qualityCutoffNotMet: qcnm})
	}
	return out
}

// seriesEvaluation is evaluateSeries's full result for one series: the
// season decisions to report (would-unmonitor/skip lines), plus the
// season-level accounting runSonarrDecisionEngine's summary line needs.
type seriesEvaluation struct {
	decisions []seasonDecision
	// alreadyUnmonitored counts seasons with monitored explicitly false
	// (present, not absent) — the season-granularity analogue of Radarr's
	// alreadyUnmonitoredCount, scoped to a single series here so the caller
	// can sum it across the whole library.
	alreadyUnmonitored int
}

// episodeHasAired reports whether an episode's airDateUtc is in the past
// relative to now, treating an absent OR unparseable airDateUtc as NOT
// aired. This is rule 3's fail-safe contract verbatim (binding controller
// resolution #2: "an episode with absent airDateUtc key counts as NOT
// aired... the guard fails safe"), extended the same way to a value that is
// present but not a valid RFC3339 timestamp: either shape means the airing
// status genuinely cannot be determined from this data, and treating either
// as "probably aired" would be exactly the untrusted-input route to a
// would-unmonitor decision on a still-airing season that §2.6 forbids — the
// single most important Sonarr guard this whole engine exists to protect.
func episodeHasAired(e episodeElement, now time.Time) bool {
	aired, _ := episodeAiringStatus(e, now)
	return aired
}

// episodeAiringStatus is episodeHasAired's finer-grained sibling: it
// separately reports whether the airing status could not be determined at
// all — airDateUtc absent (JSON key missing, or present-but-null, either of
// which decodes to a nil *string) or present but not a valid RFC3339
// timestamp — as opposed to a valid, parseable date that simply lies in the
// future. Both shapes make aired=false, but only the untrusted one is a
// data-quality problem worth a WARN (REVIEW FIX, appended controller note
// 2: "airDateUtc absent OR JSON-null on any episode = untrusted -> skip
// season with warn"); a genuinely future-dated episode is the guard doing
// exactly its job on healthy data; every other untrusted-input path in this
// function warns (statistics missing, zero/short episode list, file-count
// mismatch, missing CF score) — this one must too, distinguishably from the
// "still airing" case it must never be confused with in the log stream.
func episodeAiringStatus(e episodeElement, now time.Time) (aired bool, untrusted bool) {
	if e.AirDateUtc == nil {
		return false, true
	}
	t, err := time.Parse(time.RFC3339, *e.AirDateUtc)
	if err != nil {
		return false, true
	}
	return t.Before(now), false
}

// evaluateSeries applies the Sonarr season decision rule to every monitored
// season of one series, in the binding evaluation order described above.
// Rule 1's SERIES-level half (series.monitored == true) is the caller's
// responsibility, exactly as rule 1 (movie.monitored == true) is
// evaluateMovie's caller's responsibility in the Radarr engine — a series
// that is not monitored produces no seasonDecision at all, for any of its
// seasons. Rule 1's SEASON-level half (season.monitored == true) IS handled
// here, since it is intrinsically about this series' own season list.
func evaluateSeries(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance, s seriesElement, profiles map[int]qualityProfile, exclusionTagID int, tagActive bool, wantedSeasons map[seasonKey]bool) seriesEvaluation {
	seriesID := *s.ID // caller guarantees non-nil
	seriesTitle := titleOrAbsent(s.Title)

	// REVIEW FIX: Seasons is now a pointer (sonarr.go) precisely so this
	// absence can be told apart from a genuinely empty list and warned on —
	// a monitored series whose "seasons" key never decoded at all must not
	// silently vanish into zero decisions and zero log lines.
	warnIfFieldAbsent(logger, inst, "series", "seasons", s.Seasons == nil)
	var seasons []seriesSeasonElement
	if s.Seasons != nil {
		seasons = *s.Seasons
	}

	// Rule 1 (season half): partition this series' seasons into monitored,
	// explicitly-unmonitored, and excluded-as-untrusted (monitored absent,
	// or seasonNumber absent — without a season number a decision can be
	// evaluated but never safely reported or looked up in wantedSeasons, the
	// same defensive posture Radarr's fetchWantedCutoff takes toward a
	// record missing its id).
	var monitoredSeasons []seriesSeasonElement
	var explicitlyUnmonitored []seriesSeasonElement
	for _, season := range seasons {
		warnIfFieldAbsent(logger, inst, "season", "monitored", season.Monitored == nil)
		if season.Monitored == nil {
			continue
		}
		if !*season.Monitored {
			explicitlyUnmonitored = append(explicitlyUnmonitored, season)
			continue
		}
		if season.SeasonNumber == nil {
			logger.Warn("skipping season: missing seasonNumber field",
				"instance", inst.Name, "type", inst.Type, "seriesId", seriesID, "series", seriesTitle)
			continue
		}
		monitoredSeasons = append(monitoredSeasons, season)
	}

	// "Series with zero monitored seasons: single debug line, no per-season
	// spam" (binding controller resolution #1) — the noise-budget case: a
	// series entirely composed of unmonitored seasons (a fully completed,
	// deliberately-unmonitored classic show, most commonly) would otherwise
	// produce one debug line per season, every single cycle, forever.
	if len(monitoredSeasons) == 0 {
		switch {
		case len(explicitlyUnmonitored) > 0:
			logger.Debug("series has no monitored seasons",
				"instance", inst.Name, "type", inst.Type, "seriesId", seriesID, "series", seriesTitle, "seasons", len(explicitlyUnmonitored))
		default:
			// REVIEW FIX (PROBE C): neither monitored nor explicitly-
			// unmonitored seasons at all — an empty (or entirely absent,
			// entirely untrusted-shaped) "seasons" list on a series that IS
			// itself monitored. Indistinguishable from "nothing to do"
			// unless this warns: a monitored series counted in
			// totalSeriesMonitored must never vanish with zero decisions
			// and zero log lines of any level (§2.6; the same posture the
			// Phase 4 --only-id precedent takes toward silent zero-result
			// scopes).
			logger.Warn("monitored series produced no season decisions: no seasons, or none had a usable monitored/seasonNumber field",
				"instance", inst.Name, "type", inst.Type, "seriesId", seriesID, "series", seriesTitle)
		}
		return seriesEvaluation{alreadyUnmonitored: len(explicitlyUnmonitored)}
	}

	// The complementary case: at least one season IS monitored, so the
	// individual "already unmonitored" debug lines for the OTHER seasons are
	// exactly as informative as Radarr's per-movie equivalent, not spam.
	for _, season := range explicitlyUnmonitored {
		logger.Debug("season already unmonitored",
			"instance", inst.Name, "type", inst.Type, "seriesId", seriesID, "series", seriesTitle, "season", derefOrAbsent(season.SeasonNumber))
	}

	// Series-level cheap checks (binding evaluation-order resolution):
	// profile display-name resolution happens eagerly (mirrors evaluateMovie
	// FIX 5), then tags, then profile existence + upgradeAllowed — in that
	// checking order, matched to the plan's rule numbers (5 then 6) — all
	// resolved ONCE and, if any fails, applied uniformly to EVERY monitored
	// season of this series, before the one /episode fetch a passing series
	// needs even happens.
	var profile qualityProfile
	profileFound := false
	profileName := ""
	if s.QualityProfileID != nil {
		profile, profileFound = profiles[*s.QualityProfileID]
		if profileFound {
			profileName = profile.Name
		}
	}

	warnIfFieldAbsent(logger, inst, "series", "tags", s.Tags == nil)
	seriesLevelReason := ""
	switch {
	case tagActive && s.Tags == nil:
		seriesLevelReason = ReasonTagsUnknown
	case tagActive && containsTag(s.Tags, exclusionTagID):
		seriesLevelReason = ReasonExcludedByTag
	case !profileFound:
		logger.Warn("skipping series: unknown quality profile",
			"instance", inst.Name, "type", inst.Type, "series", seriesTitle, "qualityProfileId", derefOrAbsent(s.QualityProfileID))
		seriesLevelReason = ReasonUnknownProfile
		profileName = "unknown"
	case !profile.UpgradeAllowed:
		seriesLevelReason = ReasonUpgradesDisabled
	}

	if seriesLevelReason != "" {
		threshold := 0
		if profileFound {
			threshold = profile.CutoffFormatScore
		}
		decisions := make([]seasonDecision, 0, len(monitoredSeasons))
		for _, season := range monitoredSeasons {
			decisions = append(decisions, seasonDecision{
				seriesID: seriesID, series: seriesTitle, season: *season.SeasonNumber,
				reason: seriesLevelReason, profileName: profileName, cfThreshold: threshold,
			})
		}
		return seriesEvaluation{decisions: decisions, alreadyUnmonitored: len(explicitlyUnmonitored)}
	}

	// The series survived every series-level check: exactly one
	// /episode?seriesId=X fetch, shared by every monitored season of this
	// series, per the binding evaluation-order resolution.
	episodes, ok := fetchEpisodes(ctx, logger, client, inst, seriesID)
	if !ok {
		decisions := make([]seasonDecision, 0, len(monitoredSeasons))
		for _, season := range monitoredSeasons {
			decisions = append(decisions, seasonDecision{
				seriesID: seriesID, series: seriesTitle, season: *season.SeasonNumber,
				reason: ReasonSeasonEpisodesUnavailable, profileName: profileName, cfThreshold: profile.CutoffFormatScore,
			})
		}
		return seriesEvaluation{decisions: decisions, alreadyUnmonitored: len(explicitlyUnmonitored)}
	}

	episodesBySeason := make(map[int][]episodeElement, len(monitoredSeasons))
	for _, e := range episodes {
		if e.SeasonNumber == nil {
			logger.Warn("episode missing seasonNumber field; excluded from every season's evaluation",
				"instance", inst.Name, "type", inst.Type, "seriesId", seriesID, "series", seriesTitle, "episodeId", derefOrAbsent(e.ID))
			continue
		}
		episodesBySeason[*e.SeasonNumber] = append(episodesBySeason[*e.SeasonNumber], e)
	}

	now := time.Now().UTC()
	var decisions []seasonDecision
	var candidateSeasons []int          // season numbers passing rules 1-6, in evaluation order
	candidateIndex := make(map[int]int) // season number -> index into decisions
	seasonEpisodesFor := make(map[int][]episodeElement)
	statsFileCountFor := make(map[int]int)

	for _, season := range monitoredSeasons {
		sn := *season.SeasonNumber
		d := seasonDecision{seriesID: seriesID, series: seriesTitle, season: sn, profileName: profileName, cfThreshold: profile.CutoffFormatScore}

		stats := season.Statistics
		if stats == nil || stats.EpisodeFileCount == nil || stats.TotalEpisodeCount == nil {
			logger.Warn("skipping season: statistics missing required fields",
				"instance", inst.Name, "type", inst.Type, "seriesId", seriesID, "series", seriesTitle, "season", sn)
			d.reason = ReasonSeasonEpisodeDataInconsistent
			decisions = append(decisions, d)
			continue
		}
		statsFileCountFor[sn] = *stats.EpisodeFileCount

		// Rule 2: complete on disk.
		if !(*stats.EpisodeFileCount == *stats.TotalEpisodeCount && *stats.TotalEpisodeCount > 0) {
			d.reason = ReasonSeasonIncomplete
			decisions = append(decisions, d)
			continue
		}
		d.completeOnDisk = true

		seasonEpisodes := episodesBySeason[sn]

		// CRITICAL REVIEW FIX: binding controller resolution #2's
		// inconsistent-data guard, generalized from "zero episodes returned"
		// to ANY count that disagrees with statistics.totalEpisodeCount.
		// The zero-episode case alone left a hole the size of the guard it
		// sits next to: statistics can claim completeness while /episode
		// returns a SHORT list — e.g. episodeFileCount=3/totalEpisodeCount=3
		// but only the 2 already-aired episodes come back, the third
		// (still-to-air) one silently absent — and rule 3 below would then
		// run the airing check over an incomplete set and find every episode
		// it DID see already aired, reporting would-unmonitor on a season
		// that is still airing. The same gap also swallows an episode that
		// IS present but missing its own seasonNumber (dropped, with its own
		// warning, at the episodesBySeason build above): that episode simply
		// never reaches this season's slice, so a short count here is the
		// only place left to catch it. Untrusted data either way, not
		// evidence of anything — this is the "single most important Sonarr
		// guard"'s own input, so a partial set must skip with a reason
		// exactly like every other untrusted-input path in this function
		// does, never be guessed at (§2.6).
		if len(seasonEpisodes) != *stats.TotalEpisodeCount {
			logger.Warn("skipping season: episode count returned by /episode does not match statistics.totalEpisodeCount",
				"instance", inst.Name, "type", inst.Type, "seriesId", seriesID, "series", seriesTitle, "season", sn,
				"episodesReturned", len(seasonEpisodes), "totalEpisodeCount", *stats.TotalEpisodeCount)
			d.reason = ReasonSeasonEpisodeDataInconsistent
			decisions = append(decisions, d)
			continue
		}

		// Rule 3: the airing guard. Every episode of the season must have
		// aired; a single not-yet-aired (or undated) episode fails the
		// whole season, and — per the binding precedence pin — this is
		// checked, and can report, before rule 4 even looks at the wanted
		// set, so an airing season that also happens to have an episode in
		// /wanted/cutoff is reported as airing, never as cutoff-not-met.
		//
		// IMPORTANT REVIEW FIX: episodeAiringStatus's untrusted bit is
		// checked and warned on HERE, separately from allAired. Before this
		// fix, an absent/JSON-null/unparseable airDateUtc fell through to
		// the ordinary INFO "skip" line with reason=ReasonSeasonNotFullyAired
		// — byte-identical to a genuinely still-airing season — so the
		// report could never distinguish "healthy, expected: still airing"
		// from "this season's episode data is corrupt/undated", even though
		// every other untrusted-input path in this function (statistics
		// missing, episode count mismatch, file-count mismatch, missing CF
		// score) does warn. Appended controller note 2 is explicit on this
		// exact shape.
		allAired := true
		for _, e := range seasonEpisodes {
			aired, untrusted := episodeAiringStatus(e, now)
			if untrusted {
				logger.Warn("skipping season: episode airDateUtc is absent or unparseable; airing status cannot be determined",
					"instance", inst.Name, "type", inst.Type, "seriesId", seriesID, "series", seriesTitle, "season", sn, "episodeId", derefOrAbsent(e.ID))
			}
			if !aired {
				allAired = false
				break
			}
		}
		if !allAired {
			d.reason = ReasonSeasonNotFullyAired
			// IMPORTANT REVIEW FIX: retained so the cross-check's skip-side
			// pool (runSonarrCrossCheck) can make its own bounded, on-demand
			// /episodefile fetch for this season if it gets sampled — this
			// season never reaches rule 7, so evaluateSeries itself never
			// fetches /episodefile for it.
			d.rawEpisodesForCrossCheck = seasonEpisodes
			decisions = append(decisions, d)
			continue
		}

		// Rule 4: no episode of the season appears in /wanted/cutoff — the
		// (seriesId, seasonNumber) lookup built directly from the wanted
		// records themselves (binding controller resolution).
		if wantedSeasons[seasonKey{seriesID: seriesID, seasonNumber: sn}] {
			d.reason = ReasonQualityCutoffNotMet
			// IMPORTANT REVIEW FIX: same rationale as the rule-3 skip above
			// — this is precisely the skip reason whose independent signal
			// (episodeFile.qualityCutoffNotMet == true) is most worth
			// verifying, and without this the cross-check's skip-side pool
			// could never verify it at all.
			d.rawEpisodesForCrossCheck = seasonEpisodes
			decisions = append(decisions, d)
			continue
		}

		// Rules 1-6 all pass: this season is a rule-7 candidate. Its final
		// reason/wouldUnmonitor is filled in below, once (and only if) the
		// per-series /episodefile fetch this candidate justifies actually
		// happens.
		candidateIndex[sn] = len(decisions)
		decisions = append(decisions, d)
		candidateSeasons = append(candidateSeasons, sn)
		seasonEpisodesFor[sn] = seasonEpisodes
	}

	if len(candidateSeasons) == 0 {
		return seriesEvaluation{decisions: decisions, alreadyUnmonitored: len(explicitlyUnmonitored)}
	}

	// The most expensive fetch, made at most once per series, and only for
	// a series that actually has at least one rule-1-6-passing season
	// (binding evaluation-order resolution).
	files, ok := fetchEpisodeFiles(ctx, logger, client, inst, seriesID)
	if !ok {
		for _, sn := range candidateSeasons {
			decisions[candidateIndex[sn]].reason = ReasonCouldNotFetchCFScore
		}
		return seriesEvaluation{decisions: decisions, alreadyUnmonitored: len(explicitlyUnmonitored)}
	}

	filesBySeason := make(map[int][]episodeFileElement)
	filesByID := make(map[int]episodeFileElement, len(files))
	for _, f := range files {
		if f.SeasonNumber != nil {
			filesBySeason[*f.SeasonNumber] = append(filesBySeason[*f.SeasonNumber], f)
		}
		if f.ID != nil {
			filesByID[*f.ID] = f
		}
	}

	for _, sn := range candidateSeasons {
		idx := candidateIndex[sn]
		seasonFiles := filesBySeason[sn]

		// Binding controller resolution #3: fewer files than statistics
		// claimed is untrusted, not "some files are just missing".
		if len(seasonFiles) < statsFileCountFor[sn] {
			logger.Warn("skipping season: fewer episode files returned than statistics.episodeFileCount claims",
				"instance", inst.Name, "type", inst.Type, "seriesId", seriesID, "series", seriesTitle, "season", sn,
				"filesReturned", len(seasonFiles), "episodeFileCount", statsFileCountFor[sn])
			decisions[idx].reason = ReasonSeasonFileCountMismatch
			continue
		}

		// Rule 7 (STRICT amendment): every episode file's customFormatScore
		// must be present and meet the profile's threshold.
		missingScore := false
		belowThreshold := false
		for _, f := range seasonFiles {
			if f.CustomFormatScore == nil {
				missingScore = true
				break
			}
			if *f.CustomFormatScore < profile.CutoffFormatScore {
				belowThreshold = true
				break
			}
		}

		switch {
		case missingScore:
			logger.Warn("skipping season: could not determine custom format score for one or more episode files",
				"instance", inst.Name, "type", inst.Type, "seriesId", seriesID, "series", seriesTitle, "season", sn)
			decisions[idx].reason = ReasonCouldNotFetchCFScore
		case belowThreshold:
			decisions[idx].reason = ReasonCFCutoffNotMet
		default:
			decisions[idx].wouldUnmonitor = true
			decisions[idx].reason = ReasonCutoffMet
		}

		// Cross-check data: pair each episode's own id (wanted-set membership
		// is looked up from it later, by runSonarrCrossCheck) with its
		// file's qualityCutoffNotMet and its own monitored flag — shared
		// join logic (buildSeasonCrossCheckEpisodes) with the cross-check's
		// own on-demand fetch for skip-side seasons, rather than a second
		// copy of it.
		decisions[idx].crossCheckEpisodes = buildSeasonCrossCheckEpisodes(seasonEpisodesFor[sn], filesByID)
	}

	return seriesEvaluation{decisions: decisions, alreadyUnmonitored: len(explicitlyUnmonitored)}
}

// runSonarrDecisionEngine is the entry point for a single sonarr instance.
// It fetches quality profiles and resolves the exclusion tag once (the same
// instance-generic helpers the Radarr engine uses — fetchQualityProfiles,
// resolveExclusionTagID), then evaluates every monitored series against
// evaluateSeries, reporting a would-unmonitor or skip line per season, then
// runs the cross-check (runSonarrCrossCheck).
//
// There is no write pass in this phase (Phase 7 territory): --only-id and
// dry_run play no role here at all, per the binding scope guard that Sonarr
// writes arrive later and that no code path in this engine may ever compose
// a write, series-level or otherwise. Like checkInstanceConnectivity,
// inspectRadarrLibrary, and runRadarrDecisionEngine, it never returns
// anything: the binding error-handling rule (§2.6) is "skip that instance
// for the cycle and log a warning", with no further work for a caller to
// gate on.
func runSonarrDecisionEngine(ctx context.Context, logger *slog.Logger, inst Instance, series []seriesElement, wantedEpisodeIDs map[int]bool, wantedSeasons map[seasonKey]bool, exclusionTagLabel string) {
	client := NewAPIClient(inst.URL, inst.APIKey)

	profiles, ok := fetchQualityProfiles(ctx, logger, client, inst)
	if !ok {
		return
	}

	exclusionTagID, tagActive, ok := resolveExclusionTagID(ctx, logger, client, inst, exclusionTagLabel)
	if !ok {
		return
	}

	var allDecisions []seasonDecision
	totalSeriesMonitored := 0
	seasonsEvaluated := 0
	wouldUnmonitorCount := 0
	alreadyUnmonitoredCount := 0
	skipCounts := make(map[string]int)

	for _, s := range series {
		// Rule 1 (series half): mirrors Radarr's own rule 1 exactly — a
		// series that is not monitored (or whose monitored field could not
		// even be observed) is excluded entirely, with no seasonDecision
		// produced for any of its seasons and nothing counted, the same
		// "excluding is the safe direction, absence is untrusted rather than
		// a state" split evaluateMovie's caller applies to movies.
		warnIfFieldAbsent(logger, inst, "series", "monitored", s.Monitored == nil)
		if s.Monitored == nil {
			continue
		}
		if !*s.Monitored {
			logger.Debug("series not monitored",
				"instance", inst.Name, "type", inst.Type, "seriesId", derefOrAbsent(s.ID), "series", titleOrAbsent(s.Title))
			continue
		}
		if s.ID == nil {
			// Defensive: without an id this series can't be evaluated
			// against wantedSeasons or queried at /episode, and can't be
			// safely reported. Mirrors how the Radarr engine treats a movie
			// missing its id.
			logger.Warn("skipping series: missing id field",
				"instance", inst.Name, "type", inst.Type, "title", titleOrAbsent(s.Title))
			continue
		}
		totalSeriesMonitored++

		eval := evaluateSeries(ctx, logger, client, inst, s, profiles, exclusionTagID, tagActive, wantedSeasons)
		alreadyUnmonitoredCount += eval.alreadyUnmonitored
		allDecisions = append(allDecisions, eval.decisions...)

		for _, d := range eval.decisions {
			seasonsEvaluated++
			if d.wouldUnmonitor {
				wouldUnmonitorCount++
				logger.Info("would-unmonitor",
					"instance", inst.Name, "seriesId", d.seriesID, "series", d.series, "season", d.season, "reason", d.reason, "profile", d.profileName)
			} else {
				skipCounts[d.reason]++
				logger.Info("skip",
					"instance", inst.Name, "seriesId", d.seriesID, "series", d.series, "season", d.season, "reason", d.reason, "profile", d.profileName)
			}
		}
	}

	cc := runSonarrCrossCheck(ctx, logger, client, inst, allDecisions, wantedEpisodeIDs)
	crossCheckSummary := renderCrossCheckSummary(cc.status, cc.verified, cc.unverifiable)

	logger.Info("sonarr decision summary",
		"instance", inst.Name, "type", inst.Type,
		"totalSeriesMonitored", totalSeriesMonitored, "seasonsEvaluated", seasonsEvaluated,
		"wouldUnmonitor", wouldUnmonitorCount, "alreadyUnmonitored", alreadyUnmonitoredCount,
		"skipReasons", formatSkipCounts(skipCounts), "crossCheck", crossCheckSummary)
}

// seasonKeySpread encodes a season decision's (seriesId, season) pair into
// a single int for sampleEveryKth (the SAME sampler the Radarr engine uses —
// reused rather than forked, per the binding reuse mandate): a Sonarr season
// has no numeric id of its own the way a Radarr movie does, so the two
// numbers that DO identify it uniquely are combined into one sortable,
// deterministic key instead. seasonKeySpread comfortably exceeds any real
// season count — Sonarr season numbers are small, and even an extreme
// multi-cour anime library stays far below it — while keeping every combined
// key well within int range for any realistic seriesId.
const seasonKeySpread = 100000

func sonarrCrossCheckSampleKey(seriesID, season int) int {
	return seriesID*seasonKeySpread + season
}

// runSonarrCrossCheck implements plan §6's cross-check for the season
// engine (evidence-based per the binding controller resolution, adapted
// from runCrossCheck's Radarr shape): for up to 10 deterministically
// sampled would-unmonitor seasons and up to 10 deterministically sampled
// complete-on-disk skip seasons (fewer if fewer exist), it independently
// verifies that wanted-set membership (wantedEpisodeIDs, the same source
// rule 4 effectively consults) agrees with each episode's own
// episodeFile.qualityCutoffNotMet — computed by a different Sonarr code
// path and expected to never disagree.
//
// IMPORTANT REVIEW FIX: unlike Radarr's cross-check, this one CAN make
// additional API calls — a small, bounded, read-only number of them.
// Originally it made none at all, re-using only data evaluateSeries had
// already retained; but every rule-3 (airing) and rule-4 (in the wanted
// set) skip never reaches rule 7, so evaluateSeries never fetches
// /episodefile for it, and the skip-side pool was therefore structurally
// unverifiable by construction for precisely the skip reasons whose
// independent signal is most worth checking (a rule-4 skip's file should
// read qualityCutoffNotMet=true). For a sampled skip-side season whose
// crossCheckEpisodes is nil but whose rawEpisodesForCrossCheck IS
// populated (set only for rule-3/rule-4 skips), this makes its own
// per-series /episodefile fetch — capped by construction at
// crossCheckSampleSize (10) skip-side samples per instance per cycle,
// deduplicated per series via fetchedFilesBySeries so a series sampled via
// more than one season is only fetched once.
//
// verified/unverifiable are counted at the SEASON granularity a sample
// actually operates at: a sampled season counts as verified if at least one
// of its episodes could be compared, unverifiable if none could — mirroring
// Radarr's per-item split at the one granularity level this engine's
// samples exist. Any disagreement among actually-compared episodes is
// logged at error level and fails the whole cross-check
// (crossCheckStatusFailed); if every sampled season was unverifiable, the
// status is crossCheckStatusInconclusive rather than a false pass — the
// same "a sample that verified nothing must never look like a sample that
// verified everything and found no problems" rule Radarr's cross-check
// established.
//
// Two per-episode shapes are excluded from comparison entirely rather than
// flagged as disagreements (IMPORTANT REVIEW FIX, both confirmed live
// facts): (a) an episode whose own monitored is nil or false — Sonarr's
// /wanted/cutoff pool is filtered to monitored episodes, so an unmonitored
// episode inside an otherwise-monitored season can never appear in the
// wanted set regardless of its file's qualityCutoffNotMet; and (b) the
// mirror-image shape inWantedSet=true with qualityCutoffNotMet=false —
// /wanted/cutoff also lists episodes below the profile's custom-format
// cutoff even when the QUALITY cutoff is met (confirmed live: anime
// profile max ~3890, sample episodefile score 2120), which
// episodeFile.qualityCutoffNotMet never reflects, so this shape is fully
// explained by a legitimate CF-only wanted reason and is not evidence of
// anything wrong.
func runSonarrCrossCheck(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance, decisions []seasonDecision, wantedEpisodeIDs map[int]bool) crossCheckResult {
	byKey := make(map[int]seasonDecision, len(decisions))
	var wouldUnmonitorKeys, skipKeys []int
	for _, d := range decisions {
		key := sonarrCrossCheckSampleKey(d.seriesID, d.season)
		byKey[key] = d
		if d.wouldUnmonitor {
			wouldUnmonitorKeys = append(wouldUnmonitorKeys, key)
		} else if d.completeOnDisk {
			skipKeys = append(skipKeys, key)
		}
	}

	sampled := append(sampleEveryKth(wouldUnmonitorKeys, crossCheckSampleSize), sampleEveryKth(skipKeys, crossCheckSampleSize)...)

	// fetchedFilesBySeries caches the on-demand /episodefile fetch below by
	// series id: a nil map value (present key, nil map) records "already
	// tried, the fetch failed" so a failing series is never retried within
	// the same cross-check call.
	fetchedFilesBySeries := make(map[int]map[int]episodeFileElement)

	var result crossCheckResult
	disagreementFound := false
	for _, key := range sampled {
		d := byKey[key]

		crossCheckEpisodes := d.crossCheckEpisodes
		if len(crossCheckEpisodes) == 0 && len(d.rawEpisodesForCrossCheck) > 0 {
			filesByID, cached := fetchedFilesBySeries[d.seriesID]
			if !cached {
				if files, ok := fetchEpisodeFiles(ctx, logger, client, inst, d.seriesID); ok {
					filesByID = make(map[int]episodeFileElement, len(files))
					for _, f := range files {
						if f.ID != nil {
							filesByID[*f.ID] = f
						}
					}
				}
				fetchedFilesBySeries[d.seriesID] = filesByID
			}
			if filesByID != nil {
				crossCheckEpisodes = buildSeasonCrossCheckEpisodes(d.rawEpisodesForCrossCheck, filesByID)
			}
		}

		if len(crossCheckEpisodes) == 0 {
			result.unverifiable++
			if d.wouldUnmonitor {
				result.writeUnverifiable++
			}
			logger.Warn("cross-check: no episode file data available to verify this season",
				"instance", inst.Name, "seriesId", d.seriesID, "series", d.series, "season", d.season)
			continue
		}

		// REVIEW FIX (Phase 6 final round, F3): seasonVerified (renamed
		// nowhere — still the exact test runSonarrCrossCheck's own
		// verified/unverifiable split below reads) counts episodes that
		// reached the final agreement/disagreement comparison;
		// seasonFinalDisagreed counts how many of THOSE disagreed, so
		// seasonVerified-seasonFinalDisagreed is always >= 0 (it can never
		// undercount, unlike a counter that also folded in the structural
		// would-unmonitor/wanted-set contradiction below, which fires on
		// episodes this final comparison may exclude entirely).
		// seasonUnverifiableEpisodes counts episodes excluded from
		// comparison for a DATA reason — a missing qualityCutoffNotMet, or
		// the CF-only ambiguous shape (b) the binding live-probe ruling
		// says to "classify as unverifiable, never a disagreement" — as
		// opposed to shape (a)'s unmonitored-episode exclusion, which is
		// simply out of the comparable population and contributes to
		// neither count.
		seasonVerified := 0
		seasonFinalDisagreed := 0
		seasonUnverifiableEpisodes := 0
		for _, ep := range crossCheckEpisodes {
			if ep.monitored == nil || !*ep.monitored {
				// IMPORTANT REVIEW FIX: excluded from the comparison
				// entirely, not counted toward verified or unverifiable —
				// see the function doc comment, shape (a).
				continue
			}

			inWantedSet := wantedEpisodeIDs[ep.episodeID]

			// IMPORTANT REVIEW FIX: a would-unmonitor season is rule 4's own
			// claim that NO episode of this season appears in the wanted set
			// — decided via the SEASON key (seriesID, seasonNumber) against
			// wantedSeasons. This cross-check independently consults
			// wantedEpisodeIDs at the EPISODE level for the very same
			// /wanted/cutoff fetch. If an episode of a would-unmonitor season
			// IS in wantedEpisodeIDs, that is a flat contradiction of the
			// rule that produced the decision — not something the
			// qualityCutoffNotMet comparison below can explain away (in the
			// qualityCutoffNotMet==true case it would otherwise fall straight
			// through as a "verified" agreement). Check and flag it BEFORE
			// the nil/CF-only continues below so no per-field shape can ever
			// suppress it.
			if d.wouldUnmonitor && inWantedSet {
				disagreementFound = true
				logger.Error("cross-check disagreement: a would-unmonitor season contains an episode in the wanted/cutoff set",
					"instance", inst.Name, "seriesId", d.seriesID, "series", d.series, "season", d.season,
					"episodeId", ep.episodeID, "inWantedSet", inWantedSet, "qualityCutoffNotMet", derefOrAbsent(ep.qualityCutoffNotMet))
			}

			// REVIEW FIX (Phase 6 final round, F3): demoted from Info to
			// Debug. Up to 20 sampled seasons of ordinary 12-24-episode
			// seasons produced 200-500 INFO lines per instance per cycle —
			// burying the ERROR disagreement and WARN unverifiable lines
			// this cross-check exists to surface, the same noise-budget
			// hazard this project has already fixed twice by mandate
			// elsewhere. The one-line-per-sampled-season INFO aggregate
			// below replaces it at INFO; this per-episode detail is still
			// available, just gated behind Debug.
			attrs := []any{
				"instance", inst.Name, "seriesId", d.seriesID, "series", d.series, "season", d.season,
				"episodeId", ep.episodeID, "inWantedSet", inWantedSet, "qualityCutoffNotMet", derefOrAbsent(ep.qualityCutoffNotMet),
			}
			if d.wouldUnmonitor {
				attrs = append(attrs, "cfThreshold", d.cfThreshold)
			}
			logger.Debug("cross-check", attrs...)

			if ep.qualityCutoffNotMet == nil {
				// Data-quality issue distinct from an actual disagreement,
				// same as Radarr's cross-check: silently treating "absent"
				// as "false" here could mask a real disagreement.
				seasonUnverifiableEpisodes++
				continue
			}
			if inWantedSet && !*ep.qualityCutoffNotMet {
				// IMPORTANT REVIEW FIX: the CF-only ambiguous shape — see
				// the function doc comment, shape (b), and the binding
				// live-probe ruling ("classify as unverifiable, never a
				// disagreement"). Not a disagreement.
				seasonUnverifiableEpisodes++
				continue
			}
			seasonVerified++
			if inWantedSet != *ep.qualityCutoffNotMet {
				disagreementFound = true
				seasonFinalDisagreed++
				logger.Error("cross-check disagreement: wanted-set membership does not match episodeFile.qualityCutoffNotMet",
					"instance", inst.Name, "seriesId", d.seriesID, "series", d.series, "season", d.season,
					"episodeId", ep.episodeID, "inWantedSet", inWantedSet, "qualityCutoffNotMet", *ep.qualityCutoffNotMet)
			}
		}

		// REVIEW FIX (Phase 6 final round, F3): the season-level INFO
		// aggregate replacing the per-episode INFO lines above — one line
		// per sampled season, stating the verdict this season landed in
		// plus the three counts a human needs to judge it without reading
		// every per-episode Debug line: how many episodes were actually
		// comparable (compared), how many of those agreed (agreed), and how
		// many could not be compared at all (unverifiable). Any
		// disagreement is still separately visible via its own ERROR line
		// above; this line is a summary, not a replacement for it.
		verdict := "unverifiable"
		if seasonVerified > 0 {
			verdict = "verified"
		}
		logger.Info("cross-check season",
			"instance", inst.Name, "seriesId", d.seriesID, "series", d.series, "season", d.season, "wouldUnmonitor", d.wouldUnmonitor,
			"verdict", verdict, "compared", seasonVerified, "agreed", seasonVerified-seasonFinalDisagreed, "unverifiable", seasonUnverifiableEpisodes)

		if seasonVerified > 0 {
			result.verified++
			if d.wouldUnmonitor {
				result.writeVerified++
			}
		} else {
			result.unverifiable++
			if d.wouldUnmonitor {
				result.writeUnverifiable++
			}
			logger.Warn("cross-check: no episode in this season had a comparable episodeFile.qualityCutoffNotMet; cannot verify wanted-set agreement for this season",
				"instance", inst.Name, "seriesId", d.seriesID, "series", d.series, "season", d.season)
		}
	}

	switch {
	case disagreementFound:
		result.status = crossCheckStatusFailed
	case len(sampled) > 0 && result.verified == 0:
		result.status = crossCheckStatusInconclusive
		logger.Warn("cross-check: every sampled season was unverifiable (no comparable episodeFile.qualityCutoffNotMet); cannot determine pass or fail",
			"instance", inst.Name, "unverifiable", result.unverifiable)
	default:
		result.status = crossCheckStatusPassed
	}

	return result
}
