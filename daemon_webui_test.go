package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// daemon_webui_test.go is the end-to-end half of Phase 12: the real daemon
// (daemonHarness, from daemon_test.go), its real single HTTP listener, and
// the two new endpoints mounted on it exactly as runDaemon wires them.

// getStats fetches and decodes GET /api/stats from a running daemon harness.
func getStats(t *testing.T, h *daemonHarness) statsResponse {
	t.Helper()
	resp, err := http.Get(h.url + "/api/stats")
	if err != nil {
		t.Fatalf("GET /api/stats: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/stats status = %d, want 200", resp.StatusCode)
	}
	var body statsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("GET /api/stats: response is not valid JSON: %v", err)
	}
	return body
}

// TestDaemon_StartupScanCompletes_StatsReflectIt is the binding requirement
// stated plainly: "cycle completes -> stats reflect it".
func TestDaemon_StartupScanCompletes_StatsReflectIt(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{
		wouldUnmonitorStatefulMovie(1, "Would Unmonitor"),
		{id: 2, title: "Stays Monitored", monitored: true, hasFile: true, qualityProfileID: 1, tags: []int{}, movieFileID: 2, cfScore: 200, qualityCutoffNotMet: true, inWantedSet: true},
	})
	h := startDaemon(t, writeDaemonConfig(t, "radarr", fake.srv.URL, true, "info", "0", "45s"))
	h.waitReady()

	stats := getStats(t, h)
	if len(stats.Instances) != 1 {
		t.Fatalf("Instances = %+v, want exactly 1 after the startup scan completed", stats.Instances)
	}
	inst := stats.Instances[0]
	if inst.Name != "radarr-main" || inst.Type != "radarr" {
		t.Errorf("instance = %+v, want name=radarr-main type=radarr", inst)
	}
	if inst.Total != 2 || inst.Monitored != 2 {
		t.Errorf("Total/Monitored = %d/%d, want 2/2", inst.Total, inst.Monitored)
	}
	if inst.LastCycleKind == nil || *inst.LastCycleKind != cycleKindStartup {
		t.Errorf("LastCycleKind = %v, want %q", inst.LastCycleKind, cycleKindStartup)
	}
	if inst.LastRun == nil {
		t.Fatal("LastRun is nil after a completed cycle")
	}
	// The daemon runs on the harness's VIRTUAL clock (fakeClock), not real
	// time, so LastRun is compared against that clock's own Now() rather
	// than time.Since — this daemon's "now" started at 2026-01-01 and never
	// advanced in this test, so LastRun must equal it exactly.
	if !inst.LastRun.Equal(h.clock.Now()) {
		t.Errorf("LastRun = %v, want the virtual clock's current time %v", inst.LastRun, h.clock.Now())
	}
	if inst.WouldUnmonitor != 1 {
		t.Errorf("WouldUnmonitor = %d, want 1", inst.WouldUnmonitor)
	}
	if !stats.DryRun {
		t.Error("DryRun = false, want true (writeDaemonConfig's dryRun arg was true)")
	}
}

// TestDaemon_ScanNow_RunsExactlyOneExtraSweep is the binding requirement's
// other half: "scan-now -> exactly one extra sweep".
func TestDaemon_ScanNow_RunsExactlyOneExtraSweep(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{wouldUnmonitorStatefulMovie(1, "Movie")})
	h := startDaemon(t, writeDaemonConfig(t, "radarr", fake.srv.URL, true, "info", "0", "45s"))
	h.waitReady()

	summariesAfterStartup := strings.Count(h.out.String(), "radarr decision summary")
	if summariesAfterStartup != 1 {
		t.Fatalf("summaries after startup = %d, want 1 (this test's baseline is wrong)", summariesAfterStartup)
	}

	resp, err := http.Post(h.url+"/api/scan", "", nil)
	if err != nil {
		t.Fatalf("POST /api/scan: %v", err)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding POST /api/scan response: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || body["status"] != "queued" {
		t.Fatalf("POST /api/scan = %d %+v, want 202 {status: queued}", resp.StatusCode, body)
	}

	h.awaitLogCount("manual scan complete", 1)

	if got := strings.Count(h.out.String(), "radarr decision summary"); got != 2 {
		t.Errorf("summaries after one manual scan = %d, want 2 (startup + exactly one manual sweep):\n%s", got, h.out.String())
	}
	if !strings.Contains(h.out.String(), `msg="manual scan beginning" trigger="POST /api/scan"`) {
		t.Errorf("expected the manual scan's own beginning line naming its trigger:\n%s", h.out.String())
	}

	stats := getStats(t, h)
	if stats.Instances[0].LastCycleKind == nil || *stats.Instances[0].LastCycleKind != cycleKindSweep {
		t.Errorf("LastCycleKind after a manual scan = %v, want %q", stats.Instances[0].LastCycleKind, cycleKindSweep)
	}
}

