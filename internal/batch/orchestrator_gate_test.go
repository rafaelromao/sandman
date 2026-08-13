package batch

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/testenv"
)

const gateTestBranch = "42-fix-bug"

func writeTimedOutReviewRequest(t *testing.T, workDir string) {
	t.Helper()
	stateDir := filepath.Join(workDir, ".sandman", "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("create review state directory: %v", err)
	}
	request := `{
  "protocol": "review-wait/v1",
  "repository": "owner/repo",
  "pull_request": 17,
  "head_sha": "current-sha",
  "trigger_id": "https://github.com/owner/repo/pull/17#issuecomment-1001",
  "trigger_prefix": "/sandman review",
  "trigger_created_at": "2026-08-13T10:00:00Z",
  "confirmed_at": "2026-08-13T10:00:00Z",
  "started_at": "2026-08-13T10:00:00Z",
  "deadline_at": "unix:2800",
  "started_unix_seconds": 1000,
  "deadline_unix_seconds": 2800,
  "effective_timeout_seconds": 1800,
  "poll_plan": [120, 60, 60, 30]
}
`
	if err := os.WriteFile(filepath.Join(stateDir, "17.review_request.json"), []byte(request), 0o600); err != nil {
		t.Fatalf("write review request: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "17.head_sha"), []byte("current-sha\n"), 0o600); err != nil {
		t.Fatalf("write review head sidecar: %v", err)
	}
	state := `{
  "protocol": "review-wait/v1",
  "repository": "owner/repo",
  "pull_request": 17,
  "head_sha": "current-sha",
  "trigger_id": "https://github.com/owner/repo/pull/17#issuecomment-1001",
  "trigger_prefix": "/sandman review",
  "trigger_created_at": "2026-08-13T10:00:00Z",
  "confirmed_at": "2026-08-13T10:00:00Z",
  "started_at": "2026-08-13T10:00:00Z",
  "deadline_at": "unix:2800",
  "started_unix_seconds": 1000,
  "effective_timeout_seconds": 1800,
  "deadline_unix_seconds": 2800,
  "poll_plan": [120, 60, 60, 30],
  "state": "timed_out",
  "lifecycle": "started",
  "observed_head_sha": "current-sha",
  "elapsed_seconds": 1800,
  "reason": "request-deadline-exhausted",
  "snapshot_path": null,
  "evidence": {
    "response_counts": {
      "top_level": 0,
      "formal_reviews": 0,
      "inline_comments": 0
    }
  }
}
`
	if err := os.WriteFile(filepath.Join(stateDir, "17.review_request.json.state"), []byte(state), 0o600); err != nil {
		t.Fatalf("write review state: %v", err)
	}
}

func TestExternalGate_ReviewTimeoutBlocksWithoutRetry(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)

	branch := gateTestBranch
	worktreePath := filepath.Join(workDir, "worktree")
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0o755); err != nil {
		t.Fatalf("create worktree task directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	writeTimedOutReviewRequest(t, worktreePath)

	sb := &retrySandbox{workDir: worktreePath}
	sbFactory := &retrySandboxFactory{sandbox: sb}
	factory := &fakeRunnableFactory{results: []AgentRunResult{{
		IssueNumber: 42,
		Status:      "success",
		Branch:      branch,
	}}}
	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}},
		prs: map[string]*github.PR{branch: {
			Number:            17,
			State:             "open",
			HeadRefName:       branch,
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "success",
			ReviewDecision:    "APPROVED",
			MergeStateStatus:  "CLEAN",
		}},
	}
	o := NewOrchestrator(
		client,
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(sbFactory),
		WithRunnableFactory(factory),
		WithRunSessionOpts(gateTestRunOptions()),
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
		t.Fatalf("expected run to start, status=%q", result.Status)
	}
	if result.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", result.Status)
	}
	if len(factory.created) != 1 {
		t.Fatalf("agent launches = %d, want 1", len(factory.created))
	}

	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if got := countEventsByType(logs, "run.retry"); got != 0 {
		t.Fatalf("run.retry events = %d, want 0", got)
	}
	finished := findEvent(logs, "run.finished")
	if finished == nil {
		t.Fatalf("run.finished event not found: %v", logs)
	}
	if got := finished.Payload["gate"]; got != "review-timeout" {
		t.Fatalf("terminal gate = %v, want review-timeout", got)
	}
	if got := finished.Payload["reason"]; got != "REVIEW_TIMEOUT" {
		t.Fatalf("terminal reason = %v, want REVIEW_TIMEOUT", got)
	}
	request, ok := finished.Payload["review_request"].(map[string]any)
	if !ok {
		t.Fatalf("terminal review_request = %#v, want object", finished.Payload["review_request"])
	}
	if request["pull_request"] != float64(17) || request["head_sha"] != "current-sha" || request["trigger_id"] != "https://github.com/owner/repo/pull/17#issuecomment-1001" {
		t.Fatalf("terminal request identity = %#v", request)
	}
	for field, want := range map[string]any{
		"deadline_unix_seconds":     float64(2800),
		"effective_timeout_seconds": float64(1800),
		"elapsed_seconds":           float64(1800),
		"reason":                    "REVIEW_TIMEOUT",
		"next_action":               reviewTimeoutNextAction,
	} {
		if request[field] != want {
			t.Fatalf("terminal request %s = %v, want %v", field, request[field], want)
		}
	}

	task, err := os.ReadFile(filepath.Join(worktreePath, ".sandman", "task.md"))
	if err != nil {
		t.Fatalf("read task: %v", err)
	}
	for _, want := range []string{
		"REVIEW_TIMEOUT",
		"Repository: owner/repo",
		"Pull request: #17",
		"Current head: current-sha",
		"Trigger: https://github.com/owner/repo/pull/17#issuecomment-1001",
		"Deadline: unix:2800 (2800)",
		"Budget: 1800 seconds",
		"Elapsed: 1800 seconds",
		"Next action:",
	} {
		if !strings.Contains(string(task), want) {
			t.Fatalf("task missing %q: %s", want, task)
		}
	}
	runLog, err := os.ReadFile(filepath.Join(workDir, ".sandman", "batches", "runs", "run-test", "run.log"))
	if err == nil && !strings.Contains(string(runLog), "REVIEW_TIMEOUT") {
		t.Fatalf("run log missing timeout handoff: %s", runLog)
	}
}

func TestExternalGate_ReviewTimeoutPreservesAgentFailureRetryBehavior(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)
	branch := gateTestBranch
	worktreePath := filepath.Join(workDir, "worktree")
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0o755); err != nil {
		t.Fatalf("create worktree task directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	writeTimedOutReviewRequest(t, worktreePath)
	sb := &retrySandbox{workDir: worktreePath}
	sbFactory := &retrySandboxFactory{sandbox: sb}
	factory := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "failure", Branch: branch},
		{IssueNumber: 42, Status: "success", Branch: branch},
	}}
	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}},
		prs: map[string]*github.PR{branch: {
			Number: 17, State: "open", HeadRefName: branch, HeadRefOid: "current-sha",
			StatusCheckRollup: "success", ReviewDecision: "APPROVED", MergeStateStatus: "CLEAN",
		}},
	}
	o := NewOrchestrator(client, &retryRenderer{result: "rendered prompt"}, nil, eventLog,
		WithErrorLog(io.Discard), WithSandboxFactory(sbFactory), WithRunnableFactory(factory), WithRunSessionOpts(gateTestRunOptions()))
	bc := BatchConfig{
		Cfg: &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}, AgentName: "opencode",
		AgentCfg: config.Agent{Command: "echo hi"}, IdentityResolver: noopIdentityResolver(), Retries: 1,
	}
	result, started := o.newRunExecutor(context.Background(), bc, sbFactory, nil).Execute(context.Background(), RowSpec{
		IssueNumber: 42, Branches: map[int]string{42: branch}, BaseBranch: "main",
	})
	if !started || result.Status != "blocked" {
		t.Fatalf("timeout after retry = (%t, %q), want started blocked", started, result.Status)
	}
	if len(factory.created) != 2 {
		t.Fatalf("agent launches = %d, want 2 (failure retry followed by clean timeout handoff)", len(factory.created))
	}
	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if countEventsByType(logs, "run.retry") != 1 {
		t.Fatal("agent failure did not consume its configured retry before the timeout handoff")
	}
	finished := findEvent(logs, "run.finished")
	if finished == nil || finished.Payload["gate"] != gateReviewTimeout {
		t.Fatalf("terminal event = %#v, want review-timeout gate", finished)
	}
}

