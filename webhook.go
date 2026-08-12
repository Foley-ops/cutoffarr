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
	EventType *string         `json:"eventType"`
	Movie     *webhookItemRef `json:"movie"`
	Series    *webhookItemRef `json:"series"`

	// Episodes keeps its RAW bytes, and it is the one field here that cannot
	// use the pointer trick the rest of the struct relies on. A pointer tells
	// an absent key from a present one for every JSON value EXCEPT null:
	// encoding/json leaves a pointer field nil when the key is missing and
	// SETS it nil when the key is present as null, so `*[]webhookEpisodeRef`
	// decoded `{"episodes":null}` and a body with no episodes key at all to the
	// same nil.
	//
	// Those two are not the same event (see episodesShape). An absent array is a
	// payload that never spoke about episodes and takes the mandated
	// whole-series claim silently; a null is a VALUE, read in the position that
	// decides how much of a series may be unmonitored, and it takes the same
	// claim only with a WARN that says so out loud. This is the field that
	// decides that scope, so the distinction the decoder cannot make is made
	// from the bytes instead, in episodeRecords.
	Episodes json.RawMessage `json:"episodes"`
}

type webhookItemRef struct {
	ID    *int    `json:"id"`
	Title *string `json:"title"`
}

type webhookEpisodeRef struct {
	SeasonNumber *int `json:"seasonNumber"`
}

// scopeClaim is what ONE accepted event says about how much of its item must
// be evaluated — which, since FIX 3, is also how much of it may be WRITTEN.
//
// It exists because "the payload named no seasons" is not one situation but
// two, and they are opposites. Collapsing them (which is what returning a bare
// nil slice did) meant a single unreadable episode record anywhere in a
// 24-event season-pack burst escalated the write scope for the whole burst from
// the affected season to every eligible season of the series — sticky, so no
// later well-formed event could narrow it back — and did so silently, one
// debounce later, in a cycle whose per-item report lines are demoted to DEBUG.
type scopeClaim int

const (
	// claimWholeItem: the event named no seasons because there were none to
	// name. Every Radarr payload (a movie has no seasons), and the Sonarr
	// payload whose episodes array is absent, empty, or an explicit JSON null,
	// all of which binding controller resolution 2 defines as "evaluate all
	// seasons of the series". This is a real, mandated claim on the whole item.
	// The null reaches it with a WARN the other two do not need (see
	// warnNullEpisodes); the CLAIM is identical, and only a claim reaches the
	// queue.
	claimWholeItem scopeClaim = iota

	// claimSeasons: the event named at least one readable season number. Those
	// seasons, and no others.
	claimSeasons

	// claimNothing: the episodes key was PRESENT and this program could not
	// read a season out of it — either a non-empty array not one element of
	// which yielded a season number, or a value that is not an array of episode
	// records at all (a string, an object). That is not a statement about the
	// series; it is a payload this program could not read, and §2.6's rule for
	// untrusted input is to refuse it loudly rather than guess. Guessing "the
	// whole series" here is the widest possible guess about an unmonitor.
	claimNothing
)

func (c scopeClaim) String() string {
	switch c {
	case claimWholeItem:
		return "whole-item"
	case claimSeasons:
		return "named-seasons"
	default:
		return "none-unreadable"
	}
}

// affectedSeasons returns the deduplicated, sorted season numbers a Sonarr
// payload's episodes name, how many episode records carried NO readable
// seasonNumber, and what the payload therefore claims.
//
// The seasons are the WRITE SCOPE, which is the plan's own granularity: "a
// webhook event evaluates only that movie (Radarr) or that series' affected
// season (Sonarr)". The EVIDENCE the evaluation rests on is unchanged and still
// whole-library — the full-evidence ruling (controller resolution 4) is about
// what is read and cross-checked, not about how much may be written — so a
// webhook cycle still costs a full instance scan, bounded by the debounce.
//
// The unreadable count is returned rather than swallowed because the caller has
// to WARN about it: it is untrusted input in the position that decides how much
// this program may unmonitor, and the one thing §2.6 never permits there is
// silence. A partially unreadable payload still yields its readable seasons —
// those are a real claim — and the records that named nothing widen nothing.
//
// The count is RECORDS, so it is 0 for a value that is not an array of records
// at all; claimNothing is what the caller warns on, and the count only says
// which sentence to print.
func (p webhookPayload) affectedSeasons() (seasons []int, unreadable int, claim scopeClaim, shape episodesShape) {
	records, shape := p.episodeRecords()
	switch shape {
	case episodesAbsent:
		// No episodes key at all: the mandated whole-series claim.
		return nil, 0, claimWholeItem, shape
	case episodesNull:
		// The key is present and explicitly null. See episodesNull for why this
		// is the mandated fallback rather than a refusal, and the caller for the
		// WARN that fallback owes.
		return nil, 0, claimWholeItem, shape
	case episodesUnreadable:
		// The key is there and holds something this program cannot read. There
		// are no records to count as unreadable — the whole VALUE is — so the
		// count is 0 and the claim is what carries the refusal. See the caller,
		// which warns on the claim rather than on the count for exactly this.
		return nil, 0, claimNothing, shape
	}
	if len(records) == 0 {
		// An empty array, which resolution 2 defines the same way as an absent
		// one: evaluate all seasons of the series.
		return nil, 0, claimWholeItem, shape
	}
	for _, e := range records {
		if e.SeasonNumber != nil {
			seasons = append(seasons, *e.SeasonNumber)
			continue
		}
		unreadable++
	}
	if len(seasons) == 0 {
		return nil, unreadable, claimNothing, shape
	}
	return dedupeSortedIDs(seasons), unreadable, claimSeasons, shape
}

