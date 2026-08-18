package batch

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// B20: When re-evaluation shows CI failure after resume from await, emit run.finished with blocked status and gate: failed.
func TestRunSingle_ModeContinueCIFailureReEvaluatesToBlocked(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)

	branch := "42-fix-bug"
	worktreePath := filepath.Join(workDir, "worktree")

	sbFactory := &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: worktreePath}}
	resultFactory := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "success", Branch: branch},
	}}
	spyLog := &spyEventLog{}
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			// PR now has CI failure after resume from await
			prs: map[string]*github.PR{branch: {
				Number:            17,
				State:             "open",
				HeadRefName:       branch,
				HeadRefOid:        "current-sha",
				StatusCheckRollup: "failure",
				ReviewDecision:    "APPROVED",
				MergeStateStatus:  "BLOCKED",
			}},
		},
		renderer:        &retryRenderer{result: "rendered prompt"},
		sandboxFactory:  sbFactory,
		eventLog:        spyLog,
		errorLog:        io.Discard,
		runnableFactory: resultFactory,
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, true, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, sbFactory, nil, false, "main", nil, 0, 0, 1, 0, "", 0, false, 0, false, false, false, "", "")
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "blocked" {
		t.Fatalf("status = %q, want blocked (CI failure after resume from await)", result.Status)
	}
	logs, err := spyLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	finished := findEvent(logs, "run.finished")
	if finished == nil {
		t.Fatalf("run.finished event not found: %v", logs)
	}
	if finished.Payload["gate"] != "failed" {
		t.Fatalf("gate = %v, want failed", finished.Payload["gate"])
	}
	if finished.Payload["blocker"] != "external-gate" {
		t.Fatalf("blocker = %v, want external-gate", finished.Payload["blocker"])
	}
}

func TestEmitAwait_CarriesAwaitReasonFromGate(t *testing.T) {
	spyLog := &spyEventLog{}
	s := &runSession{
		deps:        runDeps{eventLog: spyLog},
		issueNumber: 42,
		baseBranch:  "main",
		retries:     2,
	}
	status := s.emitAwait(context.Background(), "run-await-reason", AgentRunResult{Branch: "42-fix"}, map[string]any{
		"blocker": "external-gate",
		"gate":    "pending",
	})
	if status != "await" {
		t.Fatalf("status = %q, want await", status)
	}
	events := spyLog.snapshot()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	evt := events[0]
	if evt.Type != "run.await" {
		t.Fatalf("event type = %q, want run.await", evt.Type)
	}
	if got := evt.Payload["await_reason"]; got != "pending" {
		t.Fatalf("await_reason = %v, want %q", got, "pending")
	}
	if got := evt.Payload["branch"]; got != "42-fix" {
		t.Fatalf("branch = %v, want %q", got, "42-fix")
	}
	if got := evt.Payload["base_branch"]; got != "main" {
		t.Fatalf("base_branch = %v, want %q", got, "main")
	}
	if got := evt.Payload["retries_total"]; got != 2 {
		t.Fatalf("retries_total = %v, want 2", got)
	}
}

func TestEmitAwait_ExplicitAwaitReasonOverridesGate(t *testing.T) {
	spyLog := &spyEventLog{}
	s := &runSession{
		deps:        runDeps{eventLog: spyLog},
		issueNumber: 42,
		baseBranch:  "main",
	}
	s.emitAwait(context.Background(), "run-await-explicit", AgentRunResult{Branch: "42-fix"}, map[string]any{
		"gate":         "pending",
		"await_reason": "review-timeout",
	})
	events := spyLog.snapshot()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if got := events[0].Payload["await_reason"]; got != "review-timeout" {
		t.Fatalf("await_reason = %v, want %q", got, "review-timeout")
	}
}

