package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
)

// fakePRGitHubClient is a test double that satisfies the FetchPR
// and ListOpenPRs surface area used by the review command tests.
type fakePRGitHubClient struct {
	*fakeGitHubClient
	pr         *github.PR
	prErr      error
	openPRs    []github.PR
	openPRErr  error
	prByNumber map[int]*github.PR
}

func (f *fakePRGitHubClient) FetchPR(number int) (*github.PR, error) {
	if f.prByNumber != nil {
		if pr, ok := f.prByNumber[number]; ok {
			return pr, nil
		}
	}
	return f.pr, f.prErr
}

func (f *fakePRGitHubClient) ListOpenPRs() ([]github.PR, error) {
	return f.openPRs, f.openPRErr
}

func (f *fakePRGitHubClient) ListPRComments(number int) ([]github.PRComment, error) {
	return nil, nil
}

// spyBatchRunnerWithCapture records the batch.Request passed in.
type spyBatchRunnerWithCapture struct {
	spyBatchRunner
	captured batch.Request
}

func (s *spyBatchRunnerWithCapture) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
	s.captured = req
	return s.result, s.err
}

// spyBatchRunnerMultiCapture records all batch.Requests passed in.
type spyBatchRunnerMultiCapture struct {
	spyBatchRunner
	captured []batch.Request
}

func (s *spyBatchRunnerMultiCapture) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
	s.captured = append(s.captured, req)
	return s.result, s.err
}

func (s *spyBatchRunnerMultiCapture) requests() []batch.Request {
	return s.captured
}

func newReviewDeps(t *testing.T, gh github.Client, cfg *config.Config, runner batch.Runner) Dependencies {
	t.Helper()
	return Dependencies{
		BatchRunner:  runner,
		ConfigStore:  &fakeStore{config: cfg},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		Renderer:     &prompt.Engine{},
		IssuePicker:  &fakeIssuePicker{},
		IsTTY:        func() bool { return false },
	}
}

func TestReviewCmd_NoArgsStartsDaemon(t *testing.T) {
	var buf bytes.Buffer
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultModel:       "opencode/big-pickle",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		pr:               &github.PR{Number: 42, Title: "T", Body: "B"},
	}
	runner := &spyBatchRunner{result: &batch.Result{}}
	deps := newReviewDeps(t, gh, cfg, runner)

	prev := reviewDaemonRunner
	reviewDaemonRunner = func(ctx context.Context, deps Dependencies, cfg *config.Config, sandbox string, cc int, ccSet bool, mc int, mcSet bool) error {
		return fmt.Errorf("daemon reached")
	}
	defer func() { reviewDaemonRunner = prev }()

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error from injected daemon runner")
	}
	if !strings.Contains(err.Error(), "daemon reached") {
		t.Errorf("expected daemon branch to be reached, got: %v", err)
	}
}

func TestReviewCmd_DaemonModeCreatesReviewSock(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	var buf bytes.Buffer
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
	}
	runner := &spyBatchRunner{result: &batch.Result{}}
	deps := newReviewDeps(t, gh, cfg, runner)

	prev := reviewDaemonRunner
	reviewDaemonRunner = func(ctx context.Context, deps Dependencies, cfg *config.Config, sandbox string, cc int, ccSet bool, mc int, mcSet bool) error {
		if err := os.MkdirAll(".sandman/reviews", 0755); err != nil {
			return err
		}
		broadcaster := daemon.NewBroadcaster()
		sock := daemon.NewControlSocketWithName(".sandman/reviews", "review.sock", broadcaster)
		if err := sock.Start(); err != nil {
			return err
		}
		defer sock.Stop()
		<-ctx.Done()
		return nil
	}
	defer func() { reviewDaemonRunner = prev }()

	ctx, cancel := context.WithCancel(context.Background())
	cmd := NewReviewCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})

	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, ".sandman", "reviews", "review.sock")); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if _, err := os.Stat(filepath.Join(dir, ".sandman", "reviews", "review.sock")); err != nil {
		t.Fatalf("review.sock not created: %v", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cmd did not return after cancel")
	}
}

