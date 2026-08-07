package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeMainTestConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	return path
}

func TestRun_OnceModePrintsRedactedConfigAndExitsZero(t *testing.T) {
	t.Setenv("MAIN_TEST_API_KEY", "sekret-value-123")
	path := writeMainTestConfig(t, `
instances:
  - name: radarr-main
    type: radarr
    url: http://radarr:7878
    api_key: ${MAIN_TEST_API_KEY}
`)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path, "--once"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "sekret-value-123") {
		t.Errorf("stdout contains the raw API key, want it redacted:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "<redacted>") {
		t.Errorf("stdout does not contain the redaction marker:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "radarr-main") {
		t.Errorf("stdout does not mention the configured instance name:\n%s", stdout.String())
	}
	if stderr.String() != "" {
		t.Errorf("stderr = %q, want empty on success", stderr.String())
	}
}

func TestRun_InvalidConfigExitsNonZeroWithStderrMessage(t *testing.T) {
	path := writeMainTestConfig(t, `
instances:
  - name: radarr-main
    type: bogus
    url: http://radarr:7878
    api_key: key1
`)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path, "--once"}, &stdout, &stderr)

	if code == 0 {
		t.Fatal("exit code = 0, want non-zero for invalid config")
	}
	if stderr.String() == "" {
		t.Error("stderr is empty, want a fatal error message")
	}
}

func TestRun_DryRunFlagForcesTrueEvenIfConfigDisablesIt(t *testing.T) {
	path := writeMainTestConfig(t, `
dry_run: false
instances: []
`)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path, "--once", "--dry-run"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "dry_run=true") {
		t.Errorf("stdout does not show dry_run forced true:\n%s", stdout.String())
	}
}

func TestRun_WithoutDryRunFlagHonorsConfigFalse(t *testing.T) {
	path := writeMainTestConfig(t, `
dry_run: false
instances: []
`)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path, "--once"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "dry_run=false") {
		t.Errorf("stdout does not show dry_run=false:\n%s", stdout.String())
	}
}

func TestRun_WithoutOnceLogsDaemonModeMessageAndExitsZero(t *testing.T) {
	path := writeMainTestConfig(t, `
instances: []
`)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "later phase") {
		t.Errorf("stdout does not mention daemon mode arriving later:\n%s", stdout.String())
	}
}

func TestRun_RedactedConfigPrintedUnconditionally_LogLevelWarn(t *testing.T) {
	t.Setenv("MAIN_TEST_WARN_API_KEY", "sekret-warn-value")
	path := writeMainTestConfig(t, `
log_level: warn
instances:
  - name: radarr-main
    type: radarr
    url: http://radarr:7878
    api_key: ${MAIN_TEST_WARN_API_KEY}
`)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path, "--once"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "sekret-warn-value") {
		t.Errorf("stdout contains the raw API key, want it redacted:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "<redacted>") {
		t.Errorf("stdout does not contain the redaction marker even though log_level=warn (config printout must be unconditional):\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "radarr-main") {
		t.Errorf("stdout does not mention the configured instance name even though log_level=warn:\n%s", stdout.String())
	}
}

func TestRun_RedactedConfigPrintedUnconditionally_LogLevelError(t *testing.T) {
	t.Setenv("MAIN_TEST_ERROR_API_KEY", "sekret-error-value")
	path := writeMainTestConfig(t, `
log_level: error
instances:
  - name: sonarr-main
    type: sonarr
    url: http://sonarr:8989
    api_key: ${MAIN_TEST_ERROR_API_KEY}
`)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path, "--once"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "sekret-error-value") {
		t.Errorf("stdout contains the raw API key, want it redacted:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "<redacted>") {
		t.Errorf("stdout does not contain the redaction marker even though log_level=error (config printout must be unconditional):\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "sonarr-main") {
		t.Errorf("stdout does not mention the configured instance name even though log_level=error:\n%s", stdout.String())
	}
}

func TestRun_EmptyInstancesLogsWarnNotFatal(t *testing.T) {
	path := writeMainTestConfig(t, `
log_level: debug
instances: []
`)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path, "--once"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "level=WARN") {
		t.Errorf("stdout does not contain a WARN level log for empty instances:\n%s", stdout.String())
	}
}
