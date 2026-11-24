package batch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
)

const (
	ciWaitProtocol = "ci-wait/v1"
	// ciWaitTimeout is deliberately separate from review_timeout. CI has no
	// reviewer request to supply a deadline, so the runtime owns this budget.
	ciWaitTimeout     = 30 * time.Minute
	gateCIWaitTimeout = "ci-wait-timeout"
)

type ciWaitRegistration struct {
	Protocol             string                  `json:"protocol"`
	PullRequest          int                     `json:"pull_request"`
	HeadSHA              string                  `json:"head_sha"`
	ExecutionID          string                  `json:"execution_id,omitempty"`
	SourceExecutionID    string                  `json:"source_execution_id,omitempty"`
	SourceWorkflowRunID  int64                   `json:"source_workflow_run_id,omitempty"`
	SourceWorkflowRunIDs []int64                 `json:"source_workflow_run_ids,omitempty"`
	ActionsExecution     *ciWaitActionsExecution `json:"actions_execution,omitempty"`
	StartedUnixSeconds   int64                   `json:"started_unix_seconds"`
	DeadlineUnixSeconds  int64                   `json:"deadline_unix_seconds"`
	EffectiveTimeoutSecs int64                   `json:"effective_timeout_seconds"`
	RemediationAttempts  int                     `json:"remediation_attempts"`
}

