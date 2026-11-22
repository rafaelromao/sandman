package batch

import (
	"context"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/github"
)

// RerunWorkflowJob makes the continuity harness's GitHub a client that can
// rerun a workflow job, like the production CLI client.
func (g *continuityGitHub) RerunWorkflowJob(_ context.Context, request github.WorkflowRerunRequest) (github.WorkflowRerunResult, error) {
	g.mu.Lock()
	g.reruns = append(g.reruns, request)
	hook := g.onRerun
	g.mu.Unlock()
	if hook != nil {
		hook(g)
	}
	return github.WorkflowRerunResult{State: github.WorkflowRerunConfirmed, Reason: "selected-job-and-quality-gate-rerun-observed", ObservedAttempt: 3}, nil
}

func (g *continuityGitHub) rerunCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.reruns)
}

func (g *continuityGitHub) rollup() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pr.StatusCheckRollup
}

func repeatLaunch(count int, launch func(*continuityGitHub) string) []func(*continuityGitHub) string {
	launches := make([]func(*continuityGitHub) string, count)
	for index := range launches {
		launches[index] = launch
	}
	return launches
}

func (g *continuityGitHub) advanceRuntime(by time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.runtimeOffset += by
}

// settledFailure is the pull request's checks once CI finished failing in the
// diagnosed job and its quality gate of one workflow run.
func settledFailure(executionID string) func(*github.PR) {
	return func(pr *github.PR) {
		pr.StatusCheckRollup, pr.MergeStateStatus, pr.CIHasActiveChecks = "failure", "CLEAN", false
		pr.CIExecutionID = executionID
		pr.CIFailedWorkflowRunID = ciTestRun
		pr.CIFailedWorkflowRunFailedChecks = []github.CIFailedCheck{
			{Name: ciTestJob, DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/11"},
			{Name: "quality-gate", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/15"},
		}
		pr.CIFailedWorkflowRunCheckNames = []string{"build-macos", ciTestJob, "quality-gate"}
	}
}

// The incident this window exists for: current-head CI fails because the
// hosted provider's free usage limit was reached. The implementor diagnoses it
// as transient and spends its one rerun, which fails the same way; relaunches
// then find nothing to do. The Run used to park until an operator aborted it.
// It now waits in a CI recovery window, the runtime reruns the job at an hour,
// the rerun turns green and the Run goes on to deliver the review request.
func TestAFKContinuityContract_TransientCIFailureRecoversInTheWindowAndMerges(t *testing.T) {
	receipt := ciTestReceipt()
	receipt.PullRequest, receipt.SourceHeadSHA = continuityPR, "head-1"
	var live *continuityGitHub
	outcome := runContinuityScenario(t, continuityScenario{
		name:    "transient CI failure recovers in the CI recovery window",
		pr:      github.PR{StatusCheckRollup: "failure", MergeStateStatus: "CLEAN"},
		retries: 3,
		setup: func(g *continuityGitHub) {
			live = g
			g.update(settledFailure("exec-1"))
			g.onRerun = func(g *continuityGitHub) {
				// The runtime's rerun is submitted; the hour that follows lets
				// it finish green.
				g.update(func(pr *github.PR) { pr.StatusCheckRollup, pr.CIHasActiveChecks = "pending", true })
			}
		},
		launchReceipts: map[int]ciRerunActionEvidence{0: receipt},
		launches: append([]func(*continuityGitHub) string{
			func(g *continuityGitHub) string {
				// The implementor's one authorized rerun is running.
				g.update(func(pr *github.PR) {
					pr.StatusCheckRollup, pr.CIHasActiveChecks, pr.CIExecutionID = "pending", true, "exec-rerun-1"
				})
				return "success"
			},
		}, repeatLaunch(12, func(g *continuityGitHub) string {
			// While CI is red the relaunches find nothing to do. Once it is
			// green the implementor delivers the review request and merges
			// the approved pull request.
			if g.rollup() == "success" {
				g.trigger()
				g.publish("APPROVED")
				if g.mergeable() {
					g.merge()
				}
			}
			return "success"
		})...),
		polls: []func(*continuityGitHub){
			func(g *continuityGitHub) {
				// The implementor's rerun finished and failed the same way.
				g.update(settledFailure("exec-2"))
			},
			nil, nil, nil, nil, nil,
			func(g *continuityGitHub) {
				// An hour on, the runtime's rerun has finished green.
				g.advanceRuntime(time.Hour)
				g.update(func(pr *github.PR) {
					pr.StatusCheckRollup, pr.CIHasActiveChecks, pr.MergeStateStatus = "success", false, "CLEAN"
				})
			},
			func(g *continuityGitHub) { g.advanceRuntime(time.Hour) },
			func(g *continuityGitHub) { g.advanceRuntime(time.Hour) },
		},
	})
	waited := false
	for _, event := range outcome.current() {
		if event.Type == "run.await" && event.Payload["await_reason"] == gateCIRecovery {
			waited = true
			if _, ok := ciRecoveryFromEvidence(event.Payload); !ok {
				t.Fatalf("ci-recovery wait carries no window: %v", event.Payload)
			}
		}
		if event.Type == "run.await" && event.Payload["reason"] == resumeStalledReason {
			t.Fatalf("the Run parked in a stalled hold: %s", continuityEventSummary(outcome.events))
		}
	}
	if !waited {
		t.Fatalf("the Run never waited in a CI recovery window\nevents: %s", continuityEventSummary(outcome.events))
	}
	if live.rerunCount() != 1 {
		t.Fatalf("runtime reruns = %d, want exactly one for the hour\nevents: %s", live.rerunCount(), continuityEventSummary(outcome.events))
	}
	if want := (github.WorkflowRerunRequest{Repository: ciTestRepo, PullRequest: continuityPR, HeadSHA: "head-1", TestedSHA: ciTestTested, RunID: ciTestRun, JobName: ciTestJob}); live.reruns[0] != want {
		t.Fatalf("rerun = %+v, want %+v", live.reruns[0], want)
	}
	order := []string{}
	for _, event := range outcome.current() {
		switch {
		case event.Type == "run.await" && event.Payload["await_reason"] == gateCIRecovery:
			order = append(order, "ci-recovery")
		case event.Type == "run.capacity_queued" && event.Payload["reason"] == "REVIEW_REQUEST_REQUIRED":
			order = append(order, "review-request")
		}
	}
	if len(order) < 2 || order[0] != "ci-recovery" || order[len(order)-1] != "review-request" || outcome.triggers != 1 {
		t.Fatalf("lifecycle order = %v with %d review requests, want the CI recovery window and then one review-request delivery\nevents: %s",
			order, outcome.triggers, continuityEventSummary(outcome.events))
	}
	if got := outcome.verdict(); got != "merged" {
		t.Fatalf("outcome = %s, want merged after the window recovers\nevents: %s", got, continuityEventSummary(outcome.events))
	}
}
