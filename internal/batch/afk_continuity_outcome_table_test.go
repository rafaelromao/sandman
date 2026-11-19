package batch

// AFK-continuity outcome table contract (issue #2996).
//
// Every continuity outcome of a lifecycle exit is one row of the outcome
// table: the Run keeps waiting on an operation that is still resolving, holds
// without an operation deadline, or ends. This contract lists every row with
// its current outcome. A change that adds, removes or flips a row must update
// this list on purpose; any other change fails here.

import (
	"fmt"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
)

// continuityContractRow is one row of the outcome table with the outcome the
// AFK-continuity contract currently requires.
type continuityContractRow struct {
	row     continuityRow
	outcome continuityOutcomeKind
	probe   continuityProbe
}

// continuityProbe observes one row's outcome through the code that decides
// it. Rows of operations whose owners decide them outside the table also name
// the scenario that pins them end to end.
type continuityProbe struct {
	observe func(t *testing.T, table continuityTable) continuityOutcomeKind
	// consultsTable reports that the decision site looks the row up, so
	// flipping the row flips the observed outcome.
	consultsTable bool
	pinnedBy      func(*testing.T)
}

// continuityContractRows enumerates the outcome table, each row with the
// probe that observes it. Sibling tickets of #2994 flip their own rows here
// (see ADR-0063).
func continuityContractRows() []continuityContractRow {
	owned := gateOwnedWorkRequired
	relaunch := func(gate, reason string) map[string]any {
		return map[string]any{"gate": gate, "reason": reason, "pull_request": continuityPR, "head_sha": continuityProbeHead}
	}
	ownedWork := func(gate, reason, reviewDecision string) map[string]any {
		evidence := relaunch(gate, reason)
		evidence["expected_head_sha"] = continuityProbeHead
		evidence["review_decision"] = reviewDecision
		return evidence
	}
	// A relaunch keyed by no listed gate or reason.
	other := relaunch("custom-gate", "CUSTOM_REASON")
	return []continuityContractRow{
		// External operations that are still resolving keep waiting.
		{continuityRow{gate: continuityGateCurrentHeadCI, cause: causeResolving}, outcomeWait, activeOperationProbe(true, false)},
		{continuityRow{gate: continuityGateReviewRequest, cause: causeResolving}, outcomeWait, activeOperationProbe(false, true)},
		{continuityRow{gate: gateGitHubUnavailable, reason: githubUnavailableReason, cause: causeResolving}, outcomeWait, githubWindowProbe(false)},
		{continuityRow{gate: continuityGateQuota, reason: "AGENT_USAGE_LIMIT", cause: causeResolving}, outcomeWait, foldedWaitProbe("usage-limit", TestUsageLimitDuringPRRemediationRetainsQuotaRecovery)},
		{continuityRow{gate: continuityGateCapacity, cause: causeResolving}, outcomeWait, foldedWaitProbe("capacity", TestFindReadyContinuationsUsesLatestDurableCapacityPhase)},

		// Lifecycle failure reasons on an open current-head pull request.
		{continuityRow{reason: "PULL_REQUEST_CLOSED", cause: causeFailure}, outcomeTerminal, failureReasonProbe("PULL_REQUEST_CLOSED")},
		{continuityRow{reason: missingPRReason, cause: causeFailure}, outcomeTerminal, failureReasonProbe(missingPRReason)},
		{continuityRow{reason: "REVIEW_REQUEST_REQUIRED", cause: causeFailure}, outcomeTerminal, failureReasonProbe("REVIEW_REQUEST_REQUIRED")},
		{continuityRow{reason: blindReviewRequiredReason, cause: causeFailure}, outcomeHold, failureReasonProbe(blindReviewRequiredReason)},
		{continuityRow{reason: "merged-pr-missing-closing-reference", cause: causeFailure}, outcomeTerminal, failureReasonProbe("merged-pr-missing-closing-reference")},
		{continuityRow{reason: "CI_WAIT_STATE_ERROR", cause: causeFailure}, outcomeTerminal, failureReasonProbe("CI_WAIT_STATE_ERROR")},
		{continuityRow{reason: stateErrorReason, cause: causeFailure}, outcomeTerminal, failureReasonProbe(stateErrorReason)},
		{continuityRow{reason: lookupGateReason, cause: causeFailure}, outcomeTerminal, failureReasonProbe(lookupGateReason)},
		{continuityRow{reason: githubUnavailableReason, cause: causeFailure}, outcomeHold, failureReasonProbe(githubUnavailableReason)},
		{continuityRow{reason: "IMPLEMENTOR_ACTION_REQUIRED", cause: causeFailure}, outcomeTerminal, failureReasonProbe("IMPLEMENTOR_ACTION_REQUIRED")},
		{continuityRow{reason: "REMEDIATION_BUDGET_EXHAUSTED", cause: causeFailure}, outcomeTerminal, failureReasonProbe("REMEDIATION_BUDGET_EXHAUSTED")},
		{continuityRow{reason: idleGateReason, cause: causeFailure}, outcomeHold, failureReasonProbe(idleGateReason)},
		{continuityRow{reason: reviewTimeoutReason, cause: causeFailure}, outcomeHold, failureReasonProbe(reviewTimeoutReason)},
		{continuityRow{reason: "CI_WAIT_TIMEOUT", cause: causeFailure}, outcomeHold, failureReasonProbe("CI_WAIT_TIMEOUT")},
		{continuityRow{cause: causeFailure}, outcomeTerminal, failureReasonProbe("SOMETHING_NEW")},

		// Bounded operations that expired with nothing else resolving them.
		{continuityRow{gate: gateReviewTimeout, reason: reviewTimeoutReason, cause: causeExpired}, outcomeHold, expiredRequestProbe()},
		{continuityRow{gate: gateGitHubUnavailable, reason: githubUnavailableReason, cause: causeExpired}, outcomeTerminal, githubWindowProbe(true)},

		// A relaunch that would act on an unchanged resume signature.
		{continuityRow{gate: gateActionableFeedback, reason: informalFeedbackReason, cause: causeStall}, outcomeHold, relaunchProbe(causeStall, relaunch(gateActionableFeedback, informalFeedbackReason), false)},
		{continuityRow{gate: gateActionableFeedback, reason: actionableFeedbackReason, cause: causeStall}, outcomeHold, relaunchProbe(causeStall, relaunch(gateActionableFeedback, actionableFeedbackReason), false)},
		{continuityRow{gate: gateCIWaitTimeout, reason: "CI_WAIT_STATE_ERROR", cause: causeStall}, outcomeHold, relaunchProbe(causeStall, relaunch(gateCIWaitTimeout, "CI_WAIT_STATE_ERROR"), false)},
		{continuityRow{gate: gateBlindReviewRequired, reason: blindReviewRequiredReason, cause: causeStall}, outcomeHold, relaunchProbe(causeStall, relaunch(gateBlindReviewRequired, blindReviewRequiredReason), false)},
		{continuityRow{gate: gateReadyToMerge, cause: causeStall}, outcomeHold, relaunchProbe(causeStall, relaunch(gateReadyToMerge, "REVIEW_APPROVED"), false)},
		{continuityRow{gate: "ci-failure", reason: "CI_FAILURE", cause: causeStall}, outcomeHold, relaunchProbe(causeStall, relaunch("ci-failure", "CI_FAILURE"), false)},
		{continuityRow{gate: "merge-conflict", reason: "MERGE_CONFLICT", cause: causeStall}, outcomeHold, relaunchProbe(causeStall, relaunch("merge-conflict", "MERGE_CONFLICT"), false)},
		{continuityRow{gate: gateCIWaitTimeout, reason: "CI_WAIT_TIMEOUT", cause: causeStall}, outcomeHold, relaunchProbe(causeStall, relaunch(gateCIWaitTimeout, "CI_WAIT_TIMEOUT"), false)},
		{continuityRow{gate: gateReviewTimeout, reason: reviewTimeoutReason, cause: causeStall}, outcomeHold, relaunchProbe(causeStall, relaunch(gateReviewTimeout, reviewTimeoutReason), false)},
		{continuityRow{gate: gatePRHeadChanged, reason: "PR_HEAD_RECONCILE_REQUIRED", cause: causeStall}, outcomeHold, relaunchProbe(causeStall, relaunch(gatePRHeadChanged, "PR_HEAD_RECONCILE_REQUIRED"), false)},
		{continuityRow{gate: owned, reason: idleGateReason, cause: causeStall}, outcomeHold, relaunchProbe(causeStall, ownedWork(owned, idleGateReason, ""), true)},
		{continuityRow{gate: owned, reason: "REVIEW_REQUEST_REQUIRED", cause: causeStall}, outcomeHold, relaunchProbe(causeStall, ownedWork(gateReviewRequestRequired, "REVIEW_REQUEST_REQUIRED", ""), true)},
		{continuityRow{gate: owned, reason: missingPRReason, cause: causeStall}, outcomeHold, relaunchProbe(causeStall, ownedWork(owned, missingPRReason, ""), true)},
		{continuityRow{cause: causeStall}, outcomeHold, relaunchProbe(causeStall, other, false)},

		// A used-up lifecycle relaunch allowance (for owned work, the final
		// ordinary attempt).
		{continuityRow{reason: actionableFeedbackReason, cause: causeExhausted}, outcomeTerminal, relaunchProbe(causeExhausted, relaunch(gateActionableFeedback, actionableFeedbackReason), false)},
		{continuityRow{reason: "CI_WAIT_STATE_ERROR", cause: causeExhausted}, outcomeTerminal, relaunchProbe(causeExhausted, relaunch(gateCIWaitTimeout, "CI_WAIT_STATE_ERROR"), false)},
		{continuityRow{gate: gateActionableFeedback, reason: informalFeedbackReason, cause: causeExhausted}, outcomeHold, relaunchProbe(causeExhausted, relaunch(gateActionableFeedback, informalFeedbackReason), false)},
		{continuityRow{gate: gateReadyToMerge, cause: causeExhausted}, outcomeHold, relaunchProbe(causeExhausted, relaunch(gateReadyToMerge, "REVIEW_APPROVED"), false)},
		{continuityRow{gate: "ci-failure", reason: "CI_FAILURE", cause: causeExhausted}, outcomeHold, relaunchProbe(causeExhausted, relaunch("ci-failure", "CI_FAILURE"), false)},
		{continuityRow{gate: "merge-conflict", reason: "MERGE_CONFLICT", cause: causeExhausted}, outcomeHold, relaunchProbe(causeExhausted, relaunch("merge-conflict", "MERGE_CONFLICT"), false)},
		{continuityRow{gate: gateBlindReviewRequired, reason: blindReviewRequiredReason, cause: causeExhausted}, outcomeHold, relaunchProbe(causeExhausted, relaunch(gateBlindReviewRequired, blindReviewRequiredReason), false)},
		{continuityRow{gate: gateCIWaitTimeout, reason: "CI_WAIT_TIMEOUT", cause: causeExhausted}, outcomeHold, relaunchProbe(causeExhausted, relaunch(gateCIWaitTimeout, "CI_WAIT_TIMEOUT"), false)},
		{continuityRow{gate: gateReviewTimeout, reason: reviewTimeoutReason, cause: causeExhausted}, outcomeHold, relaunchProbe(causeExhausted, relaunch(gateReviewTimeout, reviewTimeoutReason), false)},
		{continuityRow{gate: gatePRHeadChanged, reason: "PR_HEAD_RECONCILE_REQUIRED", cause: causeExhausted}, outcomeHold, relaunchProbe(causeExhausted, relaunch(gatePRHeadChanged, "PR_HEAD_RECONCILE_REQUIRED"), false)},
		{continuityRow{gate: owned, reason: idleGateReason, cause: causeExhausted}, outcomeHold, relaunchProbe(causeExhausted, ownedWork(owned, idleGateReason, ""), true)},
		{continuityRow{gate: owned, reason: "REVIEW_REQUEST_REQUIRED", cause: causeExhausted}, outcomeTerminal, relaunchProbe(causeExhausted, ownedWork(gateReviewRequestRequired, "REVIEW_REQUEST_REQUIRED", ""), true)},
		{continuityRow{gate: owned, reason: missingPRReason, cause: causeExhausted}, outcomeTerminal, relaunchProbe(causeExhausted, ownedWork(owned, missingPRReason, ""), true)},
		// An idle gate whose missing evidence is not only an unrequested
		// review (here formal requested changes) is unfinished owned work.
		{continuityRow{gate: owned, reason: actionableFeedbackReason, cause: causeExhausted}, outcomeTerminal, relaunchProbe(causeExhausted, ownedWork(owned, idleGateReason, "CHANGES_REQUESTED"), true)},
		{continuityRow{gate: owned, cause: causeExhausted}, outcomeTerminal, relaunchProbe(causeExhausted, ownedWork(owned, "CUSTOM_REASON", ""), true)},
		{continuityRow{cause: causeExhausted}, outcomeHold, relaunchProbe(causeExhausted, other, false)},

		// The Run's stalled signatures cannot be read: fail closed.
		{continuityRow{cause: causeUnreadable}, outcomeHold, relaunchProbe(causeUnreadable, relaunch(gateActionableFeedback, informalFeedbackReason), false)},
	}
}

