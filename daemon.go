package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// daemon.go holds the long-running side of cutoffarr: the reconciliation cycle
// itself (shared verbatim with --once), and — in webhook.go's company — the
// webhook listener, the per-item debounce queue, and the loop that ties them
// together.
//
// The single most important property of this file is what it does NOT contain:
// any decision logic, any second copy of the pipeline, any cache. Every cycle
// the daemon runs is the SAME pipeline a --once run performs, re-fetched from
// scratch (binding controller note 3: "no caching of decisions, payloads, or
// cross-check verdicts across debounce/poll iterations — every write is still
// preceded by its own fresh GETs and full pre-write re-verification"). What the
// daemon adds is only WHEN a cycle runs and WHAT it is scoped to.

// scanCycle is the input to one pass over the configured instances.
//
// It is the same shape whether the pass came from --once, from the daemon's
// startup scan, from the reconciliation ticker, or from an expired webhook
// debounce; only the field values differ. That is deliberate — it is what makes
// "the daemon reuses the run-once pipeline verbatim" a property of the code
// rather than a promise in a comment.
type scanCycle struct {
	// instanceName limits the pass to one configured instance. Empty means
	// every configured instance, in config order. A webhook cycle always names
	// one (the instance whose endpoint received the event); a reconciliation
	// sweep never does.
	instanceName string

	// samples is the --samples debugging aid, Radarr-only and --once-only in
	// practice: nothing in daemon mode ever sets it.
	samples []string

	// scope narrows what is REPORTED and WRITTEN within each in-scope instance,
	// and carries the level of the per-item report lines. See scope.go.
	scope evalScope

	// dryRun is cfg.DryRun after --dry-run's force-on override. It is threaded
	// all the way down to the write sites rather than consulted here, because
	// §2.1 requires the flag to be checked immediately before each HTTP write
	// call rather than once at startup.
	dryRun bool

	// reverse says whether this cycle runs the Phase 10 reverse scan, and
	// whether that scan may write (cfg.ReverseScanRemonitor).
	//
	// It is a per-cycle field rather than a config lookup inside the engines
	// because it is a SCHEDULING decision: the reverse scan belongs to
	// full-library cycles — the startup scan, the reconciliation sweep, a
	// --once run — and never to a WEBHOOK cycle, which fires unattended on every
	// import, where a whole-library second pass would be both a surprise and a
	// cost the debounce was sized to prevent. Every cycle this file builds is
	// one of those two, so nothing here ever asks for the third case: a scoped
	// --once --only-id run, which gets a reverse pass narrowed to the one item
	// it names when (and only when) the write switch is on (scopedReverseOptions
	// — the acceptance instrument, built in main.go and unreachable from daemon
	// mode, where those flags are refused). The zero value is "no reverse pass",
	// so a cycle has to ask.
	reverse reverseOptions
}

// runScanCycle performs one full pass: for every in-scope instance, the
// read-only connectivity check, then (if that succeeded) the library read and
// the decision engine for that instance's type.
//
// It is the extracted body of what used to be run()'s --once branch, unchanged
// in behavior. §2.6's rule governs throughout: one instance's failure is warned
// about by the helper that met it and never stops the others.
//
// ctx carries the daemon's shutdown. It is checked BETWEEN INSTANCES here, and
// again between items inside the engines; the write paths detach their own
// requests from it so an in-flight item completes (binding controller note 4).
func runScanCycle(ctx context.Context, logger *slog.Logger, cfg Config, cycle scanCycle) {
	for _, inst := range cfg.Instances {
		// An instance the caller did not name is not merely left unwritten: it
		// is not contacted at all. "--instance radarr-4k" has to mean the run
		// touches radarr-4k and nothing else, or it would be a weaker statement
		// than it looks — and a webhook for radarr-4k must not scan the sonarr.
		if cycle.instanceName != "" && inst.Name != cycle.instanceName {
			continue
		}
		if ctx.Err() != nil {
			logger.Info("shutdown requested: the remaining instances are not contacted this cycle",
				"instance", inst.Name, "type", inst.Type)
			return
		}

		// The READ path logs through a logger whose INFO records are demoted
		// to the cycle's report level (see demoteInfoTo): the *arr's version,
		// its quality profiles and the library totals are news exactly once,
		// and a daemon reprinting them on every sweep buries the lines that
		// are not. On the startup scan and on --once the level IS info, and
		// demoteInfoTo hands the logger straight back untouched.
		//
		// The ENGINES get the undemoted logger: their summaries and their
		// confirmed writes stay at INFO on every cycle forever, and they level
		// their own per-item report lines from the same scope.
		readLogger := demoteInfoTo(logger, cycle.scope.itemLevel)

		ok := checkInstanceConnectivity(ctx, readLogger, inst)
		if ok && inst.Type == "radarr" {
			movies, wantedIDs, dataOK := inspectRadarrLibrary(ctx, readLogger, inst, cycle.samples)
			if dataOK {
				runRadarrDecisionEngine(ctx, logger, inst, movies, wantedIDs, cfg.ExclusionTag, cycle.scope, cycle.dryRun, cycle.reverse)
			}
		}
		if ok && inst.Type == "sonarr" {
			series, wantedEpisodeIDs, wantedSeasons, dataOK := inspectSonarrLibrary(ctx, readLogger, inst)
			if dataOK {
				runSonarrDecisionEngine(ctx, logger, inst, series, wantedEpisodeIDs, wantedSeasons, cfg.ExclusionTag, cycle.scope, cycle.dryRun, cycle.reverse)
			}
		}
	}
}

