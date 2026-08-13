package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeConfig writes the given YAML content to a temp file and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	return path
}

func TestLoadConfig_DryRunDefaultsTrueWhenAbsent(t *testing.T) {
	path := writeConfig(t, `
instances: []
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if !cfg.DryRun {
		t.Errorf("DryRun = false, want true when dry_run is absent from config")
	}
}

func TestLoadConfig_DryRunHonorsExplicitFalse(t *testing.T) {
	path := writeConfig(t, `
dry_run: false
instances: []
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if cfg.DryRun {
		t.Errorf("DryRun = true, want false when dry_run: false is set explicitly")
	}
}

func TestLoadConfig_DryRunHonorsExplicitTrue(t *testing.T) {
	path := writeConfig(t, `
dry_run: true
instances: []
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if !cfg.DryRun {
		t.Errorf("DryRun = false, want true when dry_run: true is set explicitly")
	}
}

func TestLoadConfig_DefaultsAppliedWhenAbsent(t *testing.T) {
	path := writeConfig(t, `
instances: []
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if cfg.PollInterval != 24*time.Hour {
		t.Errorf("PollInterval = %v, want 24h", cfg.PollInterval)
	}
	if cfg.WebhookPort != 9898 {
		t.Errorf("WebhookPort = %d, want 9898", cfg.WebhookPort)
	}
	if cfg.WebhookDebounce != 45*time.Second {
		t.Errorf("WebhookDebounce = %v, want 45s", cfg.WebhookDebounce)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "info")
	}
	if cfg.ExclusionTag != "cutoffarr-exclude" {
		t.Errorf("ExclusionTag = %q, want %q", cfg.ExclusionTag, "cutoffarr-exclude")
	}
}

func TestLoadConfig_ExplicitValuesOverrideDefaults(t *testing.T) {
	path := writeConfig(t, `
poll_interval: 2h
webhook_port: 1234
webhook_debounce: 10s
log_level: debug
exclusion_tag: my-tag
instances: []
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if cfg.PollInterval != 2*time.Hour {
		t.Errorf("PollInterval = %v, want 2h", cfg.PollInterval)
	}
	if cfg.WebhookPort != 1234 {
		t.Errorf("WebhookPort = %d, want 1234", cfg.WebhookPort)
	}
	if cfg.WebhookDebounce != 10*time.Second {
		t.Errorf("WebhookDebounce = %v, want 10s", cfg.WebhookDebounce)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "debug")
	}
	if cfg.ExclusionTag != "my-tag" {
		t.Errorf("ExclusionTag = %q, want %q", cfg.ExclusionTag, "my-tag")
	}
}

func TestLoadConfig_ExpandsEnvVarsInValues(t *testing.T) {
	t.Setenv("TEST_RADARR_KEY", "super-secret-key")
	path := writeConfig(t, `
instances:
  - name: radarr-main
    type: radarr
    url: http://radarr:7878
    api_key: ${TEST_RADARR_KEY}
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if len(cfg.Instances) != 1 {
		t.Fatalf("len(Instances) = %d, want 1", len(cfg.Instances))
	}
	if cfg.Instances[0].APIKey != "super-secret-key" {
		t.Errorf("APIKey = %q, want %q", cfg.Instances[0].APIKey, "super-secret-key")
	}
}

func TestLoadConfig_UnsetEnvVarIsFatalAndNamesTheVar(t *testing.T) {
	os.Unsetenv("TEST_TOTALLY_UNSET_VAR")
	path := writeConfig(t, `
instances:
  - name: radarr-main
    type: radarr
    url: http://radarr:7878
    api_key: ${TEST_TOTALLY_UNSET_VAR}
`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig returned nil error, want error for unset env var")
	}
	if !strings.Contains(err.Error(), "TEST_TOTALLY_UNSET_VAR") {
		t.Errorf("error %q does not name the missing variable TEST_TOTALLY_UNSET_VAR", err.Error())
	}
}

func TestLoadConfig_UnsetEnvVarNamedOnceEvenIfReferencedTwice(t *testing.T) {
	os.Unsetenv("TEST_DUPLICATE_UNSET_VAR")
	path := writeConfig(t, `
instances:
  - name: radarr-main
    type: radarr
    url: http://radarr:7878
    api_key: ${TEST_DUPLICATE_UNSET_VAR}
  - name: sonarr-main
    type: sonarr
    url: http://sonarr:8989
    api_key: ${TEST_DUPLICATE_UNSET_VAR}
`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig returned nil error, want error for unset env var")
	}
	count := strings.Count(err.Error(), "TEST_DUPLICATE_UNSET_VAR")
	if count != 1 {
		t.Errorf("error mentions TEST_DUPLICATE_UNSET_VAR %d times, want exactly 1 (deduplicated): %q", count, err.Error())
	}
}

func TestLoadConfig_DuplicateInstanceNamesFatal(t *testing.T) {
	path := writeConfig(t, `
instances:
  - name: radarr-main
    type: radarr
    url: http://radarr:7878
    api_key: key1
  - name: radarr-main
    type: radarr
    url: http://radarr2:7878
    api_key: key2
`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig returned nil error, want error for duplicate instance names")
	}
	if !strings.Contains(err.Error(), "radarr-main") {
		t.Errorf("error %q does not mention the duplicated name", err.Error())
	}
}

func TestLoadConfig_UnknownInstanceTypeFatal(t *testing.T) {
	path := writeConfig(t, `
instances:
  - name: mystery
    type: plexarr
    url: http://mystery:1234
    api_key: key1
`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig returned nil error, want error for unknown instance type")
	}
	if !strings.Contains(err.Error(), "plexarr") {
		t.Errorf("error %q does not mention the unknown type", err.Error())
	}
}

func TestLoadConfig_MissingInstanceFieldsFatal(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"missing name", "instances:\n  - type: radarr\n    url: http://radarr:7878\n    api_key: key1\n"},
		{"missing type", "instances:\n  - name: radarr-main\n    url: http://radarr:7878\n    api_key: key1\n"},
		{"missing url", "instances:\n  - name: radarr-main\n    type: radarr\n    api_key: key1\n"},
		{"missing api_key", "instances:\n  - name: radarr-main\n    type: radarr\n    url: http://radarr:7878\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.yaml)
			_, err := LoadConfig(path)
			if err == nil {
				t.Fatalf("LoadConfig returned nil error, want error for %s", tc.name)
			}
		})
	}
}

