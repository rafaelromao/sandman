package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const managedCIPageSize = 100

var runnerAcquisitionAnnotation = regexp.MustCompile(`(?i)\bthe job was not acquired by a runner\b|\bjob was not acquired by a runner\b`)
var failedPrerequisiteAnnotation = regexp.MustCompile(`(?i)^([a-z0-9][a-z0-9_.-]*) must succeed \(result: ([^)]+)\)$`)
var managedCITestStepName = regexp.MustCompile(
	`(?i)^(?:(?:run|execute)\s+)?` +
		`(?:go\s+test|pytest|python(?:3(?:\.\d+)?)?\s+-m\s+(?:pytest|unittest)|` +
		`(?:npm|pnpm|yarn)\s+(?:run\s+)?test|bun\s+test|cargo\s+test|dotnet\s+test|` +
		`mvn\s+test|gradlew?\s+test|make\s+test)(?:\s|$)|` +
		`^(?:(?:run|execute)\s+)?(?:(?:unit|integration|smoke|e2e|acceptance|regression)\s+)?tests?(?:\s|$)`,
)
var managedCITestDiagnostic = regexp.MustCompile(
	`(?i)\btest\w*\b.*\b(?:fail|error|panic)\w*\b|` +
		`\b(?:assert(?:ion)?|expected|actual|got|want)\b|` +
		`\b(?:undefined:|cannot use|syntax error|declared and not used|imported and not used|` +
		`not enough arguments|too many arguments|cannot find module|failed to compile|compilation failed)\b`,
)

type actionsWorkflowRun struct {
	ID           int64  `json:"id"`
	RunNumber    int64  `json:"run_number"`
	RunAttempt   int    `json:"run_attempt"`
	WorkflowID   int64  `json:"workflow_id"`
	Event        string `json:"event"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	HeadSHA      string `json:"head_sha"`
	PullRequests []struct {
		Number int `json:"number"`
		Base   struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_requests"`
}

type actionsWorkflowRunsPage struct {
	TotalCount   int                  `json:"total_count"`
	WorkflowRuns []actionsWorkflowRun `json:"workflow_runs"`
}

type checkRunAnnotation struct {
	Level     string `json:"annotation_level"`
	Message   string `json:"message"`
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
}

type checkRunAnnotationsPage []checkRunAnnotation

type actionsJobDetailsURL struct {
	RunID int64
	JobID int64
}

