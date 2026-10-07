package batch

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
)

// finishObserved applies an authoritative terminal observation without acquiring
// execution capacity or starting an agent/container. Cleanup uses the resolved
// sandbox policy; observation never re-arbitrates the selected lifecycle action.
func (e *runExecutor) finishObserved(ctx context.Context, row RowSpec, status string, extras map[string]any) AgentRunResult {
	session := newRunSession(e, row)
	branch := row.Branches[row.IssueNumber]
	factory := e.sbFactory
	if factory == nil {
		factory = defaultSandboxFactory{}
	}
	wt := factory.NewSandbox(".", session.worktreeDir(), branch, row.BaseBranch, nil)
	result := AgentRunResult{IssueNumber: row.IssueNumber, Issue: issueRef(row.IssueNumber), Status: status, Branch: branch}
	result.Status = session.finishDecidedTerminal(ctx, session.issueRunID(), result, extras, wt, branch)
	return result
}

// observeLifecycle rechecks an awaiting continuation without acquiring an
// execution slot or starting a sandbox. A resolved gate can therefore be
// durably queued before the scheduler waits for capacity.
func (e *runExecutor) observeLifecycle(ctx context.Context, row RowSpec) (string, map[string]any, bool) {
	if row.IssueNumber <= 0 || e.deps.githubClient == nil {
		return "", nil, false
	}
	session := newRunSession(e, row)
	branch := strings.TrimSpace(row.Branches[row.IssueNumber])
	if branch == "" {
		return "", nil, false
	}
	if issue, err := e.deps.githubClient.FetchIssue(ctx, row.IssueNumber); err == nil && issue != nil {
		session.issueState = issue.State
	}
	runID := row.RunID
	if runID == "" {
		runID = buildRunID(row.IssueNumber, row.RunTS, row.RunShortID)
	}
	workDir := filepath.Join(e.deps.layout.WorktreeDir, branch)
	return session.handleLifecycleDecision(ctx, workDir, branch, session.runLogPathFor(runID), runID, true)
}

func (e *runExecutor) persistObservedAwait(ctx context.Context, row RowSpec, extras map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.deps.eventLog == nil {
		return nil
	}
	payload := cloneLifecycleExtras(extras)
	payload["await"], payload["branch"], payload["base_branch"], payload["batch_id"] = true, row.Branches[row.IssueNumber], row.BaseBranch, row.BatchID
	return e.deps.eventLog.Log(events.Event{Type: "run.await", Timestamp: newRunSession(e, row).runtimeNow(), RunID: row.RunID, Issue: row.IssueNumber, IssueRef: issueRef(row.IssueNumber), Payload: payload})
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
	for _, key := range []string{"gate", "reason", "next_action", "review_request", "ci_wait", "pull_request", "head_sha"} {
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
