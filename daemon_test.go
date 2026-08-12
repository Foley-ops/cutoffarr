package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// --- the virtual clock ------------------------------------------------------
//
// Real waits are not an option here: the debounce defaults to 45 seconds and
// poll_interval has a one-hour MINIMUM (config.go refuses less, deliberately),
// so a suite that waited for either would be measured in hours. Shrinking them
// to milliseconds instead would make every timing assertion a race against the
// machine's load. The clock is therefore virtual and the tests move it by hand.
//
// The one race that matters — the test advancing time before the daemon has
// armed its timer — is closed by construction rather than by synchronization:
// virtual time only ever moves forward, and NewTimer fires IMMEDIATELY for a
// non-positive duration. A daemon that arms late computes its duration from a
// clock that has already passed the deadline, gets a non-positive number, and
// wakes at once.

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	c    chan time.Time
	at   time.Time
	done bool
}

func (t *fakeTimer) C() <-chan time.Time { return t.c }
func (t *fakeTimer) Stop()               { t.done = true }

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(d time.Duration) daemonTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c: make(chan time.Time, 1), at: c.now.Add(d)}
	if !t.at.After(c.now) {
		t.c <- c.now
		t.done = true
		return t
	}
	c.timers = append(c.timers, t)
	return t
}

// Advance moves virtual time forward and fires every timer that comes due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	var still []*fakeTimer
	for _, t := range c.timers {
		if t.done {
			continue
		}
		if !t.at.After(c.now) {
			t.done = true
			select {
			case t.c <- c.now:
			default:
			}
			continue
		}
		still = append(still, t)
	}
	c.timers = still
}

// --- the daemon harness -----------------------------------------------------

// daemonHarness runs run() without --once on a goroutine, with an ephemeral
// listener, a virtual clock, and a shutdown channel the test controls.
type daemonHarness struct {
	t        *testing.T
	url      string
	clock    *fakeClock
	signals  chan os.Signal
	exitCode chan int
	out      *syncBuffer
	ready    chan struct{}
	exited   bool
}

// syncBuffer is a bytes.Buffer safe to read while the daemon writes to it. The
// daemon logs from its own goroutine throughout, so every assertion in this
// file reads the log concurrently with production code writing it — without
// this, -race would (correctly) fail every test here.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func startDaemon(t *testing.T, configPath string) *daemonHarness {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("taking an ephemeral port: %v", err)
	}
	h := &daemonHarness{
		t:        t,
		url:      "http://" + listener.Addr().String(),
		clock:    newFakeClock(),
		signals:  make(chan os.Signal, 2),
		exitCode: make(chan int, 1),
		out:      &syncBuffer{},
		ready:    make(chan struct{}),
	}
	go func() {
		h.exitCode <- run([]string{"--config", configPath}, h.out, h.out, daemonOptions{
			clock:             h.clock,
			listener:          listener,
			signals:           h.signals,
			forceExit:         func(int) { panic("forceExit must not be reached in this test") },
			onStartupScanDone: func() { close(h.ready) },
		})
	}()
	t.Cleanup(func() {
		if !h.exited {
			h.stop()
		}
	})
	return h
}

// waitReady blocks until the startup scan has finished.
func (h *daemonHarness) waitReady() {
	h.t.Helper()
	select {
	case <-h.ready:
	case <-time.After(5 * time.Second):
		h.t.Fatalf("the daemon never finished its startup scan:\n%s", h.out.String())
	}
}

