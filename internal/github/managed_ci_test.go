package github

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidManagedCIExecutionStatusRequiresKnownConsistentPair(t *testing.T) {
	tests := []struct {
		name       string
		status     string
		conclusion string
		valid      bool
	}{
		{name: "queued", status: "queued", valid: true},
		{name: "running", status: "in_progress", valid: true},
		{name: "completed success", status: "completed", conclusion: "success", valid: true},
		{name: "completed startup failure", status: "completed", conclusion: "startup_failure", valid: true},
		{name: "unknown status", status: "mystery"},
		{name: "unknown conclusion", status: "completed", conclusion: "mystery"},
		{name: "completed without conclusion", status: "completed"},
		{name: "queued with conclusion", status: "queued", conclusion: "success"},
		{name: "running with conclusion", status: "in_progress", conclusion: "failure"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ValidManagedCIExecutionStatus(test.status, test.conclusion); got != test.valid {
				t.Fatalf("ValidManagedCIExecutionStatus(%q, %q) = %t, want %t",
					test.status, test.conclusion, got, test.valid)
			}
		})
	}
}

func TestCLIClient_ManagedCIConfirmsIncidentRunnerShortage(t *testing.T) {
	fixture := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("testdata", "managed-ci", "incident", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"repo","owner":{"login":"owner"}}`},
		{output: fixture("jobs.json")},
		{output: fixture("workflow-run.json")},
		{output: fixture("annotations.json")},
		{output: fixture("workflow-runs.json")},
	}}
	client := &CLIClient{runner: runner}
	observation, err := client.ObserveManagedCI(context.Background(), ManagedCIRequest{
		Repository:          "owner/repo",
		PullRequest:         17,
		BaseRefName:         "main",
		HeadSHA:             "incident-source-head",
		FailedWorkflowRunID: "45270155099",
		FailedChecks: []CIFailedCheck{{
			Name:       "build-macos",
			DetailsURL: "https://github.com/owner/repo/actions/runs/45270155099/job/138027527159",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(observation.Failures) != 1 || observation.Failures[0].Classification != ManagedCIRunnerShortage {
		t.Fatalf("failed-check evidence = %+v, want one verified runner shortage", observation.Failures)
	}
	if observation.Source.WorkflowID != 273743359 || observation.Source.RunID != 45270155099 ||
		observation.Source.RunNumber != 5288 || observation.Source.Attempt != 1 {
		t.Fatalf("source identity = %+v, want incident workflow/run/attempt", observation.Source)
	}
	if observation.Replacement != nil {
		t.Fatalf("replacement = %+v, want none in the incident source fixture", observation.Replacement)
	}
	for _, call := range runner.calls {
		if strings.Contains(strings.ToUpper(strings.Join(call.args, " ")), "POST") {
			t.Fatalf("managed CI observation attempted a mutation: %v", call.args)
		}
	}
	if len(runner.calls) != 5 {
		t.Fatalf("API calls = %d, want repository, source run, jobs, annotations and workflow runs", len(runner.calls))
	}
}

func TestCLIClient_ManagedCIReplacementLookupErrorPreservesSourceEvidence(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"repo","owner":{"login":"owner"}}`},
		{output: fixtureText(t, "jobs.json")},
		{output: fixtureText(t, "workflow-run.json")},
		{output: fixtureText(t, "annotations.json")},
		{err: errors.New("workflow run listing unavailable")},
	}}
	observation, err := (&CLIClient{runner: runner}).ObserveManagedCI(context.Background(), ManagedCIRequest{
		Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "incident-source-head",
		FailedWorkflowRunID: "45270155099",
		FailedChecks: []CIFailedCheck{{
			Name: "build-macos", DetailsURL: "https://github.com/owner/repo/actions/runs/45270155099/job/138027527159",
		}},
	})
	if err == nil {
		t.Fatal("replacement lookup error = nil, want inconclusive result")
	}
	if observation.Source.RunID != 45270155099 || len(observation.Failures) != 1 ||
		observation.Failures[0].Classification != ManagedCIRunnerShortage {
		t.Fatalf("partial observation = %+v, want validated source and shortage evidence alongside error", observation)
	}
	if observation.Replacement != nil {
		t.Fatalf("replacement = %+v, want unknown after lookup error", observation.Replacement)
	}
}

