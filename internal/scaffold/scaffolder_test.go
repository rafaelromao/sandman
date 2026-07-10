package scaffold

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

func TestScaffold_PersistsRuntimeDefaults(t *testing.T) {
	dir := t.TempDir()
	s := &Scaffolder{}

	if err := s.Scaffold(dir, Options{BuildTools: "generic"}, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".sandman", "config.yaml"))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "retries: 3") {
		t.Errorf("scaffolded config missing %q, got:\n%s", "retries: 3", content)
	}
	if !strings.Contains(content, "run_idle_timeout: 1800") {
		t.Errorf("scaffolded config missing %q, got:\n%s", "run_idle_timeout: 1800", content)
	}
	if !strings.Contains(content, "parallel: 1") {
		t.Errorf("scaffolded config missing %q, got:\n%s", "parallel: 1", content)
	}
	if !strings.Contains(content, "parallel_reviews: 1") {
		t.Errorf("scaffolded config missing %q, got:\n%s", "parallel_reviews: 1", content)
	}
	if !strings.Contains(content, "model: opencode/big-pickle") {
		t.Errorf("scaffolded config missing %q, got:\n%s", "model: opencode/big-pickle", content)
	}
	if !strings.Contains(content, "review_agent: opencode") {
		t.Errorf("scaffolded config missing %q, got:\n%s", "review_agent: opencode", content)
	}
	if !strings.Contains(content, "review_model: opencode/big-pickle") {
		t.Errorf("scaffolded config missing %q, got:\n%s", "review_model: opencode/big-pickle", content)
	}
}

func TestScaffold_ParallelReviewsSeeded(t *testing.T) {
	dir := t.TempDir()
	s := &Scaffolder{}

	if err := s.Scaffold(dir, Options{BuildTools: "generic", ParallelReviews: 8}, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".sandman", "config.yaml"))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "parallel_reviews: 8") {
		t.Errorf("scaffolded config missing %q, got:\n%s", "parallel_reviews: 8", content)
	}
}

func TestScaffold_ParallelReviewsDefault(t *testing.T) {
	dir := t.TempDir()
	s := &Scaffolder{}

	if err := s.Scaffold(dir, Options{BuildTools: "generic"}, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".sandman", "config.yaml"))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "parallel_reviews: 1") {
		t.Errorf("scaffolded config missing %q, got:\n%s", "parallel_reviews: 1", content)
	}
}

func TestScaffold_SharedPackagesIncludeOpensshClient(t *testing.T) {
	for _, preset := range []string{"generic", "go", "dotnet", "node", "python", "elixir"} {
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

func TestScaffold_SharedPackagesIncludeRipgrepAndYq(t *testing.T) {
	for _, preset := range []string{"generic", "go", "dotnet", "node", "python", "elixir"} {
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
			if !strings.Contains(content, " ripgrep ") {
				t.Fatalf("Dockerfile missing ripgrep shared package, got:\n%s", content)
			}
			if !strings.Contains(content, " yq ") {
				t.Fatalf("Dockerfile missing yq shared package, got:\n%s", content)
			}
		})
	}
}

func TestScaffold_AllPresetsIncludeRTK(t *testing.T) {
	for _, preset := range []string{"generic", "go", "dotnet", "node", "python", "elixir"} {
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
			want := "RUN curl -fsSL https://github.com/rtk-ai/rtk/releases/download/" + DefaultRTKVersion + "/rtk-x86_64-unknown-linux-musl.tar.gz | tar -xz -C /usr/local/bin"
			if !strings.Contains(content, want) {
				t.Fatalf("Dockerfile missing RTK install, got:\n%s", content)
			}
		})
	}
}

func TestScaffold_DotnetPresetWritesPinnedDockerfile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "global.json"), []byte(`{"sdk":{"version":"8.0.100"}}`), 0644); err != nil {
		t.Fatalf("write global.json: %v", err)
	}

	s := &Scaffolder{}
	wantDotnetVersion, err := s.resolveDotnetVersion(dir, "", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolve dotnet version: %v", err)
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
	if !strings.Contains(content, "# sandman build-tools: dotnet") {
		t.Fatalf("Dockerfile missing build-tools metadata, got:\n%s", content)
	}
	if !strings.Contains(content, "# sandman dotnet-version: "+wantDotnetVersion) {
		t.Fatalf("Dockerfile missing dotnet-version metadata, got:\n%s", content)
	}
	if !strings.Contains(content, "RUN mise use -g --pin dotnet@"+wantDotnetVersion) {
		t.Fatalf("Dockerfile missing pinned dotnet install, got:\n%s", content)
	}
	if !strings.Contains(content, "RUN npm install -g opencode-ai@"+DefaultBuiltInAgentVersion("opencode")) {
		t.Fatalf("Dockerfile missing pinned opencode install, got:\n%s", content)
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
	if !strings.Contains(content, "RUN git clone https://github.com/rafaelromao/codeindex /tmp/codeindex && pip3 install -e /tmp/codeindex --break-system-packages") {
		t.Fatalf("Dockerfile missing codeindex install, got:\n%s", content)
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
	if !strings.Contains(content, "# sandman default-agent: opencode") {
		t.Fatalf("Dockerfile missing default-agent metadata, got:\n%s", content)
	}
	if !strings.Contains(content, "# sandman installed-agents: opencode") {
		t.Fatalf("Dockerfile missing installed-agents metadata, got:\n%s", content)
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
	if cfg.Git.BaseBranch != "main" {
		t.Fatalf("git.base_branch: got %q, want %q", cfg.Git.BaseBranch, "main")
	}
	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(configData), "author_name:") || strings.Contains(string(configData), "author_email:") {
		t.Fatalf("scaffolded config should not persist git author settings, got:\n%s", configData)
	}
}

func TestScaffold_RustPresetWritesPinnedDockerfile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte("[package]\nname = \"demo\"\nversion = \"0.1.0\"\nrust-version = \"1.77.0\"\n"), 0644); err != nil {
		t.Fatalf("write Cargo.toml: %v", err)
	}

	s := &Scaffolder{}
	if err := s.Scaffold(dir, Options{BuildTools: "rust", ToolVersion: "1.95"}, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	dockerfilePath := filepath.Join(dir, ".sandman", "Dockerfile")
	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "# sandman build-tools: rust") {
		t.Fatalf("Dockerfile missing build-tools metadata, got:\n%s", content)
	}
	if !strings.Contains(content, "# sandman rust-version: 1.95.0") {
		t.Fatalf("Dockerfile missing rust-version metadata, got:\n%s", content)
	}
	if !strings.Contains(content, "RUN mise use -g --pin rust@1.95.0") {
		t.Fatalf("Dockerfile missing pinned rust install, got:\n%s", content)
	}
	if !strings.Contains(content, "RUN npm install -g opencode-ai@"+DefaultBuiltInAgentVersion("opencode")) {
		t.Fatalf("Dockerfile missing pinned opencode install, got:\n%s", content)
	}
}

func TestReadDockerfileMetadata_ParsesMiseVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Dockerfile")
	content := "# sandman build-tools: generic\n# sandman default-agent: opencode\n# sandman installed-agents: opencode\n# sandman mise-version: " + DefaultMISEVersion + "\nFROM debian:bookworm-slim\n"
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

func TestResolveVersion_RustResolver_Selectors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte("[package]\nname = \"demo\"\nversion = \"0.1.0\"\nrust-version = \"1.77.0\"\n"), 0644); err != nil {
		t.Fatalf("write Cargo.toml: %v", err)
	}

	tests := []struct {
		name     string
		selector string
		want     string
	}{
		{name: "repo", selector: "repo", want: "1.77.0"},
		{name: "latest", selector: "latest", want: "1.96.1"},
		{name: "lts", selector: "lts", want: "1.95.0"},
		{name: "shorthand", selector: "1.95", want: "1.95.0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveVersion(rustResolver, dir, tt.selector, &fakePrompter{confirm: true})
			if err != nil {
				t.Fatalf("resolveVersion %s: %v", tt.selector, err)
			}
			if got != tt.want {
				t.Fatalf("resolveVersion %s: got %q, want %q", tt.selector, got, tt.want)
			}
		})
	}
}

func TestResolveVersion_RustResolver_InteractiveSelection(t *testing.T) {
	dir := t.TempDir()
	got, err := resolveVersion(rustResolver, dir, "", &fakePrompter{selected: "lts"})
	if err != nil {
		t.Fatalf("resolveVersion interactive rust: %v", err)
	}
	if got != "1.95.0" {
		t.Fatalf("resolveVersion interactive rust: got %q, want %q", got, "1.95.0")
	}
}

func TestResolveVersion_RustResolver_UsesRustToolchainTomlHint(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rust-toolchain.toml"), []byte("[toolchain]\nchannel = \"1.77.0\"\n"), 0644); err != nil {
		t.Fatalf("write rust-toolchain.toml: %v", err)
	}

	got, err := resolveVersion(rustResolver, dir, "repo", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolveVersion rust-toolchain.toml: %v", err)
	}
	if got != "1.77.0" {
		t.Fatalf("resolveVersion rust-toolchain.toml: got %q, want %q", got, "1.77.0")
	}
}

func TestResolveVersion_RustResolver_StableChannelPinsExactVersion(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rust-toolchain.toml"), []byte("[toolchain]\nchannel = \"stable\"\n"), 0644); err != nil {
		t.Fatalf("write rust-toolchain.toml: %v", err)
	}

	got, err := resolveVersion(rustResolver, dir, "repo", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolveVersion stable rust-toolchain.toml: %v", err)
	}
	if got != "1.96.1" {
		t.Fatalf("resolveVersion stable rust-toolchain.toml: got %q, want %q", got, "1.96.1")
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

			if err := s.Scaffold(dir, Options{BuildTools: "node", Agent: "opencode", ToolVersion: tt.selector}, &fakePrompter{confirm: true}); err != nil {
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

// referenceNormalizeGoVersionSelector and referenceGoPreviousMinorPrefix are
// inlined copies of the pre-migration production functions. They are kept
// here (rather than re-exported from production code) so the test oracle
// remains an independent comparison point that does not depend on the
// migrated versionResolver closures.
func referenceNormalizeGoVersionSelector(selector string) string {
	selector = strings.TrimSpace(selector)
	if len(selector) > 2 && strings.HasPrefix(strings.ToLower(selector), "go") && selector[2] >= '0' && selector[2] <= '9' {
		return selector[2:]
	}
	if len(selector) > 1 && strings.HasPrefix(strings.ToLower(selector), "v") && selector[1] >= '0' && selector[1] <= '9' {
		return selector[1:]
	}
	return selector
}

func referenceGoPreviousMinorPrefix(version string) (string, error) {
	version = referenceNormalizeGoVersionSelector(version)
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return "", fmt.Errorf("unexpected Go version %q", version)
	}

	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return "", fmt.Errorf("parse Go major version %q: %w", version, err)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", fmt.Errorf("parse Go minor version %q: %w", version, err)
	}
	if minor == 0 {
		return "", fmt.Errorf("unexpected Go version %q", version)
	}
	minor--

	prefix := fmt.Sprintf("%d.%d", major, minor)
	if major == 1 && minor <= 20 {
		return "prefix:" + prefix, nil
	}
	return prefix, nil
}

