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

func TestCLIClient_ManagedCIRequiresHostedRunnerTypeEvidenceForIncident(t *testing.T) {
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
	if len(observation.Failures) != 1 || observation.Failures[0].Classification != ManagedCIUnexplainedFailure {
		t.Fatalf("failed-check evidence = %+v, want unexplained without explicit hosted-runner evidence", observation.Failures)
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
	if len(runner.calls) != 4 {
		t.Fatalf("API calls = %d, want no replacement lookup without explicit hosted-runner evidence", len(runner.calls))
	}
}

func TestCLIClient_ManagedCIReplaysIncidentAndFindsReplacementOutsideRollup(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{output: fixtureAt(t, "incident-replay", "jobs.json")},
		{output: fixtureAt(t, "incident-replay", "source-run.json")},
		{output: fixtureAt(t, "incident-replay", "shortage-annotations.json")},
		{output: fixtureAt(t, "incident-replay", "aggregate-annotations.json")},
		{output: fixtureAt(t, "incident-replay", "workflow-runs.json")},
	}}
	observation, err := (&CLIClient{runner: runner}).ObserveManagedCI(context.Background(), ManagedCIRequest{
		Repository: "rafaelromao/sandman", PullRequest: 3029, BaseRefName: "main",
		HeadSHA: "f454c9077c0c41046fe8429e57777795b66e46a7", FailedWorkflowRunID: "45270155099",
		FailedChecks: []CIFailedCheck{
			{
				Name:       "build (macos-latest)",
				DetailsURL: "https://github.com/rafaelromao/sandman/actions/runs/45270155099/job/138027527159",
			},
			{
				Name:       "quality-gate",
				DetailsURL: "https://github.com/rafaelromao/sandman/actions/runs/45270155099/job/138036364100",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(observation.Failures) != 2 ||
		observation.Failures[0].Classification != ManagedCIRunnerShortage ||
		observation.Failures[1].Classification != ManagedCIDependentFailure ||
		len(observation.Failures[1].CausedByChecks) != 1 ||
		observation.Failures[1].CausedByChecks[0] != "build-macos" ||
		len(observation.Failures[1].CausedByResults) != 1 ||
		observation.Failures[1].CausedByResults[0] != "abandoned" ||
		len(observation.SourceJobs) != 2 {
		t.Fatalf("incident failures = %+v, want shortage plus its explicitly dependent quality gate", observation.Failures)
	}
	if observation.Replacement == nil || observation.Replacement.RunID != 45276896986 ||
		observation.Replacement.RunNumber != 5289 || observation.Replacement.Conclusion != "success" {
		t.Fatalf("replacement = %+v, want verified same-PR/head/workflow run 45276896986", observation.Replacement)
	}
	for _, call := range runner.calls {
		if strings.Contains(strings.ToUpper(strings.Join(call.args, " ")), "POST") {
			t.Fatalf("managed CI observation attempted a mutation: %v", call.args)
		}
	}
}

func TestCLIClient_ManagedCILeavesUnrelatedAnnotatedActionFailureUnexplained(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"repo","owner":{"login":"owner"}}`},
		{output: fixtureAt(t, "unrelated-annotation", "jobs.json")},
		{output: fixtureAt(t, "unrelated-annotation", "workflow-run.json")},
		{output: fixtureAt(t, "unrelated-annotation", "annotations.json")},
	}}
	observation, err := (&CLIClient{runner: runner}).ObserveManagedCI(context.Background(), ManagedCIRequest{
		Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "incident-source-head",
		FailedWorkflowRunID: "45270155099",
		FailedChecks: []CIFailedCheck{{
			Name: "unit-tests", DetailsURL: "https://github.com/owner/repo/actions/runs/45270155099/job/138027527160",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(observation.Failures) != 1 ||
		observation.Failures[0].Classification != ManagedCIUnexplainedFailure {
		t.Fatalf("failed-check evidence = %+v, want one unexplained action failure", observation.Failures)
	}
	if observation.Replacement != nil {
		t.Fatalf("replacement = %+v, want none without verified runner shortage", observation.Replacement)
	}
	if len(runner.calls) != 4 {
		t.Fatalf("API calls = %d, want repository, source run, jobs and annotations only", len(runner.calls))
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
			name: "runner acquisition without hosted type is unexplained",
			job:  observedUnassignedRunnerJob(),
			annotations: []checkRunAnnotation{{
				Level: "failure", Message: "The job was not acquired by a runner within 10 minutes of the request.",
			}},
			want: ManagedCIUnexplainedFailure,
		},
		{
			name: "explicit GitHub-hosted runner acquisition failure",
			job:  observedUnassignedRunnerJob(),
			annotations: []checkRunAnnotation{{
				Level: "failure", Message: "The job was not acquired by a GitHub-hosted runner within 10 minutes of the request. Job was cancelled.",
			}},
			want: ManagedCIRunnerShortage,
		},
		{
			name: "GitHub hosted runner incident annotation",
			job:  observedUnassignedRunnerJob(),
			annotations: []checkRunAnnotation{{
				Level: "failure", Message: "The job was not acquired by Runner of type hosted even after multiple attempts",
			}},
			want: ManagedCIRunnerShortage,
		},
		{
			name: "missing runner assignment fields are inconclusive",
			job: CIWorkflowJob{
				Status: "completed", Conclusion: "cancelled", Steps: []CIWorkflowStep{},
				runnerNameObserved: true, stepsObserved: true,
			},
			annotations: []checkRunAnnotation{{
				Level: "failure", Message: "The job was not acquired by Runner of type hosted even after multiple attempts",
			}},
			want: ManagedCIUnexplainedFailure,
		},
		{
			name: "missing steps field is inconclusive",
			job: CIWorkflowJob{
				Status: "completed", Conclusion: "cancelled",
				runnerIDObserved: true, runnerNameObserved: true,
			},
			annotations: []checkRunAnnotation{{
				Level: "failure", Message: "The job was not acquired by Runner of type hosted even after multiple attempts",
			}},
			want: ManagedCIUnexplainedFailure,
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
			name: "generic runner acquisition wording",
			job:  observedUnassignedRunnerJob(),
			annotations: []checkRunAnnotation{{
				Level: "failure", Message: "The job was not acquired by a runner because workflow access was denied.",
			}},
			want: ManagedCIUnexplainedFailure,
		},
		{
			name: "runner acquisition annotation identifies a self-hosted pool",
			job:  observedUnassignedRunnerJob(),
			annotations: []checkRunAnnotation{{
				Level: "failure",
				Message: "The job was not acquired by a runner within 10 minutes of the request " +
					"for the self-hosted pool.",
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
		{
			name: "go test failure output",
			job: CIWorkflowJob{Status: "completed", Conclusion: "failure", RunnerID: 42, Steps: []CIWorkflowStep{
				{Name: "go test ./...", Status: "completed", Conclusion: "failure"},
			}},
			annotations: []checkRunAnnotation{{
				Level: "failure", Path: "internal/example_test.go", StartLine: 42,
				Message: "--- FAIL: TestExample (0.00s)",
			}},
			want: ManagedCIConfirmedCodeTestFailure,
		},
		{
			name: "source-linked go compiler error in build step",
			job: CIWorkflowJob{Status: "completed", Conclusion: "failure", RunnerID: 42, Steps: []CIWorkflowStep{
				{Name: "Run go build ./...", Status: "completed", Conclusion: "failure"},
			}},
			annotations: []checkRunAnnotation{{
				Level: "failure", Path: "internal/example.go", StartLine: 42,
				Message: "undefined: missingSymbol",
			}},
			want: ManagedCIConfirmedCodeTestFailure,
		},
		{
			name: "source-linked compiler error in package build step",
			job: CIWorkflowJob{Status: "completed", Conclusion: "failure", RunnerID: 42, Steps: []CIWorkflowStep{
				{Name: "npm run build", Status: "completed", Conclusion: "failure"},
			}},
			annotations: []checkRunAnnotation{{
				Level: "failure", Path: "src/app.ts", StartLine: 42,
				Message: "Cannot use type 'number' as type 'string'.",
			}},
			want: ManagedCIConfirmedCodeTestFailure,
		},
		{
			name: "compiler diagnostic on unrelated step is unexplained",
			job: CIWorkflowJob{Status: "completed", Conclusion: "failure", RunnerID: 42, Steps: []CIWorkflowStep{
				{Name: "Upload coverage artifact", Status: "completed", Conclusion: "failure"},
			}},
			annotations: []checkRunAnnotation{{
				Level: "failure", Path: "internal/example.go", StartLine: 42,
				Message: "undefined: missingSymbol",
			}},
			want: ManagedCIUnexplainedFailure,
		},
		{
			name: "infrastructure diagnostic in build step is unexplained",
			job: CIWorkflowJob{Status: "completed", Conclusion: "failure", RunnerID: 42, Steps: []CIWorkflowStep{
				{Name: "go build ./...", Status: "completed", Conclusion: "failure"},
			}},
			annotations: []checkRunAnnotation{{
				Level: "failure", Path: "internal/example.go", StartLine: 42,
				Message: "Compiler dependency download failed: connection refused by dependency mirror.",
			}},
			want: ManagedCIUnexplainedFailure,
		},
		{
			name: "file annotation on unrelated failed step is unexplained",
			job: CIWorkflowJob{Status: "completed", Conclusion: "failure", RunnerID: 42, Steps: []CIWorkflowStep{
				{Name: "Upload coverage artifact", Status: "completed", Conclusion: "failure"},
			}},
			annotations: []checkRunAnnotation{{
				Level: "failure", Path: "internal/example_test.go", StartLine: 42, Message: "failed to upload artifact.",
			}},
			want: ManagedCIUnexplainedFailure,
		},
		{
			name: "artifact annotation on failed test step is unexplained",
			job: CIWorkflowJob{Status: "completed", Conclusion: "failure", RunnerID: 42, Steps: []CIWorkflowStep{
				{Name: "go test ./...", Status: "completed", Conclusion: "failure"},
			}},
			annotations: []checkRunAnnotation{{
				Level: "failure", Path: "internal/example_test.go", StartLine: 42, Message: "failed to upload artifact.",
			}},
			want: ManagedCIUnexplainedFailure,
		},
		{
			name: "infrastructure status annotation is not a code test failure",
			job: CIWorkflowJob{Status: "completed", Conclusion: "failure", RunnerID: 42, Steps: []CIWorkflowStep{
				{Name: "go test ./...", Status: "completed", Conclusion: "failure"},
			}},
			annotations: []checkRunAnnotation{{
				Level: "failure", Path: "internal/dependency_test.go", StartLine: 42,
				Message: "Expected HTTP 200, got 503 from the dependency mirror.",
			}},
			want: ManagedCIUnexplainedFailure,
		},
		{
			name: "artifact diagnostic on a failed test step",
			job: CIWorkflowJob{Status: "completed", Conclusion: "failure", RunnerID: 42, Steps: []CIWorkflowStep{
				{Name: "go test ./...", Status: "completed", Conclusion: "failure"},
			}},
			annotations: []checkRunAnnotation{{
				Level: "failure", Path: "internal/example_test.go", StartLine: 42,
				Message: "Expected an artifact cache entry, got none.",
			}},
			want: ManagedCIUnexplainedFailure,
		},
		{
			name: "fixture provisioning diagnostic on a failed test step",
			job: CIWorkflowJob{Status: "completed", Conclusion: "failure", RunnerID: 42, Steps: []CIWorkflowStep{
				{Name: "go test ./...", Status: "completed", Conclusion: "failure"},
			}},
			annotations: []checkRunAnnotation{{
				Level: "failure", Path: "internal/example_test.go", StartLine: 42,
				Message: "Test fixture provisioning: expected a writable volume, got a read-only filesystem.",
			}},
			want: ManagedCIUnexplainedFailure,
		},
		{
			name: "workflow configuration annotation is not source evidence",
			job: CIWorkflowJob{Status: "completed", Conclusion: "failure", RunnerID: 42, Steps: []CIWorkflowStep{
				{Name: "go test ./...", Status: "completed", Conclusion: "failure"},
			}},
			annotations: []checkRunAnnotation{{
				Level: "failure", Path: ".github/workflows/go.yml", StartLine: 42,
				Message: "TestExample failed.",
			}},
			want: ManagedCIUnexplainedFailure,
		},
		{
			name: "test setup step is not a test execution",
			job: CIWorkflowJob{Status: "completed", Conclusion: "failure", RunnerID: 42, Steps: []CIWorkflowStep{
				{Name: "Test setup credentials", Status: "completed", Conclusion: "failure"},
			}},
			annotations: []checkRunAnnotation{{
				Level: "failure", Path: "internal/example_test.go", StartLine: 42,
				Message: "TestExample failed.",
			}},
			want: ManagedCIUnexplainedFailure,
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

func observedUnassignedRunnerJob() CIWorkflowJob {
	return CIWorkflowJob{
		Status: "completed", Conclusion: "cancelled", Steps: []CIWorkflowStep{},
		runnerIDObserved: true, runnerNameObserved: true, stepsObserved: true,
	}
}

func TestCIWorkflowJobJSONTracksExplicitMissingRunnerAndStepEvidence(t *testing.T) {
	annotation := []checkRunAnnotation{{
		Level: "failure", Message: "The job was not acquired by Runner of type hosted even after multiple attempts",
	}}
	tests := []struct {
		name string
		json string
		want ManagedCIFailureClassification
	}{
		{
			name: "explicit null runner and empty steps",
			json: `{"status":"completed","conclusion":"cancelled","runner_id":null,"runner_name":null,"steps":[]}`,
			want: ManagedCIRunnerShortage,
		},
		{
			name: "omitted runner id",
			json: `{"status":"completed","conclusion":"cancelled","runner_name":null,"steps":[]}`,
			want: ManagedCIUnexplainedFailure,
		},
		{
			name: "omitted runner name",
			json: `{"status":"completed","conclusion":"cancelled","runner_id":null,"steps":[]}`,
			want: ManagedCIUnexplainedFailure,
		},
		{
			name: "omitted steps",
			json: `{"status":"completed","conclusion":"cancelled","runner_id":null,"runner_name":null}`,
			want: ManagedCIUnexplainedFailure,
		},
		{
			name: "null steps",
			json: `{"status":"completed","conclusion":"cancelled","runner_id":null,"runner_name":null,"steps":null}`,
			want: ManagedCIUnexplainedFailure,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var job CIWorkflowJob
			if err := json.Unmarshal([]byte(test.json), &job); err != nil {
				t.Fatal(err)
			}
			if got := classifyManagedCIFailure(job, annotation); got != test.want {
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

	classification, causes = classifyManagedCIFailureEvidence(job, []checkRunAnnotation{
		{Level: "failure", Message: "Process completed with exit code 1."},
		{Level: "failure", Message: "build-macos must succeed (result: abandoned)"},
	})
	if classification != ManagedCIDependentFailure || len(causes) != 1 || causes[0] != "build-macos" {
		t.Fatalf("incident aggregate wrapper and prerequisite = (%q, %v), want only build-macos dependency",
			classification, causes)
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

func TestCLIClient_ManagedCIRejectsContradictorySameAttemptConclusion(t *testing.T) {
	runs := fixtureAt(t, "same-run-attempt", "workflow-runs.json")
	runs = strings.Replace(runs, `"conclusion": "cancelled"`, `"conclusion": "success"`, 1)
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
		t.Fatal("same-attempt workflow listing with a contradictory terminal conclusion was accepted")
	}
}

func TestManagedCIExecutionReadsCompatibleRequiresForwardStatusProgression(t *testing.T) {
	tests := []struct {
		name              string
		earlierStatus     string
		earlierConclusion string
		laterStatus       string
		laterConclusion   string
		want              bool
	}{
		{name: "same queued status", earlierStatus: "queued", laterStatus: "queued", want: true},
		{name: "queued advances to running", earlierStatus: "queued", laterStatus: "in_progress", want: true},
		{name: "running advances to completed", earlierStatus: "in_progress", laterStatus: "completed", laterConclusion: "cancelled", want: true},
		{name: "waiting advances to queued", earlierStatus: "waiting", laterStatus: "queued", want: true},
		{name: "running regresses to queued", earlierStatus: "in_progress", laterStatus: "queued"},
		{name: "queued regresses to pending", earlierStatus: "queued", laterStatus: "pending"},
		{name: "completed keeps its conclusion", earlierStatus: "completed", earlierConclusion: "cancelled", laterStatus: "completed", laterConclusion: "cancelled", want: true},
		{name: "completed changes its conclusion", earlierStatus: "completed", earlierConclusion: "cancelled", laterStatus: "completed", laterConclusion: "success"},
		{name: "unknown status", earlierStatus: "in_progress", laterStatus: "unknown"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			earlier := ManagedCIExecution{Status: test.earlierStatus, Conclusion: test.earlierConclusion}
			later := ManagedCIExecution{Status: test.laterStatus, Conclusion: test.laterConclusion}
			if got := managedCIExecutionReadsCompatible(earlier, later); got != test.want {
				t.Fatalf("status compatibility = %t, want %t", got, test.want)
			}
		})
	}
}

func TestCLIClient_ManagedCIRejectsRegressedSameAttemptStatus(t *testing.T) {
	source := actionsWorkflowRun{}
	if err := json.Unmarshal([]byte(fixtureText(t, "workflow-run.json")), &source); err != nil {
		t.Fatal(err)
	}
	source.Status = "in_progress"
	source.Conclusion = ""
	sourceJSON, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}

	runs := struct {
		TotalCount   int                  `json:"total_count"`
		WorkflowRuns []actionsWorkflowRun `json:"workflow_runs"`
	}{}
	if err := json.Unmarshal([]byte(fixtureAt(t, "same-run-attempt", "workflow-runs.json")), &runs); err != nil {
		t.Fatal(err)
	}
	runs.WorkflowRuns[0].Status = "queued"
	runs.WorkflowRuns[0].Conclusion = ""
	runsJSON, err := json.Marshal(runs)
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"repo","owner":{"login":"owner"}}`},
		{output: fixtureText(t, "jobs.json")},
		{output: string(sourceJSON)},
		{output: fixtureText(t, "annotations.json")},
		{output: string(runsJSON)},
	}}
	_, err = (&CLIClient{runner: runner}).ObserveManagedCI(context.Background(), ManagedCIRequest{
		Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "incident-source-head",
		FailedWorkflowRunID: "45270155099",
		FailedChecks: []CIFailedCheck{{
			Name: "build-macos", DetailsURL: "https://github.com/owner/repo/actions/runs/45270155099/job/138027527159",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "contradicts the source execution status") {
		t.Fatalf("same-attempt in-progress-to-queued regression was accepted: %v", err)
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
		observation.ReplacementFailures[0].Attempt != observation.Replacement.Attempt ||
		observation.ReplacementFailures[0].DetailsURL !=
			"https://github.com/owner/repo/actions/runs/45276896986/job/138027527160" {
		t.Fatalf("completed replacement diagnosis = %+v; want its independently confirmed test failure", observation)
	}
	for _, call := range runner.calls {
		if strings.Contains(strings.ToUpper(strings.Join(call.args, " ")), "POST") {
			t.Fatalf("managed CI diagnosis attempted a mutation: %v", call.args)
		}
	}
}

func TestCLIClient_DiagnosesRetainedCompletedExecutionFailure(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"repo","owner":{"login":"owner"}}`},
		{output: fixtureAt(t, "new-run", "completed-failure-attempt.json")},
		{output: fixtureAt(t, "new-run", "completed-failure-jobs.json")},
		{output: fixtureAt(t, "new-run", "completed-failure-annotations.json")},
	}}
	failures, err := (&CLIClient{runner: runner}).DiagnoseManagedCIExecutionFailure(
		context.Background(),
		ManagedCIExecution{
			Repository: "owner/repo", PullRequest: 17, BaseRefName: "main",
			HeadSHA: "incident-source-head", WorkflowID: 273743359, Event: "pull_request",
			RunID: 45276896986, RunNumber: 5289, Attempt: 1, Status: "completed", Conclusion: "failure",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 1 || failures[0].Classification != ManagedCIConfirmedCodeTestFailure ||
		failures[0].RunID != 45276896986 || failures[0].Attempt != 1 {
		t.Fatalf("retained execution diagnosis = %+v; want exact attempt's confirmed test failure", failures)
	}
	for _, call := range runner.calls {
		if strings.Contains(strings.ToUpper(strings.Join(call.args, " ")), "POST") {
			t.Fatalf("retained execution diagnosis attempted a mutation: %v", call.args)
		}
	}
}

func TestCLIClient_ManagedCIRejectsContradictoryReplacementAttemptConclusion(t *testing.T) {
	attempt := fixtureAt(t, "new-run", "completed-failure-attempt.json")
	attempt = strings.Replace(attempt, `"conclusion": "failure"`, `"conclusion": "success"`, 1)
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"repo","owner":{"login":"owner"}}`},
		{output: fixtureText(t, "jobs.json")},
		{output: fixtureText(t, "workflow-run.json")},
		{output: fixtureText(t, "annotations.json")},
		{output: fixtureAt(t, "new-run", "completed-failure-runs.json")},
		{output: attempt},
		{output: fixtureAt(t, "new-run", "completed-failure-jobs.json")},
		{output: fixtureAt(t, "new-run", "completed-failure-annotations.json")},
	}}
	_, err := (&CLIClient{runner: runner}).ObserveManagedCI(context.Background(), ManagedCIRequest{
		Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "incident-source-head",
		FailedWorkflowRunID: "45270155099",
		FailedChecks: []CIFailedCheck{{
			Name: "build-macos", DetailsURL: "https://github.com/owner/repo/actions/runs/45270155099/job/138027527159",
		}},
	})
	if err == nil {
		t.Fatal("contradictory replacement attempt conclusion was accepted for job diagnosis")
	}
}

