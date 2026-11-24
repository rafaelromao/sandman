package batch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
)

type lifecycleCLIResultRunner struct {
	output string
}

type managedCILifecycleClient struct {
	*fakeGitHubClient
	observation github.ManagedCIObservation
	err         error
	requests    []github.ManagedCIRequest
}

func (c *managedCILifecycleClient) ObserveManagedCI(_ context.Context, request github.ManagedCIRequest) (github.ManagedCIObservation, error) {
	c.requests = append(c.requests, request)
	return c.observation, c.err
}

func (r lifecycleCLIResultRunner) Run(ctx context.Context, _ string, _ ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "printf", "%s", r.output)
}

func TestLifecycleObservation_VerifiedMergeWinsOverRetainedReview(t *testing.T) {
	workDir := t.TempDir()
	writeTimedOutReviewRequest(t, workDir)
	now := time.Unix(2_000, 0)
	session := &runSession{
		issueNumber: 42,
		deps: runDeps{
			githubClient: &fakeGitHubClient{
				issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
				prs: map[string]*github.PR{gateTestBranch: {
					Number: 17, State: "merged", Merged: true, Body: "Closes #42", HeadRefOid: "current-sha",
					StatusCheckRollup: "failure", MergeStateStatus: "CONFLICTING",
				}},
			},
			errorLog: io.Discard,
		},
		opts: runSessionOptions{
			now:         func() time.Time { return now },
			currentHead: func(string) (string, error) { return "current-sha", nil },
		},
	}
	var observer lifecycleObservation = newLifecycleObserver(session)

	verdict := observer.Observe(context.Background(), lifecycleObservationRequest{
		WorkDir: workDir, Branch: gateTestBranch, RunID: "run-test", HostPathsReady: true, AttemptStatus: "success",
	})
	if verdict.Action != lifecycleSuccess || len(verdict.Evidence) != 0 {
		t.Fatalf("verified merge verdict = (%v, %#v), want success with no failure evidence", verdict.Action, verdict.Evidence)
	}
}

func TestLifecycleObservation_AuthoritativeGreenDoesNotRequireReplacementLinkage(t *testing.T) {
	client := &managedCILifecycleClient{
		fakeGitHubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
			prs: map[string]*github.PR{gateTestBranch: {
				Number: 17, State: "open", Body: "Closes #42", HeadRefOid: "current-sha",
				BaseRefName: "main", StatusCheckRollup: "success", ReviewDecision: "APPROVED",
				MergeStateStatus: "CLEAN",
			}},
		},
	}
	session := &runSession{
		issueNumber: 42, deps: runDeps{githubClient: client, errorLog: io.Discard},
		renderCfg: prompt.RenderConfig{ReviewCommand: "/sandman review"},
		opts: runSessionOptions{
			currentHead: func(string) (string, error) { return "current-sha", nil },
		},
	}
	verdict := newLifecycleObserver(session).Observe(t.Context(), lifecycleObservationRequest{
		WorkDir: t.TempDir(), Branch: gateTestBranch, RunID: "run-authoritative-green",
		HostPathsReady: true, AttemptStatus: "success",
	})
	if verdict.Action == lifecycleAwait && verdict.Evidence["gate"] == string(lifecycleGatePending) {
		t.Fatalf("authoritative current-head green CI was blocked on replacement evidence: %+v", verdict)
	}
	if len(client.requests) != 0 {
		t.Fatalf("green PR unexpectedly queried failed-workflow replacement evidence: %+v", client.requests)
	}
}

func TestLifecycleObservation_WaitsForVerifiedReplacementWithoutPRJobLinks(t *testing.T) {
	now := time.Unix(2_000, 0).UTC()
	workDir := t.TempDir()
	failedChecks := []github.CIFailedCheck{
		{Name: "build-macos", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/11"},
		{Name: "CI / quality-gate", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/12"},
	}
	client := &managedCILifecycleClient{
		fakeGitHubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
			prs: map[string]*github.PR{gateTestBranch: {
				Number: 17, State: "open", HeadRefOid: "current-sha", BaseRefName: "main",
				StatusCheckRollup: "failure", CIExecutionID: "old-failed-checks", MergeStateStatus: "BLOCKED",
				CIFailedWorkflow: &github.CIFailedWorkflow{
					RunID: "101", FailedChecks: failedChecks, CheckNames: []string{"build-macos", "CI / quality-gate"},
				},
			}},
		},
		observation: github.ManagedCIObservation{
			Source: github.ManagedCIExecution{
				Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "current-sha",
				WorkflowID: 273743359, Event: "pull_request", RunID: 101, RunNumber: 5288, Attempt: 1,
				Status: "completed", Conclusion: "cancelled",
			},
			Failures: []github.ManagedCIFailureEvidence{
				{
					JobID: 138, RunID: 101, Attempt: 1, CheckName: "build-macos",
					DetailsURL: failedChecks[0].DetailsURL, Classification: github.ManagedCIRunnerShortage,
				},
				{
					JobID: 140, RunID: 101, Attempt: 1, CheckName: "CI / quality-gate",
					DetailsURL: failedChecks[1].DetailsURL, Classification: github.ManagedCIDependentFailure,
					CausedByChecks: []string{"build-macos"},
				},
			},
			Replacement: &github.ManagedCIExecution{
				Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "current-sha",
				WorkflowID: 273743359, Event: "pull_request", RunID: 102, RunNumber: 5289, Attempt: 1, Status: "queued",
			},
		},
	}
	log := &spyEventLog{}
	session := &runSession{
		issueNumber: 42,
		deps:        runDeps{githubClient: client, eventLog: log, errorLog: io.Discard},
		renderCfg:   prompt.RenderConfig{ReviewCommand: "/sandman review"},
		opts: runSessionOptions{
			now:         func() time.Time { return now },
			currentHead: func(string) (string, error) { return "current-sha", nil },
		},
	}
	verdict := newLifecycleObserver(session).Observe(t.Context(), lifecycleObservationRequest{
		WorkDir: workDir, Branch: gateTestBranch, RunID: "run-replacement-without-links", HostPathsReady: true,
		AttemptStatus: "success",
	})
	if verdict.Action != lifecycleAwait || verdict.Evidence["gate"] != string(lifecycleGatePending) {
		t.Fatalf("verified queued replacement verdict = %+v, want pending CI await", verdict)
	}
	if len(client.requests) != 1 || client.requests[0].Repository != "owner/repo" ||
		client.requests[0].PullRequest != 17 || client.requests[0].HeadSHA != "current-sha" ||
		client.requests[0].BaseRefName != "main" || client.requests[0].FailedWorkflowRunID != "101" {
		t.Fatalf("managed CI request = %+v, want full current PR/source identity", client.requests)
	}
	wait, ok := verdict.Evidence["ci_wait"].(map[string]any)
	if !ok || wait["execution_id"] != "actions:273743359:102:1" {
		t.Fatalf("CI wait = %#v, want the replacement identity before rollup job links exist", verdict.Evidence["ci_wait"])
	}
}

func TestLifecycleObservation_ReplacementStatusDoesNotManufactureGreenCI(t *testing.T) {
	tests := []struct {
		name       string
		status     string
		conclusion string
		wantAction lifecycleAction
	}{
		{name: "queued", status: "queued", wantAction: lifecycleAwait},
		{name: "running", status: "in_progress", wantAction: lifecycleAwait},
		{name: "passed but required rollup is stale failure", status: "completed", conclusion: "success", wantAction: lifecycleAwait},
		{name: "completed failure is actionable", status: "completed", conclusion: "failure", wantAction: lifecycleResume},
		{name: "unknown replacement status is inconclusive", status: "mystery", wantAction: lifecycleAwait},
		{name: "unknown completed conclusion is inconclusive", status: "completed", conclusion: "mystery", wantAction: lifecycleAwait},
		{name: "active replacement with conclusion is contradictory", status: "queued", conclusion: "success", wantAction: lifecycleAwait},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Unix(4_000, 0).UTC()
			client := &managedCILifecycleClient{
				fakeGitHubClient: &fakeGitHubClient{
					issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
					prs: map[string]*github.PR{gateTestBranch: {
						Number: 17, State: "open", HeadRefOid: "current-sha", BaseRefName: "main",
						StatusCheckRollup: "failure", CIExecutionID: "old-failed-checks", MergeStateStatus: "BLOCKED",
						CIFailedWorkflow: &github.CIFailedWorkflow{
							RunID: "101", FailedChecks: failedCIChecks("101", "build-macos"), CheckNames: []string{"build-macos"},
						},
					}},
				},
				observation: github.ManagedCIObservation{
					Source: github.ManagedCIExecution{
						Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "current-sha",
						WorkflowID: 273743359, Event: "pull_request", RunID: 101, RunNumber: 5288, Attempt: 1,
						Status: "completed", Conclusion: "cancelled",
					},
					Failures: []github.ManagedCIFailureEvidence{{
						JobID: 138, RunID: 101, Attempt: 1, CheckName: "build-macos",
						DetailsURL:     "https://github.com/owner/repo/actions/runs/101/job/11",
						Classification: github.ManagedCIRunnerShortage,
					}},
					Replacement: &github.ManagedCIExecution{
						Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "current-sha",
						WorkflowID: 273743359, Event: "pull_request", RunID: 102, RunNumber: 5289, Attempt: 1,
						Status: test.status, Conclusion: test.conclusion,
					},
				},
			}
			session := &runSession{
				issueNumber: 42,
				deps:        runDeps{githubClient: client, errorLog: io.Discard},
				renderCfg:   prompt.RenderConfig{ReviewCommand: "/sandman review"},
				opts: runSessionOptions{
					now:         func() time.Time { return now },
					currentHead: func(string) (string, error) { return "current-sha", nil },
				},
			}
			verdict := newLifecycleObserver(session).Observe(t.Context(), lifecycleObservationRequest{
				WorkDir: t.TempDir(), Branch: gateTestBranch, RunID: "run-replacement-status",
				HostPathsReady: true, AttemptStatus: "success",
			})
			if verdict.Action != test.wantAction {
				t.Fatalf("replacement status verdict = %+v, want action %v", verdict, test.wantAction)
			}
			if test.status == "completed" && test.conclusion == "success" {
				if verdict.Evidence["gate"] != string(lifecycleGatePending) {
					t.Fatalf("passed replacement turned stale PR failure green: %+v", verdict.Evidence)
				}
				if _, ok := verdict.Evidence["ci_wait"]; !ok {
					t.Fatalf("stale required-check failure lost its bounded replacement wait: %+v", verdict.Evidence)
				}
			}
			if test.conclusion == "failure" && managedCIReplacementStillResolves(verdict.Evidence, client.prs[gateTestBranch], "owner/repo") {
				t.Fatalf("completed failing replacement remained authorized as a resolving wait: %+v", verdict.Evidence)
			}
		})
	}
}

