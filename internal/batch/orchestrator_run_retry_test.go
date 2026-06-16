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
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

func TestRunSingle_EmitsRunRetryBetweenAttemptsOnFailure(t *testing.T) {
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
		workDir:    filepath.Join(workDir, "worktree"),
		execErrors: []error{errors.New("exit 1"), nil},
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
	}
	var resetCalls int
	o.runSessionOpts.retryReset = func(ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error {
		resetCalls++
		// Mark the PR as merged on the post-attempt check so the second
		// attempt is reported as success (matching the existing
		// TestRunSingle_RetriesResetBranchAndRerender fixture).
		if resetCalls == 1 {
			pr.Merged = true
		}
		return nil
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, &retrySandboxFactory{sandbox: rtSandbox}, nil, false, "main", nil, 0, 0, 2, 0, "", 0, false, 0, false, false, false)
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success", result.Status)
	}
	if result.RetriesTotal != 2 {
		t.Fatalf("RetriesTotal = %d, want 2", result.RetriesTotal)
	}

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
		t.Fatalf("expected exactly 1 run.retry event (3 attempts with 1→2 transition only), got %d (events: %v)", len(retryEvents), logs)
	}
	got := retryEvents[0]
	if got.RunID == "" {
		t.Errorf("run.retry RunID = empty, want non-empty")
	}
	if got.Issue != 42 {
		t.Errorf("run.retry Issue = %d, want 42", got.Issue)
	}
	if got.IssueRef == nil || *got.IssueRef != 42 {
		t.Errorf("run.retry IssueRef = %v, want pointer to 42", got.IssueRef)
	}
	// JSON numbers round-trip as float64; compare via float-cast.
	if attempt, _ := got.Payload["attempt"].(float64); attempt != 2 {
		t.Errorf("run.retry attempt = %v, want 2", got.Payload["attempt"])
	}
	if maxAttempts, _ := got.Payload["max_attempts"].(float64); maxAttempts != 3 {
		t.Errorf("run.retry max_attempts = %v, want 3", got.Payload["max_attempts"])
	}
	if got.Payload["previous_status"] != "failure" {
		t.Errorf("run.retry previous_status = %v, want \"failure\"", got.Payload["previous_status"])
	}
	if got.Payload["branch"] != branch {
		t.Errorf("run.retry branch = %v, want %q", got.Payload["branch"], branch)
	}
	if lines, _ := got.Payload["last_log_lines"].([]any); len(lines) == 0 {
		t.Errorf("run.retry last_log_lines is empty, want non-empty")
	}

	// Verify ordering: run.started, run.retry, run.finished (in that order
	// for the same RunID).
	var types []string
	for _, e := range logs {
		if e.RunID == got.RunID {
			types = append(types, e.Type)
		}
	}
	wantOrder := []string{"run.started", "run.retry", "run.finished"}
	if len(types) != len(wantOrder) {
		t.Fatalf("event types for RunID %q = %v, want %v", got.RunID, types, wantOrder)
	}
	for i, w := range wantOrder {
		if types[i] != w {
			t.Fatalf("event types for RunID %q = %v, want %v (mismatch at index %d)", got.RunID, types, wantOrder, i)
		}
	}
}

// TestRunSingle_EmitsRunRetryWithAbortedStatusAfterHeartbeatKill asserts that
// when the heartbeat kills the first attempt (flipping its status to
// "aborted"), the run.retry event captures previous_status: "aborted" — the
// value withHeartbeat writes after the status flip. This pins the
// integration with the existing heartbeat.
func TestRunSingle_EmitsRunRetryWithAbortedStatusAfterHeartbeatKill(t *testing.T) {
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

	logPath := filepath.Join(workDir, ".sandman", "logs", "42.log")
	stall := &heartbeatStallRunnable{logPath: logPath, proc: proc}
	success := &fakeRunnable{result: AgentRunResult{IssueNumber: heartbeatTestIssueNum, Status: "success", Branch: branch}}
	runFactory := &heartbeatDualRunnableFactory{first: stall, second: success}

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			heartbeatTestIssueNum: {Number: heartbeatTestIssueNum, Title: "Fix bug"},
		},
		// PR is NOT merged at start, so the pre-retry guard on attempt 1
		// does not short-circuit the retry.
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
	}
	o.runSessionOpts.killTimeout = 50 * time.Millisecond

	result, started := o.runSingle(context.Background(), context.Background(), heartbeatTestIssueNum, cfg, "test-agent", config.Agent{Command: "true"}, false, nil, noopIdentityResolver(), map[int]string{heartbeatTestIssueNum: branch}, prompt.RenderConfig{}, nil, factory, nil, false, "main", nil, 0, 0, 1, heartbeatTestIdle, "", 0, false, 0, false, false, false)
	if !started {
		t.Fatal("expected run to start")
	}
	if !proc.killObserved() {
		t.Error("expected process.Kill to be called by heartbeat on first attempt")
	}
	if result.RetriesTotal != 2 {
		t.Errorf("RetriesTotal = %d, want 2 (one stalled + one success)", result.RetriesTotal)
	}

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
	got := retryEvents[0]
	if got.Payload["previous_status"] != "aborted" {
		t.Errorf("run.retry previous_status = %v, want \"aborted\" (heartbeat flips the status after killing the process)", got.Payload["previous_status"])
	}
	if attempt, _ := got.Payload["attempt"].(float64); attempt != 2 {
		t.Errorf("run.retry attempt = %v, want 2", got.Payload["attempt"])
	}
	if maxAttempts, _ := got.Payload["max_attempts"].(float64); maxAttempts != 2 {
		t.Errorf("run.retry max_attempts = %v, want 2", got.Payload["max_attempts"])
	}
}