const continuityProbeHead = "head-1"

func continuityProbePR() *github.PR {
	return &github.PR{
		Number: continuityPR, State: "open", HeadRefName: continuityBranch, HeadRefOid: continuityProbeHead,
		StatusCheckRollup: "SUCCESS", MergeStateStatus: "CLEAN",
	}
}

// activeOperationProbe observes the active-wait rule: running current-head CI
// or a confirmed in-deadline review request keeps the Run waiting, also when
// a retained request has expired.
func activeOperationProbe(ciRunning, reviewRequested bool) continuityProbe {
	return continuityProbe{observe: func(t *testing.T, _ continuityTable) continuityOutcomeKind {
		pr := continuityProbePR()
		if ciRunning {
			pr.StatusCheckRollup = "PENDING"
		}
		for _, evidence := range []retainedReviewEvidence{{}, {present: true, outcome: retainedReviewTimeout}} {
			decision := decideRecoverableLifecycle(lifecycleGatePending, pr, continuityProbeHead, "", evidence, reviewRequested, "success", true)
			if decision.action != lifecycleAwait {
				t.Fatalf("resolving operation with retained %q = %v, want await", evidence.outcome, decision.action)
			}
		}
		return outcomeWait
	}}
}

// githubWindowProbe observes the GitHub recovery window after a failed
// lookup: it waits while open and ends with GATE_LOOKUP_FAILED once 24 hours
// have passed since its first failure.
func githubWindowProbe(expired bool) continuityProbe {
	return continuityProbe{observe: func(t *testing.T, _ continuityTable) continuityOutcomeKind {
		now := time.Date(2026, time.November, 19, 12, 0, 0, 0, time.UTC)
		session := &runSession{opts: runSessionOptions{now: func() time.Time { return now }}}
		recovery := githubRecovery{}
		if expired {
			recovery.WindowStarted = now.Add(-githubRecoveryBound - time.Minute)
		}
		verdict := newLifecycleObserver(session).recordGitHubLookupFailure(
			lifecycleObservationRequest{Branch: continuityBranch, RunID: "run-probe"},
			lifecycleVerdict{Evidence: map[string]any{"lookup_error": "gh pr list: exit status 1"}},
			recovery, expired, false,
		)
		if expired && verdict.Evidence["reason"] != lookupGateReason {
			t.Fatalf("expired GitHub recovery window = %v, want %s", verdict.Evidence, lookupGateReason)
		}
		return probedOutcome(t, verdict)
	}}
}

