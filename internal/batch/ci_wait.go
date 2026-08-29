package batch

import (
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
	registration, err := readCIWaitRegistration(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read CI wait state: %w", err)
	}
	if os.IsNotExist(err) || !strings.EqualFold(registration.HeadSHA, headSHA) {
		now := time.Now().UTC()
		registration = ciWaitRegistration{
			Protocol:             ciWaitProtocol,
			PullRequest:          pr.Number,
			HeadSHA:              headSHA,
			StartedUnixSeconds:   now.Unix(),
			DeadlineUnixSeconds:  now.Add(ciWaitTimeout).Unix(),
			EffectiveTimeoutSecs: int64(ciWaitTimeout / time.Second),
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("create CI wait state directory: %w", err)
		}
		if err := atomicfs.WriteAtomicJSON(path, registration, 0o600); err != nil {
			return nil, fmt.Errorf("write CI wait state: %w", err)
		}
	}
	return ciWaitEvidenceFromRegistration(registration, pr.Number)
}

func ciWaitEvidenceFromRegistration(registration ciWaitRegistration, prNumber int) (map[string]any, error) {
	if registration.Protocol != ciWaitProtocol || registration.PullRequest != prNumber || registration.DeadlineUnixSeconds <= registration.StartedUnixSeconds || registration.EffectiveTimeoutSecs <= 0 || registration.DeadlineUnixSeconds-registration.StartedUnixSeconds != registration.EffectiveTimeoutSecs {
		return nil, fmt.Errorf("CI wait state is invalid")
	}
	return map[string]any{
		"ci_wait": map[string]any{
			"protocol":                  registration.Protocol,
			"pull_request":              registration.PullRequest,
			"head_sha":                  registration.HeadSHA,
			"started_unix_seconds":      registration.StartedUnixSeconds,
			"deadline_unix_seconds":     registration.DeadlineUnixSeconds,
			"effective_timeout_seconds": registration.EffectiveTimeoutSecs,
			"remediation_attempts":      registration.RemediationAttempts,
		},
	}, nil
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
