package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- the handler ------------------------------------------------------------
//
// Two properties are asserted over and over in this file, deliberately:
// EVERY request gets 200, and NOTHING in the handler path evaluates anything.
// The *arr on the other end is a different program that is waiting on this
// response — a 4xx/5xx would surface a failing webhook to a human for something
// that is either our problem or none of our business (and some *arr versions
// disable a hook that keeps failing), and an evaluation would make an import
// wait on a full library scan.

// webhookTestOptions are the two seams a handler test may need to move off the
// production values: the queue bound (1024 distinct keys is unreachable from a
// handler test) and the reconciliation interval, which is what decides whether
// a dropped item has anything at all coming back for it.
//
// pollInterval carries the CONFIG's own semantics rather than a helper-local
// remapping: 0 means the webhooks-only daemon, exactly as config.go documents
// it, so a test naming 0 here is naming the setup a human can really deploy.
type webhookTestOptions struct {
	queueLimit   int
	pollInterval time.Duration
}

// newWebhookTestServer builds the handler under test with a fake clock and an
// in-memory queue, returning both so a test can assert on what was queued. It
// is the ordinary case: the production queue bound, and a daemon that does have
// a reconciliation sweep.
func newWebhookTestServer(t *testing.T, instances map[string]string, debounce time.Duration, level slog.Level) (http.Handler, *debounceQueue, *bytes.Buffer, *fakeClock) {
	t.Helper()
	return newWebhookTestServerWith(t, instances, debounce, level,
		webhookTestOptions{queueLimit: webhookQueueLimit, pollInterval: time.Hour})
}

func newWebhookTestServerWith(t *testing.T, instances map[string]string, debounce time.Duration, level slog.Level, opts webhookTestOptions) (http.Handler, *debounceQueue, *bytes.Buffer, *fakeClock) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level}))
	clock := newFakeClock()
	queue := newDebounceQueue(opts.queueLimit)
	handler := newWebhookHandler(&webhookServer{
		logger:       logger,
		queue:        queue,
		debounce:     debounce,
		now:          clock.Now,
		instances:    instances,
		pollInterval: opts.pollInterval,
	})
	return handler, queue, &buf, clock
}

