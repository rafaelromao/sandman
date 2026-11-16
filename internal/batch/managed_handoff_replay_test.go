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

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

// incidentReplayFactory models one implementation attempt that stays busy with
// required local verification until the test releases it, followed by the
// runtime's merge relaunch.
type incidentReplayFactory struct {
	mu            sync.Mutex
	created       int
	prompts       []string
	started       chan struct{}
	release       chan struct{}
	onMergeLaunch func()
}

func (f *incidentReplayFactory) NewRunnable(_ *github.Issue, branch string, _ sandbox.Sandbox) Runnable {
	f.mu.Lock()
	f.created++
	attempt := f.created
	f.mu.Unlock()
	if attempt == 1 {
		return &controlledRunnable{result: AgentRunResult{Status: "success", Branch: branch}, started: f.started, release: f.release}
	}
	if f.onMergeLaunch != nil {
		f.onMergeLaunch()
	}
	return &incidentReplayMergeRunnable{factory: f, branch: branch}
}

type incidentReplayMergeRunnable struct {
	factory *incidentReplayFactory
	branch  string
}

func (r *incidentReplayMergeRunnable) Run(_ context.Context, _ prompt.IssueRenderer, _ string, cfg prompt.RenderConfig) AgentRunResult {
	r.factory.mu.Lock()
	r.factory.prompts = append(r.factory.prompts, cfg.TaskPrompt)
	r.factory.mu.Unlock()
	return AgentRunResult{Status: "success", Branch: r.branch}
}

// incidentGitHub serializes access to the pull request, which the runtime's
// closing-reference guard reads while the agent attempt executes.
type incidentGitHub struct {
	*registrationGitHubClient
	mu sync.Mutex
}

func (c *incidentGitHub) FindPRByBranch(ctx context.Context, branch string) (*github.PR, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	pr, err := c.registrationGitHubClient.FindPRByBranch(ctx, branch)
	if pr == nil {
		return pr, err
	}
	snapshot := *pr
	return &snapshot, err
}

func (c *incidentGitHub) update(change func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	change()
}