func TestExternalGate_ReviewTimeoutRejectsStaleOrMalformedState(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)
	worktreePath := filepath.Join(workDir, "worktree")
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0o755); err != nil {
		t.Fatalf("create worktree task directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	writeTimedOutReviewRequest(t, worktreePath)
	statePath := filepath.Join(worktreePath, ".sandman", "state", "17.review_request.json.state")
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	state = []byte(strings.Replace(string(state), `"head_sha": "current-sha"`, `"head_sha": "stale-sha"`, 1))
	if err := os.WriteFile(statePath, state, 0o600); err != nil {
		t.Fatalf("write stale state: %v", err)
	}

	sb := &retrySandbox{workDir: worktreePath}
	sbFactory := &retrySandboxFactory{sandbox: sb}
	factory := &fakeRunnableFactory{results: []AgentRunResult{{IssueNumber: 42, Status: "success", Branch: gateTestBranch}}}
	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}},
		prs: map[string]*github.PR{gateTestBranch: {
			Number:            17,
			State:             "merged",
			Merged:            true,
			HeadRefName:       gateTestBranch,
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "success",
			ReviewDecision:    "APPROVED",
			MergeStateStatus:  "CLEAN",
		}},
	}
	o := NewOrchestrator(client, &retryRenderer{result: "rendered prompt"}, nil, eventLog,
		WithErrorLog(io.Discard), WithSandboxFactory(sbFactory), WithRunnableFactory(factory), WithRunSessionOpts(gateTestRunOptions()))
	bc := BatchConfig{
		Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	result, started := o.newRunExecutor(context.Background(), bc, sbFactory, nil).Execute(context.Background(), RowSpec{
		IssueNumber: 42, Branches: map[int]string{42: gateTestBranch}, BaseBranch: "main",
	})
	if !started || result.Status != "blocked" {
		t.Fatalf("stale state result = (%t, %q), want started blocked", started, result.Status)
	}
	if len(factory.created) != 1 {
		t.Fatalf("agent launches = %d, want 1", len(factory.created))
	}
	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if countEventsByType(logs, "run.retry") != 0 {
		t.Fatal("stale review state consumed a retry")
	}
	finished := findEvent(logs, "run.finished")
	if finished == nil || finished.Payload["gate"] != gateReviewTimeoutError {
		t.Fatalf("stale state terminal event = %#v, want %q", finished, gateReviewTimeoutError)
	}
}

func TestExternalGate_ReviewTimeoutRetainsResponseCounters(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)
	worktreePath := filepath.Join(workDir, "worktree")
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0o755); err != nil {
		t.Fatalf("create worktree task directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	writeTimedOutReviewRequest(t, worktreePath)
	statePath := filepath.Join(worktreePath, ".sandman", "state", "17.review_request.json.state")
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	stateText := strings.Replace(string(state), `"top_level": 0`, `"top_level": 2`, 1)
	stateText = strings.Replace(stateText, `"formal_reviews": 0`, `"formal_reviews": 1`, 1)
	stateText = strings.Replace(stateText, `"inline_comments": 0`, `"inline_comments": 3`, 1)
	if err := os.WriteFile(statePath, []byte(stateText), 0o600); err != nil {
		t.Fatalf("write counters: %v", err)
	}

	sb := &retrySandbox{workDir: worktreePath}
	sbFactory := &retrySandboxFactory{sandbox: sb}
	factory := &fakeRunnableFactory{results: []AgentRunResult{{IssueNumber: 42, Status: "success", Branch: gateTestBranch}}}
	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}},
		prs: map[string]*github.PR{gateTestBranch: {
			Number: 17, State: "open", HeadRefName: gateTestBranch, HeadRefOid: "current-sha",
			StatusCheckRollup: "pending", MergeStateStatus: "BLOCKED",
		}},
	}
	o := NewOrchestrator(client, &retryRenderer{result: "rendered prompt"}, nil, eventLog,
		WithErrorLog(io.Discard), WithSandboxFactory(sbFactory), WithRunnableFactory(factory), WithRunSessionOpts(gateTestRunOptions()))
	bc := BatchConfig{
		Cfg: &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}, AgentName: "opencode",
		AgentCfg: config.Agent{Command: "echo hi"}, IdentityResolver: noopIdentityResolver(), Retries: 3,
	}
	result, started := o.newRunExecutor(context.Background(), bc, sbFactory, nil).Execute(context.Background(), RowSpec{
		IssueNumber: 42, Branches: map[int]string{42: gateTestBranch}, BaseBranch: "main",
	})
	if !started || result.Status != "blocked" {
		t.Fatalf("counter result = (%t, %q), want started blocked", started, result.Status)
	}
	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	finished := findEvent(logs, "run.finished")
	request, ok := finished.Payload["review_request"].(map[string]any)
	if !ok {
		t.Fatalf("review request payload = %#v", finished.Payload["review_request"])
	}
	counts, ok := request["response_counts"].(map[string]any)
	if !ok || counts["top_level"] != float64(2) || counts["formal_reviews"] != float64(1) || counts["inline_comments"] != float64(3) {
		t.Fatalf("response counts = %#v, want top=2 formal=1 inline=3", request["response_counts"])
	}
}

func TestExternalGate_ReviewTimeoutRejectsInconsistentStatePair(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(string) string
	}{
		{
			name: "poll plan changed",
			mutate: func(state string) string {
				return strings.Replace(state, `"poll_plan": [120, 60, 60, 30]`, `"poll_plan": [30]`, 1)
			},
		},
		{
			name: "lifecycle invalid",
			mutate: func(state string) string {
				return strings.Replace(state, `"lifecycle": "started"`, `"lifecycle": "finished"`, 1)
			},
		},
		{
			name: "elapsed negative",
			mutate: func(state string) string {
				return strings.Replace(state, `"elapsed_seconds": 1800`, `"elapsed_seconds": -1`, 1)
			},
		},
		{
			name: "elapsed missing",
			mutate: func(state string) string {
				return strings.Replace(state, "  \"elapsed_seconds\": 1800,\n", "", 1)
			},
		},
		{
			name: "response counts missing",
			mutate: func(state string) string {
				return strings.Replace(state, `"response_counts": {`, `"response_counts": null`, 1)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workDir := testenv.MkdirShort(t, "sm-orch-")
			writeTimedOutReviewRequest(t, workDir)
			statePath := filepath.Join(workDir, ".sandman", "state", "17.review_request.json.state")
			state, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatalf("read review state: %v", err)
			}
			if err := os.WriteFile(statePath, []byte(tt.mutate(string(state))), 0o600); err != nil {
				t.Fatalf("write review state: %v", err)
			}

			_, err = readReviewTimeoutHandoff(workDir, "owner/repo", &github.PR{
				Number:     17,
				State:      "open",
				HeadRefOid: "current-sha",
			}, "current-sha")
			if err == nil {
				t.Fatal("readReviewTimeoutHandoff() accepted an inconsistent state pair")
			}
		})
	}
}

func TestExternalGate_ReviewTimeoutRejectsSupersededResponse(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeTimedOutReviewRequest(t, workDir)
	statePath := filepath.Join(workDir, ".sandman", "state", "17.review_request.json.state")
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read review state: %v", err)
	}
	stateText := strings.Replace(string(state), `"state": "timed_out"`, `"state": "responded"`, 1)
	stateText = strings.Replace(stateText, `"evidence": {`, `"evidence": {\n    "classification": {"request_state": "superseded"},`, 1)
	if err := os.WriteFile(statePath, []byte(stateText), 0o600); err != nil {
		t.Fatalf("write superseded state: %v", err)
	}
	if _, err := readReviewTimeoutHandoff(workDir, "owner/repo", &github.PR{
		Number: 17, State: "open", HeadRefOid: "current-sha",
	}, "current-sha"); err == nil {
		t.Fatal("readReviewTimeoutHandoff() accepted a superseded response")
	}
}