func postWebhook(t *testing.T, handler http.Handler, instance, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook/"+instance, strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestWebhookHandler_Download_Queues200AndTheItemsKey(t *testing.T) {
	handler, queue, buf, _ := newWebhookTestServer(t, map[string]string{"radarr-main": "radarr"}, 45*time.Second, slog.LevelDebug)

	rec := postWebhook(t, handler, "radarr-main", `{"eventType":"Download","isUpgrade":false,"movie":{"id":42,"title":"Some Film"}}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if queue.size() != 1 {
		t.Fatalf("queue size = %d, want 1", queue.size())
	}
	out := buf.String()
	if !strings.Contains(out, "radarr-main/movie/42") {
		t.Errorf("the queued key must be logged in the instance/kind/id form:\n%s", out)
	}
}

// TestWebhookHandler_Upgrade_IsTheSameEventTypeAndIsAccepted pins the payload
// fact the whole design rests on: BOTH "On Import" and "On Upgrade" arrive as
// eventType "Download". Accepting only some other spelling of "upgrade" would
// silently drop half the events this project exists to react to.
func TestWebhookHandler_Upgrade_IsTheSameEventTypeAndIsAccepted(t *testing.T) {
	handler, queue, _, _ := newWebhookTestServer(t, map[string]string{"radarr-main": "radarr"}, 45*time.Second, slog.LevelDebug)

	rec := postWebhook(t, handler, "radarr-main", `{"eventType":"Download","isUpgrade":true,"movie":{"id":42}}`)

	if rec.Code != http.StatusOK || queue.size() != 1 {
		t.Fatalf("status/queue = %d/%d, want 200/1: an upgrade is a Download too", rec.Code, queue.size())
	}
}

// TestWebhookHandler_Test_IsFriendlyAndLoggedAtInfo: this event type exists to
// be seen by a person clicking Test in the *arr's UI, and it is how the human
// will validate the hookup. A debug shrug would leave them staring at an empty
// log wondering whether it worked.
func TestWebhookHandler_Test_IsFriendlyAndLoggedAtInfo(t *testing.T) {
	handler, queue, buf, _ := newWebhookTestServer(t, map[string]string{"sonarr-main": "sonarr"}, 45*time.Second, slog.LevelInfo)

	rec := postWebhook(t, handler, "sonarr-main", `{"eventType":"Test","series":{"id":1}}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if queue.size() != 0 {
		t.Errorf("a Test event queues no work, got %d queued", queue.size())
	}
	out := buf.String()
	if !strings.Contains(out, "level=INFO") || !strings.Contains(out, "webhook test received") {
		t.Errorf("a Test event must be visible at INFO and say plainly that the hookup works:\n%s", out)
	}
	if !strings.Contains(out, "instance=sonarr-main") {
		t.Errorf("the test line must name the instance it arrived as:\n%s", out)
	}
}

// TestWebhookHandler_UnhandledEventType_Is200AndDebugOnly: Grab, Rename,
// HealthIssue and a dozen others arrive at whatever URL the human configured.
// None of them changes a cutoff status, and warning about each would make the
// log unreadable within a day.
func TestWebhookHandler_UnhandledEventType_Is200AndDebugOnly(t *testing.T) {
	handler, queue, buf, _ := newWebhookTestServer(t, map[string]string{"radarr-main": "radarr"}, 45*time.Second, slog.LevelDebug)

	rec := postWebhook(t, handler, "radarr-main", `{"eventType":"Grab","movie":{"id":42}}`)

	if rec.Code != http.StatusOK || queue.size() != 0 {
		t.Fatalf("status/queue = %d/%d, want 200/0", rec.Code, queue.size())
	}
	out := buf.String()
	if !strings.Contains(out, "level=DEBUG") || !strings.Contains(out, "eventType=Grab") {
		t.Errorf("an ignored event type is a debug line naming it:\n%s", out)
	}
	if strings.Contains(out, "level=WARN") {
		t.Errorf("an event type this project does not act on is not a problem:\n%s", out)
	}
}

// TestWebhookHandler_UnknownInstance_Is200AndWarns mirrors --instance's fatal
// for the same mistake, non-fatally: a daemon must not die on a stray webhook,
// and the sender is a different program that may simply be pointed at the wrong
// URL. Naming the configured instances makes the typo fixable from one line.
func TestWebhookHandler_UnknownInstance_Is200AndWarns(t *testing.T) {
	handler, queue, buf, _ := newWebhookTestServer(t, map[string]string{"radarr-main": "radarr", "sonarr-main": "sonarr"}, 45*time.Second, slog.LevelInfo)

	rec := postWebhook(t, handler, "radar-main", `{"eventType":"Download","movie":{"id":42}}`)

	if rec.Code != http.StatusOK || queue.size() != 0 {
		t.Fatalf("status/queue = %d/%d, want 200/0", rec.Code, queue.size())
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "radar-main") {
		t.Errorf("an unknown instance name must warn and name what arrived:\n%s", out)
	}
	if !strings.Contains(out, "radarr-main") || !strings.Contains(out, "sonarr-main") {
		t.Errorf("the warning must name the configured instances so the typo is fixable:\n%s", out)
	}
}

func TestWebhookHandler_MalformedPayloads_AreAll200AndWarn(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"not json at all", `<html>nope</html>`, "not valid JSON"},
		{"json but not an object", `[1,2,3]`, "not valid JSON"},
		{"no eventType", `{"movie":{"id":42}}`, "no eventType"},
		{"eventType is JSON null", `{"eventType":null,"movie":{"id":42}}`, "no eventType"},
		{"download with no movie", `{"eventType":"Download"}`, "no id"},
		{"download with a null movie id", `{"eventType":"Download","movie":{"id":null}}`, "no id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler, queue, buf, _ := newWebhookTestServer(t, map[string]string{"radarr-main": "radarr"}, 45*time.Second, slog.LevelInfo)

			rec := postWebhook(t, handler, "radarr-main", tc.body)

			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200: this project never makes an *arr's webhook look broken", rec.Code)
			}
			if queue.size() != 0 {
				t.Errorf("nothing may be queued from a payload that could not be read, got %d", queue.size())
			}
			out := buf.String()
			if !strings.Contains(out, "level=WARN") || !strings.Contains(out, tc.want) {
				t.Errorf("expected a WARN containing %q:\n%s", tc.want, out)
			}
		})
	}
}