// referenceGoVersion replicates the pre-refactor Go resolution algorithm
// locally so the parity tests have an independent oracle to compare against.
// It is intentionally a copy of the algorithm (not a call into the migrated
// resolveVersion/resolveMiseVersion path) so a regression in the extracted
// resolver surfaces here.
func referenceGoVersion(dir, selector string, prompter Prompter) (string, error) {
	hint, found, err := readGoVersionHint(dir)
	if err != nil {
		return "", err
	}

	choice := strings.TrimSpace(selector)
	if choice == "repo" && !found {
		choice = ""
	}
	if choice == "" {
		if found {
			if prompter != nil {
				selected, err := prompter.Select(fmt.Sprintf("Choose a Go version (repo: %s):", hint), []string{"repo", "latest", "lts"})
				if err == nil {
					choice = referenceNormalizeGoVersionSelector(selected)
				}
			}
			if choice == "" {
				choice = "repo"
			}
		} else {
			if prompter != nil {
				selected, err := prompter.Select("Choose a Go version:", []string{"latest", "lts"})
				if err == nil {
					choice = referenceNormalizeGoVersionSelector(selected)
				}
			}
			if choice == "" {
				choice = "latest"
			}
		}
	}

	return referenceGoVersionChoice(choice, hint, found)
}

func referenceGoVersionChoice(choice, hint string, hintFound bool) (string, error) {
	choice = referenceNormalizeGoVersionSelector(choice)
	if choice == "" {
		return "", fmt.Errorf("empty version selector")
	}

	switch strings.ToLower(choice) {
	case "repo":
		if !hintFound {
			return "", fmt.Errorf("no repo Go version hint found")
		}
		return referenceGoMiseVersion(referenceNormalizeGoVersionSelector(hint))
	case "latest", "lts":
		if strings.ToLower(choice) == "latest" {
			return referenceGoMiseVersion("latest")
		}
		latest, err := referenceGoMiseVersion("latest")
		if err != nil {
			return "", err
		}
		prefix, err := referenceGoPreviousMinorPrefix(latest)
		if err != nil {
			return "", err
		}
		return referenceGoMiseVersion(prefix)
	}

	return referenceGoMiseVersion(choice)
}

func referenceGoMiseVersion(selector string) (string, error) {
	selector = referenceNormalizeGoVersionSelector(selector)
	args := []string{"latest"}
	if selector == "" || strings.EqualFold(selector, "latest") {
		args = append(args, "go")
	} else {
		args = append(args, "go@"+selector)
	}

	cmd := exec.Command("mise", args...)
	out, err := cmd.Output()
	if err == nil {
		version := strings.TrimSpace(string(out))
		if version != "" {
			return version, nil
		}
	}

	if version, ok := bundledGoVersionCatalog[selector]; ok {
		return version, nil
	}
	if selector == "" || strings.EqualFold(selector, "latest") {
		if version, ok := bundledGoVersionCatalog["latest"]; ok {
			return version, nil
		}
	}
	if err != nil {
		return "", fmt.Errorf("resolve go version %q: %w", selector, err)
	}
	return "", fmt.Errorf("resolve go version %q: mise returned empty output and no bundled fallback", selector)
}

func TestResolveVersion_GoResolver_Selectors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".go-version"), []byte("1.24\n"), 0644); err != nil {
		t.Fatalf("write .go-version: %v", err)
	}
	prompter := &fakePrompter{confirm: true}

	tests := []struct {
		name     string
		selector string
	}{
		{name: "repo", selector: "repo"},
		{name: "latest", selector: "latest"},
		{name: "lts", selector: "lts"},
		{name: "specific_version", selector: "1.25"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveVersion(goResolver, dir, tt.selector, prompter)
			if err != nil {
				t.Fatalf("resolveVersion %s: %v", tt.selector, err)
			}
			want, err := referenceGoVersion(dir, tt.selector, prompter)
			if err != nil {
				t.Fatalf("referenceGoVersion %s: %v", tt.selector, err)
			}
			if got != want {
				t.Fatalf("resolveVersion %s: got %q, want %q", tt.selector, got, want)
			}
		})
	}
}

func TestResolveVersion_GoResolver_RepoFallsBackToLatestWithoutHint(t *testing.T) {
	dir := t.TempDir()

	got, err := resolveVersion(goResolver, dir, "repo", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolveVersion repo without hint: %v", err)
	}
	want, err := referenceGoVersion(dir, "repo", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("referenceGoVersion repo: %v", err)
	}
	if got != want {
		t.Fatalf("expected repo fallback to match reference (%q), got %q", want, got)
	}
}

func TestResolveVersion_GoResolver_EmptySelectorDefaultsToLatest(t *testing.T) {
	dir := t.TempDir()

	got, err := resolveVersion(goResolver, dir, "", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolveVersion empty selector: %v", err)
	}
	want, err := referenceGoVersion(dir, "", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("referenceGoVersion empty selector: %v", err)
	}
	if got != want {
		t.Fatalf("resolveVersion empty selector: got %q, want %q", got, want)
	}
}

func TestResolveVersion_GoResolver_EmptySelectorDefaultsToRepoWithHint(t *testing.T) {
	dir := t.TempDir()
	hintSelector := "1.24"
	if err := os.WriteFile(filepath.Join(dir, ".go-version"), []byte(hintSelector+"\n"), 0644); err != nil {
		t.Fatalf("write .go-version: %v", err)
	}

	got, err := resolveVersion(goResolver, dir, "", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolveVersion empty selector with hint: %v", err)
	}
	want, err := referenceGoVersion(dir, "", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("referenceGoVersion empty selector with hint: %v", err)
	}
	if got != want {
		t.Fatalf("resolveVersion empty selector with hint: got %q, want %q", got, want)
	}
}

func TestResolveVersion_GoResolver_EmptySelectorWithoutPrompterDefaultsToLatest(t *testing.T) {
	dir := t.TempDir()

	got, err := resolveVersion(goResolver, dir, "", nil)
	if err != nil {
		t.Fatalf("resolveVersion empty selector without prompter: %v", err)
	}
	want, err := referenceGoVersion(dir, "", nil)
	if err != nil {
		t.Fatalf("referenceGoVersion empty selector without prompter: %v", err)
	}
	if got != want {
		t.Fatalf("resolveVersion empty selector without prompter: got %q, want %q", got, want)
	}
}

func TestResolveVersion_GoResolver_RepoFailsWhenPrompterSelectsRepoWithoutHint(t *testing.T) {
	dir := t.TempDir()
	prompter := &fakePrompter{selected: "repo"}

	_, err := resolveVersion(goResolver, dir, "repo", prompter)
	if err == nil {
		t.Fatal("expected error when prompter selects repo without a hint")
	}
}

func TestResolveVersion_GoResolver_EmptySelectorErrors(t *testing.T) {
	dir := t.TempDir()
	resolver := versionResolver{
		label:      goResolver.label,
		miseTool:   goResolver.miseTool,
		hintReader: goResolver.hintReader,
		normalize:  func(string) string { return "" },
		catalog:    goResolver.catalog,
	}

	_, err := resolveVersion(resolver, dir, "", &fakePrompter{confirm: true})
	if err == nil {
		t.Fatal("expected error for empty selector after normalization")
	}
}

// referenceDotnetVersion replicates the pre-refactor .NET resolution
// algorithm locally so the parity tests have an independent oracle to
// compare against. It is intentionally a copy of the algorithm (not a call
// into the migrated resolveVersion/resolveMiseVersion path) so a regression
// in the extracted resolver surfaces here.
func referenceDotnetVersion(dir, selector string, prompter Prompter) (string, error) {
	hint, found, err := readDotnetVersionHint(dir)
	if err != nil {
		return "", err
	}

	choice := strings.TrimSpace(selector)
	if choice == "repo" && !found {
		choice = ""
	}
	if choice == "" {
		if found {
			if prompter != nil {
				selected, err := prompter.Select(fmt.Sprintf("Choose a .NET SDK version (repo: %s):", hint), []string{"repo", "latest", "lts"})
				if err == nil {
					choice = referenceNormalizeDotnetVersionSelector(selected)
				}
			}
			if choice == "" {
				choice = "repo"
			}
		} else {
			if prompter != nil {
				selected, err := prompter.Select("Choose a .NET SDK version:", []string{"latest", "lts"})
				if err == nil {
					choice = referenceNormalizeDotnetVersionSelector(selected)
				}
			}
			if choice == "" {
				choice = "latest"
			}
		}
	}

	return referenceDotnetVersionChoice(choice, hint, found)
}

func referenceDotnetVersionChoice(choice, hint string, hintFound bool) (string, error) {
	choice = referenceNormalizeDotnetVersionSelector(choice)
	if choice == "" {
		return "", fmt.Errorf("empty version selector")
	}

	switch strings.ToLower(choice) {
	case "repo":
		if !hintFound {
			return "", fmt.Errorf("no repo .NET SDK version hint found")
		}
		return referenceDotnetMiseVersion(referenceNormalizeDotnetVersionSelector(hint))
	case "latest", "lts":
		return referenceDotnetMiseVersion(choice)
	}

	return referenceDotnetMiseVersion(choice)
}

func referenceDotnetMiseVersion(selector string) (string, error) {
	selector = referenceNormalizeDotnetVersionSelector(selector)
	args := []string{"latest"}
	switch strings.ToLower(selector) {
	case "", "latest":
		args = append(args, "dotnet")
	case "lts":
		args = append(args, "dotnet@lts")
	default:
		args = append(args, "dotnet@"+selector)
	}

	cmd := exec.Command("mise", args...)
	out, err := cmd.Output()
	if err == nil {
		version := strings.TrimSpace(string(out))
		if version != "" {
			return version, nil
		}
	}

	if version, ok := bundledDotnetVersionCatalog[selector]; ok {
		return version, nil
	}
	if selector == "" || strings.EqualFold(selector, "latest") {
		if version, ok := bundledDotnetVersionCatalog["latest"]; ok {
			return version, nil
		}
	}
	if strings.EqualFold(selector, "lts") {
		if version, ok := bundledDotnetVersionCatalog["lts"]; ok {
			return version, nil
		}
	}
	if selector != "" && selector != "latest" && selector != "lts" {
		return selector, nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve dotnet version %q: %w", selector, err)
	}
	return "", fmt.Errorf("resolve dotnet version %q: mise returned empty output and no bundled fallback", selector)
}

func referenceNormalizeDotnetVersionSelector(selector string) string {
	selector = strings.TrimSpace(selector)
	if len(selector) > 6 && strings.HasPrefix(strings.ToLower(selector), "dotnet") && selector[6] >= '0' && selector[6] <= '9' {
		return selector[6:]
	}
	if len(selector) > 1 && strings.HasPrefix(strings.ToLower(selector), "v") && selector[1] >= '0' && selector[1] <= '9' {
		return selector[1:]
	}
	return selector
}

