package cmd

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// These tests drive the real command, graph preparation, scheduler, executor
// and worktree sandbox. Only GitHub and Agent execution are controlled seams.
func (c *specDiscoveryGitHubClient) FindPRByBranch(_ context.Context, branch string) (*github.PR, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if pr := c.prs[branch]; pr != nil {
		copy := *pr
		return &copy, nil
	}
	return nil, nil
}

type planningRunnableFactory struct {
	gh       *specDiscoveryGitHubClient
	started  chan int
	release  map[int]chan struct{}
	fail     int
	keepOpen int
	mu       sync.Mutex
	active   int
	maximum  int
}

type planningRunnable struct {
	factory *planningRunnableFactory
	number  int
	branch  string
}

func (f *planningRunnableFactory) NewRunnable(issue *github.Issue, branch string, _ sandbox.Sandbox) batch.Runnable {
	return &planningRunnable{factory: f, number: issue.Number, branch: branch}
}

func (r *planningRunnable) Run(ctx context.Context, _ prompt.IssueRenderer, _ string, _ prompt.RenderConfig) batch.AgentRunResult {
	f := r.factory
	f.mu.Lock()
	f.active++
	f.maximum = max(f.maximum, f.active)
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.active--; f.mu.Unlock() }()
	f.started <- r.number
	select {
	case <-f.release[r.number]:
	case <-ctx.Done():
		return batch.AgentRunResult{Status: "aborted"}
	}
	if r.number == f.fail {
		return batch.AgentRunResult{Status: "failure"}
	}
	if r.number != f.keepOpen {
		f.gh.setIssueState(r.number, "closed")
	}
	f.gh.mu.Lock()
	if f.gh.prs == nil {
		f.gh.prs = make(map[string]*github.PR)
	}
	f.gh.prs[r.branch] = &github.PR{Number: 100 + r.number, State: "merged", Merged: true, HeadRefName: r.branch, Body: fmt.Sprintf("Closes #%d", r.number)}
	f.gh.mu.Unlock()
	return batch.AgentRunResult{Status: "success"}
}

type preparedExecutionRunner struct {
	runner batch.Runner
	req    batch.Request
	before func(batch.Request)
}

func (r *preparedExecutionRunner) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
	r.req = req
	if r.before != nil {
		r.before(req)
	}
	return r.runner.RunBatch(ctx, req)
}

func startPreparedExecution(t *testing.T, gh github.Client, factory *planningRunnableFactory, before func(batch.Request), args ...string) (context.Context, <-chan error, *recordingEventLog, *preparedExecutionRunner) {
	t.Helper()
	dir := testenv.MkdirShort(t, "sm-plan-")
	t.Chdir(dir)
	initRunIntegrationRepoWithRemote(t, dir)
	store := &fakeStore{config: &config.Config{
		Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, ReviewCommand: "/oc review",
		AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}},
	}}
	log := &recordingEventLog{}
	orchestrator := batch.NewOrchestrator(gh, &prompt.Engine{}, store, log,
		batch.WithRunnableFactory(factory), batch.WithErrorLog(io.Discard))
	runner := &preparedExecutionRunner{runner: orchestrator, before: before}
	cmd := NewRunCmd(Dependencies{BatchRunner: runner, ConfigStore: store, GitHubClient: gh, EventLog: log, RepoRoot: dir})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	cmd.SetContext(ctx)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(append(args, "--parallel", "2", "--retries", "0", "--start-delay", "0", "--run-idle-timeout", "0"))
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() { defer close(exited); done <- cmd.Execute() }()
	// A failed assertion cancels and joins the command before Chdir cleanup.
	t.Cleanup(func() { cancel(); <-exited })
	return ctx, done, log, runner
}

func newPlanningRunnables(gh *specDiscoveryGitHubClient) *planningRunnableFactory {
	f := &planningRunnableFactory{gh: gh, started: make(chan int, len(gh.issues)), release: make(map[int]chan struct{})}
	for n := range gh.issues {
		f.release[n] = make(chan struct{})
	}
	return f
}

