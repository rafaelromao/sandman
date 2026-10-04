package batch

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/testenv"
)

func TestWaitingContract_PendingReviewOnCleanPRDoesNotResume(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-wait-")
	t.Chdir(root)
	worktree := filepath.Join(root, "worktree")
	factory := &reviewRequestSeedingFactory{
		workDir: worktree, prNumber: 17, headSHA: "current-sha",
		results: []AgentRunResult{{IssueNumber: 42, Status: "success", Branch: gateTestBranch}},
	}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Waiting"}},
		prs: map[string]*github.PR{gateTestBranch: {
			Number: 17, State: "open", HeadRefName: gateTestBranch, HeadRefOid: "current-sha",
			StatusCheckRollup: "success", MergeStateStatus: "CLEAN",
		}},
	}
	log := &spyEventLog{}
	opts := gateTestRunOptions()
	opts.awaitResumeMax = 1
	sbFactory := &retrySandboxFactory{sandbox: &retrySandbox{workDir: worktree}}
	o := NewOrchestrator(client, &retryRenderer{result: "task"}, nil, log,
		WithErrorLog(io.Discard), WithSandboxFactory(sbFactory), WithRunnableFactory(factory), WithRunSessionOpts(opts))
	result, _ := o.newRunExecutor(context.Background(), BatchConfig{
		Cfg:      &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentCfg: config.Agent{Command: "true"}, IdentityResolver: noopIdentityResolver(), Retries: 0,
	}, sbFactory, nil).Execute(context.Background(), RowSpec{
		IssueNumber: 42, Branches: map[int]string{42: gateTestBranch}, BaseBranch: "main",
		RenderCfg: prompt.RenderConfig{ReviewCommand: "/sandman review"},
	})
	if factory.created != 1 || countEventsByType(log.snapshot(), "run.resumed") != 0 || result.Status != "await" {
		t.Fatalf("pending review: launches=%d resumes=%d status=%q; want one launch, await, and no resume before response", factory.created, countEventsByType(log.snapshot(), "run.resumed"), result.Status)
	}
}

func TestWaitingContract_ManagedCleanPRRequiresDelegatedApproval(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-wait-")
	t.Chdir(root)
	worktree := filepath.Join(root, "worktree")
	factory := &controlledRunnableFactory{runnables: map[int]Runnable{42: &controlledRunnable{
		result: AgentRunResult{IssueNumber: 42, Status: "success", Branch: gateTestBranch},
	}}}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Waiting"}},
		prs: map[string]*github.PR{gateTestBranch: {
			Number: 17, State: "open", HeadRefName: gateTestBranch, HeadRefOid: "current-sha",
			StatusCheckRollup: "success", ReviewDecision: "APPROVED", MergeStateStatus: "CLEAN",
		}},
	}
	log := &spyEventLog{}
	sbFactory := &retrySandboxFactory{sandbox: &retrySandbox{workDir: worktree}}
	o := NewOrchestrator(client, &retryRenderer{result: "task"}, nil, log,
		WithErrorLog(io.Discard), WithSandboxFactory(sbFactory), WithRunnableFactory(factory), WithRunSessionOpts(gateTestRunOptions()))
	result, _ := o.newRunExecutor(context.Background(), BatchConfig{
		Cfg:      &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentCfg: config.Agent{Command: "true"}, IdentityResolver: noopIdentityResolver(), Retries: 0,
	}, sbFactory, nil).Execute(context.Background(), RowSpec{
		IssueNumber: 42, Branches: map[int]string{42: gateTestBranch}, BaseBranch: "main",
		RenderCfg: prompt.RenderConfig{ReviewCommand: "/sandman review"},
	})
	if len(factory.created) != 1 || countEventsByType(log.snapshot(), "run.resumed") != 0 || result.Status != "failure" {
		t.Fatalf("missing delegated approval: launches=%d resumes=%d status=%q", len(factory.created), countEventsByType(log.snapshot(), "run.resumed"), result.Status)
	}
	finished := findEvent(log.snapshot(), "run.finished")
	if finished == nil || finished.Payload["reason"] != "REVIEW_REQUEST_REQUIRED" {
		t.Fatalf("missing-delivery outcome=%#v, want structured owned-work failure", finished)
	}
}

