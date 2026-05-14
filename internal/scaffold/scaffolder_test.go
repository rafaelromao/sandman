package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
)

type fakePrompter struct {
	confirm    bool
	confirmErr error
	selected   string
	selectErr  error
}

func (f *fakePrompter) Confirm(msg string) (bool, error) {
	return f.confirm, f.confirmErr
}

func (f *fakePrompter) Select(msg string, options []string) (string, error) {
	return f.selected, f.selectErr
}

func TestScaffold_CreatesConfigWithDefaults(t *testing.T) {
	dir := t.TempDir()
	s := &Scaffolder{}

	err := s.Scaffold(dir, Options{}, &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	configPath := filepath.Join(dir, ".sandman", "config.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "agent: opencode") {
		t.Errorf("config missing default agent, got:\n%s", content)
	}
	if !strings.Contains(content, "default_parallel: 4") {
		t.Errorf("config missing default parallel, got:\n%s", content)
	}
	if !strings.Contains(content, "container_capacity: 4") {
		t.Errorf("config missing container capacity, got:\n%s", content)
	}
	if !strings.Contains(content, "max_containers: 0") {
		t.Errorf("config missing max containers, got:\n%s", content)
	}
	if !strings.Contains(content, "worktree_dir: .sandman/worktrees") {
		t.Errorf("config missing default worktree dir, got:\n%s", content)
	}
	if !strings.Contains(content, "sandbox: podman") {
		t.Errorf("config missing default sandbox, got:\n%s", content)
	}
	if !strings.Contains(content, "agents:") {
		t.Errorf("config missing agents section, got:\n%s", content)
	}
	if !strings.Contains(content, "preset: opencode") {
		t.Errorf("config missing default agent preset, got:\n%s", content)
	}
	if strings.Contains(content, "command: opencode") {
		t.Errorf("config should not hard-code the default agent command, got:\n%s", content)
	}
}

func TestScaffold_CreatesDockerfileAndPromptMd(t *testing.T) {
	dir := t.TempDir()
	s := &Scaffolder{}

	err := s.Scaffold(dir, Options{Lang: "go"}, &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	dockerfilePath := filepath.Join(dir, ".sandman", "Dockerfile")
	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	if !strings.Contains(string(data), "FROM golang:latest") {
		t.Errorf("Dockerfile missing expected base image, got:\n%s", data)
	}

	promptPath := filepath.Join(dir, ".sandman", "prompt.md")
	if _, err := os.Stat(promptPath); os.IsNotExist(err) {
		t.Errorf("prompt.md not created")
	}
}

func TestScaffold_AutoDetectsLanguages(t *testing.T) {
	tests := []struct {
		file     string
		lang     string
		expected string
	}{
		{"go.mod", "go", "FROM golang:latest"},
		{"package.json", "node", "FROM node:latest"},
		{"requirements.txt", "python", "FROM python:latest"},
		{"Cargo.toml", "rust", "FROM rust:latest"},
		{"pom.xml", "java", "FROM maven:latest"},
		{"build.gradle", "java", "FROM maven:latest"},
		{"composer.json", "php", "FROM php:latest"},
		{"mix.exs", "elixir", "FROM elixir:latest"},
		{"build.zig", "zig", "FROM ziglang/zig:latest"},
		{"Gemfile", "ruby", "FROM ruby:latest"},
		{"Package.swift", "swift", "FROM swift:latest"},
		{"CMakeLists.txt", "cpp", "FROM gcc:latest"},
		{"Makefile", "cpp", "FROM gcc:latest"},
		{"project.clj", "clojure", "FROM clojure:latest"},
		{"deps.edn", "clojure", "FROM clojure:latest"},
		{"build.gradle.kts", "kotlin", "FROM gradle:latest"},
	}

	for _, tt := range tests {
		t.Run(tt.lang, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, tt.file), []byte("x"), 0644); err != nil {
				t.Fatalf("write %s: %v", tt.file, err)
			}
			s := &Scaffolder{}

			err := s.Scaffold(dir, Options{}, &fakePrompter{confirm: true})
			if err != nil {
				t.Fatalf("scaffold: %v", err)
			}

			dockerfilePath := filepath.Join(dir, ".sandman", "Dockerfile")
			data, err := os.ReadFile(dockerfilePath)
			if err != nil {
				t.Fatalf("read Dockerfile: %v", err)
			}
			if !strings.Contains(string(data), tt.expected) {
				t.Errorf("expected %q in Dockerfile, got:\n%s", tt.expected, data)
			}
		})
	}
}

