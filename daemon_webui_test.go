package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// --- the composed top-level mux ---------------------------------------------

// TestDaemon_ComposedMux_RouteTable is round-3's regression test for a
// defect nothing else in the suite could catch: webhook_test.go exercises
// newWebhookHandler in ISOLATION (its own dedicated mux), and every other
// daemon test only ever POSTs to a real "/webhook/{instance}" path — nothing
// exercised the REAL route table runDaemon actually serves once the webhook
// mux and the webui mux are composed onto one *http.ServeMux (daemon.go).
//
// That composition silently changed one thing: mounting "/webhook/" as a
// SUBTREE pattern makes net/http auto-register a 301 redirect from the bare
// "/webhook" (no trailing slash) to "/webhook/" — a route this daemon never
// had before (it used to be an ordinary 404, same as any other unmatched
// path). This table was captured by running the exact composition daemon.go
// builds and recording every cell's real status code BEFORE the fix ("/webhook"
// GET/POST were both 301, not 404 — reproduced and confirmed empirically),
// then again after adding the explicit "/webhook" -> http.NotFound
// registration; every other cell is unchanged by that fix, which is the
// point — the fix touches exactly the one route it needs to and nothing
// else moves.
func TestDaemon_ComposedMux_RouteTable(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{wouldUnmonitorStatefulMovie(1, "Movie")})
	h := startDaemon(t, writeDaemonConfig(t, "radarr", fake.srv.URL, true, "info", "0", "45s"))
	h.waitReady()

	// Redirects are never followed: a 301 must show up AS a 301 in this
	// table, not be silently resolved into whatever it redirects to.
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}

	cases := []struct {
		method, path string
		want         int
	}{
		// The regression itself: "/webhook" (no trailing slash, no instance)
		// must still be a plain 404 for both methods, never the subtree's
		// auto-redirect.
		{http.MethodGet, "/webhook", http.StatusNotFound},
		{http.MethodPost, "/webhook", http.StatusNotFound},
		{http.MethodPut, "/webhook", http.StatusNotFound},

		// "/webhook/" itself (empty instance name): the inner mux's own
		// "POST /webhook/" catch-all (handleUnroutable) matches only POST;
		// a GET/PUT to the SAME path, which the inner mux DOES have a
		// pattern for (just the wrong method), is 405 rather than 404 —
		// unchanged by this fix, asserted here so a future change to this
		// composition cannot silently flip it either.
		{http.MethodGet, "/webhook/", http.StatusMethodNotAllowed},
		{http.MethodPost, "/webhook/", http.StatusOK},
		{http.MethodPut, "/webhook/", http.StatusMethodNotAllowed},

		// A real, named instance's webhook endpoint: untouched by this
		// change either way.
		{http.MethodGet, "/webhook/radarr-main", http.StatusMethodNotAllowed},
		{http.MethodPost, "/webhook/radarr-main", http.StatusOK},
		{http.MethodPut, "/webhook/radarr-main", http.StatusMethodNotAllowed},

		// The webui/stats/scan routes this daemon added THIS phase — proof
		// the fix's extra "/webhook" registration does not somehow shadow
		// or interfere with any of them.
		{http.MethodGet, "/", http.StatusOK},
		{http.MethodPost, "/", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/stats", http.StatusOK},
		{http.MethodPost, "/api/stats", http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/scan", http.StatusAccepted},
		{http.MethodGet, "/api/scan", http.StatusMethodNotAllowed},

		// [v2.2] The action endpoint. A GET is 405 from the route pattern
		// itself, and a POST with no body is a 400 — the endpoint's own
		// "could not be read as JSON" refusal, which is the correct answer
		// and proves the route reaches the handler rather than the mux's
		// catch-all. Nothing here performs an action: the daemon under test
		// runs with gui_actions absent (false), so even a well-formed body
		// would be a 403.
		{http.MethodGet, "/api/action", http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/action", http.StatusBadRequest},

		// A path nothing recognizes: the catch-all "/" webui mux's own 404,
		// unaffected by any of the above.
		{http.MethodGet, "/nope", http.StatusNotFound},
		{http.MethodPost, "/nope", http.StatusNotFound},
	}

	for _, c := range cases {
		req, err := http.NewRequest(c.method, h.url+c.path, nil)
		if err != nil {
			t.Fatalf("building %s %s: %v", c.method, c.path, err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", c.method, c.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, resp.StatusCode, c.want)
		}
	}
}

// --- the API surface during the startup scan --------------------------------

// TestDaemon_APIDuringTheStartupScan_StatsAvailableAndScanAlreadyPending is
// round-3's regression test for round-1 fix #2, which had none: moving
// d.scan.begin() to before srv.Serve starts accepting (daemon.go) closes a
// narrow race where a POST /api/scan landing between the listener accepting
// a connection and the startup scan's own begin() call would see
// running=false and wrongly report "queued". Before this test, reverting
// that line back to its original call site (just before the startup scan's
// own runScanCycle) left the ENTIRE suite green — nothing exercised the API
// surface while the startup scan was still in flight at all.
//
// This also exercises two other binding, previously-untested guarantees for
// that same window: GET /api/stats answers promptly with instances: [] (not
// an error, and not blocked on the running cycle) while the startup scan is
// still blocked mid-read, and the manual scan POST/api/scan queues here
// actually runs — exactly once — once the startup scan finishes.
func TestDaemon_APIDuringTheStartupScan_StatsAvailableAndScanAlreadyPending(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{wouldUnmonitorStatefulMovie(1, "First")})
	fake.onRequest = func(method, path string) {
		if path == "/api/v3/movie" {
			<-release // hold the startup scan open
		}
	}
	h := startDaemon(t, writeDaemonConfig(t, "radarr", fake.srv.URL, true, "debug", "0", "45s"))

	eventually(t, "the startup scan to reach the blocked library read", func() bool {
		return strings.Contains(h.out.String(), "system status")
	})
	if strings.Contains(h.out.String(), "startup scan complete") {
		t.Fatal("the scan should still be blocked at this point")
	}

	// GET /api/stats must answer promptly (never blocks on the running
	// cycle) with an empty instances array (the startup scan has not
	// recorded anything for this instance yet).
	stats := getStats(t, h)
	if len(stats.Instances) != 0 {
		t.Errorf("Instances = %+v, want empty while the startup scan is still running", stats.Instances)
	}

	// This is the assertion round-1 fix #2 exists for: a POST landing here,
	// strictly before the startup scan's own runScanCycle call, must still
	// see running=true (begin() was called before srv.Serve started
	// accepting, i.e. before this request could even have been received)
	// and report already-pending, not queued.
	got := postScanTo(t, h.url)
	if got.status != http.StatusAccepted || got.body["status"] != "already-pending" {
		t.Fatalf("POST /api/scan while the startup scan is running = %+v, want 202 {status: already-pending}", got)
	}

	releaseOnce.Do(func() { close(release) })
	h.waitReady()

	// The request queued above must actually run, exactly once, once the
	// startup scan finishes.
	h.awaitLogCount("manual scan complete", 1)
	if got := strings.Count(h.out.String(), "radarr decision summary"); got != 2 {
		t.Errorf("summaries = %d, want 2 (startup + exactly one queued follow-up scan):\n%s", got, h.out.String())
	}
}

// --- unreachable instances --------------------------------------------------

// TestDaemon_UnreachableInstance_VisibleInStatsWithSkippedStatus is
// controller addition N2's end-to-end pin, through the REAL daemon and its
// REAL startup scan: an instance whose connectivity check fails on every
// cycle (a 500 from /api/v3/system/status — an *arr mid-restart is the
// ordinary real-world cause) must still show up in GET /api/stats, not be
// silently absent, carrying an explicit skipped LastCycleStatus and never a
// claim to have swept it.
func TestDaemon_UnreachableInstance_VisibleInStatsWithSkippedStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/system/status", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	h := startDaemon(t, writeDaemonConfig(t, "radarr", srv.URL, true, "info", "0", "45s"))
	h.waitReady()

	stats := getStats(t, h)
	if len(stats.Instances) != 1 {
		t.Fatalf("Instances = %+v, want exactly 1: an instance that has NEVER been reached must still be VISIBLE, not absent", stats.Instances)
	}
	inst := stats.Instances[0]
	if inst.Name != "radarr-main" || inst.Type != "radarr" {
		t.Errorf("instance = %+v, want name=radarr-main type=radarr", inst)
	}
	if inst.LastCycleStatus.Status != cycleStatusSkipped {
		t.Errorf("LastCycleStatus.Status = %q, want %q", inst.LastCycleStatus.Status, cycleStatusSkipped)
	}
	if inst.LastCycleStatus.Reason == "" {
		t.Error("LastCycleStatus.Reason is empty; the operator has no idea why this instance is unreachable")
	}
	if inst.LastRun != nil {
		t.Errorf("LastRun = %v, want nil: this instance has never once completed a cycle", inst.LastRun)
	}
	if inst.Total != 0 {
		t.Errorf("Total = %d, want 0: no library read has ever succeeded for this instance", inst.Total)
	}
}