func TestLifecycleObservation_ConfirmedCodeFailureIsNotHiddenByRunnerShortage(t *testing.T) {
	now := time.Unix(5_000, 0).UTC()
	failedChecks := []github.CIFailedCheck{
		{Name: "build-macos", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/11"},
		{Name: "unit-tests", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/12"},
	}
	client := &managedCILifecycleClient{
		fakeGitHubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
			prs: map[string]*github.PR{gateTestBranch: {
				Number: 17, State: "open", HeadRefOid: "current-sha", BaseRefName: "main",
				StatusCheckRollup: "failure", CIExecutionID: "old-failed-checks",
				CIFailedWorkflow: &github.CIFailedWorkflow{
					RunID: "101", FailedChecks: failedChecks, CheckNames: []string{"build-macos", "unit-tests"},
				},
			}},
		},
		observation: github.ManagedCIObservation{
			Source: github.ManagedCIExecution{
				Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "current-sha",
				WorkflowID: 273743359, Event: "pull_request", RunID: 101, RunNumber: 5288, Attempt: 1,
				Status: "completed", Conclusion: "cancelled",
			},
			Failures: []github.ManagedCIFailureEvidence{
				{
					JobID: 138, RunID: 101, Attempt: 1, CheckName: "build-macos",
					DetailsURL: failedChecks[0].DetailsURL, Classification: github.ManagedCIRunnerShortage,
				},
				{
					JobID: 139, RunID: 101, Attempt: 1, CheckName: "unit-tests",
					DetailsURL: failedChecks[1].DetailsURL, Classification: github.ManagedCIConfirmedCodeTestFailure,
				},
			},
			Replacement: &github.ManagedCIExecution{
				Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "current-sha",
				WorkflowID: 273743359, Event: "pull_request", RunID: 102, RunNumber: 5289, Attempt: 1, Status: "queued",
			},
		},
	}
	session := &runSession{
		issueNumber: 42, deps: runDeps{githubClient: client, errorLog: io.Discard},
		renderCfg: prompt.RenderConfig{ReviewCommand: "/sandman review"},
		opts: runSessionOptions{
			now:         func() time.Time { return now },
			currentHead: func(string) (string, error) { return "current-sha", nil },
		},
	}
	verdict := newLifecycleObserver(session).Observe(t.Context(), lifecycleObservationRequest{
		WorkDir: t.TempDir(), Branch: gateTestBranch, RunID: "run-mixed-ci-failure",
		HostPathsReady: true, AttemptStatus: "success",
	})
	if verdict.Action != lifecycleResume || verdict.Evidence["gate"] != "ci-failure" {
		t.Fatalf("independent code/test failure was hidden by runner shortage: %+v", verdict)
	}
	if wait, ok := verdict.Evidence["ci_wait"].(map[string]any); ok {
		if _, replacement := wait["actions_execution"]; replacement {
			t.Fatalf("mixed failure created a replacement wait: %#v", wait)
		}
	}
}

func TestLifecycleObservation_UnknownManagedCILookupWaitsForGitHubRecovery(t *testing.T) {
	tests := []struct {
		name         string
		failedChecks []github.CIFailedCheck
		observation  github.ManagedCIObservation
		err          error
		wantAction   lifecycleAction
		wantGate     string
	}{
		{
			name:       "optional lookup failed",
			err:        errors.New("Actions read unavailable"),
			wantAction: lifecycleAwait,
			wantGate:   gateGitHubUnavailable,
		},
		{
			name:        "incomplete response",
			observation: github.ManagedCIObservation{},
			wantAction:  lifecycleAwait,
			wantGate:    gateGitHubUnavailable,
		},
		{
			name: "unknown failure classification",
			observation: github.ManagedCIObservation{
				Source: github.ManagedCIExecution{
					Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "current-sha",
					WorkflowID: 273743359, Event: "pull_request", RunID: 101, RunNumber: 5288, Attempt: 1,
					Status: "completed", Conclusion: "cancelled",
				},
				Failures: []github.ManagedCIFailureEvidence{{
					JobID: 138, RunID: 101, Attempt: 1, CheckName: "build-macos",
					DetailsURL:     "https://github.com/owner/repo/actions/runs/101/job/11",
					Classification: "unrecognized",
				}},
			},
			wantAction: lifecycleAwait,
			wantGate:   gateGitHubUnavailable,
		},
		{
			name:         "confirmed independent source failure survives unknown classification",
			failedChecks: failedCIChecks("101", "build-macos", "unit-tests"),
			observation: github.ManagedCIObservation{
				Source: github.ManagedCIExecution{
					Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "current-sha",
					WorkflowID: 273743359, Event: "pull_request", RunID: 101, RunNumber: 5288, Attempt: 1,
					Status: "completed", Conclusion: "cancelled",
				},
				Failures: []github.ManagedCIFailureEvidence{
					{
						JobID: 138, RunID: 101, Attempt: 1, CheckName: "build-macos",
						DetailsURL:     "https://github.com/owner/repo/actions/runs/101/job/11",
						Classification: "unrecognized",
					},
					{
						JobID: 139, RunID: 101, Attempt: 1, CheckName: "unit-tests",
						DetailsURL:     "https://github.com/owner/repo/actions/runs/101/job/12",
						Classification: github.ManagedCIConfirmedCodeTestFailure,
					},
				},
			},
			wantAction: lifecycleResume,
			wantGate:   "ci-failure",
		},
		{
			name:         "confirmed independent source failure survives contradictory replacement",
			failedChecks: failedCIChecks("101", "build-macos", "unit-tests"),
			observation: github.ManagedCIObservation{
				Source: github.ManagedCIExecution{
					Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "current-sha",
					WorkflowID: 273743359, Event: "pull_request", RunID: 101, RunNumber: 5288, Attempt: 1,
					Status: "completed", Conclusion: "cancelled",
				},
				Failures: []github.ManagedCIFailureEvidence{
					{
						JobID: 138, RunID: 101, Attempt: 1, CheckName: "build-macos",
						DetailsURL:     "https://github.com/owner/repo/actions/runs/101/job/11",
						Classification: github.ManagedCIRunnerShortage,
					},
					{
						JobID: 139, RunID: 101, Attempt: 1, CheckName: "unit-tests",
						DetailsURL:     "https://github.com/owner/repo/actions/runs/101/job/12",
						Classification: github.ManagedCIConfirmedCodeTestFailure,
					},
				},
				Replacement: &github.ManagedCIExecution{
					Repository: "owner/repo", PullRequest: 17, BaseRefName: "release", HeadSHA: "current-sha",
					WorkflowID: 273743359, Event: "pull_request", RunID: 102, RunNumber: 5289, Attempt: 1, Status: "queued",
				},
			},
			wantAction: lifecycleResume,
			wantGate:   "ci-failure",
		},
		{
			name: "contradictory replacement identity",
			observation: github.ManagedCIObservation{
				Source: github.ManagedCIExecution{
					Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "current-sha",
					WorkflowID: 273743359, Event: "pull_request", RunID: 101, RunNumber: 5288, Attempt: 1,
					Status: "completed", Conclusion: "cancelled",
				},
				Failures: []github.ManagedCIFailureEvidence{{
					JobID: 138, RunID: 101, Attempt: 1, CheckName: "build-macos",
					DetailsURL:     "https://github.com/owner/repo/actions/runs/101/job/11",
					Classification: github.ManagedCIRunnerShortage,
				}},
				Replacement: &github.ManagedCIExecution{
					Repository: "owner/repo", PullRequest: 17, BaseRefName: "release", HeadSHA: "current-sha",
					WorkflowID: 273743359, Event: "pull_request", RunID: 102, RunNumber: 5289, Attempt: 1, Status: "queued",
				},
			},
			wantAction: lifecycleAwait,
			wantGate:   gateGitHubUnavailable,
		},
		{
			name: "shortage has no verified replacement",
			observation: github.ManagedCIObservation{
				Source: github.ManagedCIExecution{
					Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "current-sha",
					WorkflowID: 273743359, Event: "pull_request", RunID: 101, RunNumber: 5288, Attempt: 1,
					Status: "completed", Conclusion: "cancelled",
				},
				Failures: []github.ManagedCIFailureEvidence{{
					JobID: 138, RunID: 101, Attempt: 1, CheckName: "build-macos",
					DetailsURL:     "https://github.com/owner/repo/actions/runs/101/job/11",
					Classification: github.ManagedCIRunnerShortage,
				}},
			},
			wantAction: lifecycleResume,
			wantGate:   "ci-failure",
		},
		{
			name: "unexplained cancellation cannot start replacement wait",
			observation: github.ManagedCIObservation{
				Source: github.ManagedCIExecution{
					Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "current-sha",
					WorkflowID: 273743359, Event: "pull_request", RunID: 101, RunNumber: 5288, Attempt: 1,
					Status: "completed", Conclusion: "cancelled",
				},
				Failures: []github.ManagedCIFailureEvidence{{
					JobID: 138, RunID: 101, Attempt: 1, CheckName: "build-macos",
					DetailsURL:     "https://github.com/owner/repo/actions/runs/101/job/11",
					Classification: github.ManagedCIUnexplainedFailure,
				}},
				Replacement: &github.ManagedCIExecution{
					Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "current-sha",
					WorkflowID: 273743359, Event: "pull_request", RunID: 102, RunNumber: 5289, Attempt: 1, Status: "queued",
				},
			},
			wantAction: lifecycleResume,
			wantGate:   "ci-failure",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Unix(5_500, 0).UTC()
			failedChecks := test.failedChecks
			if failedChecks == nil {
				failedChecks = failedCIChecks("101", "build-macos")
			}
			client := &managedCILifecycleClient{
				fakeGitHubClient: &fakeGitHubClient{
					issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
					prs: map[string]*github.PR{gateTestBranch: {
						Number: 17, State: "open", HeadRefOid: "current-sha", BaseRefName: "main",
						StatusCheckRollup: "failure", CIExecutionID: "old-failed-checks",
						CIFailedWorkflow: &github.CIFailedWorkflow{
							RunID: "101", FailedChecks: failedChecks, CheckNames: []string{"build-macos"},
						},
					}},
				},
				observation: test.observation,
				err:         test.err,
			}
			session := &runSession{
				issueNumber: 42, deps: runDeps{githubClient: client, errorLog: io.Discard},
				renderCfg: prompt.RenderConfig{ReviewCommand: "/sandman review"},
				opts: runSessionOptions{
					now:         func() time.Time { return now },
					currentHead: func(string) (string, error) { return "current-sha", nil },
				},
			}
			verdict := newLifecycleObserver(session).Observe(t.Context(), lifecycleObservationRequest{
				WorkDir: t.TempDir(), Branch: gateTestBranch, RunID: "run-unverified-replacement",
				HostPathsReady: true, AttemptStatus: "success",
			})
			if verdict.Action != test.wantAction || verdict.Evidence["gate"] != test.wantGate {
				t.Fatalf("managed CI lookup verdict = %+v, want action %v and gate %q", verdict, test.wantAction, test.wantGate)
			}
			if test.wantGate == gateGitHubUnavailable {
				if _, ok := verdict.Evidence[githubRecoveryKey].(map[string]any); !ok {
					t.Fatalf("inconclusive managed CI observation did not enter bounded GitHub recovery: %#v", verdict.Evidence)
				}
				if _, ok := verdict.Evidence["ci_wait"]; ok {
					t.Fatalf("inconclusive evidence established a CI execution wait: %#v", verdict.Evidence)
				}
			} else if wait, ok := verdict.Evidence["ci_wait"].(map[string]any); ok {
				if _, replacement := wait["actions_execution"]; replacement {
					t.Fatalf("unverified evidence established a replacement wait: %#v", wait)
				}
			}
		})
	}
}