// ObserveManagedCI reads the current failed workflow, its job-level check
// annotations, and same-head executions of the same workflow. It has no
// Actions mutation calls; any missing or contradictory identity fails closed.
func (c *CLIClient) ObserveManagedCI(ctx context.Context, request ManagedCIRequest) (ManagedCIObservation, error) {
	request.Repository = strings.TrimSpace(request.Repository)
	request.BaseRefName = strings.TrimSpace(request.BaseRefName)
	request.HeadSHA = strings.TrimSpace(request.HeadSHA)
	request.FailedWorkflowRunID = strings.TrimSpace(request.FailedWorkflowRunID)
	if request.PullRequest <= 0 || request.Repository == "" || request.BaseRefName == "" ||
		request.HeadSHA == "" || len(request.FailedChecks) == 0 {
		return ManagedCIObservation{}, fmt.Errorf("managed CI observation: incomplete pull request or failed-check identity")
	}
	runID, err := strconv.ParseInt(request.FailedWorkflowRunID, 10, 64)
	if err != nil || runID <= 0 {
		return ManagedCIObservation{}, fmt.Errorf("managed CI observation: invalid workflow run ID %q", request.FailedWorkflowRunID)
	}
	owner, repo, err := c.resolveRepo(ctx)
	if err != nil {
		return ManagedCIObservation{}, fmt.Errorf("managed CI observation: resolve repository: %w", err)
	}
	repository := owner + "/" + repo
	if !strings.EqualFold(request.Repository, repository) {
		return ManagedCIObservation{}, fmt.Errorf("managed CI observation: repository mismatch")
	}

	jobs, err := c.listManagedWorkflowJobs(ctx, owner, repo, runID)
	if err != nil {
		return ManagedCIObservation{}, fmt.Errorf("managed CI observation: list source jobs: %w", err)
	}
	jobsByID := make(map[int64]CIWorkflowJob, len(jobs))
	for _, job := range jobs {
		if _, duplicate := jobsByID[job.ID]; duplicate {
			return ManagedCIObservation{}, fmt.Errorf("managed CI observation: duplicate source job identity")
		}
		jobsByID[job.ID] = job
	}

	selectedJobs := make([]CIWorkflowJob, 0, len(request.FailedChecks))
	sourceAttempt := 0
	for _, check := range request.FailedChecks {
		identity, ok := parseManagedActionsJobURL(check.DetailsURL, request.Repository)
		if !ok || identity.RunID != runID {
			return ManagedCIObservation{}, fmt.Errorf("managed CI observation: failed check link is not bound to the source run")
		}
		job, exists := jobsByID[identity.JobID]
		if !exists || !strings.EqualFold(strings.TrimSpace(job.Name), strings.TrimSpace(check.Name)) ||
			job.RunID != runID || job.RunAttempt <= 0 || strings.TrimSpace(job.CheckRunURL) == "" {
			return ManagedCIObservation{}, fmt.Errorf("managed CI observation: failed check has no matching current-attempt job")
		}
		if sourceAttempt == 0 {
			sourceAttempt = job.RunAttempt
		} else if sourceAttempt != job.RunAttempt {
			return ManagedCIObservation{}, fmt.Errorf("managed CI observation: failed checks identify conflicting workflow attempts")
		}
		selectedJobs = append(selectedJobs, job)
	}
	sourceRun, err := c.fetchManagedWorkflowRunAttempt(ctx, owner, repo, runID, sourceAttempt)
	if err != nil {
		return ManagedCIObservation{}, err
	}
	source, identityOK := managedCIExecution(sourceRun, repository, request)
	if !identityOK || source.RunID != runID || source.Attempt != sourceAttempt ||
		!strings.EqualFold(strings.TrimSpace(sourceRun.Event), "pull_request") {
		return ManagedCIObservation{}, fmt.Errorf("managed CI observation: source workflow identity is incomplete or conflicting")
	}

	observation := ManagedCIObservation{Source: source, Failures: make([]ManagedCIFailureEvidence, 0, len(request.FailedChecks))}
	verifiedShortage := false
	for index, check := range request.FailedChecks {
		job := selectedJobs[index]
		checkRunID, ok := managedCheckRunID(job.CheckRunURL, request.Repository)
		if !ok {
			return ManagedCIObservation{}, fmt.Errorf("managed CI observation: job check-run identity is invalid")
		}
		annotations, err := c.listManagedCheckRunAnnotations(ctx, owner, repo, checkRunID)
		if err != nil {
			return ManagedCIObservation{}, fmt.Errorf("managed CI observation: list check-run annotations: %w", err)
		}
		classification, causedByChecks := classifyManagedCIFailureEvidence(job, annotations)
		if classification == ManagedCIRunnerShortage {
			verifiedShortage = true
		}
		observation.Failures = append(observation.Failures, ManagedCIFailureEvidence{
			JobID: job.ID, RunID: job.RunID, Attempt: job.RunAttempt,
			CheckName: strings.TrimSpace(check.Name), DetailsURL: strings.TrimSpace(check.DetailsURL),
			Classification: classification, CausedByChecks: causedByChecks,
		})
	}
	if verifiedShortage {
		replacement, err := c.findManagedCIReplacement(ctx, owner, repo, source)
		if err != nil {
			// The source identity and failures above are fully validated even
			// though replacement identity is not. Preserve them so a confirmed
			// independent source failure remains actionable while replacement
			// selection is treated as unknown by the lifecycle.
			return observation, err
		}
		observation.Replacement = replacement
		if replacement != nil && replacement.Status == "completed" && replacement.Conclusion != "success" {
			failures, err := c.diagnoseManagedCIExecutionFailure(ctx, owner, repo, request, *replacement)
			if err != nil {
				// Keep the verified source evidence and replacement identity;
				// only the replacement's failure diagnosis is inconclusive.
				observation.Replacement = replacement
				return observation, err
			}
			observation.ReplacementFailures = failures
		}
	}
	return observation, nil
}

