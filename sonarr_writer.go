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
//  8. Believe the server, not the status code, on both calls.
//
// Errors are returned, never retried (§2.6); the caller logs them and moves
// to the next season.
func unmonitorSeason(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance, seriesID, seasonNumber, exclusionTagID int, tagActive bool, dryRun bool) (written bool, err error) {
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
	// rather than a duplicate. An episode whose own monitored could not be
	// read is excluded too — but warned about, because excluding it means it
	// keeps whatever state it has inside a season that is about to stop being
	// evaluated at all.
	var episodeIDs []int
	for _, e := range seasonEpisodes {
		if e.ID == nil {
			logger.Warn("episode has no id in the pre-write fetch; it cannot be named in the episode monitor write",
				append(append([]any(nil), attrs...), "episodeNumber", derefOrAbsent(e.EpisodeNumber))...)
			continue
		}
		if e.Monitored == nil {
			logger.Warn("episode monitored is absent or JSON null in the pre-write fetch; excluded from the episode monitor write",
				append(append([]any(nil), attrs...), "episodeId", *e.ID)...)
			continue
		}
		if !*e.Monitored {
			continue
		}
		episodeIDs = append(episodeIDs, *e.ID)
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

	// FIRST write call (binding order). episodesConfirmed stays true when no
	// call was needed at all: "nothing to do" is not "could not be verified".
	episodesConfirmed := true
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
			confirmed, err := putEpisodeMonitor(ctx, client, episodeIDs, subject)
			if err != nil {
				return false, err
			}
			episodesWritten = true
			episodesConfirmed = confirmed
			if !confirmed {
				logger.Warn("episode monitor write accepted but the response was unverifiable; proceeding to the season write and letting the next cycle reconcile it",
					append(append([]any(nil), attrs...), "episodeIds", len(episodeIDs))...)
			}
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
	if !episodesConfirmed {
		// Both calls must be confirmed before this counts as a write
		// (controller resolution 1). The season write is confirmed and the
		// episode write probably landed too — calling it a failure would say
		// two things that are likely both false — so it joins the
		// accepted-but-unconfirmed class and the next cycle settles it.
		return false, fmt.Errorf("%s: the season write was confirmed but the episode monitor write was not (%w)", subject, errWriteUnverified)
	}

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
	if err := json.Unmarshal(rawStats, &stats); err != nil || stats.TotalEpisodeCount == nil {
		logger.Warn("season statistics.totalEpisodeCount could not be read from the pre-write fetch; refusing to write",
			append(append([]any(nil), attrs...), "error", err)...)
		return fmt.Errorf("%s: %w: %q.totalEpisodeCount could not be read from the pre-write fetch", subject, errSeasonUnverifiableAtWrite, statisticsKey)
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

// putEpisodeMonitor issues the episode half of the write and reports whether
// the server's response confirms it. A non-2xx (or an unreadable body) is an
// error — nothing was written and the caller must not proceed to the season
// write, or it would leave monitored episodes inside a season nothing will
// ever evaluate again. A 2xx whose body cannot settle the question returns
// (false, nil): the write probably landed, so aborting would be worse than
// proceeding, but it cannot be counted as confirmed.
func putEpisodeMonitor(ctx context.Context, client *APIClient, episodeIDs []int, subject string) (confirmed bool, err error) {
	body, err := json.Marshal(episodeMonitorRequest{EpisodeIDs: episodeIDs, Monitored: false})
	if err != nil {
		return false, fmt.Errorf("%s: encoding the episode monitor request: %w", subject, err)
	}

	resp, err := client.DoJSON(ctx, http.MethodPut, episodeMonitorPath, body)
	if err != nil {
		return false, fmt.Errorf("%s: unmonitoring the season's episodes: %w", subject, err)
	}
	defer resp.Body.Close()

	echo, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes))
	if err != nil {
		return false, nil
	}
	return verifyEpisodeMonitorEcho(echo, episodeIDs) == nil, nil
}

// verifyEpisodeMonitorEcho confirms the episodes the server echoed back really
// are the ones that were asked for and really do say monitored is false.
// Anything else — a body that is not a JSON array of objects, an id missing
// from it, an unreadable or true monitored value — means the change is
// unconfirmed. It returns a plain error because its only caller turns the
// distinction into a bool; the season-level echo check is where the
// unverified/contradicted distinction actually has to be preserved.
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
			return fmt.Errorf("the episode monitor response says episode %d is still monitored: %s", id, bodySnippet(echo))
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
//     expected to revisit it. Reported as writeRehearsalErrors in dry-run,
//     where no write was attempted and only a rehearsal can have failed.
//
//   - writeEchoUnverified: a 2xx whose body cannot settle whether the change
//     landed, on either call. Probably applied, cannot be proven, and must not
//     be reported as either success or failure.
//
//   - writesRefused: unmonitorSeason declined before any HTTP write was sent —
//     the exclusion tag reappeared, tags or monitored could not be read, the
//     series or season was already unmonitored (a race), the season's own
//     state could not be verified, or the season started airing since the
//     decision. Each logs its specific reason at the moment it refuses.
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

	if reason := writeGateBlockReason(cc, pending); reason != "" {
		attrs := []any{"instance", inst.Name, "type", inst.Type, "crossCheck", cc.status}
		attrs = append(attrs, cc.logAttrs()...)
		attrs = append(attrs, "withheldWrites", pending, "dryRun", dryRun)
		msg := "writes withheld for this instance: " + reason
		// Same noise-budget rule the Radarr pass applies: blocking a pass that
		// had nothing to write anyway is a health signal, not an alarm — but
		// an UNRECOGNIZED status is itself a bug signal and stays WARN however
		// little was pending.
		if pending > 0 || !isKnownCrossCheckStatus(cc.status) {
			logger.Warn(msg, attrs...)
		} else {
			logger.Info(msg, attrs...)
		}
		return 0, 0, 0, 0, pending
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

		written, err := unmonitorSeason(ctx, logger, client, inst, d.seriesID, d.season, exclusionTagID, tagActive, dryRun)
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