// referenceNodeVersion replicates the pre-refactor Node resolution algorithm
// locally so the parity tests have an independent oracle to compare against.
func referenceNodeVersion(dir, selector string, prompter Prompter) (string, error) {
	hint, found, err := readNodeVersionHint(dir)
	if err != nil {
		return "", err
	}

	choice := strings.TrimSpace(selector)
	if choice == "repo" && !found {
		choice = ""
	}
	if choice == "" {
		if found {
			if prompter != nil {
				selected, err := prompter.Select(fmt.Sprintf("Choose a Node version (repo: %s):", hint), []string{"repo", "latest", "lts"})
				if err == nil {
					choice = referenceNormalizeNodeVersionSelector(selected)
				}
			}
			if choice == "" {
				choice = "repo"
			}
		} else {
			if prompter != nil {
				selected, err := prompter.Select("Choose a Node version:", []string{"latest", "lts"})
				if err == nil {
					choice = referenceNormalizeNodeVersionSelector(selected)
				}
			}
			if choice == "" {
				choice = "latest"
			}
		}
	}

	return referenceNodeVersionChoice(choice, hint, found)
}

func referenceNodeVersionChoice(choice, hint string, hintFound bool) (string, error) {
	choice = referenceNormalizeNodeVersionSelector(choice)
	if choice == "" {
		return "", fmt.Errorf("empty version selector")
	}

	switch strings.ToLower(choice) {
	case "repo":
		if !hintFound {
			return "", fmt.Errorf("no repo Node version hint found")
		}
		return referenceNodeMiseVersion(referenceNormalizeNodeVersionSelector(hint))
	case "latest", "lts":
		return referenceNodeMiseVersion(choice)
	}

	return referenceNodeMiseVersion(choice)
}

func referenceNodeMiseVersion(selector string) (string, error) {
	selector = referenceNormalizeNodeVersionSelector(selector)
	args := []string{"latest"}
	switch strings.ToLower(selector) {
	case "", "latest":
		args = append(args, "node")
	case "lts":
		args = append(args, "node@lts")
	default:
		args = append(args, "node@"+selector)
	}

	cmd := exec.Command("mise", args...)
	out, err := cmd.Output()
	if err == nil {
		version := strings.TrimSpace(string(out))
		if version != "" {
			return version, nil
		}
	}

	if version, ok := bundledNodeVersionCatalog[selector]; ok {
		return version, nil
	}
	if selector == "" || strings.EqualFold(selector, "latest") {
		if version, ok := bundledNodeVersionCatalog["latest"]; ok {
			return version, nil
		}
	}
	if strings.EqualFold(selector, "lts") {
		if version, ok := bundledNodeVersionCatalog["lts"]; ok {
			return version, nil
		}
	}
	if selector != "" && selector != "latest" && selector != "lts" && nodeVersionSelectorPattern.MatchString(selector) {
		return selector, nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve node version %q: %w", selector, err)
	}
	return "", fmt.Errorf("resolve node version %q: mise returned empty output and no bundled fallback", selector)
}

func referenceNormalizeNodeVersionSelector(selector string) string {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return ""
	}
	lower := strings.ToLower(selector)
	switch {
	case lower == "repo", lower == "latest", lower == "lts":
		return lower
	case strings.HasPrefix(lower, "lts/"):
		return "lts"
	}
	if len(selector) > 1 && strings.HasPrefix(lower, "v") && selector[1] >= '0' && selector[1] <= '9' {
		return selector[1:]
	}
	if version := nodeVersionSelectorPattern.FindString(selector); version != "" {
		return version
	}
	return selector
}

// referencePythonVersion replicates the pre-refactor Python resolution
// algorithm locally so the parity tests have an independent oracle to compare
// against. The lts branch and pythonPreviousMinorPrefix are inlined because
// they are deleted in the slice that lands the migration.
func referencePythonVersion(dir, selector string, prompter Prompter) (string, error) {
	hint, found, err := readPythonVersionHint(dir)
	if err != nil {
		return "", err
	}

	choice := strings.TrimSpace(selector)
	if choice == "repo" && !found {
		choice = ""
	}
	if choice == "" {
		if found {
			if prompter != nil {
				selected, err := prompter.Select(fmt.Sprintf("Choose a Python version (repo: %s):", hint), []string{"repo", "latest", "lts"})
				if err == nil {
					choice = referenceNormalizePythonVersionSelector(selected)
				}
			}
			if choice == "" {
				choice = "repo"
			}
		} else {
			if prompter != nil {
				selected, err := prompter.Select("Choose a Python version:", []string{"latest", "lts"})
				if err == nil {
					choice = referenceNormalizePythonVersionSelector(selected)
				}
			}
			if choice == "" {
				choice = "latest"
			}
		}
	}

	return referencePythonVersionChoice(choice, hint, found)
}

func referencePythonVersionChoice(choice, hint string, hintFound bool) (string, error) {
	choice = referenceNormalizePythonVersionSelector(choice)
	if choice == "" {
		return "", fmt.Errorf("empty version selector")
	}

	switch strings.ToLower(choice) {
	case "repo":
		if !hintFound {
			return "", fmt.Errorf("no repo Python version hint found")
		}
		return referencePythonMiseVersion(referenceNormalizePythonVersionSelector(hint))
	case "latest", "lts":
		if strings.ToLower(choice) == "latest" {
			return referencePythonMiseVersion("latest")
		}
		latest, err := referencePythonMiseVersion("latest")
		if err != nil {
			return "", err
		}
		prefix, err := referencePythonPreviousMinorPrefix(latest)
		if err != nil {
			return "", err
		}
		return referencePythonMiseVersion(prefix)
	}

	return referencePythonMiseVersion(choice)
}

func referencePythonMiseVersion(selector string) (string, error) {
	selector = referenceNormalizePythonVersionSelector(selector)
	args := []string{"latest"}
	if selector == "" || strings.EqualFold(selector, "latest") {
		args = append(args, "python")
	} else {
		args = append(args, "python@"+selector)
	}

	cmd := exec.Command("mise", args...)
	out, err := cmd.Output()
	if err == nil {
		version := strings.TrimSpace(string(out))
		if version != "" {
			return version, nil
		}
	}

	if version, ok := bundledPythonVersionCatalog[selector]; ok {
		return version, nil
	}
	if selector == "" || strings.EqualFold(selector, "latest") {
		if version, ok := bundledPythonVersionCatalog["latest"]; ok {
			return version, nil
		}
	}
	if err != nil {
		return "", fmt.Errorf("resolve python version %q: %w", selector, err)
	}
	return "", fmt.Errorf("resolve python version %q: mise returned empty output and no bundled fallback", selector)
}

func referenceNormalizePythonVersionSelector(selector string) string {
	selector = strings.TrimSpace(selector)
	if len(selector) > 6 && strings.HasPrefix(strings.ToLower(selector), "python") && selector[6] >= '0' && selector[6] <= '9' {
		return selector[6:]
	}
	if len(selector) > 1 && strings.HasPrefix(strings.ToLower(selector), "v") && selector[1] >= '0' && selector[1] <= '9' {
		return selector[1:]
	}
	return selector
}

func referencePythonPreviousMinorPrefix(version string) (string, error) {
	version = referenceNormalizePythonVersionSelector(version)
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return "", fmt.Errorf("unexpected Python version %q", version)
	}

	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return "", fmt.Errorf("parse Python major version %q: %w", version, err)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", fmt.Errorf("parse Python minor version %q: %w", version, err)
	}
	if minor == 0 {
		return "", fmt.Errorf("unexpected Python version %q", version)
	}
	minor--

	return fmt.Sprintf("%d.%d", major, minor), nil
}

func TestResolveVersion_DotnetResolver_Selectors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "global.json"), []byte(`{"sdk":{"version":"8.0.100"}}`), 0644); err != nil {
		t.Fatalf("write global.json: %v", err)
	}
	prompter := &fakePrompter{confirm: true}

	tests := []struct {
		name     string
		selector string
	}{
		{name: "repo", selector: "repo"},
		{name: "latest", selector: "latest"},
		{name: "lts", selector: "lts"},
		{name: "specific_version", selector: "9"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveVersion(dotnetResolver, dir, tt.selector, prompter)
			if err != nil {
				t.Fatalf("resolveVersion %s: %v", tt.selector, err)
			}
			want, err := referenceDotnetVersion(dir, tt.selector, prompter)
			if err != nil {
				t.Fatalf("referenceDotnetVersion %s: %v", tt.selector, err)
			}
			if got != want {
				t.Fatalf("resolveVersion %s: got %q, want %q", tt.selector, got, want)
			}
		})
	}
}

func TestResolveVersion_DotnetResolver_RepoFallsBackToLatestWithoutHint(t *testing.T) {
	dir := t.TempDir()

	got, err := resolveVersion(dotnetResolver, dir, "repo", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolveVersion repo without hint: %v", err)
	}
	want, err := referenceDotnetVersion(dir, "repo", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("referenceDotnetVersion repo: %v", err)
	}
	if got != want {
		t.Fatalf("expected repo fallback to match reference (%q), got %q", want, got)
	}
}

func TestResolveVersion_DotnetResolver_EmptySelectorDefaultsToLatest(t *testing.T) {
	dir := t.TempDir()

	got, err := resolveVersion(dotnetResolver, dir, "", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolveVersion empty selector: %v", err)
	}
	want, err := referenceDotnetVersion(dir, "", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("referenceDotnetVersion empty selector: %v", err)
	}
	if got != want {
		t.Fatalf("resolveVersion empty selector: got %q, want %q", got, want)
	}
}

func TestResolveVersion_DotnetResolver_EmptySelectorDefaultsToRepoWithHint(t *testing.T) {
	dir := t.TempDir()
	hintSelector := "8.0.100"
	if err := os.WriteFile(filepath.Join(dir, "global.json"), []byte(`{"sdk":{"version":"`+hintSelector+`"}}`), 0644); err != nil {
		t.Fatalf("write global.json: %v", err)
	}

	got, err := resolveVersion(dotnetResolver, dir, "", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolveVersion empty selector with hint: %v", err)
	}
	want, err := referenceDotnetVersion(dir, "", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("referenceDotnetVersion empty selector with hint: %v", err)
	}
	if got != want {
		t.Fatalf("resolveVersion empty selector with hint: got %q, want %q", got, want)
	}
}

func TestResolveVersion_DotnetResolver_RepoFailsWhenPrompterSelectsRepoWithoutHint(t *testing.T) {
	dir := t.TempDir()
	prompter := &fakePrompter{selected: "repo"}

	_, err := resolveVersion(dotnetResolver, dir, "repo", prompter)
	if err == nil {
		t.Fatal("expected error when prompter selects repo without a hint")
	}
}

func TestResolveVersion_DotnetResolver_PassThroughUnknownSelector(t *testing.T) {
	dir := t.TempDir()
	selector := "7.0.999"

	got, err := resolveVersion(dotnetResolver, dir, selector, &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolveVersion pass-through: %v", err)
	}
	want, err := referenceDotnetVersion(dir, selector, &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("referenceDotnetVersion pass-through: %v", err)
	}
	if got != want {
		t.Fatalf("resolveVersion pass-through: got %q, want %q", got, want)
	}
}