func nextPlanningStart(t *testing.T, ctx context.Context, done <-chan error, f *planningRunnableFactory) int {
	t.Helper()
	select {
	case n := <-f.started:
		return n
	case err := <-done:
		t.Fatalf("command finished before expected AgentRun: %v", err)
		return 0
	case <-ctx.Done():
		t.Fatal("prepared AgentRun did not start before deadline")
		return 0
	}
}

func TestRun_PreparedGraphExecutesConcurrentEligibleAgentRuns(t *testing.T) {
	for _, stateAPI := range []bool{false, true} {
		t.Run(fmt.Sprintf("state-api=%t", stateAPI), func(t *testing.T) { testPreparedConcurrentExecution(t, stateAPI) })
	}
}

func testPreparedConcurrentExecution(t *testing.T, stateAPI bool) {
	gh := newPlanningGitHub()
	fresh := &freshPlanningGitHub{gh, make(map[int]int)}
	var client github.Client = gh
	if stateAPI {
		client = fresh
	}
	f := newPlanningRunnables(gh)
	ctx, done, log, runner := startPreparedExecution(t, client, f, nil, "1", "3", "7", "8")
	a, b := nextPlanningStart(t, ctx, done, f), nextPlanningStart(t, ctx, done, f)
	for _, n := range []int{a, b} {
		if !slices.Contains([]int{5, 7, 8}, n) {
			t.Fatalf("ineligible initial AgentRun #%d", n)
		}
	}
	if a == b {
		t.Fatal("same Issue started twice")
	}
	select {
	case n := <-f.started:
		t.Fatalf("parallelism exceeded: #%d", n)
	default:
	}
	close(f.release[a])
	close(f.release[b])
	for range 5 {
		n := nextPlanningStart(t, ctx, done, f)
		close(f.release[n])
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertPreparedEvents(t, log, map[int]string{1: "success", 2: "success", 3: "success", 4: "success", 5: "success", 7: "success", 8: "success"})
	rows, _ := log.Read()
	finished := make(map[int]bool)
	for _, event := range rows {
		if event.Type == "run.started" {
			for _, blocker := range runner.req.Dependencies[event.Issue] {
				if !finished[blocker] {
					t.Errorf("#%d started before #%d finished successfully", event.Issue, blocker)
				}
			}
		}
		if event.Type == "run.finished" && event.Payload["status"] == "success" {
			finished[event.Issue] = true
		}
	}
	if f.maximum != 2 {
		t.Fatalf("maximum active AgentRuns = %d, want 2", f.maximum)
	}
	if stateAPI && fresh.stateReads[4] < 3 {
		t.Errorf("shared child was not freshly checked by all parents: %v", fresh.stateReads)
	}
	if !slices.Equal(gh.issues[1].BlockedBy, []int{4, 8}) || len(gh.issues[2].BlockedBy) != 0 {
		t.Fatal("synthetic gates modified GitHub metadata")
	}
}

func assertPreparedEvents(t *testing.T, log *recordingEventLog, want map[int]string) {
	t.Helper()
	rows, _ := log.Read()
	got := make(map[int]string)
	for _, state := range events.ProjectRunStates(rows) {
		got[state.IssueNumber()] = state.Status()
	}
	for n, status := range want {
		if got[n] != status {
			t.Errorf("#%d projected %q, want %q; events=%+v", n, got[n], status, rows)
		}
	}
	for _, event := range rows {
		if event.Type == "run.await" {
			t.Errorf("dependency gates must not create await events: %+v", event)
		}
	}
}

type freshPlanningGitHub struct {
	*specDiscoveryGitHubClient
	stateReads map[int]int
}

func (c *freshPlanningGitHub) FetchIssueState(_ context.Context, n int) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stateReads[n]++
	return c.issues[n].State, nil
}