func TestScaffold_AutoDetectsDotNet(t *testing.T) {
	tests := []struct {
		file     string
		expected string
	}{
		{"app.csproj", "FROM mcr.microsoft.com/dotnet/sdk:latest"},
		{"app.fsproj", "FROM mcr.microsoft.com/dotnet/sdk:latest"},
		{"app.sln", "FROM mcr.microsoft.com/dotnet/sdk:latest"},
	}

	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, tt.file), []byte("x"), 0644); err != nil {
				t.Fatalf("write %s: %v", tt.file, err)
			}
			s := &Scaffolder{}

			err := s.Scaffold(dir, Options{}, &fakePrompter{confirm: true})
			if err != nil {
				t.Fatalf("scaffold: %v", err)
			}

			dockerfilePath := filepath.Join(dir, ".sandman", "Dockerfile")
			data, err := os.ReadFile(dockerfilePath)
			if err != nil {
				t.Fatalf("read Dockerfile: %v", err)
			}
			if !strings.Contains(string(data), tt.expected) {
				t.Errorf("expected %q in Dockerfile, got:\n%s", tt.expected, data)
			}
		})
	}
}

func TestScaffold_LangOverride(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example\n"), 0644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	s := &Scaffolder{}

	err := s.Scaffold(dir, Options{Lang: "node"}, &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	dockerfilePath := filepath.Join(dir, ".sandman", "Dockerfile")
	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	if !strings.Contains(string(data), "FROM node:latest") {
		t.Errorf("expected node base image, got:\n%s", data)
	}
}

func TestScaffold_FromImageOverride(t *testing.T) {
	dir := t.TempDir()
	s := &Scaffolder{}

	err := s.Scaffold(dir, Options{Lang: "go", FromImage: "my-image:latest"}, &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	dockerfilePath := filepath.Join(dir, ".sandman", "Dockerfile")
	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	if !strings.Contains(string(data), "FROM my-image:latest") {
		t.Errorf("expected custom base image, got:\n%s", data)
	}
}

func TestScaffold_ExistingDirectoryBlocksWithoutConfirmation(t *testing.T) {
	dir := t.TempDir()
	sandmanDir := filepath.Join(dir, ".sandman")
	if err := os.MkdirAll(sandmanDir, 0755); err != nil {
		t.Fatalf("create .sandman: %v", err)
	}
	s := &Scaffolder{}

	err := s.Scaffold(dir, Options{}, &fakePrompter{confirm: false})
	if err == nil {
		t.Fatal("expected error when user declines overwrite")
	}
}

func TestScaffold_AmbiguousDetectionPrompts(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example\n"), 0644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
	s := &Scaffolder{}

	err := s.Scaffold(dir, Options{}, &fakePrompter{confirm: true, selected: "go"})
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	dockerfilePath := filepath.Join(dir, ".sandman", "Dockerfile")
	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	if !strings.Contains(string(data), "FROM golang:latest") {
		t.Errorf("expected golang base image after selection, got:\n%s", data)
	}
}

func TestScaffold_FailedDetectionPrompts(t *testing.T) {
	dir := t.TempDir()
	s := &Scaffolder{}

	err := s.Scaffold(dir, Options{}, &fakePrompter{confirm: true, selected: "python"})
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	dockerfilePath := filepath.Join(dir, ".sandman", "Dockerfile")
	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	if !strings.Contains(string(data), "FROM python:latest") {
		t.Errorf("expected python base image after selection, got:\n%s", data)
	}
}

func TestScaffold_UnknownLang_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	s := &Scaffolder{}

	err := s.Scaffold(dir, Options{Lang: "goo"}, &fakePrompter{confirm: true})
	if err == nil {
		t.Fatal("expected error for unknown language")
	}
	if !strings.Contains(err.Error(), "unknown language") {
		t.Errorf("expected 'unknown language' error, got: %v", err)
	}
}

func TestScaffold_PromptMd_IsSeeded(t *testing.T) {
	dir := t.TempDir()
	s := &Scaffolder{}

	err := s.Scaffold(dir, Options{Lang: "go"}, &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".sandman", "prompt.md"))
	if err != nil {
		t.Fatalf("read prompt.md: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "# Context") {
		t.Errorf("prompt.md missing # Context header, got:\n%s", content)
	}
	if !strings.Contains(content, "{{ISSUE_NUMBER}}") {
		t.Errorf("prompt.md missing built-in key example, got:\n%s", content)
	}
	if !strings.Contains(content, "{{.PromptFile}}") {
		t.Errorf("prompt.md missing prompt file contract, got:\n%s", content)
	}
	if !strings.Contains(content, "promptArgs") {
		t.Errorf("prompt.md missing promptArgs mention, got:\n%s", content)
	}
}

func TestScaffold_AllPresetsHaveInstallCommands(t *testing.T) {
	for agent := range config.BuiltInAgentPresets {
		if _, ok := agentInstallCommands[agent]; !ok {
			t.Errorf("agent %q has no install command in agentInstallCommands map", agent)
		}
	}
	if len(agentInstallCommands) != len(config.BuiltInAgentPresets) {
		t.Errorf("agentInstallCommands has %d entries but BuiltInAgentPresets has %d", len(agentInstallCommands), len(config.BuiltInAgentPresets))
	}
}

func TestScaffold_InvalidAgent_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	s := &Scaffolder{}

	err := s.Scaffold(dir, Options{Lang: "go", Agent: "unknown"}, &fakePrompter{confirm: true})
	if err == nil {
		t.Fatal("expected error for unknown agent")
	}
	if !strings.Contains(err.Error(), "unknown agent") {
		t.Errorf("expected 'unknown agent' error, got: %v", err)
	}
}

