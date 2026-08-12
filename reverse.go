package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
)

// reverse.go is the Phase 10 reverse scan: the same decision function, run over
// the opposite population, to catch items that are UNMONITORED but do NOT meet
// the criteria — almost always an accidental unmonitor, and invisible to
// everything else this program does.
//
// The single rule this file exists to obey is that it contains NO quality,
// airing, completeness, tag or profile logic of its own. Every judgement it
// makes is made by evaluateMovie / evaluateSeries — the functions the forward
// scan uses, called with a direction and a wanted set, and nothing else
// changed. A second copy of any rule here would be a second thing to keep
// correct, and the two copies would disagree the first time either is fixed.
//
// What this file DOES own:
//
//   - which population each direction is about (scanDirection),
//   - which skip reasons constitute a finding worth reporting,
//   - the reporting vocabulary and the per-cycle accounting,
//   - the write gate the reverse direction answers to, which is not the
//     forward one.

// scanDirection names which population an evaluation pass is about.
//
// FORWARD is everything this project did before Phase 10: monitored items,
// evaluated against the monitored wanted set, to decide what should be
// unmonitored. REVERSE is the mirror: unmonitored items, evaluated against the
// unmonitored wanted set (/wanted/cutoff?monitored=false), to find the ones
// that should not have been unmonitored at all.
//
// It is a type rather than a bool parameter so a call site cannot silently mean
// the opposite of what it says, and so the one question the evaluators ask of
// it — "which monitored state is this pass about" — is asked through a named
// method rather than an unexplained boolean.
type scanDirection int

const (
	directionForward scanDirection = iota
	directionReverse
)

// wantsMonitored is the monitored state an item must be in to belong to this
// direction's population. It is the whole of what a direction means to the
// evaluators: rule 1's expectation, and nothing else.
func (d scanDirection) wantsMonitored() bool { return d == directionForward }

// isReverseFinding reports whether a decision reason means "this item is
// unmonitored AND still fails the criteria" — the one thing the reverse scan
// reports.
//
// It is deliberately a small, explicit allowlist of EXISTING reason constants
// rather than "anything that is not wouldUnmonitor", because most of the ways
// an evaluation can decline are not findings at all and reporting them would be
// telling the human something false:
//
//   - ReasonCutoffMet (wouldUnmonitor) is the healthy case: the item meets the
//     criteria, so being unmonitored is exactly right and there is nothing to
//     report. This is the plan's own rule.
//   - ReasonNoFile: an unmonitored movie with no file is a deliberate choice a
//     human made (binding controller resolution 2); it is counted at debug and
//     never reported.
//   - ReasonExcludedByTag: §2.5 is absolute in every mode — an excluded item is
//     not written, not reported, not mentioned as a finding.
//   - ReasonTagsUnknown, ReasonUnknownProfile, ReasonCouldNotFetchCFScore,
//     ReasonSeasonEpisodesUnavailable, ReasonSeasonEpisodeDataInconsistent,
//     ReasonSeasonFileCountMismatch: untrusted input. §2.6 says never guess,
//     and "we could not read this item" is not evidence that it is below its
//     cutoff. Each already warns where it happens.
//   - ReasonUpgradesDisabled: the profile forbids upgrades, so the item cannot
//     improve whether it is monitored or not. Re-monitoring it would achieve
//     nothing, and reporting it as "unmonitored but below cutoff" would send a
//     human to fix something that is not broken.
//   - ReasonSeasonIncomplete, ReasonSeasonNotFullyAired: the plan's reverse
//     pool is complete, fully-aired seasons only.
//
// What is left is exactly the plan's "still below cutoff / below CF score".
func isReverseFinding(reason string) bool {
	switch reason {
	case ReasonQualityCutoffNotMet, ReasonCFCutoffNotMet:
		return true
	default:
		return false
	}
}

// reverseOptions is what a CYCLE says about the reverse scan: whether to run it
// at all, and whether it may write.
//
// The two are separate because they answer to different things. enabled is
// SCHEDULING — the reverse scan belongs to full-library cycles (the startup
// scan, the reconciliation sweep, a --once run) and never to a webhook or an
// --only-id run, which are surgical and forward-looking by construction.
// remonitor is CONFIGURATION — cfg.ReverseScanRemonitor, default false, the
// only thing that lets the reverse scan write anything ever.
//
// The zero value is "do not run", which is what every scoped cycle wants and
// what a caller that forgot about this phase gets.
type reverseOptions struct {
	enabled   bool
	remonitor bool
}

