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

var runnerAcquisitionAnnotation = regexp.MustCompile(
	`(?i)^(?:the job was not acquired by runner of type hosted even after multiple attempts|` +
		`the job was not acquired by (?:a )?github[- ]hosted runner within 10 minutes of the request)\.?` +
		`(?:\s*job was cancelled\.?)?$`,
)
var selfHostedRunnerAnnotation = regexp.MustCompile(`(?i)\bself[- ]hosted\b`)
var failedPrerequisiteAnnotation = regexp.MustCompile(`(?i)^([a-z0-9][a-z0-9_.-]*) must succeed \(result: ([^)]+)\)$`)
var aggregateExitWrapperAnnotation = regexp.MustCompile(`(?i)^process completed with exit code \d+\.?$`)
var managedCITestStepName = regexp.MustCompile(
	`(?i)^(?:(?:run|execute)\s+)?` +
		`(?:go\s+test|pytest|python(?:3(?:\.\d+)?)?\s+-m\s+(?:pytest|unittest)|` +
		`(?:npm|pnpm|yarn)\s+(?:run\s+)?test|bun\s+test|cargo\s+test|dotnet\s+test|` +
		`mvn\s+test|gradlew?\s+test|make\s+test)(?:\s|$)|` +
		`^(?:(?:run|execute)\s+)?(?:(?:unit|integration|smoke|e2e|acceptance|regression)\s+)?tests?` +
		`(?:\s+(?:suite|all|only))?$`,
)
var managedCICompilerStepName = regexp.MustCompile(
	`(?i)^(?:(?:run|execute)\s+)?(?:go\s+(?:build|vet)|cargo\s+(?:build|check)|` +
		`dotnet\s+build|msbuild|mvn\s+(?:compile|package)|gradlew?\s+(?:build|compile|assemble)|` +
		`(?:npm|pnpm|yarn)\s+(?:run\s+)?build|bun\s+build|tsc|javac|make\s+(?:build|compile)|` +
		`(?:build|compile)(?:\s+(?:project|application|package|source))?)(?:\s|$)`,
)
var managedCITestDiagnostic = regexp.MustCompile(
	`(?i)(?:^|\b)---\s*fail:\s*test\w*\b|` +
		`(?-i:\bTest[A-Z]\w*)\b.{0,160}\b(?:fail|error|panic)\w*\b|` +
		`\bassert(?:ion)?\s+(?:failed|error)\b|` +
		`\b(?:undefined:|cannot use|syntax error|declared and not used|imported and not used|` +
		`not enough arguments|too many arguments|cannot find module|failed to compile|compilation failed)\b`,
)
var managedCICompilerDiagnostic = regexp.MustCompile(
	`(?i)\b(?:undefined:|cannot use\b|syntax error\b|declared and not used\b|imported and not used\b|` +
		`not enough arguments\b|too many arguments\b|cannot find module\b|failed to compile\b|compilation failed\b)`,
)
var managedCIInfrastructureDiagnostic = regexp.MustCompile(
	`(?i)\b(?:dependency mirror|connection refused|connection reset|network timeout|timed out|` +
		`rate limit|service unavailable|HTTP\s+[45]\d{2}\b|DNS lookup|TLS handshake|proxy error|` +
		`artifact|cache|runner|checkout|download|upload|provision(?:ing)?|install|registry|container image|` +
		`fixture|writable volume|read-only filesystem)\b`,
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
			SHA string `json:"sha"`
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

func (p *actionsWorkflowRunsPage) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	totalCountJSON, hasTotalCount := fields["total_count"]
	runsJSON, hasRuns := fields["workflow_runs"]
	if !hasTotalCount || len(totalCountJSON) == 0 || string(totalCountJSON) == "null" ||
		!hasRuns || len(runsJSON) == 0 || string(runsJSON) == "null" {
		return fmt.Errorf("workflow-run page is missing required fields")
	}
	var decoded struct {
		TotalCount   int                  `json:"total_count"`
		WorkflowRuns []actionsWorkflowRun `json:"workflow_runs"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if decoded.TotalCount < 0 || decoded.WorkflowRuns == nil {
		return fmt.Errorf("workflow-run page has invalid count or run list")
	}
	*p = actionsWorkflowRunsPage(decoded)
	return nil
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
	request, scope, err := c.prepareManagedCIObservation(ctx, request)
	if err != nil {
		return ManagedCIObservation{}, err
	}
	source, sourceJobs, selectedJobs, err := c.readManagedCISource(ctx, scope, request)
	if err != nil {
		return ManagedCIObservation{}, err
	}
	failures, verifiedShortage, err := c.classifyManagedCISourceFailures(
		ctx, scope, request, selectedJobs,
	)
	if err != nil {
		return ManagedCIObservation{}, err
	}
	observation := ManagedCIObservation{
		Source: source, SourceJobs: managedCIJobsForAttempt(sourceJobs, source.Attempt),
		Failures: failures,
	}
	if !verifiedShortage {
		return observation, nil
	}
	return c.observeManagedCIReplacement(ctx, scope, request, source, observation)
}

// ObserveManagedCIActiveExecution binds visible active PR job links to their
// exact Actions attempt. It is used when a retained completed attempt may have
// been superseded by a same-run rerun that is already visible in the PR rollup.
func (c *CLIClient) ObserveManagedCIActiveExecution(
	ctx context.Context,
	request ManagedCIActiveExecutionRequest,
) (ManagedCIExecution, error) {
	request.Repository = strings.TrimSpace(request.Repository)
	request.BaseRefName = strings.TrimSpace(request.BaseRefName)
	request.BaseSHA = strings.TrimSpace(request.BaseSHA)
	request.HeadSHA = strings.TrimSpace(request.HeadSHA)
	if request.Repository == "" || request.PullRequest <= 0 || request.BaseRefName == "" ||
		request.BaseSHA == "" ||
		request.HeadSHA == "" || request.WorkflowID <= 0 || request.RunID <= 0 ||
		len(request.Checks) == 0 {
		return ManagedCIExecution{}, fmt.Errorf("managed CI active execution: incomplete request identity")
	}
	owner, repo, err := c.resolveRepo(ctx)
	if err != nil {
		return ManagedCIExecution{}, fmt.Errorf("managed CI active execution: resolve repository: %w", err)
	}
	repository := owner + "/" + repo
	if !strings.EqualFold(request.Repository, repository) {
		return ManagedCIExecution{}, fmt.Errorf("managed CI active execution: repository mismatch")
	}
	prIdentity, err := c.fetchManagedCIPullRequestIdentity(ctx, owner, repo, request.PullRequest)
	if err != nil || !managedCIPullRequestIdentityMatchesExecution(prIdentity, ManagedCIExecution{
		Repository: repository, PullRequest: request.PullRequest,
		BaseRefName: request.BaseRefName, BaseSHA: request.BaseSHA, HeadSHA: request.HeadSHA,
	}) {
		return ManagedCIExecution{}, fmt.Errorf("managed CI active execution: current pull request identity changed or is unavailable")
	}

	jobIDs := make(map[int64]struct{}, len(request.Checks))
	for _, check := range request.Checks {
		identity, ok := parseManagedActionsJobURL(check.DetailsURL, repository)
		if !ok || identity.RunID != request.RunID || check.RunID != request.RunID ||
			!strings.EqualFold(strings.TrimSpace(check.Repository), repository) ||
			strings.TrimSpace(check.Name) == "" {
			return ManagedCIExecution{}, fmt.Errorf("managed CI active execution: check link identity is incomplete or conflicting")
		}
		if _, duplicate := jobIDs[identity.JobID]; duplicate {
			return ManagedCIExecution{}, fmt.Errorf("managed CI active execution: duplicate active job link")
		}
		jobIDs[identity.JobID] = struct{}{}
	}

	jobs, err := c.listManagedWorkflowJobs(ctx, owner, repo, request.RunID)
	if err != nil {
		return ManagedCIExecution{}, fmt.Errorf("managed CI active execution: list workflow jobs: %w", err)
	}
	jobsByID := make(map[int64]CIWorkflowJob, len(jobs))
	for _, job := range jobs {
		if _, duplicate := jobsByID[job.ID]; duplicate {
			return ManagedCIExecution{}, fmt.Errorf("managed CI active execution: duplicate workflow job identity")
		}
		jobsByID[job.ID] = job
	}
	activeAttempt := 0
	for _, check := range request.Checks {
		identity, _ := parseManagedActionsJobURL(check.DetailsURL, repository)
		job, exists := jobsByID[identity.JobID]
		if !exists || job.RunID != request.RunID ||
			!strings.EqualFold(strings.TrimSpace(job.Name), strings.TrimSpace(check.Name)) ||
			!managedCIJobIsActive(job) || job.RunAttempt <= 0 {
			return ManagedCIExecution{}, fmt.Errorf("managed CI active execution: visible check has no matching active job")
		}
		if activeAttempt == 0 {
			activeAttempt = job.RunAttempt
		} else if activeAttempt != job.RunAttempt {
			return ManagedCIExecution{}, fmt.Errorf("managed CI active execution: visible checks identify conflicting attempts")
		}
	}
	attempt, err := c.fetchManagedWorkflowRunAttempt(ctx, owner, repo, request.RunID, activeAttempt)
	if err != nil {
		return ManagedCIExecution{}, err
	}
	execution, ok := managedCIExecution(attempt, repository, ManagedCIRequest{
		Repository: request.Repository, PullRequest: request.PullRequest,
		BaseRefName: request.BaseRefName, BaseSHA: request.BaseSHA, HeadSHA: request.HeadSHA,
	}, true)
	if !ok || execution.RunID != request.RunID || execution.WorkflowID != request.WorkflowID ||
		execution.Attempt != activeAttempt ||
		!strings.EqualFold(strings.TrimSpace(execution.Event), "pull_request") ||
		!managedCIExecutionIsActive(execution) {
		return ManagedCIExecution{}, fmt.Errorf("managed CI active execution: workflow attempt identity is incomplete or conflicting")
	}
	return execution, nil
}

// RefreshManagedCIExecution observes the current state of one retained run.
// The exact PR identity is re-read first because a pending PR rollup may not
// expose the Actions run through check links, and older gh versions omit the
// PR base SHA from `gh pr` JSON.
func (c *CLIClient) RefreshManagedCIExecution(
	ctx context.Context,
	execution ManagedCIExecution,
) (ManagedCIExecution, error) {
	if execution.PullRequest <= 0 || execution.WorkflowID <= 0 || execution.RunID <= 0 ||
		execution.RunNumber <= 0 || execution.Attempt <= 0 ||
		strings.TrimSpace(execution.Repository) == "" ||
		strings.TrimSpace(execution.BaseRefName) == "" ||
		strings.TrimSpace(execution.BaseSHA) == "" ||
		strings.TrimSpace(execution.HeadSHA) == "" ||
		!strings.EqualFold(strings.TrimSpace(execution.Event), "pull_request") {
		return ManagedCIExecution{}, fmt.Errorf("managed CI refresh: incomplete retained execution identity")
	}
	owner, repo, err := c.resolveRepo(ctx)
	if err != nil {
		return ManagedCIExecution{}, fmt.Errorf("managed CI refresh: resolve repository: %w", err)
	}
	repository := owner + "/" + repo
	if !strings.EqualFold(strings.TrimSpace(execution.Repository), repository) {
		return ManagedCIExecution{}, fmt.Errorf("managed CI refresh: repository mismatch")
	}
	prIdentity, err := c.fetchManagedCIPullRequestIdentity(ctx, owner, repo, execution.PullRequest)
	if err != nil {
		return ManagedCIExecution{}, fmt.Errorf("managed CI refresh: fetch pull request identity: %w", err)
	}
	if prIdentity.PullRequest != execution.PullRequest ||
		!managedCIPullRequestIdentityMatchesExecution(prIdentity, execution) {
		return ManagedCIExecution{}, fmt.Errorf("managed CI refresh: pull request identity changed")
	}

	run, err := c.fetchManagedWorkflowRun(ctx, owner, repo, execution.RunID)
	if err != nil {
		return ManagedCIExecution{}, fmt.Errorf("managed CI refresh: fetch workflow run: %w", err)
	}
	refreshed, ok := managedCIExecution(run, repository, ManagedCIRequest{
		Repository: repository, PullRequest: execution.PullRequest,
		BaseRefName: execution.BaseRefName, BaseSHA: execution.BaseSHA,
		HeadSHA: execution.HeadSHA,
	}, true)
	if !ok || refreshed.RunID != execution.RunID ||
		refreshed.WorkflowID != execution.WorkflowID ||
		refreshed.RunNumber != execution.RunNumber ||
		refreshed.Attempt < execution.Attempt ||
		!strings.EqualFold(refreshed.Event, execution.Event) ||
		!ValidManagedCIExecutionStatus(refreshed.Status, refreshed.Conclusion) {
		return ManagedCIExecution{}, fmt.Errorf("managed CI refresh: workflow execution identity is incomplete or conflicting")
	}
	return refreshed, nil
}

func managedCIJobIsActive(job CIWorkflowJob) bool {
	if strings.TrimSpace(job.Conclusion) != "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(job.Status)) {
	case "queued", "in_progress":
		return true
	default:
		return false
	}
}

type managedCIObservationScope struct {
	owner      string
	repo       string
	repository string
	runID      int64
}

func (c *CLIClient) prepareManagedCIObservation(
	ctx context.Context,
	request ManagedCIRequest,
) (ManagedCIRequest, managedCIObservationScope, error) {
	request.Repository = strings.TrimSpace(request.Repository)
	request.BaseRefName = strings.TrimSpace(request.BaseRefName)
	request.BaseSHA = strings.TrimSpace(request.BaseSHA)
	request.HeadSHA = strings.TrimSpace(request.HeadSHA)
	request.FailedWorkflowRunID = strings.TrimSpace(request.FailedWorkflowRunID)
	if request.PullRequest <= 0 || request.Repository == "" || request.BaseRefName == "" ||
		request.HeadSHA == "" || len(request.FailedChecks) == 0 {
		return ManagedCIRequest{}, managedCIObservationScope{},
			fmt.Errorf("managed CI observation: incomplete pull request or failed-check identity")
	}
	runID, err := strconv.ParseInt(request.FailedWorkflowRunID, 10, 64)
	if err != nil || runID <= 0 {
		return ManagedCIRequest{}, managedCIObservationScope{},
			fmt.Errorf("managed CI observation: invalid workflow run ID %q", request.FailedWorkflowRunID)
	}
	owner, repo, err := c.resolveRepo(ctx)
	if err != nil {
		return ManagedCIRequest{}, managedCIObservationScope{},
			fmt.Errorf("managed CI observation: resolve repository: %w", err)
	}
	repository := owner + "/" + repo
	if !strings.EqualFold(request.Repository, repository) {
		return ManagedCIRequest{}, managedCIObservationScope{},
			fmt.Errorf("managed CI observation: repository mismatch")
	}
	if request.BaseSHA == "" {
		identity, err := c.fetchManagedCIPullRequestIdentity(ctx, owner, repo, request.PullRequest)
		if err != nil {
			return ManagedCIRequest{}, managedCIObservationScope{},
				fmt.Errorf("managed CI observation: fetch pull request identity: %w", err)
		}
		if identity.PullRequest != request.PullRequest ||
			!strings.EqualFold(identity.Repository, repository) ||
			identity.BaseRefName != request.BaseRefName ||
			!strings.EqualFold(identity.HeadSHA, request.HeadSHA) {
			return ManagedCIRequest{}, managedCIObservationScope{},
				fmt.Errorf("managed CI observation: pull request identity is incomplete or conflicting")
		}
		request.BaseSHA = identity.BaseSHA
	}
	return request, managedCIObservationScope{
		owner: owner, repo: repo, repository: repository, runID: runID,
	}, nil
}

func (c *CLIClient) FetchManagedCIPullRequestIdentity(
	ctx context.Context,
	pullRequest int,
) (ManagedCIPullRequestIdentity, error) {
	if pullRequest <= 0 {
		return ManagedCIPullRequestIdentity{}, fmt.Errorf("managed CI pull request identity: invalid pull request number")
	}
	owner, repo, err := c.resolveRepo(ctx)
	if err != nil {
		return ManagedCIPullRequestIdentity{}, fmt.Errorf("managed CI pull request identity: resolve repository: %w", err)
	}
	return c.fetchManagedCIPullRequestIdentity(ctx, owner, repo, pullRequest)
}

func (c *CLIClient) fetchManagedCIPullRequestIdentity(
	ctx context.Context,
	owner, repo string,
	pullRequest int,
) (ManagedCIPullRequestIdentity, error) {
	path := fmt.Sprintf("repos/%s/%s/pulls/%d", owner, repo, pullRequest)
	out, err := c.readCommand(ctx, "gh api managed pull request identity", "api", path)
	if err != nil {
		return ManagedCIPullRequestIdentity{}, err
	}
	var payload struct {
		Number int `json:"number"`
		Base   struct {
			Ref  string `json:"ref"`
			SHA  string `json:"sha"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"base"`
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return ManagedCIPullRequestIdentity{}, fmt.Errorf("parse pull request identity: %w", err)
	}
	repository := owner + "/" + repo
	identity := ManagedCIPullRequestIdentity{
		Repository: repository, PullRequest: payload.Number,
		BaseRefName: strings.TrimSpace(payload.Base.Ref),
		BaseSHA:     strings.TrimSpace(payload.Base.SHA),
		HeadSHA:     strings.TrimSpace(payload.Head.SHA),
	}
	if identity.PullRequest != pullRequest ||
		!strings.EqualFold(strings.TrimSpace(payload.Base.Repo.FullName), repository) ||
		identity.BaseRefName == "" || identity.BaseSHA == "" || identity.HeadSHA == "" {
		return ManagedCIPullRequestIdentity{}, fmt.Errorf("pull request identity is incomplete or conflicting")
	}
	return identity, nil
}

func managedCIPullRequestIdentityMatchesExecution(
	identity ManagedCIPullRequestIdentity,
	execution ManagedCIExecution,
) bool {
	return identity.PullRequest == execution.PullRequest &&
		strings.EqualFold(strings.TrimSpace(identity.Repository), strings.TrimSpace(execution.Repository)) &&
		strings.TrimSpace(identity.BaseRefName) == strings.TrimSpace(execution.BaseRefName) &&
		strings.EqualFold(strings.TrimSpace(identity.BaseSHA), strings.TrimSpace(execution.BaseSHA)) &&
		strings.EqualFold(strings.TrimSpace(identity.HeadSHA), strings.TrimSpace(execution.HeadSHA))
}

func (c *CLIClient) readManagedCISource(
	ctx context.Context,
	scope managedCIObservationScope,
	request ManagedCIRequest,
) (ManagedCIExecution, []CIWorkflowJob, []CIWorkflowJob, error) {
	jobs, err := c.listManagedWorkflowJobs(ctx, scope.owner, scope.repo, scope.runID)
	if err != nil {
		return ManagedCIExecution{}, nil, nil, fmt.Errorf("managed CI observation: list source jobs: %w", err)
	}
	jobsByID := make(map[int64]CIWorkflowJob, len(jobs))
	for _, job := range jobs {
		if _, duplicate := jobsByID[job.ID]; duplicate {
			return ManagedCIExecution{}, nil, nil,
				fmt.Errorf("managed CI observation: duplicate source job identity")
		}
		jobsByID[job.ID] = job
	}
	selectedJobs := make([]CIWorkflowJob, 0, len(request.FailedChecks))
	sourceAttempt := 0
	for _, check := range request.FailedChecks {
		identity, ok := parseManagedActionsJobURL(check.DetailsURL, request.Repository)
		if !ok || identity.RunID != scope.runID {
			return ManagedCIExecution{}, nil, nil,
				fmt.Errorf("managed CI observation: failed check link is not bound to the source run")
		}
		job, exists := jobsByID[identity.JobID]
		if !exists || !strings.EqualFold(strings.TrimSpace(job.Name), strings.TrimSpace(check.Name)) ||
			job.RunID != scope.runID || job.RunAttempt <= 0 || strings.TrimSpace(job.CheckRunURL) == "" {
			return ManagedCIExecution{}, nil, nil,
				fmt.Errorf("managed CI observation: failed check has no matching current-attempt job")
		}
		if sourceAttempt == 0 {
			sourceAttempt = job.RunAttempt
		} else if sourceAttempt != job.RunAttempt {
			return ManagedCIExecution{}, nil, nil,
				fmt.Errorf("managed CI observation: failed checks identify conflicting workflow attempts")
		}
		selectedJobs = append(selectedJobs, job)
	}
	sourceRun, err := c.fetchManagedWorkflowRunAttempt(ctx, scope.owner, scope.repo, scope.runID, sourceAttempt)
	if err != nil {
		return ManagedCIExecution{}, nil, nil, err
	}
	source, identityOK := managedCIExecution(sourceRun, scope.repository, request, false)
	if !identityOK || source.RunID != scope.runID || source.Attempt != sourceAttempt ||
		!strings.EqualFold(strings.TrimSpace(sourceRun.Event), "pull_request") {
		return ManagedCIExecution{}, nil, nil,
			fmt.Errorf("managed CI observation: source workflow identity is incomplete or conflicting")
	}
	return source, jobs, selectedJobs, nil
}

func (c *CLIClient) classifyManagedCISourceFailures(
	ctx context.Context,
	scope managedCIObservationScope,
	request ManagedCIRequest,
	selectedJobs []CIWorkflowJob,
) ([]ManagedCIFailureEvidence, bool, error) {
	failures := make([]ManagedCIFailureEvidence, 0, len(request.FailedChecks))
	verifiedShortage := false
	for index, check := range request.FailedChecks {
		job := selectedJobs[index]
		checkRunID, ok := managedCheckRunID(job.CheckRunURL, request.Repository)
		if !ok {
			return nil, false, fmt.Errorf("managed CI observation: job check-run identity is invalid")
		}
		annotations, err := c.listManagedCheckRunAnnotations(ctx, scope.owner, scope.repo, checkRunID)
		if err != nil {
			return nil, false, fmt.Errorf("managed CI observation: list check-run annotations: %w", err)
		}
		classification, causedByChecks, causedByResults := classifyManagedCIFailureEvidenceDetailed(job, annotations)
		if classification == ManagedCIRunnerShortage {
			verifiedShortage = true
		}
		failures = append(failures, ManagedCIFailureEvidence{
			JobID: job.ID, RunID: job.RunID, Attempt: job.RunAttempt,
			CheckName: strings.TrimSpace(check.Name), DetailsURL: strings.TrimSpace(check.DetailsURL),
			Classification: classification, CausedByChecks: causedByChecks, CausedByResults: causedByResults,
		})
	}
	return failures, verifiedShortage, nil
}

func (c *CLIClient) observeManagedCIReplacement(
	ctx context.Context,
	scope managedCIObservationScope,
	request ManagedCIRequest,
	source ManagedCIExecution,
	observation ManagedCIObservation,
) (ManagedCIObservation, error) {
	replacement, err := c.findManagedCIReplacement(ctx, scope.owner, scope.repo, source)
	if err != nil {
		// The source identity and failures are fully validated even though the
		// replacement identity is not. Preserve source failures; lifecycle treats
		// replacement selection as unknown.
		return observation, err
	}
	observation.Replacement = replacement
	if replacement != nil && managedCISourceHasIndependentFailures(observation.Failures) {
		if jobs, jobsErr := c.listManagedWorkflowJobs(ctx, scope.owner, scope.repo, replacement.RunID); jobsErr == nil {
			observation.ReplacementJobs = managedCIJobsForAttempt(jobs, replacement.Attempt)
		}
	}
	if replacement != nil && replacement.Status == "completed" && replacement.Conclusion != "success" {
		failures, err := c.diagnoseManagedCIExecutionFailure(ctx, scope.owner, scope.repo, request, *replacement)
		if err != nil {
			// Keep the verified source evidence and replacement identity; only
			// the replacement's failure diagnosis is inconclusive.
			return observation, err
		}
		observation.ReplacementFailures = failures
	}
	return observation, nil
}

func managedCISourceHasIndependentFailures(failures []ManagedCIFailureEvidence) bool {
	for _, failure := range failures {
		if failure.Classification == ManagedCIConfirmedCodeTestFailure {
			return true
		}
	}
	return false
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

func (c *CLIClient) fetchManagedWorkflowRun(
	ctx context.Context,
	owner, repo string,
	runID int64,
) (actionsWorkflowRun, error) {
	if runID <= 0 {
		return actionsWorkflowRun{}, fmt.Errorf("managed CI refresh: invalid workflow run ID")
	}
	path := fmt.Sprintf("repos/%s/%s/actions/runs/%d", owner, repo, runID)
	out, err := c.readCommand(ctx, "gh api current workflow run", "api", path)
	if err != nil {
		return actionsWorkflowRun{}, fmt.Errorf("gh api current workflow run: %w", err)
	}
	var run actionsWorkflowRun
	if err := json.Unmarshal(out, &run); err != nil {
		return actionsWorkflowRun{}, fmt.Errorf("parse current workflow run: %w", err)
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

func managedCIJobsForAttempt(jobs []CIWorkflowJob, attempt int) []ManagedCIJobEvidence {
	out := make([]ManagedCIJobEvidence, 0, len(jobs))
	for _, job := range jobs {
		if job.RunAttempt != attempt {
			continue
		}
		out = append(out, ManagedCIJobEvidence{
			JobID: job.ID, RunID: job.RunID, Attempt: job.RunAttempt, Name: strings.TrimSpace(job.Name),
			Status:     strings.ToLower(strings.TrimSpace(job.Status)),
			Conclusion: strings.ToLower(strings.TrimSpace(job.Conclusion)),
		})
	}
	return out
}

func (c *CLIClient) diagnoseManagedCIExecutionFailure(
	ctx context.Context,
	owner, repo string,
	request ManagedCIRequest,
	execution ManagedCIExecution,
) ([]ManagedCIFailureEvidence, error) {
	identity, err := c.fetchManagedCIPullRequestIdentity(ctx, owner, repo, execution.PullRequest)
	if err != nil || !managedCIPullRequestIdentityMatchesExecution(identity, execution) {
		return nil, fmt.Errorf("managed CI observation: current pull request identity changed before failure diagnosis")
	}
	run, err := c.fetchManagedWorkflowRunAttempt(ctx, owner, repo, execution.RunID, execution.Attempt)
	if err != nil {
		return nil, err
	}
	verified, ok := managedCIExecution(run, execution.Repository, request, true)
	if !ok || verified.RunID != execution.RunID || verified.RunNumber != execution.RunNumber ||
		verified.WorkflowID != execution.WorkflowID || verified.Attempt != execution.Attempt ||
		!strings.EqualFold(strings.TrimSpace(verified.Event), strings.TrimSpace(execution.Event)) ||
		!strings.EqualFold(verified.Status, execution.Status) ||
		!strings.EqualFold(verified.Conclusion, execution.Conclusion) {
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
		classification, causedByChecks, causedByResults := classifyManagedCIFailureEvidenceDetailed(job, annotations)
		failures = append(failures, ManagedCIFailureEvidence{
			JobID: job.ID, RunID: job.RunID, Attempt: job.RunAttempt, CheckName: strings.TrimSpace(job.Name),
			DetailsURL: fmt.Sprintf("https://github.com/%s/%s/actions/runs/%d/job/%d",
				owner, repo, execution.RunID, job.ID),
			Classification: classification, CausedByChecks: causedByChecks, CausedByResults: causedByResults,
		})
	}
	if len(failures) == 0 {
		return nil, fmt.Errorf("managed CI observation: completed failed replacement has no diagnosed failed job")
	}
	return failures, nil
}

func (c *CLIClient) DiagnoseManagedCIExecutionFailure(
	ctx context.Context,
	execution ManagedCIExecution,
) ([]ManagedCIFailureEvidence, error) {
	if execution.PullRequest <= 0 || strings.TrimSpace(execution.BaseRefName) == "" ||
		strings.TrimSpace(execution.BaseSHA) == "" ||
		strings.TrimSpace(execution.HeadSHA) == "" || execution.WorkflowID <= 0 ||
		execution.RunID <= 0 || execution.RunNumber <= 0 || execution.Attempt <= 0 ||
		!strings.EqualFold(strings.TrimSpace(execution.Event), "pull_request") ||
		!strings.EqualFold(strings.TrimSpace(execution.Status), "completed") ||
		strings.EqualFold(strings.TrimSpace(execution.Conclusion), "success") ||
		!ValidManagedCIExecutionStatus(execution.Status, execution.Conclusion) {
		return nil, fmt.Errorf("managed CI diagnosis: execution is not a valid completed failure")
	}
	owner, repo, err := c.resolveRepo(ctx)
	if err != nil {
		return nil, fmt.Errorf("managed CI diagnosis: resolve repository: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(execution.Repository), owner+"/"+repo) {
		return nil, fmt.Errorf("managed CI diagnosis: execution repository does not match the current repository")
	}
	request := ManagedCIRequest{
		Repository: execution.Repository, PullRequest: execution.PullRequest,
		BaseRefName: execution.BaseRefName, BaseSHA: execution.BaseSHA, HeadSHA: execution.HeadSHA,
	}
	return c.diagnoseManagedCIExecutionFailure(ctx, owner, repo, request, execution)
}

func managedCIExecutionIsActive(execution ManagedCIExecution) bool {
	if !ValidManagedCIExecutionStatus(execution.Status, execution.Conclusion) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(execution.Status)) {
	case "queued", "in_progress", "waiting", "requested", "pending":
		return true
	default:
		return false
	}
}

func managedCIExecution(
	run actionsWorkflowRun,
	repository string,
	request ManagedCIRequest,
	// allowMissingLinkForBoundRun is safe only when the exact run ID is already
	// bound to this PR/base by a previously verified execution.
	allowMissingLinkForBoundRun bool,
) (ManagedCIExecution, bool) {
	if run.ID <= 0 || run.RunNumber <= 0 || run.RunAttempt <= 0 || run.WorkflowID <= 0 ||
		strings.TrimSpace(request.BaseSHA) == "" || strings.TrimSpace(run.HeadSHA) == "" ||
		!ValidManagedCIExecutionStatus(run.Status, run.Conclusion) {
		return ManagedCIExecution{}, false
	}
	if !strings.EqualFold(strings.TrimSpace(run.HeadSHA), request.HeadSHA) {
		return ManagedCIExecution{}, false
	}
	if len(run.PullRequests) == 0 {
		if !allowMissingLinkForBoundRun {
			return ManagedCIExecution{}, false
		}
		return managedCIExecutionFromBoundRun(run, repository, request), true
	}
	matches := 0
	for _, pr := range run.PullRequests {
		if pr.Number == request.PullRequest &&
			strings.TrimSpace(pr.Base.Ref) == strings.TrimSpace(request.BaseRefName) &&
			strings.EqualFold(strings.TrimSpace(pr.Base.SHA), strings.TrimSpace(request.BaseSHA)) &&
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
		BaseSHA: request.BaseSHA, HeadSHA: request.HeadSHA, WorkflowID: run.WorkflowID,
		Event: strings.ToLower(strings.TrimSpace(run.Event)), RunID: run.ID, RunNumber: run.RunNumber,
		Attempt: run.RunAttempt, Status: strings.ToLower(strings.TrimSpace(run.Status)),
		Conclusion: strings.ToLower(strings.TrimSpace(run.Conclusion)),
	}, true
}

func managedCIExecutionFromBoundRun(
	run actionsWorkflowRun,
	repository string,
	request ManagedCIRequest,
) ManagedCIExecution {
	return ManagedCIExecution{
		Repository: repository, PullRequest: request.PullRequest, BaseRefName: request.BaseRefName,
		BaseSHA: request.BaseSHA, HeadSHA: request.HeadSHA, WorkflowID: run.WorkflowID,
		Event: strings.ToLower(strings.TrimSpace(run.Event)), RunID: run.ID, RunNumber: run.RunNumber,
		Attempt: run.RunAttempt, Status: strings.ToLower(strings.TrimSpace(run.Status)),
		Conclusion: strings.ToLower(strings.TrimSpace(run.Conclusion)),
	}
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
	actualRepository, identity, ok := managedActionsJobURLIdentity(value)
	if !ok || !strings.EqualFold(actualRepository, repository) {
		return actionsWorkflowJobDetails{}, false
	}
	return identity, true
}

func managedActionsJobURLIdentity(value string) (string, actionsWorkflowJobDetails, bool) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || !strings.EqualFold(parsed.Host, "github.com") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", actionsWorkflowJobDetails{}, false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 7 || parts[0] == "" || parts[1] == "" ||
		parts[2] != "actions" || parts[3] != "runs" || parts[5] != "job" {
		return "", actionsWorkflowJobDetails{}, false
	}
	runID, runErr := strconv.ParseInt(parts[4], 10, 64)
	jobID, jobErr := strconv.ParseInt(parts[6], 10, 64)
	if runErr != nil || jobErr != nil || runID <= 0 || jobID <= 0 {
		return "", actionsWorkflowJobDetails{}, false
	}
	return parts[0] + "/" + parts[1], actionsWorkflowJobDetails{RunID: runID, JobID: jobID}, true
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
	classification, causes, _ := classifyManagedCIFailureEvidenceDetailed(job, annotations)
	return classification, causes
}

func classifyManagedCIFailureEvidenceDetailed(
	job CIWorkflowJob,
	annotations []checkRunAnnotation,
) (ManagedCIFailureClassification, []string, []string) {
	if managedCIRunnerShortageEvidence(job, annotations) {
		return ManagedCIRunnerShortage, nil, nil
	}
	if causes, results, ok := managedCIDependentFailureEvidence(job, annotations); ok {
		return ManagedCIDependentFailure, causes, results
	}
	if managedCIConfirmedCodeTestFailureEvidence(job, annotations) {
		return ManagedCIConfirmedCodeTestFailure, nil, nil
	}
	return ManagedCIUnexplainedFailure, nil, nil
}

func managedCIRunnerShortageEvidence(job CIWorkflowJob, annotations []checkRunAnnotation) bool {
	if !managedCIJobConclusion(job, "cancelled") || !job.runnerIDObserved || !job.runnerNameObserved ||
		!job.stepsObserved || job.Steps == nil || job.RunnerID != 0 ||
		strings.TrimSpace(job.RunnerName) != "" || len(job.Steps) != 0 {
		return false
	}
	for _, annotation := range annotations {
		message := strings.TrimSpace(annotation.Message)
		if strings.EqualFold(strings.TrimSpace(annotation.Level), "failure") &&
			runnerAcquisitionAnnotation.MatchString(message) &&
			!selfHostedRunnerAnnotation.MatchString(message) {
			return true
		}
	}
	return false
}

func managedCIDependentFailureEvidence(job CIWorkflowJob, annotations []checkRunAnnotation) ([]string, []string, bool) {
	if !managedCIJobConclusion(job, "failure") {
		return nil, nil, false
	}
	var causes []string
	var results []string
	for _, annotation := range annotations {
		if !strings.EqualFold(strings.TrimSpace(annotation.Level), "failure") {
			continue
		}
		message := strings.TrimSpace(annotation.Message)
		match := failedPrerequisiteAnnotation.FindStringSubmatch(message)
		if len(match) != 3 {
			if aggregateExitWrapperAnnotation.MatchString(message) {
				continue
			}
			return nil, nil, false
		}
		if strings.EqualFold(strings.TrimSpace(match[2]), "success") {
			return nil, nil, false
		}
		causes = append(causes, strings.TrimSpace(match[1]))
		results = append(results, strings.TrimSpace(match[2]))
	}
	return causes, results, len(causes) > 0
}

func managedCIConfirmedCodeTestFailureEvidence(job CIWorkflowJob, annotations []checkRunAnnotation) bool {
	if !managedCIJobConclusion(job, "failure") {
		return false
	}
	failedTestStep, failedCompilerStep := false, false
	for _, step := range job.Steps {
		if !strings.EqualFold(strings.TrimSpace(step.Status), "completed") ||
			!strings.EqualFold(strings.TrimSpace(step.Conclusion), "failure") {
			continue
		}
		stepName := strings.TrimSpace(step.Name)
		failedTestStep = failedTestStep || managedCITestStepName.MatchString(stepName)
		failedCompilerStep = failedCompilerStep || managedCICompilerStepName.MatchString(stepName)
	}
	if !failedTestStep && !failedCompilerStep {
		return false
	}
	for _, annotation := range annotations {
		message := strings.TrimSpace(annotation.Message)
		if !strings.EqualFold(strings.TrimSpace(annotation.Level), "failure") ||
			!managedCITestSourcePath(annotation.Path) || annotation.StartLine <= 0 ||
			managedCIInfrastructureDiagnostic.MatchString(message) {
			continue
		}
		if failedTestStep && managedCITestDiagnostic.MatchString(message) ||
			failedCompilerStep && managedCICompilerDiagnostic.MatchString(message) {
			return true
		}
	}
	return false
}

func managedCIJobConclusion(job CIWorkflowJob, conclusion string) bool {
	return strings.EqualFold(strings.TrimSpace(job.Status), "completed") &&
		strings.EqualFold(strings.TrimSpace(job.Conclusion), conclusion)
}

func managedCITestSourcePath(path string) bool {
	path = strings.ToLower(strings.TrimSpace(path))
	switch {
	case strings.HasSuffix(path, ".go"), strings.HasSuffix(path, ".py"),
		strings.HasSuffix(path, ".js"), strings.HasSuffix(path, ".jsx"),
		strings.HasSuffix(path, ".ts"), strings.HasSuffix(path, ".tsx"),
		strings.HasSuffix(path, ".rs"), strings.HasSuffix(path, ".cs"),
		strings.HasSuffix(path, ".java"), strings.HasSuffix(path, ".kt"),
		strings.HasSuffix(path, ".sh"):
		return true
	default:
		return false
	}
}

func (c *CLIClient) findManagedCIReplacement(
	ctx context.Context,
	owner, repo string,
	source ManagedCIExecution,
) (*ManagedCIExecution, error) {
	runs, err := c.listManagedCIWorkflowRuns(ctx, owner, repo, source)
	if err != nil {
		return nil, err
	}
	sourceListed, err := validateManagedCIWorkflowRunList(runs, source)
	if err != nil {
		return nil, err
	}
	candidates, err := managedCIReplacementCandidates(runs, source)
	if err != nil {
		return nil, err
	}
	selected, err := selectManagedCIReplacement(candidates)
	if err != nil {
		return nil, err
	}
	if !sourceListed && selected == nil {
		return nil, fmt.Errorf("workflow-run list omits the verified source and contains no verified replacement")
	}
	return selected, nil
}

func (c *CLIClient) listManagedCIWorkflowRuns(
	ctx context.Context,
	owner, repo string,
	source ManagedCIExecution,
) ([]actionsWorkflowRun, error) {
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
	return runs, nil
}

func validateManagedCIWorkflowRunList(
	runs []actionsWorkflowRun,
	source ManagedCIExecution,
) (bool, error) {
	runNumbers := make(map[int64]int64, len(runs))
	runIDs := make(map[int64]int64, len(runs))
	type workflowRunIdentity struct {
		runID   int64
		attempt int
	}
	seenExecutions := make(map[workflowRunIdentity]struct{}, len(runs))
	sourceListed := false
	for _, run := range runs {
		if run.ID <= 0 || run.RunNumber <= 0 || run.RunAttempt <= 0 ||
			strings.TrimSpace(run.Event) == "" || !ValidManagedCIExecutionStatus(run.Status, run.Conclusion) ||
			strings.TrimSpace(run.HeadSHA) == "" || run.WorkflowID <= 0 {
			return false, fmt.Errorf("workflow-run list contains incomplete execution identity")
		}
		if run.WorkflowID != source.WorkflowID ||
			!strings.EqualFold(strings.TrimSpace(run.Event), "pull_request") ||
			!strings.EqualFold(strings.TrimSpace(run.HeadSHA), source.HeadSHA) {
			return false, fmt.Errorf("workflow-run list contradicts its workflow, event, or head filter")
		}
		if previous, exists := runNumbers[run.RunNumber]; exists && previous != run.ID {
			return false, fmt.Errorf("workflow-run list reuses a run number for different run IDs")
		}
		runNumbers[run.RunNumber] = run.ID
		identity := workflowRunIdentity{runID: run.ID, attempt: run.RunAttempt}
		if _, duplicate := seenExecutions[identity]; duplicate {
			return false, fmt.Errorf("workflow-run list duplicates an execution identity")
		}
		seenExecutions[identity] = struct{}{}
		if previous, exists := runIDs[run.ID]; exists && previous != run.RunNumber {
			return false, fmt.Errorf("workflow-run list assigns conflicting run numbers to one run ID")
		}
		runIDs[run.ID] = run.RunNumber
		if run.ID == source.RunID && run.RunNumber != source.RunNumber {
			return false, fmt.Errorf("workflow-run list contains a contradictory attempt identity")
		}
		if run.ID == source.RunID {
			sourceExecution, ok := managedCIExecution(run, source.Repository, ManagedCIRequest{
				Repository: source.Repository, PullRequest: source.PullRequest,
				BaseRefName: source.BaseRefName, BaseSHA: source.BaseSHA, HeadSHA: source.HeadSHA,
			}, true)
			if !ok || sourceExecution.Attempt < source.Attempt {
				return false, fmt.Errorf("workflow-run list does not confirm the source pull request execution")
			}
			if sourceExecution.Attempt == source.Attempt &&
				!managedCIExecutionReadsCompatible(source, sourceExecution) {
				return false, fmt.Errorf("workflow-run list contradicts the source execution status or conclusion for the same attempt")
			}
			sourceListed = true
		}
	}
	return sourceListed, nil
}

// The workflow-run listing is read after the exact source attempt. Its status
// may advance, but it cannot regress or contradict a completed conclusion.
func managedCIExecutionReadsCompatible(earlier, later ManagedCIExecution) bool {
	earlierStatus := strings.ToLower(strings.TrimSpace(earlier.Status))
	laterStatus := strings.ToLower(strings.TrimSpace(later.Status))
	earlierStage, earlierOK := managedCIExecutionStatusStage(earlierStatus)
	laterStage, laterOK := managedCIExecutionStatusStage(laterStatus)
	if !earlierOK || !laterOK || laterStage < earlierStage {
		return false
	}
	if earlierStatus == "completed" {
		return laterStatus == "completed" &&
			strings.EqualFold(strings.TrimSpace(earlier.Conclusion), strings.TrimSpace(later.Conclusion))
	}
	if laterStatus == "completed" {
		return ValidManagedCIExecutionStatus(later.Status, later.Conclusion)
	}
	return true
}

func managedCIExecutionStatusStage(status string) (int, bool) {
	switch status {
	case "requested", "waiting", "pending":
		return 0, true
	case "queued":
		return 1, true
	case "in_progress":
		return 2, true
	case "completed":
		return 3, true
	default:
		return 0, false
	}
}

func managedCIReplacementCandidates(
	runs []actionsWorkflowRun,
	source ManagedCIExecution,
) ([]ManagedCIExecution, error) {
	var candidates []ManagedCIExecution
	for _, run := range runs {
		isNewer := run.ID == source.RunID && run.RunAttempt > source.Attempt
		if run.ID != source.RunID && run.RunNumber > source.RunNumber {
			isNewer = true
		}
		if !isNewer {
			continue
		}
		allowMissingLinkForSameRunAttempt := run.ID == source.RunID
		if len(run.PullRequests) == 0 && !allowMissingLinkForSameRunAttempt {
			return nil, fmt.Errorf("newer workflow run has no pull request identity")
		}
		candidate, ok := managedCIExecution(run, source.Repository, ManagedCIRequest{
			Repository: source.Repository, PullRequest: source.PullRequest, BaseRefName: source.BaseRefName,
			BaseSHA: source.BaseSHA, HeadSHA: source.HeadSHA,
		}, allowMissingLinkForSameRunAttempt)
		if !ok {
			return nil, fmt.Errorf("newer workflow run has a conflicting pull request identity")
		}
		candidates = append(candidates, candidate)
	}
	return candidates, nil
}

func selectManagedCIReplacement(candidates []ManagedCIExecution) (*ManagedCIExecution, error) {
	var selected *ManagedCIExecution
	for _, candidate := range candidates {
		if selected != nil &&
			candidate.RunNumber == selected.RunNumber &&
			candidate.Attempt == selected.Attempt &&
			candidate.RunID != selected.RunID {
			return nil, fmt.Errorf("workflow-run list contains contradictory replacement identity")
		}
		if selected != nil &&
			(candidate.RunNumber < selected.RunNumber ||
				(candidate.RunNumber == selected.RunNumber && candidate.Attempt <= selected.Attempt)) {
			continue
		}
		copy := candidate
		selected = &copy
	}
	return selected, nil
}