func TestLifecycleObservation_ReplacementSuccessRetainsOriginalCITimeout(t *testing.T) {
	now := time.Unix(6_000, 0).UTC()
	workDir := t.TempDir()
	client := &managedCILifecycleClient{
		fakeGitHubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
			prs: map[string]*github.PR{gateTestBranch: {
				Number: 17, State: "open", HeadRefOid: "current-sha", BaseRefName: "main",
				StatusCheckRollup: "failure", CIExecutionID: "old-failed-checks",
				CIFailedWorkflow: &github.CIFailedWorkflow{
					RunID: "101", FailedChecks: failedCIChecks("101", "build-macos"), CheckNames: []string{"build-macos"},
				},
			}},
		},
		observation: github.ManagedCIObservation{
			Source: github.ManagedCIExecution{
				Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "current-sha",
				WorkflowID: 273743359, Event: "pull_request", RunID: 101, RunNumber: 5288, Attempt: 1,
				Status: "completed", Conclusion: "cancelled",
			},
			Failures: []github.ManagedCIFailureEvidence{{
				JobID: 138, RunID: 101, Attempt: 1, CheckName: "build-macos",
				DetailsURL:     "https://github.com/owner/repo/actions/runs/101/job/11",
				Classification: github.ManagedCIRunnerShortage,
			}},
			Replacement: &github.ManagedCIExecution{
				Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "current-sha",
				WorkflowID: 273743359, Event: "pull_request", RunID: 102, RunNumber: 5289, Attempt: 1,
				Status: "completed", Conclusion: "success",
			},
		},
	}
	session := &runSession{
		issueNumber: 42, deps: runDeps{githubClient: client, errorLog: io.Discard},
		renderCfg: prompt.RenderConfig{ReviewCommand: "/sandman review"},
		opts: runSessionOptions{
			now:         func() time.Time { return now },
			currentHead: func(string) (string, error) { return "current-sha", nil },
		},
	}
	observer := newLifecycleObserver(session)
	request := lifecycleObservationRequest{
		WorkDir: workDir, Branch: gateTestBranch, RunID: "run-replacement-deadline",
		HostPathsReady: true, AttemptStatus: "success",
	}
	initial := observer.Observe(t.Context(), request)
	if initial.Action != lifecycleAwait || !managedCIReplacementStillResolves(initial.Evidence, client.prs[gateTestBranch], "owner/repo") {
		t.Fatalf("passed replacement with stale failing PR rollup = %+v, want bounded await", initial)
	}
	wait := initial.Evidence["ci_wait"].(map[string]any)
	deadline := wait["deadline_unix_seconds"].(int64)
	client.prs[gateTestBranch].CIExecutionID = "replacement-check-links-now-visible"
	now = now.Add(5 * time.Minute)
	linked := observer.Observe(t.Context(), request)
	if linked.Action != lifecycleAwait {
		t.Fatalf("stale failing rollup after replacement check links appeared = %+v, want original wait", linked)
	}
	linkedWait := linked.Evidence["ci_wait"].(map[string]any)
	if linkedWait["deadline_unix_seconds"] != deadline ||
		!managedCIReplacementStillResolves(linked.Evidence, client.prs[gateTestBranch], "owner/repo") {
		t.Fatalf("appearance of replacement links renewed or regressed its wait: %#v", linkedWait)
	}
	now = time.Unix(deadline, 0).UTC()
	expired := observer.Observe(t.Context(), request)
	if expired.Action != lifecycleResume ||
		stringEvidence(expired.Evidence, "gate") != gateCIWaitTimeout ||
		stringEvidence(expired.Evidence, "reason") != "CI_WAIT_TIMEOUT" {
		t.Fatalf("replacement wait expiry = %+v, want the existing CI_WAIT_TIMEOUT continuation", expired)
	}
	registration, err := readCIWaitRegistration(filepath.Join(workDir, ".sandman", "state", "17.ci_wait.json"))
	if err != nil || registration.DeadlineUnixSeconds != deadline {
		t.Fatalf("replacement success renewed its bounded deadline: registration=%+v err=%v", registration, err)
	}
}

