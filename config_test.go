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