// episodesShape is what the episodes KEY turned out to hold, which is a
// separate question from what its records say. Only this level can tell an
// absent key from an explicit null, and only that distinction separates the
// widest claim this program makes from a refusal to make any.
type episodesShape int

const (
	// episodesAbsent: no episodes key in the body at all.
	episodesAbsent episodesShape = iota
	// episodesReadable: a JSON array, possibly empty, whose elements decoded.
	episodesReadable
	// episodesNull: the key is present and holds an explicit JSON null — what a
	// serializer emits for an empty collection, and therefore what an *arr event
	// carries when it has no episodes to name and says so by value rather than
	// by omission.
	//
	// It is UNTRUSTED INPUT in the position that decides how much of a series
	// may be unmonitored, and the binding ruling on it is neither "refuse" nor
	// "wave through": WARN naming the field, then fall back to the same
	// whole-series scope an absent key gets, provided the rest of the payload
	// validated (which the handler establishes before ever asking — an event
	// with no series id never reaches here). The fallback is the mandated one;
	// what the null changes is that the widest scope this program takes is
	// stated out loud rather than assumed, so it can never again be
	// indistinguishable from the legitimate absent case.
	episodesNull
	// episodesUnreadable: the key is present and holds something that is not
	// an array of episode records — a string, an object, or an array whose
	// elements are not objects. Unlike a null, none of these is a shape an *arr
	// has any reason to send, so none of them is read as a claim at all.
	episodesUnreadable
)

// episodeRecords decodes the raw episodes value into those four shapes.
//
// The nil check on the decoded slice is the null case and the whole point of
// keeping the bytes: `json.Unmarshal([]byte("null"), &records)` succeeds and
// leaves records nil, while an empty array yields a non-nil empty slice. That
// is the only signal in the standard library that separates a present null from
// an absent key, and this program has to separate them — not because they claim
// different scopes (both end at the whole series) but because one of them is a
// value that was READ and must be reported as such.
func (p webhookPayload) episodeRecords() ([]webhookEpisodeRef, episodesShape) {
	if len(p.Episodes) == 0 {
		return nil, episodesAbsent
	}
	var records []webhookEpisodeRef
	if err := json.Unmarshal(p.Episodes, &records); err != nil {
		return nil, episodesUnreadable
	}
	if records == nil {
		return nil, episodesNull
	}
	return records, episodesReadable
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

	// wholeItem records the keys for which some event claimed the ENTIRE item
	// rather than particular seasons — a Sonarr payload with no episodes array
	// (binding controller resolution 2: "absent/empty episodes array →
	// evaluate all seasons of the series"), and every Radarr payload, which has
	// no seasons to name.
	//
	// It is STICKY, and that is the whole point. A burst for one series can
	// mix events that name their seasons with events that do not, and those two
	// kinds of event make different claims: "season 2 changed" and "something
	// in this series changed". Accumulating only the named numbers would let the
	// event that claimed the MOST silently narrow the scope, so once a key has
	// been claimed whole it stays whole until it is evaluated. Widening is the
	// safe direction for a write scope; narrowing on the strength of an event
	// that said nothing about seasons is not.
	wholeItem map[queueKey]bool

	// notify carries "something changed, recompute your wait" to the daemon
	// loop. Buffered with room for one, and sent to non-blockingly: the loop
	// only ever needs to know THAT something changed.
	notify chan struct{}
}

