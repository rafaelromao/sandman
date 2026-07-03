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
		workDir: filepath.Join(workDir, "worktree"),
	}
	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })
	// PR stays unmerged for the whole run. The orchestrator's post-attempt
	// check flips the result to "failure" whenever pr.Merged is false; this
	// is expected behaviour for an issue-driven run where the PR is the
	// success signal. The third runnable returns success but the
	// post-attempt check still flips it to "failure" because the PR is not
	// merged. The point of this test is to count run.retry events, not to
	// validate the success/failure signal.
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
	// Override retryReset so the orchestrator's resetRetryBranch does not
	// try to run `git reset --hard` on the retrySandbox (which would
	// return an error from its execErrors slice and short-circuit the
	// loop with a failure errResult).
	o.runSessionOpts.retryReset = func(ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error {
		return nil
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	resultFactory := o.runnableFactory.(*fakeRunnableFactory)
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, &retrySandboxFactory{sandbox: rtSandbox}, nil, false, "main", nil, 0, 0, 2, 0, "", 0, false, 0, false, false, false, "", "")
	if !started {
		t.Fatalf("expected run to start, result.Status=%q, created=%d", result.Status, len(resultFactory.created))
	}
	// The PR is unmerged, so the post-attempt check after the third
	// (success) attempt flips result.Status to "failure". This is the
	// expected end-state for an issue-driven run with an unmerged PR.
	if result.Status != "failure" {
		t.Fatalf("status = %q, want failure (unmerged PR forces failure regardless of agent zero exit)", result.Status)
	}
	if result.RetriesTotal != 3 {
		t.Fatalf("RetriesTotal = %d, want 3 (3 attempts: fail, fail, succeed)", result.RetriesTotal)
	}
	if len(resultFactory.created) != 3 {
		t.Fatalf("created runnables = %d, want 3 (3 attempts)", len(resultFactory.created))
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
	if len(retryEvents) != 2 {
		t.Fatalf("expected exactly 2 run.retry events (1→2 and 2→3 transitions for a 3-attempt run), got %d (events: %v)", len(retryEvents), logs)
	}

	// First retry: 1→2 transition, previous_status=failure (attempt 0 failed).
	got0 := retryEvents[0]
	if got0.Issue != 42 {
		t.Errorf("run.retry[0] Issue = %d, want 42", got0.Issue)
	}
	if got0.IssueRef == nil || *got0.IssueRef != 42 {
		t.Errorf("run.retry[0] IssueRef = %v, want pointer to 42", got0.IssueRef)
	}
	if attempt, _ := got0.Payload["attempt"].(float64); attempt != 2 {
		t.Errorf("run.retry[0] attempt = %v, want 2", got0.Payload["attempt"])
	}
	if maxAttempts, _ := got0.Payload["max_attempts"].(float64); maxAttempts != 3 {
		t.Errorf("run.retry[0] max_attempts = %v, want 3", got0.Payload["max_attempts"])
	}
	if got0.Payload["previous_status"] != "failure" {
		t.Errorf("run.retry[0] previous_status = %v, want \"failure\"", got0.Payload["previous_status"])
	}
	if got0.Payload["branch"] != branch {
		t.Errorf("run.retry[0] branch = %v, want %q", got0.Payload["branch"], branch)
	}
	// Last 2 log lines at the 1→2 transition: --- run 1/3 ---, --- retry 2/3 ---
	lines0, _ := got0.Payload["last_log_lines"].([]any)
	wantLines0 := []string{"--- run 1/3 ---", "--- retry 2/3 ---"}
	if len(lines0) != len(wantLines0) {
		t.Errorf("run.retry[0] last_log_lines = %v (len=%d), want %v", lines0, len(lines0), wantLines0)
	} else {
		for i, want := range wantLines0 {
			if got, _ := lines0[i].(string); got != want {
				t.Errorf("run.retry[0] last_log_lines[%d] = %q, want %q", i, got, want)
			}
		}
	}

	// Second retry: 2→3 transition, previous_status=failure (attempt 1 failed).
	got1 := retryEvents[1]
	if got1.RunID != got0.RunID {
		t.Errorf("run.retry[1] RunID = %q, want %q (same RunID across both events)", got1.RunID, got0.RunID)
	}
	if attempt, _ := got1.Payload["attempt"].(float64); attempt != 3 {
		t.Errorf("run.retry[1] attempt = %v, want 3", got1.Payload["attempt"])
	}
	if maxAttempts, _ := got1.Payload["max_attempts"].(float64); maxAttempts != 3 {
		t.Errorf("run.retry[1] max_attempts = %v, want 3", got1.Payload["max_attempts"])
	}
	if got1.Payload["previous_status"] != "failure" {
		t.Errorf("run.retry[1] previous_status = %v, want \"failure\"", got1.Payload["previous_status"])
	}
	// At the 2→3 transition the log has 3 lines: the run marker from
	// attempt 0 and the two retry markers from attempts 1 and 2.
	// readTailLines keeps the last 3 lines, so all three are returned.
	lines1, _ := got1.Payload["last_log_lines"].([]any)
	wantLines1 := []string{"--- run 1/3 ---", "--- retry 2/3 ---", "--- retry 3/3 ---"}
	if len(lines1) != len(wantLines1) {
		t.Errorf("run.retry[1] last_log_lines = %v (len=%d), want %v", lines1, len(lines1), wantLines1)
	} else {
		for i, want := range wantLines1 {
			if got, _ := lines1[i].(string); got != want {
				t.Errorf("run.retry[1] last_log_lines[%d] = %q, want %q", i, got, want)
			}
		}
	}

	// Verify ordering: run.started, run.retry (1→2), run.retry (2→3), run.finished.
	var types []string
	for _, e := range logs {
		if e.RunID == got0.RunID {
			types = append(types, e.Type)
		}
	}
	wantOrder := []string{"run.started", "run.retry", "run.retry", "run.finished"}
	if len(types) != len(wantOrder) {
		t.Fatalf("event types for RunID %q = %v, want %v", got0.RunID, types, wantOrder)
	}
	for i, w := range wantOrder {
		if types[i] != w {
			t.Fatalf("event types for RunID %q = %v, want %v (mismatch at index %d)", got0.RunID, types, wantOrder, i)
		}
	}
}

func TestRunSingle_AlreadyResolvedTaskMarkerShortCircuitsToSuccess(t *testing.T) {
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

	oldLookup := lookupOpenPRFn
	lookupOpenPRFn = func(string) (bool, int, string, error) {
		return false, 0, "", nil
	}
	t.Cleanup(func() { lookupOpenPRFn = oldLookup })

	eventsPath := filepath.Join(t.TempDir(), "events.jsonl")
	eventLog := &events.JSONLLogger{Path: eventsPath}
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{},
		},
		renderer: &retryRenderer{result: "rendered prompt"},
		errorLog: io.Discard,
		layout:   paths.NewLayout(&config.Config{}, workDir),
		eventLog: eventLog,
		sandboxFactory: &retrySandboxFactory{
			sandbox: rtSandbox,
		},
		runnableFactory: &taskWritingRunnableFactory{
			taskPath:    filepath.Join(workDir, "worktree", ".sandman", "task.md"),
			result:      AgentRunResult{IssueNumber: 42, Status: "failure", Branch: branch},
			taskContent: "## Status: already resolved",
		},
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, &retrySandboxFactory{sandbox: rtSandbox}, nil, false, "main", nil, 0, 0, 3, 0, "", 0, false, 0, false, false, false, "", "")
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success", result.Status)
	}
	if result.RetriesTotal != 1 {
		t.Fatalf("retries total = %d, want 1", result.RetriesTotal)
	}
	if got := o.runnableFactory.(*taskWritingRunnableFactory).created; got != 1 {
		t.Fatalf("runnable launches = %d, want 1", got)
	}
}

