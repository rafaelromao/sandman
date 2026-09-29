package batch

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

func TestUsageLimitFailureUsesOrdinaryAgentRetry(t *testing.T) {
	result, sandbox, log, waits := runUsageLimitBatch(t, 1, 0, 1)

	if result.Runs[0].Status != "failure" {
		t.Fatalf("status = %q, want failure because no external operation resolves the usage limit", result.Runs[0].Status)
	}
	if got := sandbox.attemptCount(); got != 2 {
		t.Fatalf("agent attempts = %d, want 2", got)
	}
	if len(waits) != 0 {
		t.Fatalf("await waits = %v, want none", waits)
	}
	commands := sandbox.commandsSnapshot()
	if len(commands) != 2 || strings.Contains(commands[1], "--session") {
		t.Fatalf("commands = %q, want a fresh ordinary retry", commands)
	}
	if got := countEventsByType(log.snapshot(), "run.await"); got != 0 {
		t.Fatalf("run.await events = %d, want 0", got)
	}
	if got := countEventsByType(log.snapshot(), "run.retry"); got != 1 {
		t.Fatalf("run.retry events = %d, want one ordinary retry", got)
	}
	finished := findEvent(log.snapshot(), "run.finished")
	if finished == nil || finished.Payload["reason"] != "PULL_REQUEST_MISSING" {
		t.Fatalf("run.finished = %#v, want failure for the next unresolved implementor-owned blocker", finished)
	}
}

func TestUsageLimitExhaustionDoesNotCreateAwait(t *testing.T) {
	result, sandbox, log, waits := runUsageLimitBatch(t, 31, 0, 1)

	if result.Runs[0].Status != "failure" {
		t.Fatalf("status = %q, want failure after the fresh retry has no merged PR", result.Runs[0].Status)
	}
	if got := sandbox.attemptCount(); got != 2 {
		t.Fatalf("agent attempts = %d, want initial attempt plus one configured retry", got)
	}
	if len(waits) != 0 {
		t.Fatalf("await waits = %v, want none", waits)
	}
	commands := sandbox.commandsSnapshot()
	if len(commands) != 2 || strings.Contains(commands[1], "--session") {
		t.Fatalf("commands = %q, want ordinary retry to start a fresh session", commands)
	}
	if got := countEventsByType(log.snapshot(), "run.await"); got != 0 {
		t.Fatalf("run.await events = %d, want 0", got)
	}
	if got := countEventsByType(log.snapshot(), "run.retry"); got != 1 {
		t.Fatalf("run.retry events = %d, want one configured retry", got)
	}
	finished := findEvent(log.snapshot(), "run.finished")
	if finished == nil || finished.Payload["reason"] != "AGENT_USAGE_LIMIT" {
		t.Fatalf("run.finished = %#v, want structured usage-limit failure", finished)
	}
}

func TestUsageLimitFailureHonorsRetriesDisabled(t *testing.T) {
	result, sandbox, log, waits := runUsageLimitBatch(t, 2, 0, 0)
	if result.Runs[0].Status != "failure" {
		t.Fatalf("status = %q, want failure", result.Runs[0].Status)
	}
	if got := sandbox.attemptCount(); got != 1 {
		t.Fatalf("agent attempts = %d, want one attempt with retries disabled", got)
	}
	if len(waits) != 0 || countEventsByType(log.snapshot(), "run.await") != 0 {
		t.Fatalf("usage-limit path must not await: waits=%v events=%v", waits, log.snapshot())
	}
	if got := countEventsByType(log.snapshot(), "run.retry"); got != 0 {
		t.Fatalf("run.retry events = %d, want 0", got)
	}
}

