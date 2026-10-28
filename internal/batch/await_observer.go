package batch

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
)

// finishObserved applies an authoritative terminal observation without acquiring
// execution capacity or starting an agent/container. Cleanup uses the resolved
// sandbox policy; observation never re-arbitrates the selected lifecycle action.
func (e *runExecutor) finishObserved(ctx context.Context, row RowSpec, verdict lifecycleVerdict) AgentRunResult {
	return e.applyObserved(ctx, newRunSession(e, row), row, verdict)
}

// abortObserved applies an explicit abort decision through the same terminal
// application. The decision, not the context's state, selects run.aborted, so
// an abort requested by a phase that failed without cancellation is recorded
// the same way as one requested by cancellation. Extras carry the abort's
// attribution, such as a dependency cascade's aborted_by.
func (e *runExecutor) abortObserved(ctx context.Context, row RowSpec, extras map[string]any) AgentRunResult {
	session := newRunSession(e, row)
	session.explicitAbort = true
	return e.applyObserved(ctx, session, row, lifecycleVerdict{Action: lifecycleAborted, Evidence: extras})
}

func (e *runExecutor) applyObserved(ctx context.Context, session *runSession, row RowSpec, verdict lifecycleVerdict) AgentRunResult {
	branch := row.Branches[row.IssueNumber]
	factory := e.sbFactory
	if factory == nil {
		factory = defaultSandboxFactory{}
	}
	wt := factory.NewSandbox(session.repoRoot(), session.worktreeDir(), branch, row.BaseBranch, nil)
	result := AgentRunResult{IssueNumber: row.IssueNumber, Issue: issueRef(row.IssueNumber), Status: verdict.status(), Branch: branch}
	result.Status = session.finishDecidedTerminal(ctx, session.issueRunID(), result, verdict.Evidence, wt, branch)
	return result
}

// lifecycleObserverForRow prepares the observation module without acquiring an
// execution slot or starting a sandbox. A resolved gate can therefore be
// durably queued before the scheduler waits for capacity.
func (e *runExecutor) lifecycleObserverForRow(ctx context.Context, row RowSpec) (lifecycleObservation, lifecycleObservationRequest, bool) {
	if row.IssueNumber <= 0 || e.deps.githubClient == nil {
		return nil, lifecycleObservationRequest{}, false
	}
	session := newRunSession(e, row)
	branch := strings.TrimSpace(row.Branches[row.IssueNumber])
	if branch == "" {
		return nil, lifecycleObservationRequest{}, false
	}
	if issue, err := e.deps.githubClient.FetchIssue(ctx, row.IssueNumber); err == nil && issue != nil {
		session.issueState = issue.State
	}
	runID := row.RunID
	if runID == "" {
		runID = buildRunID(row.IssueNumber, row.RunTS, row.RunShortID)
	}
	workDir := filepath.Join(e.deps.layout.WorktreeDir, branch)
	return newLifecycleObserver(session), lifecycleObservationRequest{
		WorkDir: workDir, Branch: branch, LogPath: session.runLogPathFor(runID), RunID: runID,
		HostPathsReady: true, AttemptStatus: "success",
	}, true
}

func (e *runExecutor) observeRowLifecycle(ctx context.Context, row RowSpec) lifecycleVerdict {
	observer, request, ready := e.lifecycleObserverForRow(ctx, row)
	if !ready {
		return lifecycleVerdict{}
	}
	return observer.Observe(ctx, request)
}

// observeAwaitingRowLifecycle re-observes an already-admitted external wait.
// Re-observation is not an agent attempt: if CI has failed since the row
// yielded, implementation ownership resumes unless a previously selected,
// identity-valid workflow prerequisite still authorizes waiting.
func (e *runExecutor) observeAwaitingRowLifecycle(ctx context.Context, row RowSpec) lifecycleVerdict {
	observer, request, ready := e.lifecycleObserverForRow(ctx, row)
	if !ready {
		return lifecycleVerdict{}
	}
	return observer.Observe(ctx, request)
}

func (e *runExecutor) persistObservedAwait(ctx context.Context, row RowSpec, extras map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.deps.eventLog == nil {
		return nil
	}
	var previous map[string]any
	if states, err := events.ReadRunStates(e.deps.eventLog); err != nil {
		return err
	} else if state, ok := states[row.RunID]; ok && state.IsAwaiting() && !state.IsCapacityQueued() && state.AwaitEvent != nil {
		previous = state.AwaitEvent.Payload
		if !awaitOperationChanged(state.AwaitEvent.Payload, extras) {
			return nil
		}
	}
	payload := mergeRetainedAwaitEvidence(previous, extras)
	payload["await"], payload["branch"], payload["base_branch"], payload["batch_id"] = true, row.Branches[row.IssueNumber], row.BaseBranch, row.BatchID
	return e.deps.eventLog.Log(events.Event{Type: "run.await", Timestamp: newRunSession(e, row).runtimeNow(), RunID: row.RunID, Issue: row.IssueNumber, IssueRef: issueRef(row.IssueNumber), Payload: payload})
}

