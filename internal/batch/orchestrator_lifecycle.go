package batch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/github"
)

// lifecycleAction is the terminal or non-terminal action an implementation-PR
// lifecycle state maps to. The merged-terminal outcomes landed in slice 1; the
// recoverable live-gate states (failed / pending / ready-to-merge) gained their
// await action in slice 2 as recoverable pull-request states moved into the
// runtime-owned lifecycle decision.
type lifecycleAction int

const (
	lifecycleNone lifecycleAction = iota
	lifecycleSuccess
	lifecycleFailure
	lifecycleAwait
	lifecycleResume
	lifecycleAborted
)

// lifecycleGate mirrors the string gates that checkPRExternalGateForPR emits
// so the decision point can match on a closed set instead of raw strings.
type lifecycleGate string

const (
	lifecycleGateNone         lifecycleGate = "none"
	lifecycleGatePending      lifecycleGate = "pending"
	lifecycleGateReady        lifecycleGate = "ready-to-merge"
	lifecycleGateResolved     lifecycleGate = "resolved"
	lifecycleGateFailed       lifecycleGate = "failed"
	lifecycleGateUnavailable  lifecycleGate = "unavailable"
	lifecycleGateOther        lifecycleGate = "other"
	lifecycleGateRetainedOnly lifecycleGate = "retained-only"
)

// errLifecycleObservationTestStop lets package tests bound a foreground wait
// without turning it into cancellation. Production wait implementations never
// return this sentinel.
var errLifecycleObservationTestStop = errors.New("stop lifecycle observation for test")

// retainedReviewEvidence is the set of facts the decision point can derive
// from local retained review artifacts. The adapter currently uses the shape
// to preserve evidence while the live PR state remains authoritative.
type retainedReviewEvidence struct {
	present          bool
	outcome          retainedReviewOutcome
	actionable       bool
	informalFeedback []informalFeedbackEvidence
	payload          map[string]any
	stateError       bool
}

// mergedMergeFacts holds the merge-intent facts the adapter gathers from the
// live PR (and the closing-reference repair records). A nil value means the
// adapter has not gathered them yet; the decision point asks for them before
// it can resolve a merged outcome.
type mergedMergeFacts struct {
	mergedWithClosingIntent bool
	mergedWithoutClosingRef bool
}

// implementationPRFacts is the immutable input to
// decideImplementationPRLifecycle. It is deliberately narrow: the host paths
// are not part of the decision, so the direct-call adapter gathers them before
// invoking the decision point.
//
// reviewRequested reports whether a confirmed delegated-review request is
// actively resolving for this PR at the current head: a validated request
// envelope matching repository/PR/head whose deadline has not passed and
// whose wait state is still pending. A requested review counts as an ongoing
// external operation even before the review run starts (issue #2743).
type implementationPRFacts struct {
	pr                 *github.PR
	headSHA            string
	repository         string
	mergeFacts         *mergedMergeFacts
	retainedEvidence   retainedReviewEvidence
	reviewRequested    bool
	reviewResolverLive bool
	attemptStatus      string
	afterAgentAttempt  bool
	requireReview      bool
	blindReview        bool
	outcomes           continuityTable
}

// lifecycleDecision is the outcome of decideImplementationPRLifecycle. The
// adapter interprets it:
//   - lifecycleNone means the adapter still needs more evidence or a PR
//     lookup before this decision can be applied.
//   - handled reports whether execution may stop on this decision.
//   - completionFailure flags failure outcomes that should carry the merge
//     completion diagnostic payload.
//   - needMergeFacts instructs the adapter to gather the merge-intent facts and
//     invoke the decision point a second time.
type lifecycleDecision struct {
	action            lifecycleAction
	gate              lifecycleGate
	cause             continuityCause
	handled           bool
	completionFailure bool
	needMergeFacts    bool
	ciRepairRequired  bool
	ciExecutionID     string
	extras            map[string]any
	// failureExtras carries structured terminal-failure evidence (reason,
	// next_action) for policy failures. It never carries "gate" or "await":
	// terminal run.finished events must not look like waits.
	failureExtras map[string]any
}

// unhandled is the zero-ish decision returned when the lifecycle state still
// belongs to the legacy handler.
func unhandled(gate lifecycleGate) lifecycleDecision {
	return lifecycleDecision{gate: gate}
}

// decidedAwait returns a handled recoverable-await decision for a live gate.
// The adapter still enriches these awaits with retained review evidence
// (actionable-feedback / informal) before they are emitted.
func decidedAwait(gate lifecycleGate) lifecycleDecision {
	return lifecycleDecision{
		action:  lifecycleAwait,
		gate:    gate,
		handled: true,
	}
}

// decideImplementationPRLifecycle folds the current implementation-PR
// lifecycle state and retained review evidence into one decision. It is pure:
// callers gather live PR facts and decode local evidence before invoking it.
func decideImplementationPRLifecycle(in implementationPRFacts) lifecycleDecision {
	if in.pr == nil {
		return unhandled(lifecycleGateNone)
	}
	outcomes := in.outcomes
	if outcomes == nil {
		outcomes = continuityOutcomes
	}
	gate := lifecycleGate(checkPRExternalGateForPR(in.pr, in.headSHA, true))
	if in.blindReview && gate == lifecycleGateReady &&
		(!strings.EqualFold(strings.TrimSpace(in.pr.StatusCheckRollup), "success") || ciActive(in.pr, in.headSHA)) {
		// Blind rounds and merge work require explicitly green checks. The
		// legacy gate also accepts an absent rollup for historical callers.
		gate = lifecycleGatePending
	}
	switch gate {
	case lifecycleGateResolved:
		if in.mergeFacts == nil {
			// The PR is merged but the adapter has not supplied the
			// merge-intent facts yet. Ask for them and recompute.
			return lifecycleDecision{
				gate:           lifecycleGateResolved,
				needMergeFacts: true,
			}
		}
		if in.mergeFacts.mergedWithClosingIntent {
			return lifecycleDecision{
				action:  lifecycleSuccess,
				gate:    lifecycleGateResolved,
				handled: true,
			}
		}
		if in.mergeFacts.mergedWithoutClosingRef {
			return lifecycleDecision{
				action:            lifecycleFailure,
				gate:              lifecycleGateResolved,
				handled:           true,
				completionFailure: true,
			}
		}
		// Both merge checks came back false on a resolved PR. A merged PR
		// whose completion evidence cannot be verified is a terminal policy
		// failure; it must not fall through to the ordinary success path.
		return lifecycleDecision{
			action:            lifecycleFailure,
			gate:              lifecycleGateResolved,
			handled:           true,
			completionFailure: true,
		}
	case lifecycleGateFailed, lifecycleGatePending, lifecycleGateReady:
		decision := decideRecoverableLifecycleWithOutcomes(outcomes, gate, in.pr, in.headSHA, in.repository, in.retainedEvidence, in.reviewRequested, in.reviewResolverLive, in.attemptStatus, in.afterAgentAttempt)
		if in.blindReview && !in.retainedEvidence.stateError &&
			strings.EqualFold(in.pr.HeadRefOid, in.headSHA) && in.headSHA != "" &&
			strings.EqualFold(strings.TrimSpace(in.pr.StatusCheckRollup), "success") &&
			!ciActive(in.pr, in.headSHA) &&
			!strings.EqualFold(strings.TrimSpace(in.pr.ReviewDecision), "CHANGES_REQUESTED") &&
			!strings.EqualFold(strings.TrimSpace(in.pr.MergeStateStatus), "DIRTY") &&
			!strings.EqualFold(strings.TrimSpace(in.pr.MergeStateStatus), "CONFLICTING") {
			if in.retainedEvidence.outcome == retainedReviewApproval {
				// A current-head blind publication is informal approval; the
				// aggregate reviewDecision need not contain a formal approval,
				// but approval cannot make a blocked/unknown head mergeable.
				if strings.EqualFold(strings.TrimSpace(in.pr.MergeStateStatus), "CLEAN") {
					return decideRecoverableLifecycleWithOutcomes(outcomes, lifecycleGateReady, in.pr, in.headSHA, in.repository, in.retainedEvidence, false, false, in.attemptStatus, in.afterAgentAttempt)
				}
				return decision
			}
			if !in.retainedEvidence.actionable {
				return blindReviewRequiredDecision(in.pr, in.retainedEvidence.payload)
			}
		}
		if in.requireReview && decision.action == lifecycleResume && decision.gate == lifecycleGateReady && in.retainedEvidence.outcome != retainedReviewApproval {
			return lifecycleRemediationDecision(gateReviewRequestRequired, "REVIEW_REQUEST_REQUIRED", "deliver a confirmed current-head delegated-review request and obtain request-scoped approval before merge work", in.pr, in.retainedEvidence.payload)
		}
		return decision
	case lifecycleGateUnavailable:
		// A non-open, non-merged PR is closed without a merge (B2.4): an
		// irrecoverable policy outcome that can never await.
		return lifecycleDecision{
			action:        lifecycleFailure,
			gate:          lifecycleGateUnavailable,
			handled:       true,
			failureExtras: map[string]any{"reason": "PULL_REQUEST_CLOSED", "next_action": "publish a recoverable implementation pull request before continuing"},
		}
	default:
		return unhandled(gate)
	}
}

// Structured terminal-failure reasons for lifecycle policy failures. A run
// must never enter a waiting state whose reason is not already being
// resolved, and must never wait for work it should perform itself
// (issue #2743): failures carry the evidence and next action instead.
const (
	idleGateReason                  = "EXTERNAL_GATE_IDLE"
	idleGateNextAction              = "confirm an external operation is resolving the gate (running CI or a confirmed review request) or advance the pull-request head; the run cannot wait without active resolution"
	implementorActionRequiredReason = "IMPLEMENTOR_ACTION_REQUIRED"
	missingPRReason                 = "PULL_REQUEST_MISSING"
	missingPRNextAction             = "push the implementation branch and create the pull request before yielding to review or CI"
	stateErrorReason                = "REVIEW_STATE_ERROR"
	stateErrorNextAction            = "inspect the retained review state for the pull request, repair or clear the corrupt artifacts, and continue with a confirmed review request"
	lookupGateReason                = "GATE_LOOKUP_FAILED"
	lookupGateNextAction            = "GitHub stayed unavailable for 24 hours; once it recovers, run `sandman run <issue> --continue`, which starts a fresh 24-hour recovery window"
)

