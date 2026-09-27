package batch

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// runMissingPRCase runs one issue-driven session with no pull request behind
// the branch through the production RunExecutor path. The fake supplies one
// successful agent result per attempt so bounded retries can exhaust without
// tripping the fake factory.
func runMissingPRCase(t *testing.T, mode IssueMode, prevRunID string) (AgentRunResult, []events.Event, int) {
	t.Helper()
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
	factory := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "success", Branch: gateTestBranch},
		{IssueNumber: 42, Status: "success", Branch: gateTestBranch},
		{IssueNumber: 42, Status: "success", Branch: gateTestBranch},
		{IssueNumber: 42, Status: "success", Branch: gateTestBranch},
	}}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}},
		prs:    map[string]*github.PR{},
	}
	runOpts := gateTestRunOptions()
	runOpts.awaitResumeMax = 1
	o := NewOrchestrator(
		client,
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&retrySandboxFactory{sandbox: sb}),
		WithRunnableFactory(factory),
		WithRunSessionOpts(runOpts),
	)

	bc := BatchConfig{
		Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	row := RowSpec{
		IssueNumber: 42,
		Mode:        mode,
		Branches:    map[int]string{42: gateTestBranch},
		BaseBranch:  "main",
	}
	if mode == ModeContinue {
		row.PreviousRunIDs = map[int]string{42: prevRunID}
	}
	sbFactory := &retrySandboxFactory{sandbox: sb}
	result, started := o.newRunExecutor(context.Background(), bc, sbFactory, nil).Execute(context.Background(), row)
	if !started {
		t.Fatalf("expected run to start, status=%q", result.Status)
	}
	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	return result, logs, len(factory.created)
}

// Tracer for issue #2743: an open work item with a successful agent exit and
// no pull request must not enter external waiting. Missing PR creation is
// implementor-owned work, not a publication wait.
func TestRunExecutor_MissingPRDoesNotAwait(t *testing.T) {
	result, logs, _ := runMissingPRCase(t, ModeFresh, "")
	if got := countEventsByType(logs, "run.await"); got != 0 {
		t.Fatalf("run.await events = %d, want 0 (no PR means no external resolver)", got)
	}
	if result.Status == "await" {
		t.Fatalf("result status = %q, want non-await terminal outcome", result.Status)
	}
	if result.Status == "success" {
		t.Fatalf("result status = %q, want failure (unfinished PR work is not success)", result.Status)
	}
	states := events.ProjectRunStates(logs)
	if len(states) != 1 {
		t.Fatalf("projected states = %d, want 1", len(states))
	}
	if states[0].IsAwaiting() {
		t.Fatal("projected run is awaiting without external work")
	}
	if got := finishedStatus(t, logs); got != "failure" {
		t.Fatalf("finished status = %q, want failure", got)
	}
}

// Continuation path of the same tracer: a --continue re-entry with no PR must
// also refuse waiting and terminalize instead of parking the run.
func TestRunExecutor_ContinuationMissingPRDoesNotAwait(t *testing.T) {
	result, logs, _ := runMissingPRCase(t, ModeContinue, "previous-run")
	if got := countEventsByType(logs, "run.await"); got != 0 {
		t.Fatalf("run.await events = %d, want 0 (no PR means no external resolver)", got)
	}
	if result.Status == "await" {
		t.Fatalf("result status = %q, want non-await terminal outcome", result.Status)
	}
	if result.Status == "success" {
		t.Fatalf("result status = %q, want failure (unfinished PR work is not success)", result.Status)
	}
	states := events.ProjectRunStates(logs)
	if len(states) != 1 {
		t.Fatalf("projected states = %d, want 1", len(states))
	}
	if states[0].IsAwaiting() {
		t.Fatal("projected run is awaiting without external work")
	}
	if got := finishedStatus(t, logs); got != "failure" {
		t.Fatalf("finished status = %q, want failure", got)
	}
}