func TestWaitingContract_RepairBudgetSurvivesExecutorReentry(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-wait-")
	t.Chdir(root)
	worktree := filepath.Join(root, "worktree")
	factory := &controlledRunnableFactory{runnables: map[int]Runnable{42: &controlledRunnable{
		result: AgentRunResult{IssueNumber: 42, Status: "success", Branch: gateTestBranch},
	}}}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Waiting"}},
		prs: map[string]*github.PR{gateTestBranch: {
			Number: 17, State: "open", HeadRefName: gateTestBranch, HeadRefOid: "current-sha",
			StatusCheckRollup: "failure", MergeStateStatus: "BLOCKED",
		}},
	}
	log := &spyEventLog{}
	opts := gateTestRunOptions()
	opts.awaitResumeMax = 1
	sbFactory := &retrySandboxFactory{sandbox: &retrySandbox{workDir: worktree}}
	o := NewOrchestrator(client, &retryRenderer{result: "task"}, nil, log,
		WithErrorLog(io.Discard), WithSandboxFactory(sbFactory), WithRunnableFactory(factory), WithRunSessionOpts(opts))
	bc := BatchConfig{
		Cfg:      &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentCfg: config.Agent{Command: "true"}, IdentityResolver: noopIdentityResolver(), Retries: 0,
	}
	for _, runID := range []string{"budget-first", "budget-second"} {
		result, _ := o.newRunExecutor(context.Background(), bc, sbFactory, nil).Execute(context.Background(), RowSpec{
			IssueNumber: 42, Mode: ModeContinue, RunID: runID,
			Branches: map[int]string{42: gateTestBranch}, BaseBranch: "main",
		})
		if result.Status != "failure" {
			t.Fatalf("exhausted repair status=%q", result.Status)
		}
	}
	if len(factory.created) != 1 {
		t.Fatalf("same-head repair launches across executor re-entry=%d, want one durable allowed attempt", len(factory.created))
	}
}

type waitingTerminalLog struct {
	spyEventLog
	finished chan struct{}
}

func (l *waitingTerminalLog) Log(event events.Event) error {
	err := l.spyEventLog.Log(event)
	if event.Issue == 42 && event.Type == "run.finished" {
		close(l.finished)
	}
	return err
}

func TestWaitingContract_ObservedMergeFinishesWithoutExecutionSlot(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-wait-")
	t.Chdir(root)
	initGitRepo(t, root)
	client := &reviewWaitSchedulerGitHubClient{fakeGitHubClient: fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open"}, 43: {Number: 43, State: "closed"}},
		prs: map[string]*github.PR{
			"42-await": {Number: 17, State: "open", Body: "Closes #42", HeadRefName: "42-await", HeadRefOid: "current-sha", StatusCheckRollup: "pending", MergeStateStatus: "BLOCKED"},
			"43-busy":  {Number: 43, State: "merged", Merged: true, Body: "Closes #43", HeadRefName: "43-busy"},
		},
	}}
	busyStarted := make(chan struct{})
	releaseBusy := make(chan struct{})
	factory := &controlledRunnableFactory{runnables: map[int]Runnable{
		42: &controlledRunnable{result: AgentRunResult{IssueNumber: 42, Status: "success", Branch: "42-await"}},
		43: &controlledRunnable{started: busyStarted, release: releaseBusy, result: AgentRunResult{IssueNumber: 43, Status: "success", Branch: "43-busy"}},
	}}
	log := &waitingTerminalLog{finished: make(chan struct{})}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{
		Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}},
	}}, log, WithErrorLog(io.Discard), WithRunnableFactory(factory), WithSandboxFactory(reviewWaitSchedulerSandboxFactory{}),
		WithRunSessionOpts(runSessionOptions{
			releaseAwaitCapacity: true, currentHead: func(string) (string, error) { return "current-sha", nil },
			awaitWait: func(ctx context.Context, _ time.Duration) error {
				select {
				case <-busyStarted:
					client.setPR("42-await", func(pr *github.PR) { pr.State, pr.Merged = "merged", true })
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			},
		}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = o.RunBatch(ctx, Request{Issues: []int{42, 43}, Branches: map[int]string{42: "42-await", 43: "43-busy"}, Parallel: 1})
	}()
	finishedWithoutSlot := false
	select {
	case <-log.finished:
		finishedWithoutSlot = true
	case <-time.After(3 * time.Second):
	}
	close(releaseBusy)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("batch did not finish")
	}
	if !finishedWithoutSlot {
		t.Fatal("verified merged continuation waited for an unrelated execution slot")
	}
}