func TestReviewCmd_DaemonSocketAcceptsConnections(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: .git\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
	}
	runner := &spyBatchRunner{result: &batch.Result{}}
	deps := newReviewDeps(t, gh, cfg, runner)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- runReviewDaemon(ctx, deps, cfg, "", 0, false, 0, false) }()

	sockPath := filepath.Join(dir, ".sandman", "reviews", "review.sock")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("review.sock not created: %v", err)
	}

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("connect to review.sock: %v", err)
	}
	defer conn.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected error from runReviewDaemon: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runReviewDaemon did not return after cancel")
	}
}

func TestReviewCmd_OneShotRendersPromptAndInvokesBatch(t *testing.T) {
	var buf bytes.Buffer
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultModel:       "opencode/big-pickle",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "openai/gpt-5",
		Agent:              "opencode",
		Sandbox:            "podman",
		Agents:             map[string]config.Agent{},
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		pr: &github.PR{
			Number: 17,
			Title:  "Refactor daemon",
			Body:   "Splits the orchestrator.",
		},
	}
	runner := &spyBatchRunnerWithCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"17"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "repo=owner/repo agent=opencode model=openai/gpt-5") {
		t.Errorf("expected repo/agent/model info line, got %q", buf.String())
	}
	if len(runner.captured.Issues) != 0 {
		t.Errorf("expected empty Issues (prompt-only), got %v", runner.captured.Issues)
	}
	if runner.captured.PromptConfig.PromptFlag == "" {
		t.Fatal("expected rendered prompt in PromptConfig.PromptFlag")
	}
	want := []string{
		"Review pull request #17: Refactor daemon",
		"Splits the orchestrator.",
		"gh pr diff 17",
		"gh pr comment 17",
	}
	for _, w := range want {
		if !strings.Contains(runner.captured.PromptConfig.PromptFlag, w) {
			t.Errorf("rendered prompt missing %q\nprompt:\n%s", w, runner.captured.PromptConfig.PromptFlag)
		}
	}
	if runner.captured.Agent != "opencode" {
		t.Errorf("expected review agent 'opencode', got %q", runner.captured.Agent)
	}
	if runner.captured.Model != "openai/gpt-5" {
		t.Errorf("expected review model 'openai/gpt-5', got %q", runner.captured.Model)
	}
	if runner.captured.Sandbox != "podman" {
		t.Errorf("expected default sandbox from config 'podman', got %q", runner.captured.Sandbox)
	}
	if !runner.captured.Review {
		t.Errorf("expected Review=true on one-shot review batch request, got false")
	}
	if runner.captured.PRNumber != 17 {
		t.Errorf("expected PRNumber=17 on one-shot review batch request, got %d", runner.captured.PRNumber)
	}
	if runner.captured.ReviewFocus != "" {
		t.Errorf("expected empty ReviewFocus on one-shot review batch request, got %q", runner.captured.ReviewFocus)
	}
	if runner.captured.RunID != "PR17" {
		t.Errorf("expected RunID='PR17' on one-shot review batch request, got %q", runner.captured.RunID)
	}
	if !strings.Contains(runner.captured.RunDir, "PR17") {
		t.Errorf("expected RunDir to contain PR17, got %q", runner.captured.RunDir)
	}
	if runner.captured.Parallel != 4 {
		t.Errorf("expected default review parallel 4, got %d", runner.captured.Parallel)
	}
	if runner.captured.OutputWriter == nil {
		t.Error("expected OutputWriter to be set (non-nil) for one-shot review batch request")
	}
}

func TestReviewCmd_OneShotParallelFlagOverridesConfig(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:          "opencode",
		DefaultModel:          "opencode/big-pickle",
		DefaultReviewAgent:    "opencode",
		DefaultReviewModel:    "openai/gpt-5",
		DefaultReviewParallel: 4,
		Agent:                 "opencode",
		Sandbox:               "podman",
		Agents:                map[string]config.Agent{},
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		pr: &github.PR{
			Number: 17,
			Title:  "Refactor daemon",
			Body:   "Splits the orchestrator.",
		},
	}
	runner := &spyBatchRunnerWithCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"17", "--parallel", "7"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if runner.captured.Parallel != 7 {
		t.Fatalf("expected --parallel to set request parallel to 7, got %d", runner.captured.Parallel)
	}
}

func TestReviewCmd_AgentFlagOverridesReviewAgent(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultModel:       "opencode/big-pickle",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "openai/gpt-5",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		pr:               &github.PR{Number: 1, Title: "T", Body: "B"},
	}
	runner := &spyBatchRunnerWithCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"1", "--agent", "opencode"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if runner.captured.Agent != "opencode" {
		t.Errorf("expected --agent to specify review agent, got %q", runner.captured.Agent)
	}
}