// foldedWaitProbe observes, through the event fold, that a started Run in a
// quota or capacity wait stays waiting. pinnedBy replays the wait end to end.
func foldedWaitProbe(wait string, pinnedBy func(*testing.T)) continuityProbe {
	return continuityProbe{pinnedBy: pinnedBy, observe: func(t *testing.T, _ continuityTable) continuityOutcomeKind {
		history := []events.Event{
			{Type: "run.started", RunID: "run-probe", Issue: 42, Payload: map[string]any{"branch": continuityBranch}},
		}
		switch wait {
		case "usage-limit":
			history = append(history, events.Event{Type: "run.await", RunID: "run-probe", Issue: 42, Payload: map[string]any{"await": true, "await_reason": "usage-limit"}})
		case "capacity":
			history = append(history,
				events.Event{Type: "run.await", RunID: "run-probe", Issue: 42, Payload: map[string]any{"await": true, "gate": string(lifecycleGatePending)}},
				events.Event{Type: "run.capacity_queued", RunID: "run-probe", Issue: 42, Payload: map[string]any{"branch": continuityBranch}},
			)
		}
		states, err := events.ReadRunStates(&spyEventLog{events: history})
		if err != nil {
			t.Fatal(err)
		}
		state := states["run-probe"]
		if state.IsTerminal() || state.Status() != "waiting" {
			t.Fatalf("%s wait projects as %q (terminal %v), want waiting", wait, state.Status(), state.IsTerminal())
		}
		return outcomeWait
	}}
}

