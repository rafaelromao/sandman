package batch

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
)

// Explicit abort of a started Run that the batch owns while it waits is a
// terminal decision like any other. It is applied once by the shared terminal
// application: one run.aborted with the Run's branch and worktree evidence,
// the Run's artifact manifest closed, and the worktree preserved.
func TestWaitingContract_ExplicitAbortOfAWaitingRunAppliesTheSharedTerminal(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	initGitRepo(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := &cancelOnAwaitLog{cancel: cancel}
	cfg := &config.Config{Agent: "test", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", AgentProviders: map[string]config.Agent{"test": {Command: "true"}}}
	client := &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open"}}, prs: map[string]*github.PR{"42-parent": {Number: 17, State: "open", HeadRefName: "42-parent", HeadRefOid: "head", StatusCheckRollup: "pending"}}}
	factory := &controlledRunnableFactory{runnables: map[int]Runnable{42: &controlledRunnable{result: AgentRunResult{IssueNumber: 42, Status: "success", Branch: "42-parent"}}}}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: cfg}, log,
		WithErrorLog(io.Discard), WithSandboxFactory(reviewWaitSchedulerSandboxFactory{}), WithRunnableFactory(factory), WithRunSessionOpts(runSessionOptions{currentHead: func(string) (string, error) { return "head", nil }}))

	result, _ := o.RunBatch(ctx, Request{Issues: []int{42}, RunTS: "261004120000", RunShortID: "late", Branches: map[int]string{42: "42-parent"}})

	if result == nil || len(result.Runs) != 1 || result.Runs[0].Status != "aborted" {
		t.Fatalf("result = %+v, want the waiting Run aborted", result)
	}
	var aborted []events.Event
	var started events.Event
	for _, e := range log.snapshot() {
		switch e.Type {
		case "run.aborted":
			aborted = append(aborted, e)
		case "run.started":
			started = e
		}
	}
	if len(aborted) != 1 {
		t.Fatalf("run.aborted events = %d, want exactly one terminal", len(aborted))
	}
	payload := aborted[0].Payload
	if aborted[0].RunID != started.RunID || payload["branch"] != "42-parent" || payload["worktree_state"] != "preserved" {
		t.Fatalf("run.aborted %s payload = %v, want Run %s with its branch and preserved worktree evidence", aborted[0].RunID, payload, started.RunID)
	}
	layout := paths.NewLayout(cfg, root)
	manifests, _ := filepath.Glob(filepath.Join(layout.BatchesDir, "*", "runs", started.RunID, "run.json"))
	if len(manifests) != 1 {
		t.Fatalf("run manifests for %s = %v, want one", started.RunID, manifests)
	}
	manifest, err := batchindex.ReadManifest(filepath.Dir(manifests[0]))
	if err != nil || manifest.Status != batchindex.RunManifestStatusAborted {
		t.Fatalf("run.json status = %q (err %v), want aborted", manifest.Status, err)
	}
	if _, err := os.Stat(filepath.Join(layout.WorktreeDir, "42-parent")); err != nil {
		t.Fatalf("explicit abort removed the worktree: %v", err)
	}
	states := events.ProjectRunStates(log.snapshot())
	if len(states) != 1 || states[0].Status() != "aborted" || !states[0].IsTerminal() || len(FindReadyContinuations(log.snapshot(), layout)) != 0 || countEventsByType(log.snapshot(), "run.retry") != 0 {
		t.Fatalf("aborted waiting Run is not a closed terminal: %+v", states)
	}
}

