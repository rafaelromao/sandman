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
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

func TestContextRollover(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)

	branch := "42-context-rollover"
	sb := &contextRolloverSandbox{workDir: filepath.Join(workDir, "worktree")}
	factory := &contextRolloverRunnableFactory{sandbox: sb}
	eventLog := &events.JSONLLogger{Path: filepath.Join(workDir, "events.jsonl")}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Recover context", State: "closed"},
		},
	}

	o := NewOrchestrator(
		client,
		&retryRenderer{result: "# Task\n\nInitial task."},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&contextRolloverSandboxFactory{sandbox: sb}),
		WithRunnableFactory(factory),
		WithRunSessionOpts(runSessionOptions{
			retryReset: func(context.Context, sandbox.Sandbox, string, string) error {
				return errors.New("context rollover must preserve the worktree")
			},
		}),
	)

	cfg := &config.Config{
		WorktreeDir: "worktrees",
		Git:         config.GitConfig{BaseBranch: "main"},
	}
	bc := BatchConfig{
		Cfg:              cfg,
		AgentName:        "opencode",
		AgentCfg:         config.BuiltInAgentPresets["opencode"].Agent("opencode"),
		IdentityResolver: noopIdentityResolver(),
		Retries:          1,
	}
	row := RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: branch},
		BaseBranch:  "main",
		RunTS:       "260814071754",
		RunShortID:  "5d21",
	}

	result, started := o.newRunExecutor(context.Background(), bc, &contextRolloverSandboxFactory{sandbox: sb}, nil).Execute(context.Background(), row)
	if !started {
		t.Fatalf("expected AgentRun to start, result=%+v", result)
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success after clean retry", result.Status)
	}
	if factory.created != 2 {
		t.Fatalf("runnable launches = %d, want 2", factory.created)
	}
	if !sb.secondAttemptStartedAfterFirstExit() {
		t.Fatal("clean retry started before the context-exhausted attempt exited")
	}
	if !strings.Contains(sb.secondTask, "Context Recovery Guard") {
		t.Fatalf("clean retry task lacks recovery guard:\n%s", sb.secondTask)
	}
	if !strings.Contains(sb.secondTask, "Initial task.") {
		t.Fatalf("clean retry task lost the original Task:\n%s", sb.secondTask)
	}
	if strings.Contains(sb.secondCommand, "--continue") {
		t.Fatalf("clean retry reused OpenCode continuation: %q", sb.secondCommand)
	}

	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	var retry events.Event
	var foundRetry bool
	var runIDs []string
	for _, event := range logs {
		runIDs = append(runIDs, event.RunID)
		if event.Type == "run.retry" {
			retry = event
			foundRetry = true
		}
	}
	if !foundRetry {
		t.Fatalf("expected run.retry event, got %v", logs)
	}
	if retry.Payload["reason"] != "context-exhausted" {
		t.Fatalf("retry reason = %v, want context-exhausted", retry.Payload["reason"])
	}
	wantRunID := buildRunID(42, row.RunTS, row.RunShortID)
	for _, runID := range runIDs {
		if runID != wantRunID {
			t.Fatalf("event RunID = %q, want %q", runID, wantRunID)
		}
	}
}

