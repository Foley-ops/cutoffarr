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

// unmonitoredValue and monitoredValue are the raw JSON literals written into
// monitoredKey — false by the forward (unmonitor) direction, true by the
// reverse (remonitor) one, and nothing else, ever. They are json.RawMessage
// rather than Go bools marshaled at write time so the payload surgery below
// never re-encodes anything: the fetched object's bytes are reused verbatim and
// exactly one value is substituted.
var (
	unmonitoredValue = json.RawMessage("false")
	monitoredValue   = json.RawMessage("true")
)

// monitoredWriteValue is the literal a direction writes, and
// monitoredWriteTarget the same thing as a Go bool for the places that compare
// against a decoded value (an episode's current state, an echo's answer). The
// two directions are structural opposites and there is no third, so these two
// functions are the whole of the difference between them.
func monitoredWriteValue(rev *reverseWriteContext) json.RawMessage {
	if rev != nil {
		return monitoredValue
	}
	return unmonitoredValue
}

func monitoredWriteTarget(rev *reverseWriteContext) bool { return rev != nil }

// monitoredWriteVerb is the same fact as a word, for the messages a human reads.
// Every shared write-path sentence that names what is being done has to come
// through here or through an explicit per-direction branch: those sentences were
// all written when only one direction existed, and a wrong one is not cosmetic —
// the reverse pass surfaces this exact string in its ERROR line, where "the
// write that failed was an unmonitor" is the opposite of what happened.
func monitoredWriteVerb(rev *reverseWriteContext) string {
	if rev != nil {
		return "re-monitoring"
	}
	return "unmonitoring"
}

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
//     key must be present AND readable as a boolean (otherwise our assumed
//     field name may be wrong for this Radarr version and setting it would
//     ADD a key rather than change one — and a JSON null, which decodes into
//     a plain bool as false with no error at all, would let step 5 below
//     state an observation about the movie that was never made), and the
//     returned object's id must be the id we asked for (guarding against a
//     proxy, redirect, or cache handing back a different movie — writing
//     monitored:false onto the wrong movie is precisely the mistake this
//     project must never make). §2.6: never guess. The "monitored" branches
//     wrap errMonitoredUnverifiable so the caller counts them as the
//     refusals they are (writesRefused) rather than as failed writes; the
//     identity check is fatal for this item on its own terms.
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
//     (false, nil), so runWritePass can count the race (writesRefused)
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
//  7. Believe the server, not the status code: the returned object must be
//     this movie and must itself say monitored is false before the write
//     counts as done. A 2xx alone is not proof — see verifyWriteEcho, which
//     applies step 3's identity check to the echo for the same reason it
//     exists on the fetch. When the server accepts the write but says
//     nothing that confirms it, the error wraps
//     errWriteUnverified so the caller can report "accepted, unconfirmed"
//     rather than "failed"; written is false either way, because only a
//     confirmed change may be counted as one.
//
// Errors are returned, never retried (§2.6: "Never retry writes
// automatically within a cycle"); the caller logs them and moves to the
// next item.
// The reverse direction (Phase 10) reuses every step above verbatim and adds
// exactly one of its own, between steps 5 and 6: the fresh payload must still
// FAIL the criteria. That check is evaluateMovie — the decision function
// itself — re-run against the object this write path just fetched, so "still a
// finding" cannot drift from "is a finding". Steps 3, 4 and 5 are direction-
// aware only in what they refuse: the reverse direction refuses a movie that is
// already MONITORED (the same race, from the other side).
//
// unmonitorMovie and remonitorMovie are the two entry points, and they exist as
// named wrappers rather than a bool parameter so that a call site says which
// direction it means in the one word a reader will actually read.
func unmonitorMovie(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance, movieID, exclusionTagID int, tagActive bool, dryRun bool) (written bool, err error) {
	return writeMovieMonitored(ctx, logger, client, inst, movieID, exclusionTagID, tagActive, dryRun, nil)
}

