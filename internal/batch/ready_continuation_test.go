package batch

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

func TestFindReadyContinuationsUsesLatestDurableCapacityPhase(t *testing.T) {
	t.Parallel()
	layout := paths.NewLayout(nil, t.TempDir())
	started := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	awaited := started.Add(time.Minute)
	queued := awaited.Add(time.Minute)

	eventsBefore := readyContinuationEvents("run-ready", "batch-ready", started, awaited, queued)
	ready := FindReadyContinuations(eventsBefore, layout)
	if len(ready) != 1 {
		t.Fatalf("ready continuations = %#v, want one", ready)
	}
	got := ready[0]
	if got.IssueNumber != 42 || got.RunID != "run-ready" || got.PreviousRunID != "run-ready" || got.PreviousRunBatchID != "batch-ready" || got.BatchID != "batch-ready" {
		t.Fatalf("ready continuation identity = %#v", got)
	}
	if got.Branch != "42-fix" || got.BaseBranch != "main" || got.IssueTitle != "Fix bug" {
		t.Fatalf("ready continuation worktree identity = %#v", got)
	}

	// A later external await means the operation is still pending; a later
	// continuation means the queued evidence has already been consumed.
	stillWaiting := append(append([]events.Event(nil), eventsBefore...), events.Event{
		Type: "run.await", Timestamp: queued.Add(time.Minute), RunID: "run-ready", Issue: 42,
		Payload: map[string]any{"await_reason": "pending"},
	})
	if got := FindReadyContinuations(stillWaiting, layout); len(got) != 0 {
		t.Fatalf("ready after newer await = %#v, want none", got)
	}
	continued := append(append([]events.Event(nil), eventsBefore...), events.Event{
		Type: "run.continued", Timestamp: queued.Add(time.Minute), RunID: "run-new", Issue: 42,
		Payload: map[string]any{"branch": "42-fix", "batch_id": "batch-new"},
	})
	if got := FindReadyContinuations(continued, layout); len(got) != 0 {
		t.Fatalf("ready after newer run = %#v, want none", got)
	}
}

func TestApplyReadyContinuationsRestoresPromptAndSchedulerIdentity(t *testing.T) {
	workDir := t.TempDir()
	layout := paths.NewLayout(nil, workDir)
	branch := "42-fix"
	taskPath := filepath.Join(layout.WorktreeDir, branch, ".sandman", "task.md")
	if err := os.MkdirAll(filepath.Dir(taskPath), 0o755); err != nil {
		t.Fatalf("create Task directory: %v", err)
	}
	const task = "# Task\n\nFinish the implementation.\n"
	if err := os.WriteFile(taskPath, []byte(task), 0o600); err != nil {
		t.Fatalf("write Task: %v", err)
	}
	request := Request{Issues: []int{43}}
	ready := []ReadyContinuation{{
		IssueNumber: 42, RunID: "original-run", PreviousRunID: "original-run",
		PreviousRunBatchID: "old-batch", BatchID: "old-batch", Branch: branch,
		BaseBranch: "main", IssueTitle: "Fix bug",
	}}
	if err := ApplyReadyContinuations(&request, ready, layout, 900); err != nil {
		t.Fatalf("ApplyReadyContinuations: %v", err)
	}
	if len(request.Issues) != 2 || request.Issues[1] != 42 {
		t.Fatalf("issues = %v, want existing issue plus ready continuation", request.Issues)
	}
	if request.IssueMode(42) != ModeContinue || request.RunIDs[42] != "original-run" || !request.ReadyContinuations[42] {
		t.Fatalf("restored scheduler identity = mode:%v run:%q ready:%v", request.IssueMode(42), request.RunIDs[42], request.ReadyContinuations[42])
	}
	if request.PreviousRunIDs[42] != "original-run" || request.PreviousRunBatchIDs[42] != "old-batch" || !request.ReuseSession[42] {
		t.Fatalf("restored previous identity = runs:%v batches:%v reuse:%v", request.PreviousRunIDs, request.PreviousRunBatchIDs, request.ReuseSession)
	}
	if request.Branches[42] != branch || request.BaseBranches[42] != "main" || request.IssueTitles[42] != "Fix bug" {
		t.Fatalf("restored worktree identity = branches:%v bases:%v titles:%v", request.Branches, request.BaseBranches, request.IssueTitles)
	}
	if !strings.Contains(request.TaskPrompts[42], task) || !strings.Contains(request.TaskPrompts[42], "Delegated review response timeout: `900` seconds") {
		t.Fatalf("restored prompt omitted Task or current review timeout:\n%s", request.TaskPrompts[42])
	}
}