func TestScaffold_AgentDockerfileHasInstall(t *testing.T) {
	dir := t.TempDir()
	s := &Scaffolder{}

	err := s.Scaffold(dir, Options{Lang: "go", Agent: "claude-code"}, &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "claude-code") {
		t.Errorf("Dockerfile missing claude-code install, got:\n%s", content)
	}
}

func TestScaffold_AllAgentPresets_GenerateUsableFiles(t *testing.T) {
	for agent := range config.BuiltInAgentPresets {
		t.Run(agent, func(t *testing.T) {
			dir := t.TempDir()
			s := &Scaffolder{}

			err := s.Scaffold(dir, Options{Lang: "go", Agent: agent}, &fakePrompter{confirm: true})
			if err != nil {
				t.Fatalf("scaffold: %v", err)
			}

			configPath := filepath.Join(dir, ".sandman", "config.yaml")
			cfg, err := config.Load(configPath)
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			if cfg.Agent != agent {
				t.Errorf("expected agent %q, got %q", agent, cfg.Agent)
			}
			resolved, err := cfg.ResolveAgentProvider(agent)
			if err != nil {
				t.Fatalf("resolve agent %q: %v", agent, err)
			}
			if resolved.Preset != agent {
				t.Errorf("expected preset %q, got %q", agent, resolved.Preset)
			}

			dockerfileData, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
			if err != nil {
				t.Fatalf("read Dockerfile: %v", err)
			}
			content := string(dockerfileData)
			if !strings.Contains(content, agent) {
				t.Errorf("Dockerfile missing %q, got:\n%s", agent, content)
			}
			if !strings.Contains(content, "FROM golang:latest") {
				t.Errorf("Dockerfile missing base image, got:\n%s", content)
			}
		})
	}
}

func TestScaffold_AgentFlagSelectsConfigPreset(t *testing.T) {
	dir := t.TempDir()
	s := &Scaffolder{}

	err := s.Scaffold(dir, Options{Lang: "go", Agent: "claude-code"}, &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	configPath := filepath.Join(dir, ".sandman", "config.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "preset: claude-code") {
		t.Errorf("config missing expected preset, got:\n%s", content)
	}
	if strings.Contains(content, "preset: opencode") {
		t.Errorf("config should not contain default preset, got:\n%s", content)
	}
}

func TestScaffold_MakefileDeprioritizedWhenOtherLanguageDetected(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example\n"), 0644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte("all:\n"), 0644); err != nil {
		t.Fatalf("write Makefile: %v", err)
	}
	s := &Scaffolder{}

	// Should NOT prompt — Makefile is deprioritized when go.mod exists
	err := s.Scaffold(dir, Options{}, &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	dockerfilePath := filepath.Join(dir, ".sandman", "Dockerfile")
	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	if !strings.Contains(string(data), "FROM golang:latest") {
		t.Errorf("expected golang base image when Makefile + go.mod exist, got:\n%s", data)
	}
}

func TestScaffold_CMakeListsNotDeprioritizedWhenOtherLanguageDetected(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example\n"), 0644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "CMakeLists.txt"), []byte("cmake_minimum_required(VERSION 3.0)\n"), 0644); err != nil {
		t.Fatalf("write CMakeLists.txt: %v", err)
	}
	s := &Scaffolder{}

	// Should prompt because CMakeLists.txt is a strong C/C++ signal
	err := s.Scaffold(dir, Options{}, &fakePrompter{confirm: true, selected: "go"})
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	dockerfilePath := filepath.Join(dir, ".sandman", "Dockerfile")
	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	if !strings.Contains(string(data), "FROM golang:latest") {
		t.Errorf("expected golang after selection, got:\n%s", data)
	}
}
