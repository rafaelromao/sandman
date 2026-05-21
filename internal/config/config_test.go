package config

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestLoad_ValidConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `default_agent: pi
build_tools: go
review_command: /review please
default_parallel: 3
worktree_dir: /tmp/wt
sandbox: worktree
git:
  author_name: Dev
  author_email: dev@example.com
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.DefaultAgent != "pi" {
		t.Errorf("default_agent: got %q, want %q", cfg.DefaultAgent, "pi")
	}
	if cfg.Agent != cfg.DefaultAgent {
		t.Errorf("agent alias: got %q, want %q", cfg.Agent, cfg.DefaultAgent)
	}
	if cfg.BuildTools != "go" {
		t.Errorf("build_tools: got %q, want %q", cfg.BuildTools, "go")
	}
	if cfg.ReviewCommand != "/review please" {
		t.Errorf("review_command: got %q, want %q", cfg.ReviewCommand, "/review please")
	}
	if cfg.DefaultParallel != 3 {
		t.Errorf("default_parallel: got %d, want %d", cfg.DefaultParallel, 3)
	}
	if cfg.WorktreeDir != "/tmp/wt" {
		t.Errorf("worktree_dir: got %q, want %q", cfg.WorktreeDir, "/tmp/wt")
	}
	if cfg.Sandbox != "worktree" {
		t.Errorf("sandbox: got %q, want %q", cfg.Sandbox, "worktree")
	}
	if cfg.Git.AuthorName != "Dev" {
		t.Errorf("git.author_name: got %q, want %q", cfg.Git.AuthorName, "Dev")
	}
	if cfg.Git.AuthorEmail != "dev@example.com" {
		t.Errorf("git.author_email: got %q, want %q", cfg.Git.AuthorEmail, "dev@example.com")
	}
	if _, ok := cfg.AgentProviders["opencode"]; !ok {
		t.Fatal("expected built-in opencode agent in derived map")
	}
}

func TestLoad_DefaultAgentDefaultsToOpenCode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `git:
  default_branch: main
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.DefaultAgent != DefaultAgent {
		t.Fatalf("default_agent: got %q, want %q", cfg.DefaultAgent, DefaultAgent)
	}
}

func TestLoad_RejectsUnknownDefaultAgent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `default_agent: codex
git:
  default_branch: main
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "agent \"codex\" not found") {
		t.Fatalf("expected unknown agent error, got %v", err)
	}
}

func TestConfig_ResolveAgentProvider_BuiltInPreset(t *testing.T) {
	cfg := &Config{}

	agent, err := cfg.ResolveAgentProvider("opencode")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if agent.Preset != "opencode" {
		t.Errorf("preset: got %q, want %q", agent.Preset, "opencode")
	}
	wantCmd := `opencode run{{if .ModelFlag}} {{.ModelFlag}}{{end}} "$(cat {{.PromptFile}})"`
	if agent.Command != wantCmd {
		t.Errorf("command: got %q, want %q", agent.Command, wantCmd)
	}
	wantDirs := []string{"~/.config/opencode", "~/.local/share/opencode", "~/.claude"}
	if !reflect.DeepEqual(agent.ConfigDirs, wantDirs) {
		t.Errorf("config_dirs: got %v, want %v", agent.ConfigDirs, wantDirs)
	}
	if agent.KeychainAuth {
		t.Error("keychain_auth: expected false")
	}
}

func TestConfig_ResolveAgentProvider_Pi(t *testing.T) {
	cfg := &Config{}

	agent, err := cfg.ResolveAgentProvider("pi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if agent.Preset != "pi" {
		t.Fatalf("preset: got %q, want %q", agent.Preset, "pi")
	}
	if !strings.Contains(agent.Command, "--provider {{.ModelProvider}}") {
		t.Fatalf("expected pi command to use provider/model flags, got %q", agent.Command)
	}
}

func TestSplitPiModel(t *testing.T) {
	provider, model, err := SplitPiModel("openai/gpt-4.1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if provider != "openai" || model != "gpt-4.1" {
		t.Fatalf("unexpected split: %q / %q", provider, model)
	}
}

func TestSplitPiModel_InvalidFormat(t *testing.T) {
	_, _, err := SplitPiModel("gpt-4.1")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestConfig_GetAndSetDefaultAgent(t *testing.T) {
	cfg := &Config{DefaultAgent: DefaultAgent}

	if got, err := cfg.GetValue("default_agent"); err != nil || got != DefaultAgent {
		t.Fatalf("GetValue(default_agent) = %q, %v", got, err)
	}
	if err := cfg.SetValue("default_agent", "pi"); err != nil {
		t.Fatalf("SetValue(default_agent): %v", err)
	}
	if cfg.DefaultAgent != "pi" || cfg.Agent != "pi" {
		t.Fatalf("default_agent not updated: %#v", cfg)
	}
	if _, err := cfg.GetValue("agent"); err == nil {
		t.Fatal("expected old key to be rejected")
	}
	if err := cfg.SetValue("agent", "opencode"); err == nil {
		t.Fatal("expected old key to be rejected")
	}
}

func TestBuiltInPresets_AreOnlySupportedAgents(t *testing.T) {
	want := []string{"opencode", "pi"}
	got := make([]string, 0, len(BuiltInAgentPresets))
	for name := range BuiltInAgentPresets {
		got = append(got, name)
	}
	if !reflect.DeepEqual(sorted(got), want) {
		t.Fatalf("built-in presets mismatch: got %v, want %v", got, want)
	}
}

func sorted(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}
