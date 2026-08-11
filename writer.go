package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
//  4. Re-check the exclusion tag against this SAME fresh object (Phase 5,
//     mandated write-path safety fix). exclusionTagID/tagActive are the
//     same values the decision engine resolved once per instance and used
//     to evaluate this movie originally, but the scan that produced that
//     decision may be minutes old, and §2.5 says the exclusion tag always
//     wins, in every mode. A tag added to this movie between the scan and
//     the write pass must still stop the write here — the decision that
//     said would-unmonitor was correct when it was made; it is this
//     re-check, not that decision, that is responsible for catching a tag
//     added since. Consistent with decision.go rule 4's own handling of the
//     same field: "tags" entirely absent from the fresh object, present but
//     not a JSON array of ids, or present as the JSON literal null (which
//     decodes to a nil slice with no decode error — the shape a check that
//     only asked "did decoding succeed" would silently treat as "present and
//     empty") are all untrusted input, not evidence the tag is absent, and
//     refuse the write with a warning rather than guessing. Every refusal
//     here — untrusted tags or a confirmed exclusion tag — returns a non-nil
//     error wrapping errTagsUnverifiable or errExcludedAtWrite rather than
//     (false, nil), so runWritePass can count it (writesRefused) instead of
//     letting it vanish silently from the summary.
//
//  5. Skip the write entirely if the fresh object is already unmonitored.
//     This is the scan-to-write race (something else unmonitored it in the
//     meantime), and §2.4's spirit is to change exactly one thing, only
//     when it needs changing. The fresh payload's "monitored" value is
//     decoded into a *bool, not a bool, for the same reason step 4's tags
//     re-check is: json.Unmarshal("null", &aBool) succeeds with the field
//     left false, indistinguishable from a genuine monitored:false, which
//     would make this branch claim an observation ("already unmonitored")
//     that was never actually made. Like step 4's refusals, this returns a
//     non-nil error wrapping errAlreadyUnmonitoredAtWrite rather than
//     (false, nil), so runWritePass can count the race under its own name
//     instead of it vanishing into an unexplained gap in the summary.
//
//  6. Check dryRun as the LAST thing before the HTTP call (§2.1: "Every
//     write code path must check the dry-run flag immediately before the
//     HTTP write call, not just at startup"). Everything above this line
//     runs identically in both modes, which is what makes a dry-run a real
//     rehearsal of the write rather than a different code path that merely
//     claims to be one. It also means the steps above can fail in dry-run;
//     the caller reports those as rehearsal failures, never as write ones.
//
//  7. Believe the server, not the status code: the returned object must
//     itself say monitored is false before the write counts as done. A 2xx
//     alone is not proof — see verifyWriteEcho. When the server accepts the
//     write but says nothing that confirms it, the error wraps
//     errWriteUnverified so the caller can report "accepted, unconfirmed"
//     rather than "failed"; written is false either way, because only a
//     confirmed change may be counted as one.
//
// Errors are returned, never retried (§2.6: "Never retry writes
// automatically within a cycle"); the caller logs them and moves to the
// next item.
func unmonitorMovie(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance, movieID, exclusionTagID int, tagActive bool, dryRun bool) (written bool, err error) {
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

	title := titleFromPayload(payload)

	// Step 4: the exclusion tag always wins, in every mode (§2.5) — checked
	// here, against the FRESH payload's own tags, not against the decision
	// made from the (possibly stale) library scan. Only evaluated when the
	// tag is actually active in this instance; when it is not, rule 4 is
	// vacuous regardless of tags, same as the decision engine's own rule 4.
	//
	// Every branch below returns a non-nil error wrapping a sentinel
	// (errTagsUnverifiable for the two untrusted-input shapes, or
	// errExcludedAtWrite for a confirmed exclusion tag) rather than (false,
	// nil) — REVIEW FIX, Phase 5 round 2: runWritePass needs to tell "this
	// would-unmonitor decision produced no write because the pre-write tag
	// re-check refused it" apart from every other reason, and count it in
	// the summary (writesRefused), the same way it already distinguishes an
	// unverified echo from a rejected write. A plain (false, nil) here was
	// indistinguishable, in the summary, from a run that fetched nothing at
	// all.
	if tagActive {
		rawTags, tagsPresent := payload["tags"]
		if !tagsPresent {
			logger.Warn("movie tags absent from the pre-write fetch; cannot verify the exclusion tag is not present, refusing to write",
				"instance", inst.Name, "type", inst.Type, "id", movieID, "title", title)
			return false, fmt.Errorf("movie %d: %w: %q key absent from the pre-write fetch", movieID, errTagsUnverifiable, "tags")
		}
		var tags []int
		if err := json.Unmarshal(rawTags, &tags); err != nil {
			logger.Warn("movie tags in the pre-write fetch are not a JSON array of ids; cannot verify the exclusion tag is not present, refusing to write",
				"instance", inst.Name, "type", inst.Type, "id", movieID, "title", title, "error", err)
			return false, fmt.Errorf("movie %d: %w: %q is not a JSON array of ids in the pre-write fetch: %v", movieID, errTagsUnverifiable, "tags", err)
		}
		// tags == nil here means the fresh payload's "tags" key held the
		// JSON literal null: json.Unmarshal decodes that into a nil []int
		// with NO error (distinct from "[]", which decodes to a non-nil,
		// empty slice), so the tagsPresent/decode-error guards above do not
		// catch it. Left unchecked, containsIntID(nil, exclusionTagID) is
		// simply false and the write proceeds with the exclusion tag's
		// status genuinely unknown — the exact outcome this whole re-check
		// exists to prevent. The decision side treats the identical wire
		// shape as untrusted input for the same reason: movieListElement.Tags
		// is *[]int (radarr.go), so "tags": null leaves m.Tags == nil and
		// decision.go rule 4 returns ReasonTagsUnknown rather than passing.
		if tags == nil {
			logger.Warn("movie tags in the pre-write fetch is JSON null; cannot verify the exclusion tag is not present, refusing to write",
				"instance", inst.Name, "type", inst.Type, "id", movieID, "title", title)
			return false, fmt.Errorf("movie %d: %w: %q is JSON null in the pre-write fetch", movieID, errTagsUnverifiable, "tags")
		}
		if containsIntID(tags, exclusionTagID) {
			logger.Info("movie carries the exclusion tag as of the pre-write fetch, skipping write",
				"instance", inst.Name, "type", inst.Type, "id", movieID, "title", title, "reason", ReasonExcludedByTag)
			return false, fmt.Errorf("movie %d: %w", movieID, errExcludedAtWrite)
		}
	}

	rawMonitored, present := payload[monitoredKey]
	if !present {
		return false, fmt.Errorf("movie %d: %q is absent from the pre-write fetch; refusing to write a field this Radarr may not have", movieID, monitoredKey)
	}
	// Decode into *bool, not bool: this is the same JSON-null trap the fresh
	// payload's "tags" field has above, landing on the one field the entire
	// write path pivots on. json.Unmarshal([]byte("null"), &monitored) (a
	// plain bool) returns a NIL error and leaves monitored == false — visually
	// identical to a genuine monitored:false — so the "already unmonitored,
	// skip" branch immediately below would make a factual claim about the
	// movie's state ("it is unmonitored") that was never actually observed,
	// and the movie would never be unmonitored on any future cycle either. A
	// *bool round-trips null to a nil pointer, with no decode error, so it can
	// be told apart from both "false" (a non-nil pointer to false) and a
	// genuinely non-boolean value (a decode error, unchanged from before).
	var monitoredPtr *bool
	if err := json.Unmarshal(rawMonitored, &monitoredPtr); err != nil {
		return false, fmt.Errorf("movie %d: %q is not a boolean in the pre-write fetch (%s): %w", movieID, monitoredKey, rawMonitored, err)
	}
	if monitoredPtr == nil {
		return false, fmt.Errorf("movie %d: %q is JSON null in the pre-write fetch; refusing to write a field whose value cannot be trusted", movieID, monitoredKey)
	}
	if !*monitoredPtr {
		logger.Info("already unmonitored, skipping write",
			"instance", inst.Name, "type", inst.Type, "id", movieID, "title", title)
		// REVIEW FIX (Phase 5 round 4): this used to be (false, nil), the same
		// shape as "nothing needed doing" everywhere else in this function —
		// but runWritePass's `if !written { continue }` branch counts nothing
		// for that shape, so an instance where every candidate loses this race
		// reported every counter on the summary line as zero, with nothing
		// explaining where the promised writes went (the same gap
		// errExcludedAtWrite/errTagsUnverifiable/writesRefused closed for the
		// pre-write tag re-check, one review round earlier). Wrapping a
		// sentinel here lets runWritePass count this race under its own name
		// instead.
		return false, fmt.Errorf("movie %d: %w", movieID, errAlreadyUnmonitoredAtWrite)
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
		// Same class as an unreadable echo below: the server took the write
		// (2xx) and we simply cannot see what it said about it.
		return false, fmt.Errorf("movie %d: the write returned %d but its body could not be read, so the change is unconfirmed (%w): %v", movieID, resp.StatusCode, errWriteUnverified, err)
	}
	if err := verifyWriteEcho(echo, movieID, resp.StatusCode); err != nil {
		return false, err
	}

	return true, nil
}

