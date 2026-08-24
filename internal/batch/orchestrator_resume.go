package batch

import (
	"context"
	"path/filepath"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

// defaultAwaitResumeMax bounds in-session agent relaunches per session when
// runSessionOptions.awaitResumeMax is zero (the production default).
const defaultAwaitResumeMax = 3

// Entry re-evaluation machinery for resuming agent work on PR lifecycle
// transitions (issue #2595). A session that re-enters a run whose PR gate is
// already resolvable must not launch the agent blindly: a merely pending gate
// emits run.await and ends without launching, while a ready-to-merge or
// actionable-feedback gate relaunches the agent with the request-scoped
// evidence (the entry launch IS the resume).

// isEntryResumeCandidate reports whether the session should re-evaluate the
// external gate before its first agent launch. Continuation sessions always
// qualify; fresh sessions qualify when the worktree still carries retained
// review artifacts from an earlier run.
func (s *runSession) isEntryResumeCandidate(workDir string) bool {
	if s.mode == ModeContinue {
		return true
	}
	return reviewTimeoutArtifactsPresent(workDir)
}

// resumeEvidenceFor builds the request-scoped evidence envelope handed to a
// resumed agent session. extras carries the gate outcome (gate, reason,
// next_action, and any retained review_request); the live PR supplies the
// current pull-request facts when the gate outcome did not already carry
// them.
func (s *runSession) resumeEvidenceFor(ctx context.Context, branch string, extras map[string]any) map[string]any {
	evidence := map[string]any{}
	if extras != nil {
		for k, v := range extras {
			evidence[k] = v
		}
	}
	switch gate := evidence["gate"]; gate {
	case gateReadyToMerge:
		setDefaultEvidence(evidence, "outcome", gateReadyToMerge)
		setDefaultEvidence(evidence, "reason", "REVIEW_APPROVED")
		setDefaultEvidence(evidence, "next_action", "revalidate current-head approval, CI, and mergeability, then execute the normal pull-request merge gate")
	case gateActionableFeedback:
		setDefaultEvidence(evidence, "outcome", gateActionableFeedback)
		setDefaultEvidence(evidence, "reason", actionableFeedbackReason)
		setDefaultEvidence(evidence, "next_action", actionableFeedbackNextAction)
	}
	if s.deps.githubClient != nil {
		if pr, err := s.deps.githubClient.FindPRByBranch(ctx, branch); err == nil && pr != nil {
			if _, ok := evidence["pull_request"]; !ok {
				evidence["pull_request"] = pr.Number
			}
			if _, ok := evidence["head_sha"]; !ok {
				evidence["head_sha"] = pr.HeadRefOid
			}
		}
	}
	return evidence
}

func setDefaultEvidence(evidence map[string]any, key string, value any) {
	if _, ok := evidence[key]; !ok {
		evidence[key] = value
	}
}

// resumePromptFor composes the attempt prompt for a resumed session: the
// preserved task content with the request-scoped evidence attached, wrapped
// in the canonical continuation prompt. Without evidence the output is
// byte-identical to today's continuation.
func (s *runSession) resumePromptFor(taskContent string, evidence map[string]any, reviewTimeout int) string {
	return prompt.ContinuationTaskPromptWithReviewTimeout(prompt.TaskWithReviewEvidence(taskContent, evidence), reviewTimeout)
}

// tryEntryResume re-evaluates the external gate at session entry for resume
// candidates. It returns (result, started, handled): handled is true only
// when the session ended at entry (immediate run.await without launching);
// when the gate is resume-worthy, the entry launch prompt is prepared and
// handled is false so the caller proceeds with the normal launch flow.
func (s *runSession) tryEntryResume(ctx context.Context, branch string, wt sandbox.Sandbox, logPath, runID string) (AgentRunResult, bool, bool) {
	if !s.isEntryResumeCandidate(wt.WorkDir()) || s.deps.githubClient == nil {
		return AgentRunResult{}, false, false
	}
	hostPathsReady := s.restoreHostPathsBeforeExternalGate(wt)
	gateStatus, extras, handled := s.handleLifecycleDecision(ctx, wt.WorkDir(), branch, logPath, runID, hostPathsReady)
	if !handled {
		return AgentRunResult{}, false, false
	}
	if gateStatus == "success" || gateStatus == "failure" || gateStatus == "aborted" {
		result := AgentRunResult{
			IssueNumber:  s.issueNumber,
			Issue:        issueRef(s.issueNumber),
			Status:       gateStatus,
			Branch:       branch,
			RetriesTotal: 1,
		}
		result.Status = s.emitTerminal(ctx, runID, result, extras)
		return result, true, true
	}
	if gateStatus != "await" && gateStatus != "resume" {
		return AgentRunResult{}, false, false
	}
	gate, _ := extras["gate"].(string)
	if !isResumeGate(gate) {
		result := AgentRunResult{IssueNumber: s.issueNumber, Issue: issueRef(s.issueNumber), Status: "await", Branch: branch, RetriesTotal: 1}
		if !s.opts.foregroundLifecycle {
			result.Status = s.emitAwait(ctx, runID, result, extras)
			return result, true, true
		}
		s.emitAwait(ctx, runID, result, extras)
		gateStatus, nextExtras, handled := s.observeLifecycle(ctx, wt.WorkDir(), branch, logPath, runID, result, extras, hostPathsReady)
		if !handled {
			return AgentRunResult{}, false, false
		}
		if gateStatus == "await" || gateStatus == "resume" {
			gate, _ = nextExtras["gate"].(string)
			if isResumeGate(gate) {
				evidence := s.resumeEvidenceFor(ctx, branch, nextExtras)
				taskContent, _, _ := ReadTaskContent(filepath.Join(wt.WorkDir(), ".sandman", "task.md"))
				s.renderCfg.TaskPrompt = s.resumePromptFor(taskContent, evidence, s.renderCfg.ReviewTimeout)
				return AgentRunResult{}, false, false
			}
		}
		result.Status = gateStatus
		if gateStatus == "await" {
			return result, true, true
		}
		result.Status = s.emitTerminal(ctx, runID, result, nextExtras)
		return result, true, true
	}
	if isCIRemediationGate(gate) && !consumeCIWaitRemediation(wt.WorkDir(), extras) {
		result := AgentRunResult{IssueNumber: s.issueNumber, Issue: issueRef(s.issueNumber), Status: "failure", Branch: branch, RetriesTotal: 1}
		result.Status = s.emitTerminal(ctx, runID, result, map[string]any{
			"gate":        extras["gate"],
			"reason":      "REMEDIATION_BUDGET_EXHAUSTED",
			"next_action": "advance the pull-request head before requesting another remediation run",
		})
		return result, true, true
	}
	evidence := s.resumeEvidenceFor(ctx, branch, extras)
	taskContent, _, _ := ReadTaskContent(filepath.Join(wt.WorkDir(), ".sandman", "task.md"))
	s.renderCfg.TaskPrompt = s.resumePromptFor(taskContent, evidence, s.renderCfg.ReviewTimeout)
	return AgentRunResult{}, false, false
}

// resumeCapFor returns the per-session in-session resume cap, defaulting to
// defaultAwaitResumeMax when the session option is unset.
func (s *runSession) resumeCapFor() int {
	if s.opts.awaitResumeMax > 0 {
		return s.opts.awaitResumeMax
	}
	return defaultAwaitResumeMax
}

// resumePromptFromGate is the in-session counterpart of tryEntryResume: after
// the agent completes cleanly and the external gate turns resume-worthy
// (ready-to-merge or actionable-feedback), it emits run.resumed and returns
// the continuation prompt carrying the request-scoped review evidence. The
// returned bool reports whether a relaunch should happen; callers leave the
// gate-handling (await / terminal) untouched when it is false — in
// particular, when the per-session resume cap is exhausted the gate falls
// back to run.await.
func (s *runSession) resumePromptFromGate(ctx context.Context, wt sandbox.Sandbox, branch, runID string, extras map[string]any) (string, bool) {
	if s.deps.githubClient == nil || s.resumeCount >= s.resumeCapFor() {
		return "", false
	}
	gate, _ := extras["gate"].(string)
	if !isResumeGate(gate) {
		return "", false
	}
	if isCIRemediationGate(gate) && !consumeCIWaitRemediation(wt.WorkDir(), extras) {
		return "", false
	}
	evidence := s.resumeEvidenceFor(ctx, branch, extras)
	taskContent, _, _ := ReadTaskContent(filepath.Join(wt.WorkDir(), ".sandman", "task.md"))
	s.resumeCount++
	s.emitResume(ctx, runID, branch, gate, evidence)
	return s.resumePromptFor(taskContent, evidence, s.renderCfg.ReviewTimeout), true
}

func isResumeGate(gate string) bool {
	switch gate {
	case gateReadyToMerge, gateActionableFeedback, gateReviewTimeout, gateCIWaitTimeout, "ci-failure", "merge-conflict":
		return true
	default:
		return false
	}
}

func isCIRemediationGate(gate string) bool {
	switch gate {
	case gateCIWaitTimeout, "ci-failure", "merge-conflict":
		return true
	default:
		return false
	}
}

// emitResume writes the run.resumed event (issue #2595). It records the
// resume trigger (reason + gate), the run coordinates, and the retained
// review_request so the projection and operators can attribute the relaunch.
func (s *runSession) emitResume(ctx context.Context, runID, branch, gate string, evidence map[string]any) {
	if s.deps.eventLog == nil {
		return
	}
	reason := "feedback"
	if gate == gateReadyToMerge {
		reason = "approval"
	}
	event := events.Event{
		Type:      "run.resumed",
		Timestamp: time.Now(),
		RunID:     runID,
		Issue:     s.issueNumber,
		Payload: map[string]any{
			"reason":        reason,
			"gate":          gate,
			"branch":        branch,
			"base_branch":   s.baseBranch,
			"retries_total": s.retries,
			"run_id":        runID,
		},
	}
	if s.issueNumber > 0 {
		event.IssueRef = issueRef(s.issueNumber)
	}
	if reviewRequest, ok := evidence["review_request"]; ok {
		event.Payload["review_request"] = reviewRequest
	}
	_ = s.deps.eventLog.Log(event)
}