// fullScanReverseOptions is what a FULL-LIBRARY cycle asks for: run the reverse
// scan, and let it write only if the config says so.
//
// It exists so the three full-scan call sites — the startup scan, the
// reconciliation sweep, and a --once run — cannot drift apart from each other,
// and so that the scoped cycles' "no reverse pass" stays the plain zero value
// rather than something they have to spell.
func fullScanReverseOptions(cfg Config) reverseOptions {
	return reverseOptions{enabled: true, remonitor: cfg.ReverseScanRemonitor}
}

// reverseCounts is one instance's reverse-scan accounting for one cycle.
//
// findings is what the report-only default produces. The five write counters
// exist only when the remonitor flag is on, and they exist so that every
// finding ends in exactly one of them:
//
//	findings == remonitored + remonitorsRefused + reverseWithheld
//	            + reverseWriteErrors + reverseEchoUnverified
//
// That identity is the same discipline the forward passes are held to, and for
// the same reason: two separate review rounds of the forward write path found
// silent paths where promised writes evaporated with every counter reading
// zero, and the only structural defense is numbers that are required to add up.
type reverseCounts struct {
	// skipped means the pass could not run for this instance — the unmonitored
	// wanted set was incomplete, or a shutdown interrupted the evaluation — so
	// findings says nothing and must not be printed as though it did.
	skipped bool

	findings int

	// noFile is the pool statistic binding controller resolution 2 asks for at
	// debug only: unmonitored movies with no file are a deliberate user choice
	// and are never findings, but knowing how many there are explains the gap
	// between "unmonitored items" and "items the reverse scan looked at".
	noFile int

	remonitored    int
	refused        int
	writeErrors    int
	echoUnverified int
	withheld       int
}

// summaryAttrs renders this cycle's reverse-scan accounting for the engine's
// summary line.
//
// Three distinct states, all of which must be tellable apart at a glance:
//
//   - the pass did not run (a webhook or --only-id cycle): NOTHING is printed.
//     A reverseFindings=0 on a cycle that never looked would be a false
//     all-clear, and those cycles are the majority in a busy daemon.
//   - the pass ran but could not be trusted: reverseScan=skipped and NO finding
//     count — a number this cycle is in no position to state — but, with the
//     switch on, still the three frozen write counters, all of them 0.
//   - the pass ran: reverseFindings=N, plus — only when the write switch is on
//     — the five write counters, always present including as 0.
//
// The write counters being ABSENT with the switch off is itself the point
// (binding controller resolution 7): report-only and write-enabled cycles must
// not look alike, and the presence of the word remonitored on the line is the
// cheapest possible way for a human to tell which one they are reading. That is
// why the skipped state prints them too — REVIEW FIX (Phase 10 round 2). This
// used to return reverseScan=skipped alone, reasoning that one token saying
// "nothing happened" was the stronger statement; but it made a flag-ON skipped
// cycle byte-identical to a flag-OFF one, and it did so on precisely the cycle a
// human is most likely to be reading (something went wrong and nothing was
// re-monitored), where "is this daemon even allowed to write?" is the first
// question they have. The zeroes are honest: every path that sets skipped —
// runRadarr's incomplete wanted set and its shutdown boundary, runSonarr's twins
// — returns before remonitorMovies / remonitorSeasons is ever reached, so all
// three counters are provably 0 there. They are printed from the fields rather
// than as literal zeroes so a future path that skipped AFTER writing could not
// silently under-report itself.
//
// The two mode-dependent counters (reverseWriteErrors and its dry-run twin,
// reverseEchoUnverified) stay out of the skipped line: they are diagnostics of a
// write half that provably did not run, and they are not part of the vocabulary
// resolution 7 froze.
func (c reverseCounts) summaryAttrs(opts reverseOptions, dryRun bool) []any {
	if !opts.enabled {
		return nil
	}
	if c.skipped {
		attrs := []any{"reverseScan", "skipped"}
		if opts.remonitor {
			attrs = append(attrs, "remonitored", c.remonitored, "remonitorsRefused", c.refused, "reverseWithheld", c.withheld)
		}
		return attrs
	}
	attrs := []any{"reverseFindings", c.findings}
	if !opts.remonitor {
		return attrs
	}
	attrs = append(attrs, "remonitored", c.remonitored, "remonitorsRefused", c.refused, "reverseWithheld", c.withheld)
	// The same mode-dependent split the forward summaries make, for the same
	// reason: writeErrors counts failed WRITES, so in dry-run it is
	// unconditionally 0 and the rehearsal's own failures are reported under a
	// name that cannot be misread as "N writes failed"; an unverifiable echo
	// cannot happen in dry-run, where no PUT is ever sent.
	if dryRun {
		attrs = append(attrs, "reverseWriteErrors", 0, "reverseRehearsalErrors", c.writeErrors)
	} else {
		attrs = append(attrs, "reverseWriteErrors", c.writeErrors, "reverseEchoUnverified", c.echoUnverified)
	}
	return attrs
}

