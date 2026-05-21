package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/prompt"
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

func TestScaffold_SharedPackagesIncludeOpensshClient(t *testing.T) {
	for _, preset := range []string{"generic", "go", "node", "python", "rust"} {
		t.Run(preset, func(t *testing.T) {
			dir := t.TempDir()
			s := &Scaffolder{}

			err := s.Scaffold(dir, Options{BuildTools: preset}, &fakePrompter{confirm: true})
			if err != nil {
				t.Fatalf("scaffold: %v", err)
			}

			data, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
			if err != nil {
				t.Fatalf("read Dockerfile: %v", err)
			}
			content := string(data)
			if !strings.Contains(content, " openssh-client ") {
				t.Fatalf("Dockerfile missing openssh-client shared package, got:\n%s", content)
			}
		})
	}
}

func TestScaffold_NodePresetWritesPinnedDockerfile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"demo","engines":{"node":"20"}}`), 0644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}

	s := &Scaffolder{}
	wantNodeVersion, err := s.resolveNodeVersion(dir, "", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolve node version: %v", err)
	}

	err = s.Scaffold(dir, Options{Agent: "opencode"}, &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	dockerfilePath := filepath.Join(dir, ".sandman", "Dockerfile")
	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "# sandman build-tools: node") {
		t.Fatalf("Dockerfile missing build-tools metadata, got:\n%s", content)
	}
	if !strings.Contains(content, "# sandman node-version: "+wantNodeVersion) {
		t.Fatalf("Dockerfile missing node-version metadata, got:\n%s", content)
	}
	if !strings.Contains(content, "RUN mise use -g --pin node@"+wantNodeVersion) {
		t.Fatalf("Dockerfile missing pinned node install, got:\n%s", content)
	}
	if !strings.Contains(content, "RUN corepack enable") {
		t.Fatalf("Dockerfile missing corepack enable, got:\n%s", content)
	}
	if !strings.Contains(content, "RUN npm install -g opencode-ai@"+DefaultBuiltInAgentVersion("opencode")) {
		t.Fatalf("Dockerfile missing pinned opencode install, got:\n%s", content)
	}
	if !strings.Contains(content, "RUN MISE_VERSION="+DefaultMISEVersion+" curl https://mise.run | MISE_INSTALL_PATH=/usr/local/bin/mise sh") {
		t.Fatalf("Dockerfile missing pinned mise install, got:\n%s", content)
	}
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
	if !strings.Contains(content, "RUN MISE_VERSION="+DefaultMISEVersion+" curl https://mise.run | MISE_INSTALL_PATH=/usr/local/bin/mise sh") {
		t.Fatalf("Dockerfile missing pinned mise install, got:\n%s", content)
	}
	if !strings.Contains(content, " gh ") {
		t.Fatalf("Dockerfile missing gh shared package, got:\n%s", content)
	}
	if strings.Contains(content, "/root/.local/share/mise") {
		t.Fatalf("Dockerfile should not depend on /root mise paths, got:\n%s", content)
	}
	for _, envLine := range []string{
		"ENV MISE_GLOBAL_CONFIG_FILE=\"/etc/mise/config.toml\"",
		"ENV MISE_CONFIG_DIR=\"/etc/mise\"",
		"ENV MISE_DATA_DIR=\"/usr/local/share/mise\"",
		"ENV MISE_STATE_DIR=\"/usr/local/share/mise/state\"",
		"ENV MISE_CACHE_DIR=\"/usr/local/share/mise/cache\"",
		"ENV PATH=\"/usr/local/share/mise/shims:/usr/local/share/mise/bin:$PATH\"",
	} {
		if !strings.Contains(content, envLine) {
			t.Fatalf("Dockerfile missing %q, got:\n%s", envLine, content)
		}
	}

	promptPath := filepath.Join(dir, ".sandman", "prompt.md")
	promptData, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("read prompt.md: %v", err)
	}
	if got, want := string(promptData), prompt.DefaultPrompt(); got != want {
		t.Fatalf("prompt.md mismatch\nwant:\n%s\ngot:\n%s", want, got)
	}

	configPath := filepath.Join(dir, ".sandman", "config.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Git.AuthorName != config.DefaultGitAuthorName {
		t.Fatalf("git.author_name: got %q, want %q", cfg.Git.AuthorName, config.DefaultGitAuthorName)
	}
	if cfg.Git.AuthorEmail != config.DefaultGitAuthorEmail {
		t.Fatalf("git.author_email: got %q, want %q", cfg.Git.AuthorEmail, config.DefaultGitAuthorEmail)
	}
	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(configData), "author_name: "+config.DefaultGitAuthorName) || !strings.Contains(string(configData), "author_email: "+config.DefaultGitAuthorEmail) {
		t.Fatalf("scaffolded config should persist default git author settings, got:\n%s", configData)
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
		{name: "repo falls back to latest", selector: "repo", wantPin: DefaultBuiltInAgentVersion("codex")},
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

func TestScaffold_ResolvesNodeVersionSelectors(t *testing.T) {
	tests := []struct {
		name     string
		selector string
		wantPin  string
	}{
		{name: "latest", selector: "latest"},
		{name: "lts", selector: "lts"},
		{name: "repo", selector: "repo"},
		{name: "shorthand", selector: "20"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"demo","engines":{"node":"20"}}`), 0644); err != nil {
				t.Fatalf("write package.json: %v", err)
			}

			s := &Scaffolder{}
			resolvedPin, err := s.resolveNodeVersion(dir, tt.selector, &fakePrompter{confirm: true})
			if err != nil {
				t.Fatalf("resolve node version: %v", err)
			}

			wantPin := tt.wantPin
			if wantPin == "" {
				wantPin = resolvedPin
			}

			if err := s.Scaffold(dir, Options{BuildTools: "node", Agent: "codex", ToolVersion: tt.selector}, &fakePrompter{confirm: true}); err != nil {
				t.Fatalf("scaffold: %v", err)
			}

			dockerfileData, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
			if err != nil {
				t.Fatalf("read Dockerfile: %v", err)
			}
			content := string(dockerfileData)
			if !strings.Contains(content, "# sandman node-version: "+wantPin) {
				t.Fatalf("Dockerfile missing node pin %q, got:\n%s", wantPin, content)
			}
			if !strings.Contains(content, "RUN mise use -g --pin node@"+wantPin) {
				t.Fatalf("Dockerfile missing node install pin %q, got:\n%s", wantPin, content)
			}
			if !strings.Contains(content, "RUN corepack enable") {
				t.Fatalf("Dockerfile missing corepack enable, got:\n%s", content)
			}
		})
	}
}

