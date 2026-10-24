package batch

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
)

// recoveredWaitingRun records a started Run of issue 42 waiting on CI in batch
// "old", with its wait record and preserved worktree, and returns the request
// that recovers it.
func recoveredWaitingRun(t *testing.T, log *spyEventLog, layout paths.Layout) Request {
	t.Helper()
	now := time.Now().UTC()
	log.events = append(log.events,
		events.Event{Type: "run.started", RunID: "row", Issue: 42, Timestamp: now.Add(-time.Minute), Payload: map[string]any{"batch_id": "old", "branch": "42-fix"}},
		events.Event{Type: "run.await", RunID: "row", Issue: 42, Timestamp: now, Payload: map[string]any{"ci_wait": map[string]any{"deadline_unix_seconds": now.Add(30 * time.Minute).Unix()}}},
	)
	if err := daemon.RenewRunWait(layout, layout.LocateBatch("old", ""), daemon.RunWait{Protocol: "run-wait/v1", RunID: "row", BatchID: "old", Issue: 42, Branch: "42-fix", BaseBranch: "main", OperationID: "ci:old", OperationDeadline: now.Add(30 * time.Minute)}, now); err != nil {
		t.Fatal(err)
	}
	task := filepath.Join(layout.WorktreeDir, "42-fix", ".sandman", "task.md")
	if err := os.MkdirAll(filepath.Dir(task), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(task, []byte("# Task\nPreserved work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := Request{RunTS: "261004120000", RunShortID: "pre"}
	if err := ApplyReadyContinuations(&request, FindReadyContinuations(log.snapshot(), layout), layout, 1800); err != nil {
		t.Fatal(err)
	}
	return request
}

// assertRecoveredRunAbortTerminal checks that the recovered started Run ended
// with exactly one run.aborted carrying its branch and preserved worktree
// evidence, and projects as a terminal abort.
func assertRecoveredRunAbortTerminal(t *testing.T, log []events.Event, layout paths.Layout) events.Event {
	t.Helper()
	var aborted []events.Event
	for _, e := range log {
		if e.RunID == "row" && e.Type == "run.aborted" {
			aborted = append(aborted, e)
		}
	}
	if len(aborted) != 1 {
		t.Fatalf("run.aborted events for the recovered Run = %d, want exactly one terminal", len(aborted))
	}
	if payload := aborted[0].Payload; payload["branch"] != "42-fix" || payload["worktree_state"] != "preserved" {
		t.Fatalf("run.aborted payload = %v, want the Run's branch and preserved worktree evidence", payload)
	}
	if _, err := os.Stat(filepath.Join(layout.WorktreeDir, "42-fix")); err != nil {
		t.Fatalf("abort removed the worktree: %v", err)
	}
	for _, state := range events.ProjectRunStates(log) {
		if state.RunID == "row" && (state.Status() != "aborted" || !state.IsTerminal()) {
			t.Fatalf("recovered Run projection = %+v, want terminal aborted", state)
		}
	}
	return aborted[0]
}

// A started Run whose recovered ownership cannot be published is aborted
// before it has an execution slot. The abort is still the shared terminal.
func TestWaitingContract_RecoveredOwnershipFailureOfAStartedRunAppliesTheSharedTerminal(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	initGitRepo(t, root)
	cfg := &config.Config{Agent: "test", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", AgentProviders: map[string]config.Agent{"test": {Command: "true"}}}
	layout := paths.NewLayout(cfg, root)
	log := &failedRecoveredAwaitLog{}
	request := recoveredWaitingRun(t, &log.spyEventLog, layout)
	factory := &controlledRunnableFactory{runnables: map[int]Runnable{}}
	o := NewOrchestrator(&fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open"}}}, &noopRenderer{}, &fakeConfigStore{config: cfg}, log,
		WithErrorLog(io.Discard), WithSandboxFactory(reviewWaitSchedulerSandboxFactory{}), WithRunnableFactory(factory))

	result, _ := o.RunBatch(context.Background(), request)

	if result == nil || len(result.Runs) != 1 || result.Runs[0].Status != "aborted" || len(factory.created) != 0 {
		t.Fatalf("result = %+v launches = %v, want the recovered Run aborted without a launch", result, factory.created)
	}
	assertRecoveredRunAbortTerminal(t, log.snapshot(), layout)
}

// A started Run recovered behind an in-batch prerequisite that the batch
// abort stops is aborted by that cascade. Its terminal keeps the attribution.
func TestWaitingContract_DependencyCascadeAbortOfAStartedRunAppliesTheSharedTerminal(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	initGitRepo(t, root)
	cfg := &config.Config{Agent: "test", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", AgentProviders: map[string]config.Agent{"test": {Command: "true"}}}
	layout := paths.NewLayout(cfg, root)
	log := &spyEventLog{}
	request := recoveredWaitingRun(t, log, layout)
	request.Issues = append(request.Issues, 43)
	request.Branches[43] = "43-prerequisite"
	request.Dependencies = map[int][]int{42: {43}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prerequisiteStarted := make(chan struct{})
	factory := &controlledRunnableFactory{runnables: map[int]Runnable{
		43: &controlledRunnable{started: prerequisiteStarted, release: ctx.Done(), result: AgentRunResult{IssueNumber: 43, Status: "aborted", Branch: "43-prerequisite"}},
	}}
	client := &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open"}, 43: {Number: 43, State: "open"}}}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: cfg}, log,
		WithErrorLog(io.Discard), WithSandboxFactory(reviewWaitSchedulerSandboxFactory{}), WithRunnableFactory(factory))
	go func() {
		<-prerequisiteStarted
		cancel()
	}()

	result, _ := o.RunBatch(ctx, request)

	if result == nil || len(factory.created) != 1 {
		t.Fatalf("result = %+v launches = %v, want only the prerequisite launched", result, factory.created)
	}
	recovered := false
	for _, run := range result.Runs {
		if run.IssueNumber == 42 {
			recovered = run.Status == "aborted"
		}
	}
	if !recovered {
		t.Fatalf("result = %+v, want the recovered Run aborted", result.Runs)
	}
	aborted := assertRecoveredRunAbortTerminal(t, log.snapshot(), layout)
	if abortedBy, ok := aborted.Payload["aborted_by"].([]int); !ok || len(abortedBy) != 1 || abortedBy[0] != 43 {
		t.Fatalf("run.aborted aborted_by = %#v, want [43]", aborted.Payload["aborted_by"])
	}
}