// lifecycleGateFailureEvidence identifies the unusable gate snapshot and
// required next action without carrying the lifecycle payload keys "gate"
// or "await". A finished run must remain unmistakably terminal.
func lifecycleGateFailureEvidence(reason, nextAction string, gate lifecycleGate, pr *github.PR, headSHA string) map[string]any {
	extras := map[string]any{
		"reason":            reason,
		"next_action":       nextAction,
		"external_gate":     string(gate),
		"expected_head_sha": strings.TrimSpace(headSHA),
	}
	if pr != nil {
		extras["pull_request"] = pr.Number
		extras["head_sha"] = strings.TrimSpace(pr.HeadRefOid)
		extras["ci_state"] = strings.TrimSpace(pr.StatusCheckRollup)
		extras["review_decision"] = strings.TrimSpace(pr.ReviewDecision)
		extras["merge_state"] = strings.TrimSpace(pr.MergeStateStatus)
	}
	return extras
}

// remediationBudgetFailureEvidence preserves the live pull-request facts and
// request-scoped evidence when no in-session implementation resume budget
// remains. It writes terminal diagnostics only, never an await payload.
func remediationBudgetFailureEvidence(gate string, evidence map[string]any, nextAction string) map[string]any {
	return classifiedLifecycleFailureEvidence(gate, evidence, "REMEDIATION_BUDGET_EXHAUSTED", nextAction)
}

func classifiedLifecycleFailureEvidence(gate string, evidence map[string]any, reason, nextAction string) map[string]any {
	extras := map[string]any{
		"reason":        reason,
		"next_action":   nextAction,
		"external_gate": gate,
	}
	for _, key := range []string{"pull_request", "head_sha", "ci_wait", "review_request"} {
		if value, ok := evidence[key]; ok {
			extras[key] = value
		}
	}
	return extras
}

const ciRerunPrerequisiteKey = "ci_rerun_prerequisite"

func ciRerunPrerequisiteFor(pr *github.PR, headSHA, repository string) map[string]any {
	if pr == nil {
		return nil
	}
	return ciWorkflowPrerequisite(pr, headSHA, repository, github.CIFailedWorkflow{
		RunID: pr.CIActiveFailedWorkflowRunID, FailedChecks: pr.CIActiveFailedWorkflowRunFailedChecks,
		CheckNames: pr.CIActiveFailedWorkflowRunCheckNames,
	})
}

func ciRecoveryPrerequisiteFor(pr *github.PR, headSHA, repository string) map[string]any {
	if pr == nil || pr.CIFailedWorkflow == nil {
		return ciRerunPrerequisiteFor(pr, headSHA, repository)
	}
	return ciWorkflowPrerequisite(pr, headSHA, repository, *pr.CIFailedWorkflow)
}

func ciWorkflowPrerequisite(pr *github.PR, headSHA, repository string, workflow github.CIFailedWorkflow) map[string]any {
	if pr == nil || pr.Number <= 0 || strings.TrimSpace(headSHA) == "" ||
		strings.TrimSpace(repository) == "" ||
		!strings.EqualFold(strings.TrimSpace(pr.HeadRefOid), strings.TrimSpace(headSHA)) {
		return nil
	}
	workflowRunID := strings.TrimSpace(workflow.RunID)
	if workflowRunID == "" || len(workflow.FailedChecks) == 0 {
		return nil
	}
	failedChecks := append([]github.CIFailedCheck(nil), workflow.FailedChecks...)
	checkNames := append([]string(nil), workflow.CheckNames...)
	if len(checkNames) == 0 {
		return nil
	}
	failedNames := make([]string, 0, len(failedChecks))
	for _, check := range failedChecks {
		failedNames = append(failedNames, check.Name)
	}
	return map[string]any{
		"protocol":             "ci-rerun-prerequisite/v1",
		"repository":           strings.TrimSpace(repository),
		"pull_request":         pr.Number,
		"head_sha":             strings.TrimSpace(headSHA),
		"workflow_run_id":      workflowRunID,
		"failed_check_names":   failedNames,
		"failed_checks":        failedChecks,
		"workflow_check_names": checkNames,
	}
}

func retainedCIRerunPrerequisiteMatches(payload, prerequisite map[string]any) bool {
	retained, ok := payload[ciRerunPrerequisiteKey].(map[string]any)
	if !ok || prerequisite == nil || retained["protocol"] != "ci-rerun-prerequisite/v1" {
		return false
	}
	retainedPR, validPR := lifecycleDeadlineSeconds(retained["pull_request"])
	currentPR, validCurrentPR := lifecycleDeadlineSeconds(prerequisite["pull_request"])
	action, hasAction := ciRerunActionFromPayload(payload[ciRerunActionKey])
	return validPR && validCurrentPR && hasAction && action.matchesPrerequisite(prerequisite) &&
		retained["repository"] == prerequisite["repository"] && retainedPR == currentPR &&
		retained["head_sha"] == prerequisite["head_sha"] &&
		retained["workflow_run_id"] == prerequisite["workflow_run_id"]
}

func decideRecoverableLifecycle(gate lifecycleGate, pr *github.PR, headSHA, repository string, evidence retainedReviewEvidence, reviewRequested bool, attemptStatus string, afterAgentAttempt bool) lifecycleDecision {
	return decideRecoverableLifecycleWithOutcomes(continuityOutcomes, gate, pr, headSHA, repository, evidence, reviewRequested, false, attemptStatus, afterAgentAttempt)
}

func decideRecoverableLifecycleWithOutcomes(outcomes continuityTable, gate lifecycleGate, pr *github.PR, headSHA, repository string, evidence retainedReviewEvidence, reviewRequested, reviewResolverLive bool, attemptStatus string, afterAgentAttempt bool) lifecycleDecision {
	if outcomes == nil {
		outcomes = continuityOutcomes
	}
	// CI failures and merge conflicts are branch-owned work, not external work
	// that can make progress while the agent waits. Handle these typed facts
	// before the failed-attempt guard and legacy gate/evidence rules below.
	// Resolve conflicts first: failed checks on the old head must not hide
	// newly actionable integration work behind a stalled CI repair signature.
	if pr != nil && (strings.EqualFold(strings.TrimSpace(pr.MergeStateStatus), "DIRTY") || strings.EqualFold(strings.TrimSpace(pr.MergeStateStatus), "CONFLICTING")) {
		return lifecycleRemediationDecision("merge-conflict", "MERGE_CONFLICT", "rebase or merge the base branch, resolve conflicts, and push a new head", pr, evidence.payload)
	}
	if pr != nil && strings.EqualFold(strings.TrimSpace(pr.StatusCheckRollup), "failure") {
		if managedCIReplacementStillResolves(evidence.payload, pr, repository) {
			extras := cloneLifecycleExtras(evidence.payload)
			if extras == nil {
				extras = map[string]any{}
			}
			extras["pull_request"] = pr.Number
			extras["head_sha"] = pr.HeadRefOid
			extras["reason"] = "CI_FAILURE"
			extras["next_action"] = "wait for the verified newer current-head Actions execution to finish and for its required-check rollup to become authoritative"
			extras["gate"] = string(lifecycleGatePending)
			extras["await"] = true
			return applyResolvingLifecycleOutcome(
				outcomes, []continuityRow{{gate: continuityGateCurrentHeadCI, cause: causeResolving}},
				lifecycleDecision{action: lifecycleAwait, gate: lifecycleGatePending, handled: true, extras: extras},
				string(lifecycleGatePending), evidence.payload,
			)
		}
		prerequisite := ciRerunPrerequisiteFor(pr, headSHA, repository)
		action, hasAction := ciRerunActionFromPayload(evidence.payload[ciRerunActionKey])
		if prerequisite != nil && hasAction && action.matchesPrerequisite(prerequisite) &&
			(afterAgentAttempt || retainedCIRerunPrerequisiteMatches(evidence.payload, prerequisite)) {
			extras := cloneLifecycleExtras(evidence.payload)
			if extras == nil {
				extras = map[string]any{}
			}
			extras["pull_request"] = pr.Number
			extras["head_sha"] = pr.HeadRefOid
			extras[ciRerunPrerequisiteKey] = prerequisite
			extras[ciRerunActionKey] = action.payload()
			extras["reason"] = "CI_FAILURE"
			extras["next_action"] = "wait for the identified current-head workflow run to finish, then resume implementation-owned CI remediation"
			extras["gate"] = string(lifecycleGatePending)
			extras["await"] = true
			return applyResolvingLifecycleOutcome(
				outcomes, []continuityRow{{gate: continuityGateCurrentHeadCI, cause: causeResolving}},
				lifecycleDecision{action: lifecycleAwait, gate: lifecycleGatePending, handled: true, extras: extras},
				string(lifecycleGatePending), evidence.payload,
			)
		}
		decision := lifecycleRemediationDecision("ci-failure", "CI_FAILURE", "inspect current-head CI with gh pr checks and repair the failing checks", pr, evidence.payload)
		decision.ciRepairRequired = true
		decision.ciExecutionID = strings.TrimSpace(pr.CIExecutionID)
		return decision
	}
	if reviewResolverLive {
		// CI and the live reviewer each authorize waiting independently, even
		// if GitHub's aggregate review decision still says changes requested.
		return resolvingLifecycleDecision(outcomes, activeResolvingRows(pr, headSHA, true),
			lifecycleGatePending, evidence, reviewChangesRequestedFor(pr), false)
	}
	if attemptNeedsRetry(attemptStatus) &&
		!activeWaitAuthorized(gate, pr, headSHA, reviewRequested) &&
		!evidence.actionable && len(evidence.informalFeedback) == 0 &&
		!(gate == lifecycleGateReady && evidence.outcome == retainedReviewApproval) {
		return unhandled(gate)
	}
	if evidence.stateError && gate != lifecycleGateFailed {
		// Corrupt retained review state authorizes no wait on its own. When
		// another active signal (running CI or a confirmed request) is
		// resolving, the run may still await that operation with its gate;
		// otherwise fail closed instead of parking the run.
		if !activeWaitAuthorized(gate, pr, headSHA, reviewRequested) {
			return lifecycleDecision{
				action:        lifecycleFailure,
				gate:          lifecycleGate(gateReviewTimeoutError),
				handled:       true,
				failureExtras: lifecycleGateFailureEvidence(stateErrorReason, stateErrorNextAction, gate, pr, headSHA),
			}
		}
		// Waiting is authorized by the live operation, not by the state-read
		// error. Preserve the active gate and its deadline/identity evidence
		// while retaining the review diagnostic.
		return resolvingLifecycleDecision(outcomes, activeResolvingRows(pr, headSHA, reviewRequested),
			lifecycleGatePending, evidence, reviewChangesRequestedFor(pr), hardFailureFor(pr))
	}
	reviewChangesRequested := pr != nil && strings.EqualFold(strings.TrimSpace(pr.ReviewDecision), "CHANGES_REQUESTED")
	hardFailure := pr != nil && (strings.EqualFold(strings.TrimSpace(pr.StatusCheckRollup), "failure") || strings.EqualFold(strings.TrimSpace(pr.MergeStateStatus), "DIRTY") || strings.EqualFold(strings.TrimSpace(pr.MergeStateStatus), "CONFLICTING"))
	// Current-request approval is evaluated before any feedback. While CI is
	// pending, keep the approved request waiting instead of spending a repair
	// relaunch on comments that no longer gate this request.
	if evidence.outcome == retainedReviewApproval && gate != lifecycleGateFailed && gate != lifecycleGateReady && ciActive(pr, headSHA) {
		return resolvingLifecycleDecision(outcomes, activeResolvingRows(pr, headSHA, reviewRequested),
			gate, evidence, reviewChangesRequested, hardFailure)
	}
	if gate == lifecycleGateReady && evidence.outcome == retainedReviewApproval {
		// The external gate has finished. Merge/readiness work belongs to
		// the implementor, so relaunch it now; the batch scheduler acquires
		// an execution slot before starting the continuation.
		extras := cloneLifecycleExtras(evidence.payload)
		if extras == nil {
			extras = map[string]any{}
		}
		extras["gate"] = string(gateReadyToMerge)
		extras["await"] = true
		extras["reason"] = "REVIEW_APPROVED"
		extras["next_action"] = "revalidate current-head approval, CI, and mergeability, then execute the normal pull-request merge gate"
		return lifecycleDecision{
			action:  lifecycleResume,
			gate:    lifecycleGate(gateReadyToMerge),
			handled: true,
			extras:  extras,
		}
	}
	if evidence.outcome != retainedReviewApproval && evidence.actionable && (gate != lifecycleGateFailed || (reviewChangesRequested && !hardFailure)) {
		return lifecycleDecision{
			action:  lifecycleResume,
			gate:    lifecycleGate(gateActionableFeedback),
			handled: true,
			extras:  evidence.payload,
		}
	}
	if evidence.outcome != retainedReviewApproval && gate != lifecycleGateFailed && len(evidence.informalFeedback) > 0 {
		return lifecycleDecision{
			action:  lifecycleResume,
			gate:    lifecycleGate(gateActionableFeedback),
			handled: true,
			extras:  evidence.payload,
		}
	}
	if evidence.outcome == retainedReviewTimeout {
		// An expired delegated-review request authorizes no wait on its
		// own. When another active signal (running CI or a confirmed
		// request) is resolving, the run may still await that operation
		// with the timeout evidence attached; otherwise terminalize with
		// the timeout evidence instead of parking the run.
		if activeWaitAuthorized(gate, pr, headSHA, reviewRequested) {
			return resolvingLifecycleDecision(outcomes, activeResolvingRows(pr, headSHA, reviewRequested),
				lifecycleGatePending, evidence, reviewChangesRequested, hardFailure)
		}
		return lifecycleDecision{
			action:        lifecycleFailure,
			gate:          lifecycleGate(gateReviewTimeout),
			cause:         causeExpired,
			handled:       true,
			failureExtras: lifecycleGateFailureEvidence(reviewTimeoutReason, reviewTimeoutNextAction, gate, pr, headSHA),
		}
	}
	if gate == lifecycleGateReady {
		if reviewRequested {
			// The aggregate PR state says ready, but the active request has
			// not yet produced usable request-scoped approval evidence. Keep
			// waiting only on that confirmed in-deadline review operation.
			return resolvingLifecycleDecision(outcomes, activeResolvingRows(pr, headSHA, reviewRequested),
				gate, evidence, reviewChangesRequested, hardFailure)
		}
		if evidence.present {
			// Unusable retained request evidence cannot turn aggregate PR
			// approval into request-scoped approval.
			return lifecycleDecision{
				action:        lifecycleFailure,
				gate:          gate,
				handled:       true,
				failureExtras: lifecycleGateFailureEvidence(idleGateReason, idleGateNextAction, gate, pr, headSHA),
			}
		}
		return lifecycleDecision{
			action:  lifecycleResume,
			gate:    gate,
			handled: true,
			extras: map[string]any{
				"gate":        string(gateReadyToMerge),
				"await":       true,
				"reason":      "PULL_REQUEST_READY",
				"next_action": "revalidate current pull-request gates and execute the normal pull-request merge gate",
			},
		}
	}
	if activeWaitAuthorized(gate, pr, headSHA, reviewRequested) {
		// Running current-head CI or a confirmed in-deadline review request
		// (which counts even before the review run starts) is an actively
		// resolving external operation: waiting is legitimate.
		return resolvingLifecycleDecision(outcomes, activeResolvingRows(pr, headSHA, reviewRequested),
			gate, evidence, reviewChangesRequested, hardFailure)
	}
	// Anything else (REVIEW_REQUIRED, BLOCKED, or empty checks with nobody
	// resolving; stale heads; unmatched gates) is implementor-owned or idle
	// work. The run must fail instead of waiting.
	return lifecycleDecision{
		action:        lifecycleFailure,
		gate:          gate,
		handled:       true,
		failureExtras: lifecycleGateFailureEvidence(idleGateReason, idleGateNextAction, gate, pr, headSHA),
	}
}

