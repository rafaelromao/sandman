package cmd

import (
	"os"
	"path/filepath"
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
		{name: "minimal claude config uses the preset default", cfg: claudeDefault(""), agent: config.Agent{Preset: "claude"}, want: "sonnet"},
		{name: "minimal claude config keeps the agent's own model", cfg: claudeDefault(""), agent: config.Agent{Preset: "claude", Model: "haiku"}, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveModel(tt.model, tt.cfg, tt.agent); got != tt.want {
				t.Fatalf("resolveModel() = %q, want %q", got, tt.want)
			}
		})
	}
}

// A minimal hand-written config (only `agent:`) must still hand the claude
// preset its default model; OpenCode keeps leaving the choice to its CLI. The
// batch package's claude strategy tests cover rendering it as --model 'sonnet'.
func TestResolveModel_MinimalConfigUsesPresetDefault(t *testing.T) {
	for agentName, want := range map[string]string{"claude": "sonnet", "opencode": ""} {
		t.Run(agentName, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte("agent: "+agentName+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			agent, err := cfg.ResolveAgentProvider(cfg.DefaultAgent)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got := resolveModel("", cfg, agent); got != want {
				t.Fatalf("resolveModel() = %q, want %q", got, want)
			}
		})
	}
}