// errWriteUnverified marks the outcomes where the server ACCEPTED the write
// — a 2xx came back — but said nothing that confirms what it did with it.
// An empty body, a body that is not a JSON object, one without "monitored",
// or one whose "monitored" is not a boolean all land here.
//
// It exists because those outcomes are a different event from a rejected
// write, and conflating them misinforms the human on both halves at once. A
// non-2xx means the change did NOT happen and the movie is still monitored.
// An unverifiable 2xx means the change probably DID happen and we merely
// cannot prove it — reporting that as "unmonitor write failed" would tell
// the human the write path is broken (it may be fine) and that the movie is
// still monitored (it probably is not). Callers separate the two with
// errors.Is; see runWritePass.
//
// Deliberately NOT in this class: an echo that says "monitored": true. That
// is not a failure to confirm, it is a confirmed failure — the server told
// us the movie is still monitored — and it stays a write error.
var errWriteUnverified = errors.New("the server accepted the write but did not confirm the change")

// errExcludedAtWrite marks the pre-write exclusion-tag re-check finding the
// exclusion tag actually present on the fresh payload (Phase 5, mandated
// write-path safety fix): §2.5 says the tag always wins, in every mode, so
// this is a confirmed, correct refusal to write — not a failure of anything
// — but it must still be distinguishable from every other reason a
// would-unmonitor decision produced no write, so runWritePass can count it
// (writesRefused) rather than let it vanish into an unexplained gap between
// wouldUnmonitor and unmonitored+writeErrors+writeEchoUnverified.
var errExcludedAtWrite = errors.New("movie carries the exclusion tag as of the pre-write fetch")

