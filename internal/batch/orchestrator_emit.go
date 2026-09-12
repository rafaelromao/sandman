package batch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

// emitAwait writes a non-terminal run.await event and returns the
// await status. Unlike emitTerminal, it does not mark the run as
// finished — the run stays active and can be resumed later.
func (s *runSession) emitAwait(ctx context.Context, runID string, result AgentRunResult, extras map[string]any) string {
	if s.deps.eventLog == nil {
		return "await"
	}
	event := events.Event{
		Type:      "run.await",
		Timestamp: time.Now(),
		RunID:     runID,
		Issue:     s.issueNumber,
		Payload: map[string]any{
			"await":         true,
			"branch":        result.Branch,
			"base_branch":   s.baseBranch,
			"retries_total": s.retries,
		},
	}
	if s.issueNumber > 0 {
		event.IssueRef = issueRef(s.issueNumber)
	}
	for k, v := range extras {
		event.Payload[k] = v
	}
	// The await reason mirrors the gate reason when the caller did not
	// provide an explicit await_reason, so RunState.AwaitReason() and
	// the run.await payload always agree.
	if _, ok := event.Payload["await_reason"]; !ok {
		if gate, ok := event.Payload["gate"].(string); ok && gate != "" {
			event.Payload["await_reason"] = gate
		}
	}
	_ = s.deps.eventLog.Log(event)
	return "await"
}

// emitTerminal writes the terminal run event (run.finished or run.aborted),
// rewrites the on-disk run.json snapshot so its status matches the terminal
// event, and returns the normalised status so the caller can use it without
// recomputing. Errors updating the snapshot are logged but do not change the
// run outcome. The event-log write is skipped when the orchestrator has no
// event log.
//
// extras carries extra run.finished payload keys that the caller wants to
// merge into the event (e.g. "blocker", "pr_number", "merge_conflict" — see
// issue #1684). Passing nil is fine; the standard payload keys
// ("status", "branch", "base_branch", "retries_total", etc.) are always set
// by this function.
//
// Before normalising the terminal event, emitTerminal performs a defensive
// post-check: if the agent's branch has an open PR whose mergeable state is
// `CONFLICTING`, the terminal event payload carries `merge_conflict: true`
// and the PR number. The result is reclassified as a lifecycle failure.
func (s *runSession) normalizeTerminalResult(result AgentRunResult, extras map[string]any) (AgentRunResult, map[string]any) {
	if conflictExtras, ok := s.detectConflictingPR(result.Branch); ok {
		result.Status = "failure"
		if extras == nil {
			extras = map[string]any{}
		}
		for k, v := range conflictExtras {
			extras[k] = v
		}
	}
	return result, extras
}

// emitTerminal writes a terminal event without changing the sandbox. Callers
// that own a sandbox should use finishTerminal so the event reflects cleanup.
func (s *runSession) emitTerminal(ctx context.Context, runID string, result AgentRunResult, extras map[string]any) string {
	result, extras = s.normalizeTerminalResult(result, extras)
	return s.emitNormalizedTerminal(ctx, runID, result, extras)
}

func (s *runSession) emitNormalizedTerminal(ctx context.Context, runID string, result AgentRunResult, extras map[string]any) string {
	terminalEventType, terminalStatus := terminalRunEvent(ctx, result.Status)
	s.updateRunManifestStatus(runID, batchindex.RunManifestStatus(terminalStatus))
	if s.deps.eventLog == nil {
		return terminalStatus
	}
	retriesDone := result.RetriesTotal - 1
	if retriesDone < 0 {
		retriesDone = 0
	}
	worktreeState := "preserved"
	if state, ok := extras["worktree_state"].(string); ok && state != "" {
		worktreeState = state
	}
	event := events.Event{
		Type:      terminalEventType,
		Timestamp: time.Now(),
		RunID:     runID,
		Issue:     s.issueNumber,
		Payload: map[string]any{
			"status":         terminalStatus,
			"branch":         result.Branch,
			"base_branch":    s.baseBranch,
			"worktree_state": worktreeState,
			"retries_total":  s.retries,
			"retries_done":   retriesDone,
		},
	}
	if s.issueNumber > 0 {
		event.IssueRef = issueRef(s.issueNumber)
	}
	if s.review {
		event.Payload["review"] = true
		event.Payload["pr_number"] = s.prNumber
		event.Payload["review_focus"] = s.reviewFocus
		if s.issueNumber > 0 {
			event.Payload["issue_number"] = s.issueNumber
		}
	}
	for k, v := range extras {
		event.Payload[k] = v
	}
	_ = s.deps.eventLog.Log(event)
	return terminalStatus
}

