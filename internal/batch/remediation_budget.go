package batch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/paths"
)

var errRemediationBudgetExhausted = errors.New("durable remediation budget exhausted")

type remediationBudget struct {
	Protocol    string         `json:"protocol"`
	PullRequest int            `json:"pull_request"`
	HeadSHA     string         `json:"head_sha"`
	Attempts    map[string]int `json:"attempts"`
}

// reserveRemediation consumes the relevant head/request budget before launch.
// Scheduling, observation and executor reconstruction consume nothing. Initial
// publication/request work belongs to the ordinary configured retry budget.
func (s *runSession) reserveRemediation(ctx context.Context, workDir string, extras map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if isImplementorOwnedGateFailure(extras) {
		return nil
	}
	gate, _ := extras["gate"].(string)
	prNumber := remediationNumber(extras["pull_request"])
	head, _ := extras["head_sha"].(string)
	if ci, ok := extras["ci_wait"].(map[string]any); ok {
		if prNumber == 0 {
			prNumber = remediationNumber(ci["pull_request"])
		}
		if head == "" {
			head, _ = ci["head_sha"].(string)
		}
	}
	if prNumber == 0 || strings.TrimSpace(head) == "" {
		return fmt.Errorf("remediation has incomplete PR/head identity")
	}
	scope := "implementation"
	switch gate {
	case "ci-failure", "merge-conflict", gateCIWaitTimeout:
		path := filepath.Join(paths.NewLayout(nil, workDir).StateDir, fmt.Sprintf("%d.ci_wait.json", prNumber))
		registration, err := readCIWaitRegistration(path)
		if err != nil {
			return err
		}
		if _, err := ciWaitEvidenceFromRegistration(registration, int(prNumber)); err != nil || !strings.EqualFold(registration.HeadSHA, head) {
			return fmt.Errorf("CI remediation identity is invalid")
		}
		if registration.RemediationAttempts >= s.resumeCapFor() {
			return errRemediationBudgetExhausted
		}
		registration.RemediationAttempts++
		if err := ctx.Err(); err != nil {
			return err
		}
		return atomicfs.WriteAtomicJSON(path, registration, 0o600)
	case gatePRHeadChanged:
		scope = "head-reconciliation"
	default:
		if request, ok := extras["review_request"].(map[string]any); ok {
			if trigger, _ := request["trigger_id"].(string); trigger != "" {
				scope = "review:" + trigger
			}
		}
	}
	path := filepath.Join(paths.NewLayout(nil, workDir).StateDir, fmt.Sprintf("%d.lifecycle-budget.json", prNumber))
	var budget remediationBudget
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		if err := json.Unmarshal(data, &budget); err != nil || budget.Protocol != "lifecycle-budget/v1" || budget.PullRequest != int(prNumber) || budget.Attempts == nil {
			return fmt.Errorf("remediation budget state is invalid")
		}
	}
	if os.IsNotExist(err) || !strings.EqualFold(budget.HeadSHA, head) {
		budget = remediationBudget{Protocol: "lifecycle-budget/v1", PullRequest: int(prNumber), HeadSHA: head, Attempts: map[string]int{}}
	}
	if budget.Attempts[scope] >= s.resumeCapFor() {
		return errRemediationBudgetExhausted
	}
	if budget.Attempts[scope] < 0 {
		return fmt.Errorf("remediation attempt count is invalid")
	}
	budget.Attempts[scope]++
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return atomicfs.WriteAtomicJSON(path, budget, 0o600)
}

func remediationNumber(value any) int {
	if n, ok := value.(int); ok {
		return n
	}
	n, _ := lifecycleDeadlineSeconds(value)
	return int(n)
}

func remediationReservationFailure(extras map[string]any, err error) map[string]any {
	gate, _ := extras["gate"].(string)
	result := remediationBudgetFailureEvidence(gate, extras, "advance the relevant PR head or confirmed request before attempting further autonomous repair")
	if !errors.Is(err, errRemediationBudgetExhausted) {
		result["reason"] = "REMEDIATION_STATE_ERROR"
		result["next_action"] = "repair persisted remediation evidence before resuming this operation"
		result["budget_error"] = err.Error()
	}
	return result
}
