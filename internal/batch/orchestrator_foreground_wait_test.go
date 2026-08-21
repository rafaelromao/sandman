package batch

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/testenv"
)

func TestRunExecutor_ForegroundLifecycleWaitKeepsSessionUntilMerge(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)

	branch := "42-fix-bug"
	sb := &retrySandbox{workDir: filepath.Join(workDir, "worktree")}
	sbFactory := &retrySandboxFactory{sandbox: sb}
	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	factory := &fakeRunnableFactory{results: []AgentRunResult{{
		IssueNumber: 42,
		Status:      "success",
		Branch:      branch,
	}}}
	client := &perRunGateSequenceClient{
		fakeGitHubClient: fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}},
		},
		responses: []*github.PR{
			{Number: 17, State: "open", Body: "Closes #42", HeadRefName: branch, HeadRefOid: "current-sha", StatusCheckRollup: "pending", MergeStateStatus: "BLOCKED"},
			{Number: 17, State: "open", Body: "Closes #42", HeadRefName: branch, HeadRefOid: "current-sha", StatusCheckRollup: "pending", MergeStateStatus: "BLOCKED"},
			{Number: 17, State: "merged", Merged: true, Body: "Closes #42", HeadRefName: branch, HeadRefOid: "current-sha"},
		},
	}
	var waits []time.Duration
	options := gateTestRunOptions()
	options.lifecyclePollPlan = []time.Duration{7 * time.Second}
	options.lifecycleWait = func(ctx context.Context, interval time.Duration) error {
		waits = append(waits, interval)
		return nil
	}
	o := NewOrchestrator(
		client,
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(sbFactory),
		WithRunnableFactory(factory),
		WithRunSessionOpts(options),
	)
	bc := BatchConfig{
		Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}

	result, started := o.newRunExecutor(context.Background(), bc, sbFactory, nil).Execute(context.Background(), RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: branch},
		BaseBranch:  "main",
	})
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success", result.Status)
	}
	if result.RetriesTotal != 1 {
		t.Fatalf("retries total = %d, want 1", result.RetriesTotal)
	}
	if len(factory.created) != 1 {
		t.Fatalf("agent launches = %d, want 1", len(factory.created))
	}
	if len(waits) != 1 || waits[0] != 7*time.Second {
		t.Fatalf("lifecycle waits = %v, want [7s]", waits)
	}
	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if countEventsByType(logs, "run.retry") != 0 {
		t.Fatalf("run.retry events = %d, want 0", countEventsByType(logs, "run.retry"))
	}
	if countEventsByType(logs, "run.await") == 0 {
		t.Fatal("expected run.await before merged terminal outcome")
	}
	if finishedStatus(t, logs) != "success" {
		t.Fatalf("finished status = %q, want success", finishedStatus(t, logs))
	}
	if sb.restoreHostPathsCalled == false {
		t.Fatal("expected host paths restored during lifecycle observation")
	}
}

