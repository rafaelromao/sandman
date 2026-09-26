package cmd

import (
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
)

func TestResolveModel(t *testing.T) {
	opencodeDefault := func(model string) *config.Config {
		return &config.Config{DefaultAgent: "opencode", DefaultModel: model}
	}
	claudeDefault := func(model string) *config.Config {
		return &config.Config{DefaultAgent: "claude", DefaultModel: model}
	}
	customDefault := &config.Config{
		DefaultAgent: "mine",
		DefaultModel: "openai/gpt-4.1",
		Agents:       map[string]config.Agent{"mine": {Command: "mine run"}},
	}
	tests := []struct {
		name  string
		model string
		cfg   *config.Config
		agent config.Agent
		want  string
	}{
		{name: "uses flag when provided", model: " gpt-4.1 ", cfg: opencodeDefault("openai/gpt-4.1"), agent: config.Agent{Preset: "opencode"}, want: "gpt-4.1"},
		{name: "falls back to default model for built-in preset", cfg: opencodeDefault(" openai/gpt-4.1 "), agent: config.Agent{Preset: "opencode"}, want: "openai/gpt-4.1"},
		{name: "returns empty for custom agent without model", cfg: opencodeDefault("openai/gpt-4.1"), agent: config.Agent{}, want: ""},
		{name: "returns empty when both are blank", cfg: opencodeDefault("  "), agent: config.Agent{Preset: "opencode"}, want: ""},
		{name: "global model does not cross to another preset", cfg: opencodeDefault("opencode/big-pickle"), agent: config.Agent{Preset: "claude"}, want: "sonnet"},
		{name: "claude default reaches opencode as its preset default", cfg: claudeDefault("opus"), agent: config.Agent{Preset: "opencode"}, want: config.DefaultModel},
		{name: "claude default forwards global model to claude", cfg: claudeDefault("opus"), agent: config.Agent{Preset: "claude"}, want: "opus"},
		{name: "cross-preset agent keeps its own model", cfg: opencodeDefault("opencode/big-pickle"), agent: config.Agent{Preset: "claude", Model: "haiku"}, want: ""},
		{name: "flag wins across presets", model: "opus", cfg: opencodeDefault("opencode/big-pickle"), agent: config.Agent{Preset: "claude"}, want: "opus"},
		{name: "preset-less default agent keeps global model", cfg: customDefault, agent: config.Agent{Preset: "opencode"}, want: "openai/gpt-4.1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveModel(tt.model, tt.cfg, tt.agent); got != tt.want {
				t.Fatalf("resolveModel() = %q, want %q", got, tt.want)
			}
		})
	}
}
