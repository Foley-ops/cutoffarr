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
	"time"
)

// sonarr_writer.go is the Sonarr write path (Phase 7). It is deliberately
// shaped so a reader can diff it against writer.go — the Radarr write path —
// almost line for line: same fresh-GET-then-surgery-then-PUT sequence, same
// null-decode discipline on every load-bearing field, same pre-write
// exclusion-tag re-check (literally the same function), same refusal
// sentinels, same "believe the echo, not the status code" rule, same §2.1
// dry-run gate immediately before each HTTP write.
//
// Three things differ, and each is forced by what a Sonarr season IS:
//
//  1. The decision unit is a SEASON, but the writable object is a SERIES.
//     Unmonitoring a season therefore means PUTting the whole series object
//     back with exactly one value changed, nested one level deeper than
//     Radarr's top-level "monitored": seasons[n].monitored for the target
//     season number. The series-level "monitored" field is read (a series
//     that is no longer monitored is a race, see below) but NEVER written —
//     no code path here assigns to it, and assembleSeasonWrite refuses to
//     emit a payload whose series-level monitored differs from the fresh GET
//     even if a future edit tries.
//
//  2. There are TWO write calls per season, not one: PUT /episode/monitor for
//     the season's monitored episode ids, then PUT /series/{id}. Both are
//     required because Sonarr's season monitoring and its per-episode
//     monitoring are separate state, and a season flipped without its
//     episodes leaves episodes that can still grab.
//
//     The ORDER is binding (controller resolution 1) and is the whole reason
//     it is stated here: episodes FIRST. If /episode/monitor succeeds and the
//     series PUT then fails, the season is still monitored, so the next cycle
//     re-evaluates it and converges — and the episodes already set false are
//     excluded from the next attempt's id list, so the retry is smaller, not
//     duplicated. The reverse order strands episodes monitored forever: the
//     season would go unmonitored, rule 1 would exclude it from every future
//     cycle, and nothing would ever revisit the episodes left behind.
//
//     Phase 10's REVERSE direction reaches that same stranded shape by the
//     other road — its episode write sets true, so a failed season PUT leaves
//     an unmonitored season full of monitored episodes — which is why the
//     reverse scan reports exactly that state (ReasonSeasonMonitorMismatch)
//     rather than the direction relying on a convergence it would not have had.
//
//  3. Pre-write re-verification has one more moving part than Radarr's,
//     because the season's most important guard (rule 3, the airing guard)
//     depends on data that lives at a different endpoint. A fresh
//     GET /episode?seriesId=X happens at write time and the airing guard is
//     re-run over it: a season that started airing between the decision and
//     the write is refused, not written.

// seriesPath returns the /api/v3/series/{id} path used for both halves of the
// season write path (the fresh GET and the PUT), so the two can never drift
// apart. Mirrors moviePath (writer.go).
func seriesPath(seriesID int) string {
	return "/api/v3/series/" + strconv.Itoa(seriesID)
}

// episodeMonitorPath is Sonarr's bulk episode-monitoring endpoint — the
// second of this project's permitted writes (§2.2), and the only one that is
// not a full-object PUT: its request body is a purpose-built command shape
// ({"episodeIds": [...], "monitored": false}), not a resource being handed
// back, so nothing about §2.4's byte-preservation mandate applies to it.
const episodeMonitorPath = "/api/v3/episode/monitor"

// seasonsKey and seasonNumberKey name the two keys the season surgery reaches
// through to get at the one value it changes, exactly as monitoredKey
// (writer.go) names that value. Naming them here means "cutoffarr changes
// exactly one field, inside exactly one array element" stays checkable by
// grepping.
const (
	seasonsKey      = "seasons"
	seasonNumberKey = "seasonNumber"
	statisticsKey   = "statistics"
)

// episodeMonitorRequest is the body of PUT /api/v3/episode/monitor. Monitored
// is a plain bool because the field must be present in the JSON and, since
// Phase 10, really does take both values: false on the forward path, true on
// the reverse one (monitoredWriteTarget decides, from the direction, in one
// place). It was documented here as "only ever false — nothing here ever
// re-monitors anything", which the reverse scan's write half made untrue.
type episodeMonitorRequest struct {
	EpisodeIDs []int `json:"episodeIds"`
	Monitored  bool  `json:"monitored"`
}

// errSeasonUnverifiableAtWrite marks the pre-write fetch failing to establish
// the target season's own state: the "seasons" key absent, JSON null, or not
// an array; an element of it that is not a JSON object; the target season
// number appearing zero times (it vanished between the decision and the
// write) or more than once (nothing can say which one the decision meant);
// the season's statistics missing, so the episode set backing the airing
// guard cannot be checked for completeness; or a fresh /episode response
// short of what those statistics claim.
//
// Same class as errTagsUnverifiable and errMonitoredUnverifiable — untrusted
// input, no PUT was ever sent, §2.6 says never guess — and counted the same
// way (writesRefused) for the same reason: an instance where every fresh
// fetch is refused this way would otherwise report unmonitored=0 with every
// other counter also at zero, leaving no number in the summary that explains
// where the promised writes went.
var errSeasonUnverifiableAtWrite = errors.New("the season's own state in the pre-write fetch could not be verified")

// errSeasonAiringAtWrite marks the pre-write airing re-check refusing: an
// episode of the target season has not aired yet, or its airDateUtc is absent
// or unparseable so its airing status cannot be determined at all.
//
// It is deliberately its own sentinel rather than folded into
// errSeasonUnverifiableAtWrite: this is the single most important Sonarr
// guard, it is the one refusal that means "the decision was correct when it
// was made and the world changed underneath it", and a human reading the log
// should be able to tell it apart from a data-shape problem at a glance. Like
// every refusal it is counted (writesRefused), warned once, and never
// retried within the cycle.
var errSeasonAiringAtWrite = errors.New("the season is no longer fully aired as of the pre-write fetch")

// errEpisodeMonitorContradicted marks the episode half of the write being
// told, by the server itself, that it did NOT happen: a 2xx whose echo names an
// episode this write asked to change and reports it still in the state the
// write was trying to leave.
//
// It is a CONFIRMED FAILURE, not a failure to confirm — the exact line
// verifySeasonWriteEcho draws on the season half, drawn here too because the
// episode half is the one where getting it wrong strands something. It is
// deliberately NOT errWriteUnverified: that class means "probably applied,
// treat it as done, the next cycle will reconcile it", and this shape is the
// server saying the opposite.
//
// Its wording is direction-neutral (Phase 10 round 3): it used to say "asked to
// unmonitor ... is still monitored", which is the forward half of a check that
// has been symmetric in the requested value since the reverse write path
// existed, and which read as the opposite of what happened on a reverse write.
var errEpisodeMonitorContradicted = errors.New("the episode monitor response says an episode this write named is still in the state the write was trying to change")

// errEpisodeMonitorUnconfirmed marks a 2xx from the episode write whose body
// cannot settle whether it landed: an unrecognized shape, a requested id the
// echo never mentions, a monitored value it does not state, or a body that
// could not be read at all.
//
// Like the contradicted case it stops the write before the season PUT
// (CRITICAL review fix), and for the same reason: the season PUT is the step
// that makes the season invisible to rule 1 forever, so it may only follow an
// episode write the server CONFIRMED. Aborting strands nothing — the season
// flag is untouched, so the next cycle re-reads /episode, drops whatever really
// landed from the id list, and retries. Abort converges whether or not the
// episodes changed; proceeding strands them in the branch where they did not.
//
// Going forward the season is left monitored, which is what keeps it in every
// future cycle's forward pass. Coming back it is left unmonitored, and any
// episode that really did land inside it is what the reverse pass now reports as
// ReasonSeasonMonitorMismatch — so "the next cycle sees it" is true in both
// directions rather than only the one this was first written for.
//
// It is also not errWriteUnverified, because no season write was even
// attempted: the season's own flag is definitively unchanged, and reporting it
// as accepted-but-unconfirmed would tell a human the opposite of what happened.
var errEpisodeMonitorUnconfirmed = errors.New("the episode monitor write was accepted but its response could not confirm it")

// errNotRecoveryAtWrite marks a season the RECOVERY pass admitted on
// decision-time evidence (recoveryCandidate) failing its qualification against
// the FRESH pre-write data: at least one episode of it is still monitored.
//
// Recovery qualification is decided by the fresh data and nothing else
// (controller ruling item 2), and a season with a monitored episode left in it
// is an ORDINARY write — which needs the ordinary gate, which in this
// situation is shut. Refusing (loudly, counted, never retried) is what keeps
// the recovery path from becoming a second way to make an ordinary write.
//
// It is a refusal rather than a silent withhold on purpose: a bare
// (false, nil) return from unmonitorSeason means "dry-run withheld at the §2.1
// gate" and nothing else, precisely so no real outcome can fall through that
// branch uncounted. Two review rounds closed exactly that hole; this stays on
// the counted side of it.
var errNotRecoveryAtWrite = errors.New("the season the recovery pass admitted still has monitored episodes at write time")

// errMismatchSeasonWouldWriteEpisodes marks the REVERSE direction refusing a
// ReasonSeasonMonitorMismatch season whose episodes are not all monitored
// already (REVIEW FIX, Phase 10 round 4).
//
// That reason is the one finding whose season MEETS every criterion: rules 4, 5
// and 7 all passed, and it is reported solely because the season's own flag
// disagrees with an episode inside it. Two very different states produce it:
//
//   - our own half-done write — PUT /episode/monitor landed, the season PUT did
//     not — which always leaves EVERY episode monitored, so finishing it is the
//     season flag and nothing else;
//   - a human who monitored one episode of an otherwise-finished unmonitored
//     season by hand, which leaves the others false.
//
// The ordinary episode write does not distinguish them: it names every episode
// currently in the wrong state, so the second case is re-monitored WHOLE — and
// the next forward cycle, seeing a monitored season that meets the criteria,
// unmonitors all of it including the episode the human deliberately chose. Rule
// 1 protected that episode before this phase existed. The plan scopes
// re-monitoring to "an unmonitored item that FAILS criteria", which this season
// does not, so the write is restricted to the shape it was added for: the season
// flag alone. Anything wider is refused here — counted (remonitorsRefused),
// warned once, and still reported every cycle, which is what the finding was for.
var errMismatchSeasonWouldWriteEpisodes = errors.New("re-monitoring an unmonitored season with monitored episodes may write only the season flag, and this one has unmonitored episodes in it")

