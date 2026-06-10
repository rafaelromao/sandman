package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

func TestLoad_ValidConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `agent: pi
build_tools: go
review_command: /review please
parallel: 3
start_delay: 5
retries: 2
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
		t.Errorf("agent: got %q, want %q", cfg.DefaultAgent, "pi")
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
		t.Errorf("parallel: got %d, want %d", cfg.DefaultParallel, 3)
	}
	if cfg.StartDelay != 5 {
		t.Errorf("start_delay: got %d, want %d", cfg.StartDelay, 5)
	}
	if cfg.Retries != 2 {
		t.Errorf("retries: got %d, want %d", cfg.Retries, 2)
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

func TestLoad_RejectsLegacyGitBranchKey(t *testing.T) {
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
		t.Fatalf("agent: got %q, want %q", cfg.DefaultAgent, DefaultAgent)
	}
}

func TestLoad_RejectsUnknownDefaultAgent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `agent: codex
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
	wantCmd := `opencode run{{if .DangerouslySkipPermissions}} --dangerously-skip-permissions{{end}}{{if .SessionName}} --title '{{.SessionName}}'{{end}}{{if .ModelFlag}} {{.ModelFlag}}{{end}} "$(cat {{.PromptFile}})"`
	if agent.Command != wantCmd {
		t.Errorf("command: got %q, want %q", agent.Command, wantCmd)
	}
	wantDirs := []string{"~/.config/opencode", "~/.local/share/opencode", "~/.claude", "~/.agents"}
	if !reflect.DeepEqual(agent.ConfigDirs, wantDirs) {
		t.Errorf("config_dirs: got %v, want %v", agent.ConfigDirs, wantDirs)
	}
	if agent.KeychainAuth {
		t.Error("keychain_auth: expected false")
	}
}

func TestBuiltInAgentPresets_OpencodeExcludesMutableState(t *testing.T) {
	preset, ok := BuiltInAgentPresets["opencode"]
	if !ok {
		t.Fatal("expected opencode preset to exist")
	}

	wantExcluded := []string{
		"~/.local/share/opencode/token-optimizer",
		"~/.local/share/opencode/storage",
		"~/.local/share/opencode/snapshot",
		"~/.local/share/opencode/tool-output",
		"~/.local/share/opencode/repos",
		"~/.local/share/opencode/log",
		"~/.local/share/opencode/node_modules",
		"~/.local/share/opencode/opencode.db",
		"~/.local/share/opencode/opencode.db-shm",
		"~/.local/share/opencode/opencode.db-wal",
	}
	for _, want := range wantExcluded {
		if !slices.Contains(preset.SnapshotExcludes, want) {
			t.Errorf("expected SnapshotExcludes to contain %q, got %v", want, preset.SnapshotExcludes)
		}
	}
}

func TestBuiltInAgentPresets_OpencodeLiveMountsDatabase(t *testing.T) {
	preset, ok := BuiltInAgentPresets["opencode"]
	if !ok {
		t.Fatal("expected opencode preset to exist")
	}

	wantLive := []string{
		"~/.local/share/opencode/opencode.db",
		"~/.local/share/opencode/opencode.db-shm",
		"~/.local/share/opencode/opencode.db-wal",
	}
	for _, want := range wantLive {
		if !slices.Contains(preset.LiveMounts, want) {
			t.Errorf("expected LiveMounts to contain %q, got %v", want, preset.LiveMounts)
		}
	}
}

func TestBuiltInAgentPresets_PiExcludesMutableState(t *testing.T) {
	preset, ok := BuiltInAgentPresets["pi"]
	if !ok {
		t.Fatal("expected pi preset to exist")
	}

	wantExcluded := []string{
		"~/.pi/agent/npm",
		"~/.pi/agent/sessions",
	}
	for _, want := range wantExcluded {
		if !slices.Contains(preset.SnapshotExcludes, want) {
			t.Errorf("expected SnapshotExcludes to contain %q, got %v", want, preset.SnapshotExcludes)
		}
	}
}

func TestBuiltInAgentPresets_PiLiveMountsRuntimeState(t *testing.T) {
	preset, ok := BuiltInAgentPresets["pi"]
	if !ok {
		t.Fatal("expected pi preset to exist")
	}

	wantLive := []string{
		"~/.pi/agent/npm",
		"~/.pi/agent/sessions",
	}
	for _, want := range wantLive {
		if !slices.Contains(preset.LiveMounts, want) {
			t.Errorf("expected LiveMounts to contain %q, got %v", want, preset.LiveMounts)
		}
	}
}

