package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
)

func TestConfigGet_Agent(t *testing.T) {
	var buf bytes.Buffer
	store := &fakeStore{
		config: &config.Config{Agent: "codex"},
	}
	cmd := NewConfigGetCmd(store)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"agent"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "codex") {
		t.Errorf("expected output to contain 'codex', got: %q", buf.String())
	}
}

func TestConfigGet_MaxContainers(t *testing.T) {
	var buf bytes.Buffer
	store := &fakeStore{
		config: &config.Config{Agent: "codex", MaxContainers: 0},
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
		config: &config.Config{Agent: "codex", BuildTools: "go"},
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

func TestConfigGet_ContainerCapacity(t *testing.T) {
	var buf bytes.Buffer
	store := &fakeStore{
		config: &config.Config{Agent: "codex", ContainerCapacity: 4},
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

func TestConfigGet_UnknownKey_ReturnsError(t *testing.T) {
	var buf bytes.Buffer
	store := &fakeStore{
		config: &config.Config{Agent: "codex"},
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

func TestConfigSet_Agent_UpdatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("agent: opencode\n"), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	store := &config.FileStore{Path: path}
	cmd := NewConfigSetCmd(store)
	cmd.SetArgs([]string{"agent", "codex"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if cfg.Agent != "codex" {
		t.Errorf("agent: got %q, want %q", cfg.Agent, "codex")
	}
}

func TestConfigSet_BuildTools_UpdatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("agent: opencode\n"), 0644); err != nil {
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

func TestConfigSet_UnknownKey_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("agent: opencode\n"), 0644); err != nil {
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
	if err := os.WriteFile(path, []byte("agent: opencode\n"), 0644); err != nil {
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
	if err := os.WriteFile(path, []byte("agent: opencode\n"), 0644); err != nil {
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

func TestConfigSet_GitAuthorName_UpdatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("agent: opencode\n"), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	store := &config.FileStore{Path: path}
	cmd := NewConfigSetCmd(store)
	cmd.SetArgs([]string{"git.author_name", "Alice"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if cfg.Git.AuthorName != "Alice" {
		t.Errorf("git.author_name: got %q, want %q", cfg.Git.AuthorName, "Alice")
	}
}

func TestConfigSet_MaxContainers_AutoModeUpdatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("agent: opencode\ncontainer_capacity: 4\nmax_containers: 2\n"), 0644); err != nil {
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
	if err := os.WriteFile(path, []byte("agent: opencode\ncontainer_capacity: 4\nmax_containers: 0\n"), 0644); err != nil {
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
