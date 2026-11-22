package github

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

const (
	rerunRepo     = "rafaelromao/sandman"
	rerunHead     = "source-head"
	rerunTested   = "tested-sha"
	rerunRunID    = "101"
	rerunJobName  = "opencode-canary"
	rerunOldJobID = 11
	rerunOldGate  = 15
	rerunNewJobID = 31
	rerunNewGate  = 35
)

func rerunRequest() WorkflowRerunRequest {
	return WorkflowRerunRequest{
		Repository: rerunRepo, PullRequest: 18, HeadSHA: rerunHead, TestedSHA: rerunTested,
		RunID: rerunRunID, JobName: rerunJobName,
	}
}

func rerunPR(state, head string) string {
	return fmt.Sprintf(`{"number":18,"state":%q,"merged_at":null,"head":{"sha":%q},"base":{"repo":{"full_name":"RafaelRomao/Sandman"}}}`, state, head)
}

func rerunRun(attempt int, status, conclusion string) string {
	return fmt.Sprintf(`{"id":101,"event":"pull_request","head_sha":%q,"run_attempt":%d,"status":%q,"conclusion":%q,`+
		`"repository":{"full_name":"rafaelromao/sandman"},"pull_requests":[{"number":18,"head":{"sha":%q}}]}`,
		rerunTested, attempt, status, conclusion, rerunHead)
}

func rerunJob(id, attempt int, name, status, conclusion string) string {
	return fmt.Sprintf(`{"id":%d,"run_id":101,"run_attempt":%d,"name":%q,"status":%q,"conclusion":%q,"workflow_name":"CI"}`,
		id, attempt, name, status, conclusion)
}

func rerunJobs(jobs ...string) string { return `{"jobs":[` + strings.Join(jobs, ",") + `]}` }

// Attempt 1 failed in the diagnosed job and its quality gate.
func failedAttempt() string {
	return rerunJobs(
		rerunJob(rerunOldJobID, 1, rerunJobName, "completed", "failure"),
		rerunJob(rerunOldGate, 1, "quality-gate", "completed", "failure"),
		rerunJob(12, 1, "build-macos", "completed", "success"),
	)
}

func rerunRunnerWithReads(extra ...fakeResponse) *fakeRunner {
	return &fakeRunner{responses: append([]fakeResponse{
		{output: rerunPR("open", rerunHead)},
		{output: rerunRun(1, "completed", "failure")},
		{output: failedAttempt()},
	}, extra...)}
}

// messageRunner keeps a scripted failure's text on stderr so the client can
// classify it, which the plain fake runner discards.
type messageRunner struct{ *fakeRunner }

func (m messageRunner) Run(ctx context.Context, name string, arg ...string) *exec.Cmd {
	idx := len(m.calls)
	cmd := m.fakeRunner.Run(ctx, name, arg...)
	if idx < len(m.responses) && m.responses[idx].err != nil {
		return exec.Command("sh", "-c", `printf '%s' "$1" >&2; exit 1`, "sh", m.responses[idx].err.Error())
	}
	return cmd
}

func callArgs(runner *fakeRunner) [][]string {
	var calls [][]string
	for _, call := range runner.calls {
		calls = append(calls, call.args)
	}
	return calls
}

func TestCLIClient_RerunWorkflowJob_ConfirmsTheRerunOfTheSelectedJobAndItsQualityGate(t *testing.T) {
	runner := rerunRunnerWithReads(
		fakeResponse{output: `{}`}, // POST
		fakeResponse{output: rerunRun(2, "in_progress", "")},
		fakeResponse{output: rerunPR("open", rerunHead)},
		fakeResponse{output: rerunJobs(
			rerunJob(rerunNewJobID, 2, rerunJobName, "in_progress", ""),
			rerunJob(rerunNewGate, 2, "quality-gate", "queued", ""),
		)},
	)
	result, err := (&CLIClient{runner: messageRunner{runner}}).RerunWorkflowJob(context.Background(), rerunRequest())
	if err != nil {
		t.Fatal(err)
	}
	want := WorkflowRerunResult{State: WorkflowRerunConfirmed, Reason: "selected-job-and-quality-gate-rerun-observed", ObservedAttempt: 2}
	if result != want {
		t.Fatalf("result = %+v, want %+v", result, want)
	}
	posts := 0
	for _, args := range callArgs(runner) {
		if strings.Contains(strings.Join(args, " "), "--method POST") {
			posts++
			wantPost := []string{"api", "--method", "POST", "repos/rafaelromao/sandman/actions/jobs/11/rerun"}
			if !reflect.DeepEqual(args, wantPost) {
				t.Fatalf("mutation = %v, want the failed job's database ID rerun %v", args, wantPost)
			}
		}
	}
	if posts != 1 || len(runner.calls) != 7 {
		t.Fatalf("posts = %d calls = %d, want one mutation between three reads and three read-backs", posts, len(runner.calls))
	}
}