func (c *CLIClient) fetchManagedWorkflowRunAttempt(ctx context.Context, owner, repo string, runID int64, attempt int) (actionsWorkflowRun, error) {
	if attempt <= 0 {
		return actionsWorkflowRun{}, fmt.Errorf("managed CI observation: invalid source attempt")
	}
	path := fmt.Sprintf("repos/%s/%s/actions/runs/%d/attempts/%d", owner, repo, runID, attempt)
	out, err := c.readCommand(ctx, "gh api workflow run attempt", "api", path)
	if err != nil {
		return actionsWorkflowRun{}, fmt.Errorf("gh api workflow run attempt: %w", err)
	}
	var run actionsWorkflowRun
	if err := json.Unmarshal(out, &run); err != nil {
		return actionsWorkflowRun{}, fmt.Errorf("parse workflow run attempt: %w", err)
	}
	return run, nil
}

func (c *CLIClient) listManagedWorkflowJobs(ctx context.Context, owner, repo string, runID int64) ([]CIWorkflowJob, error) {
	const maxPages = 10
	var jobs []CIWorkflowJob
	totalCount := -1
	for page := 1; page <= maxPages; page++ {
		path := fmt.Sprintf("repos/%s/%s/actions/runs/%d/jobs?filter=all&per_page=%d&page=%d",
			owner, repo, runID, managedCIPageSize, page)
		out, err := c.readCommand(ctx, "gh api all workflow jobs", "api", path)
		if err != nil {
			return nil, err
		}
		var response struct {
			TotalCount int             `json:"total_count"`
			Jobs       []CIWorkflowJob `json:"jobs"`
		}
		if err := json.Unmarshal(out, &response); err != nil {
			return nil, fmt.Errorf("parse all workflow jobs: %w", err)
		}
		if totalCount >= 0 && response.TotalCount != totalCount {
			return nil, fmt.Errorf("workflow job list changed during pagination")
		}
		totalCount = response.TotalCount
		jobs = append(jobs, response.Jobs...)
		if len(jobs) >= totalCount {
			break
		}
		if len(response.Jobs) < managedCIPageSize {
			return nil, fmt.Errorf("workflow job list is incomplete")
		}
	}
	if totalCount < 0 || len(jobs) != totalCount {
		return nil, fmt.Errorf("workflow job list is incomplete")
	}
	for _, job := range jobs {
		if job.ID <= 0 || job.RunID != runID || job.RunAttempt <= 0 || strings.TrimSpace(job.Name) == "" {
			return nil, fmt.Errorf("workflow job list has incomplete or foreign identity")
		}
	}
	return jobs, nil
}

func (c *CLIClient) diagnoseManagedCIExecutionFailure(
	ctx context.Context,
	owner, repo string,
	request ManagedCIRequest,
	execution ManagedCIExecution,
) ([]ManagedCIFailureEvidence, error) {
	run, err := c.fetchManagedWorkflowRunAttempt(ctx, owner, repo, execution.RunID, execution.Attempt)
	if err != nil {
		return nil, err
	}
	verified, ok := managedCIExecution(run, execution.Repository, request)
	if !ok || verified.RunID != execution.RunID || verified.RunNumber != execution.RunNumber ||
		verified.WorkflowID != execution.WorkflowID || verified.Attempt != execution.Attempt {
		return nil, fmt.Errorf("managed CI observation: completed replacement identity changed during diagnosis")
	}
	jobs, err := c.listManagedWorkflowJobs(ctx, owner, repo, execution.RunID)
	if err != nil {
		return nil, fmt.Errorf("managed CI observation: list replacement jobs: %w", err)
	}
	var failures []ManagedCIFailureEvidence
	for _, job := range jobs {
		if job.RunAttempt != execution.Attempt || !strings.EqualFold(strings.TrimSpace(job.Status), "completed") {
			continue
		}
		conclusion := strings.ToLower(strings.TrimSpace(job.Conclusion))
		if conclusion == "" || conclusion == "success" || conclusion == "skipped" || conclusion == "neutral" {
			continue
		}
		checkRunID, ok := managedCheckRunID(job.CheckRunURL, request.Repository)
		if !ok {
			return nil, fmt.Errorf("managed CI observation: completed replacement job has no valid check-run identity")
		}
		annotations, err := c.listManagedCheckRunAnnotations(ctx, owner, repo, checkRunID)
		if err != nil {
			return nil, fmt.Errorf("managed CI observation: list replacement annotations: %w", err)
		}
		classification, causedByChecks := classifyManagedCIFailureEvidence(job, annotations)
		failures = append(failures, ManagedCIFailureEvidence{
			JobID: job.ID, RunID: job.RunID, Attempt: job.RunAttempt, CheckName: strings.TrimSpace(job.Name),
			Classification: classification, CausedByChecks: causedByChecks,
		})
	}
	if len(failures) == 0 {
		return nil, fmt.Errorf("managed CI observation: completed failed replacement has no diagnosed failed job")
	}
	return failures, nil
}

