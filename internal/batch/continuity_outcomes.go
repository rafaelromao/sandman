package batch

// Continuity outcome table (issue #2996, ADR-0063). One table decides the
// AFK-continuity outcome of every lifecycle exit: the Run keeps waiting on an
// external operation that is still resolving, holds without an operation
// deadline, or ends. Decision sites derive the cause and the evidence, look
// the outcome up here, and render it with their own payloads. The
// AFK-continuity contract enumerates every row.

// continuityCause is why a lifecycle exit needs a continuity outcome.
type continuityCause string

const (
	// causeResolving: an external operation is still resolving the gate.
	causeResolving continuityCause = "resolving"
	// causeFailure: the lifecycle selected a failure reason for a pull
	// request that is open at its current head.
	causeFailure continuityCause = "failure-reason"
	// causeStall: a relaunch would act on an unchanged resume signature,
	// either one the Run's stalled signatures already hold or a repeat on a
	// gate that stalls on repeat.
	causeStall continuityCause = "stall"
	// causeExhausted: the lifecycle relaunch allowance is used up. For owned
	// work, the final ordinary attempt.
	causeExhausted continuityCause = "allowance-used"
	// causeExpired: a bounded operation passed its deadline with nothing
	// else resolving it.
	causeExpired continuityCause = "expired-operation"
	// causeUnreadable: the Run's stalled signatures cannot be read, so a
	// relaunch cannot be proven to act on new evidence.
	causeUnreadable continuityCause = "unreadable-ledger"
)

// continuityOutcomeKind is what a lifecycle exit does to the Run.
type continuityOutcomeKind string

const (
	// outcomeWait keeps waiting on the resolving operation, under its own
	// deadline.
	outcomeWait continuityOutcomeKind = "wait"
	// outcomeHold enters a continuity hold: a wait without an operation
	// deadline, recovered under hold:<RunID>.
	outcomeHold continuityOutcomeKind = "hold"
	// outcomeTerminal ends the Run with a classified permanent reason.
	outcomeTerminal continuityOutcomeKind = "terminal"
)

// Gates of the external operations whose waits the table pins.
const (
	continuityGateCurrentHeadCI = "current-head-ci"
	continuityGateReviewRequest = "review-request"
	continuityGateQuota         = "usage-limit"
	continuityGateCapacity      = "capacity"
)

// continuityRow keys one outcome by gate, reason and cause. An empty gate or
// reason matches any.
type continuityRow struct {
	gate   string
	reason string
	cause  continuityCause
}

// continuityTable maps rows to outcomes.
type continuityTable map[continuityRow]continuityOutcomeKind

// outcome looks a row up from the most specific key to the least: gate and
// reason, reason alone, gate alone, then the cause's default. A reason row
// outranks a gate row because permanent reasons stay permanent on any gate.
// A row the table does not know is terminal.
func (t continuityTable) outcome(row continuityRow) continuityOutcomeKind {
	for _, key := range []continuityRow{
		row,
		{reason: row.reason, cause: row.cause},
		{gate: row.gate, cause: row.cause},
		{cause: row.cause},
	} {
		if outcome, ok := t[key]; ok {
			return outcome
		}
	}
	return outcomeTerminal
}