func TestRunExecutor_ForegroundLifecycleWaitRepeatsFinalPollInterval(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)

	branch := "42-fix-bug"
	sb := &retrySandbox{workDir: filepath.Join(workDir, "worktree")}
	sbFactory := &retrySandboxFactory{sandbox: sb}
	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	factory := &fakeRunnableFactory{results: []AgentRunResult{{IssueNumber: 42, Status: "success", Branch: branch}}}
	pending := func() *github.PR {
		return &github.PR{Number: 17, State: "open", Body: "Closes #42", HeadRefName: branch, HeadRefOid: "current-sha", StatusCheckRollup: "pending", MergeStateStatus: "BLOCKED"}
	}
	client := &perRunGateSequenceClient{
		fakeGitHubClient: fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}}},
		responses: []*github.PR{
			pending(), // closing-reference guard
			pending(), // initial lifecycle decision
			pending(), // first observation
			pending(), // second observation
			pending(), // third observation
			{Number: 17, State: "merged", Merged: true, Body: "Closes #42", HeadRefName: branch, HeadRefOid: "current-sha"},
		},
	}
	var waits []time.Duration
	options := gateTestRunOptions()
	options.lifecyclePollPlan = []time.Duration{7 * time.Second, 3 * time.Second}
	options.lifecycleWait = func(ctx context.Context, interval time.Duration) error {
		waits = append(waits, interval)
		return nil
	}
	o := NewOrchestrator(client, &retryRenderer{result: "rendered prompt"}, nil, eventLog,
		WithErrorLog(io.Discard), WithSandboxFactory(sbFactory), WithRunnableFactory(factory), WithRunSessionOpts(options))
	bc := BatchConfig{
		Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	result, started := o.newRunExecutor(context.Background(), bc, sbFactory, nil).Execute(context.Background(), RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: branch},
		BaseBranch:  "main",
	})
	if !started || result.Status != "success" {
		t.Fatalf("run = (%t, %q), want started success", started, result.Status)
	}
	wantWaits := []time.Duration{7 * time.Second, 3 * time.Second, 3 * time.Second, 3 * time.Second}
	if len(waits) != len(wantWaits) {
		t.Fatalf("lifecycle waits = %v, want %v", waits, wantWaits)
	}
	for i := range wantWaits {
		if waits[i] != wantWaits[i] {
			t.Fatalf("lifecycle wait %d = %s, want %s", i, waits[i], wantWaits[i])
		}
	}
	if len(factory.created) != 1 {
		t.Fatalf("agent launches = %d, want 1", len(factory.created))
	}
}

func TestRunBatch_ExplicitAbortDuringForegroundWaitBlocksDependent(t *testing.T) {
	parentWaiting := make(chan struct{})
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, State: "open", Title: "Parent"},
			43: {Number: 43, State: "open", Title: "Dependent"},
		},
		prs: map[string]*github.PR{
			"42-parent": {Number: 17, State: "open", Body: "Closes #42", HeadRefName: "42-parent", HeadRefOid: "current-sha", StatusCheckRollup: "pending", MergeStateStatus: "BLOCKED"},
		},
	}
	results := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "success", Branch: "42-parent"},
		{IssueNumber: 43, Status: "success", Branch: "43-dependent"},
	}}
	log := &spyEventLog{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{
		Agent:          "test-agent",
		Sandbox:        "worktree",
		WorktreeDir:    ".sandman/worktrees",
		Git:            config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}},
	}}, log,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&fakeSandboxFactory{sandbox: &fakeSandbox{}}),
		WithRunnableFactory(results),
		WithRunSessionOpts(runSessionOptions{
			currentHead:         func(string) (string, error) { return "current-sha", nil },
			lifecyclePollPlan:   []time.Duration{time.Hour},
			foregroundLifecycle: true,
			lifecycleWait: func(ctx context.Context, interval time.Duration) error {
				select {
				case <-parentWaiting:
				default:
					close(parentWaiting)
				}
				<-ctx.Done()
				return ctx.Err()
			},
		}),
	)

	done := make(chan struct{})
	var batchResult *Result
	var batchErr error
	go func() {
		defer close(done)
		batchResult, batchErr = o.RunBatch(context.Background(), Request{
			Issues:       []int{42, 43},
			Dependencies: map[int][]int{43: {42}},
			Parallel:     2,
		})
	}()
	select {
	case <-parentWaiting:
	case <-time.After(2 * time.Second):
		t.Fatal("parent did not enter foreground lifecycle wait")
	}
	if err := o.AbortIssue(42); err != nil {
		t.Fatalf("abort parent: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("batch did not finish after explicit abort")
	}
	if batchErr == nil || !errors.Is(batchErr, ErrAborted) {
		t.Fatalf("batch error = %v, want ErrAborted", batchErr)
	}
	if batchResult == nil || len(batchResult.Runs) != 2 {
		t.Fatalf("batch result = %#v, want two outcomes", batchResult)
	}
	if len(results.created) != 1 {
		t.Fatalf("agent launches = %d, want only parent", len(results.created))
	}
	logs := log.snapshot()
	if countEventsByType(logs, "run.retry") != 0 {
		t.Fatalf("run.retry events = %d, want 0", countEventsByType(logs, "run.retry"))
	}
	if countEventsByType(logs, "run.started") != 1 {
		t.Fatalf("run.started events = %d, want only parent", countEventsByType(logs, "run.started"))
	}
	if countEventsByType(logs, "run.aborted") != 2 {
		t.Fatalf("run.aborted events = %d, want parent and dependent", countEventsByType(logs, "run.aborted"))
	}
	if got := batchResult.Runs[1].Status; got != "aborted" {
		t.Fatalf("dependent status = %q, want aborted", got)
	}
}

