package batch

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

type reviewWaitSchedulerGitHubClient struct {
	fakeGitHubClient
	mu       sync.RWMutex
	comments []github.PRComment
}

func (c *reviewWaitSchedulerGitHubClient) FindPRByBranch(ctx context.Context, branch string) (*github.PR, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	pr, err := c.fakeGitHubClient.FindPRByBranch(ctx, branch)
	if pr == nil || err != nil {
		return pr, err
	}
	// Return a snapshot so callers can inspect PR facts after the lock is
	// released while another test actor advances the fake review lifecycle.
	prSnapshot := *pr
	return &prSnapshot, nil
}

func (c *reviewWaitSchedulerGitHubClient) ListPRComments(context.Context, int) ([]github.PRComment, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.comments == nil {
		return []github.PRComment{{
			ID:        "https://github.com/owner/repo/pull/17#issuecomment-1001",
			Body:      "/sandman review",
			CreatedAt: time.Now().UTC(),
		}}, nil
	}
	return append([]github.PRComment(nil), c.comments...), nil
}

func (c *reviewWaitSchedulerGitHubClient) setPR(branch string, mutate func(*github.PR)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	mutate(c.prs[branch])
}

type reviewWaitSchedulerSandboxFactory struct{}

func (reviewWaitSchedulerSandboxFactory) NewSandbox(_, worktreeBase, branch, _ string, _ sandbox.Container) sandbox.Sandbox {
	workDir := filepath.Join(worktreeBase, branch)
	_ = os.MkdirAll(filepath.Join(workDir, ".sandman"), 0o755)
	return &fakeSandbox{workDir: workDir}
}

type reviewWaitSchedulerRunnableFactory struct {
	client               *reviewWaitSchedulerGitHubClient
	allowIndependentDone <-chan struct{}
	independentStarted   chan struct{}
	dependentStarted     chan struct{}
	mu                   sync.Mutex
	launches             map[int]int
	starts               []int
}

func (f *reviewWaitSchedulerRunnableFactory) NewRunnable(issue *github.Issue, branch string, _ sandbox.Sandbox) Runnable {
	f.mu.Lock()
	if f.launches == nil {
		f.launches = make(map[int]int)
	}
	f.launches[issue.Number]++
	launch := f.launches[issue.Number]
	f.mu.Unlock()
	return &reviewWaitSchedulerRunnable{factory: f, issue: issue.Number, branch: branch, launch: launch}
}

func (f *reviewWaitSchedulerRunnableFactory) startsSnapshot() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.starts...)
}

type reviewWaitSchedulerRunnable struct {
	factory *reviewWaitSchedulerRunnableFactory
	issue   int
	branch  string
	launch  int
}

