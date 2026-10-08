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
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

func TestUsageLimitAwaitResumesSameSessionWithIdleTimeoutDisabled(t *testing.T) {
	result, sandbox, log, waits := runUsageLimitBatch(t, 1, 0, 1)

	if result.Runs[0].Status != "success" {
		t.Fatalf("status = %q, want success", result.Runs[0].Status)
	}
	if got := sandbox.attemptCount(); got != 2 {
		t.Fatalf("agent attempts = %d, want 2", got)
	}
	if len(waits) != 1 || waits[0] != 10*time.Minute {
		t.Fatalf("await waits = %v, want [10m0s]", waits)
	}
	commands := sandbox.commandsSnapshot()
	if len(commands) != 2 || !strings.Contains(commands[1], "--session 'usage-limit-session'") {
		t.Fatalf("commands = %q, want second command to reuse the OpenCode session", commands)
	}
	if got := countEventsByType(log.snapshot(), "run.await"); got != 1 {
		t.Fatalf("run.await events = %d, want 1", got)
	}
	if got := countEventsByType(log.snapshot(), "run.continued"); got != 1 {
		t.Fatalf("run.continued events = %d, want 1", got)
	}
	if got := countEventsByType(log.snapshot(), "run.retry"); got != 0 {
		t.Fatalf("run.retry events = %d, want 0", got)
	}
	for _, event := range log.snapshot() {
		if event.Type != "run.await" {
			continue
		}
		if event.Payload["await_reason"] != "usage-limit" || event.Payload["usage_limit_poll_seconds"] != int(usageLimitPollInterval/time.Second) || event.Payload["usage_limit_retry_window_seconds"] != int(usageLimitRetryWindow/time.Second) {
			t.Fatalf("run.await payload = %#v, want usage-limit polling metadata", event.Payload)
		}
	}
}

func TestUsageLimitAwaitRetriesAfterFiveHours(t *testing.T) {
	result, sandbox, log, waits := runUsageLimitBatch(t, 31, 0, 1)

	if result.Runs[0].Status != "failure" {
		t.Fatalf("status = %q, want failure after the fresh retry has no merged PR", result.Runs[0].Status)
	}
	if got := sandbox.attemptCount(); got != 32 {
		t.Fatalf("agent attempts = %d, want initial plus 30 probes and one fresh ordinary retry", got)
	}
	if len(waits) != 30 {
		t.Fatalf("await waits = %d, want 30", len(waits))
	}
	for _, wait := range waits {
		if wait != usageLimitPollInterval {
			t.Fatalf("await waits = %v, want every wait to be 10m", waits)
		}
	}
	commands := sandbox.commandsSnapshot()
	if !strings.Contains(commands[1], "--session 'usage-limit-session'") {
		t.Fatalf("commands = %q, want the poll to reuse the OpenCode session", commands)
	}
	if strings.Contains(commands[len(commands)-1], "--session") {
		t.Fatalf("commands = %q, want the ordinary retry to start a fresh session", commands)
	}
	if got := countEventsByType(log.snapshot(), "run.await"); got != 30 {
		t.Fatalf("run.await events = %d, want 30", got)
	}
	if got := countEventsByType(log.snapshot(), "run.retry"); got != 1 {
		t.Fatalf("run.retry events = %d, want the historical ordinary retry at quota expiry", got)
	}
}

func TestUsageLimitAwaitCanFinishOnFreshRetryAfterFiveHours(t *testing.T) {
	result, sb, log, waits := runUsageLimitBatchCase(t, 31, 0, 1, true, 0)
	if result.Runs[0].Status != "success" || sb.attemptCount() != 32 || len(waits) != 30 || countEventsByType(log.snapshot(), "run.retry") != 1 {
		t.Fatalf("fresh retry could not finish: result=%+v attempts=%d waits=%d events=%+v", result, sb.attemptCount(), len(waits), log.snapshot())
	}
	commands := sb.commandsSnapshot()
	if strings.Contains(commands[len(commands)-1], "--session") {
		t.Fatal("ordinary retry reused the quota-limited conversation")
	}
}

func TestUsageLimitAwaitKeepsConfiguredOrdinaryRetryBoundary(t *testing.T) {
	for _, retries := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("retries=%d", retries), func(t *testing.T) {
			result, sb, log, waits := runUsageLimitBatch(t, 99, 0, retries)
			if result.Runs[0].Status != "failure" || sb.attemptCount() != 31+retries || len(waits) != 30 || countEventsByType(log.snapshot(), "run.retry") != retries {
				t.Fatalf("quota changed ordinary retry boundary: result=%+v attempts=%d waits=%d retries=%d", result, sb.attemptCount(), len(waits), countEventsByType(log.snapshot(), "run.retry"))
			}
		})
	}
}

