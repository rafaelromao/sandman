package batch

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

type sandboxSandbox = sandbox.Sandbox

const (
	heartbeatTestWorktreeDir = ".sandman/worktrees"
	heartbeatTestBranch      = "sandman/42-fix-bug"
	heartbeatTestIssueNum    = 42
	heartbeatTestTick        = 10 * time.Millisecond
	heartbeatTestIdle        = 1
)

// heartbeatStallRunnable bumps the worktree log's mtime once (so the heartbeat
// sees an initial advance) and then blocks until the fake process is killed.
type heartbeatStallRunnable struct {
	logPath string
	proc    *fakeProcess
	once    sync.Once
	mu      sync.Mutex
	ran     int
}

func (r *heartbeatStallRunnable) Run(ctx context.Context, _ prompt.IssueRenderer, _ string, _ prompt.RenderConfig) AgentRunResult {
	r.once.Do(func() {
		if err := os.MkdirAll(filepath.Dir(r.logPath), 0755); err == nil {
			if f, err := os.OpenFile(r.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
				_, _ = f.WriteString("agent started\n")
				_, _ = f.WriteString("processing step 1\n")
				_, _ = f.WriteString("processing step 2\n")
				_ = f.Close()
			}
		}
	})
	r.mu.Lock()
	r.ran++
	r.mu.Unlock()
	if r.proc == nil || r.proc.killed == nil {
		return AgentRunResult{IssueNumber: heartbeatTestIssueNum, Status: "success", Branch: heartbeatTestBranch}
	}
	select {
	case <-r.proc.killed:
		return AgentRunResult{IssueNumber: heartbeatTestIssueNum, Status: "failure", Branch: heartbeatTestBranch}
	case <-ctx.Done():
		return AgentRunResult{IssueNumber: heartbeatTestIssueNum, Status: "failure", Branch: heartbeatTestBranch}
	}
}

type heartbeatSingleRunnableFactory struct {
	runnable Runnable
}

func (f *heartbeatSingleRunnableFactory) NewRunnable(issue *github.Issue, branch string, sb sandboxSandbox) Runnable {
	return f.runnable
}

type heartbeatDualRunnableFactory struct {
	first  Runnable
	second Runnable
	mu     sync.Mutex
	calls  int
}

func (f *heartbeatDualRunnableFactory) NewRunnable(issue *github.Issue, branch string, sb sandboxSandbox) Runnable {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls == 1 {
		return f.first
	}
	return f.second
}

func heartbeatTestSetup(t *testing.T) (client *fakeGitHubClient, proc *fakeProcess, sb *fakeSandbox, factory *fakeSandboxFactory, workDir string) {
	t.Helper()
	workDir = t.TempDir()
	t.Chdir(workDir)
	initGitRepo(t, workDir)
	client = &fakeGitHubClient{
		issues: map[int]*github.Issue{
			heartbeatTestIssueNum: {Number: heartbeatTestIssueNum, Title: "Fix bug"},
		},
		prs: map[string]*github.PR{heartbeatTestBranch: mergedPR(heartbeatTestBranch, "")},
	}
	proc = &fakeProcess{killed: make(chan struct{})}
	worktreePath := filepath.Join(workDir, heartbeatTestWorktreeDir, "sandman", "42-fix-bug")
	sb = &fakeSandbox{workDir: worktreePath, process: proc}
	factory = &fakeSandboxFactory{sandbox: sb}
	return
}

func newHeartbeatOrchestrator(client *fakeGitHubClient, sbFactory *fakeSandboxFactory, runFactory RunnableFactory, cfgRunIdleTimeout int, eventLog events.EventLog) *Orchestrator {
	cfg := &config.Config{
		Agent:          "test-agent",
		Sandbox:        "worktree",
		WorktreeDir:    heartbeatTestWorktreeDir,
		Git:            config.GitConfig{BaseBranch: "main"},
		RunIdleTimeout: cfgRunIdleTimeout,
		AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}},
	}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: cfg}, eventLog)
	o.sandboxFactory = sbFactory
	o.runnableFactory = runFactory
	o.heartbeatTickInterval = heartbeatTestTick
	if o.errorLog == nil {
		o.errorLog = io.Discard
	}
	return o
}

func countEventsByType(snapshot []events.Event, t string) int {
	n := 0
	for _, e := range snapshot {
		if e.Type == t {
			n++
		}
	}
	return n
}

func findEvent(snapshot []events.Event, t string) *events.Event {
	for i, e := range snapshot {
		if e.Type == t {
			return &snapshot[i]
		}
	}
	return nil
}