func TestLifecycleObservation_VerdictTable(t *testing.T) {
	now := time.Unix(2_000, 0).UTC()
	tests := []struct {
		name              string
		pr                *github.PR
		prepare           func(*testing.T, string)
		wantAction        lifecycleAction
		wantKind          lifecycleVerdictKind
		wantGate          string
		wantWait          string
		wantCIExecutionID string
	}{
		{
			name: "await current-head CI with execution identity and deadline",
			pr: &github.PR{Number: 17, State: "open", HeadRefOid: "current-sha", CIExecutionID: "checks-2",
				StatusCheckRollup: "pending", MergeStateStatus: "BLOCKED"},
			wantAction: lifecycleAwait,
			wantWait:   "ci_wait",
		},
		{
			name: "await confirmed review with request identity and deadline",
			pr: &github.PR{Number: 17, State: "open", HeadRefOid: "current-sha", StatusCheckRollup: "success",
				ReviewDecision: "REVIEW_REQUIRED", MergeStateStatus: "BLOCKED"},
			prepare: func(t *testing.T, workDir string) {
				writeTimedOutReviewRequest(t, workDir)
				writeCanonicalRegistrationForTest(t, workDir)
			},
			wantAction: lifecycleAwait,
			wantWait:   "review_request",
		},
		{
			name: "resume current-head CI remediation",
			pr: &github.PR{Number: 17, State: "open", HeadRefOid: "current-sha", CIExecutionID: "failed-checks-2", StatusCheckRollup: "failure",
				MergeStateStatus: "CLEAN"},
			wantAction:        lifecycleResume,
			wantGate:          "ci-failure",
			wantCIExecutionID: "failed-checks-2",
		},
		{
			name: "resume approved clean PR for merge work",
			pr: &github.PR{Number: 17, State: "open", HeadRefOid: "current-sha", StatusCheckRollup: "success",
				ReviewDecision: "APPROVED", MergeStateStatus: "CLEAN"},
			wantAction: lifecycleResume,
			wantGate:   gateReadyToMerge,
		},
		{
			name: "verified merge completion",
			pr: &github.PR{Number: 17, State: "merged", Merged: true, Body: "Closes #42", HeadRefOid: "current-sha",
				StatusCheckRollup: "failure", MergeStateStatus: "CONFLICTING"},
			wantAction: lifecycleSuccess,
			wantKind:   lifecycleVerdictCompletion,
		},
		{
			name:       "closed without merge",
			pr:         &github.PR{Number: 17, State: "closed", HeadRefOid: "current-sha"},
			wantAction: lifecycleFailure,
			wantKind:   lifecycleVerdictClosedPR,
		},
		{
			name: "resume merge conflict remediation",
			pr: &github.PR{Number: 17, State: "open", HeadRefOid: "current-sha", StatusCheckRollup: "success",
				MergeStateStatus: "CONFLICTING"},
			wantAction: lifecycleResume,
			wantGate:   "merge-conflict",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workDir := t.TempDir()
			if tt.prepare != nil {
				tt.prepare(t, workDir)
			}
			session := &runSession{
				issueNumber: 42,
				deps: runDeps{
					githubClient: &fakeGitHubClient{
						issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
						prs:    map[string]*github.PR{gateTestBranch: tt.pr},
					},
					errorLog: io.Discard,
				},
				opts: runSessionOptions{
					now:         func() time.Time { return now },
					currentHead: func(string) (string, error) { return "current-sha", nil },
				},
			}
			observer := lifecycleObservation(newLifecycleObserver(session))
			verdict := observer.Observe(context.Background(), lifecycleObservationRequest{
				WorkDir: workDir, Branch: gateTestBranch, RunID: "run-test", HostPathsReady: true, AttemptStatus: "success",
			})
			if verdict.Action != tt.wantAction || verdict.Kind != tt.wantKind {
				t.Fatalf("verdict = (%v, kind %v, %#v), want action %v kind %v", verdict.Action, verdict.Kind, verdict.Evidence, tt.wantAction, tt.wantKind)
			}
			if tt.wantGate != "" && stringEvidence(verdict.Evidence, "gate") != tt.wantGate {
				t.Fatalf("verdict gate = %q, want %q; evidence=%#v", stringEvidence(verdict.Evidence, "gate"), tt.wantGate, verdict.Evidence)
			}
			if tt.wantCIExecutionID != "" && verdict.ciExecutionID != tt.wantCIExecutionID {
				t.Fatalf("CI execution identity = %q, want %q", verdict.ciExecutionID, tt.wantCIExecutionID)
			}
			if tt.wantWait != "" {
				wait, ok := verdict.Evidence[tt.wantWait].(map[string]any)
				if verdict.Action != lifecycleAwait || !ok || wait["deadline_unix_seconds"] == nil {
					t.Fatalf("await evidence %q = %#v, want an identity-bound deadline", tt.wantWait, verdict.Evidence[tt.wantWait])
				}
				if tt.wantWait == "ci_wait" && wait["execution_id"] != "checks-2" {
					t.Fatalf("CI execution identity = %#v, want checks-2", wait["execution_id"])
				}
				if tt.wantWait == "review_request" && (wait["trigger_id"] == nil || wait["head_sha"] != "current-sha") {
					t.Fatalf("review request identity = %#v, want current trigger and head", wait)
				}
			}
		})
	}
}

