package review

import (
	"context"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
)

// startupLog runs the daemon just far enough to start and returns its log.
func startupLog(t *testing.T, cfg *config.Config) string {
	t.Helper()
	d, logs, _ := newDaemonForTest(t, &fakeGH{authenticatedLogin: "sandman"}, &capturedRequest{}, cfg)
	d.Trigger = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return logs.String()
}

func reviewerConfig(reviewer string, agent config.Agent) *config.Config {
	return &config.Config{
		DefaultReviewAgent: reviewer,
		DefaultReviewModel: "m",
		Agents:             map[string]config.Agent{reviewer: agent},
	}
}

func TestDaemon_WarnsAtStartWhenTheReviewAgentRunsACustomCommand(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cfg   *config.Config
		lost  []string
		quiet bool
	}{
		{name: "opencode-v2", cfg: reviewerConfig("opencode-v2", config.Agent{Command: "opencode run"}),
			lost: []string{"model or variant flags", "session", "parse its output", "usage limits"}},
		{name: "aliased claude", cfg: reviewerConfig("reviewer", config.Agent{Preset: "claude", Command: "claude -p"}),
			lost: []string{"credentials", "permission_mode"}},
		{name: "built-in", cfg: reviewerConfig("opencode-v2", config.Agent{}), quiet: true},
		{name: "former built-in", cfg: reviewerConfig("opencode-v2", config.Agent{Command: config.BuiltInAgentPresets["opencode-v2"].FormerCommands[0]}), quiet: true},
		{name: "provider without a preset", cfg: reviewerConfig("mine", config.Agent{Command: "mine"}), quiet: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := startupLog(t, tc.cfg)

			warnings := strings.Count(log, "runs a custom command")
			if tc.quiet {
				if warnings != 0 {
					t.Fatalf("unexpected warning:\n%s", log)
				}
				return
			}
			if warnings != 1 {
				t.Fatalf("warnings = %d, want 1:\n%s", warnings, log)
			}
			for _, want := range tc.lost {
				if !strings.Contains(log, want) {
					t.Errorf("warning does not mention %q:\n%s", want, log)
				}
			}
		})
	}
}