func TestReviewCmd_ModelFlagOverridesReviewModel(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultModel:       "opencode/big-pickle",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		pr:               &github.PR{Number: 1, Title: "T", Body: "B"},
	}
	runner := &spyBatchRunnerWithCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"1", "--model", "openai/gpt-4.1"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if runner.captured.Model != "openai/gpt-4.1" {
		t.Errorf("expected --model to override review model, got %q", runner.captured.Model)
	}
}

func TestReviewCmd_InvalidContainerFlagsReturnError(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "container capacity less than one",
			args:    []string{"42", "--container-capacity", "-1"},
			wantErr: "container_capacity must be 0 or greater",
		},
		{
			name:    "negative max containers",
			args:    []string{"42", "--max-containers", "-1"},
			wantErr: "max_containers must be 0 or greater",
		},
		{
			name:    "container capacity negative in daemon mode",
			args:    []string{"--container-capacity", "-5"},
			wantErr: "container_capacity must be 0 or greater",
		},
		{
			name:    "negative parallel",
			args:    []string{"--parallel", "-3"},
			wantErr: "parallel must be 0 or greater",
		},
		{
			name:    "max containers negative in daemon mode",
			args:    []string{"--max-containers", "-5"},
			wantErr: "max_containers must be 0 or greater",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				DefaultAgent:       "opencode",
				DefaultReviewAgent: "opencode",
				DefaultReviewModel: "opencode/big-pickle",
				Agent:              "opencode",
				AgentProviders: map[string]config.Agent{
					"opencode": {Preset: "opencode", Command: "opencode"},
				},
			}
			gh := &fakePRGitHubClient{
				fakeGitHubClient: &fakeGitHubClient{},
				pr:               &github.PR{Number: 42, Title: "T", Body: "B"},
			}
			runner := &spyBatchRunner{result: &batch.Result{}}
			deps := newReviewDeps(t, gh, cfg, runner)

			prev := reviewDaemonRunner
			reviewDaemonRunner = func(ctx context.Context, deps Dependencies, cfg *config.Config, sandbox string, cc int, ccSet bool, mc int, mcSet bool) error {
				return nil
			}
			defer func() { reviewDaemonRunner = prev }()

			var buf bytes.Buffer
			cmd := NewReviewCmd(deps)
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			cmd.SetArgs(tt.args)

			err := cmd.Execute()
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
			var target *UsageError
			if !errors.As(err, &target) {
				t.Fatalf("expected *UsageError, got %T: %v", err, err)
			}
		})
	}
}

func TestReviewCmd_SandboxFlagDefaultsToConfig(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultModel:       "opencode/big-pickle",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		Sandbox:            "podman",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		pr:               &github.PR{Number: 1, Title: "T", Body: "B"},
	}
	runner := &spyBatchRunnerWithCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"1"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if runner.captured.Sandbox != "podman" {
		t.Errorf("expected review sandbox default from config 'podman', got %q", runner.captured.Sandbox)
	}
}

func TestReviewCmd_ContainerCapacityFlag(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultModel:       "opencode/big-pickle",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		pr:               &github.PR{Number: 1, Title: "T", Body: "B"},
	}
	runner := &spyBatchRunnerWithCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"1", "--container-capacity", "5"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if runner.captured.ContainerCapacity != 5 {
		t.Errorf("expected ContainerCapacity 5, got %d", runner.captured.ContainerCapacity)
	}
	if !runner.captured.ContainerCapacitySet {
		t.Errorf("expected ContainerCapacitySet=true")
	}
}

func TestReviewCmd_MaxContainersFlag(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultModel:       "opencode/big-pickle",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		pr:               &github.PR{Number: 1, Title: "T", Body: "B"},
	}
	runner := &spyBatchRunnerWithCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"1", "--max-containers", "3"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if runner.captured.MaxContainers != 3 {
		t.Errorf("expected MaxContainers 3, got %d", runner.captured.MaxContainers)
	}
	if !runner.captured.MaxContainersSet {
		t.Errorf("expected MaxContainersSet=true")
	}
}

