package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/scaffold"
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

	configData, err := os.ReadFile(filepath.Join(dir, ".sandman", "config.yaml"))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	if !strings.Contains(string(configData), "build_tools: generic") {
		t.Fatalf("config missing generic build_tools preset, got:\n%s", configData)
	}
	if !strings.Contains(string(configData), "review_command: /oc review") {
		t.Fatalf("config missing review_command default, got:\n%s", configData)
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
	if !strings.Contains(dockerfile, "# sandman tool-version: "+scaffold.DefaultBuiltInAgentVersion("opencode")) {
		t.Fatalf("Dockerfile missing pinned agent version, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "FROM debian:bookworm-slim") {
		t.Fatalf("Dockerfile missing Debian base image, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "RUN MISE_VERSION="+scaffold.DefaultMISEVersion+" curl https://mise.run | MISE_INSTALL_PATH=/usr/local/bin/mise sh") {
		t.Fatalf("Dockerfile missing pinned mise install, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, " gh ") {
		t.Fatalf("Dockerfile missing gh shared package, got:\n%s", dockerfile)
	}
	if strings.Contains(dockerfile, "/root/.local/share/mise") {
		t.Fatalf("Dockerfile should not depend on /root mise paths, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "RUN npm install -g opencode-ai@"+scaffold.DefaultBuiltInAgentVersion("opencode")) {
		t.Fatalf("Dockerfile missing pinned opencode install, got:\n%s", dockerfile)
	}

	promptData, err := os.ReadFile(filepath.Join(dir, ".sandman", "prompt.md"))
	if err != nil {
		t.Fatalf("read prompt.md: %v", err)
	}
	promptText := string(promptData)
	if want := prompt.DefaultPrompt(); promptText != want {
		t.Fatalf("prompt.md mismatch\nwant:\n%s\ngot:\n%s", want, promptText)
	}
}

func TestInit_PythonBuildToolsScaffoldsPinnedDockerfile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	var out bytes.Buffer
	cmd := NewInitCmd()
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{"--build-tools", "python"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	configData, err := os.ReadFile(filepath.Join(dir, ".sandman", "config.yaml"))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	if !strings.Contains(string(configData), "build_tools: python") {
		t.Fatalf("config missing python build_tools preset, got:\n%s", configData)
	}

	dockerfileData, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile := string(dockerfileData)
	if !strings.Contains(dockerfile, "# sandman build-tools: python") {
		t.Fatalf("Dockerfile missing build-tools metadata, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "# sandman python-version:") {
		t.Fatalf("Dockerfile missing python-version metadata, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "RUN mise use -g --pin python@") {
		t.Fatalf("Dockerfile missing pinned python install, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "RUN pip3 install uv") {
		t.Fatalf("Dockerfile missing uv install, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "FROM debian:bookworm-slim") {
		t.Fatalf("Dockerfile missing Debian base image, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "RUN MISE_VERSION="+scaffold.DefaultMISEVersion+" curl https://mise.run | MISE_INSTALL_PATH=/usr/local/bin/mise sh") {
		t.Fatalf("Dockerfile missing pinned mise install, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, " gh ") {
		t.Fatalf("Dockerfile missing gh shared package, got:\n%s", dockerfile)
	}

	promptData, err := os.ReadFile(filepath.Join(dir, ".sandman", "prompt.md"))
	if err != nil {
		t.Fatalf("read prompt.md: %v", err)
	}
	promptText := string(promptData)
	if want := prompt.DefaultPrompt(); promptText != want {
		t.Fatalf("prompt.md mismatch\nwant:\n%s\ngot:\n%s", want, promptText)
	}
}

func TestInit_DefaultsToPythonPresetForPythonRepo(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\nname = \"demo\"\n"), 0644); err != nil {
		t.Fatalf("write pyproject.toml: %v", err)
	}

	var out bytes.Buffer
	cmd := NewInitCmd()
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	configData, err := os.ReadFile(filepath.Join(dir, ".sandman", "config.yaml"))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	if !strings.Contains(string(configData), "build_tools: python") {
		t.Fatalf("config missing python build_tools preset, got:\n%s", configData)
	}

	dockerfileData, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile := string(dockerfileData)
	if !strings.Contains(dockerfile, "# sandman build-tools: python") {
		t.Fatalf("Dockerfile missing python build-tools metadata, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "# sandman python-version:") {
		t.Fatalf("Dockerfile missing python-version metadata, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "RUN mise use -g --pin python@") {
		t.Fatalf("Dockerfile missing pinned python install, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "RUN pip3 install uv") {
		t.Fatalf("Dockerfile missing uv install, got:\n%s", dockerfile)
	}
}

func TestInit_DefaultsToGoPresetForGoRepo(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/demo\n\ngo 1.24\n"), 0644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	var out bytes.Buffer
	cmd := NewInitCmd()
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	configData, err := os.ReadFile(filepath.Join(dir, ".sandman", "config.yaml"))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	if !strings.Contains(string(configData), "build_tools: go") {
		t.Fatalf("config missing go build_tools preset, got:\n%s", configData)
	}

	dockerfileData, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile := string(dockerfileData)
	if !strings.Contains(dockerfile, "# sandman build-tools: go") {
		t.Fatalf("Dockerfile missing go build-tools metadata, got:\n%s", dockerfile)
	}
	wantGoVersion := "1.24.13"
	if !strings.Contains(dockerfile, "RUN mise use -g --pin go@"+wantGoVersion) {
		t.Fatalf("Dockerfile missing pinned go install %q, got:\n%s", wantGoVersion, dockerfile)
	}
	if !strings.Contains(dockerfile, "ENV GOPATH=\"/.local/share/go\"") {
		t.Fatalf("Dockerfile missing GOPATH env, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "ENV GOMODCACHE=\"/.cache/go/pkg/mod\"") {
		t.Fatalf("Dockerfile missing GOMODCACHE env, got:\n%s", dockerfile)
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
	if !strings.Contains(string(configData), "build_tools: generic") {
		t.Fatalf("config missing generic build_tools preset, got:\n%s", configData)
	}
	if !strings.Contains(string(configData), "preset: claude-code") {
		t.Errorf("config missing claude-code preset, got:\n%s", configData)
	}

	dockerfileData, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	if !strings.Contains(string(dockerfileData), "@anthropic-ai/claude-code@"+scaffold.DefaultBuiltInAgentVersion("claude-code")) {
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