// reversePass is one instance's reverse scan, assembled by the decision engine
// that already holds every input it needs. It is a struct rather than a
// twelve-parameter function purely for legibility; nothing here outlives the
// cycle that built it.
type reversePass struct {
	// logger is the engine's UNDEMOTED logger: this pass decides the level of
	// each line it writes (findings ride the cycle's item level, warnings and
	// the summary do not), which a pre-demoted logger would take away from it.
	logger *slog.Logger
	// cycleLogger demotes INFO on repeating cycles, and exists here for exactly
	// one line: the unmonitored wanted/cutoff fetch's own totals, which are
	// news once and repetition forever after.
	cycleLogger *slog.Logger

	client *APIClient
	inst   Instance

	profiles       map[int]qualityProfile
	exclusionTagID int
	tagActive      bool

	// cc is the FORWARD cross-check's result for this cycle. The reverse scan
	// never runs a cross-check of its own — it has no second signal to compare
	// against — so what it uses this for is narrow and stated where it is used
	// (reverseWriteGateBlockReason): a data-layer health signal, not evidence
	// about any individual finding.
	cc crossCheckResult

	itemLevel slog.Level
	dryRun    bool
	opts      reverseOptions
}

// runRadarr is the Radarr reverse scan: every UNMONITORED movie, evaluated by
// evaluateMovie — the forward decision function, unchanged — against the
// unmonitored wanted set.
//
// The pool is binding controller resolution 2's: monitored == false AND hasFile
// == true. Rule 1's half (monitored) is applied here, exactly as the forward
// engine applies its own; rule 2's (hasFile) is left to evaluateMovie so that
// "has a file" keeps meaning what it means in one place, and the resulting
// no-file decisions are counted rather than reported.
//
// It costs one /moviefile fetch per unmonitored movie that reaches rule 6 —
// that is, one per unmonitored movie whose quality cutoff IS met, since the
// ones below it stop at rule 5. That is inherent to evaluating the real
// criteria rather than a cheaper approximation of them, and it is the price of
// the literal-reuse mandate; it is bounded by the unmonitored population and
// runs only on full-library cycles.
func (p reversePass) runRadarr(ctx context.Context, movies []movieListElement) reverseCounts {
	var c reverseCounts

	wantedIDs, ok := fetchWantedCutoff(ctx, p.cycleLogger, p.client, p.inst, unmonitoredWantedFilter())
	if !ok {
		// The completeness contract, in the direction where an incomplete set
		// is a different kind of wrong: absence from this set means "its
		// quality cutoff is met", so a short set turns healthy movies into
		// findings — and, with the remonitor flag on, into writes. The forward
		// pass above never consulted it and is unaffected.
		p.logger.Warn("skipping the reverse scan for this instance: the unmonitored wanted/cutoff set could not be fetched completely, and an incomplete set would report movies as below cutoff that are not; the forward pass is unaffected",
			"instance", p.inst.Name, "type", p.inst.Type)
		c.skipped = true
		return c
	}

	var findings []movieDecision
	for _, m := range movies {
		// The same shutdown boundary the forward evaluation draws, and the same
		// ending: a partial reverse scan is not a reverse scan, so nothing it
		// half-saw is reported or counted as a finding total.
		if ctx.Err() != nil {
			p.logger.Info("shutdown requested: abandoning this instance's reverse scan mid-evaluation; a partial reverse scan is never reported as a finding count",
				"instance", p.inst.Name, "type", p.inst.Type, "evaluated", c.findings+c.noFile)
			c.skipped = true
			return c
		}

		// monitored absent is untrusted input, not a state — the forward loop
		// has already warned about this exact movie via warnIfFieldAbsent, so
		// this excludes it without saying so twice.
		if m.Monitored == nil || *m.Monitored {
			continue
		}
		if m.ID == nil {
			// The forward loop never reaches its own missing-id guard for an
			// unmonitored movie (rule 1 excludes it first), so this is the only
			// place an unmonitored movie with no id is ever noticed.
			p.logger.Warn("reverse scan: skipping movie: missing id field",
				"instance", p.inst.Name, "type", p.inst.Type, "title", titleOrAbsent(m.Title))
			continue
		}

		d := evaluateMovie(ctx, p.logger, p.client, p.inst, m, p.profiles, p.exclusionTagID, p.tagActive, wantedIDs)
		if d.reason == ReasonNoFile {
			c.noFile++
			continue
		}
		if !isReverseFinding(d.reason) {
			continue
		}
		c.findings++
		findings = append(findings, d)
		p.logger.Log(ctx, p.itemLevel, "reverse-scan finding",
			"id", d.id, "title", d.title, "reason", d.reason, "profile", d.profileName, "instance", p.inst.Name)
	}

	if c.noFile > 0 {
		p.logger.Debug("reverse scan: unmonitored movies with no file are not findings; leaving a movie unmonitored and undownloaded is a deliberate choice",
			"instance", p.inst.Name, "type", p.inst.Type, "reverseNoFile", c.noFile)
	}

	// THE REPORT-ONLY DEFAULT, structurally. With the flag off there is no
	// write pass to gate, no fresh pre-write fetch, no payload assembled and no
	// dry-run branch to get wrong — the reverse scan simply ends here, and the
	// only evidence it ran at all is what it reported.
	if p.opts.remonitor {
		p.remonitorMovies(ctx, findings, reverseWriteContext{profiles: p.profiles, wantedIDs: wantedIDs}, &c)
	}
	return c
}

