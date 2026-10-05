package batch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	Wait               *daemon.RunWait
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
		if issue <= 0 || latestRunByIssue[issue] != state.RunID || !state.IsActive() || !(state.IsAwaiting() || state.IsCapacityQueued() || state.Status() == "queued") {
			continue
		}
		event := state.CapacityQueuedEvent
		batchID := state.BatchID()
		if state.IsCapacityQueued() && event != nil {
			batchID = strings.TrimSpace(payloadStringValue(event.Payload, "batch_id"))
		}
		if batchID == "" {
			batchID = state.BatchID()
		}
		if batchID != "" && daemon.IsRunActive(daemon.BatchDir(layout.SandmanDir, batchID)) {
			continue
		}
		claim, claimErr := daemon.ClaimRun(layout.SandmanDir, state.RunID)
		if claimErr != nil {
			continue
		}
		_ = claim.Close()
		wait, waitErr := daemon.ReadRunWait(layout.BatchDir(batchID), state.RunID)
		if waitErr == nil {
			var valid bool
			wait, _, valid = reconcileRecoveryWait(wait, state)
			if !valid {
				continue
			}
			if wait.Issue != issue || !wait.RecoverableAt(time.Now().UTC()) {
				continue
			}
			batchID = wait.BatchID
			ready = append(ready, ReadyContinuation{IssueNumber: issue, RunID: state.RunID, PreviousRunID: state.RunID, PreviousRunBatchID: batchID,
				BatchID: batchID, Branch: wait.Branch, BaseBranch: wait.BaseBranch, Wait: &wait})
			continue
		}
		if !os.IsNotExist(waitErr) || !state.IsCapacityQueued() || event == nil || !payloadBoolValue(event.Payload, "ready_continuation") {
			continue
		}
		legacyWait, valid := legacyRecoveryWait(state)
		if !valid || !legacyWait.RecoverableAt(time.Now().UTC()) {
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
			Wait:               &legacyWait,
		})
	}
	return ready
}

// Events determine whether execution has started. Reconcile a sidecar left
// between the authoritative await event and its schedule checkpoint.
func reconcileRecoveryWait(record daemon.RunWait, state events.RunState) (daemon.RunWait, bool, bool) {
	if !state.HasStarted() || !record.InitialAdmission && record.Ready == state.IsCapacityQueued() {
		return record, false, true
	}
	record.InitialAdmission, record.AdmissionMode = false, int(ModeContinue)
	record.Branch = state.Branch()
	record.PreviousRunID, record.PreviousBatchID = state.RunID, record.BatchID
	record.Ready, record.UsageLimitProbe = state.IsCapacityQueued(), false
	record.OperationID, record.OperationDeadline = "capacity", time.Time{}
	if record.Ready && state.CapacityQueuedEvent != nil {
		record.LeaseExpiresAt = state.CapacityQueuedEvent.Timestamp.Add(daemon.RunRecoveryGrace)
	}
	if !record.Ready {
		if state.AwaitEvent == nil {
			return record, true, false
		}
		if state.AwaitReason() == "usage-limit" {
			seconds, ok := lifecycleDeadlineSeconds(state.AwaitEvent.Payload["usage_limit_deadline_unix_seconds"])
			if !ok || seconds <= 0 {
				return record, true, false
			}
			record.UsageLimitProbe = true
			record.OperationDeadline = time.Unix(seconds, 0)
			record.OperationID = fmt.Sprintf("quota:%d", seconds)
			record.NextPollAt = state.AwaitEvent.Timestamp.Add(usageLimitPollInterval)
		} else if deadline, gate, ok := lifecycleDeadline(state.AwaitEvent.Payload); ok {
			record.OperationDeadline = deadline
			record.OperationID = fmt.Sprintf("%s:%d", gate, deadline.Unix())
		} else {
			return record, true, false
		}
	}
	if !record.OperationDeadline.IsZero() && record.OperationDeadline.Before(record.LeaseExpiresAt) {
		record.LeaseExpiresAt = record.OperationDeadline
	}
	return record, true, record.Branch != ""
}

func legacyRecoveryWait(state events.RunState) (daemon.RunWait, bool) {
	event := state.CapacityQueuedEvent
	if !state.IsCapacityQueued() || event == nil || event.Timestamp.IsZero() || !payloadBoolValue(event.Payload, "ready_continuation") {
		return daemon.RunWait{}, false
	}
	record := daemon.RunWait{Protocol: "run-wait/v1", RunID: state.RunID, BatchID: state.BatchID(), Issue: state.IssueNumber(), Branch: state.Branch(), BaseBranch: strings.TrimSpace(payloadStringValue(event.Payload, "base_branch")), AdmissionMode: int(ModeContinue), Ready: true, OperationID: "capacity", LeaseExpiresAt: event.Timestamp.Add(daemon.RunRecoveryGrace), RecoveryEventAt: event.Timestamp}
	if deadline, gate, ok := lifecycleDeadline(event.Payload); ok {
		record.OperationDeadline = deadline
		record.OperationID = fmt.Sprintf("%s:%d", gate, deadline.Unix())
		if deadline.Before(record.LeaseExpiresAt) {
			record.LeaseExpiresAt = deadline
		}
	}
	return record, record.BatchID != "" && record.Branch != "" && record.BaseBranch != ""
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
		if continuation.Wait != nil {
			if req.RecoveryWaits == nil {
				req.RecoveryWaits = map[int]daemon.RunWait{}
			}
			req.RecoveryWaits[continuation.IssueNumber] = *continuation.Wait
			if continuation.Wait.InitialAdmission {
				issue := continuation.IssueNumber
				if !requestContainsIssue(req.Issues, issue) {
					req.Issues = append(req.Issues, issue)
				}
				if req.Mode == nil {
					req.Mode = map[int]IssueMode{}
				}
				req.Mode[issue] = IssueMode(continuation.Wait.AdmissionMode)
				if req.RunIDs == nil {
					req.RunIDs = map[int]string{}
				}
				req.RunIDs[issue] = continuation.RunID
				if req.Branches == nil {
					req.Branches = map[int]string{}
				}
				req.Branches[issue] = continuation.Branch
				if req.BaseBranches == nil {
					req.BaseBranches = map[int]string{}
				}
				req.BaseBranches[issue] = continuation.BaseBranch
				if req.ReuseSession == nil {
					req.ReuseSession = map[int]bool{}
				}
				req.ReuseSession[issue] = continuation.Wait.ReuseSession
				if len(continuation.Wait.Dependencies) > 0 {
					if req.Dependencies == nil {
						req.Dependencies = map[int][]int{}
					}
					req.Dependencies[issue] = append([]int(nil), continuation.Wait.Dependencies...)
				}
				if continuation.Wait.PreviousRunID != "" {
					if req.PreviousRunIDs == nil {
						req.PreviousRunIDs = map[int]string{}
					}
					req.PreviousRunIDs[issue] = continuation.Wait.PreviousRunID
				}
				if continuation.Wait.PreviousBatchID != "" {
					if req.PreviousRunBatchIDs == nil {
						req.PreviousRunBatchIDs = map[int]string{}
					}
					req.PreviousRunBatchIDs[issue] = continuation.Wait.PreviousBatchID
				}
				continue
			}
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
		req.ReadyContinuations[issue] = continuation.Wait == nil || continuation.Wait.Ready
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
