package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
)

func TestReviewCmd_NoArgsStartsDaemon(t *testing.T) {
	cfg := &config.Config{DefaultReviewAgent: "opencode"}
	deps := Dependencies{ConfigStore: &fakeStore{config: cfg}, RepoRoot: t.TempDir()}

	prev := reviewDaemonRunner
	reviewDaemonRunner = func(ctx context.Context, deps Dependencies, cfg *config.Config, sandbox string, cc int, ccSet bool, mc int, mcSet bool, agent string, model string, parallel int, parallelSet bool) error {
		return errReviewDaemonReached
	}
	defer func() { reviewDaemonRunner = prev }()

	cmd := NewReviewCmd(deps)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	if err := cmd.Execute(); err != errReviewDaemonReached {
		t.Fatalf("expected daemon runner to be reached, got %v", err)
	}
}

func TestReviewCmd_RejectsPositionalArgumentsBeforeLoadingConfig(t *testing.T) {
	cmd := NewReviewCmd(Dependencies{})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected positional arguments to be rejected")
	}
	if !strings.Contains(err.Error(), "unknown command") && !strings.Contains(err.Error(), "accepts 0 arg(s)") {
		t.Fatalf("expected a usage error for positional arguments, got %v", err)
	}
}

func TestReviewCmd_ForwardsDaemonOptions(t *testing.T) {
	cfg := &config.Config{DefaultReviewAgent: "opencode"}
	deps := Dependencies{ConfigStore: &fakeStore{config: cfg}, RepoRoot: t.TempDir()}

	var got struct {
		sandbox, agent, model     string
		cc, mc, parallel          int
		ccSet, mcSet, parallelSet bool
	}
	prev := reviewDaemonRunner
	reviewDaemonRunner = func(ctx context.Context, deps Dependencies, cfg *config.Config, sandbox string, cc int, ccSet bool, mc int, mcSet bool, agent string, model string, parallel int, parallelSet bool) error {
		got.sandbox, got.agent, got.model = sandbox, agent, model
		got.cc, got.mc, got.parallel = cc, mc, parallel
		got.ccSet, got.mcSet, got.parallelSet = ccSet, mcSet, parallelSet
		return nil
	}
	defer func() { reviewDaemonRunner = prev }()

	cmd := NewReviewCmd(deps)
	cmd.SetArgs([]string{"--sandbox", "worktree", "--container-capacity", "2", "--max-containers", "3", "--agent", "claude", "--model", "anthropic/claude", "--parallel", "4"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute daemon command: %v", err)
	}
	if got.sandbox != "worktree" || got.agent != "claude" || got.model != "anthropic/claude" {
		t.Fatalf("daemon options = sandbox=%q agent=%q model=%q", got.sandbox, got.agent, got.model)
	}
	if got.cc != 2 || !got.ccSet || got.mc != 3 || !got.mcSet || got.parallel != 4 || !got.parallelSet {
		t.Fatalf("daemon numeric options = cc=%d/%t mc=%d/%t parallel=%d/%t", got.cc, got.ccSet, got.mc, got.mcSet, got.parallel, got.parallelSet)
	}
}

func TestReviewCmd_RejectsNegativeDaemonLimits(t *testing.T) {
	for _, args := range [][]string{{"--parallel", "-1"}, {"--container-capacity", "-1"}, {"--max-containers", "-1"}} {
		cmd := NewReviewCmd(Dependencies{ConfigStore: &fakeStore{config: &config.Config{}}, RepoRoot: t.TempDir()})
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("expected validation error for %v", args)
		}
	}
}

var errReviewDaemonReached = &reviewDaemonReachedError{}

type reviewDaemonReachedError struct{}

func (*reviewDaemonReachedError) Error() string { return "daemon reached" }