// errTagsUnverifiable marks the pre-write exclusion-tag re-check refusing to
// write because the fresh payload's own tags could not be trusted: the
// "tags" key was absent, present but not a JSON array of ids, or present as
// the JSON literal null (which decodes to a nil []int with no error — the
// one shape a check that only asked "did decoding succeed" would silently
// treat as "present and empty" and let through; see the null-specific
// comment where this is returned). Same class of untrusted input as
// "monitored" being absent from the fetch — §2.6: never guess — and the
// same reason it must be counted (writesRefused) rather than silently
// swallowed: an instance where the exclusion tag is active and every fresh
// fetch is refused this way would otherwise report unmonitored=0 with every
// other counter also at zero, leaving no number in the summary that explains
// where the promised writes went.
var errTagsUnverifiable = errors.New("movie tags in the pre-write fetch could not be verified against the exclusion tag")

// errAlreadyUnmonitoredAtWrite marks the scan-to-write race: the fresh
// pre-write fetch shows the movie is already unmonitored, so
// unmonitorMovie's step 5 skips the write — correctly, §2.4's spirit is to
// change exactly one thing, and only when it needs changing. This used to be
// reported as (false, nil), the same shape runWritePass treats as "nothing
// to count" for the dry-run-withheld case, so an instance where every
// would-unmonitor candidate lost this race between the scan and the write
// pass reported wouldUnmonitor=N against every other counter — unmonitored,
// writeErrors, writeEchoUnverified, writesRefused — at zero, with nothing on
// the summary line explaining where the N promised writes went. Wrapping a
// sentinel here, instead, lets runWritePass count this race under its own
// name (alreadyUnmonitoredAtWrite), exactly as errExcludedAtWrite and
// errTagsUnverifiable already do for the pre-write tag re-check's refusals.
// Deliberately its own counter, not folded into the scan-time
// alreadyUnmonitored count (decision.go): that one counts movies rule 1
// excluded from the report before ever reaching the write pass; this one
// counts a would-unmonitor decision that reached the write pass and lost a
// race there. Conflating the two would make the report's own decision
// (wouldUnmonitor=N) look inconsistent with a scan-time count that never
// promised anything about those N movies in the first place.
var errAlreadyUnmonitoredAtWrite = errors.New("movie is already unmonitored as of the pre-write fetch")

