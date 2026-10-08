package batch

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/testenv"
)

func TestQuotaAccountingRestoresPollingWithoutChargingDowntime(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-quota-account-")
	t.Chdir(root)
	now := time.Now().UTC()
	worktree := filepath.Join(root, "worktree")
	if err := os.MkdirAll(filepath.Join(worktree, ".sandman"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".sandman", "task.md"), []byte("# Task\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	log := &spyEventLog{events: []events.Event{
		{Type: "run.started", RunID: "row", Issue: 42, Timestamp: now.Add(-7 * time.Hour), Payload: map[string]any{"branch": gateTestBranch, "batch_id": "old"}},
		{Type: "run.await", RunID: "row", Issue: 42, Timestamp: now.Add(-2 * time.Hour), Payload: map[string]any{
			"await_reason": "usage-limit", "branch": gateTestBranch, "batch_id": "old", "usage_limit_waited_seconds": 17400,
			"usage_limit_deadline_unix_seconds": now.Add(-time.Hour).Unix(),
		}},
	}}
	client := &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open"}}, prs: map[string]*github.PR{gateTestBranch: {Number: 17, State: "open", HeadRefName: gateTestBranch, HeadRefOid: "current-sha"}}}
	launches := 0
	factory := &controlledRunnableFactory{runnables: map[int]Runnable{42: waitingRunnableFunction(func(context.Context) AgentRunResult {
		launches++
		return AgentRunResult{IssueNumber: 42, Status: "failure", Branch: gateTestBranch, UsageLimitReached: true}
	})}}
	sbFactory := &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: worktree}}
	opts := gateTestRunOptions()
	opts.now = func() time.Time { return now }
	o := NewOrchestrator(client, &noopRenderer{}, nil, log, WithErrorLog(io.Discard), WithRunnableFactory(factory), WithSandboxFactory(sbFactory), WithRunSessionOpts(opts))
	result, _ := o.newRunExecutor(context.Background(), BatchConfig{Cfg: &config.Config{WorktreeDir: "worktrees"}, AgentCfg: config.BuiltInAgentPresets["opencode"].Agent("opencode"), IdentityResolver: noopIdentityResolver(), Retries: 1}, sbFactory, nil).Execute(context.Background(), RowSpec{
		IssueNumber: 42, Mode: ModeContinue, RunID: "row", UsageLimitProbe: true, Branches: map[int]string{42: gateTestBranch}, BaseBranch: "main",
	})
	states := events.ProjectRunStates(log.snapshot())
	if result.Status != "await" || launches != 1 || len(states) != 1 || states[0].AwaitEvent == nil {
		t.Fatalf("downtime removed quota recovery: result=%+v launches=%d events=%+v", result, launches, log.snapshot())
	}
	await := states[0].AwaitEvent
	if await.Payload["usage_limit_waited_seconds"] != 17400 || !result.UsageLimitDeadline.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("reconstruction reset accounted polling or charged downtime: result=%+v await=%+v", result, await)
	}
}

type unreadableQuotaAccountingLog struct{ *spyEventLog }

func (unreadableQuotaAccountingLog) Read() ([]events.Event, error) {
	return nil, errors.New("quota event log temporarily unreadable")
}