func TestLoadConfig_InvalidURLFatal(t *testing.T) {
	cases := []string{
		"not-a-url",
		"ftp://radarr:7878",
		"radarr:7878",
	}
	for _, u := range cases {
		t.Run(u, func(t *testing.T) {
			path := writeConfig(t, `
instances:
  - name: radarr-main
    type: radarr
    url: `+u+`
    api_key: key1
`)
			_, err := LoadConfig(path)
			if err == nil {
				t.Fatalf("LoadConfig returned nil error, want error for invalid url %q", u)
			}
		})
	}
}

func TestLoadConfig_UnknownLogLevelFatal(t *testing.T) {
	path := writeConfig(t, `
log_level: verbose
instances: []
`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig returned nil error, want error for unknown log_level")
	}
	if !strings.Contains(err.Error(), "verbose") {
		t.Errorf("error %q does not mention the unknown log_level", err.Error())
	}
}

// TestLoadConfig_EmptyExclusionTagFatal is FIX 8 (controller-mandated
// correction after the initial Phase 3 review): exclusion_tag is now
// load-bearing (Phase 3's decision engine rule 4 resolves it to a tag id),
// and an explicit empty string silently disables that rule for every movie
// without any indication that happened — an empty label matches no real
// Radarr tag, so resolveExclusionTagID's "exclusion tag not defined"
// outcome would be indistinguishable from a deliberately-omitted config
// key. An absent exclusion_tag key still defaults to "cutoffarr-exclude"
// (toConfig's default only applies when the *string pointer itself is nil,
// i.e. the key was never in the YAML at all), so this check unambiguously
// means "explicitly set to empty in the file".
func TestLoadConfig_EmptyExclusionTagFatal(t *testing.T) {
	path := writeConfig(t, `
exclusion_tag: ""
instances: []
`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig returned nil error, want error for an empty exclusion_tag")
	}
	if !strings.Contains(err.Error(), "exclusion_tag") {
		t.Errorf("error %q does not mention exclusion_tag", err.Error())
	}
}