// unmonitorSeason performs the Sonarr write operation for exactly ONE season:
// the fresh reads, every pre-write re-verification, then PUT /episode/monitor
// followed by PUT /api/v3/series/{id} (§2.2, §2.4, and the binding write
// order). It returns written=true only when BOTH calls were made AND the
// server's own responses confirm them, so callers count verified writes
// rather than intentions.
//
// One season per call, rather than one series with all its eligible seasons
// batched, is a deliberate choice: a season is the decision unit, so a season
// is the write unit. Each season's outcome is then independently countable
// against the accounting identity, each gets its own fresh look at the world,
// and a refusal on one season of a series (a race, a reappeared tag, a season
// that started airing) cannot silently suppress or drag along another. Each
// PUT still changes exactly one value, so writing season 1 and then season 2
// of the same series is two writes that each preserve everything else
// verbatim — including the season the other one changed.
//
// The steps below mirror unmonitorMovie's numbered rationale; only what is
// genuinely different about a season is restated:
//
//  1. GET the series fresh. Same two reasons as the movie path: §2.4 requires
//     PUTting back the CURRENT object, and seriesElement is a tiny decoded
//     subset of what Sonarr returns, so the payload must come from the wire.
//
//  2. Decode into map[string]json.RawMessage, and the target season into one
//     as well. RawMessage keeps every value's original bytes, so a 13-digit
//     sizeOnDisk, an unfamiliar nested object, and the sibling seasons all
//     survive the round trip exactly.
//
//  3. Refuse anything not fully recognized: the returned object's id must be
//     the id asked for, and every field the write pivots on must be readable.
//
//  4. Re-check the exclusion tag against this SAME fresh payload (§2.5: the
//     tag always wins, in every mode) — the identical shared helper the
//     movie path uses.
//
//  5. Refuse the scan-to-write races: the series is no longer monitored, or
//     the season is already unmonitored. Both decode through *bool for the
//     same reason the movie path does — json.Unmarshal("null", &aBool)
//     succeeds and leaves false, which would let a refusal state an
//     observation that was never made.
//
//  6. Re-run the airing guard against FRESH episode data (binding), including
//     the completeness check that guard depends on: an /episode response
//     short of the season's own statistics.totalEpisodeCount is exactly the
//     shape that lets a still-to-air episode go unseen.
//
//  7. Check dryRun as the LAST thing before EACH HTTP write call (§2.1) —
//     twice here, once per call, never once at the top.
//
//  8. Believe the server, not the status code, on both calls — and on the
//     EPISODE call, believe it before sending the season one. The season PUT
//     is what removes the season from every future cycle (rule 1), so it may
//     only follow an episode write the server confirmed.
//
// The second return value, recovery, is this function's verdict on what the
// FRESH data says this season is (controller ruling item 2): true when every
// episode of the season is already unmonitored, so the season flag is the only
// thing left to write. That is the state a partially completed write leaves
// behind, and the one state whose season PUT cannot strand anything — there is
// nothing left to strand. It is decided here, from the fresh /episode read,
// and nowhere else; the caller counts a completed one as recoveredWrites.
//
// requireRecovery is set by the recovery pass — the write pass's second, gated
// path, which runs when the ordinary write gate is shut. It turns the verdict
// above into a precondition: a season that no longer qualifies is refused
// (errNotRecoveryAtWrite) rather than written on evidence nothing authorized.
//
// Errors are returned, never retried (§2.6); the caller logs them and moves
// to the next season.
// The reverse direction (Phase 10) reuses every step above verbatim, with three
// differences and no fourth:
//
//   - it writes true instead of false, into the same one value;
//   - its races are the mirror ones (the season is already MONITORED), and it
//     adds the same "still fails the criteria on fresh data" re-verification the
//     movie path does, run through evaluateSeries itself;
//   - it names the season's UNMONITORED episodes in the episode call, for the
//     same reason the forward path names the monitored ones: an episode already
//     in the target state needs no write, and leaving it out makes a retry after
//     a partial failure smaller rather than duplicated.
//
// Everything else — the series-monitored guard, the fresh airing and
// completeness re-checks, the episodes-first order, both dry-run gates, the
// echo-before-the-season-PUT rule — is the same code doing the same thing. The
// series-monitored guard in particular is not merely reused but load-bearing in
// the new direction: binding controller resolution 3 says a season under an
// UNMONITORED series may never be re-monitored, because a series-level unmonitor
// is a human retiring a show, and this is the second place that is enforced.
func unmonitorSeason(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance, seriesID, seasonNumber, exclusionTagID int, tagActive bool, dryRun bool, requireRecovery bool) (written bool, recovery bool, err error) {
	return writeSeasonMonitored(ctx, logger, client, inst, seriesID, seasonNumber, exclusionTagID, tagActive, dryRun, requireRecovery, nil)
}

// remonitorSeason is the reverse direction's entry point. It has no recovery
// path and takes no requireRecovery, because both states a half-done re-monitor
// can leave are ordinary findings on the next cycle and are simply retried:
//
//   - nothing landed, so the season is still unmonitored with unmonitored
//     episodes — still below its cutoff, still reported;
//   - the episode write landed and the season PUT did not, so the season is
//     unmonitored with MONITORED episodes inside it — reported as
//     ReasonSeasonMonitorMismatch, and finished by the next cycle's season PUT
//     alone. "Alone" is enforced, not merely expected: a mismatch season with
//     unmonitored episodes in it is not ours to finish and is refused
//     (errMismatchSeasonWouldWriteEpisodes).
//
// REVIEW FIX (Phase 10 round 3): the second case used to be neither true nor
// stated. Its episodes have left the monitored=false wanted set, so rule 4 stops
// firing and the season evaluated as "cutoff met" — not a finding — while the
// forward pass excluded it at rule 1 for being unmonitored. It was stranded
// permanently, with Sonarr still upgrading its episodes and no line of any level
// about it. The mismatch reason is what closed that, and it is why this function
// still needs no recovery machinery of its own.
func remonitorSeason(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance, seriesID, seasonNumber, exclusionTagID int, tagActive bool, dryRun bool, rev reverseWriteContext) (written bool, err error) {
	written, _, err = writeSeasonMonitored(ctx, logger, client, inst, seriesID, seasonNumber, exclusionTagID, tagActive, dryRun, false, &rev)
	return written, err
}