// continuityOutcomes is the outcome table.
var continuityOutcomes = continuityTable{
	// External operations that are still resolving keep waiting. Their owners
	// retain scheduling and evidence, while each decision site consumes this
	// disposition: current-head CI, a confirmed in-deadline review request,
	// the GitHub recovery window, quota admission and the batch scheduler.
	{gate: continuityGateCurrentHeadCI, cause: causeResolving}:                            outcomeWait,
	{gate: continuityGateReviewRequest, cause: causeResolving}:                            outcomeWait,
	{gate: gateGitHubUnavailable, reason: githubUnavailableReason, cause: causeResolving}: outcomeWait,
	{gate: continuityGateQuota, reason: "AGENT_USAGE_LIMIT", cause: causeResolving}:       outcomeWait,
	{gate: continuityGateCapacity, cause: causeResolving}:                                 outcomeWait,
	{gate: gateCIRecovery, reason: ciRecoveryReason, cause: causeResolving}:               outcomeWait, // CI recovery window (#3011)

	// Lifecycle failure reasons. A permanent reason may end a Run whose pull
	// request is open; a recoverable one holds instead. Unknown reasons are
	// permanent: a new reason must be classified here, and the continuity
	// contract enforces that.
	{reason: "PULL_REQUEST_CLOSED", cause: causeFailure}:                 outcomeTerminal, // closed without merge
	{reason: missingPRReason, cause: causeFailure}:                       outcomeTerminal, // the implementor never published
	{reason: "REVIEW_REQUEST_REQUIRED", cause: causeFailure}:             outcomeTerminal, // no review request was delivered
	{reason: actionableFeedbackReason, cause: causeFailure}:              outcomeTerminal, // formal requested changes remain blocking
	{reason: blindReviewRequiredReason, cause: causeFailure}:             outcomeHold,     // owned blind review; exhaustion holds
	{reason: "merged-pr-missing-closing-reference", cause: causeFailure}: outcomeTerminal, // merged without verifiable completion
	{reason: "CI_WAIT_STATE_ERROR", cause: causeFailure}:                 outcomeTerminal, // corrupt CI wait state
	{reason: stateErrorReason, cause: causeFailure}:                      outcomeTerminal, // corrupt review state, nothing resolving
	{reason: lookupGateReason, cause: causeFailure}:                      outcomeTerminal, // GitHub stayed unavailable for 24 hours
	{reason: githubUnavailableReason, cause: causeFailure}:               outcomeHold,     // waits for GitHub to recover
	{reason: ciRecoveryReason, cause: causeFailure}:                      outcomeHold,     // waits for the runtime to rerun a transient CI failure
	{reason: ciRecoveryExhaustedReason, cause: causeFailure}:             outcomeTerminal, // the CI recovery window passed 24 hours
	{reason: implementorActionRequiredReason, cause: causeFailure}:       outcomeTerminal, // legacy verification fallback
	{reason: "REMEDIATION_BUDGET_EXHAUSTED", cause: causeFailure}:        outcomeTerminal, // genuine requested changes left unrepaired (formal, daemon verdict or approval override)
	{reason: reviewFeedbackStalledReason, cause: causeFailure}:           outcomeTerminal, // informal review feedback an unchanged relaunch could not act on
	{reason: idleGateReason, cause: causeFailure}:                        outcomeHold,     // nothing resolving yet; waits like rc2
	{reason: reviewTimeoutReason, cause: causeFailure}:                   outcomeHold,     // expired request; agrees with the expired row
	{reason: "CI_WAIT_TIMEOUT", cause: causeFailure}:                     outcomeHold,     // bounded CI wait expired; waits like rc2
	{reason: "CAPACITY_WAIT", cause: causeFailure}:                       outcomeTerminal, // fail closed if capacity admission is ever terminalized
	{cause: causeFailure}: outcomeTerminal, // unknown reasons are permanent

	// Bounded operations that expired with nothing else resolving them.
	{gate: gateReviewTimeout, reason: reviewTimeoutReason, cause: causeExpired}:         outcomeHold,     // expired delegated-review request
	{gate: gateGitHubUnavailable, reason: githubUnavailableReason, cause: causeExpired}: outcomeTerminal, // 24-hour window: GATE_LOOKUP_FAILED
	{gate: gateCIRecovery, reason: ciRecoveryReason, cause: causeExpired}:               outcomeTerminal, // 24-hour window: CI_RECOVERY_EXHAUSTED

	// Stalls: a relaunch that would act on an unchanged resume signature.
	{gate: gateActionableFeedback, reason: informalFeedbackReason, cause: causeStall}:     outcomeTerminal, // REVIEW_FEEDBACK_STALLED (#2999)
	{gate: gateActionableFeedback, reason: actionableFeedbackReason, cause: causeStall}:   outcomeHold,
	{gate: gateCIWaitTimeout, reason: "CI_WAIT_STATE_ERROR", cause: causeStall}:           outcomeHold,
	{gate: gateBlindReviewRequired, reason: blindReviewRequiredReason, cause: causeStall}: outcomeHold,
	{gate: gateReadyToMerge, cause: causeStall}:                                           outcomeHold,
	{gate: "ci-failure", reason: "CI_FAILURE", cause: causeStall}:                         outcomeHold,
	{gate: "merge-conflict", reason: "MERGE_CONFLICT", cause: causeStall}:                 outcomeHold,
	{gate: gateCIWaitTimeout, reason: "CI_WAIT_TIMEOUT", cause: causeStall}:               outcomeHold,
	{gate: gateReviewTimeout, reason: reviewTimeoutReason, cause: causeStall}:             outcomeHold,
	{gate: gatePRHeadChanged, reason: "PR_HEAD_RECONCILE_REQUIRED", cause: causeStall}:    outcomeHold,
	{gate: gateOwnedWorkRequired, reason: idleGateReason, cause: causeStall}:              outcomeHold,
	{gate: gateOwnedWorkRequired, reason: "REVIEW_REQUEST_REQUIRED", cause: causeStall}:   outcomeHold,
	{gate: gateOwnedWorkRequired, reason: missingPRReason, cause: causeStall}:             outcomeHold,
	{cause: causeStall}: outcomeHold,

	// A used-up relaunch allowance. Genuine requested changes (formal
	// CHANGES_REQUESTED, the review daemon's changes_requested verdict, or a
	// daemon approval its own verdict sections contradict) and unusable CI wait
	// state end the Run on any gate. Owned work ends
	// through its final ordinary attempt, except the idle gate whose only
	// missing evidence is a review nobody requested.
	{reason: actionableFeedbackReason, cause: causeExhausted}:                                     outcomeTerminal,
	{reason: "CI_WAIT_STATE_ERROR", cause: causeExhausted}:                                        outcomeTerminal,
	{gate: gateOwnedWorkRequired, reason: actionableFeedbackReason, cause: causeExhausted}:        outcomeTerminal,
	{gate: gateActionableFeedback, reason: informalFeedbackReason, cause: causeExhausted}:         outcomeHold,
	{gate: gateReadyToMerge, cause: causeExhausted}:                                               outcomeHold,
	{gate: "ci-failure", reason: "CI_FAILURE", cause: causeExhausted}:                             outcomeHold,
	{gate: "merge-conflict", reason: "MERGE_CONFLICT", cause: causeExhausted}:                     outcomeHold,
	{gate: gateBlindReviewRequired, reason: blindReviewRequiredReason, cause: causeExhausted}:     outcomeHold,
	{gate: gateCIWaitTimeout, reason: "CI_WAIT_TIMEOUT", cause: causeExhausted}:                   outcomeHold,
	{gate: gateReviewTimeout, reason: reviewTimeoutReason, cause: causeExhausted}:                 outcomeHold,
	{gate: gatePRHeadChanged, reason: "PR_HEAD_RECONCILE_REQUIRED", cause: causeExhausted}:        outcomeHold,
	{gate: gateOwnedWorkRequired, reason: idleGateReason, cause: causeExhausted}:                  outcomeHold,
	{gate: gateOwnedWorkRequired, reason: implementorActionRequiredReason, cause: causeExhausted}: outcomeTerminal,
	{gate: gateOwnedWorkRequired, reason: "REVIEW_REQUEST_REQUIRED", cause: causeExhausted}:       outcomeTerminal,
	{gate: gateOwnedWorkRequired, reason: missingPRReason, cause: causeExhausted}:                 outcomeTerminal,
	{gate: gateOwnedWorkRequired, cause: causeExhausted}:                                          outcomeTerminal,
	{cause: causeExhausted}: outcomeHold,

	// Without the Run's stalled signatures a relaunch could repeat stalled
	// evidence: fail closed into a hold.
	{cause: causeUnreadable}: outcomeHold,
}

func quotaLifecycleDecision(table continuityTable) lifecycleVerdict {
	if table == nil {
		table = continuityOutcomes
	}
	row := continuityRow{
		gate:   continuityGateQuota,
		reason: "AGENT_USAGE_LIMIT",
		cause:  causeResolving,
	}
	wait := lifecycleVerdict{
		Action: lifecycleAwait,
		Evidence: map[string]any{
			"gate":        row.gate,
			"reason":      row.reason,
			"await":       true,
			"next_action": "resume after provider usage limit resets",
		},
	}
	terminal := lifecycleVerdict{
		Action: lifecycleFailure,
		Evidence: map[string]any{
			"reason":        row.reason,
			"external_gate": row.gate,
			"next_action":   "retry with an agent provider that has available capacity or resume after its usage limit resets",
		},
	}
	return (&lifecycleObserver{outcomes: table}).applyContinuityDisposition(row, wait, terminal)
}

// failureReasons is the failure-reason classification view of the table:
// reason -> permanent.
func (t continuityTable) failureReasons() map[string]bool {
	reasons := map[string]bool{}
	for row, outcome := range t {
		if row.cause == causeFailure && row.reason != "" {
			reasons[row.reason] = outcome == outcomeTerminal
		}
	}
	return reasons
}