func TestRunExecutor_ResumeCapFallsBackToForegroundObservation(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)

	branch := "42-fix-bug"
	sb := &retrySandbox{workDir: filepath.Join(workDir, "worktree")}
	sbFactory := &retrySandboxFactory{sandbox: sb}
	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	factory := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "success", Branch: branch},
		{IssueNumber: 42, Status: "success", Branch: branch},
	}}
	ready := func() *github.PR {
		return &github.PR{Number: 17, State: "open", Body: "Closes #42", HeadRefName: branch, HeadRefOid: "current-sha", StatusCheckRollup: "success", ReviewDecision: "APPROVED", MergeStateStatus: "CLEAN"}
	}
	client := &perRunGateSequenceClient{
		fakeGitHubClient: fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}}},
		responses: []*github.PR{
			ready(), // closing-reference guard
			ready(), // first lifecycle decision
			ready(), // resume evidence
			ready(), // second closing-reference guard
			ready(), // second lifecycle decision
			{Number: 17, State: "merged", Merged: true, Body: "Closes #42", HeadRefName: branch, HeadRefOid: "current-sha"},
		},
	}
	var waits []time.Duration
	options := gateTestRunOptions()
	options.awaitResumeMax = 1
	options.lifecyclePollPlan = []time.Duration{5 * time.Second}
	options.lifecycleWait = func(ctx context.Context, interval time.Duration) error {
		waits = append(waits, interval)
		return nil
	}
	o := NewOrchestrator(client, &retryRenderer{result: "rendered prompt"}, nil, eventLog,
		WithErrorLog(io.Discard), WithSandboxFactory(sbFactory), WithRunnableFactory(factory), WithRunSessionOpts(options))
	bc := BatchConfig{
		Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	result, started := o.newRunExecutor(context.Background(), bc, sbFactory, nil).Execute(context.Background(), RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: branch},
		BaseBranch:  "main",
	})
	if !started || result.Status != "success" {
		t.Fatalf("run = (%t, %q), want started success", started, result.Status)
	}
	if len(factory.created) != 2 {
		t.Fatalf("agent launches = %d, want initial plus one resume", len(factory.created))
	}
	if len(waits) != 1 || waits[0] != 5*time.Second {
		t.Fatalf("lifecycle waits = %v, want [5s] after resume cap", waits)
	}
	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if countEventsByType(logs, "run.retry") != 0 {
		t.Fatalf("run.retry events = %d, want 0", countEventsByType(logs, "run.retry"))
	}
	if countEventsByType(logs, "run.resumed") != 1 {
		t.Fatalf("run.resumed events = %d, want 1", countEventsByType(logs, "run.resumed"))
	}
}

