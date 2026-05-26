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

func newInitTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestInit_CreatesSandmanFiles(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	newInitTestHome(t)

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
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve home dir: %v", err)
	}
	skillPath := filepath.Join(home, ".agents", "skills", "sandman", "SKILL.md")
	if _, err := os.Stat(skillPath); err != nil {
		t.Errorf("shared skill not created: %v", err)
	}
}

func TestInit_GenericBuildToolsScaffoldsPinnedDockerfile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	home := newInitTestHome(t)

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
	if !strings.Contains(dockerfile, "# sandman default-agent: opencode") {
		t.Fatalf("Dockerfile missing default-agent metadata, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "# sandman installed-agents: opencode,pi") {
		t.Fatalf("Dockerfile missing installed-agents metadata, got:\n%s", dockerfile)
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
	skillPath := filepath.Join(home, ".agents", "skills", "sandman", "SKILL.md")
	skillData, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("read shared skill: %v", err)
	}
	if want := prompt.SandmanSkill(); string(skillData) != want {
		t.Fatalf("shared skill mismatch\nwant:\n%s\ngot:\n%s", want, skillData)
	}
}

func TestInit_LeavesExistingSharedSkillUntouched(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	home := newInitTestHome(t)
	skillPath := filepath.Join(home, ".agents", "skills", "sandman", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillPath), 0755); err != nil {
		t.Fatalf("create skill dir: %v", err)
	}
	if err := os.WriteFile(skillPath, []byte("custom skill"), 0644); err != nil {
		t.Fatalf("seed skill: %v", err)
	}

	var out bytes.Buffer
	cmd := NewInitCmd()
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{"--build-tools", "generic"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("read shared skill: %v", err)
	}
	if got := string(data); got != "custom skill" {
		t.Fatalf("shared skill should not be overwritten\nwant:\n%s\ngot:\n%s", "custom skill", got)
	}
}

func TestInit_PythonBuildToolsScaffoldsPinnedDockerfile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	newInitTestHome(t)

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
	newInitTestHome(t)

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
	newInitTestHome(t)

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

func TestInit_DefaultsToNodePresetForNodeRepo(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	newInitTestHome(t)

	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"demo","engines":{"node":"20"}}`), 0644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}

	var out bytes.Buffer
	cmd := NewInitCmd()
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	configData, err := os.ReadFile(filepath.Join(dir, ".sandman", "config.yaml"))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	if !strings.Contains(string(configData), "build_tools: node") {
		t.Fatalf("config missing node build_tools preset, got:\n%s", configData)
	}

	dockerfileData, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile := string(dockerfileData)
	if !strings.Contains(dockerfile, "# sandman build-tools: node") {
		t.Fatalf("Dockerfile missing node build-tools metadata, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "RUN corepack enable") {
		t.Fatalf("Dockerfile missing corepack enable, got:\n%s", dockerfile)
	}
}

func TestInit_DefaultsToDotnetPresetForDotnetRepo(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	newInitTestHome(t)

	if err := os.WriteFile(filepath.Join(dir, "global.json"), []byte(`{"sdk":{"version":"8.0.100"}}`), 0644); err != nil {
		t.Fatalf("write global.json: %v", err)
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
	if !strings.Contains(string(configData), "build_tools: dotnet") {
		t.Fatalf("config missing dotnet build_tools preset, got:\n%s", configData)
	}

	dockerfileData, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile := string(dockerfileData)
	if !strings.Contains(dockerfile, "# sandman build-tools: dotnet") {
		t.Fatalf("Dockerfile missing dotnet build-tools metadata, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "# sandman dotnet-version:") {
		t.Fatalf("Dockerfile missing dotnet-version metadata, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "RUN mise use -g --pin dotnet@") {
		t.Fatalf("Dockerfile missing pinned dotnet install, got:\n%s", dockerfile)
	}
}

func TestInit_ExplicitGenericOverridesNodeRepoHint(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	newInitTestHome(t)

	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"demo","engines":{"node":"20"}}`), 0644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}

	var out bytes.Buffer
	cmd := NewInitCmd()
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{"--build-tools", "generic"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	configData, err := os.ReadFile(filepath.Join(dir, ".sandman", "config.yaml"))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	if !strings.Contains(string(configData), "build_tools: generic") {
		t.Fatalf("config missing generic build_tools preset, got:\n%s", configData)
	}
}

func TestInit_DefaultAgentFlagSelectsConfigPreset(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	newInitTestHome(t)

	var out bytes.Buffer
	cmd := NewInitCmd()
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{"--build-tools", "generic", "--default-agent", "pi"})

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
	if !strings.Contains(string(configData), "default_agent: pi") {
		t.Errorf("config missing default_agent preset, got:\n%s", configData)
	}

	dockerfileData, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	if !strings.Contains(string(dockerfileData), "RUN npm install -g opencode-ai@"+scaffold.DefaultBuiltInAgentVersion("opencode")) {
		t.Errorf("Dockerfile missing opencode install, got:\n%s", dockerfileData)
	}
	if !strings.Contains(string(dockerfileData), "RUN npm install -g --ignore-scripts @earendil-works/pi-coding-agent@"+scaffold.DefaultBuiltInAgentVersion("pi")) {
		t.Errorf("Dockerfile missing pi install, got:\n%s", dockerfileData)
	}
	if !strings.Contains(string(dockerfileData), "RUN mise use -g --pin node@22.19.0") {
		t.Errorf("Dockerfile missing node pin for pi, got:\n%s", dockerfileData)
	}
}

func TestInit_InfersToolVersionRepoWhenUnset(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	newInitTestHome(t)

	var out bytes.Buffer
	cmd := NewInitCmd()
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{"--build-tools", "generic"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dockerfileData, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	if !strings.Contains(string(dockerfileData), "# sandman installed-agents: opencode,pi") {
		t.Fatalf("expected installed-agents metadata, got:\n%s", dockerfileData)
	}
}

func TestInit_ExistingDirectoryPrompts(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	newInitTestHome(t)
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
