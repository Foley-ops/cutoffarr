package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// actionDeadline bounds one action end to end: the handler's own write
// deadline and the context it hands the runner are both set from it, so the
// two can never drift into a state where the work outlives the connection
// allowed to report it.
//
// Ten minutes is chosen against the slowest thing an action legitimately
// does — a Sonarr instance's full file report, which is one /episodefile
// fetch per series plus a walk of every mapped root — with room for a large
// library on a slow share. It is a ceiling, not a delay: an ordinary action
// answers in well under a second.
const actionDeadline = 10 * time.Minute

// webui.go is Phase 12's (v2c) HTTP surface: the embedded GUI page and its
// two JSON endpoints, mounted on the SAME listener the webhook endpoint
// already uses (daemon.go's runDaemon composes this handler with
// newWebhookHandler's on one top-level mux). It is a pure READ surface plus
// one queue-a-scan write that reuses the EXISTING runScanCycle pipeline —
// nothing here calls an *arr, decides anything, or writes anything to a
// *arr's API: GET /api/stats copies out of statsStore (stats.go), and
// POST /api/scan only flips scanCoordinator's pending flag for the daemon's
// own loop goroutine to act on (see that type's doc comment, daemon.go).
// TestTree_HasExactlyThreeWriteVerbCallSites (writer_test.go) is therefore
// unaffected: this file names no client write verb and calls .DoJSON
// nowhere.
//
// No authentication (plan §8, README): this is a LAN homelab tool, and the
// two endpoints it adds answer only questions the log already answers
// (what would this cycle do, is dry-run on) or trigger the exact same
// full-library sweep a human could already get by restarting the process —
// nothing here exposes an *arr API key or any control an operator on the
// LAN does not already have some other way to reach.

//go:embed webui.html
var webUIPage []byte

// webUIServer is the state the three handlers below share: stats to read
// from and scan to request against. Neither field is ever written to by this
// file — stats.recordInstance is called only from daemon.go's runScanCycle,
// and scan.requestScan is the only mutation handleScan ever makes.
type webUIServer struct {
	stats  *statsStore
	scan   *scanCoordinator
	logger *slog.Logger

	// actions is [v2.2] the human-clicked action system's single-flight
	// executor (actions.go). It is the ONE field on this struct that can cause
	// anything in the world to change, and handleAction below is the ONLY
	// place it is ever touched — which is what makes the owner's ruling
	// structurally true rather than merely intended: no sweep, webhook or
	// reconciliation path has a reference to it at all.
	//
	// Nil is a supported state and means "this deployment has no action
	// system wired" (every pre-v2.2 test constructs a webUIServer without it).
	// The handler answers such a request with a 403 rather than panicking.
	actions *actionRunner
}

// newWebUIHandler builds the mux serving the dashboard and its two
// endpoints. Method match lives in the route pattern (Go 1.22+ ServeMux),
// exactly as newWebhookHandler's own comment explains: a request to a
// pattern's path with the wrong method gets ServeMux's own 405 for free,
// and a path none of these three patterns names falls through to
// ServeMux's own 404 — the "404 for other paths unchanged" half of the API
// contract needs no code here at all.
//
// "GET /{$}" — not the bare "GET /" a subtree pattern would need — matches
// ONLY the exact root path. This mux is itself mounted at "/" on the daemon's
// top-level mux (daemon.go), which is what makes it the catch-all default
// for every path "/webhook/" doesn't claim; "{$}" is what stops THIS mux
// from treating every one of those as "the index page".
func newWebUIHandler(s *webUIServer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("POST /api/scan", s.handleScan)
	mux.HandleFunc("POST /api/action", s.handleAction)
	return mux
}

// handleIndex serves the embedded page verbatim. It is go:embed'd bytes, so
// there is nothing to fail on: no filesystem read, no template render, no
// error path a response code needs to branch on.
func (s *webUIServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(webUIPage)
}

