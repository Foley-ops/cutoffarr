package main

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newConnectivityTestLogger builds a slog.Logger backed by a buffer, at the
// given level, matching the TextHandler setup main.go uses in production.
func newConnectivityTestLogger(level slog.Level) (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level}))
	return logger, &buf
}

const radarrStatusJSON = `{
	"appName": "Radarr",
	"instanceName": "Radarr",
	"version": "5.14.0.9383",
	"buildTime": "2025-01-01T00:00:00Z",
	"isProduction": true,
	"isDocker": true,
	"urlBase": ""
}`

const radarrProfilesJSON = `[
	{"id": 1, "name": "HD-1080p", "upgradeAllowed": true, "cutoff": 7, "cutoffFormatScore": 10000, "items": []},
	{"id": 2, "name": "Ultra-HD", "upgradeAllowed": true, "cutoff": 19, "cutoffFormatScore": 5000, "items": []}
]`

const sonarrStatusJSON = `{
	"appName": "Sonarr",
	"instanceName": "Sonarr",
	"version": "4.0.9.2244",
	"buildTime": "2025-01-01T00:00:00Z",
	"isProduction": true,
	"isDocker": true,
	"urlBase": ""
}`

const sonarrProfilesJSON = `[
	{"id": 1, "name": "HDTV-720p", "upgradeAllowed": true, "cutoff": 4, "cutoffFormatScore": 2500, "items": []},
	{"id": 2, "name": "WEB-1080p", "upgradeAllowed": true, "cutoff": 9, "cutoffFormatScore": 8000, "items": []}
]`

// recordingHandler returns an http.Handler that serves statusJSON at
// /api/v3/system/status and profilesJSON at /api/v3/qualityprofile (each
// with the given status code), recording every request path and its
// X-Api-Key header value into gotPaths/gotAPIKeys.
func recordingHandler(t *testing.T, statusCode int, statusJSON string, profilesCode int, profilesJSON string, gotPaths *[]string, gotAPIKeys *[]string) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/system/status", func(w http.ResponseWriter, r *http.Request) {
		*gotPaths = append(*gotPaths, r.URL.Path)
		*gotAPIKeys = append(*gotAPIKeys, r.Header.Get("X-Api-Key"))
		w.WriteHeader(statusCode)
		w.Write([]byte(statusJSON))
	})
	mux.HandleFunc("/api/v3/qualityprofile", func(w http.ResponseWriter, r *http.Request) {
		*gotPaths = append(*gotPaths, r.URL.Path)
		*gotAPIKeys = append(*gotAPIKeys, r.Header.Get("X-Api-Key"))
		w.WriteHeader(profilesCode)
		w.Write([]byte(profilesJSON))
	})
	return mux
}

func TestCheckInstanceConnectivity_RadarrHappyPath(t *testing.T) {
	var gotPaths, gotAPIKeys []string
	srv := httptest.NewServer(recordingHandler(t, http.StatusOK, radarrStatusJSON, http.StatusOK, radarrProfilesJSON, &gotPaths, &gotAPIKeys))
	defer srv.Close()

	logger, buf := newConnectivityTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-main", Type: "radarr", URL: srv.URL, APIKey: "radarr-key"}

	checkInstanceConnectivity(context.Background(), logger, inst)

	if len(gotPaths) != 2 {
		t.Fatalf("got %d requests, want 2: %v", len(gotPaths), gotPaths)
	}
	if gotPaths[0] != "/api/v3/system/status" {
		t.Errorf("first request path = %q, want /api/v3/system/status", gotPaths[0])
	}
	if gotPaths[1] != "/api/v3/qualityprofile" {
		t.Errorf("second request path = %q, want /api/v3/qualityprofile", gotPaths[1])
	}
	for _, key := range gotAPIKeys {
		if key != "radarr-key" {
			t.Errorf("X-Api-Key header = %q, want %q", key, "radarr-key")
		}
	}

	out := buf.String()
	for _, want := range []string{
		"radarr-main", "appName=Radarr", "version=5.14.0.9383",
		"name=HD-1080p", "cutoff=7", "cutoffFormatScore=10000", "upgradeAllowed=true",
		"name=Ultra-HD", "cutoff=19", "cutoffFormatScore=5000",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log output does not contain %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "level=WARN") {
		t.Errorf("happy path should not log any warnings:\n%s", out)
	}
}

