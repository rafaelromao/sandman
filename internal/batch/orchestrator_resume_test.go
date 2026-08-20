package batch

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/events"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// B20: When re-evaluation shows CI failure after resume from await, emit a
// recoverable run.await event with gate: failed.
func TestRunSingle_ModeContinueCIFailureReEvaluatesToAwait(t *testing.T) {
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
	if result.Status != "await" {
		t.Fatalf("status = %q, want await (CI failure after resume from await)", result.Status)
	}
	logs, err := spyLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	await := findEvent(logs, "run.await")
	if await == nil {
		t.Fatalf("run.await event not found: %v", logs)
	}
	if await.Payload["gate"] != "failed" {
		t.Fatalf("gate = %v, want failed", await.Payload["gate"])
	}
	if _, ok := await.Payload["blocker"]; ok {
		t.Fatalf("await blocker = %v, want absent", await.Payload["blocker"])
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
		"gate": "pending",
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
			currentHead:    func(string) (string, error) { return "current-sha", nil },
			awaitResumeMax: 1,
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
			currentHead:    func(string) (string, error) { return "current-sha", nil },
			awaitResumeMax: 1,
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
			currentHead:    func(string) (string, error) { return "current-sha", nil },
			awaitResumeMax: 1,
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
			currentHead: func(string) (string, error) { return "current-sha", nil },
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
			currentHead:    func(string) (string, error) { return "current-sha", nil },
			awaitResumeMax: 1,
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

// CI failure with retained actionable evidence awaits at entry without
// launching an agent: the live PR state is recoverable but not resume-worthy.
func TestRunSingle_CIFailurePrecedesActionableEvidenceAwaitsAtEntry(t *testing.T) {
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
			currentHead:    func(string) (string, error) { return "current-sha", nil },
			awaitResumeMax: 1,
		},
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, sbFactory, nil, false, "main", nil, 0, 0, 1, 0, "", 0, false, 0, false, false, false, "", "")
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "await" {
		t.Fatalf("status = %q, want await (CI failure precedes actionable evidence)", result.Status)
	}
	if got := len(resultFactory.created); got != 0 {
		t.Fatalf("agent launches = %d, want none before recoverable await", got)
	}
	logs, err := spyLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if got := countEventsByType(logs, "run.resumed"); got != 0 {
		t.Fatalf("run.resumed events = %d, want 0", got)
	}
	await := findEvent(logs, "run.await")
	if await == nil || await.Payload["gate"] != "failed" {
		t.Fatalf("await event = %#v, want failed gate", await)
	}
}

// Retained concrete informal feedback resumes a continuation session at entry
// (gate actionable-feedback, REVIEW_INFORMAL_FEEDBACK): the entry launch is the
// resume, and the in-session loop relaunches once on the steady gate before
// exhausting the resume cap.
func TestEntryReevaluation_ModeContinueInformalFeedbackResumesAgentWithEvidence(t *testing.T) {
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
	writeInformalRespondedClassification(t, worktreePath, "Please fix the race in internal/socketpath/socketpath.go.")

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
			currentHead:    func(string) (string, error) { return "current-sha", nil },
			awaitResumeMax: 1,
		},
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, true, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, sbFactory, nil, false, "main", nil, 0, 0, 1, 0, "", 0, false, 0, false, false, false, "", "")
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "await" {
		t.Fatalf("status = %q, want await (post-resume steady gate covered by resume cap fallback)", result.Status)
	}
	if got := len(resultFactory.created); got != 2 {
		t.Fatalf("agent launches = %d, want 2 (entry resume + one in-session relaunch)", got)
	}
	if got := len(resultFactory.configs); got != 2 {
		t.Fatalf("captured configs = %d, want 2", got)
	}
	for i, cfg := range resultFactory.configs {
		if !strings.Contains(cfg.TaskPrompt, "REVIEW_INFORMAL_FEEDBACK") {
			t.Fatalf("config %d prompt missing informal-feedback reason:\n%s", i, cfg.TaskPrompt)
		}
		if !strings.Contains(cfg.TaskPrompt, "socketpath.go") {
			t.Fatalf("config %d prompt missing informal feedback body:\n%s", i, cfg.TaskPrompt)
		}
	}
	logs, err := spyLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if got := countEventsByType(logs, "run.retry"); got != 0 {
		t.Fatalf("run.retry events = %d, want 0 (resume must not consume retry budget)", got)
	}
	resumedEvt := findEvent(logs, "run.resumed")
	if resumedEvt == nil || resumedEvt.Payload["gate"] != gateActionableFeedback {
		t.Fatalf("run.resumed gate = %v, want actionable-feedback", resumedEvt.Payload["gate"])
	}
	if got := countEventsByType(logs, "run.resumed"); got != 1 {
		t.Fatalf("run.resumed events = %d, want exactly 1 (entry relaunch emits no run.resumed; one in-session relaunch does)", got)
	}
	awaitEvt := findEvent(logs, "run.await")
	if awaitEvt == nil || awaitEvt.Payload["gate"] != gateActionableFeedback {
		t.Fatalf("run.await gate = %v, want actionable-feedback", awaitEvt.Payload["gate"])
	}
	if request, ok := awaitEvt.Payload["review_request"].(map[string]any); !ok || request["informal_feedback"] == nil {
		t.Fatalf("run.await review request missing informal feedback: %#v", awaitEvt.Payload["review_request"])
	}
}

