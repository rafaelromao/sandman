package batch

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

func TestUsageLimitRetryWaitsBeforeRetryPreparation(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)

	const branch = "42-usage-limit"
	var order []string
	var orderMu sync.Mutex
	record := func(step string) {
		orderMu.Lock()
		defer orderMu.Unlock()
		order = append(order, step)
	}
	sb := &usageLimitRetrySandbox{
		workDir: filepath.Join(workDir, "worktree"),
		onExec: func(attempt int) {
			record("attempt-" + string(rune('0'+attempt)))
		},
	}
	previousRetryMarker := logRetryMarkerFn
	logRetryMarkerFn = func(logPath string, attempt, maxRetries int) error {
		record("marker")
		return previousRetryMarker(logPath, attempt, maxRetries)
	}
	t.Cleanup(func() { logRetryMarkerFn = previousRetryMarker })

	o := NewOrchestrator(
		&fakeGitHubClient{issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Usage limit", State: "closed"},
		}},
		&retryRenderer{result: "# Task\n\nRetry safely."},
		nil,
		&events.JSONLLogger{Path: filepath.Join(workDir, "events.jsonl")},
		WithErrorLog(io.Discard),
		WithSandboxFactory(&usageLimitRetrySandboxFactory{sandbox: sb}),
		WithRunnableFactory(&usageLimitRetryRunnableFactory{}),
		WithRunSessionOpts(runSessionOptions{
			usageLimitRetryWait: func(ctx context.Context, delay time.Duration) error {
				if delay != time.Second {
					t.Errorf("retry delay = %s, want 1s", delay)
				}
				record("wait")
				return nil
			},
		}),
	)

	result, started := o.newRunExecutor(context.Background(), BatchConfig{
		Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentName:        "opencode",
		AgentCfg:         config.BuiltInAgentPresets["opencode"].Agent("opencode"),
		IdentityResolver: noopIdentityResolver(),
		Retries:          1,
		RunIdleTimeout:   1,
	}, &usageLimitRetrySandboxFactory{sandbox: sb}, nil).Execute(context.Background(), RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: branch},
		BaseBranch:  "main",
		RunTS:       "260906151914",
		RunShortID:  "limit",
	})
	if !started {
		t.Fatalf("expected run to start, result=%+v", result)
	}
	orderMu.Lock()
	gotOrder := append([]string(nil), order...)
	orderMu.Unlock()
	wantOrder := []string{"attempt-1", "wait", "marker", "attempt-2"}
	if len(gotOrder) != len(wantOrder) {
		t.Fatalf("order = %v, want %v", gotOrder, wantOrder)
	}
	for i, want := range wantOrder {
		if gotOrder[i] != want {
			t.Fatalf("order = %v, want %v", gotOrder, wantOrder)
		}
	}
	if result.Status != "failure" {
		t.Fatalf("status = %q, want failure without a merged pull request", result.Status)
	}
}