func TestRunSingle_AlreadyResolvedOpenPREndsFailure(t *testing.T) {
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

	oldLookup := lookupOpenPRFn
	lookupOpenPRFn = func(string) (bool, int, string, error) {
		return true, 17, "MERGEABLE", nil
	}
	t.Cleanup(func() { lookupOpenPRFn = oldLookup })

	eventsPath := filepath.Join(t.TempDir(), "events.jsonl")
	eventLog := &events.JSONLLogger{Path: eventsPath}
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: {Number: 17, State: "open", Merged: false, HeadRefName: branch}},
		},
		renderer: &retryRenderer{result: "rendered prompt"},
		errorLog: io.Discard,
		layout:   paths.NewLayout(&config.Config{}, workDir),
		eventLog: eventLog,
		sandboxFactory: &retrySandboxFactory{
			sandbox: rtSandbox,
		},
		runnableFactory: &taskWritingRunnableFactory{
			taskPath:    filepath.Join(workDir, "worktree", ".sandman", "task.md"),
			result:      AgentRunResult{IssueNumber: 42, Status: "failure", Branch: branch},
			taskContent: "## Status: already resolved",
		},
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, &retrySandboxFactory{sandbox: rtSandbox}, nil, false, "main", nil, 0, 0, 3, 0, "", 0, false, 0, false, false, false, "", "")
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "failure" {
		t.Fatalf("status = %q, want failure (alreadyResolved + open PR should be failure, not success)", result.Status)
	}

	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	var terminalPayload map[string]any
	for _, e := range logs {
		if e.Type == "run.finished" {
			terminalPayload = e.Payload
		}
	}
	if terminalPayload == nil {
		t.Fatalf("run.finished event not found in logs: %v", logs)
	}
	if blocker, _ := terminalPayload["blocker"].(string); blocker != "open-pr-blocks-already-resolved" {
		t.Fatalf("run.finished payload blocker = %q, want %q (payload=%v)", blocker, "open-pr-blocks-already-resolved", terminalPayload)
	}
	prNumber, ok := terminalPayload["pr_number"].(float64)
	if !ok {
		t.Fatalf("run.finished payload pr_number has wrong type %T, want number", terminalPayload["pr_number"])
	}
	if prNumber != 17 {
		t.Fatalf("run.finished payload pr_number = %v, want 17", prNumber)
	}
}

