package batch

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/reviewlaunch"
	"github.com/rafaelromao/sandman/internal/sandbox"
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

func TestWaitingContract_QuotaProbeWithoutDeadlineFailsClosed(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	log := &spyEventLog{}
	factory := &controlledRunnableFactory{runnables: map[int]Runnable{}}
	o := NewOrchestrator(&fakeGitHubClient{}, &noopRenderer{}, nil, log,
		WithErrorLog(io.Discard), WithRunnableFactory(factory))
	result, started := o.newRunExecutor(context.Background(), BatchConfig{}, nil, nil).Execute(context.Background(), RowSpec{
		IssueNumber: 42, RunID: "missing-quota", UsageLimitProbe: true,
	})
	if started || result.Status != "failure" || len(factory.created) != 0 {
		t.Fatalf("invalid recovery launched: result=%+v started=%v launches=%v", result, started, factory.created)
	}
	finished := findEvent(log.snapshot(), "run.finished")
	if finished == nil || finished.Payload["reason"] != "QUOTA_RECOVERY_STATE_ERROR" {
		t.Fatalf("missing structured recovery failure: %+v", finished)
	}
}

func TestWaitingContract_CancelledRemediationPreservesBudget(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &runSession{}
	err := s.reserveRemediation(ctx, root, map[string]any{"gate": gateReadyToMerge, "pull_request": 17, "head_sha": "head"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("reservation error=%v, want cancellation", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".sandman", "state", "17.lifecycle-budget.json")); !os.IsNotExist(err) {
		t.Fatalf("cancelled reservation changed budget: %v", err)
	}
}

func TestWaitingContract_CancelledQuotaOwnerWakesSiblings(t *testing.T) {
	gate := newBatchQuotaGate()
	gate.report(42, AgentRunResult{Status: "await", UsageLimitReached: true}, false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- gate.wait(ctx) }()
	gate.retire(42)
	if err := <-done; err != nil {
		t.Fatalf("cancelled quota owner must wake remaining owned work to re-test availability: %v", err)
	}
}

func TestWaitingContract_FailedQuotaProbeCannotReopenAdmission(t *testing.T) {
	gate := newBatchQuotaGate()
	gate.report(42, AgentRunResult{Status: "await", UsageLimitReached: true}, false)
	gate.report(42, AgentRunResult{Status: "failure"}, true)
	gate.retire(42)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := gate.wait(ctx); !errors.Is(err, errQuotaUnavailable) {
		t.Fatalf("failed probe authorized unverified quota recovery: %v", err)
	}
}

func TestWaitingOwnerNilLogPreservesQuotaSchedule(t *testing.T) {
	root := t.TempDir()
	batchDir := filepath.Join(root, "batches", "batch")
	now := time.Now().UTC()
	owner, err := newWaitOwner(batchDir, daemon.RunWait{Protocol: "run-wait/v1", RunID: "row", BatchID: "batch", Issue: 42, Branch: "42-work", BaseBranch: "main", InitialAdmission: true, OperationID: "admission"}, func() time.Time { return now }, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.close()
	deadline := now.Add(5 * time.Hour)
	if err := owner.checkpoint(RowSpec{RunID: "row", IssueNumber: 42, Branches: map[int]string{42: "42-work"}, UsageLimitProbe: true, UsageLimitDeadline: deadline}, false, usageLimitPollInterval); err != nil {
		t.Fatal(err)
	}
	record, err := daemon.ReadRunWait(batchDir, "row")
	if err != nil || record.InitialAdmission || !record.UsageLimitProbe || !record.OperationDeadline.Equal(deadline) {
		t.Fatalf("nil logger lost runtime-derived quota schedule: %+v err=%v", record, err)
	}
}

func TestWaitingContract_ReviewerLaunchExhaustionIsRequestScoped(t *testing.T) {
	layout := paths.NewLayout(nil, t.TempDir())
	for i := 0; i < 3; i++ {
		if _, err := reviewlaunch.RecordFailure(layout.StateDir, 17, "request", "head"); err != nil {
			t.Fatal(err)
		}
	}
	s := &runSession{deps: runDeps{layout: layout}}
	for _, tc := range []struct {
		trigger, head string
		want          bool
	}{{"request", "head", true}, {"new-request", "head", false}, {"request", "new-head", false}} {
		exhausted, err := s.exhaustedReviewLaunch(map[string]any{"review_request": map[string]any{"trigger_id": tc.trigger, "head_sha": tc.head, "pull_request": 17}}, 17, tc.head)
		if err != nil || exhausted != tc.want {
			t.Fatalf("request %s/%s exhaustion=%v error=%v, want %v", tc.trigger, tc.head, exhausted, err, tc.want)
		}
	}
}

func TestWaitingContract_AdmissionToAwaitPreservesOperationAfterCrash(t *testing.T) {
	root := t.TempDir()
	layout := paths.NewLayout(nil, root)
	now := time.Now().UTC()
	log := &spyEventLog{events: []events.Event{
		{Type: "run.started", RunID: "row", Issue: 42, Timestamp: now.Add(-2 * time.Minute), Payload: map[string]any{"branch": "42-fix", "batch_id": "batch"}},
		{Type: "run.capacity_queued", RunID: "row", Issue: 42, Timestamp: now.Add(-time.Minute), Payload: map[string]any{"ready_continuation": true, "branch": "42-fix", "base_branch": "main", "batch_id": "batch"}},
	}}
	claim, err := daemon.ClaimRun(layout.SandmanDir, "row")
	if err != nil {
		t.Fatal(err)
	}
	defer claim.Close()
	owner, err := newWaitOwner(layout.BatchDir("batch"), daemon.RunWait{Protocol: "run-wait/v1", RunID: "row", BatchID: "batch", Issue: 42, Branch: "42-fix", BaseBranch: "main", Ready: true, OperationID: "capacity"}, func() time.Time { return now }, log)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.close()
	e := &runExecutor{deps: runDeps{eventLog: log, layout: layout, runSessionOpts: runSessionOptions{now: func() time.Time { return now }}}}
	row := RowSpec{RunID: "row", IssueNumber: 42, BatchID: "batch", BaseBranch: "main", Branches: map[int]string{42: "42-fix"}}
	session := newRunSession(e, row)
	evidence, err := session.ciWaitEvidence(root, &github.PR{Number: 17, HeadRefOid: "head"}, "head")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.persistObservedAwait(context.Background(), row, evidence); err != nil {
		t.Fatal(err)
	}
	if err := owner.checkpoint(row, false, time.Minute); err != nil {
		t.Fatal(err)
	}
	owner.close()
	if err := claim.Close(); err != nil {
		t.Fatal(err)
	}
	ready := FindReadyContinuations(log.snapshot(), layout)
	if len(ready) != 1 || ready[0].Wait == nil || ready[0].Wait.Ready || !ready[0].Wait.OperationDeadline.Equal(now.Add(30*time.Minute).Truncate(time.Second)) {
		t.Fatalf("new CI operation lost across admission/crash: %+v", ready)
	}
	status, _, handled := session.priorObservation("row", "42-fix", "head")
	if !handled || status != "await" {
		t.Fatalf("established CI operation could not survive transient observation: %q handled=%v", status, handled)
	}
}

type dependencyClosureClient struct {
	*fakeGitHubClient
	parentFinished atomic.Bool
	closureState   string
	closureError   bool
}

func (c *dependencyClosureClient) FetchIssue(ctx context.Context, issue int) (*github.Issue, error) {
	if issue == 42 && c.parentFinished.Load() {
		if c.closureError {
			return nil, errors.New("dependency closure observation unavailable")
		}
		return &github.Issue{Number: 42, State: c.closureState}, nil
	}
	return c.fakeGitHubClient.FetchIssue(ctx, issue)
}

type dependencyClosureLog struct {
	spyEventLog
	parentFinished func()
}

func (l *dependencyClosureLog) Log(event events.Event) error {
	err := l.spyEventLog.Log(event)
	if err == nil && event.Issue == 42 && event.Type == "run.finished" && event.Payload["status"] == "success" {
		l.parentFinished()
	}
	return err
}

func TestWaitingContract_DependencyAdmissionRequiresVerifiedClosure(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		readError   bool
	}{{"unknown", "", false}, {"open", "open", false}, {"read-error", "closed", true}} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Chdir(root)
			initGitRepo(t, root)
			client := &dependencyClosureClient{fakeGitHubClient: &fakeGitHubClient{
				issues: map[int]*github.Issue{42: {Number: 42, State: "closed"}, 43: {Number: 43, State: "closed"}},
				prs:    map[string]*github.PR{"42-parent": {Number: 17, State: "closed", Merged: true, HeadRefName: "42-parent", HeadRefOid: "head", Body: "Closes #42"}},
			}, closureState: tc.state, closureError: tc.readError}
			log := &dependencyClosureLog{parentFinished: func() { client.parentFinished.Store(true) }}
			factory := &controlledRunnableFactory{runnables: map[int]Runnable{42: &controlledRunnable{result: AgentRunResult{Status: "success", Branch: "42-parent"}}, 43: &controlledRunnable{result: AgentRunResult{Status: "success"}}}}
			o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", AgentProviders: map[string]config.Agent{"test": {Command: "true"}}}}, log,
				WithErrorLog(io.Discard), WithSandboxFactory(reviewWaitSchedulerSandboxFactory{}), WithRunnableFactory(factory), WithRunSessionOpts(runSessionOptions{currentHead: func(string) (string, error) { return "head", nil }}))
			result, err := o.RunBatch(context.Background(), Request{Issues: []int{42, 43}, Branches: map[int]string{42: "42-parent", 43: "43-child"}, Dependencies: map[int][]int{43: {42}}})
			if err != nil || result == nil || len(result.Runs) != 2 || result.Runs[0].Status != "success" || result.Runs[1].Status != "blocked" || !equalPriorityInts(factory.created, []int{42}) {
				t.Fatalf("unverified dependency closure admitted work: result=%+v err=%v launches=%v", result, err, factory.created)
			}
		})
	}
}