func TestCLIClient_ManagedCIDoesNotInferShortageFromCapacityNotice(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "managed-ci", "incident", "capacity-annotation.json"))
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"repo","owner":{"login":"owner"}}`},
		{output: fixtureText(t, "jobs.json")},
		{output: fixtureText(t, "workflow-run.json")},
		{output: string(fixture)},
	}}
	observation, err := (&CLIClient{runner: runner}).ObserveManagedCI(context.Background(), ManagedCIRequest{
		Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "incident-source-head",
		FailedWorkflowRunID: "45270155099",
		FailedChecks: []CIFailedCheck{{
			Name: "build-macos", DetailsURL: "https://github.com/owner/repo/actions/runs/45270155099/job/138027527159",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(observation.Failures) != 1 || observation.Failures[0].Classification != ManagedCIUnexplainedFailure {
		t.Fatalf("capacity-only evidence = %+v, want unexplained failure", observation.Failures)
	}
	if len(runner.calls) != 4 {
		t.Fatalf("API calls = %d, want no replacement lookup without a verified shortage", len(runner.calls))
	}
}

func TestClassifyManagedCIFailureRequiresCorroboratedRunnerAcquisitionEvidence(t *testing.T) {
	tests := []struct {
		name        string
		job         CIWorkflowJob
		annotations []checkRunAnnotation
		want        ManagedCIFailureClassification
	}{
		{
			name: "explicit runner acquisition failure",
			job:  CIWorkflowJob{Status: "completed", Conclusion: "cancelled"},
			annotations: []checkRunAnnotation{{
				Level: "failure", Message: "The job was not acquired by a runner within 10 minutes of the request.",
			}},
			want: ManagedCIRunnerShortage,
		},
		{
			name: "generic cancellation",
			job:  CIWorkflowJob{Status: "completed", Conclusion: "cancelled"},
			want: ManagedCIUnexplainedFailure,
		},
		{
			name: "capacity notice alone",
			job:  CIWorkflowJob{Status: "completed", Conclusion: "cancelled"},
			annotations: []checkRunAnnotation{{
				Level: "failure", Message: "GitHub-hosted runner capacity is currently limited.",
			}},
			want: ManagedCIUnexplainedFailure,
		},
		{
			name: "aggregate name is not runner shortage evidence",
			job:  CIWorkflowJob{Name: "quality-gate", Status: "completed", Conclusion: "failure"},
			annotations: []checkRunAnnotation{{
				Level: "failure", Message: "The quality gate failed after a required check failed.",
			}},
			want: ManagedCIUnexplainedFailure,
		},
		{
			name: "runner was assigned",
			job:  CIWorkflowJob{Status: "completed", Conclusion: "cancelled", RunnerID: 42, RunnerName: "GitHub Actions 12"},
			annotations: []checkRunAnnotation{{
				Level: "failure", Message: "The job was not acquired by a runner within 10 minutes of the request.",
			}},
			want: ManagedCIUnexplainedFailure,
		},
		{
			name: "job executed steps",
			job: CIWorkflowJob{Status: "completed", Conclusion: "cancelled", Steps: []CIWorkflowStep{
				{Name: "checkout", Status: "completed", Conclusion: "success"},
			}},
			annotations: []checkRunAnnotation{{
				Level: "failure", Message: "The job was not acquired by a runner within 10 minutes of the request.",
			}},
			want: ManagedCIUnexplainedFailure,
		},
		{
			name: "confirmed code test annotation",
			job: CIWorkflowJob{Status: "completed", Conclusion: "failure", RunnerID: 42, Steps: []CIWorkflowStep{
				{Name: "go test ./...", Status: "completed", Conclusion: "failure"},
			}},
			annotations: []checkRunAnnotation{{
				Level: "failure", Path: "internal/example_test.go", StartLine: 42, Message: "TestExample failed.",
			}},
			want: ManagedCIConfirmedCodeTestFailure,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyManagedCIFailure(test.job, test.annotations); got != test.want {
				t.Fatalf("classification = %q, want %q", got, test.want)
			}
		})
	}
}

func TestClassifyManagedCIFailureUsesExplicitAggregateCauseNotJobName(t *testing.T) {
	job := CIWorkflowJob{Status: "completed", Conclusion: "failure"}
	classification, causes := classifyManagedCIFailureEvidence(job, []checkRunAnnotation{{
		Level: "failure", Message: "build-macos must succeed (result: cancelled)",
	}})
	if classification != ManagedCIDependentFailure || len(causes) != 1 || causes[0] != "build-macos" {
		t.Fatalf("explicit failed-prerequisite annotation = (%q, %v), want dependent failure caused by build-macos",
			classification, causes)
	}

	classification, causes = classifyManagedCIFailureEvidence(
		CIWorkflowJob{Name: "quality-gate", Status: "completed", Conclusion: "failure"},
		[]checkRunAnnotation{{Level: "failure", Message: "The quality gate failed after a required check failed."}},
	)
	if classification != ManagedCIUnexplainedFailure || len(causes) != 0 {
		t.Fatalf("aggregate name without explicit cause = (%q, %v), want unexplained", classification, causes)
	}

	classification, causes = classifyManagedCIFailureEvidence(job, []checkRunAnnotation{
		{Level: "failure", Message: "build-macos must succeed (result: cancelled)"},
		{Level: "failure", Message: "An independent validation failed."},
	})
	if classification != ManagedCIUnexplainedFailure || len(causes) != 0 {
		t.Fatalf("mixed aggregate annotations = (%q, %v), want unexplained", classification, causes)
	}
}

func TestCLIClient_ManagedCIFollowsSameRunHigherAttempt(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"repo","owner":{"login":"owner"}}`},
		{output: fixtureText(t, "jobs.json")},
		{output: fixtureText(t, "workflow-run.json")},
		{output: fixtureText(t, "annotations.json")},
		{output: fixtureAt(t, "same-run-attempt", "workflow-runs.json")},
	}}
	observation, err := (&CLIClient{runner: runner}).ObserveManagedCI(context.Background(), ManagedCIRequest{
		Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "incident-source-head",
		FailedWorkflowRunID: "45270155099",
		FailedChecks: []CIFailedCheck{{
			Name: "build-macos", DetailsURL: "https://github.com/owner/repo/actions/runs/45270155099/job/138027527159",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if observation.Replacement == nil || observation.Replacement.RunID != observation.Source.RunID ||
		observation.Replacement.Attempt != observation.Source.Attempt+1 {
		t.Fatalf("replacement = %+v, source = %+v; want a verified higher attempt of the same run", observation.Replacement, observation.Source)
	}
	if observation.Replacement.Status != "queued" {
		t.Fatalf("replacement status = %q, want queued before job checks appear", observation.Replacement.Status)
	}
}

func TestCLIClient_ManagedCIRejectsContradictorySameRunNumber(t *testing.T) {
	runs := fixtureAt(t, "same-run-attempt", "workflow-runs.json")
	position := strings.LastIndex(runs, `"run_number": 5288`)
	if position < 0 {
		t.Fatal("same-run fixture has no replacement run number")
	}
	runs = runs[:position] + strings.Replace(runs[position:], `"run_number": 5288`, `"run_number": 5290`, 1)
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"repo","owner":{"login":"owner"}}`},
		{output: fixtureText(t, "jobs.json")},
		{output: fixtureText(t, "workflow-run.json")},
		{output: fixtureText(t, "annotations.json")},
		{output: runs},
	}}
	_, err := (&CLIClient{runner: runner}).ObserveManagedCI(context.Background(), ManagedCIRequest{
		Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "incident-source-head",
		FailedWorkflowRunID: "45270155099",
		FailedChecks: []CIFailedCheck{{
			Name: "build-macos", DetailsURL: "https://github.com/owner/repo/actions/runs/45270155099/job/138027527159",
		}},
	})
	if err == nil {
		t.Fatal("same run ID with a conflicting run number was accepted as a replacement")
	}
}

func TestCLIClient_ManagedCIUsesFailedJobAttemptWhenRunHasReplacement(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"repo","owner":{"login":"owner"}}`},
		{output: fixtureText(t, "jobs.json")},
		{output: fixtureText(t, "workflow-run.json")},
		{output: fixtureText(t, "annotations.json")},
		{output: fixtureAt(t, "same-run-attempt", "workflow-runs.json")},
	}}
	observation, err := (&CLIClient{runner: runner}).ObserveManagedCI(context.Background(), ManagedCIRequest{
		Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "incident-source-head",
		FailedWorkflowRunID: "45270155099",
		FailedChecks: []CIFailedCheck{{
			Name: "build-macos", DetailsURL: "https://github.com/owner/repo/actions/runs/45270155099/job/138027527159",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(observation.Failures) != 1 || observation.Failures[0].Classification != ManagedCIRunnerShortage ||
		observation.Source.Attempt != 1 || observation.Replacement == nil || observation.Replacement.Attempt != 2 {
		t.Fatalf("observation = %+v, want shortage in attempt 1 and its verified attempt-2 replacement", observation)
	}
}

func TestCLIClient_ManagedCIFollowsHigherNumberedWorkflowRun(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"repo","owner":{"login":"owner"}}`},
		{output: fixtureText(t, "jobs.json")},
		{output: fixtureText(t, "workflow-run.json")},
		{output: fixtureText(t, "annotations.json")},
		{output: fixtureAt(t, "new-run", "workflow-runs.json")},
	}}
	observation, err := (&CLIClient{runner: runner}).ObserveManagedCI(context.Background(), ManagedCIRequest{
		Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "incident-source-head",
		FailedWorkflowRunID: "45270155099",
		FailedChecks: []CIFailedCheck{{
			Name: "build-macos", DetailsURL: "https://github.com/owner/repo/actions/runs/45270155099/job/138027527159",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if observation.Replacement == nil || observation.Replacement.RunID != 45276896986 ||
		observation.Replacement.RunNumber != observation.Source.RunNumber+1 ||
		observation.Replacement.Attempt != 1 || observation.Replacement.Status != "queued" {
		t.Fatalf("replacement = %+v, source = %+v; want same-workflow higher-numbered queued run", observation.Replacement, observation.Source)
	}
}

func TestCLIClient_ManagedCIDiagnosesCompletedReplacementFailure(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"repo","owner":{"login":"owner"}}`},
		{output: fixtureText(t, "jobs.json")},
		{output: fixtureText(t, "workflow-run.json")},
		{output: fixtureText(t, "annotations.json")},
		{output: fixtureAt(t, "new-run", "completed-failure-runs.json")},
		{output: fixtureAt(t, "new-run", "completed-failure-attempt.json")},
		{output: fixtureAt(t, "new-run", "completed-failure-jobs.json")},
		{output: fixtureAt(t, "new-run", "completed-failure-annotations.json")},
	}}
	observation, err := (&CLIClient{runner: runner}).ObserveManagedCI(context.Background(), ManagedCIRequest{
		Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "incident-source-head",
		FailedWorkflowRunID: "45270155099",
		FailedChecks: []CIFailedCheck{{
			Name: "build-macos", DetailsURL: "https://github.com/owner/repo/actions/runs/45270155099/job/138027527159",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if observation.Replacement == nil || observation.Replacement.RunID != 45276896986 ||
		len(observation.ReplacementFailures) != 1 ||
		observation.ReplacementFailures[0].Classification != ManagedCIConfirmedCodeTestFailure ||
		observation.ReplacementFailures[0].RunID != observation.Replacement.RunID ||
		observation.ReplacementFailures[0].Attempt != observation.Replacement.Attempt {
		t.Fatalf("completed replacement diagnosis = %+v; want its independently confirmed test failure", observation)
	}
	for _, call := range runner.calls {
		if strings.Contains(strings.ToUpper(strings.Join(call.args, " ")), "POST") {
			t.Fatalf("managed CI diagnosis attempted a mutation: %v", call.args)
		}
	}
}

func TestCLIClient_ManagedCIRequiresEveryReplacementIdentityField(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(map[string]any)
		wantUnknown bool
	}{
		{
			name:        "wrong pull request",
			wantUnknown: true,
			mutate: func(candidate map[string]any) {
				candidate["pull_requests"].([]any)[0].(map[string]any)["number"] = float64(18)
			},
		},
		{
			name:        "wrong base",
			wantUnknown: true,
			mutate: func(candidate map[string]any) {
				candidate["pull_requests"].([]any)[0].(map[string]any)["base"].(map[string]any)["ref"] = "release"
			},
		},
		{
			name:        "wrong source head",
			wantUnknown: true,
			mutate: func(candidate map[string]any) {
				candidate["pull_requests"].([]any)[0].(map[string]any)["head"].(map[string]any)["sha"] = "different-head"
			},
		},
		{
			name:        "wrong workflow",
			wantUnknown: true,
			mutate: func(candidate map[string]any) {
				candidate["workflow_id"] = float64(99)
			},
		},
		{
			name: "timestamp branch and check name without run ordering",
			mutate: func(candidate map[string]any) {
				candidate["run_number"] = float64(5287)
				candidate["created_at"] = "2099-01-01T00:00:00Z"
				candidate["head_branch"] = "same-branch"
				candidate["name"] = "same-check"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var page map[string]any
			if err := json.Unmarshal([]byte(fixtureAt(t, "new-run", "workflow-runs.json")), &page); err != nil {
				t.Fatal(err)
			}
			runs := page["workflow_runs"].([]any)
			test.mutate(runs[1].(map[string]any))
			encoded, err := json.Marshal(page)
			if err != nil {
				t.Fatal(err)
			}
			runner := &fakeRunner{responses: []fakeResponse{
				{output: `{"name":"repo","owner":{"login":"owner"}}`},
				{output: fixtureText(t, "jobs.json")},
				{output: fixtureText(t, "workflow-run.json")},
				{output: fixtureText(t, "annotations.json")},
				{output: string(encoded)},
			}}
			observation, err := (&CLIClient{runner: runner}).ObserveManagedCI(context.Background(), ManagedCIRequest{
				Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "incident-source-head",
				FailedWorkflowRunID: "45270155099",
				FailedChecks: []CIFailedCheck{{
					Name: "build-macos", DetailsURL: "https://github.com/owner/repo/actions/runs/45270155099/job/138027527159",
				}},
			})
			if test.wantUnknown {
				if err == nil {
					t.Fatalf("contradictory newer run identity was treated as proof no replacement exists")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if observation.Replacement != nil {
				t.Fatalf("mismatched candidate accepted as replacement: %+v", observation.Replacement)
			}
		})
	}
}

func TestCLIClient_ManagedCIReportsIncompleteReplacementLookup(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"repo","owner":{"login":"owner"}}`},
		{output: fixtureText(t, "jobs.json")},
		{output: fixtureText(t, "workflow-run.json")},
		{output: fixtureText(t, "annotations.json")},
		{output: fixtureAt(t, "incomplete", "workflow-runs.json")},
	}}
	_, err := (&CLIClient{runner: runner}).ObserveManagedCI(context.Background(), ManagedCIRequest{
		Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "incident-source-head",
		FailedWorkflowRunID: "45270155099",
		FailedChecks: []CIFailedCheck{{
			Name: "build-macos", DetailsURL: "https://github.com/owner/repo/actions/runs/45270155099/job/138027527159",
		}},
	})
	if err == nil {
		t.Fatal("incomplete workflow-run list was treated as proof that no replacement exists")
	}
}

func TestCLIClient_ManagedCIReportsUnlinkedNewerRunAsUnknown(t *testing.T) {
	var page map[string]any
	if err := json.Unmarshal([]byte(fixtureAt(t, "new-run", "workflow-runs.json")), &page); err != nil {
		t.Fatal(err)
	}
	candidate := page["workflow_runs"].([]any)[1].(map[string]any)
	delete(candidate, "pull_requests")
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"repo","owner":{"login":"owner"}}`},
		{output: fixtureText(t, "jobs.json")},
		{output: fixtureText(t, "workflow-run.json")},
		{output: fixtureText(t, "annotations.json")},
		{output: string(encoded)},
	}}
	_, err = (&CLIClient{runner: runner}).ObserveManagedCI(context.Background(), ManagedCIRequest{
		Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "incident-source-head",
		FailedWorkflowRunID: "45270155099",
		FailedChecks: []CIFailedCheck{{
			Name: "build-macos", DetailsURL: "https://github.com/owner/repo/actions/runs/45270155099/job/138027527159",
		}},
	})
	if err == nil {
		t.Fatal("newer same-head run without PR linkage was treated as proof no replacement exists")
	}
}

func fixtureText(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "managed-ci", "incident", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func fixtureAt(t *testing.T, directory, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "managed-ci", directory, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