// reverseSeasonFinding pairs a reported season with the one fact about it the
// write side needs and the decision itself does not carry: whether its SERIES
// is monitored.
//
// That fact decides everything about what may happen next (binding controller
// resolution 3). A season under a monitored series may be re-monitored; a
// season under an UNMONITORED series may only ever be reported, because a
// series-level unmonitor is a human deliberately retiring a show and
// re-monitoring its seasons would be this program arguing with that decision.
// Series-level monitored is never written in either direction, which has been a
// structural rule of the Sonarr write path since Phase 7.
type reverseSeasonFinding struct {
	decision        seasonDecision
	seriesMonitored bool
}

// runSonarr is the Sonarr reverse scan: every UNMONITORED season, of a
// monitored OR unmonitored series, evaluated by evaluateSeries — the forward
// decision function in its reverse direction — against the unmonitored wanted
// set.
//
// The pool includes seasons of unmonitored series deliberately (binding
// controller resolution 3): the plan asks for both, and "this retired show has
// a season that never finished upgrading" is exactly the kind of thing a human
// wants told to them once. What differs is only what may be DONE about it.
//
// Cost: evaluateSeries returns before its /episode fetch for any series with no
// unmonitored season at all, so a fully-monitored library pays nothing here
// beyond the series-level checks. A series with seasons in BOTH states is
// evaluated twice per cycle — once per direction — which is the price of asking
// two different questions about it with one implementation.
func (p reversePass) runSonarr(ctx context.Context, series []seriesElement) reverseCounts {
	var c reverseCounts

	_, wantedSeasons, ok := fetchSonarrWantedCutoff(ctx, p.cycleLogger, p.client, p.inst, unmonitoredWantedFilter())
	if !ok {
		p.logger.Warn("skipping the reverse scan for this instance: the unmonitored wanted/cutoff set could not be fetched completely, and an incomplete set would report seasons as below cutoff that are not; the forward pass is unaffected",
			"instance", p.inst.Name, "type", p.inst.Type)
		c.skipped = true
		return c
	}

	var findings []reverseSeasonFinding
	for _, s := range series {
		if ctx.Err() != nil {
			p.logger.Info("shutdown requested: abandoning this instance's reverse scan mid-evaluation; a partial reverse scan is never reported as a finding count",
				"instance", p.inst.Name, "type", p.inst.Type, "findings", c.findings)
			c.skipped = true
			return c
		}

		// monitored absent is untrusted input, not a state, and it is the one
		// thing the write side keys on — so a series whose own monitored flag
		// could not be read produces no finding in either direction. The forward
		// loop has already warned about this exact series.
		if s.Monitored == nil {
			continue
		}
		if s.ID == nil {
			// The forward loop reaches its own missing-id guard only for a
			// MONITORED series, so an unmonitored one with no id is noticed here
			// or nowhere.
			if !*s.Monitored {
				p.logger.Warn("reverse scan: skipping series: missing id field",
					"instance", p.inst.Name, "type", p.inst.Type, "title", titleOrAbsent(s.Title))
			}
			continue
		}

		eval := evaluateSeries(ctx, p.logger, p.client, p.inst, s, p.profiles, p.exclusionTagID, p.tagActive, wantedSeasons, directionReverse)
		for _, d := range eval.decisions {
			if !isReverseFinding(d.reason) {
				continue
			}
			c.findings++
			findings = append(findings, reverseSeasonFinding{decision: d, seriesMonitored: *s.Monitored})
			p.logger.Log(ctx, p.itemLevel, "reverse-scan finding",
				"instance", p.inst.Name, "seriesId", d.seriesID, "series", d.series, "season", d.season,
				"reason", d.reason, "profile", d.profileName, "seriesMonitored", *s.Monitored)
		}
	}

	// The report-only default, structurally: with the flag off there is no write
	// pass to gate — see the Radarr twin.
	if p.opts.remonitor {
		p.remonitorSeasons(ctx, findings, reverseWriteContext{profiles: p.profiles, wantedSeasons: wantedSeasons}, &c)
	}
	return c
}

