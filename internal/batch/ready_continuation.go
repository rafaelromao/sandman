package batch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
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

func quotaWaitDueAt(evidence map[string]any, startedAt time.Time) time.Time {
	if startedAt.IsZero() {
		return time.Time{}
	}
	allowance, err := restoreUsageLimitAllowance(evidence)
	if err != nil {
		return startedAt
	}
	remaining := max(0, usageLimitRetryWindow-allowance.waited)
	if remaining == 0 {
		return startedAt
	}
	wait := usageLimitPollInterval
	if !allowance.providerReset.IsZero() {
		delta := allowance.providerReset.Sub(startedAt)
		if delta >= -usageLimitProviderResetHorizon && delta <= usageLimitProviderResetHorizon {
			wait = max(0, delta)
		}
	}
	return startedAt.Add(min(wait, remaining))
}

func quotaSchedulesMatch(checkpoint, eventSchedule time.Time) bool {
	if checkpoint.IsZero() || eventSchedule.IsZero() {
		return checkpoint.IsZero() && eventSchedule.IsZero()
	}
	delta := checkpoint.Sub(eventSchedule)
	return delta >= -time.Second && delta <= time.Second
}

func quotaWaitScheduleStartedAt(event *events.Event) time.Time {
	if event == nil {
		return time.Time{}
	}
	if payloadBoolValue(event.Payload, "usage_limit_ownership_recovery") {
		if startedAt := payloadStringValue(event.Payload, "usage_limit_wait_started_at"); startedAt != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, startedAt); err == nil && !parsed.IsZero() {
				return parsed.UTC()
			}
		}
	}
	return event.Timestamp
}

// FindReadyContinuations returns only the latest per-issue lifecycle states
// that are durably capacity-queued and whose original batch no longer has a
// live owner. A later lifecycle event or a live batch always takes precedence.
func FindReadyContinuations(eventLog []events.Event, layout paths.Layout) []ReadyContinuation {
	return FindReadyContinuationsAt(eventLog, layout, time.Now().UTC())
}

