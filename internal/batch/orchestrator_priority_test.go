package batch

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

func TestRunBatch_ReadyAwaitedRowPrecedesQueuedIndependentWork(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "Awaited"},
			2: {Number: 2, Title: "Independent"},
			3: {Number: 3, Title: "Dependent"},
		},
		prs: map[string]*github.PR{
			"2-independent": {Number: 2, State: "merged", Merged: true, Body: "Closes #2", HeadRefName: "2-independent"},
			"3-dependent":   {Number: 3, State: "merged", Merged: true, Body: "Closes #3", HeadRefName: "3-dependent"},
		},
		findPRSequence: map[string][]*github.PR{
			"1-awaited": {
				{Number: 1, State: "open", Body: "Closes #1", HeadRefName: "1-awaited"},
				{Number: 1, State: "open", Body: "Closes #1", HeadRefName: "1-awaited"},
				{Number: 1, State: "open", Body: "Closes #1", HeadRefName: "1-awaited", StatusCheckRollup: "failure"},
				{Number: 1, State: "open", Body: "Closes #1", HeadRefName: "1-awaited", StatusCheckRollup: "failure"},
				{Number: 1, State: "open", Body: "Closes #1", HeadRefName: "1-awaited", StatusCheckRollup: "failure"},
				{Number: 1, State: "merged", Merged: true, Body: "Closes #1", HeadRefName: "1-awaited"},
				{Number: 1, State: "merged", Merged: true, Body: "Closes #1", HeadRefName: "1-awaited"},
				{Number: 1, State: "merged", Merged: true, Body: "Closes #1", HeadRefName: "1-awaited"},
			},
		},
	}
	independentStarted := make(chan struct{})
	allowIndependentFinish := make(chan struct{})
	timerElapsed := make(chan struct{})
	allowTimerReturn := make(chan struct{})
	priorityQueued := make(chan struct{})
	log := &spyEventLog{}
	factory := &awaitPriorityRunnableFactory{
		independentStarted:     independentStarted,
		allowIndependentFinish: allowIndependentFinish,
	}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{
		Agent:          "test-agent",
		Sandbox:        "worktree",
		WorktreeDir:    ".sandman/worktrees",
		Git:            config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}},
	}}, log,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&freshSandboxFactory{}),
		WithRunnableFactory(factory),
		WithRunSessionOpts(runSessionOptions{
			releaseAwaitCapacity: true,
			awaitWait: func(ctx context.Context, _ time.Duration) error {
				select {
				case <-timerElapsed:
				case <-ctx.Done():
					return ctx.Err()
				}
				select {
				case <-allowTimerReturn:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			},
			startWaiterQueued: func(priority bool) {
				if priority {
					close(priorityQueued)
				}
			},
		}),
	)

	done := make(chan struct{})
	var result *Result
	var runErr error
	go func() {
		defer close(done)
		result, runErr = o.RunBatch(context.Background(), Request{
			Issues:       []int{1, 2, 3},
			Branches:     map[int]string{1: "1-awaited", 2: "2-independent", 3: "3-dependent"},
			Dependencies: map[int][]int{3: {1}},
			Parallel:     1,
		})
	}()

	select {
	case <-independentStarted:
	case <-time.After(time.Second):
		t.Fatal("independent row did not use the released await capacity")
	}
	close(timerElapsed)
	close(allowTimerReturn)
	select {
	case <-priorityQueued:
	case <-time.After(time.Second):
		t.Fatalf("awaited row did not join the priority queue after its timer elapsed; starts=%v events=%v", factory.startsSnapshot(), log.snapshot())
	}
	close(allowIndependentFinish)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("batch did not finish")
	}
	if runErr != nil {
		t.Fatalf("run batch: %v", runErr)
	}
	if result == nil || len(result.Runs) != 3 {
		t.Fatalf("batch result = %#v, want three runs", result)
	}
	if got := countEventsByType(log.snapshot(), "run.await"); got == 0 {
		t.Fatal("expected the awaited row to emit run.await before resuming")
	}
	for _, run := range result.Runs {
		if run.Status != "success" {
			t.Fatalf("issue %d status = %q, want success", run.IssueNumber, run.Status)
		}
	}
	if got := factory.startsSnapshot(); !equalPriorityInts(got, []int{1, 2, 1, 3}) {
		t.Fatalf("start order = %v, want [1 2 1 3]", got)
	}
	if got := factory.maxActiveSnapshot(); got > 1 {
		t.Fatalf("peak active runs = %d, want at most 1", got)
	}
}

type awaitPriorityRunnableFactory struct {
	mu                     sync.Mutex
	starts                 []int
	active                 int
	maxActive              int
	independentStarted     chan struct{}
	allowIndependentFinish <-chan struct{}
}

func (f *awaitPriorityRunnableFactory) NewRunnable(issue *github.Issue, _ string, _ sandbox.Sandbox) Runnable {
	return &awaitPriorityRunnable{factory: f, issue: issue.Number}
}

type awaitPriorityRunnable struct {
	factory *awaitPriorityRunnableFactory
	issue   int
}

func (r *awaitPriorityRunnable) Run(ctx context.Context, _ prompt.IssueRenderer, _ string, _ prompt.RenderConfig) AgentRunResult {
	f := r.factory
	f.mu.Lock()
	f.starts = append(f.starts, r.issue)
	f.active++
	if f.active > f.maxActive {
		f.maxActive = f.active
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}()

	if r.issue == 2 {
		select {
		case <-f.independentStarted:
		default:
			close(f.independentStarted)
		}
		select {
		case <-f.allowIndependentFinish:
		case <-ctx.Done():
			return AgentRunResult{IssueNumber: r.issue, Status: "aborted"}
		}
	}
	return AgentRunResult{IssueNumber: r.issue, Status: "success"}
}

func (f *awaitPriorityRunnableFactory) startsSnapshot() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.starts...)
}

func (f *awaitPriorityRunnableFactory) maxActiveSnapshot() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxActive
}

func equalPriorityInts(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
