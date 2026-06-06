package cmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/skill"
)

func TestConfigGet_DefaultAgent(t *testing.T) {
	var buf bytes.Buffer
	store := &fakeStore{
		config: &config.Config{DefaultAgent: "pi", Agent: "pi"},
	}
	cmd := NewConfigGetCmd(store)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"default_agent"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "pi") {
		t.Errorf("expected output to contain 'pi', got: %q", buf.String())
	}
}

func TestConfigGet_MaxContainers(t *testing.T) {
	var buf bytes.Buffer
	store := &fakeStore{
		config: &config.Config{DefaultAgent: "opencode", Agent: "opencode", MaxContainers: 0},
	}
	cmd := NewConfigGetCmd(store)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"max_containers"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "0") {
		t.Errorf("expected output to contain '0', got: %q", buf.String())
	}
}

func TestConfigGet_BuildTools(t *testing.T) {
	var buf bytes.Buffer
	store := &fakeStore{
		config: &config.Config{DefaultAgent: "opencode", Agent: "opencode", BuildTools: "go"},
	}
	cmd := NewConfigGetCmd(store)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"build_tools"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "go") {
		t.Errorf("expected output to contain 'go', got: %q", buf.String())
	}
}

func TestConfigGet_ReviewCommand(t *testing.T) {
	var buf bytes.Buffer
	store := &fakeStore{
		config: &config.Config{DefaultAgent: "opencode", Agent: "opencode", ReviewCommand: "/review please"},
	}
	cmd := NewConfigGetCmd(store)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"review_command"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "/review please") {
		t.Errorf("expected output to contain review command, got: %q", buf.String())
	}
}

func TestConfigGet_ContainerCapacity(t *testing.T) {
	var buf bytes.Buffer
	store := &fakeStore{
		config: &config.Config{DefaultAgent: "opencode", Agent: "opencode", ContainerCapacity: 4},
	}
	cmd := NewConfigGetCmd(store)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"container_capacity"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "4") {
		t.Errorf("expected output to contain '4', got: %q", buf.String())
	}
}

func TestConfigGet_StartDelay(t *testing.T) {
	var buf bytes.Buffer
	store := &fakeStore{
		config: &config.Config{DefaultAgent: "opencode", Agent: "opencode", StartDelay: 12},
	}
	cmd := NewConfigGetCmd(store)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"start_delay"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "12") {
		t.Errorf("expected output to contain '12', got: %q", buf.String())
	}
}

func TestConfigGet_RunIdleTimeout(t *testing.T) {
	var buf bytes.Buffer
	store := &fakeStore{
		config: &config.Config{DefaultAgent: "opencode", Agent: "opencode", RunIdleTimeout: 1800},
	}
	cmd := NewConfigGetCmd(store)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"run_idle_timeout"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "1800") {
		t.Errorf("expected output to contain '1800', got: %q", buf.String())
	}
}

func TestConfigGet_UnknownKey_ReturnsError(t *testing.T) {
	var buf bytes.Buffer
	store := &fakeStore{
		config: &config.Config{DefaultAgent: "opencode", Agent: "opencode"},
	}
	cmd := NewConfigGetCmd(store)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"unknown_key"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
	if !strings.Contains(err.Error(), "unknown config key") {
		t.Errorf("expected error to mention 'unknown config key', got: %v", err)
	}
}