func TestScaffold_RepoSelectorFallsBackToLatest_WhenNoGoHints(t *testing.T) {
	dir := t.TempDir()
	s := &Scaffolder{}

	version, err := s.resolveGoVersion(dir, "repo", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolveGoVersion with repo selector: %v", err)
	}
	latest, err := s.resolveGoVersion(dir, "latest", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolveGoVersion with latest selector: %v", err)
	}
	if version != latest {
		t.Fatalf("expected repo fallback to latest (%q), got %q", latest, version)
	}
}

func TestScaffold_RepoSelectorFallsBackToLatest_WhenNoPythonHints(t *testing.T) {
	dir := t.TempDir()
	s := &Scaffolder{}

	version, err := s.resolvePythonVersion(dir, "repo", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolvePythonVersion with repo selector: %v", err)
	}
	latest, err := s.resolvePythonVersion(dir, "latest", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolvePythonVersion with latest selector: %v", err)
	}
	if version != latest {
		t.Fatalf("expected repo fallback to latest (%q), got %q", latest, version)
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
			if !strings.Contains(content, "RUN MISE_VERSION="+DefaultMISEVersion+" curl https://mise.run | MISE_INSTALL_PATH=/usr/local/bin/mise sh") {
				t.Fatalf("Dockerfile missing pinned mise install, got:\n%s", content)
			}
			if !strings.Contains(content, " gh ") {
				t.Fatalf("Dockerfile missing gh shared package, got:\n%s", content)
			}
		})
	}
}

func TestScaffold_AllAgentPresets_GenerateGoPresetFiles(t *testing.T) {
	for agent := range config.BuiltInAgentPresets {
		t.Run(agent, func(t *testing.T) {
			dir := t.TempDir()
			s := &Scaffolder{}
			wantGoVersion, err := s.resolveGoVersion(dir, "", &fakePrompter{confirm: true})
			if err != nil {
				t.Fatalf("resolve go version: %v", err)
			}

			if err := s.Scaffold(dir, Options{BuildTools: "go", Agent: agent}, &fakePrompter{confirm: true}); err != nil {
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
			if !strings.Contains(content, "RUN mise use -g --pin go@"+wantGoVersion) {
				t.Fatalf("Dockerfile missing pinned go install %q, got:\n%s", wantGoVersion, content)
			}
			if !strings.Contains(content, "ENV GOPATH=\"/.local/share/go\"") {
				t.Fatalf("Dockerfile missing GOPATH env, got:\n%s", content)
			}
			if !strings.Contains(content, "ENV GOMODCACHE=\"/.cache/go/pkg/mod\"") {
				t.Fatalf("Dockerfile missing GOMODCACHE env, got:\n%s", content)
			}
		})
	}
}

func TestScaffold_UnknownBuildToolsPreset_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	s := &Scaffolder{}

	err := s.Scaffold(dir, Options{BuildTools: "nonexistent"}, &fakePrompter{confirm: true})
	if err == nil {
		t.Fatal("expected error for unknown build-tools preset")
	}
	if !strings.Contains(err.Error(), "unknown build-tools preset") {
		t.Fatalf("expected unknown build-tools preset error, got: %v", err)
	}
}