// TestRunSingle_AlreadyResolvedConflictingPREndsFailure exercises the
// Guard A path when the open PR is in CONFLICTING state. The state
// doesn't change the guard's behaviour: any open PR blocks the
// alreadyResolved short-circuit. The run must end status=failure with
// the same blocker payload as the MERGEABLE case.
func TestRunSingle_AlreadyResolvedConflictingPREndsFailure(t *testing.T) {
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

	oldLookup := lookupOpenPRFn
	lookupOpenPRFn = func(string) (bool, int, string, error) {
		return true, 17, "CONFLICTING", nil
	}
	t.Cleanup(func() { lookupOpenPRFn = oldLookup })

	eventsPath := filepath.Join(t.TempDir(), "events.jsonl")
	eventLog := &events.JSONLLogger{Path: eventsPath}
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: {Number: 17, State: "open", Merged: false, HeadRefName: branch}},
		},
		renderer: &retryRenderer{result: "rendered prompt"},
		errorLog: io.Discard,
		layout:   paths.NewLayout(&config.Config{}, workDir),
		eventLog: eventLog,
		sandboxFactory: &retrySandboxFactory{
			sandbox: rtSandbox,
		},
		runnableFactory: &taskWritingRunnableFactory{
			taskPath:    filepath.Join(workDir, "worktree", ".sandman", "task.md"),
			result:      AgentRunResult{IssueNumber: 42, Status: "failure", Branch: branch},
			taskContent: "## Status: already resolved",
		},
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, &retrySandboxFactory{sandbox: rtSandbox}, nil, false, "main", nil, 0, 0, 3, 0, "", 0, false, 0, false, false, false, "", "")
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "failure" {
		t.Fatalf("status = %q, want failure (alreadyResolved + CONFLICTING open PR should be failure, not success)", result.Status)
	}

	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	var terminalPayload map[string]any
	for _, e := range logs {
		if e.Type == "run.finished" {
			terminalPayload = e.Payload
		}
	}
	if terminalPayload == nil {
		t.Fatalf("run.finished event not found in logs: %v", logs)
	}
	if blocker, _ := terminalPayload["blocker"].(string); blocker != "open-pr-blocks-already-resolved" {
		t.Fatalf("run.finished payload blocker = %q, want %q (payload=%v)", blocker, "open-pr-blocks-already-resolved", terminalPayload)
	}
}