func TestLifecycleObservation_FailedCIWithActiveSiblingKeepsRuntimePrerequisiteWait(t *testing.T) {
	now := time.Unix(2_000, 0).UTC()
	workDir := t.TempDir()
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
		prs: map[string]*github.PR{gateTestBranch: {
			Number: 17, State: "open", HeadRefOid: "current-sha", CIExecutionID: "checks-with-pending-sibling",
			StatusCheckRollup: "failure", CIHasActiveChecks: true, CIActiveFailedWorkflowRunID: "101",
			CIActiveFailedWorkflowRunFailedCheckNames: []string{"opencode-canary"},
			CIActiveFailedWorkflowRunFailedChecks:     failedCIChecks("101", "opencode-canary"), MergeStateStatus: "BLOCKED",
			CIActiveFailedWorkflowRunCheckNames: []string{"opencode-canary", "build-macos"},
		}},
	}
	log := &spyEventLog{}
	session := &runSession{
		issueNumber: 42,
		deps:        runDeps{githubClient: client, eventLog: log, errorLog: io.Discard},
		opts: runSessionOptions{
			now:         func() time.Time { return now },
			currentHead: func(string) (string, error) { return "current-sha", nil },
		},
	}
	observer := newLifecycleObserver(session)
	request := lifecycleObservationRequest{
		WorkDir: workDir, Branch: gateTestBranch, RunID: "run-cannot-rerun-yet", HostPathsReady: true,
		AttemptStatus: "success",
	}

	entry := observer.Observe(t.Context(), request)
	if entry.Action != lifecycleResume || entry.Evidence["gate"] != "ci-failure" {
		t.Fatalf("initial entry must launch the implementation agent to diagnose/repair despite active siblings: %+v", entry)
	}

	request.AfterAgentAttempt = true
	untouchedFailure := observer.Observe(t.Context(), request)
	if untouchedFailure.Action != lifecycleResume || untouchedFailure.Evidence["reason"] != "CI_FAILURE" || !untouchedFailure.ciRepairRequired {
		t.Fatalf("an agent exit without a helper result must retain CI remediation ownership: %+v", untouchedFailure)
	}
	request.CIRerunAction = captureCIRerunActionFromOpenCodeToolEvent(t,
		`sh "$HOME/.agents/skills/sandman/implement/ci-rerun-v1.sh" --repository owner/repo --pull-request 17 --head-sha current-sha --tested-sha tested-merge-sha --run-id 101 --job-id 987654321 --diagnosis transient-provider-timeout`,
		`{"protocol":"ci-rerun/v1","state":"waiting","reason":"workflow-still-running","repository":"owner/repo","pull_request":17,"source_head_sha":"current-sha","tested_sha":"tested-merge-sha","diagnosis":"transient-provider-timeout","run_id":"101","job_id":"987654321","job_name":"opencode-canary","observed_attempt":1}`)
	if request.CIRerunAction == nil {
		t.Fatal("structured helper tool result was not captured")
	}
	blockedRerun := observer.Observe(t.Context(), request)
	if blockedRerun.Action != lifecycleAwait || blockedRerun.Evidence["gate"] != string(lifecycleGatePending) || blockedRerun.Evidence["reason"] != "CI_FAILURE" {
		t.Fatalf("verified helper result should authorize observation of its active workflow prerequisite: %+v", blockedRerun)
	}
	ciWait, ok := blockedRerun.Evidence["ci_wait"].(map[string]any)
	if !ok || ciWait["execution_id"] != "checks-with-pending-sibling" {
		t.Fatalf("prerequisite await lost identified CI execution: %#v", blockedRerun.Evidence)
	}
	prerequisite, ok := blockedRerun.Evidence["ci_rerun_prerequisite"].(map[string]any)
	if !ok || prerequisite["workflow_run_id"] != "101" || prerequisite["head_sha"] != "current-sha" || prerequisite["pull_request"] != 17 {
		t.Fatalf("prerequisite await lost its typed workflow identity: %#v", blockedRerun.Evidence)
	}
	if action, ok := blockedRerun.Evidence["ci_rerun_action"].(map[string]any); !ok || action["job_id"] != "987654321" {
		t.Fatalf("prerequisite await lost the diagnosed helper result: %#v", blockedRerun.Evidence["ci_rerun_action"])
	}
	initialDeadline, ok := ciWait["deadline_unix_seconds"].(int64)
	if !ok {
		t.Fatalf("CI wait deadline has unexpected type %T", ciWait["deadline_unix_seconds"])
	}

	client.prs[gateTestBranch].CIHasActiveChecks = false
	client.prs[gateTestBranch].CIActiveFailedWorkflowRunID = ""
	now = now.Add(2 * time.Minute)
	completedPrerequisite := observer.Observe(t.Context(), request)
	if completedPrerequisite.Action != lifecycleResume || completedPrerequisite.Evidence["gate"] != "ci-failure" {
		t.Fatalf("completed workflow prerequisite must return failed CI to implementation ownership: %+v", completedPrerequisite)
	}
	retained, err := readCIWaitRegistration(filepath.Join(workDir, ".sandman", "state", "17.ci_wait.json"))
	if err != nil || retained.DeadlineUnixSeconds != initialDeadline {
		t.Fatalf("same CI execution deadline changed across the prerequisite: registration=%+v error=%v", retained, err)
	}

	client.prs[gateTestBranch].CIHasActiveChecks = true
	client.prs[gateTestBranch].CIActiveFailedWorkflowRunID = "101"
	client.prs[gateTestBranch].CIActiveFailedWorkflowRunFailedCheckNames = []string{"opencode-canary", "quality-gate"}
	client.prs[gateTestBranch].CIActiveFailedWorkflowRunFailedChecks = failedCIChecks("101", "opencode-canary", "quality-gate")
	client.prs[gateTestBranch].CIActiveFailedWorkflowRunCheckNames = []string{"opencode-canary", "quality-gate", "build-macos"}
	client.prs[gateTestBranch].CIExecutionID = "checks-rerun-attempt-2"
	request.CIRerunAction = captureCIRerunActionFromOpenCodeToolEvent(t,
		`sh "$HOME/.agents/skills/sandman/implement/ci-rerun-v1.sh" --repository owner/repo --pull-request 17 --head-sha current-sha --tested-sha tested-merge-sha --run-id 101 --job-id 987654321 --diagnosis transient-provider-timeout`,
		`{"protocol":"ci-rerun/v1","state":"confirmed","reason":"selected-job-and-quality-gate-rerun-observed","repository":"owner/repo","pull_request":17,"source_head_sha":"current-sha","tested_sha":"tested-merge-sha","diagnosis":"transient-provider-timeout","run_id":"101","job_id":"987654321","job_name":"opencode-canary","observed_attempt":2}`)
	if request.CIRerunAction == nil {
		t.Fatal("confirmed helper rerun result was not captured")
	}
	confirmedRerun := observer.Observe(t.Context(), request)
	if confirmedRerun.Action != lifecycleAwait || confirmedRerun.Evidence["gate"] != string(lifecycleGatePending) {
		t.Fatalf("confirmed new attempt should authorize waiting while the failure-first rollup remains visible: %+v", confirmedRerun)
	}
	newCI, ok := confirmedRerun.Evidence["ci_wait"].(map[string]any)
	if !ok || newCI["execution_id"] != "checks-rerun-attempt-2" || newCI["deadline_unix_seconds"] != now.Add(ciWaitTimeout).Unix() {
		t.Fatalf("confirmed rerun did not start a deadline for its verified new execution: %#v", confirmedRerun.Evidence)
	}
}