func TestResolveVersion_NodeResolver_Selectors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"demo","engines":{"node":"20"}}`), 0644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
	prompter := &fakePrompter{confirm: true}

	tests := []struct {
		name     string
		selector string
	}{
		{name: "repo", selector: "repo"},
		{name: "latest", selector: "latest"},
		{name: "lts", selector: "lts"},
		{name: "specific_version", selector: "20"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveVersion(nodeResolver, dir, tt.selector, prompter)
			if err != nil {
				t.Fatalf("resolveVersion %s: %v", tt.selector, err)
			}
			want, err := referenceNodeVersion(dir, tt.selector, prompter)
			if err != nil {
				t.Fatalf("referenceNodeVersion %s: %v", tt.selector, err)
			}
			if got != want {
				t.Fatalf("resolveVersion %s: got %q, want %q", tt.selector, got, want)
			}
		})
	}
}

func TestResolveVersion_NodeResolver_RepoFallsBackToLatestWithoutHint(t *testing.T) {
	dir := t.TempDir()

	got, err := resolveVersion(nodeResolver, dir, "repo", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolveVersion repo without hint: %v", err)
	}
	want, err := referenceNodeVersion(dir, "repo", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("referenceNodeVersion repo: %v", err)
	}
	if got != want {
		t.Fatalf("expected repo fallback to match reference (%q), got %q", want, got)
	}
}

func TestResolveVersion_NodeResolver_EmptySelectorDefaultsToLatest(t *testing.T) {
	dir := t.TempDir()

	got, err := resolveVersion(nodeResolver, dir, "", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolveVersion empty selector: %v", err)
	}
	want, err := referenceNodeVersion(dir, "", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("referenceNodeVersion empty selector: %v", err)
	}
	if got != want {
		t.Fatalf("resolveVersion empty selector: got %q, want %q", got, want)
	}
}

func TestResolveVersion_NodeResolver_EmptySelectorDefaultsToRepoWithHint(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"demo","engines":{"node":"20"}}`), 0644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}

	got, err := resolveVersion(nodeResolver, dir, "", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolveVersion empty selector with hint: %v", err)
	}
	want, err := referenceNodeVersion(dir, "", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("referenceNodeVersion empty selector with hint: %v", err)
	}
	if got != want {
		t.Fatalf("resolveVersion empty selector with hint: got %q, want %q", got, want)
	}
}

func TestResolveVersion_NodeResolver_RepoFailsWhenPrompterSelectsRepoWithoutHint(t *testing.T) {
	dir := t.TempDir()
	prompter := &fakePrompter{selected: "repo"}

	_, err := resolveVersion(nodeResolver, dir, "repo", prompter)
	if err == nil {
		t.Fatal("expected error when prompter selects repo without a hint")
	}
}

func TestResolveVersion_NodeResolver_PassThroughUnknownSelector(t *testing.T) {
	dir := t.TempDir()
	selector := "v20.99.99"

	got, err := resolveVersion(nodeResolver, dir, selector, &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolveVersion pass-through: %v", err)
	}
	want, err := referenceNodeVersion(dir, selector, &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("referenceNodeVersion pass-through: %v", err)
	}
	if got != want {
		t.Fatalf("resolveVersion pass-through: got %q, want %q", got, want)
	}
}

func TestResolveVersion_PythonResolver_Selectors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".python-version"), []byte("3.12\n"), 0644); err != nil {
		t.Fatalf("write .python-version: %v", err)
	}
	prompter := &fakePrompter{confirm: true}

	tests := []struct {
		name     string
		selector string
	}{
		{name: "repo", selector: "repo"},
		{name: "latest", selector: "latest"},
		{name: "lts", selector: "lts"},
		{name: "specific_version", selector: "3.12"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveVersion(pythonResolver, dir, tt.selector, prompter)
			if err != nil {
				t.Fatalf("resolveVersion %s: %v", tt.selector, err)
			}
			want, err := referencePythonVersion(dir, tt.selector, prompter)
			if err != nil {
				t.Fatalf("referencePythonVersion %s: %v", tt.selector, err)
			}
			if got != want {
				t.Fatalf("resolveVersion %s: got %q, want %q", tt.selector, got, want)
			}
		})
	}
}

func TestResolveVersion_PythonResolver_RepoFallsBackToLatestWithoutHint(t *testing.T) {
	dir := t.TempDir()

	got, err := resolveVersion(pythonResolver, dir, "repo", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolveVersion repo without hint: %v", err)
	}
	want, err := referencePythonVersion(dir, "repo", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("referencePythonVersion repo: %v", err)
	}
	if got != want {
		t.Fatalf("expected repo fallback to match reference (%q), got %q", want, got)
	}
}

func TestResolveVersion_PythonResolver_EmptySelectorDefaultsToLatest(t *testing.T) {
	dir := t.TempDir()

	got, err := resolveVersion(pythonResolver, dir, "", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolveVersion empty selector: %v", err)
	}
	want, err := referencePythonVersion(dir, "", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("referencePythonVersion empty selector: %v", err)
	}
	if got != want {
		t.Fatalf("resolveVersion empty selector: got %q, want %q", got, want)
	}
}

func TestResolveVersion_PythonResolver_EmptySelectorDefaultsToRepoWithHint(t *testing.T) {
	dir := t.TempDir()
	hintSelector := "3.12"
	if err := os.WriteFile(filepath.Join(dir, ".python-version"), []byte(hintSelector+"\n"), 0644); err != nil {
		t.Fatalf("write .python-version: %v", err)
	}

	got, err := resolveVersion(pythonResolver, dir, "", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolveVersion empty selector with hint: %v", err)
	}
	want, err := referencePythonVersion(dir, "", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("referencePythonVersion empty selector with hint: %v", err)
	}
	if got != want {
		t.Fatalf("resolveVersion empty selector with hint: got %q, want %q", got, want)
	}
}

func TestResolveVersion_PythonResolver_RepoFailsWhenPrompterSelectsRepoWithoutHint(t *testing.T) {
	dir := t.TempDir()
	prompter := &fakePrompter{selected: "repo"}

	_, err := resolveVersion(pythonResolver, dir, "repo", prompter)
	if err == nil {
		t.Fatal("expected error when prompter selects repo without a hint")
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
			if cfg.DefaultAgent != agent {
				t.Errorf("expected agent %q, got %q", agent, cfg.DefaultAgent)
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
			if !strings.Contains(content, "# sandman default-agent: "+agent) {
				t.Fatalf("Dockerfile missing default-agent metadata, got:\n%s", content)
			}
			if !strings.Contains(content, "# sandman installed-agents: opencode") {
				t.Fatalf("Dockerfile missing installed-agents metadata, got:\n%s", content)
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
	if !strings.Contains(content, "# sandman default-agent: opencode") {
		t.Fatalf("Dockerfile missing default-agent metadata, got:\n%s", content)
	}
	if !strings.Contains(content, "# sandman python-version:") {
		t.Fatalf("Dockerfile missing python-version metadata, got:\n%s", content)
	}
	if !strings.Contains(content, "# sandman installed-agents: opencode") {
		t.Fatalf("Dockerfile missing installed-agents metadata, got:\n%s", content)
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
	if !strings.Contains(content, "RUN git clone https://github.com/rafaelromao/codeindex /tmp/codeindex && pip3 install -e /tmp/codeindex --break-system-packages") {
		t.Fatalf("Dockerfile missing codeindex install, got:\n%s", content)
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

func TestScaffold_DotnetRepoAutoDetect(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "global.json"), []byte(`{"sdk":{"version":"8.0.100"}}`), 0644); err != nil {
		t.Fatalf("write global.json: %v", err)
	}

	s := &Scaffolder{}
	preset, err := s.resolveBuildToolsPreset(dir, Options{}, &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolve build tools preset: %v", err)
	}
	if preset.Name != "dotnet" {
		t.Fatalf("expected preset %q, got %q", "dotnet", preset.Name)
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

func TestScaffold_GenericBuildToolsOverridesDotnetRepoHints(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "global.json"), []byte(`{"sdk":{"version":"8.0.100"}}`), 0644); err != nil {
		t.Fatalf("write global.json: %v", err)
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

func TestScaffold_HasElixirRepoHint(t *testing.T) {
	tests := []struct {
		name    string
		setupFn func(dir string)
		want    bool
	}{
		{
			name: "mix.exs",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, "mix.exs"), []byte("defmodule Demo.MixProject do\n  use Mix.Project\n\n  def project do\n    [\n      app: :demo,\n      version: \"0.1.0\",\n      elixir: \"~> 1.18\",\n      elixirc_paths: elixirc_paths(Mix.env())\n    ]\n  end\nend\n"), 0644)
			},
			want: true,
		},
		{
			name: ".formatter.exs",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, ".formatter.exs"), []byte("[\n  inputs: [\"{mix,.formatter}.exs\", \"{config,lib,test}/**/*.{ex,exs}\"]\n]\n"), 0644)
			},
			want: true,
		},
		{
			name: ".elixir_version",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, ".elixir_version"), []byte("1.18.4\n"), 0644)
			},
			want: true,
		},
		{
			name: ".tool-versions with elixir",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, ".tool-versions"), []byte("elixir 1.18.4\nerlang 28.5\n"), 0644)
			},
			want: true,
		},
		{
			name: ".tool-versions without elixir",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, ".tool-versions"), []byte("go 1.24\nnode 20.0.0\n"), 0644)
			},
			want: false,
		},
		{
			name: "empty dir",
			setupFn: func(dir string) {
				_ = dir
			},
			want: false,
		},
		{
			name: "non-elixir repo",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/demo\n\ngo 1.24\n"), 0644)
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			tt.setupFn(dir)

			if got := hasElixirRepoHint(dir); got != tt.want {
				t.Errorf("hasElixirRepoHint(%q) = %v, want %v", dir, got, tt.want)
			}
		})
	}
}