func TestReviewCmd_SandboxFlagOverride(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultModel:       "opencode/big-pickle",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		pr:               &github.PR{Number: 1, Title: "T", Body: "B"},
	}
	runner := &spyBatchRunnerWithCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"1", "--sandbox", "podman"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if runner.captured.Sandbox != "podman" {
		t.Errorf("expected --sandbox override 'podman', got %q", runner.captured.Sandbox)
	}
}

func TestReviewCmd_DaemonFlagsCapture(t *testing.T) {
	var buf bytes.Buffer
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
	}
	runner := &spyBatchRunner{result: &batch.Result{}}
	deps := newReviewDeps(t, gh, cfg, runner)

	var (
		capturedSandbox string
		capturedCC      int
		capturedCCSet   bool
		capturedMC      int
		capturedMCSet   bool
	)
	prev := reviewDaemonRunner
	reviewDaemonRunner = func(ctx context.Context, deps Dependencies, cfg *config.Config, sandbox string, cc int, ccSet bool, mc int, mcSet bool) error {
		capturedSandbox = sandbox
		capturedCC = cc
		capturedCCSet = ccSet
		capturedMC = mc
		capturedMCSet = mcSet
		return nil
	}
	defer func() { reviewDaemonRunner = prev }()

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--sandbox", "podman", "--container-capacity", "5", "--max-containers", "3"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedSandbox != "podman" {
		t.Errorf("expected daemon to receive sandbox 'podman', got %q", capturedSandbox)
	}
	if capturedCC != 5 {
		t.Errorf("expected daemon to receive container-capacity 5, got %d", capturedCC)
	}
	if !capturedCCSet {
		t.Errorf("expected daemon to receive container-capacity-set=true")
	}
	if capturedMC != 3 {
		t.Errorf("expected daemon to receive max-containers 3, got %d", capturedMC)
	}
	if !capturedMCSet {
		t.Errorf("expected daemon to receive max-containers-set=true")
	}
}

func TestReviewCmd_DaemonParallelFlagOverridesConfig(t *testing.T) {
	var buf bytes.Buffer
	cfg := &config.Config{
		DefaultAgent:          "opencode",
		DefaultReviewAgent:    "opencode",
		DefaultReviewModel:    "opencode/big-pickle",
		DefaultReviewParallel: 4,
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
	}
	runner := &spyBatchRunner{result: &batch.Result{}}
	deps := newReviewDeps(t, gh, cfg, runner)

	var capturedParallel int
	prev := reviewDaemonRunner
	reviewDaemonRunner = func(ctx context.Context, deps Dependencies, cfg *config.Config, sandbox string, cc int, ccSet bool, mc int, mcSet bool) error {
		capturedParallel = cfg.DefaultReviewParallel
		return nil
	}
	defer func() { reviewDaemonRunner = prev }()

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--parallel", "8"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedParallel != 8 {
		t.Fatalf("expected daemon config parallel 8, got %d", capturedParallel)
	}
	if cfg.DefaultReviewParallel != 8 {
		t.Fatalf("expected loaded config to be overridden to 8, got %d", cfg.DefaultReviewParallel)
	}
}

func TestReviewCmd_ZeroContainerFlagsForwarded(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultModel:       "opencode/big-pickle",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		pr:               &github.PR{Number: 1, Title: "T", Body: "B"},
	}
	runner := &spyBatchRunnerWithCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	var buf bytes.Buffer
	cmd := NewReviewCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"1", "--container-capacity", "0", "--max-containers", "0"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !runner.captured.ContainerCapacitySet {
		t.Fatal("expected ContainerCapacitySet=true for --container-capacity=0")
	}
	if runner.captured.ContainerCapacity != 0 {
		t.Errorf("expected container_capacity=0, got %d", runner.captured.ContainerCapacity)
	}
	if !runner.captured.MaxContainersSet {
		t.Fatal("expected MaxContainersSet=true for --max-containers=0")
	}
	if runner.captured.MaxContainers != 0 {
		t.Errorf("expected max_containers=0, got %d", runner.captured.MaxContainers)
	}
}

func TestReviewCmd_FetchPRErrorBubblesUp(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		prErr:            &testError{msg: "gh pr view failed"},
	}
	runner := &spyBatchRunner{result: &batch.Result{}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"9"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when FetchPR fails")
	}
	if !strings.Contains(err.Error(), "fetch PR") {
		t.Errorf("error should mention fetch PR, got: %v", err)
	}
}