func TestApplyReadyContinuationsDoesNotOverrideExplicitOverride(t *testing.T) {
	request := Request{Issues: []int{42}, Mode: map[int]IssueMode{42: ModeOverride}}
	ready := []ReadyContinuation{{IssueNumber: 42, RunID: "old", PreviousRunID: "old", PreviousRunBatchID: "batch", Branch: "42-fix", BaseBranch: "main"}}
	if err := ApplyReadyContinuations(&request, ready, paths.NewLayout(nil, t.TempDir()), 900); err != nil {
		t.Fatalf("ApplyReadyContinuations: %v", err)
	}
	if request.IssueMode(42) != ModeOverride || request.ReadyContinuations[42] {
		t.Fatalf("override mode was replaced: mode=%v ready=%v", request.IssueMode(42), request.ReadyContinuations[42])
	}
}

func TestRunBatchRehydratesReadyContinuationAfterRestart(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	initGitRepo(t, workDir)
	layout := paths.NewLayout(&config.Config{WorktreeDir: ".sandman/worktrees"}, workDir)

	const (
		issueReady  = 42
		issueBusy   = 43
		readyID     = "260929120000-abcd-42"
		oldBatchID  = "260929120000-abcd-42"
		readyBranch = "42-ready"
		busyBranch  = "43-busy"
	)
	started := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	awaited := started.Add(time.Minute)
	queued := awaited.Add(time.Minute)
	log := &spyEventLog{events: []events.Event{
		{Type: "run.started", Timestamp: started, RunID: readyID, Issue: issueReady, Payload: map[string]any{
			"branch": readyBranch, "base_branch": "main", "batch_id": oldBatchID,
		}},
		{Type: "run.await", Timestamp: awaited, RunID: readyID, Issue: issueReady, Payload: map[string]any{"gate": "pending", "await": true}},
		{Type: "run.capacity_queued", Timestamp: queued, RunID: readyID, Issue: issueReady, Payload: map[string]any{
			"ready_continuation": true, "branch": readyBranch, "base_branch": "main", "batch_id": oldBatchID,
			"previous_run_id": readyID, "previous_run_batch_id": oldBatchID, "issue_title": "Ready issue",
		}},
	}}
	ready := FindReadyContinuations(log.snapshot(), layout)
	if len(ready) != 1 {
		t.Fatalf("discovered ready continuations = %#v, want one", ready)
	}

	readyTask := filepath.Join(layout.WorktreeDir, readyBranch, ".sandman", "task.md")
	if err := os.MkdirAll(filepath.Dir(readyTask), 0o755); err != nil {
		t.Fatalf("create ready worktree: %v", err)
	}
	if err := os.WriteFile(readyTask, []byte("# Task\n\nMerge the approved PR.\n"), 0o600); err != nil {
		t.Fatalf("write ready Task: %v", err)
	}

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			issueReady: {Number: issueReady, State: "open", Title: "Ready issue"},
			issueBusy:  {Number: issueBusy, State: "open", Title: "Busy issue"},
		},
		prs: map[string]*github.PR{
			readyBranch: {Number: 42, State: "merged", Merged: true, Body: "Closes #42", HeadRefName: readyBranch},
			busyBranch:  {Number: 43, State: "merged", Merged: true, Body: "Closes #43", HeadRefName: busyBranch},
		},
	}
	factory := &readyRecoveryRunnableFactory{busyStarted: make(chan struct{}), allowBusyFinish: make(chan struct{})}
	request := Request{
		Issues:     []int{issueBusy},
		Branches:   map[int]string{issueBusy: busyBranch},
		RunTS:      "260929123000",
		RunShortID: "cafe",
		Parallel:   1,
		PromptConfig: prompt.RenderConfig{
			ReviewCommand: "/sandman review",
			ReviewTimeout: 1800,
		},
	}
	if err := ApplyReadyContinuations(&request, ready, layout, 1800); err != nil {
		t.Fatalf("rehydrate ready continuation: %v", err)
	}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{
		Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees",
		Git:            config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}},
	}}, log,
		WithErrorLog(io.Discard),
		WithSandboxFactory(reviewWaitSchedulerSandboxFactory{}),
		WithRunnableFactory(factory),
		WithRunSessionOpts(runSessionOptions{
			baseBranchSync: func(string, string) error { return nil },
			currentHead:    func(string) (string, error) { return "current-sha", nil },
		}),
	)

	done := make(chan struct{})
	var result *Result
	var runErr error
	go func() {
		defer close(done)
		result, runErr = o.RunBatch(context.Background(), request)
	}()
	select {
	case <-factory.busyStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("ordinary row did not acquire capacity")
	}
	if starts := factory.startsSnapshot(); len(starts) != 1 || starts[0] != issueBusy {
		t.Fatalf("ready continuation started before the occupied slot freed: %v", starts)
	}
	close(factory.allowBusyFinish)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("rehydrated continuation did not start after capacity became available")
	}
	if runErr != nil {
		t.Fatalf("RunBatch: %v; events=%v", runErr, log.snapshot())
	}
	if result == nil || len(result.Runs) != 2 {
		t.Fatalf("result = %#v, want two runs", result)
	}
	if starts := factory.startsSnapshot(); len(starts) != 1 || starts[0] != issueBusy {
		t.Fatalf("runnable starts = %v, want only the busy row; merged ready row should revalidate without relaunch", starts)
	}
	var continued bool
	for _, event := range log.snapshot() {
		if event.Type == "run.continued" && event.RunID == readyID {
			continued = true
		}
	}
	if !continued {
		t.Fatalf("rehydrated RunID %q did not continue; events=%v", readyID, log.snapshot())
	}
	states := events.ProjectRunStates(log.snapshot())
	for _, state := range states {
		if state.RunID == readyID && (state.IsAwaiting() || state.IsCapacityQueued() || state.IsActive()) {
			t.Fatalf("ready continuation did not reach verified terminal success: %#v", state)
		}
	}
}