// failureReasonProbe observes the failure-reason classification on a pull
// request that is open at its current head.
func failureReasonProbe(reason string) continuityProbe {
	return continuityProbe{consultsTable: true, observe: func(t *testing.T, table continuityTable) continuityOutcomeKind {
		pr := continuityProbePR()
		decision := lifecycleDecision{
			action: lifecycleFailure, gate: lifecycleGatePending, handled: true,
			failureExtras: lifecycleGateFailureEvidence(reason, "inspect the probe", lifecycleGatePending, pr, continuityProbeHead),
		}
		return failureContinuityOutcome(t, table, decision, pr)
	}}
}

// expiredRequestProbe observes a retained delegated-review request that
// expired while nothing else (running CI, a confirmed request) resolves it.
func expiredRequestProbe() continuityProbe {
	return continuityProbe{consultsTable: true, observe: func(t *testing.T, table continuityTable) continuityOutcomeKind {
		pr := continuityProbePR()
		decision := decideRecoverableLifecycle(lifecycleGatePending, pr, continuityProbeHead, "",
			retainedReviewEvidence{present: true, outcome: retainedReviewTimeout}, false, "success", true)
		if decision.action != lifecycleFailure {
			t.Fatalf("expired request decision = %v, want the expired-request failure", decision.action)
		}
		return failureContinuityOutcome(t, table, decision, pr)
	}}
}