// --- the write direction ----------------------------------------------------

// reverseWriteContext is everything the reverse direction's pre-write
// re-verification needs to re-run the decision function against FRESH data:
// this instance's quality profiles, and the unmonitored wanted set this cycle
// already paged.
//
// The wanted set is the one input that is not re-fetched per item, and that is
// deliberate: it is minutes old at most (the same cycle fetched it), re-paging
// it per candidate would turn one fetch into one-per-write, and the forward
// write path makes exactly the same trade with its own wanted set. Everything
// else the re-evaluation reads — the movie or series object, its file scores —
// comes from the write path's own fresh GETs.
type reverseWriteContext struct {
	profiles      map[int]qualityProfile
	wantedIDs     map[int]bool       // Radarr: the unmonitored wanted set
	wantedSeasons map[seasonKey]bool // Sonarr: the same set, keyed by season
}

// verifyMovieStillAReverseFinding re-runs evaluateMovie against the body of the
// write path's own fresh pre-write GET, and refuses the write unless the movie
// STILL fails the criteria.
//
// It re-runs the decision function rather than re-checking a remembered reason
// for the same reason the reverse scan reuses that function in the first place:
// "still a finding" and "is a finding" must be the same question, asked of the
// same code. A movie upgraded between the scan and this moment now meets the
// criteria, and re-monitoring it would undo this project's own correct work —
// after which the next forward cycle would unmonitor it again, and the two
// directions would flap it against each other forever.
//
// The decode is a plain one. A corrupted "tags" array cannot change the outcome
// here: when the exclusion tag is active, preWriteExclusionTagCheck has already
// run against these exact bytes and refused every untrusted tags shape there is
// (absent, JSON null, non-array, a null element, ambiguous casing), and when it
// is not active, rule 4 is vacuous whatever tags say.
//
// WHAT THIS CAN AND CANNOT SEE, stated because the boundary is a real one. Rules
// 2, 3, 4 and 6 read the fresh object and — for the custom-format score — a
// fresh /moviefile, so a file removed, a profile changed, upgrades switched off,
// a tag added or a score raised since the scan are all caught here. Rule 5 reads
// the unmonitored wanted set this cycle already paged (reverseWriteContext),
// which is minutes old at most but is not re-fetched per item, so a movie whose
// QUALITY cutoff was met in that window still reads as a finding. The
// consequence is bounded and self-correcting — the movie is re-monitored once,
// meets the criteria, and the next forward cycle unmonitors it again — and the
// alternative is re-paging an entire wanted set per write candidate, a cost the
// forward path does not pay for its own equally-fresh set either.
func verifyMovieStillAReverseFinding(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance, body []byte, movieID, exclusionTagID int, tagActive bool, rev reverseWriteContext, attrs []any) error {
	var fresh movieListElement
	if err := json.Unmarshal(body, &fresh); err != nil {
		logger.Warn("the pre-write fetch could not be read as a movie, so whether it still fails the criteria cannot be determined; refusing to re-monitor",
			append(append([]any(nil), attrs...), "error", err)...)
		return fmt.Errorf("movie %d: %w: the pre-write fetch could not be decoded: %v", movieID, errNoLongerAReverseFinding, err)
	}
	if fresh.ID == nil || *fresh.ID != movieID {
		// verifyMovieIdentity has already established this against the same
		// bytes; reaching here would mean the two decodes disagree about the
		// object, which is not something to write through.
		logger.Warn("the pre-write fetch does not identify the movie being re-monitored; refusing to write", attrs...)
		return fmt.Errorf("movie %d: %w: the pre-write fetch identifies a different movie", movieID, errNoLongerAReverseFinding)
	}

	d := evaluateMovie(ctx, logger, client, inst, fresh, rev.profiles, exclusionTagID, tagActive, rev.wantedIDs)
	if !isReverseFinding(d.reason) {
		logger.Info("the movie no longer fails the criteria as of the pre-write fetch, skipping the re-monitor",
			append(append([]any(nil), attrs...), "reason", d.reason)...)
		return fmt.Errorf("movie %d: %w: it now evaluates as %q", movieID, errNoLongerAReverseFinding, d.reason)
	}
	return nil
}