// A Run aborted before it starts has no worktree or artifact manifest to
// close: its abort stays the bare admission record.
func TestWaitingContract_ExplicitAbortOfAnUnstartedAdmissionRecordsOnlyTheAbort(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	initGitRepo(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := &spyEventLog{}
	cfg := &config.Config{Agent: "test", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", AgentProviders: map[string]config.Agent{"test": {Command: "true"}}}
	client := &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open"}, 43: {Number: 43, State: "open"}}}
	busyStarted := make(chan struct{})
	factory := &controlledRunnableFactory{runnables: map[int]Runnable{
		42: &controlledRunnable{started: busyStarted, release: ctx.Done(), result: AgentRunResult{IssueNumber: 42, Status: "failure", Branch: "42-busy"}},
		43: &controlledRunnable{result: AgentRunResult{IssueNumber: 43, Status: "success", Branch: "43-queued"}},
	}}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: cfg}, log,
		WithErrorLog(io.Discard), WithSandboxFactory(reviewWaitSchedulerSandboxFactory{}), WithRunnableFactory(factory))
	go func() {
		<-busyStarted
		cancel()
	}()

	result, _ := o.RunBatch(ctx, Request{Issues: []int{42, 43}, Parallel: 1, RunTS: "261004120000", RunShortID: "adm", Branches: map[int]string{42: "42-busy", 43: "43-queued"}})

	if result == nil || len(result.Runs) != 2 || result.Runs[1].Status != "aborted" {
		t.Fatalf("result = %+v, want the queued admission aborted", result)
	}
	queuedAborts := 0
	queuedRunID := ""
	for _, e := range log.snapshot() {
		if e.Issue != 43 {
			continue
		}
		if e.Type == "run.started" {
			t.Fatalf("queued admission started: %+v", e)
		}
		if e.Type == "run.aborted" {
			queuedAborts++
			queuedRunID = e.RunID
			if _, ok := e.Payload["worktree_state"]; ok {
				t.Fatalf("unstarted abort payload = %v, want no worktree evidence", e.Payload)
			}
		}
	}
	if queuedAborts != 1 {
		t.Fatalf("run.aborted events for the queued admission = %d, want exactly one", queuedAborts)
	}
	for _, state := range events.ProjectRunStates(log.snapshot()) {
		if state.RunID == queuedRunID && (state.Status() != "aborted" || !state.IsTerminal()) {
			t.Fatalf("queued admission projection = %+v, want terminal aborted", state)
		}
	}
}

// A phase can request the abort of a started Run after failing on lease,
// checkpoint or accounting I/O while the batch is still live. The selected
// abort decision, not the context, makes its terminal a run.aborted.
func TestApplyAbort_StartedRunWithoutCancellationRecordsTheSelectedAbort(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	initGitRepo(t, root)
	const runID = "261004120000-live-42"
	log := &spyEventLog{events: []events.Event{{Type: "run.started", RunID: runID, Issue: 42, IssueRef: issueRef(42), Payload: map[string]any{"branch": "42-parent", "batch_id": "261004120000-live-42"}}}}
	o := NewOrchestrator(&fakeGitHubClient{}, &noopRenderer{}, nil, log, WithErrorLog(io.Discard), WithSandboxFactory(reviewWaitSchedulerSandboxFactory{}))
	bc := BatchConfig{Cfg: &config.Config{WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}}, IdentityResolver: noopIdentityResolver()}
	row := RowSpec{IssueNumber: 42, RunID: runID, BatchID: "261004120000-live-42", Branches: map[int]string{42: "42-parent"}, BaseBranch: "main"}
	executor := o.newRunExecutor(context.Background(), bc, reviewWaitSchedulerSandboxFactory{}, nil)

	result := o.applyAbort(context.Background(), executor, row, AgentRunResult{}, nil)

	if result.Status != "aborted" {
		t.Fatalf("status = %q, want aborted", result.Status)
	}
	var terminals []events.Event
	for _, e := range log.snapshot() {
		if e.Type == "run.aborted" || e.Type == "run.finished" {
			terminals = append(terminals, e)
		}
	}
	if len(terminals) != 1 || terminals[0].Type != "run.aborted" || terminals[0].Payload["branch"] != "42-parent" || terminals[0].Payload["worktree_state"] != "preserved" {
		t.Fatalf("terminal events = %+v, want one run.aborted with the Run's branch and preserved worktree evidence", terminals)
	}
}