// writeSeasonMonitored is the single implementation behind both. rev == nil is
// the forward direction; non-nil is the reverse one.
func writeSeasonMonitored(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance, seriesID, seasonNumber, exclusionTagID int, tagActive bool, dryRun bool, requireRecovery bool, rev *reverseWriteContext) (written bool, recovery bool, err error) {
	// PHASE 8, the shutdown boundary (binding controller note 4). This is the
	// function that boundary exists for: a season is TWO writes, and a
	// cancellation landing between them leaves episodes unmonitored inside a
	// season that is still monitored — the exact partial state the recovery
	// path exists to mop up. Detaching from the shutdown cancellation makes the
	// pair atomic with respect to shutdown; the signal is checked BETWEEN
	// seasons instead, by the write pass, so a season either makes both calls
	// or never makes the first. See unmonitorMovie for why this is bounded.
	ctx = context.WithoutCancel(ctx)
	path := seriesPath(seriesID)
	subject := fmt.Sprintf("series %d season %d", seriesID, seasonNumber)

	body, err := fetchBody(ctx, client, path, nil)
	if err != nil {
		return false, false, fmt.Errorf("fetching series %d before write: %w", seriesID, err)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return false, false, fmt.Errorf("%s: response to the pre-write fetch is not a JSON object: %w", subject, err)
	}

	if err := verifySeriesIdentity(payload, seriesID); err != nil {
		return false, false, err
	}

	title := titleFromPayload(payload)
	attrs := []any{"instance", inst.Name, "type", inst.Type, "seriesId", seriesID, "series", title, "season", seasonNumber}

	// Step 4: §2.5, against the FRESH payload's own tags — the same function
	// the movie path calls, not a second copy of the same four refusals.
	if tagActive {
		if err := preWriteExclusionTagCheck(logger, payload, "series", subject, exclusionTagID, attrs); err != nil {
			return false, false, err
		}
	}

	// Step 5a: the series-level monitored flag. It is READ here and written
	// nowhere: a series that is no longer monitored means the world moved
	// under this decision, and unmonitoring one of its seasons would be a
	// write nobody asked for.
	seriesMonitored, err := readMonitoredFlag(logger, payload, "series", subject, attrs)
	if err != nil {
		return false, false, err
	}
	if !seriesMonitored {
		logger.Info("series is no longer monitored as of the pre-write fetch, skipping season write", attrs...)
		return false, false, fmt.Errorf("%s: %w: the series itself is no longer monitored", subject, errAlreadyUnmonitoredAtWrite)
	}

	seasonElems, targetIdx, targetSeason, err := locateTargetSeason(logger, payload, subject, seasonNumber, attrs)
	if err != nil {
		return false, false, err
	}

	// Step 5b: the season's own monitored flag, with the same null-decode
	// discipline the series-level one just had.
	seasonMonitored, err := readMonitoredFlag(logger, targetSeason, "season", subject, attrs)
	if err != nil {
		return false, false, err
	}
	if rev == nil && !seasonMonitored {
		logger.Info("season already unmonitored as of the pre-write fetch, skipping write", attrs...)
		return false, false, fmt.Errorf("%s: %w", subject, errAlreadyUnmonitoredAtWrite)
	}
	if rev != nil && seasonMonitored {
		// The reverse direction's mirror race: something else re-monitored this
		// season between the scan and the write pass, so there is nothing to do.
		logger.Info("season already monitored as of the pre-write fetch, skipping write", attrs...)
		return false, false, fmt.Errorf("%s: %w", subject, errAlreadyMonitoredAtWrite)
	}

	// Step 6: fresh episode data, then the airing guard re-run over it. A
	// fetch failure is not a refusal — nothing about the season is untrusted,
	// the data simply could not be read — so it is returned as an ordinary
	// error and counted as a write failure, exactly as a failed pre-write GET
	// is on the movie path.
	episodes, ok := fetchEpisodes(ctx, logger, client, inst, seriesID)
	if !ok {
		return false, false, fmt.Errorf("%s: fresh episode data could not be fetched before the write", subject)
	}
	seasonEpisodes, err := episodesOfSeason(logger, episodes, seriesID, seasonNumber, subject, attrs)
	if err != nil {
		return false, false, err
	}
	if err := verifySeasonStillWritable(logger, targetSeason, seasonEpisodes, subject, attrs); err != nil {
		return false, false, err
	}

	// The reverse direction's own re-verification, after the guards above (which
	// are cheap and shared) and before anything is assembled: the season must
	// still FAIL the criteria as of fresh data, or re-monitoring it would undo
	// this project's own correct work and the next forward cycle would unmonitor
	// it again.
	//
	// It hands back the reason the FRESH data reached, because one of them —
	// ReasonSeasonMonitorMismatch — is a narrower write mandate than the others
	// and the episode list below is where that is enforced.
	var revReason string
	if rev != nil {
		reason, err := verifySeasonStillAReverseFinding(ctx, logger, client, inst, body, seriesID, seasonNumber, exclusionTagID, tagActive, *rev, subject, attrs)
		if err != nil {
			return false, false, err
		}
		revReason = reason
	}

	// The episode ids to unmonitor: ONLY episodes this fresh read says are
	// still monitored (binding). An episode already false needs no write, and
	// leaving it out is what makes a retry after a partial failure smaller
	// rather than a duplicate.
	//
	// An episode that cannot be named (no id) or whose own monitored value
	// cannot be read is a REFUSAL, not an exclusion — REVIEW FIX. Excluding it
	// and writing the season anyway would leave a possibly-monitored episode
	// inside a season rule 1 removes from every future cycle: nothing would
	// ever revisit it, which is precisely the stranded state the binding
	// episodes-first order was chosen to prevent, arrived at by a different
	// road. Every other unreadable load-bearing field in this write path
	// refuses; so does this one.
	//
	// Both refusals are the same in either direction; their stated RATIONALE is
	// not, and it was the forward one in both (REVIEW FIX, Phase 10 round 5).
	// Stranding is a forward consequence: rule 1 hides an unmonitored season from
	// every future cycle, so an episode left monitored inside one is never looked
	// at again. A re-monitor strands nothing — the season it produces is
	// monitored, and the forward pass will evaluate it next cycle. What is true
	// coming back is the mirror: the episode this write could not name or read
	// would be left UNMONITORED inside a season this write is making monitored,
	// so the write cannot do the thing it claims to do, on data it could not
	// read. Same refusal, opposite reason, and a human diagnosing it needs the
	// one that applies.
	noIDRationale := "so unmonitoring the season would strand it, refusing to write"
	unreadableRationale := "unmonitoring the season would strand it in an unknown state, refusing to write"
	if rev != nil {
		noIDRationale = "so re-monitoring the season would leave it unmonitored inside a monitored season, refusing to write"
		unreadableRationale = "re-monitoring the season would leave it in an unknown state inside a monitored season, refusing to write"
	}
	var episodeIDs []int
	for _, e := range seasonEpisodes {
		if e.ID == nil {
			logger.Warn("an episode of this season has no id in the pre-write fetch; it could not be named in the episode monitor write, "+noIDRationale,
				append(append([]any(nil), attrs...), "episodeNumber", derefOrAbsent(e.EpisodeNumber))...)
			return false, false, fmt.Errorf("%s: %w: an episode of this season has no id in the pre-write fetch", subject, errSeasonUnverifiableAtWrite)
		}
		if e.Monitored == nil {
			logger.Warn("an episode of this season has an absent or JSON-null monitored field in the pre-write fetch; "+unreadableRationale,
				append(append([]any(nil), attrs...), "episodeId", *e.ID)...)
			return false, false, fmt.Errorf("%s: %w: episode %d has no readable monitored field in the pre-write fetch", subject, errSeasonUnverifiableAtWrite, *e.ID)
		}
		if *e.Monitored == monitoredWriteTarget(rev) {
			// Already in the target state: no write needed for this episode,
			// and leaving it out is what makes a retry after a partial failure
			// smaller rather than a duplicate.
			continue
		}
		episodeIDs = append(episodeIDs, *e.ID)
	}

	// THE MISMATCH FINDING'S NARROWER WRITE MANDATE (REVIEW FIX, Phase 10 round
	// 4), decided here because here is the first place the fresh episode set says
	// which of the two states produced the finding.
	//
	// A ReasonSeasonMonitorMismatch season MEETS every criterion — that is what
	// makes it the one finding this project may not simply re-monitor. The state
	// the reason was added for (our own half-done write) leaves every episode
	// monitored, so episodeIDs is empty and only the season flag is left to
	// write: that write goes ahead below, and it is what makes the reverse
	// direction converge. Anything else reaching this line is a season whose
	// unmonitored episodes somebody chose, and writing them would exceed the
	// plan's mandate ("an unmonitored item that FAILS criteria") and then be
	// undone — the next forward cycle unmonitors the whole re-monitored season,
	// episode the human picked included.
	//
	// The refusal is loud, counted (remonitorsRefused) and never retried within
	// the cycle, exactly like every other refusal here; the finding itself is
	// unaffected and is reported again next cycle, which is all the plan asks of
	// it. Deliberately not a bare withhold: (false, nil) means "dry-run withheld
	// at the §2.1 gate" and nothing else.
	if rev != nil && revReason == ReasonSeasonMonitorMismatch && len(episodeIDs) > 0 {
		logger.Warn("this season meets every criterion and is reported only because its own flag disagrees with an episode inside it; re-monitoring it may write only the season flag, but episodes of it are unmonitored — writing them would re-monitor episodes nobody asked about, so refusing to write",
			append(append([]any(nil), attrs...), "unmonitoredEpisodes", len(episodeIDs))...)
		return false, false, fmt.Errorf("%s: %w: %d episode(s) of it are unmonitored", subject, errMismatchSeasonWouldWriteEpisodes, len(episodeIDs))
	}

	// THE RECOVERY VERDICT (controller ruling item 2), decided here and nowhere
	// else, from the fresh, complete, fully-validated episode set this function
	// just built: every episode's id and monitored value was readable, the set
	// matches the season's own statistics, the season is still monitored,
	// complete on disk, fully aired and tag-clean — and not one episode of it
	// is still monitored. The only thing left to write is the season flag.
	//
	// That is exactly what a partially completed write leaves behind (the
	// /episode/monitor call landed, the season PUT did not), and it is the one
	// shape whose season PUT cannot strand anything: there is nothing left to
	// strand, and Sonarr cannot grab an unmonitored episode whatever the season
	// flag says.
	//
	// The reverse direction has no recovery concept at all (binding controller
	// resolution 6), because it needs none: BOTH of its half-done states are
	// findings on the next cycle and are retried through the ordinary path — the
	// season is still unmonitored, or it is unmonitored with monitored episodes
	// in it, which evaluateSeries reports as ReasonSeasonMonitorMismatch (Phase
	// 10 round 3; before it, that second state was silent and permanent). So the
	// verdict is only ever true going forward, and the reverse caller ignores it.
	recovery = rev == nil && len(episodeIDs) == 0

	// The recovery pass's precondition. It admitted this season on
	// decision-time evidence (recoveryCandidate); the fresh data is the
	// authority, and it just said this is an ordinary write. An ordinary write
	// needs the ordinary gate, which is why the recovery pass was running at
	// all.
	if requireRecovery && !recovery {
		logger.Warn("the ordinary write gate is shut and this season was admitted to the recovery pass (every episode already unmonitored), but the pre-write fetch shows monitored episodes; refusing to write",
			append(append([]any(nil), attrs...), "monitoredEpisodes", len(episodeIDs))...)
		return false, recovery, fmt.Errorf("%s: %w: %d episode(s) are still monitored", subject, errNotRecoveryAtWrite, len(episodeIDs))
	}

	// The season payload is assembled BEFORE either write goes out, even
	// though it is the second one that sends it. An assembly failure (or the
	// series-level monitored guard inside it tripping) must not be discovered
	// after the episode call has already landed: that would leave the season
	// half-written for a reason that had nothing to do with the server. It
	// also makes the dry-run rehearsal complete — the payload is really built,
	// really checked, and only then withheld.
	encoded, err := assembleSeasonWrite(payload, seasonElems, targetIdx, targetSeason, subject, rev)
	if err != nil {
		return false, recovery, err
	}

	// FIRST write call (binding order).
	episodesWritten := false
	if len(episodeIDs) == 0 {
		// Every episode is already in the state this write wants, so only the
		// season flag is left. Going FORWARD that is the recovery shape (what a
		// partially completed unmonitor leaves behind); coming back it is
		// ordinary — a season somebody unmonitored at the season level only —
		// and the reverse direction has no recovery concept to claim.
		msg := "no monitored episodes remain in this season; the episode monitor write is not needed and this is a recovery write (the season flag alone)"
		if rev != nil {
			msg = "every episode of this season is already monitored; the episode monitor write is not needed and only the season flag is left to write"
		}
		logger.Debug(msg, append(append([]any(nil), attrs...), "episodes", len(seasonEpisodes), "recovery", recovery)...)
	} else {
		// §2.1: the dry-run gate, immediately before this HTTP write call and
		// nowhere earlier. Everything above runs identically in both modes.
		if dryRun {
			logger.Debug("dry-run: episode monitor write withheld immediately before the PUT",
				append(append([]any(nil), attrs...), "method", http.MethodPut, "path", episodeMonitorPath, "episodeIds", len(episodeIDs))...)
		} else {
			// CRITICAL review fix: ANY answer short of a confirmation stops
			// the write here, before the season PUT. The season PUT is what
			// makes the season invisible to rule 1 forever, so it may only
			// follow an episode write the server confirmed — otherwise a
			// still-monitored episode ends up inside a season nothing will
			// ever evaluate again, which is the stranded state the binding
			// episodes-first order exists to prevent, reached by the one road
			// left open. Aborting strands nothing: the season's own flag is
			// untouched, the next cycle re-reads /episode, and the retry is
			// smaller by whatever really landed.
			if err := putEpisodeMonitor(ctx, logger, client, inst, seriesID, episodeIDs, subject, attrs, rev); err != nil {
				// The message says which state the season was left in, because
				// the two directions leave opposite ones and a human reading
				// this line is reading it to find that out (Phase 10 round 3 —
				// it used to say "left monitored" in both, which is the
				// opposite of the truth on a reverse write).
				msg := "episode monitor write could not be confirmed; the season write is withheld and the season is left monitored so the next cycle can retry it"
				if rev != nil {
					msg = "episode monitor write could not be confirmed; the season write is withheld and the season is left unmonitored so the next cycle can retry it — if any episode did land, that cycle sees the season as an unmonitored season with monitored episodes"
				}
				logger.Warn(msg, append(append([]any(nil), attrs...), "episodeIds", len(episodeIDs), "error", err)...)
				return false, recovery, err
			}
			episodesWritten = true
		}
	}

	// §2.1 again: the SECOND dry-run gate, immediately before the second HTTP
	// write call. Nothing below this line may run in dry-run mode.
	if dryRun {
		logger.Debug("dry-run: write withheld immediately before the PUT",
			append(append([]any(nil), attrs...), "method", http.MethodPut, "path", path)...)
		return false, recovery, nil
	}

	resp, err := client.DoJSON(ctx, http.MethodPut, path, encoded)
	if err != nil {
		if episodesWritten && rev != nil {
			// The REVERSE partial completion, and it needed both a correction and
			// a voice (REVIEW FIX, Phase 10 round 3). The episode write landed and
			// the season write did not, so this season is left UNMONITORED with
			// monitored episodes inside it — the mirror of the forward case, and
			// a materially different state: the forward pass excludes the season
			// at rule 1 for every cycle to come, so nothing in this program
			// enforces a cutoff on those episodes while Sonarr goes on upgrading
			// them.
			//
			// It converges, but only because the reverse pass now RECOGNIZES that
			// state (ReasonSeasonMonitorMismatch) and retries the season flag; it
			// used to evaluate as "cutoff met", which is not a finding, and the
			// season was stranded permanently with no line of any level about it.
			// The warning stays regardless: an automatic retry next cycle is not a
			// reason to leave a human unable to see that a write was left half
			// done, and the message says which half.
			logger.Warn("the episode monitor write landed but the season write did not: this season is left unmonitored with monitored episodes inside it, which the forward pass cannot see at all; the next reverse scan reports it as a monitor mismatch and retries the season flag",
				append(append([]any(nil), attrs...), "error", err)...)
			return false, recovery, fmt.Errorf("%s: the episode monitor write completed but the season write failed, so the season is left unmonitored with monitored episodes in it: %w", subject, err)
		}
		if episodesWritten {
			// Controller resolution 1's partial completion: the episode write
			// landed and the series write did not, so the season is STILL
			// monitored. Naming the completed half is what lets a human read
			// the state the season was actually left in — and the next cycle
			// converges on its own, since the episodes now reading false are
			// excluded from that attempt's id list (and, if every episode is,
			// through the recovery path built for exactly this).
			return false, recovery, fmt.Errorf("%s: the episode monitor write completed but the season write failed, so the season is still monitored: %w", subject, err)
		}
		return false, recovery, fmt.Errorf("writing %s: %w", subject, err)
	}
	defer resp.Body.Close()

	echo, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes))
	if err != nil {
		return false, recovery, fmt.Errorf("%s: the write returned %d but its body could not be read, so the change is unconfirmed (%w): %v", subject, resp.StatusCode, errWriteUnverified, err)
	}
	if err := verifySeasonWriteEcho(echo, seriesID, seasonNumber, resp.StatusCode, subject, monitoredWriteTarget(rev)); err != nil {
		return false, recovery, err
	}

	// written=true still requires BOTH calls confirmed (controller resolution
	// 1) — it just cannot be decided here any more, because an unconfirmed
	// episode write already returned above without sending this PUT at all.
	return true, recovery, nil
}

