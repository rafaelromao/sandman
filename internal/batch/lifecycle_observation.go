package batch

import (
	"context"
	"errors"
	"strings"
)

// lifecycleObservationRequest describes one runtime observation of an
// implementation pull request.
type lifecycleObservationRequest struct {
	WorkDir           string
	Branch            string
	RunID             string
	HostPathsReady    bool
	AttemptStatus     string
	AfterAgentAttempt bool
	CIRerunAction     *ciRerunActionEvidence
}

// lifecycleVerdict is the selected lifecycle action and the evidence used to
// apply it. lifecycleNone means the observation did not select an action.
type lifecycleVerdict struct {
	Action           lifecycleAction
	Evidence         map[string]any
	Kind             lifecycleVerdictKind
	ImplementorWork  bool
	ciRepairRequired bool
	ciExecutionID    string
}

type lifecycleVerdictKind uint8

const (
	lifecycleVerdictOrdinary lifecycleVerdictKind = iota
	lifecycleVerdictCompletion
	lifecycleVerdictClosedPR
	// lifecycleVerdictGitHubUnavailable is a wait for GitHub to recover from
	// a failed pull-request lookup or refresh (issue #2938).
	lifecycleVerdictGitHubUnavailable
)

// lifecycleObservation is the single operation callers use to observe an
// implementation pull request. The implementation owns evidence gathering,
// lifecycle selection, and post-decision policy.
type lifecycleObservation interface {
	Observe(context.Context, lifecycleObservationRequest) lifecycleVerdict
}

type lifecycleObserver struct {
	*runSession
}

func newLifecycleObserver(session *runSession) *lifecycleObserver {
	return &lifecycleObserver{runSession: session}
}

func (o *lifecycleObserver) Observe(ctx context.Context, request lifecycleObservationRequest) lifecycleVerdict {
	recovery, recoveryBase, recovering, recoveryUnknown := o.githubRecoveryWindow(request.RunID)
	if recovering {
		if verdict, handled := o.observeGitHubRecovery(ctx, request, &recovery, recoveryBase); handled {
			return verdict
		}
	}
	verdict := o.decideLifecycleForAttempt(
		ctx,
		request.WorkDir,
		request.Branch,
		request.RunID,
		request.HostPathsReady,
		request.AttemptStatus,
		request.AfterAgentAttempt,
		request.CIRerunAction,
	)
	if verdict.Kind == lifecycleVerdictGitHubUnavailable && ctx.Err() == nil {
		return o.recordGitHubLookupFailure(request, verdict, recovery, recovering, recoveryUnknown)
	}
	if !verdict.handled() || ctx.Err() != nil {
		return verdict
	}
	verdict.ImplementorWork = isImplementorOwnedGateFailure(verdict.Evidence)
	return o.applyContinuity(request.Branch, request.RunID, verdict)
}

// waitForTransition keeps a recoverable verdict in the foreground. Deadline
// expiry is a typed resume decision and goes through the same continuity
// override as every other observed action.
func (o *lifecycleObserver) waitForTransition(ctx context.Context, request lifecycleObservationRequest, result AgentRunResult, initial lifecycleVerdict) lifecycleVerdict {
	verdict := initial
	plan := o.lifecyclePollIntervals(verdict.Evidence)
	for index := 0; ; index++ {
		_, githubWait := githubRecoveryNextAttempt(verdict.Evidence)
		if deadline, gate, ok := lifecycleDeadline(verdict.Evidence); ok && !githubWait && !o.runtimeNow().Before(deadline) {
			evidence := cloneLifecycleExtras(verdict.Evidence)
			evidence["gate"] = gate
			evidence["reason"] = lifecycleDeadlineReason(gate)
			evidence["next_action"] = lifecycleDeadlineNextAction(gate)
			deadlineVerdict := lifecycleVerdict{Action: lifecycleResume, Evidence: evidence}
			deadlineVerdict.ImplementorWork = isImplementorOwnedGateFailure(evidence)
			deadlineVerdict = o.applyContinuity(request.Branch, request.RunID, deadlineVerdict)
			if deadlineVerdict.Action != lifecycleAwait {
				return deadlineVerdict
			}
			// A stalled or exhausted remediation keeps observing the gate
			// without the expired deadline.
			verdict = deadlineVerdict
			delete(verdict.Evidence, "review_request")
			delete(verdict.Evidence, "ci_wait")
		}
		interval := plan[len(plan)-1]
		if index < len(plan) {
			interval = plan[index]
		}
		if next, ok := githubRecoveryNextAttempt(verdict.Evidence); ok {
			// Sleep until the next hourly recovery attempt.
			interval = max(next.Sub(o.runtimeNow()), 0)
		}
		if err := o.waitForLifecyclePoll(ctx, interval); err != nil {
			if errors.Is(err, errLifecycleObservationTestStop) {
				verdict.Action = lifecycleAwait
				return verdict
			}
			return lifecycleVerdict{Action: lifecycleAborted}
		}
		verdict = o.Observe(ctx, request)
		if !verdict.handled() {
			if ctx.Err() != nil {
				return lifecycleVerdict{Action: lifecycleAborted}
			}
			verdict = failureForUnhandledObservation(verdict)
		}
		if verdict.Action == lifecycleAwait {
			o.emitAwait(ctx, request.RunID, result, verdict.Evidence)
			continue
		}
		return verdict
	}
}

func isImplementorOwnedGateFailure(evidence map[string]any) bool {
	reason, _ := evidence["reason"].(string)
	return reason == idleGateReason || reason == missingPRReason || reason == "REVIEW_REQUEST_REQUIRED"
}