func TestExternalGate_ReviewTimeoutIgnoresHeadSidecarWithoutRequest(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)
	worktreePath := filepath.Join(workDir, "worktree")
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman", "state"), 0o755); err != nil {
		t.Fatalf("create review state directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "state", "17.head_sha"), []byte("current-sha\n"), 0o600); err != nil {
		t.Fatalf("write review head sidecar: %v", err)
	}

	sb := &retrySandbox{workDir: worktreePath}
	sbFactory := &retrySandboxFactory{sandbox: sb}
	factory := &fakeRunnableFactory{results: []AgentRunResult{{IssueNumber: 42, Status: "success", Branch: gateTestBranch}}}
	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}},
		prs: map[string]*github.PR{gateTestBranch: {
			Number:            17,
			State:             "open",
			HeadRefName:       gateTestBranch,
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "success",
			ReviewDecision:    "APPROVED",
			MergeStateStatus:  "CLEAN",
		}},
	}
	o := NewOrchestrator(client, &retryRenderer{result: "rendered prompt"}, nil, eventLog,
		WithErrorLog(io.Discard), WithSandboxFactory(sbFactory), WithRunnableFactory(factory), WithRunSessionOpts(gateTestRunOptions()))
	bc := BatchConfig{
		Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	result, started := o.newRunExecutor(context.Background(), bc, sbFactory, nil).Execute(context.Background(), RowSpec{
		IssueNumber: 42, Branches: map[int]string{42: gateTestBranch}, BaseBranch: "main",
	})
	if !started || result.Status != "blocked" {
		t.Fatalf("head-only state result = (%t, %q), want started blocked", started, result.Status)
	}
	if len(factory.created) != 1 {
		t.Fatalf("agent launches = %d, want 1", len(factory.created))
	}
	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if countEventsByType(logs, "run.retry") != 0 {
		t.Fatal("head-only review state consumed a retry")
	}
	finished := findEvent(logs, "run.finished")
	if finished == nil || finished.Payload["gate"] != gateReadyToMerge {
		t.Fatalf("head-only state terminal event = %#v, want %q", finished, gateReadyToMerge)
	}
}

func TestExternalGate_ReviewTimeoutIgnoresRetainedArtifactsWithoutCurrentPR(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeTimedOutReviewRequest(t, workDir)

	session := &runSession{
		deps: runDeps{
			githubClient: &fakeGitHubClient{prs: map[string]*github.PR{}},
			errorLog:     io.Discard,
		},
		opts: gateTestRunOptions(),
	}
	status, extras, handled := session.handleReviewTimeoutGate(context.Background(), workDir, "missing-pr-branch", "", "run-test", "current-sha")
	if handled || status != "" || extras != nil {
		t.Fatalf("retained timeout without current PR = (%q, %#v, %t), want ordinary no-PR path", status, extras, handled)
	}
}

func TestExternalGate_ReviewTimeoutRejectsMissingEvidenceCounters(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeTimedOutReviewRequest(t, workDir)
	statePath := filepath.Join(workDir, ".sandman", "state", "17.review_request.json.state")
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read review state: %v", err)
	}
	state = []byte(strings.Replace(string(state), `"evidence": {
    "response_counts": {
      "top_level": 0,
      "formal_reviews": 0,
      "inline_comments": 0
    }
  }`, `"evidence": null`, 1))
	if err := os.WriteFile(statePath, state, 0o600); err != nil {
		t.Fatalf("write empty-evidence state: %v", err)
	}

	_, err = readReviewTimeoutHandoff(workDir, "owner/repo", &github.PR{
		Number:     17,
		State:      "open",
		HeadRefOid: "current-sha",
	}, "current-sha")
	if err == nil {
		t.Fatal("readReviewTimeoutHandoff() accepted missing response counters")
	}
}

func writeRetainedCurrentHeadApproval(t *testing.T, workDir string) {
	t.Helper()
	stateDir := filepath.Join(workDir, ".sandman", "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("create review state directory: %v", err)
	}
	request := `{
  "protocol": "review-wait/v1",
  "repository": "owner/repo",
  "pull_request": 17,
  "head_sha": "current-sha",
  "trigger_id": "https://github.com/owner/repo/pull/17#issuecomment-1001",
  "trigger_prefix": "/sandman review",
  "trigger_created_at": "2026-08-13T10:00:00Z",
  "confirmed_at": "2026-08-13T10:00:00Z",
  "started_at": "2026-08-13T10:00:00Z",
  "deadline_at": "unix:1786617000",
  "started_unix_seconds": 1786615200,
  "deadline_unix_seconds": 1786617000,
  "effective_timeout_seconds": 1800,
  "poll_plan": [120, 60, 60, 30]
}
`
	if err := os.WriteFile(filepath.Join(stateDir, "17.review_request.json"), []byte(request), 0o600); err != nil {
		t.Fatalf("write review request: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "17.head_sha"), []byte("current-sha\n"), 0o600); err != nil {
		t.Fatalf("write review head sidecar: %v", err)
	}
	state := `{
  "protocol": "review-wait/v1",
  "repository": "owner/repo",
  "pull_request": 17,
  "head_sha": "current-sha",
  "trigger_id": "https://github.com/owner/repo/pull/17#issuecomment-1001",
  "trigger_prefix": "/sandman review",
  "trigger_created_at": "2026-08-13T10:00:00Z",
  "confirmed_at": "2026-08-13T10:00:00Z",
  "started_at": "2026-08-13T10:00:00Z",
  "deadline_at": "unix:1786617000",
  "started_unix_seconds": 1786615200,
  "effective_timeout_seconds": 1800,
  "deadline_unix_seconds": 1786617000,
  "poll_plan": [120, 60, 60, 30],
  "state": "timed_out",
  "lifecycle": "started",
  "observed_head_sha": "current-sha",
  "elapsed_seconds": 1800,
  "reason": "request-deadline-exhausted",
  "snapshot_path": null,
  "evidence": {
    "response_counts": {
      "top_level": 0,
      "formal_reviews": 1,
      "inline_comments": 0
    },
    "classification": {
      "protocol": "review-classification/v1",
      "request": {
        "repository": "owner/repo",
        "pull_request": 17,
        "head_sha": "current-sha",
        "trigger_id": "https://github.com/owner/repo/pull/17#issuecomment-1001",
        "trigger_prefix": "/sandman review",
        "trigger_created_at": "2026-08-13T10:00:00Z",
        "deadline_at": "unix:1786617000",
        "deadline_unix_seconds": 1786617000
      },
      "observed_head_sha": "current-sha",
      "request_state": "active",
      "decision": "approved",
      "window": {
        "start": "2026-08-13T10:00:00Z",
        "end": null,
        "deadline_at": "unix:1786617000",
        "deadline_unix_seconds": 1786617000,
        "next_trigger": null
      },
      "response_counts": {
        "top_level": 0,
        "formal_reviews": 1,
        "inline_comments": 0
      },
      "sources": {
        "top_level": [],
        "formal_reviews": [{
          "id": "review-1",
          "source": "formal_review",
          "state": "APPROVED",
          "response_timestamp": "2026-08-13T10:05:00.000000000Z",
          "commit_id": "current-sha",
          "head_status": "current"
        }],
        "inline_comments": []
      },
      "formal": {
        "decision": "approved",
        "approval_evidence": [{
          "id": "review-1",
          "source": "formal_review",
          "state": "APPROVED",
          "response_timestamp": "2026-08-13T10:05:00.000000000Z",
          "commit_id": "current-sha",
          "head_status": "current"
        }],
        "ambiguous_approval_evidence": [],
        "requested_changes": []
      },
      "boundary_evidence": {
        "request": {
          "repository": "owner/repo",
          "pull_request": 17,
          "head_sha": "current-sha",
          "trigger_id": "https://github.com/owner/repo/pull/17#issuecomment-1001",
          "trigger_prefix": "/sandman review",
          "trigger_created_at": "2026-08-13T10:00:00Z",
          "deadline_at": "unix:1786617000",
          "deadline_unix_seconds": 1786617000
        },
        "sources": {
          "top_level": [],
          "formal_reviews": [{
            "id": "review-1",
            "source": "formal_review",
            "state": "APPROVED",
            "response_timestamp": "2026-08-13T10:05:00.000000000Z",
            "commit_id": "current-sha",
            "head_status": "current"
          }],
          "inline_comments": []
        }
      }
    }
  }
}
`
	if err := os.WriteFile(filepath.Join(stateDir, "17.review_request.json.state"), []byte(state), 0o600); err != nil {
		t.Fatalf("write review wait state: %v", err)
	}
}

func TestExternalGate_LateCurrentHeadApprovalIsReadyToMergeWithoutRetry(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)
	worktreePath := filepath.Join(workDir, "worktree")
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0o755); err != nil {
		t.Fatalf("create worktree task directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte("# Task\n\n## External Gate\n\n- State: pending.\n"), 0o644); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	writeRetainedCurrentHeadApproval(t, worktreePath)

	branch := gateTestBranch
	sb := &retrySandbox{workDir: worktreePath}
	sbFactory := &retrySandboxFactory{sandbox: sb}
	factory := &fakeRunnableFactory{results: []AgentRunResult{{IssueNumber: 42, Status: "success", Branch: branch}}}
	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}},
		prs: map[string]*github.PR{branch: {
			Number:            17,
			State:             "open",
			Body:              "Closes #42",
			HeadRefName:       branch,
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "success",
			ReviewDecision:    "APPROVED",
			MergeStateStatus:  "CLEAN",
		}},
	}
	o := NewOrchestrator(
		client,
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(sbFactory),
		WithRunnableFactory(factory),
		WithRunSessionOpts(gateTestRunOptions()),
	)
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
	if !started || result.Status != "blocked" {
		t.Fatalf("late approval result = (%t, %q), want started blocked", started, result.Status)
	}
	if len(factory.created) != 1 {
		t.Fatalf("agent launches = %d, want 1", len(factory.created))
	}
	if client.editPRBodyCalls != 0 || client.closeIssueCalls != 0 {
		t.Fatalf("GitHub mutations = edit body %d, close issue %d, want none", client.editPRBodyCalls, client.closeIssueCalls)
	}
	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if countEventsByType(logs, "run.continued") != 1 {
		t.Fatalf("run.continued events = %d, want 1", countEventsByType(logs, "run.continued"))
	}
	if countEventsByType(logs, "run.retry") != 0 {
		t.Fatalf("run.retry events = %d, want 0", countEventsByType(logs, "run.retry"))
	}
	finished := findEvent(logs, "run.finished")
	if finished == nil || finished.Payload["gate"] != gateReadyToMerge {
		t.Fatalf("terminal event = %#v, want %q", finished, gateReadyToMerge)
	}
	request, ok := finished.Payload["review_request"].(map[string]any)
	if !ok {
		t.Fatalf("terminal review request = %#v, want object", finished.Payload["review_request"])
	}
	classification, ok := request["classification"].(map[string]any)
	if !ok || classification["protocol"] != "review-classification/v1" {
		t.Fatalf("terminal classification = %#v, want retained classification", request["classification"])
	}
	states := events.ProjectRunStates(logs)
	if len(states) != 1 || states[0].Finished == nil || states[0].Finished.Payload["gate"] != gateReadyToMerge {
		t.Fatalf("projected state = %#v, want ready-to-merge external gate", states)
	}
}