func TestBuiltInAgentPresets_OpencodeEnvPermissionAllowAll(t *testing.T) {
	preset, ok := BuiltInAgentPresets["opencode"]
	if !ok {
		t.Fatal("expected opencode preset to exist")
	}

	raw, ok := preset.Env["OPENCODE_PERMISSION"]
	if !ok {
		t.Fatal("expected opencode preset to set OPENCODE_PERMISSION")
	}

	var parsed map[string]string
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("OPENCODE_PERMISSION must be valid JSON object: %v\nraw=%s", err, raw)
	}

	wantKeys := []string{"external_directory"}
	if len(parsed) != len(wantKeys) {
		t.Errorf("OPENCODE_PERMISSION map size: got %d, want %d (keys=%v)", len(parsed), len(wantKeys), keysOf(parsed))
	}
	for _, key := range wantKeys {
		value, present := parsed[key]
		if !present {
			t.Errorf("OPENCODE_PERMISSION missing key %q (got keys=%v)", key, keysOf(parsed))
			continue
		}
		if value != "allow" {
			t.Errorf("OPENCODE_PERMISSION[%q]: got %q, want %q", key, value, "allow")
		}
	}

	if _, ok := parsed["external_directory"]; !ok {
		t.Error("OPENCODE_PERMISSION must contain external_directory (the key that caused the observed subagent hang)")
	}
}

func TestAgentWithOverrides_MergesPresetEnv(t *testing.T) {
	preset := AgentPreset{
		DisplayName: "OpenCode",
		Command:     "opencode",
		Env:         map[string]string{"OPENCODE_PERMISSION": OpencodePermissionExternalDirectoryAllow},
	}

	agent := preset.AgentWithOverrides("opencode", Agent{Env: map[string]string{"API_KEY": "abc123"}})
	if agent.Env["OPENCODE_PERMISSION"] != OpencodePermissionExternalDirectoryAllow {
		t.Fatalf("expected preset OPENCODE_PERMISSION to be preserved, got %#v", agent.Env)
	}
	if agent.Env["API_KEY"] != "abc123" {
		t.Fatalf("expected override env to be merged, got %#v", agent.Env)
	}
}

func TestAgentWithOverrides_UserOpencodePermissionOverridesPreset(t *testing.T) {
	preset := AgentPreset{
		DisplayName: "OpenCode",
		Command:     "opencode",
		Env:         map[string]string{"OPENCODE_PERMISSION": OpencodePermissionExternalDirectoryAllow},
	}

	agent := preset.AgentWithOverrides("opencode", Agent{Env: map[string]string{"OPENCODE_PERMISSION": `{"external_directory":"allow","read":"allow"}`}})
	if got := agent.Env["OPENCODE_PERMISSION"]; got != `{"external_directory":"allow","read":"allow"}` {
		t.Fatalf("expected user OPENCODE_PERMISSION to override preset, got %q", got)
	}
	if agent.OpencodePermissionMode != "custom" {
		t.Fatalf("expected custom permission mode, got %q", agent.OpencodePermissionMode)
	}
}