func activeResolvingRows(pr *github.PR, headSHA string, reviewRequested bool) []continuityRow {
	rows := make([]continuityRow, 0, 2)
	if ciActive(pr, headSHA) {
		rows = append(rows, continuityRow{gate: continuityGateCurrentHeadCI, cause: causeResolving})
	}
	if reviewRequested {
		rows = append(rows, continuityRow{gate: continuityGateReviewRequest, cause: causeResolving})
	}
	return rows
}

func resolvingLifecycleDecision(outcomes continuityTable, rows []continuityRow, gate lifecycleGate, evidence retainedReviewEvidence, reviewChangesRequested, hardFailure bool) lifecycleDecision {
	decision := decidedAwaitWithEvidence(gate, evidence, reviewChangesRequested, hardFailure)
	return applyResolvingLifecycleOutcome(outcomes, rows, decision, string(gate), evidence.payload)
}

func applyResolvingLifecycleOutcome(outcomes continuityTable, rows []continuityRow, decision lifecycleDecision, gate string, evidence map[string]any) lifecycleDecision {
	// Multiple live operations may independently resolve the gate. Any one
	// selected wait keeps the Run waiting; otherwise a hold outranks terminal.
	selected := outcomeTerminal
	for _, row := range rows {
		if outcomes.outcome(row) == outcomeWait {
			selected = outcomeWait
			break
		}
		if outcomes.outcome(row) == outcomeHold {
			selected = outcomeHold
		}
	}
	switch selected {
	case outcomeWait:
		return decision
	case outcomeHold:
		decision.action = lifecycleAwait
		decision.extras = continuityHold(decision.extras, gate)
		return decision
	default:
		decision.action = lifecycleFailure
		decision.extras = nil
		decision.failureExtras = remediationBudgetFailureEvidence(gate, evidence,
			"inspect the current pull-request evidence; the resolving operation is no longer authorized to keep this Run waiting")
		return decision
	}
}

func reviewChangesRequestedFor(pr *github.PR) bool {
	return pr != nil && strings.EqualFold(strings.TrimSpace(pr.ReviewDecision), "CHANGES_REQUESTED")
}

func hardFailureFor(pr *github.PR) bool {
	return pr != nil && (strings.EqualFold(strings.TrimSpace(pr.StatusCheckRollup), "failure") ||
		strings.EqualFold(strings.TrimSpace(pr.MergeStateStatus), "DIRTY") ||
		strings.EqualFold(strings.TrimSpace(pr.MergeStateStatus), "CONFLICTING"))
}

func attemptNeedsRetry(status string) bool {
	status = strings.TrimSpace(strings.ToLower(status))
	return status != "" && status != "success"
}

// activeWaitAuthorized reports whether an external operation is actively
// resolving the pull-request gate: CI is queued or running on the current
// head, or a confirmed review request is in-deadline.
// Only then may the run enter waiting (issue #2743).
func activeWaitAuthorized(gate lifecycleGate, pr *github.PR, headSHA string, reviewRequested bool) bool {
	_ = gate // The selected gate documents the decision site; authorization is operation-based.
	return ciActive(pr, headSHA) || reviewRequested
}

func managedCIReplacementStillResolves(evidence map[string]any, pr *github.PR, repository string) bool {
	if pr == nil || pr.Number <= 0 || pr.CIMultipleFailedWorkflows ||
		strings.TrimSpace(pr.BaseRefName) == "" || strings.TrimSpace(pr.BaseRefOid) == "" ||
		strings.TrimSpace(pr.HeadRefOid) == "" {
		return false
	}
	wait, ok := evidence["ci_wait"].(map[string]any)
	if !ok || wait["protocol"] != ciWaitProtocol ||
		!strings.EqualFold(strings.TrimSpace(stringEvidence(wait, "head_sha")), strings.TrimSpace(pr.HeadRefOid)) {
		return false
	}
	execution, ok := wait["actions_execution"].(map[string]any)
	if !ok {
		return false
	}
	replacement, replacementOK := execution["replacement"].(bool)
	executionPR, prOK := lifecycleDeadlineSeconds(execution["pull_request"])
	workflowID, workflowOK := lifecycleDeadlineSeconds(execution["workflow_id"])
	runID, runOK := lifecycleDeadlineSeconds(execution["run_id"])
	runNumber, numberOK := lifecycleDeadlineSeconds(execution["run_number"])
	attempt, attemptOK := lifecycleDeadlineSeconds(execution["attempt"])
	executionRepository := strings.TrimSpace(stringEvidence(execution, "repository"))
	expectedExecutionID := fmt.Sprintf("actions:%d:%d:%d", workflowID, runID, attempt)
	if !replacementOK || !replacement ||
		!prOK || executionPR != int64(pr.Number) || !workflowOK || !runOK || !numberOK || !attemptOK ||
		workflowID <= 0 || runID <= 0 || runNumber <= 0 || attempt <= 0 ||
		executionRepository == "" || strings.TrimSpace(repository) != "" &&
		!strings.EqualFold(executionRepository, strings.TrimSpace(repository)) ||
		!strings.EqualFold(strings.TrimSpace(stringEvidence(execution, "event")), "pull_request") ||
		!strings.EqualFold(strings.TrimSpace(stringEvidence(execution, "head_sha")), strings.TrimSpace(pr.HeadRefOid)) ||
		strings.TrimSpace(stringEvidence(execution, "base_ref_name")) != strings.TrimSpace(pr.BaseRefName) ||
		!strings.EqualFold(strings.TrimSpace(stringEvidence(execution, "base_sha")), strings.TrimSpace(pr.BaseRefOid)) ||
		stringEvidence(wait, "execution_id") != expectedExecutionID ||
		!validCIWaitActionsStatus(stringEvidence(execution, "status"), stringEvidence(execution, "conclusion")) {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(pr.StatusCheckRollup), "success") {
		// The current PR-head rollup is authoritative. A previously observed
		// replacement cannot keep its CI deadline alive after required checks
		// have turned green.
		return false
	}
	sourceExecutionID := stringEvidence(wait, "source_execution_id")
	if sourceExecutionID != strings.TrimSpace(pr.CIExecutionID) {
		if strings.TrimSpace(pr.CIExecutionID) == "" {
			// An incomplete rollup identity is not contradictory; retain the
			// already bounded operation while GitHub's read catches up.
		} else if failedRunID := managedCIFailedRunID(pr); failedRunID != "" &&
			!managedCIWaitExecutionLinkedToPR(pr, execution, wait) {
			return false
		} else if failedRunID == "" {
			if !ciActive(pr, pr.HeadRefOid) {
				return false
			}
		}
	}
	status := strings.ToLower(strings.TrimSpace(stringEvidence(execution, "status")))
	conclusion := strings.ToLower(strings.TrimSpace(stringEvidence(execution, "conclusion")))
	switch status {
	case "queued", "in_progress", "waiting", "requested", "pending":
		return true
	case "completed":
		// A passing Actions run does not manufacture a green required-check
		// rollup. Keep observing until the PR's authoritative rollup catches up.
		// A completed failure must be diagnosed afresh; pending PR checks do
		// not override the terminal conclusion of this exact execution.
		return conclusion == "success"
	default:
		return false
	}
}