func failureContinuityOutcome(t *testing.T, table continuityTable, decision lifecycleDecision, pr *github.PR) continuityOutcomeKind {
	t.Helper()
	verdict, held := failureContinuity(table, decision, lifecycleFailureExtras(decision, 0), pr, continuityProbeHead)
	if !held {
		return outcomeTerminal
	}
	return probedOutcome(t, verdict)
}

// relaunchProbe observes a lifecycle relaunch that the continuity rules stop:
// one whose signature the Run's stalled signatures hold (and, on a gate that
// stalls on repeat, one that repeats the last relaunch), one whose allowance
// is used up, or one whose stalled signatures cannot be read.
func relaunchProbe(cause continuityCause, evidence map[string]any, owned bool) continuityProbe {
	return continuityProbe{consultsTable: cause != causeUnreadable, observe: func(t *testing.T, table continuityTable) continuityOutcomeKind {
		signatureEvidence := cloneLifecycleExtras(evidence)
		if owned {
			signatureEvidence["gate"] = gateOwnedWorkRequired
		}
		signature := resumeSignature(signatureEvidence, continuityProbeHead)
		session := &runSession{decisionPRHead: continuityProbeHead}
		switch cause {
		case causeUnreadable:
			session.deps.eventLog = unreadableEventLog{}
		case causeStall:
			session.deps.eventLog = &spyEventLog{events: []events.Event{
				{Type: "run.started", RunID: "run-probe", Payload: map[string]any{"branch": continuityBranch}},
				{Type: "run.await", RunID: "run-probe", Payload: map[string]any{"await": true, "resume_stalled": true, stalledResumesKey: []any{signature}}},
			}}
			stalled, err := latestStalledResumes(session.deps.eventLog, "run-probe")
			if err != nil || !containsSignature(stalled, signature) {
				t.Fatalf("stall probe event did not retain its signature: %v, %v", stalled, err)
			}
		case causeExhausted:
			switch {
			case owned:
				session.finalOwnedWorkAttempt = true
			case evidence["gate"] == gateActionableFeedback:
				session.feedbackAllowance = &reviewFeedbackAllowance{usedByHead: map[string]int{repairRound(evidence, continuityProbeHead): session.resumeCapFor()}}
			default:
				session.resumeCount = session.resumeCapFor()
			}
		}
		outcome := relaunchOutcome(t, session, table, evidence, owned)
		if cause == causeStall && !owned && stallsOnRepeat(evidence) {
			repeat := &runSession{decisionPRHead: continuityProbeHead, lastResumeSignature: signature}
			if repeated := relaunchOutcome(t, repeat, table, evidence, owned); repeated != outcome {
				t.Fatalf("a repeated relaunch = %s, a recorded stall = %s; both are the same stall", repeated, outcome)
			}
		}
		return outcome
	}}
}