func TestEmitAwait_NoGateLeavesAwaitReasonAbsent(t *testing.T) {
	spyLog := &spyEventLog{}
	s := &runSession{
		deps:        runDeps{eventLog: spyLog},
		issueNumber: 42,
		baseBranch:  "main",
	}
	s.emitAwait(context.Background(), "run-await-no-gate", AgentRunResult{Branch: "42-fix"}, nil)
	events := spyLog.snapshot()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if _, ok := events[0].Payload["await_reason"]; ok {
		t.Fatalf("await_reason = %v, want absent when no gate is present", events[0].Payload["await_reason"])
	}
}

// Entry re-evaluation: a continuation session re-entering while the PR gate is
// merely pending must not launch the agent or hold capacity: it emits run.await
// and ends the session without consuming a retry.
func TestEntryReevaluation_ModeContinuePendingGateAwaitsImmediately(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)

	branch := "42-fix-bug"
	worktreePath := filepath.Join(workDir, "worktree")

	sbFactory := &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: worktreePath}}
	resultFactory := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "success", Branch: branch},
	}}
	spyLog := &spyEventLog{}
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs: map[string]*github.PR{branch: {
				Number:            17,
				State:             "open",
				HeadRefName:       branch,
				HeadRefOid:        "current-sha",
				StatusCheckRollup: "pending",
				ReviewDecision:    "REVIEW_REQUIRED",
				MergeStateStatus:  "BLOCKED",
			}},
		},
		renderer:        &retryRenderer{result: "rendered prompt"},
		sandboxFactory:  sbFactory,
		eventLog:        spyLog,
		errorLog:        io.Discard,
		runnableFactory: resultFactory,
		runSessionOpts: runSessionOptions{
			currentHead: func(string) (string, error) { return "current-sha", nil },
		},
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, true, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, sbFactory, nil, false, "main", nil, 0, 0, 1, 0, "", 0, false, 0, false, false, false, "", "")
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "await" {
		t.Fatalf("status = %q, want await (pending gate must not launch)", result.Status)
	}
	if got := len(resultFactory.created); got != 0 {
		t.Fatalf("agent launches = %d, want 0 at pending gate", got)
	}
	logs, err := spyLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if got := countEventsByType(logs, "run.await"); got != 1 {
		t.Fatalf("run.await events = %d, want 1", got)
	}
	if got := countEventsByType(logs, "run.finished"); got != 0 {
		t.Fatalf("run.finished events = %d, want 0 (await is non-terminal)", got)
	}
	if got := countEventsByType(logs, "run.retry"); got != 0 {
		t.Fatalf("run.retry events = %d, want 0", got)
	}
	awaitEvt := findEvent(logs, "run.await")
	if awaitEvt == nil {
		t.Fatal("run.await event not found")
	}
	if awaitEvt.Payload["gate"] != "pending" {
		t.Fatalf("gate = %v, want pending", awaitEvt.Payload["gate"])
	}
	if awaitEvt.Payload["await_reason"] != "pending" {
		t.Fatalf("await_reason = %v, want pending", awaitEvt.Payload["await_reason"])
	}
}