func TestQuotaAccountingUnavailableStillPermitsOrdinaryRecovery(t *testing.T) {
	for _, kind := range []string{"missing-operation", "missing-counter", "malformed-counter", "unreadable"} {
		t.Run(kind, func(t *testing.T) {
			root := testenv.MkdirShort(t, "sm-quota-fallback-")
			t.Chdir(root)
			log := &spyEventLog{}
			if kind != "missing-operation" {
				payload := map[string]any{"await_reason": "usage-limit", "branch": gateTestBranch, "usage_limit_deadline_unix_seconds": time.Now().Add(time.Hour).Unix()}
				if kind == "malformed-counter" {
					payload["usage_limit_waited_seconds"] = "untrusted"
				}
				log.events = []events.Event{{Type: "run.started", RunID: "row", Issue: 42, Payload: map[string]any{"branch": gateTestBranch}}, {Type: "run.await", RunID: "row", Issue: 42, Payload: payload}}
			}
			var eventLog events.EventLog = log
			if kind == "unreadable" {
				eventLog = unreadableQuotaAccountingLog{log}
			}
			client := &reviewWaitSchedulerGitHubClient{comments: []github.PRComment{}, fakeGitHubClient: fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open"}}, prs: map[string]*github.PR{gateTestBranch: {Number: 17, State: "open", HeadRefName: gateTestBranch, HeadRefOid: "current-sha", Body: "Closes #42"}}}}
			launches := 0
			factory := &controlledRunnableFactory{runnables: map[int]Runnable{42: waitingRunnableFunction(func(context.Context) AgentRunResult {
				launches++
				if launches == 1 {
					return AgentRunResult{IssueNumber: 42, Branch: gateTestBranch, Status: "failure", UsageLimitReached: true}
				}
				client.setPR(gateTestBranch, func(pr *github.PR) { pr.State, pr.Merged = "merged", true })
				return AgentRunResult{IssueNumber: 42, Branch: gateTestBranch, Status: "success"}
			})}}
			sbFactory := &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: root}}
			o := NewOrchestrator(client, &noopRenderer{}, nil, eventLog, WithErrorLog(io.Discard), WithRunnableFactory(factory), WithSandboxFactory(sbFactory), WithRunSessionOpts(gateTestRunOptions()))
			result, _ := o.newRunExecutor(context.Background(), BatchConfig{Cfg: &config.Config{WorktreeDir: "worktrees"}, AgentCfg: config.BuiltInAgentPresets["opencode"].Agent("opencode"), IdentityResolver: noopIdentityResolver(), Retries: 1}, sbFactory, nil).Execute(context.Background(), RowSpec{IssueNumber: 42, Mode: ModeContinue, RunID: "row", UsageLimitProbe: true, Branches: map[int]string{42: gateTestBranch}, BaseBranch: "main"})
			wantAwaits := 0
			if kind != "missing-operation" {
				wantAwaits = 1 // Existing evidence only; no new wait may be invented.
			}
			if result.Status != "success" || launches != 2 || countEventsByType(log.snapshot(), "run.await") != wantAwaits || countEventsByType(log.snapshot(), "run.retry") != 1 {
				t.Fatalf("%s accounting vetoed ordinary recovery or renewed waiting: result=%+v launches=%d events=%+v", kind, result, launches, log.snapshot())
			}
		})
	}
}

func TestQuotaFailureDoesNotAssignSiblingOutcome(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-quota-sibling-")
	t.Chdir(root)
	initGitRepo(t, root)
	client := &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open"}, 43: {Number: 43, State: "closed"}}, prs: map[string]*github.PR{
		"42-limit": {Number: 17, State: "open", HeadRefName: "42-limit"}, "43-own": {Number: 18, State: "merged", Merged: true, Body: "Closes #43"},
	}}
	limited, sibling := 0, 0
	factory := &controlledRunnableFactory{runnables: map[int]Runnable{
		42: waitingRunnableFunction(func(context.Context) AgentRunResult {
			limited++
			return AgentRunResult{IssueNumber: 42, Branch: "42-limit", Status: "failure", UsageLimitReached: true}
		}),
		43: waitingRunnableFunction(func(context.Context) AgentRunResult {
			sibling++
			return AgentRunResult{IssueNumber: 43, Branch: "43-own", Status: "success"}
		}),
	}}
	log := &spyEventLog{}
	now := time.Now().UTC()
	var clockMu sync.Mutex
	cfg := &config.Config{Agent: "opencode", Sandbox: "worktree", WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"opencode": config.BuiltInAgentPresets["opencode"].Agent("opencode")}}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: cfg}, log, WithErrorLog(io.Discard), WithRunnableFactory(factory), WithSandboxFactory(reviewWaitSchedulerSandboxFactory{}), WithRunSessionOpts(runSessionOptions{
		releaseAwaitCapacity: true, now: func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }, awaitWait: func(_ context.Context, interval time.Duration) error {
			clockMu.Lock()
			now = now.Add(interval)
			clockMu.Unlock()
			return nil
		},
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, _ := o.RunBatch(ctx, Request{Issues: []int{42, 43}, Branches: map[int]string{42: "42-limit", 43: "43-own"}, Parallel: 1, Retries: 0, RunTS: "261008120000", RunShortID: "siblings"})
	if result == nil || result.Runs[0].Status != "failure" || result.Runs[1].Status != "success" || limited != 31 || sibling != 1 {
		t.Fatalf("quota owner assigned another row's outcome: result=%+v limited=%d sibling=%d states=%+v", result, limited, sibling, events.ProjectRunStates(log.snapshot()))
	}
	for _, state := range events.ProjectRunStates(log.snapshot()) {
		if state.IssueNumber() == 43 && (!state.IsTerminal() || state.Status() != "success") {
			t.Fatalf("sibling projection lost its own verified outcome: %+v", state)
		}
	}
}

