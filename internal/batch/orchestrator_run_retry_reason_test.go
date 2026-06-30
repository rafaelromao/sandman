package batch

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

// retryReasonVocabulary is the closed set of reason values that the
// orchestrator's run.retry emit may use. The set is the slice-3 starting
// set documented in ADR-0035; the helpers in this file assert that every
// retry event observed on disk carries a reason drawn from this set.
var retryReasonVocabulary = map[string]struct{}{
	"agent-stalled":   {},
	"agent-failed":    {},
	"sandbox-timeout": {},
	"kill-timeout":    {},
	"manual":          {},
}

// retryReasonForRun reads the on-disk event log, returns every run.retry
// event for the given runID, and asserts each carries a non-empty
// payload["reason"] drawn from retryReasonVocabulary. The helper
// collapses the boilerplate that every test in this file repeats so
// failures point at the property under test, not at the read path.
func retryReasonForRun(t *testing.T, eventLog *events.JSONLLogger, runID string) []any {
	t.Helper()
	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	var reasons []any
	for _, e := range logs {
		if e.Type != "run.retry" {
			continue
		}
		if runID != "" && e.RunID != runID {
			continue
		}
		raw, ok := e.Payload["reason"]
		if !ok || raw == nil {
			t.Fatalf("run.retry event for run %q missing reason key in payload %+v", e.RunID, e.Payload)
		}
		reason, _ := raw.(string)
		if reason == "" {
			t.Fatalf("run.retry event for run %q has empty reason string in payload %+v", e.RunID, e.Payload)
		}
		if _, ok := retryReasonVocabulary[reason]; !ok {
			t.Fatalf("run.retry event for run %q has reason %q outside the closed vocabulary %v", e.RunID, reason, retryReasonVocabulary)
		}
		reasons = append(reasons, reason)
	}
	return reasons
}

// TestRunSingle_EmitsRunRetryWithAgentFailedReason asserts that when the
// previous attempt ended with Status="failure" (agent exited non-zero,
// heartbeat did NOT trip), the orchestrator's run.retry emit carries
// reason: "agent-failed". This pins the agent-failed arm of the closed
// vocabulary to the existing failure path the orchestrator already
// exercises in TestRunSingle_EmitsRunRetryBetweenAttemptsOnFailure.
func TestRunSingle_EmitsRunRetryWithAgentFailedReason(t *testing.T) {
	workDir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get wd: %v", err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	branch := "sandman/42-fix-bug"
	rtSandbox := &retrySandbox{
		workDir: filepath.Join(workDir, "worktree"),
	}
	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })

	pr := &github.PR{Number: 17, State: "closed", Merged: false, HeadRefName: branch}
	eventsPath := filepath.Join(t.TempDir(), "events.jsonl")
	eventLog := &events.JSONLLogger{Path: eventsPath}
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: pr},
		},
		renderer: &retryRenderer{result: "rendered prompt"},
		errorLog: io.Discard,
		layout:   paths.NewLayout(&config.Config{}, workDir),
		eventLog: eventLog,
		sandboxFactory: &retrySandboxFactory{
			sandbox: rtSandbox,
		},
		runnableFactory: &fakeRunnableFactory{results: []AgentRunResult{
			{IssueNumber: 42, Status: "failure", Branch: branch},
			{IssueNumber: 42, Status: "failure", Branch: branch},
			{IssueNumber: 42, Status: "success", Branch: branch},
		}},
	}
	o.runSessionOpts.retryReset = func(ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error {
		return nil
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	_, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, &retrySandboxFactory{sandbox: rtSandbox}, nil, false, "main", nil, 0, 0, 2, 0, "", 0, false, 0, false, false, false, "", "")
	if !started {
		t.Fatal("expected run to start")
	}

	reasons := retryReasonForRun(t, eventLog, "")
	if len(reasons) != 2 {
		t.Fatalf("expected 2 run.retry events, got %d", len(reasons))
	}
	for i, r := range reasons {
		if r != "agent-failed" {
			t.Errorf("run.retry[%d].reason = %v, want \"agent-failed\"", i, r)
		}
	}
}