func TestBuiltInAgentPresets_PiEnvUnchanged(t *testing.T) {
	preset, ok := BuiltInAgentPresets["pi"]
	if !ok {
		t.Fatal("expected pi preset to exist")
	}
	if _, ok := preset.Env["OPENCODE_PERMISSION"]; ok {
		t.Error("pi preset must not set OPENCODE_PERMISSION (pi has its own permission model)")
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
	wantDirs := []string{"~/.pi", "~/.claude", "~/.agents"}
	if !reflect.DeepEqual(agent.ConfigDirs, wantDirs) {
		t.Errorf("config_dirs: got %v, want %v", agent.ConfigDirs, wantDirs)
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
	content := `agent: opencode
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.DefaultParallel != 4 {
		t.Errorf("parallel: got %d, want %d", cfg.DefaultParallel, 4)
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
	if cfg.ReviewCommand != "/sandman review" {
		t.Errorf("review_command: got %q, want %q", cfg.ReviewCommand, "/sandman review")
	}
	if cfg.Retries != DefaultRetries {
		t.Errorf("retries: got %d, want %d", cfg.Retries, DefaultRetries)
	}
}

func TestLoad_MissingContainerSettings_AppliesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `agent: opencode
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

func TestLoad_MissingRunIdleTimeout_AppliesDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `agent: opencode
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.RunIdleTimeout != DefaultRunIdleTimeout {
		t.Errorf("run_idle_timeout: got %d, want %d", cfg.RunIdleTimeout, DefaultRunIdleTimeout)
	}
	if DefaultRunIdleTimeout != 1800 {
		t.Errorf("DefaultRunIdleTimeout: got %d, want 1800", DefaultRunIdleTimeout)
	}
}

func TestLoad_MissingRetries_AppliesDefault(t *testing.T) {
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

	if cfg.Retries != DefaultRetries {
		t.Errorf("retries: got %d, want %d", cfg.Retries, DefaultRetries)
	}
	if DefaultRetries != 3 {
		t.Errorf("DefaultRetries: got %d, want 3", DefaultRetries)
	}
}

func TestLoad_RetriesZeroIsAccepted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `default_agent: opencode
retries: 0
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Retries != 0 {
		t.Errorf("retries: got %d, want 0", cfg.Retries)
	}
}

func TestLoad_RunIdleTimeoutZeroIsAccepted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `agent: opencode
run_idle_timeout: 0
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.RunIdleTimeout != 0 {
		t.Errorf("run_idle_timeout: got %d, want 0", cfg.RunIdleTimeout)
	}
}

func TestLoad_RunIdleTimeoutPositive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `agent: opencode
run_idle_timeout: 600
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.RunIdleTimeout != 600 {
		t.Errorf("run_idle_timeout: got %d, want 600", cfg.RunIdleTimeout)
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
			content: `agent: opencode
container_capacity: -1
`,
			wantErr: "container_capacity must be 0 or greater",
		},
		{
			name: "negative max containers",
			content: `agent: opencode
max_containers: -1
`,
			wantErr: "max_containers must be 0 or greater",
		},
		{
			name: "negative start delay",
			content: `agent: opencode
start_delay: -1
`,
			wantErr: "start_delay must be 0 or greater",
		},
		{
			name: "negative run idle timeout",
			content: `agent: opencode
run_idle_timeout: -1
`,
			wantErr: "run_idle_timeout must be 0 or greater",
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
	content := `agent: opencode
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
	content := `agent: opencode
parallel: -2
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.DefaultParallel != 4 {
		t.Errorf("parallel: got %d, want %d", cfg.DefaultParallel, 4)
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

	if got, err := cfg.GetValue("agent"); err != nil || got != DefaultAgent {
		t.Fatalf("GetValue(agent) = %q, %v", got, err)
	}
	if got, err := cfg.GetValue("retries"); err != nil || got != "0" {
		t.Fatalf("GetValue(retries) = %q, %v", got, err)
	}
	if err := cfg.SetValue("agent", "pi"); err != nil {
		t.Fatalf("SetValue(agent): %v", err)
	}
	if cfg.DefaultAgent != "pi" || cfg.Agent != "pi" {
		t.Fatalf("agent not updated: %#v", cfg)
	}
	if _, err := cfg.GetValue("default_agent"); err == nil {
		t.Fatal("expected old key to be rejected")
	}
	if err := cfg.SetValue("default_agent", "opencode"); err == nil {
		t.Fatal("expected old key to be rejected")
	}
	if err := cfg.SetValue("retries", "3"); err != nil {
		t.Fatalf("SetValue(retries): %v", err)
	}
	if cfg.Retries != 3 {
		t.Fatalf("retries not updated: %#v", cfg)
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

func TestLoad_AgentsMapDoesNotPopulateAgentProviders(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `agent: opencode
agents:
  custom-agent:
    name: custom-agent
    command: custom-command
  another-custom:
    name: another-custom
    preset: pi
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, ok := cfg.AgentProviders["opencode"]; !ok {
		t.Fatal("expected opencode built-in in AgentProviders")
	}
	if _, ok := cfg.AgentProviders["pi"]; !ok {
		t.Fatal("expected pi built-in in AgentProviders")
	}
	if _, ok := cfg.AgentProviders["custom-agent"]; ok {
		t.Error("custom-agent should not be in AgentProviders")
	}
	if _, ok := cfg.AgentProviders["another-custom"]; ok {
		t.Error("another-custom should not be in AgentProviders")
	}
}

func TestConfig_ResolveAgentProvider_NonBuiltIn_ReturnsNotFound(t *testing.T) {
	cfg := &Config{}

	_, err := cfg.ResolveAgentProvider("custom-agent")
	if err == nil {
		t.Fatal("expected error for unknown agent")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected 'not found' error, got: %v", err)
	}

	_, err = cfg.ResolveAgentProvider("another-custom")
	if err == nil {
		t.Fatal("expected error for unknown agent")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected 'not found' error, got: %v", err)
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
		{"agent", "opencode", false},
		{"build_tools", "go", false},
		{"review_command", "/oc review", false},
		{"review_agent", "opencode", false},
		{"review_model", "opencode/big-pickle", false},
		{"parallel", "4", false},
		{"start_delay", "0", false},
		{"start_delay", "5", false},
		{"run_idle_timeout", "0", false},
		{"run_idle_timeout", "1800", false},
		{"retries", "0", false},
		{"retries", "3", false},
		{"container_capacity", "4", false},
		{"container_capacity", "0", false},
		{"max_containers", "0", false},
		{"worktree_dir", "/tmp/wt", false},
		{"sandbox", "podman", false},
		{"git.base_branch", "master", false},
		{"git.default_branch", "master", true},
		{"unknown_key", "value", true},
		{"parallel", "not-a-number", true},
		{"parallel", "-1", true},
		{"start_delay", "not-a-number", true},
		{"start_delay", "-1", true},
		{"run_idle_timeout", "not-a-number", true},
		{"run_idle_timeout", "-1", true},
		{"retries", "-1", true},
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

func TestLoad_DefaultModelAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `agent: opencode
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.DefaultModel != "" {
		t.Fatalf("model: got %q, want empty string to preserve per-agent model", cfg.DefaultModel)
	}
}

func TestLoad_ModelEmptyPreservesPerAgentModel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `agent: opencode
model: ""
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.DefaultModel != "" {
		t.Fatalf("model: got %q, want empty string to preserve per-agent model", cfg.DefaultModel)
	}
}

func TestLoad_ModelFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `agent: opencode
model: openai/gpt-4.1
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.DefaultModel != "openai/gpt-4.1" {
		t.Fatalf("model: got %q, want %q", cfg.DefaultModel, "openai/gpt-4.1")
	}
}

func TestLoad_ParallelFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `agent: opencode
parallel: 8
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.DefaultParallel != 8 {
		t.Fatalf("parallel: got %d, want %d", cfg.DefaultParallel, 8)
	}
}

func TestDefaultModelConstant(t *testing.T) {
	if DefaultModel != "opencode/big-pickle" {
		t.Fatalf("DefaultModel: got %q, want %q", DefaultModel, "opencode/big-pickle")
	}
}

func TestConfig_GetAndSetModel(t *testing.T) {
	cfg := &Config{DefaultModel: DefaultModel}

	if got, err := cfg.GetValue("model"); err != nil || got != DefaultModel {
		t.Fatalf("GetValue(model) = %q, %v", got, err)
	}
	if err := cfg.SetValue("model", "openai/gpt-4.1"); err != nil {
		t.Fatalf("SetValue(model): %v", err)
	}
	if cfg.DefaultModel != "openai/gpt-4.1" {
		t.Fatalf("model not updated: %#v", cfg)
	}
}

func TestConfig_GetAndSetParallel(t *testing.T) {
	cfg := &Config{DefaultParallel: DefaultParallel}

	if got, err := cfg.GetValue("parallel"); err != nil || got != "4" {
		t.Fatalf("GetValue(parallel) = %q, %v", got, err)
	}
	if err := cfg.SetValue("parallel", "8"); err != nil {
		t.Fatalf("SetValue(parallel): %v", err)
	}
	if cfg.DefaultParallel != 8 {
		t.Fatalf("parallel not updated: %#v", cfg)
	}
	if err := cfg.SetValue("parallel", "-1"); err == nil {
		t.Fatal("expected error for negative parallel")
	}
}

func TestLoad_ReviewAgentAndModelFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `agent: opencode
model: openai/gpt-4.1
review_agent: pi
review_model: openai/gpt-5
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.DefaultReviewAgent != "pi" {
		t.Errorf("review_agent: got %q, want %q", cfg.DefaultReviewAgent, "pi")
	}
	if cfg.DefaultReviewModel != "openai/gpt-5" {
		t.Errorf("review_model: got %q, want %q", cfg.DefaultReviewModel, "openai/gpt-5")
	}
}

func TestLoad_ReviewAgentDefaultsToDefaultAgent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `agent: pi
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.DefaultReviewAgent != "pi" {
		t.Errorf("review_agent fallback: got %q, want %q", cfg.DefaultReviewAgent, "pi")
	}
}

func TestLoad_ReviewModelDefaultsToDefaultModel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `agent: opencode
model: openai/gpt-4.1
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.DefaultReviewModel != "openai/gpt-4.1" {
		t.Errorf("review_model fallback: got %q, want %q", cfg.DefaultReviewModel, "openai/gpt-4.1")
	}
}

func TestConfig_ReviewAgentAndModelResolution(t *testing.T) {
	tests := []struct {
		name         string
		defaultAgent string
		defaultModel string
		reviewAgent  string
		reviewModel  string
		wantAgent    string
		wantModel    string
	}{
		{
			name:         "empty fields fall back to defaults",
			defaultAgent: "opencode",
			defaultModel: "opencode/big-pickle",
			reviewAgent:  "",
			reviewModel:  "",
			wantAgent:    "opencode",
			wantModel:    "opencode/big-pickle",
		},
		{
			name:         "explicit review agent and model win",
			defaultAgent: "opencode",
			defaultModel: "opencode/big-pickle",
			reviewAgent:  "pi",
			reviewModel:  "openai/gpt-5",
			wantAgent:    "pi",
			wantModel:    "openai/gpt-5",
		},
		{
			name:         "review agent set, model falls back to default",
			defaultAgent: "opencode",
			defaultModel: "opencode/big-pickle",
			reviewAgent:  "pi",
			reviewModel:  "",
			wantAgent:    "pi",
			wantModel:    "opencode/big-pickle",
		},
		{
			name:         "review model set, agent falls back to default",
			defaultAgent: "opencode",
			defaultModel: "opencode/big-pickle",
			reviewAgent:  "",
			reviewModel:  "openai/gpt-5",
			wantAgent:    "opencode",
			wantModel:    "openai/gpt-5",
		},
		{
			name:         "empty defaults still resolve to constants",
			defaultAgent: "",
			defaultModel: "",
			reviewAgent:  "",
			reviewModel:  "",
			wantAgent:    DefaultAgent,
			wantModel:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				DefaultAgent:       tt.defaultAgent,
				DefaultModel:       tt.defaultModel,
				DefaultReviewAgent: tt.reviewAgent,
				DefaultReviewModel: tt.reviewModel,
			}

			if got := cfg.EffectiveReviewAgent(); got != tt.wantAgent {
				t.Errorf("EffectiveReviewAgent: got %q, want %q", got, tt.wantAgent)
			}
			if got := cfg.EffectiveReviewModel(); got != tt.wantModel {
				t.Errorf("EffectiveReviewModel: got %q, want %q", got, tt.wantModel)
			}
		})
	}
}

func TestConfig_GetAndSetReviewAgentAndModel(t *testing.T) {
	cfg := &Config{DefaultAgent: "opencode", DefaultModel: "opencode/big-pickle"}

	if got, err := cfg.GetValue("review_agent"); err != nil || got != "opencode" {
		t.Fatalf("GetValue(review_agent) = %q, %v", got, err)
	}
	if got, err := cfg.GetValue("review_model"); err != nil || got != "opencode/big-pickle" {
		t.Fatalf("GetValue(review_model) = %q, %v", got, err)
	}

	if err := cfg.SetValue("review_agent", "pi"); err != nil {
		t.Fatalf("SetValue(review_agent): %v", err)
	}
	if cfg.DefaultReviewAgent != "pi" {
		t.Fatalf("review_agent not updated: %#v", cfg)
	}

	if err := cfg.SetValue("review_model", "openai/gpt-5"); err != nil {
		t.Fatalf("SetValue(review_model): %v", err)
	}
	if cfg.DefaultReviewModel != "openai/gpt-5" {
		t.Fatalf("review_model not updated: %#v", cfg)
	}

	if err := cfg.SetValue("review_agent", "unknown-agent"); err == nil {
		t.Fatal("expected validation error for unknown review agent")
	}
}
