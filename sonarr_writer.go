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
// is a plain bool rather than a constant because the field must be present in
// the JSON; it is only ever set to false by this project — nothing here ever
// re-monitors anything.
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
// told, by the server itself, that it did NOT happen: a 2xx whose echo names
// an episode this write asked to unmonitor and says it is still monitored.
//
// It is a CONFIRMED FAILURE, not a failure to confirm — the exact line
// verifySeasonWriteEcho draws on the season half, drawn here too because the
// episode half is the one where getting it wrong strands something. It is
// deliberately NOT errWriteUnverified: that class means "probably applied,
// treat it as done, the next cycle will reconcile it", and this shape is the
// server saying the opposite.
var errEpisodeMonitorContradicted = errors.New("the episode monitor response says an episode this write asked to unmonitor is still monitored")

// errEpisodeMonitorUnconfirmed marks a 2xx from the episode write whose body
// cannot settle whether it landed: an unrecognized shape, a requested id the
// echo never mentions, a monitored value it does not state, or a body that
// could not be read at all.
//
// Like the contradicted case it stops the write before the season PUT
// (CRITICAL review fix), and for the same reason: the season PUT is the step
// that makes the season invisible to rule 1 forever, so it may only follow an
// episode write the server CONFIRMED. Aborting strands nothing — the season
// stays monitored, so the next cycle re-reads /episode, drops whatever really
// landed from the id list, and retries. Abort converges whether or not the
// episodes changed; proceeding strands them in the branch where they did not.
//
// It is also not errWriteUnverified, because no season write was even
// attempted: the season is definitively still monitored, and reporting it as
// accepted-but-unconfirmed would tell a human the opposite of what happened.
var errEpisodeMonitorUnconfirmed = errors.New("the episode monitor write was accepted but its response could not confirm it")