type ciWaitActionsExecution struct {
	Repository  string `json:"repository"`
	PullRequest int    `json:"pull_request"`
	BaseRefName string `json:"base_ref_name"`
	HeadSHA     string `json:"head_sha"`
	WorkflowID  int64  `json:"workflow_id"`
	Event       string `json:"event"`
	RunID       int64  `json:"run_id"`
	RunNumber   int64  `json:"run_number"`
	Attempt     int    `json:"attempt"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	Replacement bool   `json:"replacement"`
}

func (s *runSession) ciWaitEvidence(ctx context.Context, workDir string, pr *github.PR, headSHA string, verified ...*github.ManagedCIExecution) (map[string]any, error) {
	if pr == nil || pr.Number <= 0 || strings.TrimSpace(headSHA) == "" || !strings.EqualFold(strings.TrimSpace(pr.HeadRefOid), strings.TrimSpace(headSHA)) {
		return nil, nil
	}
	var candidate *ciWaitActionsExecution
	if len(verified) > 1 {
		return nil, fmt.Errorf("CI wait state: multiple verified Actions executions")
	}
	if len(verified) == 1 && verified[0] != nil {
		candidate = ciWaitActionsExecutionFromExecution(verified[0], pr, headSHA)
		if candidate == nil {
			return nil, fmt.Errorf("CI wait state: verified Actions execution does not match the current pull request")
		}
	}
	path := filepath.Join(paths.NewLayout(nil, workDir).StateDir, fmt.Sprintf("%d.ci_wait.json", pr.Number))
	var evidence map[string]any
	err := withOperationLock(ctx, path, func() error {
		registration, err := readCIWaitRegistration(path)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("read CI wait state: %w", err)
		}
		if err == nil {
			if _, validationErr := ciWaitEvidenceFromRegistration(registration, pr.Number); validationErr != nil {
				return validationErr
			}
		}
		newHead := !os.IsNotExist(err) && !strings.EqualFold(registration.HeadSHA, headSHA)
		newExecution := false
		selectedActions := candidate
		if !newHead {
			if candidate != nil && registration.ActionsExecution != nil {
				switch compareCIWaitActionsExecution(candidate, registration.ActionsExecution) {
				case 1:
					newExecution = true
				case 0:
					// Re-observation of the same verified Actions execution keeps
					// its original bound.
				case -1:
					// A stale provider listing cannot roll a durable wait back.
					selectedActions = registration.ActionsExecution
				case 2:
					return fmt.Errorf("CI wait state: verified Actions execution conflicts with the retained workflow identity")
				}
			} else if candidate != nil {
				newExecution = registration.ActionsExecution == nil
			}
			if candidate == nil && registration.ActionsExecution != nil {
				// Once a read-only Actions identity has established the current
				// wait generation, the PR rollup's check links may lag or remain
				// stale. Only another verified Actions identity may advance it.
				selectedActions = registration.ActionsExecution
			}
			if selectedActions == nil && registration.ActionsExecution == nil {
				// Preserve the legacy current-head check identity behavior until
				// a verified Actions execution has bound this wait.
				newExecution = ciActive(pr, headSHA) && pr.CIExecutionID != "" && registration.ExecutionID != pr.CIExecutionID
			}
		}
		if os.IsNotExist(err) || newHead || newExecution {
			now := s.runtimeNow()
			var sourceRunID int64
			if pr.CIFailedWorkflow != nil {
				sourceRunID, _ = strconv.ParseInt(strings.TrimSpace(pr.CIFailedWorkflow.RunID), 10, 64)
			}
			sourceRunIDs := []int64(nil)
			if !os.IsNotExist(err) && !newHead {
				sourceRunIDs = append(sourceRunIDs, registration.SourceWorkflowRunIDs...)
				sourceRunIDs = appendWorkflowRunID(sourceRunIDs, registration.SourceWorkflowRunID)
			}
			sourceRunIDs = appendWorkflowRunID(sourceRunIDs, sourceRunID)
			registration = ciWaitRegistration{
				Protocol:             ciWaitProtocol,
				PullRequest:          pr.Number,
				HeadSHA:              headSHA,
				ExecutionID:          pr.CIExecutionID,
				SourceExecutionID:    pr.CIExecutionID,
				SourceWorkflowRunID:  sourceRunID,
				SourceWorkflowRunIDs: sourceRunIDs,
				ActionsExecution:     selectedActions,
				StartedUnixSeconds:   now.Unix(),
				DeadlineUnixSeconds:  now.Add(ciWaitTimeout).Unix(),
				EffectiveTimeoutSecs: int64(ciWaitTimeout / time.Second),
			}
			if selectedActions != nil {
				registration.ExecutionID = managedCIExecutionID(selectedActions)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return fmt.Errorf("create CI wait state directory: %w", err)
			}
			if err := atomicfs.WriteAtomicJSON(path, registration, 0o600); err != nil {
				return fmt.Errorf("write CI wait state: %w", err)
			}
		} else if selectedActions != nil && registration.ActionsExecution == nil {
			// A verified replacement binds an existing legacy/head-only wait
			// to a new bounded operation exactly once.
			now := s.runtimeNow()
			registration.ExecutionID = managedCIExecutionID(selectedActions)
			registration.SourceExecutionID = pr.CIExecutionID
			if pr.CIFailedWorkflow != nil {
				registration.SourceWorkflowRunID, _ = strconv.ParseInt(strings.TrimSpace(pr.CIFailedWorkflow.RunID), 10, 64)
			}
			registration.SourceWorkflowRunIDs = appendWorkflowRunID(
				registration.SourceWorkflowRunIDs, registration.SourceWorkflowRunID,
			)
			registration.ActionsExecution = selectedActions
			registration.StartedUnixSeconds = now.Unix()
			registration.DeadlineUnixSeconds = now.Add(ciWaitTimeout).Unix()
			registration.EffectiveTimeoutSecs = int64(ciWaitTimeout / time.Second)
			if err := atomicfs.WriteAtomicJSON(path, registration, 0o600); err != nil {
				return fmt.Errorf("write CI wait state: %w", err)
			}
		} else if candidate != nil && registration.ActionsExecution != nil &&
			compareCIWaitActionsExecution(candidate, registration.ActionsExecution) == 0 &&
			managedCIExecutionStatusAdvances(candidate, registration.ActionsExecution) {
			// Status can advance while the execution identity and its deadline
			// remain fixed. A delayed provider read cannot regress a retained
			// execution or replace its terminal conclusion.
			registration.ActionsExecution = candidate
			if err := atomicfs.WriteAtomicJSON(path, registration, 0o600); err != nil {
				return fmt.Errorf("update CI wait observation: %w", err)
			}
		}
		evidence, err = ciWaitEvidenceFromRegistration(registration, pr.Number)
		return err
	})
	return evidence, err
}

func ciWaitActionsExecutionFromExecution(execution *github.ManagedCIExecution, pr *github.PR, headSHA string) *ciWaitActionsExecution {
	if execution == nil || pr == nil || execution.RunID <= 0 || execution.RunNumber <= 0 ||
		execution.WorkflowID <= 0 || execution.Attempt <= 0 || strings.TrimSpace(execution.Repository) == "" ||
		!strings.EqualFold(strings.TrimSpace(execution.Event), "pull_request") ||
		!validCIWaitActionsStatus(execution.Status, execution.Conclusion) ||
		pr.Number != execution.PullRequest || strings.TrimSpace(pr.BaseRefName) != strings.TrimSpace(execution.BaseRefName) ||
		!strings.EqualFold(strings.TrimSpace(execution.HeadSHA), strings.TrimSpace(headSHA)) ||
		!strings.EqualFold(strings.TrimSpace(pr.HeadRefOid), strings.TrimSpace(headSHA)) {
		return nil
	}
	return &ciWaitActionsExecution{
		Repository: strings.TrimSpace(execution.Repository), PullRequest: execution.PullRequest,
		BaseRefName: strings.TrimSpace(execution.BaseRefName), HeadSHA: strings.TrimSpace(execution.HeadSHA),
		WorkflowID: execution.WorkflowID, Event: strings.ToLower(strings.TrimSpace(execution.Event)),
		RunID: execution.RunID, RunNumber: execution.RunNumber, Attempt: execution.Attempt,
		Status:     strings.ToLower(strings.TrimSpace(execution.Status)),
		Conclusion: strings.ToLower(strings.TrimSpace(execution.Conclusion)), Replacement: true,
	}
}

func validCIWaitActionsStatus(status, conclusion string) bool {
	return github.ValidManagedCIExecutionStatus(status, conclusion)
}

func managedCIExecutionStatusAdvances(candidate, retained *ciWaitActionsExecution) bool {
	if candidate == nil || retained == nil {
		return false
	}
	retainedStatus := strings.ToLower(strings.TrimSpace(retained.Status))
	candidateStatus := strings.ToLower(strings.TrimSpace(candidate.Status))
	if retainedStatus == "completed" {
		return false
	}
	if candidateStatus == "completed" {
		return true
	}
	return retainedStatus != "in_progress" && candidateStatus == "in_progress"
}

// compareCIWaitActionsExecution returns 1 for a strictly newer execution, 0
// for the same execution, -1 for an older execution, and 2 for conflicting
// identity that cannot safely be ordered.
func compareCIWaitActionsExecution(candidate, retained *ciWaitActionsExecution) int {
	if candidate == nil || retained == nil || !strings.EqualFold(candidate.Repository, retained.Repository) ||
		candidate.PullRequest != retained.PullRequest ||
		!strings.EqualFold(candidate.HeadSHA, retained.HeadSHA) || candidate.WorkflowID != retained.WorkflowID {
		return 2
	}
	if !strings.EqualFold(candidate.Event, "pull_request") || !strings.EqualFold(retained.Event, "pull_request") {
		return 2
	}
	if strings.TrimSpace(candidate.BaseRefName) != strings.TrimSpace(retained.BaseRefName) {
		// A changed PR base is a distinct identity, but a higher numbered run
		// in the same workflow/PR/head can still prove the new operation. A
		// rerun of the same run cannot retroactively change its base binding.
		if candidate.RunID == retained.RunID || candidate.RunNumber == retained.RunNumber {
			return 2
		}
		if candidate.RunNumber > retained.RunNumber {
			return 1
		}
		return -1
	}
	if candidate.RunID == retained.RunID {
		if candidate.RunNumber != retained.RunNumber {
			return 2
		}
		switch {
		case candidate.Attempt > retained.Attempt:
			return 1
		case candidate.Attempt < retained.Attempt:
			return -1
		default:
			return 0
		}
	}
	switch {
	case candidate.RunNumber > retained.RunNumber:
		return 1
	case candidate.RunNumber < retained.RunNumber:
		return -1
	default:
		return 2
	}
}

func managedCIExecutionID(execution *ciWaitActionsExecution) string {
	if execution == nil {
		return ""
	}
	return fmt.Sprintf("actions:%d:%d:%d", execution.WorkflowID, execution.RunID, execution.Attempt)
}

func appendWorkflowRunID(runIDs []int64, runID int64) []int64 {
	if runID <= 0 {
		return runIDs
	}
	for _, existing := range runIDs {
		if existing == runID {
			return runIDs
		}
	}
	return append(runIDs, runID)
}

func ciWaitEvidenceFromRegistration(registration ciWaitRegistration, prNumber int) (map[string]any, error) {
	if registration.Protocol != ciWaitProtocol || registration.PullRequest != prNumber || registration.HeadSHA == "" || registration.SourceWorkflowRunID < 0 || !validCIWaitSourceWorkflowRunIDs(registration.SourceWorkflowRunIDs) || registration.DeadlineUnixSeconds <= registration.StartedUnixSeconds || registration.EffectiveTimeoutSecs <= 0 || registration.DeadlineUnixSeconds-registration.StartedUnixSeconds != registration.EffectiveTimeoutSecs ||
		(registration.ActionsExecution != nil && (registration.ActionsExecution.PullRequest != prNumber ||
			!strings.EqualFold(registration.ActionsExecution.HeadSHA, registration.HeadSHA) ||
			registration.ActionsExecution.Repository == "" || registration.ActionsExecution.BaseRefName == "" ||
			registration.ActionsExecution.WorkflowID <= 0 || !strings.EqualFold(registration.ActionsExecution.Event, "pull_request") ||
			registration.ActionsExecution.RunID <= 0 ||
			registration.ActionsExecution.RunNumber <= 0 || registration.ActionsExecution.Attempt <= 0 ||
			!registration.ActionsExecution.Replacement ||
			!validCIWaitActionsStatus(registration.ActionsExecution.Status, registration.ActionsExecution.Conclusion) ||
			registration.ExecutionID != managedCIExecutionID(registration.ActionsExecution))) {
		return nil, fmt.Errorf("CI wait state is invalid")
	}
	wait := map[string]any{
		"protocol":                  registration.Protocol,
		"pull_request":              registration.PullRequest,
		"head_sha":                  registration.HeadSHA,
		"started_unix_seconds":      registration.StartedUnixSeconds,
		"deadline_unix_seconds":     registration.DeadlineUnixSeconds,
		"effective_timeout_seconds": registration.EffectiveTimeoutSecs,
		"remediation_attempts":      registration.RemediationAttempts,
	}
	if len(registration.SourceWorkflowRunIDs) > 0 {
		wait["source_workflow_run_ids"] = append([]int64(nil), registration.SourceWorkflowRunIDs...)
	}
	if registration.ExecutionID != "" {
		wait["execution_id"] = registration.ExecutionID
	}
	if registration.SourceExecutionID != "" {
		wait["source_execution_id"] = registration.SourceExecutionID
	}
	if registration.SourceWorkflowRunID > 0 {
		wait["source_workflow_run_id"] = registration.SourceWorkflowRunID
	}
	if registration.ActionsExecution != nil {
		wait["actions_execution"] = map[string]any{
			"repository": registration.ActionsExecution.Repository, "pull_request": registration.ActionsExecution.PullRequest,
			"base_ref_name": registration.ActionsExecution.BaseRefName, "head_sha": registration.ActionsExecution.HeadSHA,
			"workflow_id": registration.ActionsExecution.WorkflowID, "event": registration.ActionsExecution.Event,
			"run_id":     registration.ActionsExecution.RunID,
			"run_number": registration.ActionsExecution.RunNumber, "attempt": registration.ActionsExecution.Attempt,
			"status": registration.ActionsExecution.Status, "conclusion": registration.ActionsExecution.Conclusion,
			"replacement": registration.ActionsExecution.Replacement,
		}
	}
	return map[string]any{"ci_wait": wait}, nil
}

func validCIWaitSourceWorkflowRunIDs(runIDs []int64) bool {
	seen := make(map[int64]struct{}, len(runIDs))
	for _, runID := range runIDs {
		if runID <= 0 {
			return false
		}
		if _, duplicate := seen[runID]; duplicate {
			return false
		}
		seen[runID] = struct{}{}
	}
	return true
}

func readCIWaitRegistration(path string) (ciWaitRegistration, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ciWaitRegistration{}, err
	}
	var registration ciWaitRegistration
	if err := json.Unmarshal(data, &registration); err != nil {
		return ciWaitRegistration{}, err
	}
	return registration, nil
}