func TestLoadConfig_AbsentExclusionTagStillDefaultsAndIsValid(t *testing.T) {
	path := writeConfig(t, `
instances: []
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if cfg.ExclusionTag != "cutoffarr-exclude" {
		t.Errorf("ExclusionTag = %q, want the default %q when the key is absent entirely", cfg.ExclusionTag, "cutoffarr-exclude")
	}
}

// TestLoadConfig_WhitespaceOnlyExclusionTagFatal is FIX 3 (controller-
// mandated correction after the whole-branch review): FIX 8's emptiness
// check compared cfg.ExclusionTag directly against "", so a whitespace-
// only value like "  " passed it untouched — reproducing exactly the
// silent-disable FIX 8 was meant to close, since a value that is all
// whitespace can no more match a real Radarr tag label than an empty
// string can. Trimming before the check catches this.
func TestLoadConfig_WhitespaceOnlyExclusionTagFatal(t *testing.T) {
	path := writeConfig(t, `
exclusion_tag: "  "
instances: []
`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig returned nil error, want error for a whitespace-only exclusion_tag")
	}
	if !strings.Contains(err.Error(), "exclusion_tag") {
		t.Errorf("error %q does not mention exclusion_tag", err.Error())
	}
}

// TestLoadConfig_ExclusionTagWithSurroundingWhitespaceIsTrimmed pins the
// other half of FIX 3: a legitimate label with accidental surrounding
// whitespace is trimmed and stored trimmed, rather than being passed
// through verbatim to the decision engine's case-insensitive tag-label
// match (resolveExclusionTagID uses strings.EqualFold, which does not
// itself strip whitespace) where the padding would silently prevent it
// from ever matching the real Radarr tag's label.
func TestLoadConfig_ExclusionTagWithSurroundingWhitespaceIsTrimmed(t *testing.T) {
	path := writeConfig(t, `
exclusion_tag: "  my-tag  "
instances: []
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if cfg.ExclusionTag != "my-tag" {
		t.Errorf("ExclusionTag = %q, want the trimmed value %q", cfg.ExclusionTag, "my-tag")
	}
}

func TestLoadConfig_PollIntervalZeroDisablesSweepAndIsValid(t *testing.T) {
	path := writeConfig(t, `
poll_interval: 0
instances: []
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if cfg.PollInterval != 0 {
		t.Errorf("PollInterval = %v, want 0", cfg.PollInterval)
	}
}

func TestLoadConfig_PollIntervalAtLeastOneHourIsValid(t *testing.T) {
	for _, v := range []string{"1h", "25h"} {
		t.Run(v, func(t *testing.T) {
			path := writeConfig(t, "poll_interval: "+v+"\ninstances: []\n")
			_, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("LoadConfig returned error for poll_interval %s: %v", v, err)
			}
		})
	}
}

func TestLoadConfig_PollIntervalBetweenZeroAndOneHourFatal(t *testing.T) {
	for _, v := range []string{"30m", "59m", "1s"} {
		t.Run(v, func(t *testing.T) {
			path := writeConfig(t, "poll_interval: "+v+"\ninstances: []\n")
			_, err := LoadConfig(path)
			if err == nil {
				t.Fatalf("LoadConfig returned nil error, want error for poll_interval %s (below 1h minimum)", v)
			}
		})
	}
}

func TestLoadConfig_EmptyInstancesListIsNotFatal(t *testing.T) {
	path := writeConfig(t, `
instances: []
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if len(cfg.Instances) != 0 {
		t.Errorf("len(Instances) = %d, want 0", len(cfg.Instances))
	}
}

func TestLoadConfig_MissingInstancesKeyIsNotFatal(t *testing.T) {
	path := writeConfig(t, `
log_level: info
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if len(cfg.Instances) != 0 {
		t.Errorf("len(Instances) = %d, want 0", len(cfg.Instances))
	}
}

func TestLoadConfig_FullExample(t *testing.T) {
	t.Setenv("RADARR_MAIN_API_KEY", "radarr-key")
	t.Setenv("SONARR_MAIN_API_KEY", "sonarr-key")
	path := writeConfig(t, `
dry_run: true
poll_interval: 24h
webhook_port: 9898
webhook_debounce: 45s
log_level: info
exclusion_tag: cutoffarr-exclude

instances:
  - name: radarr-main
    type: radarr
    url: http://radarr:7878
    api_key: ${RADARR_MAIN_API_KEY}
  - name: sonarr-main
    type: sonarr
    url: http://sonarr:8989
    api_key: ${SONARR_MAIN_API_KEY}
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if len(cfg.Instances) != 2 {
		t.Fatalf("len(Instances) = %d, want 2", len(cfg.Instances))
	}
	if cfg.Instances[0].APIKey != "radarr-key" || cfg.Instances[1].APIKey != "sonarr-key" {
		t.Errorf("api keys not expanded correctly: %+v", cfg.Instances)
	}
}

func TestLoadConfig_MissingFileIsFatal(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "does-not-exist.yml"))
	if err == nil {
		t.Fatal("LoadConfig returned nil error, want error for missing file")
	}
}

func TestLoadConfig_UnknownTopLevelKeyFatal(t *testing.T) {
	path := writeConfig(t, `
dryrun: false
instances: []
`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig returned nil error, want error for unknown top-level key")
	}
	if !strings.Contains(err.Error(), "dryrun") {
		t.Errorf("error %q does not name the unknown key %q", err.Error(), "dryrun")
	}
}

func TestLoadConfig_UnknownInstanceKeyFatal(t *testing.T) {
	path := writeConfig(t, `
instances:
  - name: radarr-main
    type: radarr
    url: http://radarr:7878
    apikey: key1
`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig returned nil error, want error for unknown instance key")
	}
	if !strings.Contains(err.Error(), "apikey") {
		t.Errorf("error %q does not name the unknown key %q", err.Error(), "apikey")
	}
}

func TestLoadConfig_WebhookPortZeroFatal(t *testing.T) {
	path := writeConfig(t, `
webhook_port: 0
instances: []
`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig returned nil error, want error for webhook_port 0")
	}
	if !strings.Contains(err.Error(), "webhook_port") {
		t.Errorf("error %q does not mention webhook_port", err.Error())
	}
}

func TestLoadConfig_WebhookPortOutOfRangeFatal(t *testing.T) {
	for _, v := range []string{"-1", "65536", "100000"} {
		t.Run(v, func(t *testing.T) {
			path := writeConfig(t, "webhook_port: "+v+"\ninstances: []\n")
			_, err := LoadConfig(path)
			if err == nil {
				t.Fatalf("LoadConfig returned nil error, want error for webhook_port %s", v)
			}
			if !strings.Contains(err.Error(), "webhook_port") {
				t.Errorf("error %q does not mention webhook_port", err.Error())
			}
		})
	}
}

func TestLoadConfig_WebhookPortBoundsAreValid(t *testing.T) {
	for _, v := range []string{"1", "65535"} {
		t.Run(v, func(t *testing.T) {
			path := writeConfig(t, "webhook_port: "+v+"\ninstances: []\n")
			_, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("LoadConfig returned error for webhook_port %s: %v", v, err)
			}
		})
	}
}

