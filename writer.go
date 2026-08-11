package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
)

// monitoredKey is the one and only JSON key this project ever modifies, on
// the one and only object it ever writes back. Naming it once, here, is
// deliberate: every reference to the write payload's mutable surface goes
// through this constant, so the claim "cutoffarr changes exactly one field"
// is checkable by grepping for it.
const monitoredKey = "monitored"

// unmonitoredValue is the raw JSON literal written into monitoredKey. It is
// a json.RawMessage rather than a Go bool marshaled at write time so the
// payload surgery below never re-encodes anything: the fetched object's
// bytes are reused verbatim and exactly one value is substituted.
var unmonitoredValue = json.RawMessage("false")

// moviePath returns the /api/v3/movie/{id} path used for both halves of the
// write path (the fresh GET and the PUT), so the two can never drift apart.
func moviePath(movieID int) string {
	return "/api/v3/movie/" + strconv.Itoa(movieID)
}

// unmonitorMovie performs the project's first and only write operation:
// GET /api/v3/movie/{id}, set monitored to false, PUT the same object back
// (plan §2.2, §2.4). It returns written=true only when a PUT succeeded AND
// the object the server returned confirms the change, so callers count
// verified writes rather than intentions or assumptions.
//
// The sequence, and why each step exists:
//
//  1. GET the movie fresh, even though the caller already has a decoded
//     movieListElement for it. Two reasons. First, §2.4 requires PUTting
//     back the CURRENT object, and the library scan may be minutes old by
//     the time the write pass runs. Second — and this is the load-bearing
//     one — movieListElement is a tiny decoded subset (7 fields of the
//     ~50 Radarr returns); marshaling it back would silently destroy every
//     field it does not model. The payload must therefore come from the
//     wire, not from our model.
//
//  2. Decode into map[string]json.RawMessage, NOT map[string]any. RawMessage
//     keeps each value's original bytes untouched, so numbers, ordering
//     inside arrays, and any structure this codebase has no knowledge of
//     survive the round trip exactly. A map[string]any decode would turn
//     every JSON number into a float64 and re-encode it, which corrupts
//     large integers (sizeOnDisk, ids beyond 2^53) — a silent data-loss bug
//     on an object we are handing straight back to Radarr as authoritative.
//
//  3. Refuse to write anything we do not fully recognize: the "monitored"
//     key must actually be present (otherwise our assumed field name may be
//     wrong for this Radarr version, and setting it would ADD a key rather
//     than change one), and the returned object's id must be the id we
//     asked for (guarding against a proxy, redirect, or cache handing back
//     a different movie — writing monitored:false onto the wrong movie is
//     precisely the mistake this project must never make). §2.6: never
//     guess.
//
//  4. Skip the write entirely if the fresh object is already unmonitored.
//     This is the scan-to-write race (something else unmonitored it in the
//     meantime), and §2.4's spirit is to change exactly one thing, only
//     when it needs changing.
//
//  5. Check dryRun as the LAST thing before the HTTP call (§2.1: "Every
//     write code path must check the dry-run flag immediately before the
//     HTTP write call, not just at startup"). Everything above this line
//     runs identically in both modes, which is what makes a dry-run a real
//     rehearsal of the write rather than a different code path that merely
//     claims to be one. It also means the steps above can fail in dry-run;
//     the caller reports those as rehearsal failures, never as write ones.
//
//  6. Believe the server, not the status code: the returned object must
//     itself say monitored is false before the write counts as done. A 2xx
//     alone is not proof — see verifyWriteEcho.
//
// Errors are returned, never retried (§2.6: "Never retry writes
// automatically within a cycle"); the caller logs them and moves to the
// next item.
func unmonitorMovie(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance, movieID int, dryRun bool) (written bool, err error) {
	path := moviePath(movieID)

	body, err := fetchBody(ctx, client, path, nil)
	if err != nil {
		return false, fmt.Errorf("fetching movie %d before write: %w", movieID, err)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return false, fmt.Errorf("movie %d: response to the pre-write fetch is not a JSON object: %w", movieID, err)
	}

	if err := verifyMovieIdentity(payload, movieID); err != nil {
		return false, err
	}

	rawMonitored, present := payload[monitoredKey]
	if !present {
		return false, fmt.Errorf("movie %d: %q is absent from the pre-write fetch; refusing to write a field this Radarr may not have", movieID, monitoredKey)
	}
	var monitored bool
	if err := json.Unmarshal(rawMonitored, &monitored); err != nil {
		return false, fmt.Errorf("movie %d: %q is not a boolean in the pre-write fetch (%s): %w", movieID, monitoredKey, rawMonitored, err)
	}
	if !monitored {
		logger.Info("already unmonitored, skipping write",
			"instance", inst.Name, "type", inst.Type, "id", movieID)
		return false, nil
	}

	// The single mutation. Every other key in payload still holds the exact
	// bytes Radarr sent.
	payload[monitoredKey] = unmonitoredValue

	encoded, err := encodePayload(payload)
	if err != nil {
		return false, fmt.Errorf("movie %d: re-encoding the fetched object for write: %w", movieID, err)
	}

	// §2.1: the dry-run gate, immediately before the HTTP write call and
	// nowhere later. Nothing below this line may run in dry-run mode.
	if dryRun {
		logger.Debug("dry-run: write withheld immediately before the PUT",
			"instance", inst.Name, "type", inst.Type, "id", movieID, "method", http.MethodPut, "path", path)
		return false, nil
	}

	resp, err := client.DoJSON(ctx, http.MethodPut, path, encoded)
	if err != nil {
		return false, fmt.Errorf("writing movie %d: %w", movieID, err)
	}
	defer resp.Body.Close()

	// Radarr echoes the updated object back, and that echo is the only
	// evidence that the write took effect. Discarding it and reporting
	// success from the status code alone would mean trusting a 2xx to mean
	// something it does not always mean: a cache or proxy answering the
	// write, a replayed old object, or (before the client stopped following
	// redirects) a PUT silently downgraded to a GET would all produce a
	// perfectly good 200 while the movie stayed monitored. Since the whole
	// project reports "unmonitored=N" from this return value — and Phase 5's
	// no-op contract reads that count — an unconfirmed write is treated as a
	// failure, never as a success. §2.6 still applies: it is reported once
	// and never retried.
	echo, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes))
	if err != nil {
		return false, fmt.Errorf("movie %d: the write returned %d but its body could not be read, so the change is unconfirmed: %w", movieID, resp.StatusCode, err)
	}
	if err := verifyWriteEcho(echo, movieID, resp.StatusCode); err != nil {
		return false, err
	}

	return true, nil
}