func TestCheckInstanceConnectivity_SonarrHappyPath(t *testing.T) {
	var gotPaths, gotAPIKeys []string
	srv := httptest.NewServer(recordingHandler(t, http.StatusOK, sonarrStatusJSON, http.StatusOK, sonarrProfilesJSON, &gotPaths, &gotAPIKeys))
	defer srv.Close()

	logger, buf := newConnectivityTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "sonarr-key"}

	checkInstanceConnectivity(context.Background(), logger, inst)

	if len(gotPaths) != 2 || gotPaths[0] != "/api/v3/system/status" || gotPaths[1] != "/api/v3/qualityprofile" {
		t.Fatalf("unexpected request paths: %v", gotPaths)
	}
	for _, key := range gotAPIKeys {
		if key != "sonarr-key" {
			t.Errorf("X-Api-Key header = %q, want %q", key, "sonarr-key")
		}
	}

	out := buf.String()
	for _, want := range []string{
		"sonarr-main", "appName=Sonarr", "version=4.0.9.2244",
		"name=HDTV-720p", "cutoff=4", "cutoffFormatScore=2500", "upgradeAllowed=true",
		"name=WEB-1080p", "cutoff=9", "cutoffFormatScore=8000",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log output does not contain %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "level=WARN") {
		t.Errorf("happy path should not log any warnings:\n%s", out)
	}
}

func TestCheckInstanceConnectivity_SystemStatusNonTwoxx_SkipsWithWarning(t *testing.T) {
	var gotPaths, gotAPIKeys []string
	srv := httptest.NewServer(recordingHandler(t, http.StatusInternalServerError, radarrStatusJSON, http.StatusOK, radarrProfilesJSON, &gotPaths, &gotAPIKeys))
	defer srv.Close()

	logger, buf := newConnectivityTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-broken", Type: "radarr", URL: srv.URL, APIKey: "key"}

	checkInstanceConnectivity(context.Background(), logger, inst)

	if len(gotPaths) != 1 {
		t.Fatalf("expected qualityprofile to never be called after system/status failure, got requests: %v", gotPaths)
	}

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected a warning to be logged:\n%s", out)
	}
	if !strings.Contains(out, "radarr-broken") {
		t.Errorf("warning does not mention instance name:\n%s", out)
	}
	if strings.Contains(out, "level=INFO") {
		t.Errorf("no info should be logged when system/status fails:\n%s", out)
	}
}

func TestCheckInstanceConnectivity_QualityProfileNonTwoxx_SkipsWithWarning(t *testing.T) {
	var gotPaths, gotAPIKeys []string
	srv := httptest.NewServer(recordingHandler(t, http.StatusOK, radarrStatusJSON, http.StatusServiceUnavailable, radarrProfilesJSON, &gotPaths, &gotAPIKeys))
	defer srv.Close()

	logger, buf := newConnectivityTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-half-broken", Type: "radarr", URL: srv.URL, APIKey: "key"}

	checkInstanceConnectivity(context.Background(), logger, inst)

	if len(gotPaths) != 2 {
		t.Fatalf("expected both endpoints to be called, got: %v", gotPaths)
	}

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected a warning to be logged:\n%s", out)
	}
	if !strings.Contains(out, "appName=Radarr") {
		t.Errorf("system/status succeeded so its info line should still appear:\n%s", out)
	}
	if strings.Contains(out, "name=HD-1080p") {
		t.Errorf("quality profile info should not appear when that call failed:\n%s", out)
	}
}