// TestRunSingle_EmitsRunRetryWithAgentStalledReason asserts that when the
// heartbeat watchdog killed the previous attempt (withHeartbeat rewrites
// result.Status to "aborted" and abortedByHeartbeat is true), the
// orchestrator's run.retry emit carries reason: "agent-stalled". This
// pins the agent-stalled arm of the closed vocabulary to the existing
// heartbeat path the orchestrator already exercises in
// TestRunSingle_EmitsRunRetryWithAbortedStatusAfterHeartbeatKill.
func TestRunSingle_EmitsRunRetryWithAgentStalledReason(t *testing.T) {
	workDir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get wd: %v", err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	initGitRepo(t, workDir)

	branch := "sandman/42-fix-bug"
	worktreePath := filepath.Join(workDir, heartbeatTestWorktreeDir, "sandman", "42-fix-bug")
	proc := &fakeProcess{killed: make(chan struct{})}
	sb := &fakeSandbox{workDir: worktreePath, process: proc}
	factory := &fakeSandboxFactory{sandbox: sb}

	logPath := filepath.Join(workDir, ".sandman", "batches", "68cb-260622105532", "runs", "68cb-260622105532-42", "run.log")
	stall := &heartbeatStallRunnable{logPath: logPath, proc: proc}
	success := &fakeRunnable{result: AgentRunResult{IssueNumber: heartbeatTestIssueNum, Status: "success", Branch: branch}}
	runFactory := &heartbeatDualRunnableFactory{first: stall, second: success}

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			heartbeatTestIssueNum: {Number: heartbeatTestIssueNum, Title: "Fix bug"},
		},
		prs: map[string]*github.PR{heartbeatTestBranch: {Number: 17, State: "open", Merged: false, HeadRefName: heartbeatTestBranch}},
	}

	eventsPath := filepath.Join(t.TempDir(), "events.jsonl")
	eventLog := &events.JSONLLogger{Path: eventsPath}
	cfg := &config.Config{
		Agent:          "test-agent",
		Sandbox:        "worktree",
		WorktreeDir:    heartbeatTestWorktreeDir,
		Git:            config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}},
	}
	o := &Orchestrator{
		githubClient:          client,
		renderer:              &noopRenderer{},
		configStore:           &fakeConfigStore{config: cfg},
		eventLog:              eventLog,
		sandboxFactory:        factory,
		runnableFactory:       runFactory,
		heartbeatTickInterval: heartbeatTestTick,
		errorLog:              io.Discard,
		layout:                paths.NewLayout(cfg, workDir),
	}
	o.runSessionOpts.killTimeout = 50 * time.Millisecond

	_, started := o.runSingle(context.Background(), context.Background(), heartbeatTestIssueNum, cfg, "test-agent", config.Agent{Command: "true"}, false, nil, noopIdentityResolver(), map[int]string{heartbeatTestIssueNum: branch}, prompt.RenderConfig{}, nil, factory, nil, false, "main", nil, 0, 0, 1, heartbeatTestIdle, "", 0, false, 0, false, false, false, "260622105532", "68cb")
	if !started {
		t.Fatal("expected run to start")
	}
	if !proc.killObserved() {
		t.Error("expected process.Kill to be called by heartbeat on first attempt")
	}

	reasons := retryReasonForRun(t, eventLog, "")
	if len(reasons) != 1 {
		t.Fatalf("expected 1 run.retry event, got %d", len(reasons))
	}
	if reasons[0] != "agent-stalled" {
		t.Errorf("run.retry[0].reason = %v, want \"agent-stalled\" (heartbeat killed previous attempt)", reasons[0])
	}
}