func TestWaitingContract_DependentInitialAdmissionSurvivesReturnedAwait(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	initGitRepo(t, root)
	cfg := &config.Config{Agent: "test", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", AgentProviders: map[string]config.Agent{"test": {Command: "true"}}}
	parentTask := filepath.Join(root, cfg.WorktreeDir, "42-parent", ".sandman", "task.md")
	if err := os.MkdirAll(filepath.Dir(parentTask), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parentTask, []byte("# Task\nPreserved parent work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open"}, 43: {Number: 43, State: "open"}}, prs: map[string]*github.PR{"42-parent": {Number: 17, State: "open", HeadRefName: "42-parent", HeadRefOid: "head", StatusCheckRollup: "pending"}}}
	log := &spyEventLog{}
	factory := &controlledRunnableFactory{runnables: map[int]Runnable{42: &controlledRunnable{result: AgentRunResult{Status: "success", Branch: "42-parent"}}, 43: &controlledRunnable{result: AgentRunResult{Status: "success"}}}}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: cfg}, log,
		WithErrorLog(io.Discard), WithSandboxFactory(reviewWaitSchedulerSandboxFactory{}), WithRunnableFactory(factory), WithRunSessionOpts(runSessionOptions{currentHead: func(string) (string, error) { return "head", nil }}))
	result, err := o.RunBatch(context.Background(), Request{Issues: []int{42, 43}, RunTS: "261004120000", RunShortID: "deps", Branches: map[int]string{42: "42-parent", 43: "43-child"}, Dependencies: map[int][]int{43: {42}}})
	if err != nil || result == nil || result.Runs[0].Status != "await" || result.Runs[1].Status != "queued" || !equalPriorityInts(factory.created, []int{42}) {
		t.Fatalf("unfinished parent admitted dependent: result=%+v err=%v launches=%v", result, err, factory.created)
	}
	ready := FindReadyContinuations(log.snapshot(), paths.NewLayout(cfg, root))
	request := Request{}
	if len(ready) != 2 {
		t.Fatalf("returned await lost owned parent/dependent intent: %+v", ready)
	}
	if err := ApplyReadyContinuations(&request, ready, paths.NewLayout(cfg, root), 1800); err != nil {
		t.Fatal(err)
	}
	if !equalPriorityInts(request.Dependencies[43], []int{42}) || request.IssueMode(43) != ModeFresh {
		t.Fatalf("recovery lost initial dependency edge: %+v", request)
	}
}