func TestScaffold_PythonPresetWritesPinnedDockerfile(t *testing.T) {
	dir := t.TempDir()
	s := &Scaffolder{}

	err := s.Scaffold(dir, Options{BuildTools: "python"}, &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	dockerfilePath := filepath.Join(dir, ".sandman", "Dockerfile")
	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "# sandman build-tools: python") {
		t.Fatalf("Dockerfile missing build-tools metadata, got:\n%s", content)
	}
	if !strings.Contains(content, "# sandman agent-provider: opencode") {
		t.Fatalf("Dockerfile missing agent metadata, got:\n%s", content)
	}
	if !strings.Contains(content, "# sandman python-version:") {
		t.Fatalf("Dockerfile missing python-version metadata, got:\n%s", content)
	}
	if !strings.Contains(content, "# sandman tool-version: "+DefaultBuiltInAgentVersion("opencode")) {
		t.Fatalf("Dockerfile missing pinned version, got:\n%s", content)
	}
	if !strings.Contains(content, "FROM debian:bookworm-slim") {
		t.Fatalf("Dockerfile missing Debian base image, got:\n%s", content)
	}
	if !strings.Contains(content, "RUN mise use -g --pin python@") {
		t.Fatalf("Dockerfile missing pinned python install, got:\n%s", content)
	}
	if !strings.Contains(content, "RUN pip3 install uv") {
		t.Fatalf("Dockerfile missing uv install, got:\n%s", content)
	}
	if !strings.Contains(content, "RUN npm install -g opencode-ai@"+DefaultBuiltInAgentVersion("opencode")) {
		t.Fatalf("Dockerfile missing pinned opencode install, got:\n%s", content)
	}
	if !strings.Contains(content, "RUN MISE_VERSION="+DefaultMISEVersion+" curl https://mise.run | MISE_INSTALL_PATH=/usr/local/bin/mise sh") {
		t.Fatalf("Dockerfile missing pinned mise install, got:\n%s", content)
	}

	promptPath := filepath.Join(dir, ".sandman", "prompt.md")
	promptData, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("read prompt.md: %v", err)
	}
	if got, want := string(promptData), prompt.DefaultPrompt(); got != want {
		t.Fatalf("prompt.md mismatch\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestScaffold_RustPresetWritesPinnedDockerfile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rust-toolchain.toml"), []byte("[toolchain]\nchannel = \"1.85\"\n"), 0644); err != nil {
		t.Fatalf("write rust-toolchain.toml: %v", err)
	}

	s := &Scaffolder{}
	wantRustVersion, err := s.resolveRustVersion(dir, "repo", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolve rust version: %v", err)
	}
	if err := s.Scaffold(dir, Options{Agent: "opencode"}, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	dockerfilePath := filepath.Join(dir, ".sandman", "Dockerfile")
	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "# sandman build-tools: rust") {
		t.Fatalf("Dockerfile missing rust build-tools metadata, got:\n%s", content)
	}
	if !strings.Contains(content, "# sandman rust-version: "+wantRustVersion) {
		t.Fatalf("Dockerfile missing rust-version metadata, got:\n%s", content)
	}
	if !strings.Contains(content, "RUN mise use -g --pin rust@"+wantRustVersion) {
		t.Fatalf("Dockerfile missing pinned rust install, got:\n%s", content)
	}
	if !strings.Contains(content, "RUN npm install -g opencode-ai@"+DefaultBuiltInAgentVersion("opencode")) {
		t.Fatalf("Dockerfile missing pinned opencode install, got:\n%s", content)
	}
	if !strings.Contains(content, "RUN MISE_VERSION="+DefaultMISEVersion+" curl https://mise.run | MISE_INSTALL_PATH=/usr/local/bin/mise sh") {
		t.Fatalf("Dockerfile missing pinned mise install, got:\n%s", content)
	}
}