// TestRunSingle_EmitsRunRetryWithKillTimeoutReasonOnParentCtxCancel
// asserts that when the parent context is cancelled (the previous
// attempt saw ctx.Err() before exiting, status flipped to "aborted",
// heartbeat did NOT trip), the orchestrator's run.retry emit carries
// reason: "kill-timeout". The prompt-only retry loop is used so the
// post-attempt abort short-circuit on the mergeRequired branch does
// not skip the retry emit.
func TestRunSingle_EmitsRunRetryWithKillTimeoutReasonOnParentCtxCancel(t *testing.T) {
	workDir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get wd: %v", err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	initGitRepo(t, workDir)

	branch := "sandman/prompt-only-kill-timeout"
	rtSandbox := &retrySandbox{workDir: filepath.Join(workDir, "worktree")}

	// First runnable blocks until the context is cancelled, then
	// returns an aborted AgentRunResult. Second runnable returns success so
	// the loop ends.
	proc := &fakeProcess{killed: make(chan struct{})}

	eventsPath := filepath.Join(t.TempDir(), "events.jsonl")
	eventLog := &events.JSONLLogger{Path: eventsPath}
	cfg := &config.Config{
		Agent:          "test-agent",
		Sandbox:        "worktree",
		WorktreeDir:    heartbeatTestWorktreeDir,
		Git:            config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}},
	}
	o := &Orchestrator{
		renderer: &retryRenderer{result: "rendered prompt"},
		errorLog: io.Discard,
		layout:   paths.NewLayout(cfg, workDir),
		eventLog: eventLog,
		sandboxFactory: &retrySandboxFactory{
			sandbox: rtSandbox,
		},
	}
	o.runSessionOpts.retryReset = func(ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error {
		return nil
	}
	// Override the process so the first runnable can be killed.
	first := &ctxCancelRunnable{proc: proc}
	second := &fakeRunnable{result: AgentRunResult{Status: "success", Branch: branch}}
	o.runnableFactory = &dualRunnableFactory{first: first, second: second}

	parentCtx, cancelParent := context.WithCancel(context.Background())
	runCtx, cancelRun := context.WithCancel(parentCtx)

	// Run the retry loop in a goroutine; cancel the parent ctx after a
	// short delay so the first attempt sees ctx.Err() and returns
	// aborted. The second attempt runs with the parent ctx still
	// cancelled; the orchestrator must surface kill-timeout at the
	// logRetry call site.
	done := make(chan struct{})
	var result AgentRunResult
	var started bool
	go func() {
		defer close(done)
		result, started = o.runPromptOnlySingle(runCtx, cfg, "opencode", config.Agent{Command: "echo hi"}, noopIdentityResolver(), branch, prompt.RenderConfig{}, nil, &retrySandboxFactory{sandbox: rtSandbox}, nil, ModeFresh, "main", 0, 0, 2, "", 0, false, 0, false, false, false, false, 0, "", "run-kill-timeout-1", nil, 0, "", "")
	}()

	// Give the first runnable time to start, then cancel the parent
	// ctx. The first runnable observes the cancellation and returns
	// aborted; the orchestrator writes run.retry with reason
	// kill-timeout because the parent ctx (not the heartbeat) tripped.
	time.Sleep(100 * time.Millisecond)
	cancelParent()
	cancelRun()
	<-done

	if !started {
		t.Fatalf("expected run to start, result.Status=%q", result.Status)
	}

	// Read the events; the run.retry for the first→second transition
	// should carry reason: "kill-timeout".
	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	var retryEvents []events.Event
	for _, e := range logs {
		if e.Type == "run.retry" {
			retryEvents = append(retryEvents, e)
		}
	}
	if len(retryEvents) != 1 {
		t.Fatalf("expected exactly 1 run.retry event, got %d (events: %v)", len(retryEvents), logs)
	}
	raw, ok := retryEvents[0].Payload["reason"]
	if !ok {
		t.Fatalf("run.retry payload missing reason key, got %+v", retryEvents[0].Payload)
	}
	reason, _ := raw.(string)
	if reason != "kill-timeout" {
		t.Errorf("run.retry reason = %q, want \"kill-timeout\" (parent ctx cancelled previous attempt)", reason)
	}
}

// --- fakes used only by the reason-vocabulary tests ---

// ctxCancelRunnable blocks until the context is cancelled, then
// returns an aborted AgentRunResult. It mirrors the production shape
// of an AgentRun that observes ctx.Done() and bails out — the
// orchestrator surfaces that result via withHeartbeat's post-flight
// to the kill-timeout arm of the reason vocabulary.
type ctxCancelRunnable struct {
	proc *fakeProcess
}

func (r *ctxCancelRunnable) Run(ctx context.Context, _ prompt.IssueRenderer, _ string, _ prompt.RenderConfig) AgentRunResult {
	<-ctx.Done()
	if r.proc != nil && r.proc.killed != nil {
		select {
		case r.proc.killed <- struct{}{}:
		default:
		}
	}
	return AgentRunResult{Status: "aborted", Branch: "sandman/prompt-only-kill-timeout"}
}

// dualRunnableFactory is a RunnableFactory that returns the first
// runnable on the first call and the second on every subsequent call.
// It lets a test drive a two-attempt retry loop where the two attempts
// have different behaviour.
type dualRunnableFactory struct {
	first  Runnable
	second Runnable
	used   bool
}

func (f *dualRunnableFactory) NewRunnable(issue *github.Issue, branch string, sb sandbox.Sandbox) Runnable {
	if !f.used {
		f.used = true
		return f.first
	}
	return f.second
}
