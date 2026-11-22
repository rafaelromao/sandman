package batch

import (
	"slices"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/github"
)

// GitHub recovery runs on the reusable recovery window and keeps its own
// health judgement: the components that make pull-request lookups fail,
// requested from the injected status checker before each hourly attempt
// (issue #3009).
func TestGitHubRecovery_AsksTheStatusCheckerForPullRequestLookupComponents(t *testing.T) {
	f := newGitHubRecoveryFixture(t)
	status := &scopedProbeStatus{report: github.StatusReport{Indicator: "none"}}
	observe := func() lifecycleVerdict {
		s := f.session()
		s.deps.githubStatus = status
		verdict := newLifecycleObserver(s).Observe(t.Context(), lifecycleObservationRequest{
			WorkDir: t.TempDir(), Branch: gateTestBranch, RunID: f.runID, HostPathsReady: true, AttemptStatus: "success",
		})
		s.emitAwait(t.Context(), f.runID, AgentRunResult{Branch: gateTestBranch}, verdict.Evidence)
		return verdict
	}

	githubRecoveryOf(t, observe())
	if len(status.requested) != 0 {
		t.Fatalf("the first failure checked status %d times, want a window opened with no check", len(status.requested))
	}
	f.now = f.now.Add(githubRecoveryInterval)
	githubRecoveryOf(t, observe())
	if len(status.requested) != 1 || !slices.Equal(status.requested[0], github.PullRequestLookupComponents) || status.plainCalls != 0 {
		t.Fatalf("status requests = %v (plain calls %d), want one request for the pull-request lookup components %v",
			status.requested, status.plainCalls, github.PullRequestLookupComponents)
	}

	// A degraded pull-request lookup component, as the status page client
	// reports it for that set, skips the attempt and keeps the window open.
	status.report = github.StatusReport{Indicator: "minor", Outage: true, Components: []string{"Pull Requests: partial_outage"}}
	lookups := f.lookups
	f.now = f.now.Add(githubRecoveryInterval)
	recovery := githubRecoveryOf(t, observe())
	if f.lookups != lookups || recovery.LastStatus != "outage" || !recovery.NextAttempt.Equal(f.now.Add(time.Hour)) {
		t.Fatalf("lookups %d→%d, recovery = %+v, want the attempt skipped on an outage", lookups, f.lookups, recovery)
	}
}