// finishTerminal cleans successful runs before recording their terminal event
// so worktree_state describes the actual on-disk result.
func (s *runSession) finishTerminal(ctx context.Context, runID string, result AgentRunResult, extras map[string]any, wt sandbox.Sandbox, branch string) string {
	result, extras = s.normalizeTerminalResult(result, extras)
	_, terminalStatus := terminalRunEvent(ctx, result.Status)
	worktreeState := "preserved"
	if terminalStatus == "success" && !s.review && (s.cfg == nil || s.cfg.EffectiveCleanupWorktrees()) {
		restoreErr := wt.RestoreHostPaths()
		if restoreErr != nil && s.deps.errorLog != nil {
			fmt.Fprintf(s.deps.errorLog, "warning: restore host paths for succeeded run %d: %v\n", s.issueNumber, restoreErr)
		}
		stopErr := wt.Stop()
		if stopErr != nil && s.deps.errorLog != nil {
			fmt.Fprintf(s.deps.errorLog, "warning: auto-clean worktree %s for succeeded run %d: %v\n", branch, s.issueNumber, stopErr)
		}
		worktreeRemoved := false
		if workDir := wt.WorkDir(); workDir != "" {
			_, statErr := os.Stat(workDir)
			worktreeRemoved = os.IsNotExist(statErr)
		}
		if (restoreErr == nil && stopErr == nil) || worktreeRemoved {
			worktreeState = "cleaned"
		}
		if cleanupErr := errors.Join(restoreErr, stopErr); cleanupErr != nil {
			if extras == nil {
				extras = make(map[string]any)
			}
			extras["cleanup_error"] = cleanupErr.Error()
		}
	}
	if extras == nil {
		extras = make(map[string]any)
	}
	extras["worktree_state"] = worktreeState
	return s.emitNormalizedTerminal(ctx, runID, result, extras)
}

// emitEarlyFailure logs a terminal run.finished event (status "failure") for
// an early-return path in execute() that exits before run.started is emitted.
// Without this event, the portal keeps the run in its last projected state
// (typically "queued"), and dependent runs see a silently-set failure status
// with no corresponding event in the log. See issue #2136.
//
// The error detail is also written to errorLog (stderr) by the caller; this
// event additionally persists the underlying error in the payload under
// `error_message` so the operator can read it back from .sandman/events.jsonl
// after the fact. Without this, the only place the underlying message
// survives is the live stderr at run time — a regression that bit run
// 260721104202-24a8-2316 (and its sibling rows 2317/2318/2319 in batch
// 260721104202-24a8-2318+11), where `wt.Start` failed mid-batch and the
// operator had no on-disk breadcrumb to diagnose why.
//
// This does NOT emit run.started — the run never started the agent — so
// ProjectRunStates folds run.finished directly, transitioning the run from
// "queued" to a terminal "failure" status. Safe to call when no run manifest
// exists yet (the manifest is written later in execute()).
//
// reason should be a short diagnostic string identifying the failure point
// (e.g. "fetch issue", "start sandbox"). underlyingErr, when non-nil, is
// preserved verbatim under `error_message` so the operator can recover the
// actionable diagnostic from the persisted event log.
func (s *runSession) emitEarlyFailure(reason, branch string, underlyingErr error) {
	if s.deps.eventLog == nil {
		return
	}
	runID := buildRunID(s.issueNumber, s.runTS, s.runShortID)
	payload := map[string]any{
		"status":        "failure",
		"branch":        branch,
		"base_branch":   s.baseBranch,
		"retries_total": s.retries,
		"retries_done":  0,
		"early_failure": true,
		"error":         reason,
	}
	if underlyingErr != nil {
		payload["error_message"] = underlyingErr.Error()
	}
	_ = s.deps.eventLog.Log(events.Event{
		Type:      "run.finished",
		Timestamp: time.Now(),
		RunID:     runID,
		Issue:     s.issueNumber,
		IssueRef:  issueRef(s.issueNumber),
		Payload:   payload,
	})
}

// detectConflictingPR inspects the branch's open PR and, when its mergeable
// state is `CONFLICTING`, returns a payload extras map with
// `merge_conflict: true` and `pr_number` set, plus a `true` ok flag.
//
// Errors from the underlying `gh pr list` lookup are logged to `errorLog`
// but treated as a soft pass-through: a transient gh failure must not
// silently flip a real success into a fake failure. See issue #1684.
func (s *runSession) detectConflictingPR(branch string) (map[string]any, bool) {
	if strings.TrimSpace(branch) == "" {
		return nil, false
	}
	exists, prNumber, mergeable, err := LookupOpenPR(branch)
	if err != nil {
		fmt.Fprintf(s.deps.errorLog, "warning: lookup open PR for branch %q: %v\n", branch, err)
		return nil, false
	}
	if !exists {
		return nil, false
	}
	if strings.EqualFold(mergeable, "CONFLICTING") {
		fmt.Fprintf(s.deps.errorLog, "error: branch %q has CONFLICTING open PR #%d\n", branch, prNumber)
		return map[string]any{"merge_conflict": true, "pr_number": prNumber}, true
	}
	return nil, false
}

// updateRunManifestStatus rewrites the run.json snapshot with the terminal
// status. Failures are logged to errorLog and ignored; the event log remains
// authoritative.
func (s *runSession) updateRunManifestStatus(runID string, status batchindex.RunManifestStatus) {
	batchDir := s.deps.layout.BatchDir(s.batchID)
	if s.batchID == "" {
		batchDir = s.deps.layout.BatchesDir
	}
	if err := daemon.UpdateRunManifestStatus(batchDir, runID, status); err != nil {
		fmt.Fprintf(s.deps.errorLog, "error: update run manifest status for run %s: %v\n", runID, err)
	}
}

func terminalRunEvent(ctx context.Context, status string) (string, string) {
	eventType := "run.finished"
	terminalStatus := status
	terminalCode := events.RunStatusFromPayload(terminalStatus)
	if terminalCode.String() == "" {
		terminalStatus = "failure"
		terminalCode = events.RunStatusFailure
	}
	if ctx.Err() != nil && !terminalCode.IsSuccess() {
		eventType = "run.aborted"
		terminalStatus = "aborted"
	}
	return eventType, terminalStatus
}