func TestConfig_RedactedHidesAPIKeys(t *testing.T) {
	cfg := Config{
		Instances: []Instance{
			{Name: "radarr-main", Type: "radarr", URL: "http://radarr:7878", APIKey: "top-secret"},
		},
	}
	redacted := cfg.Redacted()
	if redacted.Instances[0].APIKey != "<redacted>" {
		t.Errorf("Redacted APIKey = %q, want %q", redacted.Instances[0].APIKey, "<redacted>")
	}
	// Original must be untouched.
	if cfg.Instances[0].APIKey != "top-secret" {
		t.Errorf("Redacted() mutated the original config's APIKey")
	}
}

// TestLoadConfig_NegativeWebhookDebounce_IsFatal closes the last unvalidated
// duration in the config. Phase 8 made webhook_debounce load-bearing: it is the
// only thing bounding how many full instance scans a burst of webhooks costs,
// and a negative value would put every key's deadline in the past — turning a
// 24-episode season pack into 24 full library evaluations, which is precisely
// the behavior the debounce exists to prevent. Zero is still allowed, and means
// "evaluate as soon as the loop sees it".
func TestLoadConfig_NegativeWebhookDebounce_IsFatal(t *testing.T) {
	path := writeConfig(t, `
webhook_debounce: -5s
instances: []
`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected a negative webhook_debounce to be fatal")
	}
	if !strings.Contains(err.Error(), "webhook_debounce") {
		t.Errorf("the error must name the field: %v", err)
	}
}

