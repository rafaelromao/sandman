package batch

import (
	"context"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/github"
)

// A runtime rerun that fails again on the same diagnosed job keeps the window
// open: the next due attempt reruns the newer failed attempt once, and the
// 24-hour bound does not move.
func TestCIRerunRecoveryRepeatFailureKeepsWindowAndRerunsAgain(t *testing.T) {
	now := time.Date(2026, time.November, 23, 12, 0, 0, 0, time.UTC)
	status := &scopedProbeStatus{report: github.StatusReport{Indicator: "none"}}
	pr := &github.PR{
		Number: 17, State: "open", HeadRefOid: "head", StatusCheckRollup: "failure",
		CIActiveFailedWorkflowRunID: "101",
		CIActiveFailedWorkflowRunFailedChecks: []github.CIFailedCheck{
			{Name: "opencode-canary", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/11"},
			{Name: "quality-gate", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/12"},
		},
		CIActiveFailedWorkflowRunCheckNames: []string{"opencode-canary", "quality-gate"},
	}
	client := &ciRecoveryFakeClient{
		fakeGitHubClient: &fakeGitHubClient{prs: map[string]*github.PR{"42-work": pr}},
		jobs: []github.CIWorkflowJob{
			{RunID: 101, RunAttempt: 2, ID: 21, Name: "opencode-canary", Status: "completed", Conclusion: "failure"},
			{RunID: 101, RunAttempt: 2, ID: 22, Name: "quality-gate", Status: "completed", Conclusion: "failure"},
		},
		rerunJobs: []github.CIWorkflowJob{
			{RunID: 101, RunAttempt: 3, ID: 31, Name: "opencode-canary", Status: "in_progress"},
			{RunID: 101, RunAttempt: 3, ID: 32, Name: "quality-gate", Status: "queued"},
		},
	}
	log := &spyEventLog{}
	e, row := ciRecoveryFixture(t, now, client, log, status)
	makeCIRRecoveryDue(t, log, now)

	first := e.observeAwaitingRowLifecycle(context.Background(), row)
	if first.Action != lifecycleAwait || len(client.rerunCalls) != 1 {
		t.Fatalf("first rerun = %s/%v calls=%v", first.status(), first.Evidence, client.rerunCalls)
	}
	firstState, ok := ciRecoveryFromEvidence(first.Evidence)
	if !ok {
		t.Fatalf("first wait has no CI recovery evidence: %v", first.Evidence)
	}
	if err := e.persistObservedAwait(context.Background(), row, first.Evidence); err != nil {
		t.Fatal(err)
	}

	// The rerun finishes red again on the same job.
	client.jobs = []github.CIWorkflowJob{
		{RunID: 101, RunAttempt: 3, ID: 31, Name: "opencode-canary", Status: "completed", Conclusion: "failure"},
		{RunID: 101, RunAttempt: 3, ID: 32, Name: "quality-gate", Status: "completed", Conclusion: "failure"},
	}
	client.rerunJobs = []github.CIWorkflowJob{
		{RunID: 101, RunAttempt: 4, ID: 41, Name: "opencode-canary", Status: "in_progress"},
		{RunID: 101, RunAttempt: 4, ID: 42, Name: "quality-gate", Status: "queued"},
	}

	now = now.Add(30 * time.Minute)
	e.deps.runSessionOpts.now = func() time.Time { return now }
	quiet := e.observeAwaitingRowLifecycle(context.Background(), row)
	if quiet.Action != lifecycleAwait || len(client.rerunCalls) != 1 {
		t.Fatalf("mid-hour observation = %s/%v calls=%v, want a quiet wait", quiet.status(), quiet.Evidence, client.rerunCalls)
	}

	now = now.Add(30 * time.Minute)
	e.deps.runSessionOpts.now = func() time.Time { return now }
	second := e.observeAwaitingRowLifecycle(context.Background(), row)
	state, valid := ciRecoveryFromEvidence(second.Evidence)
	if second.Action != lifecycleAwait || stringEvidence(second.Evidence, "gate") != ciRecoveryGate || !valid {
		t.Fatalf("repeat failure left the window: %s/%v", second.status(), second.Evidence)
	}
	if len(client.rerunCalls) != 2 || client.rerunCalls[0] != 21 || client.rerunCalls[1] != 31 {
		t.Fatalf("rerun calls = %v, want job 21 then job 31", client.rerunCalls)
	}
	if state.RerunAttempts != 2 || state.LastWorkflowAttempt != 3 || state.LastRerunOutcome != "confirmed" {
		t.Fatalf("state = attempts %d, last attempt %d, outcome %q; want 2, 3, confirmed",
			state.RerunAttempts, state.LastWorkflowAttempt, state.LastRerunOutcome)
	}
	if !state.bound().Equal(firstState.bound()) {
		t.Fatalf("bound moved from %s to %s", firstState.bound(), state.bound())
	}
}
