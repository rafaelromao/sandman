package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadConfigText(t *testing.T, content string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return Load(path)
}

func requireErrorMentions(t *testing.T, err error, wants ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error mentioning %q, got nil", wants)
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
}

// OpenCode v2's claude-agent provider starts the Claude Code CLI, which only
// the claude preset installs and keeps signed in, so the incident's config
// must fail to load instead of failing every launch.
func TestLoad_RefusesClaudeAgentModelOnOpenCodeV2(t *testing.T) {
	_, err := loadConfigText(t, "agent: opencode-v2\nmodel: claude-agent/sonnet\n")

	requireErrorMentions(t, err,
		`model "claude-agent/sonnet" cannot run on agent "opencode-v2"`,
		"the claude-agent provider runs the Claude Code CLI",
		`set agent to claude and model to "sonnet", or use a model of another provider`,
	)
}

func TestLoad_RefusesClaudeAgentModelsOnEveryPairingAndOtherAgent(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wants   []string
	}{
		{
			name:    "review_model on the review agent",
			content: "agent: claude\nmodel: sonnet\nreview_agent: opencode-v2\nreview_model: claude-agent/opus\n",
			wants: []string{
				`review_model "claude-agent/opus" cannot run on agent "opencode-v2"`,
				`set review_agent to claude and review_model to "opus"`,
			},
		},
		{
			name:    "review_model on the default review agent",
			content: "agent: claude\nmodel: sonnet\nreview_model: claude-agent/opus\n",
			wants:   []string{`review_model "claude-agent/opus" cannot run on agent "opencode"`},
		},
		{
			name:    "model on the default agent",
			content: "model: claude-agent/sonnet\n",
			wants:   []string{`model "claude-agent/sonnet" cannot run on agent "opencode"`},
		},
		{
			name:    "model on the opencode preset",
			content: "agent: opencode\nmodel: claude-agent/sonnet\n",
			wants: []string{
				`model "claude-agent/sonnet" cannot run on agent "opencode"`,
				`set agent to claude and model to "sonnet"`,
			},
		},
		{
			name:    "an agent's own model under a built-in name",
			content: "agent: claude\nmodel: sonnet\nagents:\n  opencode-v2:\n    model: claude-agent/sonnet\n",
			wants: []string{
				`agents.opencode-v2.model "claude-agent/sonnet" cannot run on agent "opencode-v2"`,
				`use a model of another provider, or the claude agent with model "sonnet"`,
			},
		},
		{
			name:    "an agent's own model under a custom name on the opencode-v2 preset",
			content: "agents:\n  mine:\n    preset: opencode-v2\n    model: claude-agent/sonnet\n",
			wants: []string{
				`agents.mine.model "claude-agent/sonnet" cannot run on agent "mine"`,
				`set agents.mine.preset to claude and agents.mine.model to "sonnet"`,
			},
		},
		{
			name:    "an agent without a preset",
			content: "agents:\n  mine:\n    command: my-agent run\n    model: claude-agent/sonnet\n",
			wants: []string{
				`agents.mine.model "claude-agent/sonnet" cannot run on agent "mine"`,
				`set agents.mine.preset to claude and agents.mine.model to "sonnet"`,
			},
		},
		{
			name:    "agents are checked in name order",
			content: "agents:\n  zed:\n    preset: opencode\n    model: claude-agent/opus\n  alpha:\n    preset: opencode\n    model: claude-agent/sonnet\n",
			wants:   []string{`agents.alpha.model "claude-agent/sonnet"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadConfigText(t, tc.content)
			requireErrorMentions(t, err, append([]string{"the claude-agent provider runs the Claude Code CLI"}, tc.wants...)...)
		})
	}
}

func TestLoad_AcceptsPairingsOutsideTheClaudeAgentRule(t *testing.T) {
	cases := map[string]string{
		"opencode-v2 with another provider": "agent: opencode-v2\nmodel: openai/gpt-6-luna\nreview_agent: opencode-v2\nreview_model: openai/gpt-6.1-sol\n",
		"claude with its own model":         "agent: claude\nmodel: sonnet\n",
		"a model naming no provider":        "agent: opencode\nmodel: gpt-4.1\n",
		"claude-agent models on the claude preset": "agent: claude\nmodel: claude-agent/sonnet\nreview_agent: claude\nreview_model: claude-agent/opus\n" +
			"agents:\n  mine:\n    preset: claude\n    model: claude-agent/sonnet\n",
		"an unused agent entry that does not resolve": "agent: opencode-v2\nmodel: openai/gpt-6-luna\nagents:\n  unused:\n    model: openai/gpt-6-luna\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadConfigText(t, content); err != nil {
				t.Fatalf("Load: %v", err)
			}
		})
	}
}

func TestBuiltInAgentPresets_OnlyClaudeRunsClaudeAgentModels(t *testing.T) {
	var owners []string
	for name, preset := range BuiltInAgentPresets {
		for _, provider := range preset.ExclusiveModelProviders {
			if provider == "claude-agent" {
				owners = append(owners, name)
			}
		}
	}
	if len(owners) != 1 || owners[0] != "claude" {
		t.Fatalf("presets reserving claude-agent models = %v, want only claude", owners)
	}
}