func TestLifecycleObservation_RerunReceiptDoesNotMaskUnidentifiedErrorContext(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
		prs: map[string]*github.PR{gateTestBranch: {
			Number: 17, State: "open", HeadRefOid: "current-sha", CIExecutionID: "checks-with-error-context",
			StatusCheckRollup: "failure", CIHasActiveChecks: true, MergeStateStatus: "BLOCKED",
		}},
	}
	log := &spyEventLog{}
	session := &runSession{
		issueNumber: 42,
		deps:        runDeps{githubClient: client, eventLog: log, errorLog: io.Discard},
		opts: runSessionOptions{
			currentHead: func(string) (string, error) { return "current-sha", nil },
		},
	}
	request := lifecycleObservationRequest{
		WorkDir: t.TempDir(), Branch: gateTestBranch, RunID: "run-error-context", HostPathsReady: true,
		AttemptStatus: "success", AfterAgentAttempt: true,
		CIRerunAction: captureCIRerunActionFromOpenCodeToolEvent(t,
			`sh "$HOME/.agents/skills/sandman/implement/ci-rerun-v1.sh" --repository owner/repo --pull-request 17 --head-sha current-sha --tested-sha tested-merge-sha --run-id 101 --job-id 987654321 --diagnosis transient-provider-timeout`,
			`{"protocol":"ci-rerun/v1","state":"waiting","reason":"workflow-still-running","repository":"owner/repo","pull_request":17,"source_head_sha":"current-sha","tested_sha":"tested-merge-sha","diagnosis":"transient-provider-timeout","run_id":"101","job_id":"987654321","job_name":"opencode-canary","observed_attempt":1}`),
	}
	verdict := newLifecycleObserver(session).Observe(t.Context(), request)
	if verdict.Action != lifecycleResume || verdict.Evidence["reason"] != "CI_FAILURE" || !verdict.ciRepairRequired {
		t.Fatalf("receipt for the active workflow masked an unidentified error context: %+v", verdict)
	}
}

func TestLifecycleObservation_FailedCIWithUnrelatedActiveWorkflowResumesImplementation(t *testing.T) {
	workDir := t.TempDir()
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
		prs: map[string]*github.PR{gateTestBranch: {
			Number: 17, State: "open", HeadRefOid: "current-sha", CIExecutionID: "checks-with-unrelated-active-workflow",
			StatusCheckRollup: "failure", CIHasActiveChecks: true, MergeStateStatus: "BLOCKED",
		}},
	}
	session := &runSession{
		issueNumber: 42,
		deps:        runDeps{githubClient: client, errorLog: io.Discard},
		opts: runSessionOptions{
			currentHead: func(string) (string, error) { return "current-sha", nil },
		},
	}
	request := lifecycleObservationRequest{
		WorkDir: workDir, Branch: gateTestBranch, RunID: "run-unrelated-active", HostPathsReady: true,
		AttemptStatus: "success", AfterAgentAttempt: true,
		CIRerunAction: captureCIRerunActionFromOpenCodeToolEvent(t,
			`sh "$HOME/.agents/skills/sandman/implement/ci-rerun-v1.sh" --repository owner/repo --pull-request 17 --head-sha current-sha --tested-sha tested-merge-sha --run-id 101 --job-id 987654321 --diagnosis transient-provider-timeout`,
			`{"protocol":"ci-rerun/v1","state":"waiting","reason":"workflow-still-running","repository":"owner/repo","pull_request":17,"source_head_sha":"current-sha","tested_sha":"tested-merge-sha","diagnosis":"transient-provider-timeout","run_id":"101","job_id":"987654321","job_name":"opencode-canary","observed_attempt":1}`),
	}
	verdict := newLifecycleObserver(session).Observe(t.Context(), request)
	if verdict.Action != lifecycleResume || verdict.Evidence["reason"] != "CI_FAILURE" || !verdict.ciRepairRequired {
		t.Fatalf("valid receipt for one workflow deferred a separate failed workflow: %+v", verdict)
	}
}

func TestLifecycleObservation_RerunReceiptDoesNotMaskSameWorkflowFailure(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
		prs: map[string]*github.PR{gateTestBranch: {
			Number: 17, State: "open", HeadRefOid: "current-sha", CIExecutionID: "checks-with-multiple-failures",
			StatusCheckRollup: "failure", CIHasActiveChecks: true, CIActiveFailedWorkflowRunID: "101",
			CIActiveFailedWorkflowRunFailedCheckNames: []string{"opencode-canary", "source-tests"},
			CIActiveFailedWorkflowRunFailedChecks:     failedCIChecks("101", "opencode-canary", "source-tests"),
			CIActiveFailedWorkflowRunCheckNames:       []string{"opencode-canary", "source-tests", "build-macos"},
			MergeStateStatus:                          "BLOCKED",
		}},
	}
	session := &runSession{
		issueNumber: 42,
		deps:        runDeps{githubClient: client, errorLog: io.Discard},
		opts:        runSessionOptions{currentHead: func(string) (string, error) { return "current-sha", nil }},
	}
	request := lifecycleObservationRequest{
		WorkDir: t.TempDir(), Branch: gateTestBranch, RunID: "run-same-workflow-failure", HostPathsReady: true,
		AttemptStatus: "success", AfterAgentAttempt: true,
		CIRerunAction: captureCIRerunActionFromOpenCodeToolEvent(t,
			`sh "$HOME/.agents/skills/sandman/implement/ci-rerun-v1.sh" --repository owner/repo --pull-request 17 --head-sha current-sha --tested-sha tested-merge-sha --run-id 101 --job-id 987654321 --diagnosis transient-provider-timeout`,
			`{"protocol":"ci-rerun/v1","state":"waiting","reason":"workflow-still-running","repository":"owner/repo","pull_request":17,"source_head_sha":"current-sha","tested_sha":"tested-merge-sha","diagnosis":"transient-provider-timeout","run_id":"101","job_id":"987654321","job_name":"opencode-canary","observed_attempt":1}`),
	}
	verdict := newLifecycleObserver(session).Observe(t.Context(), request)
	if verdict.Action != lifecycleResume || verdict.Evidence["reason"] != "CI_FAILURE" || !verdict.ciRepairRequired {
		t.Fatalf("receipt for opencode-canary deferred same-workflow source-tests failure: %+v", verdict)
	}
}

func TestAwaitingRowLifecycle_ReobservesOnlyTheRetainedWorkflowPrerequisite(t *testing.T) {
	root := t.TempDir()
	now := time.Unix(2_000, 0).UTC()
	ciWait := map[string]any{
		"protocol": ciWaitProtocol, "pull_request": 17, "head_sha": "current-sha", "execution_id": "checks-101",
		"started_unix_seconds": now.Unix(), "deadline_unix_seconds": now.Add(ciWaitTimeout).Unix(),
		"effective_timeout_seconds": int64(ciWaitTimeout / time.Second),
	}
	prerequisite := map[string]any{
		"protocol": "ci-rerun-prerequisite/v1", "repository": "owner/repo", "pull_request": 17, "head_sha": "current-sha", "workflow_run_id": "101",
		"failed_check_names":   []string{"opencode-canary"},
		"failed_checks":        failedCIChecks("101", "opencode-canary"),
		"workflow_check_names": []string{"opencode-canary", "build-macos"},
	}
	action := map[string]any{
		"protocol": "ci-rerun/v1", "state": "waiting", "reason": "workflow-still-running", "repository": "owner/repo",
		"pull_request": 17, "source_head_sha": "current-sha", "tested_sha": "tested-merge-sha",
		"diagnosis": "transient-provider-timeout", "run_id": "101", "job_id": "987654321", "job_name": "opencode-canary", "observed_attempt": 1,
	}
	log := &spyEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-rerun-prerequisite", Issue: 42, Timestamp: now.Add(-time.Minute), Payload: map[string]any{"branch": gateTestBranch}},
		{Type: "run.await", RunID: "run-rerun-prerequisite", Issue: 42, Timestamp: now, Payload: map[string]any{
			"await": true, "await_reason": "pending", "branch": gateTestBranch,
			"ci_wait": ciWait, "ci_rerun_prerequisite": prerequisite, "ci_rerun_action": action,
		}},
	}}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
		prs: map[string]*github.PR{gateTestBranch: {
			Number: 17, State: "open", HeadRefOid: "current-sha", CIExecutionID: "checks-101",
			StatusCheckRollup: "failure", CIHasActiveChecks: true, CIActiveFailedWorkflowRunID: "101",
			CIActiveFailedWorkflowRunFailedCheckNames: []string{"opencode-canary"},
			CIActiveFailedWorkflowRunFailedChecks:     failedCIChecks("101", "opencode-canary"), MergeStateStatus: "BLOCKED",
			CIActiveFailedWorkflowRunCheckNames: []string{"opencode-canary", "build-macos"},
		}},
	}
	e := &runExecutor{deps: runDeps{
		githubClient: client, eventLog: log, layout: paths.NewLayout(nil, root), errorLog: io.Discard,
		runSessionOpts: runSessionOptions{
			now:         func() time.Time { return now },
			currentHead: func(string) (string, error) { return "current-sha", nil },
		},
	}}
	row := RowSpec{IssueNumber: 42, RunID: "run-rerun-prerequisite", Branches: map[int]string{42: gateTestBranch}}
	continued := e.observeAwaitingRowLifecycle(t.Context(), row)
	if continued.Action != lifecycleAwait || continued.Evidence["gate"] != string(lifecycleGatePending) || continued.ciRepairRequired {
		t.Fatalf("same identified active workflow prerequisite should remain awaited: %+v", continued)
	}

	client.prs[gateTestBranch].CIActiveFailedWorkflowRunID = ""
	withoutPrerequisite := e.observeAwaitingRowLifecycle(t.Context(), row)
	if withoutPrerequisite.Action != lifecycleResume || withoutPrerequisite.Evidence["reason"] != "CI_FAILURE" || !withoutPrerequisite.ciRepairRequired {
		t.Fatalf("completed or changed workflow prerequisite must return CI repair to the implementation agent: %+v", withoutPrerequisite)
	}
}

