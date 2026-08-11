package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
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

func TestRun_Once_ChecksConnectivityForEveryInstanceAndSkipsFailuresWithoutStopping(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/system/status":
			w.Write([]byte(`{"appName": "Radarr", "version": "5.14.0.9383"}`))
		case "/api/v3/qualityprofile":
			w.Write([]byte(`[{"name": "HD-1080p", "upgradeAllowed": true, "cutoff": 7, "cutoffFormatScore": 10000}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer healthy.Close()

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer broken.Close()

	t.Setenv("MAIN_TEST_HEALTHY_KEY", "healthy-key")
	t.Setenv("MAIN_TEST_BROKEN_KEY", "broken-key")
	path := writeMainTestConfig(t, `
instances:
  - name: radarr-broken
    type: radarr
    url: `+broken.URL+`
    api_key: ${MAIN_TEST_BROKEN_KEY}
  - name: radarr-healthy
    type: radarr
    url: `+healthy.URL+`
    api_key: ${MAIN_TEST_HEALTHY_KEY}
`)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path, "--once"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 even with a failing instance; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "radarr-broken") {
		t.Errorf("expected a warning naming the broken instance:\n%s", out)
	}
	if !strings.Contains(out, "radarr-healthy") || !strings.Contains(out, "appName=Radarr") {
		t.Errorf("expected the healthy instance (after the broken one) to still be checked:\n%s", out)
	}
	if !strings.Contains(out, "name=HD-1080p") {
		t.Errorf("expected the healthy instance's quality profile to be logged:\n%s", out)
	}
}

func TestRun_WithoutOnce_DoesNotRunConnectivityChecks(t *testing.T) {
	// A URL with nothing listening: if run() attempted a connectivity
	// check here in daemon mode, it would still merely warn rather than
	// crash, but no connectivity-related log lines should appear at all
	// since this phase's connectivity check is scoped to --once.
	path := writeMainTestConfig(t, `
instances:
  - name: radarr-main
    type: radarr
    url: http://127.0.0.1:1
    api_key: key1
`)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "system status") || strings.Contains(out, "skipping instance") {
		t.Errorf("daemon mode (no --once) should not run connectivity checks:\n%s", out)
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
	out := stdout.String()
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("stdout does not contain a WARN level log for empty instances:\n%s", out)
	}

	// The redacted config printout is mandatory startup output and must be
	// the very first thing printed, ahead of any other log line including
	// this warning.
	configIdx := strings.Index(out, "configuration loaded")
	warnIdx := strings.Index(out, "no instances configured")
	if configIdx == -1 || warnIdx == -1 {
		t.Fatalf("expected both the config printout and the no-instances warning in stdout:\n%s", out)
	}
	if configIdx != 0 {
		t.Errorf("expected the redacted config printout to be the very first output, but it started at byte %d:\n%s", configIdx, out)
	}
	if configIdx > warnIdx {
		t.Errorf("expected the redacted config printout (at %d) to appear before the no-instances warning (at %d):\n%s", configIdx, warnIdx, out)
	}
}

// --- --only-id flag validation (Phase 4) ----------------------------------

// TestRun_OnlyIDZero_IsFatalFlagError and its sibling below pin the flag's
// validation: a movie id is always a positive integer, so 0 and negatives
// are user error rather than something to silently interpret. 0 matters
// especially: it is also the flag's absent-value sentinel, so accepting it
// would turn "--only-id 0" into a silent full-library run — the exact
// widening of scope this phase must never do by accident.
func TestRun_OnlyIDZero_IsFatalFlagError(t *testing.T) {
	path := writeMainTestConfig(t, `
instances: []
`)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path, "--once", "--only-id", "0"}, &stdout, &stderr)

	if code == 0 {
		t.Fatal("exit code = 0, want non-zero for --only-id 0")
	}
	if !strings.Contains(stderr.String(), "only-id") {
		t.Errorf("stderr does not explain the invalid flag:\n%s", stderr.String())
	}
}

func TestRun_OnlyIDNegative_IsFatalFlagError(t *testing.T) {
	path := writeMainTestConfig(t, `
instances: []
`)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path, "--once", "--only-id", "-3"}, &stdout, &stderr)

	if code == 0 {
		t.Fatal("exit code = 0, want non-zero for a negative --only-id")
	}
	if !strings.Contains(stderr.String(), "only-id") {
		t.Errorf("stderr does not explain the invalid flag:\n%s", stderr.String())
	}
}

func TestRun_OnlyIDNonInteger_IsFatalFlagError(t *testing.T) {
	path := writeMainTestConfig(t, `
instances: []
`)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path, "--once", "--only-id", "not-a-number"}, &stdout, &stderr)

	if code == 0 {
		t.Fatal("exit code = 0, want non-zero for a non-integer --only-id")
	}
	if stderr.String() == "" {
		t.Error("stderr is empty, want a flag parse error message")
	}
}

// TestRun_OnlyIDWithoutOnce_WarnsThatItHasNoEffect: the flag scopes a
// single pass, and daemon mode does not run one yet. Ignoring it silently
// would let someone believe a run was scoped when it was not.
func TestRun_OnlyIDWithoutOnce_WarnsThatItHasNoEffect(t *testing.T) {
	path := writeMainTestConfig(t, `
instances: []
`)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path, "--only-id", "42"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "only-id") {
		t.Errorf("expected a warning that --only-id has no effect without --once:\n%s", out)
	}
}