func managedCIFailedRunID(pr *github.PR) string {
	if pr == nil {
		return ""
	}
	if pr.CIFailedWorkflow != nil {
		return strings.TrimSpace(pr.CIFailedWorkflow.RunID)
	}
	return strings.TrimSpace(pr.CIActiveFailedWorkflowRunID)
}

func managedCIReplacementRollupIsStale(
	evidence map[string]any,
	pr *github.PR,
	repository string,
	source *github.ManagedCIExecution,
) bool {
	if pr == nil || !strings.EqualFold(strings.TrimSpace(pr.StatusCheckRollup), "failure") ||
		pr.Number <= 0 || strings.TrimSpace(pr.BaseRefName) == "" || strings.TrimSpace(pr.BaseRefOid) == "" ||
		strings.TrimSpace(pr.HeadRefOid) == "" || strings.TrimSpace(repository) == "" {
		return false
	}
	wait, ok := evidence["ci_wait"].(map[string]any)
	if !ok || wait["protocol"] != ciWaitProtocol ||
		!strings.EqualFold(strings.TrimSpace(stringEvidence(wait, "head_sha")), strings.TrimSpace(pr.HeadRefOid)) {
		return false
	}
	execution, ok := wait["actions_execution"].(map[string]any)
	if !ok {
		return false
	}
	replacement, _ := execution["replacement"].(bool)
	executionPR, prOK := lifecycleDeadlineSeconds(execution["pull_request"])
	workflowID, workflowOK := lifecycleDeadlineSeconds(execution["workflow_id"])
	runID, runOK := lifecycleDeadlineSeconds(execution["run_id"])
	runNumber, numberOK := lifecycleDeadlineSeconds(execution["run_number"])
	attempt, attemptOK := lifecycleDeadlineSeconds(execution["attempt"])
	if !replacement || !prOK || !workflowOK || !runOK || !numberOK || !attemptOK ||
		executionPR != int64(pr.Number) || workflowID <= 0 || runID <= 0 || runNumber <= 0 || attempt <= 0 ||
		!strings.EqualFold(strings.TrimSpace(stringEvidence(execution, "repository")), strings.TrimSpace(repository)) ||
		!strings.EqualFold(strings.TrimSpace(stringEvidence(execution, "event")), "pull_request") ||
		!strings.EqualFold(strings.TrimSpace(stringEvidence(execution, "head_sha")), strings.TrimSpace(pr.HeadRefOid)) ||
		strings.TrimSpace(stringEvidence(execution, "base_ref_name")) != strings.TrimSpace(pr.BaseRefName) ||
		!strings.EqualFold(strings.TrimSpace(stringEvidence(execution, "base_sha")), strings.TrimSpace(pr.BaseRefOid)) ||
		stringEvidence(wait, "execution_id") != fmt.Sprintf("actions:%d:%d:%d", workflowID, runID, attempt) ||
		!strings.EqualFold(strings.TrimSpace(stringEvidence(execution, "status")), "completed") ||
		strings.EqualFold(strings.TrimSpace(stringEvidence(execution, "conclusion")), "success") {
		return false
	}
	currentFailedRunID := ""
	if pr.CIFailedWorkflow != nil {
		currentFailedRunID = strings.TrimSpace(pr.CIFailedWorkflow.RunID)
	}
	if currentFailedRunID == "" {
		currentFailedRunID = strings.TrimSpace(pr.CIActiveFailedWorkflowRunID)
	}
	if currentFailedRunID != fmt.Sprint(runID) {
		return true
	}
	if managedCIVisibleReplacementChecksPending(pr, execution) {
		return false
	}
	return !managedCIWaitExecutionIdentityMatches(execution, source)
}

func managedCIWaitMatchesReplacement(evidence map[string]any, replacement *github.ManagedCIExecution) bool {
	wait, ok := evidence["ci_wait"].(map[string]any)
	if !ok || replacement == nil {
		return false
	}
	execution, ok := wait["actions_execution"].(map[string]any)
	if !ok || !managedCIWaitExecutionIdentityMatches(execution, replacement) {
		return false
	}
	isReplacement, _ := execution["replacement"].(bool)
	return isReplacement &&
		strings.EqualFold(stringEvidence(execution, "status"), strings.TrimSpace(replacement.Status)) &&
		strings.EqualFold(stringEvidence(execution, "conclusion"), strings.TrimSpace(replacement.Conclusion))
}

func managedCIWaitIncludesExecution(evidence map[string]any, candidate *github.ManagedCIExecution) bool {
	wait, ok := evidence["ci_wait"].(map[string]any)
	if !ok || candidate == nil {
		return false
	}
	execution, ok := wait["actions_execution"].(map[string]any)
	return ok && managedCIWaitExecutionIsReplacement(execution) &&
		managedCIWaitExecutionIdentityMatches(execution, candidate)
}

func managedCIReplacementOutcomeConflicts(evidence map[string]any, candidate *github.ManagedCIExecution) bool {
	wait, ok := evidence["ci_wait"].(map[string]any)
	if !ok || candidate == nil {
		return false
	}
	execution, ok := wait["actions_execution"].(map[string]any)
	if !ok || !managedCIWaitExecutionIdentityMatches(execution, candidate) {
		return false
	}
	retainedStatus := strings.ToLower(strings.TrimSpace(stringEvidence(execution, "status")))
	candidateStatus := strings.ToLower(strings.TrimSpace(candidate.Status))
	retainedConclusion := strings.ToLower(strings.TrimSpace(stringEvidence(execution, "conclusion")))
	candidateConclusion := strings.ToLower(strings.TrimSpace(candidate.Conclusion))
	return retainedStatus == "completed" && candidateStatus == "completed" &&
		retainedConclusion != "" && candidateConclusion != "" && retainedConclusion != candidateConclusion
}

func ciWaitActionsExecutionFromEvidence(evidence map[string]any) (*ciWaitActionsExecution, bool) {
	replacement, ok := evidence["replacement"].(bool)
	if !ok || !replacement {
		return nil, false
	}
	pullRequest, prOK := lifecycleDeadlineSeconds(evidence["pull_request"])
	workflowID, workflowOK := lifecycleDeadlineSeconds(evidence["workflow_id"])
	runID, runOK := lifecycleDeadlineSeconds(evidence["run_id"])
	runNumber, runNumberOK := lifecycleDeadlineSeconds(evidence["run_number"])
	attempt, attemptOK := lifecycleDeadlineSeconds(evidence["attempt"])
	if !prOK || !workflowOK || !runOK || !runNumberOK || !attemptOK {
		return nil, false
	}
	baseSHA := strings.TrimSpace(stringEvidence(evidence, "base_sha"))
	if baseSHA == "" {
		return nil, false
	}
	return &ciWaitActionsExecution{
		Repository:  strings.TrimSpace(stringEvidence(evidence, "repository")),
		PullRequest: int(pullRequest), BaseRefName: strings.TrimSpace(stringEvidence(evidence, "base_ref_name")),
		BaseSHA: baseSHA, HeadSHA: strings.TrimSpace(stringEvidence(evidence, "head_sha")),
		WorkflowID: workflowID, Event: strings.ToLower(strings.TrimSpace(stringEvidence(evidence, "event"))),
		RunID: runID, RunNumber: runNumber, Attempt: int(attempt),
		Status:      strings.ToLower(strings.TrimSpace(stringEvidence(evidence, "status"))),
		Conclusion:  strings.ToLower(strings.TrimSpace(stringEvidence(evidence, "conclusion"))),
		Replacement: true,
	}, true
}

func managedCIWaitExecutionIsReplacement(execution map[string]any) bool {
	replacement, ok := execution["replacement"].(bool)
	return ok && replacement
}

func managedCIWaitExecutionIdentityMatches(execution map[string]any, candidate *github.ManagedCIExecution) bool {
	if candidate == nil {
		return false
	}
	pullRequest, prOK := lifecycleDeadlineSeconds(execution["pull_request"])
	workflowID, workflowOK := lifecycleDeadlineSeconds(execution["workflow_id"])
	runID, runOK := lifecycleDeadlineSeconds(execution["run_id"])
	runNumber, runNumberOK := lifecycleDeadlineSeconds(execution["run_number"])
	attempt, attemptOK := lifecycleDeadlineSeconds(execution["attempt"])
	return prOK && workflowOK && runOK && runNumberOK && attemptOK &&
		pullRequest == int64(candidate.PullRequest) && workflowID == candidate.WorkflowID &&
		runID == candidate.RunID && runNumber == candidate.RunNumber && attempt == int64(candidate.Attempt) &&
		strings.EqualFold(stringEvidence(execution, "repository"), strings.TrimSpace(candidate.Repository)) &&
		strings.TrimSpace(stringEvidence(execution, "base_ref_name")) == strings.TrimSpace(candidate.BaseRefName) &&
		strings.EqualFold(stringEvidence(execution, "base_sha"), strings.TrimSpace(candidate.BaseSHA)) &&
		strings.EqualFold(stringEvidence(execution, "head_sha"), strings.TrimSpace(candidate.HeadSHA)) &&
		strings.EqualFold(stringEvidence(execution, "event"), strings.TrimSpace(candidate.Event))
}