// errRecoveryAllowanceViolated marks a season the write pass admitted through
// the cross-check gate's narrow recovery allowance (see
// seasonWriteRecoverySignature) turning out, at the moment of the write, to
// still contain monitored episodes.
//
// The allowance exists only because unmonitoring a season whose episodes are
// ALL already unmonitored cannot strand anything — there is nothing left to
// strand. A season that no longer matches that description is an ordinary
// write, and an ordinary write needs the ordinary gate, which for this
// instance is shut. Refusing (loudly, counted, never retried) is the whole
// point of calling the allowance narrow.
var errRecoveryAllowanceViolated = errors.New("the season admitted by the cross-check gate's recovery allowance still has monitored episodes at write time")

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
// recoveryOnly is set by the write pass for the one class of season it may
// write while the cross-check gate is shut: a season all of whose episodes are
// already unmonitored (see seasonWriteRecoverySignature). It is a promise this
// function re-verifies against its own fresh data — if any episode of the
// season is still monitored, the write is refused rather than performed on
// unauthorized evidence.
//
// Errors are returned, never retried (§2.6); the caller logs them and moves
// to the next season.
func unmonitorSeason(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance, seriesID, seasonNumber, exclusionTagID int, tagActive bool, dryRun bool, recoveryOnly bool) (written bool, err error) {
	path := seriesPath(seriesID)
	subject := fmt.Sprintf("series %d season %d", seriesID, seasonNumber)

	body, err := fetchBody(ctx, client, path, nil)
	if err != nil {
		return false, fmt.Errorf("fetching series %d before write: %w", seriesID, err)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return false, fmt.Errorf("%s: response to the pre-write fetch is not a JSON object: %w", subject, err)
	}

	if err := verifySeriesIdentity(payload, seriesID); err != nil {
		return false, err
	}

	title := titleFromPayload(payload)
	attrs := []any{"instance", inst.Name, "type", inst.Type, "seriesId", seriesID, "series", title, "season", seasonNumber}

	// Step 4: §2.5, against the FRESH payload's own tags — the same function
	// the movie path calls, not a second copy of the same four refusals.
	if tagActive {
		if err := preWriteExclusionTagCheck(logger, payload, "series", subject, exclusionTagID, attrs); err != nil {
			return false, err
		}
	}

	// Step 5a: the series-level monitored flag. It is READ here and written
	// nowhere: a series that is no longer monitored means the world moved
	// under this decision, and unmonitoring one of its seasons would be a
	// write nobody asked for.
	seriesMonitored, err := readMonitoredFlag(logger, payload, "series", subject, attrs)
	if err != nil {
		return false, err
	}
	if !seriesMonitored {
		logger.Info("series is no longer monitored as of the pre-write fetch, skipping season write", attrs...)
		return false, fmt.Errorf("%s: %w: the series itself is no longer monitored", subject, errAlreadyUnmonitoredAtWrite)
	}

	seasonElems, targetIdx, targetSeason, err := locateTargetSeason(logger, payload, subject, seasonNumber, attrs)
	if err != nil {
		return false, err
	}

	// Step 5b: the season's own monitored flag, with the same null-decode
	// discipline the series-level one just had.
	seasonMonitored, err := readMonitoredFlag(logger, targetSeason, "season", subject, attrs)
	if err != nil {
		return false, err
	}
	if !seasonMonitored {
		logger.Info("season already unmonitored as of the pre-write fetch, skipping write", attrs...)
		return false, fmt.Errorf("%s: %w", subject, errAlreadyUnmonitoredAtWrite)
	}

	// Step 6: fresh episode data, then the airing guard re-run over it. A
	// fetch failure is not a refusal — nothing about the season is untrusted,
	// the data simply could not be read — so it is returned as an ordinary
	// error and counted as a write failure, exactly as a failed pre-write GET
	// is on the movie path.
	episodes, ok := fetchEpisodes(ctx, logger, client, inst, seriesID)
	if !ok {
		return false, fmt.Errorf("%s: fresh episode data could not be fetched before the write", subject)
	}
	seasonEpisodes := episodesOfSeason(episodes, seasonNumber)
	if err := verifySeasonStillWritable(logger, targetSeason, seasonEpisodes, subject, attrs); err != nil {
		return false, err
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
	var episodeIDs []int
	for _, e := range seasonEpisodes {
		if e.ID == nil {
			logger.Warn("an episode of this season has no id in the pre-write fetch; it could not be named in the episode monitor write, so unmonitoring the season would strand it, refusing to write",
				append(append([]any(nil), attrs...), "episodeNumber", derefOrAbsent(e.EpisodeNumber))...)
			return false, fmt.Errorf("%s: %w: an episode of this season has no id in the pre-write fetch", subject, errSeasonUnverifiableAtWrite)
		}
		if e.Monitored == nil {
			logger.Warn("an episode of this season has an absent or JSON-null monitored field in the pre-write fetch; unmonitoring the season would strand it in an unknown state, refusing to write",
				append(append([]any(nil), attrs...), "episodeId", *e.ID)...)
			return false, fmt.Errorf("%s: %w: episode %d has no readable monitored field in the pre-write fetch", subject, errSeasonUnverifiableAtWrite, *e.ID)
		}
		if !*e.Monitored {
			continue
		}
		episodeIDs = append(episodeIDs, *e.ID)
	}

	// The recovery allowance's own promise, re-verified against the fresh,
	// complete, fully-validated episode set this function just built (every
	// episode's id and monitored value readable, the set matching the season's
	// own statistics). The gate that would normally authorize this write is
	// shut; the only reason this season got past it is that unmonitoring it
	// cannot strand anything, and that is true only while every episode of it
	// is already unmonitored.
	if recoveryOnly && len(episodeIDs) > 0 {
		logger.Warn("this season was admitted only by the cross-check gate's recovery allowance (every episode already unmonitored), but the pre-write fetch shows monitored episodes; refusing to write",
			append(append([]any(nil), attrs...), "monitoredEpisodes", len(episodeIDs))...)
		return false, fmt.Errorf("%s: %w: %d episode(s) are still monitored", subject, errRecoveryAllowanceViolated, len(episodeIDs))
	}

	// The season payload is assembled BEFORE either write goes out, even
	// though it is the second one that sends it. An assembly failure (or the
	// series-level monitored guard inside it tripping) must not be discovered
	// after the episode call has already landed: that would leave the season
	// half-written for a reason that had nothing to do with the server. It
	// also makes the dry-run rehearsal complete — the payload is really built,
	// really checked, and only then withheld.
	encoded, err := assembleSeasonWrite(payload, seasonElems, targetIdx, targetSeason, subject)
	if err != nil {
		return false, err
	}

	// FIRST write call (binding order).
	episodesWritten := false
	if len(episodeIDs) == 0 {
		logger.Debug("no monitored episodes remain in this season; the episode monitor write is not needed",
			append(append([]any(nil), attrs...), "episodes", len(seasonEpisodes))...)
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
			// left open. Aborting strands nothing: the season stays monitored,
			// the next cycle re-reads /episode, and the retry is smaller by
			// whatever really landed.
			if err := putEpisodeMonitor(ctx, client, episodeIDs, subject); err != nil {
				logger.Warn("episode monitor write could not be confirmed; the season write is withheld and the season is left monitored so the next cycle can retry it",
					append(append([]any(nil), attrs...), "episodeIds", len(episodeIDs), "error", err)...)
				return false, err
			}
			episodesWritten = true
		}
	}

	// §2.1 again: the SECOND dry-run gate, immediately before the second HTTP
	// write call. Nothing below this line may run in dry-run mode.
	if dryRun {
		logger.Debug("dry-run: write withheld immediately before the PUT",
			append(append([]any(nil), attrs...), "method", http.MethodPut, "path", path)...)
		return false, nil
	}

	resp, err := client.DoJSON(ctx, http.MethodPut, path, encoded)
	if err != nil {
		if episodesWritten {
			// Controller resolution 1's partial completion: the episode write
			// landed and the series write did not, so the season is STILL
			// monitored. Naming the completed half is what lets a human read
			// the state the season was actually left in — and the next cycle
			// converges on its own, since the episodes now reading false are
			// excluded from that attempt's id list.
			return false, fmt.Errorf("%s: the episode monitor write completed but the season write failed, so the season is still monitored: %w", subject, err)
		}
		return false, fmt.Errorf("writing %s: %w", subject, err)
	}
	defer resp.Body.Close()

	echo, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes))
	if err != nil {
		return false, fmt.Errorf("%s: the write returned %d but its body could not be read, so the change is unconfirmed (%w): %v", subject, resp.StatusCode, errWriteUnverified, err)
	}
	if err := verifySeasonWriteEcho(echo, seriesID, seasonNumber, resp.StatusCode, subject); err != nil {
		return false, err
	}

	// written=true still requires BOTH calls confirmed (controller resolution
	// 1) — it just cannot be decided here any more, because an unconfirmed
	// episode write already returned above without sending this PUT at all.
	return true, nil
}