func TestRun_PreparedGraphBlocksParentsAfterChildFailureOrLiveOpenSuccess(t *testing.T) {
	for _, failure := range []bool{true, false} {
		for _, stateAPI := range []bool{false, true} {
			t.Run(fmt.Sprintf("failure=%t/state-api=%t", failure, stateAPI), func(t *testing.T) {
				gh := newSpecDiscoveryGitHub(map[int]*github.Issue{
					1: {Number: 1, State: "open", Title: "Root", Body: "## Children\n\n- #2\n- #4\n"},
					2: {Number: 2, State: "open", Title: "Nested", Body: "## Children\n\n- #4\n"},
					3: {Number: 3, State: "open", Title: "Shared parent", Body: "## Children\n\n- #4\n"},
					4: {Number: 4, State: "open", Title: "Shared child"},
					7: {Number: 7, State: "open", Title: "Independent"},
				})
				fresh := &freshPlanningGitHub{gh, make(map[int]int)}
				var client github.Client = gh
				if stateAPI {
					client = fresh
				}
				f := newPlanningRunnables(gh)
				childStatus := "success"
				if failure {
					f.fail = 4
					childStatus = "failure"
				} else {
					f.keepOpen = 4
				}
				ctx, done, log, _ := startPreparedExecution(t, client, f, nil, "1", "3", "7")
				for range 2 {
					n := nextPlanningStart(t, ctx, done, f)
					if n != 4 && n != 7 {
						t.Fatalf("parent #%d started before its child completed", n)
					}
					close(f.release[n])
				}
				<-done
				assertPreparedEvents(t, log, map[int]string{1: "blocked", 2: "blocked", 3: "blocked", 4: childStatus, 7: "success"})
				select {
				case n := <-f.started:
					t.Fatalf("blocked parent #%d launched an Agent", n)
				default:
				}
				rows, _ := log.Read()
				for _, e := range rows {
					if e.Type == "run.blocked" && (e.Issue == 2 || e.Issue == 3) {
						if blockers, ok := e.Payload["blocked_by"].([]int); !ok || !slices.Equal(blockers, []int{4}) {
							t.Errorf("wrong blocked child evidence: %+v", e)
						}
					}
				}
				if !failure && stateAPI && fresh.stateReads[4] < 2 {
					t.Errorf("successful child's closure was not freshly checked by both parents: %v", fresh.stateReads)
				}
			})
		}
	}
}

func TestRun_PreparedExternalBlockersAreFreshlyRechecked(t *testing.T) {
	for _, closed := range []bool{false, true} {
		for _, stateAPI := range []bool{false, true} {
			t.Run(fmt.Sprintf("live-closed=%t/state-api=%t", closed, stateAPI), func(t *testing.T) {
				gh := newSpecDiscoveryGitHub(map[int]*github.Issue{
					9:  {Number: 9, State: "open", Title: "External blocker"},
					10: {Number: 10, State: "open", Title: "Dependent", BlockedBy: []int{9}},
					11: {Number: 11, State: "open", Title: "Independent"},
				})
				fresh := &freshPlanningGitHub{gh, make(map[int]int)}
				var client github.Client = gh
				if stateAPI {
					client = fresh
				}
				f := newPlanningRunnables(gh)
				ctx, done, log, _ := startPreparedExecution(t, client, f, func(req batch.Request) {
					if !slices.Equal(req.Blocked[10], []int{9}) {
						t.Errorf("external blocker lost during preparation: %v", req.Blocked)
					}
					if closed {
						gh.setIssueState(9, "closed")
					}
				}, "10", "11")
				launches, dependentStatus := 1, "blocked"
				if closed {
					launches, dependentStatus = 2, "success"
				}
				for range launches {
					n := nextPlanningStart(t, ctx, done, f)
					close(f.release[n])
				}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				assertPreparedEvents(t, log, map[int]string{10: dependentStatus, 11: "success"})
				if stateAPI {
					if fresh.stateReads[9] != 1 {
						t.Errorf("fresh external reads = %v, want one", fresh.stateReads)
					}
				} else if gh.fetchCount[9] != 2 {
					t.Errorf("external full fetches = %v, want preparation plus live recheck", gh.fetchCount)
				}
			})
		}
	}
}
