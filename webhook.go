package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// webhook.go is the daemon's inbound half: one endpoint,
// POST /webhook/{instance-name}, plus the per-item debounce queue an accepted
// event lands in.
//
// Two rules shape everything here, and both come from the same place — the
// *arr on the other end of the connection is waiting on this response, and it
// is not our program:
//
//  1. The handler NEVER makes the *arr wait on our evaluation. It parses a
//     minimal payload, puts a key in an in-memory map, and returns 200. No
//     HTTP call, no library read, no decision — those happen later, on the
//     daemon's own goroutine.
//  2. The handler NEVER answers 4xx/5xx. An unknown instance, an eventType we
//     ignore, a payload we cannot read, a payload missing its id: all are 200
//     with a log line of the appropriate level. A non-2xx would make Radarr or
//     Sonarr surface a failing webhook to the human for something that is
//     either our problem or deliberately none of our business, and some *arr
//     versions disable a hook that keeps failing. The log is where our opinion
//     goes; the status code is only "we received it".

// webhookMaxBodyBytes bounds what the handler will read from one webhook
// request. Real *arr payloads are a few kilobytes; this is generous and still
// keeps an unbounded or hostile body from being buffered.
const webhookMaxBodyBytes = 1 << 20 // 1 MB

// Event types, verbatim from the *arr side (binding controller resolution 2).
//
// "Download" is the one that matters, and its name is a historical artifact
// worth stating out loud: BOTH "On Import" and "On Upgrade" arrive as
// eventType "Download" (Radarr distinguishes them with an isUpgrade bool,
// Sonarr similarly). Either one means a file just landed for this item, which
// is exactly when its cutoff status may have changed, so both are accepted and
// nothing reads isUpgrade.
//
// "Test" is what the *arr sends when a human clicks Test in its UI. It is the
// one event type whose whole purpose is to be seen by a person, so it gets a
// friendly INFO line naming the instance rather than the debug shrug every
// other unhandled type gets.
const (
	webhookEventDownload = "Download"
	webhookEventTest     = "Test"
)

// webhookItemKind distinguishes the two things a key can name. It is derived
// from the CONFIGURED instance type, never sniffed from the payload: the
// endpoint the event arrived on is what says which library it is about, and a
// radarr payload arriving at a sonarr's endpoint is a misconfiguration to be
// surfaced, not a shape to be guessed at.
const (
	webhookKindMovie  = "movie"
	webhookKindSeries = "series"
)

// queueKey identifies one debounced item: an instance's movie or series.
// Rendered as "instance/movie/123" — the form the plan names — for logs.
type queueKey struct {
	instance string
	kind     string
	id       int
}

func (k queueKey) String() string {
	return k.instance + "/" + k.kind + "/" + strconv.Itoa(k.id)
}

// webhookPayload is the subset of an *arr webhook body this project reads,
// decoded with the house pointer/null discipline: every field is a pointer so
// "the key was absent" can be told apart from "the key was present with the
// zero value". id 0 is not a real *arr id, but a payload whose movie.id
// decoded to a plain int would make an absent key and a null one both look
// like a decision to act on item 0.
type webhookPayload struct {
	EventType *string              `json:"eventType"`
	Movie     *webhookItemRef      `json:"movie"`
	Series    *webhookItemRef      `json:"series"`
	Episodes  *[]webhookEpisodeRef `json:"episodes"`
}

type webhookItemRef struct {
	ID    *int    `json:"id"`
	Title *string `json:"title"`
}

type webhookEpisodeRef struct {
	SeasonNumber *int `json:"seasonNumber"`
}

// affectedSeasons returns the deduplicated, sorted season numbers a Sonarr
// payload's episodes name. An absent or empty episodes array yields none.
//
// These are EVIDENCE, not scope. The evaluation a webhook triggers is scoped to
// the SERIES, exactly as --only-id is, because that is the machinery the
// binding full-evidence ruling says to reuse (controller resolution 4) — and
// the season set the engine then acts on is a superset of the affected one,
// decided by the same rules a reconciliation sweep applies. Logging what
// arrived is what lets a human confirm the hookup end to end.
func (p webhookPayload) affectedSeasons() []int {
	if p.Episodes == nil {
		return nil
	}
	var seasons []int
	for _, e := range *p.Episodes {
		if e.SeasonNumber != nil {
			seasons = append(seasons, *e.SeasonNumber)
		}
	}
	return dedupeSortedIDs(seasons)
}