// The mutation is never repeated, even when its outcome cannot be confirmed.
func TestCLIClient_RerunWorkflowJob_NeverSubmitsTwice(t *testing.T) {
	runner := rerunRunnerWithReads(
		fakeResponse{output: `{}`},
		fakeResponse{output: rerunRun(1, "completed", "failure")}, // no new attempt appeared
		fakeResponse{output: rerunPR("open", rerunHead)},
		fakeResponse{output: failedAttempt()},
	)
	result, err := (&CLIClient{runner: messageRunner{runner}}).RerunWorkflowJob(context.Background(), rerunRequest())
	if err != nil {
		t.Fatal(err)
	}
	if result.State != WorkflowRerunUncertain || result.Reason != "rerun-submission-not-confirmed" {
		t.Fatalf("result = %+v, want an unconfirmed submission", result)
	}
	posts := 0
	for _, args := range callArgs(runner) {
		if strings.Contains(strings.Join(args, " "), "--method POST") {
			posts++
		}
	}
	if posts != 1 {
		t.Fatalf("mutations = %d, want exactly one", posts)
	}
}

func TestCLIClient_RerunWorkflowJob_RefusesWithoutMutatingWhenEvidenceDoesNotMatch(t *testing.T) {
	for name, tc := range map[string]struct {
		responses []fakeResponse
		reason    string
	}{
		"pull request head moved": {
			responses: []fakeResponse{{output: rerunPR("open", "other-head")}},
			reason:    "pull-request-or-source-head-mismatch",
		},
		"pull request closed": {
			responses: []fakeResponse{{output: rerunPR("closed", rerunHead)}},
			reason:    "pull-request-or-source-head-mismatch",
		},
		"pull request lookup failed": {
			responses: []fakeResponse{{err: errors.New("exit status 1")}},
			reason:    "pull-request-lookup-failed",
		},
		"workflow run tested another sha": {
			responses: []fakeResponse{
				{output: rerunPR("open", rerunHead)},
				{output: strings.Replace(rerunRun(1, "completed", "failure"), rerunTested, "other-tested", 1)},
			},
			reason: "workflow-run-linkage-mismatch",
		},
		"diagnosed job absent from the latest attempt": {
			responses: []fakeResponse{
				{output: rerunPR("open", rerunHead)},
				{output: rerunRun(1, "completed", "failure")},
				{output: rerunJobs(rerunJob(12, 1, "build-macos", "completed", "success"))},
			},
			reason: "failed-job-not-in-current-attempt",
		},
		"diagnosed job already passed": {
			responses: []fakeResponse{
				{output: rerunPR("open", rerunHead)},
				{output: rerunRun(1, "completed", "success")},
				{output: rerunJobs(rerunJob(rerunOldJobID, 1, rerunJobName, "completed", "success"))},
			},
			reason: "failed-job-not-in-current-attempt",
		},
		"diagnosed job name is ambiguous": {
			responses: []fakeResponse{
				{output: rerunPR("open", rerunHead)},
				{output: rerunRun(1, "completed", "failure")},
				{output: rerunJobs(
					rerunJob(rerunOldJobID, 1, rerunJobName, "completed", "failure"),
					rerunJob(14, 1, rerunJobName, "completed", "failure"),
				)},
			},
			reason: "failed-job-not-in-current-attempt",
		},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &fakeRunner{responses: tc.responses}
			result, err := (&CLIClient{runner: messageRunner{runner}}).RerunWorkflowJob(context.Background(), rerunRequest())
			if err != nil {
				t.Fatal(err)
			}
			if result.State != WorkflowRerunRefused || result.Reason != tc.reason {
				t.Fatalf("result = %+v, want refused %q", result, tc.reason)
			}
			for _, args := range callArgs(runner) {
				if strings.Contains(strings.Join(args, " "), "--method POST") {
					t.Fatalf("mutated despite a refusal: %v", args)
				}
			}
		})
	}
}

// A workflow that is still running is a wait, not a rerun.
func TestCLIClient_RerunWorkflowJob_WaitsForARunningWorkflow(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: rerunPR("open", rerunHead)},
		{output: rerunRun(2, "in_progress", "")},
		{output: rerunJobs(
			rerunJob(rerunNewJobID, 2, rerunJobName, "completed", "failure"),
			rerunJob(rerunNewGate, 2, "quality-gate", "in_progress", ""),
		)},
	}}
	result, err := (&CLIClient{runner: messageRunner{runner}}).RerunWorkflowJob(context.Background(), rerunRequest())
	if err != nil {
		t.Fatal(err)
	}
	if result.State != WorkflowRerunWaiting || result.Reason != "workflow-still-running" || result.ObservedAttempt != 2 {
		t.Fatalf("result = %+v, want waiting on the running attempt 2", result)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("calls = %d, want only the three reads", len(runner.calls))
	}
}