func TestExternalGate_LateStaleApprovalRemainsPending(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(string) string
		wantReason string
	}{
		{
			name: "stale formal approval",
			mutate: func(state string) string {
				return mutateRetainedClassification(state, func(classification map[string]any) {
					moveApprovalToAmbiguous(classification, "stale-sha", "stale")
				})
			},
			wantReason: "pending",
		},
		{
			name: "unknown formal approval",
			mutate: func(state string) string {
				return mutateRetainedClassification(state, func(classification map[string]any) {
					moveApprovalToAmbiguous(classification, "", "unknown")
				})
			},
			wantReason: "pending",
		},
		{
			name: "superseded request",
			mutate: func(state string) string {
				state = strings.Replace(state, `"request_state": "active"`, `"request_state": "superseded"`, 1)
				state = strings.Replace(state, `"decision": "approved"`, `"decision": "pending"`, 1)
				state = strings.Replace(state, `"end": null`, `"end": "2026-08-13T10:10:00Z"`, 1)
				return strings.Replace(state, `"next_trigger": null`, `"next_trigger": {"id":"trigger-2","body":"/sandman review","created_at":"2026-08-13T10:10:00Z"}`, 1)
			},
			wantReason: "pending",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workDir := testenv.MkdirShort(t, "sm-orch-")
			writeRetainedCurrentHeadApproval(t, workDir)
			statePath := filepath.Join(workDir, ".sandman", "state", "17.review_request.json.state")
			state, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatalf("read state: %v", err)
			}
			if err := os.WriteFile(statePath, []byte(tt.mutate(string(state))), 0o600); err != nil {
				t.Fatalf("write state: %v", err)
			}

			session := &runSession{
				issueNumber: 42,
				deps: runDeps{
					githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
						Number: 17, State: "open", HeadRefOid: "current-sha", StatusCheckRollup: "success", ReviewDecision: "APPROVED", MergeStateStatus: "CLEAN",
					}}},
					errorLog: io.Discard,
				},
				opts: gateTestRunOptions(),
			}
			status, extras, handled := session.handleReviewTimeoutGate(context.Background(), workDir, gateTestBranch, "", "run-test", "current-sha")
			if !handled || status != "blocked" {
				t.Fatalf("late stale approval = (%q, %#v, %t), want blocked", status, extras, handled)
			}
			if got := extras["gate"]; got != tt.wantReason {
				t.Fatalf("late stale approval gate = %v, want %q", got, tt.wantReason)
			}
		})
	}
}

func TestExternalGate_LateApprovalPreservesHardGatePrecedence(t *testing.T) {
	tests := []struct {
		name string
		pr   *github.PR
		want string
	}{
		{
			name: "failed CI",
			pr: &github.PR{
				Number: 17, State: "open", HeadRefOid: "current-sha", StatusCheckRollup: "failure", ReviewDecision: "APPROVED", MergeStateStatus: "CLEAN",
			},
			want: "failed",
		},
		{
			name: "conflicting merge",
			pr: &github.PR{
				Number: 17, State: "open", HeadRefOid: "current-sha", StatusCheckRollup: "success", ReviewDecision: "APPROVED", MergeStateStatus: "CONFLICTING",
			},
			want: "failed",
		},
		{
			name: "pending checks",
			pr: &github.PR{
				Number: 17, State: "open", HeadRefOid: "current-sha", StatusCheckRollup: "pending", ReviewDecision: "APPROVED", MergeStateStatus: "BLOCKED",
			},
			want: "pending",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workDir := testenv.MkdirShort(t, "sm-orch-")
			writeRetainedCurrentHeadApproval(t, workDir)
			session := &runSession{
				issueNumber: 42,
				deps:        runDeps{githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: tt.pr}}, errorLog: io.Discard},
				opts:        gateTestRunOptions(),
			}
			status, extras, handled := session.handleReviewTimeoutGate(context.Background(), workDir, gateTestBranch, "", "run-test", "current-sha")
			if !handled || status != "blocked" {
				t.Fatalf("hard-gate result = (%q, %#v, %t), want blocked", status, extras, handled)
			}
			if got := extras["gate"]; got != tt.want {
				t.Fatalf("hard-gate reason = %v, want %q", got, tt.want)
			}
		})
	}
}

func TestExternalGate_LateApprovalRejectsMissingClassification(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeRetainedCurrentHeadApproval(t, workDir)
	statePath := filepath.Join(workDir, ".sandman", "state", "17.review_request.json.state")
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	state = []byte(strings.Replace(string(state), `"classification": {`, `"classification": null, "unused": {`, 1))
	if err := os.WriteFile(statePath, state, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}

	session := &runSession{
		issueNumber: 42,
		deps: runDeps{githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
			Number: 17, State: "open", HeadRefOid: "current-sha", StatusCheckRollup: "success", ReviewDecision: "APPROVED", MergeStateStatus: "CLEAN",
		}}}, errorLog: io.Discard},
		opts: gateTestRunOptions(),
	}
	status, extras, handled := session.handleReviewTimeoutGate(context.Background(), workDir, gateTestBranch, "", "run-test", "current-sha")
	if !handled || status != "blocked" || extras["gate"] != gateReviewTimeout {
		t.Fatalf("missing classification result = (%q, %#v, %t), want retained timeout", status, extras, handled)
	}
}