func TestWaitingContract_CancellationDoesNotPrepareAnotherRetry(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-wait-")
	t.Chdir(root)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	launches := 0
	factory := &promptOnlyRunnableFactory{hook: func(issue *github.Issue, branch string) AgentRunResult {
		launches++
		cancel()
		return AgentRunResult{IssueNumber: issue.Number, Status: "failure", Branch: branch}
	}}
	log := &spyEventLog{}
	sbFactory := &retrySandboxFactory{sandbox: &retrySandbox{workDir: filepath.Join(root, "worktree")}}
	o := NewOrchestrator(&fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open"}}},
		&retryRenderer{result: "task"}, nil, log,
		WithErrorLog(io.Discard), WithSandboxFactory(sbFactory), WithRunnableFactory(factory), WithRunSessionOpts(gateTestRunOptions()))
	result, _ := o.newRunExecutor(ctx, BatchConfig{
		Cfg:      &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentCfg: config.Agent{Command: "true"}, IdentityResolver: noopIdentityResolver(), Retries: 3,
	}, sbFactory, nil).Execute(ctx, RowSpec{IssueNumber: 42, Branches: map[int]string{42: gateTestBranch}, BaseBranch: "main"})
	if launches != 1 || countEventsByType(log.snapshot(), "run.retry") != 0 || result.Status != "aborted" {
		t.Fatalf("cancel: launches=%d retries=%d status=%q", launches, countEventsByType(log.snapshot(), "run.retry"), result.Status)
	}
}

func TestWaitingContract_ObservationErrorKeepsOnlyEstablishedDeadline(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-wait-")
	t.Chdir(root)
	now := time.Now().UTC()
	for _, established := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-operation", true: "established-CI"}[established], func(t *testing.T) {
			log := &spyEventLog{}
			if established {
				log.events = []events.Event{
					{Type: "run.started", RunID: "row", Issue: 42, Payload: map[string]any{"branch": gateTestBranch}},
					{Type: "run.await", RunID: "row", Issue: 42, Payload: map[string]any{
						"await_reason": "pending", "branch": gateTestBranch, "ci_wait": map[string]any{
							"protocol": ciWaitProtocol, "pull_request": 17, "head_sha": "current-sha",
							"deadline_unix_seconds": now.Add(time.Minute).Unix(),
							"started_unix_seconds":  now.Add(-29 * time.Minute).Unix(), "effective_timeout_seconds": 1800,
						},
					}},
				}
			}
			s := &runSession{
				deps:        runDeps{eventLog: log, githubClient: &fakeGitHubClient{findPRErr: errors.New("temporary transport failure")}, errorLog: io.Discard},
				issueNumber: 42, issueState: "open", runID: "row",
				opts: runSessionOptions{currentHead: func(string) (string, error) { return "current-sha", nil }},
			}
			status, extras, _ := s.handleLifecycleDecision(context.Background(), root, gateTestBranch, "", "row", true)
			want := "failure"
			if established {
				want = "await"
			}
			if status != want {
				t.Fatalf("established=%v observation=%q, want %s", established, status, want)
			}
			if established {
				deadline, _, ok := lifecycleDeadline(extras)
				if !ok || deadline.Unix() != now.Add(time.Minute).Unix() {
					t.Fatalf("observation renewed deadline: %#v", extras)
				}
			}
		})
	}
}

func TestWaitingContract_MissingPublicationUsesConfiguredRetryBudget(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-wait-")
	t.Chdir(root)
	worktree := filepath.Join(root, "worktree")
	if err := os.MkdirAll(filepath.Join(worktree, ".sandman"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".sandman", "task.md"), []byte("# Task\n\nPublish the implementation.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	factory := &controlledRunnableFactory{runnables: map[int]Runnable{42: &controlledRunnable{
		result: AgentRunResult{IssueNumber: 42, Status: "success", Branch: gateTestBranch},
	}}}
	log := &spyEventLog{}
	sbFactory := &retrySandboxFactory{sandbox: &retrySandbox{workDir: worktree}}
	o := NewOrchestrator(&fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open"}}},
		&retryRenderer{result: "task"}, nil, log, WithErrorLog(io.Discard), WithSandboxFactory(sbFactory), WithRunnableFactory(factory), WithRunSessionOpts(gateTestRunOptions()))
	result, _ := o.newRunExecutor(context.Background(), BatchConfig{
		Cfg:      &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentCfg: config.Agent{Command: "true"}, IdentityResolver: noopIdentityResolver(), Retries: 1,
	}, sbFactory, nil).Execute(context.Background(), RowSpec{IssueNumber: 42, Branches: map[int]string{42: gateTestBranch}, BaseBranch: "main"})
	if len(factory.created) != 2 || result.Status != "failure" || countEventsByType(log.snapshot(), "run.await") != 0 {
		t.Fatalf("publication: launches=%d status=%q events=%v", len(factory.created), result.Status, log.snapshot())
	}
	finished := findEvent(log.snapshot(), "run.finished")
	if finished == nil || finished.Payload["reason"] != missingPRReason || finished.Payload["retries_done"] != 1 {
		t.Fatalf("publication budget outcome=%#v", finished)
	}
}