func TestCLIClient_RerunWorkflowJob_ClassifiesARejectedSubmission(t *testing.T) {
	for name, tc := range map[string]struct {
		post  fakeResponse
		state WorkflowRerunState
		why   string
	}{
		"authorization denied": {post: fakeResponse{err: errors.New("HTTP 403: Resource not accessible by integration")}, state: WorkflowRerunRefused, why: "rerun-authorization-denied"},
		"transport failure":    {post: fakeResponse{err: errors.New("connection reset")}, state: WorkflowRerunUncertain, why: "rerun-submission-not-confirmed"},
	} {
		t.Run(name, func(t *testing.T) {
			runner := rerunRunnerWithReads(
				tc.post,
				fakeResponse{output: rerunRun(1, "completed", "failure")},
				fakeResponse{output: rerunPR("open", rerunHead)},
				fakeResponse{output: failedAttempt()},
			)
			result, err := (&CLIClient{runner: messageRunner{runner}}).RerunWorkflowJob(context.Background(), rerunRequest())
			if err != nil {
				t.Fatal(err)
			}
			if result.State != tc.state || result.Reason != tc.why {
				t.Fatalf("result = %+v, want %s %q", result, tc.state, tc.why)
			}
		})
	}
}

// A lost read-back after the one mutation is uncertain, never a second POST.
func TestCLIClient_RerunWorkflowJob_UnreadableReadBackIsUncertain(t *testing.T) {
	runner := rerunRunnerWithReads(
		fakeResponse{output: `{}`},
		fakeResponse{err: errors.New("gh: timeout")},
	)
	result, err := (&CLIClient{runner: messageRunner{runner}}).RerunWorkflowJob(context.Background(), rerunRequest())
	if err != nil {
		t.Fatal(err)
	}
	if result.State != WorkflowRerunUncertain || result.Reason != "rerun-readback-failed" {
		t.Fatalf("result = %+v, want uncertain read-back", result)
	}
}

func TestCLIClient_RerunWorkflowJob_CancellationIsAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner := &fakeRunner{responses: []fakeResponse{{err: exec.ErrNotFound}}}
	if _, err := (&CLIClient{runner: messageRunner{runner}}).RerunWorkflowJob(ctx, rerunRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("a cancelled attempt ran %d commands", len(runner.calls))
	}
}

func TestCLIClient_RerunWorkflowJob_RejectsAnIncompleteRequest(t *testing.T) {
	for name, mutate := range map[string]func(*WorkflowRerunRequest){
		"repository":   func(r *WorkflowRerunRequest) { r.Repository = "no-slash" },
		"pull request": func(r *WorkflowRerunRequest) { r.PullRequest = 0 },
		"run":          func(r *WorkflowRerunRequest) { r.RunID = "abc" },
		"job name":     func(r *WorkflowRerunRequest) { r.JobName = " " },
		"head":         func(r *WorkflowRerunRequest) { r.HeadSHA = "" },
		"tested":       func(r *WorkflowRerunRequest) { r.TestedSHA = "" },
	} {
		t.Run(name, func(t *testing.T) {
			request := rerunRequest()
			mutate(&request)
			runner := &fakeRunner{}
			if _, err := (&CLIClient{runner: messageRunner{runner}}).RerunWorkflowJob(context.Background(), request); err == nil {
				t.Fatal("incomplete request was accepted")
			}
			if len(runner.calls) != 0 {
				t.Fatalf("an invalid request ran %d commands", len(runner.calls))
			}
		})
	}
}

// Paginated `gh api --paginate` output concatenates one JSON document per page.
func TestDecodeWorkflowJobPagesReadsEveryPage(t *testing.T) {
	jobs, err := decodeWorkflowJobPages([]byte(rerunJobs(rerunJob(1, 1, "a", "completed", "success")) + "\n" + rerunJobs(rerunJob(2, 1, "b", "completed", "failure"))))
	if err != nil || len(jobs) != 2 || jobs[1].Name != "b" {
		t.Fatalf("jobs = %+v, err = %v, want both pages", jobs, err)
	}
	if _, err := decodeWorkflowJobPages([]byte(`{"jobs":`)); err == nil {
		t.Fatal("truncated page decoded")
	}
}