// TestRunSingle_AlreadyResolvedMergedPRStillSuccess pins the contract
// that a merged PR does not block the alreadyResolved short-circuit. The
// OpenPR helper only enumerates state=open PRs, so a merged PR is
// reported as "no open PR" by LookupOpenPR; the orchestrator relies on
// `checkPRMerged` for the merged signal, which runs before the open-PR
// guard in the mergeRequired arm. End state should be `success`.
func TestRunSingle_AlreadyResolvedMergedPRStillSuccess(t *testing.T) {
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

	oldLookup := lookupOpenPRFn
	lookupOpenPRFn = func(string) (bool, int, string, error) {
		return false, 0, "", nil
	}
	t.Cleanup(func() { lookupOpenPRFn = oldLookup })

	eventsPath := filepath.Join(t.TempDir(), "events.jsonl")
	eventLog := &events.JSONLLogger{Path: eventsPath}
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: {Number: 17, State: "merged", Merged: true, HeadRefName: branch}},
		},
		renderer: &retryRenderer{result: "rendered prompt"},
		errorLog: io.Discard,
		layout:   paths.NewLayout(&config.Config{}, workDir),
		eventLog: eventLog,
		sandboxFactory: &retrySandboxFactory{
			sandbox: rtSandbox,
		},
		runnableFactory: &taskWritingRunnableFactory{
			taskPath:    filepath.Join(workDir, "worktree", ".sandman", "task.md"),
			result:      AgentRunResult{IssueNumber: 42, Status: "failure", Branch: branch},
			taskContent: "## Status: already resolved",
		},
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, &retrySandboxFactory{sandbox: rtSandbox}, nil, false, "main", nil, 0, 0, 3, 0, "", 0, false, 0, false, false, false, "", "")
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success (alreadyResolved + merged PR should be success)", result.Status)
	}
}

type taskWritingRunnableFactory struct {
	created     int
	taskPath    string
	taskContent string
	result      AgentRunResult
}

func (f *taskWritingRunnableFactory) NewRunnable(issue *github.Issue, branch string, sb sandbox.Sandbox) Runnable {
	f.created++
	return &taskWritingRunnable{taskPath: f.taskPath, taskContent: f.taskContent, result: f.result}
}

type taskWritingRunnable struct {
	taskPath    string
	taskContent string
	result      AgentRunResult
}

func (r *taskWritingRunnable) Run(ctx context.Context, renderer prompt.IssueRenderer, command string, renderCfg prompt.RenderConfig) AgentRunResult {
	if err := os.MkdirAll(filepath.Dir(r.taskPath), 0755); err == nil {
		_ = os.WriteFile(r.taskPath, []byte(r.taskContent), 0644)
	}
	return r.result
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

	logPath := filepath.Join(workDir, ".sandman", "batches", "68cb-260622105532", "runs", "68cb-260622105532-42", "run.log")
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
		layout:                paths.NewLayout(cfg, workDir),
	}
	o.runSessionOpts.killTimeout = 50 * time.Millisecond

	result, started := o.runSingle(context.Background(), context.Background(), heartbeatTestIssueNum, cfg, "test-agent", config.Agent{Command: "true"}, false, nil, noopIdentityResolver(), map[int]string{heartbeatTestIssueNum: branch}, prompt.RenderConfig{}, nil, factory, nil, false, "main", nil, 0, 0, 1, heartbeatTestIdle, "", 0, false, 0, false, false, false, "260622105532", "68cb")
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
	// At retry time the log contains --- run 1/2 --- (from attempt 0's
	// logRunMarkerFn), the 3 lines the stalled runnable wrote before
	// being killed, and --- retry 2/2 --- (from attempt 1's
	// logRetryMarkerFn in prepareAttempt). readTailLines keeps the
	// last 3 lines, so the expected payload is the 2 trailing stall
	// lines plus the retry marker.
	lines, _ := got.Payload["last_log_lines"].([]any)
	wantLines := []string{"processing step 1", "processing step 2", "--- retry 2/2 ---"}
	if len(lines) != len(wantLines) {
		t.Errorf("run.retry last_log_lines = %v (len=%d), want %v", lines, len(lines), wantLines)
	} else {
		for i, want := range wantLines {
			if got, _ := lines[i].(string); got != want {
				t.Errorf("run.retry last_log_lines[%d] = %q, want %q", i, got, want)
			}
		}
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
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, &retrySandboxFactory{sandbox: rtSandbox}, nil, false, "main", nil, 0, 0, 0, 0, "", 0, false, 0, false, false, false, "", "")
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
// prompt-only retry loop emits run.retry with issue: 0 in the JSON payload,
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
	result, started := o.runPromptOnlySingle(context.Background(), cfg, "opencode", config.Agent{Command: "echo hi"}, noopIdentityResolver(), branch, prompt.RenderConfig{}, nil, &retrySandboxFactory{sandbox: rtSandbox}, nil, ModeFresh, "main", 0, 0, 2, "", 0, false, 0, false, false, false, false, 0, "", "run-prompt-123", nil, 0, "", "")
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