func TestScaffold_AllAgentPresets_GeneratePythonPresetFiles(t *testing.T) {
	for agent := range config.BuiltInAgentPresets {
		t.Run(agent, func(t *testing.T) {
			dir := t.TempDir()
			s := &Scaffolder{}
			wantPythonVersion, err := s.resolvePythonVersion(dir, "", &fakePrompter{confirm: true})
			if err != nil {
				t.Fatalf("resolve python version: %v", err)
			}

			if err := s.Scaffold(dir, Options{BuildTools: "python", Agent: agent}, &fakePrompter{confirm: true}); err != nil {
				t.Fatalf("scaffold: %v", err)
			}

			configPath := filepath.Join(dir, ".sandman", "config.yaml")
			cfg, err := config.Load(configPath)
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			if cfg.BuildTools != "python" {
				t.Errorf("expected build tools %q, got %q", "python", cfg.BuildTools)
			}

			dockerfileData, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
			if err != nil {
				t.Fatalf("read Dockerfile: %v", err)
			}
			content := string(dockerfileData)
			if !strings.Contains(content, "# sandman build-tools: python") {
				t.Fatalf("Dockerfile missing python build-tools metadata, got:\n%s", content)
			}
			if !strings.Contains(content, "RUN mise use -g --pin python@"+wantPythonVersion) {
				t.Fatalf("Dockerfile missing pinned python install %q, got:\n%s", wantPythonVersion, content)
			}
			if !strings.Contains(content, "RUN pip3 install uv") {
				t.Fatalf("Dockerfile missing uv install, got:\n%s", content)
			}
		})
	}
}

func TestScaffold_PythonRepoAutoDetect(t *testing.T) {
	tests := []struct {
		name    string
		setupFn func(dir string)
		want    string
	}{
		{
			name: "pyproject.toml",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\nname = \"demo\"\n"), 0644)
			},
			want: "python",
		},
		{
			name: "setup.py",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, "setup.py"), []byte("from setuptools import setup\n"), 0644)
			},
			want: "python",
		},
		{
			name: "Pipfile",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, "Pipfile"), []byte("[packages]\n"), 0644)
			},
			want: "python",
		},
		{
			name: "setup.cfg",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, "setup.cfg"), []byte("[metadata]\nname = demo\n"), 0644)
			},
			want: "python",
		},
		{
			name: ".python-version",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, ".python-version"), []byte("3.12\n"), 0644)
			},
			want: "python",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			tt.setupFn(dir)

			s := &Scaffolder{}
			preset, err := s.resolveBuildToolsPreset(dir, Options{}, &fakePrompter{confirm: true})
			if err != nil {
				t.Fatalf("resolve build tools preset: %v", err)
			}
			if preset.Name != tt.want {
				t.Errorf("expected preset %q, got %q", tt.want, preset.Name)
			}
		})
	}
}

func TestScaffold_RustRepoAutoDetect(t *testing.T) {
	tests := []struct {
		name    string
		setupFn func(dir string)
	}{
		{
			name: "Cargo.toml rust-version",
			setupFn: func(dir string) {
				_ = os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte("[package]\nname = \"demo\"\nrust-version = \"1.85\"\n"), 0644)
			},
		},
		{
			name: "rust-toolchain.toml",
			setupFn: func(dir string) {
				_ = os.WriteFile(filepath.Join(dir, "rust-toolchain.toml"), []byte("[toolchain]\nchannel = \"stable\"\n"), 0644)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			tt.setupFn(dir)

			s := &Scaffolder{}
			preset, err := s.resolveBuildToolsPreset(dir, Options{}, &fakePrompter{confirm: true})
			if err != nil {
				t.Fatalf("resolve build tools preset: %v", err)
			}
			if preset.Name != "rust" {
				t.Errorf("expected preset %q, got %q", "rust", preset.Name)
			}
		})
	}
}