// remonitorMovie is the reverse direction's entry point: the same write, the
// same guards, the opposite value, plus the fresh re-evaluation rev carries the
// inputs for. It is only ever reached with reverse_scan_remonitor set (see
// reverse.go), and it obeys dryRun and the exclusion tag exactly as the forward
// path does.
func remonitorMovie(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance, movieID, exclusionTagID int, tagActive bool, dryRun bool, rev reverseWriteContext) (written bool, err error) {
	return writeMovieMonitored(ctx, logger, client, inst, movieID, exclusionTagID, tagActive, dryRun, &rev)
}

// writeMovieMonitored is the single implementation behind both. rev == nil is
// the forward direction; non-nil is the reverse one and carries what the
// reverse re-verification needs.
func writeMovieMonitored(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance, movieID, exclusionTagID int, tagActive bool, dryRun bool, rev *reverseWriteContext) (written bool, err error) {
	// PHASE 8, the shutdown boundary (binding controller note 4): once a write
	// operation has STARTED, it runs to its own end. context.WithoutCancel
	// detaches every request below from the daemon's shutdown cancellation, so
	// a SIGTERM landing between the pre-write GET and the PUT cannot turn a
	// clean write into a "context canceled" write error — or, on the Sonarr
	// twin, into a half-written season. The shutdown is instead checked
	// BETWEEN items, by the write pass, before this function is entered at all:
	// an item either completes or never starts.
	//
	// Nothing is unbounded here: every request still carries the client's
	// 15-second timeout (client.go), and the write pass withholds every
	// REMAINING item, so a shutdown is delayed by at most one item's writes.
	ctx = context.WithoutCancel(ctx)
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
	subject := fmt.Sprintf("movie %d", movieID)
	attrs := []any{"instance", inst.Name, "type", inst.Type, "id", movieID, "title", title}

	if tagActive {
		if err := preWriteExclusionTagCheck(logger, payload, "movie", subject, exclusionTagID, attrs); err != nil {
			return false, err
		}
	}

	// readMonitoredFlag covers the three branches that are one class: the
	// fresh payload's own "monitored" could not be read, so nothing is known
	// about the field this write path exists to change. Each warns in its own
	// words and each returns an error wrapping errMonitoredUnverifiable —
	// REVIEW FIX (Phase 5 final round): they used to be bare errors, which
	// runWritePass could only classify as failed WRITES, printing "unmonitor
	// write failed" at ERROR and writeErrors=N about a movie no PUT was ever
	// sent for. They are refusals, the same class as the untrusted-input
	// "tags" branches above, and are counted as such (writesRefused).
	monitored, err := readMonitoredFlag(logger, payload, "movie", subject, attrs)
	if err != nil {
		return false, err
	}
	if rev != nil {
		// The reverse direction's own step 5: the movie must still be
		// unmonitored, and it must still FAIL the criteria as of this fresh
		// payload. Both are races the forward path has mirrors of, and the
		// second one is the important one — re-monitoring a movie that has been
		// upgraded since the scan would undo this project's own correct work,
		// and the next forward cycle would unmonitor it again, forever.
		if monitored {
			logger.Info("already monitored, skipping write",
				"instance", inst.Name, "type", inst.Type, "id", movieID, "title", title)
			return false, fmt.Errorf("movie %d: %w", movieID, errAlreadyMonitoredAtWrite)
		}
		if err := verifyMovieStillAReverseFinding(ctx, logger, client, inst, body, movieID, exclusionTagID, tagActive, *rev, attrs); err != nil {
			return false, err
		}
	} else if !monitored {
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
		// sentinel here lets runWritePass count this race as the refusal it is.
		return false, fmt.Errorf("movie %d: %w", movieID, errAlreadyUnmonitoredAtWrite)
	}

	// The single mutation. Every other key in payload still holds the exact
	// bytes Radarr sent.
	payload[monitoredKey] = monitoredWriteValue(rev)

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
	if err := verifyWriteEcho(echo, movieID, resp.StatusCode, rev != nil); err != nil {
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
//
// Its text names no item type, and neither does any other sentinel in this
// group: both write paths share them, so a wording like "movie carries the
// exclusion tag" produced Sonarr refusals reading "series 3 season 1: movie
// carries the exclusion tag". The caller's subject prefix ("movie 7", "series
// 3 season 1") is what names the thing.
var errExcludedAtWrite = errors.New("the exclusion tag is present as of the pre-write fetch")

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
var errTagsUnverifiable = errors.New("the tags in the pre-write fetch could not be verified against the exclusion tag")

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
// sentinel here, instead, lets runWritePass count it (writesRefused), exactly
// as errExcludedAtWrite and errTagsUnverifiable already do for the pre-write
// tag re-check's refusals.
//
// It is deliberately NOT folded into the scan-time alreadyUnmonitored count
// (decision.go): that one counts movies rule 1 excluded from the report before
// they ever reached the write pass; this one counts a would-unmonitor decision
// that reached the write pass and lost a race there. Conflating them would
// make the report's own decision (wouldUnmonitor=N) look inconsistent with a
// scan-time count that never promised anything about those N movies. It is
// equally deliberately not given a THIRD summary attr of its own (as review
// round 4 first did, alreadyUnmonitoredAtWrite): a second counter one
// character away from alreadyUnmonitored on the same line trades one confusion
// for another, and the summary's question is "did every promised write end
// somewhere countable", which one refusal counter answers. The distinct causes
// live where a human actually diagnoses them — the per-movie log line each
// branch writes at the moment it refuses.
var errAlreadyUnmonitoredAtWrite = errors.New("already unmonitored as of the pre-write fetch")

// errAlreadyMonitoredAtWrite is errAlreadyUnmonitoredAtWrite's mirror for the
// reverse direction: the fresh pre-write fetch shows the movie (or season) is
// ALREADY monitored, so there is nothing to re-monitor. Something else — a
// human in the *arr's UI, another tool — got there between the scan and the
// write pass.
//
// It is a separate sentinel rather than a shared "already in the target state"
// one because the two say opposite things about the world and a human reading a
// refusal needs to know which happened; and it is a refusal rather than a
// silent no-op for the reason every other refusal here is one — a promised
// write that ends nowhere countable is a gap in the summary.
var errAlreadyMonitoredAtWrite = errors.New("already monitored as of the pre-write fetch")

// errNoLongerAReverseFinding marks the reverse direction's own pre-write
// re-verification refusing: the finding this write rests on could not be
// re-established from the fresh data, so re-monitoring would be acting on
// something nothing currently says is true.
//
// Its text names the re-establishment rather than the outcome (REVIEW FIX, Phase
// 10 round 5) because it covers two different endings, and only one of them is
// "it now meets the criteria": the fresh evaluation can also come back with
// untrusted input — an unknown profile, unreadable tags, a score that could not
// be fetched — where nothing about the item was established at all. Both refuse,
// both are counted the same, and each wrapping error says which happened; a
// sentinel that asserted the first would put a claim nobody observed into the
// error chain of the second. The log lines make the same distinction (see
// verifyMovieStillAReverseFinding).
//
// This is the reverse mirror of the forward path's airing and completeness
// re-checks, and it exists for a sharper reason than tidiness. The reverse and
// forward passes act on opposite conclusions about the same item: if the
// reverse pass re-monitored something that now meets the criteria, the very
// next forward cycle would unmonitor it again, and the two would flap that item
// against each other forever. The re-check is the decision function itself
// (evaluateMovie / evaluateSeries) re-run on fresh data, so "still a finding"
// can never drift from "is a finding".
var errNoLongerAReverseFinding = errors.New("the finding could not be re-established from the pre-write fetch")

// errMonitoredUnverifiable marks the pre-write fetch's own "monitored" field
// being unreadable: the key absent (this Radarr version may not have the field
// at all, and setting it would ADD a key rather than change one), present but
// not a boolean, or present as the JSON literal null — which a plain `var b
// bool` decode turns into false with NO error, indistinguishable from a
// genuine monitored:false and therefore able to make the already-unmonitored
// branch state an observation about the movie that was never made.
//
// Same class of untrusted input as errTagsUnverifiable, and counted the same
// way (writesRefused) for the same reason: no PUT was ever sent, so nothing
// Radarr did can have failed, and reporting these as writeErrors would tell a
// human that N writes were rejected by a server that never saw them.
var errMonitoredUnverifiable = errors.New("the monitored field in the pre-write fetch could not be verified")

// isWriteRefusal reports whether err is one of the write paths' refusals: a
// would-unmonitor decision the write path declined to act on before any HTTP
// write was sent. Every such branch logs its own specific reason at the moment
// it refuses, so the caller counts them under one name (writesRefused) without
// losing any of the causes; see runWritePass's reconciliation identity for why
// they must be counted at all. Stated as one function rather than a chain of
// errors.Is at the call site so a future refusal added to unmonitorMovie or
// unmonitorSeason has exactly one place to be registered — and so both
// engines' write passes classify refusals by the same rule rather than two
// drifting copies of it.
func isWriteRefusal(err error) bool {
	return errors.Is(err, errExcludedAtWrite) ||
		errors.Is(err, errTagsUnverifiable) ||
		errors.Is(err, errMonitoredUnverifiable) ||
		errors.Is(err, errAlreadyUnmonitoredAtWrite) ||
		errors.Is(err, errAlreadyMonitoredAtWrite) ||
		errors.Is(err, errNoLongerAReverseFinding) ||
		errors.Is(err, errSeasonUnverifiableAtWrite) ||
		errors.Is(err, errSeasonAiringAtWrite) ||
		errors.Is(err, errNotRecoveryAtWrite) ||
		errors.Is(err, errSeasonNotCleanlyUnmonitored)
}

// preWriteExclusionTagCheck re-checks the exclusion tag against a FRESH
// pre-write payload — the movie's or the series' — and refuses the write if
// the tag is present or if the payload's own tags cannot be trusted at all.
//
// §2.5 says the exclusion tag always wins, in every mode, and the scan that
// produced a decision may be minutes old by the time the write pass reaches
// it: a tag added between the two must still stop the write here. That is a
// property of the tag, not of movies, so both write paths call this rather
// than carrying two copies of four identical refusals (the copy is what
// eventually drifts, and this is the check that must never drift).
//
// Every branch returns a non-nil error wrapping a sentinel — errTagsUnverifiable
// for the three untrusted-input shapes, errExcludedAtWrite for a confirmed
// exclusion tag — rather than a bare "did not write", so the caller can count
// it (writesRefused) instead of letting a promised write vanish from the
// summary. noun names the thing for the human-facing log line ("movie",
// "series"); subject prefixes the error ("movie 7", "series 3 season 1").
func preWriteExclusionTagCheck(logger *slog.Logger, payload map[string]json.RawMessage, noun, subject string, exclusionTagID int, attrs []any) error {
	// rawMapField (shared.go), not payload["tags"]: the lookup has to match
	// the decode it is re-checking. encoding/json matches JSON keys to struct
	// fields case-INsensitively, so the read path was made case-insensitive
	// with an ambiguity refusal in Phase 6/7 — while this, the check §2.5 says
	// always wins, was still an exact map index (DEFERRED DEBT from the Phase 7
	// branch review, cleared in Phase 8). See rawMapField's doc comment for the
	// two shapes that mismatch produced; the ambiguous one let a payload
	// carrying the exclusion tag under the other spelling pass this check.
	rawTags, tagMatches := rawMapField(payload, "tags")
	if tagMatches > 1 {
		logger.Warn(noun+" carries more than one differently-cased tags key in the pre-write fetch; either could be the authoritative field, so the exclusion tag cannot be verified, refusing to write",
			append(append([]any(nil), attrs...), "tagsKeyMatches", tagMatches)...)
		return fmt.Errorf("%s: %w: %d differently-cased %q keys in the pre-write fetch, so the field's provenance is ambiguous", subject, errTagsUnverifiable, tagMatches, "tags")
	}
	if tagMatches == 0 {
		logger.Warn(noun+" tags absent from the pre-write fetch; cannot verify the exclusion tag is not present, refusing to write", attrs...)
		return fmt.Errorf("%s: %w: %q key absent from the pre-write fetch", subject, errTagsUnverifiable, "tags")
	}
	// decodeTagIDs (shared.go, REVIEW FIX carried forward to Phase 6):
	// unlike a bare `json.Unmarshal(rawTags, &tags)` into []int, this also
	// refuses a null ELEMENT inside an otherwise well-formed array (e.g.
	// "tags": [3, null, 9]) rather than silently decoding it as tag id 0 —
	// see decodeTagIDs's doc comment. The nil-slice-on-"tags": null
	// behavior a plain []int decode has is otherwise preserved exactly, so
	// the tags == nil check just below is unaffected.
	tags, err := decodeTagIDs(rawTags)
	if err != nil {
		if errors.Is(err, errNullTagElement) {
			logger.Warn(noun+" tags in the pre-write fetch contain a JSON null element; cannot verify the exclusion tag is not present, refusing to write",
				append(append([]any(nil), attrs...), "error", err)...)
			return fmt.Errorf("%s: %w: %q contains a JSON null element in the pre-write fetch: %v", subject, errTagsUnverifiable, "tags", err)
		}
		logger.Warn(noun+" tags in the pre-write fetch are not a JSON array of ids; cannot verify the exclusion tag is not present, refusing to write",
			append(append([]any(nil), attrs...), "error", err)...)
		return fmt.Errorf("%s: %w: %q is not a JSON array of ids in the pre-write fetch: %v", subject, errTagsUnverifiable, "tags", err)
	}
	// tags == nil here means the fresh payload's "tags" key held the
	// JSON literal null: decodeTagIDs (like a plain []int decode) turns
	// that into a nil slice with NO error (distinct from "[]", which
	// decodes to a non-nil, empty slice), so the tagsPresent/decode-error
	// guards above do not catch it. Left unchecked, containsIntID(nil,
	// exclusionTagID) is simply false and the write proceeds with the
	// exclusion tag's status genuinely unknown — the exact outcome this
	// whole re-check exists to prevent. The decision side treats the
	// identical wire shape as untrusted input for the same reason:
	// movieListElement.Tags and seriesElement.Tags are both *[]int, so
	// "tags": null leaves them nil and rule 4 returns ReasonTagsUnknown
	// rather than passing.
	if tags == nil {
		logger.Warn(noun+" tags in the pre-write fetch is JSON null; cannot verify the exclusion tag is not present, refusing to write", attrs...)
		return fmt.Errorf("%s: %w: %q is JSON null in the pre-write fetch", subject, errTagsUnverifiable, "tags")
	}
	if containsIntID(tags, exclusionTagID) {
		logger.Info(noun+" carries the exclusion tag as of the pre-write fetch, skipping write",
			append(append([]any(nil), attrs...), "reason", ReasonExcludedByTag)...)
		return fmt.Errorf("%s: %w", subject, errExcludedAtWrite)
	}
	return nil
}

// readMonitoredFlag reads a "monitored" value out of a fresh pre-write
// payload — a movie object, a series object, or one season inside a series'
// seasons array — with the null-decode discipline every one of those needs.
//
// The key must be present AND readable as a boolean: otherwise our assumed
// field name may be wrong for this *arr version and setting it would ADD a
// key rather than change one. And it is decoded into *bool, not bool, because
// json.Unmarshal([]byte("null"), &aBool) returns a NIL error and leaves the
// bool false — visually identical to a genuine monitored:false — so an
// "already unmonitored, skip" branch reading a plain bool would make a factual
// claim about the object's state that was never actually observed, and the
// item would never be unmonitored on any future cycle either. A *bool
// round-trips null to a nil pointer, with no decode error, so it can be told
// apart from both "false" and a genuinely non-boolean value.
//
// All three failures wrap errMonitoredUnverifiable so callers count them as
// the refusals they are (writesRefused) rather than as failed writes: no HTTP
// write was ever sent, so nothing the server did can have failed.
func readMonitoredFlag(logger *slog.Logger, payload map[string]json.RawMessage, noun, subject string, attrs []any) (bool, error) {
	raw, present := payload[monitoredKey]
	if !present {
		logger.Warn(noun+" monitored field absent from the pre-write fetch; this instance may not have the field at all, refusing to write", attrs...)
		return false, fmt.Errorf("%s: %w: %s %q key absent from the pre-write fetch; refusing to write a field this instance may not have", subject, errMonitoredUnverifiable, noun, monitoredKey)
	}
	var ptr *bool
	if err := json.Unmarshal(raw, &ptr); err != nil {
		logger.Warn(noun+" monitored field in the pre-write fetch is not a boolean; cannot tell whether it is monitored, refusing to write",
			append(append([]any(nil), attrs...), "error", err)...)
		return false, fmt.Errorf("%s: %w: %s %q is not a boolean in the pre-write fetch (%s): %v", subject, errMonitoredUnverifiable, noun, monitoredKey, raw, err)
	}
	if ptr == nil {
		logger.Warn(noun+" monitored field in the pre-write fetch is JSON null; cannot tell whether it is monitored, refusing to write", attrs...)
		return false, fmt.Errorf("%s: %w: %s %q is JSON null in the pre-write fetch; refusing to write a field whose value cannot be trusted", subject, errMonitoredUnverifiable, noun, monitoredKey)
	}
	return *ptr, nil
}

// verifyWriteEcho confirms the object the server returned from the PUT really
// is this movie and really does carry monitored:false. Anything else — a body
// that is not a JSON object, one describing a different movie, no monitored
// key, a non-boolean value, JSON null, or true — means the write is
// unconfirmed, and the error carries the status plus a bounded snippet of what
// actually came back so the log says what was seen rather than only what was
// expected.
//
// The identity check is the same guard the pre-write GET has carried since
// Phase 4 (verifyMovieIdentity), applied to the other end of the exchange —
// REVIEW FIX (Phase 5 final round). It was missing here, and this echo is the
// ONLY evidence this project accepts that a write landed: a proxy, redirect,
// or cache answering the PUT with some other movie's object would have been
// read as movie N's confirmed write, incrementing unmonitored and logging
// msg=unmonitor id=N on the strength of a document about a different film.
// Absence or mismatch is "cannot tell", not "did not happen" — a real Radarr
// may well have applied the write — so it joins the unverifiable class rather
// than the failure one.
//
// The six "cannot tell" cases wrap errWriteUnverified; the one "told us it
// did not happen" case does not. See errWriteUnverified for why that line is
// drawn where it is.
//
// want (Phase 10) is the value the write asked for: false on the forward path,
// true on the reverse one. Everything below is symmetric in it — "the server
// says the movie is still monitored" and "the server says it is still
// unmonitored" are the same confirmed failure read from opposite directions.
func verifyWriteEcho(echo []byte, movieID, status int, want bool) error {
	var confirmed map[string]json.RawMessage
	if err := json.Unmarshal(echo, &confirmed); err != nil {
		return fmt.Errorf("movie %d: the write returned %d but the response is not a JSON object, so %q is unconfirmed (%w): %s", movieID, status, monitoredKey, errWriteUnverified, bodySnippet(echo))
	}
	if err := verifyMovieIdentity(confirmed, movieID); err != nil {
		return fmt.Errorf("movie %d: the write returned %d but the returned object cannot be confirmed to be this movie, so the change is unconfirmed (%w): %v: %s", movieID, status, errWriteUnverified, err, bodySnippet(echo))
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
	if *stillMonitoredPtr != want {
		verb := "unmonitored"
		if want {
			verb = "re-monitored"
		}
		return fmt.Errorf("movie %d: the write returned %d but the returned object still has %q: %t; the movie was NOT %s: %s",
			movieID, status, monitoredKey, *stillMonitoredPtr, verb, bodySnippet(echo))
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