func TestUsageLimitAwaitCancellationPreventsFreshRetry(t *testing.T) {
	result, sb, log, waits := runUsageLimitBatchCase(t, 99, 0, 1, false, 31)
	if result.Runs[0].Status != "aborted" || sb.attemptCount() != 31 || len(waits) != 30 || countEventsByType(log.snapshot(), "run.retry") != 0 {
		t.Fatalf("cancelled quota recovery launched retry: result=%+v attempts=%d waits=%d events=%+v", result, sb.attemptCount(), len(waits), log.snapshot())
	}
}

func TestUsageLimitAwaitPollsWhileQuotaRemainsExhausted(t *testing.T) {
	_, sandbox, log, waits := runUsageLimitBatch(t, 2, 0, 1)

	if got := sandbox.attemptCount(); got != 3 {
		t.Fatalf("agent attempts = %d, want 3", got)
	}
	if len(waits) != 2 || waits[0] != 10*time.Minute || waits[1] != 10*time.Minute {
		t.Fatalf("await waits = %v, want [10m0s 10m0s]", waits)
	}
	commands := sandbox.commandsSnapshot()
	if !strings.Contains(commands[1], "--session 'usage-limit-session'") || !strings.Contains(commands[2], "--session 'usage-limit-session'") {
		t.Fatalf("commands = %q, want every poll to reuse the OpenCode session", commands)
	}
	if got := countEventsByType(log.snapshot(), "run.await"); got != 2 {
		t.Fatalf("run.await events = %d, want 2", got)
	}
}

func TestUsageLimitAwaitExcludesCustomOpenCodePreset(t *testing.T) {
	session := runSession{
		issueNumber: 42,
		agentCfg: config.Agent{
			Preset:  opencodeProvider,
			Command: "custom-opencode run",
		},
	}
	if session.shouldAwaitUsageLimit(AgentRunResult{UsageLimitReached: true}) {
		t.Fatal("custom OpenCode-preset command unexpectedly entered usage-limit waiting")
	}
}

func runUsageLimitBatch(t *testing.T, failures, idleTimeout, retries int) (*Result, *usageLimitRetrySandbox, *spyEventLog, []time.Duration) {
	t.Helper()
	return runUsageLimitBatchCase(t, failures, idleTimeout, retries, false, 0)
}

func runUsageLimitBatchCase(t *testing.T, failures, idleTimeout, retries int, finishOnSuccess bool, cancelOnAttempt int) (*Result, *usageLimitRetrySandbox, *spyEventLog, []time.Duration) {
	t.Helper()
	root := t.TempDir()
	t.Chdir(root)
	initGitRepo(t, root)

	const branch = "42-usage-limit"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sb := &usageLimitRetrySandbox{workDir: filepath.Join(root, "worktree"), failures: failures}
	if cancelOnAttempt > 0 {
		sb.onAttempt = func(attempt int) {
			if attempt == cancelOnAttempt {
				cancel()
			}
		}
	}
	log := &spyEventLog{}
	var waits []time.Duration
	client := &reviewWaitSchedulerGitHubClient{fakeGitHubClient: fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, Title: "Usage limit", State: "closed"}},
		prs:    map[string]*github.PR{branch: {Number: 7, State: "open", Body: "Closes #42", HeadRefName: branch}},
	}}
	// A continued session needs a merged PR to finish successfully. A timeout
	// test omits it so the ordinary retry reaches a fresh agent launch.
	if failures < 31 || finishOnSuccess {
		sb.onSuccess = func() { client.setPR(branch, func(pr *github.PR) { pr.State, pr.Merged = "merged", true }) }
	}
	clockNow := time.Now().UTC()
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
			now:                  func() time.Time { return clockNow },
			awaitWait: func(_ context.Context, delay time.Duration) error {
				waits = append(waits, delay)
				clockNow = clockNow.Add(delay)
				return nil
			},
		}),
	)
	result, err := o.RunBatch(ctx, Request{
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
	workDir   string
	failures  int
	onSuccess func()
	onAttempt func(int)

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
	if s.onAttempt != nil {
		s.onAttempt(attempt)
	}
	_, _ = io.WriteString(stdout, `{"type":"text","sessionID":"usage-limit-session","part":{"text":"working"}}`+"\n")
	if attempt <= s.failures {
		_, _ = io.WriteString(stderr, "Error: The usage limit has been reached\n")
		return errors.New("OpenCode usage limit")
	}
	if s.onSuccess != nil {
		s.onSuccess()
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