// reverseWriteGateBlockReason decides whether this cycle's forward cross-check
// authorizes the reverse pass's writes, returning "" to open the gate or the
// reason it stays shut (binding controller resolution 6).
//
// The reverse pass has no cross-check of its own: a cross-check compares two
// independently computed signals, and the reverse direction has only one. What
// it uses the forward verdict for is therefore narrower than what the forward
// gate uses it for, and the difference is worth stating. The forward gate asks
// "was anything verified about the decisions I am about to act on" — a question
// about evidence for specific writes, answered by the would-unmonitor pool's own
// counts. This gate asks "did this instance's data agree with itself this
// cycle", which is a question about the sample as a whole, so it reads the
// aggregate counts: a reverse write acts on no forward decision, and the
// would-unmonitor pool is routinely empty on exactly the cycles this pass has
// work to do.
//
// FAILED, inconclusive, and any status a future change might add all block the
// pass, with the withheld accounting saying so.
//
// REVIEW FIX (Phase 10 round 2): a bare `status == passed` used to open the
// gate, which re-introduced verbatim the rule this project already rejected once
// for the forward direction (writeGateBlockReason, hardened in the phase-4
// round-2 review: "far weaker than it reads"). "passed" is awarded whenever
// nothing DISAGREED, which includes two states where nothing was checked:
//
//   - Nothing sampled at all. Both cross-checks fall through to `default:
//     passed` when their sample pool is empty — no would-unmonitor decision and
//     no sample-eligible skip existed — so verified and unverifiable are both 0
//     and not one comparison was made. renderCrossCheckSummary carves this case
//     out with its own wording precisely so it "can never be mistaken for 'a
//     sample was taken and it checked out clean'"; a gate that treats it as one
//     is the same mistake in executable form. It is not a corner case in this
//     direction: a library whose only unmonitored items are the findings
//     produces exactly this, so it was the ONLY way the reverse gate had ever
//     opened in the test suite.
//   - A sample that verified almost nothing. One verified item out of twenty
//     still passes, and one movie is not evidence that an instance's data agrees
//     with itself. The forward gate blocks this shape; so does this one, on the
//     aggregate counts that match its own question.
//
// There is no recovery-equivalent here, and none is needed: a half-done
// re-monitor converges by itself, because a season or movie that is still
// unmonitored is still a finding next cycle. That is also why blocking costs so
// little: a withheld reverse write is retried, in full, by the next cycle whose
// data does check out.
func reverseWriteGateBlockReason(cc crossCheckResult) string {
	switch cc.status {
	case crossCheckStatusPassed:
		// The evidence conditions below apply only to this status; every other
		// answer is already a block.
	case crossCheckStatusFailed:
		return "the cross-check found a disagreement, so this instance's data cannot be trusted in either direction"
	case crossCheckStatusInconclusive:
		return "the cross-check verified nothing this cycle, so nothing establishes that this instance's data is sound"
	default:
		return "the cross-check status is unrecognized, which blocks every write"
	}
	if cc.verified == 0 && cc.unverifiable == 0 {
		return "the cross-check sampled nothing this cycle, so no health signal exists to authorize a write"
	}
	if cc.unverifiable > cc.verified {
		return "most of what the cross-check sampled could not be verified, so this instance's data is only partly established as sound"
	}
	return ""
}