// episodesOfSeason filters a fresh /episode response down to one season.
// An episode whose own seasonNumber is absent belongs to no season here, the
// same way it is excluded from every season's evaluation at decision time —
// and the completeness check that follows is what turns that exclusion into a
// refusal rather than a silently short episode set.
//
// The drop WARNs, naming the episode, exactly as the structurally identical
// decision-time drop does (REVIEW FIX). It used to be silent, which left the
// count guard to report the SYMPTOM — "the fresh episode list has 2 episodes
// but statistics claims 3" — with nothing anywhere naming the cause, so an
// operator saw an unexplained refusal repeat every cycle. Worse, a season
// whose statistics happen to match absorbs the drop entirely and the guard
// never fires at all.
//
// It also enforces, at write time only, the provenance the READ path
// deliberately does not (DEFERRED DEBT from the Phase 7 branch review, cleared
// in Phase 8). belongsToSeries (sonarr.go) treats an ABSENT seriesId as
// belonging, because at decision time "the key is missing" is not evidence of a
// routing mistake and the count guards backstop a wholesale wrong-series
// response. Here the consequence is different in kind: an episode of the target
// season is about to have its id NAMED in PUT /api/v3/episode/monitor, so an
// unprovenanced record would be written to on the strength of a ?seriesId=X
// query nothing in the response corroborated. Every other unreadable
// load-bearing field in this write path refuses; so does this one.
//
// The requirement is scoped to the episodes this write actually names. An
// unprovenanced record sitting in some OTHER season is the read path's problem
// (it warns there), not a veto over a season whose own episodes all check out.
func episodesOfSeason(logger *slog.Logger, episodes []episodeElement, seriesID, seasonNumber int, subject string, attrs []any) ([]episodeElement, error) {
	var out []episodeElement
	for _, e := range episodes {
		if e.SeasonNumber == nil {
			logger.Warn("episode missing seasonNumber field in the pre-write fetch; excluded from this season's episode set",
				append(append([]any(nil), attrs...), "episodeId", derefOrAbsent(e.ID))...)
			continue
		}
		if *e.SeasonNumber != seasonNumber {
			continue
		}
		if e.SeriesID == nil || *e.SeriesID != seriesID {
			logger.Warn("an episode of this season does not state that it belongs to this series in the pre-write fetch; it cannot be named in the episode monitor write, refusing to write",
				append(append([]any(nil), attrs...), "episodeId", derefOrAbsent(e.ID), "recordSeriesId", derefOrAbsent(e.SeriesID))...)
			return nil, fmt.Errorf("%s: %w: an episode of this season has an absent or mismatched seriesId in the pre-write fetch", subject, errSeasonUnverifiableAtWrite)
		}
		out = append(out, e)
	}
	return out, nil
}

// locateTargetSeason finds the target season inside the fresh payload's
// "seasons" array and returns the array (as raw elements, so every sibling
// keeps its exact bytes), the target's index, and the target decoded as a raw
// object ready for its single mutation.
//
// Every failure here is a refusal, not an error: nothing was written, and
// each one means the object about to be handed back to Sonarr as
// authoritative could not be fully understood.
func locateTargetSeason(logger *slog.Logger, payload map[string]json.RawMessage, subject string, seasonNumber int, attrs []any) ([]json.RawMessage, int, map[string]json.RawMessage, error) {
	rawSeasons, present := payload[seasonsKey]
	if !present {
		logger.Warn("series seasons absent from the pre-write fetch; refusing to write",
			attrs...)
		return nil, 0, nil, fmt.Errorf("%s: %w: %q key absent from the pre-write fetch", subject, errSeasonUnverifiableAtWrite, seasonsKey)
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(rawSeasons, &elems); err != nil {
		logger.Warn("series seasons in the pre-write fetch is not a JSON array; refusing to write",
			append(append([]any(nil), attrs...), "error", err)...)
		return nil, 0, nil, fmt.Errorf("%s: %w: %q is not a JSON array in the pre-write fetch: %v", subject, errSeasonUnverifiableAtWrite, seasonsKey, err)
	}
	// A nil slice here means the key held the JSON literal null, which
	// decodes with NO error — the same trap the tags and monitored fields
	// carry, on the array this write performs its surgery inside.
	if elems == nil {
		logger.Warn("series seasons in the pre-write fetch is JSON null; refusing to write", attrs...)
		return nil, 0, nil, fmt.Errorf("%s: %w: %q is JSON null in the pre-write fetch", subject, errSeasonUnverifiableAtWrite, seasonsKey)
	}

	targetIdx := -1
	var target map[string]json.RawMessage
	for i, raw := range elems {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			logger.Warn("a season in the pre-write fetch is not a JSON object; refusing to write the whole series object back",
				append(append([]any(nil), attrs...), "index", i, "error", err)...)
			return nil, 0, nil, fmt.Errorf("%s: %w: seasons[%d] is not a JSON object in the pre-write fetch: %v", subject, errSeasonUnverifiableAtWrite, i, err)
		}
		rawNumber, ok := obj[seasonNumberKey]
		if !ok {
			logger.Warn("a season in the pre-write fetch has no seasonNumber; it cannot be identified",
				append(append([]any(nil), attrs...), "index", i)...)
			continue
		}
		var number *int
		if err := json.Unmarshal(rawNumber, &number); err != nil || number == nil {
			logger.Warn("a season in the pre-write fetch has an unreadable seasonNumber; it cannot be identified",
				append(append([]any(nil), attrs...), "index", i)...)
			continue
		}
		if *number != seasonNumber {
			continue
		}
		if targetIdx != -1 {
			// Two seasons claiming the same number: nothing can say which one
			// this decision was about, and writing either would be a guess.
			logger.Warn("the target season appears more than once in the pre-write fetch; refusing to write",
				attrs...)
			return nil, 0, nil, fmt.Errorf("%s: %w: the target season appears more than once in the pre-write fetch", subject, errSeasonUnverifiableAtWrite)
		}
		targetIdx = i
		target = obj
	}
	if targetIdx == -1 {
		logger.Warn("the target season is not present in the pre-write fetch; refusing to write", attrs...)
		return nil, 0, nil, fmt.Errorf("%s: %w: the target season is not present in the pre-write fetch", subject, errSeasonUnverifiableAtWrite)
	}
	return elems, targetIdx, target, nil
}

