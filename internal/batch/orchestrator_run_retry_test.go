package batch

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
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

	branch := "42-fix-bug"
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
	resultFactory := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "failure", Branch: branch},
		{IssueNumber: 42, Status: "failure", Branch: branch},
		{IssueNumber: 42, Status: "success", Branch: branch},
	}}
	o := NewOrchestrator(
		&fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: pr},
		},
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&retrySandboxFactory{sandbox: rtSandbox}),
		WithRunnableFactory(resultFactory),
		// Override retryReset so the orchestrator's resetRetryBranch does not
		// try to run `git reset --hard` on the retrySandbox (which would
		// return an error from its execErrors slice and short-circuit the
		// loop with a failure errResult).
		WithRunSessionOpts(runSessionOptions{retryReset: func(ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error {
			return nil
		}}),
	)

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	bc := BatchConfig{
		Cfg:              cfg,
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          2,
	}
	row := RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: branch},
		BaseBranch:  "main",
	}
	result, started := o.newRunExecutor(context.Background(), bc, &retrySandboxFactory{sandbox: rtSandbox}, nil).Execute(context.Background(), row)
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
	// Last 3 log lines at the 1→2 transition: just the first retry
	// marker (--- retry 1/2 ---). The initial attempt no longer
	// writes a run marker.
	lines0, _ := got0.Payload["last_log_lines"].([]any)
	wantLines0 := []string{"--- retry 1/2 ---"}
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
	// At the 2→3 transition the log has 2 lines: the two retry
	// markers from attempts 1 and 2 (the initial attempt no longer
	// writes a run marker). readTailLines keeps the last 3 lines,
	// so both are returned.
	lines1, _ := got1.Payload["last_log_lines"].([]any)
	wantLines1 := []string{"--- retry 1/2 ---", "--- retry 2/2 ---"}
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

	branch := "42-fix-bug"
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
	resultFactory := &taskWritingRunnableFactory{
		taskPath:    filepath.Join(workDir, "worktree", ".sandman", "task.md"),
		result:      AgentRunResult{IssueNumber: 42, Status: "failure", Branch: branch},
		taskContent: "## Status: already resolved",
	}
	o := NewOrchestrator(
		&fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{},
		},
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&retrySandboxFactory{sandbox: rtSandbox}),
		WithRunnableFactory(resultFactory),
	)

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	bc := BatchConfig{
		Cfg:              cfg,
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	row := RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: branch},
		BaseBranch:  "main",
	}
	result, started := o.newRunExecutor(context.Background(), bc, &retrySandboxFactory{sandbox: rtSandbox}, nil).Execute(context.Background(), row)
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success", result.Status)
	}
	if result.RetriesTotal != 1 {
		t.Fatalf("retries total = %d, want 1", result.RetriesTotal)
	}
	if got := resultFactory.created; got != 1 {
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

	branch := "42-fix-bug"
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
	o := NewOrchestrator(
		&fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: {Number: 17, State: "open", Merged: false, HeadRefName: branch}},
		},
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&retrySandboxFactory{sandbox: rtSandbox}),
		WithRunnableFactory(&taskWritingRunnableFactory{
			taskPath:    filepath.Join(workDir, "worktree", ".sandman", "task.md"),
			result:      AgentRunResult{IssueNumber: 42, Status: "failure", Branch: branch},
			taskContent: "## Status: already resolved",
		}),
	)

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	bc := BatchConfig{
		Cfg:              cfg,
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	row := RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: branch},
		BaseBranch:  "main",
	}
	result, started := o.newRunExecutor(context.Background(), bc, &retrySandboxFactory{sandbox: rtSandbox}, nil).Execute(context.Background(), row)
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

	branch := "42-fix-bug"
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
	o := NewOrchestrator(
		&fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: {Number: 17, State: "open", Merged: false, HeadRefName: branch}},
		},
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&retrySandboxFactory{sandbox: rtSandbox}),
		WithRunnableFactory(&taskWritingRunnableFactory{
			taskPath:    filepath.Join(workDir, "worktree", ".sandman", "task.md"),
			result:      AgentRunResult{IssueNumber: 42, Status: "failure", Branch: branch},
			taskContent: "## Status: already resolved",
		}),
	)

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	bc := BatchConfig{
		Cfg:              cfg,
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	row := RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: branch},
		BaseBranch:  "main",
	}
	result, started := o.newRunExecutor(context.Background(), bc, &retrySandboxFactory{sandbox: rtSandbox}, nil).Execute(context.Background(), row)
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

	branch := "42-fix-bug"
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
	o := NewOrchestrator(
		&fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: {Number: 17, State: "merged", Merged: true, HeadRefName: branch}},
		},
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&retrySandboxFactory{sandbox: rtSandbox}),
		WithRunnableFactory(&taskWritingRunnableFactory{
			taskPath:    filepath.Join(workDir, "worktree", ".sandman", "task.md"),
			result:      AgentRunResult{IssueNumber: 42, Status: "failure", Branch: branch},
			taskContent: "## Status: already resolved",
		}),
	)

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	bc := BatchConfig{
		Cfg:              cfg,
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	row := RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: branch},
		BaseBranch:  "main",
	}
	result, started := o.newRunExecutor(context.Background(), bc, &retrySandboxFactory{sandbox: rtSandbox}, nil).Execute(context.Background(), row)
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

	branch := "42-fix-bug"
	worktreePath := filepath.Join(workDir, heartbeatTestWorktreeDir, "42-fix-bug")
	proc := &fakeProcess{killed: make(chan struct{})}
	sb := &fakeSandbox{workDir: worktreePath, process: proc}
	factory := &fakeSandboxFactory{sandbox: sb}

	logPath := filepath.Join(workDir, ".sandman", "batches", "260622105532-68cb", "runs", "260622105532-68cb-42", "run.log")
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
	o := NewOrchestrator(
		client,
		&noopRenderer{},
		&fakeConfigStore{config: cfg},
		eventLog,
		WithSandboxFactory(factory),
		WithRunnableFactory(runFactory),
		WithHeartbeatTickInterval(heartbeatTestTick),
		WithErrorLog(io.Discard),
		WithRunSessionOpts(runSessionOptions{killTimeout: 50 * time.Millisecond}),
	)

	bc := BatchConfig{
		Cfg:              cfg,
		AgentName:        "test-agent",
		AgentCfg:         config.Agent{Command: "true"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          1,
		RunIdleTimeout:   heartbeatTestIdle,
	}
	row := RowSpec{
		IssueNumber: heartbeatTestIssueNum,
		Branches:    map[int]string{heartbeatTestIssueNum: branch},
		BaseBranch:  "main",
		RunTS:       "260622105532",
		RunShortID:  "68cb",
	}
	result, started := o.newRunExecutor(context.Background(), bc, factory, nil).Execute(context.Background(), row)
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
	// At retry time the log contains the 3 lines the stalled runnable
	// wrote before being killed, followed by --- retry 1/1 --- (from
	// attempt 1's logRetryMarkerFn in prepareAttempt). readTailLines
	// keeps the last 3 lines, so the expected payload is the 2
	// trailing stall lines plus the retry marker.
	lines, _ := got.Payload["last_log_lines"].([]any)
	wantLines := []string{"processing step 1", "processing step 2", "--- retry 1/1 ---"}
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

	branch := "42-fix-bug"
	rtSandbox := &retrySandbox{workDir: filepath.Join(workDir, "worktree")}
	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })
	pr := &github.PR{Number: 17, State: "open", Merged: false, HeadRefName: branch}
	eventsPath := filepath.Join(t.TempDir(), "events.jsonl")
	eventLog := &events.JSONLLogger{Path: eventsPath}
	o := NewOrchestrator(
		&fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: pr},
		},
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&retrySandboxFactory{sandbox: rtSandbox}),
		WithRunnableFactory(&fakeRunnableFactory{results: []AgentRunResult{
			{IssueNumber: 42, Status: "failure", Branch: branch},
		}}),
	)

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	bc := BatchConfig{
		Cfg:              cfg,
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
	}
	row := RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: branch},
		BaseBranch:  "main",
	}
	result, started := o.newRunExecutor(context.Background(), bc, &retrySandboxFactory{sandbox: rtSandbox}, nil).Execute(context.Background(), row)
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
	o := NewOrchestrator(
		nil,
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&retrySandboxFactory{sandbox: rtSandbox}),
		WithRunSessionOpts(runSessionOptions{retryReset: func(ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error {
			return nil
		}}),
	)

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	bc := BatchConfig{
		Cfg:              cfg,
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          2,
	}
	row := RowSpec{
		Mode:              ModeFresh,
		Branches:          map[int]string{0: branch},
		BaseBranch:        "main",
		BatchID:           batchIDForPromptOnly("", "", "run-prompt-123", ""),
		RunID:             "run-prompt-123",
		UserProvidedRunID: "run-prompt-123",
	}
	result, started := o.newRunExecutor(context.Background(), bc, &retrySandboxFactory{sandbox: rtSandbox}, nil).Execute(context.Background(), row)
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
	o := NewOrchestrator(
		nil,
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&retrySandboxFactory{sandbox: rtSandbox}),
		WithRunnableFactory(&fakeRunnableFactory{results: []AgentRunResult{
			{IssueNumber: 0, Status: "failure", Branch: branch},
		}}),
	)

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	bc := BatchConfig{
		Cfg:              cfg,
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
	}
	row := RowSpec{
		Mode:              ModeFresh,
		Branches:          map[int]string{0: branch},
		BaseBranch:        "main",
		BatchID:           batchIDForPromptOnly("", "", "run-prompt-456", ""),
		RunID:             "run-prompt-456",
		UserProvidedRunID: "run-prompt-456",
	}
	result, started := o.newRunExecutor(context.Background(), bc, &retrySandboxFactory{sandbox: rtSandbox}, nil).Execute(context.Background(), row)
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

	branch := "42-fix-bug"
	rtSandbox := &retrySandbox{
		workDir: filepath.Join(workDir, "worktree"),
	}
	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })

	eventsPath := filepath.Join(t.TempDir(), "events.jsonl")
	eventLog := &events.JSONLLogger{Path: eventsPath}
	resultFactory := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "success", Branch: branch},
		{IssueNumber: 42, Status: "failure", Branch: branch},
	}}
	o := NewOrchestrator(
		&fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug", State: "closed"}},
			prs:    map[string]*github.PR{},
		},
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&retrySandboxFactory{sandbox: rtSandbox}),
		WithRunnableFactory(resultFactory),
		WithRunSessionOpts(runSessionOptions{retryReset: func(ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error {
			return nil
		}}),
	)

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	bc := BatchConfig{
		Cfg:              cfg,
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          1,
	}
	row := RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: branch},
		BaseBranch:  "main",
	}
	result, started := o.newRunExecutor(context.Background(), bc, &retrySandboxFactory{sandbox: rtSandbox}, nil).Execute(context.Background(), row)
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

