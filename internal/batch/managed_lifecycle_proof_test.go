package batch

import (
	"bytes"
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

// Issue #2631 proof: one managed change driven through request, await,
// approval resume, verified merge, and dependent release on the production
// RunExecutor path with no generic retry consumption.
//
// The fake GitHub client evolves its pull-request facts across passes the way
// the live platform would: pending CI while the first attempt ends, an
// approved clean head after review, then a merged pull request once the
// resumed agent executes the merge gate. Each pass after the first constructs
// a fresh Orchestrator over the same event log, so continuation is restart
// recovery through persisted events rather than in-memory state.
type proofGitHubClient struct {
	*fakeGitHubClient
	factory *fakeRunnableFactory
	// locked, when non-nil, pins every lookup to one pull-request snapshot.
	locked *github.PR
	// entry is served until the resumed agent has launched; merged is served
	// once evolveAt launches exist, mirroring the agent merging the pull
	// request during its resumed session.
	entry    *github.PR
	merged   *github.PR
	evolveAt int
}

func (c *proofGitHubClient) FindPRByBranch(ctx context.Context, branch string) (*github.PR, error) {
	if c.locked != nil {
		return c.locked, nil
	}
	c.factory.mu.Lock()
	launched := len(c.factory.created)
	c.factory.mu.Unlock()
	if launched >= c.evolveAt {
		return c.merged, nil
	}
	return c.entry, nil
}

// runProofPass executes one RowSpec through the production executor with the
// shared proof fixtures. seed writes per-pass worktree state (for example
// retained review classifications) before the session starts.
func runProofPass(t *testing.T, logPath string, client *proofGitHubClient, factory *fakeRunnableFactory, mode IssueMode, prevRunID string, seed func(workDir string)) (AgentRunResult, bool) {
	t.Helper()
	workDir := testenv.MkdirShort(t, "sm-proof-")
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatalf("change working directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	sb := &retrySandbox{workDir: filepath.Join(workDir, "worktree")}
	worktreePath := sb.WorkDir()
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0o755); err != nil {
		t.Fatalf("create worktree task directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if seed != nil {
		seed(worktreePath)
	}
	eventLog := &events.JSONLLogger{Path: logPath}
	runOpts := gateTestRunOptions()
	if mode == ModeContinue {
		// Production-sized in-session resume budget: a consumed resume must
		// not foreclose a later legitimate await in the same session.
		runOpts.awaitResumeMax = 3
	}
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
		Branches:    map[int]string{42: gateTestBranch},
		BaseBranch:  "main",
	}
	if mode == ModeContinue {
		row.Mode = ModeContinue
		row.PreviousRunIDs = map[int]string{42: prevRunID}
	}
	sbFactory := &retrySandboxFactory{sandbox: sb}
	return o.newRunExecutor(context.Background(), bc, sbFactory, nil).Execute(context.Background(), row)
}

func proofRunID(t *testing.T, logs []events.Event) string {
	t.Helper()
	started := findEvent(logs, "run.started")
	if started == nil {
		t.Fatalf("run.started event not found: %v", logs)
	}
	return started.RunID
}

func proofFinishedPayload(t *testing.T, logs []events.Event) map[string]any {
	t.Helper()
	finished := findEvent(logs, "run.finished")
	if finished == nil {
		t.Fatalf("run.finished event not found: %v", logs)
	}
	return finished.Payload
}

func TestManagedLifecycleProof_AwaitApprovalResumeMergeAndDependentRelease(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	factory := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "success", Branch: gateTestBranch},
		{IssueNumber: 42, Status: "success", Branch: gateTestBranch},
		{IssueNumber: 42, Status: "success", Branch: gateTestBranch},
	}}
	client := &proofGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{
				42:  {Number: 42, State: "open", Title: "Fix bug"},
				100: {Number: 100, State: "open", Title: "Dependent feature", BlockedBy: []int{42}},
			},
		},
		factory: factory,
		entry: &github.PR{
			Number:            17,
			State:             "open",
			HeadRefName:       gateTestBranch,
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "success",
			ReviewDecision:    "APPROVED",
			MergeStateStatus:  "CLEAN",
		},
		merged: &github.PR{
			Number:           17,
			State:            "merged",
			Merged:           true,
			HeadRefName:      gateTestBranch,
			HeadRefOid:       "current-sha",
			Body:             "Closes #42",
			MergeStateStatus: "CLEAN",
		},
	}
	// Pass 1 pins running CI so the clean agent exit awaits the managed gate.
	client.locked = &github.PR{
		Number:            17,
		State:             "open",
		HeadRefName:       gateTestBranch,
		HeadRefOid:        "current-sha",
		StatusCheckRollup: "pending",
		MergeStateStatus:  "BLOCKED",
	}
	result, started := runProofPass(t, logPath, client, factory, ModeFresh, "", nil)
	if !started {
		t.Fatal("pass 1: expected run to start")
	}
	if result.Status != "await" {
		t.Fatalf("pass 1: status = %q, want await", result.Status)
	}
	if result.RetriesTotal != 1 {
		t.Fatalf("pass 1: RetriesTotal = %d, want 1 (no generic retry for await)", result.RetriesTotal)
	}
	if len(factory.created) != 1 {
		t.Fatalf("pass 1: agent launches = %d, want 1", len(factory.created))
	}
	eventLog := &events.JSONLLogger{Path: logPath}
	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("pass 1: read events: %v", err)
	}
	runID := proofRunID(t, logs)
	if countEventsByType(logs, "run.retry") != 0 {
		t.Fatalf("pass 1: run.retry events present, want none")
	}
	if findEvent(logs, "run.finished") != nil {
		t.Fatalf("pass 1: run.finished present, want non-terminal await without a terminal event")
	}
	awaitStates := events.ProjectRunStates(logs)
	if len(awaitStates) != 1 {
		t.Fatalf("pass 1: projected run states = %d, want 1", len(awaitStates))
	}
	if awaitStates[0].IsTerminal() {
		t.Fatal("pass 1: projected state is terminal, want non-terminal await")
	}
	if awaitStates[0].RetriesDone() != 0 {
		t.Fatalf("pass 1: projected RetriesDone = %d, want 0", awaitStates[0].RetriesDone())
	}

	// The dependent stays held while its blocker is open and awaiting.
	resolver := NewDependencyResolver(client.fakeGitHubClient)
	resolver.warningWriter = &bytes.Buffer{}
	resolved, err := resolver.Resolve(context.Background(), []int{100}, false, nil)
	if err != nil {
		t.Fatalf("resolve while blocked: %v", err)
	}
	if len(resolved.Blocked[100]) != 1 || resolved.Blocked[100][0] != 42 {
		t.Fatalf("dependent blocked = %v, want [42] while parent awaits", resolved.Blocked[100])
	}

	// Pass 2 restarts the coordinator: a fresh Orchestrator over the same log
	// re-enters the same RunID, resumes on the approved head at entry, takes
	// one in-session approval resume, and observes the merge the resumed
	// agent performs.
	client.locked = nil
	client.evolveAt = 3
	result, started = runProofPass(t, logPath, client, factory, ModeContinue, runID, func(workDir string) {
		writeCurrentHeadApprovalClassification(t, workDir)
	})
	if !started {
		t.Fatal("pass 2: expected run to start")
	}
	if result.Status != "success" {
		t.Fatalf("pass 2: status = %q, want success", result.Status)
	}
	if len(factory.created) != 3 {
		t.Fatalf("pass 2: agent launches = %d, want 3 (entry resume plus one in-session resume)", len(factory.created))
	}
	logs, err = eventLog.Read()
	if err != nil {
		t.Fatalf("pass 2: read events: %v", err)
	}
	if got := proofRunID(t, logs); got != runID {
		t.Fatalf("pass 2: RunID = %q, want preserved %q", got, runID)
	}
	resumed := findEvent(logs, "run.resumed")
	if resumed == nil {
		t.Fatalf("pass 2: run.resumed event not found")
	}
	if reason, _ := resumed.Payload["reason"].(string); reason != "approval" {
		t.Fatalf("pass 2: resume reason = %q, want approval", reason)
	}
	if countEventsByType(logs, "run.retry") != 0 {
		t.Fatalf("pass 2: run.retry events present, want none across resume and merge")
	}
	if finishedStatus(t, logs) != "success" {
		t.Fatalf("pass 2: finished status = %q, want success", finishedStatus(t, logs))
	}
	if got, _ := proofFinishedPayload(t, logs)["retries_done"].(float64); got != 0 {
		t.Fatalf("pass 2: retries_done = %v, want 0", got)
	}
	states := events.ProjectRunStates(logs)
	if len(states) != 1 {
		t.Fatalf("pass 2: projected run states = %d, want 1", len(states))
	}
	if !states[0].IsTerminal() {
		t.Fatal("pass 2: projected state is not terminal after verified merge")
	}
	if states[0].RetriesDone() != 0 {
		t.Fatalf("pass 2: projected RetriesDone = %d, want 0", states[0].RetriesDone())
	}

	// Pass 3 re-enters after another restart against the merged head: the
	// terminal outcome is re-read, not recomputed, with no further launch.
	client.locked = client.merged
	result, started = runProofPass(t, logPath, client, factory, ModeContinue, runID, nil)
	if !started {
		t.Fatal("pass 3: expected run to start")
	}
	if result.Status != "success" {
		t.Fatalf("pass 3: status = %q, want success", result.Status)
	}
	if len(factory.created) != 3 {
		t.Fatalf("pass 3: agent launches = %d, want still 3 (no relaunch on terminal re-entry)", len(factory.created))
	}

	// Verified closure releases the dependent: closing the work item drops
	// the blocker and admits the dependent.
	client.fakeGitHubClient.issues[42].State = "closed"
	resolved, err = resolver.Resolve(context.Background(), []int{100}, false, nil)
	if err != nil {
		t.Fatalf("resolve after closure: %v", err)
	}
	if len(resolved.Blocked[100]) != 0 {
		t.Fatalf("dependent blocked = %v, want released after verified closure", resolved.Blocked[100])
	}
}