// TestRunSingle_EmitsZeroRunRetryEventsOnSingleAttempt asserts that a run
// configured with retries=0 emits zero run.retry events (the terminal
// run.finished covers the single attempt).
func TestRunSingle_EmitsZeroRunRetryEventsOnSingleAttempt(t *testing.T) {
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
	rtSandbox := &retrySandbox{workDir: filepath.Join(workDir, "worktree")}
	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })
	pr := &github.PR{Number: 17, State: "open", Merged: false, HeadRefName: branch}
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
		}},
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, &retrySandboxFactory{sandbox: rtSandbox}, nil, false, "main", nil, 0, 0, 0, 0, "", 0, false, 0, false, false, false)
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "failure" {
		t.Fatalf("status = %q, want failure", result.Status)
	}

	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	var retryCount int
	var foundFinished bool
	for _, e := range logs {
		if e.Type == "run.retry" {
			retryCount++
		}
		if e.Type == "run.finished" {
			foundFinished = true
		}
	}
	if retryCount != 0 {
		t.Errorf("expected 0 run.retry events for a 1-attempt run, got %d (events: %v)", retryCount, logs)
	}
	if !foundFinished {
		t.Errorf("expected run.finished event in event log")
	}
}

// TestRunPromptOnly_EmitsRunRetryBetweenAttemptsOnFailure asserts that the
// prompt-only retry loop emits run.retry with Issue: 0 and IssueRef: nil,
// matching the existing prompt-only convention for run.started/run.finished.
func TestRunPromptOnly_EmitsRunRetryBetweenAttemptsOnFailure(t *testing.T) {
	workDir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get wd: %v", err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	branch := "sandman/prompt-only-123"
	rtSandbox := &retrySandbox{workDir: filepath.Join(workDir, "worktree"), execErrors: []error{errors.New("exit 1"), nil}}
	eventsPath := filepath.Join(t.TempDir(), "events.jsonl")
	eventLog := &events.JSONLLogger{Path: eventsPath}
	o := &Orchestrator{
		renderer: &retryRenderer{result: "rendered prompt"},
		errorLog: io.Discard,
		layout:   paths.NewLayout(&config.Config{}, workDir),
		eventLog: eventLog,
		sandboxFactory: &retrySandboxFactory{
			sandbox: rtSandbox,
		},
	}
	o.runSessionOpts.retryReset = func(ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error {
		return nil
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runPromptOnlySingle(context.Background(), cfg, "opencode", config.Agent{Command: "echo hi"}, noopIdentityResolver(), branch, prompt.RenderConfig{}, nil, &retrySandboxFactory{sandbox: rtSandbox}, nil, ModeFresh, "main", 0, 0, 2, "", 0, false, 0, false, false, false, false, 0, "", "run-prompt-123", nil)
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success", result.Status)
	}

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
	got := retryEvents[0]
	if got.Issue != 0 {
		t.Errorf("run.retry Issue = %d, want 0 (prompt-only)", got.Issue)
	}
	if got.IssueRef != nil {
		t.Errorf("run.retry IssueRef = %v, want nil (prompt-only)", got.IssueRef)
	}
	if attempt, _ := got.Payload["attempt"].(float64); attempt != 2 {
		t.Errorf("run.retry attempt = %v, want 2", got.Payload["attempt"])
	}
	if maxAttempts, _ := got.Payload["max_attempts"].(float64); maxAttempts != 3 {
		t.Errorf("run.retry max_attempts = %v, want 3", got.Payload["max_attempts"])
	}
	if got.Payload["previous_status"] != "failure" {
		t.Errorf("run.retry previous_status = %v, want \"failure\"", got.Payload["previous_status"])
	}
}
