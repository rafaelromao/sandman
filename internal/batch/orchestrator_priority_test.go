package batch

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
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
			4: {Number: 4, Title: "Queued independent"},
		},
		prs: map[string]*github.PR{
			"2-independent": {Number: 2, State: "merged", Merged: true, Body: "Closes #2", HeadRefName: "2-independent"},
			"3-dependent":   {Number: 3, State: "merged", Merged: true, Body: "Closes #3", HeadRefName: "3-dependent"},
			"4-independent": {Number: 4, State: "merged", Merged: true, Body: "Closes #4", HeadRefName: "4-independent"},
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
					select {
					case <-priorityQueued:
					default:
						close(priorityQueued)
					}
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
			Issues:       []int{1, 2, 3, 4},
			Branches:     map[int]string{1: "1-awaited", 2: "2-independent", 3: "3-dependent", 4: "4-independent"},
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
		t.Fatalf("batch did not finish; starts=%v events=%v", factory.startsSnapshot(), log.snapshot())
	}
	if runErr != nil {
		t.Fatalf("run batch: %v", runErr)
	}
	if result == nil || len(result.Runs) != 4 {
		t.Fatalf("batch result = %#v, want four runs", result)
	}
	if got := countEventsByType(log.snapshot(), "run.await"); got == 0 {
		t.Fatal("expected the awaited row to emit run.await before resuming")
	}
	for _, run := range result.Runs {
		if run.Status != "success" {
			t.Fatalf("issue %d status = %q, want success", run.IssueNumber, run.Status)
		}
	}
	if got := factory.startsSnapshot(); len(got) != 5 || !equalPriorityInts(got[:3], []int{1, 2, 1}) || got[3]+got[4] != 7 || got[3] == got[4] {
		t.Fatalf("start order = %v, want awaited row [1 2 1] before remaining issues 3 and 4", got)
	}
	if got := factory.maxActiveSnapshot(); got > 1 {
		t.Fatalf("peak active runs = %d, want at most 1", got)
	}
}

func TestRunBatch_AwaitingRowDoesNotLetDependentBlockIndependentWork(t *testing.T) {
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
			"1-awaited":     {Number: 1, State: "open", Body: "Closes #1", HeadRefName: "1-awaited", StatusCheckRollup: "pending", MergeStateStatus: "BLOCKED"},
			"2-independent": {Number: 2, State: "merged", Merged: true, Body: "Closes #2", HeadRefName: "2-independent"},
		},
	}
	independentStarted := make(chan struct{})
	allowIndependentFinish := make(chan struct{})
	awaiting := make(chan struct{})
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
				case <-awaiting:
				default:
					close(awaiting)
				}
				<-ctx.Done()
				return ctx.Err()
			},
		}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = o.RunBatch(ctx, Request{
			Issues:       []int{1, 3, 2},
			Branches:     map[int]string{1: "1-awaited", 2: "2-independent", 3: "3-dependent"},
			Dependencies: map[int][]int{3: {1}},
			Parallel:     1,
		})
	}()

	select {
	case <-awaiting:
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("first row did not enter lifecycle await")
	}
	states := events.ProjectRunStates(log.snapshot())
	if len(states) == 0 || !states[0].IsAwaiting() || states[0].AwaitReason() != "pending" {
		cancel()
		<-done
		t.Fatalf("CI-pending row was not projected as waiting: %#v", states)
	}
	select {
	case <-independentStarted:
		close(allowIndependentFinish)
	case <-time.After(200 * time.Millisecond):
		cancel()
		<-done
		t.Fatal("dependent waiting on the awaited row blocked later independent work")
	}
	cancel()
	<-done
}

func TestRunBatch_RecentAwaitingRowsDoNotStarveQueuedWork(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "Awaited one"},
			2: {Number: 2, Title: "Awaited two"},
			3: {Number: 3, Title: "Awaited three"},
			4: {Number: 4, Title: "Queued work"},
		},
		prs: map[string]*github.PR{
			"1-awaited-one":   {Number: 1, State: "open", Body: "Closes #1", HeadRefName: "1-awaited-one", StatusCheckRollup: "pending", MergeStateStatus: "BLOCKED"},
			"2-awaited-two":   {Number: 2, State: "open", Body: "Closes #2", HeadRefName: "2-awaited-two", StatusCheckRollup: "pending", MergeStateStatus: "BLOCKED"},
			"3-awaited-three": {Number: 3, State: "open", Body: "Closes #3", HeadRefName: "3-awaited-three", StatusCheckRollup: "pending", MergeStateStatus: "BLOCKED"},
			"4-queued-work":   {Number: 4, State: "merged", Merged: true, Body: "Closes #4", HeadRefName: "4-queued-work"},
		},
	}
	ordinaryQueued := make(chan struct{})
	var ordinaryQueuedOnce sync.Once
	factory := &recentAwaitingRunnableFactory{queuedStarted: make(chan struct{})}
	log := &spyEventLog{}
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
				case <-ordinaryQueued:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			},
			startWaiterQueued: func(priority bool) {
				if !priority {
					ordinaryQueuedOnce.Do(func() { close(ordinaryQueued) })
				}
			},
		}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = o.RunBatch(ctx, Request{
			Issues:   []int{1, 2, 3, 4},
			Branches: map[int]string{1: "1-awaited-one", 2: "2-awaited-two", 3: "3-awaited-three", 4: "4-queued-work"},
			Parallel: 1,
		})
	}()

	select {
	case <-factory.queuedStarted:
		cancel()
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatalf("queued work starved; starts=%v events=%v", factory.startsSnapshot(), log.snapshot())
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("batch did not stop after cancellation")
	}

	starts := factory.startsSnapshot()
	if !containsPriorityInt(starts, 4) {
		t.Fatalf("queued work did not start; starts=%v", starts)
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

type recentAwaitingRunnableFactory struct {
	mu            sync.Mutex
	starts        []int
	queuedStarted chan struct{}
}

func (f *recentAwaitingRunnableFactory) NewRunnable(issue *github.Issue, _ string, _ sandbox.Sandbox) Runnable {
	return &recentAwaitingRunnable{factory: f, issue: issue.Number}
}

type recentAwaitingRunnable struct {
	factory *recentAwaitingRunnableFactory
	issue   int
}

func (r *recentAwaitingRunnable) Run(ctx context.Context, _ prompt.IssueRenderer, _ string, _ prompt.RenderConfig) AgentRunResult {
	if ctx.Err() != nil {
		return AgentRunResult{IssueNumber: r.issue, Status: "aborted"}
	}
	r.factory.mu.Lock()
	r.factory.starts = append(r.factory.starts, r.issue)
	if r.issue == 4 {
		select {
		case <-r.factory.queuedStarted:
		default:
			close(r.factory.queuedStarted)
		}
	}
	r.factory.mu.Unlock()
	return AgentRunResult{IssueNumber: r.issue, Status: "success"}
}

func (f *recentAwaitingRunnableFactory) startsSnapshot() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.starts...)
}

func containsPriorityInt(values []int, want int) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
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
