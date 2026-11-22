package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// WorkflowRerunState is the outcome of one workflow job rerun attempt. The
// states are the CI rerun helper's: the runtime reads them from the client the
// way it reads the helper's receipt from the agent.
type WorkflowRerunState string

const (
	// WorkflowRerunConfirmed: the read-back observed a new attempt that holds
	// the selected job and its quality gate.
	WorkflowRerunConfirmed WorkflowRerunState = "confirmed"
	// WorkflowRerunWaiting: the workflow (or the selected job) is still
	// running, so GitHub accepts no rerun yet. Nothing was submitted.
	WorkflowRerunWaiting WorkflowRerunState = "waiting"
	// WorkflowRerunRefused: the live evidence does not match the request, or
	// GitHub denied it. Nothing was submitted, or the submission was denied.
	WorkflowRerunRefused WorkflowRerunState = "refused"
	// WorkflowRerunUncertain: the rerun was submitted but its effect could not
	// be confirmed. It is never submitted again within the same attempt.
	WorkflowRerunUncertain WorkflowRerunState = "uncertain"
)

// WorkflowRerunRequest identifies one failed workflow job execution to rerun.
// The job is addressed by name inside the identified workflow run because job
// database IDs change with every attempt of that run.
type WorkflowRerunRequest struct {
	// Repository is "owner/name".
	Repository  string
	PullRequest int
	// HeadSHA is the pull request's source head; TestedSHA is the commit the
	// workflow run executed. They differ for merge-ref workflows.
	HeadSHA   string
	TestedSHA string
	// RunID is the workflow run's database ID.
	RunID string
	// JobName is the diagnosed failed job's name.
	JobName string
}

// WorkflowRerunResult reports one rerun attempt.
type WorkflowRerunResult struct {
	State           WorkflowRerunState
	Reason          string
	ObservedAttempt int
}

// WorkflowJobRerunner is the optional capability the CI recovery window
// type-asserts against to rerun a diagnosed failed job and its dependents,
// the aggregate quality gate included. Production CLIClient satisfies it;
// existing test fakes do not, which leaves them unchanged. An attempt performs
// at most one mutation and reads GitHub back afterwards. An error means the
// attempt could not run at all (an invalid request or a cancelled context);
// every other outcome is a result.
type WorkflowJobRerunner interface {
	RerunWorkflowJob(ctx context.Context, request WorkflowRerunRequest) (WorkflowRerunResult, error)
}

var (
	rerunRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	rerunNumericID         = regexp.MustCompile(`^[1-9][0-9]*$`)
)

type workflowRerunPullRequest struct {
	Number   int     `json:"number"`
	State    string  `json:"state"`
	MergedAt *string `json:"merged_at"`
	Head     struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
}

type workflowRerunRun struct {
	ID         int64  `json:"id"`
	Event      string `json:"event"`
	HeadSHA    string `json:"head_sha"`
	RunAttempt int    `json:"run_attempt"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	PullRequests []struct {
		Number int `json:"number"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_requests"`
}