func TestUsageLimitRetryPromptOnlyWaits(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)

	var order []string
	var orderMu sync.Mutex
	record := func(step string) {
		orderMu.Lock()
		defer orderMu.Unlock()
		order = append(order, step)
	}
	sb := &usageLimitRetrySandbox{
		workDir: filepath.Join(workDir, "worktree"),
		onExec: func(attempt int) {
			record("attempt-" + string(rune('0'+attempt)))
		},
	}
	o := NewOrchestrator(
		nil,
		&retryRenderer{result: "# Task\n\nRetry safely."},
		nil,
		&events.JSONLLogger{Path: filepath.Join(workDir, "events.jsonl")},
		WithErrorLog(io.Discard),
		WithSandboxFactory(&usageLimitRetrySandboxFactory{sandbox: sb}),
		WithRunnableFactory(&usageLimitRetryRunnableFactory{}),
		WithRunSessionOpts(runSessionOptions{
			retryReset: func(context.Context, sandbox.Sandbox, string, string) error { return nil },
			usageLimitRetryWait: func(ctx context.Context, delay time.Duration) error {
				if delay != 2*time.Second {
					t.Errorf("retry delay = %s, want 2s", delay)
				}
				record("wait")
				return nil
			},
		}),
	)

	result, started := o.newRunExecutor(context.Background(), BatchConfig{
		Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentName:        "opencode",
		AgentCfg:         config.BuiltInAgentPresets["opencode"].Agent("opencode"),
		IdentityResolver: noopIdentityResolver(),
		Retries:          1,
		RunIdleTimeout:   2,
	}, &usageLimitRetrySandboxFactory{sandbox: sb}, nil).Execute(context.Background(), RowSpec{
		Mode:              ModeFresh,
		Branches:          map[int]string{0: "usage-limit-prompt"},
		BaseBranch:        "main",
		BatchID:           "usage-limit-prompt",
		RunID:             "usage-limit-prompt",
		UserProvidedRunID: "usage-limit-prompt",
	})
	if !started {
		t.Fatalf("expected prompt-only run to start, result=%+v", result)
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success", result.Status)
	}

	orderMu.Lock()
	gotOrder := append([]string(nil), order...)
	orderMu.Unlock()
	wantOrder := []string{"attempt-1", "wait", "attempt-2"}
	if len(gotOrder) != len(wantOrder) {
		t.Fatalf("order = %v, want %v", gotOrder, wantOrder)
	}
	for i, want := range wantOrder {
		if gotOrder[i] != want {
			t.Fatalf("order = %v, want %v", gotOrder, wantOrder)
		}
	}
}

func TestUsageLimitRetryCancellationPreventsRetry(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)

	const branch = "42-usage-limit-cancel"
	sb := &usageLimitRetrySandbox{workDir: filepath.Join(workDir, "worktree")}
	eventLog := &events.JSONLLogger{Path: filepath.Join(workDir, "events.jsonl")}
	waitStarted := make(chan struct{})
	previousRetryMarker := logRetryMarkerFn
	markerCalls := 0
	logRetryMarkerFn = func(string, int, int) error {
		markerCalls++
		return nil
	}
	t.Cleanup(func() { logRetryMarkerFn = previousRetryMarker })
	o := NewOrchestrator(
		&fakeGitHubClient{issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Usage limit", State: "closed"},
		}},
		&retryRenderer{result: "# Task\n\nRetry safely."},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&usageLimitRetrySandboxFactory{sandbox: sb}),
		WithRunnableFactory(&usageLimitRetryRunnableFactory{}),
		WithRunSessionOpts(runSessionOptions{
			usageLimitRetryWait: func(ctx context.Context, _ time.Duration) error {
				close(waitStarted)
				<-ctx.Done()
				return ctx.Err()
			},
		}),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var result AgentRunResult
	var started bool
	go func() {
		defer close(done)
		result, started = o.newRunExecutor(ctx, BatchConfig{
			Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
			AgentName:        "opencode",
			AgentCfg:         config.BuiltInAgentPresets["opencode"].Agent("opencode"),
			IdentityResolver: noopIdentityResolver(),
			Retries:          1,
			RunIdleTimeout:   1,
		}, &usageLimitRetrySandboxFactory{sandbox: sb}, nil).Execute(ctx, RowSpec{
			IssueNumber: 42,
			Branches:    map[int]string{42: branch},
			BaseBranch:  "main",
			RunTS:       "260906151914",
			RunShortID:  "cancel",
		})
	}()

	<-waitStarted
	cancel()
	<-done
	if !started {
		t.Fatalf("expected run to start, result=%+v", result)
	}
	if result.Status != "aborted" {
		t.Fatalf("status = %q, want aborted", result.Status)
	}
	if sb.attemptCount() != 1 {
		t.Fatalf("agent attempts = %d, want 1", sb.attemptCount())
	}
	if markerCalls != 0 {
		t.Fatalf("retry marker calls = %d, want 0", markerCalls)
	}

	logged, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	for _, event := range logged {
		if event.Type == "run.retry" {
			t.Fatalf("unexpected retry event after cooldown cancellation: %+v", logged)
		}
	}
}