func TestScaffold_JavaRepoAutoDetect(t *testing.T) {
	tests := []struct {
		name    string
		setupFn func(dir string)
		want    string
	}{
		{
			name: "pom.xml",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, "pom.xml"), []byte("<project></project>\n"), 0644)
			},
			want: javaBuildToolsPreset,
		},
		{
			name: "build.gradle",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, "build.gradle"), []byte("plugins { id 'java' }\n"), 0644)
			},
			want: javaBuildToolsPreset,
		},
		{
			name: "build.gradle.kts",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, "build.gradle.kts"), []byte("plugins { kotlin(\"jvm\") }\n"), 0644)
			},
			want: javaBuildToolsPreset,
		},
		{
			name: "node takes priority over java",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"demo","engines":{"node":"20"}}`), 0644)
				os.WriteFile(filepath.Join(dir, "pom.xml"), []byte("<project></project>\n"), 0644)
			},
			want: nodeBuildToolsPreset,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			tt.setupFn(dir)

			s := &Scaffolder{}
			preset, err := s.resolveBuildToolsPreset(dir, Options{}, &fakePrompter{confirm: true})
			if err != nil {
				t.Fatalf("resolveBuildToolsPreset: %v", err)
			}
			if preset.Name != tt.want {
				t.Errorf("expected preset %q, got %q", tt.want, preset.Name)
			}
		})
	}
}

func TestScaffold_GenericBuildToolsOverridesJavaRepoHint(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "pom.xml"), []byte("<project></project>\n"), 0644)

	s := &Scaffolder{}
	preset, err := s.resolveBuildToolsPreset(dir, Options{BuildTools: "generic"}, &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolveBuildToolsPreset: %v", err)
	}
	if preset.Name != "generic" {
		t.Errorf("expected explicit generic preset, got %q", preset.Name)
	}
}

func TestScaffold_RubyPresetIsRegistered(t *testing.T) {
	preset, ok := builtInBuildToolsPresets[rubyBuildToolsPreset]
	if !ok {
		t.Fatalf("ruby preset not registered in builtInBuildToolsPresets")
	}
	if preset.Name != rubyBuildToolsPreset {
		t.Errorf("preset.Name = %q, want %q", preset.Name, rubyBuildToolsPreset)
	}
	if preset.BaseImage != "debian:bookworm-slim" {
		t.Errorf("preset.BaseImage = %q, want %q", preset.BaseImage, "debian:bookworm-slim")
	}
	if preset.MiseVersion != DefaultMISEVersion {
		t.Errorf("preset.MiseVersion = %q, want %q", preset.MiseVersion, DefaultMISEVersion)
	}
	if len(preset.SharedPackages) != len(sharedPackages) {
		t.Errorf("preset.SharedPackages length = %d, want %d", len(preset.SharedPackages), len(sharedPackages))
	}
	if !containsString(KnownBuildToolsPresets, rubyBuildToolsPreset) {
		t.Errorf("KnownBuildToolsPresets missing %q, got %v", rubyBuildToolsPreset, KnownBuildToolsPresets)
	}
}

func TestScaffold_JavaPresetIsRegistered(t *testing.T) {
	preset, ok := builtInBuildToolsPresets[javaBuildToolsPreset]
	if !ok {
		t.Fatalf("java preset not registered in builtInBuildToolsPresets")
	}
	if preset.Name != javaBuildToolsPreset {
		t.Errorf("preset.Name = %q, want %q", preset.Name, javaBuildToolsPreset)
	}
	if preset.BaseImage != "debian:bookworm-slim" {
		t.Errorf("preset.BaseImage = %q, want %q", preset.BaseImage, "debian:bookworm-slim")
	}
	if preset.MiseVersion != DefaultMISEVersion {
		t.Errorf("preset.MiseVersion = %q, want %q", preset.MiseVersion, DefaultMISEVersion)
	}
	if len(preset.SharedPackages) != len(sharedPackages) {
		t.Errorf("preset.SharedPackages length = %d, want %d", len(preset.SharedPackages), len(sharedPackages))
	}
	if !containsString(KnownBuildToolsPresets, javaBuildToolsPreset) {
		t.Errorf("KnownBuildToolsPresets missing %q, got %v", javaBuildToolsPreset, KnownBuildToolsPresets)
	}
}

func TestScaffold_ElixirPresetIsRegistered(t *testing.T) {
	preset, ok := builtInBuildToolsPresets[elixirBuildToolsPreset]
	if !ok {
		t.Fatalf("elixir preset not registered in builtInBuildToolsPresets")
	}
	if preset.Name != elixirBuildToolsPreset {
		t.Errorf("preset.Name = %q, want %q", preset.Name, elixirBuildToolsPreset)
	}
	if preset.BaseImage != "debian:bookworm-slim" {
		t.Errorf("preset.BaseImage = %q, want %q", preset.BaseImage, "debian:bookworm-slim")
	}
	if preset.MiseVersion != DefaultMISEVersion {
		t.Errorf("preset.MiseVersion = %q, want %q", preset.MiseVersion, DefaultMISEVersion)
	}
	if len(preset.SharedPackages) != len(sharedPackages) {
		t.Errorf("preset.SharedPackages length = %d, want %d", len(preset.SharedPackages), len(sharedPackages))
	}
	if !containsString(KnownBuildToolsPresets, elixirBuildToolsPreset) {
		t.Errorf("KnownBuildToolsPresets missing %q, got %v", elixirBuildToolsPreset, KnownBuildToolsPresets)
	}
}

func TestScaffold_ElixirRepoAutoDetect(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mix.exs"), []byte("defmodule Demo.MixProject do\n  use Mix.Project\n\n  def project do\n    [app: :demo, version: \"0.1.0\", elixir: \"~> 1.18\"]\n  end\nend\n"), 0644); err != nil {
		t.Fatalf("write mix.exs: %v", err)
	}

	s := &Scaffolder{}
	preset, err := s.resolveBuildToolsPreset(dir, Options{}, &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolve build tools preset: %v", err)
	}
	if preset.Name != elixirBuildToolsPreset {
		t.Errorf("expected elixir preset, got %q", preset.Name)
	}
}

func TestScaffold_GenericBuildToolsOverridesElixirRepoHint(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mix.exs"), []byte("defmodule Demo.MixProject do\n  use Mix.Project\n  def project do\n    [app: :demo, version: \"0.1.0\", elixir: \"~> 1.18\"]\n  end\nend\n"), 0644); err != nil {
		t.Fatalf("write mix.exs: %v", err)
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

func TestScaffold_NodePresetTakesPriorityOverElixir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"demo","engines":{"node":"20"}}`), 0644)
	os.WriteFile(filepath.Join(dir, "mix.exs"), []byte("defmodule Demo.MixProject do\n  use Mix.Project\n  def project do\n    [app: :demo, version: \"0.1.0\", elixir: \"~> 1.18\"]\n  end\nend\n"), 0644)

	s := &Scaffolder{}
	preset, err := s.resolveBuildToolsPreset(dir, Options{}, &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolve build tools preset: %v", err)
	}
	if preset.Name != "node" {
		t.Errorf("expected Node preset to take priority over Elixir, got %q", preset.Name)
	}
}

func TestScaffold_HasRubyRepoHint(t *testing.T) {
	tests := []struct {
		name    string
		setupFn func(dir string)
		want    bool
	}{
		{
			name: "Gemfile",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, "Gemfile"), []byte("source 'https://rubygems.org'\ngem 'rails'\n"), 0644)
			},
			want: true,
		},
		{
			name: "gemspec",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, "demo.gemspec"), []byte("Gem::Specification.new do |s|\n  s.name = 'demo'\nend\n"), 0644)
			},
			want: true,
		},
		{
			name: ".ruby-version",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, ".ruby-version"), []byte("3.2.2\n"), 0644)
			},
			want: true,
		},
		{
			name: ".tool-versions with ruby",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, ".tool-versions"), []byte("ruby 3.2.2\n"), 0644)
			},
			want: true,
		},
		{
			name: ".tool-versions without ruby",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, ".tool-versions"), []byte("go 1.24\nnode 20.0.0\n"), 0644)
			},
			want: false,
		},
		{
			name: "empty dir",
			setupFn: func(dir string) {
				_ = dir
			},
			want: false,
		},
		{
			name: "non-ruby repo",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/demo\n\ngo 1.24\n"), 0644)
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			tt.setupFn(dir)

			if got := hasRubyRepoHint(dir); got != tt.want {
				t.Errorf("hasRubyRepoHint(%q) = %v, want %v", dir, got, tt.want)
			}
		})
	}
}

func TestJavaResolver_LtsFromLatest(t *testing.T) {
	tests := []struct {
		latest string
		want   string
	}{
		{latest: "21.0.2", want: "17"},
		{latest: "17.0.10", want: "11"},
		{latest: "11.0.22", want: "8"},
	}
	for _, tt := range tests {
		t.Run("latest="+tt.latest, func(t *testing.T) {
			got, err := javaResolver.ltsFromLatest(tt.latest)
			if err != nil {
				t.Fatalf("javaResolver.ltsFromLatest(%q) returned error: %v", tt.latest, err)
			}
			if got != tt.want {
				t.Errorf("javaResolver.ltsFromLatest(%q) = %q, want %q", tt.latest, got, tt.want)
			}
			if _, ok := bundledJavaVersionCatalog[got]; !ok {
				t.Errorf("ltsFromLatest returned %q, which is not in bundledJavaVersionCatalog", got)
			}
		})
	}

	t.Run("latest=8 has no prior LTS", func(t *testing.T) {
		_, err := javaResolver.ltsFromLatest("8.0.412")
		if err == nil {
			t.Errorf("javaResolver.ltsFromLatest(\"8.0.412\") expected error, got nil")
		}
	})
}

func TestBundledJavaVersionCatalog(t *testing.T) {
	expectedKeys := []string{"latest", "lts", "21", "21.0", "21.0.2", "17", "17.0", "17.0.10", "11", "11.0", "11.0.22", "8", "8.0", "8.0.412"}
	for _, key := range expectedKeys {
		v, ok := bundledJavaVersionCatalog[key]
		if !ok {
			t.Errorf("bundledJavaVersionCatalog missing key %q", key)
			continue
		}
		if v == "" {
			t.Errorf("bundledJavaVersionCatalog[%q] is empty", key)
		}
		if !strings.Contains(v, ".") {
			t.Errorf("bundledJavaVersionCatalog[%q] = %q, want a pinned version with dots", key, v)
		}
	}
}

