package batch

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
)

// ReadyContinuation is the durable, event-derived input needed to re-enter a
// continuation that had resolved its external gate but had not yet acquired a
// scheduler slot when the owning process stopped.
type ReadyContinuation struct {
	IssueNumber        int
	RunID              string
	PreviousRunID      string
	PreviousRunBatchID string
	BatchID            string
	Branch             string
	BaseBranch         string
	IssueTitle         string
}

// FindReadyContinuations returns only the latest per-issue lifecycle states
// that are durably capacity-queued and whose original batch no longer has a
// live owner. A later lifecycle event or a live batch always takes precedence.
func FindReadyContinuations(eventLog []events.Event, layout paths.Layout) []ReadyContinuation {
	states := events.ProjectRunStates(eventLog)
	latestRunByIssue := make(map[int]string)
	for _, event := range eventLog {
		if event.Issue <= 0 || event.RunID == "" || !changesRunLifecycle(event.Type) {
			continue
		}
		latestRunByIssue[event.Issue] = event.RunID
	}

	ready := make([]ReadyContinuation, 0)
	for _, state := range states {
		issue := state.IssueNumber()
		if issue <= 0 || latestRunByIssue[issue] != state.RunID || !state.IsCapacityQueued() {
			continue
		}
		event := state.CapacityQueuedEvent
		if event == nil || !payloadBoolValue(event.Payload, "ready_continuation") {
			continue
		}
		batchID := strings.TrimSpace(payloadStringValue(event.Payload, "batch_id"))
		if batchID == "" {
			batchID = state.BatchID()
		}
		if batchID != "" && daemon.IsRunActive(daemon.BatchDir(layout.SandmanDir, batchID)) {
			continue
		}
		previousRunID := strings.TrimSpace(payloadStringValue(event.Payload, "previous_run_id"))
		if previousRunID == "" {
			previousRunID = state.RunID
		}
		previousBatchID := strings.TrimSpace(payloadStringValue(event.Payload, "previous_run_batch_id"))
		if previousBatchID == "" {
			previousBatchID = batchID
		}
		ready = append(ready, ReadyContinuation{
			IssueNumber:        issue,
			RunID:              state.RunID,
			PreviousRunID:      previousRunID,
			PreviousRunBatchID: previousBatchID,
			BatchID:            batchID,
			Branch:             strings.TrimSpace(payloadStringValue(event.Payload, "branch")),
			BaseBranch:         strings.TrimSpace(payloadStringValue(event.Payload, "base_branch")),
			IssueTitle:         strings.TrimSpace(payloadStringValue(event.Payload, "issue_title")),
		})
	}
	return ready
}

// ApplyReadyContinuations adds restart-safe continuations to a request. The
// worktree Task is the durable prompt source; live PR/review facts are
// revalidated by the ordinary continuation lifecycle before agent execution.
func ApplyReadyContinuations(req *Request, ready []ReadyContinuation, layout paths.Layout, reviewTimeout int) error {
	if req == nil {
		return fmt.Errorf("ready continuation request is nil")
	}
	for _, continuation := range ready {
		if continuation.IssueNumber <= 0 {
			return fmt.Errorf("ready continuation issue is invalid")
		}
		if req.IssueMode(continuation.IssueNumber) == ModeOverride {
			continue
		}
		if continuation.RunID == "" || continuation.PreviousRunID == "" || continuation.PreviousRunBatchID == "" || continuation.Branch == "" || continuation.BaseBranch == "" {
			return fmt.Errorf("ready continuation for issue %d has incomplete identity", continuation.IssueNumber)
		}

		worktreePath := filepath.Join(layout.WorktreeDir, continuation.Branch)
		taskPath := filepath.Join(worktreePath, ".sandman", "task.md")
		taskContent, taskExists, err := ReadTaskContent(taskPath)
		if err != nil {
			return fmt.Errorf("read ready continuation Task for issue %d: %w", continuation.IssueNumber, err)
		}
		if !taskExists {
			return fmt.Errorf("ready continuation Task %q is missing", taskPath)
		}

		if !requestContainsIssue(req.Issues, continuation.IssueNumber) {
			req.Issues = append(req.Issues, continuation.IssueNumber)
		}
		if req.Mode == nil {
			req.Mode = make(map[int]IssueMode)
		}
		if req.PreviousRunIDs == nil {
			req.PreviousRunIDs = make(map[int]string)
		}
		if req.PreviousRunBatchIDs == nil {
			req.PreviousRunBatchIDs = make(map[int]string)
		}
		if req.RunIDs == nil {
			req.RunIDs = make(map[int]string)
		}
		if req.ReadyContinuations == nil {
			req.ReadyContinuations = make(map[int]bool)
		}
		if req.ReuseSession == nil {
			req.ReuseSession = make(map[int]bool)
		}
		if req.Branches == nil {
			req.Branches = make(map[int]string)
		}
		if req.BaseBranches == nil {
			req.BaseBranches = make(map[int]string)
		}
		if req.TaskPrompts == nil {
			req.TaskPrompts = make(map[int]string)
		}
		if req.IssueTitles == nil {
			req.IssueTitles = make(map[int]string)
		}

		issue := continuation.IssueNumber
		req.Mode[issue] = ModeContinue
		req.PreviousRunIDs[issue] = continuation.PreviousRunID
		req.PreviousRunBatchIDs[issue] = continuation.PreviousRunBatchID
		req.RunIDs[issue] = continuation.RunID
		req.ReadyContinuations[issue] = true
		req.ReuseSession[issue] = true
		req.Branches[issue] = continuation.Branch
		req.BaseBranches[issue] = continuation.BaseBranch
		req.TaskPrompts[issue] = prompt.ContinuationTaskPromptWithReviewTimeout(taskContent, reviewTimeout)
		if continuation.IssueTitle != "" {
			req.IssueTitles[issue] = continuation.IssueTitle
		}
	}
	return nil
}

func changesRunLifecycle(eventType string) bool {
	switch eventType {
	case "run.started", "run.continued", "run.queued", "run.capacity_queued", "run.blocked", "run.await", "run.resumed", "run.retry", "run.finished", "run.aborted", "run.cancelled":
		return true
	default:
		return false
	}
}

func payloadStringValue(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return value
}

func payloadBoolValue(payload map[string]any, key string) bool {
	value, _ := payload[key].(bool)
	return value
}

func requestContainsIssue(issues []int, target int) bool {
	for _, issue := range issues {
		if issue == target {
			return true
		}
	}
	return false
}