// Boilerplate-only retained feedback stays a pending gate: the continuation
// session ends at entry with run.await and must not launch the agent.
func TestEntryReevaluation_ModeContinueBoilerplateInformalAwaitsImmediately(t *testing.T) {
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
	writeInformalRespondedClassification(t, worktreePath, "looks good to me, thanks!")

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
			currentHead:    func(string) (string, error) { return "current-sha", nil },
			awaitResumeMax: 1,
		},
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, true, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, sbFactory, nil, false, "main", nil, 0, 0, 1, 0, "", 0, false, 0, false, false, false, "", "")
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "await" {
		t.Fatalf("status = %q, want await (boilerplate evidence must not launch)", result.Status)
	}
	if got := len(resultFactory.created); got != 0 {
		t.Fatalf("agent launches = %d, want 0 at pending gate", got)
	}
	logs, err := spyLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if got := countEventsByType(logs, "run.resumed"); got != 0 {
		t.Fatalf("run.resumed events = %d, want 0", got)
	}
	if got := countEventsByType(logs, "run.retry"); got != 0 {
		t.Fatalf("run.retry events = %d, want 0", got)
	}
	awaitEvt := findEvent(logs, "run.await")
	if awaitEvt == nil || awaitEvt.Payload["gate"] != "pending" {
		t.Fatalf("run.await gate = %v, want pending", awaitEvt.Payload["gate"])
	}
	if request, ok := awaitEvt.Payload["review_request"].(map[string]any); ok && request["informal_feedback"] != nil {
		t.Fatalf("informal_feedback = %#v, want none for boilerplate feedback", request["informal_feedback"])
	}
}