// Entry re-evaluation: a continuation session re-entering while the PR gate is
// ready-to-merge resumes the agent with request-scoped merge evidence instead
// of launching blindly. The resumed session ends on the same gate (the
// in-session resume loop is a later slice) without consuming a retry.
func TestEntryReevaluation_ModeContinueReadyToMergeResumesAgentWithEvidence(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)

	branch := "42-fix-bug"
	worktreePath := filepath.Join(workDir, "worktree")

	sbFactory := &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: worktreePath}}
	resultFactory := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "success", Branch: branch},
		{IssueNumber: 42, Status: "success", Branch: branch},
	}}
	spyLog := &spyEventLog{}
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs: map[string]*github.PR{branch: {
				Number:            17,
				State:             "open",
				HeadRefName:       branch,
				HeadRefOid:        "current-sha",
				StatusCheckRollup: "success",
				ReviewDecision:    "APPROVED",
				MergeStateStatus:  "CLEAN",
			}},
		},
		renderer:        &retryRenderer{result: "rendered prompt"},
		sandboxFactory:  sbFactory,
		eventLog:        spyLog,
		errorLog:        io.Discard,
		runnableFactory: resultFactory,
		runSessionOpts: runSessionOptions{
			currentHead:      func(string) (string, error) { return "current-sha", nil },
			gatePollInitial:  time.Millisecond,
			gatePollMaxSleep: time.Millisecond,
			gatePollBudget:   time.Second,
			awaitResumeMax:   1,
		},
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, true, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, sbFactory, nil, false, "main", nil, 0, 0, 1, 0, "", 0, false, 0, false, false, false, "", "")
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "await" {
		t.Fatalf("status = %q, want await (post-resume gate still ready-to-merge, covered by resume cap fallback)", result.Status)
	}
	if got := len(resultFactory.created); got != 2 {
		t.Fatalf("agent launches = %d, want 2 (entry resume + in-session resume)", got)
	}
	if got := len(resultFactory.configs); got != 2 {
		t.Fatalf("captured configs = %d, want 2", got)
	}
	for i, cfg := range resultFactory.configs {
		if !strings.Contains(cfg.TaskPrompt, "## Review Evidence") {
			t.Fatalf("config %d prompt missing ## Review Evidence section:\n%s", i, cfg.TaskPrompt)
		}
		if !strings.Contains(cfg.TaskPrompt, "ready-to-merge") {
			t.Fatalf("config %d prompt missing merge evidence:\n%s", i, cfg.TaskPrompt)
		}
	}
	logs, err := spyLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if got := countEventsByType(logs, "run.retry"); got != 0 {
		t.Fatalf("run.retry events = %d, want 0", got)
	}
	resumedEvt := findEvent(logs, "run.resumed")
	if resumedEvt == nil || resumedEvt.Payload["gate"] != gateReadyToMerge {
		t.Fatalf("run.resumed gate = %v, want ready-to-merge", resumedEvt.Payload["gate"])
	}
	awaitEvt := findEvent(logs, "run.await")
	if awaitEvt == nil || awaitEvt.Payload["gate"] != "ready-to-merge" {
		t.Fatalf("run.await gate = %v, want ready-to-merge", awaitEvt.Payload["gate"])
	}
}

// In-session resume: after the agent completes cleanly and the gate is
// ready-to-merge, the run relaunches the agent in the same attempt (no
// run.retry) with the merge evidence attached, bounded by the resume cap;
// the final same-gate observation falls back to run.await.
func TestRunSingle_ReadyToMergeResumesWithinSameAttempt(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)

	branch := "42-fix-bug"
	worktreePath := filepath.Join(workDir, "worktree")

	sbFactory := &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: worktreePath}}
	resultFactory := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "success", Branch: branch},
		{IssueNumber: 42, Status: "success", Branch: branch},
	}}
	spyLog := &spyEventLog{}
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs: map[string]*github.PR{branch: {
				Number:            17,
				State:             "open",
				HeadRefName:       branch,
				HeadRefOid:        "current-sha",
				StatusCheckRollup: "success",
				ReviewDecision:    "APPROVED",
				MergeStateStatus:  "CLEAN",
			}},
		},
		renderer:        &retryRenderer{result: "rendered prompt"},
		sandboxFactory:  sbFactory,
		eventLog:        spyLog,
		errorLog:        io.Discard,
		runnableFactory: resultFactory,
		runSessionOpts: runSessionOptions{
			currentHead:      func(string) (string, error) { return "current-sha", nil },
			gatePollInitial:  time.Millisecond,
			gatePollMaxSleep: time.Millisecond,
			gatePollBudget:   time.Second,
			awaitResumeMax:   1,
		},
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, sbFactory, nil, false, "main", nil, 0, 0, 1, 0, "", 0, false, 0, false, false, false, "", "")
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "await" {
		t.Fatalf("status = %q, want await (resume cap exhausted on steady ready-to-merge gate)", result.Status)
	}
	if got := len(resultFactory.created); got != 2 {
		t.Fatalf("agent launches = %d, want 2 (initial + one resumed relaunch)", got)
	}
	if got := len(resultFactory.configs); got != 2 {
		t.Fatalf("captured configs = %d, want 2", got)
	}
	if strings.Contains(resultFactory.configs[0].TaskPrompt, "## Review Evidence") {
		t.Fatalf("initial launch must not carry evidence:\n%s", resultFactory.configs[0].TaskPrompt)
	}
	if !strings.Contains(resultFactory.configs[1].TaskPrompt, "## Review Evidence") {
		t.Fatalf("resumed relaunch must carry evidence:\n%s", resultFactory.configs[1].TaskPrompt)
	}
	logs, err := spyLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if got := countEventsByType(logs, "run.retry"); got != 0 {
		t.Fatalf("run.retry events = %d, want 0 (resume must not consume retry budget)", got)
	}
	if got := countEventsByType(logs, "run.resumed"); got != 1 {
		t.Fatalf("run.resumed events = %d, want 1", got)
	}
	resumedEvt := findEvent(logs, "run.resumed")
	if resumedEvt == nil || resumedEvt.Payload["reason"] != "approval" {
		t.Fatalf("run.resumed reason = %v, want approval", resumedEvt.Payload["reason"])
	}
	awaitEvt := findEvent(logs, "run.await")
	if awaitEvt == nil || awaitEvt.Payload["gate"] != "ready-to-merge" {
		t.Fatalf("run.await gate = %v, want ready-to-merge", awaitEvt.Payload["gate"])
	}
}