type cancelOnAwaitLog struct {
	spyEventLog
	cancel context.CancelFunc
}

func (l *cancelOnAwaitLog) Log(event events.Event) error {
	err := l.spyEventLog.Log(event)
	if event.Type == "run.await" {
		l.cancel()
	}
	return err
}

func TestWaitingContract_LateBatchAbortRevokesReturnedAwait(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	initGitRepo(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := &cancelOnAwaitLog{cancel: cancel}
	cfg := &config.Config{Agent: "test", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", AgentProviders: map[string]config.Agent{"test": {Command: "true"}}}
	client := &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open"}}, prs: map[string]*github.PR{"42-parent": {Number: 17, State: "open", HeadRefName: "42-parent", HeadRefOid: "head", StatusCheckRollup: "pending"}}}
	factory := &controlledRunnableFactory{runnables: map[int]Runnable{42: &controlledRunnable{result: AgentRunResult{IssueNumber: 42, Status: "success", Branch: "42-parent"}}}}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: cfg}, log,
		WithErrorLog(io.Discard), WithSandboxFactory(reviewWaitSchedulerSandboxFactory{}), WithRunnableFactory(factory), WithRunSessionOpts(runSessionOptions{currentHead: func(string) (string, error) { return "head", nil }}))
	result, _ := o.RunBatch(ctx, Request{Issues: []int{42}, RunTS: "261004120000", RunShortID: "late", Branches: map[int]string{42: "42-parent"}})
	if result == nil || result.Runs[0].Status != "aborted" || countEventsByType(log.snapshot(), "run.retry") != 0 || !equalPriorityInts(factory.created, []int{42}) {
		t.Fatalf("late cancellation retained await intent: result=%+v launches=%v events=%v", result, factory.created, log.snapshot())
	}
	states := events.ProjectRunStates(log.snapshot())
	if len(states) != 1 || states[0].Status() != "aborted" || !states[0].IsTerminal() || len(FindReadyContinuations(log.snapshot(), paths.NewLayout(cfg, root))) != 0 {
		t.Fatalf("explicit cancellation became recoverable ownerless work: %+v", states)
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

func TestWaitingContract_QuotaRecoveryReadmitsSibling(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-wait-")
	t.Chdir(root)
	initGitRepo(t, root)
	client := &reviewWaitSchedulerGitHubClient{fakeGitHubClient: fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "closed"}, 43: {Number: 43, State: "closed"}},
		prs:    map[string]*github.PR{"43-sibling": {Number: 43, State: "merged", Merged: true, Body: "Closes #43"}},
	}}
	var starts []int
	var hookMu sync.Mutex
	firstAttempts := 0
	factory := &promptOnlyRunnableFactory{hook: func(issue *github.Issue, branch string) AgentRunResult {
		hookMu.Lock()
		defer hookMu.Unlock()
		starts = append(starts, issue.Number)
		if issue.Number == 42 {
			firstAttempts++
			if firstAttempts == 1 {
				return AgentRunResult{IssueNumber: 42, Branch: branch, Status: "failure", UsageLimitReached: true}
			}
			client.mu.Lock()
			client.prs[branch] = &github.PR{Number: 42, State: "merged", Merged: true, Body: "Closes #42", HeadRefName: branch}
			client.mu.Unlock()
		}
		return AgentRunResult{IssueNumber: issue.Number, Branch: branch, Status: "success"}
	}}
	cfg := &config.Config{
		Agent: "opencode", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{"opencode": config.BuiltInAgentPresets["opencode"].Agent("opencode")},
	}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: cfg}, &spyEventLog{},
		WithErrorLog(io.Discard), WithRunnableFactory(factory), WithSandboxFactory(&freshSandboxFactory{}),
		WithRunSessionOpts(runSessionOptions{releaseAwaitCapacity: true, awaitWait: func(context.Context, time.Duration) error { return nil }}))
	result, err := o.RunBatch(context.Background(), Request{Issues: []int{42, 43}, Branches: map[int]string{42: "42-first", 43: "43-sibling"}, Parallel: 2, Retries: 0})
	if err != nil || result == nil || result.Runs[1].Status != "success" {
		t.Fatalf("quota recovery: starts=%v result=%v err=%v; sibling must be readmitted", starts, result, err)
	}
}

