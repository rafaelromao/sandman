package batch

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// assertSharedAbortTerminal checks that the started Run of issue 42 ended with
// exactly one run.aborted carrying its branch and worktree evidence, a closed
// artifact manifest and its worktree kept.
func assertSharedAbortTerminal(t *testing.T, log []events.Event, layout paths.Layout) {
	t.Helper()
	var aborted []events.Event
	var started events.Event
	for _, e := range log {
		if e.Issue != 42 {
			continue
		}
		switch e.Type {
		case "run.aborted":
			aborted = append(aborted, e)
		case "run.started":
			started = e
		}
	}
	if len(aborted) != 1 {
		t.Fatalf("run.aborted events for the returned Run = %d, want exactly one terminal", len(aborted))
	}
	if payload := aborted[0].Payload; aborted[0].RunID != started.RunID || payload["branch"] != "42-parent" || payload["worktree_state"] != "preserved" {
		t.Fatalf("run.aborted %s payload = %v, want Run %s with its branch and preserved worktree evidence", aborted[0].RunID, payload, started.RunID)
	}
	manifests, _ := filepath.Glob(filepath.Join(layout.BatchesDir, "*", "runs", started.RunID, "run.json"))
	if len(manifests) != 1 {
		t.Fatalf("run manifests for %s = %v, want one", started.RunID, manifests)
	}
	manifest, err := batchindex.ReadManifest(filepath.Dir(manifests[0]))
	if err != nil || manifest.Status != batchindex.RunManifestStatusAborted {
		t.Fatalf("run.json status = %q (err %v), want aborted", manifest.Status, err)
	}
	if _, err := os.Stat(filepath.Join(layout.WorktreeDir, "42-parent")); err != nil {
		t.Fatalf("abort removed the worktree: %v", err)
	}
	for _, state := range events.ProjectRunStates(log) {
		if state.RunID == started.RunID && (state.Status() != "aborted" || !state.IsTerminal()) {
			t.Fatalf("returned Run projection = %+v, want terminal aborted", state)
		}
	}
}

func returnedAwaitClient(extra map[int]*github.Issue) *fakeGitHubClient {
	issues := map[int]*github.Issue{42: {Number: 42, State: "open"}}
	for n, issue := range extra {
		issues[n] = issue
	}
	return &fakeGitHubClient{issues: issues, prs: map[string]*github.PR{"42-parent": {Number: 17, State: "open", HeadRefName: "42-parent", HeadRefOid: "head", StatusCheckRollup: "pending"}}}
}