func TestLoadConfig_ZeroWebhookDebounce_IsAllowed(t *testing.T) {
	path := writeConfig(t, `
webhook_debounce: 0s
instances: []
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("zero is a legitimate choice (no debounce), got %v", err)
	}
	if cfg.WebhookDebounce != 0 {
		t.Errorf("WebhookDebounce = %v, want 0", cfg.WebhookDebounce)
	}
}

// --- reverse_scan_remonitor (Phase 10) -------------------------------------
//
// The reverse scan is report-only by default and its writes live behind this
// one flag, so "absent means false" is the safety property the whole phase
// rests on: a config written before Phase 10 existed, or one that simply never
// mentions the key, must never re-monitor anything. It is a *bool in rawConfig
// for exactly the reason dry_run is — "absent" has to be tellable from
// "explicitly set" — even though this flag's default happens to coincide with
// Go's zero value, because a future default change must not silently depend on
// that coincidence.

func TestLoadConfig_ReverseScanRemonitorDefaultsFalseWhenAbsent(t *testing.T) {
	path := writeConfig(t, `
instances: []
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if cfg.ReverseScanRemonitor {
		t.Errorf("ReverseScanRemonitor = true, want false when reverse_scan_remonitor is absent from config")
	}
}

func TestLoadConfig_ReverseScanRemonitorHonorsExplicitTrue(t *testing.T) {
	path := writeConfig(t, `
reverse_scan_remonitor: true
instances: []
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if !cfg.ReverseScanRemonitor {
		t.Errorf("ReverseScanRemonitor = false, want true when reverse_scan_remonitor: true is set explicitly")
	}
}

func TestLoadConfig_ReverseScanRemonitorHonorsExplicitFalse(t *testing.T) {
	path := writeConfig(t, `
reverse_scan_remonitor: false
instances: []
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if cfg.ReverseScanRemonitor {
		t.Errorf("ReverseScanRemonitor = true, want false when reverse_scan_remonitor: false is set explicitly")
	}
}

// TestLoadConfig_ReverseScanRemonitorNonBoolean_IsFatal is the
// present-but-invalid rule this project applies to every other typed field: a
// value yaml cannot read as a bool must stop the process, never fall back to
// the safe-looking default. "reverse_scan_remonitor: yes please" silently
// meaning false would be a configuration the human believes is ON while the
// program behaves as though it were off — and unlike most misconfigurations,
// that one is invisible precisely because its symptom is that nothing happens.
//
// The error is yaml.v3's own, surfaced verbatim under this project's "config:
// parsing yaml" prefix, exactly as a non-boolean dry_run or a non-integer
// webhook_port already is: it locates the value by LINE and states the type it
// could not be read as, rather than naming the key. This test pins fatality and
// that locating information, not a wording this package does not own.
func TestLoadConfig_ReverseScanRemonitorNonBoolean_IsFatal(t *testing.T) {
	path := writeConfig(t, `
reverse_scan_remonitor: "yes please"
instances: []
`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig returned nil error, want a fatal error for a non-boolean reverse_scan_remonitor")
	}
	for _, want := range []string{"parsing yaml", "line 2", "bool"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must locate the offending value (%q missing): %v", want, err)
		}
	}
}

// --- media_root_map (Phase 11) ----------------------------------------------
//
// Binding controller resolution 1: media_root_map absent means the file report
// is OFF for that instance — the feature is opt-in per instance, so an existing
// config written before Phase 11 existed must keep behaving exactly as it did
// (no disk access of any kind). Present-but-invalid (non-map, empty key/value,
// relative paths) is fatal at startup, the same "never guess, refuse instead"
// posture every other typed config field in this file already has.

func TestLoadConfig_MediaRootMapAbsentLeavesInstanceMapNil(t *testing.T) {
	path := writeConfig(t, `
instances:
  - name: radarr-main
    type: radarr
    url: http://radarr:7878
    api_key: key1
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if len(cfg.Instances[0].MediaRootMap) != 0 {
		t.Errorf("MediaRootMap = %v, want empty/nil when media_root_map is absent", cfg.Instances[0].MediaRootMap)
	}
}

func TestLoadConfig_MediaRootMapHonorsExplicitMapping(t *testing.T) {
	path := writeConfig(t, `
instances:
  - name: radarr-main
    type: radarr
    url: http://radarr:7878
    api_key: key1
    media_root_map:
      /movies: /data/media/Movies
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	got := cfg.Instances[0].MediaRootMap
	if got["/movies"] != "/data/media/Movies" {
		t.Errorf("MediaRootMap = %v, want {/movies: /data/media/Movies}", got)
	}
}

func TestLoadConfig_MediaRootMapEmptyMapIsValidAndTreatedAsAbsent(t *testing.T) {
	// An explicit empty map has nothing to report on, so it is accepted rather
	// than treated as a malformed value: functionally identical to omitting the
	// key altogether (see filereport.go's off-vs-configured distinction).
	path := writeConfig(t, `
instances:
  - name: radarr-main
    type: radarr
    url: http://radarr:7878
    api_key: key1
    media_root_map: {}
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if len(cfg.Instances[0].MediaRootMap) != 0 {
		t.Errorf("MediaRootMap = %v, want empty", cfg.Instances[0].MediaRootMap)
	}
}

func TestLoadConfig_MediaRootMapMultipleRoots(t *testing.T) {
	path := writeConfig(t, `
instances:
  - name: sonarr-main
    type: sonarr
    url: http://sonarr:8989
    api_key: key1
    media_root_map:
      /tv_shows: /data/media/TV_Shows
      /anime: /data/media/Anime
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	got := cfg.Instances[0].MediaRootMap
	if got["/tv_shows"] != "/data/media/TV_Shows" || got["/anime"] != "/data/media/Anime" {
		t.Errorf("MediaRootMap = %v, want both roots", got)
	}
}

// TestLoadConfig_MediaRootMapNonMapIsFatal is the "non-map" half of resolution
// 1's present-but-invalid rule. It needs no bespoke validation code at all:
// rawInstance.MediaRootMap is typed map[string]string, so yaml.v3 already
// refuses a scalar value there the same way it refuses a non-boolean
// reverse_scan_remonitor above — this test pins that the existing decode
// behavior really does cover this case rather than silently ignoring it.
func TestLoadConfig_MediaRootMapNonMapIsFatal(t *testing.T) {
	path := writeConfig(t, `
instances:
  - name: radarr-main
    type: radarr
    url: http://radarr:7878
    api_key: key1
    media_root_map: "/movies"
`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig returned nil error, want a fatal error for a non-map media_root_map")
	}
	if !strings.Contains(err.Error(), "parsing yaml") {
		t.Errorf("error %q should be surfaced as a yaml parse failure", err.Error())
	}
}

func TestLoadConfig_MediaRootMapEmptyKeyIsFatal(t *testing.T) {
	path := writeConfig(t, `
instances:
  - name: radarr-main
    type: radarr
    url: http://radarr:7878
    api_key: key1
    media_root_map:
      "": /data/media/Movies
`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig returned nil error, want a fatal error for an empty media_root_map key")
	}
	if !strings.Contains(err.Error(), "radarr-main") {
		t.Errorf("error %q does not name the offending instance", err.Error())
	}
}

func TestLoadConfig_MediaRootMapEmptyValueIsFatal(t *testing.T) {
	path := writeConfig(t, `
instances:
  - name: radarr-main
    type: radarr
    url: http://radarr:7878
    api_key: key1
    media_root_map:
      /movies: ""
`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig returned nil error, want a fatal error for an empty media_root_map value")
	}
	if !strings.Contains(err.Error(), "radarr-main") {
		t.Errorf("error %q does not name the offending instance", err.Error())
	}
}

func TestLoadConfig_MediaRootMapRelativeArrPathIsFatal(t *testing.T) {
	path := writeConfig(t, `
instances:
  - name: radarr-main
    type: radarr
    url: http://radarr:7878
    api_key: key1
    media_root_map:
      movies: /data/media/Movies
`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig returned nil error, want a fatal error for a relative media_root_map key")
	}
	if !strings.Contains(err.Error(), "movies") {
		t.Errorf("error %q does not name the offending relative path", err.Error())
	}
}

func TestLoadConfig_MediaRootMapRelativeDiskPathIsFatal(t *testing.T) {
	path := writeConfig(t, `
instances:
  - name: radarr-main
    type: radarr
    url: http://radarr:7878
    api_key: key1
    media_root_map:
      /movies: data/media/Movies
`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig returned nil error, want a fatal error for a relative media_root_map value")
	}
	if !strings.Contains(err.Error(), "data/media/Movies") {
		t.Errorf("error %q does not name the offending relative path", err.Error())
	}
}

// --- gui_actions (v2.2) -----------------------------------------------------
//
// The GUI action system's master switch, and the reason the owner's ruling
// ("forbidden for the AUTOMATION to do, not for the human") can be executable
// without weakening the permanent no-file-writes rule one inch: with this
// false — which is what an absent key, and therefore every config written
// before v2.2 existed, means — the action endpoints refuse and the buttons
// render disabled. It is a *bool in rawConfig for exactly the reason dry_run
// and reverse_scan_remonitor are: "absent" must be tellable from "explicitly
// set", even where the default coincides with Go's zero value.

func TestLoadConfig_GUIActionsDefaultsFalseWhenAbsent(t *testing.T) {
	path := writeConfig(t, `
instances: []
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if cfg.GUIActions {
		t.Errorf("GUIActions = true, want false when gui_actions is absent from config")
	}
}

func TestLoadConfig_GUIActionsHonorsExplicitTrue(t *testing.T) {
	path := writeConfig(t, `
gui_actions: true
instances: []
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if !cfg.GUIActions {
		t.Errorf("GUIActions = false, want true when gui_actions: true is set explicitly")
	}
}

func TestLoadConfig_GUIActionsHonorsExplicitFalse(t *testing.T) {
	path := writeConfig(t, `
gui_actions: false
instances: []
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if cfg.GUIActions {
		t.Errorf("GUIActions = true, want false when gui_actions: false is set explicitly")
	}
}

// TestLoadConfig_GUIActionsNonBoolean_IsFatal is the present-but-invalid rule
// every other typed field in this config already obeys, applied to the one
// switch that authorizes cutoffarr to touch a file at all: "gui_actions: yes
// please" silently meaning false would be a human believing the buttons are
// live while the program refuses every click — and, far worse in the other
// direction, a future typo'd truthy string must never be read as ON.
func TestLoadConfig_GUIActionsNonBoolean_IsFatal(t *testing.T) {
	path := writeConfig(t, `
gui_actions: "yes please"
instances: []
`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig returned nil error, want a fatal error for a non-boolean gui_actions")
	}
	for _, want := range []string{"parsing yaml", "line 2", "bool"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must locate the offending value (%q missing): %v", want, err)
		}
	}
}