// verifySeasonStillWritable re-runs, against fresh data, the two decision-time
// guards whose subject matter can change between the scan and the write: the
// season's episode set must be complete, and every episode in it must have
// aired.
//
// The completeness half is not decoration. The airing guard is only as sound
// as the episode set it runs over: a /episode response short of the season's
// own statistics.totalEpisodeCount is precisely the shape that lets a
// still-to-air episode go unseen, which is the CRITICAL hole the decision
// engine's own guard was extended to close. Re-running the airing check
// without it would re-run a weaker guard than the one that authorized this
// write in the first place — at the moment of actual consequence.
func verifySeasonStillWritable(logger *slog.Logger, targetSeason map[string]json.RawMessage, seasonEpisodes []episodeElement, subject string, attrs []any) error {
	rawStats, present := targetSeason[statisticsKey]
	if !present {
		logger.Warn("season statistics absent from the pre-write fetch; the episode set backing the airing guard cannot be checked for completeness, refusing to write", attrs...)
		return fmt.Errorf("%s: %w: %q absent from the pre-write fetch", subject, errSeasonUnverifiableAtWrite, statisticsKey)
	}
	var stats seasonStatisticsElement
	if err := json.Unmarshal(rawStats, &stats); err != nil || stats.TotalEpisodeCount == nil || stats.EpisodeFileCount == nil {
		logger.Warn("season statistics could not be read from the pre-write fetch; refusing to write",
			append(append([]any(nil), attrs...), "error", err)...)
		return fmt.Errorf("%s: %w: %q could not be read from the pre-write fetch", subject, errSeasonUnverifiableAtWrite, statisticsKey)
	}

	// Rule 2's own condition, re-run against the fresh payload (REVIEW FIX).
	// "This season is finished, stop monitoring it" rests entirely on the
	// season being complete on disk; if a file was removed between the scan
	// and the write, that premise is gone and unmonitoring the season would
	// strand the missing episode forever — rule 1 excludes an unmonitored
	// season from every future cycle, so nothing would ever notice the gap.
	// The value is already in hand from the same statistics object the
	// completeness check below reads.
	if *stats.EpisodeFileCount != *stats.TotalEpisodeCount {
		logger.Warn("the season is no longer complete on disk as of the pre-write fetch; refusing to write",
			append(append([]any(nil), attrs...), "episodeFileCount", *stats.EpisodeFileCount, "totalEpisodeCount", *stats.TotalEpisodeCount)...)
		return fmt.Errorf("%s: %w: statistics now report %d files for %d episodes", subject, errSeasonUnverifiableAtWrite, *stats.EpisodeFileCount, *stats.TotalEpisodeCount)
	}
	// A season claiming zero episodes would make the airing loop below run
	// over nothing and "pass" vacuously — the guard satisfied by the absence
	// of anything to check. Rule 2 requires totalEpisodeCount > 0 for exactly
	// that reason at decision time; the fresh payload has to clear the same
	// bar, or this write rests on a season that no longer resembles the one
	// the decision was about.
	if *stats.TotalEpisodeCount <= 0 {
		logger.Warn("the season's own statistics now claim it has no episodes; the airing guard would have nothing to check, refusing to write",
			append(append([]any(nil), attrs...), "totalEpisodeCount", *stats.TotalEpisodeCount)...)
		return fmt.Errorf("%s: %w: %q.totalEpisodeCount is %d in the pre-write fetch", subject, errSeasonUnverifiableAtWrite, statisticsKey, *stats.TotalEpisodeCount)
	}
	if len(seasonEpisodes) != *stats.TotalEpisodeCount {
		logger.Warn("the fresh episode list does not match the season's own statistics.totalEpisodeCount; the airing guard cannot be trusted over an incomplete set, refusing to write",
			append(append([]any(nil), attrs...), "episodesReturned", len(seasonEpisodes), "totalEpisodeCount", *stats.TotalEpisodeCount)...)
		return fmt.Errorf("%s: %w: the fresh episode list has %d episodes but statistics claims %d", subject, errSeasonUnverifiableAtWrite, len(seasonEpisodes), *stats.TotalEpisodeCount)
	}

	now := time.Now().UTC()
	untrustedCount, unairedCount := 0, 0
	for _, e := range seasonEpisodes {
		aired, untrusted := episodeAiringStatus(e, now)
		if untrusted {
			untrustedCount++
			logger.Warn("episode airDateUtc is absent or unparseable in the pre-write fetch; airing status cannot be determined, refusing to write",
				append(append([]any(nil), attrs...), "episodeId", derefOrAbsent(e.ID))...)
			continue
		}
		if !aired {
			unairedCount++
			logger.Warn("the season has an episode that has not aired yet as of the pre-write fetch; refusing to write",
				append(append([]any(nil), attrs...), "episodeId", derefOrAbsent(e.ID), "airDateUtc", derefOrAbsent(e.AirDateUtc))...)
		}
	}
	if untrustedCount > 0 || unairedCount > 0 {
		return fmt.Errorf("%s: %w: %d episode(s) not yet aired, %d with an undeterminable air date", subject, errSeasonAiringAtWrite, unairedCount, untrustedCount)
	}
	return nil
}

// assembleSeasonWrite performs the single mutation and re-encodes the payload
// for the PUT body: seasons[targetIdx].monitored becomes false, and every
// other byte of every other key — and of every other season — is the bytes
// Sonarr sent.
//
// The series-level monitored check at the end is a runtime guard on the one
// invariant this whole file exists to protect, not a redundant assertion.
// Nothing above assigns to payload[monitoredKey]; this makes that a fact the
// program itself refuses to violate, so a future edit that reached for the
// series-level flag would fail here instead of silently unmonitoring a whole
// show.
func assembleSeasonWrite(payload map[string]json.RawMessage, seasonElems []json.RawMessage, targetIdx int, targetSeason map[string]json.RawMessage, subject string, rev *reverseWriteContext) ([]byte, error) {
	originalMonitored, hadMonitored := payload[monitoredKey]

	targetSeason[monitoredKey] = monitoredWriteValue(rev)
	encodedSeason, err := encodePayload(targetSeason)
	if err != nil {
		return nil, fmt.Errorf("%s: re-encoding the target season for write: %w", subject, err)
	}

	// A copy, so the caller's slice (and therefore the fetched bytes it still
	// refers to) is never mutated in place.
	patched := append([]json.RawMessage(nil), seasonElems...)
	patched[targetIdx] = encodedSeason

	encodedSeasons, err := encodeRawArray(patched)
	if err != nil {
		return nil, fmt.Errorf("%s: re-encoding the seasons array for write: %w", subject, err)
	}
	payload[seasonsKey] = encodedSeasons

	if nowMonitored, present := payload[monitoredKey]; present != hadMonitored || !bytes.Equal(nowMonitored, originalMonitored) {
		return nil, fmt.Errorf("%s: refusing to write: the assembled payload's series-level %q differs from the pre-write fetch (%s -> %s); unmonitoring only ever happens to a season",
			subject, monitoredKey, originalMonitored, nowMonitored)
	}

	encoded, err := encodePayload(payload)
	if err != nil {
		return nil, fmt.Errorf("%s: re-encoding the fetched object for write: %w", subject, err)
	}
	return encoded, nil
}

