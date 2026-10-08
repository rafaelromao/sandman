package batch

import (
	"context"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/testenv"
)

func TestUsageLimitGate_TerminalFailureDoesNotAssignSiblingOutcome(t *testing.T) {
	dir := testenv.MkdirShort(t, "sm-usage-gate-")
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "First"},
			43: {Number: 43, Title: "Second", State: "closed"},
		},
		prs: map[string]*github.PR{"43-second": {Number: 17, State: "merged", Merged: true, Body: "Closes #43"}},
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
	if len(factory.created) != 2 || factory.created[0] != 42 || factory.created[1] != 43 {
		t.Fatalf("created=%v, want each independent row to establish its own outcome", factory.created)
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
	if paused.Status != "success" {
		t.Fatalf("independent sibling status=%q, want its verified merged success", paused.Status)
	}
	snap := spyLog.snapshot()
	foundAwait := false
	for _, e := range snap {
		if e.Issue == 43 && e.Type == "run.await" {
			foundAwait = true
		}
	}
	if foundAwait {
		t.Fatal("paused row must not emit run.await")
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
	if !st.IsTerminal() || st.Status() != "success" {
		t.Fatalf("sibling lost its own verified outcome: active=%v queued=%v status=%q", st.IsActive(), st.IsCapacityQueued(), st.Status())
	}
}