func TestCheckInstanceConnectivity_MalformedSystemStatusJSON_SkipsWithWarning(t *testing.T) {
	var gotPaths, gotAPIKeys []string
	srv := httptest.NewServer(recordingHandler(t, http.StatusOK, "not json", http.StatusOK, radarrProfilesJSON, &gotPaths, &gotAPIKeys))
	defer srv.Close()

	logger, buf := newConnectivityTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-malformed", Type: "radarr", URL: srv.URL, APIKey: "key"}

	checkInstanceConnectivity(context.Background(), logger, inst)

	if len(gotPaths) != 1 {
		t.Fatalf("expected qualityprofile to never be called after malformed system/status, got: %v", gotPaths)
	}

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected a warning to be logged for malformed JSON:\n%s", out)
	}
	if !strings.Contains(out, "radarr-malformed") {
		t.Errorf("warning does not mention instance name:\n%s", out)
	}
}

func TestCheckInstanceConnectivity_MalformedQualityProfileJSON_SkipsWithWarning(t *testing.T) {
	var gotPaths, gotAPIKeys []string
	srv := httptest.NewServer(recordingHandler(t, http.StatusOK, radarrStatusJSON, http.StatusOK, "{not valid array", &gotPaths, &gotAPIKeys))
	defer srv.Close()

	logger, buf := newConnectivityTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-malformed-profiles", Type: "radarr", URL: srv.URL, APIKey: "key"}

	checkInstanceConnectivity(context.Background(), logger, inst)

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected a warning to be logged for malformed JSON:\n%s", out)
	}
	if !strings.Contains(out, "radarr-malformed-profiles") {
		t.Errorf("warning does not mention instance name:\n%s", out)
	}
}

func TestCheckInstanceConnectivity_UnreachableServer_SkipsWithWarning(t *testing.T) {
	// Bind an ephemeral port and immediately close it, so nothing is
	// listening: the client's dial will fail with connection refused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("closing listener: %v", err)
	}

	logger, buf := newConnectivityTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-unreachable", Type: "radarr", URL: "http://" + addr, APIKey: "key"}

	done := make(chan struct{})
	go func() {
		checkInstanceConnectivity(context.Background(), logger, inst)
		close(done)
	}()
	<-done // must not hang or crash

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected a warning to be logged for unreachable server:\n%s", out)
	}
	if !strings.Contains(out, "radarr-unreachable") {
		t.Errorf("warning does not mention instance name:\n%s", out)
	}
}

func TestCheckInstanceConnectivity_ZeroValueField_LogsWarnNamingField(t *testing.T) {
	statusJSON := `{"appName": "", "version": "5.14.0.9383"}`
	var gotPaths, gotAPIKeys []string
	srv := httptest.NewServer(recordingHandler(t, http.StatusOK, statusJSON, http.StatusOK, radarrProfilesJSON, &gotPaths, &gotAPIKeys))
	defer srv.Close()

	logger, buf := newConnectivityTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-zero", Type: "radarr", URL: srv.URL, APIKey: "key"}

	checkInstanceConnectivity(context.Background(), logger, inst)

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected a warning about the zero-value field:\n%s", out)
	}
	if !strings.Contains(out, "appName") {
		t.Errorf("warning does not name the zero-value field appName:\n%s", out)
	}
	// Processing should still continue (informational, not a skip).
	if !strings.Contains(out, "name=HD-1080p") {
		t.Errorf("expected processing to continue past a zero-value field:\n%s", out)
	}
}

