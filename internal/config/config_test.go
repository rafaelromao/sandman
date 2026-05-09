package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad_ValidConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `agent: codex
default_parallel: 3
worktree_dir: /tmp/wt
pr_template: .github/pull_request_template.md
sandbox: worktree
git:
  author_name: Dev
  author_email: dev@example.com
agents:
  codex:
    name: codex
    command: "codex --worktree {{.Worktree}}"
    env:
      API_KEY: secret
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Agent != "codex" {
		t.Errorf("agent: got %q, want %q", cfg.Agent, "codex")
	}
	if cfg.DefaultParallel != 3 {
		t.Errorf("default_parallel: got %d, want %d", cfg.DefaultParallel, 3)
	}
	if cfg.WorktreeDir != "/tmp/wt" {
		t.Errorf("worktree_dir: got %q, want %q", cfg.WorktreeDir, "/tmp/wt")
	}
	if cfg.PRTemplate != ".github/pull_request_template.md" {
		t.Errorf("pr_template: got %q, want %q", cfg.PRTemplate, ".github/pull_request_template.md")
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
	agent, ok := cfg.AgentProviders["codex"]
	if !ok {
		t.Fatalf("agents.codex: missing")
	}
	if agent.Name != "codex" {
		t.Errorf("agents.codex.name: got %q, want %q", agent.Name, "codex")
	}
	if agent.Command != "codex --worktree {{.Worktree}}" {
		t.Errorf("agents.codex.command: got %q, want %q", agent.Command, "codex --worktree {{.Worktree}}")
	}
	if agent.Env["API_KEY"] != "secret" {
		t.Errorf("agents.codex.env[API_KEY]: got %q, want %q", agent.Env["API_KEY"], "secret")
	}
}

func TestLoad_MissingOptionalFields_AppliesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `agent: codex
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.DefaultParallel != 1 {
		t.Errorf("default_parallel: got %d, want %d", cfg.DefaultParallel, 1)
	}
	if cfg.WorktreeDir != ".sandman/worktrees" {
		t.Errorf("worktree_dir: got %q, want %q", cfg.WorktreeDir, ".sandman/worktrees")
	}
	if cfg.Sandbox != "worktree" {
		t.Errorf("sandbox: got %q, want %q", cfg.Sandbox, "worktree")
	}
}

func TestLoad_NegativeDefaultParallel_DefaultsToOne(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `agent: codex
default_parallel: -2
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.DefaultParallel != 1 {
		t.Errorf("default_parallel: got %d, want %d", cfg.DefaultParallel, 1)
	}
}

func TestLoad_MissingFile_ReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent.yaml")

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !strings.Contains(err.Error(), "read config") {
		t.Errorf("error message should mention 'read config', got: %v", err)
	}
}

func TestLoad_MissingAgent_ReturnsValidationError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `default_parallel: 2
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected validation error for missing agent")
	}
	if !strings.Contains(err.Error(), "agent") {
		t.Errorf("error message should mention 'agent', got: %v", err)
	}
}

func TestLoad_InvalidYAML_ReturnsParseError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `agent: [invalid
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected parse error for invalid YAML")
	}
	if !strings.Contains(err.Error(), "parse config") {
		t.Errorf("error message should mention 'parse config', got: %v", err)
	}
}

func TestConfig_GetValue(t *testing.T) {
	cfg := &Config{
		Agent:           "codex",
		DefaultParallel: 4,
		WorktreeDir:     "/tmp/wt",
		PRTemplate:      ".github/pr.md",
		Sandbox:         "worktree",
		Git: GitConfig{
			AuthorName:  "Dev",
			AuthorEmail: "dev@example.com",
		},
	}

	tests := []struct {
		key     string
		want    string
		wantErr bool
	}{
		{"agent", "codex", false},
		{"default_parallel", "4", false},
		{"worktree_dir", "/tmp/wt", false},
		{"pr_template", ".github/pr.md", false},
		{"sandbox", "worktree", false},
		{"git.author_name", "Dev", false},
		{"git.author_email", "dev@example.com", false},
		{"unknown_key", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got, err := cfg.GetValue(tt.key)
			if (err != nil) != tt.wantErr {
				t.Fatalf("GetValue(%q) error = %v, wantErr %v", tt.key, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("GetValue(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestConfig_SetValue(t *testing.T) {
	cfg := &Config{Agent: "opencode"}

	tests := []struct {
		key     string
		value   string
		wantErr bool
	}{
		{"agent", "codex", false},
		{"default_parallel", "4", false},
		{"worktree_dir", "/tmp/wt", false},
		{"pr_template", ".github/pr.md", false},
		{"sandbox", "container", false},
		{"git.author_name", "Alice", false},
		{"git.author_email", "alice@example.com", false},
		{"unknown_key", "value", true},
		{"default_parallel", "not-a-number", true},
		{"default_parallel", "-1", true},
	}

	for _, tt := range tests {
		t.Run(tt.key+"_"+tt.value, func(t *testing.T) {
			// Work on a fresh copy for each test
			c := &Config{Agent: cfg.Agent}
			err := c.SetValue(tt.key, tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("SetValue(%q, %q) error = %v, wantErr %v", tt.key, tt.value, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			got, _ := c.GetValue(tt.key)
			if got != tt.value {
				t.Errorf("after SetValue(%q, %q), GetValue = %q, want %q", tt.key, tt.value, got, tt.value)
			}
		})
	}
}
