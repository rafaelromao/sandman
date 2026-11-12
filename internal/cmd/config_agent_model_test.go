package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
)

func TestConfigSet_RefusesClaudeAgentModelOnAnotherAgentAndKeepsTheFile(t *testing.T) {
	cases := []struct {
		name     string
		original string
		args     []string
		want     string
	}{
		{
			name:     "model on an opencode-v2 config",
			original: "agent: opencode-v2\nmodel: openai/gpt-6-luna\n",
			args:     []string{"model", "claude-agent/sonnet"},
			want:     `model "claude-agent/sonnet" cannot run on agent "opencode-v2"`,
		},
		{
			name:     "agent away from claude while the model is claude-agent",
			original: "agent: claude\nmodel: claude-agent/sonnet\n",
			args:     []string{"agent", "opencode-v2"},
			want:     `model "claude-agent/sonnet" cannot run on agent "opencode-v2"`,
		},
		{
			name:     "review_agent away from claude while the review model is claude-agent",
			original: "agent: claude\nmodel: sonnet\nreview_agent: claude\nreview_model: claude-agent/opus\n",
			args:     []string{"review_agent", "opencode-v2"},
			want:     `review_model "claude-agent/opus" cannot run on agent "opencode-v2"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.original), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			cmd := NewConfigSetCmd(&config.FileStore{Path: path})
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(tc.args)

			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("config set %v: error %v, want it to mention %q", tc.args, err, tc.want)
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatalf("read config: %v", readErr)
			}
			if string(data) != tc.original {
				t.Fatalf("config.yaml changed to %q, want it unchanged", data)
			}
		})
	}
}

// The review model is checked as stored: clearing it hands the review agent
// its preset default, never the global model of another agent.
func TestConfigSet_ClearingReviewModelIsNotRefusedForTheGlobalModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := "agent: claude\nmodel: claude-agent/sonnet\nreview_agent: opencode\nreview_model: opencode/big-pickle\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cmd := NewConfigSetCmd(&config.FileStore{Path: path})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"review_model", ""})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("config set review_model \"\": %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if got := cfg.EffectiveReviewModel(); got != config.DefaultModelForAgent("opencode") {
		t.Fatalf("review model = %q, want the opencode preset default", got)
	}
}
