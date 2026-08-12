package main

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

// String makes a direction printable in a log attribute without a switch at
// the call site.
func (d scanDirection) String() string {
	if d == directionReverse {
		return "reverse"
	}
	return "forward"
}

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