// handleStats answers with the current snapshot — valid JSON with an empty
// instances array before the first cycle completes (statsStore starts with
// an empty map; snapshot's own loop over s.order simply runs zero times),
// never an error: there is no failure mode here at all, since snapshot()
// only ever copies already-valid in-memory data.
//
// No logging here, deliberately (binding controller note on the noise
// budget): the GUI polls this every 30s for as long as it is open, and even
// a DEBUG line per request would grow without bound over a long-running
// daemon for a read that changes nothing.
func (s *webUIServer) handleStats(w http.ResponseWriter, r *http.Request) {
	snap := s.stats.snapshot()
	// [v2.2] The action switches, read from the runner's own config so the
	// page and the endpoint can never disagree about which button is live. A
	// server with no runner wired reports both as false — the honest answer,
	// since every action against it is refused — rather than omitting them and
	// leaving the page to guess a default.
	if s.actions != nil {
		snap.GUIActions = s.actions.cfg.GUIActions
		snap.ReverseScanRemonitor = s.actions.cfg.ReverseScanRemonitor
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	// The encode error is not checked: by the time Encode could fail, the 200
	// header is already written and there is nothing left to correct it with
	// — the same "nothing more to do" the webhook handler's own
	// fmt.Fprintln(w, "accepted") accepts silently.
	_ = json.NewEncoder(w).Encode(snap)
}

// handleScan is the whole of POST /api/scan: ask the coordinator to queue
// one manual scan, and report which of the two states resulted. It never
// blocks on the scan itself — scanCoordinator.requestScan only flips a flag
// and notifies the daemon's own loop goroutine, mirroring the webhook
// handler's own "never make the caller wait on evaluation" rule (webhook.go)
// even though this endpoint has no *arr on the other end waiting.
//
// Logged once, at DEBUG: rare enough (a human clicking a button) that even
// INFO would be reasonable, but the MEANINGFUL, once-per-scan lines are the
// loop's own "manual scan beginning"/"manual scan complete" (daemon.go),
// which stay at INFO; this line exists only so an operator watching
// log_level: debug can see the click land before the loop picks it up.
func (s *webUIServer) handleScan(w http.ResponseWriter, r *http.Request) {
	queued := s.scan.requestScan()
	status := "already-pending"
	if queued {
		status = "queued"
	}
	if s.logger != nil {
		s.logger.Debug("manual scan requested via POST /api/scan", "status", status)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": status})
}

// handleAction is v2.2's whole action surface, and — this is the property the
// owner's ruling rests on — the only path in this program from which an
// executor can be reached at all.
//
// It does three things and delegates everything else: refuse a body it cannot
// read (400), refuse when no action system is wired (403), and hand the parsed
// request to the single-flight runner, which owns every switch check, every
// re-verification, every refusal and the audit line for all of them. The
// runner chooses the status code along with the outcome, because the two must
// never be able to disagree.
//
// A note on what is NOT here: no rate limit, no authentication. This page has
// never had either (plan §8: "Authentication on the GUI (LAN tool; document
// that)"), and v2.2 does not change the threat model so much as make it worth
// restating, which the README's "Acting on findings" section does plainly:
// anything that can reach this port can click these buttons, so the port
// belongs on a LAN you trust, and gui_actions stays false until you have
// decided that it is.
//
// What IS here, since a review round found it missing, is the guard that trust
// model does not cover: a web page the operator opens in a browser that is
// already on that LAN is not a LAN peer, and it can post a form here with no
// CORS involvement at all. crossSiteRefusal (actions.go) is that check, and its
// doc comment is where the reasoning lives.
func (s *webUIServer) handleAction(w http.ResponseWriter, r *http.Request) {
	writeJSON := func(code int, body any) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	}

	// Both exits below LOG, and that is a review fix. They are the only two
	// answers this endpoint gives without reaching the runner — which owns the
	// audit line for everything else, including the cross-site refusal — so
	// until this round they were the only two ways to poke the one
	// unauthenticated endpoint in this program that can move files and leave no
	// trace at all, at any level. That hid two very different things behind the
	// same silence: a burst of malformed probes against the port, and an
	// operator's "I click the button and nothing happens" on a deployment that
	// never wired an action system. WARN for both: neither is a normal event on
	// a working deployment, and Debug is invisible on every deployment that
	// does not opt into it, which is all of them.
	if s.actions == nil {
		const reason = "this cutoffarr has no action system wired, so no finding can be acted on from here"
		// msg=action, with the audit line's own vocabulary, so the one grep an
		// operator is told to use finds this too. There is no runner to name a
		// kind or an instance, and inventing either would be worse than the
		// three attrs that are actually known.
		s.logWarn("action", "source", "gui", "outcome", actionOutcomeDisabled,
			"status", http.StatusForbidden, "detail", reason)
		writeJSON(http.StatusForbidden, actionResponse{
			Outcome: actionOutcomeDisabled,
			Message: reason,
			Reason:  reason,
		})
		return
	}

	// A bounded reader, because this body is a few hundred bytes of JSON and
	// the listener is reachable by whatever can route to the container — the
	// same reasoning behind the server's own timeouts (daemon.go).
	var req actionRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		reason := fmt.Sprintf("the action request could not be read as JSON: %v", err)
		// NOT msg=action: nothing here parsed, so there is no kind, no
		// instance and no item — calling it an action would put a line in the
		// audit vocabulary that names nothing. The error itself is the payload,
		// since "unknown field" and "unexpected EOF" mean quite different
		// things about who is talking to this port.
		s.logWarn("an action request could not be read", "source", "gui",
			"status", http.StatusBadRequest, "error", err)
		writeJSON(http.StatusBadRequest, actionResponse{Outcome: actionOutcomeRefused, Message: reason, Reason: reason})
		return
	}

	// The browser guard (review finding). The body is decoded FIRST, on
	// purpose: decoding has no side effects, and it is what lets this refusal
	// carry the same kind=/path=/id= audit line every other refusal carries
	// instead of one that says only "something was rejected". The rule itself
	// is the runner's — see crossSiteRefusal, which explains why a
	// content-type check stops a cross-site form post that confirm: true and
	// DisallowUnknownFields do not.
	if resp, refused := s.actions.crossSiteRefusal(req, r.Header); refused {
		writeJSON(resp.status, resp)
		return
	}

	// Self-review finding, and the reason this endpoint needs a deadline of
	// its own. The daemon's *http.Server sets WriteTimeout: 30s — generous
	// for every endpoint that came before, all of which answer out of memory.
	// An action does not: rule 3 makes it re-derive its finding from LIVE
	// data, and for a Sonarr instance that is a per-series /episodefile fetch
	// across the whole library plus a full walk of every mapped root. Minutes,
	// not seconds, on a real library.
	//
	// Without this the connection is torn down mid-action and the operator
	// sees a network error with no way to know whether the file moved — the
	// exact "never lies about what happened" failure this phase exists to
	// prevent. The write deadline is extended, and the same constant bounds
	// the work itself, so a genuinely stuck *arr cannot hold the single-flight
	// lock forever and leave every later click queued behind it.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(actionDeadline)); err != nil {
		// Not fatal: an http.ResponseWriter that cannot carry a deadline (a
		// test recorder, a future middleware) still serves the request
		// correctly, it just inherits the server's own timeout.
		//
		// WARN, not Debug — review fix. "Not fatal" is not the same as "not
		// worth saying": inheriting the server's own 30s WriteTimeout is
		// exactly the mid-action connection teardown this deadline exists to
		// prevent, on a request that may be renaming files, and at Debug it
		// was invisible on every deployment that does not opt into debug
		// logging. If this ever fires in production it is the first thing
		// anyone investigating a truncated action needs to see.
		s.logWarn("the action response deadline could not be extended, so this action falls back to the server's own write timeout and may be cut off mid-flight", "error", err)
	}
	ctx, cancel := context.WithTimeout(r.Context(), actionDeadline)
	defer cancel()

	resp := s.actions.run(ctx, req)
	code := resp.status
	if code == 0 {
		code = http.StatusOK
	}
	writeJSON(code, resp)
}

// logWarn is the nil-safe logger this file's handlers use. Every line it
// carries is about the action endpoint, and every one of them is a thing that
// does not happen on a working deployment — a click answered before the runner
// was reached, a body nothing could parse, a deadline that would not set — so
// they share one level rather than being spread across three.
//
// Nil-safe because a webUIServer is legitimately constructed without a logger
// (every pre-v2.2 test helper does), and a missing logger must never be the
// reason an action endpoint panics.
func (s *webUIServer) logWarn(msg string, args ...any) {
	if s.logger != nil {
		s.logger.Warn(msg, args...)
	}
}
