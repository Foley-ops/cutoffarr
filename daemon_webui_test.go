package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
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

// TestDaemon_ScanNow_RequestWhileRunning_SurvivesAsExactlyOneQueuedFollowUp
// proves the single-flight coordination end to end, including the
// REVIEW FIX to scanCoordinator.requestScan (daemon.go): a POST that
// arrives while the daemon's loop goroutine is already inside a manual
// scan's runScanCycle call must NOT be silently dropped — it must survive
// as pending and run once the in-flight cycle ends — while a THIRD POST,
// arriving once one is already queued behind the running cycle, must still
// be refused outright (the actual "never two queued" guarantee: at most
// one extra scan may ever be queued behind whatever is currently running).
//
// Renamed from …_ReturnsAlreadyPending (this test's own prior name):
// the wire response for the second POST is still, correctly,
// already-pending — that part never changed — but this test used to also
// assert that only ONE manual scan ever ran, i.e. that the second POST's
// request was silently discarded rather than queued. That was pinning the
// exact bug the fix corrects: the old requestScan returned false without
// ever setting scanCoordinator.pending when only .running was true, so a
// "Scan now" click landing while any cycle was running queued nothing at
// all, yet was reported as "already-pending" — a statement that a sweep is
// coming, when none was. The corrected implementation still forbids TWO
// requests from ever being queued at once (there is only one pending
// flag), but a request that lands during a RUNNING cycle is no longer lost.
func TestDaemon_ScanNow_RequestWhileRunning_SurvivesAsExactlyOneQueuedFollowUp(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})

	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{wouldUnmonitorStatefulMovie(1, "Movie")})
	h := startDaemon(t, writeDaemonConfig(t, "radarr", fake.srv.URL, true, "info", "0", "45s"))
	h.waitReady()

	// Installed only AFTER the startup scan has already finished, so it can
	// never pause the startup scan itself — only the manual scans this test
	// triggers next. Not gated to fire once: BOTH manual scans this test
	// expects to run will pass through their own GET /api/v3/movie, and the
	// second one must sail through once release is closed (a receive on an
	// already-closed channel returns immediately rather than blocking).
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

	// The coordinator truthfully reports already-pending (something IS
	// running right now) — but, unlike the pre-fix behavior, this request
	// survives to run once the running cycle ends.
	second := postScanTo(t, h.url)
	if second.status != http.StatusAccepted || second.body["status"] != "already-pending" {
		t.Fatalf("POST /api/scan while the first is still running = %+v, want 202 {status: already-pending}", second)
	}

	// A THIRD POST, while one is already queued behind the running cycle,
	// must be refused outright: this is the real "never two queued"
	// guarantee.
	third := postScanTo(t, h.url)
	if third.status != http.StatusAccepted || third.body["status"] != "already-pending" {
		t.Fatalf("POST /api/scan while one scan is already pending behind the running cycle = %+v, want 202 {status: already-pending}", third)
	}

	close(release)
	h.awaitLogCount("manual scan complete", 2)

	// Exactly TWO manual sweeps ran (the first POST's own, plus the ONE
	// queued behind it by the second POST) — never three, which is what
	// the third POST above would have produced had it also been queued.
	if got := strings.Count(h.out.String(), "radarr decision summary"); got != 3 {
		t.Errorf("summaries = %d, want 3 (startup + the first manual scan + exactly one queued follow-up, never a second stacked one):\n%s", got, h.out.String())
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

// TestDaemon_ScanNow_NeverInterleavesWithAWebhookCycle proves — structurally,
// not by inspecting whether a log line "tore" — that a webhook cycle and a
// manual scan never actually run at the same time.
//
// REVIEW FIX: this test used to grep every "radarr decision summary" line
// for a trailing "crossCheck=" attr, on the theory that two concurrently
// running cycles would corrupt one another's log line. That instrument
// cannot fail: h.out is a mutex-guarded syncBuffer (see its own doc comment)
// and each slog record is written in one Write call, so lines never tear no
// matter how many goroutines log concurrently — the test could pass even if
// two cycles genuinely ran at once. And the retry loop it used to POST
// /api/scan up to 200 times papered over precisely the window this test
// exists to prove is safe (a POST arriving WHILE the webhook cycle is
// running), rather than exercising it.
//
// Now: the webhook cycle is held open deterministically inside its own
// GET /api/v3/movie library read (the same fake.onRequest technique the
// sibling TestDaemon_ScanNow_SecondRequestWhileFirstIsRunning_
// ReturnsAlreadyPending test above uses for a manual scan). A SINGLE POST
// /api/scan during that window is asserted to return already-pending — the
// coordinator correctly seeing a webhook cycle as "running" — and then the
// read is released and the manual scan the POST queued is asserted to run
// afterwards, proving the request survived rather than being silently
// dropped (the requestScan bug this same round fixed in daemon.go). An
// in-flight counter inside onRequest fails the test outright if two library
// reads are ever open at the same time, which is what an actual
// interleaving regression would produce.
func TestDaemon_ScanNow_NeverInterleavesWithAWebhookCycle(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{wouldUnmonitorStatefulMovie(1, "Movie")})
	h := startDaemon(t, writeDaemonConfig(t, "radarr", fake.srv.URL, true, "info", "0", "1ms"))
	h.waitReady()

	var inFlight int32
	var sawOverlap int32
	blocked := make(chan struct{})
	released := make(chan struct{})
	var once sync.Once

	// Installed only AFTER the startup scan has already finished, so it can
	// never pause the startup scan itself — only the webhook cycle this test
	// triggers next, and (once released) the manual scan queued behind it.
	fake.onRequest = func(method, path string) {
		if method != http.MethodGet || path != "/api/v3/movie" {
			return
		}
		if atomic.AddInt32(&inFlight, 1) > 1 {
			atomic.StoreInt32(&sawOverlap, 1)
		}
		defer atomic.AddInt32(&inFlight, -1)
		// Only the FIRST library read this test ever sees — the webhook
		// cycle's own — blocks. The manual scan's later read (after
		// release) must sail through, or this would deadlock waiting on a
		// release that already happened.
		once.Do(func() {
			close(blocked)
			<-released
		})
	}

	h.post("radarr-main", downloadMoviePayload)
	h.clock.Advance(time.Second)
	h.awaitLogCount("webhook debounce expired; evaluating", 1)

	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatalf("the webhook cycle never reached its library read:\n%s", h.out.String())
	}

	// A single POST — not a retry loop — while the webhook cycle is
	// deterministically held open inside its own library read.
	resp := postScanTo(t, h.url)
	if resp.status != http.StatusAccepted || resp.body["status"] != "already-pending" {
		t.Fatalf("POST /api/scan while the webhook cycle is running = %+v, want 202 {status: already-pending}", resp)
	}

	close(released)
	h.awaitLogCount("manual scan complete", 1)

	if atomic.LoadInt32(&sawOverlap) != 0 {
		t.Fatal("two library reads were open at the same time: the webhook cycle and the manual scan actually ran concurrently, which the coordinator's single-flight guarantee must prevent")
	}

	// Both cycles ran, in addition to the startup scan — proving the
	// already-pending POST above was truthfully queued rather than dropped.
	out := h.out.String()
	if got := strings.Count(out, "radarr decision summary"); got != 3 {
		t.Fatalf("summaries = %d, want 3 (startup + webhook cycle + manual scan):\n%s", got, out)
	}
}