// TestRunSingle_RetryBannersUseRetriesBudgetAsDenominator pins the
// issue #1961 contract end-to-end: a run with a 3-retry budget that
// exhausts all 3 retries writes the three retry markers
// "--- retry 1/3 ---", "--- retry 2/3 ---", "--- retry 3/3 ---" to
// the run log, and the initial attempt writes no banner. The
// denominator is the retry budget (3), not the total attempt count
// (4). Acceptance criteria 1–4 from issue #1961.
func TestRunSingle_RetryBannersUseRetriesBudgetAsDenominator(t *testing.T) {
	workDir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get wd: %v", err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	branch := "42-fix-bug"
	rtSandbox := &retrySandbox{
		workDir: filepath.Join(workDir, "worktree"),
	}
	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })

	// Closed PR + unmerged: every attempt's status flips to
	// "failure" in the post-attempt check, so the orchestrator
	// actually executes all 4 attempts (1 initial + 3 retries).
	pr := &github.PR{Number: 17, State: "closed", Merged: false, HeadRefName: branch}
	eventsPath := filepath.Join(t.TempDir(), "events.jsonl")
	eventLog := &events.JSONLLogger{Path: eventsPath}
	resultFactory := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "failure", Branch: branch},
		{IssueNumber: 42, Status: "failure", Branch: branch},
		{IssueNumber: 42, Status: "failure", Branch: branch},
		{IssueNumber: 42, Status: "failure", Branch: branch},
	}}
	o := NewOrchestrator(
		&fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: pr},
		},
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&retrySandboxFactory{sandbox: rtSandbox}),
		WithRunnableFactory(resultFactory),
		WithRunSessionOpts(runSessionOptions{retryReset: func(ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error {
			return nil
		}}),
	)

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	// retries=3 → 4 attempts total.
	bc := BatchConfig{
		Cfg:              cfg,
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	row := RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: branch},
		BaseBranch:  "main",
	}
	result, started := o.newRunExecutor(context.Background(), bc, &retrySandboxFactory{sandbox: rtSandbox}, nil).Execute(context.Background(), row)
	if !started {
		t.Fatalf("expected run to start, result.Status=%q, created=%d", result.Status, len(resultFactory.created))
	}
	if result.Status != "failure" {
		t.Fatalf("status = %q, want failure (unmerged PR forces failure)", result.Status)
	}
	if result.RetriesTotal != 4 {
		t.Fatalf("RetriesTotal = %d, want 4 (1 initial + 3 retries)", result.RetriesTotal)
	}
	if len(resultFactory.created) != 4 {
		t.Fatalf("created runnables = %d, want 4 (1 initial + 3 retries)", len(resultFactory.created))
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
	if len(retryEvents) != 3 {
		t.Fatalf("expected exactly 3 run.retry events (1→2, 2→3, 3→4 for a 4-attempt run), got %d (events: %v)", len(retryEvents), logs)
	}

	logPath := filepath.Join(workDir, ".sandman", "batches", "-", "runs", "--42", "run.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	content := string(data)
	if strings.Contains(content, "--- run ") {
		t.Errorf("log must not contain a '--- run ' banner for the initial attempt, got:\n%s", content)
	}
	for _, want := range []string{"--- retry 1/3 ---", "--- retry 2/3 ---", "--- retry 3/3 ---"} {
		if !strings.Contains(content, want) {
			t.Errorf("log missing %q, got:\n%s", want, content)
		}
	}
	for _, unwanted := range []string{"--- retry 1/4 ---", "--- retry 2/4 ---", "--- retry 3/4 ---", "--- retry 4/4 ---"} {
		if strings.Contains(content, unwanted) {
			t.Errorf("log must not use total-attempt denominator %q, got:\n%s", unwanted, content)
		}
	}
}

// TestRunSingle_InitialAttemptOnlyHasNoBanner pins AC1 (initial
// attempt does not show a retry label) and AC5 (initial-attempt
// coverage) directly: a run with retries=0 produces a run log file
// that contains no `--- run ---` and no `--- retry ---` banner at
// all — the agent's own output is the only content.
func TestRunSingle_InitialAttemptOnlyHasNoBanner(t *testing.T) {
	workDir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get wd: %v", err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	branch := "42-fix-bug"
	// Use a real fakeSandbox (not retrySandbox) so the agent
	// writes the run log via execStdout; retrySandbox returns
	// execErrors and does not write to the log file.
	rtSandbox := &fakeSandbox{workDir: filepath.Join(workDir, "worktree"), execStdout: "agent output\n"}
	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })
	pr := &github.PR{Number: 17, State: "open", Merged: false, HeadRefName: branch}
	o := NewOrchestrator(
		&fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: pr},
		},
		&retryRenderer{result: "rendered prompt"},
		nil,
		nil,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&fakeSandboxFactory{sandbox: rtSandbox}),
	)

	cfg := &config.Config{WorktreeDir: "worktree", Git: config.GitConfig{BaseBranch: "main"}}
	bc := BatchConfig{
		Cfg:              cfg,
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "opencode run {{.PromptFile}}"},
		IdentityResolver: noopIdentityResolver(),
	}
	row := RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: branch},
		BaseBranch:  "main",
		RunTS:       "260622105532",
		RunShortID:  "68cb",
	}
	result, started := o.newRunExecutor(context.Background(), bc, &fakeSandboxFactory{sandbox: rtSandbox}, nil).Execute(context.Background(), row)
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "failure" {
		t.Fatalf("status = %q, want failure (unmerged PR forces failure regardless of agent exit)", result.Status)
	}

	logPath := filepath.Join(workDir, ".sandman", "batches", "260622105532-68cb", "runs", "260622105532-68cb-42", "run.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	content := string(data)
	if strings.Contains(content, "--- run ") {
		t.Errorf("log must not contain a '--- run ' banner for the initial attempt, got:\n%s", content)
	}
	if strings.Contains(content, "--- retry ") {
		t.Errorf("log must not contain a '--- retry ' banner when no retries are budgeted, got:\n%s", content)
	}
}