func managedCIExecution(run actionsWorkflowRun, repository string, request ManagedCIRequest) (ManagedCIExecution, bool) {
	if run.ID <= 0 || run.RunNumber <= 0 || run.RunAttempt <= 0 || run.WorkflowID <= 0 ||
		strings.TrimSpace(run.HeadSHA) == "" || !ValidManagedCIExecutionStatus(run.Status, run.Conclusion) {
		return ManagedCIExecution{}, false
	}
	if !strings.EqualFold(strings.TrimSpace(run.HeadSHA), request.HeadSHA) {
		return ManagedCIExecution{}, false
	}
	matches := 0
	for _, pr := range run.PullRequests {
		if pr.Number == request.PullRequest &&
			strings.EqualFold(strings.TrimSpace(pr.Base.Ref), request.BaseRefName) &&
			strings.EqualFold(strings.TrimSpace(pr.Head.SHA), request.HeadSHA) {
			matches++
			continue
		}
		if pr.Number == request.PullRequest {
			return ManagedCIExecution{}, false
		}
	}
	if matches != 1 {
		return ManagedCIExecution{}, false
	}
	return ManagedCIExecution{
		Repository: repository, PullRequest: request.PullRequest, BaseRefName: request.BaseRefName,
		HeadSHA: request.HeadSHA, WorkflowID: run.WorkflowID,
		Event: strings.ToLower(strings.TrimSpace(run.Event)), RunID: run.ID, RunNumber: run.RunNumber,
		Attempt: run.RunAttempt, Status: strings.ToLower(strings.TrimSpace(run.Status)),
		Conclusion: strings.ToLower(strings.TrimSpace(run.Conclusion)),
	}, true
}

// ValidManagedCIExecutionStatus reports whether an Actions execution status
// and conclusion are a recognized, internally consistent pair.
func ValidManagedCIExecutionStatus(status, conclusion string) bool {
	status = strings.ToLower(strings.TrimSpace(status))
	conclusion = strings.ToLower(strings.TrimSpace(conclusion))
	switch status {
	case "queued", "in_progress", "waiting", "requested", "pending":
		return conclusion == ""
	case "completed":
		switch conclusion {
		case "action_required", "cancelled", "failure", "neutral", "success", "skipped", "stale", "startup_failure", "timed_out":
			return true
		}
	}
	return false
}

func parseManagedActionsJobURL(value, repository string) (actionsWorkflowJobDetails, bool) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || !strings.EqualFold(parsed.Host, "github.com") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return actionsWorkflowJobDetails{}, false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	repoParts := strings.Split(repository, "/")
	if len(parts) != 7 || len(repoParts) != 2 || !strings.EqualFold(parts[0], repoParts[0]) ||
		!strings.EqualFold(parts[1], repoParts[1]) || parts[2] != "actions" || parts[3] != "runs" || parts[5] != "job" {
		return actionsWorkflowJobDetails{}, false
	}
	runID, runErr := strconv.ParseInt(parts[4], 10, 64)
	jobID, jobErr := strconv.ParseInt(parts[6], 10, 64)
	if runErr != nil || jobErr != nil || runID <= 0 || jobID <= 0 {
		return actionsWorkflowJobDetails{}, false
	}
	return actionsWorkflowJobDetails{RunID: runID, JobID: jobID}, true
}

type actionsWorkflowJobDetails struct {
	RunID int64
	JobID int64
}