func (o *lifecycleObserver) applyContinuity(branch, runID string, verdict lifecycleVerdict) lifecycleVerdict {
	if verdict.Action != lifecycleResume && verdict.Action != lifecycleAwait {
		return verdict
	}
	stalled, readErr := latestStalledResumes(o.deps.eventLog, runID)
	if verdict.Action == lifecycleAwait {
		if len(stalled) == 0 {
			return verdict
		}
		held := cloneLifecycleExtras(verdict.Evidence)
		held[stalledResumesKey] = appendStalledResume(stalled[:len(stalled)-1], stalled[len(stalled)-1])
		verdict.Evidence = held
		return verdict
	}

	signatureEvidence := verdict.Evidence
	if verdict.ImplementorWork {
		signatureEvidence = cloneLifecycleExtras(verdict.Evidence)
		signatureEvidence["gate"] = gateOwnedWorkRequired
	} else if verdict.ciRepairRequired && stringEvidence(verdict.Evidence, "reason") == "CI_FAILURE" {
		signatureEvidence = cloneLifecycleExtras(verdict.Evidence)
		signatureEvidence["gate"] = "ci-failure"
		if verdict.ciExecutionID != "" {
			signatureEvidence["ci_execution_id"] = verdict.ciExecutionID
		} else {
			delete(signatureEvidence, "ci_execution_id")
		}
	}
	signature := resumeSignature(signatureEvidence, o.decisionPRHead)
	allowanceRound := repairRound(verdict.Evidence, o.decisionPRHead)
	o.pendingResumeSignature = signature
	o.pendingResumeRound = allowanceRound
	if readErr != nil {
		prior := stalledResumes(verdict.Evidence)
		if verdict.ImplementorWork {
			verdict.Action = lifecycleAwait
			verdict.Evidence = o.ownedWorkHold(verdict.Evidence, signature, prior)
		} else {
			verdict.Action = lifecycleAwait
			verdict.Evidence = stalledAwaitExtras(verdict.Evidence, signature, prior)
		}
		return verdict
	}
	if verdict.ImplementorWork {
		if containsSignature(stalled, signature) ||
			(o.finalOwnedWorkAttempt && strings.TrimSpace(o.renderCfg.ReviewCommand) == "" && idleGateAwaitsReview(verdict.Evidence)) {
			verdict.Action = lifecycleAwait
			verdict.Evidence = o.ownedWorkHold(verdict.Evidence, signature, stalled)
		}
		return verdict
	}
	if containsSignature(stalled, signature) {
		verdict.Action = lifecycleAwait
		verdict.Evidence = stalledAwaitExtras(verdict.Evidence, signature, stalled)
		return verdict
	}
	if o.lastResumeRound != "" && allowanceRound != o.lastResumeRound {
		o.resumeCount = 0
	}
	gate, _ := verdict.Evidence["gate"].(string)
	repeated := signature == o.lastResumeSignature && stallsOnRepeat(verdict.Evidence)
	if !repeated && o.resumeCount < o.resumeCapFor() {
		return verdict
	}
	if formalChangesRequested(verdict.Evidence) || exhaustionTerminal(verdict.Evidence) {
		verdict.Action = lifecycleFailure
		verdict.Evidence = remediationBudgetFailureEvidence(gate, verdict.Evidence,
			"inspect the current pull-request remediation evidence and continue in a fresh session")
		verdict.ImplementorWork = false
		return verdict
	}
	verdict.Action = lifecycleAwait
	verdict.Evidence = stalledAwaitExtras(verdict.Evidence, signature, stalled)
	return verdict
}

func stringEvidence(evidence map[string]any, key string) string {
	value, _ := evidence[key].(string)
	return value
}

func (o *lifecycleObserver) resumeUnavailable(ctx context.Context, verdict lifecycleVerdict, nextAction string) lifecycleVerdict {
	if ctx.Err() != nil {
		return lifecycleVerdict{Action: lifecycleAborted}
	}
	if verdict.Action != lifecycleResume {
		return verdict
	}
	return lifecycleVerdict{
		Action: lifecycleFailure,
		Evidence: remediationBudgetFailureEvidence(
			stringEvidence(verdict.Evidence, "gate"),
			verdict.Evidence,
			nextAction,
		),
	}
}

func (o *lifecycleObserver) failUnperformedResume(verdict lifecycleVerdict, reason, nextAction string) lifecycleVerdict {
	if verdict.Action != lifecycleResume {
		return verdict
	}
	return lifecycleVerdict{
		Action:   lifecycleFailure,
		Evidence: lifecycleGateFailureEvidence(reason, nextAction, lifecycleGateNone, nil, ""),
	}
}

func (v lifecycleVerdict) status() string {
	switch v.Action {
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

func (v lifecycleVerdict) handled() bool { return v.Action != lifecycleNone }

func (v lifecycleVerdict) terminal() bool {
	switch v.Action {
	case lifecycleSuccess, lifecycleFailure, lifecycleAborted:
		return true
	default:
		return false
	}
}

func (v lifecycleVerdict) supersedesQuotaProbe() bool {
	return v.Action == lifecycleSuccess || v.Kind == lifecycleVerdictCompletion || v.Kind == lifecycleVerdictClosedPR
}

func failureForUnhandledObservation(verdict lifecycleVerdict) lifecycleVerdict {
	if verdict.Action != lifecycleNone {
		return verdict
	}
	return lifecycleVerdict{
		Action: lifecycleFailure,
		Evidence: lifecycleGateFailureEvidence(
			idleGateReason,
			idleGateNextAction,
			lifecycleGateNone,
			nil,
			"",
		),
	}
}