func mutateRetainedClassification(state string, mutate func(map[string]any)) string {
	var envelope map[string]any
	if err := json.Unmarshal([]byte(state), &envelope); err != nil {
		return state
	}
	evidence, _ := envelope["evidence"].(map[string]any)
	classification, _ := evidence["classification"].(map[string]any)
	mutate(classification)
	updated, err := json.Marshal(envelope)
	if err != nil {
		return state
	}
	return string(updated)
}

func moveApprovalToAmbiguous(classification map[string]any, commit, headStatus string) {
	sources, _ := classification["sources"].(map[string]any)
	formalSources, _ := sources["formal_reviews"].([]any)
	approval, _ := formalSources[0].(map[string]any)
	approval["head_status"] = headStatus
	if commit == "" {
		delete(approval, "commit_id")
	} else {
		approval["commit_id"] = commit
	}
	formal, _ := classification["formal"].(map[string]any)
	formal["approval_evidence"] = []any{}
	formal["ambiguous_approval_evidence"] = []any{approval}
	formal["decision"] = "ambiguous"
	classification["decision"] = "pending"
	boundary, _ := classification["boundary_evidence"].(map[string]any)
	boundary["sources"] = sources
}

func gateTestRunOptions() runSessionOptions {
	return runSessionOptions{
		currentHead:      func(string) (string, error) { return "current-sha", nil },
		gatePollInitial:  time.Millisecond,
		gatePollMaxSleep: time.Millisecond,
		// Leave ample room for race-enabled CI scheduling between scripted polls.
		gatePollBudget: time.Second,
	}
}

func runCleanGateCase(t *testing.T, pr *github.PR) (AgentRunResult, []events.Event, int) {
	return runCleanGateCaseForIssue(t, "open", pr)
}

func runCleanGateCaseForIssue(t *testing.T, issueState string, pr *github.PR) (AgentRunResult, []events.Event, int) {
	t.Helper()
	if pr != nil && pr.HeadRefOid == "" {
		pr.HeadRefOid = "current-sha"
	}
	workDir := testenv.MkdirShort(t, "sm-orch-")
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatalf("change working directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	sb := &retrySandbox{workDir: filepath.Join(workDir, "worktree")}
	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })

	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	factory := &fakeRunnableFactory{results: []AgentRunResult{{
		IssueNumber: 42,
		Status:      "success",
		Branch:      gateTestBranch,
	}}}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: issueState, Title: "Fix bug"}},
		prs:    map[string]*github.PR{gateTestBranch: pr},
	}
	o := NewOrchestrator(
		client,
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&retrySandboxFactory{sandbox: sb}),
		WithRunnableFactory(factory),
		WithRunSessionOpts(gateTestRunOptions()),
	)

	bc := BatchConfig{
		Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	result, started := o.newRunExecutor(context.Background(), bc, &retrySandboxFactory{sandbox: sb}, nil).Execute(context.Background(), RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: gateTestBranch},
		BaseBranch:  "main",
	})
	if !started {
		t.Fatalf("expected run to start, status=%q", result.Status)
	}
	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	return result, logs, len(factory.created)
}

func assertExternalGateTerminal(t *testing.T, logs []events.Event, gate string) {
	t.Helper()
	if got := countEventsByType(logs, "run.retry"); got != 0 {
		t.Fatalf("run.retry events = %d, want 0", got)
	}
	finished := findEvent(logs, "run.finished")
	if finished == nil {
		t.Fatalf("run.finished event not found: %v", logs)
	}
	if got := finished.Payload["status"]; got != "blocked" {
		t.Fatalf("terminal status = %v, want blocked", got)
	}
	if got, _ := finished.Payload["blocker"].(string); got != "external-gate" {
		t.Fatalf("terminal blocker = %q, want external-gate", got)
	}
	if got, _ := finished.Payload["gate"].(string); got != gate {
		t.Fatalf("terminal gate = %q, want %q", got, gate)
	}
	if got := finished.Payload["retries_total"]; got != float64(3) {
		t.Fatalf("terminal retries_total = %v, want configured ceiling 3", got)
	}
	if got := finished.Payload["retries_done"]; got != float64(0) {
		t.Fatalf("terminal retries_done = %v, want 0", got)
	}

	states := events.ProjectRunStates(logs)
	if len(states) != 1 {
		t.Fatalf("projected states = %d, want 1", len(states))
	}
	if got := states[0].Status(); got != "blocked" {
		t.Fatalf("projected status = %q, want blocked", got)
	}
	if states[0].Finished == nil {
		t.Fatal("projected finished event is nil")
	}
	if states[0].Finished.Payload["gate"] != gate {
		t.Fatalf("projected gate = %v, want %q", states[0].Finished.Payload["gate"], gate)
	}
}

func TestRunSingle_PendingCIGateDoesNotConsumeRetries(t *testing.T) {
	result, logs, launches := runCleanGateCase(t, &github.PR{
		Number:            17,
		State:             "open",
		HeadRefName:       gateTestBranch,
		StatusCheckRollup: "pending",
		ReviewDecision:    "APPROVED",
		MergeStateStatus:  "BLOCKED",
	})

	if result.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", result.Status)
	}
	if result.RetriesTotal != 1 {
		t.Fatalf("retries total = %d, want 1", result.RetriesTotal)
	}
	if launches != 1 {
		t.Fatalf("agent launches = %d, want 1", launches)
	}
	assertExternalGateTerminal(t, logs, "pending")
}

func TestRunSingle_PendingDelegatedReviewDoesNotConsumeRetries(t *testing.T) {
	result, logs, launches := runCleanGateCase(t, &github.PR{
		Number:            17,
		State:             "open",
		HeadRefName:       gateTestBranch,
		StatusCheckRollup: "success",
		MergeStateStatus:  "BLOCKED",
	})

	if result.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", result.Status)
	}
	if result.RetriesTotal != 1 {
		t.Fatalf("retries total = %d, want 1", result.RetriesTotal)
	}
	if launches != 1 {
		t.Fatalf("agent launches = %d, want 1", launches)
	}
	assertExternalGateTerminal(t, logs, "pending")
}

func TestRunSingle_ApprovedCleanOpenPRIsReadyToMergeWithoutRetry(t *testing.T) {
	result, logs, launches := runCleanGateCase(t, &github.PR{
		Number:            17,
		State:             "open",
		HeadRefName:       gateTestBranch,
		StatusCheckRollup: "success",
		ReviewDecision:    "APPROVED",
		MergeStateStatus:  "CLEAN",
	})

	if result.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", result.Status)
	}
	if result.RetriesTotal != 1 {
		t.Fatalf("retries total = %d, want 1", result.RetriesTotal)
	}
	if launches != 1 {
		t.Fatalf("agent launches = %d, want 1", launches)
	}
	assertExternalGateTerminal(t, logs, "ready-to-merge")
}

func TestRunSingle_ApprovedCleanOpenPRWithoutChecksIsReadyToMerge(t *testing.T) {
	result, logs, launches := runCleanGateCase(t, &github.PR{
		Number:           17,
		State:            "open",
		HeadRefName:      gateTestBranch,
		ReviewDecision:   "APPROVED",
		MergeStateStatus: "CLEAN",
	})

	if result.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", result.Status)
	}
	if launches != 1 {
		t.Fatalf("agent launches = %d, want 1", launches)
	}
	assertExternalGateTerminal(t, logs, gateReadyToMerge)
}

func TestRunSingle_RestoresHostPathsBeforeExternalGateHeadCheck(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)

	branch := gateTestBranch
	sb := &retrySandbox{workDir: filepath.Join(workDir, "worktree")}
	sbFactory := &retrySandboxFactory{sandbox: sb}
	factory := &fakeRunnableFactory{results: []AgentRunResult{{
		IssueNumber: 42,
		Status:      "success",
		Branch:      branch,
	}}}
	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}},
		prs: map[string]*github.PR{branch: {
			Number:            17,
			State:             "open",
			HeadRefName:       branch,
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "success",
			ReviewDecision:    "APPROVED",
			MergeStateStatus:  "CLEAN",
		}},
	}
	o := NewOrchestrator(
		client,
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(sbFactory),
		WithRunnableFactory(factory),
		WithRunSessionOpts(runSessionOptions{
			currentHead: func(string) (string, error) {
				if !sb.restoreHostPathsCalled {
					t.Error("current-head resolver ran before host paths were restored")
				}
				return "current-sha", nil
			},
			gatePollInitial:  time.Millisecond,
			gatePollMaxSleep: time.Millisecond,
			gatePollBudget:   5 * time.Millisecond,
		}),
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
		t.Fatalf("expected run to start, status=%q", result.Status)
	}
	if result.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", result.Status)
	}
	if !sb.restoreHostPathsCalled {
		t.Fatal("expected RestoreHostPaths before external gate check")
	}
}

