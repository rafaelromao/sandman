package batch

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
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

func TestWaitingContract_DependentReadmittedAfterYieldingPrerequisite(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-yield-dep-")
	t.Chdir(root)
	initGitRepo(t, root)
	client := &reviewWaitSchedulerGitHubClient{fakeGitHubClient: fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open"}, 43: {Number: 43, State: "closed"}, 44: {Number: 44, State: "closed"}},
		prs:    map[string]*github.PR{"42-parent": {Number: 42, State: "open", HeadRefName: "42-parent", HeadRefOid: "current-sha", Body: "Closes #42", StatusCheckRollup: "pending"}, "43-next": mergedPR("43-next", "Closes #43"), "44-free": mergedPR("44-free", "Closes #44")},
	}}
	log := &spyEventLog{}
	releasePoll := make(chan struct{})
	var attempts atomic.Int32
	factory := &controlledRunnableFactory{runnables: map[int]Runnable{
		42: waitingRunnableFunction(func(context.Context) AgentRunResult {
			attempts.Add(1)
			return AgentRunResult{IssueNumber: 42, Status: "success", Branch: "42-parent"}
		}),
		43: waitingRunnableFunction(func(context.Context) AgentRunResult {
			for _, state := range events.ProjectRunStates(log.snapshot()) {
				if state.IssueNumber() == 42 && (!state.IsTerminal() || state.Status() != "success") {
					t.Errorf("dependent admitted before prerequisite terminal success: %+v", state)
				}
			}
			return AgentRunResult{IssueNumber: 43, Status: "success", Branch: "43-next"}
		}),
		44: waitingRunnableFunction(func(context.Context) AgentRunResult {
			client.mu.Lock()
			client.issues[42] = &github.Issue{Number: 42, State: "closed"}
			client.prs["42-parent"] = mergedPR("42-parent", "Closes #42")
			client.mu.Unlock()
			close(releasePoll)
			return AgentRunResult{IssueNumber: 44, Status: "success", Branch: "44-free"}
		}),
	}}
	cfg := &config.Config{Agent: "opencode", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", AgentProviders: map[string]config.Agent{"opencode": config.BuiltInAgentPresets["opencode"].Agent("opencode")}}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: cfg}, log, WithErrorLog(io.Discard), WithRunnableFactory(factory), WithSandboxFactory(reviewWaitSchedulerSandboxFactory{}), WithRunSessionOpts(runSessionOptions{releaseAwaitCapacity: true, currentHead: func(string) (string, error) { return "current-sha", nil }, awaitWait: func(ctx context.Context, _ time.Duration) error {
		select {
		case <-releasePoll:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := o.RunBatch(ctx, Request{Issues: []int{42, 43, 44}, Parallel: 1, RunTS: "261006090000", RunShortID: "dependent", Branches: map[int]string{42: "42-parent", 43: "43-next", 44: "44-free"}, Dependencies: map[int][]int{43: {42}}})
	if err != nil || result == nil || attempts.Load() != 1 {
		t.Fatalf("yielded dependency never readmitted: result=%+v err=%v attempts=%d", result, err, attempts.Load())
	}
	for _, run := range result.Runs {
		if run.Status != "success" {
			t.Fatalf("dependent remained unfinished after parent success: %+v", run)
		}
	}
	if countEventsByType(log.snapshot(), "run.await") == 0 {
		t.Fatal("prerequisite never suspended")
	}
}

type observedCleanupSandbox struct {
	*contextRolloverSandbox
	starts int
	stops  int
}

func (s *observedCleanupSandbox) Start(sandbox.SandboxStart) error { s.starts++; return nil }
func (s *observedCleanupSandbox) Stop() error                      { s.stops++; return nil }

func TestWaitingContract_TerminalObservationUsesResolvedContainerPolicy(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-observe-policy-")
	t.Chdir(root)
	sb := &observedCleanupSandbox{contextRolloverSandbox: &contextRolloverSandbox{workDir: filepath.Join(root, "worktree")}}
	created := 0
	resolved := sandboxFactoryFunc(func(_, _, _, _ string, container sandbox.Container) sandbox.Sandbox {
		created++
		if container != nil {
			t.Fatal("terminal observation allocated a container")
		}
		return sb
	})
	log := &spyEventLog{}
	o := NewOrchestrator(nil, nil, nil, log, WithErrorLog(io.Discard))
	executor := o.newRunExecutor(context.Background(), BatchConfig{Cfg: &config.Config{Sandbox: "podman"}, SandboxMode: "podman"}, resolved, nil)
	result := executor.finishObserved(context.Background(), RowSpec{IssueNumber: 42, RunID: "observed", Branches: map[int]string{42: "42-work"}, BaseBranch: "main"}, "success", nil)
	if result.Status != "success" || created != 1 || sb.starts != 0 || sb.started != 0 || sb.stops != 1 || !sb.hostPathsRestored {
		t.Fatalf("resolved cleanup skipped or execution started: result=%+v created=%d sandbox=%+v", result, created, sb)
	}
	if len(log.snapshot()) != 1 || log.snapshot()[0].Payload["worktree_state"] != "cleaned" {
		t.Fatalf("terminal cleanup projection=%+v", log.snapshot())
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

func TestWaitingContract_FreshRepairAllowanceAcrossExecutorReentry(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-wait-")
	t.Chdir(root)
	worktree := filepath.Join(root, "worktree")
	budgetPath := filepath.Join(worktree, ".sandman", "state", "17.lifecycle-budget.json")
	if err := os.MkdirAll(filepath.Dir(budgetPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(budgetPath, []byte("obsolete corrupt repair budget"), 0o600); err != nil {
		t.Fatal(err)
	}
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
	if len(factory.created) != 4 {
		t.Fatalf("same-head repair launches across executor re-entry=%d, want entry plus one legacy relaunch per session", len(factory.created))
	}
	for _, event := range log.snapshot() {
		if event.Type != "run.finished" {
			continue
		}
		if event.Payload["reason"] != "REMEDIATION_BUDGET_EXHAUSTED" || event.Payload["await"] != nil || event.Payload["gate"] != nil {
			t.Fatalf("legacy exhausted-session failure lost terminal semantics: %+v", event)
		}
	}
}

func TestWaitingContract_TerminalObservationDropsActiveWaitMarkers(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	log := &spyEventLog{}
	o := NewOrchestrator(nil, nil, nil, log, WithErrorLog(io.Discard))
	executor := o.newRunExecutor(context.Background(), BatchConfig{}, &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: root}}, nil)
	result := executor.finishObserved(context.Background(), RowSpec{IssueNumber: 42, RunID: "terminal-observation", Branches: map[int]string{42: "42-work"}}, "failure", map[string]any{
		"reason": "REMEDIATION_BUDGET_EXHAUSTED", "await": true, "await_reason": "ci-failure", "gate": "ci-failure", "head_sha": "head",
	})
	finished := findEvent(log.snapshot(), "run.finished")
	if result.Status != "failure" || finished == nil {
		t.Fatalf("observed failure was not terminalized: result=%+v events=%+v", result, log.snapshot())
	}
	for _, key := range []string{"await", "await_reason", "gate"} {
		if _, present := finished.Payload[key]; present {
			t.Fatalf("terminal failure retained active %s marker: %+v", key, finished)
		}
	}
	if finished.Payload["external_gate"] != "ci-failure" || finished.Payload["head_sha"] != "head" {
		t.Fatalf("terminal failure discarded diagnostic evidence: %+v", finished)
	}
}

func TestWaitingContract_CancelledRemediationDoesNotResume(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &runSession{deps: runDeps{githubClient: &fakeGitHubClient{}}}
	_, resume := s.resumePromptFromGate(ctx, &fakeSandbox{workDir: root}, "branch", "run", map[string]any{"gate": gateReadyToMerge, "pull_request": 17, "head_sha": "head"})
	if resume || s.resumeCount != 0 {
		t.Fatal("cancelled repair resumed or consumed the session allowance")
	}
	if _, err := os.Stat(filepath.Join(root, ".sandman", "state", "17.lifecycle-budget.json")); !os.IsNotExist(err) {
		t.Fatalf("cancelled repair created a historical budget: %v", err)
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

func TestWaitingContract_FailedQuotaProbeRetiresOnlyItsOwnPause(t *testing.T) {
	gate := newBatchQuotaGate()
	gate.report(42, AgentRunResult{Status: "await", UsageLimitReached: true}, false)
	gate.report(43, AgentRunResult{Status: "await", UsageLimitReached: true}, false)
	gate.report(42, AgentRunResult{Status: "failure"}, true)
	gate.retire(42)
	if !gate.paused() {
		t.Fatal("failed owner cleared another active quota pause")
	}
	gate.modelProgress(43)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := gate.wait(ctx); err != nil {
		t.Fatalf("retired failure prevented another owner's verified recovery: %v", err)
	}
}

func TestWaitingContract_VerifiedQuotaProgressReopensOnlyThatBatchGate(t *testing.T) {
	gate := newBatchQuotaGate()
	gate.report(42, AgentRunResult{Status: "await", UsageLimitReached: true}, false)
	if !gate.paused() {
		t.Fatal("quota gate should start paused")
	}
	gate.modelProgress(42)
	if gate.paused() {
		t.Fatal("verified model progress should reopen sibling admission")
	}
	gate.modelProgress(42)
	gate.report(42, AgentRunResult{Status: "await", UsageLimitReached: true}, false)
	if !gate.paused() {
		t.Fatal("later recognized provider limit should pause admission again")
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
	if err != nil || record.InitialAdmission || !record.UsageLimitProbe || !record.OperationDeadline.IsZero() || record.OperationID != "quota:row" || !record.LeaseExpiresAt.Equal(now.Add(daemon.RunRecoveryGrace)) {
		t.Fatalf("nil logger lost runtime-derived quota schedule: %+v err=%v", record, err)
	}
}

func TestWaitingContract_RenewalFailureStopsActiveExecution(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-renew-fail-")
	t.Chdir(root)
	initGitRepo(t, root)
	started := make(chan struct{})
	stopped := make(chan struct{})
	pulse := make(chan time.Time, 1)
	log := &spyEventLog{}
	factory := &controlledRunnableFactory{runnables: map[int]Runnable{42: waitingRunnableFunction(func(ctx context.Context) AgentRunResult {
		close(started)
		<-ctx.Done()
		close(stopped)
		return AgentRunResult{IssueNumber: 42, Status: "aborted", Branch: "42-work"}
	})}}
	cfg := &config.Config{Agent: "test", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", AgentProviders: map[string]config.Agent{"test": {Command: "true"}}}
	o := NewOrchestrator(&fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open"}}}, &noopRenderer{}, &fakeConfigStore{config: cfg}, log, WithErrorLog(io.Discard), WithRunnableFactory(factory), WithSandboxFactory(reviewWaitSchedulerSandboxFactory{}), WithRunSessionOpts(runSessionOptions{waitOwnerPulse: func(string) <-chan time.Time { return pulse }}))
	request := Request{Issues: []int{42}, RunTS: "261005120000", RunShortID: "renew", Branches: map[int]string{42: "42-work"}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	var result *Result
	go func() { defer close(done); result, _ = o.RunBatch(ctx, request) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("agent did not start")
	}
	layout := paths.NewLayout(cfg, root)
	path := filepath.Join(layout.StateDir, "waiting", buildRunID(42, request.RunTS, request.RunShortID)+".json")
	if err := os.WriteFile(path, []byte("invalid ownership checkpoint"), 0o600); err != nil {
		t.Fatal(err)
	}
	pulse <- time.Now()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("renewal failure did not cancel active execution")
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("renewal abort did not finish")
	}
	states := events.ProjectRunStates(log.snapshot())
	if result == nil || result.Runs[0].Status != "aborted" || len(states) != 1 || !states[0].IsTerminal() || states[0].Status() != "aborted" || len(factory.created) != 1 {
		t.Fatalf("lease failure retained execution: result=%+v states=%+v", result, states)
	}
}

func TestWaitingContract_ReviewerLaunchHistoryCannotPreemptApproval(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprintf("corrupt=%v", corrupt), func(t *testing.T) {
			root := testenv.MkdirShort(t, "sm-review-history-")
			t.Chdir(root)
			worktree := filepath.Join(root, "worktree")
			writeCurrentHeadApprovalClassification(t, worktree)
			data, err := os.ReadFile(filepath.Join(worktree, ".sandman", "state", "17.review_request.json"))
			if err != nil {
				t.Fatal(err)
			}
			var request reviewRequestEnvelope
			if err := json.Unmarshal(data, &request); err != nil {
				t.Fatal(err)
			}
			layout := paths.NewLayout(nil, root)
			if err := os.MkdirAll(layout.StateDir, 0o755); err != nil {
				t.Fatal(err)
			}
			key := sha256.Sum256([]byte(request.TriggerID + "\x00" + "current-sha"))
			legacy := fmt.Sprintf(`{"protocol":"review-launch/v1","pull_request":17,"trigger":%q,"head_sha":"current-sha","attempts":3}`, request.TriggerID)
			if corrupt {
				legacy = "obsolete corrupt launch budget"
			}
			if err := os.WriteFile(filepath.Join(layout.StateDir, fmt.Sprintf("17.review-launch-%x.json", key)), []byte(legacy), 0o600); err != nil {
				t.Fatal(err)
			}
			client := &reviewWaitSchedulerGitHubClient{comments: []github.PRComment{}, fakeGitHubClient: fakeGitHubClient{
				issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
				prs:    map[string]*github.PR{gateTestBranch: {Number: 17, State: "open", HeadRefName: gateTestBranch, HeadRefOid: "current-sha", StatusCheckRollup: "success", ReviewDecision: "APPROVED", MergeStateStatus: "CLEAN", Body: "Closes #42"}},
			}}
			launches := 0
			factory := &controlledRunnableFactory{runnables: map[int]Runnable{42: waitingRunnableFunction(func(context.Context) AgentRunResult {
				launches++
				client.setPR(gateTestBranch, func(pr *github.PR) { pr.State, pr.Merged = "merged", true })
				return AgentRunResult{IssueNumber: 42, Status: "success", Branch: gateTestBranch}
			})}}
			sbFactory := &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: worktree}}
			log := &spyEventLog{}
			o := NewOrchestrator(client, &noopRenderer{}, nil, log, WithErrorLog(io.Discard), WithRunnableFactory(factory), WithSandboxFactory(sbFactory), WithRunSessionOpts(gateTestRunOptions()))
			result, _ := o.newRunExecutor(context.Background(), BatchConfig{Cfg: &config.Config{WorktreeDir: "worktrees"}, AgentCfg: config.Agent{Command: "true"}, IdentityResolver: noopIdentityResolver()}, sbFactory, nil).Execute(context.Background(), RowSpec{
				IssueNumber: 42, Mode: ModeContinue, RunID: "approved", Branches: map[int]string{42: gateTestBranch}, BaseBranch: "main", RenderCfg: prompt.RenderConfig{ReviewCommand: "/sandman review"},
			})
			if result.Status != "success" || launches != 1 {
				t.Fatalf("launch history preempted approved merge work: result=%+v launches=%d events=%+v", result, launches, log.snapshot())
			}
		})
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
	busyStarted := make(chan struct{})
	factory := &controlledRunnableFactory{runnables: map[int]Runnable{
		44: waitingRunnableFunction(func(ctx context.Context) AgentRunResult {
			close(busyStarted)
			select {
			case <-log.limited:
			case <-ctx.Done():
				return AgentRunResult{Status: "aborted"}
			}
			return AgentRunResult{IssueNumber: 44, Status: "success", Branch: "44-busy"}
		}),
		42: waitingRunnableFunction(func(ctx context.Context) AgentRunResult {
			if attempts.Add(1) == 1 {
				// Occupy the other execution slot before closing quota admission.
				// Otherwise row 44 can be deferred too, leaving row 43 waiting
				// for 44 while quota recovery waits for row 43's deferral.
				select {
				case <-busyStarted:
				case <-ctx.Done():
					return AgentRunResult{Status: "aborted"}
				}
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
		// The scheduler has registered the quota pause before this callback.
		// run.await itself is emitted earlier, inside the executor.
		log.limitOnce.Do(func() { close(log.limited) })
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

type midRunQuotaRecovery struct {
	mu              sync.Mutex
	attempts        int
	progress        func()
	firstLimit      chan<- struct{}
	recoveryStarted chan<- struct{}
	recoveryDone    chan<- struct{}
	release         <-chan struct{}
	client          *reviewWaitSchedulerGitHubClient
	firstOnce       sync.Once
	doneOnce        sync.Once
}

func (r *midRunQuotaRecovery) setQuotaSignals(progress, _ func()) {
	r.mu.Lock()
	r.progress = progress
	r.mu.Unlock()
}

func (r *midRunQuotaRecovery) Run(ctx context.Context, _ prompt.IssueRenderer, _ string, _ prompt.RenderConfig) AgentRunResult {
	r.mu.Lock()
	r.attempts++
	attempt := r.attempts
	progress := r.progress
	r.mu.Unlock()
	if attempt == 1 {
		r.firstOnce.Do(func() { close(r.firstLimit) })
		return AgentRunResult{IssueNumber: 42, Branch: "42-recovery", Status: "failure", UsageLimitReached: true}
	}
	close(r.recoveryStarted)
	if progress != nil {
		progress()
	}
	select {
	case <-r.release:
		if r.client != nil {
			r.client.mu.Lock()
			r.client.prs["42-recovery"] = &github.PR{Number: 42, State: "merged", Merged: true, HeadRefName: "42-recovery", Body: "Closes #42"}
			r.client.mu.Unlock()
		}
		r.doneOnce.Do(func() { close(r.recoveryDone) })
		return AgentRunResult{IssueNumber: 42, Branch: "42-recovery", Status: "success"}
	case <-ctx.Done():
		return AgentRunResult{IssueNumber: 42, Branch: "42-recovery", Status: "aborted"}
	}
}

type waitForSignalRunnable struct {
	signal <-chan struct{}
}

func (r *waitForSignalRunnable) Run(ctx context.Context, _ prompt.IssueRenderer, _ string, _ prompt.RenderConfig) AgentRunResult {
	select {
	case <-r.signal:
		return AgentRunResult{Status: "success"}
	case <-ctx.Done():
		return AgentRunResult{Status: "aborted"}
	}
}

func TestWaitingContract_ModelProgressReadmitsSiblingBeforeRecoveryReturns(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-mid-quota-")
	t.Chdir(root)
	initGitRepo(t, root)
	client := &reviewWaitSchedulerGitHubClient{fakeGitHubClient: fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "closed"}, 43: {Number: 43, State: "closed"}, 44: {Number: 44, State: "closed"}},
		prs: map[string]*github.PR{
			"43-sibling": {Number: 43, State: "merged", Merged: true, HeadRefName: "43-sibling", Body: "Closes #43"},
			"44-busy":    {Number: 44, State: "merged", Merged: true, HeadRefName: "44-busy", Body: "Closes #44"},
		},
	}}
	firstLimit := make(chan struct{})
	recoveryStarted := make(chan struct{})
	recoveryDone := make(chan struct{})
	releaseRecovery := make(chan struct{})
	siblingStarted := make(chan struct{})
	recovery := &midRunQuotaRecovery{firstLimit: firstLimit, recoveryStarted: recoveryStarted, recoveryDone: recoveryDone, release: releaseRecovery, client: client}
	factory := &controlledRunnableFactory{runnables: map[int]Runnable{
		42: recovery,
		43: waitingRunnableFunction(func(context.Context) AgentRunResult {
			close(siblingStarted)
			return AgentRunResult{IssueNumber: 43, Branch: "43-sibling", Status: "success"}
		}),
		44: &waitForSignalRunnable{signal: firstLimit},
	}}
	cfg := &config.Config{Agent: "opencode", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"opencode": config.BuiltInAgentPresets["opencode"].Agent("opencode")}}
	log := &spyEventLog{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: cfg}, log, WithErrorLog(io.Discard), WithRunnableFactory(factory), WithSandboxFactory(&freshSandboxFactory{}), WithRunSessionOpts(runSessionOptions{releaseAwaitCapacity: true, awaitWait: func(context.Context, time.Duration) error { return nil }}))

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = o.RunBatch(context.Background(), Request{Issues: []int{44, 42, 43}, RunTS: "261007120000", RunShortID: "midquota", Parallel: 2, Branches: map[int]string{42: "42-recovery", 43: "43-sibling", 44: "44-busy"}, Dependencies: map[int][]int{43: {44}}})
	}()
	select {
	case <-recoveryStarted:
	case <-time.After(5 * time.Second):
		t.Fatalf("quota recovery did not re-enter; events=%v", log.snapshot())
	}
	select {
	case <-siblingStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("verified model progress did not readmit sibling before recovery returned")
	}
	select {
	case <-recoveryDone:
		t.Fatal("recovery returned before sibling was admitted")
	default:
	}
	close(releaseRecovery)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("batch did not finish after recovery release")
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
		Agent: "opencode", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{"opencode": config.BuiltInAgentPresets["opencode"].Agent("opencode")},
	}}, log, WithErrorLog(io.Discard), WithRunnableFactory(factory), WithSandboxFactory(&freshSandboxFactory{}),
		WithRunSessionOpts(runSessionOptions{startWaiterQueued: func(bool) {
			<-limitedStarted
			<-busyStarted
			releaseOnce.Do(func() { close(releaseLimit) })
		}}))
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer close(done)
		_, _ = o.RunBatch(ctx, Request{Issues: []int{42, 43, 44}, Branches: map[int]string{42: "42-limit", 43: "43-busy", 44: "44-next"}, Parallel: 2})
	}()
	// Persistence of the rejected admission is the deterministic boundary;
	// release the unrelated occupied slot only once the paused row is recorded.
	deadline := time.Now().Add(3 * time.Second)
	for countEventsByType(log.snapshot(), "run.capacity_queued") == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if countEventsByType(log.snapshot(), "run.capacity_queued") == 0 {
		cancel()
		close(releaseBusy)
		t.Fatal("active quota pause did not reject the queued start")
	}
	cancel()
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

func TestWaitingContract_ConsumedQuotaPollingStillPermitsOrdinaryRetries(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-wait-")
	t.Chdir(root)
	now := time.Now().UTC()
	log := &spyEventLog{events: []events.Event{
		{Type: "run.started", RunID: "row", Issue: 42},
		{Type: "run.await", RunID: "row", Issue: 42, Payload: map[string]any{"await_reason": "usage-limit", "usage_limit_waited_seconds": 18000, "usage_limit_deadline_unix_seconds": now.Add(-time.Minute).Unix()}},
	}}
	factory := &controlledRunnableFactory{runnables: map[int]Runnable{42: &controlledRunnable{result: AgentRunResult{IssueNumber: 42, Branch: "42-limit", Status: "failure", UsageLimitReached: true}}}}
	o := NewOrchestrator(&fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "closed"}}}, &noopRenderer{}, nil, log, WithErrorLog(io.Discard), WithRunnableFactory(factory), WithSandboxFactory(&freshSandboxFactory{}))
	e := o.newRunExecutor(context.Background(), BatchConfig{
		Cfg: &config.Config{}, AgentCfg: config.BuiltInAgentPresets["opencode"].Agent("opencode"), IdentityResolver: noopIdentityResolver(), Retries: 1,
	}, &freshSandboxFactory{}, nil)
	result, _ := e.Execute(context.Background(), RowSpec{IssueNumber: 42, RunID: "row", Mode: ModeContinue, UsageLimitProbe: true, Branches: map[int]string{42: "42-limit"}})
	if result.Status != "failure" || len(factory.created) != 2 || !result.UsageLimitReached || countEventsByType(log.snapshot(), "run.await") != 1 || countEventsByType(log.snapshot(), "run.retry") != 1 {
		t.Fatalf("consumed polling renewed waiting or vetoed ordinary retries: result=%#v starts=%v events=%+v", result, factory.created, log.snapshot())
	}
}