func relaunchOutcome(t *testing.T, session *runSession, table continuityTable, evidence map[string]any, owned bool) continuityOutcomeKind {
	t.Helper()
	observer := &lifecycleObserver{runSession: session, outcomes: table}
	verdict := observer.applyContinuity(continuityBranch, "run-probe", lifecycleVerdict{
		Action: lifecycleResume, Evidence: cloneLifecycleExtras(evidence), ImplementorWork: owned,
	})
	if verdict.Action == lifecycleResume && verdict.ImplementorWork {
		// The final owned-work attempt returns to the ordinary retry path,
		// which ends the Run with the owned work's own reason when retries
		// are exhausted.
		return outcomeTerminal
	}
	return probedOutcome(t, verdict)
}

// probedOutcome classifies an observed verdict: a failure is terminal, a
// continuity wait is a hold, and any other wait keeps waiting.
func probedOutcome(t *testing.T, verdict lifecycleVerdict) continuityOutcomeKind {
	t.Helper()
	switch {
	case verdict.Action == lifecycleFailure:
		if verdict.Evidence["await"] != nil || verdict.Evidence["gate"] != nil {
			t.Fatalf("terminal evidence carries wait markers: %v", verdict.Evidence)
		}
		return outcomeTerminal
	case verdict.Action == lifecycleAwait && continuityHoldPayload(verdict.Evidence):
		return outcomeHold
	case verdict.Action == lifecycleAwait:
		return outcomeWait
	}
	t.Fatalf("verdict = %s/%v, want a wait, a hold or a terminal failure", verdict.status(), verdict.Evidence)
	return ""
}

// The outcome table holds exactly the contract's rows with the contract's
// outcomes.
func TestAFKContinuityContract_OutcomeTableRows(t *testing.T) {
	want := map[continuityRow]continuityOutcomeKind{}
	for _, contract := range continuityContractRows() {
		if _, duplicate := want[contract.row]; duplicate {
			t.Fatalf("contract lists %+v twice", contract.row)
		}
		want[contract.row] = contract.outcome
	}
	for row, outcome := range continuityOutcomes {
		expected, listed := want[row]
		if !listed {
			t.Errorf("table row %+v -> %s is not in the AFK-continuity contract", row, outcome)
			continue
		}
		if outcome != expected {
			t.Errorf("table row %+v -> %s, contract requires %s", row, outcome, expected)
		}
	}
	for row, outcome := range want {
		if _, ok := continuityOutcomes[row]; !ok {
			t.Errorf("contract row %+v -> %s is missing from the table", row, outcome)
		}
	}
}