// putEpisodeMonitor issues the episode half of the write and returns nil ONLY
// when the server's own response confirms it. Every other outcome is an error
// that stops the season write, and every one of them says what came back.
//
// The three classes, all fatal to this season's write for this cycle:
//
//   - a transport failure or non-2xx: nothing was written.
//   - errEpisodeMonitorContradicted: the echo names a requested episode as
//     still monitored. The server told us the write did not happen.
//   - errEpisodeMonitorUnconfirmed: a 2xx whose body cannot settle it — an
//     unrecognized shape, a requested id it never mentions, a monitored value
//     it does not state, or a body that could not be READ at all.
//
// The last one used to be `return false, nil` with no log and no wrapped
// error, and the caller sent the season PUT on it (CRITICAL review fix). Two
// things were wrong with that. The season PUT is what removes the season from
// every future cycle (rule 1), so sending it on an unconfirmed episode write
// is the one way left to strand a monitored episode inside a season nothing
// will ever look at again. And the stated rationale — "aborting would strand
// the episodes it probably already changed" — is inverted: aborting leaves the
// season MONITORED, so the next cycle re-reads /episode, drops whatever really
// landed from the id list, and retries. Abort converges in both branches;
// proceeding strands in one.
//
// Every returned error carries the offending episode id where there is one and
// a bounded snippet of the body, because this is the failure the first live
// run is most likely to hit (Sonarr's real /episode/monitor echo shape is
// unconfirmed by any live write) and it is the one a human has to be able to
// diagnose from the log alone.
func putEpisodeMonitor(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance, seriesID int, episodeIDs []int, subject string, attrs []any, rev *reverseWriteContext) error {
	body, err := json.Marshal(episodeMonitorRequest{EpisodeIDs: episodeIDs, Monitored: monitoredWriteTarget(rev)})
	if err != nil {
		return fmt.Errorf("%s: encoding the episode monitor request: %w", subject, err)
	}

	resp, err := client.DoJSON(ctx, http.MethodPut, episodeMonitorPath, body)
	if err != nil {
		// The verb, not a fixed word (REVIEW FIX, Phase 10 round 5): this error
		// is what reverseCounts.record prints on its ERROR line, so a failed
		// re-monitor used to tell the human `msg="remonitor write failed"
		// error="...unmonitoring the season's episodes..."` — the two halves of
		// one line contradicting each other about what was attempted.
		return fmt.Errorf("%s: %s the season's episodes: %w", subject, monitoredWriteVerb(rev), err)
	}
	defer resp.Body.Close()

	echo, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes))
	if err != nil {
		unconfirmed := fmt.Errorf("%s: the episode monitor write returned %d but its body could not be read (%w): %v", subject, resp.StatusCode, errEpisodeMonitorUnconfirmed, err)
		return confirmEpisodeMonitorByReRead(ctx, logger, client, inst, seriesID, episodeIDs, subject, attrs, unconfirmed, rev)
	}
	if err := verifyEpisodeMonitorEcho(echo, episodeIDs, monitoredWriteTarget(rev)); err != nil {
		if errors.Is(err, errEpisodeMonitorContradicted) {
			// NOT re-read, deliberately. The server did not fail to answer the
			// question — it answered it, with "that episode is still
			// monitored". Re-reading here would be shopping for a second
			// opinion on a confirmed failure, and a race that flipped the
			// episode between the echo and the re-read would convert the
			// server's own "no" into a "yes".
			return fmt.Errorf("%s: the episode monitor write returned %d but %w", subject, resp.StatusCode, err)
		}
		unconfirmed := fmt.Errorf("%s: the episode monitor write returned %d but %v (%w)", subject, resp.StatusCode, err, errEpisodeMonitorUnconfirmed)
		return confirmEpisodeMonitorByReRead(ctx, logger, client, inst, seriesID, episodeIDs, subject, attrs, unconfirmed, rev)
	}
	return nil
}

// confirmEpisodeMonitorByReRead is the read-only disambiguation of an episode
// write the ECHO could not confirm (DEFERRED DEBT from the Phase 7 branch
// review, cleared in Phase 8 before any live write).
//
// The problem it solves is specific and was going to bite on the very first
// live run: Sonarr's real PUT /api/v3/episode/monitor echo shape is unconfirmed
// by any live write of ours, and an unrecognized shape made the episode half
// permanently unconfirmable — every season write on that instance aborted,
// every cycle, forever. Correct (nothing is stranded, the season stays
// monitored) but never converging, and indistinguishable in the log from a
// server that keeps rejecting the write.
//
// The answer is more evidence, not more tolerance: GET /api/v3/episode
// ?seriesId=X — the same read this write path already makes, no new endpoint
// and no new verb — and require it to say, of EVERY episode this write named,
// that it exists, states this series as its own, and is now monitored: false.
// Anything less keeps the original abort, wrapped with what the re-read said.
//
// Only reached on a NON-contradicted failure. See putEpisodeMonitor for why a
// contradicted echo is never re-read.
func confirmEpisodeMonitorByReRead(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance, seriesID int, episodeIDs []int, subject string, attrs []any, unconfirmed error, rev *reverseWriteContext) error {
	logger.Info("the episode monitor response could not confirm the write; re-reading the season's episodes to settle whether it landed",
		append(append([]any(nil), attrs...), "episodeIds", len(episodeIDs), "error", unconfirmed)...)

	episodes, ok := fetchEpisodes(ctx, logger, client, inst, seriesID)
	if !ok {
		return fmt.Errorf("%s: the read-only re-read of this series' episodes also failed, so the episode write remains unconfirmed: %w", subject, unconfirmed)
	}

	byID := make(map[int]episodeElement, len(episodes))
	for _, e := range episodes {
		if e.ID != nil {
			byID[*e.ID] = e
		}
	}
	for _, id := range episodeIDs {
		e, found := byID[id]
		switch {
		case !found:
			return fmt.Errorf("%s: the re-read does not mention episode %d, so the episode write remains unconfirmed: %w", subject, id, unconfirmed)
		case e.SeriesID == nil || *e.SeriesID != seriesID:
			// Same write-time provenance rule episodesOfSeason applies: a
			// record that does not state this series cannot be evidence about
			// this series' write.
			return fmt.Errorf("%s: the re-read's episode %d does not state that it belongs to this series, so the episode write remains unconfirmed: %w", subject, id, unconfirmed)
		case e.Monitored == nil:
			return fmt.Errorf("%s: the re-read does not say whether episode %d is monitored, so the episode write remains unconfirmed: %w", subject, id, unconfirmed)
		case *e.Monitored != monitoredWriteTarget(rev):
			return fmt.Errorf("%s: the re-read says episode %d still reads monitored=%t rather than %t, so the episode write did not land: %w",
				subject, id, *e.Monitored, monitoredWriteTarget(rev), unconfirmed)
		}
	}

	logger.Warn("the episode monitor response could not be recognized, but a read-only re-read confirms every episode this write named is now in the state it asked for; treating the episode write as confirmed",
		append(append([]any(nil), attrs...), "episodeIds", len(episodeIDs), "echoError", unconfirmed)...)
	return nil
}

// verifyEpisodeMonitorEcho confirms the episodes the server echoed back really
// are the ones that were asked for and really do say monitored is false.
//
// It draws the same line verifySeasonWriteEcho draws on the season half, and
// the CRITICAL review round is what put it here: an echo that says a requested
// episode is STILL monitored wraps errEpisodeMonitorContradicted — a confirmed
// failure — while a body that merely cannot answer the question returns a plain
// error the caller classes as unconfirmed. Collapsing the two into one bool was
// what let a server's own "no, that did not happen" be treated as "cannot tell,
// carry on".
//
// Every branch names the episode it is about and quotes a bounded snippet of
// the body, so an unrecognized live shape is diagnosable from the error alone.
//
// want (Phase 10) is the state the write asked for: false going forward, true
// coming back. The contradiction rule is symmetric in it — an echo naming a
// requested episode in the state the write was trying to leave is the server
// saying the write did not happen, whichever direction that is.
func verifyEpisodeMonitorEcho(echo []byte, episodeIDs []int, want bool) error {
	var confirmed []map[string]json.RawMessage
	if err := json.Unmarshal(echo, &confirmed); err != nil {
		return fmt.Errorf("the episode monitor response is not a JSON array of objects: %s", bodySnippet(echo))
	}
	monitoredByID := make(map[int]*bool, len(confirmed))
	for _, obj := range confirmed {
		rawID, present := obj["id"]
		if !present {
			continue
		}
		var id *int
		if err := json.Unmarshal(rawID, &id); err != nil || id == nil {
			continue
		}
		var monitored *bool
		if raw, present := obj[monitoredKey]; present {
			if err := json.Unmarshal(raw, &monitored); err != nil {
				monitored = nil
			}
		}
		// REVIEW FIX: this map used to be last-wins, so an echo naming episode
		// 100 twice — once monitored:true, once monitored:false — settled the
		// question in whichever order the server happened to serialize them.
		// This is the ONLY evidence this project accepts that the episode half
		// landed, and locateTargetSeason already refuses the identical ambiguity
		// on the way IN; an echo that says two different things about one
		// episode cannot confirm anything, so it does not get to.
		if _, duplicate := monitoredByID[*id]; duplicate {
			return fmt.Errorf("the episode monitor response mentions episode %d more than once, so it cannot confirm what happened to it: %s", *id, bodySnippet(echo))
		}
		monitoredByID[*id] = monitored
	}
	for _, id := range episodeIDs {
		monitored, present := monitoredByID[id]
		if !present {
			return fmt.Errorf("the episode monitor response does not mention episode %d: %s", id, bodySnippet(echo))
		}
		if monitored == nil {
			return fmt.Errorf("the episode monitor response does not say whether episode %d is monitored: %s", id, bodySnippet(echo))
		}
		if *monitored != want {
			return fmt.Errorf("%w: episode %d: %s", errEpisodeMonitorContradicted, id, bodySnippet(echo))
		}
	}
	return nil
}