// TestWebhookHandler_SonarrPayload_UsesSeriesIdAndRecordsTheAffectedSeasons:
// the KEY is the series (that is what the debounce collapses a season pack
// into), and the season numbers are recorded as evidence of what triggered it.
func TestWebhookHandler_SonarrPayload_UsesSeriesIdAndRecordsTheAffectedSeasons(t *testing.T) {
	handler, queue, buf, clock := newWebhookTestServer(t, map[string]string{"sonarr-main": "sonarr"}, 45*time.Second, slog.LevelDebug)

	rec := postWebhook(t, handler, "sonarr-main",
		`{"eventType":"Download","series":{"id":7,"title":"A Show"},"episodes":[{"seasonNumber":2},{"seasonNumber":2},{"seasonNumber":3}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	due, seasons := queue.expired(clock.Now().Add(time.Hour))
	if len(due) != 1 || due[0] != (queueKey{instance: "sonarr-main", kind: webhookKindSeries, id: 7}) {
		t.Fatalf("queued key = %+v, want sonarr-main/series/7", due)
	}
	if got := joinInts(seasons[due[0]]); got != "2,3" {
		t.Errorf("affected seasons = %q, want %q (deduplicated and sorted)", got, "2,3")
	}
	if !strings.Contains(buf.String(), "affectedSeasons=2,3") {
		t.Errorf("what triggered the cycle must be visible in the log:\n%s", buf.String())
	}
}

// TestWebhookHandler_SonarrPayload_NoEpisodes_StillQueuesTheSeries: an absent
// or empty episodes array is not a reason to ignore the event — the series is
// what gets evaluated either way.
func TestWebhookHandler_SonarrPayload_NoEpisodes_StillQueuesTheSeries(t *testing.T) {
	handler, queue, _, _ := newWebhookTestServer(t, map[string]string{"sonarr-main": "sonarr"}, 45*time.Second, slog.LevelDebug)

	if rec := postWebhook(t, handler, "sonarr-main", `{"eventType":"Download","series":{"id":7}}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if queue.size() != 1 {
		t.Errorf("queue size = %d, want 1", queue.size())
	}
}

// TestWebhookHandler_RadarrPayloadToASonarrEndpoint_IsIgnoredWithAWarning:
// the item kind comes from the CONFIGURED instance type, never from sniffing
// the payload. The endpoint an event arrived on is what says which library it
// is about, and a mismatch is a misconfiguration to surface rather than a shape
// to guess at.
func TestWebhookHandler_RadarrPayloadToASonarrEndpoint_IsIgnoredWithAWarning(t *testing.T) {
	handler, queue, buf, _ := newWebhookTestServer(t, map[string]string{"sonarr-main": "sonarr"}, 45*time.Second, slog.LevelInfo)

	rec := postWebhook(t, handler, "sonarr-main", `{"eventType":"Download","movie":{"id":42}}`)

	if rec.Code != http.StatusOK || queue.size() != 0 {
		t.Fatalf("status/queue = %d/%d, want 200/0", rec.Code, queue.size())
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "series.id") {
		t.Errorf("the warning must name the field that was expected for this instance's type:\n%s", out)
	}
}

// TestWebhookHandler_NonPostMethod_Is405NotAQueuedEvent: the endpoint is
// POST-only, expressed in the route pattern so this file names no HTTP method
// anywhere (see newWebhookHandler and the tree-wide write-verb audit). A GET
// probe from a browser or a health checker must not queue work.
func TestWebhookHandler_NonPostMethod_Is405NotAQueuedEvent(t *testing.T) {
	handler, queue, _, _ := newWebhookTestServer(t, map[string]string{"radarr-main": "radarr"}, 45*time.Second, slog.LevelInfo)

	req := httptest.NewRequest(http.MethodGet, "/webhook/radarr-main", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
	if queue.size() != 0 {
		t.Errorf("a non-POST must queue nothing, got %d", queue.size())
	}
}

// TestWebhookHandler_OversizedBody_IsBoundedNotBuffered: the body is read
// through a limit reader, so a hostile or runaway sender cannot make this
// process buffer arbitrary memory on a request it answers 200 to regardless.
func TestWebhookHandler_OversizedBody_IsBoundedNotBuffered(t *testing.T) {
	handler, queue, _, _ := newWebhookTestServer(t, map[string]string{"radarr-main": "radarr"}, 45*time.Second, slog.LevelInfo)

	body := `{"eventType":"Download","movie":{"id":42},"padding":"` + strings.Repeat("x", webhookMaxBodyBytes+1024) + `"}`
	rec := postWebhook(t, handler, "radarr-main", body)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 even for a body we refuse to read in full", rec.Code)
	}
	// Truncated at the limit, the JSON no longer parses — so nothing is queued
	// and the refusal is the ordinary malformed-payload one.
	if queue.size() != 0 {
		t.Errorf("a truncated body must not be acted on, got %d queued", queue.size())
	}
}

// --- the debounce queue -----------------------------------------------------

func TestDebounceQueue_RepeatedEventsForOneKey_ResetTheTimerRatherThanStacking(t *testing.T) {
	q := newDebounceQueue(webhookQueueLimit)
	clock := newFakeClock()
	key := queueKey{instance: "sonarr-main", kind: webhookKindSeries, id: 7}

	q.add(key, nil, clock.Now(), 45*time.Second)
	clock.Advance(30 * time.Second)
	q.add(key, nil, clock.Now(), 45*time.Second) // 15s before the first would fire

	if q.size() != 1 {
		t.Fatalf("size = %d, want 1: the same item is one key, however many events name it", q.size())
	}
	// The original deadline has passed, but the reset moved it.
	clock.Advance(20 * time.Second) // t=50s: past the first deadline, before the second
	if due, _ := q.expired(clock.Now()); len(due) != 0 {
		t.Fatalf("the timer must have been RESET by the second event, got %v due at t=50s", due)
	}
	clock.Advance(30 * time.Second) // t=80s: past the reset deadline (75s)
	if due, _ := q.expired(clock.Now()); len(due) != 1 {
		t.Fatalf("expected the key to fire once after the reset debounce, got %v", due)
	}
	if q.size() != 0 {
		t.Errorf("an expired key is removed, got %d still queued", q.size())
	}
}

func TestDebounceQueue_DistinctKeysDoNotCollapse(t *testing.T) {
	q := newDebounceQueue(webhookQueueLimit)
	clock := newFakeClock()
	q.add(queueKey{instance: "radarr-main", kind: webhookKindMovie, id: 1}, nil, clock.Now(), time.Minute)
	q.add(queueKey{instance: "radarr-main", kind: webhookKindMovie, id: 2}, nil, clock.Now(), time.Minute)
	q.add(queueKey{instance: "radarr-4k", kind: webhookKindMovie, id: 1}, nil, clock.Now(), time.Minute)

	if q.size() != 3 {
		t.Fatalf("size = %d, want 3: two movies of one instance and the same id in another are three different things", q.size())
	}
	clock.Advance(2 * time.Minute)
	due, _ := q.expired(clock.Now())
	if len(due) != 3 {
		t.Fatalf("expected all three keys due, got %v", due)
	}
	// Deterministic order (instance, then id) so a coalesced cycle's logs and
	// scope rendering never depend on map iteration.
	want := []string{"radarr-4k/movie/1", "radarr-main/movie/1", "radarr-main/movie/2"}
	for i, k := range due {
		if k.String() != want[i] {
			t.Errorf("due[%d] = %s, want %s", i, k.String(), want[i])
		}
	}
}

// TestDebounceQueue_Overflow_DropsTheLongestSettlingItemAndReportsIt: the queue
// is fed by an outside system, so it is bounded. The entry dropped is the one
// with the earliest deadline — the longest-settling, and therefore the one
// closest to being evaluated anyway. Nothing is lost permanently: the
// reconciliation sweep covers it, exactly as it covers a queue lost to a
// restart.
func TestDebounceQueue_Overflow_DropsTheLongestSettlingItemAndReportsIt(t *testing.T) {
	q := newDebounceQueue(3)
	clock := newFakeClock()
	for i := 1; i <= 3; i++ {
		q.add(queueKey{instance: "radarr-main", kind: webhookKindMovie, id: i}, nil, clock.Now(), time.Minute)
		clock.Advance(time.Second)
	}

	_, dropped := q.add(queueKey{instance: "radarr-main", kind: webhookKindMovie, id: 4}, nil, clock.Now(), time.Minute)

	if q.size() != 3 {
		t.Errorf("size = %d, want the limit of 3", q.size())
	}
	if dropped == nil || dropped.id != 1 {
		t.Fatalf("dropped = %v, want the earliest-deadline key (movie 1)", dropped)
	}
	// A repeat of an ALREADY-QUEUED key must not evict anything: it is a reset,
	// not a new entry.
	if _, d := q.add(queueKey{instance: "radarr-main", kind: webhookKindMovie, id: 2}, nil, clock.Now(), time.Minute); d != nil {
		t.Errorf("resetting an existing key evicted %v; only a NEW key can overflow the queue", d)
	}
}

// TestDebounceQueue_SeasonNumbersAccumulateAcrossEventsForOneKey: a season pack
// arrives as many events, and the seasons they name are evidence about the same
// key. Losing all but the last would make the log say a 24-episode import
// touched one season.
func TestDebounceQueue_SeasonNumbersAccumulateAcrossEventsForOneKey(t *testing.T) {
	q := newDebounceQueue(webhookQueueLimit)
	clock := newFakeClock()
	key := queueKey{instance: "sonarr-main", kind: webhookKindSeries, id: 7}

	q.add(key, []int{2}, clock.Now(), time.Minute)
	q.add(key, []int{3, 2}, clock.Now(), time.Minute)

	clock.Advance(2 * time.Minute)
	due, seasons := q.expired(clock.Now())
	if len(due) != 1 {
		t.Fatalf("expected one key, got %v", due)
	}
	if got := joinInts(seasons[due[0]]); got != "2,3" {
		t.Errorf("accumulated seasons = %q, want %q", got, "2,3")
	}
}

// --- overflow, through the handler ------------------------------------------
//
// The queue's own overflow behavior is unit-tested above, but the WARN that
// TELLS A HUMAN about it lives in the handler, and until these two tests
// existed nothing executed it: every handler test built the queue with the
// production limit of 1024, which no handler test can reach. Deleting the
// entire `if dropped != nil { ... }` block left the suite green — turning a
// queue overflow into a silent drop, which is the one thing the bound must
// never be.

// TestWebhookHandler_QueueOverflow_WarnsAndNamesTheSweepThatWillCoverIt is the
// ordinary daemon: something was dropped, and the reconciliation sweep really
// is coming for it.
func TestWebhookHandler_QueueOverflow_WarnsAndNamesTheSweepThatWillCoverIt(t *testing.T) {
	const limit = 3
	handler, queue, buf, clock := newWebhookTestServerWith(t, map[string]string{"radarr-main": "radarr"}, 45*time.Second, slog.LevelInfo,
		webhookTestOptions{queueLimit: limit, pollInterval: time.Hour})

	// limit+1 DISTINCT items: the first `limit` fill the queue, the last one
	// evicts the longest-settling entry (movie 1, whose deadline is earliest).
	// The clock moves between events because that is what makes one of them the
	// longest-settling: events that share a timestamp to the nanosecond are all
	// equally old, and which of THOSE is dropped is not a property worth
	// asserting.
	for id := 1; id <= limit+1; id++ {
		body := fmt.Sprintf(`{"eventType":"Download","movie":{"id":%d,"title":"Film %d"}}`, id, id)
		if rec := postWebhook(t, handler, "radarr-main", body); rec.Code != http.StatusOK {
			t.Fatalf("movie %d: status = %d, want 200 — an overflow is OUR capacity problem, never the sender's", id, rec.Code)
		}
		clock.Advance(time.Second)
	}

	if queue.size() != limit {
		t.Errorf("queue size = %d, want the limit of %d", queue.size(), limit)
	}
	out := buf.String()
	if n := strings.Count(out, "the webhook debounce queue is full"); n != 1 {
		t.Fatalf("expected exactly one overflow WARN, got %d:\n%s", n, out)
	}
	for _, want := range []string{"level=WARN", "droppedKey=radarr-main/movie/1", "queueLimit=3", "reconciliation sweep"} {
		if !strings.Contains(out, want) {
			t.Errorf("the overflow WARN must carry %q — a human needs to know WHAT was dropped and what is coming back for it:\n%s", want, out)
		}
	}
}

// TestWebhookHandler_QueueOverflow_WithNoReconciliationSweep_SaysNothingWillRevisitIt
// is the same overflow in the configuration config.go explicitly permits:
// poll_interval 0, the webhooks-only daemon. There IS no reconciliation sweep,
// so the comforting half of the ordinary message would be a lie — the dropped
// key really is gone until the process restarts, and an operator who reads
// "the next sweep will pick it up" would close the log and move on.
func TestWebhookHandler_QueueOverflow_WithNoReconciliationSweep_SaysNothingWillRevisitIt(t *testing.T) {
	const limit = 2
	handler, _, buf, clock := newWebhookTestServerWith(t, map[string]string{"sonarr-main": "sonarr"}, 45*time.Second, slog.LevelInfo,
		webhookTestOptions{queueLimit: limit, pollInterval: 0})

	for id := 1; id <= limit+1; id++ {
		body := fmt.Sprintf(`{"eventType":"Download","series":{"id":%d}}`, id)
		if rec := postWebhook(t, handler, "sonarr-main", body); rec.Code != http.StatusOK {
			t.Fatalf("series %d: status = %d, want 200", id, rec.Code)
		}
		clock.Advance(time.Second)
	}

	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "droppedKey=sonarr-main/series/1") {
		t.Fatalf("the overflow must still warn and name the key it dropped:\n%s", out)
	}
	if strings.Contains(out, "reconciliation sweep instead") {
		t.Errorf("with poll_interval 0 there is no sweep; promising one is the misdirecting error path this project refuses everywhere else:\n%s", out)
	}
	for _, want := range []string{"restarted", "poll_interval is 0"} {
		if !strings.Contains(out, want) {
			t.Errorf("the webhooks-only overflow WARN must say %q — that the item is gone until a restart, and why:\n%s", want, out)
		}
	}
}

// TestWebhookHandler_PathNamingNoSingleInstance_IsStill200AndWarns closes a
// silent hole found in self-review. {instance} matches exactly one non-empty
// segment, so "/webhook/" and "/webhook/radarr/main" — the latter being what an
// *arr posts to when a configured instance NAME contains a slash, which the
// config permits — fell through to ServeMux's 404 and this program never knew
// an event had been aimed at it.
func TestWebhookHandler_PathNamingNoSingleInstance_IsStill200AndWarns(t *testing.T) {
	for _, path := range []string{"/webhook/", "/webhook/radarr/main"} {
		t.Run(path, func(t *testing.T) {
			handler, queue, buf, _ := newWebhookTestServer(t, map[string]string{"radarr-main": "radarr"}, 45*time.Second, slog.LevelInfo)

			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"eventType":"Download","movie":{"id":1}}`))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", rec.Code)
			}
			if queue.size() != 0 {
				t.Errorf("nothing may be queued from a path that names no instance, got %d", queue.size())
			}
			out := buf.String()
			if !strings.Contains(out, "level=WARN") || !strings.Contains(out, path) {
				t.Errorf("the warning must quote the path so the misconfiguration is fixable:\n%s", out)
			}
		})
	}
}
