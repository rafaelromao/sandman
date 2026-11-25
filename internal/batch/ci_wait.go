package batch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	Repository  string                 `json:"repository"`
	PullRequest int                    `json:"pull_request"`
	BaseRefName string                 `json:"base_ref_name"`
	BaseSHA     string                 `json:"base_sha"`
	HeadSHA     string                 `json:"head_sha"`
	CheckLinks  []ciWaitCheckExecution `json:"check_links,omitempty"`
	WorkflowID  int64                  `json:"workflow_id"`
	Event       string                 `json:"event"`
	RunID       int64                  `json:"run_id"`
	RunNumber   int64                  `json:"run_number"`
	Attempt     int                    `json:"attempt"`
	Status      string                 `json:"status"`
	Conclusion  string                 `json:"conclusion"`
	Replacement bool                   `json:"replacement"`
}

type ciWaitCheckExecution struct {
	Name       string `json:"name"`
	DetailsURL string `json:"details_url"`
	Repository string `json:"repository"`
	RunID      int64  `json:"run_id"`
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
		missing := os.IsNotExist(err)
		if err == nil {
			if _, validationErr := ciWaitEvidenceFromRegistration(registration, pr.Number); validationErr != nil {
				return validationErr
			}
		}
		registration, writeReason, err := nextCIWaitRegistration(
			registration, missing, candidate, pr, headSHA, s.runtimeNow,
		)
		if err != nil {
			return err
		}
		if writeReason != "" {
			if writeReason == "write CI wait state" {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					return fmt.Errorf("create CI wait state directory: %w", err)
				}
			}
			if err := atomicfs.WriteAtomicJSON(path, registration, 0o600); err != nil {
				return fmt.Errorf("%s: %w", writeReason, err)
			}
		}
		evidence, err = ciWaitEvidenceFromRegistration(registration, pr.Number)
		return err
	})
	return evidence, err
}

func nextCIWaitRegistration(
	registration ciWaitRegistration,
	missing bool,
	candidate *ciWaitActionsExecution,
	pr *github.PR,
	headSHA string,
	now func() time.Time,
) (ciWaitRegistration, string, error) {
	selection, err := selectCIWaitActions(registration, missing, candidate, pr, headSHA)
	if err != nil {
		return ciWaitRegistration{}, "", err
	}
	if selection.missing || selection.newHead || selection.newExecution {
		return newCIWaitRegistration(
			registration, selection.missing, selection.newHead, selection.selected, pr, headSHA, now(),
		), "write CI wait state", nil
	}
	if selection.selected != nil && registration.ActionsExecution == nil {
		return bindCIWaitActionsExecution(registration, selection.selected, pr, now()), "write CI wait state", nil
	}
	if registration.ActionsExecution != nil {
		// Visible PR-rollup links can arrive after the verified run identity.
		// Retain those exact links without creating a new wait generation or
		// renewing its deadline; once observed they remain useful across restart
		// even if GitHub's rollup later drops or replaces them.
		retained := *registration.ActionsExecution
		links := mergeCIWaitCheckLinks(
			retained.CheckLinks,
			ciWaitCheckLinksForExecution(pr, &retained),
		)
		changed := !sameCIWaitCheckLinks(retained.CheckLinks, links)
		retained.CheckLinks = links
		if candidate != nil &&
			compareCIWaitActionsExecution(candidate, registration.ActionsExecution) == 0 &&
			managedCIExecutionStatusAdvances(candidate, registration.ActionsExecution) {
			// Status can advance while identity and deadline remain fixed.
			retained.Status = candidate.Status
			retained.Conclusion = candidate.Conclusion
			changed = true
		}
		if changed {
			registration.ActionsExecution = &retained
			return registration, "update CI wait observation", nil
		}
	}
	return registration, "", nil
}

type ciWaitActionsSelection struct {
	selected     *ciWaitActionsExecution
	missing      bool
	newHead      bool
	newExecution bool
}