type workflowRerunJob struct {
	ID           int64  `json:"id"`
	RunID        int64  `json:"run_id"`
	RunAttempt   int    `json:"run_attempt"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	WorkflowName string `json:"workflow_name"`
}

const qualityGateJobName = "quality-gate"

// RerunWorkflowJob reruns the diagnosed failed job of a workflow run, which
// GitHub extends to its dependent jobs including the quality gate, after
// verifying the pull request, run and job against live state. It mirrors the
// CI rerun helper the implementor uses once per failed execution.
func (c *CLIClient) RerunWorkflowJob(ctx context.Context, request WorkflowRerunRequest) (WorkflowRerunResult, error) {
	if err := ctx.Err(); err != nil {
		return WorkflowRerunResult{}, err
	}
	runID, err := validateWorkflowRerunRequest(request)
	if err != nil {
		return WorkflowRerunResult{}, err
	}
	api := workflowRerunAPI{client: c, repository: request.Repository, number: request.PullRequest, runID: request.RunID}
	refuse := func(reason string) (WorkflowRerunResult, error) {
		if err := ctx.Err(); err != nil {
			return WorkflowRerunResult{}, err
		}
		return WorkflowRerunResult{State: WorkflowRerunRefused, Reason: reason}, nil
	}

	pr, err := api.pullRequestView(ctx)
	if err != nil {
		return refuse("pull-request-lookup-failed")
	}
	if !workflowRerunPullRequestMatches(pr, request) {
		return refuse("pull-request-or-source-head-mismatch")
	}
	run, err := api.run(ctx)
	if err != nil {
		return refuse("workflow-run-lookup-failed")
	}
	if !workflowRerunRunMatches(run, request, runID) || run.RunAttempt < 1 || strings.TrimSpace(run.Status) == "" {
		return refuse("workflow-run-linkage-mismatch")
	}
	jobs, err := api.jobs(ctx)
	if err != nil {
		return refuse("workflow-jobs-lookup-failed")
	}
	job, ok := selectWorkflowRerunJob(jobs, runID, run.RunAttempt, request.JobName)
	if !ok {
		return refuse("failed-job-not-in-current-attempt")
	}
	priorGate := qualityGateJobID(jobs, runID, run.RunAttempt)
	if job.Status == "queued" || job.Status == "in_progress" {
		return WorkflowRerunResult{State: WorkflowRerunWaiting, Reason: "recovery-already-active", ObservedAttempt: run.RunAttempt}, nil
	}
	if run.Status == "queued" || run.Status == "in_progress" {
		return WorkflowRerunResult{State: WorkflowRerunWaiting, Reason: "workflow-still-running", ObservedAttempt: run.RunAttempt}, nil
	}
	if run.Status != "completed" || job.Status != "completed" || !workflowRerunFailed(job.Conclusion) {
		return refuse("current-execution-is-not-rerunnable")
	}

	// One mutation attempt. A lost or ambiguous response never causes a second
	// POST: the read-back below decides what happened.
	postErr := api.rerun(ctx, job.ID)
	if err := ctx.Err(); err != nil {
		return WorkflowRerunResult{}, err
	}
	uncertain := func(reason string, attempt int) (WorkflowRerunResult, error) {
		return WorkflowRerunResult{State: WorkflowRerunUncertain, Reason: reason, ObservedAttempt: attempt}, nil
	}
	afterRun, err := api.run(ctx)
	if err != nil {
		return uncertain("rerun-readback-failed", run.RunAttempt)
	}
	afterPR, err := api.pullRequestView(ctx)
	if err != nil {
		return uncertain("pull-request-readback-failed", run.RunAttempt)
	}
	afterJobs, err := api.jobs(ctx)
	if err != nil {
		return uncertain("rerun-jobs-readback-failed", run.RunAttempt)
	}
	if !workflowRerunRunMatches(afterRun, request, runID) {
		return uncertain("rerun-readback-identity-changed", run.RunAttempt)
	}
	if !workflowRerunPullRequestMatches(afterPR, request) {
		return uncertain("pull-request-readback-identity-changed", run.RunAttempt)
	}
	if afterRun.RunAttempt > run.RunAttempt &&
		replacementJobCount(afterJobs, runID, afterRun.RunAttempt, job.ID, job.Name, job.WorkflowName) == 1 &&
		replacementQualityGateCount(afterJobs, runID, afterRun.RunAttempt, priorGate) == 1 {
		return WorkflowRerunResult{State: WorkflowRerunConfirmed, Reason: "selected-job-and-quality-gate-rerun-observed", ObservedAttempt: afterRun.RunAttempt}, nil
	}
	if postErr != nil && strings.Contains(strings.ToLower(postErr.Error()), "workflow run containing this job is already running") &&
		(afterRun.Status == "in_progress" || afterRun.Status == "queued") &&
		workflowJobStillFailed(afterJobs, job.ID, runID, run.RunAttempt) &&
		otherActiveJobCount(afterJobs, runID, run.RunAttempt, job.ID) > 0 {
		return WorkflowRerunResult{State: WorkflowRerunWaiting, Reason: "workflow-still-running", ObservedAttempt: afterRun.RunAttempt}, nil
	}
	if postErr != nil {
		lower := strings.ToLower(postErr.Error())
		for _, denied := range []string{"403", "resource not accessible", "permission", "not authorized"} {
			if strings.Contains(lower, denied) {
				return refuse("rerun-authorization-denied")
			}
		}
	}
	return uncertain("rerun-submission-not-confirmed", afterRun.RunAttempt)
}

func validateWorkflowRerunRequest(request WorkflowRerunRequest) (int64, error) {
	switch {
	case !rerunRepositoryPattern.MatchString(request.Repository):
		return 0, fmt.Errorf("workflow rerun: repository %q is not owner/name", request.Repository)
	case request.PullRequest <= 0:
		return 0, fmt.Errorf("workflow rerun: pull request must be positive, got %d", request.PullRequest)
	case !rerunNumericID.MatchString(request.RunID):
		return 0, fmt.Errorf("workflow rerun: workflow run %q is not a database ID", request.RunID)
	case strings.TrimSpace(request.JobName) == "":
		return 0, fmt.Errorf("workflow rerun: the diagnosed job name is required")
	case strings.TrimSpace(request.HeadSHA) == "" || strings.TrimSpace(request.TestedSHA) == "":
		return 0, fmt.Errorf("workflow rerun: the source head and tested commit are required")
	}
	runID, err := strconv.ParseInt(request.RunID, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("workflow rerun: workflow run %q: %w", request.RunID, err)
	}
	return runID, nil
}

type workflowRerunAPI struct {
	client     *CLIClient
	repository string
	number     int
	runID      string
}

func (a workflowRerunAPI) pullRequestView(ctx context.Context) (workflowRerunPullRequest, error) {
	var pr workflowRerunPullRequest
	out, err := a.client.readCommand(ctx, "gh api pull request", "api", fmt.Sprintf("repos/%s/pulls/%d", a.repository, a.number))
	if err != nil {
		return pr, err
	}
	return pr, json.Unmarshal(out, &pr)
}

func (a workflowRerunAPI) run(ctx context.Context) (workflowRerunRun, error) {
	var run workflowRerunRun
	out, err := a.client.readCommand(ctx, "gh api workflow run", "api", fmt.Sprintf("repos/%s/actions/runs/%s", a.repository, a.runID))
	if err != nil {
		return run, err
	}
	return run, json.Unmarshal(out, &run)
}

func (a workflowRerunAPI) jobs(ctx context.Context) ([]workflowRerunJob, error) {
	out, err := a.client.readCommand(ctx, "gh api workflow jobs", "api", "--paginate",
		fmt.Sprintf("repos/%s/actions/runs/%s/jobs?filter=latest&per_page=100", a.repository, a.runID))
	if err != nil {
		return nil, err
	}
	return decodeWorkflowJobPages(out)
}

// rerun submits the one mutation. It is deliberately not a read: a timeout may
// follow a successful remote write, so it is never retried by availability.
func (a workflowRerunAPI) rerun(ctx context.Context, jobID int64) error {
	callCtx, cancel := a.client.boundContext(ctx)
	defer cancel()
	cmd := a.client.command(callCtx, "gh", "api", "--method", "POST", fmt.Sprintf("repos/%s/actions/jobs/%d/rerun", a.repository, jobID))
	_, err := a.client.runCmd(callCtx, cmd, "gh api rerun workflow job")
	return err
}

// decodeWorkflowJobPages reads the concatenated JSON documents `gh api
// --paginate` prints, one per page.
func decodeWorkflowJobPages(data []byte) ([]workflowRerunJob, error) {
	var jobs []workflowRerunJob
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var page struct {
			Jobs []workflowRerunJob `json:"jobs"`
		}
		if err := decoder.Decode(&page); err != nil {
			if err == io.EOF {
				return jobs, nil
			}
			return nil, fmt.Errorf("parse workflow jobs: %w", err)
		}
		jobs = append(jobs, page.Jobs...)
	}
}