// --- the clock the daemon sees ---------------------------------------------
//
// Every wait in the daemon goes through this interface so the tests can drive
// the debounce and the reconciliation interval on a virtual clock instead of
// sleeping (binding controller note 6: "timers use injectable clocks for tests
// — no real sleeps beyond trivial ones; keep the suite fast"). A 45-second
// debounce and a one-hour poll interval are not things a test can wait for, and
// shrinking them to milliseconds would trade determinism for speed and get
// neither.

type daemonClock interface {
	Now() time.Time
	NewTimer(d time.Duration) daemonTimer
}

// daemonTimer is the subset of *time.Timer the loop uses. Stop matters: the
// loop arms a timer on every iteration and abandons it as soon as anything else
// wakes it, so a non-stopping timer would accumulate one live runtime timer per
// webhook event.
type daemonTimer interface {
	C() <-chan time.Time
	Stop()
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) NewTimer(d time.Duration) daemonTimer { return &systemTimer{t: time.NewTimer(d)} }

type systemTimer struct{ t *time.Timer }

func (s *systemTimer) C() <-chan time.Time { return s.t.C }
func (s *systemTimer) Stop()               { s.t.Stop() }

// --- the daemon ------------------------------------------------------------

// webhookQueueLimit bounds the number of distinct items the debounce queue
// holds. Well above any plausible real burst (a season pack is ONE key, since
// the key is the series), and low enough that a runaway sender cannot grow the
// process's memory through it. See debounceQueue.add for what overflow does,
// and handleWebhook's drop path for the one thing that is NOT uniform about it:
// what comes back for a dropped key is the reconciliation sweep, and a
// poll_interval-0 daemon has none, so the warning says so rather than promising
// a recovery that configuration does not have.
const webhookQueueLimit = 1024

// webhookShutdownGrace bounds how long the HTTP server is given to finish
// in-flight requests during shutdown. A webhook handler does no I/O beyond
// reading its request body, so this is generous; it exists so a half-open
// connection cannot hold the process open.
const webhookShutdownGrace = 5 * time.Second

// daemonOptions are the seams the tests drive the daemon through. Every field
// is optional; the zero value is exactly what production uses.
type daemonOptions struct {
	// clock replaces the system clock, so debounce and reconciliation waits
	// can be advanced instantly and deterministically.
	clock daemonClock

	// listener replaces binding cfg.WebhookPort, so tests can take an
	// ephemeral port (:0) and learn its address without racing the daemon.
	listener net.Listener

	// signals replaces the real SIGTERM/SIGINT subscription, so the shutdown
	// path can be exercised without sending signals to the test process.
	signals <-chan os.Signal

	// forceExit replaces os.Exit for the second-signal path.
	forceExit func(int)

	// onStartupScanDone, when set, is called once the startup scan has
	// finished and before the loop's first wait.
	onStartupScanDone func()
}

// daemon is the assembled long-running service. It exists as a type only so
// the loop, the webhook cycles, and the reconciliation sweep can share the
// handful of values they all need without a nine-parameter function.
type daemon struct {
	logger *slog.Logger
	cfg    Config
	clock  daemonClock
	queue  *debounceQueue
}