// verifySeasonWriteEcho confirms the object the server returned from the
// season PUT really is this series and really does carry monitored:false on
// the target season. It applies verifySeriesIdentity to the echo for exactly
// the reason writer.go's verifyWriteEcho applies verifyMovieIdentity to its
// own: this echo is the ONLY evidence this project accepts that a write
// landed, and a proxy, redirect, or cache answering the PUT with some other
// series' object would otherwise be read as this season's confirmed write.
//
// The "cannot tell" cases wrap errWriteUnverified; the one "told us it did not
// happen" case — an echo whose target season still says monitored: true — does
// not, for the same reason the movie path draws that line: it is a confirmed
// failure, not a failure to confirm.
//
// want (Phase 10) is the value the write asked for, and the "told us it did not
// happen" case is symmetric in it.
func verifySeasonWriteEcho(echo []byte, seriesID, seasonNumber, status int, subject string, want bool) error {
	var confirmed map[string]json.RawMessage
	if err := json.Unmarshal(echo, &confirmed); err != nil {
		return fmt.Errorf("%s: the write returned %d but the response is not a JSON object, so the change is unconfirmed (%w): %s", subject, status, errWriteUnverified, bodySnippet(echo))
	}
	if err := verifySeriesIdentity(confirmed, seriesID); err != nil {
		return fmt.Errorf("%s: the write returned %d but the returned object cannot be confirmed to be this series, so the change is unconfirmed (%w): %v: %s", subject, status, errWriteUnverified, err, bodySnippet(echo))
	}
	rawSeasons, present := confirmed[seasonsKey]
	if !present {
		return fmt.Errorf("%s: the write returned %d but the returned object has no %q key, so the change is unconfirmed (%w): %s", subject, status, seasonsKey, errWriteUnverified, bodySnippet(echo))
	}
	var elems []struct {
		SeasonNumber *int  `json:"seasonNumber"`
		Monitored    *bool `json:"monitored"`
	}
	if err := json.Unmarshal(rawSeasons, &elems); err != nil {
		return fmt.Errorf("%s: the write returned %d but %q in the response could not be read, so the change is unconfirmed (%w): %s", subject, status, seasonsKey, errWriteUnverified, bodySnippet(echo))
	}
	// The WHOLE array is scanned, not just up to the first element carrying the
	// target season number (REVIEW FIX). Returning on the first match resolved
	// a contradictory duplicate — season 1 appearing twice, monitored:false
	// then monitored:true — in the server's favour, on the sole evidence this
	// project accepts that a write landed, while locateTargetSeason refuses the
	// identical ambiguity on the way in. An echo that says two things about one
	// season confirms neither of them.
	matchIdx := -1
	for i, s := range elems {
		if s.SeasonNumber == nil || *s.SeasonNumber != seasonNumber {
			continue
		}
		if matchIdx != -1 {
			return fmt.Errorf("%s: the write returned %d but the returned object contains this season more than once, so the change is unconfirmed (%w): %s", subject, status, errWriteUnverified, bodySnippet(echo))
		}
		matchIdx = i
	}
	if matchIdx == -1 {
		return fmt.Errorf("%s: the write returned %d but the returned object does not mention this season, so the change is unconfirmed (%w): %s", subject, status, errWriteUnverified, bodySnippet(echo))
	}
	// *bool, not bool: a "monitored": null in the echo would decode into a
	// plain bool as false with NO error, and this function would then report a
	// CONFIRMED write on a server that said nothing about the field at all.
	if elems[matchIdx].Monitored == nil {
		return fmt.Errorf("%s: the write returned %d but the returned season's %q is absent or JSON null, which does not confirm the change (%w): %s", subject, status, monitoredKey, errWriteUnverified, bodySnippet(echo))
	}
	if *elems[matchIdx].Monitored != want {
		verb := "unmonitored"
		if want {
			verb = "re-monitored"
		}
		return fmt.Errorf("%s: the write returned %d but the returned object still has the season %q: %t; the season was NOT %s: %s",
			subject, status, monitoredKey, *elems[matchIdx].Monitored, verb, bodySnippet(echo))
	}
	return nil
}

// verifySeriesIdentity confirms an object claimed to be a given series really
// carries that id. An absent id is treated the same as a mismatched one: the
// mirror of verifyMovieIdentity (writer.go), and for the same reason —
// "probably the right show" is not a standard this write path is allowed to
// work to.
func verifySeriesIdentity(payload map[string]json.RawMessage, seriesID int) error {
	rawID, present := payload["id"]
	if !present {
		return fmt.Errorf("series %d: the object has no id; refusing to write to an unidentifiable object", seriesID)
	}
	var gotID int
	if err := json.Unmarshal(rawID, &gotID); err != nil {
		return fmt.Errorf("series %d: non-numeric id (%s): %w", seriesID, rawID, err)
	}
	if gotID != seriesID {
		return fmt.Errorf("series %d: the object identifies series %d instead; refusing to write to the wrong series", seriesID, gotID)
	}
	return nil
}

// encodeRawArray serializes an array of already-encoded JSON values with HTML
// escaping switched OFF, for exactly the reason encodePayload (writer.go)
// does: Marshal rewrites "&", "<" and ">" into unicode escapes inside every
// string it encodes, including inside a json.RawMessage it re-scans rather
// than copies. The seasons array is handed straight back to Sonarr as
// authoritative, so its untouched elements must be untouched in bytes, not
// merely in meaning.
func encodeRawArray(elems []json.RawMessage) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(elems); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// recoveryCandidate reports whether a season decision looks, on DECISION-time
// evidence, like a season the recovery pass should take a fresh look at: every
// episode of it explicitly reported monitored == false when it was evaluated.
//
// It is a cost filter and nothing more. The authority on whether a season
// really qualifies for the recovery path is the fresh pre-write data, and it
// lives in unmonitorSeason (see its recovery return); this exists so that a
// shut-gate instance with two hundred pending seasons does not issue four
// hundred fresh GETs to discover that none of them qualifies. A season that
// became recovery-shaped between the decision and the write is simply picked
// up by the next cycle, which is the same conservative direction every other
// stale-decision case in this project takes.
//
// An episode whose monitored flag is absent or JSON null does not match
// (untrusted input is never evidence of anything), and an empty episode set
// does not match either (a condition nothing can satisfy must not be satisfied
// by nothing).
func recoveryCandidate(d seasonDecision) bool {
	if len(d.crossCheckEpisodes) == 0 {
		return false
	}
	// An episode set that lost an episode on the way in (one with no id —
	// buildSeasonCrossCheckEpisodes) cannot establish "every episode of this
	// season is already unmonitored": the dropped episode is precisely the one
	// that might still be monitored, and it is unnameable, so a write could not
	// have unmonitored it either. Cleared DEFERRED DEBT from the Phase 7 branch
	// review; the write-time recovery verdict (unmonitorSeason, from fresh
	// data) remains the backstop, and this keeps the filter from ever handing
	// it a season on evidence that was never complete.
	if !d.crossCheckEpisodesComplete {
		return false
	}
	for _, ep := range d.crossCheckEpisodes {
		if ep.monitored == nil || *ep.monitored {
			return false
		}
	}
	return true
}

// recoveryGateBlockReason is the RECOVERY pass's own gate, the counterpart of
// writeGateBlockReason for the ordinary one: "" admits the recovery pass, any
// other string is the reason it stays shut (binding controller ruling item 3).
//
// The two gates differ in exactly one place, and the reason is what the gates
// are FOR. The ordinary gate demands evidence that the wanted-set signal
// behind a would-unmonitor decision was corroborated, because unmonitoring a
// season with monitored episodes in it acts on that signal. A recovery
// season's one remaining action — the season PUT — does not: its rule-4 pass
// is structural (Sonarr's /wanted/cutoff is filtered to monitored episodes, so
// a season with none cannot appear in it), and rules 2, 3 and 7 read /episode
// and /episodefile directly. There is also nothing left to strand. So an
// inconclusive cross-check — the exact verdict a recovering instance produces,
// since a season whose episodes are all unmonitored is unverifiable by
// construction (see runSonarrCrossCheck's shape (a)) — does not block it.
//
// FAILED and unrecognized still block everything, and that rule has no
// exceptions: a data layer with proven disagreements taints every read this
// decision rests on, and an unrecognized status is a bug signal rather than a
// state to reason from.
func recoveryGateBlockReason(cc crossCheckResult) string {
	switch cc.status {
	case crossCheckStatusPassed, crossCheckStatusInconclusive:
		return ""
	case crossCheckStatusFailed:
		return "the cross-check found a disagreement, which blocks every write including a recovery"
	default:
		return "the cross-check status is unrecognized, which blocks every write including a recovery"
	}
}

