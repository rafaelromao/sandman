package batch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// hookedEventLog runs hook after each appended event, on the logging
// goroutine, so a test can sequence external observations against durable
// lifecycle evidence.
type hookedEventLog struct {
	spyEventLog
	hook func(events.Event)
	// fail, when set, can reject an append before it is recorded.
	fail func(events.Event) error
}

func (l *hookedEventLog) Log(event events.Event) error {
	if l.fail != nil {
		if err := l.fail(event); err != nil {
			return err
		}
	}
	err := l.spyEventLog.Log(event)
	if err == nil && l.hook != nil {
		l.hook(event)
	}
	return err
}

type acceptanceClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *acceptanceClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *acceptanceClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

const (
	acceptanceRunID  = "260929120000-abcd-42"
	acceptanceBranch = "42-fix"
)

// quotaPauseFixture runs a ready continuation (issue 42) beside a sibling
// (issue 43) whose launch reaches its provider usage limit. The sibling's
// quota poll blocks until the test ends, so every observation 42 makes in
// between happens inside the batch quota pause.
type quotaPauseFixture struct {
	t       *testing.T
	layout  paths.Layout
	log     *hookedEventLog
	client  *reviewWaitSchedulerGitHubClient
	clock   *acceptanceClock
	limited chan struct{}
	cancel  context.CancelFunc
	done    chan struct{}
}