// verifyWriteEcho confirms the object the server returned from the PUT
// really does carry monitored:false. Anything else — a body that is not a
// JSON object, no monitored key, a non-boolean value, JSON null, or true —
// means the write is unconfirmed, and the error carries the status plus a
// bounded snippet of what actually came back so the log says what was seen
// rather than only what was expected.
//
// The five "cannot tell" cases wrap errWriteUnverified; the one "told us it
// did not happen" case does not. See errWriteUnverified for why that line is
// drawn where it is.
func verifyWriteEcho(echo []byte, movieID, status int) error {
	var confirmed map[string]json.RawMessage
	if err := json.Unmarshal(echo, &confirmed); err != nil {
		return fmt.Errorf("movie %d: the write returned %d but the response is not a JSON object, so %q is unconfirmed (%w): %s", movieID, status, monitoredKey, errWriteUnverified, bodySnippet(echo))
	}
	raw, present := confirmed[monitoredKey]
	if !present {
		return fmt.Errorf("movie %d: the write returned %d but the returned object has no %q key, so the change is unconfirmed (%w): %s", movieID, status, monitoredKey, errWriteUnverified, bodySnippet(echo))
	}
	// *bool, not bool: the same null trap the pre-write fetch's own
	// "monitored" decode has above, now on the echo. A plain
	// `var stillMonitored bool; json.Unmarshal(raw, &stillMonitored)` decodes
	// the JSON literal null into stillMonitored == false with NO error — a
	// write whose echo says "monitored": null would then read as a CONFIRMED
	// success (stillMonitored is false, so the function returns nil below),
	// when the server has told us nothing about the field at all. A *bool
	// keeps null (nil pointer) distinguishable from a genuine false.
	var stillMonitoredPtr *bool
	if err := json.Unmarshal(raw, &stillMonitoredPtr); err != nil {
		return fmt.Errorf("movie %d: the write returned %d but %q came back as %s, which is not a boolean (%w): %s", movieID, status, monitoredKey, raw, errWriteUnverified, bodySnippet(echo))
	}
	if stillMonitoredPtr == nil {
		return fmt.Errorf("movie %d: the write returned %d but %q came back as JSON null, which does not confirm the change (%w): %s", movieID, status, monitoredKey, errWriteUnverified, bodySnippet(echo))
	}
	stillMonitored := *stillMonitoredPtr
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

// titleFromPayload extracts "title" from the fresh pre-write fetch for use
// in log lines, mirroring decision.go's titleOrAbsent "absent" convention so
// a write-path log line reads the same way a report line does when the
// field cannot be found. Unlike verifyMovieIdentity's id check, a missing or
// malformed title is never fatal here — it is used only for a human-facing
// log attribute, not to decide whether the write proceeds.
func titleFromPayload(payload map[string]json.RawMessage) string {
	raw, present := payload["title"]
	if !present {
		return "absent"
	}
	var title string
	if err := json.Unmarshal(raw, &title); err != nil {
		return "absent"
	}
	return title
}

// containsIntID reports whether id is present in ids. A small local helper
// rather than decision.go's containsTag: that one takes a *[]int (to
// distinguish "tags absent" from "tags present but empty" for the caller),
// whereas the pre-write tag re-check above has already made that
// distinction itself (payload["tags"] present/absent) before ever reaching
// this call, so a plain []int is all that is needed here.
func containsIntID(ids []int, id int) bool {
	for _, i := range ids {
		if i == id {
			return true
		}
	}
	return false
}
