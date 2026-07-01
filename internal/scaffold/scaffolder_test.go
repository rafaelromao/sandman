package scaffold

import (
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
	for _, preset := range []string{"generic", "go", "dotnet", "node", "python"} {
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
	for _, preset := range []string{"generic", "go", "dotnet", "node", "python"} {
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
	for _, preset := range []string{"generic", "go", "dotnet", "node", "python"} {
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
			content: "defmodule Demo.MixProject do\n  use Mix.Project\n\n  def project do\n    [\n      app: :demo,\n      version: \"0.1.0\",\n      elixir: \"~> 1.18\",\n      elixirc_paths: elixirc_paths(Mix.env())\n    ]\n  end\n\n  defp deps do\n    [\n      {:plug, \"~> 1.11\"}\n    ]\n  end\nend\n",
			want:   "~> 1.18",
			wantOK: true,
		},
		{
			name:     "mix.exs with elixir dep ignored",
			filename: "mix.exs",
			content: "defmodule Demo.MixProject do\n  use Mix.Project\n\n  def project do\n    [\n      app: :demo,\n      version: \"0.1.0\"\n    ]\n  end\n\n  defp deps do\n    [\n      {:elixir, \"~> 1.18\"},\n      {:plug, \"~> 1.11\"}\n    ]\n  end\nend\n",
			want:   "",
			wantOK: false,
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
