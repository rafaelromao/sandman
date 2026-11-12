package review

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
)

// The review daemon refuses a claude-agent model on another agent at startup,
// naming the flags, before it scans or launches anything.
func TestDaemon_RunFailsFastOnClaudeAgentModelForAnotherAgent(t *testing.T) {
	cases := []struct {
		name   string
		config *config.Config
		agent  string
		model  string
		want   string
	}{
		{
			name:   "--model on a configured opencode-v2 reviewer",
			config: &config.Config{DefaultReviewAgent: "opencode-v2", DefaultReviewModel: "openai/gpt-6.1-sol"},
			model:  "claude-agent/opus",
			want:   `--model "claude-agent/opus" cannot run on agent "opencode-v2"`,
		},
		{
			name:   "--agent and --model away from a configured claude reviewer",
			config: &config.Config{DefaultReviewAgent: "claude", DefaultReviewModel: "claude-agent/opus"},
			agent:  "opencode-v2",
			model:  "claude-agent/opus",
			want:   `--model "claude-agent/opus" cannot run on agent "opencode-v2"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &capturedRequest{}
			d, _, _ := newDaemonForTest(t, &fakeGH{}, runner, tc.config)
			d.Agent, d.Model = tc.agent, tc.model
			d.Trigger = make(chan struct{}, 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- d.Run(ctx) }()

			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("Run error = %v, want it to mention %q", err, tc.want)
				}
				if !strings.Contains(err.Error(), `set --agent to claude and --model to "opus"`) {
					t.Errorf("Run error %q does not give the fix with the flags", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Run did not fail fast for a refused review pairing")
			}
			if runner.calls != 0 {
				t.Fatalf("review batches launched: %d, want none", runner.calls)
			}
		})
	}
}
