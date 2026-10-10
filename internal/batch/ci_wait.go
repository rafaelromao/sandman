package batch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	Protocol             string `json:"protocol"`
	PullRequest          int    `json:"pull_request"`
	HeadSHA              string `json:"head_sha"`
	ExecutionID          string `json:"execution_id,omitempty"`
	StartedUnixSeconds   int64  `json:"started_unix_seconds"`
	DeadlineUnixSeconds  int64  `json:"deadline_unix_seconds"`
	EffectiveTimeoutSecs int64  `json:"effective_timeout_seconds"`
	RemediationAttempts  int    `json:"remediation_attempts"`
}

func (s *runSession) ciWaitEvidence(workDir string, pr *github.PR, headSHA string) (map[string]any, error) {
	if pr == nil || pr.Number <= 0 || strings.TrimSpace(headSHA) == "" || !strings.EqualFold(strings.TrimSpace(pr.HeadRefOid), strings.TrimSpace(headSHA)) {
		return nil, nil
	}
	path := filepath.Join(paths.NewLayout(nil, workDir).StateDir, fmt.Sprintf("%d.ci_wait.json", pr.Number))
	var evidence map[string]any
	err := withOperationLock(context.Background(), path, func() error {
		registration, err := readCIWaitRegistration(path)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("read CI wait state: %w", err)
		}
		if err == nil {
			if _, validationErr := ciWaitEvidenceFromRegistration(registration, pr.Number); validationErr != nil {
				return validationErr
			}
		}
		// A real rerun is a new external operation even on the same head.
		// Bind legacy head-only state once when execution identity is available;
		// subsequent observations/restarts of that execution retain its deadline.
		newExecution := ciActive(pr, headSHA) && pr.CIExecutionID != "" && registration.ExecutionID != pr.CIExecutionID
		if os.IsNotExist(err) || !strings.EqualFold(registration.HeadSHA, headSHA) || newExecution {
			now := s.runtimeNow()
			registration = ciWaitRegistration{
				Protocol:             ciWaitProtocol,
				PullRequest:          pr.Number,
				HeadSHA:              headSHA,
				ExecutionID:          pr.CIExecutionID,
				StartedUnixSeconds:   now.Unix(),
				DeadlineUnixSeconds:  now.Add(ciWaitTimeout).Unix(),
				EffectiveTimeoutSecs: int64(ciWaitTimeout / time.Second),
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return fmt.Errorf("create CI wait state directory: %w", err)
			}
			if err := atomicfs.WriteAtomicJSON(path, registration, 0o600); err != nil {
				return fmt.Errorf("write CI wait state: %w", err)
			}
		}
		evidence, err = ciWaitEvidenceFromRegistration(registration, pr.Number)
		return err
	})
	return evidence, err
}

func ciWaitEvidenceFromRegistration(registration ciWaitRegistration, prNumber int) (map[string]any, error) {
	if registration.Protocol != ciWaitProtocol || registration.PullRequest != prNumber || registration.HeadSHA == "" || registration.DeadlineUnixSeconds <= registration.StartedUnixSeconds || registration.EffectiveTimeoutSecs <= 0 || registration.DeadlineUnixSeconds-registration.StartedUnixSeconds != registration.EffectiveTimeoutSecs {
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
	if registration.ExecutionID != "" {
		wait["execution_id"] = registration.ExecutionID
	}
	return map[string]any{"ci_wait": wait}, nil
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