func workflowRerunPullRequestMatches(pr workflowRerunPullRequest, request WorkflowRerunRequest) bool {
	return pr.Number == request.PullRequest && strings.EqualFold(pr.State, "open") && pr.MergedAt == nil &&
		pr.Head.SHA == request.HeadSHA && strings.EqualFold(pr.Base.Repo.FullName, request.Repository)
}

func workflowRerunRunMatches(run workflowRerunRun, request WorkflowRerunRequest, runID int64) bool {
	if run.ID != runID || !strings.EqualFold(run.Repository.FullName, request.Repository) ||
		run.Event != "pull_request" || run.HeadSHA != request.TestedSHA {
		return false
	}
	linked := 0
	for _, pr := range run.PullRequests {
		if pr.Number == request.PullRequest && pr.Head.SHA == request.HeadSHA {
			linked++
		}
	}
	return linked == 1
}

func workflowRerunFailed(conclusion string) bool {
	return conclusion == "failure" || conclusion == "timed_out"
}

// selectWorkflowRerunJob finds the one job of the latest attempt with the
// diagnosed name. Several jobs of that name leave the diagnosis ambiguous.
func selectWorkflowRerunJob(jobs []workflowRerunJob, runID int64, attempt int, name string) (workflowRerunJob, bool) {
	var selected workflowRerunJob
	matches := 0
	for _, job := range jobs {
		if job.RunID == runID && job.RunAttempt == attempt && job.Name == name {
			selected = job
			matches++
		}
	}
	if matches != 1 {
		return workflowRerunJob{}, false
	}
	failed := selected.Status == "completed" && workflowRerunFailed(selected.Conclusion)
	active := selected.Status == "queued" || selected.Status == "in_progress"
	return selected, failed || active
}