func managedCIVisibleReplacementChecksPending(pr *github.PR, execution map[string]any) bool {
	if pr == nil || !ciActive(pr, pr.HeadRefOid) {
		return false
	}
	runID, ok := lifecycleDeadlineSeconds(execution["run_id"])
	if !ok || runID <= 0 {
		return false
	}
	if managedCIFailedRunID(pr) == fmt.Sprint(runID) {
		return true
	}
	repository := strings.TrimSpace(stringEvidence(execution, "repository"))
	for _, check := range pr.CIActiveWorkflowChecks {
		if check.RunID == runID && repository != "" &&
			strings.EqualFold(strings.TrimSpace(check.Repository), repository) {
			return true
		}
	}
	return false
}

func managedCIReplacementWait(evidence map[string]any) (map[string]any, bool) {
	wait, ok := evidence["ci_wait"].(map[string]any)
	if !ok {
		return nil, false
	}
	execution, ok := wait["actions_execution"].(map[string]any)
	if !ok {
		return nil, false
	}
	replacement, _ := execution["replacement"].(bool)
	return execution, replacement
}

func managedCIWaitExecutionLinkedToPR(pr *github.PR, execution, wait map[string]any) bool {
	if pr == nil {
		return false
	}
	runID := strings.TrimSpace(fmt.Sprint(execution["run_id"]))
	sourceRunID := strings.TrimSpace(fmt.Sprint(wait["source_workflow_run_id"]))
	currentFailedRunID := ""
	if pr.CIFailedWorkflow != nil {
		currentFailedRunID = strings.TrimSpace(pr.CIFailedWorkflow.RunID)
	}
	if currentFailedRunID == "" {
		currentFailedRunID = strings.TrimSpace(pr.CIActiveFailedWorkflowRunID)
	}
	if currentFailedRunID == "" {
		return false
	}
	if currentFailedRunID == runID || sourceRunID != "" && currentFailedRunID == sourceRunID {
		return true
	}
	switch runIDs := wait["source_workflow_run_ids"].(type) {
	case []int64:
		for _, id := range runIDs {
			if currentFailedRunID == fmt.Sprint(id) {
				return true
			}
		}
	case []any:
		for _, value := range runIDs {
			id, ok := lifecycleDeadlineSeconds(value)
			if ok && currentFailedRunID == fmt.Sprint(id) {
				return true
			}
		}
	}
	return false
}

// ciActive reports whether CI is currently resolving on the pull request:
// checks are queued or running rather than absent or terminal.
func ciActive(pr *github.PR, headSHA string) bool {
	if pr == nil || strings.TrimSpace(headSHA) == "" ||
		!strings.EqualFold(strings.TrimSpace(pr.HeadRefOid), strings.TrimSpace(headSHA)) {
		return false
	}
	if pr.CIHasActiveChecks {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(pr.StatusCheckRollup)) {
	case "pending", "in_progress", "queued":
		return true
	}
	return false
}

func decidedAwaitWithEvidence(gate lifecycleGate, evidence retainedReviewEvidence, reviewChangesRequested, hardFailure bool) lifecycleDecision {
	decision := decidedAwait(gate)
	if evidence.payload != nil && (gate != lifecycleGateFailed || evidence.actionable && reviewChangesRequested && !hardFailure) {
		decision.extras = cloneLifecycleExtras(evidence.payload)
		decision.extras["gate"] = string(gate)
		decision.extras["await"] = true
	}
	return decision
}

func lifecycleRemediationDecision(gate, reason, nextAction string, pr *github.PR, retained map[string]any) lifecycleDecision {
	extras := cloneLifecycleExtras(retained)
	if extras == nil {
		extras = map[string]any{}
	}
	extras["pull_request"] = pr.Number
	extras["head_sha"] = pr.HeadRefOid
	extras["reason"] = reason
	extras["next_action"] = nextAction
	return lifecycleDecision{action: lifecycleResume, gate: lifecycleGate(gate), handled: true, extras: extras}
}

// lifecycleFailureExtras returns the terminal extras for a lifecycle failure
// decision, deferring to the existing completion diagnostic for merged
// PRs missing the closing reference. Policy-failure evidence (reason,
// next_action) is merged verbatim; it never carries "gate" or "await" so a
// terminal run.finished event cannot look like a wait.
func lifecycleFailureExtras(d lifecycleDecision, issueNumber int) map[string]any {
	if d.completionFailure {
		return mergeCompletionFailureExtras(nil, issueNumber)
	}
	if len(d.failureExtras) == 0 {
		return nil
	}
	return cloneLifecycleExtras(d.failureExtras)
}