func TestWaitingContract_ExpiredQuotaProbeStillFinishesVerifiedMerge(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-wait-")
	t.Chdir(root)
	now := time.Now().UTC()
	branch := "42-merged-quota"
	log := &spyEventLog{}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
		prs: map[string]*github.PR{branch: {
			Number:      17,
			State:       "merged",
			Merged:      true,
			Body:        "Closes #42",
			HeadRefName: branch,
			HeadRefOid:  "current-sha",
		}},
	}
	factory := &controlledRunnableFactory{}
	o := NewOrchestrator(client, &noopRenderer{}, nil, log,
		WithErrorLog(io.Discard),
		WithRunnableFactory(factory),
		WithSandboxFactory(&freshSandboxFactory{}),
		WithRunSessionOpts(runSessionOptions{currentHead: func(string) (string, error) { return "current-sha", nil }}),
	)
	e := o.newRunExecutor(context.Background(), BatchConfig{
		Cfg:      &config.Config{},
		AgentCfg: config.BuiltInAgentPresets["opencode"].Agent("opencode"),
	}, &freshSandboxFactory{}, nil)
	result, started := e.Execute(context.Background(), RowSpec{
		IssueNumber:        42,
		RunID:              "row",
		Mode:               ModeContinue,
		UsageLimitProbe:    true,
		UsageLimitDeadline: now.Add(-time.Minute),
		Branches:           map[int]string{42: branch},
	})
	if started || result.Status != "success" {
		t.Fatalf("expired quota probe = (started=%t, result=%#v), want observation-only success", started, result)
	}
	if len(factory.created) != 0 {
		t.Fatalf("expired quota probe launched agents: %v", factory.created)
	}
	if got := countEventsByType(log.snapshot(), "run.finished"); got != 1 {
		t.Fatalf("run.finished events = %d, want one terminal observation", got)
	}
}
