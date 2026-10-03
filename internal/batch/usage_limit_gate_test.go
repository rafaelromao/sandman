package batch

import (
	"context"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/testenv"
)

func TestUsageLimitGate_PausesNotYetStartedRuns(t *testing.T) {
	dir := testenv.MkdirShort(t, "sm-usage-gate-")
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "First"},
			43: {Number: 43, Title: "Second"},
		},
	}
	spyLog := &spyEventLog{}
	factory := &controlledRunnableFactory{
		runnables: map[int]Runnable{
			42: &controlledRunnable{result: AgentRunResult{IssueNumber: 42, Status: "failure", UsageLimitReached: true, Branch: "42-first"}},
			43: &controlledRunnable{result: AgentRunResult{IssueNumber: 43, Status: "success", Branch: "43-second"}},
		},
	}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog,
		WithRunnableFactory(factory),
		WithSandboxFactory(&freshSandboxFactory{}),
	)
	result, _ := o.RunBatch(context.Background(), Request{
		Issues:   []int{42, 43},
		Branches: map[int]string{42: "42-first", 43: "43-second"},
		Parallel: 1,
		Retries:  0,
	})
	if result == nil {
		t.Fatal("result is nil")
	}
	for _, id := range factory.created {
		if id == 43 {
			t.Fatalf("issue 43 runnable created despite usage-limit gate; created=%v", factory.created)
		}
	}
	if len(factory.created) != 1 || factory.created[0] != 42 {
		t.Fatalf("created=%v, want [42]", factory.created)
	}
	var paused *AgentRunResult
	for i := range result.Runs {
		if result.Runs[i].IssueNumber == 43 {
			paused = &result.Runs[i]
		}
	}
	if paused == nil {
		t.Fatal("no result for issue 43")
	}
	if paused.Status != "queued" {
		t.Fatalf("paused status=%q, want queued", paused.Status)
	}
	snap := spyLog.snapshot()
	foundAwait := false
	foundQueued := false
	for _, e := range snap {
		if e.Issue == 43 && e.Type == "run.await" {
			foundAwait = true
		}
		if e.Issue == 43 && e.Type == "run.capacity_queued" {
			foundQueued = true
			if e.Payload["ready_continuation"] != true {
				t.Fatalf("capacity_queued missing ready_continuation: %#v", e.Payload)
			}
			if e.Payload["reason"] != "usage-limit-paused" && e.Payload["gate"] != "usage-limit" {
				t.Fatalf("capacity_queued missing usage-limit reason/gate: %#v", e.Payload)
			}
		}
	}
	if foundAwait {
		t.Fatal("paused row must not emit run.await")
	}
	if !foundQueued {
		t.Fatalf("expected run.capacity_queued for issue 43, got %v", snap)
	}
	states := events.ProjectRunStates(snap)
	var st *events.RunState
	for i := range states {
		if states[i].IssueNumber() == 43 {
			st = &states[i]
		}
	}
	if st == nil {
		t.Fatal("no RunState for issue 43")
	}
	if !st.IsCapacityQueued() || st.Status() != "queued" || !st.IsActive() {
		t.Fatalf("state active=%v queued=%v status=%q", st.IsActive(), st.IsCapacityQueued(), st.Status())
	}
}
