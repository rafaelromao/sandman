package batch

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// The ordinary managed observation cadence is 30 seconds from the first poll.
func TestAwaitPollInterval_DefaultIsThirtySecondsFromFirstPoll(t *testing.T) {
	for poll := 0; poll < 8; poll++ {
		if got := awaitPollInterval(runSessionOptions{}, poll); got != 30*time.Second {
			t.Fatalf("default poll %d interval = %v, want 30s", poll, got)
		}
	}
}

func TestAwaitPollInterval_ExplicitOverrideStillWins(t *testing.T) {
	opts := runSessionOptions{lifecyclePollPlan: []time.Duration{7 * time.Second, 3 * time.Second}}
	for poll, want := range []time.Duration{7 * time.Second, 3 * time.Second, 3 * time.Second} {
		if got := awaitPollInterval(opts, poll); got != want {
			t.Fatalf("override poll %d interval = %v, want %v", poll, got, want)
		}
	}
}

// A driver started without its own interval falls back to the same default,
// and a cancelled wait aborts the row without another observation.
func TestRowDriverDefaultPollIntervalIsTheManagedObservationInterval(t *testing.T) {
	run := RunSpec{RunID: "run-42", IssueNumber: 42, BatchID: "batch-a", Branch: "42-fix", BaseBranch: "main"}
	var waits []time.Duration
	gates := &rowDriverWaitRecorder{rowDriverGateFake: &rowDriverGateFake{}, waits: &waits}
	executor := &rowDriverExecutorFake{observations: []lifecycleVerdict{
		{Action: lifecycleAwait, Evidence: map[string]any{"gate": "ci"}},
		{Action: lifecycleAwait, Evidence: map[string]any{"gate": "ci"}},
		{Action: lifecycleFailure},
	}}
	got := newRowDriver(executor, gates, &rowDriverClockFake{}).Drive(context.Background(), run, ContinuationCursor{}, rowDriverOptions{
		Awaiting: true, ReleaseAwaitCapacity: true,
	})
	if got.Result.Status != "failure" || len(waits) != 2 || waits[0] != 30*time.Second || waits[1] != 30*time.Second {
		t.Fatalf("Drive = %+v waits = %v, want two 30s waits (one per re-observation)", got.Result, waits)
	}
	if executor.observeCalls != 3 || executor.executeCalls != 0 || gates.acquires != 0 {
		t.Fatalf("observations=%d executions=%d admissions=%d, want one observation per wait and no execution", executor.observeCalls, executor.executeCalls, gates.acquires)
	}
}

func TestAwaitWaitDefaultTimerIsCancellationAware(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := waitForAwaitPoll(ctx, managedObservationInterval); err == nil {
		t.Fatal("cancelled default wait returned nil")
	}
	if elapsed := time.Since(start); elapsed >= managedObservationInterval/2 {
		t.Fatalf("cancelled default wait blocked for %v", elapsed)
	}
}

// A row that yields for current-head CI and sees it finish right after its
// first observation waits exactly once, for the production default interval.
func TestBatchObservation_DefaultCadenceWaitsThirtySecondsOnceBeforeResolution(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-cadence-")
	t.Chdir(root)
	initGitRepo(t, root)
	client := &reviewWaitSchedulerGitHubClient{fakeGitHubClient: fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
		prs:    map[string]*github.PR{"42-parent": {Number: 42, State: "open", HeadRefName: "42-parent", HeadRefOid: "current-sha", Body: "Closes #42", StatusCheckRollup: "pending"}},
	}}
	log := &spyEventLog{}
	var mu sync.Mutex
	var waits []time.Duration
	factory := &controlledRunnableFactory{runnables: map[int]Runnable{
		42: waitingRunnableFunction(func(context.Context) AgentRunResult {
			return AgentRunResult{IssueNumber: 42, Status: "success", Branch: "42-parent"}
		}),
	}}
	cfg := &config.Config{Agent: "opencode", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", AgentProviders: map[string]config.Agent{"opencode": config.BuiltInAgentPresets["opencode"].Agent("opencode")}}
	// No lifecyclePollPlan override: the production default cadence applies.
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: cfg}, log, WithErrorLog(io.Discard), WithRunnableFactory(factory), WithSandboxFactory(reviewWaitSchedulerSandboxFactory{}), WithRunSessionOpts(runSessionOptions{
		releaseAwaitCapacity: true,
		currentHead:          func(string) (string, error) { return "current-sha", nil },
		awaitWait: func(ctx context.Context, interval time.Duration) error {
			mu.Lock()
			waits = append(waits, interval)
			mu.Unlock()
			// CI finishes and the pull request merges while the row waits.
			client.mu.Lock()
			client.issues[42] = &github.Issue{Number: 42, State: "closed"}
			client.prs["42-parent"] = mergedPR("42-parent", "Closes #42")
			client.mu.Unlock()
			return ctx.Err()
		},
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := o.RunBatch(ctx, Request{Issues: []int{42}, Parallel: 1, RunTS: "261006090000", RunShortID: "cadence", Branches: map[int]string{42: "42-parent"}})
	if err != nil || result == nil || len(result.Runs) != 1 || result.Runs[0].Status != "success" {
		t.Fatalf("batch did not resolve after the first poll: result=%+v err=%v", result, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(waits) != 1 || waits[0] != 30*time.Second {
		t.Fatalf("external waits = %v, want exactly one 30s wait before the first re-observation", waits)
	}
}