// In-session resume driven by retained informal feedback: the agent completes
// cleanly, the pending gate resolves through the informal hook, and the run
// relaunches the agent exactly once (bounded by the resume cap) with the
// informal evidence in the prompt and zero run.retry.
func TestRunSingle_InformalFeedbackResumesWithinSameAttempt(t *testing.T) {
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
	writeInformalRespondedClassification(t, worktreePath, "Please fix the race in internal/socketpath/socketpath.go.")

	resultFactory := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "success", Branch: branch},
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
			currentHead:    func(string) (string, error) { return "current-sha", nil },
			awaitResumeMax: 1,
		},
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, sbFactory, nil, false, "main", nil, 0, 0, 1, 0, "", 0, false, 0, false, false, false, "", "")
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "await" {
		t.Fatalf("status = %q, want await after resumed relaunch on steady informal gate", result.Status)
	}
	if got := len(resultFactory.created); got != 2 {
		t.Fatalf("agent launches = %d, want 2 (entry resume + informal-feedback resume)", got)
	}
	if got := len(resultFactory.configs); got != 2 {
		t.Fatalf("captured configs = %d, want 2", got)
	}
	if !strings.Contains(resultFactory.configs[1].TaskPrompt, "REVIEW_INFORMAL_FEEDBACK") {
		t.Fatalf("resumed prompt missing informal-feedback evidence:\n%s", resultFactory.configs[1].TaskPrompt)
	}
	if !strings.Contains(resultFactory.configs[1].TaskPrompt, "socketpath.go") {
		t.Fatalf("resumed prompt missing informal feedback body:\n%s", resultFactory.configs[1].TaskPrompt)
	}
	logs, err := spyLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if got := countEventsByType(logs, "run.retry"); got != 0 {
		t.Fatalf("run.retry events = %d, want 0 (resume must not consume retry budget)", got)
	}
	resumedEvt := findEvent(logs, "run.resumed")
	if resumedEvt == nil || resumedEvt.Payload["gate"] != gateActionableFeedback {
		t.Fatalf("run.resumed gate = %v, want actionable-feedback", resumedEvt.Payload["gate"])
	}
	if got := countEventsByType(logs, "run.resumed"); got != 1 {
		t.Fatalf("run.resumed events = %d, want exactly 1", got)
	}
	if got, _ := resumedEvt.Payload["reason"].(string); got != "feedback" {
		t.Fatalf("run.resumed reason = %v, want feedback", resumedEvt.Payload["reason"])
	}
	if request, ok := resumedEvt.Payload["review_request"].(map[string]any); !ok || request["informal_feedback"] == nil {
		t.Fatalf("run.resumed review request missing informal feedback: %#v", resumedEvt.Payload["review_request"])
	}
	awaitEvt := findEvent(logs, "run.await")
	if awaitEvt == nil || awaitEvt.Payload["gate"] != gateActionableFeedback {
		t.Fatalf("run.await gate = %v, want actionable-feedback", awaitEvt.Payload["gate"])
	}
}