func qualityGateJobID(jobs []workflowRerunJob, runID int64, attempt int) int64 {
	for _, job := range jobs {
		if job.RunID == runID && job.RunAttempt == attempt && job.Name == qualityGateJobName {
			return job.ID
		}
	}
	return 0
}

func workflowJobKnownStatus(status string) bool {
	return status == "queued" || status == "in_progress" || status == "completed"
}

func replacementJobCount(jobs []workflowRerunJob, runID int64, attempt int, previousID int64, name, workflow string) int {
	count := 0
	for _, job := range jobs {
		if job.ID != previousID && job.RunID == runID && job.RunAttempt == attempt && job.Name == name &&
			(workflow == "" || job.WorkflowName == workflow) && workflowJobKnownStatus(job.Status) {
			count++
		}
	}
	return count
}

func replacementQualityGateCount(jobs []workflowRerunJob, runID int64, attempt int, previousID int64) int {
	count := 0
	for _, job := range jobs {
		if job.ID != previousID && job.RunID == runID && job.RunAttempt == attempt && job.Name == qualityGateJobName &&
			workflowJobKnownStatus(job.Status) {
			count++
		}
	}
	return count
}

func workflowJobStillFailed(jobs []workflowRerunJob, id, runID int64, attempt int) bool {
	for _, job := range jobs {
		if job.ID == id && job.RunID == runID && job.RunAttempt == attempt &&
			job.Status == "completed" && workflowRerunFailed(job.Conclusion) {
			return true
		}
	}
	return false
}

func otherActiveJobCount(jobs []workflowRerunJob, runID int64, attempt int, excluded int64) int {
	count := 0
	for _, job := range jobs {
		if job.ID != excluded && job.RunID == runID && job.RunAttempt == attempt &&
			(job.Status == "queued" || job.Status == "in_progress") {
			count++
		}
	}
	return count
}

// The production client keeps the optional workflow rerun capability the CI
// recovery window type-asserts against.
var _ WorkflowJobRerunner = (*CLIClient)(nil)