// post sends a webhook body to the daemon's endpoint and returns the status.
func (h *daemonHarness) post(instance, body string) int {
	h.t.Helper()
	resp, err := http.Post(h.url+"/webhook/"+instance, "application/json", strings.NewReader(body))
	if err != nil {
		h.t.Fatalf("posting a webhook: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// stop delivers one shutdown signal and waits for the exit code.
func (h *daemonHarness) stop() int {
	h.t.Helper()
	h.exited = true
	h.signals <- syscall.SIGTERM
	select {
	case code := <-h.exitCode:
		return code
	case <-time.After(10 * time.Second):
		h.t.Fatalf("the daemon did not exit within 10s of SIGTERM:\n%s", h.out.String())
		return -1
	}
}

// eventually polls cond until it holds or the deadline passes. It exists so a
// test can wait for the daemon's own goroutine to reach an observable state
// without sleeping for a fixed duration: it returns the instant the condition
// becomes true, which is typically the first poll.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// writeDaemonConfig writes a config pointing at one instance.
func writeDaemonConfig(t *testing.T, instType, url string, dryRun bool, logLevel, pollInterval, debounce string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := fmt.Sprintf(`
dry_run: %t
log_level: %s
poll_interval: %s
webhook_debounce: %s
instances:
  - name: %s-main
    type: %s
    url: %s
    api_key: key1
`, dryRun, logLevel, pollInterval, debounce, instType, instType, url)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing the daemon test config: %v", err)
	}
	return path
}

// startDaemonWithArgs is startDaemon for a test that needs to pass extra flags
// (the --only-id/--instance no-effect warnings, which are emitted on the daemon
// branch before the daemon starts).
func startDaemonWithArgs(t *testing.T, args []string) *daemonHarness {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("taking an ephemeral port: %v", err)
	}
	h := &daemonHarness{
		t:        t,
		url:      "http://" + listener.Addr().String(),
		clock:    newFakeClock(),
		signals:  make(chan os.Signal, 2),
		exitCode: make(chan int, 1),
		out:      &syncBuffer{},
		ready:    make(chan struct{}),
	}
	go func() {
		h.exitCode <- run(args, h.out, h.out, daemonOptions{
			clock:             h.clock,
			listener:          listener,
			signals:           h.signals,
			forceExit:         func(int) { panic("forceExit must not be reached in this test") },
			onStartupScanDone: func() { close(h.ready) },
		})
	}()
	t.Cleanup(func() {
		if !h.exited {
			h.stop()
		}
	})
	return h
}

// mark records the current length of the captured log, so a later since(mark)
// returns only what a particular cycle produced.
func (h *daemonHarness) mark() int { return len(h.out.String()) }

func (h *daemonHarness) since(mark int) string {
	out := h.out.String()
	if mark > len(out) {
		return ""
	}
	return out[mark:]
}

// awaitLogCount blocks until the log contains want occurrences of substr.
func (h *daemonHarness) awaitLogCount(substr string, want int) {
	h.t.Helper()
	eventually(h.t, fmt.Sprintf("%d occurrence(s) of %q in the daemon log", want, substr), func() bool {
		return strings.Count(h.out.String(), substr) >= want
	})
}

const downloadMoviePayload = `{"eventType":"Download","isUpgrade":false,"movie":{"id":1,"title":"Imported"}}`

// --- webhook -> debounce -> ONE scoped evaluation ---------------------------

// TestDaemon_SeasonPackBurst_ProducesExactlyOneEvaluationOfThatSeries is the
// plan's own acceptance criterion, in the shape that motivated the debounce at
// all: importing a 24-episode season pack fires 24 webhooks, and 24 full
// library evaluations of the same show would be absurd — and would read Sonarr
// mid-import, before it has finished its own database writes.
func TestDaemon_SeasonPackBurst_ProducesExactlyOneEvaluationOfThatSeries(t *testing.T) {
	fake := writableSonarrFake(t)
	h := startDaemon(t, writeDaemonConfig(t, "sonarr", fake.srv.URL, true, "debug", "0", "45s"))
	h.waitReady()
	startupSummaries := strings.Count(h.out.String(), "sonarr decision summary")

	for episode := 1; episode <= 24; episode++ {
		body := fmt.Sprintf(`{"eventType":"Download","series":{"id":1,"title":"Write Me"},"episodes":[{"seasonNumber":1,"episodeNumber":%d}]}`, episode)
		if code := h.post("sonarr-main", body); code != http.StatusOK {
			t.Fatalf("event %d: status = %d, want 200", episode, code)
		}
	}
	eventually(t, "all 24 events to be queued as one key", func() bool {
		return strings.Count(h.out.String(), "webhook queued") == 24
	})

	h.clock.Advance(45 * time.Second)
	h.awaitLogCount("webhook debounce expired; evaluating", 1)
	// Nothing else may follow. Give the loop room to misbehave before asserting.
	h.clock.Advance(10 * time.Minute)
	time.Sleep(20 * time.Millisecond)
	h.stop()

	out := h.out.String()
	if n := strings.Count(out, "webhook debounce expired; evaluating"); n != 1 {
		t.Errorf("24 events for one series must produce exactly 1 evaluation, got %d:\n%s", n, out)
	}
	if n := strings.Count(out, "sonarr decision summary") - startupSummaries; n != 1 {
		t.Errorf("expected exactly one post-startup evaluation of the series, got %d", n)
	}
	if !strings.Contains(out, "onlyId=1") {
		t.Errorf("the evaluation must be SCOPED to the series the events named:\n%s", out)
	}
}

// TestDaemon_TwoItemsOfOneInstance_CoalesceIntoOneEvaluation is the mandated
// optimization (binding controller note 2). Two distinct keys are two items,
// but they are one INSTANCE, and the evaluation is per-instance and
// full-evidence — so running it twice would read the whole library twice to
// write two movies.
func TestDaemon_TwoItemsOfOneInstance_CoalesceIntoOneEvaluation(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{
		wouldUnmonitorStatefulMovie(1, "First"),
		wouldUnmonitorStatefulMovie(2, "Second"),
	})
	h := startDaemon(t, writeDaemonConfig(t, "radarr", fake.srv.URL, true, "debug", "0", "45s"))
	h.waitReady()
	mark := h.mark()

	h.post("radarr-main", downloadMoviePayload)
	h.post("radarr-main", `{"eventType":"Download","movie":{"id":2,"title":"Second"}}`)
	eventually(t, "both events to be queued", func() bool {
		return strings.Count(h.out.String(), "webhook queued") == 2
	})

	h.clock.Advance(45 * time.Second)
	h.awaitLogCount("webhook debounce expired; evaluating", 1)
	time.Sleep(20 * time.Millisecond)
	h.stop()

	cycle := h.since(mark)
	if n := strings.Count(cycle, "webhook debounce expired; evaluating"); n != 1 {
		t.Errorf("two items of ONE instance must coalesce into one evaluation, got %d:\n%s", n, cycle)
	}
	if !strings.Contains(cycle, `scopeIds=1,2`) {
		t.Errorf("the coalesced evaluation's write scope must be the union of the ids:\n%s", cycle)
	}
	if n := strings.Count(cycle, "radarr decision summary"); n != 1 {
		t.Errorf("expected exactly one summary for the coalesced cycle, got %d:\n%s", n, cycle)
	}
}