func (s *lifecycleObserver) decideLifecycleForAttempt(ctx context.Context, workDir, branch, runID string, hostPathsReady bool, attemptStatus string, usageLimitReached, afterAgentAttempt bool, ciRerunAction *ciRerunActionEvidence) lifecycleVerdict {
	s.decisionPRHead = ""
	if s.deps.githubClient == nil {
		return lifecycleVerdict{}
	}
	headSHA, currentHeadErr := s.currentGateHeadSnapshot(workDir)
	worktreeHeadSHA := headSHA
	if !hostPathsReady {
		headSHA = ""
	}
	var headReconcileErr error
	pr := s.ciRecoveryPR
	usedRecoverySnapshot := pr != nil
	s.ciRecoveryPR = nil
	var err error
	if pr == nil {
		pr, err = s.lifecyclePRForBranch(ctx, branch)
	}
	knownHead := ""
	if currentHeadErr == nil && hostPathsReady {
		knownHead = worktreeHeadSHA
	}
	if err == nil && s.historicalClosedPR(pr, knownHead, runID) {
		pr = nil
	}
	repository := ""
	var managedCISource *github.ManagedCIExecution
	var managedCIReplacement *github.ManagedCIExecution
	var managedCIReplacementFailures []github.ManagedCIFailureEvidence
	var managedCIReplacementFailure map[string]any
	var managedCISourceFailure map[string]any
	confirmedIndependentCI := false
	managedCIObservationUnknown := false
	gate := lifecycleGateNone
	refreshUnavailable := false
	reviewRegistrationFailure := false
	var blindEvidence retainedReviewEvidence
	if err != nil {
		if s.deps.errorLog != nil {
			fmt.Fprintf(s.deps.errorLog, "warning: external gate lookup for branch %q: %v\n", branch, err)
		}
		gate = lifecycleGatePending
	}
	if err == nil && pr != nil {
		gate = lifecycleGate(checkPRExternalGateForPR(pr, headSHA, true))
	}
	if err == nil && pr != nil && (pr.CIFailedWorkflow != nil || strings.TrimSpace(pr.CIActiveFailedWorkflowRunID) != "" || (afterAgentAttempt && ciRerunAction != nil)) {
		if name, repoErr := s.deps.githubClient.RepoName(ctx); repoErr == nil {
			repository = strings.TrimSpace(name)
		}
	}
	if pr != nil && strings.EqualFold(strings.TrimSpace(pr.State), "open") && hostPathsReady {
		// Reconcile the local head before failed-attempt admission filtering so
		// current-head review evidence cannot be rejected as stale.
		headSHA, worktreeHeadSHA, headReconcileErr = s.livePRHeadForLifecycle(ctx, workDir, branch, pr, worktreeHeadSHA, currentHeadErr)
		gate = lifecycleGate(checkPRExternalGateForPR(pr, headSHA, true))
	}
	if pr != nil && strings.EqualFold(strings.TrimSpace(pr.State), "open") {
		if s.blindReviewMode() && err == nil {
			var lookupErr error
			blindEvidence, lookupErr = s.blindReviewEvidence(ctx, pr, headSHA)
			if lookupErr != nil {
				// Use the existing bounded GitHub recovery window, including
				// failed attempts; missing evidence is not an approval.
				err = lookupErr
			}
		}
		// Register a delivered review request before any admission filtering:
		// a trigger posted during an attempt that then exited non-success is
		// still a confirmed external operation of this Run (issue #2915).
		registrationErr := s.ensureReviewRegistrationForPR(ctx, workDir, pr, headSHA, runID)
		if attemptNeedsRetry(attemptStatus) && err == nil {
			// Avoid side effects for an attempt that has no lifecycle evidence
			// worth evaluating. This is admission filtering only: it does not
			// select an action; the full decision below remains authoritative
			// whenever an active operation or retained evidence is present.
			reviewRequested := s.confirmedReviewRequestActive(ctx, workDir, pr, headSHA)
			evidence := s.retainedLifecycleEvidence(ctx, workDir, pr, headSHA)
			if s.blindReviewMode() {
				evidence = blindEvidence
			}
			if !reviewRequested && !ciActive(pr, headSHA) &&
				!strings.EqualFold(strings.TrimSpace(pr.StatusCheckRollup), "failure") &&
				!strings.EqualFold(strings.TrimSpace(pr.MergeStateStatus), "DIRTY") &&
				!strings.EqualFold(strings.TrimSpace(pr.MergeStateStatus), "CONFLICTING") &&
				!evidence.actionable && len(evidence.informalFeedback) == 0 &&
				!(evidence.outcome == retainedReviewApproval && gate == lifecycleGateReady) {
				return lifecycleVerdict{}
			}
		}
		headChanged := errors.Is(registrationErr, errReviewRegistrationHeadChanged)
		reviewRegistrationFailure = registrationErr != nil && !headChanged
		refreshLivePR := headChanged ||
			(registrationErr == nil && s.reviewRegistrationObserved)
		if refreshLivePR && !usedRecoverySnapshot {
			// Registration may observe comments and persist while the live PR
			// changes. Refresh after a confirmed head change or successful
			// observation before allowing the gate to terminalize.
			refreshedPR, refreshErr := lookupPRForExternalGate(ctx, s.deps.githubClient, branch)
			if refreshErr != nil {
				if s.deps.errorLog != nil {
					fmt.Fprintf(s.deps.errorLog, "warning: external gate refresh for branch %q: %v\n", branch, refreshErr)
				}
				// A requested refresh has no safe fallback. Do not reuse a
				// pre-registration snapshot or poll until it can be replaced.
				pr = nil
				gate = lifecycleGatePending
				refreshUnavailable = true
				err = nil
			} else {
				pr = refreshedPR
				err = nil
				if pr != nil && strings.EqualFold(strings.TrimSpace(pr.State), "open") && hostPathsReady {
					headSHA, worktreeHeadSHA, headReconcileErr = s.livePRHeadForLifecycle(ctx, workDir, branch, pr, worktreeHeadSHA, currentHeadErr)
				}
			}
		}
	}
	if err == nil && pr != nil {
		gate = lifecycleGate(checkPRExternalGateForPR(pr, headSHA, true))
	}
	if err == nil && pr != nil && !pr.Merged && strings.EqualFold(strings.TrimSpace(pr.State), "open") {
		managedCI := s.observeManagedCIFailureForAttempt(
			ctx, pr, headSHA, repository, runID, branch, worktreeHeadSHA,
		)
		pr = managedCI.PR
		repository = managedCI.Failure.Repository
		managedCISource = managedCI.Failure.Source
		managedCIReplacement = managedCI.Failure.Replacement
		managedCIReplacementFailures = managedCI.Failure.ReplacementFailures
		managedCISourceFailure = managedCI.SourceFailure
		confirmedIndependentCI = managedCI.Failure.IndependentFailure
		managedCIObservationUnknown = managedCI.Failure.Unknown
		managedCIReplacementFailure = managedCI.ReplacementFailure
		if managedCIObservationUnknown && !confirmedIndependentCI && ctx.Err() == nil {
			err = managedCI.Error
		}
	}

	if gate == lifecycleGateNone {
		if s.lifecycleAlreadyResolved && pr == nil {
			// Preserve the verifier fallback for an already-resolved task with
			// no pull request, while still routing observed PRs through the
			// lifecycle decision above.
			return lifecycleVerdict{}
		}
		if attemptNeedsRetry(attemptStatus) {
			return lifecycleVerdict{}
		}
		if strings.EqualFold(strings.TrimSpace(s.issueState), "closed") {
			// A closed work item with no PR has no pending publication or
			// external operation to resolve. Preserve the existing closed
			// issue completion path; only an open issue with no PR is a
			// missing-publication failure.
			return lifecycleVerdict{}
		}
		// A missing pull request is implementor-owned work (push the branch,
		// create the PR), not an external publication wait. There is no
		// active resolver, so the run must not enter waiting. Entry
		// continuations use this structured outcome to launch the agent for
		// the owned publication work; a session that exits with no PR
		// terminalizes as failure. Transient publication lag is not inferred
		// without an initiated operation.
		// See issue #2743.
		if ctx.Err() != nil {
			return lifecycleVerdict{Action: lifecycleAborted}
		}
		return lifecycleVerdict{Action: lifecycleResume, Evidence: map[string]any{
			"reason":      missingPRReason,
			"next_action": missingPRNextAction,
			"branch":      branch,
			"gate":        gateOwnedWorkRequired,
		}}
	}
	if refreshUnavailable || err != nil {
		// GitHub could not be observed. That is an external outage that
		// usually recovers, not evidence about the pull request: the Run waits
		// for GitHub (issue #2938). An established review or CI operation is
		// retained unchanged; its deadline is judged after recovery.
		if ctx.Err() != nil {
			return lifecycleVerdict{Action: lifecycleAborted}
		}
		lookupError := "lookup-failed"
		if refreshUnavailable {
			lookupError = "refresh-failed"
		}
		retained, established := s.priorOperationEvidence(runID, branch, worktreeHeadSHA)
		if !established && usageLimitReached && attemptNeedsRetry(attemptStatus) {
			// Provider quota recovery owns recognized usage-limit failures;
			// do not replace that gate with a GitHub outage wait.
			return lifecycleVerdict{}
		}
		retained["lookup_error"] = lookupError
		return lifecycleVerdict{Action: lifecycleAwait, Evidence: retained, Kind: lifecycleVerdictGitHubUnavailable}
	}

	// Merged PRs resolve before retained evidence is consulted, so stale or
	// malformed review records cannot override verified completion. The
	// probe runs without the confirmed-request check: verified outcomes
	// never need it, and merged verification must not perform extra
	// repository lookups.
	var mergeFacts *mergedMergeFacts
	decision := decideImplementationPRLifecycle(implementationPRFacts{
		pr:                pr,
		headSHA:           headSHA,
		repository:        repository,
		attemptStatus:     attemptStatus,
		afterAgentAttempt: afterAgentAttempt,
		outcomes:          s.continuityTable(),
	})
	if decision.needMergeFacts {
		if ctx.Err() != nil {
			return lifecycleVerdict{Action: lifecycleAborted}
		}
		merged := pr != nil && (pr.Merged || strings.EqualFold(strings.TrimSpace(pr.State), "merged"))
		mergeFacts = &mergedMergeFacts{
			mergedWithClosingIntent: merged && pr.ClosesIssue(s.issueNumber),
			mergedWithoutClosingRef: merged && !pr.ClosesIssue(s.issueNumber),
		}
		decision = decideImplementationPRLifecycle(implementationPRFacts{
			pr:                pr,
			headSHA:           headSHA,
			repository:        repository,
			mergeFacts:        mergeFacts,
			attemptStatus:     attemptStatus,
			afterAgentAttempt: afterAgentAttempt,
			outcomes:          s.continuityTable(),
		})
		if ctx.Err() != nil {
			return lifecycleVerdict{Action: lifecycleAborted}
		}
	}
	// The probe runs without retained evidence: it may only terminalize
	// verified outcomes (merged success or completion failure). Recoverable
	// gates need the retained evidence below before the decision can select
	// resume, await, or a policy failure (issue #2743).
	if decision.action == lifecycleSuccess || (decision.action == lifecycleFailure && decision.completionFailure) {
		return lifecycleVerdict{Action: decision.action, Evidence: lifecycleFailureExtras(decision, s.issueNumber), Kind: lifecycleVerdictCompletion}
	}
	if ctx.Err() != nil {
		return lifecycleVerdict{Action: lifecycleAborted}
	}
	// A confirmed in-deadline review request is an actively resolving
	// external operation even before the review run starts. Gather it here,
	// after verified outcomes have resolved, so idle gates cannot borrow a
	// wait from it and verified completion never pays for the lookup.
	reviewRequested := s.confirmedReviewRequestActive(ctx, workDir, pr, headSHA)
	evidence := s.retainedLifecycleEvidence(ctx, workDir, pr, headSHA)
	if s.blindReviewMode() {
		evidence = blindEvidence
	}
	if evidence.payload == nil {
		evidence.payload = map[string]any{}
	}
	if retained, established := s.priorOperationEvidence(runID, branch, worktreeHeadSHA); established {
		evidence.payload = mergeLifecycleDiagnostics(retained, evidence.payload)
	}
	reviewResolution := s.expiredReviewRequestResolution(ctx, pr.Number, headSHA, evidence.payload)
	if reviewResolution.expired {
		evidence.payload = cloneLifecycleExtras(evidence.payload)
		evidence.payload["review_run_observed"] = reviewResolution.reviewRunObserved
	}
	if afterAgentAttempt && repository != "" && ciRerunAction != nil && pr != nil &&
		ciRerunAction.matchesPullRequest(pr.Number, repository, headSHA) {
		evidence.payload = mergeLifecycleDiagnostics(evidence.payload, map[string]any{ciRerunActionKey: ciRerunAction.payload()})
	}
	if ciRerunEvidence := s.retainedCIRerunEvidence(ctx, runID, branch, headSHA, pr.Number,
		pr.CIActiveFailedWorkflowRunID, repository, pr.CIActiveFailedWorkflowRunFailedChecks,
		pr.CIActiveFailedWorkflowRunCheckNames); ciRerunEvidence != nil {
		evidence.payload = mergeLifecycleDiagnostics(evidence.payload, ciRerunEvidence)
	}
	if strings.EqualFold(strings.TrimSpace(pr.StatusCheckRollup), "failure") {
		if prerequisite := ciRecoveryPrerequisiteFor(pr, headSHA, repository); prerequisite != nil {
			evidence.payload[ciRecoveryPrerequisiteKey] = prerequisite
		}
	}
	if reviewRegistrationFailure {
		// A delivered trigger whose durable identity could not be recorded is
		// not a safe review wait: there is no restart-safe observer binding.
		// A separate active CI operation may still authorize an await.
		evidence.present = true
		evidence.stateError = true
	}
	if ctx.Err() != nil {
		return lifecycleVerdict{Action: lifecycleAborted}
	}
	if strings.EqualFold(strings.TrimSpace(pr.State), "open") &&
		strings.EqualFold(strings.TrimSpace(pr.HeadRefOid), strings.TrimSpace(headSHA)) {
		var ciEvidence map[string]any
		var ciErr error
		waitExecution := managedCIReplacement
		sourceIsRetainedExecution := waitExecution == nil &&
			managedCIWaitIncludesExecution(evidence.payload, managedCISource)
		if sourceIsRetainedExecution {
			// The replacement may move from Replacement to Source once its jobs
			// appear in the PR rollup. Reconcile its status without changing the
			// bounded execution identity or deadline.
			waitExecution = managedCISource
		}
		if !confirmedIndependentCI || waitExecution != nil {
			switch strings.ToLower(strings.TrimSpace(pr.StatusCheckRollup)) {
			case "pending", "in_progress", "queued", "failure":
				if waitExecution != nil {
					ciEvidence, ciErr = s.ciWaitEvidence(ctx, workDir, pr, headSHA, waitExecution)
				} else {
					ciEvidence, ciErr = s.ciWaitEvidence(ctx, workDir, pr, headSHA)
				}
			default:
				if strings.EqualFold(strings.TrimSpace(pr.MergeStateStatus), "DIRTY") ||
					strings.EqualFold(strings.TrimSpace(pr.MergeStateStatus), "CONFLICTING") {
					ciEvidence, ciErr = s.ciWaitEvidence(ctx, workDir, pr, headSHA)
				}
			}
		}
		if ctx.Err() != nil {
			return lifecycleVerdict{Action: lifecycleAborted}
		}
		if ciErr != nil {
			return lifecycleVerdict{Action: lifecycleResume, Evidence: map[string]any{
				"gate":        gateCIWaitTimeout,
				"reason":      "CI_WAIT_STATE_ERROR",
				"next_action": "inspect the persisted CI wait state and repair the current pull-request checks",
			}}
		}
		if waitExecution != nil {
			if managedCIReplacementOutcomeConflicts(ciEvidence, waitExecution) {
				retained, _ := s.priorOperationEvidence(runID, branch, worktreeHeadSHA)
				retained["lookup_error"] = "contradictory-managed-ci-replacement-outcome"
				return lifecycleVerdict{
					Action: lifecycleAwait, Evidence: retained, Kind: lifecycleVerdictGitHubUnavailable,
				}
			}
			if !confirmedIndependentCI && managedCIReplacement != nil && managedCIWaitMatchesReplacement(ciEvidence, managedCIReplacement) &&
				len(managedCIReplacementFailures) > 0 {
				pr, managedCIReplacementFailure = managedCIReplacementFailurePR(
					pr, managedCIReplacement, managedCIReplacementFailures,
				)
				confirmedIndependentCI = managedCIReplacementFailure != nil
			}
		}
		if !confirmedIndependentCI && sourceIsRetainedExecution && managedCISource != nil &&
			strings.EqualFold(strings.TrimSpace(managedCISource.Status), "completed") &&
			!strings.EqualFold(strings.TrimSpace(managedCISource.Conclusion), "success") {
			retained, established := s.priorOperationEvidence(runID, branch, worktreeHeadSHA)
			if !established {
				retained = cloneLifecycleExtras(ciEvidence)
			} else {
				retained = mergeLifecycleDiagnostics(retained, ciEvidence)
			}
			retained["lookup_error"] = "current-replacement-source-failure-unclassified"
			return lifecycleVerdict{
				Action: lifecycleAwait, Evidence: retained, Kind: lifecycleVerdictGitHubUnavailable,
			}
		}
		if !confirmedIndependentCI &&
			managedCIReplacementRollupIsStale(ciEvidence, pr, repository, managedCISource) {
			retained, established := s.priorOperationEvidence(runID, branch, worktreeHeadSHA)
			if established {
				retained = mergeLifecycleDiagnostics(retained, ciEvidence)
			} else {
				retained = cloneLifecycleExtras(ciEvidence)
			}
			if retained == nil {
				retained = map[string]any{}
			}
			retained["lookup_error"] = "replacement-outcome-not-reflected-in-current-rollup"
			return lifecycleVerdict{
				Action: lifecycleAwait, Evidence: retained, Kind: lifecycleVerdictGitHubUnavailable,
			}
		}
		evidence.payload = mergeLifecycleDiagnostics(evidence.payload, ciEvidence)
		if managedCISourceFailure != nil {
			evidence.payload = mergeLifecycleDiagnostics(evidence.payload, map[string]any{
				"managed_ci_source_failure": managedCISourceFailure,
			})
		}
		if confirmedIndependentCI {
			// A confirmed source or current replacement failure is actionable
			// even when older lifecycle evidence retained a replacement wait.
			// It cannot be hidden by unrelated hosted-runner evidence.
			delete(evidence.payload, "ci_wait")
			if managedCIReplacementFailure != nil {
				evidence.payload = mergeLifecycleDiagnostics(evidence.payload, map[string]any{
					"managed_ci_replacement_failure": managedCIReplacementFailure,
				})
				if prerequisite := ciRecoveryPrerequisiteFor(pr, headSHA, repository); prerequisite != nil {
					evidence.payload[ciRecoveryPrerequisiteKey] = prerequisite
				}
			}
		}
	}
	if evidence.outcome == retainedReviewApproval || evidence.actionable || len(evidence.informalFeedback) > 0 {
		if request, ok := evidence.payload["review_request"].(map[string]any); ok {
			resolved := cloneLifecycleExtras(request)
			resolved["operation_resolved"] = true
			evidence.payload["review_request"] = resolved
		}
	}
	s.decisionPRHead = strings.TrimSpace(pr.HeadRefOid)
	decision = decideImplementationPRLifecycle(implementationPRFacts{
		pr:                 pr,
		headSHA:            headSHA,
		repository:         repository,
		mergeFacts:         mergeFacts,
		retainedEvidence:   evidence,
		reviewRequested:    reviewRequested,
		reviewResolverLive: reviewResolution.reviewResolverLive,
		attemptStatus:      attemptStatus,
		afterAgentAttempt:  afterAgentAttempt,
		requireReview:      strings.TrimSpace(s.renderCfg.ReviewCommand) != "",
		blindReview:        s.blindReviewMode(),
		outcomes:           s.continuityTable(),
	})
	deferredDeadlineGate := ""
	deferredDeadlineEvidence := map[string]any(nil)
	if strings.EqualFold(pr.State, "open") && !evidence.stateError {
		deadlines := cloneLifecycleExtras(evidence.payload)
		// A replacement wait is authorized by its exact execution status,
		// not by a stale rollup that still says some check is pending. A
		// completed failure must be diagnosed afresh and cannot consume the
		// replacement's old CI deadline.
		if _, isReplacement := managedCIReplacementWait(evidence.payload); isReplacement {
			if !managedCIReplacementStillResolves(evidence.payload, pr, repository) {
				delete(deadlines, "ci_wait")
			}
		} else if !ciActive(pr, headSHA) {
			// A completed ordinary CI operation cannot time out unrelated
			// review or merge work.
			delete(deadlines, "ci_wait")
		}
		// A timely response resolves its request lifetime. Capacity latency
		// after that response must not retroactively turn approval into timeout.
		if evidence.outcome == retainedReviewApproval || evidence.actionable || len(evidence.informalFeedback) > 0 {
			delete(deadlines, "review_request")
		}
		if reviewResolution.reviewResolverLive && decision.action == lifecycleResume {
			// An expired review with a live resolver cannot replace a typed
			// CI repair or merge-conflict action. Its wait bound is diagnostic.
			delete(deadlines, "review_request")
		}
		if deadline, gate, bounded := lifecycleDeadline(deadlines); bounded && !s.runtimeNow().Before(deadline) {
			expiredGate := gate
			deferExpiredOperation := false
			switch gate {
			case gateReviewTimeout:
				deferExpiredOperation = decision.action == lifecycleAwait &&
					(ciActive(pr, headSHA) || reviewResolution.reviewResolverLive)
				if ciDeadline, ok := lifecycleCIWaitDeadline(deadlines); ok && !s.runtimeNow().Before(ciDeadline) {
					// The CI operation has reached its own bound. Apply its
					// unchanged timeout row rather than letting review expiry
					// override it.
					gate = gateCIWaitTimeout
					deferExpiredOperation = false
				}
			case gateCIWaitTimeout:
				// A current-head review does not extend CI's bounded operation.
				// Apply CI_WAIT_TIMEOUT when CI expires first.
			}
			if gate == gateCIWaitTimeout && managedCIReplacementStillResolves(evidence.payload, pr, repository) {
				if execution, isReplacement := managedCIReplacementWait(evidence.payload); isReplacement {
					deadlineEvidence := cloneLifecycleExtras(deadlines)
					deadlineEvidence["gate"] = gateCIWaitTimeout
					deadlineEvidence["reason"] = "CI_WAIT_TIMEOUT"
					deadlineEvidence["next_action"] = lifecycleDeadlineNextAction(gateCIWaitTimeout)
					deadlineEvidence["pull_request"] = pr.Number
					deadlineEvidence["head_sha"] = headSHA
					switch {
					case strings.EqualFold(stringEvidence(execution, "status"), "completed") &&
						strings.EqualFold(stringEvidence(execution, "conclusion"), "success"):
						deadlineEvidence["replacement_ci_outcome"] = "passed_required_check_rollup_stale"
					default:
						deadlineEvidence["replacement_ci_outcome"] = "pending_past_deadline"
					}
					return lifecycleVerdict{
						Action:   lifecycleAwait,
						Evidence: continuityHold(deadlineEvidence, gateCIWaitTimeout),
					}
				}
			}
			if deferExpiredOperation {
				extras := cloneLifecycleExtras(decision.extras)
				if extras == nil {
					extras = cloneLifecycleExtras(deadlines)
				}
				if extras == nil {
					extras = map[string]any{}
				}
				switch expiredGate {
				case gateReviewTimeout:
					deferredDeadlineEvidence, _ = deadlines["review_request"].(map[string]any)
					delete(extras, "review_request")
				}
				deferredDeadlineGate = expiredGate
				if stringEvidence(extras, "gate") == "" {
					extras["gate"] = string(decision.gate)
				}
				extras["await"] = true
				decision.extras = extras
			} else {
				deadlines["gate"] = gate
				deadlines["reason"] = lifecycleDeadlineReason(gate)
				deadlines["next_action"] = lifecycleDeadlineNextAction(gate)
				deadlines["pull_request"] = pr.Number
				deadlines["head_sha"] = headSHA
				deadlines = mergeLifecycleDiagnostics(deadlines, s.retainedReviewDiagnostics(ctx, workDir, branch, pr, headSHA))
				deadlineVerdict := lifecycleVerdict{Action: lifecycleResume, Evidence: deadlines}
				if gate == gateReviewTimeout {
					// Only the terminal outcome is decided here; on a hold,
					// Observe applies continuity once to the returned resume.
					if reviewResolution.expired {
						if terminal, ended := s.expiredReviewTerminal(deadlineVerdict); ended {
							return terminal
						}
					}
				}
				return deadlineVerdict
			}
		}
	}
	if decision.action == lifecycleFailure && isImplementorOwnedGateFailure(decision.failureExtras) && !evidence.stateError && strings.EqualFold(strings.TrimSpace(pr.HeadRefOid), strings.TrimSpace(headSHA)) {
		previousGate := decision.gate
		decision = lifecycleRemediationDecision(gateOwnedWorkRequired, idleGateReason, idleGateNextAction, pr, evidence.payload)
		if strings.TrimSpace(s.renderCfg.ReviewCommand) != "" {
			decision = lifecycleRemediationDecision(gateReviewRequestRequired, "REVIEW_REQUEST_REQUIRED", "deliver a confirmed current-head delegated-review request before yielding or attempting merge work", pr, evidence.payload)
		} else if s.blindReviewMode() && evidence.outcome != retainedReviewApproval &&
			strings.EqualFold(strings.TrimSpace(pr.StatusCheckRollup), "success") && !ciActive(pr, headSHA) {
			decision = blindReviewRequiredDecision(pr, evidence.payload)
		}
		decision.extras["external_gate"] = string(previousGate)
		decision.extras["expected_head_sha"] = headSHA
		decision.extras["review_decision"] = strings.TrimSpace(pr.ReviewDecision)
	}
	if decision.action == lifecycleSuccess || decision.action == lifecycleFailure {
		failureExtras := lifecycleFailureExtras(decision, s.issueNumber)
		if decision.action == lifecycleFailure {
			// Terminal failures keep the retained diagnostics as evidence.
			// They never carry "gate" or "await", so a finished event
			// cannot look like a wait.
			if diagnostics := s.retainedReviewDiagnostics(ctx, workDir, branch, pr, headSHA); len(diagnostics) > 0 {
				failureExtras = mergeLifecycleDiagnostics(failureExtras, diagnostics)
			}
			if decision.cause == causeExpired && decision.gate == lifecycleGate(gateReviewTimeout) {
				if reviewResolution.expired {
					failureExtras["review_run_observed"] = reviewResolution.reviewRunObserved
					if terminal, ended := s.expiredReviewTerminal(lifecycleVerdict{
						Action: lifecycleResume, Evidence: failureExtras,
					}); ended {
						return terminal
					}
				}
			}
			// AFK continuity: the shared outcome table decides whether an
			// open current-head pull-request failure becomes a continuity hold.
			if verdict, held := failureContinuity(s.continuityTable(), decision, failureExtras, pr, headSHA); held {
				return verdict
			}
		}
		verdict := lifecycleVerdict{Action: decision.action, Evidence: failureExtras}
		if decision.gate == lifecycleGateUnavailable {
			verdict.Kind = lifecycleVerdictClosedPR
		}
		return verdict
	}
	if !decision.handled {
		return lifecycleVerdict{}
	}
	extras := decision.extras
	if diagnostics := s.retainedReviewDiagnostics(ctx, workDir, branch, pr, headSHA); len(diagnostics) > 0 {
		extras = mergeLifecycleDiagnostics(extras, diagnostics)
	}
	if extras == nil {
		extras = map[string]any{"gate": string(decision.gate), "await": true}
	}
	if deferredDeadlineGate != "" {
		diagnostic := map[string]any{"gate": deferredDeadlineGate}
		if refreshed, ok := extras["review_request"]; ok {
			diagnostic["evidence"] = refreshed
			delete(extras, "review_request")
		} else if deferredDeadlineEvidence != nil {
			diagnostic["evidence"] = cloneLifecycleExtras(deferredDeadlineEvidence)
		}
		extras["expired_operation_diagnostic"] = diagnostic
	}
	if decision.action == lifecycleResume {
		// Live typed remediation outranks retained review labels.
		extras["gate"] = string(decision.gate)
	} else if _, ok := extras["gate"]; !ok {
		extras["gate"] = string(decision.gate)
	}
	extras["await"] = true
	if headReconcileErr != nil && (decision.action == lifecycleAwait || decision.action == lifecycleResume) {
		extras["gate"] = gatePRHeadChanged
		extras["reason"] = "PR_HEAD_RECONCILE_REQUIRED"
		extras["pull_request"] = pr.Number
		extras["head_sha"] = strings.TrimSpace(pr.HeadRefOid)
		extras["worktree_head_sha"] = strings.TrimSpace(worktreeHeadSHA)
		extras["head_reconcile_error"] = headReconcileErr.Error()
		nextAction := "safely reconcile the implementation worktree to the exact current pull-request head without discarding local changes, then revalidate review, CI, and mergeability"
		if action, ok := extras["next_action"].(string); ok && strings.TrimSpace(action) != "" {
			nextAction += "; then " + strings.TrimSpace(action)
		}
		extras["next_action"] = nextAction
		return lifecycleVerdict{Action: lifecycleResume, Evidence: extras}
	}
	return lifecycleVerdict{
		Action: decision.action, Evidence: extras,
		ciRepairRequired: decision.ciRepairRequired, ciExecutionID: decision.ciExecutionID,
	}
}

