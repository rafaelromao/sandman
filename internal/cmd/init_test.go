package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInit_CreatesSandmanFiles(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	var out bytes.Buffer
	cmd := NewInitCmd()
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{"--build-tools", "generic"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, ".sandman", "config.yaml")); err != nil {
		t.Errorf("config.yaml not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "Dockerfile")); err != nil {
		t.Errorf("Dockerfile not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "prompt.md")); err != nil {
		t.Errorf("prompt.md not created: %v", err)
	}
}

func TestInit_GenericBuildToolsScaffoldsPinnedDockerfile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	var out bytes.Buffer
	cmd := NewInitCmd()
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{"--build-tools", "generic"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dockerfileData, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile := string(dockerfileData)
	if !strings.Contains(dockerfile, "# sandman build-tools: generic") {
		t.Fatalf("Dockerfile missing build-tools metadata, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "# sandman agent-provider: opencode") {
		t.Fatalf("Dockerfile missing agent metadata, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "# sandman tool-version: 1.15.0") {
		t.Fatalf("Dockerfile missing pinned agent version, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "FROM debian:bookworm-slim") {
		t.Fatalf("Dockerfile missing Debian base image, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "RUN curl -fsSL https://github.com/jdx/mise/releases/download/v2026.5.8/mise-v2026.5.8-linux-x64.tar.gz") {
		t.Fatalf("Dockerfile missing mise install, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "RUN npm install -g opencode-ai@1.15.0") {
		t.Fatalf("Dockerfile missing pinned opencode install, got:\n%s", dockerfile)
	}

	promptData, err := os.ReadFile(filepath.Join(dir, ".sandman", "prompt.md"))
	if err != nil {
		t.Fatalf("read prompt.md: %v", err)
	}
	prompt := string(promptData)
	if !strings.Contains(prompt, "mise first") {
		t.Fatalf("prompt.md missing mise guidance, got:\n%s", prompt)
	}
}

func TestInit_AgentFlagSelectsConfigPreset(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	var out bytes.Buffer
	cmd := NewInitCmd()
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{"--build-tools", "generic", "--agent", "claude-code"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	configData, err := os.ReadFile(filepath.Join(dir, ".sandman", "config.yaml"))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	if !strings.Contains(string(configData), "preset: claude-code") {
		t.Errorf("config missing claude-code preset, got:\n%s", configData)
	}

	dockerfileData, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	if !strings.Contains(string(dockerfileData), "@anthropic-ai/claude-code@2.1.142") {
		t.Errorf("Dockerfile missing pinned claude-code install, got:\n%s", dockerfileData)
	}
}

func TestInit_ExistingDirectoryPrompts(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.MkdirAll(filepath.Join(dir, ".sandman"), 0755); err != nil {
		t.Fatalf("create .sandman: %v", err)
	}

	var out bytes.Buffer
	cmd := NewInitCmd()
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader("n\n"))
	cmd.SetArgs([]string{"--build-tools", "generic"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when declining overwrite")
	}
}