// TestDaemon_WebhookForAnUnconfiguredInstance_NeverEvaluatesAnything: a stray
// or mistyped hook must not cost a library scan, and must not take the daemon
// down either.
func TestDaemon_WebhookForAnUnconfiguredInstance_NeverEvaluatesAnything(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{wouldUnmonitorStatefulMovie(1, "First")})
	h := startDaemon(t, writeDaemonConfig(t, "radarr", fake.srv.URL, true, "debug", "0", "45s"))
	h.waitReady()
	mark := h.mark()

	if code := h.post("radarr-typo", downloadMoviePayload); code != http.StatusOK {
		t.Fatalf("status = %d, want 200: an unknown instance is our problem, not the sender's", code)
	}
	h.clock.Advance(10 * time.Minute)
	time.Sleep(20 * time.Millisecond)
	if code := h.stop(); code != 0 {
		t.Errorf("exit code = %d, want 0: a stray webhook must never take the daemon down", code)
	}

	cycle := h.since(mark)
	if strings.Contains(cycle, "webhook debounce expired") || strings.Contains(cycle, "radarr decision summary") {
		t.Errorf("an unknown instance must queue and evaluate nothing:\n%s", cycle)
	}
	if !strings.Contains(cycle, "level=WARN") {
		t.Errorf("it must still be visible:\n%s", cycle)
	}
}

// --- the reconciliation sweep ----------------------------------------------

func TestDaemon_ReconciliationTicker_FiresTheFullSweepOnSchedule(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{wouldUnmonitorStatefulMovie(1, "First")})
	h := startDaemon(t, writeDaemonConfig(t, "radarr", fake.srv.URL, true, "info", "1h", "45s"))
	h.waitReady()
	// The loop computes its first deadline from the clock as it enters; waiting
	// for it to say so is the synchronization point, not a sleep.
	eventually(t, "the reconciliation schedule to be announced", func() bool {
		return strings.Contains(h.out.String(), "reconciliation sweep scheduled")
	})
	mark := h.mark()

	h.clock.Advance(time.Hour)
	h.awaitLogCount("reconciliation sweep complete", 1)

	// And again: the sweep re-arms itself rather than firing once.
	h.clock.Advance(time.Hour)
	h.awaitLogCount("reconciliation sweep complete", 2)
	h.stop()

	cycle := h.since(mark)
	if n := strings.Count(cycle, "radarr decision summary"); n != 2 {
		t.Errorf("expected one full sweep per poll interval, got %d summaries:\n%s", n, cycle)
	}
	if strings.Contains(cycle, "onlyId=") || strings.Contains(cycle, "scopeIds=") {
		t.Errorf("a reconciliation sweep is the WHOLE library, never a scoped one:\n%s", cycle)
	}
}