func (r *reviewWaitSchedulerRunnable) Run(ctx context.Context, _ prompt.IssueRenderer, _ string, _ prompt.RenderConfig) AgentRunResult {
	f := r.factory
	f.mu.Lock()
	f.starts = append(f.starts, r.issue)
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return AgentRunResult{IssueNumber: r.issue, Status: "aborted", Branch: r.branch}
	}
	switch {
	case r.issue == 1 && r.launch == 2:
		f.client.setPR(r.branch, func(pr *github.PR) {
			pr.State = "merged"
			pr.Merged = true
			pr.Body = "Closes #1"
		})
	case r.issue == 2:
		select {
		case <-f.independentStarted:
		default:
			close(f.independentStarted)
		}
		select {
		case <-f.allowIndependentDone:
		case <-ctx.Done():
			return AgentRunResult{IssueNumber: r.issue, Status: "aborted", Branch: r.branch}
		}
	case r.issue == 3:
		select {
		case <-f.dependentStarted:
		default:
			close(f.dependentStarted)
		}
	}
	return AgentRunResult{IssueNumber: r.issue, Status: "success", Branch: r.branch}
}

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
				{Number: 1, State: "open", Body: "Closes #1", HeadRefName: "1-awaited", HeadRefOid: "current-sha", StatusCheckRollup: "pending", MergeStateStatus: "BLOCKED"},
				{Number: 1, State: "open", Body: "Closes #1", HeadRefName: "1-awaited", HeadRefOid: "current-sha", StatusCheckRollup: "pending", MergeStateStatus: "BLOCKED"},
				{Number: 1, State: "open", Body: "Closes #1", HeadRefName: "1-awaited", HeadRefOid: "current-sha", StatusCheckRollup: "failure"},
				{Number: 1, State: "open", Body: "Closes #1", HeadRefName: "1-awaited", HeadRefOid: "current-sha", StatusCheckRollup: "failure"},
				{Number: 1, State: "open", Body: "Closes #1", HeadRefName: "1-awaited", HeadRefOid: "current-sha", StatusCheckRollup: "failure"},
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
		WithSandboxFactory(reviewWaitSchedulerSandboxFactory{}),
		WithRunnableFactory(factory),
		WithRunSessionOpts(runSessionOptions{
			releaseAwaitCapacity: true,
			currentHead:          func(string) (string, error) { return "current-sha", nil },
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
			"1-awaited":     {Number: 1, State: "open", Body: "Closes #1", HeadRefName: "1-awaited", HeadRefOid: "current-sha", StatusCheckRollup: "pending", MergeStateStatus: "BLOCKED"},
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
		WithSandboxFactory(reviewWaitSchedulerSandboxFactory{}),
		WithRunnableFactory(factory),
		WithRunSessionOpts(runSessionOptions{
			releaseAwaitCapacity: true,
			currentHead:          func(string) (string, error) { return "current-sha", nil },
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
			"1-awaited-one":   {Number: 1, State: "open", Body: "Closes #1", HeadRefName: "1-awaited-one", HeadRefOid: "current-sha", StatusCheckRollup: "pending", MergeStateStatus: "BLOCKED"},
			"2-awaited-two":   {Number: 2, State: "open", Body: "Closes #2", HeadRefName: "2-awaited-two", HeadRefOid: "current-sha", StatusCheckRollup: "pending", MergeStateStatus: "BLOCKED"},
			"3-awaited-three": {Number: 3, State: "open", Body: "Closes #3", HeadRefName: "3-awaited-three", HeadRefOid: "current-sha", StatusCheckRollup: "pending", MergeStateStatus: "BLOCKED"},
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
		WithSandboxFactory(reviewWaitSchedulerSandboxFactory{}),
		WithRunnableFactory(factory),
		WithRunSessionOpts(runSessionOptions{
			releaseAwaitCapacity: true,
			currentHead:          func(string) (string, error) { return "current-sha", nil },
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

// A confirmed review request is an ongoing external operation before the
// reviewer run starts. The implementation releases its slot, independent
// work starts while its dependent stays queued, and request-scoped approval
// resumes the same implementation once a slot is free (issue #2743).
func TestRunBatch_ConfirmedReviewReleasesSlotAndResumesAfterResponse(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	const (
		implBranch  = "1-awaiting-review"
		independent = "2-independent"
		dependent   = "3-dependent"
		currentHead = "current-sha"
		prNumber    = 17
	)
	client := &reviewWaitSchedulerGitHubClient{
		fakeGitHubClient: fakeGitHubClient{
			issues: map[int]*github.Issue{
				1: {Number: 1, Title: "Implementation"},
				2: {Number: 2, Title: "Independent"},
				3: {Number: 3, Title: "Dependent"},
			},
			prs: map[string]*github.PR{
				implBranch:  {Number: prNumber, State: "open", Body: "Closes #1", HeadRefName: implBranch, HeadRefOid: currentHead, StatusCheckRollup: "success", ReviewDecision: "REVIEW_REQUIRED", MergeStateStatus: "BLOCKED"},
				independent: {Number: 2, State: "merged", Merged: true, Body: "Closes #2", HeadRefName: independent},
				dependent:   {Number: 3, State: "merged", Merged: true, Body: "Closes #3", HeadRefName: dependent},
			},
		},
	}
	awaitEntered := make(chan struct{})
	responseReady := make(chan struct{})
	independentStarted := make(chan struct{})
	allowIndependentDone := make(chan struct{})
	dependentStarted := make(chan struct{})
	priorityQueued := make(chan struct{})
	factory := &reviewWaitSchedulerRunnableFactory{
		client:               client,
		allowIndependentDone: allowIndependentDone,
		independentStarted:   independentStarted,
		dependentStarted:     dependentStarted,
	}
	log := &spyEventLog{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{
		Agent:          "test-agent",
		Sandbox:        "worktree",
		WorktreeDir:    ".sandman/worktrees",
		Git:            config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}},
	}}, log,
		WithErrorLog(io.Discard),
		WithSandboxFactory(reviewWaitSchedulerSandboxFactory{}),
		WithRunnableFactory(factory),
		WithRunSessionOpts(runSessionOptions{
			releaseAwaitCapacity: true,
			currentHead:          func(string) (string, error) { return currentHead, nil },
			awaitWait: func(ctx context.Context, _ time.Duration) error {
				select {
				case <-awaitEntered:
				default:
					close(awaitEntered)
				}
				select {
				case <-responseReady:
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

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	var result *Result
	var runErr error
	go func() {
		defer close(done)
		result, runErr = o.RunBatch(ctx, Request{
			Issues:       []int{1, 3, 2},
			Branches:     map[int]string{1: implBranch, 2: independent, 3: dependent},
			Dependencies: map[int][]int{3: {1}},
			Parallel:     1,
			PromptConfig: prompt.RenderConfig{ReviewCommand: "/sandman review", ReviewTimeout: 1800},
		})
	}()

	select {
	case <-awaitEntered:
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		t.Fatalf("implementation did not yield on the confirmed review request: %v", log.snapshot())
	}
	select {
	case <-independentStarted:
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		t.Fatalf("independent work did not use the released slot: %v", factory.startsSnapshot())
	}
	states := events.ProjectRunStates(log.snapshot())
	var implAwaiting bool
	for _, state := range states {
		if state.IssueNumber() == 1 && state.IsAwaiting() {
			implAwaiting = true
			if got := state.AwaitReviewRequest(); got == nil {
				t.Fatalf("awaiting implementation has no confirmed request evidence: %#v", state.AwaitEvent.Payload)
			}
		}
	}
	if !implAwaiting {
		t.Fatalf("implementation was not projected waiting during delegated review: %#v", states)
	}
	select {
	case <-dependentStarted:
		t.Fatal("dependent work started before its awaited implementation completed")
	default:
	}

	implWorktree := filepath.Join(dir, ".sandman", "worktrees", implBranch)
	writeRespondedApprovalForCanonicalRequest(t, implWorktree, prNumber)
	client.setPR(implBranch, func(pr *github.PR) {
		pr.ReviewDecision = "APPROVED"
		pr.MergeStateStatus = "CLEAN"
	})
	close(responseReady)

	select {
	case <-priorityQueued:
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		t.Fatalf("approved implementation did not queue for the occupied slot: starts=%v events=%v", factory.startsSnapshot(), log.snapshot())
	}
	var readyState *events.RunState
	for _, state := range events.ProjectRunStates(log.snapshot()) {
		if state.IssueNumber() == 1 {
			copy := state
			readyState = &copy
			break
		}
	}
	if readyState == nil || !readyState.IsActive() || !readyState.IsAwaiting() || !readyState.IsCapacityQueued() || readyState.Status() != "waiting" {
		t.Fatalf("resolved continuation phase = %#v, want active waiting run with durable ready evidence", readyState)
	}
	if readyState.CapacityQueuedEvent == nil || readyState.CapacityQueuedEvent.Payload["ready_continuation"] != true {
		t.Fatalf("resolved continuation is not durably marked ready: %#v", readyState.CapacityQueuedEvent)
	}
	if starts := factory.startsSnapshot(); len(starts) != 2 || starts[0] != 1 || starts[1] != 2 {
		t.Fatalf("ready implementation ran before a slot freed: starts=%v", starts)
	}
	select {
	case <-dependentStarted:
		t.Fatal("dependent work started while implementation continuation waited for capacity")
	default:
	}
	close(allowIndependentDone)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("reviewed implementation did not resume and release its dependent after capacity became available")
	}
	if runErr != nil {
		t.Fatalf("run batch: %v; events=%v", runErr, log.snapshot())
	}
	if result == nil || len(result.Runs) != 3 {
		t.Fatalf("batch result = %#v, want three runs", result)
	}
	for _, run := range result.Runs {
		if run.Status != "success" {
			t.Fatalf("issue %d status = %q, want success", run.IssueNumber, run.Status)
		}
	}
	starts := factory.startsSnapshot()
	if len(starts) != 4 || starts[0] != 1 || starts[1] != 2 || starts[2] != 1 || starts[3] != 3 {
		t.Fatalf("start order = %v, want initial, independent, resumed implementation, dependent", starts)
	}
	logs := log.snapshot()
	if countEventsByType(logs, "run.await") != 1 {
		t.Fatalf("run.await events = %d, want exactly the delegated-review await", countEventsByType(logs, "run.await"))
	}
	if countEventsByType(logs, "run.continued") != 1 {
		t.Fatalf("run.continued events = %d, want one automatic review-completion continuation", countEventsByType(logs, "run.continued"))
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