func TestRunBatch_KillsStuckRunAfterIdleTimeout(t *testing.T) {
	client, proc, sb, factory, workDir := heartbeatTestSetup(t)
	_ = sb
	logPath := filepath.Join(workDir, ".sandman", "logs", "42.log")
	runnable := &heartbeatStallRunnable{logPath: logPath, proc: proc}
	runFactory := &heartbeatSingleRunnableFactory{runnable: runnable}
	spyLog := &spyEventLog{}
	o := newHeartbeatOrchestrator(client, factory, runFactory, 0, spyLog)

	start := time.Now()
	result, _ := o.RunBatch(context.Background(), Request{
		Issues:            []int{heartbeatTestIssueNum},
		RunIdleTimeoutSet: true,
		RunIdleTimeout:    heartbeatTestIdle,
	})
	elapsed := time.Since(start)

	if result == nil || len(result.Runs) != 1 {
		t.Fatalf("expected one run, got %#v", result)
	}
	if result.Runs[0].Status != "aborted" {
		t.Errorf("status = %q, want aborted", result.Runs[0].Status)
	}
	if !proc.killCalled {
		t.Error("expected process.Kill to be called by heartbeat")
	}
	if elapsed > 2*time.Second {
		t.Errorf("heartbeat took %v to fire, want < 2s", elapsed)
	}
	if got := findEvent(spyLog.events, "run.idle_timeout"); got == nil {
		t.Fatalf("expected run.idle_timeout event, got %v", spyLog.events)
	} else {
		if got.Payload["issue"] != heartbeatTestIssueNum {
			t.Errorf("idle_timeout payload issue = %v, want %d", got.Payload["issue"], heartbeatTestIssueNum)
		}
		if got.Payload["attempt"] != 1 {
			t.Errorf("idle_timeout payload attempt = %v, want 1", got.Payload["attempt"])
		}
		if got.Payload["idle_timeout_seconds"] != heartbeatTestIdle {
			t.Errorf("idle_timeout payload idle_timeout_seconds = %v, want %d", got.Payload["idle_timeout_seconds"], heartbeatTestIdle)
		}
		if _, ok := got.Payload["idle_seconds"]; !ok {
			t.Error("expected idle_seconds in payload")
		}
		if reason, _ := got.Payload["reason"].(string); reason != "run_idle_timeout" {
			t.Errorf("reason = %q, want \"run_idle_timeout\"", reason)
		}
		lastLines, _ := got.Payload["last_log_lines"].([]string)
		if len(lastLines) != 3 {
			t.Errorf("last_log_lines = %v (len=%d), want 3 lines", lastLines, len(lastLines))
		}
	}
}

func TestRunBatch_IdleTimeoutRetriesAndSucceeds(t *testing.T) {
	client, proc, _, factory, workDir := heartbeatTestSetup(t)
	logPath := filepath.Join(workDir, ".sandman", "logs", "42.log")
	stall := &heartbeatStallRunnable{logPath: logPath, proc: proc}
	success := &fakeRunnable{result: AgentRunResult{IssueNumber: heartbeatTestIssueNum, Status: "success", Branch: heartbeatTestBranch}}
	runFactory := &heartbeatDualRunnableFactory{first: stall, second: success}
	spyLog := &spyEventLog{}
	o := newHeartbeatOrchestrator(client, factory, runFactory, 0, spyLog)
	o.runSessionOpts.killTimeout = 50 * time.Millisecond

	result, _ := o.RunBatch(context.Background(), Request{
		Issues:            []int{heartbeatTestIssueNum},
		RunIdleTimeoutSet: true,
		RunIdleTimeout:    heartbeatTestIdle,
		Retries:           1,
	})

	if result == nil || len(result.Runs) != 1 {
		t.Fatalf("expected one run, got %#v", result)
	}
	// The PR is already merged at the start of the run (heartbeatTestSetup
	// fixture), so the pre-retry guard short-circuits the retry after the
	// first attempt is aborted. The run is reported as success without
	// running the second attempt.
	if result.Runs[0].Status != "success" {
		t.Errorf("status = %q, want success", result.Runs[0].Status)
	}
	if result.Runs[0].RetriesTotal != 1 {
		t.Errorf("RetriesTotal = %d, want 1 (pre-retry guard short-circuits the retry when PR is already merged)", result.Runs[0].RetriesTotal)
	}
	if countEventsByType(spyLog.events, "run.idle_timeout") != 1 {
		t.Errorf("expected exactly 1 run.idle_timeout event, got %d (events: %v)", countEventsByType(spyLog.events, "run.idle_timeout"), spyLog.events)
	}
	if got := countEventsByType(spyLog.events, "run.started"); got != 1 {
		t.Errorf("expected 1 run.started event (per issue, not per attempt), got %d", got)
	}
	if got := findEvent(spyLog.events, "run.finished"); got == nil {
		t.Errorf("expected run.finished event, got %v", spyLog.events)
	} else if got.Payload["status"] != "success" {
		t.Errorf("run.finished status = %v, want success", got.Payload["status"])
	}
	if countEventsByType(spyLog.events, "run.aborted") != 0 {
		t.Errorf("expected no run.aborted event (terminal event is emitted once after the loop with the final result), got %d", countEventsByType(spyLog.events, "run.aborted"))
	}
	if !proc.killCalled {
		t.Error("expected process.Kill to have been called by heartbeat on first attempt")
	}
}

