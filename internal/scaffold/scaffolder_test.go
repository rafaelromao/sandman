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
	if !strings.Contains(content, "# sandman tool-version: "+DefaultBuiltInAgentVersion("opencode")) {
		t.Fatalf("Dockerfile missing pinned version, got:\n%s", content)
	}
	if !strings.Contains(content, "FROM debian:bookworm-slim") {
		t.Fatalf("Dockerfile missing Debian base image, got:\n%s", content)
	}
	if !strings.Contains(content, "RUN npm install -g opencode-ai@"+DefaultBuiltInAgentVersion("opencode")) {
		t.Fatalf("Dockerfile missing pinned opencode install, got:\n%s", content)
	}
	if !strings.Contains(content, "RUN curl -fsSL https://github.com/jdx/mise/releases/download/"+DefaultMISEVersion+"/mise-"+DefaultMISEVersion+"-linux-x64.tar.gz") {
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

func TestReadDockerfileMetadata_ParsesMiseVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Dockerfile")
	content := "# sandman build-tools: generic\n# sandman agent-provider: opencode\n# sandman tool-version: 1.15.0\n# sandman mise-version: " + DefaultMISEVersion + "\nFROM debian:bookworm-slim\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}

	meta, found, err := readDockerfileMetadata(path)
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	if !found {
		t.Fatal("expected metadata to be found")
	}
	if meta.MiseVersion != DefaultMISEVersion {
		t.Fatalf("expected mise version %q, got %q", DefaultMISEVersion, meta.MiseVersion)
	}
}

func TestScaffold_ResolvesToolVersionSelectors(t *testing.T) {
	tests := []struct {
		name     string
		selector string
		wantPin  string
	}{
		{name: "latest", selector: "latest", wantPin: DefaultBuiltInAgentVersion("codex")},
		{name: "lts", selector: "lts", wantPin: builtInAgentVersionCatalog["codex"][1]},
		{name: "semver shorthand", selector: "0.130", wantPin: DefaultBuiltInAgentVersion("codex")},
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
			if !strings.Contains(content, "RUN curl -fsSL https://github.com/jdx/mise/releases/download/"+DefaultMISEVersion+"/mise-"+DefaultMISEVersion+"-linux-x64.tar.gz") {
				t.Fatalf("Dockerfile missing mise install, got:\n%s", content)
			}
		})
	}
}

func TestScaffold_AllAgentPresets_GenerateGoPresetFiles(t *testing.T) {
	for agent := range config.BuiltInAgentPresets {
		t.Run(agent, func(t *testing.T) {
			dir := t.TempDir()
			s := &Scaffolder{}

			err := s.Scaffold(dir, Options{BuildTools: "go", Agent: agent}, &fakePrompter{confirm: true})
			if err != nil {
				t.Fatalf("scaffold: %v", err)
			}

			configPath := filepath.Join(dir, ".sandman", "config.yaml")
			cfg, err := config.Load(configPath)
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			if cfg.BuildTools != "go" {
				t.Errorf("expected build tools %q, got %q", "go", cfg.BuildTools)
			}

			dockerfileData, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
			if err != nil {
				t.Fatalf("read Dockerfile: %v", err)
			}
			content := string(dockerfileData)
			if !strings.Contains(content, "# sandman build-tools: go") {
				t.Fatalf("Dockerfile missing go build-tools metadata, got:\n%s", content)
			}
			if !strings.Contains(content, "RUN mise use -g --pin go@latest") {
				t.Fatalf("Dockerfile missing pinned go install, got:\n%s", content)
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

func TestValidateDockerfileMetadata_AllowsGoPreset(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".sandman"), 0755); err != nil {
		t.Fatalf("create .sandman: %v", err)
	}
	content := "# sandman build-tools: go\n# sandman agent-provider: opencode\n# sandman go-version: 1.24\n# sandman tool-version: 1.15.0\n# sandman mise-version: " + DefaultMISEVersion + "\nFROM debian:bookworm-slim\n"
	if err := os.WriteFile(filepath.Join(dir, ".sandman", "Dockerfile"), []byte(content), 0644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}

	if err := ValidateDockerfileMetadata(dir, "go", "opencode"); err != nil {
		t.Fatalf("validate metadata: %v", err)
	}
}