// startQuotaPauseFixture starts the batch. onPaused runs once, when 42 records
// its usage-limit pause; onPausedAwait runs once, on the first run.await 42
// persists after that.
func startQuotaPauseFixture(t *testing.T, onPaused, onPausedAwait func(*quotaPauseFixture), fail ...func(events.Event) error) *quotaPauseFixture {
	t.Helper()
	root := testenv.MkdirShort(t, "sm-accept-")
	t.Chdir(root)
	initGitRepo(t, root)
	cfg := &config.Config{
		Agent: "opencode", DefaultAgent: "opencode", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees",
		Git:            config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{"opencode": config.BuiltInAgentPresets["opencode"].Agent("opencode")},
	}
	layout := paths.NewLayout(cfg, root)
	clock := &acceptanceClock{now: time.Now().UTC().Truncate(time.Second)}
	start := clock.Now()
	f := &quotaPauseFixture{t: t, layout: layout, clock: clock, limited: make(chan struct{}), done: make(chan struct{})}
	f.log = &hookedEventLog{}
	if len(fail) > 0 {
		f.log.fail = fail[0]
	}
	f.log.events = readyContinuationEvents(acceptanceRunID, acceptanceRunID, start.Add(-2*time.Minute), start.Add(-time.Minute), start)
	ready := FindReadyContinuations(f.log.snapshot(), layout)
	if len(ready) != 1 {
		t.Fatalf("ready continuations = %#v, want one", ready)
	}
	worktree := filepath.Join(layout.WorktreeDir, acceptanceBranch)
	if err := os.MkdirAll(filepath.Join(worktree, ".sandman", "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".sandman", "task.md"), []byte("# Task\nRepair CI.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.client = &reviewWaitSchedulerGitHubClient{fakeGitHubClient: fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}, 43: {Number: 43, State: "open", Title: "Busy"}},
		prs: map[string]*github.PR{acceptanceBranch: {
			Number: 17, State: "open", HeadRefName: acceptanceBranch, HeadRefOid: "head", Body: "Closes #42", StatusCheckRollup: "pending", CIExecutionID: "ci-a",
		}},
	}}
	var once sync.Once
	var pausedOnce, awaitOnce bool
	var hookMu sync.Mutex
	f.log.hook = func(event events.Event) {
		if event.RunID != acceptanceRunID {
			return
		}
		hookMu.Lock()
		defer hookMu.Unlock()
		switch {
		case !pausedOnce && event.Type == "run.capacity_queued" && event.Payload["reason"] == "usage-limit-paused":
			pausedOnce = true
			if onPaused != nil {
				onPaused(f)
			}
		case pausedOnce && !awaitOnce && event.Type == "run.await":
			awaitOnce = true
			if onPausedAwait != nil {
				onPausedAwait(f)
			}
		}
	}
	factory := &promptOnlyRunnableFactory{hook: func(issue *github.Issue, branch string) AgentRunResult {
		if issue.Number == 43 {
			return AgentRunResult{IssueNumber: 43, Status: "failure", Branch: branch, UsageLimitReached: true}
		}
		return AgentRunResult{IssueNumber: issue.Number, Status: "success", Branch: branch}
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	f.cancel = cancel
	opts := runSessionOptions{
		releaseAwaitCapacity: true,
		now:                  clock.Now,
		currentHead:          func(string) (string, error) { return "head", nil },
		lifecyclePollPlan:    []time.Duration{5 * time.Millisecond},
		baseBranchSync:       func(string, string) error { return nil },
		awaitWait: func(waitCtx context.Context, d time.Duration) error {
			if d == usageLimitPollInterval {
				// The sibling holds the quota pause until the test ends.
				once.Do(func() { close(f.limited) })
				<-waitCtx.Done()
				return waitCtx.Err()
			}
			select {
			case <-f.limited:
			case <-waitCtx.Done():
				return waitCtx.Err()
			}
			// 42 waited outside the pause: make it ready so it enters the pause.
			f.client.setPR(acceptanceBranch, func(pr *github.PR) { pr.StatusCheckRollup = "failure" })
			return nil
		},
	}
	request := Request{
		Issues: []int{43}, Branches: map[int]string{43: "43-busy"}, Agent: "opencode",
		RunTS: "260929123000", RunShortID: "cafe", Parallel: 1, Retries: 1,
	}
	if err := ApplyReadyContinuations(&request, ready, layout, 1800); err != nil {
		t.Fatal(err)
	}
	o := NewOrchestrator(f.client, &noopRenderer{}, &fakeConfigStore{config: cfg}, f.log,
		WithErrorLog(io.Discard),
		WithSandboxFactory(reviewWaitSchedulerSandboxFactory{}),
		WithRunnableFactory(factory),
		WithRunSessionOpts(opts),
	)
	go func() {
		defer close(f.done)
		_, _ = o.RunBatch(ctx, request)
	}()
	t.Cleanup(f.stop)
	return f
}

func (f *quotaPauseFixture) stop() {
	f.cancel()
	select {
	case <-f.done:
	case <-time.After(10 * time.Second):
		f.t.Error("batch did not stop after cancellation")
	}
}

// eventually polls cond until it holds or the deadline passes.
func (f *quotaPauseFixture) eventually(what string, cond func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			f.t.Fatalf("%s before quota reopened; events=%+v", what, f.log.snapshot())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (f *quotaPauseFixture) stillPaused() {
	f.t.Helper()
	select {
	case <-f.limited:
	default:
		f.t.Fatal("sibling never reached its usage limit")
	}
	for _, state := range events.ProjectRunStates(f.log.snapshot()) {
		if state.RunID == acceptanceRunID && state.IsTerminal() {
			f.t.Fatalf("ready continuation finished during the quota pause: %+v", state)
		}
	}
}

// readRowWait reads the run's wait sidecar from the batch that owns it.
func readRowWait(t *testing.T, layout paths.Layout, log *spyEventLog, runID string) (daemon.RunWait, error) {
	t.Helper()
	batchID := ""
	for _, event := range log.snapshot() {
		if event.RunID == runID {
			if id, ok := event.Payload["batch_id"].(string); ok && id != "" {
				batchID = id
			}
		}
	}
	location, err := daemon.ResolveBatchLocation(layout, batchID)
	if err != nil {
		return daemon.RunWait{}, err
	}
	return daemon.ReadRunWaitAt(layout, location, runID)
}

func ciExecutionAwait(event events.Event, execution string) bool {
	if event.Type != "run.await" || event.RunID != acceptanceRunID {
		return false
	}
	ciWait, ok := event.Payload["ci_wait"].(map[string]any)
	return ok && ciWait["execution_id"] == execution
}

func TestObservationAcceptance_ChangedOperationDuringQuotaPauseIsDurable(t *testing.T) {
	var bAt atomic.Int64
	f := startQuotaPauseFixture(t,
		func(f *quotaPauseFixture) {
			f.client.setPR(acceptanceBranch, func(pr *github.PR) { pr.StatusCheckRollup, pr.CIExecutionID = "pending", "ci-a" })
		},
		func(f *quotaPauseFixture) {
			bAt.Store(f.clock.Advance(time.Minute).Unix())
			f.client.setPR(acceptanceBranch, func(pr *github.PR) { pr.CIExecutionID = "ci-b" })
		},
	)
	f.eventually("CI execution B was not persisted as run.await", func() bool {
		for _, event := range f.log.snapshot() {
			if ciExecutionAwait(event, "ci-b") {
				return true
			}
		}
		return false
	})
	wantDeadline := time.Unix(bAt.Load(), 0).Add(ciWaitTimeout)
	f.eventually("wait sidecar did not checkpoint CI execution B", func() bool {
		wait, err := readRowWait(t, f.layout, &f.log.spyEventLog, acceptanceRunID)
		return err == nil && !wait.Ready && wait.OperationDeadline.Equal(wantDeadline) &&
			strings.HasSuffix(wait.OperationID, fmt.Sprintf(":%d", wantDeadline.Unix()))
	})
	f.stillPaused()
}

func TestObservationAcceptance_ResumeDuringQuotaPauseIsDurable(t *testing.T) {
	var awaitedAt atomic.Int64
	f := startQuotaPauseFixture(t,
		func(f *quotaPauseFixture) {
			f.client.setPR(acceptanceBranch, func(pr *github.PR) { pr.StatusCheckRollup = "pending" })
		},
		func(f *quotaPauseFixture) {
			awaitedAt.Store(int64(len(f.log.snapshot())))
			f.client.setPR(acceptanceBranch, func(pr *github.PR) { pr.StatusCheckRollup = "failure" })
		},
	)
	f.eventually("resume after the paused await was not persisted as run.capacity_queued", func() bool {
		snapshot, from := f.log.snapshot(), int(awaitedAt.Load())
		if from == 0 || from > len(snapshot) {
			return false
		}
		for _, event := range snapshot[from:] {
			if event.RunID == acceptanceRunID && event.Type == "run.capacity_queued" {
				return true
			}
		}
		return false
	})
	f.eventually("wait sidecar did not checkpoint the ready continuation", func() bool {
		wait, err := readRowWait(t, f.layout, &f.log.spyEventLog, acceptanceRunID)
		return err == nil && wait.Ready
	})
	f.stillPaused()
}

func TestObservationAcceptance_ExecuteAwaitCheckpointsContinuation(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-accept-exec-")
	t.Chdir(root)
	initGitRepo(t, root)
	worktree := filepath.Join(root, "worktrees", "42-work")
	if err := os.MkdirAll(filepath.Join(worktree, ".sandman", "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".sandman", "task.md"), []byte("# Task\nComplete the pull request.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	client := &reviewWaitSchedulerGitHubClient{fakeGitHubClient: fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "closed"}}, prs: map[string]*github.PR{
		"42-work": {Number: 17, State: "open", HeadRefName: "42-work", HeadRefOid: "head", Body: "Closes #42", StatusCheckRollup: "pending", CIExecutionID: "checks"},
	}}}
	factory := &promptOnlyRunnableFactory{hook: func(issue *github.Issue, branch string) AgentRunResult {
		return AgentRunResult{IssueNumber: issue.Number, Status: "success", Branch: branch}
	}}
	cfg := &config.Config{Agent: "test", Sandbox: "worktree", WorktreeDir: filepath.Join(root, "worktrees"), Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test": {Command: "true"}}}
	layout := paths.NewLayout(cfg, root)
	log := &spyEventLog{}
	var firstWait *daemon.RunWait
	var runID string
	var waitErr error
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: cfg}, log, WithErrorLog(io.Discard), WithRunnableFactory(factory), WithSandboxFactory(&retrySandboxFactory{sandbox: &retrySandbox{workDir: worktree}}), WithRunSessionOpts(runSessionOptions{
		releaseAwaitCapacity: true, now: func() time.Time { return now }, currentHead: func(string) (string, error) { return "head", nil },
		awaitWait: func(context.Context, time.Duration) error {
			if firstWait == nil {
				for _, event := range log.snapshot() {
					if event.Type == "run.started" && event.Issue == 42 {
						runID = event.RunID
					}
				}
				wait, err := readRowWait(t, layout, log, runID)
				firstWait, waitErr = &wait, err
			}
			client.setPR("42-work", func(pr *github.PR) { pr.StatusCheckRollup, pr.State, pr.Merged = "success", "merged", true })
			return nil
		},
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := o.RunBatch(ctx, Request{Issues: []int{42}, Branches: map[int]string{42: "42-work"}, RunTS: "261008174547", RunShortID: "exec-await", Retries: 3, Parallel: 1})
	if err != nil || result == nil || result.Runs[0].Status != "success" || firstWait == nil {
		t.Fatalf("awaited run did not complete: result=%+v error=%v events=%+v", result, err, log.snapshot())
	}
	if waitErr != nil || runID == "" || firstWait.PreviousRunID != runID || firstWait.PreviousBatchID == "" {
		t.Fatalf("first external wait checkpointed without its continuation identity: run=%q wait=%+v err=%v", runID, *firstWait, waitErr)
	}
}

func TestObservationAcceptance_UnacceptedPausedObservationAbortsRow(t *testing.T) {
	var failed atomic.Bool
	f := startQuotaPauseFixture(t,
		func(f *quotaPauseFixture) {
			f.client.setPR(acceptanceBranch, func(pr *github.PR) { pr.StatusCheckRollup, pr.CIExecutionID = "pending", "ci-a" })
		},
		func(f *quotaPauseFixture) {
			f.clock.Advance(time.Minute)
			f.client.setPR(acceptanceBranch, func(pr *github.PR) { pr.CIExecutionID = "ci-b" })
		},
		func(event events.Event) error {
			if ciExecutionAwait(event, "ci-b") && failed.CompareAndSwap(false, true) {
				return errors.New("event log temporarily unavailable")
			}
			return nil
		},
	)
	var terminal events.RunState
	f.eventually("row did not reach a terminal outcome after its observation could not be accepted", func() bool {
		for _, state := range events.ProjectRunStates(f.log.snapshot()) {
			if state.RunID == acceptanceRunID && state.IsTerminal() {
				terminal = state
				return true
			}
		}
		return false
	})
	if !failed.Load() {
		t.Fatal("await persistence was never rejected")
	}
	for _, event := range f.log.snapshot() {
		if event.RunID == acceptanceRunID && event.Payload["reason"] == "AGENT_USAGE_LIMIT" {
			t.Fatalf("unaccepted observation was reported as quota exhaustion: %+v", event)
		}
	}
	if terminal.Status() != "aborted" {
		t.Fatalf("unaccepted observation finished as %q, want aborted; events=%+v", terminal.Status(), f.log.snapshot())
	}
}