func runUsageLimitBatch(t *testing.T, failures, idleTimeout, retries int) (*Result, *usageLimitRetrySandbox, *spyEventLog, []time.Duration) {
	t.Helper()
	root := t.TempDir()
	t.Chdir(root)
	initGitRepo(t, root)

	const branch = "42-usage-limit"
	sb := &usageLimitRetrySandbox{workDir: filepath.Join(root, "worktree"), failures: failures}
	log := &spyEventLog{}
	var waits []time.Duration
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, Title: "Usage limit", State: "open"}},
		prs:    map[string]*github.PR{branch: nil},
	}
	cfg := &config.Config{
		Agent:          "opencode",
		DefaultAgent:   "opencode",
		Sandbox:        "worktree",
		WorktreeDir:    filepath.Join(root, "worktrees"),
		Git:            config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{"opencode": config.BuiltInAgentPresets["opencode"].Agent("opencode")},
	}
	o := NewOrchestrator(client, &retryRenderer{result: "# Task\n\nRetry safely."}, &fakeConfigStore{config: cfg}, log,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&usageLimitRetrySandboxFactory{sandbox: sb}),
		WithRunnableFactory(&usageLimitRetryRunnableFactory{}),
		WithRunSessionOpts(runSessionOptions{
			releaseAwaitCapacity: true,
			awaitWait: func(_ context.Context, delay time.Duration) error {
				waits = append(waits, delay)
				return nil
			},
		}),
	)
	result, err := o.RunBatch(context.Background(), Request{
		Issues:            []int{42},
		Branches:          map[int]string{42: branch},
		Agent:             "opencode",
		Retries:           retries,
		Parallel:          1,
		RunIdleTimeout:    idleTimeout,
		RunIdleTimeoutSet: true,
		RunTS:             "260906151914",
		RunShortID:        "limit",
	})
	if result == nil {
		t.Fatalf("run batch result is nil: %v", err)
	}
	return result, sb, log, waits
}

type usageLimitRetrySandbox struct {
	workDir  string
	failures int

	mu       sync.Mutex
	attempts int
	commands []string
}

func (s *usageLimitRetrySandbox) Start(sandbox.SandboxStart) error {
	return os.MkdirAll(filepath.Join(s.workDir, ".sandman"), 0o755)
}

func (s *usageLimitRetrySandbox) Exec(_ context.Context, command string, stdout, stderr io.Writer) error {
	s.mu.Lock()
	s.attempts++
	attempt := s.attempts
	s.commands = append(s.commands, command)
	s.mu.Unlock()
	_, _ = io.WriteString(stdout, `{"type":"text","sessionID":"usage-limit-session","part":{"text":"working"}}`+"\n")
	if attempt <= s.failures {
		_, _ = io.WriteString(stderr, "Error: The usage limit has been reached\n")
		return errors.New("OpenCode usage limit")
	}
	return nil
}

func (s *usageLimitRetrySandbox) ExecInteractive(context.Context, string) error { return nil }
func (s *usageLimitRetrySandbox) Stop() error                                   { return nil }
func (s *usageLimitRetrySandbox) WorkDir() string                               { return s.workDir }
func (s *usageLimitRetrySandbox) RepoPath() string                              { return filepath.Dir(s.workDir) }
func (s *usageLimitRetrySandbox) Process() sandbox.Process                      { return nil }
func (s *usageLimitRetrySandbox) RestoreHostPaths() error                       { return nil }
func (s *usageLimitRetrySandbox) WritePrompt(content string) error {
	return os.WriteFile(filepath.Join(s.workDir, ".sandman", "task.md"), []byte(content), 0o644)
}

func (s *usageLimitRetrySandbox) attemptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

func (s *usageLimitRetrySandbox) commandsSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...)
}

type usageLimitRetrySandboxFactory struct {
	sandbox *usageLimitRetrySandbox
}

func (f *usageLimitRetrySandboxFactory) NewSandbox(string, string, string, string, sandbox.Container) sandbox.Sandbox {
	return f.sandbox
}

type usageLimitRetryRunnableFactory struct{}

func (usageLimitRetryRunnableFactory) NewRunnable(issue *github.Issue, branch string, sandbox sandbox.Sandbox) Runnable {
	return NewAgentRun(issue, branch, sandbox)
}

var _ sandbox.Sandbox = (*usageLimitRetrySandbox)(nil)