// A started Run returned to the batch while it waits keeps its wait owner.
// When that ownership turns invalid, the abort is the same terminal decision
// as any other: it is applied through the shared terminal application.
func TestWaitingContract_LeaseFailureOfAReturnedWaitingRunAppliesTheSharedTerminal(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-returned-lease-")
	t.Chdir(root)
	initGitRepo(t, root)
	log := &spyEventLog{}
	cfg := &config.Config{Agent: "test", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", AgentProviders: map[string]config.Agent{"test": {Command: "true"}}}
	siblingStarted, releaseSibling := make(chan struct{}), make(chan struct{})
	factory := &controlledRunnableFactory{runnables: map[int]Runnable{
		42: &controlledRunnable{result: AgentRunResult{IssueNumber: 42, Status: "success", Branch: "42-parent"}},
		43: &controlledRunnable{started: siblingStarted, release: releaseSibling, result: AgentRunResult{IssueNumber: 43, Status: "success", Branch: "43-sibling"}},
	}}
	request := Request{Issues: []int{42, 43}, Parallel: 1, RunTS: "261006120000", RunShortID: "lease", Branches: map[int]string{42: "42-parent", 43: "43-sibling"}}
	runID := buildRunID(42, request.RunTS, request.RunShortID)
	pulse := make(chan time.Time, 1)
	o := NewOrchestrator(returnedAwaitClient(map[int]*github.Issue{43: {Number: 43, State: "open"}}), &noopRenderer{}, &fakeConfigStore{config: cfg}, log,
		WithErrorLog(io.Discard), WithSandboxFactory(reviewWaitSchedulerSandboxFactory{}), WithRunnableFactory(factory),
		WithRunSessionOpts(runSessionOptions{currentHead: func(string) (string, error) { return "head", nil }, waitOwnerPulse: func(id string) <-chan time.Time {
			if id == runID {
				return pulse
			}
			return nil
		}}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	var result *Result
	go func() { defer close(done); result, _ = o.RunBatch(ctx, request) }()
	select {
	case <-siblingStarted:
	case <-ctx.Done():
		t.Fatal("sibling did not start after the waiting Run returned")
	}
	layout := paths.NewLayout(cfg, root)
	// A started Run's wait record lives in its Run folder.
	records, _ := filepath.Glob(filepath.Join(layout.BatchesDir, "*", "runs", runID, "wait.json"))
	if len(records) != 1 {
		t.Fatalf("wait records for %s = %v, want one", runID, records)
	}
	if err := os.WriteFile(records[0], []byte("invalid ownership checkpoint"), 0o600); err != nil {
		t.Fatal(err)
	}
	pulse <- time.Now()
	for countEventsByType(log.snapshot(), "run.aborted") == 0 {
		select {
		case <-ctx.Done():
			t.Fatal("invalid ownership did not abort the returned Run")
		case <-time.After(5 * time.Millisecond):
		}
	}
	close(releaseSibling)
	<-done

	if result == nil || len(result.Runs) != 2 || result.Runs[0].Status != "aborted" || result.Runs[1].Status == "aborted" {
		t.Fatalf("result = %+v, want only the returned Run aborted", result)
	}
	assertSharedAbortTerminal(t, log.snapshot(), layout)
}

// A batch cancelled while a sibling still runs aborts its returned waiting
// Run at the final cancellation fence, through the shared terminal.
func TestWaitingContract_CancellationFenceAbortOfAReturnedWaitingRunAppliesTheSharedTerminal(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-returned-fence-")
	t.Chdir(root)
	initGitRepo(t, root)
	log := &spyEventLog{}
	cfg := &config.Config{Agent: "test", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", AgentProviders: map[string]config.Agent{"test": {Command: "true"}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	siblingStarted := make(chan struct{})
	factory := &controlledRunnableFactory{runnables: map[int]Runnable{
		42: &controlledRunnable{result: AgentRunResult{IssueNumber: 42, Status: "success", Branch: "42-parent"}},
		43: &controlledRunnable{started: siblingStarted, release: ctx.Done(), result: AgentRunResult{IssueNumber: 43, Status: "aborted", Branch: "43-sibling"}},
	}}
	o := NewOrchestrator(returnedAwaitClient(map[int]*github.Issue{43: {Number: 43, State: "open"}}), &noopRenderer{}, &fakeConfigStore{config: cfg}, log,
		WithErrorLog(io.Discard), WithSandboxFactory(reviewWaitSchedulerSandboxFactory{}), WithRunnableFactory(factory),
		WithRunSessionOpts(runSessionOptions{currentHead: func(string) (string, error) { return "head", nil }}))
	go func() {
		<-siblingStarted
		cancel()
	}()

	result, _ := o.RunBatch(ctx, Request{Issues: []int{42, 43}, Parallel: 1, RunTS: "261006120000", RunShortID: "fence", Branches: map[int]string{42: "42-parent", 43: "43-sibling"}})

	if result == nil || len(result.Runs) != 2 || result.Runs[0].Status != "aborted" {
		t.Fatalf("result = %+v, want the returned Run aborted", result)
	}
	assertSharedAbortTerminal(t, log.snapshot(), paths.NewLayout(cfg, root))
}

// An unstarted admission returned queued behind an unfinished prerequisite
// has nothing to close: the cancellation fence records only its abort.
func TestWaitingContract_CancellationFenceAbortOfAReturnedQueuedAdmissionRecordsOnlyTheAbort(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-returned-queued-")
	t.Chdir(root)
	initGitRepo(t, root)
	log := &spyEventLog{}
	cfg := &config.Config{Agent: "test", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", AgentProviders: map[string]config.Agent{"test": {Command: "true"}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	siblingStarted := make(chan struct{})
	factory := &controlledRunnableFactory{runnables: map[int]Runnable{
		42: &controlledRunnable{result: AgentRunResult{IssueNumber: 42, Status: "success", Branch: "42-parent"}},
		43: &controlledRunnable{result: AgentRunResult{IssueNumber: 43, Status: "success", Branch: "43-queued"}},
		44: &controlledRunnable{started: siblingStarted, release: ctx.Done(), result: AgentRunResult{IssueNumber: 44, Status: "aborted", Branch: "44-sibling"}},
	}}
	client := returnedAwaitClient(map[int]*github.Issue{43: {Number: 43, State: "open", Body: "Blocked by #42"}, 44: {Number: 44, State: "open"}})
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: cfg}, log,
		WithErrorLog(io.Discard), WithSandboxFactory(reviewWaitSchedulerSandboxFactory{}), WithRunnableFactory(factory),
		WithRunSessionOpts(runSessionOptions{currentHead: func(string) (string, error) { return "head", nil }}))
	go func() {
		<-siblingStarted
		cancel()
	}()

	result, _ := o.RunBatch(ctx, Request{Issues: []int{42, 43, 44}, Parallel: 1, RunTS: "261006120000", RunShortID: "queued", Branches: map[int]string{42: "42-parent", 43: "43-queued", 44: "44-sibling"}, Dependencies: map[int][]int{43: {42}}})

	if result == nil || len(result.Runs) != 3 || result.Runs[1].Status != "aborted" {
		t.Fatalf("result = %+v, want the queued admission aborted", result)
	}
	aborts := 0
	for _, e := range log.snapshot() {
		if e.Issue != 43 {
			continue
		}
		if e.Type == "run.started" {
			t.Fatalf("queued admission started: %+v", e)
		}
		if e.Type == "run.aborted" {
			aborts++
			if _, ok := e.Payload["worktree_state"]; ok {
				t.Fatalf("unstarted abort payload = %v, want no worktree evidence", e.Payload)
			}
		}
	}
	if aborts != 1 {
		t.Fatalf("run.aborted events for the queued admission = %d, want exactly one", aborts)
	}
	assertSharedAbortTerminal(t, log.snapshot(), paths.NewLayout(cfg, root))
}
