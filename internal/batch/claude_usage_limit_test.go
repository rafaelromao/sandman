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

func TestClaudeUsageLimitFailsWithoutExternalWait(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	initGitRepo(t, root)

	const branch = "42-claude-limit"
	sb := &claudeUsageLimitSandbox{workDir: filepath.Join(root, "worktree"), failures: 1}
	log := &spyEventLog{}
	var waits []time.Duration
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, Title: "Claude usage limit", State: "open"}},
	}
	cfg := &config.Config{
		Agent:          "claude",
		DefaultAgent:   "claude",
		Sandbox:        "worktree",
		WorktreeDir:    filepath.Join(root, "worktrees"),
		Git:            config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{"claude": config.BuiltInAgentPresets["claude"].Agent("claude")},
	}
	o := NewOrchestrator(client, &retryRenderer{result: "# Task\n\nRetry safely."}, &fakeConfigStore{config: cfg}, log,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&claudeUsageLimitSandboxFactory{sandbox: sb}),
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
		Agent:             "claude",
		Model:             "sonnet",
		Retries:           0,
		Parallel:          1,
		RunIdleTimeout:    0,
		RunIdleTimeoutSet: true,
		RunTS:             "260925151914",
		RunShortID:        "claud",
	})
	if result == nil {
		t.Fatalf("run batch result is nil: %v", err)
	}

	if result.Runs[0].Status != "failure" {
		t.Fatalf("status = %q, want terminal usage-limit failure", result.Runs[0].Status)
	}
	if len(waits) != 0 {
		t.Fatalf("await waits = %v, want none", waits)
	}
	commands := sb.commandsSnapshot()
	if len(commands) != 1 {
		t.Fatalf("commands = %q, want one failed launch", commands)
	}
	if strings.Contains(commands[0], "--continue") {
		t.Fatalf("first command = %q, want a fresh conversation", commands[0])
	}
	if !strings.Contains(commands[0], "claude -p --output-format stream-json --verbose") || !strings.Contains(commands[0], "--model 'sonnet'") {
		t.Fatalf("command = %q, want the claude preset with the requested model", commands[0])
	}
	if got := countEventsByType(log.snapshot(), "run.await"); got != 0 {
		t.Fatalf("run.await events = %d, want 0", got)
	}
	if got := countEventsByType(log.snapshot(), "run.retry"); got != 0 {
		t.Fatalf("run.retry events = %d, want 0", got)
	}
	finished := findEvent(log.snapshot(), "run.finished")
	if finished == nil || finished.Payload["reason"] != "AGENT_USAGE_LIMIT" {
		t.Fatalf("run.finished = %#v, want structured usage-limit failure", finished)
	}
}

type claudeUsageLimitSandbox struct {
	workDir  string
	failures int

	mu       sync.Mutex
	attempts int
	commands []string
}

func (s *claudeUsageLimitSandbox) Start(sandbox.SandboxStart) error {
	return os.MkdirAll(filepath.Join(s.workDir, ".sandman"), 0o755)
}

func (s *claudeUsageLimitSandbox) Exec(_ context.Context, command string, stdout, _ io.Writer) error {
	s.mu.Lock()
	s.attempts++
	attempt := s.attempts
	s.commands = append(s.commands, command)
	s.mu.Unlock()
	_, _ = io.WriteString(stdout, `{"type":"system","subtype":"init","session_id":"conv-1"}`+"\n")
	if attempt <= s.failures {
		_, _ = io.WriteString(stdout, claudeUsageLimitResult+"\n")
		return errors.New("exit status 1")
	}
	_, _ = io.WriteString(stdout, `{"type":"result","subtype":"success","is_error":false,"result":"done"}`+"\n")
	return nil
}

func (s *claudeUsageLimitSandbox) ExecInteractive(context.Context, string) error { return nil }
func (s *claudeUsageLimitSandbox) Stop() error                                   { return nil }
func (s *claudeUsageLimitSandbox) WorkDir() string                               { return s.workDir }
func (s *claudeUsageLimitSandbox) RepoPath() string                              { return filepath.Dir(s.workDir) }
func (s *claudeUsageLimitSandbox) Process() sandbox.Process                      { return nil }
func (s *claudeUsageLimitSandbox) RestoreHostPaths() error                       { return nil }
func (s *claudeUsageLimitSandbox) WritePrompt(content string) error {
	return os.WriteFile(filepath.Join(s.workDir, ".sandman", "task.md"), []byte(content), 0o644)
}

func (s *claudeUsageLimitSandbox) commandsSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...)
}

type claudeUsageLimitSandboxFactory struct {
	sandbox *claudeUsageLimitSandbox
}

func (f *claudeUsageLimitSandboxFactory) NewSandbox(string, string, string, string, sandbox.Container) sandbox.Sandbox {
	return f.sandbox
}

var _ sandbox.Sandbox = (*claudeUsageLimitSandbox)(nil)