func TestJavaResolver_NormalizeAndPassThrough(t *testing.T) {
	tests := []struct {
		selector string
		want     bool
	}{
		{selector: "21", want: true},
		{selector: "21.0", want: true},
		{selector: "21.0.2", want: true},
		{selector: "17.0.10", want: true},
		{selector: "11.0.22", want: true},
		{selector: "", want: false},
		{selector: "abc", want: false},
		{selector: "temurin@21", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.selector, func(t *testing.T) {
			got := javaResolver.passThroughValid(tt.selector)
			if got != tt.want {
				t.Errorf("javaResolver.passThroughValid(%q) = %v, want %v", tt.selector, got, tt.want)
			}
		})
	}

	normalizeTests := []struct {
		in   string
		want string
	}{
		{in: "21", want: "21"},
		{in: "java21", want: "21"},
		{in: "jdk21", want: "21"},
		{in: "openjdk-21", want: "21"},
		{in: "21.0.2", want: "21.0.2"},
	}
	for _, tt := range normalizeTests {
		t.Run("normalize_"+tt.in, func(t *testing.T) {
			got := javaResolver.normalize(tt.in)
			if got != tt.want {
				t.Errorf("javaResolver.normalize(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestScaffold_JavaPresetResolvesRangeSelectorToCatalogPin(t *testing.T) {
	dir := t.TempDir()
	fakeMise := filepath.Join(dir, "mise")
	if err := os.WriteFile(fakeMise, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatalf("write fake mise: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	s := &Scaffolder{}
	got, err := s.resolveJavaVersion(dir, "21", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolveJavaVersion: %v", err)
	}
	if !strings.HasPrefix(got, "21") {
		t.Errorf("expected java version starting with 21, got %q", got)
	}
}

func TestReadJavaVersionHint(t *testing.T) {
	tests := []struct {
		name    string
		setupFn func(dir string)
		want    string
	}{
		{
			name: "pom.xml with java.version",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, "pom.xml"), []byte("<project><properties><java.version>21</java.version></properties></project>\n"), 0644)
			},
			want: "21",
		},
		{
			name: "pom.xml with maven.compiler.source",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, "pom.xml"), []byte("<project><properties><maven.compiler.source>17</maven.compiler.source></properties></project>\n"), 0644)
			},
			want: "17",
		},
		{
			name: "build.gradle sourceCompatibility",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, "build.gradle"), []byte("plugins { id 'java' }\n\njava {\n    sourceCompatibility = '21'\n    targetCompatibility = '21'\n}\n"), 0644)
			},
			want: "21",
		},
		{
			name: "build.gradle.kts jvmTarget",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, "build.gradle.kts"), []byte("plugins { kotlin(\"jvm\") }\n\ntasks.withType<JavaCompile> {\n    sourceCompatibility = \"17\"\n    targetCompatibility = \"17\"\n}\n"), 0644)
			},
			want: "17",
		},
		{
			name: ".tool-versions java",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, ".tool-versions"), []byte("java 21.0.2\ngradle 8.5\n"), 0644)
			},
			want: "21.0.2",
		},
		{
			name: "no hint",
			setupFn: func(dir string) {
				_ = dir
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			tt.setupFn(dir)

			got, found, err := readJavaVersionHint(dir)
			if err != nil {
				t.Fatalf("readJavaVersionHint(%q) returned error: %v", dir, err)
			}
			if tt.want == "" {
				if found {
					t.Errorf("readJavaVersionHint(%q) found = true, want false (got %q)", dir, got)
				}
				return
			}
			if !found {
				t.Fatalf("readJavaVersionHint(%q) found = false, want true", dir)
			}
			if got != tt.want {
				t.Errorf("readJavaVersionHint(%q) = %q, want %q", dir, got, tt.want)
			}
		})
	}
}