// Vertical lifecycle (issue #2595 demo): a fresh run exits to the delegated
// review gate (run.await pending), a CHANGES_REQUESTED review resumes the
// agent with actionable evidence, the pushed head lands back on a pending
// gate, and the APPROVED+CLEAN transition resumes the agent again before the
// PR merges into verified success. Across the whole lifecycle there is zero
// run.retry, at most three launches per session, and the worktree task.md is
// preserved.
func TestRunSingle_FullLifecycleRequestsFeedbackThenApprovalThenMergeSuccess(t *testing.T) {
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

	resultFactory := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "success", Branch: branch},
		{IssueNumber: 42, Status: "success", Branch: branch},
		{IssueNumber: 42, Status: "success", Branch: branch},
		{IssueNumber: 42, Status: "success", Branch: branch},
		{IssueNumber: 42, Status: "success", Branch: branch},
	}}
	spyLog := &spyEventLog{}
	pr := &github.PR{
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
		prs:    map[string]*github.PR{branch: pr},
	}
	headForGate := "current-sha"
	phase := 0
	lookups := 0
	client.findPRHook = func() {
		lookups++
		switch phase {
		case 1:
			if lookups >= 8 {
				pr.HeadRefOid = "new-sha"
				pr.StatusCheckRollup = "pending"
				pr.ReviewDecision = "REVIEW_REQUIRED"
				pr.MergeStateStatus = "BLOCKED"
			}
		case 2:
			if lookups >= 6 {
				pr.State = "merged"
				pr.Merged = true
				pr.Body = "Closes #42"
			}
		}
	}

	sbFactory := &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: worktreePath}}
	runSession := func(continuation bool, previousRunID string) (AgentRunResult, bool) {
		o := &Orchestrator{
			githubClient:    client,
			renderer:        &retryRenderer{result: "rendered prompt"},
			sandboxFactory:  sbFactory,
			eventLog:        spyLog,
			errorLog:        io.Discard,
			runnableFactory: resultFactory,
			runSessionOpts: runSessionOptions{
				currentHead: func(string) (string, error) { return headForGate, nil },
			},
		}
		cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
		return o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, continuation, map[int]string{42: previousRunID}, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, sbFactory, nil, false, "main", nil, 0, 0, 1, 0, "", 0, false, 0, false, false, false, "", "")
	}

	phase = 0
	result, started := runSession(false, "")
	if !started {
		t.Fatalf("session 1 not started: %q", result.Status)
	}
	if result.Status != "await" || len(resultFactory.created) != 1 {
		t.Fatalf("session 1 = (%q, %d launches), want await after 1 launch", result.Status, len(resultFactory.created))
	}

	pr.ReviewDecision = "CHANGES_REQUESTED"
	pr.StatusCheckRollup = "success"
	pr.MergeStateStatus = "CLEAN"
	writeTimedOutReviewRequest(t, worktreePath)
	writeFormalChangesRequestedClassification(t, worktreePath, "current")

	phase = 1
	headForGate = "current-sha"
	lookups = 0
	result, started = runSession(true, "run-a")
	if !started {
		t.Fatalf("session 2 not started: %q", result.Status)
	}
	if result.Status != "await" || len(resultFactory.created) != 3 {
		t.Fatalf("session 2 = (%q, %d launches), want await after entry resume + in-session resume", result.Status, len(resultFactory.created))
	}

	pr.ReviewDecision = "APPROVED"
	pr.StatusCheckRollup = "success"
	pr.MergeStateStatus = "CLEAN"
	phase = 2
	headForGate = "new-sha"
	lookups = 0
	result, started = runSession(true, "run-b")
	if !started {
		t.Fatalf("session 3 not started: %q", result.Status)
	}
	if result.Status != "success" || len(resultFactory.created) != 5 {
		t.Fatalf("session 3 = (%q, %d launches), want success after merged PR (entry resume + merge resume)", result.Status, len(resultFactory.created))
	}

	logs, err := spyLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if got := countEventsByType(logs, "run.retry"); got != 0 {
		t.Fatalf("run.retry events = %d, want 0", got)
	}
	if got := countEventsByType(logs, "run.resumed"); got != 2 {
		t.Fatalf("run.resumed events = %d, want 2", got)
	}
	resumedEvt := findEvent(logs, "run.resumed")
	if resumedEvt == nil || resumedEvt.Payload["reason"] != "feedback" {
		t.Fatalf("run.resumed = %#v, want feedback reason", resumedEvt)
	}
	if resumedEvt.Payload["run_id"] != resumedEvt.RunID {
		t.Fatalf("run.resumed run_id = %v, want own RunID %s", resumedEvt.Payload["run_id"], resumedEvt.RunID)
	}
	var awaits []events.Event
	for _, e := range logs {
		if e.Type == "run.await" {
			awaits = append(awaits, e)
		}
	}
	if len(awaits) != 2 {
		t.Fatalf("run.await events = %d, want 2", len(awaits))
	}
	if awaits[0].Payload["gate"] != "pending" || awaits[1].Payload["gate"] != "pending" {
		t.Fatalf("await gates = %v, %v, want pending, pending", awaits[0].Payload["gate"], awaits[1].Payload["gate"])
	}
	finished := findEvent(logs, "run.finished")
	if finished == nil {
		t.Fatalf("run.finished event not found: %v", logs)
	}
	if status, _ := finished.Payload["status"].(string); !events.RunStatusFromPayload(status).IsSuccess() {
		t.Fatalf("run.finished = %#v, want success", finished)
	}
	if countEventsByType(logs, "run.started") != 1 || countEventsByType(logs, "run.continued") != 2 {
		t.Fatalf("run.started = %d, run.continued = %d, want 1 and 2", countEventsByType(logs, "run.started"), countEventsByType(logs, "run.continued"))
	}
	var seq []string
	for _, e := range logs {
		switch e.Type {
		case "run.started", "run.continued", "run.resumed", "run.await":
			seq = append(seq, e.Type)
		}
	}
	wantSeq := []string{"run.started", "run.await", "run.continued", "run.resumed", "run.await", "run.continued", "run.resumed"}
	if len(seq) != len(wantSeq) {
		t.Fatalf("lifecycle sequence = %v, want %v", seq, wantSeq)
	}
	for i, want := range wantSeq {
		if seq[i] != want {
			t.Fatalf("lifecycle event %d = %s, want %s (sequence %v)", i, seq[i], want, seq)
		}
	}
	if len(resultFactory.configs) != 5 {
		t.Fatalf("captured configs = %d, want 5", len(resultFactory.configs))
	}
	if strings.Contains(resultFactory.configs[0].TaskPrompt, "## Review Evidence") {
		t.Fatalf("initial launch must not carry evidence")
	}
	for i, evi := range []string{"REVIEW_CHANGES_REQUESTED", "REVIEW_CHANGES_REQUESTED", "REVIEW_APPROVED", "REVIEW_APPROVED"} {
		if !strings.Contains(resultFactory.configs[i+1].TaskPrompt, evi) {
			t.Fatalf("launch %d prompt missing %s evidence:\n%s", i+1, evi, resultFactory.configs[i+1].TaskPrompt)
		}
	}
	task, err := os.ReadFile(filepath.Join(worktreePath, ".sandman", "task.md"))
	if err != nil {
		t.Fatalf("read final task: %v", err)
	}
	if !strings.Contains(string(task), "# Task") {
		t.Fatalf("task.md lost original content: %s", task)
	}
}