type quotaParallelLog struct {
	spyEventLog
	limited, deferred    chan struct{}
	limitOnce, deferOnce sync.Once
}

func (l *quotaParallelLog) Log(event events.Event) error {
	err := l.spyEventLog.Log(event)
	if event.Issue == 42 && event.Type == "run.await" {
		l.limitOnce.Do(func() { close(l.limited) })
	}
	if event.Issue == 43 && event.Type == "run.capacity_queued" {
		l.deferOnce.Do(func() { close(l.deferred) })
	}
	return err
}

type waitingRunnableFunction func(context.Context) AgentRunResult

func (f waitingRunnableFunction) Run(ctx context.Context, _ prompt.IssueRenderer, _ string, _ prompt.RenderConfig) AgentRunResult {
	return f(ctx)
}

func TestWaitingContract_ParallelQuotaRecoveryReadmitsDeferredSibling(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-par-quota-")
	t.Chdir(root)
	initGitRepo(t, root)
	log := &quotaParallelLog{limited: make(chan struct{}), deferred: make(chan struct{})}
	client := &reviewWaitSchedulerGitHubClient{fakeGitHubClient: fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "closed"}, 43: {Number: 43, State: "closed"}, 44: {Number: 44, State: "closed"}}, prs: map[string]*github.PR{
		"43-next": {Number: 43, State: "merged", Merged: true, HeadRefName: "43-next", Body: "Closes #43"}, "44-busy": {Number: 44, State: "merged", Merged: true, HeadRefName: "44-busy", Body: "Closes #44"},
	}}}
	var attempts atomic.Int32
	factory := &controlledRunnableFactory{runnables: map[int]Runnable{
		44: waitingRunnableFunction(func(ctx context.Context) AgentRunResult {
			select {
			case <-log.limited:
			case <-ctx.Done():
				return AgentRunResult{Status: "aborted"}
			}
			return AgentRunResult{IssueNumber: 44, Status: "success", Branch: "44-busy"}
		}),
		42: waitingRunnableFunction(func(context.Context) AgentRunResult {
			if attempts.Add(1) == 1 {
				return AgentRunResult{IssueNumber: 42, Status: "failure", Branch: "42-limit", UsageLimitReached: true}
			}
			client.mu.Lock()
			client.prs["42-limit"] = &github.PR{Number: 42, State: "merged", Merged: true, HeadRefName: "42-limit", Body: "Closes #42"}
			client.mu.Unlock()
			return AgentRunResult{IssueNumber: 42, Status: "success", Branch: "42-limit"}
		}),
		43: waitingRunnableFunction(func(context.Context) AgentRunResult {
			return AgentRunResult{IssueNumber: 43, Status: "success", Branch: "43-next"}
		}),
	}}
	cfg := &config.Config{Agent: "opencode", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", AgentProviders: map[string]config.Agent{"opencode": config.BuiltInAgentPresets["opencode"].Agent("opencode")}}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: cfg}, log, WithErrorLog(io.Discard), WithRunnableFactory(factory), WithSandboxFactory(reviewWaitSchedulerSandboxFactory{}), WithRunSessionOpts(runSessionOptions{releaseAwaitCapacity: true, awaitWait: func(ctx context.Context, _ time.Duration) error {
		select {
		case <-log.deferred:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := o.RunBatch(ctx, Request{Issues: []int{44, 42, 43}, RunTS: "261005120000", RunShortID: "parallel", Parallel: 2, Branches: map[int]string{44: "44-busy", 42: "42-limit", 43: "43-next"}, Dependencies: map[int][]int{43: {44}}})
	if err != nil || result == nil || attempts.Load() != 2 {
		t.Fatalf("parallel recovery failed: result=%+v err=%v attempts=%d", result, err, attempts.Load())
	}
	for _, run := range result.Runs {
		if run.Status != "success" {
			t.Fatalf("unexpired deferred row failed instead of readmission: %+v", run)
		}
	}
	select {
	case <-log.deferred:
	default:
		t.Fatal("sibling never experienced quota deferral")
	}
}

type waitingCancelLog struct {
	spyEventLog
	cancel context.CancelFunc
}

func (l *waitingCancelLog) Log(event events.Event) error {
	err := l.spyEventLog.Log(event)
	if event.Issue == 43 && event.Type == "run.capacity_queued" {
		l.cancel()
	}
	return err
}

func TestWaitingContract_BatchAbortReachesQuotaDeferredRows(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-wait-")
	t.Chdir(root)
	initGitRepo(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := &waitingCancelLog{cancel: cancel}
	client := &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42}, 43: {Number: 43}, 44: {Number: 44}}}
	factory := &controlledRunnableFactory{runnables: map[int]Runnable{
		42: &controlledRunnable{result: AgentRunResult{IssueNumber: 42, Branch: "42-limit", Status: "failure", UsageLimitReached: true}},
	}}
	cfg := &config.Config{Agent: "opencode", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{"opencode": config.BuiltInAgentPresets["opencode"].Agent("opencode")}}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: cfg}, log,
		WithErrorLog(io.Discard), WithRunnableFactory(factory), WithSandboxFactory(&freshSandboxFactory{}),
		WithRunSessionOpts(runSessionOptions{releaseAwaitCapacity: true, awaitWait: func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		}}))
	result, err := o.RunBatch(ctx, Request{Issues: []int{42, 43, 44}, Branches: map[int]string{42: "42-limit", 43: "43-next", 44: "44-last"}, Parallel: 1})
	if result == nil || !errors.Is(err, ErrAborted) {
		t.Fatalf("batch abort result=%v error=%v", result, err)
	}
	for _, run := range result.Runs {
		if run.Status != "aborted" {
			t.Fatalf("unfinished issue %d survived abort as %q", run.IssueNumber, run.Status)
		}
	}
	for _, state := range events.ProjectRunStates(log.snapshot()) {
		if !state.IsTerminal() || state.Status() != "aborted" {
			t.Fatalf("aborted batch left live intent: %#v", state)
		}
	}
	if len(factory.created) != 1 || countEventsByType(log.snapshot(), "run.retry") != 0 {
		t.Fatalf("abort spent retries/launched deferred work: starts=%v events=%v", factory.created, log.snapshot())
	}
}

