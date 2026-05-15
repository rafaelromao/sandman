package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoad_ValidConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `agent: codex
build_tools: go
default_parallel: 3
worktree_dir: /tmp/wt
sandbox: worktree
git:
  author_name: Dev
  author_email: dev@example.com
agents:
  codex:
    preset: codex
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
	if cfg.BuildTools != "go" {
		t.Errorf("build_tools: got %q, want %q", cfg.BuildTools, "go")
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
	agent, ok := cfg.AgentProviders["codex"]
	if !ok {
		t.Fatalf("agents.codex: missing")
	}
	if agent.Preset != "codex" {
		t.Errorf("agents.codex.preset: got %q, want %q", agent.Preset, "codex")
	}
	if agent.Command != "" {
		t.Errorf("agents.codex.command: got %q, want empty", agent.Command)
	}
	if len(agent.ConfigDirs) != 0 {
		t.Errorf("agents.codex.config_dirs: got %v, want empty", agent.ConfigDirs)
	}
	if agent.Env["API_KEY"] != "secret" {
		t.Errorf("agents.codex.env[API_KEY]: got %q, want %q", agent.Env["API_KEY"], "secret")
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
	wantCmd := `opencode run "$(cat {{.PromptFile}})"`
	if agent.Command != wantCmd {
		t.Errorf("command: got %q, want %q", agent.Command, wantCmd)
	}
	wantDirs := []string{"~/.config/opencode", "~/.local/share/opencode"}
	if !reflect.DeepEqual(agent.ConfigDirs, wantDirs) {
		t.Errorf("config_dirs: got %v, want %v", agent.ConfigDirs, wantDirs)
	}
	if agent.KeychainAuth {
		t.Error("keychain_auth: expected false")
	}
}

func TestConfig_ResolveAgentProvider_CustomProvider(t *testing.T) {
	cfg := &Config{AgentProviders: map[string]Agent{
		"custom": {
			Command: "custom --prompt {{.PromptFile}}",
		},
	}}

	agent, err := cfg.ResolveAgentProvider("custom")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if agent.Preset != "" {
		t.Errorf("preset: got %q, want empty", agent.Preset)
	}
	if agent.Command != "custom --prompt {{.PromptFile}}" {
		t.Errorf("command: got %q, want %q", agent.Command, "custom --prompt {{.PromptFile}}")
	}
}

func TestBuiltInPresets_Metadata(t *testing.T) {
	tests := []struct {
		key          string
		wantDisplay  string
		wantCommand  string
		wantDirs     []string
		wantFiles    []string
		wantKeychain bool
	}{
		{
			key:          "opencode",
			wantDisplay:  "OpenCode",
			wantCommand:  `opencode run "$(cat {{.PromptFile}})"`,
			wantDirs:     []string{"~/.config/opencode", "~/.local/share/opencode"},
			wantFiles:    nil,
			wantKeychain: false,
		},
		{
			key:          "claude-code",
			wantDisplay:  "Claude Code",
			wantCommand:  `claude --print "$(cat {{.PromptFile}})"`,
			wantDirs:     []string{"~/.claude"},
			wantFiles:    []string{"~/.claude.json"},
			wantKeychain: false,
		},
		{
			key:          "codex",
			wantDisplay:  "Codex",
			wantCommand:  `codex exec "$(cat {{.PromptFile}})"`,
			wantDirs:     []string{"~/.config/codex", "~/.local/share/codex"},
			wantFiles:    nil,
			wantKeychain: false,
		},
		{
			key:          "pi",
			wantDisplay:  "Pi",
			wantCommand:  `pi --print "$(cat {{.PromptFile}})"`,
			wantDirs:     []string{"~/.pi"},
			wantFiles:    nil,
			wantKeychain: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			preset, ok := BuiltInAgentPresets[tt.key]
			if !ok {
				t.Fatalf("missing built-in preset %q", tt.key)
			}
			if preset.DisplayName != tt.wantDisplay {
				t.Errorf("DisplayName: got %q, want %q", preset.DisplayName, tt.wantDisplay)
			}
			if preset.Command != tt.wantCommand {
				t.Errorf("Command: got %q, want %q", preset.Command, tt.wantCommand)
			}
			if !reflect.DeepEqual(preset.ConfigDirs, tt.wantDirs) {
				t.Errorf("ConfigDirs: got %v, want %v", preset.ConfigDirs, tt.wantDirs)
			}
			if !reflect.DeepEqual(preset.ConfigFiles, tt.wantFiles) {
				t.Errorf("ConfigFiles: got %v, want %v", preset.ConfigFiles, tt.wantFiles)
			}
			if preset.KeychainAuth != tt.wantKeychain {
				t.Errorf("KeychainAuth: got %v, want %v", preset.KeychainAuth, tt.wantKeychain)
			}
		})
	}
}

func TestAgent_ConfigFilesCopiedFromPreset(t *testing.T) {
	preset := AgentPreset{
		DisplayName: "test",
		Command:     "test",
		ConfigDirs:  []string{"~/.config/test"},
		ConfigFiles: []string{"~/.config/test.json"},
	}

	agent := preset.Agent("test")
	if len(agent.ConfigFiles) != 1 || agent.ConfigFiles[0] != "~/.config/test.json" {
		t.Errorf("ConfigFiles: got %v, want [\"~/.config/test.json\"]", agent.ConfigFiles)
	}
}

func TestAgentWithOverrides_ConfigFilesOverridden(t *testing.T) {
	preset := AgentPreset{
		DisplayName: "test",
		Command:     "test",
		ConfigDirs:  []string{"~/.config/test"},
		ConfigFiles: []string{"~/.config/test.json"},
	}

	override := Agent{
		ConfigFiles: []string{"~/.custom.json"},
	}

	agent := preset.AgentWithOverrides("test", override)
	if len(agent.ConfigFiles) != 1 || agent.ConfigFiles[0] != "~/.custom.json" {
		t.Errorf("ConfigFiles: got %v, want [\"~/.custom.json\"]", agent.ConfigFiles)
	}
}

func TestResolveAgentProvider_IncludesConfigFiles(t *testing.T) {
	cfg := &Config{}

	agent, err := cfg.ResolveAgentProvider("claude-code")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(agent.ConfigFiles) != 1 || agent.ConfigFiles[0] != "~/.claude.json" {
		t.Errorf("ConfigFiles: got %v, want [\"~/.claude.json\"]", agent.ConfigFiles)
	}

	if len(agent.ConfigDirs) != 1 || agent.ConfigDirs[0] != "~/.claude" {
		t.Errorf("ConfigDirs: got %v, want [\"~/.claude\"]", agent.ConfigDirs)
	}
}

func TestLoad_AgentWithKeychainAuth(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `agent: opencode
agents:
  opencode:
    command: "opencode"
    keychain_auth: true
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	agent, ok := cfg.AgentProviders["opencode"]
	if !ok {
		t.Fatalf("agents.opencode: missing")
	}
	if agent.Command != "opencode" {
		t.Errorf("agents.opencode.command: got %q, want %q", agent.Command, "opencode")
	}
	if agent.Preset != "" {
		t.Errorf("agents.opencode.preset: got %q, want empty", agent.Preset)
	}
	if !agent.KeychainAuth {
		t.Error("agents.opencode.keychain_auth: expected true")
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

	if cfg.DefaultParallel != 4 {
		t.Errorf("default_parallel: got %d, want %d", cfg.DefaultParallel, 4)
	}
	if cfg.BuildTools != DefaultBuildToolsPreset {
		t.Errorf("build_tools: got %q, want %q", cfg.BuildTools, DefaultBuildToolsPreset)
	}
	if cfg.WorktreeDir != ".sandman/worktrees" {
		t.Errorf("worktree_dir: got %q, want %q", cfg.WorktreeDir, ".sandman/worktrees")
	}
	if cfg.Sandbox != "podman" {
		t.Errorf("sandbox: got %q, want %q", cfg.Sandbox, "podman")
	}
	if cfg.Git.DefaultBranch != "main" {
		t.Errorf("git.default_branch: got %q, want %q", cfg.Git.DefaultBranch, "main")
	}
}