// debounceQueue is the in-memory, per-item debounce the plan calls for: each
// event RESETS its key's timer, so a 24-episode season-pack import produces one
// evaluation rather than 24, and the *arr has finished its own database writes
// before we read.
//
// It is deliberately not durable. Losing the queue on restart is fine and is
// the design (plan §8): the startup scan and the reconciliation sweep both
// cover everything the queue could have been holding.
type debounceQueue struct {
	mu       sync.Mutex
	deadline map[queueKey]time.Time
	seasons  map[queueKey][]int
	limit    int

	// notify carries "something changed, recompute your wait" to the daemon
	// loop. Buffered with room for one, and sent to non-blockingly: the loop
	// only ever needs to know THAT something changed.
	notify chan struct{}
}

func newDebounceQueue(limit int) *debounceQueue {
	return &debounceQueue{
		deadline: make(map[queueKey]time.Time),
		seasons:  make(map[queueKey][]int),
		limit:    limit,
		notify:   make(chan struct{}, 1),
	}
}

// add records an event for key, (re)setting its timer to fire debounce from
// now. It returns the deadline it set and, when the queue was full and
// something had to go, the key it dropped.
//
// The bound exists because the queue is fed by an outside system: a runaway
// *arr, a mass import, or a misconfigured hook could otherwise grow it without
// limit. When a NEW key arrives at a full queue, the entry with the EARLIEST
// deadline is dropped — the one that has been settling longest, and therefore
// the one closest to being evaluated anyway. Nothing is lost permanently:
// every dropped key is covered by the reconciliation sweep, which is the same
// guarantee that makes losing the whole queue on restart acceptable.
func (q *debounceQueue) add(key queueKey, seasons []int, now time.Time, debounce time.Duration) (deadline time.Time, dropped *queueKey) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if _, existing := q.deadline[key]; !existing && len(q.deadline) >= q.limit {
		var oldest queueKey
		var oldestAt time.Time
		for k, at := range q.deadline {
			if oldestAt.IsZero() || at.Before(oldestAt) {
				oldest, oldestAt = k, at
			}
		}
		delete(q.deadline, oldest)
		delete(q.seasons, oldest)
		dropped = &oldest
	}

	deadline = now.Add(debounce)
	q.deadline[key] = deadline
	if len(seasons) > 0 {
		q.seasons[key] = dedupeSortedIDs(append(q.seasons[key], seasons...))
	}

	select {
	case q.notify <- struct{}{}:
	default:
	}
	return deadline, dropped
}

// expired removes and returns every key whose timer has come due as of now,
// in deterministic (sorted) order, along with the affected seasons each had
// accumulated.
func (q *debounceQueue) expired(now time.Time) ([]queueKey, map[queueKey][]int) {
	q.mu.Lock()
	defer q.mu.Unlock()

	var due []queueKey
	for k, at := range q.deadline {
		if !at.After(now) {
			due = append(due, k)
		}
	}
	if len(due) == 0 {
		return nil, nil
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].instance != due[j].instance {
			return due[i].instance < due[j].instance
		}
		return due[i].id < due[j].id
	})
	seasons := make(map[queueKey][]int, len(due))
	for _, k := range due {
		if s := q.seasons[k]; len(s) > 0 {
			seasons[k] = s
		}
		delete(q.deadline, k)
		delete(q.seasons, k)
	}
	return due, seasons
}

// nextDeadline reports the earliest pending deadline, if any.
func (q *debounceQueue) nextDeadline() (time.Time, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	var earliest time.Time
	for _, at := range q.deadline {
		if earliest.IsZero() || at.Before(earliest) {
			earliest = at
		}
	}
	return earliest, !earliest.IsZero()
}

// len reports how many distinct keys are pending.
func (q *debounceQueue) size() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.deadline)
}

// webhookServer is the http.Handler behind POST /webhook/{instance-name}.
type webhookServer struct {
	logger   *slog.Logger
	queue    *debounceQueue
	debounce time.Duration
	now      func() time.Time

	// instances maps a configured instance NAME to its type, resolved once at
	// startup. A name that is not in here is an unknown instance.
	instances map[string]string
}