// runDaemon is the default run mode: a full scan on startup, then the webhook
// listener and the reconciliation sweep, until a signal arrives. It returns the
// process exit code.
//
// ORDER OF STARTUP, and the choice behind it (the brief requires picking one and
// documenting it): the listener BINDS AND ACCEPTS FIRST, then the startup scan
// runs, then the loop begins dispatching. An event arriving during the startup
// scan is therefore answered 200 and QUEUED, not refused.
//
// The alternative — scan first, listen after — loses those events twice over:
// the *arr records a failed webhook (some versions disable a hook that keeps
// failing), and the human sees an error whose cause is "cutoffarr was busy
// starting up". Accepting and queueing costs nothing, because the queue is a
// map and the handler does no I/O. Nothing can be evaluated concurrently with
// the startup scan either way: the dispatcher is the same single goroutine that
// runs the scan.
//
// A webhook port that cannot be bound is FATAL rather than a warning. A daemon
// silently running without its listener would reconcile only on the poll
// interval — up to a day late by default — while every log line and every
// import looked normal, which is precisely the "silently does nothing" failure
// this project refuses everywhere else.
func runDaemon(ctx context.Context, logger *slog.Logger, cfg Config, opts daemonOptions) int {
	clk := opts.clock
	if clk == nil {
		clk = systemClock{}
	}
	forceExit := opts.forceExit
	if forceExit == nil {
		forceExit = os.Exit
	}

	d := &daemon{logger: logger, cfg: cfg, clock: clk, queue: newDebounceQueue(webhookQueueLimit)}

	instanceTypes := make(map[string]string, len(cfg.Instances))
	for _, inst := range cfg.Instances {
		instanceTypes[inst.Name] = inst.Type
	}

	listener := opts.listener
	if listener == nil {
		var err error
		listener, err = net.Listen("tcp", ":"+strconv.Itoa(cfg.WebhookPort))
		if err != nil {
			logger.Error("fatal: the webhook port could not be bound, so no webhook could ever arrive; refusing to run as a daemon that only polls",
				"webhookPort", cfg.WebhookPort, "error", err)
			return 1
		}
	}

	srv := &http.Server{
		// Timeouts, because this listener is reachable by whatever can route to
		// the container and net/http has none by default: without them a single
		// connection that opens and then dribbles bytes holds a goroutine (and
		// a file descriptor) open for as long as it likes. The values are
		// generous for a request whose body is a few kilobytes of JSON and
		// whose handler does no I/O.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		Handler: newWebhookHandler(&webhookServer{
			logger:       logger,
			queue:        d.queue,
			debounce:     cfg.WebhookDebounce,
			now:          clk.Now,
			instances:    instanceTypes,
			pollInterval: cfg.PollInterval,
		}),
	}

	// The shutdown handler is installed BEFORE anything long-running starts, so
	// a signal arriving during the startup scan is a clean stop rather than the
	// default disposition killing the process mid-cycle. It is installed AFTER
	// the bind, deliberately: the fatal-bind path above returns without ever
	// subscribing to signals, which matters to any caller (a test, a future
	// embedding) that is not a process about to exit.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	loopDone := make(chan struct{})
	installShutdownHandler(logger, opts.signals, cancel, forceExit, loopDone)

	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("the webhook server stopped serving", "error", err)
		}
	}()

	logger.Info("webhook listener started",
		"address", listener.Addr().String(), "endpoint", "POST /webhook/{instance-name}",
		"debounce", cfg.WebhookDebounce, "pollInterval", cfg.PollInterval, "dryRun", cfg.DryRun)

	// The startup full scan (plan §8's "full scan on startup"), reported at
	// INFO in full: it is the one daemon cycle a human is watching, the
	// equivalent of a --once run, and the baseline every later cycle's silence
	// is read against.
	logger.Info("startup scan beginning", "instances", len(cfg.Instances), "dryRun", cfg.DryRun)
	runScanCycle(ctx, logger, cfg, scanCycle{scope: fullLibraryScope(slog.LevelInfo), dryRun: cfg.DryRun, reverse: fullScanReverseOptions(cfg)})
	// A pass that stopped early must never print the line a pass that finished
	// prints. runScanCycle returns the same way whether it covered every
	// instance or abandoned the cycle on shutdown (its own between-instances
	// check, and the engines' between-items ones), so the only thing that can
	// tell them apart here is the context — and getting this wrong put
	// "startup scan complete" immediately after "abandoning this instance's
	// cycle mid-evaluation", at the exact moment a human is reading the log
	// because they just stopped the process.
	if ctx.Err() != nil {
		logger.Info("startup scan abandoned on shutdown: it stopped between items and did not cover every instance, so nothing above describes the whole library; the next start begins with a full scan")
	} else {
		logger.Info("startup scan complete")
	}
	if opts.onStartupScanDone != nil {
		opts.onStartupScanDone()
	}

	d.loop(ctx)
	close(loopDone)

	// Shutdown, in the order that makes the guarantees true: the loop has
	// already returned (so no evaluation is running and none will start), and
	// only then is the listener closed. Shutdown's context is detached from the
	// cancelled one for the same reason the write paths detach theirs — a
	// cancelled context would make Shutdown return instantly and abandon
	// in-flight requests rather than draining them.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.WithoutCancel(ctx), webhookShutdownGrace)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("the webhook server did not shut down cleanly within the grace period", "error", err, "grace", webhookShutdownGrace)
	}
	<-serveDone
	logger.Info("shutdown complete", "pendingWebhookItemsDropped", d.queue.size())
	return 0
}