func newDebounceQueue(limit int) *debounceQueue {
	return &debounceQueue{
		deadline:  make(map[queueKey]time.Time),
		seasons:   make(map[queueKey][]int),
		wholeItem: make(map[queueKey]bool),
		limit:     limit,
		notify:    make(chan struct{}, 1),
	}
}

// add records an event for key, (re)setting its timer to fire debounce from
// now. It returns the deadline it set, whether anything is now queued for the
// key, and — when the queue was full and something had to go — the key it
// dropped.
//
// THE CLAIM IS PASSED IN, not inferred from len(seasons), and that is the whole
// of FIX 3's follow-up. "No seasons" used to mean one thing here (claim the
// whole item) and reach this function from two places that meant opposite
// things by it. A claimNothing event — episodes present, none readable — makes
// NO claim: it does not widen the key to the whole item, it does not narrow or
// discard whatever an earlier well-formed event accumulated, and it does not
// create a key that did not already exist. All it can do is reset the timer of
// an item something is already known to have happened to, which is exactly the
// amount of trust an unreadable payload has earned.
//
// queued is false only for a claimNothing event naming an item with nothing
// pending: nothing was recorded, and the caller says so.
//
// The bound exists because the queue is fed by an outside system: a runaway
// *arr, a mass import, or a misconfigured hook could otherwise grow it without
// limit. When a NEW key arrives at a full queue, the entry with the EARLIEST
// deadline is dropped — the one that has been settling longest, and therefore
// the one closest to being evaluated anyway.
//
// WHAT COMES BACK FOR A DROPPED KEY depends on the configuration, and the
// caller's WARN says which (see handleWebhook). With a reconciliation sweep
// configured, nothing is lost for longer than one poll_interval. With
// poll_interval 0 — the legal, documented webhooks-only daemon — nothing is
// scheduled that would revisit it, and the key is gone until the process
// restarts and its startup scan reads the whole library again. That is a real
// difference in consequence, so it is never papered over with one message.
func (q *debounceQueue) add(key queueKey, seasons []int, claim scopeClaim, now time.Time, debounce time.Duration) (deadline time.Time, dropped *queueKey, queued bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	_, existing := q.deadline[key]
	if claim == claimNothing && !existing {
		// Nothing readable arrived and nothing is pending for this item, so
		// there is nothing to reset and no claim to record. Creating the key
		// here would be creating it with an empty scope, which downstream reads
		// as EVERY season — the widening this claim exists to refuse.
		return time.Time{}, nil, false
	}
	if !existing && len(q.deadline) >= q.limit {
		var oldest queueKey
		var oldestAt time.Time
		for k, at := range q.deadline {
			if oldestAt.IsZero() || at.Before(oldestAt) {
				oldest, oldestAt = k, at
			}
		}
		delete(q.deadline, oldest)
		delete(q.seasons, oldest)
		delete(q.wholeItem, oldest)
		dropped = &oldest
	}

	deadline = now.Add(debounce)
	q.deadline[key] = deadline
	switch claim {
	case claimWholeItem:
		// See wholeItem: the claim is sticky and anything already accumulated
		// is discarded, because a set of season numbers can only ever be
		// narrower than "all of it".
		q.wholeItem[key] = true
		delete(q.seasons, key)
	case claimSeasons:
		if !q.wholeItem[key] {
			q.seasons[key] = dedupeSortedIDs(append(q.seasons[key], seasons...))
		}
	case claimNothing:
		// The timer reset above is the ONLY effect. The accumulated scope is
		// left exactly as the readable events left it.
	}

	select {
	case q.notify <- struct{}{}:
	default:
	}
	return deadline, dropped, true
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
		// A key claimed whole yields NO seasons, which downstream reads as
		// "every season of this item" — the same thing an absent entry means.
		if s := q.seasons[k]; len(s) > 0 && !q.wholeItem[k] {
			seasons[k] = s
		}
		delete(q.deadline, k)
		delete(q.seasons, k)
		delete(q.wholeItem, k)
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

	// pollInterval is cfg.PollInterval, and the handler needs it for exactly
	// one thing: the overflow WARN promises a recovery, and whether that
	// recovery EXISTS depends on this value. 0 is the legal, documented
	// webhooks-only daemon, and in that configuration a dropped key is gone
	// until the process restarts. See the drop path in handleWebhook.
	pollInterval time.Duration
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
	// The catch-all under the same prefix. {instance} matches exactly one
	// non-empty path segment, so a POST to "/webhook/" or to
	// "/webhook/radarr/main" (an instance NAME containing a slash — legal in
	// the config, and silently un-routable without this) would otherwise fall
	// through to ServeMux's own 404 and this program would never know an event
	// had been aimed at it. The more specific pattern above still wins for
	// every well-formed request.
	mux.HandleFunc("POST /webhook/", s.handleUnroutable)
	return mux
}