// In-session resume via the poll: the gate starts pending and flips to
// ready-to-merge between poll iterations; the run resumes the agent with the
// merge evidence instead of awaiting on the polled gate.
func TestRunSingle_GatePollTransitionToReadyResumesAgent(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)

	branch := "42-fix-bug"
	worktreePath := filepath.Join(workDir, "worktree")

	readyPR := &github.PR{
		Number:            17,
		State:             "open",
		HeadRefName:       branch,
		HeadRefOid:        "current-sha",
		StatusCheckRollup: "success",
		ReviewDecision:    "APPROVED",
		MergeStateStatus:  "CLEAN",
	}
	pendingPR := &github.PR{
		Number:            17,
		State:             "open",
		HeadRefName:       branch,
		HeadRefOid:        "current-sha",
		StatusCheckRollup: "pending",
		ReviewDecision:    "REVIEW_REQUIRED",
		MergeStateStatus:  "BLOCKED",
	}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
		prs:    map[string]*github.PR{branch: pendingPR},
	}
	lookups := 0
	client.findPRHook = func() {
		lookups++
		if lookups >= 2 {
			client.prs[branch] = readyPR
		}
	}

	sbFactory := &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: worktreePath}}
	resultFactory := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "success", Branch: branch},
		{IssueNumber: 42, Status: "success", Branch: branch},
	}}
	spyLog := &spyEventLog{}
	o := &Orchestrator{
		githubClient:    client,
		renderer:        &retryRenderer{result: "rendered prompt"},
		sandboxFactory:  sbFactory,
		eventLog:        spyLog,
		errorLog:        io.Discard,
		runnableFactory: resultFactory,
		runSessionOpts: runSessionOptions{
			currentHead:      func(string) (string, error) { return "current-sha", nil },
			gatePollInitial:  time.Millisecond,
			gatePollMaxSleep: time.Millisecond,
			gatePollBudget:   time.Second,
			awaitResumeMax:   1,
		},
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, sbFactory, nil, false, "main", nil, 0, 0, 1, 0, "", 0, false, 0, false, false, false, "", "")
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "await" {
		t.Fatalf("status = %q, want await after resumed relaunch on steady ready gate", result.Status)
	}
	if got := len(resultFactory.created); got != 2 {
		t.Fatalf("agent launches = %d, want 2 (initial + poll-transition resume)", got)
	}
	if got := len(resultFactory.configs); got != 2 {
		t.Fatalf("captured configs = %d, want 2", got)
	}
	if !strings.Contains(resultFactory.configs[1].TaskPrompt, "## Review Evidence") {
		t.Fatalf("poll-resumed relaunch must carry evidence:\n%s", resultFactory.configs[1].TaskPrompt)
	}
	logs, err := spyLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if got := countEventsByType(logs, "run.retry"); got != 0 {
		t.Fatalf("run.retry events = %d, want 0", got)
	}
	awaitEvt := findEvent(logs, "run.await")
	if awaitEvt == nil || awaitEvt.Payload["gate"] != "ready-to-merge" {
		t.Fatalf("run.await gate = %v, want ready-to-merge", awaitEvt.Payload["gate"])
	}
}