// installShutdownHandler wires the first signal to cancellation and the second
// to an immediate exit.
//
// The first signal is a REQUEST: the loop stops between items, an in-flight
// write finishes its own calls, the server drains, and the process exits 0. The
// second is an ORDER, and it is honored literally — a human who sends SIGTERM
// twice has decided they are not waiting for a graceful stop, and the most
// likely reason is that something is genuinely stuck. Exit code 1 says the stop
// was not clean.
//
// sigC is injectable so the shutdown path can be tested without sending real
// signals to the test process; nil means subscribe to the real ones.
func installShutdownHandler(logger *slog.Logger, sigC <-chan os.Signal, cancel context.CancelFunc, forceExit func(int), loopDone <-chan struct{}) {
	if sigC == nil {
		real := make(chan os.Signal, 2)
		signal.Notify(real, syscall.SIGTERM, syscall.SIGINT)
		sigC = real
	}
	go func() {
		select {
		case <-loopDone:
			return
		case sig := <-sigC:
			logger.Info("shutdown signal received; finishing the current item and stopping between items", "signal", sig.String())
		}
		cancel()
		select {
		case <-loopDone:
		case sig := <-sigC:
			logger.Warn("second shutdown signal received; exiting immediately without finishing the current item", "signal", sig.String())
			forceExit(1)
		}
	}()
}

// loop is the daemon's single worker. Everything that evaluates anything runs
// here, on this one goroutine, which is what makes "one evaluation at a time"
// (binding controller resolution 4) structural rather than a lock someone has
// to remember to take.
//
// It returns when ctx is cancelled — always between items, never inside one.
func (d *daemon) loop(ctx context.Context) {
	// The reconciliation schedule. A zero poll_interval disables the sweep
	// entirely (config.go validates it as either 0 or at least an hour) while
	// leaving the webhook listener running, which is the "webhooks only" setup.
	var nextReconcile time.Time
	if d.cfg.PollInterval > 0 {
		nextReconcile = d.clock.Now().Add(d.cfg.PollInterval)
		d.logger.Info("reconciliation sweep scheduled", "pollInterval", d.cfg.PollInterval, "nextSweep", nextReconcile)
	} else {
		d.logger.Info("reconciliation sweep disabled by poll_interval 0; this daemon reconciles on webhooks only")
	}

	for {
		if ctx.Err() != nil {
			return
		}
		now := d.clock.Now()

		if due, seasons := d.queue.expired(now); len(due) > 0 {
			d.runWebhookCycles(ctx, due, seasons)
			continue
		}

		if !nextReconcile.IsZero() && !now.Before(nextReconcile) {
			d.logger.Info("reconciliation sweep beginning", "instances", len(d.cfg.Instances), "dryRun", d.cfg.DryRun)
			// The SAME function the startup scan and --once call, with the
			// same full-library scope — only the report level differs, because
			// this cycle repeats forever and its per-item lines are repetition
			// rather than news (binding controller note 6).
			runScanCycle(ctx, d.logger, d.cfg, scanCycle{scope: fullLibraryScope(slog.LevelDebug), dryRun: d.cfg.DryRun, reverse: fullScanReverseOptions(d.cfg)})
			if ctx.Err() != nil {
				// The startup scan's rule (see runDaemon) plus the half only the
				// sweep has: the completion line RE-ARMS the schedule and
				// announces the next sweep. A daemon on its way out must not
				// schedule anything — the loop returns on the next iteration —
				// and announcing a sweep that will never run is worse than
				// saying nothing.
				d.logger.Info("reconciliation sweep abandoned on shutdown: it stopped between items and did not cover every instance, so nothing above describes the whole library; the schedule is not re-armed because this daemon is exiting")
				continue
			}
			nextReconcile = d.clock.Now().Add(d.cfg.PollInterval)
			d.logger.Info("reconciliation sweep complete", "nextSweep", nextReconcile)
			continue
		}

		wait, scheduled := d.nextWait(now, nextReconcile)
		if !scheduled {
			// Nothing is scheduled at all (poll_interval 0 and an empty
			// queue): wait for a webhook or for shutdown, indefinitely and
			// without burning a timer.
			select {
			case <-ctx.Done():
				return
			case <-d.queue.notify:
			}
			continue
		}

		timer := d.clock.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C():
		case <-d.queue.notify:
			// A new event moved (or added) a deadline; recompute the wait.
			timer.Stop()
		}
	}
}

