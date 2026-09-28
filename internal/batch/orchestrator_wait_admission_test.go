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

// runIdleGateCase runs one issue-driven session through the production
// RunExecutor path with the given pull request behind the branch (nil for no
// pull request). The fake supplies one successful agent result per attempt so
// bounded retries can exhaust without tripping the fake factory.
func runIdleGateCase(t *testing.T, mode IssueMode, prevRunID string, pr *github.PR) (AgentRunResult, []events.Event, int) {
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
		prs:    map[string]*github.PR{gateTestBranch: pr},
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
	result, logs, _ := runIdleGateCase(t, ModeFresh, "", nil)
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
	finished := findEvent(logs, "run.finished")
	if finished.Payload["reason"] != missingPRReason || finished.Payload["next_action"] != missingPRNextAction {
		t.Fatalf("missing-PR failure evidence = %#v, want structured publication next action", finished.Payload)
	}
	if _, ok := finished.Payload["blocker"]; ok {
		t.Fatalf("missing PR was mislabeled dependency blocker: %#v", finished.Payload)
	}
}

// An open pull request with no actively resolving operation (no running CI,
// no confirmed request, no live approval) is idle work, not a wait: the run
// must fail instead of parking in waiting (issue #2743).
func TestRunExecutor_IdleGateWithoutRequestFailsInsteadOfWaiting(t *testing.T) {
	idlePR := &github.PR{
		Number:            17,
		State:             "open",
		HeadRefName:       gateTestBranch,
		HeadRefOid:        "current-sha",
		StatusCheckRollup: "success",
		MergeStateStatus:  "BLOCKED",
	}
	result, logs, _ := runIdleGateCase(t, ModeFresh, "", idlePR)
	if got := countEventsByType(logs, "run.await"); got != 0 {
		t.Fatalf("run.await events = %d, want 0 (idle gate has no active resolver)", got)
	}
	if result.Status != "failure" {
		t.Fatalf("result status = %q, want failure", result.Status)
	}
	finished := findEvent(logs, "run.finished")
	if finished == nil || finished.Payload["reason"] != idleGateReason {
		t.Fatalf("finished reason = %v, want %q", finished, idleGateReason)
	}
	if finished.Payload["external_gate"] != string(lifecycleGatePending) || finished.Payload["expected_head_sha"] != "current-sha" {
		t.Fatalf("terminal failure omitted idle-gate evidence: %#v", finished.Payload)
	}
}

// Continuation path of the same tracer: a --continue re-entry with no PR must
// also refuse waiting and terminalize instead of parking the run.
func TestRunExecutor_ContinuationMissingPRDoesNotAwait(t *testing.T) {
	result, logs, _ := runIdleGateCase(t, ModeContinue, "previous-run", nil)
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
	finished := findEvent(logs, "run.finished")
	if finished.Payload["reason"] != missingPRReason || finished.Payload["next_action"] != missingPRNextAction {
		t.Fatalf("continuation missing-PR evidence = %#v, want structured publication next action", finished.Payload)
	}
}