// TestDaemon_PollIntervalZero_DisablesTheSweepButNotTheListener is the
// webhooks-only setup config.go explicitly allows ("use 0 to disable the
// sweep"). The listener must still be up, or the daemon would do nothing at all
// and say nothing about it.
func TestDaemon_PollIntervalZero_DisablesTheSweepButNotTheListener(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{wouldUnmonitorStatefulMovie(1, "First")})
	h := startDaemon(t, writeDaemonConfig(t, "radarr", fake.srv.URL, true, "debug", "0", "45s"))
	h.waitReady()
	eventually(t, "the disabled sweep to be announced", func() bool {
		return strings.Contains(h.out.String(), "reconciliation sweep disabled")
	})
	mark := h.mark()

	// A year of virtual time, and no sweep.
	h.clock.Advance(24 * 365 * time.Hour)
	time.Sleep(20 * time.Millisecond)

	// The listener is still up and still queues work.
	if code := h.post("radarr-main", downloadMoviePayload); code != http.StatusOK {
		t.Fatalf("status = %d, want 200: poll_interval 0 disables the sweep, not the endpoint", code)
	}
	h.clock.Advance(45 * time.Second)
	h.awaitLogCount("webhook debounce expired; evaluating", 1)
	h.stop()

	cycle := h.since(mark)
	if strings.Contains(cycle, "reconciliation sweep beginning") {
		t.Errorf("poll_interval 0 must disable the sweep entirely:\n%s", cycle)
	}
}

// --- §2.1 in daemon mode ----------------------------------------------------

// TestDaemon_DryRun_MakesZeroWriteRequestsAcrossEveryKindOfCycle is this
// project's most important guarantee, asserted over the run mode that performs
// the most cycles. Startup scan, reconciliation sweep and a webhook-triggered
// evaluation all run here, and writes() is the fake's catch-all — every
// non-GET request to any path, stubbed or not — so "zero writes" is a claim
// about requests nobody thought to make.
func TestDaemon_DryRun_MakesZeroWriteRequestsAcrossEveryKindOfCycle(t *testing.T) {
	radarr := newStatefulRadarrFake(t, []*statefulRadarrMovie{wouldUnmonitorStatefulMovie(1, "First")})
	h := startDaemon(t, writeDaemonConfig(t, "radarr", radarr.srv.URL, true, "debug", "1h", "45s"))
	h.waitReady()
	eventually(t, "the reconciliation schedule to be announced", func() bool {
		return strings.Contains(h.out.String(), "reconciliation sweep scheduled")
	})

	h.clock.Advance(time.Hour)
	h.awaitLogCount("reconciliation sweep complete", 1)

	h.post("radarr-main", downloadMoviePayload)
	h.clock.Advance(45 * time.Second)
	h.awaitLogCount("webhook debounce expired; evaluating", 1)
	h.stop()

	if writes := radarr.writes(); len(writes) != 0 {
		t.Fatalf("dry-run made %d write request(s) across the daemon's cycles, want ZERO: %+v", len(writes), writes)
	}
	out := h.out.String()
	if !strings.Contains(out, "wouldUnmonitor=1") {
		t.Errorf("the rehearsal must still REPORT what it would have done:\n%s", out)
	}
	if !strings.Contains(out, "withheldWrites=1") {
		t.Errorf("a dry-run's withheld writes must be accounted for in daemon mode too:\n%s", out)
	}
}

// TestDaemon_WriteMode_WebhookTriggeredWrite_HappensAndIsScoped is the same
// pipeline with the gate open. The startup scan deliberately finds nothing to
// write (the movie is below its cutoff); the webhook then announces exactly
// what a Download announces — a file landed — and the scoped evaluation writes
// it and nothing else.
func TestDaemon_WriteMode_WebhookTriggeredWrite_HappensAndIsScoped(t *testing.T) {
	radarr := newStatefulRadarrFake(t, []*statefulRadarrMovie{
		wouldUnmonitorStatefulMovie(1, "Imported"),
		wouldUnmonitorStatefulMovie(2, "Untouched"),
	})
	radarr.setWanted(1, true) // below cutoff: the startup scan skips it
	radarr.setWanted(2, true) // and its neighbour, so the sweep writes nothing either
	h := startDaemon(t, writeDaemonConfig(t, "radarr", radarr.srv.URL, false, "debug", "0", "45s"))
	h.waitReady()

	if writes := radarr.writes(); len(writes) != 0 {
		t.Fatalf("the startup scan should have had nothing to write, got %+v", writes)
	}

	// The import completes: movie 1 now meets its cutoff, movie 2 does not.
	radarr.setWanted(1, false)
	h.post("radarr-main", downloadMoviePayload)
	h.clock.Advance(45 * time.Second)
	h.awaitLogCount("webhook debounce expired; evaluating", 1)
	eventually(t, "the scoped write to land", func() bool { return len(radarr.puts()) == 1 })
	h.stop()

	puts := radarr.puts()
	if len(puts) != 1 || puts[0].path != "/api/v3/movie/1" {
		t.Fatalf("expected exactly one PUT, to the movie the webhook named, got %+v", puts)
	}
	out := h.out.String()
	if !strings.Contains(out, "msg=unmonitor ") || !strings.Contains(out, "unmonitored=1") {
		t.Errorf("the write must be announced at INFO and counted:\n%s", out)
	}
}

// --- the idle-cycle noise budget --------------------------------------------