func TestRunExecutor_ForegroundWaitCoversPullRequestPublication(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)

	branch := "42-fix-bug"
	sb := &retrySandbox{workDir: filepath.Join(workDir, "worktree")}
	sbFactory := &retrySandboxFactory{sandbox: sb}
	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	factory := &fakeRunnableFactory{results: []AgentRunResult{{IssueNumber: 42, Status: "success", Branch: branch}}}
	pending := &github.PR{Number: 17, State: "open", Body: "Closes #42", HeadRefName: branch, HeadRefOid: "current-sha", StatusCheckRollup: "pending", MergeStateStatus: "BLOCKED"}
	merged := &github.PR{Number: 17, State: "merged", Merged: true, Body: "Closes #42", HeadRefName: branch, HeadRefOid: "current-sha"}
	client := &perRunGateSequenceClient{
		fakeGitHubClient: fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}}},
		responses:        []*github.PR{nil, nil, pending, merged},
	}
	var waits []time.Duration
	options := gateTestRunOptions()
	options.lifecyclePollPlan = []time.Duration{time.Second, 2 * time.Second}
	options.lifecycleWait = func(ctx context.Context, interval time.Duration) error {
		waits = append(waits, interval)
		return nil
	}
	o := NewOrchestrator(client, &retryRenderer{result: "rendered prompt"}, nil, eventLog,
		WithErrorLog(io.Discard), WithSandboxFactory(sbFactory), WithRunnableFactory(factory), WithRunSessionOpts(options))
	bc := BatchConfig{
		Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	result, started := o.newRunExecutor(context.Background(), bc, sbFactory, nil).Execute(context.Background(), RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: branch},
		BaseBranch:  "main",
	})
	if !started || result.Status != "success" {
		t.Fatalf("run = (%t, %q), want started success", started, result.Status)
	}
	if len(waits) != 2 || waits[0] != time.Second || waits[1] != 2*time.Second {
		t.Fatalf("publication waits = %v, want [1s 2s]", waits)
	}
	if len(factory.created) != 1 {
		t.Fatalf("agent launches = %d, want 1", len(factory.created))
	}
}

func TestRunExecutor_ContinuationForegroundWaitCoversPullRequestPublication(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)

	branch := "42-fix-bug"
	sb := &retrySandbox{workDir: filepath.Join(workDir, "worktree")}
	sbFactory := &retrySandboxFactory{sandbox: sb}
	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	factory := &fakeRunnableFactory{results: []AgentRunResult{{IssueNumber: 42, Status: "success", Branch: branch}}}
	pending := &github.PR{Number: 17, State: "open", Body: "Closes #42", HeadRefName: branch, HeadRefOid: "current-sha", StatusCheckRollup: "pending", MergeStateStatus: "BLOCKED"}
	merged := &github.PR{Number: 17, State: "merged", Merged: true, Body: "Closes #42", HeadRefName: branch, HeadRefOid: "current-sha"}
	client := &perRunGateSequenceClient{
		fakeGitHubClient: fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}}},
		responses:        []*github.PR{nil, nil, pending, pending, merged},
	}
	var waits []time.Duration
	options := gateTestRunOptions()
	options.lifecyclePollPlan = []time.Duration{time.Second, 2 * time.Second}
	options.lifecycleWait = func(ctx context.Context, interval time.Duration) error {
		waits = append(waits, interval)
		return nil
	}
	o := NewOrchestrator(client, &retryRenderer{result: "rendered prompt"}, nil, eventLog,
		WithErrorLog(io.Discard), WithSandboxFactory(sbFactory), WithRunnableFactory(factory), WithRunSessionOpts(options))
	bc := BatchConfig{
		Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	result, started := o.newRunExecutor(context.Background(), bc, sbFactory, nil).Execute(context.Background(), RowSpec{
		IssueNumber:    42,
		Mode:           ModeContinue,
		Branches:       map[int]string{42: branch},
		PreviousRunIDs: map[int]string{42: "prior-run"},
		BaseBranch:     "main",
	})
	if !started || result.Status != "success" {
		t.Fatalf("run = (%t, %q), want started success", started, result.Status)
	}
	if len(waits) != 2 || waits[0] != time.Second || waits[1] != 2*time.Second {
		t.Fatalf("publication waits = %v, want [1s 2s]", waits)
	}
	if len(factory.created) != 1 {
		t.Fatalf("agent launches = %d, want 1", len(factory.created))
	}
}