func TestCheckInstanceConnectivity_ZeroValueProfileField_LogsWarnNamingField(t *testing.T) {
	profilesJSON := `[{"id": 1, "name": "HD-1080p", "upgradeAllowed": false, "cutoff": 0, "cutoffFormatScore": 0}]`
	var gotPaths, gotAPIKeys []string
	srv := httptest.NewServer(recordingHandler(t, http.StatusOK, radarrStatusJSON, http.StatusOK, profilesJSON, &gotPaths, &gotAPIKeys))
	defer srv.Close()

	logger, buf := newConnectivityTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-zero-profile", Type: "radarr", URL: srv.URL, APIKey: "key"}

	checkInstanceConnectivity(context.Background(), logger, inst)

	out := buf.String()
	for _, field := range []string{"cutoff", "cutoffFormatScore", "upgradeAllowed"} {
		if !strings.Contains(out, field) {
			t.Errorf("warning does not name zero-value field %q:\n%s", field, out)
		}
	}
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected warnings about zero-value profile fields:\n%s", out)
	}
}

func TestCheckInstanceConnectivity_AppNameMismatch_LogsWarnButStillProcessesProfiles(t *testing.T) {
	// Configured as radarr but the server responds as Sonarr.
	var gotPaths, gotAPIKeys []string
	srv := httptest.NewServer(recordingHandler(t, http.StatusOK, sonarrStatusJSON, http.StatusOK, radarrProfilesJSON, &gotPaths, &gotAPIKeys))
	defer srv.Close()

	logger, buf := newConnectivityTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-mislabeled", Type: "radarr", URL: srv.URL, APIKey: "key"}

	checkInstanceConnectivity(context.Background(), logger, inst)

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected a warning about the appName/type mismatch:\n%s", out)
	}
	if !strings.Contains(out, "appName=Sonarr") {
		t.Errorf("expected the mismatched appName to be logged:\n%s", out)
	}
	if !strings.Contains(out, "name=HD-1080p") {
		t.Errorf("mismatch is informational only; profile processing should still happen:\n%s", out)
	}
}

func TestCheckInstanceConnectivity_DebugLogsRawJSONBodies(t *testing.T) {
	var gotPaths, gotAPIKeys []string
	srv := httptest.NewServer(recordingHandler(t, http.StatusOK, radarrStatusJSON, http.StatusOK, radarrProfilesJSON, &gotPaths, &gotAPIKeys))
	defer srv.Close()

	logger, buf := newConnectivityTestLogger(slog.LevelDebug)
	inst := Instance{Name: "radarr-debug", Type: "radarr", URL: srv.URL, APIKey: "key"}

	checkInstanceConnectivity(context.Background(), logger, inst)

	out := buf.String()
	if !strings.Contains(out, `msg="system/status raw response"`) || !strings.Contains(out, "5.14.0.9383") || !strings.Contains(out, "instanceName") {
		t.Errorf("expected the raw system/status JSON body to be present at debug level:\n%s", out)
	}
	if !strings.Contains(out, `msg="qualityprofile raw response"`) || !strings.Contains(out, "HD-1080p") || !strings.Contains(out, "Ultra-HD") {
		t.Errorf("expected the raw qualityprofile JSON body to be present at debug level:\n%s", out)
	}
}

func TestCheckInstanceConnectivity_RawBodiesNotLoggedAboveDebugLevel(t *testing.T) {
	var gotPaths, gotAPIKeys []string
	srv := httptest.NewServer(recordingHandler(t, http.StatusOK, radarrStatusJSON, http.StatusOK, radarrProfilesJSON, &gotPaths, &gotAPIKeys))
	defer srv.Close()

	logger, buf := newConnectivityTestLogger(slog.LevelInfo)
	inst := Instance{Name: "radarr-info-only", Type: "radarr", URL: srv.URL, APIKey: "key"}

	checkInstanceConnectivity(context.Background(), logger, inst)

	out := buf.String()
	if strings.Contains(out, "level=DEBUG") {
		t.Errorf("no debug-level records should appear when logger level is info:\n%s", out)
	}
	if strings.Contains(out, `"buildTime"`) {
		t.Errorf("raw JSON body should not be logged at info level:\n%s", out)
	}
}