// TestDaemon_SecondCycleWithNothingChanged_LogsSummariesOnlyAtInfo is binding
// controller note 6, machine-verified. The daemon repeats this cycle forever;
// if every idle sweep reprinted the whole per-item report, the log would be
// unreadable within a day and the lines that matter — writes, warnings — would
// be buried in it.
func TestDaemon_SecondCycleWithNothingChanged_LogsSummariesOnlyAtInfo(t *testing.T) {
	fake := writableSonarrFake(t)
	h := startDaemon(t, writeDaemonConfig(t, "sonarr", fake.srv.URL, false, "info", "1h", "45s"))
	h.waitReady()
	eventually(t, "the reconciliation schedule to be announced", func() bool {
		return strings.Contains(h.out.String(), "reconciliation sweep scheduled")
	})

	// Cycle 1 is the startup scan: the full report, at INFO, as a --once run
	// would print it.
	startup := h.out.String()
	if !strings.Contains(startup, "msg=would-unmonitor") {
		t.Fatalf("the startup scan is the watched cycle and reports in full at INFO:\n%s", startup)
	}
	writesAfterStartup := len(fake.writes())
	if writesAfterStartup == 0 {
		t.Fatalf("the startup scan should have written the eligible season:\n%s", startup)
	}

	mark := h.mark()
	h.clock.Advance(time.Hour)
	h.awaitLogCount("reconciliation sweep complete", 1)
	// The cycle's own output, captured BEFORE the shutdown: the shutdown lines
	// are the daemon stopping, not the cycle running, and folding them in would
	// make this test about the wrong thing.
	cycle2 := h.since(mark)
	h.stop()

	if len(fake.writes()) != writesAfterStartup {
		t.Errorf("the second cycle must be a no-op, it made %d more write(s)", len(fake.writes())-writesAfterStartup)
	}
	if strings.Contains(cycle2, "level=WARN") || strings.Contains(cycle2, "level=ERROR") {
		t.Errorf("a steady-state cycle must be free of warnings and errors:\n%s", cycle2)
	}

	// SUMMARIES ONLY, asserted as such rather than as a list of forbidden
	// messages: an idle cycle's INFO output is the daemon's own two bookends
	// (the sweep beginning and completing) plus one decision summary per
	// instance, and NOTHING that scales with the size of the library. A
	// forbidden-substring test would pass the day someone adds a thirteenth
	// per-item INFO line nobody thought to list.
	allowedInfo := []string{
		"reconciliation sweep beginning",
		"reconciliation sweep complete",
		"sonarr decision summary",
	}
	sawSummary := false
	for _, line := range strings.Split(cycle2, "\n") {
		if !strings.Contains(line, "level=INFO") {
			continue
		}
		matched := ""
		for _, allowed := range allowedInfo {
			if strings.Contains(line, allowed) {
				matched = allowed
			}
		}
		if matched == "" {
			t.Errorf("an idle reconciliation cycle printed an INFO line that is not a summary:\n%s\n(whole cycle:\n%s)", line, cycle2)
		}
		if matched == "sonarr decision summary" {
			sawSummary = true
		}
	}
	if !sawSummary {
		t.Errorf("the summary is what an idle cycle DOES print — it is how a human sees the daemon is alive and idle:\n%s", cycle2)
	}

	// And the demotion is a demotion, not a deletion: everything the cycle
	// stopped saying at INFO is still there at DEBUG for anyone who turns it
	// on. Proven by the startup scan, which said all of it at INFO.
	if !strings.Contains(startup, "msg=would-unmonitor") || !strings.Contains(startup, "cross-check season") {
		t.Errorf("the startup scan must still report in full at INFO:\n%s", startup)
	}
}

// TestDaemon_ReconciliationCycleAtDebug_StillSaysEverything is the other half
// of the noise budget, and the reason it is a DEMOTION rather than a deletion:
// with log_level: debug, an idle sweep says exactly what the startup scan said.
func TestDaemon_ReconciliationCycleAtDebug_StillSaysEverything(t *testing.T) {
	fake := writableSonarrFake(t)
	h := startDaemon(t, writeDaemonConfig(t, "sonarr", fake.srv.URL, true, "debug", "1h", "45s"))
	h.waitReady()
	eventually(t, "the reconciliation schedule to be announced", func() bool {
		return strings.Contains(h.out.String(), "reconciliation sweep scheduled")
	})
	mark := h.mark()

	h.clock.Advance(time.Hour)
	h.awaitLogCount("reconciliation sweep complete", 1)
	h.stop()

	cycle2 := h.since(mark)
	for _, want := range []string{"msg=would-unmonitor", "cross-check season", "system status", "series library"} {
		if !strings.Contains(cycle2, want) {
			t.Errorf("at log_level=debug an idle cycle must still say %q — the noise budget demotes, it never deletes:\n%s", want, cycle2)
		}
	}
	if strings.Contains(cycle2, "level=INFO msg=would-unmonitor") {
		t.Errorf("...and it must say it at DEBUG:\n%s", cycle2)
	}
}

// --- shutdown ---------------------------------------------------------------

// TestDaemon_ShutdownDuringAnEvaluation_FinishesTheItemDrainsNothingElseExitsZero
// is the SIGTERM contract end to end: the signal arrives while an evaluation is
// under way, that item's writes complete, everything still queued is dropped
// (the next startup scan covers it), the server closes, and the process exits
// 0.
func TestDaemon_ShutdownDuringAnEvaluation_FinishesTheItemDrainsNothingElseExitsZero(t *testing.T) {
	fake := writableSonarrFake(t)
	h := startDaemon(t, writeDaemonConfig(t, "sonarr", fake.srv.URL, false, "debug", "0", "45s"))
	h.waitReady()
	writesAfterStartup := len(fake.writes())

	// Queue an item and shut down BEFORE its debounce expires: it must be
	// dropped rather than evaluated on the way out.
	h.post("sonarr-main", `{"eventType":"Download","series":{"id":1}}`)
	eventually(t, "the event to be queued", func() bool {
		return strings.Contains(h.out.String(), "webhook queued")
	})

	code := h.stop()
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 for a clean shutdown:\n%s", code, h.out.String())
	}
	if len(fake.writes()) != writesAfterStartup {
		t.Errorf("a queued item must not be evaluated during shutdown, %d write(s) appeared", len(fake.writes())-writesAfterStartup)
	}
	out := h.out.String()
	if !strings.Contains(out, "shutdown signal received") {
		t.Errorf("the signal must be acknowledged:\n%s", out)
	}
	if !strings.Contains(out, "shutdown complete") {
		t.Errorf("the shutdown must announce that it finished:\n%s", out)
	}
	if !strings.Contains(out, "pendingWebhookItemsDropped=1") {
		t.Errorf("what the queue still held must be stated, not silently discarded:\n%s", out)
	}

	// The listener really is closed: a webhook after shutdown fails to connect.
	if _, err := http.Post(h.url+"/webhook/sonarr-main", "application/json", strings.NewReader(`{}`)); err == nil {
		t.Error("the webhook server must be closed after shutdown")
	}
}

// TestDaemon_SecondSignal_ForcesAnImmediateNonZeroExit: the first signal is a
// request and the second is an order. A human who sends SIGTERM twice has
// decided they are not waiting, and the most likely reason is that something is
// stuck; exit code 1 says the stop was not clean.
func TestDaemon_SecondSignal_ForcesAnImmediateNonZeroExit(t *testing.T) {
	// A fake whose library read blocks forever: the startup scan cannot finish,
	// which is exactly the stuck daemon a second signal exists for.
	release := make(chan struct{})
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/movie" {
			<-release
		}
		w.Write([]byte(`{"appName":"Radarr","version":"5.14.0.9383"}`))
	}))
	defer func() { close(release); blocked.Close() }()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("taking an ephemeral port: %v", err)
	}
	signals := make(chan os.Signal, 2)
	forced := make(chan int, 1)
	out := &syncBuffer{}
	go func() {
		run([]string{"--config", writeDaemonConfig(t, "radarr", blocked.URL, true, "info", "0", "45s")}, out, out, daemonOptions{
			clock:     newFakeClock(),
			listener:  listener,
			signals:   signals,
			forceExit: func(code int) { forced <- code; select {} },
		})
	}()
	eventually(t, "the startup scan to reach the blocking read", func() bool {
		return strings.Contains(out.String(), "system status")
	})

	signals <- syscall.SIGTERM
	signals <- syscall.SIGTERM

	select {
	case code := <-forced:
		if code != 1 {
			t.Errorf("forced exit code = %d, want 1: a stop that could not be completed cleanly is not a clean stop", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("a second signal must force an immediate exit even while an evaluation is stuck:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "second shutdown signal received") {
		t.Errorf("the forced exit must say why it happened:\n%s", out.String())
	}
}

// --- startup ordering -------------------------------------------------------

// TestDaemon_WebhookDuringTheStartupScan_IsAcceptedAndQueued documents and pins
// the startup ordering this daemon chose: the listener BINDS AND ACCEPTS FIRST,
// then the startup scan runs, then the loop dispatches.
//
// The alternative — scan first, listen after — loses events twice over: the
// *arr records a failed webhook (some versions disable a hook that keeps
// failing) and the human sees an error whose cause is "cutoffarr was busy
// starting up". Accepting costs nothing, because the handler does no I/O beyond
// reading the body, and nothing can be evaluated concurrently with the startup
// scan either way: the dispatcher IS the goroutine running the scan.
func TestDaemon_WebhookDuringTheStartupScan_IsAcceptedAndQueued(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{wouldUnmonitorStatefulMovie(1, "First")})
	fake.onRequest = func(method, path string) {
		if path == "/api/v3/movie" {
			<-release // hold the startup scan open
		}
	}
	h := startDaemon(t, writeDaemonConfig(t, "radarr", fake.srv.URL, true, "debug", "0", "45s"))

	// The scan is blocked mid-read; the endpoint must answer anyway.
	eventually(t, "the startup scan to reach the blocked library read", func() bool {
		return strings.Contains(h.out.String(), "system status")
	})
	if strings.Contains(h.out.String(), "startup scan complete") {
		t.Fatal("the scan should still be blocked at this point")
	}
	if code := h.post("radarr-main", downloadMoviePayload); code != http.StatusOK {
		t.Fatalf("status = %d, want 200 while the startup scan is still running", code)
	}

	releaseOnce.Do(func() { close(release) })
	h.waitReady()

	// And the event queued during the scan is dispatched once the loop starts.
	h.clock.Advance(45 * time.Second)
	h.awaitLogCount("webhook debounce expired; evaluating", 1)
	h.stop()

	out := h.out.String()
	queuedAt := strings.Index(out, "webhook queued")
	scanDoneAt := strings.Index(out, "startup scan complete")
	if queuedAt == -1 || scanDoneAt == -1 || queuedAt > scanDoneAt {
		t.Errorf("the event must have been accepted BEFORE the startup scan finished:\n%s", out)
	}
}

// TestDaemon_HandlerDoesNotBlockOnEvaluation is the plan's "return 200
// immediately and queue the work; never make the *arr wait on our evaluation".
// The daemon here is stuck inside a library read for the entire test, and the
// endpoint still answers.
func TestDaemon_HandlerDoesNotBlockOnEvaluation(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer func() { releaseOnce.Do(func() { close(release) }) }()
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{wouldUnmonitorStatefulMovie(1, "First")})
	fake.onRequest = func(method, path string) {
		if path == "/api/v3/movie" {
			<-release
		}
	}
	h := startDaemon(t, writeDaemonConfig(t, "radarr", fake.srv.URL, true, "info", "0", "45s"))
	eventually(t, "the evaluation to be under way", func() bool {
		return strings.Contains(h.out.String(), "system status")
	})

	start := time.Now()
	code := h.post("radarr-main", downloadMoviePayload)
	elapsed := time.Since(start)

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	// Generous by three orders of magnitude: the point is that the response
	// does not wait on an evaluation that is blocked indefinitely, not that it
	// is fast on a loaded CI box.
	if elapsed > 2*time.Second {
		t.Errorf("the handler took %s while an evaluation was in flight; it must never wait on one", elapsed)
	}
	releaseOnce.Do(func() { close(release) })
	h.waitReady()
	h.stop()
}

// TestDaemon_ShutdownDuringAWritePass_FinishesTheInFlightItemAndWithholdsTheRest
// is the between-items boundary at the whole-daemon level. The unit tests pin
// the invariant inside each write pass (see the Sonarr season-pair test); this
// one proves the daemon really does deliver its shutdown into that boundary and
// still exits 0.
func TestDaemon_ShutdownDuringAWritePass_FinishesTheInFlightItemAndWithholdsTheRest(t *testing.T) {
	fake := newStatefulRadarrFake(t, []*statefulRadarrMovie{
		wouldUnmonitorStatefulMovie(1, "First"),
		wouldUnmonitorStatefulMovie(2, "Second"),
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("taking an ephemeral port: %v", err)
	}
	signals := make(chan os.Signal, 2)
	out := &syncBuffer{}
	exitCode := make(chan int, 1)
	var once sync.Once
	fake.onRequest = func(method, path string) {
		if method == http.MethodPut {
			once.Do(func() { signals <- syscall.SIGTERM })
		}
	}
	go func() {
		exitCode <- run([]string{"--config", writeDaemonConfig(t, "radarr", fake.srv.URL, false, "info", "0", "45s")}, out, out, daemonOptions{
			clock:     newFakeClock(),
			listener:  listener,
			signals:   signals,
			forceExit: func(int) { panic("forceExit must not be reached in this test") },
		})
	}()

	select {
	case code := <-exitCode:
		if code != 0 {
			t.Fatalf("exit code = %d, want 0:\n%s", code, out.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the daemon did not exit:\n%s", out.String())
	}

	puts := fake.puts()
	if len(puts) != 1 || puts[0].path != "/api/v3/movie/1" {
		t.Errorf("expected exactly the in-flight movie's write to complete, got %+v", puts)
	}
	log := out.String()
	if !strings.Contains(log, "shutdown requested: the remaining pending writes") {
		t.Errorf("the withheld remainder must be stated:\n%s", log)
	}
	if !strings.Contains(log, "withheldWrites=1") {
		t.Errorf("a shutdown must not make a pending write vanish from the accounting:\n%s", log)
	}
}

// TestDaemon_WebhookPortAlreadyBound_IsFatalRatherThanASilentPollOnlyDaemon:
// a daemon quietly running without its listener would reconcile only on the
// poll interval — up to a day late by default — while every log line and every
// import looked normal. That is precisely the "silently does nothing" failure
// this project refuses everywhere else, so it exits non-zero and says why.
func TestDaemon_WebhookPortAlreadyBound_IsFatalRatherThanASilentPollOnlyDaemon(t *testing.T) {
	// ":0", not "127.0.0.1:0": the daemon binds every interface, and on BSD
	// (macOS) binding 0.0.0.0:P while only 127.0.0.1:P is taken SUCCEEDS — so a
	// loopback-only occupier would leave this test starting a real daemon
	// nothing could stop.
	occupied, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("taking a port to occupy: %v", err)
	}
	defer occupied.Close()
	port := occupied.Addr().(*net.TCPAddr).Port

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := fmt.Sprintf("webhook_port: %d\npoll_interval: 0\ninstances: []\n", port)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing the config: %v", err)
	}

	var out syncBuffer
	code := run([]string{"--config", path}, &out, &out)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1 when the webhook port cannot be bound:\n%s", code, out.String())
	}
	log := out.String()
	if !strings.Contains(log, "level=ERROR") || !strings.Contains(log, "webhook port could not be bound") {
		t.Errorf("the refusal must name its cause:\n%s", log)
	}
}