// handleUnroutable answers a POST under /webhook/ that names no single
// instance segment. 200, like every other exit, with the path quoted so the
// misconfiguration is fixable from the one line.
func (s *webhookServer) handleUnroutable(w http.ResponseWriter, r *http.Request) {
	s.logger.Warn("webhook received on a path that names no single instance; ignoring it (the endpoint is POST /webhook/{instance-name}, and an instance name containing a slash can never be routed to)",
		"path", r.URL.Path, "configured", strings.Join(sortedKeys(s.instances), ", "))
	writeWebhookAccepted(w)
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

	// The seasons are consulted ONLY for a sonarr, because kind comes from the
	// configured instance type and a movie has no seasons to name: an episodes
	// array arriving on a radarr's endpoint is part of the same misconfiguration
	// the ref check above surfaces, and must not be allowed to make a claim.
	seasons, unreadable, claim, shape := []int(nil), 0, claimWholeItem, episodesAbsent
	if kind == webhookKindSeries {
		seasons, unreadable, claim, shape = payload.affectedSeasons()
	}
	deadline, dropped, queued := s.queue.add(key, seasons, claim, s.now(), s.debounce)
	switch {
	case shape == episodesNull:
		// The one shape that makes the WIDEST claim and is nonetheless a value
		// this program read rather than one it assumed. It is warned about
		// FIRST, and separately, precisely because its claim is indistinguishable
		// from the legitimate absent case everywhere else in this log line.
		s.warnNullEpisodes(name, instType, key)
	case unreadable > 0 || claim == claimNothing:
		// The WARN is owed for every claimNothing, not merely for every
		// unreadable RECORD: an episodes key holding a string or an object has
		// no records to count.
		s.warnUnreadableEpisodes(name, instType, key, unreadable, seasons, claim, queued)
	}
	if !queued {
		// claimNothing for an item with nothing pending: the warning above has
		// already said what happened and what covers it. Nothing was queued, so
		// there is no deadline to report.
		writeWebhookAccepted(w)
		return
	}

	attrs := []any{"instance", name, "type", instType, "key", key.String(),
		"eventType", *payload.EventType, "debounce", s.debounce, "evaluateAfter", deadline,
		// scopeClaim is what this event said about the write scope, and it is
		// logged unconditionally because its most important values are the ones
		// that name no seasons: without it, a whole-series claim and a payload
		// whose seasons could not be read produced byte-identical lines.
		"scopeClaim", claim.String()}
	if ref.Title != nil {
		attrs = append(attrs, "title", *ref.Title)
	}
	if len(seasons) > 0 {
		attrs = append(attrs, "affectedSeasons", joinInts(seasons))
	}
	s.logger.Debug("webhook queued; the debounce timer for this item was reset", attrs...)

	if dropped != nil {
		// The message states the RECOVERY, and the recovery is not the same in
		// both supported configurations — so the message is not either. A
		// daemon with a reconciliation sweep really will come back for the
		// dropped item; a webhooks-only daemon (poll_interval 0, which
		// config.go documents as legal) has nothing scheduled that would, and
		// the item is gone until the process restarts and its startup scan
		// covers the whole library again. Telling an operator that a sweep will
		// pick it up when no sweep exists is the misdirecting error path this
		// project refuses everywhere else: they would read the WARN, conclude
		// the item is covered, and stop looking.
		msg := "the webhook debounce queue is full; the longest-settling item was dropped and will be picked up by the next reconciliation sweep instead"
		if s.pollInterval <= 0 {
			msg = "the webhook debounce queue is full; the longest-settling item was dropped and NOTHING will revisit it until this daemon is restarted, because poll_interval is 0 and this daemon therefore has no reconciliation sweep"
		}
		s.logger.Warn(msg,
			"instance", name, "droppedKey", dropped.String(), "queueLimit", s.queue.limit, "pollInterval", s.pollInterval)
	}

	writeWebhookAccepted(w)
}