func TestConfigList_PrintsSupportedKeysAndEffectiveValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `default_agent: pi
default_model: openai/gpt-4.1
build_tools: go
review_command: /review please
default_parallel: 3
start_delay: 5
container_capacity: 7
max_containers: 2
worktree_dir: /tmp/wt
sandbox: worktree
git:
  base_branch: trunk
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var buf bytes.Buffer
	cmd := NewConfigListCmd(&config.FileStore{Path: path})
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{
		"default_agent: pi",
		"default_model: openai/gpt-4.1",
		"build_tools: go",
		"review_command: /review please",
		"default_parallel: 3",
		"start_delay: 5",
		"run_idle_timeout: 1800",
		"retries: 0",
		"container_capacity: 7",
		"max_containers: 2",
		"worktree_dir: /tmp/wt",
		"sandbox: worktree",
		"git.base_branch: trunk",
	}

	got := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(got) != len(want) {
		t.Fatalf("unexpected line count: got %d, want %d; output: %q", len(got), len(want), buf.String())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestConfigList_LoadError_ReturnsError(t *testing.T) {
	var buf bytes.Buffer
	cmd := NewConfigListCmd(&fakeStore{err: fmt.Errorf("boom")})
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when config load fails")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected load error to surface, got: %v", err)
	}
}

func TestConfigSet_DefaultAgent_UpdatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("default_agent: opencode\n"), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	store := &config.FileStore{Path: path}
	cmd := NewConfigSetCmd(store)
	cmd.SetArgs([]string{"default_agent", "pi"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if cfg.DefaultAgent != "pi" {
		t.Errorf("default_agent: got %q, want %q", cfg.DefaultAgent, "pi")
	}
}

func TestConfigSet_BuildTools_UpdatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("default_agent: opencode\n"), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	store := &config.FileStore{Path: path}
	cmd := NewConfigSetCmd(store)
	cmd.SetArgs([]string{"build_tools", "go"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if cfg.BuildTools != "go" {
		t.Errorf("build_tools: got %q, want %q", cfg.BuildTools, "go")
	}
}

func TestConfigSet_ReviewCommand_UpdatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("default_agent: opencode\n"), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	oldSync := syncSandmanSkill
	syncSandmanSkill = func(opts skill.SyncOptions) error {
		if opts.ReviewCommand != "/review please" {
			t.Fatalf("expected sync review command, got %q", opts.ReviewCommand)
		}
		return nil
	}
	t.Cleanup(func() { syncSandmanSkill = oldSync })

	store := &config.FileStore{Path: path}
	cmd := NewConfigSetCmd(store)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{"review_command", "/review please"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if cfg.ReviewCommand != "/review please" {
		t.Errorf("review_command: got %q, want %q", cfg.ReviewCommand, "/review please")
	}
}

func TestConfigSet_ReviewCommand_DoesNotSaveWhenSyncFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("default_agent: opencode\nreview_command: /old review\n"), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	oldSync := syncSandmanSkill
	syncSandmanSkill = func(opts skill.SyncOptions) error {
		return fmt.Errorf("boom")
	}
	t.Cleanup(func() { syncSandmanSkill = oldSync })

	store := &config.FileStore{Path: path}
	cmd := NewConfigSetCmd(store)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{"review_command", "/new review"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected sync failure")
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if cfg.ReviewCommand != "/old review" {
		t.Fatalf("expected config review command to stay old value, got %q", cfg.ReviewCommand)
	}
}

func TestConfigSet_StartDelay_UpdatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("default_agent: opencode\n"), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	store := &config.FileStore{Path: path}
	cmd := NewConfigSetCmd(store)
	cmd.SetArgs([]string{"start_delay", "9"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if cfg.StartDelay != 9 {
		t.Errorf("start_delay: got %d, want %d", cfg.StartDelay, 9)
	}
}

func TestConfigSet_UnknownKey_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("default_agent: opencode\n"), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	store := &config.FileStore{Path: path}
	cmd := NewConfigSetCmd(store)
	cmd.SetArgs([]string{"unknown_key", "value"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
	if !strings.Contains(err.Error(), "unknown config key") {
		t.Errorf("expected error to mention 'unknown config key', got: %v", err)
	}
}

func TestConfigSet_DefaultParallel_InvalidValue_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("default_agent: opencode\n"), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	store := &config.FileStore{Path: path}
	cmd := NewConfigSetCmd(store)
	cmd.SetArgs([]string{"default_parallel", "not-a-number"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for invalid default_parallel")
	}
	if !strings.Contains(err.Error(), "invalid value for default_parallel") {
		t.Errorf("expected error to mention 'invalid value for default_parallel', got: %v", err)
	}
}

func TestConfigSet_DefaultParallel_NegativeValue_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("default_agent: opencode\n"), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	store := &config.FileStore{Path: path}
	cmd := NewConfigSetCmd(store)
	cmd.SetArgs([]string{"default_parallel", "-1"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for negative default_parallel")
	}
	if !strings.Contains(err.Error(), "must be greater than 0") {
		t.Errorf("expected error to mention 'must be greater than 0', got: %v", err)
	}
}

func TestConfigSet_GitAuthorName_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("default_agent: opencode\n"), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	store := &config.FileStore{Path: path}
	cmd := NewConfigSetCmd(store)
	cmd.SetArgs([]string{"git.author_name", "Alice"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for removed git author key")
	}
	if !strings.Contains(err.Error(), "unknown config key") {
		t.Fatalf("expected unknown config key error, got: %v", err)
	}
}

func TestConfigSet_MaxContainers_AutoModeUpdatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("default_agent: opencode\ncontainer_capacity: 4\nmax_containers: 2\n"), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	store := &config.FileStore{Path: path}
	cmd := NewConfigSetCmd(store)
	cmd.SetArgs([]string{"max_containers", "0"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if cfg.MaxContainers != 0 {
		t.Errorf("max_containers: got %d, want %d", cfg.MaxContainers, 0)
	}
}

func TestConfigSet_ContainerCapacity_UpdatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("default_agent: opencode\ncontainer_capacity: 4\nmax_containers: 0\n"), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	store := &config.FileStore{Path: path}
	cmd := NewConfigSetCmd(store)
	cmd.SetArgs([]string{"container_capacity", "2"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if cfg.ContainerCapacity != 2 {
		t.Errorf("container_capacity: got %d, want %d", cfg.ContainerCapacity, 2)
	}
}