func TestRunBatch_IdleTimeoutZeroDisables(t *testing.T) {
	client, proc, _, factory, _ := heartbeatTestSetup(t)
	_ = proc
	success := &fakeRunnable{result: AgentRunResult{IssueNumber: heartbeatTestIssueNum, Status: "success", Branch: heartbeatTestBranch}}
	runFactory := &heartbeatSingleRunnableFactory{runnable: success}
	spyLog := &spyEventLog{}
	o := newHeartbeatOrchestrator(client, factory, runFactory, 0, spyLog)

	result, _ := o.RunBatch(context.Background(), Request{
		Issues:            []int{heartbeatTestIssueNum},
		RunIdleTimeoutSet: true,
		RunIdleTimeout:    0,
	})

	if result == nil || len(result.Runs) != 1 {
		t.Fatalf("expected one run, got %#v", result)
	}
	if result.Runs[0].Status != "success" {
		t.Errorf("status = %q, want success (idle timeout disabled)", result.Runs[0].Status)
	}
	if proc.killCalled {
		t.Error("expected process to NOT be killed when idle timeout is 0")
	}
	if countEventsByType(spyLog.events, "run.idle_timeout") != 0 {
		t.Errorf("expected no run.idle_timeout event, got events: %v", spyLog.events)
	}
}

func TestRunBatch_IdleTimeoutRequestOverridesConfig(t *testing.T) {
	client, proc, _, factory, workDir := heartbeatTestSetup(t)
	logPath := filepath.Join(workDir, ".sandman", "logs", "42.log")
	stall := &heartbeatStallRunnable{logPath: logPath, proc: proc}
	runFactory := &heartbeatSingleRunnableFactory{runnable: stall}
	spyLog := &spyEventLog{}
	o := newHeartbeatOrchestrator(client, factory, runFactory, 999, spyLog)

	_, _ = o.RunBatch(context.Background(), Request{
		Issues:            []int{heartbeatTestIssueNum},
		RunIdleTimeoutSet: true,
		RunIdleTimeout:    heartbeatTestIdle,
	})

	evt := findEvent(spyLog.events, "run.idle_timeout")
	if evt == nil {
		t.Fatalf("expected run.idle_timeout event, got %v", spyLog.events)
	}
	if got := evt.Payload["idle_timeout_seconds"]; got != heartbeatTestIdle {
		t.Errorf("idle_timeout_seconds = %v, want %d (request should override config=999)", got, heartbeatTestIdle)
	}
}

func TestRunBatch_IdleTimeoutConfigUsedWhenRequestUnset(t *testing.T) {
	client, proc, _, factory, workDir := heartbeatTestSetup(t)
	logPath := filepath.Join(workDir, ".sandman", "logs", "42.log")
	stall := &heartbeatStallRunnable{logPath: logPath, proc: proc}
	runFactory := &heartbeatSingleRunnableFactory{runnable: stall}
	spyLog := &spyEventLog{}
	o := newHeartbeatOrchestrator(client, factory, runFactory, heartbeatTestIdle, spyLog)

	_, _ = o.RunBatch(context.Background(), Request{
		Issues: []int{heartbeatTestIssueNum},
	})

	evt := findEvent(spyLog.events, "run.idle_timeout")
	if evt == nil {
		t.Fatalf("expected run.idle_timeout event, got %v", spyLog.events)
	}
	if got := evt.Payload["idle_timeout_seconds"]; got != heartbeatTestIdle {
		t.Errorf("idle_timeout_seconds = %v, want %d (config value should be used)", got, heartbeatTestIdle)
	}
}

func TestResolveRunIdleTimeout(t *testing.T) {
	if got := resolveRunIdleTimeout(Request{}, &config.Config{RunIdleTimeout: 0}); got != 0 {
		t.Errorf("resolveRunIdleTimeout unset/config0 = %d, want 0", got)
	}
	if got := resolveRunIdleTimeout(Request{}, &config.Config{RunIdleTimeout: 1800}); got != 1800 {
		t.Errorf("resolveRunIdleTimeout unset/config1800 = %d, want 1800", got)
	}
	if got := resolveRunIdleTimeout(Request{RunIdleTimeoutSet: true, RunIdleTimeout: 0}, &config.Config{RunIdleTimeout: 1800}); got != 0 {
		t.Errorf("resolveRunIdleTimeout set0/overrides = %d, want 0", got)
	}
	if got := resolveRunIdleTimeout(Request{RunIdleTimeoutSet: true, RunIdleTimeout: 42}, &config.Config{RunIdleTimeout: 1800}); got != 42 {
		t.Errorf("resolveRunIdleTimeout set42/overrides = %d, want 42", got)
	}
	if got := resolveRunIdleTimeout(Request{RunIdleTimeoutSet: true, RunIdleTimeout: 42}, nil); got != 42 {
		t.Errorf("resolveRunIdleTimeout set42/nil = %d, want 42", got)
	}
}