// Characterization of the 2026-11-12 incident (issue #2957): the pull request's
// review and CI completed while the implementor was still running its own local
// verification. The runtime never pre-empts an executing attempt, and the one
// post-attempt lifecycle decision selects merge work without an initial wait.
func TestIncidentReplay_ReadyReviewAndCIDoNotPreemptActiveVerification(t *testing.T) {
	_, registered, pr, seedDir, now := informalApprovalLifecycleFixture(t, approvingMetricsReview)
	registration, err := readFileReviewRegistration(paths.NewLayout(nil, seedDir).PRReviewRegistrationPath(17))
	if err != nil {
		t.Fatal(err)
	}
	ready := *now // review answered; the pull request will be green by this time

	dir := t.TempDir()
	t.Chdir(dir)
	workDir := filepath.Join(dir, "worktree")
	if err := os.MkdirAll(filepath.Join(workDir, ".sandman", "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := atomicfs.WriteAtomicJSON(paths.NewLayout(nil, workDir).PRReviewRegistrationPath(17), registration, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := atomicfs.WriteAtomic(filepath.Join(workDir, ".sandman", "task.md"), []byte("# Task\n\n## Next Step\nFinish local verification, then merge.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pr.Body = "Closes #42"
	registered.issues = map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}}
	// No pull request exists when the attempt starts: the agent publishes it,
	// and delivers its review request, while it keeps working.
	delete(registered.prs, gateTestBranch)
	client := &incidentGitHub{registrationGitHubClient: registered}

	factory := &incidentReplayFactory{started: make(chan struct{}), release: make(chan struct{})}
	factory.onMergeLaunch = func() {
		client.update(func() {
			pr.State, pr.Merged = "merged", true
			registered.issues[42].State = "closed"
		})
	}
	log := &events.JSONLLogger{Path: filepath.Join(dir, "events.jsonl")}
	// The attempt starts before the review request was delivered.
	attemptStart := ready.Add(-10 * time.Minute)
	clock := attemptStart
	var clockMu sync.Mutex
	opts := gateTestRunOptions()
	opts.now = func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock }
	var waitMu sync.Mutex
	lifecycleWaits, awaitWaits := 0, 0
	opts.lifecycleWait = func(context.Context, time.Duration) error {
		waitMu.Lock()
		defer waitMu.Unlock()
		lifecycleWaits++
		return errLifecycleObservationTestStop
	}
	opts.awaitWait = func(context.Context, time.Duration) error {
		waitMu.Lock()
		defer waitMu.Unlock()
		awaitWaits++
		return nil
	}
	sbFactory := &retrySandboxFactory{sandbox: &retrySandbox{workDir: workDir}}
	o := NewOrchestrator(client, &retryRenderer{result: "rendered task"}, nil, log,
		WithRunnableFactory(factory), WithSandboxFactory(sbFactory), WithErrorLog(io.Discard), WithRunSessionOpts(opts))
	bc := BatchConfig{Cfg: &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentName: "test-agent", AgentCfg: config.Agent{Command: "true"}, IdentityResolver: noopIdentityResolver(), Retries: 3}
	type outcome struct {
		result  AgentRunResult
		started bool
	}
	done := make(chan outcome, 1)
	go func() {
		result, started := o.newRunExecutor(t.Context(), bc, sbFactory, nil).Execute(t.Context(), RowSpec{
			IssueNumber: 42, Branches: map[int]string{42: gateTestBranch}, BaseBranch: "main",
			RunTS: "261112110000", RunShortID: "incident",
			RenderCfg: prompt.RenderConfig{ReviewCommand: "/sandman review", ReviewTimeout: 1800},
		})
		done <- outcome{result, started}
	}()

	select {
	case <-factory.started:
	case early := <-done:
		raw, readErr := log.Read()
		t.Fatalf("Execute returned before the attempt started: %+v started=%t events=%+v (read error: %v)", early.result, early.started, raw, readErr)
	case <-time.After(10 * time.Second):
		t.Fatal("implementation attempt never started")
	}
	// While the attempt is still verifying locally, current-head review and CI
	// both become ready.
	clockMu.Lock()
	clock = ready.Add(5 * time.Minute)
	clockMu.Unlock()
	client.update(func() {
		registered.prs[gateTestBranch] = pr
		pr.StatusCheckRollup = "success"
	})

	raw, err := log.Read()
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"run.await", "run.capacity_queued", "run.queued", "run.resumed", "run.retry", "run.continued", "run.finished", "run.aborted"} {
		if got := countEventsByType(raw, forbidden); got != 0 {
			t.Fatalf("%d %s events were recorded while the attempt was still executing: %+v", got, forbidden, raw)
		}
	}
	states := events.ProjectRunStates(raw)
	if len(states) != 1 || states[0].IsTerminal() || states[0].IsAwaiting() {
		t.Fatalf("projection while the attempt executes = %+v, want one running, non-waiting, non-terminal Run", states)
	}
	runID := states[0].RunID
	factory.mu.Lock()
	launches := factory.created
	factory.mu.Unlock()
	waitMu.Lock()
	waitsBefore := lifecycleWaits + awaitWaits
	waitMu.Unlock()
	if launches != 1 || waitsBefore != 0 {
		t.Fatalf("launches=%d waits=%d while executing, want one launch and no observation wait", launches, waitsBefore)
	}

	close(factory.release)
	var got outcome
	select {
	case got = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("run did not finish after the attempt returned")
	}
	if !got.started || got.result.Status != "success" {
		t.Fatalf("run = %+v started=%t, want success after merge work", got.result, got.started)
	}
	waitMu.Lock()
	defer waitMu.Unlock()
	if lifecycleWaits != 0 || awaitWaits != 0 {
		t.Fatalf("waits after the attempt returned = lifecycle %d / batch %d, want the decision taken without an initial wait", lifecycleWaits, awaitWaits)
	}
	factory.mu.Lock()
	defer factory.mu.Unlock()
	if factory.created != 2 || len(factory.prompts) != 1 {
		t.Fatalf("launches = %d prompts = %d, want the first attempt plus exactly one merge relaunch", factory.created, len(factory.prompts))
	}
	if !strings.Contains(factory.prompts[0], "informal approval is sufficient") {
		t.Fatalf("merge relaunch did not carry the accepted approval:\n%s", factory.prompts[0])
	}
	raw, err = log.Read()
	if err != nil {
		t.Fatal(err)
	}
	if countEventsByType(raw, "run.started") != 1 || countEventsByType(raw, "run.await") != 0 || countEventsByType(raw, "run.capacity_queued") != 0 || countEventsByType(raw, "run.retry") != 0 {
		t.Fatalf("lifecycle history = %+v, want one start, no wait, no queue and no retry", raw)
	}
	resumed := findEvent(raw, "run.resumed")
	if resumed == nil || resumed.Payload["reason"] != "approval" || resumed.RunID != runID {
		t.Fatalf("resume = %+v, want the approval decision on the same RunID %q", resumed, runID)
	}
	states = events.ProjectRunStates(raw)
	if len(states) != 1 || states[0].RunID != runID || states[0].Status() != "success" || !states[0].IsTerminal() {
		t.Fatalf("terminal projection = %+v, want one RunID ending in success", states)
	}
}