func TestReviewCmd_FallsBackToDefaultAgent(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultModel:       "opencode/big-pickle",
		DefaultReviewAgent: "",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		pr:               &github.PR{Number: 1, Title: "T", Body: "B"},
	}
	runner := &spyBatchRunnerWithCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"1"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if runner.captured.Agent != "opencode" {
		t.Errorf("expected fallback to default agent 'opencode', got %q", runner.captured.Agent)
	}
}

func TestReviewCmd_OneShotErrorsOnMissingModel(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultModel:       "",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		pr:               &github.PR{Number: 1, Title: "T", Body: "B"},
	}
	runner := &spyBatchRunner{result: &batch.Result{}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"1"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when review model is not set")
	}
	if !strings.Contains(err.Error(), "review model is not set") {
		t.Errorf("expected error about missing review model, got: %v", err)
	}
}

func TestReviewCmd_OneShotErrorsOnInvalidAgent(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "nonexistent-agent",
		DefaultReviewModel: "m",
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		pr:               &github.PR{Number: 1, Title: "T", Body: "B"},
	}
	runner := &spyBatchRunner{result: &batch.Result{}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"1"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for invalid review agent")
	}
	if !strings.Contains(err.Error(), "nonexistent-agent") {
		t.Errorf("expected error to mention agent name, got: %v", err)
	}
}

func TestReviewCmd_MultiplePRs(t *testing.T) {
	var buf bytes.Buffer
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		prByNumber: map[int]*github.PR{
			42: {Number: 42, Title: "PR 42", Body: "B"},
			43: {Number: 43, Title: "PR 43", Body: "B"},
		},
	}
	runner := &spyBatchRunnerMultiCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42", "43"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	reqs := runner.requests()
	if len(reqs) != 2 {
		t.Fatalf("expected 2 batch requests, got %d", len(reqs))
	}
	if reqs[0].PRNumber != 42 {
		t.Errorf("expected first request PRNumber=42, got %d", reqs[0].PRNumber)
	}
	if reqs[1].PRNumber != 43 {
		t.Errorf("expected second request PRNumber=43, got %d", reqs[1].PRNumber)
	}
}

func TestReviewCmd_RangeSyntax(t *testing.T) {
	var buf bytes.Buffer
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		prByNumber: map[int]*github.PR{
			42: {Number: 42, Title: "PR 42", Body: "B"},
			43: {Number: 43, Title: "PR 43", Body: "B"},
			44: {Number: 44, Title: "PR 44", Body: "B"},
			45: {Number: 45, Title: "PR 45", Body: "B"},
		},
	}
	runner := &spyBatchRunnerMultiCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42:45"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	reqs := runner.requests()
	if len(reqs) != 4 {
		t.Fatalf("expected 4 batch requests (42,43,44,45), got %d", len(reqs))
	}
	for i, n := range []int{42, 43, 44, 45} {
		if reqs[i].PRNumber != n {
			t.Errorf("request %d: expected PRNumber=%d, got %d", i, n, reqs[i].PRNumber)
		}
	}
}

func TestReviewCmd_UnboundedRangeEnd(t *testing.T) {
	var buf bytes.Buffer
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		prByNumber: map[int]*github.PR{
			100: {Number: 100, Title: "PR 100", Body: "B"},
			101: {Number: 101, Title: "PR 101", Body: "B"},
			102: {Number: 102, Title: "PR 102", Body: "B"},
		},
		openPRs: []github.PR{
			{Number: 100, Title: "PR 100", Body: "B"},
			{Number: 101, Title: "PR 101", Body: "B"},
			{Number: 102, Title: "PR 102", Body: "B"},
		},
	}
	runner := &spyBatchRunnerMultiCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"100:"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	reqs := runner.requests()
	if len(reqs) != 3 {
		t.Fatalf("expected 3 batch requests (100,101,102), got %d", len(reqs))
	}
	for i, n := range []int{100, 101, 102} {
		if reqs[i].PRNumber != n {
			t.Errorf("request %d: expected PRNumber=%d, got %d", i, n, reqs[i].PRNumber)
		}
	}
}