func TestScaffold_HasJavaRepoHint(t *testing.T) {
	tests := []struct {
		name    string
		setupFn func(dir string)
		want    bool
	}{
		{
			name: "pom.xml",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, "pom.xml"), []byte("<?xml version=\"1.0\"?>\n<project></project>\n"), 0644)
			},
			want: true,
		},
		{
			name: "build.gradle",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, "build.gradle"), []byte("plugins { id 'java' }\n"), 0644)
			},
			want: true,
		},
		{
			name: "build.gradle.kts",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, "build.gradle.kts"), []byte("plugins { kotlin(\"jvm\") }\n"), 0644)
			},
			want: true,
		},
		{
			name: "empty dir",
			setupFn: func(dir string) {
				_ = dir
			},
			want: false,
		},
		{
			name: "non-jvm repo",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/demo\n\ngo 1.24\n"), 0644)
			},
			want: false,
		},
		{
			name: "ruby repo",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, "Gemfile"), []byte("source 'https://rubygems.org'\n"), 0644)
			},
			want: false,
		},
		{
			name: "node repo",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"demo"}`), 0644)
			},
			want: false,
		},
		{
			name: "pom.xml only in subdirectory",
			setupFn: func(dir string) {
				os.MkdirAll(filepath.Join(dir, "sub"), 0755)
				os.WriteFile(filepath.Join(dir, "sub", "pom.xml"), []byte("<project></project>\n"), 0644)
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			tt.setupFn(dir)

			if got := hasJavaRepoHint(dir); got != tt.want {
				t.Errorf("hasJavaRepoHint(%q) = %v, want %v", dir, got, tt.want)
			}
		})
	}
}

func TestScaffold_RubyRepoAutoDetect(t *testing.T) {
	tests := []struct {
		name    string
		setupFn func(dir string)
		want    string
	}{
		{
			name: "Gemfile",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, "Gemfile"), []byte("source 'https://rubygems.org'\ngem 'rails'\n"), 0644)
			},
			want: "ruby",
		},
		{
			name: ".ruby-version",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, ".ruby-version"), []byte("3.2.2\n"), 0644)
			},
			want: "ruby",
		},
		{
			name: ".tool-versions",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, ".tool-versions"), []byte("ruby 3.2.2\n"), 0644)
			},
			want: "ruby",
		},
		{
			name: "gemspec",
			setupFn: func(dir string) {
				os.WriteFile(filepath.Join(dir, "demo.gemspec"), []byte("Gem::Specification.new do |s|\n  s.name = 'demo'\nend\n"), 0644)
			},
			want: "ruby",
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

func TestScaffold_GenericBuildToolsOverridesRubyRepoHint(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Gemfile"), []byte("source 'https://rubygems.org'\ngem 'rails'\n"), 0644); err != nil {
		t.Fatalf("write Gemfile: %v", err)
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

func TestScaffold_RubyPresetTakesPriorityOverGeneric(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Gemfile"), []byte("source 'https://rubygems.org'\ngem 'rails'\n"), 0644)

	s := &Scaffolder{}
	preset, err := s.resolveBuildToolsPreset(dir, Options{}, &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolve build tools preset: %v", err)
	}
	if preset.Name != "ruby" {
		t.Errorf("expected Ruby preset, got %q", preset.Name)
	}
}

func TestScaffold_ElixirPresetTakesPriorityOverRuby(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "mix.exs"), []byte("defmodule Demo.MixProject do\n  use Mix.Project\n  def project do\n    [app: :demo, version: \"0.1.0\", elixir: \"~> 1.18\"]\n  end\nend\n"), 0644)
	os.WriteFile(filepath.Join(dir, "Gemfile"), []byte("source 'https://rubygems.org'\ngem 'rails'\n"), 0644)

	s := &Scaffolder{}
	preset, err := s.resolveBuildToolsPreset(dir, Options{}, &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolve build tools preset: %v", err)
	}
	if preset.Name != "elixir" {
		t.Errorf("expected Elixir preset to take priority over Ruby, got %q", preset.Name)
	}
}

func containsString(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}

func TestScaffold_ElixirPresetResolveVersion_UsesRepoHint(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".elixir_version"), []byte("1.18.4\n"), 0644); err != nil {
		t.Fatalf("write .elixir_version: %v", err)
	}

	s := &Scaffolder{}
	got, err := s.resolveElixirVersion(dir, "repo", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolveElixirVersion: %v", err)
	}
	if got != "1.18.4" {
		t.Errorf("expected elixir version %q, got %q", "1.18.4", got)
	}
}

func TestScaffold_ElixirPresetResolveVersion_NoHintFallsBackToLatest(t *testing.T) {
	dir := t.TempDir()

	s := &Scaffolder{}
	got, err := s.resolveElixirVersion(dir, "", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolveElixirVersion: %v", err)
	}
	if got == "" {
		t.Fatalf("expected non-empty elixir version")
	}
	if !strings.Contains(got, ".") {
		t.Errorf("expected elixir version with dots, got %q", got)
	}
}

func TestScaffold_ElixirPresetResolveVersion_ExplicitSelector(t *testing.T) {
	dir := t.TempDir()
	s := &Scaffolder{}
	got, err := s.resolveElixirVersion(dir, "1.18", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolveElixirVersion: %v", err)
	}
	if !strings.HasPrefix(got, "1.18") {
		t.Errorf("expected elixir version starting with 1.18, got %q", got)
	}
}

func TestScaffold_ElixirPresetResolvesRangeSelectorToCatalogPin(t *testing.T) {
	dir := t.TempDir()
	fakeMise := filepath.Join(dir, "mise")
	if err := os.WriteFile(fakeMise, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatalf("write fake mise: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	s := &Scaffolder{}
	got, err := s.resolveElixirVersion(dir, "~> 1.18", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolveElixirVersion: %v", err)
	}
	if got != "1.18.4-otp-28" {
		t.Fatalf("expected range selector to resolve to catalog pin %q, got %q", "1.18.4-otp-28", got)
	}
}

func TestScaffold_RubyPresetResolveVersion_UsesRepoHint(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".ruby-version"), []byte("3.2.2\n"), 0644); err != nil {
		t.Fatalf("write .ruby-version: %v", err)
	}

	s := &Scaffolder{}
	got, err := s.resolveRubyVersion(dir, "repo", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolveRubyVersion: %v", err)
	}
	if got != "3.2.2" {
		t.Errorf("expected ruby version %q, got %q", "3.2.2", got)
	}
}

func TestScaffold_RubyPresetResolveVersion_NoHintFallsBackToLatest(t *testing.T) {
	dir := t.TempDir()

	s := &Scaffolder{}
	got, err := s.resolveRubyVersion(dir, "", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolveRubyVersion: %v", err)
	}
	if got == "" {
		t.Fatalf("expected non-empty ruby version")
	}
	if !strings.Contains(got, ".") {
		t.Errorf("expected ruby version with dots, got %q", got)
	}
}

func TestScaffold_RubyPresetResolveVersion_ExplicitSelector(t *testing.T) {
	dir := t.TempDir()
	s := &Scaffolder{}
	got, err := s.resolveRubyVersion(dir, "3.3", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolveRubyVersion: %v", err)
	}
	if !strings.HasPrefix(got, "3.3") {
		t.Errorf("expected ruby version starting with 3.3, got %q", got)
	}
}

func TestScaffold_RubyPresetResolvesRangeSelectorToCatalogPin(t *testing.T) {
	dir := t.TempDir()
	fakeMise := filepath.Join(dir, "mise")
	if err := os.WriteFile(fakeMise, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatalf("write fake mise: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	s := &Scaffolder{}
	got, err := s.resolveRubyVersion(dir, "3.3", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolveRubyVersion: %v", err)
	}
	if got == "" {
		t.Fatalf("expected non-empty ruby version")
	}
}

func TestScaffold_RenderRubyInstallCommand(t *testing.T) {
	got := renderRubyInstallCommand("3.2.2")
	for _, want := range []string{
		"RUN mise use -g --pin ruby@3.2.2",
		"RUN gem install bundler",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("renderRubyInstallCommand missing %q, got:\n%s", want, got)
		}
	}
}

func TestScaffold_RenderJavaInstallCommand(t *testing.T) {
	got := renderJavaInstallCommand("21.0.2")
	if !strings.Contains(got, "RUN mise use -g --pin java@21.0.2") {
		t.Errorf("renderJavaInstallCommand missing mise pin, got:\n%s", got)
	}
}

func TestScaffold_JavaPresetWritesPinnedDockerfile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "pom.xml"), []byte("<project><properties><java.version>21</java.version></properties></project>\n"), 0644)

	s := &Scaffolder{}
	if err := s.Scaffold(dir, Options{BuildTools: "java"}, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	dockerfilePath := filepath.Join(dir, ".sandman", "Dockerfile")
	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		"# sandman build-tools: java",
		"# sandman default-agent: opencode",
		"# sandman java-version: 21.0.2",
		"# sandman installed-agents: opencode",
		"FROM debian:bookworm-slim",
		"RUN mise use -g --pin java@21.0.2",
		"RUN MISE_VERSION=" + DefaultMISEVersion + " curl https://mise.run | MISE_INSTALL_PATH=/usr/local/bin/mise sh",
		"RUN npm install -g opencode-ai@" + DefaultBuiltInAgentVersion("opencode"),
	} {
		if !strings.Contains(content, want) {
			t.Errorf("Dockerfile missing %q, got:\n%s", want, content)
		}
	}
	if strings.Contains(content, "maven") || strings.Contains(content, "gradle") {
		t.Errorf("Dockerfile should not install maven or gradle (repos use mvnw/gradlew wrappers), got:\n%s", content)
	}
}

func TestScaffold_DeriveErlangOTPFromElixirVersion(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    string
	}{
		{name: "with otp suffix", version: "1.18.4-otp-28", want: "28"},
		{name: "with otp suffix and patch", version: "1.20.2-otp-29", want: "29"},
		{name: "bare version falls back to catalog", version: "1.18", want: "28"},
		{name: "selector falls back to catalog", version: "~> 1.18", want: "28"},
		{name: "empty falls back to catalog default", version: "", want: "29"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := deriveErlangOTPFromElixir(tt.version)
			if err != nil {
				t.Fatalf("deriveErlangOTPFromElixir(%q): %v", tt.version, err)
			}
			if got != tt.want {
				t.Errorf("deriveErlangOTPFromElixir(%q) = %q, want %q", tt.version, got, tt.want)
			}
		})
	}
}

func TestScaffold_RenderElixirInstallCommand(t *testing.T) {
	got := renderElixirInstallCommand("1.18.4-otp-28", "28")
	for _, want := range []string{
		"RUN mise use -g --pin erlang@28",
		"RUN mise use -g --pin elixir@1.18.4-otp-28",
		"mix local.rebar --force",
		"mix local.hex --force",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("renderElixirInstallCommand missing %q, got:\n%s", want, got)
		}
	}
}

func TestScaffold_ElixirPresetWritesPinnedDockerfile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".elixir_version"), []byte("1.18.4\n"), 0644); err != nil {
		t.Fatalf("write .elixir_version: %v", err)
	}

	s := &Scaffolder{}
	wantElixirVersion, err := s.resolveElixirVersion(dir, "", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolve elixir version: %v", err)
	}
	wantOTP, err := deriveErlangOTPFromElixir(wantElixirVersion)
	if err != nil {
		t.Fatalf("derive OTP: %v", err)
	}

	if err := s.Scaffold(dir, Options{BuildTools: "elixir"}, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	dockerfilePath := filepath.Join(dir, ".sandman", "Dockerfile")
	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		"# sandman build-tools: elixir",
		"# sandman default-agent: opencode",
		"# sandman elixir-version: " + wantElixirVersion,
		"# sandman erlang-version: " + wantOTP,
		"# sandman installed-agents: opencode",
		"FROM debian:bookworm-slim",
		// Erlang/OTP compilation requires libncurses-dev; without it
		// `mise use -g erlang` fails with "No curses library functions found".
		"libncurses-dev",
		"RUN mise use -g --pin erlang@" + wantOTP,
		"RUN mise use -g --pin elixir@" + wantElixirVersion,
		"RUN mix local.hex --force",
		"RUN mix local.rebar --force",
		"RUN MISE_VERSION=" + DefaultMISEVersion + " curl https://mise.run | MISE_INSTALL_PATH=/usr/local/bin/mise sh",
		"RUN npm install -g opencode-ai@" + DefaultBuiltInAgentVersion("opencode"),
	} {
		if !strings.Contains(content, want) {
			t.Errorf("Dockerfile missing %q, got:\n%s", want, content)
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
}

func TestScaffold_ElixirPresetAllAgentsGenerateFiles(t *testing.T) {
	for agent := range config.BuiltInAgentPresets {
		t.Run(agent, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "mix.exs"), []byte("defmodule Demo.MixProject do\n  use Mix.Project\n  def project do\n    [app: :demo, version: \"0.1.0\", elixir: \"1.18.4\"]\n  end\nend\n"), 0644); err != nil {
				t.Fatalf("write mix.exs: %v", err)
			}

			s := &Scaffolder{}
			wantElixirVersion, err := s.resolveElixirVersion(dir, "", &fakePrompter{confirm: true})
			if err != nil {
				t.Fatalf("resolve elixir version: %v", err)
			}
			wantOTP, err := deriveErlangOTPFromElixir(wantElixirVersion)
			if err != nil {
				t.Fatalf("derive OTP: %v", err)
			}

			if err := s.Scaffold(dir, Options{BuildTools: "elixir", Agent: agent}, &fakePrompter{confirm: true}); err != nil {
				t.Fatalf("scaffold: %v", err)
			}

			configPath := filepath.Join(dir, ".sandman", "config.yaml")
			cfg, err := config.Load(configPath)
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			if cfg.BuildTools != "elixir" {
				t.Errorf("expected build tools %q, got %q", "elixir", cfg.BuildTools)
			}

			dockerfileData, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
			if err != nil {
				t.Fatalf("read Dockerfile: %v", err)
			}
			content := string(dockerfileData)
			if !strings.Contains(content, "# sandman build-tools: elixir") {
				t.Errorf("Dockerfile missing elixir build-tools metadata, got:\n%s", content)
			}
			if !strings.Contains(content, "RUN mise use -g --pin erlang@"+wantOTP) {
				t.Errorf("Dockerfile missing pinned erlang install %q, got:\n%s", wantOTP, content)
			}
			if !strings.Contains(content, "RUN mise use -g --pin elixir@"+wantElixirVersion) {
				t.Errorf("Dockerfile missing pinned elixir install %q, got:\n%s", wantElixirVersion, content)
			}
		})
	}
}

func TestReadElixirVersionHint(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		content  string
		want     string
		wantOK   bool
	}{
		{
			name:     ".elixir_version with version",
			filename: ".elixir_version",
			content:  "1.18.4\n",
			want:     "1.18.4",
			wantOK:   true,
		},
		{
			name:     ".tool-versions with elixir line",
			filename: ".tool-versions",
			content:  "elixir 1.18.4\nerlang 28.5\n",
			want:     "1.18.4",
			wantOK:   true,
		},
		{
			name:     ".tool-versions with elixir at minor",
			filename: ".tool-versions",
			content:  "elixir 1.18\nerlang 28\n",
			want:     "1.18",
			wantOK:   true,
		},
		{
			name:     "mix.exs with elixir under project",
			filename: "mix.exs",
			content:  "defmodule Demo.MixProject do\n  use Mix.Project\n\n  def project do\n    [\n      app: :demo,\n      version: \"0.1.0\",\n      elixir: \"~> 1.18\",\n      elixirc_paths: elixirc_paths(Mix.env())\n    ]\n  end\n\n  defp deps do\n    [\n      {:plug, \"~> 1.11\"}\n    ]\n  end\nend\n",
			want:     "~> 1.18",
			wantOK:   true,
		},
		{
			name:     "mix.exs compact project form",
			filename: "mix.exs",
			content:  "defmodule Demo.MixProject do\n  use Mix.Project\n\n  def project, do: [app: :demo, version: \"0.1.0\", elixir: \"~> 1.18\"]\nend\n",
			want:     "~> 1.18",
			wantOK:   true,
		},
		{
			name:     "mix.exs with elixir dep ignored",
			filename: "mix.exs",
			content:  "defmodule Demo.MixProject do\n  use Mix.Project\n\n  def project do\n    [\n      app: :demo,\n      version: \"0.1.0\"\n    ]\n  end\n\n  defp deps do\n    [\n      {:elixir, \"~> 1.18\"},\n      {:plug, \"~> 1.11\"}\n    ]\n  end\nend\n",
			want:     "",
			wantOK:   false,
		},
		{
			name:     "mix.exs with no elixir line",
			filename: "mix.exs",
			content:  "defmodule Demo.MixProject do\n  use Mix.Project\n  def project, do: [app: :demo, version: \"0.1.0\"]\nend\n",
			want:     "",
			wantOK:   false,
		},
		{
			name:     ".elixir_version with comment",
			filename: ".elixir_version",
			content:  "# pinned by ops\n1.18.4\n",
			want:     "1.18.4",
			wantOK:   true,
		},
		{
			name:     "empty file",
			filename: ".elixir_version",
			content:  "",
			want:     "",
			wantOK:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, tt.filename)
			if err := os.WriteFile(path, []byte(tt.content), 0644); err != nil {
				t.Fatalf("write %s: %v", tt.filename, err)
			}

			got, ok, err := readElixirVersionHint(dir)
			if err != nil {
				t.Fatalf("readElixirVersionHint: %v", err)
			}
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v (got version %q)", ok, tt.wantOK, got)
			}
			if got != tt.want {
				t.Errorf("version = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateDockerfileMetadata_AllowsGoPreset(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".sandman"), 0755); err != nil {
		t.Fatalf("create .sandman: %v", err)
	}
	content := "# sandman build-tools: go\n# sandman default-agent: opencode\n# sandman installed-agents: opencode\n# sandman go-version: 1.24\n# sandman mise-version: " + DefaultMISEVersion + "\nFROM debian:bookworm-slim\n"
	if err := os.WriteFile(filepath.Join(dir, ".sandman", "Dockerfile"), []byte(content), 0644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}

	if err := ValidateDockerfileMetadata(dir, "go", "opencode"); err != nil {
		t.Fatalf("validate metadata: %v", err)
	}
}

func TestReadDockerfileMetadata_RubyVersion(t *testing.T) {
	dir := t.TempDir()
	content := "# sandman build-tools: ruby\n# sandman default-agent: opencode\n# sandman ruby-version: 3.2.2\n# sandman mise-version: " + DefaultMISEVersion + "\nFROM debian:bookworm-slim\n"
	path := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}

	meta, found, err := readDockerfileMetadata(path)
	if err != nil {
		t.Fatalf("readDockerfileMetadata: %v", err)
	}
	if !found {
		t.Fatal("expected metadata found")
	}
	if meta.RubyVersion != "3.2.2" {
		t.Errorf("RubyVersion = %q, want %q", meta.RubyVersion, "3.2.2")
	}
	if meta.BuildToolsPreset != "ruby" {
		t.Errorf("BuildToolsPreset = %q, want %q", meta.BuildToolsPreset, "ruby")
	}
}

func TestReadDockerfileMetadata_RustVersion(t *testing.T) {
	dir := t.TempDir()
	content := "# sandman build-tools: rust\n# sandman default-agent: opencode\n# sandman rust-version: 1.95.0\n# sandman mise-version: " + DefaultMISEVersion + "\nFROM debian:bookworm-slim\n"
	path := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}

	meta, found, err := readDockerfileMetadata(path)
	if err != nil {
		t.Fatalf("readDockerfileMetadata: %v", err)
	}
	if !found {
		t.Fatal("expected metadata found")
	}
	if meta.RustVersion != "1.95.0" {
		t.Errorf("RustVersion = %q, want %q", meta.RustVersion, "1.95.0")
	}
	if meta.BuildToolsPreset != "rust" {
		t.Errorf("BuildToolsPreset = %q, want %q", meta.BuildToolsPreset, "rust")
	}
}

func TestReadDockerfileMetadata_JavaVersion(t *testing.T) {
	dir := t.TempDir()
	content := "# sandman build-tools: java\n# sandman default-agent: opencode\n# sandman java-version: 21.0.2\n# sandman mise-version: " + DefaultMISEVersion + "\nFROM debian:bookworm-slim\n"
	path := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}

	meta, found, err := readDockerfileMetadata(path)
	if err != nil {
		t.Fatalf("readDockerfileMetadata: %v", err)
	}
	if !found {
		t.Fatal("expected metadata found")
	}
	if meta.JavaVersion != "21.0.2" {
		t.Errorf("JavaVersion = %q, want %q", meta.JavaVersion, "21.0.2")
	}
	if meta.BuildToolsPreset != "java" {
		t.Errorf("BuildToolsPreset = %q, want %q", meta.BuildToolsPreset, "java")
	}
}

func TestScaffold_RubyPresetWritesPinnedDockerfile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".ruby-version"), []byte("3.2.2\n"), 0644); err != nil {
		t.Fatalf("write .ruby-version: %v", err)
	}

	s := &Scaffolder{}
	wantRubyVersion, err := s.resolveRubyVersion(dir, "", &fakePrompter{confirm: true})
	if err != nil {
		t.Fatalf("resolve ruby version: %v", err)
	}

	if err := s.Scaffold(dir, Options{BuildTools: "ruby"}, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	dockerfilePath := filepath.Join(dir, ".sandman", "Dockerfile")
	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		"# sandman build-tools: ruby",
		"# sandman default-agent: opencode",
		"# sandman ruby-version: " + wantRubyVersion,
		"# sandman installed-agents: opencode",
		"FROM debian:bookworm-slim",
		"RUN mise use -g --pin ruby@" + wantRubyVersion,
		"RUN gem install bundler",
		"RUN MISE_VERSION=" + DefaultMISEVersion + " curl https://mise.run | MISE_INSTALL_PATH=/usr/local/bin/mise sh",
		"RUN npm install -g opencode-ai@" + DefaultBuiltInAgentVersion("opencode"),
	} {
		if !strings.Contains(content, want) {
			t.Errorf("Dockerfile missing %q, got:\n%s", want, content)
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
}

func TestScaffold_RubyPresetAllAgentsGenerateFiles(t *testing.T) {
	for agent := range config.BuiltInAgentPresets {
		t.Run(agent, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "Gemfile"), []byte("source 'https://rubygems.org'\ngem 'rails'\n"), 0644); err != nil {
				t.Fatalf("write Gemfile: %v", err)
			}

			s := &Scaffolder{}
			wantRubyVersion, err := s.resolveRubyVersion(dir, "", &fakePrompter{confirm: true})
			if err != nil {
				t.Fatalf("resolve ruby version: %v", err)
			}

			if err := s.Scaffold(dir, Options{BuildTools: "ruby", Agent: agent}, &fakePrompter{confirm: true}); err != nil {
				t.Fatalf("scaffold: %v", err)
			}

			configPath := filepath.Join(dir, ".sandman", "config.yaml")
			cfg, err := config.Load(configPath)
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			if cfg.BuildTools != "ruby" {
				t.Errorf("expected build tools %q, got %q", "ruby", cfg.BuildTools)
			}

			dockerfileData, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
			if err != nil {
				t.Fatalf("read Dockerfile: %v", err)
			}
			content := string(dockerfileData)
			if !strings.Contains(content, "# sandman build-tools: ruby") {
				t.Errorf("Dockerfile missing ruby build-tools metadata, got:\n%s", content)
			}
			if !strings.Contains(content, "RUN mise use -g --pin ruby@"+wantRubyVersion) {
				t.Errorf("Dockerfile missing pinned ruby install %q, got:\n%s", wantRubyVersion, content)
			}
			if !strings.Contains(content, "RUN gem install bundler") {
				t.Errorf("Dockerfile missing bundler install, got:\n%s", content)
			}
		})
	}
}

func TestScaffold_JavaPresetAllAgentsGenerateFiles(t *testing.T) {
	for agent := range config.BuiltInAgentPresets {
		t.Run(agent, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "pom.xml"), []byte("<project><properties><java.version>21</java.version></properties></project>\n"), 0644); err != nil {
				t.Fatalf("write pom.xml: %v", err)
			}

			s := &Scaffolder{}
			wantJavaVersion, err := s.resolveJavaVersion(dir, "", &fakePrompter{confirm: true})
			if err != nil {
				t.Fatalf("resolve java version: %v", err)
			}

			if err := s.Scaffold(dir, Options{BuildTools: "java", Agent: agent}, &fakePrompter{confirm: true}); err != nil {
				t.Fatalf("scaffold: %v", err)
			}

			configPath := filepath.Join(dir, ".sandman", "config.yaml")
			cfg, err := config.Load(configPath)
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			if cfg.BuildTools != "java" {
				t.Errorf("expected build tools %q, got %q", "java", cfg.BuildTools)
			}

			dockerfileData, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
			if err != nil {
				t.Fatalf("read Dockerfile: %v", err)
			}
			content := string(dockerfileData)
			if !strings.Contains(content, "# sandman build-tools: java") {
				t.Errorf("Dockerfile missing java build-tools metadata, got:\n%s", content)
			}
			if !strings.Contains(content, "RUN mise use -g --pin java@"+wantJavaVersion) {
				t.Errorf("Dockerfile missing pinned java install %q, got:\n%s", wantJavaVersion, content)
			}
		})
	}
}

func TestScaffold_MaterializesReviewPromptAndQualityRules(t *testing.T) {
	dir := t.TempDir()
	s := &Scaffolder{}
	if err := s.Scaffold(dir, Options{BuildTools: "generic"}, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	promptPath := filepath.Join(dir, ".sandman", "reviews", "review-prompt.md")
	promptData, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("read %s: %v", promptPath, err)
	}
	if string(promptData) != prompt.DefaultPRReviewPrompt() {
		t.Errorf("review-prompt.md should match the embedded default")
	}

	qualityPath := filepath.Join(dir, ".sandman", "reviews", "quality-rules.md")
	qualityData, err := os.ReadFile(qualityPath)
	if err != nil {
		t.Fatalf("read %s: %v", qualityPath, err)
	}
	if string(qualityData) != prompt.DefaultQualityRules() {
		t.Errorf("quality-rules.md should match the embedded default")
	}
}

func TestScaffold_ReInitPreservesUserEditedReviewFiles(t *testing.T) {
	dir := t.TempDir()
	s := &Scaffolder{}
	if err := s.Scaffold(dir, Options{BuildTools: "generic"}, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("first scaffold: %v", err)
	}

	promptPath := filepath.Join(dir, ".sandman", "reviews", "review-prompt.md")
	editedPrompt := []byte("# user-edited review prompt\n")
	if err := os.WriteFile(promptPath, editedPrompt, 0644); err != nil {
		t.Fatalf("write user-edited prompt: %v", err)
	}
	qualityPath := filepath.Join(dir, ".sandman", "reviews", "quality-rules.md")
	editedQuality := []byte("# user-edited quality rules\n")
	if err := os.WriteFile(qualityPath, editedQuality, 0644); err != nil {
		t.Fatalf("write user-edited quality rules: %v", err)
	}

	if err := s.Scaffold(dir, Options{BuildTools: "generic"}, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("second scaffold: %v", err)
	}

	promptData, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("read prompt after re-init: %v", err)
	}
	if string(promptData) != string(editedPrompt) {
		t.Errorf("user-edited review-prompt.md was clobbered by re-init\ngot: %q\nwant: %q", promptData, editedPrompt)
	}
	qualityData, err := os.ReadFile(qualityPath)
	if err != nil {
		t.Fatalf("read quality rules after re-init: %v", err)
	}
	if string(qualityData) != string(editedQuality) {
		t.Errorf("user-edited quality-rules.md was clobbered by re-init\ngot: %q\nwant: %q", qualityData, editedQuality)
	}
}

func TestScaffold_IgnoresLegacyPrioritySelectionPrompt(t *testing.T) {
	dir := t.TempDir()
	sandmanDir := filepath.Join(dir, ".sandman")
	if err := os.MkdirAll(sandmanDir, 0755); err != nil {
		t.Fatalf("create .sandman: %v", err)
	}

	legacyPromptPath := filepath.Join(sandmanDir, "priority-selection-prompt.md")
	legacyContent := []byte("# legacy prompt content that must not leak into auto-selection-prompt.md\n")
	if err := os.WriteFile(legacyPromptPath, legacyContent, 0644); err != nil {
		t.Fatalf("seed legacy prompt: %v", err)
	}

	s := &Scaffolder{}
	if err := s.Scaffold(dir, Options{BuildTools: "generic"}, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	autoPromptPath := filepath.Join(sandmanDir, "auto-selection-prompt.md")
	data, err := os.ReadFile(autoPromptPath)
	if err != nil {
		t.Fatalf("read auto-selection-prompt.md: %v", err)
	}

	got := string(data)
	if got != prompt.DefaultPriorityPrompt() {
		t.Errorf("auto-selection-prompt.md should equal the embedded default\ngot:\n%s\nwant:\n%s", got, prompt.DefaultPriorityPrompt())
	}
	if strings.Contains(got, string(legacyContent)) {
		t.Errorf("auto-selection-prompt.md must not contain the legacy prompt bytes; the soft migration is end-of-life\ngot:\n%s", got)
	}
}

func TestScaffold_InitMessageWritten(t *testing.T) {
	dir := t.TempDir()
	s := &Scaffolder{}
	buf := &bytes.Buffer{}

	if err := s.Scaffold(dir, Options{BuildTools: "generic", Writer: buf}, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	got := buf.String()
	if got == "" {
		t.Fatal("expected init summary message to be written to writer, got empty string")
	}
	if !strings.Contains(got, "Scaffold complete") {
		t.Errorf("expected message to contain %q", "Scaffold complete")
	}
	if !strings.Contains(got, "Preset:") {
		t.Errorf("expected message to contain %q", "Preset:")
	}
	if !strings.Contains(got, "generic") {
		t.Errorf("expected message to contain preset name %q", "generic")
	}
	if !strings.Contains(got, "opencode-ai@") {
		t.Errorf("expected message to contain agent pin %q", "opencode-ai@")
	}
	if !strings.Contains(got, "~/.agents/skills/sandman/") {
		t.Errorf("expected message to contain skill folder path %q", "~/.agents/skills/sandman/")
	}
	if !strings.Contains(got, ".sandman/Dockerfile") {
		t.Errorf("expected message to contain %q", ".sandman/Dockerfile")
	}
	if !strings.Contains(got, "minimal BuildToolsPreset") {
		t.Errorf("expected message to contain %q", "minimal BuildToolsPreset")
	}
}