// Every row is exercised at its decision site, or pinned to the existing
// end-to-end scenario owned outside the table. Rows whose site consults the
// table are also flipped in an isolated copy to prove the site consumes the
// selected outcome rather than repeating its own policy.
func TestAFKContinuityContract_OutcomeTableDecidesExits(t *testing.T) {
	for _, contract := range continuityContractRows() {
		contract := contract
		t.Run(fmt.Sprintf("%s/%s/%s", contract.row.gate, contract.row.reason, contract.row.cause), func(t *testing.T) {
			got := contract.probe.observe(t, continuityOutcomes)
			if got != contract.outcome {
				t.Fatalf("observed outcome = %s, contract requires %s", got, contract.outcome)
			}
			if contract.probe.pinnedBy != nil {
				contract.probe.pinnedBy(t)
			}
			if !contract.probe.consultsTable {
				return
			}

			overridden := cloneContinuityTable(continuityOutcomes)
			overridden[contract.row] = oppositeContinuityOutcome(contract.outcome)
			got = contract.probe.observe(t, overridden)
			want := oppositeContinuityOutcome(contract.outcome)
			if got != want {
				t.Errorf("after changing row to %s, observed %s; decision site must consult the table", want, got)
			}
		})
	}

	t.Run("failure_requires_current_open_head", func(t *testing.T) {
		decision := lifecycleDecision{
			action: lifecycleFailure, gate: lifecycleGatePending,
			failureExtras: lifecycleGateFailureEvidence(idleGateReason, "inspect the gate", lifecycleGatePending, continuityProbePR(), continuityProbeHead),
		}
		for _, pr := range []*github.PR{
			{Number: continuityPR, State: "closed", HeadRefOid: continuityProbeHead},
			{Number: continuityPR, State: "open", HeadRefOid: "stale-head"},
		} {
			if verdict, held := failureContinuity(continuityOutcomes, decision, lifecycleFailureExtras(decision, 0), pr, continuityProbeHead); held || verdict.Action == lifecycleAwait {
				t.Fatalf("failure on state=%s head=%s became a continuity hold", pr.State, pr.HeadRefOid)
			}
		}
	})

	t.Run("stall_precedes_exhaustion_policy", func(t *testing.T) {
		for _, evidence := range []map[string]any{
			{"gate": gateActionableFeedback, "reason": actionableFeedbackReason},
			{"gate": gateCIWaitTimeout, "reason": "CI_WAIT_STATE_ERROR"},
		} {
			stalled := relaunchProbe(causeStall, evidence, false).observe(t, continuityOutcomes)
			if stalled != outcomeHold {
				t.Errorf("recorded stall for %v = %s, want hold", evidence, stalled)
			}
		}
		for _, evidence := range []map[string]any{
			{"gate": gateActionableFeedback, "reason": actionableFeedbackReason},
			{"gate": gateCIWaitTimeout, "reason": "CI_WAIT_STATE_ERROR"},
		} {
			exhausted := relaunchProbe(causeExhausted, evidence, false).observe(t, continuityOutcomes)
			if exhausted != outcomeTerminal {
				t.Errorf("exhausted allowance for %v = %s, want terminal", evidence, exhausted)
			}
		}
	})
}

func cloneContinuityTable(table continuityTable) continuityTable {
	clone := make(continuityTable, len(table))
	for row, outcome := range table {
		clone[row] = outcome
	}
	return clone
}

func oppositeContinuityOutcome(outcome continuityOutcomeKind) continuityOutcomeKind {
	if outcome == outcomeHold {
		return outcomeTerminal
	}
	return outcomeHold
}