func (s *runSession) lifecyclePRForBranch(ctx context.Context, branch string) (*github.PR, error) {
	if s.lifecyclePRSnapshot != nil {
		pr := s.lifecyclePRSnapshot
		s.lifecyclePRSnapshot = nil
		if pr.Merged || strings.EqualFold(strings.TrimSpace(pr.State), "merged") {
			return pr, nil
		}
	}
	return lookupPRForExternalGate(ctx, s.deps.githubClient, branch)
}

// historicalClosedPR excludes an old, unmerged PR on a reused branch from
// this Run's lifecycle. A closed PR is authoritative (PULL_REQUEST_CLOSED)
// only when it is proven to belong to this logical Run: its head equals the
// Run's known head, or it closed at or after the Run's original fresh
// admission. A closure before that admission is history whatever the Run's
// head, including a continued Run with no commits (issue #2940).
//
// The original admission is read through the run.continued previous_run_id
// chain: `sandman run --continue` starts a new RunID, and in-session quota
// probes or restarts reuse it, so neither can turn a closure during the
// logical Run into history. Unknown evidence (no closure time, no resolvable
// admission, unreadable events) never makes a closed PR terminal while the
// work item is open; the Run continues to owned publication work instead. On
// a closed work item, or for an "already resolved" task, it keeps the
// closed-PR decision, so an unplaced closure cannot become a completion path.
func (s *runSession) historicalClosedPR(pr *github.PR, headSHA, runID string) bool {
	if pr == nil || pr.Merged || !strings.EqualFold(strings.TrimSpace(pr.State), "closed") {
		return false
	}
	if strings.TrimSpace(headSHA) != "" && strings.EqualFold(strings.TrimSpace(pr.HeadRefOid), strings.TrimSpace(headSHA)) {
		return false
	}
	closedAt := pr.ClosedAt
	if closedAt.IsZero() {
		// UpdatedAt never precedes the closure, so it can only make an old
		// closure look current, never the reverse.
		closedAt = pr.UpdatedAt
	}
	admitted, admissionKnown := s.originalAdmission(runID)
	if admissionKnown && !closedAt.IsZero() {
		// GitHub timestamps have second precision. A closure in the
		// admission second cannot prove that it predates this Run.
		return closedAt.Before(admitted.Truncate(time.Second))
	}
	return !strings.EqualFold(strings.TrimSpace(s.issueState), "closed") && !s.lifecycleAlreadyResolved
}