func TestCLIClient_ManagedCIRejectsContradictorySourceAttemptStatus(t *testing.T) {
	var workflowRuns map[string]any
	if err := json.Unmarshal([]byte(fixtureAt(t, "incident", "workflow-runs.json")), &workflowRuns); err != nil {
		t.Fatal(err)
	}
	run := workflowRuns["workflow_runs"].([]any)[0].(map[string]any)
	run["status"] = "in_progress"
	delete(run, "conclusion")
	contradictoryRuns, err := json.Marshal(workflowRuns)
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"repo","owner":{"login":"owner"}}`},
		{output: fixtureText(t, "jobs.json")},
		{output: fixtureText(t, "workflow-run.json")},
		{output: fixtureText(t, "annotations.json")},
		{output: string(contradictoryRuns)},
	}}
	_, err = (&CLIClient{runner: runner}).ObserveManagedCI(context.Background(), ManagedCIRequest{
		Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "incident-source-head",
		FailedWorkflowRunID: "45270155099",
		FailedChecks: []CIFailedCheck{{
			Name: "build-macos", DetailsURL: "https://github.com/owner/repo/actions/runs/45270155099/job/138027527159",
		}},
	})
	if err == nil {
		t.Fatal("workflow-run listing with a conflicting source-attempt status was accepted")
	}
}

func TestCLIClient_ManagedCIAcceptsSourceAttemptProgression(t *testing.T) {
	var sourceRun map[string]any
	if err := json.Unmarshal([]byte(fixtureAt(t, "incident", "workflow-run.json")), &sourceRun); err != nil {
		t.Fatal(err)
	}
	sourceRun["status"] = "in_progress"
	delete(sourceRun, "conclusion")
	progressingSource, err := json.Marshal(sourceRun)
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"repo","owner":{"login":"owner"}}`},
		{output: fixtureText(t, "jobs.json")},
		{output: string(progressingSource)},
		{output: fixtureText(t, "annotations.json")},
		{output: fixtureAt(t, "incident", "workflow-runs.json")},
	}}
	observation, err := (&CLIClient{runner: runner}).ObserveManagedCI(context.Background(), ManagedCIRequest{
		Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "incident-source-head",
		FailedWorkflowRunID: "45270155099",
		FailedChecks: []CIFailedCheck{{
			Name: "build-macos", DetailsURL: "https://github.com/owner/repo/actions/runs/45270155099/job/138027527159",
		}},
	})
	if err != nil {
		t.Fatalf("later completed workflow-run read contradicted an earlier in-progress source read: %v", err)
	}
	if observation.Source.Status != "in_progress" {
		t.Fatalf("source status = %q, want original exact-attempt observation", observation.Source.Status)
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
			name:        "base ref differs only by case",
			wantUnknown: true,
			mutate: func(candidate map[string]any) {
				candidate["pull_requests"].([]any)[0].(map[string]any)["base"].(map[string]any)["ref"] = "Main"
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

func TestCLIClient_ManagedCIConfirmsFailedJobInActiveWorkflow(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"repo","owner":{"login":"owner"}}`},
		{output: `{"total_count":1,"jobs":[{"id":11,"run_id":101,"run_attempt":1,"name":"unit-tests","status":"completed","conclusion":"failure","runner_id":42,"runner_name":"GitHub Actions 42","steps":[{"name":"go test ./...","status":"completed","conclusion":"failure","number":5}],"check_run_url":"https://api.github.com/repos/owner/repo/check-runs/201"}]}`},
		{output: `{"id":101,"run_number":5288,"run_attempt":1,"workflow_id":9,"event":"pull_request","status":"in_progress","head_sha":"head","pull_requests":[{"number":17,"base":{"ref":"main"},"head":{"sha":"head"}}]}`},
		{output: `[{"annotation_level":"failure","message":"TestExample failed.","path":"internal/example_test.go","start_line":42}]`},
	}}
	observation, err := (&CLIClient{runner: runner}).ObserveManagedCI(context.Background(), ManagedCIRequest{
		Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "head",
		FailedWorkflowRunID: "101",
		FailedChecks: []CIFailedCheck{{
			Name: "unit-tests", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/11",
		}},
	})
	if err != nil {
		t.Fatalf("active workflow with a completed failed job was inconclusive: %v", err)
	}
	if observation.Source.Status != "in_progress" || len(observation.Failures) != 1 ||
		observation.Failures[0].Classification != ManagedCIConfirmedCodeTestFailure {
		t.Fatalf("active-workflow observation = %+v, want a confirmed failed job in the current execution", observation)
	}
	if len(runner.calls) != 4 {
		t.Fatalf("active workflow unexpectedly queried replacement runs: calls=%d", len(runner.calls))
	}
}

func TestCLIClient_ManagedCIReportsIncompleteReplacementLookup(t *testing.T) {
	tests := []struct {
		name string
		runs string
	}{
		{name: "truncated positive count", runs: fixtureAt(t, "incomplete", "workflow-runs.json")},
		{name: "empty object", runs: `{}`},
		{name: "zero count without the source execution", runs: `{"total_count":0,"workflow_runs":[]}`},
		{name: "missing run array", runs: `{"total_count":1}`},
		{name: "missing count", runs: `{"workflow_runs":[]}`},
		{name: "source omitted from a complete empty listing", runs: `{"total_count":0,"workflow_runs":[]}`},
		{name: "duplicate source rows", runs: `{"total_count":2,"workflow_runs":[{"id":45270155099,"run_number":5288,"run_attempt":1,"workflow_id":273743359,"event":"pull_request","status":"completed","conclusion":"cancelled","head_sha":"incident-source-head","pull_requests":[{"number":17,"base":{"ref":"main"},"head":{"sha":"incident-source-head"}}]},{"id":45270155099,"run_number":5288,"run_attempt":1,"workflow_id":273743359,"event":"pull_request","status":"completed","conclusion":"cancelled","head_sha":"incident-source-head","pull_requests":[{"number":17,"base":{"ref":"main"},"head":{"sha":"incident-source-head"}}]}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{responses: []fakeResponse{
				{output: `{"name":"repo","owner":{"login":"owner"}}`},
				{output: fixtureText(t, "jobs.json")},
				{output: fixtureText(t, "workflow-run.json")},
				{output: fixtureText(t, "annotations.json")},
				{output: test.runs},
			}}
			observation, err := (&CLIClient{runner: runner}).ObserveManagedCI(context.Background(), ManagedCIRequest{
				Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "incident-source-head",
				FailedWorkflowRunID: "45270155099",
				FailedChecks: []CIFailedCheck{{
					Name: "build-macos", DetailsURL: "https://github.com/owner/repo/actions/runs/45270155099/job/138027527159",
				}},
			})
			if err == nil {
				t.Fatal("incomplete or source-less workflow-run list was treated as proof that no replacement exists")
			}
			if observation.Source.RunID != 45270155099 ||
				len(observation.Failures) != 1 ||
				observation.Failures[0].Classification != ManagedCIRunnerShortage {
				t.Fatalf("partial observation = %+v, want validated source evidence preserved with lookup error", observation)
			}
		})
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
	if name == "annotations.json" {
		// Most callers exercise positive shortage evidence; keep the raw
		// incident's generic annotation in its direct regression above.
		return fixtureAt(t, "incident-replay", "shortage-annotations.json")
	}
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