// Evidence construction slice: the two resume variants carry distinct
// defaults (merge evidence for ready-to-merge, review evidence for
// actionable-feedback), both enriched with the live PR coordinates.
func TestResumeEvidenceFor_ReadyToMergeVariantDefaults(t *testing.T) {
	s := &runSession{
		deps: runDeps{githubClient: &fakeGitHubClient{
			prs: map[string]*github.PR{"42-fix": {Number: 42, HeadRefOid: "abc123"}},
		}},
	}
	evidence := s.resumeEvidenceFor(context.Background(), "42-fix", map[string]any{
		"gate": "ready-to-merge",
	})
	if evidence["reason"] != "REVIEW_APPROVED" {
		t.Fatalf("reason = %v, want REVIEW_APPROVED", evidence["reason"])
	}
	if evidence["outcome"] != "ready-to-merge" {
		t.Fatalf("outcome = %v, want ready-to-merge", evidence["outcome"])
	}
	if _, ok := evidence["next_action"].(string); !ok || !strings.Contains(evidence["next_action"].(string), "merge gate") {
		t.Fatalf("next_action = %v, want merge-gate instruction", evidence["next_action"])
	}
	if evidence["pull_request"] != 42 || evidence["head_sha"] != "abc123" {
		t.Fatalf("live PR coordinates not enriched: %v", evidence)
	}
}

func TestResumeEvidenceFor_ActionableFeedbackVariantDefaults(t *testing.T) {
	s := &runSession{
		deps: runDeps{githubClient: &fakeGitHubClient{
			prs: map[string]*github.PR{"42-fix": {Number: 42, HeadRefOid: "abc123"}},
		}},
	}
	evidence := s.resumeEvidenceFor(context.Background(), "42-fix", map[string]any{
		"gate": "actionable-feedback",
		"review_request": map[string]any{
			"repository":   "owner/repo",
			"pull_request": float64(42),
			"head_sha":     "abc123",
		},
	})
	if evidence["reason"] != "REVIEW_CHANGES_REQUESTED" {
		t.Fatalf("reason = %v, want REVIEW_CHANGES_REQUESTED", evidence["reason"])
	}
	if evidence["outcome"] != "actionable-feedback" {
		t.Fatalf("outcome = %v, want actionable-feedback", evidence["outcome"])
	}
	if reviewRequest, ok := evidence["review_request"].(map[string]any); !ok || reviewRequest["pull_request"] != float64(42) {
		t.Fatalf("review_request not preserved: %v", evidence["review_request"])
	}
}

// The no-evidence resume prompt is byte-identical to the plain continuation
// prompt, keeping the continuation baseline (continue flow) unchanged.
func TestResumePromptFor_NoEvidenceIsByteIdenticalContinuation(t *testing.T) {
	s := &runSession{}
	task := "# Task\n\nDo the work.\n"
	got := s.resumePromptFor(task, nil, 0)
	want := prompt.ContinuationTaskPromptWithReviewTimeout(task, 0)
	if got != want {
		t.Fatalf("no-evidence resume prompt diverges from continuation prompt:\ngot:  %s\nwant: %s", got, want)
	}
}