// episodesOfSeason filters a fresh /episode response down to one season.
// An episode whose own seasonNumber is absent belongs to no season here, the
// same way it is excluded from every season's evaluation at decision time —
// and the completeness check that follows is what turns that exclusion into a
// refusal rather than a silently short episode set.
func episodesOfSeason(episodes []episodeElement, seasonNumber int) []episodeElement {
	var out []episodeElement
	for _, e := range episodes {
		if e.SeasonNumber == nil || *e.SeasonNumber != seasonNumber {
			continue
		}
		out = append(out, e)
	}
	return out
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
func assembleSeasonWrite(payload map[string]json.RawMessage, seasonElems []json.RawMessage, targetIdx int, targetSeason map[string]json.RawMessage, subject string) ([]byte, error) {
	originalMonitored, hadMonitored := payload[monitoredKey]

	targetSeason[monitoredKey] = unmonitoredValue
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
func putEpisodeMonitor(ctx context.Context, client *APIClient, episodeIDs []int, subject string) error {
	body, err := json.Marshal(episodeMonitorRequest{EpisodeIDs: episodeIDs, Monitored: false})
	if err != nil {
		return fmt.Errorf("%s: encoding the episode monitor request: %w", subject, err)
	}

	resp, err := client.DoJSON(ctx, http.MethodPut, episodeMonitorPath, body)
	if err != nil {
		return fmt.Errorf("%s: unmonitoring the season's episodes: %w", subject, err)
	}
	defer resp.Body.Close()

	echo, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes))
	if err != nil {
		return fmt.Errorf("%s: the episode monitor write returned %d but its body could not be read (%w): %v", subject, resp.StatusCode, errEpisodeMonitorUnconfirmed, err)
	}
	if err := verifyEpisodeMonitorEcho(echo, episodeIDs); err != nil {
		if errors.Is(err, errEpisodeMonitorContradicted) {
			return fmt.Errorf("%s: the episode monitor write returned %d but %w", subject, resp.StatusCode, err)
		}
		return fmt.Errorf("%s: the episode monitor write returned %d but %v (%w)", subject, resp.StatusCode, err, errEpisodeMonitorUnconfirmed)
	}
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
func verifyEpisodeMonitorEcho(echo []byte, episodeIDs []int) error {
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
		if *monitored {
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
func verifySeasonWriteEcho(echo []byte, seriesID, seasonNumber, status int, subject string) error {
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
	for _, s := range elems {
		if s.SeasonNumber == nil || *s.SeasonNumber != seasonNumber {
			continue
		}
		// *bool, not bool: a "monitored": null in the echo would decode into a
		// plain bool as false with NO error, and this function would then
		// report a CONFIRMED write on a server that said nothing about the
		// field at all.
		if s.Monitored == nil {
			return fmt.Errorf("%s: the write returned %d but the returned season's %q is absent or JSON null, which does not confirm the change (%w): %s", subject, status, monitoredKey, errWriteUnverified, bodySnippet(echo))
		}
		if *s.Monitored {
			return fmt.Errorf("%s: the write returned %d but the returned object still has the season %q: true; the season was NOT unmonitored: %s", subject, status, monitoredKey, bodySnippet(echo))
		}
		return nil
	}
	return fmt.Errorf("%s: the write returned %d but the returned object does not mention this season, so the change is unconfirmed (%w): %s", subject, status, errWriteUnverified, bodySnippet(echo))
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

// seasonWriteRecoverySignature reports whether a season decision carries the
// one signature the write gate's recovery allowance is named for: EVERY
// episode of a still-monitored season explicitly reports monitored == false.
//
// That is exactly what a partially completed write leaves behind — the
// /episode/monitor call landed, the season PUT did not — and it is the state
// the project has to be able to finish, or a single 500 on the season half
// would leave a season half-written forever.
//
// It is deliberately a signature and not a heuristic. An episode whose
// monitored flag is absent or JSON null does not match (untrusted input never
// earns an allowance), and an empty episode set does not match either (a
// signature nothing can satisfy must not be satisfied by nothing).
func seasonWriteRecoverySignature(d seasonDecision) bool {
	if len(d.crossCheckEpisodes) == 0 {
		return false
	}
	for _, ep := range d.crossCheckEpisodes {
		if ep.monitored == nil || *ep.monitored {
			return false
		}
	}
	return true
}

// recoveryAllowed decides whether one season may be written while the
// instance's write gate is SHUT. It is the narrow, explicitly named exception
// the review round directed in place of Phase 7's widening of what the
// cross-check counts as "verified".
//
// Why an exception is needed at all: with cross-check shape (a) whole again
// (see runSonarrCrossCheck), a would-unmonitor season whose episodes are all
// unmonitored has nothing comparable left in it, so it is unverifiable by
// construction and can never earn the gate's would-unmonitor evidence. Since
// that is precisely the state a partial write leaves, the gate would block its
// own retry forever.
//
// Why the exception is safe, and why it is drawn HERE rather than by relaxing
// what counts as evidence: the gate exists to stop this project acting on
// decision data it could not corroborate. For a season whose every episode is
// already unmonitored, the write cannot be acted on wrongly — Sonarr cannot
// grab an unmonitored episode whatever the season flag says, so flipping the
// season changes nothing about the world except that the project stops
// re-deciding it. The stranding hazard the gate guards is structurally absent
// because there is nothing left to strand. Every OTHER pending season of the
// same instance stays withheld, which is what makes this an allowance rather
// than an open gate.
//
// Its three conditions, each load-bearing:
//
//   - The signature itself (seasonWriteRecoverySignature).
//   - A cross-check that did not FAIL and is not unrecognized. A disagreement
//     must stop this project before writes — that rule has no exceptions, and
//     an unrecognized status is a bug signal, not a state to reason from.
//     "inconclusive" IS admitted: a sample that verified nothing is exactly
//     what an instance recovering from a partial write looks like.
//   - unmonitorSeason re-verifies the signature against its own fresh episode
//     data (recoveryOnly) and refuses if any episode is monitored by then, so
//     an allowance granted on decision-time data can never authorize an
//     ordinary write on stale evidence.
func recoveryAllowed(cc crossCheckResult, d seasonDecision) bool {
	if cc.status != crossCheckStatusPassed && cc.status != crossCheckStatusInconclusive {
		return false
	}
	return seasonWriteRecoverySignature(d)
}

// runSonarrWritePass is the Sonarr engine's third pass, and the exact
// counterpart of runWritePass (decision.go) — same two gates, same five
// outcomes, same reconciliation identity, same §2.6 "log it, count it, move
// on" posture. Only the unit differs: a SEASON, not a movie.
//
// The two gates in front of every write:
//
//   - The cross-check must have authorized writes, judged by the same
//     evidence-based rule the Radarr pass uses (writeGateBlockReason, shared
//     rather than reimplemented): an explicit PASS, plus evidence that the
//     pass says something about the would-unmonitor decisions this pass is
//     about to act on. FAILED and inconclusive — and any status a future
//     change might add — block the entire pass, loudly.
//
//     A shut gate has exactly ONE exception, and it is named rather than
//     implied: a season whose every episode is already unmonitored
//     (recoveryAllowed). It exists because that state is what a partially
//     completed write leaves behind and is unverifiable by construction, so
//     the gate would otherwise block its own retry forever; it is safe
//     because unmonitoring such a season cannot strand anything. Every other
//     pending season of the instance is still withheld.
//
//   - The decision's wouldUnmonitor bool. Never its reason text.
//
// The five outcomes, and why five (see runWritePass's own doc comment for the
// two review rounds that produced them):
//
//   - unmonitored: BOTH write calls were made and BOTH were confirmed by the
//     server's own response. The only outcome that logs msg=unmonitor.
//     Counted in SEASONS, the decision unit — never episodes, whose count is
//     an implementation detail of how a season is unmonitored.
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
//     decision, or a season admitted by the recovery allowance turned out to
//     have monitored episodes after all. Each logs its specific reason at the
//     moment it refuses.
//
//   - withheld: no attempt was made at all — the gate blocked the pass, or a
//     dry-run stopped at the gate immediately before the PUT.
//
//     wouldUnmonitor == unmonitored + echoUnverified + writeErrors
//
//   - writesRefused + withheld
func runSonarrWritePass(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance, decisions []seasonDecision, cc crossCheckResult, exclusionTagID int, tagActive bool, dryRun bool) (unmonitored, writeErrors, echoUnverified, writesRefused, withheld int) {
	pending := 0
	for _, d := range decisions {
		if d.wouldUnmonitor {
			pending++
		}
	}

	// gateBlocked == "" is the ordinary authorization: every pending write may
	// go ahead. When it is shut, the only seasons that still get written are
	// the ones the recovery allowance names one at a time by signature (see
	// recoveryAllowed) — every other pending season is withheld exactly as
	// before.
	gateBlocked := writeGateBlockReason(cc, pending)
	recovering := 0
	if gateBlocked != "" {
		for _, d := range decisions {
			if d.wouldUnmonitor && recoveryAllowed(cc, d) {
				recovering++
			}
		}
		attrs := []any{"instance", inst.Name, "type", inst.Type, "crossCheck", cc.status}
		attrs = append(attrs, cc.logAttrs()...)
		attrs = append(attrs, "withheldWrites", pending-recovering, "recoveryWrites", recovering, "dryRun", dryRun)
		msg := "writes withheld for this instance: " + gateBlocked
		if recovering > 0 {
			msg = "cross-check gate shut (" + gateBlocked + "); writing ONLY the season(s) whose every episode is already unmonitored — which cannot strand anything — and withholding the rest"
		}
		// Same noise-budget rule the Radarr pass applies: blocking a pass that
		// had nothing to write anyway is a health signal, not an alarm — but
		// an UNRECOGNIZED status is itself a bug signal and stays WARN however
		// little was pending.
		if pending > 0 || !isKnownCrossCheckStatus(cc.status) {
			logger.Warn(msg, attrs...)
		} else {
			logger.Info(msg, attrs...)
		}
		if recovering == 0 {
			return 0, 0, 0, 0, pending
		}
	}

	if pending > 0 && cc.unverifiable > 0 {
		attrs := []any{"instance", inst.Name, "type", inst.Type}
		attrs = append(attrs, cc.logAttrs()...)
		attrs = append(attrs, "pendingWrites", pending, "dryRun", dryRun)
		msg := "writes proceeding on a partially verified cross-check: some sampled seasons could not be verified"
		if dryRun {
			msg = "dry-run: write rehearsal proceeding on a partially verified cross-check: some sampled seasons could not be verified (no write is sent; this is a rehearsal)"
		}
		logger.Warn(msg, attrs...)
	}

	for _, d := range decisions {
		if !d.wouldUnmonitor {
			continue
		}

		recoveryOnly := false
		if gateBlocked != "" {
			if !recoveryAllowed(cc, d) {
				withheld++
				continue
			}
			recoveryOnly = true
		}

		written, err := unmonitorSeason(ctx, logger, client, inst, d.seriesID, d.season, exclusionTagID, tagActive, dryRun, recoveryOnly)
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

		unmonitored++
		logger.Info("unmonitor",
			"instance", inst.Name, "seriesId", d.seriesID, "series", d.series, "season", d.season, "reason", d.reason, "profile", d.profileName)
	}

	return unmonitored, writeErrors, echoUnverified, writesRefused, withheld
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
