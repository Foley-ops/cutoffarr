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

// TestRun_WithoutOnce_RunsAsADaemonAndExitsZeroOnShutdown replaces the Phase 0
// placeholder ("daemon mode is not implemented yet; it arrives in a later
// phase"), which this phase is. Without --once the process now starts the
// listener, performs a full startup scan, and stays up until it is asked to
// stop — exiting 0 on a clean shutdown.
func TestRun_WithoutOnce_RunsAsADaemonAndExitsZeroOnShutdown(t *testing.T) {
	path := writeMainTestConfig(t, `
instances: []
`)
	h := startDaemon(t, path)
	h.waitReady()

	if code := h.stop(); code != 0 {
		t.Fatalf("exit code = %d, want 0 for a clean shutdown:\n%s", code, h.out.String())
	}
	out := h.out.String()
	if strings.Contains(out, "later phase") {
		t.Errorf("the placeholder message must be gone; daemon mode is this phase:\n%s", out)
	}
	for _, want := range []string{"webhook listener started", "startup scan beginning", "startup scan complete", "shutdown complete"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected the daemon to announce %q:\n%s", want, out)
		}
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

// TestRun_WithoutOnce_StartupScanContactsEveryInstance is the exact inversion
// of the Phase 0 test it replaces, which asserted that daemon mode contacted
// NOTHING. Daemon mode now begins with a full scan, so an unreachable instance
// must produce the same warn-and-skip a --once run produces — and the daemon
// must survive it.
func TestRun_WithoutOnce_StartupScanContactsEveryInstance(t *testing.T) {
	path := writeMainTestConfig(t, `
instances:
  - name: radarr-main
    type: radarr
    url: http://127.0.0.1:1
    api_key: key1
`)
	h := startDaemon(t, path)
	h.waitReady()

	out := h.out.String()
	if !strings.Contains(out, "skipping instance") {
		t.Errorf("the startup scan must contact every configured instance and warn about the ones it cannot reach:\n%s", out)
	}
	if code := h.stop(); code != 0 {
		t.Fatalf("exit code = %d, want 0: one unreachable instance never fails the daemon:\n%s", code, h.out.String())
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

// TestRun_OnlyIDWithoutOnce_WarnsThatItHasNoEffect: the flag scopes a single
// pass, while daemon mode scopes its own passes (from webhooks) and reconciles
// the whole library on the poll interval. Ignoring the flag silently would let
// someone believe a daemon was scoped when it was not — and a daemon pinned to
// one item forever would reconcile nothing.
//
// The config names a radarr (it is never contacted — without --once nothing
// is) so this test exercises the one thing it is about. An empty instance
// list would now be refused earlier and for a different reason: a --only-id
// that no configured radarr could apply to is a fatal flag error in its own
// right, which is a separate pin (writer_test.go's --only-id scope tests).
func TestRun_OnlyIDWithoutOnce_WarnsThatItHasNoEffect(t *testing.T) {
	path := writeMainTestConfig(t, `
instances:
  - name: radarr-main
    type: radarr
    url: http://radarr.invalid:7878
    api_key: key1
`)
	h := startDaemonWithArgs(t, []string{"--config", path, "--only-id", "42"})
	h.waitReady()
	out := h.out.String()
	h.stop()

	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "only-id") {
		t.Errorf("expected a warning that --only-id has no effect without --once:\n%s", out)
	}
}

// TestRun_InstanceWithoutOnce_WarnsThatItHasNoEffect is the same courtesy
// for --instance: a scoping flag that is silently ignored is worse than one
// that is rejected, because the human believes the run was narrowed.
func TestRun_InstanceWithoutOnce_WarnsThatItHasNoEffect(t *testing.T) {
	path := writeMainTestConfig(t, `
instances:
  - name: radarr-main
    type: radarr
    url: http://radarr:7878
    api_key: key1
`)
	h := startDaemonWithArgs(t, []string{"--config", path, "--instance", "radarr-main"})
	h.waitReady()
	out := h.out.String()
	h.stop()

	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "instance") {
		t.Errorf("expected a warning that --instance has no effect without --once:\n%s", out)
	}
}

// TestRun_InstanceFlag_NamesASonarrInstance_ScopesToIt pins that --instance
// is a general instance selector, not a radarr-only one: it names which
// configured instance a pass runs against, whatever its type.
func TestRun_InstanceFlag_NamesASonarrInstance_ScopesToIt(t *testing.T) {
	var radarrHits, sonarrHits int
	radarrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		radarrHits++
		w.Write([]byte(`{"appName": "Radarr", "version": "5.14.0.9383"}`))
	}))
	defer radarrSrv.Close()
	sonarrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sonarrHits++
		w.Write([]byte(`{"appName": "Sonarr", "version": "4.0.0.0"}`))
	}))
	defer sonarrSrv.Close()

	path := writeMainTestConfig(t, `
instances:
  - name: radarr-main
    type: radarr
    url: `+radarrSrv.URL+`
    api_key: key1
  - name: sonarr-main
    type: sonarr
    url: `+sonarrSrv.URL+`
    api_key: key2
`)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path, "--once", "--instance", "sonarr-main"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if radarrHits != 0 {
		t.Errorf("the radarr instance was contacted %d time(s) despite --instance sonarr-main", radarrHits)
	}
	if sonarrHits == 0 {
		t.Error("the named sonarr instance was never contacted")
	}
}

// TestRun_OnlyIDSet_MixedConfig_IsAmbiguousAndRefusesBeforeContact replaces
// the Phase 6 F4 pin, which held only while --only-id had no Sonarr meaning:
// then, a mixed-config --only-id run skipped every sonarr entirely (with an
// INFO line) so a targeted run could not fan out into a full unscoped Sonarr
// report.
//
// Phase 7 gives --only-id a Sonarr meaning — a SERIES id — and that turns the
// same invocation into a genuine ambiguity rather than a scoping question:
// "--only-id 42" names movie 42 in the radarr AND series 42 in the sonarr,
// which are two unrelated things. Ids are per-instance, there is no safe way
// to guess which the human meant, and acting on both is the precise opposite
// of "a single item, explicitly named" — so the run refuses before contacting
// anything and says how to say it (binding controller resolution 4: when more
// than one instance of ANY type is in scope, --instance is required).
func TestRun_OnlyIDSet_MixedConfig_IsAmbiguousAndRefusesBeforeContact(t *testing.T) {
	var sonarrHits, radarrHits int
	sonarrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sonarrHits++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer sonarrSrv.Close()
	radarrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		radarrHits++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer radarrSrv.Close()

	path := writeMainTestConfig(t, `
instances:
  - name: radarr-main
    type: radarr
    url: `+radarrSrv.URL+`
    api_key: key1
  - name: sonarr-main
    type: sonarr
    url: `+sonarrSrv.URL+`
    api_key: key2
`)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path, "--once", "--only-id", "42"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2: --only-id across two instances is ambiguous\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	if sonarrHits != 0 || radarrHits != 0 {
		t.Errorf("the run contacted an instance before refusing: radarr=%d sonarr=%d", radarrHits, sonarrHits)
	}
	msg := stderr.String()
	for _, want := range []string{"--only-id", "42", "--instance", "radarr-main", "sonarr-main"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal must name %q so the human can act on it:\n%s", want, msg)
		}
	}
}