// runSonarrWritePass is the Sonarr engine's third pass, and the counterpart of
// runWritePass (decision.go) — same outcomes, same reconciliation identity,
// same §2.6 "log it, count it, move on" posture. Only the unit differs (a
// SEASON, not a movie) and one path is added that Radarr has no equivalent of.
//
// TWO GATED PATHS, not one gate with an exception (binding controller ruling,
// Phase 7 final):
//
//   - The ORDINARY write path, gated exactly as Radarr's is by the shared
//     writeGateBlockReason: an explicit cross-check PASS, plus evidence that
//     the pass says something about the would-unmonitor decisions this pass is
//     about to act on. FAILED, inconclusive, and any status a future change
//     might add block it, loudly, with the withheld accounting.
//
//   - The RECOVERY path, gated by recoveryGateBlockReason, for the one season
//     shape whose remaining action does not depend on the signal the ordinary
//     gate exists to validate: a still-monitored, complete, fully-aired,
//     tag-clean season every one of whose episodes is ALREADY unmonitored, so
//     only the season flag is left to write. unmonitorSeason decides that from
//     FRESH data and returns it (recovery); recoveryCandidate is only a cost
//     filter in front of it. Such a season's rule-4 pass is structural
//     (/wanted/cutoff is filtered to monitored episodes), rules 2/3/7 read
//     /episode and /episodefile directly, and the write cannot strand anything
//     — there is nothing left to strand. It is therefore permitted under an
//     inconclusive cross-check as well as a passed one, and blocked absolutely
//     under FAILED or an unrecognized status.
//
//     This is what a partially completed write leaves behind (the episode call
//     landed, the season PUT did not) — and, because such a season is
//     unverifiable by construction under cross-check shape (a), it is also the
//     one state the ordinary gate could never authorize the retry of. Without
//     this path a single 500 on the season half would leave a season
//     half-written forever.
//
// The second gate in front of every write, on both paths, is the decision's
// wouldUnmonitor bool. Never its reason text.
//
// The six outcomes (see runWritePass's own doc comment for the two review
// rounds that produced the first five):
//
//   - unmonitored: BOTH write calls were made and BOTH were confirmed by the
//     server's own response. The only outcome that logs msg=unmonitor.
//     Counted in SEASONS, the decision unit — never episodes, whose count is
//     an implementation detail of how a season is unmonitored.
//
//   - recoveredWrites: a confirmed write on the recovery path — the season PUT
//     alone, because there was no monitored episode left to write. Counted
//     separately from unmonitored and never folded into it: it is a different
//     event (finishing something already half-done, on a season no cross-check
//     sample can speak for), and a human who sees writes on a line that also
//     says crossCheck=inconclusive must be able to find the number that
//     explains the pairing. Each one also warns, naming its season.
//
//   - writeErrors: something was rejected or could not be read. Includes the
//     partial completion controller resolution 1 defines — episode call done,
//     season PUT refused by the server — whose error names the completed half
//     because the season is left monitored and the next cycle must be
//     expected to revisit it. Also includes an EPISODE write whose response
//     contradicts it or cannot confirm it: the season PUT is withheld in that
//     case, so the season is definitively still monitored and this is a
//     failure, not an unverifiable success. Reported as writeRehearsalErrors
//     in dry-run, where no write was attempted and only a rehearsal can have
//     failed.
//
//   - writeEchoUnverified: a 2xx from the SEASON write whose body cannot
//     settle whether the change landed. Probably applied, cannot be proven,
//     and must not be reported as either success or failure. It is only ever
//     the season half: the episode half's unconfirmed cases stop the write
//     before the season PUT and are counted above, because "treat it as
//     applied" would there be a claim about a season that was never written.
//
//   - writesRefused: unmonitorSeason declined before any HTTP write was sent —
//     the exclusion tag reappeared, tags or monitored could not be read, the
//     series or season was already unmonitored (a race), the season's own
//     state could not be verified, the season started airing since the
//     decision, or the recovery path's precondition no longer held. Each logs
//     its specific reason at the moment it refuses.
//
//   - withheld: no attempt was made at all — a gate blocked the season, or a
//     dry-run stopped at the gate immediately before the PUT.
//
// Every pending season ends in exactly one of them:
//
//	wouldUnmonitor == unmonitored + recoveredWrites + echoUnverified
//	                  + writeErrors + writesRefused + withheld
func runSonarrWritePass(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance, decisions []seasonDecision, cc crossCheckResult, exclusionTagID int, tagActive bool, dryRun bool) (unmonitored, recovered, writeErrors, echoUnverified, writesRefused, withheld int) {
	pending := 0
	for _, d := range decisions {
		if d.wouldUnmonitor {
			pending++
		}
	}

	gateBlocked := writeGateBlockReason(cc, pending)
	recoveryBlocked := recoveryGateBlockReason(cc)

	// Both gates shut: nothing is written, nothing is even FETCHED, and the
	// pass returns exactly as the Radarr twin does. An open ordinary gate
	// implies an open recovery gate (writeGateBlockReason only opens on an
	// explicit pass, which recoveryGateBlockReason also admits), so this is the
	// only combination that ends the pass here.
	if gateBlocked != "" && recoveryBlocked != "" {
		logGateWithheld(logger, inst, cc, gateBlocked, recoveryBlocked, pending, pending, 0, dryRun)
		return 0, 0, 0, 0, 0, pending
	}

	// The ordinary gate opened, but "opened" is not the same as "the sample was
	// clean": anything the cross-check could not verify is named here, at the
	// moment of consequence, with the ratio. It is deliberately NOT logged when
	// the ordinary gate is shut — an inconclusive cross-check always has
	// unverifiable samples, and a line saying writes "proceed" on a pass that
	// authorized none of them would contradict the gate line itself.
	if gateBlocked == "" && pending > 0 && cc.unverifiable > 0 {
		attrs := []any{"instance", inst.Name, "type", inst.Type}
		attrs = append(attrs, cc.logAttrs()...)
		attrs = append(attrs, "pendingWrites", pending, "dryRun", dryRun)
		msg := "writes proceeding on a partially verified cross-check: some sampled seasons could not be verified"
		if dryRun {
			msg = "dry-run: write rehearsal proceeding on a partially verified cross-check: some sampled seasons could not be verified (no write is sent; this is a rehearsal)"
		}
		logger.Warn(msg, attrs...)
	}

	shutdownNoted := false
	for _, d := range decisions {
		if !d.wouldUnmonitor {
			continue
		}

		// PHASE 8: the shutdown boundary, and the reason it is drawn HERE
		// rather than inside the write (binding controller note 4). A season is
		// two calls; unmonitorSeason detaches both from this cancellation so an
		// in-flight season finishes the pair, and this check is what stops the
		// NEXT one from starting. Interrupting mid-pair would manufacture
		// exactly the episode-written-season-unwritten state the recovery path
		// exists to mop up.
		if ctx.Err() != nil {
			withheld++
			if !shutdownNoted {
				shutdownNoted = true
				logger.Info("shutdown requested: the remaining pending season writes for this instance are withheld and the next cycle will revisit them",
					"instance", inst.Name, "type", inst.Type, "dryRun", dryRun)
			}
			continue
		}

		// With the ordinary gate shut, the recovery path is the only one left,
		// so a season that does not even look like a recovery candidate is
		// withheld without a fetch.
		requireRecovery := gateBlocked != ""
		if requireRecovery && !recoveryCandidate(d) {
			withheld++
			continue
		}

		written, recovery, err := unmonitorSeason(ctx, logger, client, inst, d.seriesID, d.season, exclusionTagID, tagActive, dryRun, requireRecovery)
		if isWriteRefusal(err) {
			// unmonitorSeason already logged the specific reason at the point
			// it refused, so nothing more is logged here and no cause is lost
			// by sharing a counter. None is a write failure (no HTTP write was
			// sent) and none is a no-op (something DID need doing).
			writesRefused++
			continue
		}
		if errors.Is(err, errWriteUnverified) {
			echoUnverified++
			logger.Warn("unmonitor write accepted but the response was unverifiable; treat it as applied and let the next cycle reconcile it",
				"instance", inst.Name, "type", inst.Type, "seriesId", d.seriesID, "series", d.series, "season", d.season, "error", err)
			continue
		}
		if err != nil {
			writeErrors++
			msg := "unmonitor write failed; skipping this season for the cycle"
			if dryRun {
				msg = "unmonitor write rehearsal failed; no write was attempted (dry-run), and this season is skipped for the cycle"
			}
			logger.Error(msg,
				"instance", inst.Name, "type", inst.Type, "seriesId", d.seriesID, "series", d.series, "season", d.season, "error", err)
			continue
		}
		if !written {
			// Dry-run: unmonitorSeason withheld both writes at their own gates
			// (§2.1) and has already logged it at debug. The ONLY remaining
			// (false, nil) case — every refusal and every race is a
			// sentinel-wrapped error, precisely so nothing real falls silently
			// through here.
			withheld++
			continue
		}

		if recovery {
			recovered++
			// One WARN per recovery, naming its season (binding). A recovery is
			// rarer and more consequential than an ordinary write: it finishes
			// something a previous cycle left half-done, and — when the
			// ordinary gate is shut — it is the one write in this project that
			// happens without cross-check authorization, so it must be
			// identifiable per season rather than only counted per instance.
			// gateBlocked names the authorization it did not have; its absence
			// says the ordinary gate was open and this season simply had
			// nothing left but its flag.
			recoveryAttrs := []any{"instance", inst.Name, "seriesId", d.seriesID, "series", d.series, "season", d.season,
				"reason", d.reason, "profile", d.profileName, "recovery", true, "crossCheck", cc.status}
			if gateBlocked != "" {
				recoveryAttrs = append(recoveryAttrs, "gateBlocked", gateBlocked,
					"recoveryReason", "the ordinary write gate was shut; every episode of this season was already unmonitored, so only the season flag remained to write and it cannot strand anything")
			}
			logger.Warn("completing a previously partial season unmonitor", recoveryAttrs...)
			continue
		}

		unmonitored++
		logger.Info("unmonitor",
			"instance", inst.Name, "seriesId", d.seriesID, "series", d.series, "season", d.season, "reason", d.reason, "profile", d.profileName)
	}

	// The gate line comes AFTER the pass, not before it, so every number on it
	// is a fact rather than a prediction: withheldWrites is what this pass
	// really withheld (in dry-run, that is everything) and recoveredWrites is
	// what really completed. The previous shape logged its counts up front and
	// had to rename both keys in dry-run to stop them claiming writes that were
	// never sent — a line that has to disclaim itself is a line reporting the
	// wrong thing.
	if gateBlocked != "" {
		logGateWithheld(logger, inst, cc, gateBlocked, recoveryBlocked, pending, withheld, recovered, dryRun)
	}

	return unmonitored, recovered, writeErrors, echoUnverified, writesRefused, withheld
}

// logGateWithheld reports a shut ordinary write gate: what it withheld, what
// the recovery path completed in spite of it, and — when the recovery path was
// shut too — why nothing at all could run.
//
// "nothing was written" and "nothing needed writing" must never look the same
// in the log, which is why this line exists at all; the level rule is the
// Radarr pass's own (Phase 5 noise-budget fix, carried forward): blocking a
// pass that had nothing to write anyway is a health signal rather than an
// alarm, but an UNRECOGNIZED status is itself a bug signal and stays WARN
// however little was pending.
func logGateWithheld(logger *slog.Logger, inst Instance, cc crossCheckResult, gateBlocked, recoveryBlocked string, pending, withheld, recovered int, dryRun bool) {
	attrs := []any{"instance", inst.Name, "type", inst.Type, "crossCheck", cc.status}
	attrs = append(attrs, cc.logAttrs()...)
	attrs = append(attrs, "withheldWrites", withheld, "recoveredWrites", recovered, "dryRun", dryRun)
	msg := "writes withheld for this instance: " + gateBlocked
	if withheld == 0 && recovered > 0 {
		// Nothing was left for the gate to withhold — every pending season of
		// this instance turned out to be a recovery. "writes withheld" beside
		// withheldWrites=0 would be a line contradicting its own number.
		msg = "the ordinary write gate is shut (" + gateBlocked + ") but withheld nothing: every pending season was completed by the recovery path"
	}
	if recoveryBlocked != "" {
		msg += "; the recovery path is shut too: " + recoveryBlocked
	}
	if pending > 0 || !isKnownCrossCheckStatus(cc.status) {
		logger.Warn(msg, attrs...)
		return
	}
	logger.Info(msg, attrs...)
}

// seriesLibraryContainsID reports whether any series in the library carries
// id. Used only to tell "--only-id names a series this instance does not
// have" (a warning, not an error) apart from a target that exists but is
// filtered out by the decision rules. Mirrors libraryContainsID (decision.go).
func seriesLibraryContainsID(series []seriesElement, id int) bool {
	for _, s := range series {
		if s.ID != nil && *s.ID == id {
			return true
		}
	}
	return false
}