// FindReadyContinuationsAt is FindReadyContinuations evaluated at now. It
// keeps recovery eligibility deterministic for callers with an injected clock.
func FindReadyContinuationsAt(eventLog []events.Event, layout paths.Layout, now time.Time) []ReadyContinuation {
	now = now.UTC()
	states := events.ProjectRunStates(eventLog)
	latestRunByIssue := make(map[int]string)
	for _, event := range eventLog {
		if event.Issue <= 0 || event.RunID == "" || !changesRunLifecycle(event.Type) {
			continue
		}
		latestRunByIssue[event.Issue] = event.RunID
	}

	ready := make([]ReadyContinuation, 0)
	idx, err := batchindex.Load(layout.BatchesIndexPath)
	if err != nil {
		return ready
	}
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
		batchLocation := layout.LocateBatch(batchID, "")
		if batchID != "" {
			var ok bool
			if batchLocation, ok = idx.LocateBatchIdentity(layout, batchID); !ok {
				continue
			}
		}
		if batchID != "" && daemon.IsRunActive(batchLocation.Dir) {
			continue
		}
		claim, claimErr := daemon.ClaimRun(layout.SandmanDir, state.RunID)
		if claimErr != nil {
			continue
		}
		_ = claim.Close()
		wait, waitErr := daemon.ReadRunWaitAt(layout, batchLocation, state.RunID)
		if waitErr == nil {
			var valid bool
			wait, _, valid = reconcileRecoveryWait(wait, state)
			if !valid {
				continue
			}
			if wait.Issue != issue || !wait.RecoverableAt(now) {
				continue
			}
			batchID = wait.BatchID
			ready = append(ready, ReadyContinuation{IssueNumber: issue, RunID: state.RunID, PreviousRunID: state.RunID, PreviousRunBatchID: batchID,
				BatchID: batchID, Branch: wait.Branch, BaseBranch: wait.BaseBranch, Wait: &wait})
			continue
		}
		if !os.IsNotExist(waitErr) {
			continue
		}
		if evidence := quotaPollingEvidence(state); evidence != nil && state.AwaitEvent != nil && !state.IsCapacityQueued() {
			quotaWait, valid := quotaAwaitRecoveryWait(state, evidence)
			if valid && quotaWait.RecoverableAt(now) {
				ready = append(ready, ReadyContinuation{
					IssueNumber:        issue,
					RunID:              state.RunID,
					PreviousRunID:      state.RunID,
					PreviousRunBatchID: quotaWait.BatchID,
					BatchID:            quotaWait.BatchID,
					Branch:             quotaWait.Branch,
					BaseBranch:         quotaWait.BaseBranch,
					Wait:               &quotaWait,
				})
			}
			continue
		}
		if state.AwaitEvent != nil && !state.IsCapacityQueued() {
			if ciWait, valid := ciRecoveryAwaitWait(state); valid && ciWait.RecoverableAt(now) {
				ready = append(ready, ReadyContinuation{
					IssueNumber:        issue,
					RunID:              state.RunID,
					PreviousRunID:      state.RunID,
					PreviousRunBatchID: ciWait.BatchID,
					BatchID:            ciWait.BatchID,
					Branch:             ciWait.Branch,
					BaseBranch:         ciWait.BaseBranch,
					Wait:               &ciWait,
				})
			}
			continue
		}
		if !state.IsCapacityQueued() || event == nil || !payloadBoolValue(event.Payload, "ready_continuation") {
			continue
		}
		legacyWait, valid := legacyRecoveryWait(state)
		if !valid || !legacyWait.RecoverableAt(now) {
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
	if !state.HasStarted() {
		return record, false, true
	}
	previous := record
	record = daemon.ReconcileReviewAfterCI(record, state)
	record.InitialAdmission, record.AdmissionMode = false, int(ModeContinue)
	record.Branch = state.Branch()
	record.PreviousRunID, record.PreviousBatchID = state.RunID, record.BatchID
	record.Ready, record.UsageLimitProbe = state.IsCapacityQueued(), false
	record.OperationID, record.OperationDeadline = "capacity", time.Time{}
	quotaScheduleChanged := false
	recoveryScheduleChanged := false
	if record.Ready && state.CapacityQueuedEvent != nil && (previous.InitialAdmission || !previous.Ready) {
		record.LeaseExpiresAt = state.CapacityQueuedEvent.Timestamp.Add(daemon.RunRecoveryGrace)
	}
	if evidence := quotaPollingEvidence(state); evidence != nil {
		record.UsageLimitProbe = true
		record.OperationID, record.OperationDeadline = "quota:"+state.RunID, time.Time{}
		if record.Ready {
			record.NextPollAt = state.CapacityQueuedEvent.Timestamp
		} else {
			eventSchedule := quotaWaitDueAt(evidence, quotaWaitScheduleStartedAt(state.AwaitEvent))
			if payloadBoolValue(state.AwaitEvent.Payload, "usage_limit_ownership_recovery") &&
				!previous.NextPollAt.IsZero() && quotaSchedulesMatch(previous.NextPollAt, eventSchedule) {
				record.NextPollAt = previous.NextPollAt
			} else {
				record.NextPollAt = eventSchedule
			}
		}
		quotaScheduleChanged = !quotaSchedulesMatch(previous.NextPollAt, record.NextPollAt)
	} else if !record.Ready {
		if state.AwaitEvent == nil {
			return record, true, false
		}
		if id, deadline, ok := githubRecoveryOperation(state.AwaitEvent.Payload); ok {
			// A GitHub recovery wait keeps its 24-hour window and its next
			// hourly attempt across restart.
			record.OperationID, record.OperationDeadline = id, deadline
			record.NextPollAt, _ = githubRecoveryNextAttempt(state.AwaitEvent.Payload)
			recoveryScheduleChanged = !quotaSchedulesMatch(previous.NextPollAt, record.NextPollAt)
		} else if id, deadline, ok := ciRecoveryOperation(state.AwaitEvent.Payload); ok {
			record.OperationID, record.OperationDeadline = id, deadline
			record.NextPollAt, _ = ciRecoveryNextAttempt(state.AwaitEvent.Payload)
			recoveryScheduleChanged = !quotaSchedulesMatch(previous.NextPollAt, record.NextPollAt)
		} else if continuityHoldPayload(state.AwaitEvent.Payload) {
			// A continuity hold has no operation deadline. It stays
			// recoverable within the ordinary ownerless grace.
			record.OperationID, record.OperationDeadline = continuityHoldOperation(state.RunID), time.Time{}
		} else if id, deadline, ok := lifecycleOwnershipOperation(state.AwaitEvent.Payload, state.RunID); ok {
			record.OperationID, record.OperationDeadline = id, deadline
		} else {
			return record, true, false
		}
	}
	if !record.OperationDeadline.IsZero() && record.OperationDeadline.Before(record.LeaseExpiresAt) {
		record.LeaseExpiresAt = record.OperationDeadline
	}
	changed := previous.InitialAdmission || previous.Ready != record.Ready || previous.UsageLimitProbe != record.UsageLimitProbe || previous.OperationID != record.OperationID || !previous.OperationDeadline.Equal(record.OperationDeadline) || quotaScheduleChanged || recoveryScheduleChanged
	if !changed {
		record.NextPollAt = previous.NextPollAt
	}
	return record, changed, record.Branch != ""
}

func quotaAwaitRecoveryWait(state events.RunState, evidence map[string]any) (daemon.RunWait, bool) {
	event := state.AwaitEvent
	if event == nil || event.Timestamp.IsZero() || state.RunID == "" || state.IssueNumber() <= 0 {
		return daemon.RunWait{}, false
	}
	batchID := state.BatchID()
	branch := state.Branch()
	baseBranch := strings.TrimSpace(payloadStringValue(event.Payload, "base_branch"))
	if batchID == "" || branch == "" || baseBranch == "" {
		return daemon.RunWait{}, false
	}
	return daemon.RunWait{
		Protocol:        "run-wait/v1",
		RunID:           state.RunID,
		BatchID:         batchID,
		Issue:           state.IssueNumber(),
		Branch:          branch,
		BaseBranch:      baseBranch,
		AdmissionMode:   int(ModeContinue),
		ReuseSession:    true,
		UsageLimitProbe: true,
		OperationID:     "quota:" + state.RunID,
		LeaseExpiresAt:  event.Timestamp.Add(daemon.RunRecoveryGrace),
		NextPollAt:      quotaWaitDueAt(evidence, quotaWaitScheduleStartedAt(event)),
		RecoveryEventAt: event.Timestamp,
	}, true
}

func ciRecoveryAwaitWait(state events.RunState) (daemon.RunWait, bool) {
	event := state.AwaitEvent
	if event == nil || event.Timestamp.IsZero() || state.RunID == "" || state.IssueNumber() <= 0 {
		return daemon.RunWait{}, false
	}
	id, deadline, ok := ciRecoveryOperation(event.Payload)
	if !ok {
		return daemon.RunWait{}, false
	}
	batchID := state.BatchID()
	branch := state.Branch()
	baseBranch := strings.TrimSpace(payloadStringValue(event.Payload, "base_branch"))
	if batchID == "" || branch == "" || baseBranch == "" {
		return daemon.RunWait{}, false
	}
	leaseExpiresAt := event.Timestamp.Add(daemon.RunRecoveryGrace)
	if deadline.Before(leaseExpiresAt) {
		leaseExpiresAt = deadline
	}
	nextPollAt, _ := ciRecoveryNextAttempt(event.Payload)
	return daemon.RunWait{
		Protocol:          "run-wait/v1",
		RunID:             state.RunID,
		BatchID:           batchID,
		Issue:             state.IssueNumber(),
		Branch:            branch,
		BaseBranch:        baseBranch,
		AdmissionMode:     int(ModeContinue),
		ReuseSession:      true,
		OperationID:       id,
		OperationDeadline: deadline,
		LeaseExpiresAt:    leaseExpiresAt,
		NextPollAt:        nextPollAt,
		RecoveryEventAt:   event.Timestamp,
	}, true
}

func recoveryWaitFromEvent(state events.RunState) (daemon.RunWait, bool) {
	if evidence := quotaPollingEvidence(state); evidence != nil && state.AwaitEvent != nil && !state.IsCapacityQueued() {
		return quotaAwaitRecoveryWait(state, evidence)
	}
	return legacyRecoveryWait(state)
}

func legacyRecoveryWait(state events.RunState) (daemon.RunWait, bool) {
	event := state.CapacityQueuedEvent
	if !state.IsCapacityQueued() || event == nil || event.Timestamp.IsZero() || !payloadBoolValue(event.Payload, "ready_continuation") {
		return daemon.RunWait{}, false
	}
	record := daemon.RunWait{Protocol: "run-wait/v1", RunID: state.RunID, BatchID: state.BatchID(), Issue: state.IssueNumber(), Branch: state.Branch(), BaseBranch: strings.TrimSpace(payloadStringValue(event.Payload, "base_branch")), AdmissionMode: int(ModeContinue), Ready: true, OperationID: "capacity", LeaseExpiresAt: event.Timestamp.Add(daemon.RunRecoveryGrace), RecoveryEventAt: event.Timestamp}
	if payloadBoolValue(event.Payload, "usage_limit_probe") {
		record.UsageLimitProbe = true
		record.OperationID = "quota:" + state.RunID
		record.NextPollAt = event.Timestamp
	} else if deadline, gate, ok := lifecycleDeadline(event.Payload); ok {
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
		req.TaskPrompts[issue] = prompt.ContinuationTaskPromptWithReviewSettings(
			taskContent, req.PromptConfig.ReviewCommand, req.PromptConfig.ReviewCommandSet, reviewTimeout,
		)
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
