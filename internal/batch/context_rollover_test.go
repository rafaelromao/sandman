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
		{
			name: "no-body status forms",
			lines: []string{
				"Error: 400 no body\n",
				"Error: 413 no body\n",
			},
			times:      []time.Duration{0, time.Second},
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
		clock = base.Add(61 * time.Second)
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

	t.Run("generic request limits do not qualify", func(t *testing.T) {
		clock := base
		detector := newContextRolloverDetector(func() time.Time { return clock }, nil, nil)
		for i := 0; i < 2; i++ {
			_, _ = detector.Write([]byte("Error: exceeds the limit of 100 requests per minute\n"))
			clock = clock.Add(time.Second)
		}
		if detector.Triggered() {
			t.Fatal("generic request limit triggered rollover")
		}
	})

	t.Run("hyphenated service errors remain excluded", func(t *testing.T) {
		for _, message := range []string{
			"rate-limit exceeded: prompt is too long",
			"too-many-requests: context window exceeded",
			"toomanyrequests: context window exceeded",
			"throttle error: token limit exceeded",
			"throttled: token limit exceeded",
			"service-unavailable: prompt is too long",
		} {
			detector := newContextRolloverDetector(func() time.Time { return base }, nil, nil)
			for i := 0; i < 2; i++ {
				_, _ = detector.Write([]byte("Error: " + message + "\n"))
			}
			if detector.Triggered() {
				t.Fatalf("%q triggered rollover", message)
			}
		}
	})

	t.Run("side channel leaves output unchanged", func(t *testing.T) {
		input := "Error: prompt is too long\n"
		detector := newContextRolloverDetector(func() time.Time { return base }, nil, nil)
		var output strings.Builder
		writer := io.MultiWriter(&output, detector)
		if _, err := writer.Write([]byte(input)); err != nil {
			t.Fatalf("write through detector: %v", err)
		}
		if output.String() != input {
			t.Fatalf("side channel changed output: got %q, want %q", output.String(), input)
		}
	})
}

func TestContextRolloverStopsAttemptBeforeRetry(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)

	branch := "prompt-context-rollover"
	sb := &contextRolloverSandbox{
		workDir:             filepath.Join(workDir, "worktree"),
		waitForCancellation: true,
	}
	factory := &contextRolloverRunnableFactory{sandbox: sb}
	eventLog := &events.JSONLLogger{Path: filepath.Join(workDir, "events.jsonl")}
	o := NewOrchestrator(
		nil,
		&retryRenderer{result: "# Task\n\nInitial task."},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&contextRolloverSandboxFactory{sandbox: sb}),
		WithRunnableFactory(factory),
	)

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	bc := BatchConfig{
		Cfg:              cfg,
		AgentName:        "opencode",
		AgentCfg:         config.BuiltInAgentPresets["opencode"].Agent("opencode"),
		IdentityResolver: noopIdentityResolver(),
		Retries:          1,
	}
	row := RowSpec{
		Mode:              ModeFresh,
		Branches:          map[int]string{0: branch},
		BaseBranch:        "main",
		BatchID:           batchIDForPromptOnly("", "", "context-rollover", ""),
		RunID:             "context-rollover",
		UserProvidedRunID: "context-rollover",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, started := o.newRunExecutor(ctx, bc, &contextRolloverSandboxFactory{sandbox: sb}, nil).Execute(ctx, row)
	if !started {
		t.Fatalf("expected prompt-only AgentRun to start, result=%+v", result)
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success after retry", result.Status)
	}
	if factory.created != 2 {
		t.Fatalf("runnable launches = %d, want 2", factory.created)
	}
	if !sb.secondAttemptStartedAfterFirstExit() {
		t.Fatal("retry launched before the stopped attempt returned")
	}
}

func TestContextRolloverDetector_IgnoresNonOpenCodeAgents(t *testing.T) {
	sb := &contextRolloverSandbox{workDir: filepath.Join(t.TempDir(), "worktree")}
	if err := os.MkdirAll(filepath.Join(sb.workDir, ".sandman"), 0o755); err != nil {
		t.Fatalf("mkdir worktree task directory: %v", err)
	}
	run := NewAgentRun(nil, "custom-agent", sb)
	run.preset = "custom"
	run.status = "success"

	result := run.Run(context.Background(), &retryRenderer{result: "task"}, "custom-agent", prompt.RenderConfig{})
	if result.ContextExhausted {
		t.Fatal("non-OpenCode output was classified as context exhaustion")
	}
	if result.Status != "failure" {
		t.Fatalf("status = %q, want ordinary failure from the simulated command", result.Status)
	}
}

func TestContextRecoveryTaskWriteFailurePreservesExistingTask(t *testing.T) {
	workDir := filepath.Join(t.TempDir(), "worktree")
	taskPath := filepath.Join(workDir, ".sandman", "task.md")
	if err := os.MkdirAll(filepath.Dir(taskPath), 0o755); err != nil {
		t.Fatalf("mkdir task directory: %v", err)
	}
	const originalTask = "# Existing task\n\nContinue the work.\n"
	if err := os.WriteFile(taskPath, []byte(originalTask), 0o644); err != nil {
		t.Fatalf("write existing task: %v", err)
	}

	run := NewAgentRun(nil, "context-recovery", &fakeSandbox{workDir: workDir})
	run.taskWriter = func(string, []byte, os.FileMode) error {
		return errors.New("disk full")
	}

	run.preset = "custom"
	result := run.Run(context.Background(), nil, "true", prompt.RenderConfig{
		TaskPrompt:      "# Recovery task",
		ContextRecovery: true,
	})

	if result.Status != "success" {
		t.Fatalf("status = %q, want success after best-effort recovery write", result.Status)
	}
	got, err := os.ReadFile(taskPath)
	if err != nil {
		t.Fatalf("read existing task: %v", err)
	}
	if string(got) != originalTask {
		t.Fatalf("existing task changed after failed recovery write: %q", got)
	}
}

func TestContextRolloverRetry(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)

	branch := "42-context-retry"
	sb := &contextRolloverSandbox{
		workDir:                filepath.Join(workDir, "worktree"),
		alwaysContextExhausted: true,
	}
	factory := &contextRolloverRunnableFactory{sandbox: sb}
	eventLog := &events.JSONLLogger{Path: filepath.Join(workDir, "events.jsonl")}
	client := &fakeGitHubClient{issues: map[int]*github.Issue{
		42: {Number: 42, Title: "Retry context", State: "closed"},
	}}
	o := NewOrchestrator(
		client,
		&retryRenderer{result: "# Task\n\nInitial task."},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&contextRolloverSandboxFactory{sandbox: sb}),
		WithRunnableFactory(factory),
	)

	row := RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: branch},
		BaseBranch:  "main",
		RunTS:       "260814071754",
		RunShortID:  "retry",
	}
	bc := BatchConfig{
		Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentName:        "opencode",
		AgentCfg:         config.BuiltInAgentPresets["opencode"].Agent("opencode"),
		IdentityResolver: noopIdentityResolver(),
		Retries:          1,
	}

	result, started := o.newRunExecutor(context.Background(), bc, &contextRolloverSandboxFactory{sandbox: sb}, nil).Execute(context.Background(), row)
	if !started {
		t.Fatalf("expected AgentRun to start, result=%+v", result)
	}
	if result.Status != "failure" {
		t.Fatalf("status = %q, want failure after budget exhaustion", result.Status)
	}
	if result.RetriesTotal != 2 {
		t.Fatalf("RetriesTotal = %d, want 2 attempts", result.RetriesTotal)
	}
	if !result.ContextExhausted {
		t.Fatal("final context-exhausted attempt did not retain its cause")
	}
	if factory.created != 2 {
		t.Fatalf("runnable launches = %d, want 2", factory.created)
	}

	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	var retryEvent, finishedEvent *events.Event
	var types []string
	for i := range logs {
		event := &logs[i]
		if event.RunID != buildRunID(42, row.RunTS, row.RunShortID) {
			t.Fatalf("event RunID = %q, want stable issue RunID", event.RunID)
		}
		types = append(types, event.Type)
		switch event.Type {
		case "run.retry":
			retryEvent = event
		case "run.finished":
			finishedEvent = event
		}
	}
	if got, want := strings.Join(types, ","), "run.started,run.retry,run.finished"; got != want {
		t.Fatalf("event order = %s, want %s", got, want)
	}
	if retryEvent == nil || retryEvent.Payload["reason"] != "context-exhausted" {
		t.Fatalf("retry event = %+v, want context-exhausted", retryEvent)
	}
	if finishedEvent == nil || finishedEvent.Payload["context_exhausted"] != true {
		t.Fatalf("finished event = %+v, want context_exhausted=true", finishedEvent)
	}
	if finishedEvent.Payload["branch"] != branch {
		t.Fatalf("finished branch = %v, want %q", finishedEvent.Payload["branch"], branch)
	}
}

type contextRolloverSandbox struct {
	mu                     sync.Mutex
	workDir                string
	waitForCancellation    bool
	alwaysContextExhausted bool
	started                int
	firstAttemptExited     bool
	secondTask             string
	secondCommand          string
	secondAttemptStarted   bool
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

	if attempt == 1 || s.alwaysContextExhausted {
		_, _ = io.WriteString(stderr, "Error: prompt is too long\nError: prompt is too long\n")
		if s.waitForCancellation {
			<-ctx.Done()
		}
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