// nextWait is how long the loop should sleep: until the earliest pending
// debounce deadline or the next reconciliation sweep, whichever is sooner.
func (d *daemon) nextWait(now, nextReconcile time.Time) (time.Duration, bool) {
	var at time.Time
	if deadline, pending := d.queue.nextDeadline(); pending {
		at = deadline
	}
	if !nextReconcile.IsZero() && (at.IsZero() || nextReconcile.Before(at)) {
		at = nextReconcile
	}
	if at.IsZero() {
		return 0, false
	}
	if wait := at.Sub(now); wait > 0 {
		return wait, true
	}
	return 0, true
}

// runWebhookCycles evaluates the expired keys, one instance at a time.
//
// THE MANDATED COALESCING (binding controller note 2): keys are grouped by
// instance and each instance gets ONE evaluation whose write scope is the union
// of its expired ids. The evaluation itself is the full-evidence one — the same
// whole-library read the --only-id path performs, so the cross-check has a real
// sample to work from — which is why a webhook event costs a full instance scan
// and why the debounce is what bounds that cost. There is no reduced-evidence
// path.
func (d *daemon) runWebhookCycles(ctx context.Context, due []queueKey, seasons map[queueKey][]int) {
	byInstance := make(map[string][]int)
	var order []string
	for _, k := range due {
		if _, seen := byInstance[k.instance]; !seen {
			order = append(order, k.instance)
		}
		byInstance[k.instance] = append(byInstance[k.instance], k.id)
	}

	for _, name := range order {
		if ctx.Err() != nil {
			// Unlike the queue-overflow WARN in handleWebhook, this recovery
			// claim does NOT depend on poll_interval, and the difference is
			// worth stating because the two lines look alike. An overflow drop
			// happens in a daemon that keeps running, so what comes back for it
			// is the reconciliation sweep — which a poll_interval-0 daemon does
			// not have. This drop happens on the way out: the loop is returning
			// and the process is exiting, and EVERY start begins with a
			// full-library startup scan (see runDaemon), sweep or no sweep.
			d.logger.Info("shutdown requested: the remaining webhook-triggered evaluations are dropped; this daemon is exiting, and every start begins with a full-library startup scan that covers them (this does not depend on poll_interval)",
				"instance", name)
			return
		}
		ids := byInstance[name]
		attrs := []any{"instance", name, "items", len(ids), "ids", joinInts(dedupeSortedIDs(ids)), "dryRun", d.cfg.DryRun}

		// The affected seasons are the WRITE SCOPE, not merely evidence about
		// what triggered the cycle: the plan's granularity for a Sonarr webhook
		// is "that series' affected season", so a season-2 import must not
		// unmonitor season 1 of the same show in an unattended cycle 45 seconds
		// later. A key with no seasons here — a Radarr movie, or a Sonarr
		// payload whose episodes array was absent or empty — carries no entry
		// and is evaluated across every eligible season, exactly as --only-id
		// is (binding controller resolution 2).
		var scopeSeasons map[int][]int
		for _, k := range due {
			if k.instance == name && len(seasons[k]) > 0 {
				if scopeSeasons == nil {
					scopeSeasons = make(map[int][]int)
				}
				scopeSeasons[k.id] = seasons[k]
				attrs = append(attrs, "affectedSeasons/"+strconv.Itoa(k.id), joinInts(seasons[k]))
			}
		}
		d.logger.Info("webhook debounce expired; evaluating", attrs...)
		runScanCycle(ctx, d.logger, d.cfg, scanCycle{
			instanceName: name,
			scope:        webhookScope(ids, scopeSeasons),
			dryRun:       d.cfg.DryRun,
		})
	}
}