// verifyWriteEcho confirms the object the server returned from the PUT
// really does carry monitored:false. Anything else — a body that is not a
// JSON object, no monitored key, a non-boolean value, or true — means the
// write is unconfirmed, and the error carries the status plus a bounded
// snippet of what actually came back so the log says what was seen rather
// than only what was expected.
func verifyWriteEcho(echo []byte, movieID, status int) error {
	var confirmed map[string]json.RawMessage
	if err := json.Unmarshal(echo, &confirmed); err != nil {
		return fmt.Errorf("movie %d: the write returned %d but the response is not a JSON object, so %q is unconfirmed: %s", movieID, status, monitoredKey, bodySnippet(echo))
	}
	raw, present := confirmed[monitoredKey]
	if !present {
		return fmt.Errorf("movie %d: the write returned %d but the returned object has no %q key, so the change is unconfirmed: %s", movieID, status, monitoredKey, bodySnippet(echo))
	}
	var stillMonitored bool
	if err := json.Unmarshal(raw, &stillMonitored); err != nil {
		return fmt.Errorf("movie %d: the write returned %d but %q came back as %s, which is not a boolean: %s", movieID, status, monitoredKey, raw, bodySnippet(echo))
	}
	if stillMonitored {
		return fmt.Errorf("movie %d: the write returned %d but the returned object still has %q: true; the movie was NOT unmonitored: %s", movieID, status, monitoredKey, bodySnippet(echo))
	}
	return nil
}

// bodySnippet bounds a response body for inclusion in an error message,
// mirroring the client's errorBodySnippetLimit so a large or unexpected
// response (an HTML error page from a reverse proxy, say) cannot bloat a log
// line.
func bodySnippet(body []byte) string {
	if len(body) > errorBodySnippetLimit {
		return string(body[:errorBodySnippetLimit]) + "..."
	}
	return string(body)
}

// encodePayload serializes the patched object for the PUT body with HTML
// escaping switched OFF, which json.Marshal does not allow.
//
// This is not cosmetic. Marshal (and every helper built on it) rewrites
// "&", "<" and ">" into their six-character unicode escapes inside every
// string it encodes — including the ones inside a json.RawMessage, which it
// re-scans rather than copying verbatim. A movie titled "Mr. & Mrs. Smith"
// would therefore be PUT back with bytes Radarr never sent, and a real
// library is full of such titles ("Cheech & Chong", "Fast & Furious", any
// <angle-bracketed> edition suffix). The escaped form decodes to the same
// string,
// so nothing is lost semantically, but §2.4 says the object goes back
// "otherwise unmodified" and the binding mandate is byte-for-byte on every
// key except monitored — a guarantee that would otherwise hold only for
// movies whose metadata happens to avoid three very common characters.
// json.Encoder is the only encoder in the standard library that exposes
// SetEscapeHTML, hence the buffer.
//
// Encode always appends a newline; it is trimmed so the body is exactly the
// object and nothing else.
func encodePayload(payload map[string]json.RawMessage) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// verifyMovieIdentity confirms the object returned by the pre-write fetch
// is the movie that was actually requested. An absent id is treated the
// same as a mismatched one: without it there is no way to confirm what we
// are about to overwrite, and "probably the right movie" is not a standard
// this write path is allowed to work to.
func verifyMovieIdentity(payload map[string]json.RawMessage, movieID int) error {
	rawID, present := payload["id"]
	if !present {
		return fmt.Errorf("movie %d: the pre-write fetch returned an object with no id; refusing to write to an unidentifiable object", movieID)
	}
	var gotID int
	if err := json.Unmarshal(rawID, &gotID); err != nil {
		return fmt.Errorf("movie %d: the pre-write fetch returned a non-numeric id (%s): %w", movieID, rawID, err)
	}
	if gotID != movieID {
		return fmt.Errorf("movie %d: the pre-write fetch returned movie %d instead; refusing to write to the wrong movie", movieID, gotID)
	}
	return nil
}