// Pending gates keep the agent serialized with review waiting: budget
// exhaustion awaits without launching the agent again.
func TestRunSingle_PendingGateBudgetExhaustionAwaitsWithoutResume(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)

	branch := "42-fix-bug"
	worktreePath := filepath.Join(workDir, "worktree")

	sbFactory := &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: worktreePath}}
	resultFactory := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "success", Branch: branch},
	}}
	spyLog := &spyEventLog{}
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs: map[string]*github.PR{branch: {
				Number:            17,
				State:             "open",
				HeadRefName:       branch,
				HeadRefOid:        "current-sha",
				StatusCheckRollup: "pending",
				ReviewDecision:    "REVIEW_REQUIRED",
				MergeStateStatus:  "BLOCKED",
			}},
		},
		renderer:        &retryRenderer{result: "rendered prompt"},
		sandboxFactory:  sbFactory,
		eventLog:        spyLog,
		errorLog:        io.Discard,
		runnableFactory: resultFactory,
		runSessionOpts: runSessionOptions{
			currentHead:      func(string) (string, error) { return "current-sha", nil },
			gatePollInitial:  time.Millisecond,
			gatePollMaxSleep: time.Millisecond,
			gatePollBudget:   5 * time.Millisecond,
		},
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, sbFactory, nil, false, "main", nil, 0, 0, 1, 0, "", 0, false, 0, false, false, false, "", "")
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "await" {
		t.Fatalf("status = %q, want await after poll budget exhaustion", result.Status)
	}
	if got := len(resultFactory.created); got != 1 {
		t.Fatalf("agent launches = %d, want 1 (pending gate must not relaunch)", got)
	}
	logs, err := spyLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if got := countEventsByType(logs, "run.resumed"); got != 0 {
		t.Fatalf("run.resumed events = %d, want 0", got)
	}
	awaitEvt := findEvent(logs, "run.await")
	if awaitEvt == nil || awaitEvt.Payload["gate"] != "pending" {
		t.Fatalf("run.await gate = %v, want pending", awaitEvt.Payload["gate"])
	}
}