func managedCheckRunID(value, repository string) (int64, bool) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || !strings.EqualFold(parsed.Host, "api.github.com") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return 0, false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	repoParts := strings.Split(repository, "/")
	if len(parts) != 5 || len(repoParts) != 2 || parts[0] != "repos" ||
		!strings.EqualFold(parts[1], repoParts[0]) || !strings.EqualFold(parts[2], repoParts[1]) || parts[3] != "check-runs" {
		return 0, false
	}
	id, err := strconv.ParseInt(parts[4], 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func (c *CLIClient) listManagedCheckRunAnnotations(ctx context.Context, owner, repo string, checkRunID int64) ([]checkRunAnnotation, error) {
	const pageSize = 100
	const maxPages = 10
	var annotations []checkRunAnnotation
	for page := 1; page <= maxPages; page++ {
		path := fmt.Sprintf("repos/%s/%s/check-runs/%d/annotations?per_page=%d&page=%d", owner, repo, checkRunID, pageSize, page)
		out, err := c.readCommand(ctx, "gh api check-run annotations", "api", path)
		if err != nil {
			return nil, err
		}
		var current checkRunAnnotationsPage
		if err := json.Unmarshal(out, &current); err != nil {
			return nil, fmt.Errorf("parse check-run annotations: %w", err)
		}
		annotations = append(annotations, current...)
		if len(current) < pageSize {
			return annotations, nil
		}
	}
	return nil, fmt.Errorf("check-run annotations are incomplete")
}

func classifyManagedCIFailure(job CIWorkflowJob, annotations []checkRunAnnotation) ManagedCIFailureClassification {
	classification, _ := classifyManagedCIFailureEvidence(job, annotations)
	return classification
}

func classifyManagedCIFailureEvidence(job CIWorkflowJob, annotations []checkRunAnnotation) (ManagedCIFailureClassification, []string) {
	if strings.EqualFold(strings.TrimSpace(job.Status), "completed") &&
		strings.EqualFold(strings.TrimSpace(job.Conclusion), "cancelled") &&
		job.RunnerID == 0 && strings.TrimSpace(job.RunnerName) == "" && len(job.Steps) == 0 {
		for _, annotation := range annotations {
			if strings.EqualFold(strings.TrimSpace(annotation.Level), "failure") &&
				runnerAcquisitionAnnotation.MatchString(strings.TrimSpace(annotation.Message)) {
				return ManagedCIRunnerShortage, nil
			}
		}
	}
	if strings.EqualFold(strings.TrimSpace(job.Status), "completed") &&
		strings.EqualFold(strings.TrimSpace(job.Conclusion), "failure") {
		var causes []string
		dependentAnnotationsOnly := true
		for _, annotation := range annotations {
			if !strings.EqualFold(strings.TrimSpace(annotation.Level), "failure") {
				continue
			}
			match := failedPrerequisiteAnnotation.FindStringSubmatch(strings.TrimSpace(annotation.Message))
			if len(match) != 3 || strings.EqualFold(strings.TrimSpace(match[2]), "success") {
				dependentAnnotationsOnly = false
				break
			}
			causes = append(causes, strings.TrimSpace(match[1]))
		}
		if dependentAnnotationsOnly && len(causes) > 0 {
			return ManagedCIDependentFailure, causes
		}
	}
	if strings.EqualFold(strings.TrimSpace(job.Status), "completed") &&
		strings.EqualFold(strings.TrimSpace(job.Conclusion), "failure") {
		// A source annotation alone does not prove a code/test failure: tie it
		// to a failed test step and a diagnostic that describes that failure.
		failedTestStep := false
		for _, step := range job.Steps {
			if strings.EqualFold(strings.TrimSpace(step.Status), "completed") &&
				strings.EqualFold(strings.TrimSpace(step.Conclusion), "failure") &&
				managedCITestStepName.MatchString(strings.TrimSpace(step.Name)) {
				failedTestStep = true
				break
			}
		}
		if failedTestStep {
			for _, annotation := range annotations {
				if strings.EqualFold(strings.TrimSpace(annotation.Level), "failure") &&
					strings.TrimSpace(annotation.Path) != "" && annotation.StartLine > 0 &&
					managedCITestDiagnostic.MatchString(strings.TrimSpace(annotation.Message)) {
					return ManagedCIConfirmedCodeTestFailure, nil
				}
			}
		}
	}
	return ManagedCIUnexplainedFailure, nil
}

func (c *CLIClient) findManagedCIReplacement(ctx context.Context, owner, repo string, source ManagedCIExecution) (*ManagedCIExecution, error) {
	const maxPages = 10
	var runs []actionsWorkflowRun
	totalCount := -1
	for page := 1; page <= maxPages; page++ {
		path := fmt.Sprintf("repos/%s/%s/actions/workflows/%d/runs?event=pull_request&head_sha=%s&per_page=%d&page=%d",
			owner, repo, source.WorkflowID, url.QueryEscape(source.HeadSHA), managedCIPageSize, page)
		out, err := c.readCommand(ctx, "gh api workflow runs", "api", path)
		if err != nil {
			return nil, fmt.Errorf("gh api workflow runs: %w", err)
		}
		var response actionsWorkflowRunsPage
		if err := json.Unmarshal(out, &response); err != nil {
			return nil, fmt.Errorf("parse workflow runs: %w", err)
		}
		if totalCount >= 0 && response.TotalCount != totalCount {
			return nil, fmt.Errorf("workflow-run list changed during pagination")
		}
		totalCount = response.TotalCount
		runs = append(runs, response.WorkflowRuns...)
		if len(runs) >= totalCount {
			break
		}
		if len(response.WorkflowRuns) < managedCIPageSize {
			return nil, fmt.Errorf("workflow-run list is incomplete")
		}
	}
	if totalCount < 0 || len(runs) != totalCount {
		return nil, fmt.Errorf("workflow-run list is incomplete")
	}
	var selected *ManagedCIExecution
	runNumbers := make(map[int64]int64, len(runs))
	runIDs := make(map[int64]int64, len(runs))
	for _, run := range runs {
		if run.ID <= 0 || run.RunNumber <= 0 || run.RunAttempt <= 0 ||
			strings.TrimSpace(run.Event) == "" || !ValidManagedCIExecutionStatus(run.Status, run.Conclusion) ||
			strings.TrimSpace(run.HeadSHA) == "" || run.WorkflowID <= 0 {
			return nil, fmt.Errorf("workflow-run list contains incomplete execution identity")
		}
		if run.WorkflowID != source.WorkflowID ||
			!strings.EqualFold(strings.TrimSpace(run.Event), "pull_request") ||
			!strings.EqualFold(strings.TrimSpace(run.HeadSHA), source.HeadSHA) {
			return nil, fmt.Errorf("workflow-run list contradicts its workflow, event, or head filter")
		}
		if previous, exists := runNumbers[run.RunNumber]; exists && previous != run.ID {
			return nil, fmt.Errorf("workflow-run list reuses a run number for different run IDs")
		}
		runNumbers[run.RunNumber] = run.ID
		if previous, exists := runIDs[run.ID]; exists && previous != run.RunNumber {
			return nil, fmt.Errorf("workflow-run list assigns conflicting run numbers to one run ID")
		}
		runIDs[run.ID] = run.RunNumber
		if run.ID == source.RunID && run.RunNumber != source.RunNumber {
			return nil, fmt.Errorf("workflow-run list contains a contradictory attempt identity")
		}
		isNewer := run.ID == source.RunID && run.RunAttempt > source.Attempt
		if run.ID != source.RunID && run.RunNumber > source.RunNumber {
			isNewer = true
		}
		if !isNewer {
			continue
		}
		if len(run.PullRequests) == 0 {
			return nil, fmt.Errorf("newer workflow run has no pull request identity")
		}
		candidate, ok := managedCIExecution(run, source.Repository, ManagedCIRequest{
			Repository: source.Repository, PullRequest: source.PullRequest, BaseRefName: source.BaseRefName, HeadSHA: source.HeadSHA,
		})
		if !ok {
			return nil, fmt.Errorf("newer workflow run has a conflicting pull request identity")
		}
		if selected != nil {
			if candidate.RunNumber == selected.RunNumber && candidate.Attempt == selected.Attempt && candidate.RunID != selected.RunID {
				return nil, fmt.Errorf("workflow-run list contains contradictory replacement identity")
			}
			if candidate.RunNumber < selected.RunNumber ||
				(candidate.RunNumber == selected.RunNumber && candidate.Attempt <= selected.Attempt) {
				continue
			}
		}
		copy := candidate
		selected = &copy
	}
	return selected, nil
}
