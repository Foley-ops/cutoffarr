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
//
// Fields are pointers so that "key absent from the JSON" (nil) can be told
// apart from "key present with the zero value" (non-nil pointing at the
// zero value). Only the former means our assumed field name may not match
// the real API; the latter is a legitimate value and must not warn.
type systemStatusResponse struct {
	AppName *string `json:"appName"`
	Version *string `json:"version"`
}

// qualityProfileResponse decodes the subset of one element of the
// /api/v3/qualityprofile array that this phase cares about. See
// systemStatusResponse for why the fields are pointers.
type qualityProfileResponse struct {
	Name              *string `json:"name"`
	Cutoff            *int    `json:"cutoff"`
	CutoffFormatScore *int    `json:"cutoffFormatScore"`
	UpgradeAllowed    *bool   `json:"upgradeAllowed"`
}

// maxResponseBodyBytes caps how much of a single *arr API response body is
// read into memory. Per plan §2.6, a response that reaches this cap is
// treated the same as any other malformed response for that instance: warn
// and skip, rather than buffering an unbounded (or truncated) body.
const maxResponseBodyBytes = 4 * 1024 * 1024 // 4 MB

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
		"instance", inst.Name, "type", inst.Type, "appName", derefOrAbsent(status.AppName), "version", derefOrAbsent(status.Version))

	warnIfFieldAbsent(logger, inst, "system/status", "appName", status.AppName == nil)
	warnIfFieldAbsent(logger, inst, "system/status", "version", status.Version == nil)

	if expected, ok := expectedAppNameByType[inst.Type]; ok && status.AppName != nil && !strings.EqualFold(*status.AppName, expected) {
		logger.Warn("appName does not match configured instance type",
			"instance", inst.Name, "type", inst.Type, "appName", *status.AppName, "expectedAppName", expected)
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
			"name", derefOrAbsent(p.Name), "cutoff", derefOrAbsent(p.Cutoff),
			"cutoffFormatScore", derefOrAbsent(p.CutoffFormatScore), "upgradeAllowed", derefOrAbsent(p.UpgradeAllowed))

		warnIfFieldAbsent(logger, inst, "qualityprofile", "name", p.Name == nil)
		warnIfFieldAbsent(logger, inst, "qualityprofile", "cutoff", p.Cutoff == nil)
		warnIfFieldAbsent(logger, inst, "qualityprofile", "cutoffFormatScore", p.CutoffFormatScore == nil)
		warnIfFieldAbsent(logger, inst, "qualityprofile", "upgradeAllowed", p.UpgradeAllowed == nil)
	}
}

// fetchBody issues a GET against path via client and returns the full
// response body, capped at maxResponseBodyBytes. Any transport error,
// non-2xx status (client.Do's job), or body-read error is returned as-is
// for the caller to treat as this instance's cycle being skipped. A body
// that reaches the cap exactly is indistinguishable from one that was
// truncated by it, so it is also reported as an error per plan §2.6.
func fetchBody(ctx context.Context, client *APIClient, path string) ([]byte, error) {
	resp, err := client.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("reading response body from %s: %w", path, err)
	}
	if len(body) == maxResponseBodyBytes {
		return nil, fmt.Errorf("reading response body from %s: response reached the %d byte limit (possibly truncated)", path, maxResponseBodyBytes)
	}
	return body, nil
}

// derefOrAbsent returns *p, or the string "absent" if p is nil. Used to log
// a pointer-typed decoded field: a nil pointer means the JSON key was not
// present in the response at all (as opposed to present with a zero
// value), which is otherwise indistinguishable once logged as a bare zero.
func derefOrAbsent[T any](p *T) any {
	if p == nil {
		return "absent"
	}
	return *p
}

// warnIfFieldAbsent logs a warning naming field when absent is true. Per
// plan §5's warning not to trust field names blindly ("if a field named
// here doesn't exist, stop and report — do not improvise"): a JSON key
// that is entirely missing from the response — not merely present with a
// zero value, which is a legitimate, common real value for fields like
// upgradeAllowed or cutoffFormatScore — is the actual signal that our
// assumed field name may not match the real API. Surfaced for the human
// gate to confirm against the running instances. It is informational only:
// it never causes the instance to be skipped.
func warnIfFieldAbsent(logger *slog.Logger, inst Instance, endpoint, field string, absent bool) {
	if !absent {
		return
	}
	logger.Warn(fmt.Sprintf("field %q missing from response", field),
		"instance", inst.Name, "type", inst.Type, "endpoint", endpoint, "field", field)
}