// Live CHANGES_REQUESTED with retained actionable evidence resumes the agent
// (await + actionable-feedback) instead of blocking; the poll can drive the
// transition too, and a resumed session carries the requested-changes
// evidence in its prompt.
func TestRunSingle_PendingGatePollFailureWithActionableFeedbackResumesAgent(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)

	branch := "42-fix-bug"
	worktreePath := filepath.Join(workDir, "worktree")
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0o755); err != nil {
		t.Fatalf("create worktree task directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	writeTimedOutReviewRequest(t, worktreePath)
	writeFormalChangesRequestedClassification(t, worktreePath, "current")

	resultFactory := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "success", Branch: branch},
		{IssueNumber: 42, Status: "success", Branch: branch},
	}}
	spyLog := &spyEventLog{}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
		prs: map[string]*github.PR{branch: {
			Number:            17,
			State:             "open",
			HeadRefName:       branch,
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "pending",
			ReviewDecision:    "CHANGES_REQUESTED",
			MergeStateStatus:  "BLOCKED",
		}},
	}
	lookups := 0
	client.findPRHook = func() {
		lookups++
		if lookups >= 2 {
			client.prs[branch].StatusCheckRollup = "success"
			client.prs[branch].MergeStateStatus = "CLEAN"
		}
	}

	sbFactory := &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: worktreePath}}
	o := &Orchestrator{
		githubClient:    client,
		renderer:        &retryRenderer{result: "rendered prompt"},
		sandboxFactory:  sbFactory,
		eventLog:        spyLog,
		errorLog:        io.Discard,
		runnableFactory: resultFactory,
		runSessionOpts: runSessionOptions{
			currentHead:      func(string) (string, error) { return "current-sha", nil },
			gatePollInitial:  time.Millisecond,
			gatePollMaxSleep: time.Millisecond,
			gatePollBudget:   time.Second,
			awaitResumeMax:   1,
		},
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, sbFactory, nil, false, "main", nil, 0, 0, 1, 0, "", 0, false, 0, false, false, false, "", "")
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "await" {
		t.Fatalf("status = %q, want await after polled actionable-feedback transition", result.Status)
	}
	if got := len(resultFactory.created); got != 2 {
		t.Fatalf("agent launches = %d, want 2 (initial + actionable-feedback resume)", got)
	}
	if got := len(resultFactory.configs); got != 2 {
		t.Fatalf("captured configs = %d, want 2", got)
	}
	if !strings.Contains(resultFactory.configs[1].TaskPrompt, "REVIEW_CHANGES_REQUESTED") {
		t.Fatalf("resumed prompt missing requested-changes evidence:\n%s", resultFactory.configs[1].TaskPrompt)
	}
	logs, err := spyLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if got := countEventsByType(logs, "run.retry"); got != 0 {
		t.Fatalf("run.retry events = %d, want 0", got)
	}
	awaitEvt := findEvent(logs, "run.await")
	if awaitEvt == nil || awaitEvt.Payload["gate"] != gateActionableFeedback {
		t.Fatalf("run.await gate = %v, want actionable-feedback", awaitEvt.Payload["gate"])
	}
}

// CI failure keeps the hard blocked gate even when actionable review
// evidence is retained: the agent cannot repair CI from the working tree.
func TestRunSingle_CIFailurePrecedesActionableEvidenceStaysBlocked(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)

	branch := "42-fix-bug"
	worktreePath := filepath.Join(workDir, "worktree")
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0o755); err != nil {
		t.Fatalf("create worktree task directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	writeTimedOutReviewRequest(t, worktreePath)
	writeFormalChangesRequestedClassification(t, worktreePath, "current")

	resultFactory := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "success", Branch: branch},
	}}
	spyLog := &spyEventLog{}
	sbFactory := &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: worktreePath}}
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs: map[string]*github.PR{branch: {
				Number:            17,
				State:             "open",
				HeadRefName:       branch,
				HeadRefOid:        "current-sha",
				StatusCheckRollup: "failure",
				ReviewDecision:    "CHANGES_REQUESTED",
				MergeStateStatus:  "CLEAN",
			}},
		},
		renderer:        &retryRenderer{result: "rendered prompt"},
		sandboxFactory:  sbFactory,
		eventLog:        spyLog,
		errorLog:        io.Discard,
		runnableFactory: resultFactory,
		runSessionOpts: runSessionOptions{
			currentHead:      func(string) (string, error) { return "current-sha", nil },
			gatePollInitial:  time.Millisecond,
			gatePollMaxSleep: time.Millisecond,
			gatePollBudget:   time.Second,
			awaitResumeMax:   1,
		},
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, sbFactory, nil, false, "main", nil, 0, 0, 1, 0, "", 0, false, 0, false, false, false, "", "")
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "blocked" {
		t.Fatalf("status = %q, want blocked (CI failure precedes actionable evidence)", result.Status)
	}
	if got := len(resultFactory.created); got != 1 {
		t.Fatalf("agent launches = %d, want 1", got)
	}
	logs, err := spyLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if got := countEventsByType(logs, "run.resumed"); got != 0 {
		t.Fatalf("run.resumed events = %d, want 0", got)
	}
	finished := findEvent(logs, "run.finished")
	if finished == nil || finished.Payload["gate"] != "failed" {
		t.Fatalf("terminal event = %#v, want failed gate", finished)
	}
}