func TestLoad_MissingContainerSettings_AppliesDefaults(t *testing.T) {
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

	if cfg.ContainerCapacity != 4 {
		t.Errorf("container_capacity: got %d, want %d", cfg.ContainerCapacity, 4)
	}
	if cfg.MaxContainers != 0 {
		t.Errorf("max_containers: got %d, want %d", cfg.MaxContainers, 0)
	}
}

func TestLoad_InvalidContainerSettings_ReturnValidationError(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name: "container capacity less than one",
			content: `agent: codex
container_capacity: 0
`,
			wantErr: "container_capacity must be at least 1",
		},
		{
			name: "negative max containers",
			content: `agent: codex
max_containers: -1
`,
			wantErr: "max_containers must be 0 or greater",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(tt.content), 0644); err != nil {
				t.Fatalf("write config: %v", err)
			}

			_, err := Load(path)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestLoad_NegativeDefaultParallel_DefaultsToFour(t *testing.T) {
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

	if cfg.DefaultParallel != 4 {
		t.Errorf("default_parallel: got %d, want %d", cfg.DefaultParallel, 4)
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
		Agent:             "codex",
		BuildTools:        "go",
		DefaultParallel:   4,
		ContainerCapacity: 3,
		MaxContainers:     0,
		WorktreeDir:       "/tmp/wt",
		Sandbox:           "podman",
		Git: GitConfig{
			DefaultBranch: "main",
			AuthorName:    "Dev",
			AuthorEmail:   "dev@example.com",
		},
	}

	tests := []struct {
		key     string
		want    string
		wantErr bool
	}{
		{"agent", "codex", false},
		{"build_tools", "go", false},
		{"default_parallel", "4", false},
		{"container_capacity", "3", false},
		{"max_containers", "0", false},
		{"worktree_dir", "/tmp/wt", false},
		{"sandbox", "podman", false},
		{"git.default_branch", "main", false},
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
		{"build_tools", "go", false},
		{"default_parallel", "4", false},
		{"container_capacity", "4", false},
		{"max_containers", "0", false},
		{"worktree_dir", "/tmp/wt", false},
		{"sandbox", "podman", false},
		{"git.default_branch", "master", false},
		{"git.author_name", "Alice", false},
		{"git.author_email", "alice@example.com", false},
		{"unknown_key", "value", true},
		{"default_parallel", "not-a-number", true},
		{"default_parallel", "-1", true},
		{"container_capacity", "0", true},
		{"container_capacity", "not-a-number", true},
		{"max_containers", "-1", true},
		{"max_containers", "not-a-number", true},
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