// newWebhookHandler builds the mux serving the webhook endpoint.
//
// The method lives in the ROUTE PATTERN rather than in an r.Method comparison,
// which is both the idiomatic net/http form since Go 1.22 and the reason this
// file names no HTTP method anywhere: a non-POST request to the endpoint gets
// ServeMux's own 405, and the tree-wide write-verb audit (writer_test.go) stays
// a statement about the CLIENT — the only side of this program that can write
// to an *arr.
func newWebhookHandler(s *webhookServer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook/{instance}", s.handleWebhook)
	return mux
}

// handleWebhook is the whole request path: resolve the instance, read a bounded
// body, decode the minimal payload, enqueue, 200. Every exit is 200 (see the
// file comment), and nothing here does I/O beyond reading the request body.
func (s *webhookServer) handleWebhook(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("instance")

	instType, known := s.instances[name]
	if !known {
		// Non-fatal by design, and deliberately not the fatal that --instance
		// gets for the same mistake: a daemon must not die on a stray webhook,
		// and the sender is a different program that may simply be pointed at
		// the wrong URL. Naming the configured instances is what makes the
		// typo fixable from this one line.
		s.logger.Warn("webhook received for an unknown instance name; ignoring it",
			"instance", name, "configured", strings.Join(sortedKeys(s.instances), ", "))
		writeWebhookAccepted(w)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, webhookMaxBodyBytes))
	if err != nil {
		s.logger.Warn("webhook body could not be read; ignoring it",
			"instance", name, "type", instType, "error", err)
		writeWebhookAccepted(w)
		return
	}

	var payload webhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		s.logger.Warn("webhook body is not valid JSON; ignoring it",
			"instance", name, "type", instType, "error", err, "bodyBytes", len(body))
		writeWebhookAccepted(w)
		return
	}

	if payload.EventType == nil {
		// Untrusted input, handled the way §2.6 says: never guess. Without an
		// eventType there is no way to tell an import from a health check.
		s.logger.Warn("webhook payload has no eventType; ignoring it",
			"instance", name, "type", instType)
		writeWebhookAccepted(w)
		return
	}

	switch *payload.EventType {
	case webhookEventTest:
		// The friendly one: this is how the human validates the hookup from
		// the *arr's own UI, so it says plainly that it worked and which
		// instance it came in as.
		s.logger.Info("webhook test received — this instance's hookup is working",
			"instance", name, "type", instType)
		writeWebhookAccepted(w)
		return
	case webhookEventDownload:
		// Fall through: this is the one event type that queues work.
	default:
		s.logger.Debug("webhook event type is not one this project acts on; ignoring it",
			"instance", name, "type", instType, "eventType", *payload.EventType)
		writeWebhookAccepted(w)
		return
	}

	kind := webhookKindMovie
	ref := payload.Movie
	if instType == "sonarr" {
		kind = webhookKindSeries
		ref = payload.Series
	}
	if ref == nil || ref.ID == nil {
		s.logger.Warn("webhook payload carries no id for this instance's item type; ignoring it (nothing can be evaluated without one)",
			"instance", name, "type", instType, "eventType", *payload.EventType, "expectedField", kind+".id")
		writeWebhookAccepted(w)
		return
	}

	key := queueKey{instance: name, kind: kind, id: *ref.ID}
	seasons := payload.affectedSeasons()
	deadline, dropped := s.queue.add(key, seasons, s.now(), s.debounce)

	attrs := []any{"instance", name, "type", instType, "key", key.String(),
		"eventType", *payload.EventType, "debounce", s.debounce, "evaluateAfter", deadline}
	if ref.Title != nil {
		attrs = append(attrs, "title", *ref.Title)
	}
	if len(seasons) > 0 {
		attrs = append(attrs, "affectedSeasons", joinInts(seasons))
	}
	s.logger.Debug("webhook queued; the debounce timer for this item was reset", attrs...)

	if dropped != nil {
		s.logger.Warn("the webhook debounce queue is full; the longest-settling item was dropped and will be picked up by the next reconciliation sweep instead",
			"instance", name, "droppedKey", dropped.String(), "queueLimit", s.queue.limit)
	}

	writeWebhookAccepted(w)
}

// writeWebhookAccepted sends the only response this endpoint ever sends.
func writeWebhookAccepted(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "accepted")
}

// sortedKeys returns a map's keys in sorted order, for a deterministic log
// line naming the configured instances.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// joinInts renders ints as "1,2,3" for a log attribute.
func joinInts(ids []int) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.Itoa(id))
	}
	return strings.Join(parts, ",")
}