func TestQuotaAccountingSurvivesSchedulerReconstruction(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-quota-restart-")
	t.Chdir(root)
	initGitRepo(t, root)
	now := time.Now().UTC()
	log := &spyEventLog{events: []events.Event{
		{Type: "run.started", RunID: "row", Issue: 42, Timestamp: now.Add(-time.Hour), Payload: map[string]any{"branch": gateTestBranch, "batch_id": "old"}},
		{Type: "run.await", RunID: "row", Issue: 42, Timestamp: now.Add(-11 * time.Minute), Payload: map[string]any{
			"await_reason": "usage-limit", "branch": gateTestBranch, "base_branch": "main", "batch_id": "old", "usage_limit_waited_seconds": 17400,
			"usage_limit_deadline_unix_seconds": now.Add(10 * time.Minute).Unix(),
		}},
	}}
	layout := paths.NewLayout(&config.Config{WorktreeDir: "worktrees"}, root)
	recovery := daemon.RunWait{Protocol: "run-wait/v1", RunID: "row", BatchID: "old", Issue: 42, Branch: gateTestBranch, BaseBranch: "main", OperationID: "quota:old", OperationDeadline: now.Add(10 * time.Minute), UsageLimitProbe: true, NextPollAt: now.Add(-time.Minute)}
	if err := daemon.RenewRunWait(layout.BatchDir("old"), recovery, now); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(layout.WorktreeDir, gateTestBranch)
	if err := os.MkdirAll(filepath.Join(worktree, ".sandman"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".sandman", "task.md"), []byte("# Task\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &reviewWaitSchedulerGitHubClient{comments: []github.PRComment{}, fakeGitHubClient: fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open"}}, prs: map[string]*github.PR{gateTestBranch: {Number: 17, State: "open", HeadRefName: gateTestBranch, HeadRefOid: "current-sha", Body: "Closes #42"}}}}
	sb := &usageLimitRetrySandbox{workDir: worktree, failures: 2, onSuccess: func() { client.setPR(gateTestBranch, func(pr *github.PR) { pr.State, pr.Merged = "merged", true }) }}
	cfg := &config.Config{Agent: "opencode", Sandbox: "worktree", WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"opencode": config.BuiltInAgentPresets["opencode"].Agent("opencode")}}
	waits := 0
	o := NewOrchestrator(client, &retryRenderer{result: "# Task\n"}, &fakeConfigStore{config: cfg}, log, WithErrorLog(io.Discard), WithSandboxFactory(&usageLimitRetrySandboxFactory{sandbox: sb}), WithRunnableFactory(usageLimitRetryRunnableFactory{}), WithRunSessionOpts(runSessionOptions{
		releaseAwaitCapacity: true, currentHead: func(string) (string, error) { return "current-sha", nil }, now: func() time.Time { return now },
		awaitWait: func(_ context.Context, interval time.Duration) error { waits++; now = now.Add(interval); return nil },
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := o.RunBatch(ctx, Request{Issues: []int{42}, Branches: map[int]string{42: gateTestBranch}, Mode: map[int]IssueMode{42: ModeContinue}, RunIDs: map[int]string{42: "row"}, PreviousRunIDs: map[int]string{42: "row"}, PreviousRunBatchIDs: map[int]string{42: "old"}, RecoveryWaits: map[int]daemon.RunWait{42: recovery}, ReuseSession: map[int]bool{42: true}, RunTS: "261008120000", RunShortID: "recovery", Retries: 1, Parallel: 1, PromptConfig: prompt.RenderConfig{ReviewCommandSet: true}})
	if err != nil || result == nil || result.Runs[0].Status != "success" || sb.attemptCount() != 3 || waits != 1 || countEventsByType(log.snapshot(), "run.retry") != 1 {
		t.Fatalf("scheduler renewed consumed polling: result=%+v error=%v launches=%d waits=%d retries=%d events=%+v", result, err, sb.attemptCount(), waits, countEventsByType(log.snapshot(), "run.retry"), log.snapshot())
	}
}

func TestQuotaOwnershipEstimateDoesNotChangeOperationOrGrace(t *testing.T) {
	root := t.TempDir()
	layout := paths.NewLayout(nil, root)
	now := time.Now().UTC()
	log := &spyEventLog{events: []events.Event{
		{Type: "run.started", RunID: "row", Issue: 42, Timestamp: now.Add(-time.Minute), Payload: map[string]any{"branch": gateTestBranch, "batch_id": "batch"}},
		{Type: "run.await", RunID: "row", Issue: 42, Timestamp: now, Payload: map[string]any{"await_reason": "usage-limit", "usage_limit_waited_seconds": 600, "usage_limit_deadline_unix_seconds": now.Add(time.Hour).Unix()}},
	}}
	claim, err := daemon.ClaimRun(layout.SandmanDir, "row")
	if err != nil {
		t.Fatal(err)
	}
	defer claim.Close()
	pulse := make(chan time.Time)
	owner, err := newWaitOwner(layout.BatchDir("batch"), daemon.RunWait{Protocol: "run-wait/v1", RunID: "row", BatchID: "batch", Issue: 42, Branch: gateTestBranch, BaseBranch: "main", UsageLimitProbe: true, OperationID: "quota:old-estimate", OperationDeadline: now.Add(time.Hour)}, func() time.Time { return now }, log, pulse)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.close()
	row := RowSpec{IssueNumber: 42, RunID: "row", Mode: ModeContinue, UsageLimitProbe: true, UsageLimitDeadline: now.Add(time.Hour), Branches: map[int]string{42: gateTestBranch}, BaseBranch: "main"}
	if err := owner.checkpoint(row, false, usageLimitPollInterval); err != nil {
		t.Fatal(err)
	}
	first, err := daemon.ReadRunWait(layout.BatchDir("batch"), "row")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour) // Capacity/downtime cannot consume polling.
	row.UsageLimitDeadline = now.Add(usageLimitRetryWindow - 10*time.Minute)
	if err := owner.checkpoint(row, true, 0); err != nil {
		t.Fatalf("recomputed quota estimate vetoed ownership: %v", err)
	}
	saved, err := daemon.ReadRunWait(layout.BatchDir("batch"), "row")
	if err != nil || saved.OperationID != first.OperationID || saved.OperationID != "quota:row" || !saved.OperationDeadline.IsZero() || !saved.LeaseExpiresAt.Equal(now.Add(daemon.RunRecoveryGrace)) {
		t.Fatalf("quota estimate became a hard lifetime or a new operation: first=%+v saved=%+v error=%v", first, saved, err)
	}
}

func TestHistoricalRepairCountCannotInvalidateCurrentHeadCIWait(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-ci-history-")
	t.Chdir(root)
	now := time.Now().UTC()
	worktree := filepath.Join(root, "worktree")
	stateDir := filepath.Join(worktree, ".sandman", "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	registration := ciWaitRegistration{Protocol: ciWaitProtocol, PullRequest: 17, HeadSHA: "current-sha", StartedUnixSeconds: now.Add(-time.Minute).Unix(), DeadlineUnixSeconds: now.Add(ciWaitTimeout - time.Minute).Unix(), EffectiveTimeoutSecs: int64(ciWaitTimeout / time.Second), RemediationAttempts: -4}
	if err := atomicfs.WriteAtomicJSON(filepath.Join(stateDir, "17.ci_wait.json"), registration, 0o600); err != nil {
		t.Fatal(err)
	}
	client := &reviewWaitSchedulerGitHubClient{comments: []github.PRComment{}, fakeGitHubClient: fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open"}}, prs: map[string]*github.PR{gateTestBranch: {Number: 17, State: "open", HeadRefName: gateTestBranch, HeadRefOid: "current-sha", Body: "Closes #42", StatusCheckRollup: "pending"}}}}
	launches := 0
	factory := &controlledRunnableFactory{runnables: map[int]Runnable{42: waitingRunnableFunction(func(context.Context) AgentRunResult {
		launches++
		client.setPR(gateTestBranch, func(pr *github.PR) { pr.State, pr.Merged = "merged", true })
		return AgentRunResult{IssueNumber: 42, Status: "success", Branch: gateTestBranch}
	})}}
	sbFactory := &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: worktree}}
	log := &spyEventLog{}
	o := NewOrchestrator(client, &noopRenderer{}, nil, log, WithErrorLog(io.Discard), WithRunnableFactory(factory), WithSandboxFactory(sbFactory), WithRunSessionOpts(gateTestRunOptions()))
	result, _ := o.newRunExecutor(context.Background(), BatchConfig{Cfg: &config.Config{}, AgentCfg: config.Agent{Command: "true"}, IdentityResolver: noopIdentityResolver()}, sbFactory, nil).Execute(context.Background(), RowSpec{IssueNumber: 42, RunID: "row", Mode: ModeContinue, Branches: map[int]string{42: gateTestBranch}, BaseBranch: "main"})
	if result.Status != "await" || launches != 0 {
		t.Fatalf("ignored repair count invalidated authorized CI waiting: result=%+v launches=%d events=%+v", result, launches, log.snapshot())
	}
}
