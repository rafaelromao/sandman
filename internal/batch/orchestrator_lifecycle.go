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
	pr               *github.PR
	headSHA          string
	mergeFacts       *mergedMergeFacts
	retainedEvidence retainedReviewEvidence
	reviewRequested  bool
	attemptStatus    string
	requireReview    bool
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
	handled           bool
	completionFailure bool
	needMergeFacts    bool
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
	gate := lifecycleGate(checkPRExternalGateForPR(in.pr, in.headSHA, true))
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
		decision := decideRecoverableLifecycle(gate, in.pr, in.headSHA, in.retainedEvidence, in.reviewRequested, in.attemptStatus)
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
	idleGateReason       = "EXTERNAL_GATE_IDLE"
	idleGateNextAction   = "confirm an external operation is resolving the gate (running CI or a confirmed review request) or advance the pull-request head; the run cannot wait without active resolution"
	missingPRReason      = "PULL_REQUEST_MISSING"
	missingPRNextAction  = "push the implementation branch and create the pull request before yielding to review or CI"
	stateErrorReason     = "REVIEW_STATE_ERROR"
	stateErrorNextAction = "inspect the retained review state for the pull request, repair or clear the corrupt artifacts, and continue with a confirmed review request"
	lookupGateReason     = "GATE_LOOKUP_FAILED"
	lookupGateNextAction = "retry the run after GitHub is reachable; no wait state was recorded"
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
	extras := map[string]any{
		"reason":        "REMEDIATION_BUDGET_EXHAUSTED",
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

func decideRecoverableLifecycle(gate lifecycleGate, pr *github.PR, headSHA string, evidence retainedReviewEvidence, reviewRequested bool, attemptStatus string) lifecycleDecision {
	// CI failures and merge conflicts are branch-owned work, not external work
	// that can make progress while the agent waits. Handle these typed facts
	// before the failed-attempt guard and legacy gate/evidence rules below.
	if pr != nil && strings.EqualFold(strings.TrimSpace(pr.StatusCheckRollup), "failure") {
		return lifecycleRemediationDecision("ci-failure", "CI_FAILURE", "inspect current-head CI with gh pr checks and repair the failing checks", pr, evidence.payload)
	}
	if pr != nil && (strings.EqualFold(strings.TrimSpace(pr.MergeStateStatus), "DIRTY") || strings.EqualFold(strings.TrimSpace(pr.MergeStateStatus), "CONFLICTING")) {
		return lifecycleRemediationDecision("merge-conflict", "MERGE_CONFLICT", "rebase or merge the base branch, resolve conflicts, and push a new head", pr, evidence.payload)
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
		extras := cloneLifecycleExtras(evidence.payload)
		if extras == nil {
			extras = map[string]any{}
		}
		// Waiting is authorized by the live operation, not by the state-read
		// error. Preserve the active gate and its deadline/identity evidence
		// while retaining the review diagnostic.
		extras["gate"] = string(lifecycleGatePending)
		extras["await"] = true
		return lifecycleDecision{
			action:  lifecycleAwait,
			gate:    lifecycleGatePending,
			handled: true,
			extras:  extras,
		}
	}
	reviewChangesRequested := pr != nil && strings.EqualFold(strings.TrimSpace(pr.ReviewDecision), "CHANGES_REQUESTED")
	hardFailure := pr != nil && (strings.EqualFold(strings.TrimSpace(pr.StatusCheckRollup), "failure") || strings.EqualFold(strings.TrimSpace(pr.MergeStateStatus), "DIRTY") || strings.EqualFold(strings.TrimSpace(pr.MergeStateStatus), "CONFLICTING"))
	if evidence.actionable && (gate != lifecycleGateFailed || (reviewChangesRequested && !hardFailure)) {
		return lifecycleDecision{
			action:  lifecycleResume,
			gate:    lifecycleGate(gateActionableFeedback),
			handled: true,
			extras:  evidence.payload,
		}
	}
	if gate != lifecycleGateFailed && len(evidence.informalFeedback) > 0 {
		return lifecycleDecision{
			action:  lifecycleResume,
			gate:    lifecycleGate(gateActionableFeedback),
			handled: true,
			extras:  evidence.payload,
		}
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
	if evidence.outcome == retainedReviewTimeout {
		// An expired delegated-review request authorizes no wait on its
		// own. When another active signal (running CI or a confirmed
		// request) is resolving, the run may still await that operation
		// with the timeout evidence attached; otherwise terminalize with
		// the timeout evidence instead of parking the run.
		if activeWaitAuthorized(gate, pr, headSHA, reviewRequested) {
			return decidedAwaitWithEvidence(lifecycleGatePending, evidence, reviewChangesRequested, hardFailure)
		}
		return lifecycleDecision{
			action:        lifecycleFailure,
			gate:          lifecycleGate(gateReviewTimeout),
			handled:       true,
			failureExtras: lifecycleGateFailureEvidence(reviewTimeoutReason, reviewTimeoutNextAction, gate, pr, headSHA),
		}
	}
	if gate == lifecycleGateReady {
		if reviewRequested {
			// The aggregate PR state says ready, but the active request has
			// not yet produced usable request-scoped approval evidence. Keep
			// waiting only on that confirmed in-deadline review operation.
			return decidedAwaitWithEvidence(gate, evidence, reviewChangesRequested, hardFailure)
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
		return decidedAwaitWithEvidence(gate, evidence, reviewChangesRequested, hardFailure)
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

// ciActive reports whether CI is currently resolving on the pull request:
// checks are queued or running rather than absent or terminal.
func ciActive(pr *github.PR, headSHA string) bool {
	if pr == nil || strings.TrimSpace(headSHA) == "" ||
		!strings.EqualFold(strings.TrimSpace(pr.HeadRefOid), strings.TrimSpace(headSHA)) {
		return false
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

// lifecycleStatusRepr maps a decided action to the status string the run
// session records, keeping the decision point independent of runSession.
func lifecycleStatusRepr(d lifecycleDecision) string {
	switch d.action {
	case lifecycleSuccess:
		return "success"
	case lifecycleFailure:
		return "failure"
	case lifecycleAwait:
		return "await"
	case lifecycleResume:
		return "resume"
	case lifecycleAborted:
		return "aborted"
	default:
		return ""
	}
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

// handleLifecycleDecision turns an AgentRun exit into the result selected by
// the lifecycle decision point. It gathers live PR facts and retained review
// evidence, while event and prompt writing remain adapter concerns.
func (s *runSession) handleLifecycleDecisionAfterAgent(ctx context.Context, workDir, branch, logPath, runID string, hostPathsReady bool) (string, map[string]any, bool) {
	return s.handleLifecycleDecisionForAttempt(ctx, workDir, branch, logPath, runID, hostPathsReady, "success")
}

func (s *runSession) handleLifecycleDecision(ctx context.Context, workDir, branch, logPath, runID string, hostPathsReady bool) (string, map[string]any, bool) {
	return s.handleLifecycleDecisionForAttempt(ctx, workDir, branch, logPath, runID, hostPathsReady, "success")
}

func (s *runSession) handleLifecycleDecisionForAttempt(ctx context.Context, workDir, branch, logPath, runID string, hostPathsReady bool, attemptStatus string) (string, map[string]any, bool) {
	if s.deps.githubClient == nil {
		return "", nil, false
	}
	headSHA, currentHeadErr := s.currentGateHeadSnapshot(workDir)
	worktreeHeadSHA := headSHA
	if !hostPathsReady {
		headSHA = ""
	}
	var headReconcileErr error
	pr, err := s.lifecyclePRForBranch(ctx, branch)
	gate := lifecycleGateNone
	refreshUnavailable := false
	reviewRegistrationFailure := false
	if err != nil {
		if s.deps.errorLog != nil {
			fmt.Fprintf(s.deps.errorLog, "warning: external gate lookup for branch %q: %v\n", branch, err)
		}
		gate = lifecycleGatePending
	}
	if err == nil && pr != nil {
		gate = lifecycleGate(checkPRExternalGateForPR(pr, headSHA, true))
	}
	if pr != nil && strings.EqualFold(strings.TrimSpace(pr.State), "open") && hostPathsReady {
		// Reconcile the local head before failed-attempt admission filtering so
		// current-head review evidence cannot be rejected as stale.
		headSHA, worktreeHeadSHA, headReconcileErr = s.livePRHeadForLifecycle(ctx, workDir, branch, pr, worktreeHeadSHA, currentHeadErr)
		gate = lifecycleGate(checkPRExternalGateForPR(pr, headSHA, true))
	}
	if pr != nil && strings.EqualFold(strings.TrimSpace(pr.State), "open") {
		if attemptNeedsRetry(attemptStatus) {
			// Avoid side effects for an attempt that has no lifecycle evidence
			// worth evaluating. This is admission filtering only: it does not
			// select an action; the full decision below remains authoritative
			// whenever an active operation or retained evidence is present.
			reviewRequested := s.confirmedReviewRequestActive(ctx, workDir, pr, headSHA)
			evidence := s.retainedLifecycleEvidence(ctx, workDir, pr, headSHA)
			if !reviewRequested && !ciActive(pr, headSHA) &&
				!strings.EqualFold(strings.TrimSpace(pr.StatusCheckRollup), "failure") &&
				!strings.EqualFold(strings.TrimSpace(pr.MergeStateStatus), "DIRTY") &&
				!strings.EqualFold(strings.TrimSpace(pr.MergeStateStatus), "CONFLICTING") &&
				!evidence.actionable && len(evidence.informalFeedback) == 0 &&
				!(evidence.outcome == retainedReviewApproval && gate == lifecycleGateReady) {
				return "", nil, false
			}
		}
		registrationErr := s.ensureReviewRegistrationForPR(ctx, workDir, pr, headSHA, runID)
		headChanged := errors.Is(registrationErr, errReviewRegistrationHeadChanged)
		reviewRegistrationFailure = registrationErr != nil && !headChanged
		refreshLivePR := headChanged ||
			(registrationErr == nil && s.reviewRegistrationObserved)
		if refreshLivePR {
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

	if gate == lifecycleGateNone {
		if s.lifecycleAlreadyResolved && pr == nil {
			// Preserve the verifier fallback for an already-resolved task with
			// no pull request, while still routing observed PRs through the
			// lifecycle decision above.
			return "", nil, false
		}
		if attemptNeedsRetry(attemptStatus) {
			return "", nil, false
		}
		if strings.EqualFold(strings.TrimSpace(s.issueState), "closed") {
			// A closed work item with no PR has no pending publication or
			// external operation to resolve. Preserve the existing closed
			// issue completion path; only an open issue with no PR is a
			// missing-publication failure.
			return "", nil, false
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
			return "aborted", nil, true
		}
		return "resume", map[string]any{
			"reason":      missingPRReason,
			"next_action": missingPRNextAction,
			"branch":      branch,
			"gate":        gateOwnedWorkRequired,
		}, true
	}
	if refreshUnavailable {
		// Re-observe only previously authorized evidence with its fixed
		// identity/deadline; without it there is no safe pending fallback.
		if ctx.Err() != nil {
			return "aborted", nil, true
		}
		if status, extras, ok := s.priorObservation(runID, branch, worktreeHeadSHA); ok {
			return status, extras, true
		}
		if attemptNeedsRetry(attemptStatus) {
			return "", nil, false
		}
		return "failure", map[string]any{
			"reason":       lookupGateReason,
			"next_action":  lookupGateNextAction,
			"branch":       branch,
			"lookup_error": "refresh-failed",
		}, true
	}
	if err != nil {
		// Transport failure cannot create an operation. An already admitted
		// bounded operation may be re-observed without renewing its deadline.
		if ctx.Err() != nil {
			return "aborted", nil, true
		}
		if status, extras, ok := s.priorObservation(runID, branch, worktreeHeadSHA); ok {
			return status, extras, true
		}
		if attemptNeedsRetry(attemptStatus) {
			return "", nil, false
		}
		return "failure", map[string]any{
			"reason":       lookupGateReason,
			"next_action":  lookupGateNextAction,
			"branch":       branch,
			"lookup_error": "lookup-failed",
		}, true
	}

	// Merged PRs resolve before retained evidence is consulted, so stale or
	// malformed review records cannot override verified completion. The
	// probe runs without the confirmed-request check: verified outcomes
	// never need it, and merged verification must not perform extra
	// repository lookups.
	var mergeFacts *mergedMergeFacts
	decision := decideImplementationPRLifecycle(implementationPRFacts{
		pr:            pr,
		headSHA:       headSHA,
		attemptStatus: attemptStatus,
	})
	if decision.needMergeFacts {
		if ctx.Err() != nil {
			return "aborted", nil, true
		}
		merged := pr != nil && (pr.Merged || strings.EqualFold(strings.TrimSpace(pr.State), "merged"))
		mergeFacts = &mergedMergeFacts{
			mergedWithClosingIntent: merged && pr.ClosesIssue(s.issueNumber),
			mergedWithoutClosingRef: merged && !pr.ClosesIssue(s.issueNumber),
		}
		decision = decideImplementationPRLifecycle(implementationPRFacts{
			pr:            pr,
			headSHA:       headSHA,
			mergeFacts:    mergeFacts,
			attemptStatus: attemptStatus,
		})
		if ctx.Err() != nil {
			return "aborted", nil, true
		}
	}
	// The probe runs without retained evidence: it may only terminalize
	// verified outcomes (merged success or completion failure). Recoverable
	// gates need the retained evidence below before the decision can select
	// resume, await, or a policy failure (issue #2743).
	if decision.action == lifecycleSuccess || (decision.action == lifecycleFailure && decision.completionFailure) {
		return lifecycleStatusRepr(decision), lifecycleFailureExtras(decision, s.issueNumber), true
	}
	if ctx.Err() != nil {
		return "aborted", nil, true
	}
	// A confirmed in-deadline review request is an actively resolving
	// external operation even before the review run starts. Gather it here,
	// after verified outcomes have resolved, so idle gates cannot borrow a
	// wait from it and verified completion never pays for the lookup.
	reviewRequested := s.confirmedReviewRequestActive(ctx, workDir, pr, headSHA)
	evidence := s.retainedLifecycleEvidence(ctx, workDir, pr, headSHA)
	if reviewRegistrationFailure {
		// A delivered trigger whose durable identity could not be recorded is
		// not a safe review wait: there is no restart-safe observer binding.
		// A separate active CI operation may still authorize an await.
		evidence.present = true
		evidence.stateError = true
	}
	if ctx.Err() != nil {
		return "aborted", nil, true
	}
	if strings.EqualFold(strings.TrimSpace(pr.State), "open") &&
		strings.EqualFold(strings.TrimSpace(pr.HeadRefOid), strings.TrimSpace(headSHA)) {
		var ciEvidence map[string]any
		var ciErr error
		switch strings.ToLower(strings.TrimSpace(pr.StatusCheckRollup)) {
		case "pending", "in_progress", "queued", "failure":
			ciEvidence, ciErr = s.ciWaitEvidence(workDir, pr, headSHA)
		default:
			if strings.EqualFold(strings.TrimSpace(pr.MergeStateStatus), "DIRTY") ||
				strings.EqualFold(strings.TrimSpace(pr.MergeStateStatus), "CONFLICTING") {
				ciEvidence, ciErr = s.ciWaitEvidence(workDir, pr, headSHA)
			}
		}
		if ciErr != nil {
			return "resume", map[string]any{
				"gate":        gateCIWaitTimeout,
				"reason":      "CI_WAIT_STATE_ERROR",
				"next_action": "inspect the persisted CI wait state and repair the current pull-request checks",
			}, true
		}
		evidence.payload = mergeLifecycleDiagnostics(evidence.payload, ciEvidence)
	}
	if evidence.outcome == retainedReviewApproval || evidence.actionable || len(evidence.informalFeedback) > 0 {
		if request, ok := evidence.payload["review_request"].(map[string]any); ok {
			resolved := cloneLifecycleExtras(request)
			resolved["operation_resolved"] = true
			evidence.payload["review_request"] = resolved
		}
	}
	decision = decideImplementationPRLifecycle(implementationPRFacts{
		pr:               pr,
		headSHA:          headSHA,
		mergeFacts:       mergeFacts,
		retainedEvidence: evidence,
		reviewRequested:  reviewRequested,
		attemptStatus:    attemptStatus,
		requireReview:    strings.TrimSpace(s.renderCfg.ReviewCommand) != "",
	})
	if strings.EqualFold(pr.State, "open") && !evidence.stateError {
		deadlines := cloneLifecycleExtras(evidence.payload)
		// A completed CI operation cannot time out unrelated review/merge work.
		// Only currently resolving checks carry a CI wait deadline.
		if !ciActive(pr, headSHA) {
			delete(deadlines, "ci_wait")
		}
		// A timely response resolves its request lifetime. Capacity latency
		// after that response must not retroactively turn approval into timeout.
		if evidence.outcome == retainedReviewApproval || evidence.actionable || len(evidence.informalFeedback) > 0 {
			delete(deadlines, "review_request")
		}
		if deadline, gate, bounded := lifecycleDeadline(deadlines); bounded && !s.runtimeNow().Before(deadline) {
			deadlines["gate"] = gate
			deadlines["reason"] = lifecycleDeadlineReason(gate)
			deadlines["next_action"] = lifecycleDeadlineNextAction(gate)
			deadlines["pull_request"] = pr.Number
			deadlines["head_sha"] = headSHA
			deadlines = mergeLifecycleDiagnostics(deadlines, s.retainedReviewDiagnostics(ctx, workDir, branch, pr, headSHA))
			return "resume", deadlines, true
		}
	}
	if decision.action == lifecycleFailure && isImplementorOwnedGateFailure(decision.failureExtras) && !evidence.stateError && strings.EqualFold(strings.TrimSpace(pr.HeadRefOid), strings.TrimSpace(headSHA)) {
		previousGate := decision.gate
		decision = lifecycleRemediationDecision(gateOwnedWorkRequired, idleGateReason, idleGateNextAction, pr, evidence.payload)
		if strings.TrimSpace(s.renderCfg.ReviewCommand) != "" {
			decision = lifecycleRemediationDecision(gateReviewRequestRequired, "REVIEW_REQUEST_REQUIRED", "deliver a confirmed current-head delegated-review request before yielding or attempting merge work", pr, evidence.payload)
		}
		decision.extras["external_gate"] = string(previousGate)
		decision.extras["expected_head_sha"] = headSHA
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
		}
		return lifecycleStatusRepr(decision), failureExtras, true
	}
	if !decision.handled {
		return "", nil, false
	}
	extras := decision.extras
	if diagnostics := s.retainedReviewDiagnostics(ctx, workDir, branch, pr, headSHA); len(diagnostics) > 0 {
		extras = mergeLifecycleDiagnostics(extras, diagnostics)
	}
	if extras == nil {
		extras = map[string]any{"gate": string(decision.gate), "await": true}
	}
	if decision.action == lifecycleResume {
		// Live typed remediation outranks retained review labels.
		extras["gate"] = string(decision.gate)
	} else if _, ok := extras["gate"]; !ok {
		extras["gate"] = string(decision.gate)
	}
	extras["await"] = true
	status := lifecycleStatusRepr(decision)
	if headReconcileErr != nil && (status == "await" || status == "resume") {
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
		return "resume", extras, true
	}
	return status, extras, true
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
	plan := make([]time.Duration, 0, len(implementationReviewPollPlan))
	for _, seconds := range implementationReviewPollPlan {
		plan = append(plan, time.Duration(seconds)*time.Second)
	}
	return plan
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

// observeLifecycle keeps a recoverable lifecycle decision in the foreground.
// The final configured interval repeats after the initial plan. A resume-worthy
// transition is returned to the caller so the existing bounded resume path can
// relaunch without consuming an agent retry.
func (s *runSession) observeLifecycle(ctx context.Context, workDir, branch, logPath, runID string, result AgentRunResult, extras map[string]any, hostPathsReady bool) (string, map[string]any, bool) {
	plan := s.lifecyclePollIntervals(extras)
	for index := 0; ; index++ {
		if deadline, gate, ok := lifecycleDeadline(extras); ok && !s.runtimeNow().Before(deadline) {
			resume := cloneLifecycleExtras(extras)
			resume["gate"] = gate
			resume["reason"] = lifecycleDeadlineReason(gate)
			resume["next_action"] = lifecycleDeadlineNextAction(gate)
			return "resume", resume, true
		}
		interval := plan[len(plan)-1]
		if index < len(plan) {
			interval = plan[index]
		}
		if err := s.waitForLifecyclePoll(ctx, interval); err != nil {
			if errors.Is(err, errLifecycleObservationTestStop) {
				return "await", extras, true
			}
			return "aborted", nil, true
		}
		status, nextExtras, handled := s.handleLifecycleDecisionForAttempt(ctx, workDir, branch, logPath, runID, hostPathsReady, result.Status)
		if !handled {
			if ctx.Err() != nil {
				return "aborted", nil, true
			}
			// The observer no longer has evidence for an active external
			// operation. Never turn an unhandled lookup/state into a synthetic
			// pending wait (issue #2743).
			status = "failure"
			nextExtras = lifecycleGateFailureEvidence(idleGateReason, idleGateNextAction, lifecycleGateNone, nil, "")
		}
		if status == "resume" {
			gate, _ := nextExtras["gate"].(string)
			if isResumeGate(gate) {
				return status, nextExtras, true
			}
			// An exhausted in-session resume budget ends the observation
			// for every gate, including CI remediation (issue #2743).
			return "failure", remediationBudgetFailureEvidence(gate, nextExtras,
				"inspect the current pull-request remediation evidence and start a new run after advancing the pull-request head"), true
		}
		if status == "await" {
			s.emitAwait(ctx, runID, result, nextExtras)
			continue
		}
		return status, nextExtras, true
	}
}

func lifecycleDeadline(extras map[string]any) (time.Time, string, bool) {
	var deadline time.Time
	var gate string
	request, ok := extras["review_request"].(map[string]any)
	resolved, _ := request["operation_resolved"].(bool)
	if ok && !resolved {
		if seconds, ok := lifecycleDeadlineSeconds(request["deadline_unix_seconds"]); ok {
			deadline, gate = time.Unix(seconds, 0), gateReviewTimeout
		}
	}
	ciWait, ok := extras["ci_wait"].(map[string]any)
	if ok {
		if seconds, ok := lifecycleDeadlineSeconds(ciWait["deadline_unix_seconds"]); ok {
			ciDeadline := time.Unix(seconds, 0)
			if deadline.IsZero() || ciDeadline.Before(deadline) {
				deadline, gate = ciDeadline, gateCIWaitTimeout
			}
		}
	}
	return deadline, gate, !deadline.IsZero()
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