type perRunGateSequenceClient struct {
	fakeGitHubClient
	responses []*github.PR
	calls     int
}

func (c *perRunGateSequenceClient) FindPRByBranch(context.Context, string) (*github.PR, error) {
	index := c.calls
	c.calls++
	if index >= len(c.responses) {
		index = len(c.responses) - 1
	}
	return c.responses[index], nil
}

func TestRunSingle_PendingGateTransitionToReadyToMergeIsTerminal(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)

	branch := gateTestBranch
	sb := &retrySandbox{workDir: filepath.Join(workDir, "worktree")}
	sbFactory := &retrySandboxFactory{sandbox: sb}
	factory := &fakeRunnableFactory{results: []AgentRunResult{{IssueNumber: 42, Status: "success", Branch: branch}}}
	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	client := &perRunGateSequenceClient{
		fakeGitHubClient: fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}}},
		responses: []*github.PR{
			{Number: 17, State: "open", HeadRefName: branch, HeadRefOid: "current-sha", StatusCheckRollup: "pending", MergeStateStatus: "BLOCKED"},
			{Number: 17, State: "open", HeadRefName: branch, HeadRefOid: "current-sha", StatusCheckRollup: "success", ReviewDecision: "APPROVED", MergeStateStatus: "CLEAN"},
		},
	}
	o := NewOrchestrator(
		client,
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(sbFactory),
		WithRunnableFactory(factory),
		WithRunSessionOpts(gateTestRunOptions()),
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
		t.Fatalf("expected run to start, status=%q", result.Status)
	}
	if result.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", result.Status)
	}
	if client.calls != 3 {
		t.Fatalf("PR lookups = %d, want pending lookup, ready poll, and terminal conflict check", client.calls)
	}
	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	assertExternalGateTerminal(t, logs, gateReadyToMerge)
	if launches := len(factory.created); launches != 1 {
		t.Fatalf("agent launches = %d, want 1", launches)
	}
}

func TestRunSingle_ClosedIssuePendingPRIsExternalGateBlocked(t *testing.T) {
	result, logs, launches := runCleanGateCaseForIssue(t, "closed", &github.PR{
		Number:            17,
		State:             "open",
		HeadRefName:       gateTestBranch,
		StatusCheckRollup: "pending",
		ReviewDecision:    "APPROVED",
		MergeStateStatus:  "BLOCKED",
	})

	if result.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", result.Status)
	}
	if result.RetriesTotal != 1 {
		t.Fatalf("retries total = %d, want 1", result.RetriesTotal)
	}
	if launches != 1 {
		t.Fatalf("agent launches = %d, want 1", launches)
	}
	assertExternalGateTerminal(t, logs, "pending")
}

func TestRunSingle_ClosedUnmergedPRDoesNotConsumeRetries(t *testing.T) {
	result, logs, launches := runCleanGateCase(t, &github.PR{
		Number:      17,
		State:       "closed",
		HeadRefName: gateTestBranch,
	})

	if result.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", result.Status)
	}
	if result.RetriesTotal != 1 {
		t.Fatalf("retries total = %d, want 1", result.RetriesTotal)
	}
	if launches != 1 {
		t.Fatalf("agent launches = %d, want 1", launches)
	}
	assertExternalGateTerminal(t, logs, "unavailable")
}

func TestRunSingle_FailedExternalGateIsActionableWithoutRetry(t *testing.T) {
	tests := []struct {
		name string
		pr   *github.PR
	}{
		{
			name: "failed CI",
			pr: &github.PR{
				Number:            17,
				State:             "open",
				HeadRefName:       gateTestBranch,
				StatusCheckRollup: "failure",
			},
		},
		{
			name: "rejected review",
			pr: &github.PR{
				Number:            17,
				State:             "open",
				HeadRefName:       gateTestBranch,
				StatusCheckRollup: "success",
				ReviewDecision:    "CHANGES_REQUESTED",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, logs, launches := runCleanGateCase(t, tt.pr)
			if result.Status != "blocked" {
				t.Fatalf("status = %q, want blocked", result.Status)
			}
			if result.RetriesTotal != 1 {
				t.Fatalf("retries total = %d, want 1", result.RetriesTotal)
			}
			if launches != 1 {
				t.Fatalf("agent launches = %d, want 1", launches)
			}
			assertExternalGateTerminal(t, logs, "failed")
		})
	}
}

type gateSequenceClient struct {
	fakeGitHubClient
	branch string
	pr     *github.PR
	calls  int32
}

func (c *gateSequenceClient) FindPRByBranch(ctx context.Context, branch string) (*github.PR, error) {
	call := atomic.AddInt32(&c.calls, 1)
	if branch == c.branch && call >= 2 {
		merged := *c.pr
		merged.State = "merged"
		merged.Merged = true
		merged.Body = "Closes #42"
		return &merged, nil
	}
	return c.pr, nil
}

func TestPollPRGateStopsWhenPRMerges(t *testing.T) {
	client := &gateSequenceClient{
		branch: gateTestBranch,
		pr: &github.PR{
			Number:            17,
			State:             "open",
			HeadRefName:       gateTestBranch,
			StatusCheckRollup: "pending",
		},
	}

	got := pollPRGate(context.Background(), client, gateTestBranch, gateTestRunOptions())
	if got != gateResolved {
		t.Fatalf("poll result = %v, want gateResolved", got)
	}
	if calls := atomic.LoadInt32(&client.calls); calls < 2 {
		t.Fatalf("PR lookups = %d, want at least 2", calls)
	}
}

func TestPollPRGateStopsWhenOpenPRGateIsReadyToMerge(t *testing.T) {
	client := &gateAvailabilityClient{responses: []*github.PR{
		{
			State:             "open",
			StatusCheckRollup: "pending",
			ReviewDecision:    "APPROVED",
			MergeStateStatus:  "BLOCKED",
		},
		{
			State:             "open",
			StatusCheckRollup: "success",
			ReviewDecision:    "APPROVED",
			MergeStateStatus:  "CLEAN",
		},
	}}

	if got := pollPRGate(context.Background(), client, gateTestBranch, gateTestRunOptions()); got != gatePollReadyToMerge {
		t.Fatalf("poll result = %v, want gatePollReadyToMerge", got)
	}
}

func TestPollPRGateContinuesPastStaleReadyState(t *testing.T) {
	client := &gateAvailabilityClient{responses: []*github.PR{
		{
			State:             "open",
			StatusCheckRollup: "pending",
			MergeStateStatus:  "BLOCKED",
		},
		{
			State:             "open",
			StatusCheckRollup: "success",
			ReviewDecision:    "APPROVED",
			MergeStateStatus:  "CLEAN",
			HeadRefOid:        "stale-sha",
		},
		{
			State:             "open",
			StatusCheckRollup: "success",
			ReviewDecision:    "APPROVED",
			MergeStateStatus:  "CLEAN",
			HeadRefOid:        "current-sha",
		},
	}}

	got := pollPRGateAtHead(context.Background(), client, gateTestBranch, "current-sha", gateTestRunOptions())
	if got != gatePollReadyToMerge {
		t.Fatalf("poll result = %v, want gatePollReadyToMerge", got)
	}
	if client.calls != 3 {
		t.Fatalf("PR lookups = %d, want 3 after stale ready state", client.calls)
	}
}

type gateAvailabilityClient struct {
	fakeGitHubClient
	responses []*github.PR
	lookupErr error
	calls     int
}