func selectCIWaitActions(
	registration ciWaitRegistration,
	missing bool,
	candidate *ciWaitActionsExecution,
	pr *github.PR,
	headSHA string,
) (ciWaitActionsSelection, error) {
	selection := ciWaitActionsSelection{
		selected: candidate, missing: missing,
		newHead: !missing && !strings.EqualFold(registration.HeadSHA, headSHA),
	}
	if selection.newHead {
		return selection, nil
	}
	if candidate != nil && registration.ActionsExecution != nil {
		switch compareCIWaitActionsExecution(candidate, registration.ActionsExecution) {
		case 1:
			selection.newExecution = true
		case 0:
			// Re-observation of the same verified Actions execution keeps
			// its original bound.
		case -1:
			// A stale provider listing cannot roll a durable wait back.
			selection.selected = registration.ActionsExecution
		case 2:
			return ciWaitActionsSelection{}, fmt.Errorf("CI wait state: verified Actions execution conflicts with the retained workflow identity")
		}
	} else if candidate != nil {
		selection.newExecution = registration.ActionsExecution == nil
	}
	if candidate == nil && registration.ActionsExecution != nil {
		// Once a read-only Actions identity has established the current
		// wait generation, the PR rollup's check links may lag or remain
		// stale. Only another verified Actions identity may advance it.
		selection.selected = registration.ActionsExecution
	}
	if selection.selected == nil && registration.ActionsExecution == nil {
		// Preserve legacy current-head check identity behavior until a
		// verified Actions execution has bound this wait.
		selection.newExecution = ciActive(pr, headSHA) && pr.CIExecutionID != "" &&
			registration.ExecutionID != pr.CIExecutionID
	}
	return selection, nil
}

