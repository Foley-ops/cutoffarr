package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// systemStatusResponse decodes the subset of /api/v3/system/status that
// this phase cares about. Extra fields present in the real response (there
// are many) are ignored by encoding/json automatically.
type systemStatusResponse struct {
	AppName string `json:"appName"`
	Version string `json:"version"`
}

// qualityProfileResponse decodes the subset of one element of the
// /api/v3/qualityprofile array that this phase cares about.
type qualityProfileResponse struct {
	Name              string `json:"name"`
	Cutoff            int    `json:"cutoff"`
	CutoffFormatScore int    `json:"cutoffFormatScore"`
	UpgradeAllowed    bool   `json:"upgradeAllowed"`
}

// expectedAppNameByType is the *arr appName each configured instance type
// is expected to report from /system/status. Used only for the informational
// sanity check in this phase, per plan §5's warning not to trust field
// names blindly; a mismatch is logged but never stops processing.
var expectedAppNameByType = map[string]string{
	"radarr": "Radarr",
	"sonarr": "Sonarr",
}

// checkInstanceConnectivity performs this phase's only *arr API calls for a
// single instance: GET /api/v3/system/status followed by
// GET /api/v3/qualityprofile. It logs what it finds and returns; it never
// returns an error because the binding error-handling rule for this phase
// (plan §2.6) is "skip that instance for the cycle and log a warning" —
// callers are expected to loop over every configured instance regardless of
// what happened to any previous one.
func checkInstanceConnectivity(ctx context.Context, logger *slog.Logger, inst Instance) {
	client := NewAPIClient(inst.URL, inst.APIKey)

	statusBody, err := fetchBody(ctx, client, "/api/v3/system/status")
	if err != nil {
		logger.Warn("skipping instance: system/status request failed",
			"instance", inst.Name, "type", inst.Type, "error", err)
		return
	}
	logger.Debug("system/status raw response",
		"instance", inst.Name, "type", inst.Type, "body", string(statusBody))

	var status systemStatusResponse
	if err := json.Unmarshal(statusBody, &status); err != nil {
		logger.Warn("skipping instance: system/status response is not valid JSON",
			"instance", inst.Name, "type", inst.Type, "error", err)
		return
	}

	logger.Info("system status",
		"instance", inst.Name, "type", inst.Type, "appName", status.AppName, "version", status.Version)

	warnIfZeroValue(logger, inst, "system/status", "appName", status.AppName == "")
	warnIfZeroValue(logger, inst, "system/status", "version", status.Version == "")

	if expected, ok := expectedAppNameByType[inst.Type]; ok && !strings.EqualFold(status.AppName, expected) {
		logger.Warn("appName does not match configured instance type",
			"instance", inst.Name, "type", inst.Type, "appName", status.AppName, "expectedAppName", expected)
	}

	profilesBody, err := fetchBody(ctx, client, "/api/v3/qualityprofile")
	if err != nil {
		logger.Warn("skipping instance: qualityprofile request failed",
			"instance", inst.Name, "type", inst.Type, "error", err)
		return
	}
	logger.Debug("qualityprofile raw response",
		"instance", inst.Name, "type", inst.Type, "body", string(profilesBody))

	var profiles []qualityProfileResponse
	if err := json.Unmarshal(profilesBody, &profiles); err != nil {
		logger.Warn("skipping instance: qualityprofile response is not valid JSON",
			"instance", inst.Name, "type", inst.Type, "error", err)
		return
	}

	for _, p := range profiles {
		logger.Info("quality profile",
			"instance", inst.Name, "type", inst.Type,
			"name", p.Name, "cutoff", p.Cutoff, "cutoffFormatScore", p.CutoffFormatScore, "upgradeAllowed", p.UpgradeAllowed)

		warnIfZeroValue(logger, inst, "qualityprofile", "name", p.Name == "")
		warnIfZeroValue(logger, inst, "qualityprofile", "cutoff", p.Cutoff == 0)
		warnIfZeroValue(logger, inst, "qualityprofile", "cutoffFormatScore", p.CutoffFormatScore == 0)
		warnIfZeroValue(logger, inst, "qualityprofile", "upgradeAllowed", !p.UpgradeAllowed)
	}
}

// fetchBody issues a GET against path via client and returns the full
// response body. Any transport error, non-2xx status (client.Do's job),
// or body-read error is returned as-is for the caller to treat as this
// instance's cycle being skipped.
func fetchBody(ctx context.Context, client *APIClient, path string) ([]byte, error) {
	resp, err := client.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body from %s: %w", path, err)
	}
	return body, nil
}

// warnIfZeroValue logs a warning naming field when isZero is true. Per plan
// §5: "If an expected field decodes to its zero value, log a warn naming
// the field rather than silently continuing" — this is a cheap signal that
// our assumed field name may not match the real API (a genuinely absent
// JSON key decodes to Go's zero value with no error), surfaced for the
// human gate to confirm against the running instances. It is informational
// only: it never causes the instance to be skipped.
func warnIfZeroValue(logger *slog.Logger, inst Instance, endpoint, field string, isZero bool) {
	if !isZero {
		return
	}
	logger.Warn(fmt.Sprintf("field %q decoded to zero value", field),
		"instance", inst.Name, "type", inst.Type, "endpoint", endpoint, "field", field)
}