func TestManagedLifecycleProof_FeedbackResumeMergesWithoutRetry(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	factory := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "success", Branch: gateTestBranch},
		{IssueNumber: 42, Status: "success", Branch: gateTestBranch},
		{IssueNumber: 42, Status: "success", Branch: gateTestBranch},
		{IssueNumber: 42, Status: "success", Branch: gateTestBranch},
		{IssueNumber: 42, Status: "success", Branch: gateTestBranch},
	}}
	pendingPR := &github.PR{
		Number:            17,
		State:             "open",
		HeadRefName:       gateTestBranch,
		HeadRefOid:        "current-sha",
		StatusCheckRollup: "pending",
		MergeStateStatus:  "BLOCKED",
	}
	mergedPR := &github.PR{
		Number:           17,
		State:            "merged",
		Merged:           true,
		HeadRefName:      gateTestBranch,
		HeadRefOid:       "current-sha",
		Body:             "Closes #42",
		MergeStateStatus: "CLEAN",
	}
	client := &proofGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{
				42: {Number: 42, State: "open", Title: "Fix bug"},
			},
		},
		factory: factory,
		entry: &github.PR{
			Number:            17,
			State:             "open",
			HeadRefName:       gateTestBranch,
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "success",
			ReviewDecision:    "REVIEW_REQUIRED",
			MergeStateStatus:  "BLOCKED",
		},
		merged: mergedPR,
	}
	client.locked = pendingPR
	result, started := runProofPass(t, logPath, client, factory, ModeFresh, "", nil)
	if !started {
		t.Fatal("pass 1: expected run to start")
	}
	if result.Status != "await" {
		t.Fatalf("pass 1: status = %q, want await", result.Status)
	}
	eventLog := &events.JSONLLogger{Path: logPath}
	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("pass 1: read events: %v", err)
	}
	runID := proofRunID(t, logs)

	// Pass 2 restarts over the same log with actionable current-head
	// feedback: entry resumes into the fix, and the merged head observed
	// afterwards terminalizes the same RunID as success without a generic
	// retry. (A fix that left head, request and responses unchanged would
	// stall into a wait instead of another relaunch.)
	client.locked = nil
	client.evolveAt = 2
	result, started = runProofPass(t, logPath, client, factory, ModeContinue, runID, func(workDir string) {
		writeInformalRespondedClassification(t, workDir, "Please fix the race in internal/socketpath/socketpath.go.")
	})
	if !started {
		t.Fatal("pass 2: expected run to start")
	}
	if result.Status != "success" {
		dbg, _ := eventLog.Read()
		for _, e := range dbg {
			t.Logf("event %s run=%s payload=%v", e.Type, e.RunID, e.Payload)
		}
		t.Logf("launches=%d", len(factory.created))
		t.Fatalf("pass 2: status = %q, want success", result.Status)
	}
	if len(factory.created) != 2 {
		t.Fatalf("pass 2: agent launches = %d, want 2 (pass 1 plus the entry feedback resume)", len(factory.created))
	}
	logs, err = eventLog.Read()
	if err != nil {
		t.Fatalf("pass 2: read events: %v", err)
	}
	if got := proofRunID(t, logs); got != runID {
		t.Fatalf("pass 2: RunID = %q, want preserved %q", got, runID)
	}
	if countEventsByType(logs, "run.resumed") != 0 {
		t.Fatalf("pass 2: the entry launch is the feedback resume; no in-session relaunch was needed")
	}
	if countEventsByType(logs, "run.retry") != 0 {
		t.Fatalf("pass 2: run.retry events present, want none across feedback resume")
	}
	if finishedStatus(t, logs) != "success" {
		t.Fatalf("pass 2: finished status = %q, want success", finishedStatus(t, logs))
	}
	if got, _ := proofFinishedPayload(t, logs)["retries_done"].(float64); got != 0 {
		t.Fatalf("pass 2: retries_done = %v, want 0", got)
	}
	states := events.ProjectRunStates(logs)
	if len(states) != 1 {
		t.Fatalf("pass 2: projected run states = %d, want 1", len(states))
	}
	if !states[0].IsTerminal() {
		t.Fatal("pass 2: projected state is not terminal after verified merge")
	}
	if states[0].RetriesDone() != 0 {
		t.Fatalf("pass 2: projected RetriesDone = %d, want 0", states[0].RetriesDone())
	}

	// Pass 3 re-enters after another restart against the merged head: the
	// terminal outcome is re-read, not recomputed, with no further launch.
	client.locked = mergedPR
	result, started = runProofPass(t, logPath, client, factory, ModeContinue, runID, nil)
	if !started {
		t.Fatal("pass 3: expected run to start")
	}
	if result.Status != "success" {
		t.Fatalf("pass 3: status = %q, want success", result.Status)
	}
	if len(factory.created) != 2 {
		t.Fatalf("pass 3: agent launches = %d, want still 2 (no relaunch on terminal re-entry)", len(factory.created))
	}
}