type waitingRaceFactory struct {
	mu        sync.Mutex
	runnables []Runnable
	created   int
}

func (f *waitingRaceFactory) NewRunnable(*github.Issue, string, sandbox.Sandbox) Runnable {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.runnables[f.created]
	f.created++
	return r
}

func TestWaitingContract_QuotaRevalidatedAfterStartGate(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-wait-")
	t.Chdir(root)
	initGitRepo(t, root)
	client := &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "closed"}, 43: {Number: 43, State: "closed"}, 44: {Number: 44, State: "closed"}}}
	releaseLimit, releaseBusy := make(chan struct{}), make(chan struct{})
	limitedStarted, busyStarted := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	factory := &waitingRaceFactory{runnables: []Runnable{
		&controlledRunnable{started: limitedStarted, release: releaseLimit, result: AgentRunResult{Status: "failure", UsageLimitReached: true}},
		&controlledRunnable{started: busyStarted, release: releaseBusy, result: AgentRunResult{Status: "success"}},
		&controlledRunnable{result: AgentRunResult{Status: "success"}},
	}}
	log := &spyEventLog{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{
		Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}},
	}}, log, WithErrorLog(io.Discard), WithRunnableFactory(factory), WithSandboxFactory(&freshSandboxFactory{}),
		WithRunSessionOpts(runSessionOptions{startWaiterQueued: func(bool) {
			<-limitedStarted
			<-busyStarted
			releaseOnce.Do(func() { close(releaseLimit) })
		}}))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = o.RunBatch(context.Background(), Request{Issues: []int{42, 43, 44}, Branches: map[int]string{42: "42-limit", 43: "43-busy", 44: "44-next"}, Parallel: 2})
	}()
	// Persistence of the rejected admission is the deterministic boundary;
	// release the unrelated occupied slot only once the paused row is recorded.
	deadline := time.Now().Add(3 * time.Second)
	for countEventsByType(log.snapshot(), "run.capacity_queued") == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(releaseBusy)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("batch failed to finish")
	}
	if factory.created != 2 {
		t.Fatalf("quota closed inside start-gate acquisition but launched %d agents, want two existing agents", factory.created)
	}
}

