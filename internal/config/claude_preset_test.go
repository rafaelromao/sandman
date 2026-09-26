package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBuiltInAgentPresets_ClaudeRunsUnmodifiedPrintMode(t *testing.T) {
	preset, ok := BuiltInAgentPresets["claude"]
	if !ok {
		t.Fatal("claude preset missing")
	}
	wantCommand := `claude -p --output-format stream-json --verbose{{if .ContinueFlag}} --continue{{end}}{{if .DangerouslySkipPermissions}} --dangerously-skip-permissions{{end}}{{if .SessionName}} --name '{{.SessionName}}'{{end}}{{if .ModelFlag}} {{.ModelFlag}}{{end}}{{if .VariantFlag}} {{.VariantFlag}}{{end}} "$(cat {{.PromptFile}})"`
	if preset.Command != wantCommand {
		t.Fatalf("claude command template:\n got %s\nwant %s", preset.Command, wantCommand)
	}
	for _, forbidden := range []string{"--bare", "--no-session-persistence", "--max-turns", "--max-budget-usd", "--session-id"} {
		if strings.Contains(preset.Command, forbidden) {
			t.Errorf("claude command template contains %s", forbidden)
		}
	}
	if preset.DisplayName != "Claude Code" || preset.DefaultModel != "sonnet" {
		t.Fatalf("display name/default model = %q/%q, want Claude Code/sonnet", preset.DisplayName, preset.DefaultModel)
	}
	wantEnv := map[string]string{
		"DISABLE_AUTOUPDATER":                      "1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		"IS_SANDBOX":                               "1",
	}
	if !reflect.DeepEqual(preset.Env, wantEnv) {
		t.Fatalf("claude env = %v, want %v", preset.Env, wantEnv)
	}
	if _, ok := preset.Env["CLAUDE_CONFIG_DIR"]; ok {
		t.Fatal("claude preset must not relocate the config dir: containers run with HOME=/")
	}
	if !reflect.DeepEqual(preset.ConfigDirs, []string{"~/.claude", "~/.agents"}) {
		t.Fatalf("claude config dirs = %v", preset.ConfigDirs)
	}
	if !reflect.DeepEqual(preset.ConfigFiles, []string{"~/.claude.json"}) {
		t.Fatalf("claude config files = %v", preset.ConfigFiles)
	}
	for _, excluded := range []string{"~/.claude/projects", "~/.claude/debug", "~/.claude/shell-snapshots", "~/.claude/todos"} {
		if !containsString(preset.SnapshotExcludes, excluded) {
			t.Errorf("claude snapshot excludes %v missing %s", preset.SnapshotExcludes, excluded)
		}
	}
	for _, kept := range []string{"~/.claude", "~/.claude/skills", "~/.claude/settings.json", "~/.claude/.credentials.json", "~/.claude/plugins"} {
		if containsString(preset.SnapshotExcludes, kept) {
			t.Errorf("claude snapshot excludes %s, which the agent needs", kept)
		}
	}
	if preset.KeychainAuth || len(preset.LiveMounts) != 0 {
		t.Fatalf("keychain auth/live mounts = %t/%v, want file-based auth and no live mounts", preset.KeychainAuth, preset.LiveMounts)
	}

	agent := preset.Agent("claude")
	if agent.OpencodePermissionMode != "" {
		t.Fatalf("claude agent permission mode = %q, want none", agent.OpencodePermissionMode)
	}
}

func TestDefaultModelForAgent(t *testing.T) {
	for agent, want := range map[string]string{
		"opencode":   DefaultModel,
		" claude ":   "sonnet",
		"custom":     "",
		"":           "",
		"claude-foo": "",
	} {
		if got := DefaultModelForAgent(agent); got != want {
			t.Errorf("DefaultModelForAgent(%q) = %q, want %q", agent, got, want)
		}
	}
	if DefaultModelForAgent(DefaultAgent) != DefaultModel {
		t.Fatal("the default agent's preset default must equal the default model")
	}
}

func TestLoad_ReviewModelDefaultFollowsReviewAgentPreset(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "claude review agent", content: "agent: claude\nreview_agent: claude\n", want: "sonnet"},
		{name: "custom agent on the claude preset", content: "agent: opencode\nreview_agent: reviewer\nagents:\n  reviewer:\n    preset: claude\n", want: "sonnet"},
		{name: "default review agent", content: "agent: claude\nmodel: opus\n", want: DefaultReviewModel},
		{name: "preset-less review agent keeps the historical default", content: "agent: opencode\nreview_agent: mine\nagents:\n  mine:\n    command: mine run\n", want: DefaultReviewModel},
		{name: "explicit review model wins", content: "agent: claude\nreview_agent: claude\nreview_model: opus\n", want: "opus"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tt.content), 0644); err != nil {
				t.Fatalf("write config: %v", err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.DefaultReviewModel != tt.want {
				t.Fatalf("review_model = %q, want %q", cfg.DefaultReviewModel, tt.want)
			}
		})
	}
}

func TestLoad_ClaudeDefaultAgentResolves(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("agent: claude\nmodel: sonnet\nreview_agent: claude\nreview_model: sonnet\n"), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	agent, err := cfg.ResolveAgentProvider(cfg.DefaultAgent)
	if err != nil {
		t.Fatalf("resolve claude: %v", err)
	}
	if agent.Preset != "claude" || agent.Command != BuiltInAgentPresets["claude"].Command {
		t.Fatalf("resolved claude agent = %+v", agent)
	}
	if _, ok := cfg.AgentProviders["claude"]; !ok {
		t.Fatal("claude missing from resolved agent providers")
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