func TestUsageLimitRetryWaitsForEachQualifyingFailure(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)

	sb := &usageLimitRetrySandbox{
		workDir:  filepath.Join(workDir, "worktree"),
		failures: 2,
	}
	waits := 0
	o := NewOrchestrator(
		&fakeGitHubClient{issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Usage limit", State: "closed"},
		}},
		&retryRenderer{result: "# Task\n\nRetry safely."},
		nil,
		&events.JSONLLogger{Path: filepath.Join(workDir, "events.jsonl")},
		WithErrorLog(io.Discard),
		WithSandboxFactory(&usageLimitRetrySandboxFactory{sandbox: sb}),
		WithRunnableFactory(&usageLimitRetryRunnableFactory{}),
		WithRunSessionOpts(runSessionOptions{
			usageLimitRetryWait: func(_ context.Context, delay time.Duration) error {
				if delay != 3*time.Second {
					t.Errorf("retry delay = %s, want 3s", delay)
				}
				waits++
				return nil
			},
		}),
	)

	_, started := o.newRunExecutor(context.Background(), BatchConfig{
		Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentName:        "opencode",
		AgentCfg:         config.BuiltInAgentPresets["opencode"].Agent("opencode"),
		IdentityResolver: noopIdentityResolver(),
		Retries:          2,
		RunIdleTimeout:   3,
	}, &usageLimitRetrySandboxFactory{sandbox: sb}, nil).Execute(context.Background(), RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: "42-usage-limit-repeat"},
		BaseBranch:  "main",
		RunTS:       "260906151914",
		RunShortID:  "repeat",
	})
	if !started {
		t.Fatal("expected run to start")
	}
	if waits != 2 {
		t.Fatalf("cooldown waits = %d, want 2", waits)
	}
	if sb.attemptCount() != 3 {
		t.Fatalf("agent attempts = %d, want 3", sb.attemptCount())
	}
}

func TestUsageLimitRetryZeroIdleTimeoutSkipsCooldown(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)

	sb := &usageLimitRetrySandbox{workDir: filepath.Join(workDir, "worktree")}
	o := NewOrchestrator(
		&fakeGitHubClient{issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Usage limit", State: "closed"},
		}},
		&retryRenderer{result: "# Task\n\nRetry safely."},
		nil,
		&events.JSONLLogger{Path: filepath.Join(workDir, "events.jsonl")},
		WithErrorLog(io.Discard),
		WithSandboxFactory(&usageLimitRetrySandboxFactory{sandbox: sb}),
		WithRunnableFactory(&usageLimitRetryRunnableFactory{}),
		WithRunSessionOpts(runSessionOptions{
			usageLimitRetryWait: func(context.Context, time.Duration) error {
				t.Fatal("zero idle timeout invoked cooldown")
				return nil
			},
		}),
	)

	_, started := o.newRunExecutor(context.Background(), BatchConfig{
		Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentName:        "opencode",
		AgentCfg:         config.BuiltInAgentPresets["opencode"].Agent("opencode"),
		IdentityResolver: noopIdentityResolver(),
		Retries:          1,
	}, &usageLimitRetrySandboxFactory{sandbox: sb}, nil).Execute(context.Background(), RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: "42-usage-limit-zero"},
		BaseBranch:  "main",
		RunTS:       "260906151914",
		RunShortID:  "zero",
	})
	if !started {
		t.Fatal("expected run to start")
	}
	if sb.attemptCount() != 2 {
		t.Fatalf("agent attempts = %d, want 2", sb.attemptCount())
	}
}

type usageLimitRetrySandbox struct {
	workDir  string
	onExec   func(int)
	failures int

	mu       sync.Mutex
	attempts int
}

func (s *usageLimitRetrySandbox) Start(sandbox.SandboxStart) error {
	return os.MkdirAll(filepath.Join(s.workDir, ".sandman"), 0o755)
}

func (s *usageLimitRetrySandbox) Exec(_ context.Context, _ string, _ io.Writer, stderr io.Writer) error {
	s.mu.Lock()
	s.attempts++
	attempt := s.attempts
	s.mu.Unlock()
	if s.onExec != nil {
		s.onExec(attempt)
	}
	failures := s.failures
	if failures == 0 {
		failures = 1
	}
	if attempt <= failures {
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
	path := filepath.Join(s.workDir, ".sandman", "task.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func (s *usageLimitRetrySandbox) attemptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
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