// TestRunPromptOnly_EmitsZeroRunRetryEventsOnSingleAttempt asserts that a
// prompt-only run configured with retries=0 emits zero run.retry events
// (the terminal run.finished covers the single attempt).
func TestRunPromptOnly_EmitsZeroRunRetryEventsOnSingleAttempt(t *testing.T) {
	workDir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get wd: %v", err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	branch := "sandman/prompt-only-456"
	rtSandbox := &retrySandbox{workDir: filepath.Join(workDir, "worktree")}
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
		runnableFactory: &fakeRunnableFactory{results: []AgentRunResult{
			{IssueNumber: 0, Status: "failure", Branch: branch},
		}},
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runPromptOnlySingle(context.Background(), cfg, "opencode", config.Agent{Command: "echo hi"}, noopIdentityResolver(), branch, prompt.RenderConfig{}, nil, &retrySandboxFactory{sandbox: rtSandbox}, nil, ModeFresh, "main", 0, 0, 0, "", 0, false, 0, false, false, false, false, 0, "", "run-prompt-456", nil, 0, "", "")
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
		t.Errorf("expected 0 run.retry events for a 1-attempt prompt-only run, got %d (events: %v)", retryCount, logs)
	}
	if !foundFinished {
		t.Errorf("expected run.finished event in event log")
	}
}

func TestRunSingle_ClosedIssueNoPRReturnsSuccess(t *testing.T) {
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

	eventsPath := filepath.Join(t.TempDir(), "events.jsonl")
	eventLog := &events.JSONLLogger{Path: eventsPath}
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug", State: "closed"}},
			prs:    map[string]*github.PR{},
		},
		renderer: &retryRenderer{result: "rendered prompt"},
		errorLog: io.Discard,
		layout:   paths.NewLayout(&config.Config{}, workDir),
		eventLog: eventLog,
		sandboxFactory: &retrySandboxFactory{
			sandbox: rtSandbox,
		},
		runnableFactory: &fakeRunnableFactory{results: []AgentRunResult{
			{IssueNumber: 42, Status: "success", Branch: branch},
			{IssueNumber: 42, Status: "failure", Branch: branch},
		}},
	}
	o.runSessionOpts.retryReset = func(ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error {
		return nil
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	resultFactory := o.runnableFactory.(*fakeRunnableFactory)
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, &retrySandboxFactory{sandbox: rtSandbox}, nil, false, "main", nil, 0, 0, 1, 0, "", 0, false, 0, false, false, false, "", "")
	if !started {
		t.Fatalf("expected run to start, result.Status=%q, created=%d", result.Status, len(resultFactory.created))
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success (closed issue with no PR on branch should be success)", result.Status)
	}
	if result.RetriesTotal != 1 {
		t.Fatalf("RetriesTotal = %d, want 1 (closed issue with no PR should succeed without retry)", result.RetriesTotal)
	}
	if len(resultFactory.created) != 1 {
		t.Fatalf("created runnables = %d, want 1 (closed issue with no PR should succeed without retry)", len(resultFactory.created))
	}
}