func TestReviewCmd_UnboundedRangeStart(t *testing.T) {
	var buf bytes.Buffer
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		prByNumber: map[int]*github.PR{
			2: {Number: 2, Title: "PR 2", Body: "B"},
			4: {Number: 4, Title: "PR 4", Body: "B"},
			5: {Number: 5, Title: "PR 5", Body: "B"},
		},
		openPRs: []github.PR{
			{Number: 2, Title: "PR 2", Body: "B"},
			{Number: 4, Title: "PR 4", Body: "B"},
			{Number: 5, Title: "PR 5", Body: "B"},
		},
	}
	runner := &spyBatchRunnerMultiCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{":5"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	reqs := runner.requests()
	if len(reqs) != 3 {
		t.Fatalf("expected 3 batch requests (2,4,5), got %d", len(reqs))
	}
	for i, n := range []int{2, 4, 5} {
		if reqs[i].PRNumber != n {
			t.Errorf("request %d: expected PRNumber=%d, got %d", i, n, reqs[i].PRNumber)
		}
	}
}

func TestReviewCmd_PRFlagRemoved(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
	}
	runner := &spyBatchRunner{result: &batch.Result{}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"--pr", "42"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when using removed --pr flag")
	}
	if !strings.Contains(err.Error(), "--pr") {
		t.Errorf("expected error mentioning --pr, got: %v", err)
	}
}

func TestReviewCmd_InvalidRangeError(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
	}
	runner := &spyBatchRunner{result: &batch.Result{}}
	deps := newReviewDeps(t, gh, cfg, runner)

	tests := []struct {
		name string
		args []string
	}{
		{"negative number", []string{"-1"}},
		{"bare colon", []string{":"}},
		{"reversed range", []string{"5:3"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := NewReviewCmd(deps)
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			cmd.SetArgs(tt.args)
			err := cmd.Execute()
			if err == nil {
				t.Error("expected error for invalid range, got nil")
			}
		})
	}
}

func TestReviewCmd_UnboundedRange_ListOpenPRError(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		prByNumber:       map[int]*github.PR{},
		openPRErr:        errors.New("boom"),
	}
	runner := &spyBatchRunner{result: &batch.Result{}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"100:"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when ListOpenPRs fails")
	}
	if !strings.Contains(err.Error(), "list open PRs: boom") {
		t.Errorf("expected error about list open PRs: boom, got: %v", err)
	}
}

func TestReviewCmd_MixedPlainAndUnboundedRange(t *testing.T) {
	var buf bytes.Buffer
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		prByNumber: map[int]*github.PR{
			42:  {Number: 42, Title: "PR 42", Body: "B"},
			7:   {Number: 7, Title: "PR 7", Body: "B"},
			100: {Number: 100, Title: "PR 100", Body: "B"},
			101: {Number: 101, Title: "PR 101", Body: "B"},
		},
		openPRs: []github.PR{
			{Number: 100, Title: "PR 100", Body: "B"},
			{Number: 101, Title: "PR 101", Body: "B"},
		},
	}
	runner := &spyBatchRunnerMultiCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42", "100:"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	reqs := runner.requests()
	if len(reqs) != 3 {
		t.Fatalf("expected 3 batch requests (42,100,101), got %d", len(reqs))
	}
	if reqs[0].PRNumber != 42 {
		t.Errorf("expected first request PRNumber=42, got %d", reqs[0].PRNumber)
	}
	if reqs[1].PRNumber != 100 {
		t.Errorf("expected second request PRNumber=100, got %d", reqs[1].PRNumber)
	}
	if reqs[2].PRNumber != 101 {
		t.Errorf("expected third request PRNumber=101, got %d", reqs[2].PRNumber)
	}
}

func TestReviewCmd_RangeTooLarge(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		prByNumber:       map[int]*github.PR{},
	}
	runner := &spyBatchRunner{result: &batch.Result{}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"1:1001"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for range > 1000")
	}
	if !strings.Contains(err.Error(), "more than 1000 pull requests") {
		t.Errorf("expected error about more than 1000 pull requests, got: %v", err)
	}
}

func TestReviewCmd_UnboundedRange_EmptyOpenPRs(t *testing.T) {
	var buf bytes.Buffer
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		prByNumber:       map[int]*github.PR{},
		openPRs:          []github.PR{},
	}
	runner := &spyBatchRunnerMultiCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"100:"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error for empty open PR list: %v", err)
	}

	reqs := runner.requests()
	if len(reqs) != 0 {
		t.Errorf("expected 0 batch requests for empty open PR list, got %d", len(reqs))
	}
}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }
