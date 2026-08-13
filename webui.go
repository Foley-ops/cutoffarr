package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
)

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
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	// The encode error is not checked: by the time Encode could fail, the 200
	// header is already written and there is nothing left to correct it with
	// — the same "nothing more to do" the webhook handler's own
	// fmt.Fprintln(w, "accepted") accepts silently.
	_ = json.NewEncoder(w).Encode(s.stats.snapshot())
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
func (s *webUIServer) handleAction(w http.ResponseWriter, r *http.Request) {
	writeJSON := func(code int, body any) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	}

	if s.actions == nil {
		writeJSON(http.StatusForbidden, actionResponse{
			Outcome: actionOutcomeDisabled,
			Message: "this cutoffarr has no action system wired, so no finding can be acted on from here",
			Reason:  "this cutoffarr has no action system wired, so no finding can be acted on from here",
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
		writeJSON(http.StatusBadRequest, actionResponse{Outcome: actionOutcomeRefused, Message: reason, Reason: reason})
		return
	}

	resp := s.actions.run(r.Context(), req)
	code := resp.status
	if code == 0 {
		code = http.StatusOK
	}
	writeJSON(code, resp)
}