func TestScaffold_NodeRepoAutoDetect(t *testing.T) {
	tests := []struct {
		name    string
		setupFn func(dir string)
		want    string
	}{
		{
			name: "package.json engines",
			setupFn: func(dir string) {
				_ = os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"demo","engines":{"node":"20"}}`), 0644)
			},
			want: "node",
		},
		{
			name: "nvmrc",
			setupFn: func(dir string) {
				_ = os.WriteFile(filepath.Join(dir, ".nvmrc"), []byte("20\n"), 0644)
			},
			want: "node",
		},
		{
			name: "tool-versions",
			setupFn: func(dir string) {
				_ = os.WriteFile(filepath.Join(dir, ".tool-versions"), []byte("node 20\n"), 0644)
			},
			want: "node",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			tt.setupFn(dir)

			s := &Scaffolder{}
			preset, err := s.resolveBuildToolsPreset(dir, Options{}, &fakePrompter{confirm: true})
			if err != nil {
				t.Fatalf("resolve build tools preset: %v", err)
			}
			if preset.Name != tt.want {
				t.Errorf("expected preset %q, got %q", tt.want, preset.Name)
			}
		})
	}
}

func TestScaffold_GenericBuildToolsOverridesNodeRepoHints(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"demo","engines":{"node":"20"}}`), 0644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}

	s := &Scaffolder{}
	preset, err := s.resolveBuildToolsPreset(dir, Options{BuildTools: "generic"}, &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolve build tools preset: %v", err)
	}
	if preset.Name != "generic" {
		t.Fatalf("expected explicit generic preset, got %q", preset.Name)
	}
}

func TestScaffold_GoPresetTakesPriorityOverPython(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/demo\n\ngo 1.24\n"), 0644)
	os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\nname = \"demo\"\n"), 0644)

	s := &Scaffolder{}
	preset, err := s.resolveBuildToolsPreset(dir, Options{}, &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolve build tools preset: %v", err)
	}
	if preset.Name != "go" {
		t.Errorf("expected Go preset to take priority over Python, got %q", preset.Name)
	}
}

func TestScaffold_RustPresetTakesPriorityOverGo(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/demo\n\ngo 1.24\n"), 0644)
	os.WriteFile(filepath.Join(dir, "rust-toolchain.toml"), []byte("[toolchain]\nchannel = \"1.85\"\n"), 0644)

	s := &Scaffolder{}
	preset, err := s.resolveBuildToolsPreset(dir, Options{}, &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolve build tools preset: %v", err)
	}
	if preset.Name != "rust" {
		t.Errorf("expected Rust preset to take priority over Go, got %q", preset.Name)
	}
}

func TestScaffold_ResolvesRustVersionSelectors(t *testing.T) {
	tests := []struct {
		name     string
		selector string
		setupFn  func(dir string)
	}{
		{name: "latest", selector: "latest"},
		{name: "lts", selector: "lts"},
		{name: "semver shorthand", selector: "1.85"},
		{
			name:     "repo from rust-toolchain",
			selector: "repo",
			setupFn: func(dir string) {
				_ = os.WriteFile(filepath.Join(dir, "rust-toolchain.toml"), []byte("[toolchain]\nchannel = \"1.85\"\n"), 0644)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.setupFn != nil {
				tt.setupFn(dir)
			}

			s := &Scaffolder{}
			resolvedPin, err := s.resolveRustVersion(dir, tt.selector, &fakePrompter{confirm: true})
			if err != nil {
				t.Fatalf("resolve rust version: %v", err)
			}
			if resolvedPin == "" {
				t.Fatal("expected rust version pin")
			}

			if err := s.Scaffold(dir, Options{BuildTools: "rust", Agent: "opencode", ToolVersion: tt.selector}, &fakePrompter{confirm: true}); err != nil {
				t.Fatalf("scaffold: %v", err)
			}
			dockerfileData, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
			if err != nil {
				t.Fatalf("read Dockerfile: %v", err)
			}
			content := string(dockerfileData)
			if !strings.Contains(content, "# sandman rust-version: "+resolvedPin) {
				t.Fatalf("Dockerfile missing rust pin %q, got:\n%s", resolvedPin, content)
			}
			if !strings.Contains(content, "RUN mise use -g --pin rust@"+resolvedPin) {
				t.Fatalf("Dockerfile missing rust install pin %q, got:\n%s", resolvedPin, content)
			}
		})
	}
}

func TestScaffold_RustVersionPromptSelectionWorks(t *testing.T) {
	dir := t.TempDir()
	s := &Scaffolder{}

	resolvedPin, err := s.resolveRustVersion(dir, "", &fakePrompter{selected: "lts"})
	if err != nil {
		t.Fatalf("resolve rust version: %v", err)
	}
	if resolvedPin == "" {
		t.Fatal("expected rust version pin")
	}

	if err := s.Scaffold(dir, Options{BuildTools: "rust", Agent: "opencode", ToolVersion: ""}, &fakePrompter{selected: "lts"}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	dockerfileData, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	content := string(dockerfileData)
	if !strings.Contains(content, "# sandman rust-version: "+resolvedPin) {
		t.Fatalf("Dockerfile missing rust pin %q, got:\n%s", resolvedPin, content)
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