// record folds one attempted re-monitor into the counters, by the same five
// outcomes the forward passes use and in the same order of precedence. Sharing
// one classifier between the movie and season paths is what keeps the two from
// counting the same ending differently.
//
// attrs names the item; the caller owns that vocabulary, since a movie line and
// a season line identify their subject differently.
func (c *reverseCounts) record(logger *slog.Logger, attrs []any, written bool, err error, dryRun bool) {
	withAttrs := func(extra ...any) []any {
		return append(append([]any(nil), attrs...), extra...)
	}
	switch {
	case isWriteRefusal(err):
		// Every refusal logged its own specific reason at the moment it
		// refused — the exclusion tag reappeared, the item is already
		// monitored, it no longer fails the criteria, a field could not be read
		// — so nothing more is logged here and no cause is lost by sharing a
		// counter. No HTTP write was sent, so none of them is a failed write.
		c.refused++
	case errors.Is(err, errWriteUnverified):
		c.echoUnverified++
		logger.Warn("remonitor write accepted but the response was unverifiable; treat it as applied and let the next cycle reconcile it",
			withAttrs("error", err)...)
	case err != nil:
		c.writeErrors++
		msg := "remonitor write failed; skipping this item for the cycle"
		if dryRun {
			msg = "remonitor write rehearsal failed; no write was attempted (dry-run), and this item is skipped for the cycle"
		}
		logger.Error(msg, withAttrs("error", err)...)
	case !written:
		// Dry-run: the write was withheld at the §2.1 gate immediately before
		// the PUT, and has already been logged at debug. The ONLY remaining
		// (false, nil) case, exactly as on the forward paths.
		c.withheld++
	default:
		c.remonitored++
		logger.Info("remonitor", attrs...)
	}
}

// remonitorMovies is the reverse pass's write half for Radarr. It runs only
// with reverse_scan_remonitor set: with the flag off, the caller never reaches
// this function, so the reverse scan composes no write call at all rather than
// composing one and gating it.
func (p reversePass) remonitorMovies(ctx context.Context, findings []movieDecision, rev reverseWriteContext, c *reverseCounts) {
	if reason := reverseWriteGateBlockReason(p.cc); reason != "" {
		c.withheld += len(findings)
		if len(findings) > 0 {
			// One line per instance per cycle, not per item: "nothing was
			// written" and "nothing needed writing" must never look the same,
			// and a blocked pass that left reverseFindings=N against a line of
			// zeroes is exactly the unexplained gap the identity forbids.
			attrs := []any{"instance", p.inst.Name, "type", p.inst.Type, "crossCheck", p.cc.status}
			attrs = append(attrs, p.cc.logAttrs()...)
			attrs = append(attrs, "reverseWithheld", len(findings), "dryRun", p.dryRun)
			p.logger.Warn("reverse-scan writes withheld for this instance: "+reason, attrs...)
		}
		return
	}

	shutdownNoted := false
	for _, d := range findings {
		// The shutdown boundary, between items and nowhere else — the write
		// itself detaches from this cancellation so an item already under way
		// finishes.
		if ctx.Err() != nil {
			c.withheld++
			if !shutdownNoted {
				shutdownNoted = true
				p.logger.Info("shutdown requested: the remaining reverse-scan writes for this instance are withheld and the next cycle will revisit them",
					"instance", p.inst.Name, "type", p.inst.Type, "dryRun", p.dryRun)
			}
			continue
		}

		written, err := remonitorMovie(ctx, p.logger, p.client, p.inst, d.id, p.exclusionTagID, p.tagActive, p.dryRun, rev)
		c.record(p.logger, []any{"id", d.id, "title", d.title, "reason", d.reason, "profile", d.profileName, "instance", p.inst.Name}, written, err, p.dryRun)
	}
}