func newCIWaitRegistration(
	registration ciWaitRegistration,
	missing bool,
	newHead bool,
	selectedActions *ciWaitActionsExecution,
	pr *github.PR,
	headSHA string,
	now time.Time,
) ciWaitRegistration {
	var sourceRunID int64
	if pr.CIFailedWorkflow != nil {
		sourceRunID, _ = strconv.ParseInt(strings.TrimSpace(pr.CIFailedWorkflow.RunID), 10, 64)
	}
	sourceRunIDs := []int64(nil)
	if !missing && !newHead {
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
	return registration
}

func bindCIWaitActionsExecution(
	registration ciWaitRegistration,
	selectedActions *ciWaitActionsExecution,
	pr *github.PR,
	now time.Time,
) ciWaitRegistration {
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
	return registration
}

func ciWaitActionsExecutionFromExecution(execution *github.ManagedCIExecution, pr *github.PR, headSHA string) *ciWaitActionsExecution {
	if execution == nil || pr == nil || execution.RunID <= 0 || execution.RunNumber <= 0 ||
		execution.WorkflowID <= 0 || execution.Attempt <= 0 || strings.TrimSpace(execution.Repository) == "" ||
		!strings.EqualFold(strings.TrimSpace(execution.Event), "pull_request") ||
		!validCIWaitActionsStatus(execution.Status, execution.Conclusion) ||
		pr.Number != execution.PullRequest || strings.TrimSpace(pr.BaseRefName) != strings.TrimSpace(execution.BaseRefName) ||
		!strings.EqualFold(strings.TrimSpace(pr.BaseRefOid), strings.TrimSpace(execution.BaseSHA)) ||
		!strings.EqualFold(strings.TrimSpace(execution.HeadSHA), strings.TrimSpace(headSHA)) ||
		!strings.EqualFold(strings.TrimSpace(pr.HeadRefOid), strings.TrimSpace(headSHA)) {
		return nil
	}
	return &ciWaitActionsExecution{
		Repository: strings.TrimSpace(execution.Repository), PullRequest: execution.PullRequest,
		BaseRefName: strings.TrimSpace(execution.BaseRefName), BaseSHA: strings.TrimSpace(execution.BaseSHA),
		HeadSHA: strings.TrimSpace(execution.HeadSHA),
		CheckLinks: ciWaitCheckLinksForExecution(pr, &ciWaitActionsExecution{
			Repository: strings.TrimSpace(execution.Repository), RunID: execution.RunID,
		}),
		WorkflowID: execution.WorkflowID, Event: strings.ToLower(strings.TrimSpace(execution.Event)),
		RunID: execution.RunID, RunNumber: execution.RunNumber, Attempt: execution.Attempt,
		Status:     strings.ToLower(strings.TrimSpace(execution.Status)),
		Conclusion: strings.ToLower(strings.TrimSpace(execution.Conclusion)), Replacement: true,
	}
}

func ciWaitCheckLinksForExecution(pr *github.PR, execution *ciWaitActionsExecution) []ciWaitCheckExecution {
	if pr == nil || execution == nil || execution.RunID <= 0 ||
		strings.TrimSpace(execution.Repository) == "" {
		return nil
	}
	var links []ciWaitCheckExecution
	for _, check := range pr.CIActiveWorkflowChecks {
		if check.RunID != execution.RunID ||
			!strings.EqualFold(strings.TrimSpace(check.Repository), strings.TrimSpace(execution.Repository)) ||
			strings.TrimSpace(check.Name) == "" || strings.TrimSpace(check.DetailsURL) == "" {
			continue
		}
		links = append(links, ciWaitCheckExecution{
			Name: strings.TrimSpace(check.Name), DetailsURL: strings.TrimSpace(check.DetailsURL),
			Repository: strings.TrimSpace(check.Repository), RunID: check.RunID,
		})
	}
	return mergeCIWaitCheckLinks(nil, links)
}

func mergeCIWaitCheckLinks(existing, additions []ciWaitCheckExecution) []ciWaitCheckExecution {
	byURL := make(map[string]ciWaitCheckExecution, len(existing)+len(additions))
	for _, link := range append(append([]ciWaitCheckExecution(nil), existing...), additions...) {
		if link.RunID <= 0 || strings.TrimSpace(link.Name) == "" ||
			strings.TrimSpace(link.DetailsURL) == "" || strings.TrimSpace(link.Repository) == "" {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(link.DetailsURL))
		if prior, exists := byURL[key]; exists &&
			(prior.RunID != link.RunID ||
				!strings.EqualFold(prior.Repository, link.Repository) ||
				prior.Name != link.Name) {
			// The same provider URL cannot safely identify two different jobs.
			delete(byURL, key)
			continue
		}
		byURL[key] = link
	}
	links := make([]ciWaitCheckExecution, 0, len(byURL))
	for _, link := range byURL {
		links = append(links, link)
	}
	sort.Slice(links, func(i, j int) bool {
		return strings.ToLower(links[i].DetailsURL) < strings.ToLower(links[j].DetailsURL)
	})
	return links
}

func sameCIWaitCheckLinks(a, b []ciWaitCheckExecution) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
	if strings.TrimSpace(candidate.BaseRefName) != strings.TrimSpace(retained.BaseRefName) ||
		!strings.EqualFold(strings.TrimSpace(candidate.BaseSHA), strings.TrimSpace(retained.BaseSHA)) {
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
			registration.ActionsExecution.BaseSHA == "" ||
			registration.ActionsExecution.WorkflowID <= 0 || !strings.EqualFold(registration.ActionsExecution.Event, "pull_request") ||
			registration.ActionsExecution.RunID <= 0 ||
			registration.ActionsExecution.RunNumber <= 0 || registration.ActionsExecution.Attempt <= 0 ||
			!registration.ActionsExecution.Replacement ||
			!validCIWaitActionsStatus(registration.ActionsExecution.Status, registration.ActionsExecution.Conclusion) ||
			!validCIWaitCheckLinks(registration.ActionsExecution.CheckLinks, registration.ActionsExecution) ||
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
		execution := map[string]any{
			"repository": registration.ActionsExecution.Repository, "pull_request": registration.ActionsExecution.PullRequest,
			"base_ref_name": registration.ActionsExecution.BaseRefName,
			"base_sha":      registration.ActionsExecution.BaseSHA, "head_sha": registration.ActionsExecution.HeadSHA,
			"workflow_id": registration.ActionsExecution.WorkflowID, "event": registration.ActionsExecution.Event,
			"run_id":     registration.ActionsExecution.RunID,
			"run_number": registration.ActionsExecution.RunNumber, "attempt": registration.ActionsExecution.Attempt,
			"status": registration.ActionsExecution.Status, "conclusion": registration.ActionsExecution.Conclusion,
			"replacement": registration.ActionsExecution.Replacement,
		}
		if len(registration.ActionsExecution.CheckLinks) > 0 {
			links := make([]any, 0, len(registration.ActionsExecution.CheckLinks))
			for _, link := range registration.ActionsExecution.CheckLinks {
				links = append(links, map[string]any{
					"name": link.Name, "details_url": link.DetailsURL,
					"repository": link.Repository, "run_id": link.RunID,
				})
			}
			execution["check_links"] = links
		}
		wait["actions_execution"] = execution
	}
	return map[string]any{"ci_wait": wait}, nil
}

func validCIWaitCheckLinks(links []ciWaitCheckExecution, execution *ciWaitActionsExecution) bool {
	if execution == nil {
		return len(links) == 0
	}
	seen := make(map[string]struct{}, len(links))
	for _, link := range links {
		key := strings.ToLower(strings.TrimSpace(link.DetailsURL))
		if key == "" || strings.TrimSpace(link.Name) == "" ||
			!strings.EqualFold(strings.TrimSpace(link.Repository), strings.TrimSpace(execution.Repository)) ||
			link.RunID != execution.RunID {
			return false
		}
		if _, duplicate := seen[key]; duplicate {
			return false
		}
		seen[key] = struct{}{}
	}
	return true
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