// originalAdmission returns the timestamp of the fresh run.started that
// admitted the logical Run runID belongs to, following continuation links.
func (s *runSession) originalAdmission(runID string) (time.Time, bool) {
	if runID == "" || s.deps.eventLog == nil {
		return time.Time{}, false
	}
	log, err := s.deps.eventLog.Read()
	if err != nil {
		return time.Time{}, false
	}
	visited := map[string]bool{}
	for runID != "" && !visited[runID] {
		visited[runID] = true
		next := ""
		for _, event := range log {
			if event.RunID != runID {
				continue
			}
			if event.Type == "run.started" {
				return event.Timestamp, !event.Timestamp.IsZero()
			}
			if event.Type == "run.continued" {
				previous, _ := event.Payload["previous_run_id"].(string)
				if previous = strings.TrimSpace(previous); previous != runID {
					next = previous
				}
				break
			}
		}
		runID = next
	}
	return time.Time{}, false
}

func cloneLifecycleExtras(extras map[string]any) map[string]any {
	clone := make(map[string]any, len(extras))
	for key, value := range extras {
		clone[key] = value
	}
	return clone
}

func mergeLifecycleDiagnostics(extras, diagnostics map[string]any) map[string]any {
	if len(diagnostics) == 0 {
		return extras
	}
	if extras == nil {
		extras = map[string]any{}
	}
	for key, value := range diagnostics {
		if key != "review_request" {
			extras[key] = value
			continue
		}
		diagnosticRequest, diagnosticOK := value.(map[string]any)
		evidenceRequest, evidenceOK := extras[key].(map[string]any)
		if !diagnosticOK || !evidenceOK {
			extras[key] = value
			continue
		}
		merged := make(map[string]any, len(diagnosticRequest)+len(evidenceRequest))
		for requestKey, requestValue := range diagnosticRequest {
			merged[requestKey] = requestValue
		}
		for requestKey, requestValue := range evidenceRequest {
			merged[requestKey] = requestValue
		}
		extras[key] = merged
	}
	return extras
}

func (s *runSession) lifecyclePollIntervals(extras map[string]any) []time.Duration {
	if len(s.opts.lifecyclePollPlan) > 0 {
		valid := true
		for _, interval := range s.opts.lifecyclePollPlan {
			if interval < 0 {
				valid = false
				break
			}
		}
		if valid {
			return append([]time.Duration(nil), s.opts.lifecyclePollPlan...)
		}
	}
	if request, ok := extras["review_request"].(map[string]any); ok {
		if raw, ok := request["poll_plan"].([]int); ok && len(raw) > 0 {
			plan := make([]time.Duration, 0, len(raw))
			valid := true
			for _, seconds := range raw {
				if seconds < 0 {
					valid = false
					break
				}
				plan = append(plan, time.Duration(seconds)*time.Second)
			}
			if valid && len(plan) > 0 {
				return plan
			}
		}
		if raw, ok := request["poll_plan"].([]any); ok && len(raw) > 0 {
			plan := make([]time.Duration, 0, len(raw))
			valid := true
			for _, value := range raw {
				seconds, ok := value.(float64)
				if !ok || seconds < 0 || seconds != float64(int(seconds)) {
					valid = false
					break
				}
				plan = append(plan, time.Duration(int(seconds))*time.Second)
			}
			if valid && len(plan) > 0 {
				return plan
			}
		}
	}
	return []time.Duration{managedObservationInterval}
}

func (s *runSession) waitForLifecyclePoll(ctx context.Context, interval time.Duration) error {
	if s.opts.lifecycleWait != nil {
		return s.opts.lifecycleWait(ctx, interval)
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func lifecycleDeadline(extras map[string]any) (time.Time, string, bool) {
	var deadline time.Time
	var gate string
	if reviewDeadline, ok := lifecycleReviewRequestDeadline(extras); ok {
		deadline, gate = reviewDeadline, gateReviewTimeout
	}
	if ciDeadline, ok := lifecycleCIWaitDeadline(extras); ok {
		if deadline.IsZero() || ciDeadline.Before(deadline) {
			deadline, gate = ciDeadline, gateCIWaitTimeout
		}
	}
	return deadline, gate, !deadline.IsZero()
}

func lifecycleReviewRequestDeadline(extras map[string]any) (time.Time, bool) {
	request, ok := extras["review_request"].(map[string]any)
	if !ok {
		return time.Time{}, false
	}
	resolved, _ := request["operation_resolved"].(bool)
	if resolved {
		return time.Time{}, false
	}
	seconds, ok := lifecycleDeadlineSeconds(request["deadline_unix_seconds"])
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0), true
}

func lifecycleCIWaitDeadline(extras map[string]any) (time.Time, bool) {
	ciWait, ok := extras["ci_wait"].(map[string]any)
	if !ok {
		return time.Time{}, false
	}
	seconds, ok := lifecycleDeadlineSeconds(ciWait["deadline_unix_seconds"])
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0), true
}

func lifecycleDeadlineSeconds(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), typed > 0
	case int64:
		return typed, typed > 0
	case float64:
		if typed > 0 && typed == float64(int64(typed)) {
			return int64(typed), true
		}
	}
	return 0, false
}

func lifecycleDeadlineReason(gate string) string {
	if gate == gateCIWaitTimeout {
		return "CI_WAIT_TIMEOUT"
	}
	return reviewTimeoutReason
}

func lifecycleDeadlineNextAction(gate string) string {
	if gate == gateCIWaitTimeout {
		return "inspect current-head CI, repair any failing checks, and push a new pull-request head"
	}
	return reviewTimeoutNextAction
}