// verifySeasonStillAReverseFinding is the season path's half of the same
// re-verification: evaluateSeries — the decision function itself, in its reverse
// direction — re-run against the write path's own fresh series payload, with the
// write refused unless the target season STILL fails the criteria.
//
// It costs a second /episode fetch and one /episodefile fetch for this series,
// because evaluateSeries owns its own reads and reusing it whole is the point.
// That is a per-candidate cost on a pass whose candidates are, by construction,
// the rare accidental unmonitors.
//
// The same freshness boundary as the movie path applies (see
// verifyMovieStillAReverseFinding): everything but rule 4's wanted set is read
// again here.
func verifySeasonStillAReverseFinding(ctx context.Context, logger *slog.Logger, client *APIClient, inst Instance, body []byte, seriesID, seasonNumber, exclusionTagID int, tagActive bool, rev reverseWriteContext, subject string, attrs []any) error {
	var fresh seriesElement
	if err := json.Unmarshal(body, &fresh); err != nil {
		logger.Warn("the pre-write fetch could not be read as a series, so whether the season still fails the criteria cannot be determined; refusing to re-monitor",
			append(append([]any(nil), attrs...), "error", err)...)
		return fmt.Errorf("%s: %w: the pre-write fetch could not be decoded: %v", subject, errNoLongerAReverseFinding, err)
	}
	if fresh.ID == nil || *fresh.ID != seriesID {
		logger.Warn("the pre-write fetch does not identify the series being re-monitored; refusing to write", attrs...)
		return fmt.Errorf("%s: %w: the pre-write fetch identifies a different series", subject, errNoLongerAReverseFinding)
	}

	eval := evaluateSeries(ctx, logger, client, inst, fresh, rev.profiles, exclusionTagID, tagActive, rev.wantedSeasons, directionReverse)
	for _, d := range eval.decisions {
		if d.season != seasonNumber {
			continue
		}
		if isReverseFinding(d.reason) {
			return nil
		}
		logger.Info("the season no longer fails the criteria as of the pre-write fetch, skipping the re-monitor",
			append(append([]any(nil), attrs...), "reason", d.reason)...)
		return fmt.Errorf("%s: %w: it now evaluates as %q", subject, errNoLongerAReverseFinding, d.reason)
	}
	// No decision at all for the target season: it is no longer an unmonitored
	// season of this series, or it is no longer readable. Either way the finding
	// this write rests on cannot be re-established, and §2.6 says never guess.
	logger.Warn("the pre-write fetch produced no reverse decision for this season, so the finding could not be re-established; refusing to write", attrs...)
	return fmt.Errorf("%s: %w: the pre-write fetch produced no decision for this season", subject, errNoLongerAReverseFinding)
}

// remonitorSeasons is the reverse pass's write half for Sonarr. Like its Radarr
// twin it is reached only with reverse_scan_remonitor set, so report-only
// composes no write call at all.
func (p reversePass) remonitorSeasons(ctx context.Context, findings []reverseSeasonFinding, rev reverseWriteContext, c *reverseCounts) {
	if reason := reverseWriteGateBlockReason(p.cc); reason != "" {
		c.withheld += len(findings)
		if len(findings) > 0 {
			attrs := []any{"instance", p.inst.Name, "type", p.inst.Type, "crossCheck", p.cc.status}
			attrs = append(attrs, p.cc.logAttrs()...)
			attrs = append(attrs, "reverseWithheld", len(findings), "dryRun", p.dryRun)
			p.logger.Warn("reverse-scan writes withheld for this instance: "+reason, attrs...)
		}
		return
	}

	shutdownNoted := false
	for _, f := range findings {
		d := f.decision
		attrs := []any{"instance", p.inst.Name, "seriesId", d.seriesID, "series", d.series, "season", d.season,
			"reason", d.reason, "profile", d.profileName}

		// Binding controller resolution 3: a season under an UNMONITORED series
		// is report-only, always. Re-monitoring it would fight a human's
		// deliberate retirement of the show, and this is where that is decided
		// — before any fetch, so nothing is even asked about a series this pass
		// has no business writing to. The write path enforces it a second time
		// from the other side (its series-monitored guard), which is where a
		// series retired between the scan and the write is caught.
		if !f.seriesMonitored {
			c.withheld++
			p.logger.Log(ctx, p.itemLevel, "reverse-scan finding withheld: its series is not monitored, so re-monitoring this season would fight a deliberate retirement of the whole show; reporting it is all this cycle will do",
				append(append([]any(nil), attrs...), "seriesMonitored", false)...)
			continue
		}

		if ctx.Err() != nil {
			c.withheld++
			if !shutdownNoted {
				shutdownNoted = true
				p.logger.Info("shutdown requested: the remaining reverse-scan writes for this instance are withheld and the next cycle will revisit them",
					"instance", p.inst.Name, "type", p.inst.Type, "dryRun", p.dryRun)
			}
			continue
		}

		written, err := remonitorSeason(ctx, p.logger, p.client, p.inst, d.seriesID, d.season, p.exclusionTagID, p.tagActive, p.dryRun, rev)
		c.record(p.logger, attrs, written, err, p.dryRun)
	}
}