func TestWaitingContract_QuotaRecoveryRestoresCIObservation(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-wait-")
	t.Chdir(root)
	initGitRepo(t, root)
	client := &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open"}}, prs: map[string]*github.PR{}}
	launches := 0
	factory := &promptOnlyRunnableFactory{hook: func(issue *github.Issue, branch string) AgentRunResult {
		launches++
		if launches == 1 {
			return AgentRunResult{IssueNumber: 42, Branch: branch, Status: "failure", UsageLimitReached: true}
		}
		client.prs[branch] = &github.PR{Number: 17, State: "open", Body: "Closes #42", HeadRefName: branch, HeadRefOid: "current-sha", StatusCheckRollup: "pending", MergeStateStatus: "BLOCKED"}
		return AgentRunResult{IssueNumber: 42, Branch: branch, Status: "success"}
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waits := 0
	cfg := &config.Config{Agent: "opencode", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{"opencode": config.BuiltInAgentPresets["opencode"].Agent("opencode")}}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: cfg}, &spyEventLog{}, WithErrorLog(io.Discard), WithRunnableFactory(factory), WithSandboxFactory(&freshSandboxFactory{}),
		WithRunSessionOpts(runSessionOptions{releaseAwaitCapacity: true, currentHead: func(string) (string, error) { return "current-sha", nil },
			awaitWait: func(context.Context, time.Duration) error {
				waits++
				if waits == 3 {
					cancel()
					return context.Canceled
				}
				return nil
			}}))
	_, _ = o.RunBatch(ctx, Request{Issues: []int{42}, Branches: map[int]string{42: "42-quota-ci"}, Parallel: 1})
	if launches != 2 || waits != 3 {
		t.Fatalf("quota → CI launched agents while observing: launches=%d waits=%d", launches, waits)
	}
}