func readyContinuationEvents(runID, batchID string, started, awaited, queued time.Time) []events.Event {
	return []events.Event{
		{Type: "run.started", Timestamp: started, RunID: runID, Issue: 42, Payload: map[string]any{"branch": "42-fix", "base_branch": "main", "batch_id": batchID}},
		{Type: "run.await", Timestamp: awaited, RunID: runID, Issue: 42, Payload: map[string]any{"gate": "pending", "await": true}},
		{Type: "run.capacity_queued", Timestamp: queued, RunID: runID, Issue: 42, Payload: map[string]any{
			"ready_continuation": true, "branch": "42-fix", "base_branch": "main", "batch_id": batchID,
			"previous_run_id": runID, "previous_run_batch_id": batchID, "issue_title": "Fix bug",
		}},
	}
}

type readyRecoveryRunnableFactory struct {
	mu              sync.Mutex
	starts          []int
	busyStarted     chan struct{}
	allowBusyFinish chan struct{}
}

func (f *readyRecoveryRunnableFactory) NewRunnable(issue *github.Issue, _ string, _ sandbox.Sandbox) Runnable {
	return &readyRecoveryRunnable{factory: f, issue: issue.Number}
}

type readyRecoveryRunnable struct {
	factory *readyRecoveryRunnableFactory
	issue   int
}

func (r *readyRecoveryRunnable) Run(ctx context.Context, _ prompt.IssueRenderer, _ string, _ prompt.RenderConfig) AgentRunResult {
	f := r.factory
	f.mu.Lock()
	f.starts = append(f.starts, r.issue)
	f.mu.Unlock()
	if r.issue == 43 {
		select {
		case <-f.busyStarted:
		default:
			close(f.busyStarted)
		}
		select {
		case <-f.allowBusyFinish:
		case <-ctx.Done():
			return AgentRunResult{IssueNumber: r.issue, Status: "aborted"}
		}
	}
	return AgentRunResult{IssueNumber: r.issue, Status: "success"}
}

func (f *readyRecoveryRunnableFactory) startsSnapshot() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.starts...)
}
