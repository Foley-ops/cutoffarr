package main

import (
	"context"
	"encoding/json"
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

	// Rule 2: hasFile == true.
	if !hasFile {
		d.reason = "no file"
		return d
	}

	// Rule 3: quality profile exists in the fetched set AND upgradeAllowed.
	var profile qualityProfile
	profileFound := false
	if m.QualityProfileID != nil {
		profile, profileFound = profiles[*m.QualityProfileID]
	}
	if !profileFound {
		logger.Warn("skipping movie: unknown quality profile",
			"instance", inst.Name, "type", inst.Type, "title", d.title, "qualityProfileId", derefOrAbsent(m.QualityProfileID))
		d.reason = "unknown quality profile"
		d.profileName = "unknown"
		return d
	}
	d.profileName = profile.Name
	d.cfThreshold = profile.CutoffFormatScore
	if !profile.UpgradeAllowed {
		d.reason = "profile has upgrades disabled"
		return d
	}

	// Rule 4: tags do not include the exclusion tag id (only when the tag
	// is actually defined in this instance; otherwise rule 4 always
	// passes, per the tag-resolution rules).
	if tagActive && containsTag(m.Tags, exclusionTagID) {
		d.reason = "excluded by tag"
		return d
	}

	// Rule 5: movie id is NOT in the complete /wanted/cutoff id set.
	if wantedIDs[id] {
		d.reason = "quality cutoff not met"
		return d
	}

	// Rule 6: the file's customFormatScore >= the profile's
	// cutoffFormatScore. Only rules 1-5 having passed reaches this point,
	// so /moviefile is fetched lazily, exactly once, right here.
	files, ok := fetchMovieFileDetails(ctx, logger, client, inst, id)
	if !ok {
		d.reason = "could not fetch custom format score"
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
		d.reason = "could not fetch custom format score"
		return d
	}
	score := *mf.CustomFormatScore
	d.cfScore = &score
	if score < profile.CutoffFormatScore {
		d.reason = "custom format cutoff not met"
		return d
	}

	d.wouldUnmonitor = true
	d.reason = "cutoff met"
	return d
}

// runRadarrDecisionEngine is the Phase 3 entry point for a single radarr
// instance: it fetches the two additional data sources the STRICT decision
// rule needs beyond what inspectRadarrLibrary (Phase 2) already gathered —
// quality profiles (refactor c) and the exclusion tag id (tag-resolution
// rules) — then evaluates every monitored movie in movies, in library
// order, logging a "would-unmonitor" or "skip" report line for each
// (msg="would-unmonitor" / msg=skip, per the plan), runs the cross-check,
// and logs an end-of-instance summary. It is called only for a radarr
// instance whose connectivity check and inspectRadarrLibrary fetch both
// already succeeded (main.go's responsibility); it never returns anything
// because, like checkInstanceConnectivity and inspectRadarrLibrary, the
// binding error-handling rule (§2.6) is "skip that instance for the cycle
// and log a warning" with no further work for a caller to gate on.
func runRadarrDecisionEngine(ctx context.Context, logger *slog.Logger, inst Instance, movies []movieListElement, wantedIDs map[int]bool, exclusionTagLabel string) {
	client := NewAPIClient(inst.URL, inst.APIKey)

	profiles, ok := fetchQualityProfiles(ctx, logger, client, inst)
	if !ok {
		return
	}

	exclusionTagID, tagActive, ok := resolveExclusionTagID(ctx, logger, client, inst, exclusionTagLabel)
	if !ok {
		return
	}

	var decisions []movieDecision
	totalMonitored := 0
	wouldUnmonitorCount := 0
	skipCounts := make(map[string]int)

	for _, m := range movies {
		if m.Monitored == nil || !*m.Monitored {
			// Rule 1: excluded from the report entirely, per the plan.
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
		totalMonitored++

		d := evaluateMovie(ctx, logger, client, inst, m, profiles, exclusionTagID, tagActive, wantedIDs)
		decisions = append(decisions, d)

		if d.wouldUnmonitor {
			wouldUnmonitorCount++
			logger.Info("would-unmonitor",
				"title", d.title, "reason", d.reason, "profile", d.profileName, "instance", inst.Name)
		} else {
			skipCounts[d.reason]++
			logger.Info("skip",
				"title", d.title, "reason", d.reason, "profile", d.profileName, "instance", inst.Name)
		}
	}

	crossCheckPassed, crossCheckCount := runCrossCheck(logger, inst, decisions, wantedIDs)
	crossCheckSummary := fmt.Sprintf("passed (%d items)", crossCheckCount)
	if !crossCheckPassed {
		crossCheckSummary = "FAILED"
	}

	logger.Info("radarr decision summary",
		"instance", inst.Name, "type", inst.Type,
		"totalMonitored", totalMonitored, "wouldUnmonitor", wouldUnmonitorCount,
		"skipReasons", formatSkipCounts(skipCounts), "crossCheck", crossCheckSummary)
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

// runCrossCheck is a placeholder pending the dedicated cross-check
// implementation (plan §6, adapted to the STRICT rule): it currently
// treats every call as passed with zero items checked. TODO: replace with
// the real deterministic sampling + agree/disagree logic.
func runCrossCheck(logger *slog.Logger, inst Instance, decisions []movieDecision, wantedIDs map[int]bool) (passed bool, checked int) {
	return true, 0
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