// Each cause has only the outcomes its decision site can render. Failure rows
// are keyed by reason alone, and an unknown failure reason is permanent.
func TestAFKContinuityContract_OutcomeTableShape(t *testing.T) {
	allowed := map[continuityCause][]continuityOutcomeKind{
		causeResolving:  {outcomeWait},
		causeFailure:    {outcomeHold, outcomeTerminal},
		causeExpired:    {outcomeHold, outcomeTerminal},
		causeStall:      {outcomeHold, outcomeTerminal},
		causeExhausted:  {outcomeHold, outcomeTerminal},
		causeUnreadable: {outcomeHold},
	}
	for row, outcome := range continuityOutcomes {
		permitted := false
		for _, candidate := range allowed[row.cause] {
			permitted = permitted || candidate == outcome
		}
		if !permitted {
			t.Errorf("row %+v -> %s: cause %q permits only %v", row, outcome, row.cause, allowed[row.cause])
		}
		if row.cause == causeFailure && row.gate != "" {
			t.Errorf("failure row %+v is keyed by gate; failure reasons are classified by reason alone", row)
		}
	}
	for _, reason := range []string{"", "SOMETHING_NEW"} {
		if got := continuityOutcomes.outcome(continuityRow{gate: string(lifecycleGatePending), reason: reason, cause: causeFailure}); got != outcomeTerminal {
			t.Errorf("unknown failure reason %q -> %s, want terminal", reason, got)
		}
	}
	if got := (continuityTable{}).outcome(continuityRow{gate: gateReadyToMerge, cause: causeStall}); got != outcomeTerminal {
		t.Errorf("a row missing from the table -> %s, want terminal", got)
	}
}

func TestContinuityRepeatedSignaturePreservesTerminalPrecedence(t *testing.T) {
	t.Run("formal requested changes remain terminal", func(t *testing.T) {
		evidence := map[string]any{
			"gate": gateBlindReviewRequired, "reason": actionableFeedbackReason,
			"pull_request": continuityPR, "head_sha": continuityProbeHead,
		}
		signature := resumeSignature(evidence, continuityProbeHead)
		session := &runSession{decisionPRHead: continuityProbeHead, lastResumeSignature: signature}
		if got := repeatedRelaunchOutcome(t, session, continuityOutcomes, evidence); got != outcomeTerminal {
			t.Fatalf("repeated formal requested changes = %s, want terminal", got)
		}
	})

	t.Run("exhaustion wins over an otherwise stalling repeat", func(t *testing.T) {
		evidence := map[string]any{
			"gate": gateActionableFeedback, "reason": informalFeedbackReason,
			"pull_request": continuityPR, "head_sha": continuityProbeHead,
		}
		signature := resumeSignature(evidence, continuityProbeHead)
		session := &runSession{
			decisionPRHead: continuityProbeHead, lastResumeSignature: signature,
			feedbackAllowance: &reviewFeedbackAllowance{
				usedByHead: map[string]int{continuityProbeHead: (&runSession{}).resumeCapFor()},
			},
		}
		table := cloneContinuityTable(continuityOutcomes)
		row := continuityRow{gate: gateActionableFeedback, reason: informalFeedbackReason, cause: causeExhausted}
		table[row] = outcomeTerminal
		if got := repeatedRelaunchOutcome(t, session, table, evidence); got != outcomeTerminal {
			t.Fatalf("repeated exhausted allowance = %s, want exhausted row's terminal outcome", got)
		}
	})
}

func repeatedRelaunchOutcome(t *testing.T, session *runSession, table continuityTable, evidence map[string]any) continuityOutcomeKind {
	t.Helper()
	observer := &lifecycleObserver{runSession: session, outcomes: table}
	verdict := observer.applyContinuity(continuityBranch, "run-probe", lifecycleVerdict{
		Action: lifecycleResume, Evidence: cloneLifecycleExtras(evidence),
	})
	return probedOutcome(t, verdict)
}
