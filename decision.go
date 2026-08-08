package main

import (
	"context"
	"encoding/json"
	"log/slog"
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