func (c *gateAvailabilityClient) FindPRByBranch(ctx context.Context, branch string) (*github.PR, error) {
	_ = ctx
	_ = branch
	index := c.calls
	c.calls++
	if c.lookupErr != nil {
		return nil, c.lookupErr
	}
	if index < len(c.responses) {
		return c.responses[index], nil
	}
	return nil, nil
}

type staleHeadGateClient struct {
	fakeGitHubClient
	calls int
}

func (c *staleHeadGateClient) FindPRByBranch(context.Context, string) (*github.PR, error) {
	c.calls++
	return &github.PR{
		State:             "open",
		HeadRefOid:        "stale-sha",
		StatusCheckRollup: "success",
		ReviewDecision:    "APPROVED",
		MergeStateStatus:  "CLEAN",
	}, nil
}

func TestHandleExternalGateKeepsPersistentStaleApprovalPending(t *testing.T) {
	client := &staleHeadGateClient{}
	resolverCalls := 0
	session := &runSession{
		deps: runDeps{githubClient: client, errorLog: io.Discard},
		opts: runSessionOptions{
			currentHead: func(string) (string, error) {
				resolverCalls++
				return "current-sha", nil
			},
			gatePollInitial:  time.Millisecond,
			gatePollMaxSleep: time.Millisecond,
			gatePollBudget:   5 * time.Millisecond,
		},
	}

	status, extras, handled := session.handleExternalGate(context.Background(), t.TempDir(), gateTestBranch, "", "run-test")
	if !handled || status != "blocked" {
		t.Fatalf("stale gate = (%q, %#v, %t), want blocked", status, extras, handled)
	}
	if got := extras["gate"]; got != "pending" {
		t.Fatalf("stale gate reason = %v, want pending", got)
	}
	if resolverCalls != 1 {
		t.Fatalf("current-head resolver calls = %d, want 1 snapshot", resolverCalls)
	}
	if client.calls < 2 {
		t.Fatalf("PR lookups = %d, want initial lookup and polling", client.calls)
	}
}

func TestHandleExternalGateHostPathRestoreFailureRemainsPending(t *testing.T) {
	client := &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
		State:             "open",
		HeadRefOid:        "current-sha",
		StatusCheckRollup: "success",
		ReviewDecision:    "APPROVED",
		MergeStateStatus:  "CLEAN",
	}}}
	session := &runSession{
		deps: runDeps{githubClient: client, errorLog: io.Discard},
		opts: gateTestRunOptions(),
	}

	status, extras, handled := session.handleExternalGateWithHostPaths(context.Background(), t.TempDir(), gateTestBranch, "", "run-test", false)
	if !handled || status != "blocked" {
		t.Fatalf("restore failure gate = (%q, %#v, %t), want blocked", status, extras, handled)
	}
	if got := extras["gate"]; got != "pending" {
		t.Fatalf("restore failure gate reason = %v, want pending", got)
	}
}

func TestHandleExternalGateFailsClosedWhenHeadCannotBeValidated(t *testing.T) {
	for _, tt := range []struct {
		name        string
		currentHead func(string) (string, error)
		prHead      string
	}{
		{
			name: "current head resolver fails",
			currentHead: func(string) (string, error) {
				return "", context.DeadlineExceeded
			},
			prHead: "current-sha",
		},
		{
			name: "pull request head is unavailable",
			currentHead: func(string) (string, error) {
				return "current-sha", nil
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
				State:             "open",
				HeadRefOid:        tt.prHead,
				StatusCheckRollup: "success",
				ReviewDecision:    "APPROVED",
				MergeStateStatus:  "CLEAN",
			}}}
			session := &runSession{
				deps: runDeps{githubClient: client, errorLog: io.Discard},
				opts: runSessionOptions{
					currentHead:      tt.currentHead,
					gatePollInitial:  time.Millisecond,
					gatePollMaxSleep: time.Millisecond,
					gatePollBudget:   5 * time.Millisecond,
				},
			}

			status, extras, handled := session.handleExternalGate(context.Background(), t.TempDir(), gateTestBranch, "", "run-test")
			if !handled || status != "blocked" {
				t.Fatalf("head validation gate = (%q, %#v, %t), want blocked", status, extras, handled)
			}
			if got := extras["gate"]; got != "pending" {
				t.Fatalf("head validation gate reason = %v, want pending", got)
			}
		})
	}
}

func TestPollPRGatePreservesLookupErrors(t *testing.T) {
	client := &gateAvailabilityClient{
		lookupErr: context.DeadlineExceeded,
	}

	got := pollPRGate(context.Background(), client, gateTestBranch, gateTestRunOptions())
	if got != gatePollUnavailable {
		t.Fatalf("poll result = %v, want gatePollUnavailable", got)
	}
}

func TestPollPRGateDetectsDisappearedPR(t *testing.T) {
	client := &gateAvailabilityClient{
		responses: []*github.PR{
			{
				State:             "open",
				StatusCheckRollup: "pending",
			},
			nil,
		},
	}

	got := pollPRGate(context.Background(), client, gateTestBranch, gateTestRunOptions())
	if got != gatePollPRMissing {
		t.Fatalf("poll result = %v, want gatePollPRMissing", got)
	}
}

func TestPollPRGateStopsWhenPendingPRCloses(t *testing.T) {
	client := &gateAvailabilityClient{responses: []*github.PR{
		{State: "open", StatusCheckRollup: "pending"},
		{State: "closed"},
	}}

	if got := pollPRGate(context.Background(), client, gateTestBranch, gateTestRunOptions()); got != gatePollUnavailable {
		t.Fatalf("poll result = %v, want gatePollUnavailable", got)
	}
	if client.calls != 2 {
		t.Fatalf("PR lookups = %d, want 2 without exhausting the poll budget", client.calls)
	}
}

type recoveringGateClient struct {
	fakeGitHubClient
	calls int
}

func (c *recoveringGateClient) FindPRByBranch(context.Context, string) (*github.PR, error) {
	c.calls++
	if c.calls == 1 {
		return nil, context.DeadlineExceeded
	}
	return &github.PR{State: "open", StatusCheckRollup: "pending"}, nil
}

func TestHandleExternalGateInitialLookupErrorRecoversToPending(t *testing.T) {
	client := &recoveringGateClient{}
	session := &runSession{
		deps: runDeps{githubClient: client, errorLog: io.Discard},
		opts: gateTestRunOptions(),
	}

	status, extras, handled := session.handleExternalGate(context.Background(), t.TempDir(), gateTestBranch, "", "run-test")
	if !handled || status != "blocked" {
		t.Fatalf("recovered gate = (%q, %#v, %t), want blocked", status, extras, handled)
	}
	if got := extras["gate"]; got != "pending" {
		t.Fatalf("recovered gate reason = %v, want pending", got)
	}
	if client.calls < 2 {
		t.Fatalf("PR lookups = %d, want recovery polling", client.calls)
	}
}

func TestCheckPRExternalGateRecognizesFullyGreenOpenPRAsReadyToMerge(t *testing.T) {
	for _, tt := range []struct {
		name   string
		rollup string
		review string
	}{
		{name: "approved with checks", rollup: "success", review: "APPROVED"},
		{name: "approved without checks", review: "APPROVED"},
		{name: "no required review", rollup: "success"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
				Number:            17,
				State:             "open",
				StatusCheckRollup: tt.rollup,
				ReviewDecision:    tt.review,
				MergeStateStatus:  "CLEAN",
			}}}

			got, err := checkPRExternalGate(context.Background(), client, gateTestBranch)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != gateReadyToMerge {
				t.Fatalf("fully green PR gate = %q, want %s", got, gateReadyToMerge)
			}
		})
	}
}

func TestCheckPRExternalGateDefersStaleApproval(t *testing.T) {
	client := &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
		Number:            17,
		State:             "open",
		HeadRefName:       gateTestBranch,
		HeadRefOid:        "stale-sha",
		StatusCheckRollup: "success",
		ReviewDecision:    "APPROVED",
		MergeStateStatus:  "CLEAN",
	}}}

	got, err := checkPRExternalGateAtHead(context.Background(), client, gateTestBranch, "current-sha")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "pending" {
		t.Fatalf("stale approved PR gate = %q, want pending", got)
	}
}