func mergeRetainedAwaitEvidence(previous, observed map[string]any) map[string]any {
	payload := cloneLifecycleExtras(observed)
	for _, operation := range []string{"ci_wait", "review_request"} {
		if _, ok := awaitIdentitySignature(payload, operation); ok {
			continue
		}
		value, ok := previous[operation].(map[string]any)
		if !ok {
			continue
		}
		if _, ok := awaitIdentitySignature(previous, operation); ok {
			payload[operation] = cloneLifecycleExtras(value)
		}
	}
	return payload
}

func awaitOperationChanged(previous, observed map[string]any) bool {
	identityBound := false
	for _, operation := range []string{"ci_wait", "review_request"} {
		previousSignature, previousOK := awaitIdentitySignature(previous, operation)
		observedSignature, observedOK := awaitIdentitySignature(observed, operation)
		if !previousOK && !observedOK {
			continue
		}
		identityBound = true
		if previousOK && !observedOK {
			continue
		}
		if previousSignature != observedSignature {
			return true
		}
	}
	if identityBound {
		return false
	}
	previousSignature, previousOK := awaitDeadlineSignature(previous)
	observedSignature, observedOK := awaitDeadlineSignature(observed)
	return previousOK != observedOK || previousSignature != observedSignature
}

func awaitIdentitySignature(extras map[string]any, operation string) (string, bool) {
	value, ok := extras[operation].(map[string]any)
	if !ok {
		return "", false
	}
	fields := []string{"pull_request", "head_sha", "execution_id", "trigger_id", "deadline_unix_seconds"}
	signature := make(map[string]any, len(fields))
	for _, key := range fields {
		if field, present := value[key]; present {
			signature[key] = field
		}
	}
	if _, ok := signature["deadline_unix_seconds"]; !ok {
		return "", false
	}
	data, err := json.Marshal(signature)
	return string(data), err == nil
}

func awaitDeadlineSignature(extras map[string]any) (string, bool) {
	deadline, gate, ok := lifecycleDeadline(extras)
	if !ok {
		return "", false
	}
	data, err := json.Marshal(map[string]any{"gate": gate, "deadline_unix_seconds": deadline.Unix()})
	return string(data), err == nil
}

func (s *runSession) issueRunID() string {
	if s.runID == "" {
		s.runID = buildRunID(s.issueNumber, s.runTS, s.runShortID)
	}
	return s.runID
}

func awaitPollInterval(opts runSessionOptions, poll int) time.Duration {
	if len(opts.lifecyclePollPlan) > 0 {
		return opts.lifecyclePollPlan[min(poll, len(opts.lifecyclePollPlan)-1)]
	}
	if poll < len(implementationReviewPollPlan) {
		return time.Duration(implementationReviewPollPlan[poll]) * time.Second
	}
	return time.Duration(implementationReviewPollPlan[len(implementationReviewPollPlan)-1]) * time.Second
}

func logCapacityQueuedContinuation(log events.EventLog, runID string, issue int, batchID string, row RowSpec, extras map[string]any, issueTitle string) error {
	return logCapacityQueuedContinuationAt(log, time.Now(), runID, issue, batchID, row, extras, issueTitle)
}

func logCapacityQueuedContinuationAt(log events.EventLog, timestamp time.Time, runID string, issue int, batchID string, row RowSpec, extras map[string]any, issueTitle string) error {
	if log == nil {
		return nil
	}
	branch := strings.TrimSpace(row.Branches[issue])
	previousRunID := strings.TrimSpace(row.PreviousRunIDs[issue])
	if previousRunID == "" {
		previousRunID = runID
	}
	previousBatchID := strings.TrimSpace(row.PreviousRunBatchIDs[issue])
	if previousBatchID == "" {
		previousBatchID = batchID
	}
	payload := map[string]any{
		"ready_continuation":    true,
		"branch":                branch,
		"base_branch":           row.BaseBranch,
		"batch_id":              batchID,
		"previous_run_id":       previousRunID,
		"previous_run_batch_id": previousBatchID,
		"reuse_session":         true,
		"issue_title":           issueTitle,
	}
	for _, key := range []string{"gate", "reason", "next_action", "review_request", "ci_wait", "pull_request", "head_sha", "usage_limit_probe", "usage_limit_waited_seconds", "usage_limit_deadline_unix_seconds"} {
		if value, ok := extras[key]; ok {
			payload[key] = value
		}
	}
	return log.Log(events.Event{
		Type:      "run.capacity_queued",
		Timestamp: timestamp,
		RunID:     runID,
		Issue:     issue,
		IssueRef:  issueRef(issue),
		Payload:   payload,
	})
}