func TestWaitingContract_QuotaExpirySurvivesReconstructedExecutor(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-wait-")
	t.Chdir(root)
	now := time.Now().UTC()
	log := &spyEventLog{events: []events.Event{
		{Type: "run.started", RunID: "row", Issue: 42},
		{Type: "run.await", RunID: "row", Issue: 42, Payload: map[string]any{"await_reason": "usage-limit", "usage_limit_deadline_unix_seconds": now.Add(-time.Minute).Unix()}},
	}}
	factory := &controlledRunnableFactory{}
	o := NewOrchestrator(&fakeGitHubClient{}, &noopRenderer{}, nil, log, WithErrorLog(io.Discard), WithRunnableFactory(factory), WithSandboxFactory(&freshSandboxFactory{}))
	e := o.newRunExecutor(context.Background(), BatchConfig{
		Cfg: &config.Config{}, AgentCfg: config.BuiltInAgentPresets["opencode"].Agent("opencode"),
	}, &freshSandboxFactory{}, nil)
	result, _ := e.Execute(context.Background(), RowSpec{IssueNumber: 42, RunID: "row", Mode: ModeContinue, UsageLimitProbe: true, Branches: map[int]string{42: "42-limit"}})
	if result.Status != "failure" || len(factory.created) != 0 || !result.UsageLimitReached {
		t.Fatalf("expired reconstructed episode renewed/launched: result=%#v starts=%v", result, factory.created)
	}
}