func TestCheckPRExternalGateHeadFreshnessPreservesPrecedence(t *testing.T) {
	tests := []struct {
		name string
		pr   *github.PR
		want string
	}{
		{
			name: "failed CI",
			pr: &github.PR{
				State:             "open",
				HeadRefOid:        "stale-sha",
				StatusCheckRollup: "failure",
				MergeStateStatus:  "CLEAN",
			},
			want: "failed",
		},
		{
			name: "requested changes",
			pr: &github.PR{
				State:             "open",
				HeadRefOid:        "stale-sha",
				StatusCheckRollup: "success",
				ReviewDecision:    "CHANGES_REQUESTED",
				MergeStateStatus:  "CLEAN",
			},
			want: "failed",
		},
		{
			name: "conflicting",
			pr: &github.PR{
				State:             "open",
				HeadRefOid:        "stale-sha",
				StatusCheckRollup: "success",
				ReviewDecision:    "APPROVED",
				MergeStateStatus:  "CONFLICTING",
			},
			want: "failed",
		},
		{
			name: "pending review",
			pr: &github.PR{
				State:             "open",
				HeadRefOid:        "stale-sha",
				StatusCheckRollup: "success",
				ReviewDecision:    "REVIEW_REQUIRED",
				MergeStateStatus:  "BLOCKED",
			},
			want: "pending",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: tt.pr}}
			got, err := checkPRExternalGateAtHead(context.Background(), client, gateTestBranch, "current-sha")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("gate = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHandleExternalGateHeadLookupFailureRemainsPending(t *testing.T) {
	client := &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
		State:             "open",
		HeadRefOid:        "stale-sha",
		StatusCheckRollup: "success",
		ReviewDecision:    "APPROVED",
		MergeStateStatus:  "CLEAN",
	}}}
	session := &runSession{
		deps: runDeps{githubClient: client, errorLog: io.Discard},
		opts: runSessionOptions{
			currentHead: func(string) (string, error) {
				return "", context.DeadlineExceeded
			},
		},
	}

	status, extras, handled := session.handleExternalGate(context.Background(), t.TempDir(), gateTestBranch, "", "run-test")
	if !handled || status != "blocked" {
		t.Fatalf("fallback gate = (%q, %#v, %t), want blocked", status, extras, handled)
	}
	if got := extras["gate"]; got != "pending" {
		t.Fatalf("fallback gate reason = %v, want pending", got)
	}
}

func TestCheckPRExternalGateMissingHeadMetadataRemainsPending(t *testing.T) {
	client := &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
		State:             "open",
		StatusCheckRollup: "success",
		ReviewDecision:    "APPROVED",
		MergeStateStatus:  "CLEAN",
	}}}

	got, err := checkPRExternalGateAtHead(context.Background(), client, gateTestBranch, "current-sha")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "pending" {
		t.Fatalf("missing PR head gate = %q, want pending", got)
	}
}

func TestConfirmExternalGateRejectsMergedPRWithoutClosingReference(t *testing.T) {
	branch := gateTestBranch
	session := &runSession{
		issueNumber: 42,
		deps: runDeps{
			githubClient: &fakeGitHubClient{
				prs: map[string]*github.PR{branch: {
					Number:      17,
					State:       "closed",
					Merged:      true,
					HeadRefName: branch,
				}},
			},
			errorLog: io.Discard,
		},
	}

	status, extras, handled := session.confirmExternalGate(context.Background(), t.TempDir(), branch, "", "run-test")
	if !handled {
		t.Fatal("expected merged gate to be handled")
	}
	if status != "failure" {
		t.Fatalf("status = %q, want failure", status)
	}
	if _, ok := extras["completion"]; !ok {
		t.Fatalf("completion extras = %#v, want merged closing-reference diagnostic", extras)
	}
}

func TestHandleExternalGateCancellationDoesNotPersistBlocker(t *testing.T) {
	workDir := t.TempDir()
	taskPath := filepath.Join(workDir, ".sandman", "task.md")
	if err := os.MkdirAll(filepath.Dir(taskPath), 0o755); err != nil {
		t.Fatalf("create task directory: %v", err)
	}
	if err := os.WriteFile(taskPath, []byte("# Existing task\n"), 0o644); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	logPath := filepath.Join(workDir, ".sandman", "run.log")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	session := &runSession{
		deps: runDeps{
			githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
				Number:            17,
				State:             "open",
				StatusCheckRollup: "pending",
			}}},
		},
		opts: gateTestRunOptions(),
	}

	status, extras, handled := session.handleExternalGate(ctx, workDir, gateTestBranch, logPath, "run-test")
	if !handled || status != "aborted" || extras != nil {
		t.Fatalf("canceled gate = (%q, %#v, %t), want (aborted, nil, true)", status, extras, handled)
	}
	if task, err := os.ReadFile(taskPath); err != nil {
		t.Fatalf("read task: %v", err)
	} else if strings.Contains(string(task), "External Gate") {
		t.Fatalf("canceled gate persisted task blocker: %q", task)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("canceled gate run log exists, stat error = %v", err)
	}
}

func TestRecordExternalGateBlockerPersistsTaskAndLog(t *testing.T) {
	workDir := t.TempDir()
	taskPath := filepath.Join(workDir, ".sandman", "task.md")
	if err := os.MkdirAll(filepath.Dir(taskPath), 0o755); err != nil {
		t.Fatalf("create task directory: %v", err)
	}
	if err := os.WriteFile(taskPath, []byte("# Existing task\n"), 0o644); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	logPath := filepath.Join(workDir, ".sandman", "run.log")
	session := &runSession{deps: runDeps{errorLog: io.Discard}}

	session.recordExternalGateBlocker(workDir, logPath, "run-test", "pending")

	task, err := os.ReadFile(taskPath)
	if err != nil {
		t.Fatalf("read task: %v", err)
	}
	if !strings.Contains(string(task), "# Existing task") || !strings.Contains(string(task), "## External Gate") || !strings.Contains(string(task), "Next action:") {
		t.Fatalf("task blocker record = %q, want preserved task and durable next action", task)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read run log: %v", err)
	}
	if !strings.Contains(string(log), "[run-test]") || !strings.Contains(string(log), "external gate pending") || !strings.Contains(string(log), "next action:") {
		t.Fatalf("run log blocker record = %q, want failure and next action", log)
	}

	session.recordExternalGateBlocker(workDir, logPath, "run-test", "failed")
	updatedTask, err := os.ReadFile(taskPath)
	if err != nil {
		t.Fatalf("read updated task: %v", err)
	}
	if strings.Count(string(updatedTask), "## External Gate") != 1 || strings.Contains(string(updatedTask), "gate is pending") || !strings.Contains(string(updatedTask), "gate is failed") {
		t.Fatalf("updated task blocker = %q, want one current failed-gate section", updatedTask)
	}
}

func TestRecordReadyToMergeExternalGatePersistsMergeAction(t *testing.T) {
	workDir := t.TempDir()
	taskPath := filepath.Join(workDir, ".sandman", "task.md")
	if err := os.MkdirAll(filepath.Dir(taskPath), 0o755); err != nil {
		t.Fatalf("create task directory: %v", err)
	}
	if err := os.WriteFile(taskPath, []byte("# Existing task\n"), 0o644); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	logPath := filepath.Join(workDir, ".sandman", "run.log")
	session := &runSession{deps: runDeps{errorLog: io.Discard}}

	session.recordExternalGateBlocker(workDir, logPath, "run-test", gateReadyToMerge)

	task, err := os.ReadFile(taskPath)
	if err != nil {
		t.Fatalf("read task: %v", err)
	}
	taskText := string(task)
	if !strings.Contains(taskText, "State: pull request external gate is ready-to-merge.") {
		t.Fatalf("task blocker = %q, want ready-to-merge state", taskText)
	}
	const nextAction = "Next action: revalidate current-head approval, CI, and mergeability, then execute the normal pull-request merge gate."
	if !strings.Contains(taskText, nextAction) || strings.Contains(taskText, "- Failure:") {
		t.Fatalf("task blocker = %q, want ready-specific next action without generic failure", taskText)
	}

	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read run log: %v", err)
	}
	logText := string(log)
	if !strings.Contains(logText, "external gate ready-to-merge: pull request is ready to merge; next action: revalidate current-head approval, CI, and mergeability, then execute the normal pull-request merge gate") {
		t.Fatalf("run log = %q, want ready-specific next action", logText)
	}
}
