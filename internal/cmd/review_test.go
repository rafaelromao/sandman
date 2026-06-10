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

// fakePRGitHubClient is a tiny test double that only satisfies the FetchPR
// surface area used by the review command tests.
type fakePRGitHubClient struct {
	*fakeGitHubClient
	pr    *github.PR
	prErr error
}

func (f *fakePRGitHubClient) FetchPR(number int) (*github.PR, error) {
	return f.pr, f.prErr
}

func (f *fakePRGitHubClient) ListOpenPRs() ([]github.PR, error) {
	return nil, nil
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

func newReviewDeps(t *testing.T, gh github.Client, cfg *config.Config, runner batch.Runner) Dependencies {
	t.Helper()
	return Dependencies{
		BatchRunner:    runner,
		ConfigStore:    &fakeStore{config: cfg},
		EventLog:       &fakeEventLog{},
		GitHubClient:   gh,
		PromptRenderer: &prompt.Engine{},
		IssuePicker:    &fakeIssuePicker{},
		IsTTY:          func() bool { return false },
	}
}

func TestReviewCmd_RequiresPRFlag(t *testing.T) {
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
		if err := os.MkdirAll(".sandman", 0755); err != nil {
			return err
		}
		broadcaster := daemon.NewBroadcaster()
		sock := daemon.NewControlSocketWithName(".sandman", "review.sock", broadcaster)
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
		if _, err := os.Stat(filepath.Join(dir, ".sandman", "review.sock")); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if _, err := os.Stat(filepath.Join(dir, ".sandman", "review.sock")); err != nil {
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

	sockPath := filepath.Join(dir, ".sandman", "review.sock")
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
		DefaultReviewAgent: "pi",
		DefaultReviewModel: "openai/gpt-5",
		Agent:              "opencode",
		Sandbox:            "podman",
		Agents:             map[string]config.Agent{},
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
			"pi":       {Preset: "pi", Command: "pi"},
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
	cmd.SetArgs([]string{"--pr", "17"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "repo=owner/repo agent=pi model=openai/gpt-5") {
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
	if runner.captured.Agent != "pi" {
		t.Errorf("expected review agent 'pi', got %q", runner.captured.Agent)
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
		t.Errorf("expected RunID=PR17, got %q", runner.captured.RunID)
	}
	if !strings.Contains(runner.captured.RunDir, "PR17") {
		t.Errorf("expected RunDir to contain PR17, got %q", runner.captured.RunDir)
	}
}

func TestReviewCmd_AgentFlagOverridesReviewAgent(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultModel:       "opencode/big-pickle",
		DefaultReviewAgent: "pi",
		DefaultReviewModel: "openai/gpt-5",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
			"pi":       {Preset: "pi", Command: "pi"},
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
	cmd.SetArgs([]string{"--pr", "1", "--agent", "opencode"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if runner.captured.Agent != "opencode" {
		t.Errorf("expected --agent to override review agent, got %q", runner.captured.Agent)
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
	cmd.SetArgs([]string{"--pr", "1", "--model", "openai/gpt-4.1"})

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
			args:    []string{"--container-capacity", "-1", "--pr", "42"},
			wantErr: "container_capacity must be 0 or greater",
		},
		{
			name:    "negative max containers",
			args:    []string{"--max-containers", "-1", "--pr", "42"},
			wantErr: "max_containers must be 0 or greater",
		},
		{
			name:    "container capacity negative in daemon mode",
			args:    []string{"--container-capacity", "-5"},
			wantErr: "container_capacity must be 0 or greater",
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
	cmd.SetArgs([]string{"--pr", "1"})

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
	cmd.SetArgs([]string{"--pr", "1", "--container-capacity", "5"})

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
	cmd.SetArgs([]string{"--pr", "1", "--max-containers", "3"})

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
	cmd.SetArgs([]string{"--pr", "1", "--sandbox", "podman"})

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
	cmd.SetArgs([]string{"--pr", "9"})

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
	cmd.SetArgs([]string{"--pr", "1"})

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
	cmd.SetArgs([]string{"--pr", "1"})

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
	cmd.SetArgs([]string{"--pr", "1"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for invalid review agent")
	}
	if !strings.Contains(err.Error(), "nonexistent-agent") {
		t.Errorf("expected error to mention agent name, got: %v", err)
	}
}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }
