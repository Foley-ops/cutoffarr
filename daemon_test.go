package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
// Two distinct races live here, and they are closed in two different ways.
//
// The ORDERING one — the test advancing time before the daemon has armed its
// timer — is closed by construction rather than by synchronization: virtual
// time only ever moves forward, and NewTimer fires IMMEDIATELY for a
// non-positive duration. A daemon that arms late computes its duration from a
// clock that has already passed the deadline, gets a non-positive number, and
// wakes at once.
//
// The MEMORY one is closed by synchronization, because nothing about time
// moving forward makes concurrent access to a bool safe. Every field the
// daemon's goroutine and the test's goroutine both touch is either behind
// c.mu or atomic: `now` and `timers` behind the mutex, and a timer's `done`
// atomic because Stop is called by the DAEMON (on shutdown, and on every queue
// notify that abandons an armed timer) while the TEST is inside Advance, which
// reads and writes the same flag under a lock the daemon never takes.

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	c  chan time.Time
	at time.Time

	// done is atomic and not merely mu-protected: Stop has no access to the
	// clock's mutex, and a daemon calling Stop concurrently with a test
	// calling Advance is the ordinary case, not the exotic one. See
	// TestFakeClock_StopRacesAdvance_WithoutADataRace.
	done atomic.Bool
}

func (t *fakeTimer) C() <-chan time.Time { return t.c }
func (t *fakeTimer) Stop()               { t.done.Store(true) }

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
		t.done.Store(true)
		return t
	}
	c.timers = append(c.timers, t)
	return t
}

// TestFakeClock_StopRacesAdvance_WithoutADataRace is a test about the TEST
// HARNESS, and it earns its place because binding controller note 6 makes
// `-race` this phase's own concurrency gate: a race INSIDE the clock every
// daemon test runs on would make a green -race run a weaker statement than it
// looks, and would surface later as an intermittent CI failure in whichever
// test happened to lose the coin toss.
//
// The pairing is the real one, not a contrivance. The daemon loop calls
// timer.Stop() from its own goroutine (daemon.go, on ctx.Done and on the queue
// notify) every time a webhook arrives while a timer is armed, and the test
// goroutine calls Advance at the same moment — TestDaemon_DryRun_... posts a
// webhook and then advances 45s, which is exactly this.
func TestFakeClock_StopRacesAdvance_WithoutADataRace(t *testing.T) {
	clock := newFakeClock()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		timer := clock.NewTimer(time.Minute)
		wg.Add(2)
		go func() { defer wg.Done(); timer.Stop() }()
		go func() { defer wg.Done(); clock.Advance(time.Second) }()
	}
	wg.Wait()
}

// Advance moves virtual time forward and fires every timer that comes due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	var still []*fakeTimer
	for _, t := range c.timers {
		if t.done.Load() {
			continue
		}
		if !t.at.After(c.now) {
			t.done.Store(true)
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

// writeTwoInstanceDaemonConfig writes a config naming BOTH a radarr and a
// sonarr, which is the shape every real deployment has and which
// writeDaemonConfig cannot express.
//
// It exists because *arr ids are PER INSTANCE: movie 1 of radarr-main and
// series 7 of sonarr-main are unrelated things that happen to be numbers, and
// every routing branch in the daemon's webhook path (grouping expired keys by
// instance, the "an instance the caller did not name is not contacted at all"
// filter, the between-instances shutdown check) is a no-op with one configured
// instance. A regression in any of them is invisible to a single-instance
// suite and writes to the wrong library in production.
func writeTwoInstanceDaemonConfig(t *testing.T, radarrURL, sonarrURL string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := fmt.Sprintf(`
dry_run: true
log_level: debug
poll_interval: 0
webhook_debounce: 45s
instances:
  - name: radarr-main
    type: radarr
    url: %s
    api_key: key1
  - name: sonarr-main
    type: sonarr
    url: %s
    api_key: key2
`, radarrURL, sonarrURL)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing the two-instance daemon test config: %v", err)
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

// startDaemonWithContext runs runDaemon on a context THE TEST already holds,
// rather than run()'s context.Background() plus a signal.
//
// The seam exists for exactly one class of assertion: what a cycle logs when
// shutdown lands INSIDE it. Going through the signal channel cannot express
// that. installShutdownHandler receives the signal on its own goroutine and
// cancels from there, so "the signal was delivered" and "the cycle's context is
// cancelled" are two events with a gap between them, and a test that fires a
// signal partway through a scan is really asserting that the gap was short
// enough — which is a race, not a property. Cancelling the context directly
// from the fake's onRequest hook makes the instant exact, the same way the two
// engines' own mid-evaluation shutdown tests do it.
//
// The context is the CALLER's, and that is what makes those hooks safe to
// install: the cancel func has to exist before fake.onRequest is assigned, and
// the assignment has to happen before this function starts the goroutine that
// serves requests (the fake reads the hook from the serving goroutine, so the
// `go` statement below is the happens-before edge the write needs).
//
// Everything else is startDaemon's wiring: the ephemeral listener, the virtual
// clock, the ready signal. Signals are still wired up so the harness's stop()
// works for a test that wants both.
func startDaemonWithContext(t *testing.T, ctx context.Context, configPath string) *daemonHarness {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("taking an ephemeral port: %v", err)
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("loading the daemon test config: %v", err)
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
	logger := slog.New(slog.NewTextHandler(h.out, &slog.HandlerOptions{Level: slogLevel(cfg.LogLevel)}))
	go func() {
		h.exitCode <- runDaemon(ctx, logger, *cfg, daemonOptions{
			clock:             h.clock,
			listener:          listener,
			signals:           h.signals,
			forceExit:         func(int) { panic("forceExit must not be reached in this test") },
			onStartupScanDone: func() { close(h.ready) },
		})
	}()
	t.Cleanup(func() {
		if h.exited {
			return
		}
		h.exited = true
		select {
		case <-h.exitCode:
		case <-time.After(10 * time.Second):
			t.Errorf("the daemon did not exit within 10s of the test's context being cancelled:\n%s", h.out.String())
		}
	})
	return h
}

// awaitExit waits for the daemon started by startDaemonWithCancel to return,
// for a test whose shutdown came from the context rather than from stop().
func (h *daemonHarness) awaitExit() int {
	h.t.Helper()
	h.exited = true
	select {
	case code := <-h.exitCode:
		return code
	case <-time.After(10 * time.Second):
		h.t.Fatalf("the daemon did not exit within 10s:\n%s", h.out.String())
		return -1
	}
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

// TestDaemon_TwoInstances_EachWebhookCycleContactsOnlyItsOwnInstance is the
// per-instance routing every other daemon test is structurally unable to
// exercise, because a one-instance config makes the grouping a no-op.
//
// The consequence it guards is the worst one in this program's vocabulary: *arr
// ids are per-instance, so a regression that unioned the coalesced id set
// across instances, or that dropped scanCycle.instanceName, would take the
// SONARR event's id 7 into the RADARR cycle and unmonitor whatever film happens
// to be movie 7 over there — a correct-looking write to the wrong library. Two
// deliberately DIFFERENT ids are what makes a crossed set visible: with the
// same id on both sides a union would be indistinguishable from correct
// routing.
func TestDaemon_TwoInstances_EachWebhookCycleContactsOnlyItsOwnInstance(t *testing.T) {
	radarr := newStatefulRadarrFake(t, []*statefulRadarrMovie{
		wouldUnmonitorStatefulMovie(1, "The Radarr One"),
		wouldUnmonitorStatefulMovie(7, "Movie Seven — the film a crossed id set would unmonitor"),
	})
	sonarr := newStatefulSonarrFake(t,
		[]*statefulSonarrSeries{
			{id: 7, title: "The Sonarr One", monitored: true, profileID: 1, tags: []int{},
				seasons: []statefulSonarrSeason{{number: 1, monitored: true, episodeFileCount: 1, totalEpisodeCount: 1}}},
		},
		[]*statefulSonarrEpisode{
			{id: 700, seriesID: 7, seasonNumber: 1, episodeNumber: 1, monitored: true, hasFile: true, airDateUtc: pastAirDate, episodeFileID: 900},
		},
		[]*statefulSonarrEpisodeFile{{id: 900, seasonNumber: 1, customFormatScore: 200, qualityCutoffNotMet: false}},
	)

	// Which fake was contacted, in order, recorded only once the startup scan
	// (which legitimately contacts both) is behind us. Safe to arm after
	// waitReady without further synchronization only because it is armed
	// BEFORE: the dispatcher is the same single goroutine that ran the scan, so
	// nothing is in flight at that instant either way.
	var mu sync.Mutex
	recording := false
	var contacted []string
	recorder := func(name string) func(string, string) {
		return func(string, string) {
			mu.Lock()
			defer mu.Unlock()
			if recording {
				contacted = append(contacted, name)
			}
		}
	}
	radarr.onRequest = recorder("radarr-main")
	sonarr.onRequest = recorder("sonarr-main")

	h := startDaemon(t, writeTwoInstanceDaemonConfig(t, radarr.srv.URL, sonarr.srv.URL))
	h.waitReady()
	mu.Lock()
	recording = true
	mu.Unlock()
	mark := h.mark()

	h.post("radarr-main", downloadMoviePayload)
	h.post("sonarr-main", `{"eventType":"Download","series":{"id":7},"episodes":[{"seasonNumber":1}]}`)
	eventually(t, "both events to be queued", func() bool {
		return strings.Count(h.out.String(), "webhook queued") == 2
	})

	// ONE advance past the debounce: both keys come due together, and the loop
	// must still run them as two separate, instance-scoped evaluations.
	h.clock.Advance(45 * time.Second)
	h.awaitLogCount("webhook debounce expired; evaluating", 2)
	time.Sleep(20 * time.Millisecond)
	h.stop()

	cycle := h.since(mark)

	// 1. Two evaluations, each naming exactly one instance and exactly its own
	//    id. A crossed id set shows up here as scopeIds=1,7.
	for _, want := range []string{
		`msg="webhook debounce expired; evaluating" instance=radarr-main items=1 ids=1`,
		`msg="webhook debounce expired; evaluating" instance=sonarr-main items=1 ids=7`,
	} {
		if !strings.Contains(cycle, want) {
			t.Errorf("expected an evaluation line %q; each instance's cycle must carry only its OWN ids:\n%s", want, cycle)
		}
	}
	if strings.Contains(cycle, "scopeIds=") {
		t.Errorf("two events for two DIFFERENT instances are two one-item scopes, never one coalesced set:\n%s", cycle)
	}

	// 2. One summary per instance, each scoped to its own id.
	for _, want := range []string{"radarr decision summary", "sonarr decision summary"} {
		if n := strings.Count(cycle, want); n != 1 {
			t.Errorf("expected exactly one %q, got %d:\n%s", want, n, cycle)
		}
	}
	for _, line := range strings.Split(cycle, "\n") {
		if strings.Contains(line, "radarr decision summary") && !strings.Contains(line, "onlyId=1") {
			t.Errorf("the radarr summary must be scoped to the movie ITS webhook named:\n%s", line)
		}
		if strings.Contains(line, "sonarr decision summary") && !strings.Contains(line, "onlyId=7") {
			t.Errorf("the sonarr summary must be scoped to the series ITS webhook named:\n%s", line)
		}
	}

	// 3. Nothing was looked for in the wrong library. A crossed id set would
	//    also surface here, as the engine's "named an id this library does not
	//    have" warning.
	if strings.Contains(cycle, "not found in this instance's library") {
		t.Errorf("an id was carried into the wrong instance's evaluation:\n%s", cycle)
	}

	// 4. The strongest form, at the transport: during each cycle the OTHER
	//    *arr received no request at all. "Not written to" is a weaker claim
	//    than "not contacted", and the plan's --instance rule is the latter.
	mu.Lock()
	seq := append([]string(nil), contacted...)
	mu.Unlock()
	if len(seq) == 0 {
		t.Fatalf("no requests were recorded after the startup scan; the webhook cycles did not run:\n%s", cycle)
	}
	transitions := 0
	for i := 1; i < len(seq); i++ {
		if seq[i] != seq[i-1] {
			transitions++
		}
	}
	if transitions != 1 || seq[0] != "radarr-main" || seq[len(seq)-1] != "sonarr-main" {
		t.Errorf("each webhook cycle must contact exactly one instance, radarr's first (expired keys are grouped in instance order); got %v", seq)
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

	if !assertIdleCycleInfoIsWithinTheBudget(t, cycle2) {
		t.Errorf("the summary is what an idle cycle DOES print — it is how a human sees the daemon is alive and idle:\n%s", cycle2)
	}

	// And the demotion is a demotion, not a deletion: everything the cycle
	// stopped saying at INFO is still there at DEBUG for anyone who turns it
	// on. Proven by the startup scan, which said all of it at INFO.
	if !strings.Contains(startup, "msg=would-unmonitor") || !strings.Contains(startup, "cross-check season") {
		t.Errorf("the startup scan must still report in full at INFO:\n%s", startup)
	}
}

// idleCycleAllowedInfo is what an idle daemon cycle is permitted to say at
// INFO, and the property it encodes is one sentence: NOTHING THAT SCALES WITH
// THE SIZE OF THE LIBRARY. The daemon repeats this cycle forever, so a per-item
// line printed here is printed a thousand times a day and buries the writes and
// warnings the log exists for; a line printed at most once per INSTANCE per
// cycle cannot.
//
// It is expressed as an allowlist rather than a list of forbidden messages
// deliberately: a forbidden-substring test would pass the day someone adds a
// thirteenth per-item INFO line nobody thought to list. The cost is that every
// legitimate addition has to be argued for here, which is the point — each of
// the entries below is per-instance, and each says something no other line
// says:
//
//   - the sweep's two bookends and the per-instance decision summary: the cycle
//     happened at all, and what it decided.
//   - "exclusion tag not defined in this instance": §2.5's exclusion tag is the
//     user's ONLY opt-out from being unmonitored, and this line says that
//     opt-out is currently inert here. A tag renamed in the *arr after startup
//     disables rule 4 from that moment on; demoted, the only notice of it
//     scrolled out of `docker logs` days ago (binding ruling R6).
//   - "writes withheld for this instance": the write gate is shut for this
//     instance, which is the difference between "nothing needed writing" and
//     "something did and was refused". This is the INFO variant, reached only
//     when nothing was pending (with pending writes the same line is a WARN and
//     was never in question). It is reachable on a genuinely idle cycle — an
//     inconclusive cross-check shuts the gate whether or not anything was
//     waiting — which is what
//     TestDaemon_IdleCycleWithAnInconclusiveCrossCheck_StaysWithinTheNoiseBudget
//     exists to construct.
//   - the per-instance decision summary of EITHER engine: Phase 10's reverse
//     scan is reported through the same line, so a radarr instance's summary is
//     now as much a part of an idle cycle as a sonarr's.
var idleCycleAllowedInfo = []string{
	"reconciliation sweep beginning",
	"reconciliation sweep complete",
	"sonarr decision summary",
	"radarr decision summary",
	"exclusion tag not defined in this instance",
	"writes withheld for this instance",
	// Phase 11: msg="file report" is the same per-instance, always-INFO
	// summary shape the decision-summary lines already are — see
	// logFileReportSummary. Per-finding lines (msg="file-report finding")
	// are demoted to debug on sweeps exactly like every other per-item line
	// here, so they never reach this allowlist at all.
	"file report",
}

// assertIdleCycleInfoIsWithinTheBudget fails the test for every INFO line of
// cycle that is not in idleCycleAllowedInfo, and reports whether the
// per-instance decision summary was among them.
func assertIdleCycleInfoIsWithinTheBudget(t *testing.T, cycle string) (sawSummary bool) {
	t.Helper()
	for _, line := range strings.Split(cycle, "\n") {
		if !strings.Contains(line, "level=INFO") {
			continue
		}
		matched := ""
		for _, allowed := range idleCycleAllowedInfo {
			if strings.Contains(line, allowed) {
				matched = allowed
			}
		}
		if matched == "" {
			t.Errorf("an idle reconciliation cycle printed an INFO line that is not on the noise budget's allowlist:\n%s\n(whole cycle:\n%s)", line, cycle)
		}
		if strings.HasSuffix(matched, "decision summary") {
			sawSummary = true
		}
	}
	return sawSummary
}

// unverifiableSonarrFake is one series whose single season is complete on disk
// and aired, and for which /episodefile returns NOTHING — a real *arr state (a
// file gone from disk with stale season statistics), and the smallest way to
// produce the combination the allowlist has to survive: a cycle with no pending
// writes AND an inconclusive cross-check.
//
// Nothing is decided as would-unmonitor (the season is skipped for the file
// count mismatch), so the cycle is idle; the season is still SAMPLED by the
// cross-check on the skip side, has no comparable episodeFile.qualityCutoffNotMet,
// and therefore lands the whole verdict on inconclusive.
func unverifiableSonarrFake(t *testing.T) *statefulSonarrFake {
	t.Helper()
	return newStatefulSonarrFake(t,
		[]*statefulSonarrSeries{
			{id: 1, title: "No Files", monitored: true, profileID: 1, tags: []int{},
				seasons: []statefulSonarrSeason{{number: 1, monitored: true, episodeFileCount: 1, totalEpisodeCount: 1}}},
		},
		[]*statefulSonarrEpisode{
			{id: 100, seriesID: 1, seasonNumber: 1, episodeNumber: 1, monitored: true, hasFile: true, airDateUtc: pastAirDate, episodeFileID: 500},
		},
		[]*statefulSonarrEpisodeFile{},
	)
}

// TestDaemon_IdleCycleWithAnInconclusiveCrossCheck_StaysWithinTheNoiseBudget is
// the sub-case the noise-budget test could not reach on its own, and the reason
// it matters is that the allowlist is a claim about the DAEMON, not about one
// fixture.
//
// TestDaemon_SecondCycleWithNothingChanged runs against writableSonarrFake,
// whose cross-check always passes, so the write gate is never shut and the
// per-instance "writes withheld for this instance" line never appears. It is
// undemoted (the write passes log it through the engine's own logger), it is
// INFO whenever nothing was pending, and an inconclusive cross-check on an idle
// cycle is an ordinary, reachable configuration — so an allowlist that had
// never seen it was encoding a rule the daemon does not follow.
//
// Unlike its sibling this cycle is NOT warning-free, and that is inherent
// rather than incidental: an inconclusive verdict always carries the WARN
// saying pass or fail could not be determined. The noise budget is about INFO.
func TestDaemon_IdleCycleWithAnInconclusiveCrossCheck_StaysWithinTheNoiseBudget(t *testing.T) {
	fake := unverifiableSonarrFake(t)
	h := startDaemon(t, writeDaemonConfig(t, "sonarr", fake.srv.URL, true, "info", "1h", "45s"))
	h.waitReady()
	eventually(t, "the reconciliation schedule to be announced", func() bool {
		return strings.Contains(h.out.String(), "reconciliation sweep scheduled")
	})

	mark := h.mark()
	h.clock.Advance(time.Hour)
	h.awaitLogCount("reconciliation sweep complete", 1)
	cycle2 := h.since(mark)
	h.stop()

	if !strings.Contains(cycle2, "crossCheck=inconclusive") {
		t.Fatalf("this test is only meaningful if the cycle's cross-check really is inconclusive:\n%s", cycle2)
	}
	gate := ""
	for _, line := range strings.Split(cycle2, "\n") {
		if strings.Contains(line, "writes withheld for this instance") {
			gate = line
		}
	}
	if gate == "" {
		t.Fatalf("a shut write gate must say so on every cycle: 'nothing was written' and 'nothing needed writing' are different facts:\n%s", cycle2)
	}
	if !strings.Contains(gate, "level=INFO") || !strings.Contains(gate, "withheldWrites=0") {
		t.Fatalf("the case this pins is the INFO one — the gate shut with nothing pending; with pending writes the same line is a WARN:\n%s", gate)
	}

	if !assertIdleCycleInfoIsWithinTheBudget(t, cycle2) {
		t.Errorf("the summary is what an idle cycle DOES print:\n%s", cycle2)
	}
	if len(fake.writes()) != 0 {
		t.Errorf("a shut gate writes nothing, got %+v", fake.writes())
	}
}

// TestDaemon_ExclusionTagNotDefined_IsSaidOnEveryCycleNotOnlyTheFirst is the
// one INFO line the idle-cycle noise budget must not demote, and the reason it
// is an exception rather than an oversight.
//
// §2.5's exclusion tag is the user's ONLY opt-out from being unmonitored. This
// line says that opt-out is inert for an instance — the tag is not defined
// there, so rule 4 excludes nothing and every item is a candidate. That is a
// statement about this program's safety posture, not about the library, and it
// does not scale with the library the way the per-item report lines the noise
// budget targets do: it is at most one line per instance per cycle.
//
// The failure it prevents is quiet and open-ended. A tag renamed or deleted in
// the *arr mid-run silently disables rule 4 from that moment on, and at the
// default log_level: info the only trace was the startup scan's line — which
// scrolls out of `docker logs` on a daemon that has been up for a week, leaving
// the operator's exclusions inert and the log saying nothing at all about it.
func TestDaemon_ExclusionTagNotDefined_IsSaidOnEveryCycleNotOnlyTheFirst(t *testing.T) {
	// writableSonarrFake serves an empty /tag array, so the configured
	// exclusion tag resolves to "not defined in this instance".
	fake := writableSonarrFake(t)
	h := startDaemon(t, writeDaemonConfig(t, "sonarr", fake.srv.URL, true, "info", "1h", "45s"))
	h.waitReady()
	eventually(t, "the reconciliation schedule to be announced", func() bool {
		return strings.Contains(h.out.String(), "reconciliation sweep scheduled")
	})

	const line = "exclusion tag not defined in this instance"
	if !strings.Contains(h.out.String(), line) {
		t.Fatalf("the startup scan must say the exclusion tag is inert:\n%s", h.out.String())
	}

	mark := h.mark()
	h.clock.Advance(time.Hour)
	h.awaitLogCount("reconciliation sweep complete", 1)
	cycle2 := h.since(mark)
	h.stop()

	if !strings.Contains(cycle2, line) {
		t.Errorf("an idle reconciliation cycle stopped saying that §2.5's only opt-out is inert for this instance; the noise budget demotes per-item report lines, never a safety-posture statement:\n%s", cycle2)
	}
	for _, l := range strings.Split(cycle2, "\n") {
		if strings.Contains(l, line) && !strings.Contains(l, "level=INFO") {
			t.Errorf("the line is present but below INFO, which at the default log level is the same as absent:\n%s", l)
		}
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

// --- the bookends of an abandoned cycle -------------------------------------
//
// These two pin the same rule at the two places a full-library cycle can be cut
// short: a pass that STOPPED EARLY must never print the line a pass that
// finished prints.
//
// The failure is small to describe and bad to meet. SIGTERM lands mid-scan, the
// engine correctly abandons the cycle and says so — "abandoning this instance's
// cycle mid-evaluation" — and the very next line is "startup scan complete", or
// "reconciliation sweep complete, nextSweep=...". An operator reading a log at
// the exact moment they are most likely to be reading one (they just stopped
// the thing) sees a completion claim over a cycle that covered part of the
// library, immediately after the line that says it did not. The sweep's version
// also re-armed the schedule and announced a next sweep, from a daemon that was
// on its way out and would never run one.

// TestDaemon_StartupScanAbandonedOnShutdown_DoesNotClaimToHaveCompleted
// cancels partway through the FIRST series' evaluation, so the startup scan
// really does return having seen part of the library.
func TestDaemon_StartupScanAbandonedOnShutdown_DoesNotClaimToHaveCompleted(t *testing.T) {
	fake := twoSeriesShutdownSonarrFake(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var once sync.Once
	fake.onRequest = func(method, path string) {
		if method == http.MethodGet && path == "/api/v3/episode" {
			once.Do(cancel) // partway through the FIRST series' evaluation
		}
	}
	h := startDaemonWithContext(t, ctx, writeDaemonConfig(t, "sonarr", fake.srv.URL, false, "info", "1h", "45s"))

	if code := h.awaitExit(); code != 0 {
		t.Fatalf("exit code = %d, want 0: an abandoned cycle is a clean stop, not a failure:\n%s", code, h.out.String())
	}
	out := h.out.String()

	if !strings.Contains(out, "abandoning this instance's cycle mid-evaluation") {
		t.Fatalf("this test is only meaningful if the scan really was cut short:\n%s", out)
	}
	if strings.Contains(out, "startup scan complete") {
		t.Errorf("a scan that stopped partway through the library claimed to have completed:\n%s", out)
	}
	if !strings.Contains(out, "startup scan abandoned on shutdown") {
		t.Errorf("it must say what happened instead; silence after the abandonment line reads as a scan that never ended:\n%s", out)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("an abandoned cycle writes nothing, got %+v", writes)
	}
}

// TestDaemon_ReconciliationSweepAbandonedOnShutdown_DoesNotCompleteOrRearm is
// the same rule on the sweep, plus the half only the sweep has: the completion
// line re-arms nextReconcile and announces the next sweep. A daemon shutting
// down must not schedule anything.
func TestDaemon_ReconciliationSweepAbandonedOnShutdown_DoesNotCompleteOrRearm(t *testing.T) {
	fake := twoSeriesShutdownSonarrFake(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The hook is installed before the daemon exists (see
	// startDaemonWithContext) and armed only once the startup scan is done: the
	// scan hits the same endpoint, and this test is about the SWEEP.
	var armed atomic.Bool
	var once sync.Once
	fake.onRequest = func(method, path string) {
		if armed.Load() && method == http.MethodGet && path == "/api/v3/episode" {
			once.Do(cancel)
		}
	}
	// dry_run: the startup scan must complete normally, so nothing may be
	// written before the sweep this test is about.
	h := startDaemonWithContext(t, ctx, writeDaemonConfig(t, "sonarr", fake.srv.URL, true, "info", "1h", "45s"))
	h.waitReady()
	eventually(t, "the reconciliation schedule to be announced", func() bool {
		return strings.Contains(h.out.String(), "reconciliation sweep scheduled")
	})
	if !strings.Contains(h.out.String(), "startup scan complete") {
		t.Fatalf("the startup scan ran to completion and must say so:\n%s", h.out.String())
	}

	mark := h.mark()
	armed.Store(true)
	h.clock.Advance(time.Hour)

	if code := h.awaitExit(); code != 0 {
		t.Fatalf("exit code = %d, want 0:\n%s", code, h.out.String())
	}
	sweep := h.since(mark)

	if !strings.Contains(sweep, "reconciliation sweep beginning") {
		t.Fatalf("the sweep must have started for this test to mean anything:\n%s", sweep)
	}
	if !strings.Contains(sweep, "abandoning this instance's cycle mid-evaluation") {
		t.Fatalf("this test is only meaningful if the sweep really was cut short:\n%s", sweep)
	}
	if strings.Contains(sweep, "reconciliation sweep complete") {
		t.Errorf("a sweep that stopped partway through the library claimed to have completed:\n%s", sweep)
	}
	if !strings.Contains(sweep, "reconciliation sweep abandoned on shutdown") {
		t.Errorf("it must say what happened instead:\n%s", sweep)
	}
	if strings.Contains(sweep, "nextSweep=") {
		t.Errorf("an exiting daemon re-armed the schedule and announced a sweep it will never run:\n%s", sweep)
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

// TestDaemon_Sonarr_WebhookNamingOneSeason_WritesThatSeasonAndNeverItsSiblings
// is the plan's own sentence, machine-verified: "a webhook event evaluates only
// that movie (Radarr) or that series' AFFECTED SEASON (Sonarr)".
//
// The scenario is the one that makes it matter. A show has two seasons that are
// both, right now, eligible to be unmonitored. An episode of season 2 is
// imported, so Sonarr fires a webhook naming season 2 — and 45 seconds later,
// unattended, with its per-item report lines demoted to DEBUG, this daemon must
// not quietly unmonitor season 1 as well. Season 1 IS eligible and the
// reconciliation sweep will get to it; the point is that a season-2 import is
// not the event that authorizes a season-1 write, and this project's whole
// discipline is writes no wider than what was asked for.
//
// Both seasons are held below their cutoff during the startup scan so that the
// only write in this test is the one the webhook triggered.
func TestDaemon_Sonarr_WebhookNamingOneSeason_WritesThatSeasonAndNeverItsSiblings(t *testing.T) {
	fake := twoWritableSeasonsSonarrFake(t)
	for _, ep := range []int{100, 200, 201} {
		fake.setEpisodeWanted(ep, true) // below cutoff: the startup scan writes nothing
	}
	h := startDaemon(t, writeDaemonConfig(t, "sonarr", fake.srv.URL, false, "debug", "0", "45s"))
	h.waitReady()

	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("the startup scan should have had nothing to write, got %+v", writes)
	}

	// The import completes. BOTH seasons are now eligible; only season 2 was
	// imported, and only season 2 is named by the event.
	for _, ep := range []int{100, 200, 201} {
		fake.setEpisodeWanted(ep, false)
	}
	h.post("sonarr-main", `{"eventType":"Download","series":{"id":1,"title":"Write Both"},"episodes":[{"seasonNumber":2,"episodeNumber":1}]}`)
	h.clock.Advance(45 * time.Second)
	h.awaitLogCount("webhook debounce expired; evaluating", 1)
	eventually(t, "season 2's write to land", func() bool { return len(fake.writes()) == 2 })
	// Room for a wider write to appear before anything is asserted absent.
	h.clock.Advance(10 * time.Minute)
	time.Sleep(20 * time.Millisecond)
	h.stop()

	if fake.seasonMonitored(1, 2) {
		t.Error("season 2 — the season the webhook named — must be unmonitored")
	}
	if fake.episodeMonitored(200) || fake.episodeMonitored(201) {
		t.Error("season 2's episodes must be unmonitored with it: a season write is two calls and both must have landed")
	}
	if !fake.seasonMonitored(1, 1) {
		t.Error("season 1 was unmonitored by an import that named season 2; a webhook's write scope is the AFFECTED season, not the whole series")
	}
	if !fake.episodeMonitored(100) {
		t.Error("season 1's episode was unmonitored by a season 2 import")
	}
	if writes := fake.writes(); len(writes) != 2 {
		t.Errorf("expected exactly the 2 calls of ONE season write, got %d: %+v", len(writes), writes)
	}
	out := h.out.String()
	if !strings.Contains(out, "unmonitored=1") {
		t.Errorf("exactly one season must be counted as written:\n%s", out)
	}
	if !strings.Contains(out, "scopeSeasons=1:2") {
		t.Errorf("the cycle must SAY what it narrowed to, or a human cannot tell a scoped cycle from a whole-series one:\n%s", out)
	}
}

// TestDaemon_Sonarr_WebhookNamingSeasonThree_WritesOnlyThatSeason is the same
// sentence as the test above, asked of a library where it can actually fail.
//
// The write scope is a set of (seriesID, seasonNumber) PAIRS, and the two ways
// that can quietly degrade both survive a two-season fixture: a scope that
// resolved to "the series" would write every eligible season, and a scope that
// resolved to "up to the named season" would write the ones before it. Three
// eligible seasons and a payload naming the LAST of them separates all three
// behaviors — only the pair-scoped one leaves seasons 1 and 2 monitored.
//
// The payload is the real shape: Sonarr's On Import for S03E05 carries that one
// episode record, and the write it authorizes is season 3's alone.
func TestDaemon_Sonarr_WebhookNamingSeasonThree_WritesOnlyThatSeason(t *testing.T) {
	fake := threeWritableSeasonsSonarrFake(t)
	eligible := []int{100, 200, 300, 305}
	for _, ep := range eligible {
		fake.setEpisodeWanted(ep, true) // below cutoff: the startup scan writes nothing
	}
	h := startDaemon(t, writeDaemonConfig(t, "sonarr", fake.srv.URL, false, "debug", "0", "45s"))
	h.waitReady()

	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("the startup scan should have had nothing to write, got %+v", writes)
	}

	// The import completes. All three seasons are now eligible; the event names
	// season 3 and nothing else.
	for _, ep := range eligible {
		fake.setEpisodeWanted(ep, false)
	}
	h.post("sonarr-main", `{"eventType":"Download","series":{"id":1,"title":"Write Three"},"episodes":[{"seasonNumber":3,"episodeNumber":5}]}`)
	h.clock.Advance(45 * time.Second)
	h.awaitLogCount("webhook debounce expired; evaluating", 1)
	eventually(t, "season 3's write to land", func() bool { return len(fake.writes()) == 2 })
	// Room for a wider write to appear before anything is asserted absent.
	h.clock.Advance(10 * time.Minute)
	time.Sleep(20 * time.Millisecond)
	h.stop()

	if fake.seasonMonitored(1, 3) {
		t.Error("season 3 — the season the webhook named — must be unmonitored")
	}
	if fake.episodeMonitored(300) || fake.episodeMonitored(305) {
		t.Error("season 3's episodes must be unmonitored with it: a season write is two calls and both must have landed")
	}
	for _, season := range []int{1, 2} {
		if !fake.seasonMonitored(1, season) {
			t.Errorf("season %d was unmonitored by an import that named season 3; the write scope is the (series, season) pair the payload named, not the series and not everything up to it", season)
		}
	}
	for _, ep := range []int{100, 200} {
		if !fake.episodeMonitored(ep) {
			t.Errorf("episode %d, of a season the payload never named, was unmonitored", ep)
		}
	}
	if writes := fake.writes(); len(writes) != 2 {
		t.Errorf("expected exactly the 2 calls of ONE season write, got %d: %+v", len(writes), writes)
	}
	out := h.out.String()
	if !strings.Contains(out, "unmonitored=1") {
		t.Errorf("exactly one season must be counted as written:\n%s", out)
	}
	if !strings.Contains(out, "scopeSeasons=1:3") {
		t.Errorf("the cycle must SAY which (series, season) pair it narrowed to:\n%s", out)
	}
}

// TestDaemon_Sonarr_BurstNamingSeasonsOneAndThree_WritesExactlyThoseTwo is the
// accumulation half: a debounce burst collects the seasons its events named into
// ONE evaluation, and what it collects is a SET.
//
// Two seasons of one show finish importing inside one debounce window — the
// ordinary shape of a backfill — and the events name seasons 1 and 3. Season 2
// is equally eligible and was named by nothing, so it must still be monitored
// when the cycle ends. The repeated season-1 event is in the burst because the
// accumulated scope must dedupe rather than write its season twice.
func TestDaemon_Sonarr_BurstNamingSeasonsOneAndThree_WritesExactlyThoseTwo(t *testing.T) {
	fake := threeWritableSeasonsSonarrFake(t)
	eligible := []int{100, 200, 300, 305}
	for _, ep := range eligible {
		fake.setEpisodeWanted(ep, true)
	}
	h := startDaemon(t, writeDaemonConfig(t, "sonarr", fake.srv.URL, false, "debug", "0", "45s"))
	h.waitReady()

	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("the startup scan should have had nothing to write, got %+v", writes)
	}
	for _, ep := range eligible {
		fake.setEpisodeWanted(ep, false)
	}

	h.post("sonarr-main", `{"eventType":"Download","series":{"id":1},"episodes":[{"seasonNumber":1,"episodeNumber":1}]}`)
	h.post("sonarr-main", `{"eventType":"Download","series":{"id":1},"episodes":[{"seasonNumber":3,"episodeNumber":5}]}`)
	h.post("sonarr-main", `{"eventType":"Download","series":{"id":1},"episodes":[{"seasonNumber":1,"episodeNumber":1}]}`)
	eventually(t, "all three events to be queued as one key", func() bool {
		return strings.Count(h.out.String(), "webhook queued") == 3
	})

	h.clock.Advance(45 * time.Second)
	h.awaitLogCount("webhook debounce expired; evaluating", 1)
	eventually(t, "both named seasons' writes to land", func() bool { return len(fake.writes()) == 4 })
	h.clock.Advance(10 * time.Minute)
	time.Sleep(20 * time.Millisecond)
	h.stop()

	for _, season := range []int{1, 3} {
		if fake.seasonMonitored(1, season) {
			t.Errorf("season %d was named by the burst and must be unmonitored", season)
		}
	}
	if !fake.seasonMonitored(1, 2) {
		t.Error("season 2 sits BETWEEN the two the burst named and was named by neither; an accumulated scope is a set of seasons, never the span they cover")
	}
	if !fake.episodeMonitored(200) {
		t.Error("season 2's episode was unmonitored by a burst that never named season 2")
	}
	if !fake.seasonMonitored(1, 4) {
		t.Error("season 4 is still airing; the decision rules exclude it, whatever the scope says")
	}
	if writes := fake.writes(); len(writes) != 4 {
		t.Errorf("expected exactly the 4 calls of TWO season writes, got %d: %+v", len(writes), writes)
	}
	out := h.out.String()
	if !strings.Contains(out, "unmonitored=2") {
		t.Errorf("exactly two seasons must be counted as written:\n%s", out)
	}
	if !strings.Contains(out, "scopeSeasons=1:1,3") {
		t.Errorf("the accumulated scope must be logged as the deduplicated set it is:\n%s", out)
	}
}

// TestDaemon_Sonarr_WebhookWithNoEpisodes_EvaluatesEverySeasonOfTheSeries is the
// other half of binding resolution 2: "absent/empty episodes array → evaluate
// all seasons of the series". Not every event that names a series can say which
// season it touched, and the safe direction there is the whole series — the
// same set --only-id acts on.
func TestDaemon_Sonarr_WebhookWithNoEpisodes_EvaluatesEverySeasonOfTheSeries(t *testing.T) {
	fake := twoWritableSeasonsSonarrFake(t)
	for _, ep := range []int{100, 200, 201} {
		fake.setEpisodeWanted(ep, true)
	}
	h := startDaemon(t, writeDaemonConfig(t, "sonarr", fake.srv.URL, false, "debug", "0", "45s"))
	h.waitReady()
	for _, ep := range []int{100, 200, 201} {
		fake.setEpisodeWanted(ep, false)
	}

	h.post("sonarr-main", `{"eventType":"Download","series":{"id":1,"title":"Write Both"}}`)
	h.clock.Advance(45 * time.Second)
	h.awaitLogCount("webhook debounce expired; evaluating", 1)
	eventually(t, "both seasons' writes to land", func() bool { return len(fake.writes()) == 4 })
	h.stop()

	if fake.seasonMonitored(1, 1) || fake.seasonMonitored(1, 2) {
		t.Error("an event that names no season evaluates every season of the series, exactly as --only-id does")
	}
	if !fake.seasonMonitored(1, 3) {
		t.Error("season 3 is still airing; the decision rules exclude it, whatever the scope says")
	}
	out := h.out.String()
	if strings.Contains(out, "scopeSeasons=") {
		t.Errorf("an unnarrowed series scope must not claim a season narrowing:\n%s", out)
	}
}

// TestDaemon_Sonarr_ABurstThatNamesASeasonAndThenNothing_WidensToTheWholeSeries
// is the coalescing edge the season narrowing creates, and the safe direction
// is the wide one.
//
// A season-pack import fires many events for one series; if one of them carries
// its episodes and another does not, the two claims are "season 2" and "every
// season". Accumulating only the named numbers would let the second event —
// which said the MOST — narrow the scope, and a write set must never be
// narrowed by an event that made no claim about seasons at all.
func TestDaemon_Sonarr_ABurstThatNamesASeasonAndThenNothing_WidensToTheWholeSeries(t *testing.T) {
	fake := twoWritableSeasonsSonarrFake(t)
	for _, ep := range []int{100, 200, 201} {
		fake.setEpisodeWanted(ep, true)
	}
	h := startDaemon(t, writeDaemonConfig(t, "sonarr", fake.srv.URL, false, "debug", "0", "45s"))
	h.waitReady()
	for _, ep := range []int{100, 200, 201} {
		fake.setEpisodeWanted(ep, false)
	}

	h.post("sonarr-main", `{"eventType":"Download","series":{"id":1},"episodes":[{"seasonNumber":2}]}`)
	h.post("sonarr-main", `{"eventType":"Download","series":{"id":1}}`) // says nothing about seasons
	h.post("sonarr-main", `{"eventType":"Download","series":{"id":1},"episodes":[{"seasonNumber":2}]}`)
	eventually(t, "all three events to be queued as one key", func() bool {
		return strings.Count(h.out.String(), "webhook queued") == 3
	})

	h.clock.Advance(45 * time.Second)
	h.awaitLogCount("webhook debounce expired; evaluating", 1)
	eventually(t, "both seasons' writes to land", func() bool { return len(fake.writes()) == 4 })
	h.stop()

	if fake.seasonMonitored(1, 1) {
		t.Error("an event carrying no episodes claims the whole series, and a later season-2 event must not narrow it back")
	}
	if fake.seasonMonitored(1, 2) {
		t.Error("season 2 must be written too")
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