func TestCIRerunActionEvidenceIsCurrentAttemptAndMatchesWorkflowIdentity(t *testing.T) {
	valid := ciRerunActionEvidence{
		Protocol: "ci-rerun/v1", State: "waiting", Reason: "workflow-still-running",
		Repository: "owner/repo", PullRequest: 17, SourceHeadSHA: "current-sha", TestedSHA: "tested-merge-sha",
		Diagnosis: "transient-provider-timeout", RunID: "101", JobID: "987654321", JobName: "opencode-canary", ObservedAttempt: 4,
	}
	line, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	prerequisite := map[string]any{
		"protocol": "ci-rerun-prerequisite/v1", "repository": "owner/repo", "pull_request": 17,
		"head_sha": "current-sha", "workflow_run_id": "101", "failed_check_names": []any{"opencode-canary", "quality-gate"},
		"failed_checks":        failedCIChecks("101", "opencode-canary", "quality-gate"),
		"workflow_check_names": []any{"opencode-canary", "quality-gate", "build-macos"},
	}
	command := `sh "$HOME/.agents/skills/sandman/implement/ci-rerun-v1.sh" --repository owner/repo --pull-request 17 --head-sha current-sha --tested-sha tested-merge-sha --run-id 101 --job-id 987654321 --diagnosis transient-provider-timeout`
	current := ciRerunHelperToolEvidence(command, string(line))
	if current == nil || !current.matchesPrerequisite(prerequisite) {
		t.Fatalf("structured current helper result did not match its prerequisite: %+v", current)
	}
	confirmed := valid
	confirmed.State = "confirmed"
	confirmed.Reason = "selected-job-and-quality-gate-rerun-observed"
	confirmed.ObservedAttempt = 5
	if !confirmed.matchesPrerequisite(prerequisite) {
		t.Fatalf("confirmed new execution did not match its prerequisite: %+v", confirmed)
	}
	withoutExecutionLinks := make(map[string]any, len(prerequisite)-1)
	for key, value := range prerequisite {
		if key != "failed_checks" {
			withoutExecutionLinks[key] = value
		}
	}
	if current.matchesPrerequisite(withoutExecutionLinks) {
		t.Fatal("name-only legacy evidence authorized waiting without failed-check execution identities")
	}
	withoutWorkflowNames := make(map[string]any, len(prerequisite)-1)
	for key, value := range prerequisite {
		if key != "workflow_check_names" {
			withoutWorkflowNames[key] = value
		}
	}
	if current.matchesPrerequisite(withoutWorkflowNames) {
		t.Fatal("helper evidence authorized waiting without names for all workflow executions")
	}
	duplicateIdentity := make(map[string]any, len(prerequisite))
	for key, value := range prerequisite {
		duplicateIdentity[key] = value
	}
	duplicateIdentity["failed_checks"] = []github.CIFailedCheck{
		{Name: "opencode-canary", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/11"},
		{Name: "quality-gate", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/11"},
	}
	if current.matchesPrerequisite(duplicateIdentity) {
		t.Fatal("duplicate execution links authorized waiting for an ambiguous prerequisite")
	}
	for name, mutate := range map[string]func(*ciRerunActionEvidence){
		"repository":   func(e *ciRerunActionEvidence) { e.Repository = "other/repo" },
		"pull request": func(e *ciRerunActionEvidence) { e.PullRequest++ },
		"source head":  func(e *ciRerunActionEvidence) { e.SourceHeadSHA = "old-head" },
		"workflow run": func(e *ciRerunActionEvidence) { e.RunID = "102" },
		"zero run id":  func(e *ciRerunActionEvidence) { e.RunID = "0" },
		"job id":       func(e *ciRerunActionEvidence) { e.JobID = "not-a-database-id" },
		"job name":     func(e *ciRerunActionEvidence) { e.JobName = "" },
		"diagnosis":    func(e *ciRerunActionEvidence) { e.Diagnosis = "deterministic-source-failure" },
		"confirmed without a new attempt": func(e *ciRerunActionEvidence) {
			e.State, e.Reason, e.ObservedAttempt = "confirmed", "selected-job-and-quality-gate-rerun-observed", 1
		},
		"reason": func(e *ciRerunActionEvidence) { e.Reason = "rerun-submission-not-confirmed" },
	} {
		t.Run(name, func(t *testing.T) {
			evidence := valid
			mutate(&evidence)
			data, err := json.Marshal(evidence)
			if err != nil {
				t.Fatal(err)
			}
			if action := ciRerunHelperToolEvidence(command, string(data)); action != nil && action.matchesPrerequisite(prerequisite) {
				t.Fatalf("mismatched helper evidence authorized a wait: %+v", evidence)
			}
		})
	}
	for name, suffix := range map[string]string{
		"ordinary echo": `; printf '%s' ` + string(line),
		"pipeline":      ` | cat`,
		"substitution":  `$(printf x)`,
	} {
		t.Run(name, func(t *testing.T) {
			if action := ciRerunHelperToolEvidence(command+suffix, string(line)); action != nil {
				t.Fatalf("composed shell command authorized helper evidence: %+v", action)
			}
		})
	}
}

func TestCIRerunActionEvidenceDoesNotMaskIndependentFailureInSameWorkflow(t *testing.T) {
	var action ciRerunActionEvidence
	if err := json.Unmarshal([]byte(`{"protocol":"ci-rerun/v1","state":"waiting","reason":"workflow-still-running","repository":"owner/repo","pull_request":17,"source_head_sha":"current-sha","tested_sha":"tested-merge-sha","diagnosis":"transient-provider-timeout","run_id":"101","job_id":"987654321","job_name":"opencode-canary","observed_attempt":1}`), &action); err != nil {
		t.Fatal(err)
	}
	prerequisite := map[string]any{
		"protocol": "ci-rerun-prerequisite/v1", "repository": "owner/repo", "pull_request": 17,
		"head_sha": "current-sha", "workflow_run_id": "101",
		"failed_check_names":   []any{"opencode-canary", "source-tests"},
		"failed_checks":        failedCIChecks("101", "opencode-canary", "source-tests"),
		"workflow_check_names": []any{"opencode-canary", "source-tests", "build-macos"},
	}
	if action.matchesPrerequisite(prerequisite) {
		t.Fatal("rerun receipt for opencode-canary authorized waiting while source-tests was independently failed")
	}
	prerequisite["failed_check_names"] = []any{"opencode-canary", "quality-gate"}
	prerequisite["failed_checks"] = failedCIChecks("101", "opencode-canary", "quality-gate")
	prerequisite["workflow_check_names"] = []any{"opencode-canary", "quality-gate", "build-macos"}
	if !action.matchesPrerequisite(prerequisite) {
		t.Fatal("dependent quality-gate failure should remain covered by the selected-job rerun")
	}
}

func TestLifecycleObservation_RerunReceiptDoesNotMaskDuplicateSameNameFailures(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
		prs: map[string]*github.PR{gateTestBranch: {
			Number: 17, State: "open", HeadRefOid: "current-sha", CIExecutionID: "checks-with-duplicate-failures",
			StatusCheckRollup: "failure", CIHasActiveChecks: true, CIActiveFailedWorkflowRunID: "101",
			CIActiveFailedWorkflowRunFailedCheckNames: []string{"opencode-canary", "opencode-canary"},
			CIActiveFailedWorkflowRunFailedChecks:     failedCIChecks("101", "opencode-canary", "opencode-canary"),
			CIActiveFailedWorkflowRunCheckNames:       []string{"build-macos", "opencode-canary", "opencode-canary"},
			MergeStateStatus:                          "BLOCKED",
		}},
	}
	session := &runSession{
		issueNumber: 42,
		deps:        runDeps{githubClient: client, errorLog: io.Discard},
		opts:        runSessionOptions{currentHead: func(string) (string, error) { return "current-sha", nil }},
	}
	request := lifecycleObservationRequest{
		WorkDir: t.TempDir(), Branch: gateTestBranch, RunID: "run-duplicate-same-name-failure", HostPathsReady: true,
		AttemptStatus: "success", AfterAgentAttempt: true,
		CIRerunAction: captureCIRerunActionFromOpenCodeToolEvent(t,
			`sh "$HOME/.agents/skills/sandman/implement/ci-rerun-v1.sh" --repository owner/repo --pull-request 17 --head-sha current-sha --tested-sha tested-merge-sha --run-id 101 --job-id 987654321 --diagnosis transient-provider-timeout`,
			`{"protocol":"ci-rerun/v1","state":"waiting","reason":"workflow-still-running","repository":"owner/repo","pull_request":17,"source_head_sha":"current-sha","tested_sha":"tested-merge-sha","diagnosis":"transient-provider-timeout","run_id":"101","job_id":"987654321","job_name":"opencode-canary","observed_attempt":1}`),
	}
	verdict := newLifecycleObserver(session).Observe(t.Context(), request)
	if verdict.Action != lifecycleResume || verdict.Evidence["reason"] != "CI_FAILURE" || !verdict.ciRepairRequired {
		t.Fatalf("receipt for one opencode-canary deferred a second same-name failure: %+v", verdict)
	}
}

func TestLifecycleDecisionRejectsDuplicateNamesFromParsedGitHubRollup(t *testing.T) {
	client := github.NewCLIClient(github.WithRunner(lifecycleCLIResultRunner{output: `[{"number":17,"state":"open","headRefName":"42-fix","headRefOid":"current-sha","statusCheckRollup":[{"__typename":"CheckRun","status":"COMPLETED","conclusion":"FAILURE","name":"opencode-canary","detailsUrl":"https://github.com/owner/repo/actions/runs/101/job/11"},{"__typename":"CheckRun","status":"COMPLETED","conclusion":"TIMED_OUT","name":"opencode-canary","detailsUrl":"https://github.com/owner/repo/actions/runs/101/job/14"},{"__typename":"CheckRun","status":"IN_PROGRESS","name":"build-macos","detailsUrl":"https://github.com/owner/repo/actions/runs/101/job/12"}]}]`}))
	client.RepoOverride = "owner/repo"
	pr, err := client.FindPRByBranch(t.Context(), "42-fix")
	if err != nil || pr == nil {
		t.Fatalf("parse current PR check rollup: pr=%+v err=%v", pr, err)
	}
	if pr.CIActiveFailedWorkflowRunID != "101" || len(pr.CIActiveFailedWorkflowRunFailedChecks) != 2 {
		t.Fatalf("parsed prerequisite lost duplicate job evidence: %+v", pr)
	}
	action := ciRerunActionEvidence{
		Protocol: "ci-rerun/v1", State: "waiting", Reason: "workflow-still-running",
		Repository: "owner/repo", PullRequest: 17, SourceHeadSHA: "current-sha", TestedSHA: "tested-merge-sha",
		Diagnosis: "transient-provider-timeout", RunID: "101", JobID: "987654321", JobName: "opencode-canary", ObservedAttempt: 1,
	}
	decision := decideRecoverableLifecycle(lifecycleGatePending, pr, "current-sha", "owner/repo",
		retainedReviewEvidence{payload: map[string]any{ciRerunActionKey: action.payload()}}, false, "success", true)
	if decision.action != lifecycleResume || !decision.ciRepairRequired {
		t.Fatalf("database job ID for one same-name check authorized waiting for both parsed failures: %+v", decision)
	}
}

func TestLifecycleDecisionDoesNotAwaitForActiveAndFailedSameNameChecks(t *testing.T) {
	client := github.NewCLIClient(github.WithRunner(lifecycleCLIResultRunner{output: `[{"number":17,"state":"open","headRefName":"42-fix","headRefOid":"current-sha","statusCheckRollup":[{"__typename":"CheckRun","status":"COMPLETED","conclusion":"TIMED_OUT","name":"opencode-canary","detailsUrl":"https://github.com/owner/repo/actions/runs/101/job/14"},{"__typename":"CheckRun","status":"IN_PROGRESS","name":"opencode-canary","detailsUrl":"https://github.com/owner/repo/actions/runs/101/job/11"},{"__typename":"CheckRun","status":"IN_PROGRESS","name":"build-macos","detailsUrl":"https://github.com/owner/repo/actions/runs/101/job/12"}]}]`}))
	client.RepoOverride = "owner/repo"
	pr, err := client.FindPRByBranch(t.Context(), "42-fix")
	if err != nil || pr == nil {
		t.Fatalf("parse current PR check rollup: pr=%+v err=%v", pr, err)
	}
	action := ciRerunActionEvidence{
		Protocol: "ci-rerun/v1", State: "waiting", Reason: "recovery-already-active",
		Repository: "owner/repo", PullRequest: 17, SourceHeadSHA: "current-sha", TestedSHA: "tested-merge-sha",
		Diagnosis: "transient-provider-timeout", RunID: "101", JobID: "987654321", JobName: "opencode-canary", ObservedAttempt: 1,
	}
	decision := decideRecoverableLifecycle(lifecycleGatePending, pr, "current-sha", "owner/repo",
		retainedReviewEvidence{payload: map[string]any{ciRerunActionKey: action.payload()}}, false, "success", true)
	if decision.action != lifecycleResume || !decision.ciRepairRequired {
		t.Fatalf("receipt for active opencode-canary authorized waiting for the failed same-name check: %+v", decision)
	}
}

func TestAwaitingRowLifecycle_FailedCIWithUnrelatedActiveCheckResumesImplementation(t *testing.T) {
	root := t.TempDir()
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
		prs: map[string]*github.PR{gateTestBranch: {
			Number: 17, State: "open", HeadRefOid: "current-sha", CIExecutionID: "checks-with-unrelated-active-workflow",
			StatusCheckRollup: "failure", CIHasActiveChecks: true, MergeStateStatus: "BLOCKED",
		}},
	}
	e := &runExecutor{deps: runDeps{
		githubClient: client,
		layout:       paths.NewLayout(nil, root),
		errorLog:     io.Discard,
		runSessionOpts: runSessionOptions{
			currentHead: func(string) (string, error) { return "current-sha", nil },
		},
	}}
	row := RowSpec{IssueNumber: 42, RunID: "run-pending-ci", Branches: map[int]string{42: gateTestBranch}}
	verdict := e.observeAwaitingRowLifecycle(t.Context(), row)
	if verdict.Action != lifecycleResume || verdict.Evidence["reason"] != "CI_FAILURE" || !verdict.ciRepairRequired {
		t.Fatalf("a failed check discovered during a pending-CI wait must return to implementation, not await unrelated active CI: %+v", verdict)
	}
}