// warnNullEpisodes is the noise an explicitly null episodes key is required to
// make, and it is a WARN about something this program then goes ahead and DOES
// — which is unusual enough here to state why.
//
// The binding ruling is that a null episodes key falls back to the same
// whole-series scope an absent key gets, on the condition the handler has
// already enforced above: a payload with no series id never reaches this point,
// so the fallback only ever applies to an event that otherwise validated. What
// separates it from the absent case is not the scope but the EVIDENCE — an
// absent key is a payload that never spoke about episodes, while a null is a
// value in the position that decides how much of a series may be unmonitored,
// and §2.6 does not let this program read such a value silently.
//
// So the line's whole job is to make the wide claim visible: it names the field,
// says what the value was, and states the consequence in seasons rather than in
// JSON, because "every eligible season of this series may be unmonitored by the
// cycle this event triggered" is the fact an operator needs and "episodes was
// null" is not.
func (s *webhookServer) warnNullEpisodes(name, instType string, key queueKey) {
	s.logger.Warn("webhook payload: the episodes key is present and holds an explicit JSON null, so it names no season; the rest of the payload validated, so this event falls back to the whole-series scope an absent episodes array gets — meaning EVERY eligible season of this series, not one affected season, is in the write scope of the cycle it triggered",
		"instance", name, "type", instType, "key", key.String(), "field", "episodes",
		"scopeClaim", claimWholeItem.String(), "writeScope", "every eligible season of this series")
}

// warnUnreadableEpisodes is the noise an episode record with no readable
// seasonNumber is required to make.
//
// It is a WARN and not a debug line for the same reason the skip lines in §2.6
// are: this is untrusted input in the position that decides how much of a
// series this program may unmonitor, and the failure mode it replaces was
// invisible — an unreadable record silently widened the write scope for the
// whole burst and the log said nothing that a well-formed no-episodes event
// would not also have said.
//
// The message states the CONSEQUENCE rather than the fact, because the fact is
// not actionable on its own, and the consequence differs by case: a partially
// readable payload still has a scope, an entirely unreadable one has none and
// may not have queued anything at all.
func (s *webhookServer) warnUnreadableEpisodes(name, instType string, key queueKey, unreadable int, seasons []int, claim scopeClaim, queued bool) {
	attrs := []any{"instance", name, "type", instType, "key", key.String()}
	if unreadable > 0 {
		attrs = append(attrs, "episodeRecordsWithNoSeasonNumber", unreadable)
	}

	if claim == claimSeasons {
		s.logger.Warn("webhook payload contains episode records with no seasonNumber; those records name no season, so they are ignored and this event's write scope is only the seasons that WERE readable (they do not widen it to the whole series)",
			append(attrs, "writeScopeSeasons", joinInts(seasons))...)
		return
	}

	// claimNothing, reached two ways, and the line says which. Either a
	// non-empty episodes array yielded no season number at all, or the episodes
	// key held something that is not an array of episode records at all. A JSON
	// null is deliberately NOT one of those: it is a shape a serializer really
	// emits for an empty collection, so it has its own ruling and its own line
	// (warnNullEpisodes). What is left here is a string, an object, or an array
	// of things that are not episode records — none of which an *arr has any
	// reason to send, and none of which is read as a claim.
	cause := "the episodes array names no readable seasonNumber at all"
	if unreadable == 0 {
		cause = "the episodes key is present but holds something that is not an array of episode records at all (a string, an object, or an array of values that are not episode records)"
	}
	// Saying WHY this is not treated as a whole-series claim matters, because
	// the neighbouring, legitimate case (no episodes key at all) IS one, and an
	// operator reading this line needs to know the difference is deliberate.
	const refusal = "; this is NOT read as a claim on the whole series (that would widen an unmonitor from the affected season to every season of it on the strength of input that could not be read)"
	if queued {
		s.logger.Warn("webhook payload: "+cause+refusal+", so only this item's debounce timer was reset and the season scope its earlier, readable events established is unchanged",
			attrs...)
		return
	}
	// Nothing was pending for the item, so nothing was queued. What covers it
	// instead depends on the configuration, exactly as the queue-overflow WARN
	// does, and for the same reason: telling an operator a sweep will pick it up
	// when no sweep exists would make them stop looking.
	coveredBy := "the next reconciliation sweep, which reads this instance's whole library"
	if s.pollInterval <= 0 {
		coveredBy = "nothing until this daemon is restarted: poll_interval is 0, so it has no reconciliation sweep, and only a startup scan reads the whole library"
	}
	s.logger.Warn("webhook payload: "+cause+refusal+", and nothing was pending for this item, so nothing was queued for it",
		append(attrs, "coveredBy", coveredBy, "pollInterval", s.pollInterval)...)
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