func TestContextRolloverDetector(t *testing.T) {
	base := time.Date(2026, time.August, 14, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		lines      []string
		times      []time.Duration
		additions  []string
		wantSignal bool
	}{
		{
			name: "two normalized built-in errors",
			lines: []string{
				"\x1b[31m[run-1] 12:00:00 Error: Input exceeds context window of this model\x1b[0m\r\n",
				"[run-1] 12:00:29 Error: context_length_exceeded\n",
			},
			times:      []time.Duration{0, 29 * time.Second},
			wantSignal: true,
		},
		{
			name: "one error",
			lines: []string{
				"[run-1] 12:00:00 Error: prompt is too long\n",
			},
			times:      []time.Duration{0},
			wantSignal: false,
		},
		{
			name: "rate limit excluded",
			lines: []string{
				"[run-1] 12:00:00 Error: rate limit exceeded\n",
				"[run-1] 12:00:01 Error: rate limit exceeded\n",
			},
			times:      []time.Duration{0, time.Second},
			wantSignal: false,
		},
		{
			name: "throttling excluded",
			lines: []string{
				"[run-1] 12:00:00 Error: Throttling error: try again\n",
				"[run-1] 12:00:01 Error: Throttling error: try again\n",
			},
			times:      []time.Duration{0, time.Second},
			wantSignal: false,
		},
		{
			name: "service unavailable excluded",
			lines: []string{
				"[run-1] 12:00:00 Error: Service unavailable: backend busy\n",
				"[run-1] 12:00:01 Error: Service unavailable: backend busy\n",
			},
			times:      []time.Duration{0, time.Second},
			wantSignal: false,
		},
		{
			name: "outside window",
			lines: []string{
				"[run-1] 12:00:00 Error: prompt is too long\n",
				"[run-1] 12:00:31 Error: prompt is too long\n",
			},
			times:      []time.Duration{0, 31 * time.Second},
			wantSignal: false,
		},
		{
			name: "additive literal",
			lines: []string{
				"[run-1] 12:00:00 Error: provider context exhausted\n",
				"[run-1] 12:00:01 Error: provider context exhausted\n",
			},
			times:      []time.Duration{0, time.Second},
			additions:  []string{"provider context exhausted"},
			wantSignal: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := base
			detector := newContextRolloverDetector(func() time.Time { return clock }, tt.additions, nil)
			for i, line := range tt.lines {
				clock = base.Add(tt.times[i])
				if _, err := detector.Write([]byte(line)); err != nil {
					t.Fatalf("detector.Write: %v", err)
				}
			}
			if got := detector.Triggered(); got != tt.wantSignal {
				t.Fatalf("Triggered() = %v, want %v", got, tt.wantSignal)
			}
		})
	}

	t.Run("sliding window keeps observations at the boundary", func(t *testing.T) {
		clock := base
		detector := newContextRolloverDetector(func() time.Time { return clock }, nil, nil)
		_, _ = detector.Write([]byte("Error: prompt is too long\n"))
		clock = base.Add(31 * time.Second)
		_, _ = detector.Write([]byte("Error: prompt is too long\n"))
		if detector.Triggered() {
			t.Fatal("observation older than 30 seconds triggered rollover")
		}
		clock = base.Add(60 * time.Second)
		_, _ = detector.Write([]byte("Error: prompt is too long\n"))
		if !detector.Triggered() {
			t.Fatal("observation exactly 30 seconds old was not retained")
		}
	})

	t.Run("partial carriage-return output is normalized", func(t *testing.T) {
		clock := base
		detector := newContextRolloverDetector(func() time.Time { return clock }, nil, nil)
		_, _ = detector.Write([]byte("[run-1] 12:00:00 Error: prompt is too long\r"))
		clock = base.Add(time.Second)
		_, _ = detector.Write([]byte("[run-1] 12:00:01 Error: prompt is too long\r"))
		detector.Flush()
		if !detector.Triggered() {
			t.Fatal("carriage-return-delimited errors did not trigger rollover")
		}
	})
}

type contextRolloverSandbox struct {
	mu                   sync.Mutex
	workDir              string
	started              int
	firstAttemptExited   bool
	secondTask           string
	secondCommand        string
	secondAttemptStarted bool
}

func (s *contextRolloverSandbox) Start(sandbox.SandboxStart) error {
	return os.MkdirAll(filepath.Join(s.workDir, ".sandman"), 0o755)
}

func (s *contextRolloverSandbox) Exec(ctx context.Context, command string, stdout, stderr io.Writer) error {
	s.mu.Lock()
	s.started++
	attempt := s.started
	if attempt == 2 {
		s.secondAttemptStarted = true
		s.secondCommand = command
		data, _ := os.ReadFile(filepath.Join(s.workDir, ".sandman", "task.md"))
		s.secondTask = string(data)
	}
	s.mu.Unlock()

	if attempt == 1 {
		_, _ = io.WriteString(stderr, "Error: prompt is too long\nError: prompt is too long\n")
		s.mu.Lock()
		s.firstAttemptExited = true
		s.mu.Unlock()
		return errors.New("simulated OpenCode context loop")
	}
	_, _ = io.WriteString(stdout, "recovered\n")
	return nil
}

func (s *contextRolloverSandbox) ExecInteractive(context.Context, string) error { return nil }
func (s *contextRolloverSandbox) Stop() error                                   { return nil }
func (s *contextRolloverSandbox) WorkDir() string                               { return s.workDir }
func (s *contextRolloverSandbox) RepoPath() string                              { return filepath.Dir(s.workDir) }
func (s *contextRolloverSandbox) Process() sandbox.Process                      { return nil }
func (s *contextRolloverSandbox) RestoreHostPaths() error                       { return nil }

func (s *contextRolloverSandbox) WritePrompt(content string) error {
	return os.WriteFile(filepath.Join(s.workDir, ".sandman", "task.md"), []byte(content), 0o644)
}

func (s *contextRolloverSandbox) secondAttemptStartedAfterFirstExit() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.firstAttemptExited && s.secondAttemptStarted
}

type contextRolloverSandboxFactory struct {
	sandbox *contextRolloverSandbox
}

func (f *contextRolloverSandboxFactory) NewSandbox(string, string, string, string, sandbox.Container) sandbox.Sandbox {
	return f.sandbox
}

type contextRolloverRunnableFactory struct {
	sandbox *contextRolloverSandbox
	created int
}

func (f *contextRolloverRunnableFactory) NewRunnable(issue *github.Issue, branch string, sb sandbox.Sandbox) Runnable {
	f.created++
	return NewAgentRun(issue, branch, sb)
}

var _ sandbox.Sandbox = (*contextRolloverSandbox)(nil)
var _ prompt.IssueRenderer = (*retryRenderer)(nil)