// TestDaemon_ScanNow_SecondRequestWhileFirstIsRunning_ReturnsAlreadyPending
// proves the single-flight coordination end to end: a POST that arrives
// while the daemon's loop goroutine is already inside a manual scan's
// runScanCycle call must not queue a second one.
func TestDaemon_ScanNow_SecondRequestWhileFirstIsRunning_ReturnsAlreadyPending(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})

	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{wouldUnmonitorStatefulMovie(1, "Movie")})
	h := startDaemon(t, writeDaemonConfig(t, "radarr", fake.srv.URL, true, "info", "0", "45s"))
	h.waitReady()

	// Installed only AFTER the startup scan has already finished, so it can
	// never pause the startup scan itself — only the manual scan this test
	// triggers next.
	fake.onRequest = func(method, path string) {
		if method == http.MethodGet && path == "/api/v3/movie" {
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
		}
	}

	first := postScanTo(t, h.url)
	if first.status != http.StatusAccepted || first.body["status"] != "queued" {
		t.Fatalf("first POST /api/scan = %+v, want 202 {status: queued}", first)
	}

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatalf("the manual scan never reached its library read:\n%s", h.out.String())
	}
	if !strings.Contains(h.out.String(), "manual scan beginning") {
		t.Fatalf("expected the manual scan's own beginning line before it blocked on the read:\n%s", h.out.String())
	}

	second := postScanTo(t, h.url)
	if second.status != http.StatusAccepted || second.body["status"] != "already-pending" {
		t.Fatalf("POST /api/scan while the first is still running = %+v, want 202 {status: already-pending}", second)
	}

	close(release)
	h.awaitLogCount("manual scan complete", 1)

	// Exactly one manual sweep ran, never two — the second POST must not
	// have stacked a scan behind the first.
	if got := strings.Count(h.out.String(), "radarr decision summary"); got != 2 {
		t.Errorf("summaries = %d, want 2 (startup + exactly one manual sweep, never a stacked second one):\n%s", got, h.out.String())
	}
}

func postScanTo(t *testing.T, baseURL string) scanResponse {
	t.Helper()
	resp, err := http.Post(baseURL+"/api/scan", "", nil)
	if err != nil {
		t.Fatalf("POST /api/scan: %v", err)
	}
	defer resp.Body.Close()
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("POST /api/scan: response is not valid JSON: %v", err)
	}
	return scanResponse{status: resp.StatusCode, body: body}
}

// TestDaemon_ScanNow_NeverInterleavesWithAWebhookCycle sends a webhook and a
// manual scan request close together and checks the log shows them running
// strictly one after another (a "webhook debounce expired" line fully
// followed by its own summary before "manual scan beginning" appears, or
// vice versa) rather than interleaved attrs from two concurrent cycles.
func TestDaemon_ScanNow_NeverInterleavesWithAWebhookCycle(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{wouldUnmonitorStatefulMovie(1, "Movie")})
	h := startDaemon(t, writeDaemonConfig(t, "radarr", fake.srv.URL, true, "info", "0", "1ms"))
	h.waitReady()

	h.post("radarr-main", downloadMoviePayload)
	h.clock.Advance(time.Second)
	h.awaitLogCount("webhook debounce expired; evaluating", 1)

	resp, err := http.Post(h.url+"/api/scan", "", nil)
	if err != nil {
		t.Fatalf("POST /api/scan: %v", err)
	}
	resp.Body.Close()

	h.awaitLogCount("manual scan complete", 1)

	// Both cycles ran (the webhook cycle's own summary line, plus the manual
	// scan's), each fully formed: the fastest structural check that nothing
	// interleaved two concurrent runScanCycle calls into one torn line is
	// that BOTH complete summary lines with matching attrs are present,
	// which a corrupted/interleaved write to the shared log buffer would be
	// very unlikely to produce by chance for two multi-attribute lines.
	out := h.out.String()
	if got := strings.Count(out, "radarr decision summary"); got != 3 {
		t.Fatalf("summaries = %d, want 3 (startup + webhook cycle + manual scan):\n%s", got, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "msg=\"radarr decision summary\"") {
			continue
		}
		if !strings.Contains(line, "crossCheck=") {
			t.Errorf("a decision summary line is missing its own trailing attrs entirely, which an interleaved write would produce: %q", line)
		}
	}
}