// TestDaemon_Sonarr_WebhookTriggeredSeasonWrite_HappensAndIsScoped is the
// Sonarr half of the webhook write path — the brief asks for both apps, and the
// two are not the same code: a season write is TWO calls, gated separately, and
// it is the one this project's shutdown boundary exists to protect.
//
// The startup scan deliberately writes nothing (the season's episode is below
// its cutoff); the import then completes and the webhook names the series.
func TestDaemon_Sonarr_WebhookTriggeredSeasonWrite_HappensAndIsScoped(t *testing.T) {
	fake := writableSonarrFake(t)
	fake.setEpisodeWanted(100, true) // below cutoff: nothing to write at startup
	h := startDaemon(t, writeDaemonConfig(t, "sonarr", fake.srv.URL, false, "debug", "0", "45s"))
	h.waitReady()

	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("the startup scan should have had nothing to write, got %+v", writes)
	}

	fake.setEpisodeWanted(100, false) // the import completed
	h.post("sonarr-main", `{"eventType":"Download","series":{"id":1,"title":"Write Me"},"episodes":[{"seasonNumber":1}]}`)
	h.clock.Advance(45 * time.Second)
	h.awaitLogCount("webhook debounce expired; evaluating", 1)
	eventually(t, "both halves of the season write to land", func() bool { return len(fake.writes()) == 2 })
	h.stop()

	if fake.seasonMonitored(1, 1) {
		t.Error("the season the webhook named must be unmonitored")
	}
	if fake.episodeMonitored(100) {
		t.Error("and so must its episode: a season write is two calls, and both must have landed")
	}
	out := h.out.String()
	if !strings.Contains(out, "unmonitored=1") || !strings.Contains(out, "onlyId=1") {
		t.Errorf("the write must be counted and the cycle must say it was scoped:\n%s", out)
	}
}

// TestDaemon_Sonarr_DryRun_ZeroWritesAcrossStartupAndAWebhookCycle is §2.1 on
// the Sonarr side, over the catch-all writes() accessor: every non-GET request
// to any path, stubbed or not.
func TestDaemon_Sonarr_DryRun_ZeroWritesAcrossStartupAndAWebhookCycle(t *testing.T) {
	fake := writableSonarrFake(t)
	h := startDaemon(t, writeDaemonConfig(t, "sonarr", fake.srv.URL, true, "debug", "0", "45s"))
	h.waitReady()

	h.post("sonarr-main", `{"eventType":"Download","series":{"id":1},"episodes":[{"seasonNumber":1}]}`)
	h.clock.Advance(45 * time.Second)
	h.awaitLogCount("webhook debounce expired; evaluating", 1)
	h.stop()

	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("dry-run made %d write request(s) across startup and a webhook cycle, want ZERO: %+v", len(writes), writes)
	}
	out := h.out.String()
	if !strings.Contains(out, "wouldUnmonitor=1") || !strings.Contains(out, "withheldWrites=1") {
		t.Errorf("the rehearsal must still report and account for what it would have done:\n%s", out)
	}
}
