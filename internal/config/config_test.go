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
start_delay: 5
worktree_dir: /tmp/wt
sandbox: worktree
git:
  base_branch: trunk
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
	if cfg.StartDelay != 5 {
		t.Errorf("start_delay: got %d, want %d", cfg.StartDelay, 5)
	}
	if cfg.WorktreeDir != "/tmp/wt" {
		t.Errorf("worktree_dir: got %q, want %q", cfg.WorktreeDir, "/tmp/wt")
	}
	if cfg.Sandbox != "worktree" {
		t.Errorf("sandbox: got %q, want %q", cfg.Sandbox, "worktree")
	}
	if cfg.Git.BaseBranch != "trunk" {
		t.Errorf("git.base_branch: got %q, want %q", cfg.Git.BaseBranch, "trunk")
	}
	if _, ok := cfg.AgentProviders["opencode"]; !ok {
		t.Fatal("expected built-in opencode agent in derived map")
	}
}

func TestLoad_IgnoresLegacyGitAuthorFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `git:
  base_branch: main
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
	if cfg.Git.BaseBranch != "main" {
		t.Fatalf("git.base_branch: got %q, want %q", cfg.Git.BaseBranch, "main")
	}
}

func TestLoad_RejectsLegacyGitDefaultBranch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `git:
  default_branch: main
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "git.default_branch was renamed to git.base_branch") {
		t.Fatalf("expected rename error, got %v", err)
	}
}

func TestLoad_DefaultAgentDefaultsToOpenCode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `git:
  base_branch: main
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
  base_branch: main
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
		t.Fatal("expected error for missing provider/model separator")
	}
	if !strings.Contains(err.Error(), "provider/model format") {
		t.Fatalf("expected error mentioning provider/model format, got %v", err)
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

func TestLoad_AgentWithKeychainAuth(t *testing.T) {
	cfg := &Config{
		DefaultAgent: "opencode",
		AgentProviders: map[string]Agent{
			"opencode": {
				Preset:       "opencode",
				Command:      "opencode",
				KeychainAuth: true,
			},
		},
	}

	agent, err := cfg.ResolveAgentProvider("opencode")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if agent.Command != "opencode" {
		t.Errorf("agents.opencode.command: got %q, want %q", agent.Command, "opencode")
	}
	if agent.Preset != "opencode" {
		t.Errorf("agents.opencode.preset: got %q, want %q", agent.Preset, "opencode")
	}
	if !agent.KeychainAuth {
		t.Error("agents.opencode.keychain_auth: expected true")
	}
}

func TestLoad_MissingOptionalFields_AppliesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `default_agent: opencode
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
	if cfg.Git.BaseBranch != "main" {
		t.Errorf("git.base_branch: got %q, want %q", cfg.Git.BaseBranch, "main")
	}
	if cfg.ReviewCommand != "/oc review" {
		t.Errorf("review_command: got %q, want %q", cfg.ReviewCommand, "/oc review")
	}
}

func TestLoad_MissingContainerSettings_AppliesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `default_agent: opencode
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
	if cfg.StartDelay != 0 {
		t.Errorf("start_delay: got %d, want %d", cfg.StartDelay, 0)
	}
}

func TestLoad_InvalidContainerSettings_ReturnValidationError(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name: "negative container capacity",
			content: `default_agent: opencode
container_capacity: -1
`,
			wantErr: "container_capacity must be 0 or greater",
		},
		{
			name: "negative max containers",
			content: `default_agent: opencode
max_containers: -1
`,
			wantErr: "max_containers must be 0 or greater",
		},
		{
			name: "negative start delay",
			content: `default_agent: opencode
start_delay: -1
`,
			wantErr: "start_delay must be 0 or greater",
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

func TestLoad_ContainerCapacityZeroIsAccepted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `default_agent: opencode
container_capacity: 0
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.ContainerCapacity != 0 {
		t.Errorf("container_capacity: got %d, want %d", cfg.ContainerCapacity, 0)
	}
}

func TestLoad_NegativeDefaultParallel_DefaultsToFour(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `default_agent: opencode
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

func TestConfig_GetAndSetBaseBranch(t *testing.T) {
	cfg := &Config{Git: GitConfig{BaseBranch: "main"}}

	if got, err := cfg.GetValue("git.base_branch"); err != nil || got != "main" {
		t.Fatalf("GetValue(git.base_branch) = %q, %v", got, err)
	}
	if err := cfg.SetValue("git.base_branch", "trunk"); err != nil {
		t.Fatalf("SetValue(git.base_branch): %v", err)
	}
	if cfg.Git.BaseBranch != "trunk" {
		t.Fatalf("base_branch not updated: %#v", cfg)
	}
	if _, err := cfg.GetValue("git.default_branch"); err == nil || !strings.Contains(err.Error(), "renamed") {
		t.Fatal("expected old get key to be rejected with rename error")
	}
	if err := cfg.SetValue("git.default_branch", "main"); err == nil || !strings.Contains(err.Error(), "renamed") {
		t.Fatal("expected old set key to be rejected with rename error")
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

func TestConfig_SetValue(t *testing.T) {
	cfg := &Config{Agent: "opencode"}

	tests := []struct {
		key     string
		value   string
		wantErr bool
	}{
		{"default_agent", "opencode", false},
		{"build_tools", "go", false},
		{"review_command", "/oc review", false},
		{"default_parallel", "4", false},
		{"start_delay", "0", false},
		{"start_delay", "5", false},
		{"container_capacity", "4", false},
		{"container_capacity", "0", false},
		{"max_containers", "0", false},
		{"worktree_dir", "/tmp/wt", false},
		{"sandbox", "podman", false},
		{"git.base_branch", "master", false},
		{"git.default_branch", "master", true},
		{"unknown_key", "value", true},
		{"default_parallel", "not-a-number", true},
		{"default_parallel", "-1", true},
		{"start_delay", "not-a-number", true},
		{"start_delay", "-1", true},
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
