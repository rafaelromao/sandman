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

func TestScaffold_GenericPresetWritesPinnedDockerfile(t *testing.T) {
	dir := t.TempDir()
	s := &Scaffolder{}

	err := s.Scaffold(dir, Options{BuildTools: "generic"}, &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	dockerfilePath := filepath.Join(dir, ".sandman", "Dockerfile")
	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "# sandman build-tools: generic") {
		t.Fatalf("Dockerfile missing build-tools metadata, got:\n%s", content)
	}
	if !strings.Contains(content, "# sandman agent-provider: opencode") {
		t.Fatalf("Dockerfile missing agent metadata, got:\n%s", content)
	}
	if !strings.Contains(content, "# sandman tool-version: 1.15.0") {
		t.Fatalf("Dockerfile missing pinned version, got:\n%s", content)
	}
	if !strings.Contains(content, "FROM debian:bookworm-slim") {
		t.Fatalf("Dockerfile missing Debian base image, got:\n%s", content)
	}
	if !strings.Contains(content, "RUN npm install -g opencode-ai@1.15.0") {
		t.Fatalf("Dockerfile missing pinned opencode install, got:\n%s", content)
	}
	if !strings.Contains(content, "RUN curl -fsSL https://github.com/jdx/mise/releases/download/v2026.5.8/mise-v2026.5.8-linux-x64.tar.gz") {
		t.Fatalf("Dockerfile missing mise install, got:\n%s", content)
	}

	promptPath := filepath.Join(dir, ".sandman", "prompt.md")
	promptData, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("read prompt.md: %v", err)
	}
	if !strings.Contains(string(promptData), "mise first") {
		t.Fatalf("prompt.md missing mise guidance, got:\n%s", promptData)
	}
}

func TestScaffold_ResolvesToolVersionSelectors(t *testing.T) {
	tests := []struct {
		name     string
		selector string
		wantPin  string
	}{
		{name: "latest", selector: "latest", wantPin: "0.130.0"},
		{name: "lts", selector: "lts", wantPin: "0.129.0"},
		{name: "semver shorthand", selector: "0.130", wantPin: "0.130.0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			s := &Scaffolder{}

			err := s.Scaffold(dir, Options{BuildTools: "generic", Agent: "codex", ToolVersion: tt.selector}, &fakePrompter{confirm: true})
			if err != nil {
				t.Fatalf("scaffold: %v", err)
			}

			dockerfileData, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
			if err != nil {
				t.Fatalf("read Dockerfile: %v", err)
			}
			content := string(dockerfileData)
			if !strings.Contains(content, "# sandman tool-version: "+tt.wantPin) {
				t.Fatalf("Dockerfile missing pinned version %q, got:\n%s", tt.wantPin, content)
			}
			if !strings.Contains(content, "@openai/codex@"+tt.wantPin) {
				t.Fatalf("Dockerfile missing codex install pin %q, got:\n%s", tt.wantPin, content)
			}
		})
	}
}

func TestScaffold_AllAgentPresets_GenerateUsableFiles(t *testing.T) {
	for agent := range config.BuiltInAgentPresets {
		t.Run(agent, func(t *testing.T) {
			dir := t.TempDir()
			s := &Scaffolder{}

			err := s.Scaffold(dir, Options{BuildTools: "generic", Agent: agent}, &fakePrompter{confirm: true})
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
			if !strings.Contains(content, "# sandman build-tools: generic") {
				t.Fatalf("Dockerfile missing build-tools metadata, got:\n%s", content)
			}
			if !strings.Contains(content, "# sandman agent-provider: "+agent) {
				t.Fatalf("Dockerfile missing agent metadata, got:\n%s", content)
			}
			if !strings.Contains(content, "FROM debian:bookworm-slim") {
				t.Fatalf("Dockerfile missing Debian base image, got:\n%s", content)
			}
			if !strings.Contains(content, "RUN curl -fsSL https://github.com/jdx/mise/releases/download/v2026.5.8/mise-v2026.5.8-linux-x64.tar.gz") {
				t.Fatalf("Dockerfile missing mise install, got:\n%s", content)
			}
		})
	}
}

func TestScaffold_UnknownBuildToolsPreset_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	s := &Scaffolder{}

	err := s.Scaffold(dir, Options{BuildTools: "python"}, &fakePrompter{confirm: true})
	if err == nil {
		t.Fatal("expected error for unknown build-tools preset")
	}
	if !strings.Contains(err.Error(), "unknown build-tools preset") {
		t.Fatalf("expected unknown build-tools preset error, got: %v", err)
	}
}
