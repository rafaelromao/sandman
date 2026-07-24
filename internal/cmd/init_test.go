package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/scaffold"
	"github.com/rafaelromao/sandman/internal/skill"
)

func init() {
	syncSandmanSkill = func(skill.SyncOptions) error { return nil }
}

func TestInit_CreatesSandmanFiles(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	called := 0
	oldInstall := syncSandmanSkill
	syncSandmanSkill = func(skill.SyncOptions) error {
		called++
		return nil
	}
	t.Cleanup(func() { syncSandmanSkill = oldInstall })

	var out bytes.Buffer
	cmd := NewInitCmd()
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{"--build-tools", "generic"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called != 1 {
		t.Fatalf("expected sandman skill installer to be called once, got %d", called)
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

func TestInit_VerboseFlagSupportsLongAndShortForms(t *testing.T) {
	for _, args := range [][]string{{"--verbose"}, {"-v"}} {
		cmd := NewInitCmd()
		flag := cmd.Flags().Lookup("verbose")
		if flag == nil {
			t.Fatal("--verbose flag missing")
		}
		if flag.Shorthand != "v" {
			t.Fatalf("--verbose shorthand = %q, want %q", flag.Shorthand, "v")
		}
		if err := cmd.ParseFlags(args); err != nil {
			t.Fatalf("%v should parse: %v", args, err)
		}
		verbose, err := cmd.Flags().GetBool("verbose")
		if err != nil {
			t.Fatalf("read verbose flag: %v", err)
		}
		if !verbose {
			t.Fatalf("%v did not enable verbose output", args)
		}
	}
}

func TestInit_VerboseControlsScaffoldDiagnostics(t *testing.T) {
	tests := []struct {
		name       string
		verboseArg string
		wantWarn   bool
	}{
		{name: "default is quiet", wantWarn: false},
		{name: "long form", verboseArg: "--verbose", wantWarn: true},
		{name: "short form", verboseArg: "-v", wantWarn: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			if err := os.MkdirAll(filepath.Join(dir, ".sandman"), 0755); err != nil {
				t.Fatalf("create .sandman: %v", err)
			}
			hooksDir := filepath.Join(dir, ".git", "hooks")
			if err := os.MkdirAll(hooksDir, 0755); err != nil {
				t.Fatalf("create hooks directory: %v", err)
			}
			if err := os.WriteFile(filepath.Join(hooksDir, "pre-commit"), []byte("#!/bin/sh\n"), 0755); err != nil {
				t.Fatalf("create pre-commit hook: %v", err)
			}

			var out bytes.Buffer
			cmd := NewInitCmd()
			cmd.SetOut(&out)
			cmd.SetIn(strings.NewReader("y\n"))
			args := []string{"--build-tools", "generic"}
			if tt.verboseArg != "" {
				args = append(args, tt.verboseArg)
			}
			cmd.SetArgs(args)

			if err := cmd.Execute(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(out.String(), "Directory .sandman/ already exists. Overwrite?") {
				t.Fatalf("overwrite prompt missing from output: %q", out.String())
			}
			gotWarn := strings.Contains(out.String(), "pre-commit hook already exists")
			if gotWarn != tt.wantWarn {
				t.Fatalf("diagnostic warning present = %t, want %t; output: %q", gotWarn, tt.wantWarn, out.String())
			}
			if !strings.Contains(out.String(), "Scaffold complete.") {
				t.Fatalf("init summary missing from output: %q", out.String())
			}
		})
	}
}

func TestInit_ReviewCommandFlagStoredInConfig(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	oldInstall := syncSandmanSkill
	syncSandmanSkill = func(opts skill.SyncOptions) error {
		if opts.ReviewCommand != "/custom review" {
			t.Fatalf("expected sync review command, got %q", opts.ReviewCommand)
		}
		return nil
	}
	t.Cleanup(func() { syncSandmanSkill = oldInstall })

	var out bytes.Buffer
	cmd := NewInitCmd()
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{"--build-tools", "generic", "--review-command", "/custom review"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	configData, err := os.ReadFile(filepath.Join(dir, ".sandman", "config.yaml"))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	if !strings.Contains(string(configData), "review_command: /custom review") {
		t.Fatalf("config missing review command override, got:\n%s", configData)
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
	if !strings.Contains(string(configData), "review_command: /sandman review") {
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
	if !strings.Contains(dockerfile, "# sandman installed-agents: opencode") {
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
	if !strings.Contains(dockerfile, "RUN npm install -g opencode-ai@") {
		t.Fatalf("Dockerfile missing opencode install, got:\n%s", dockerfile)
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

func TestInit_RustBuildToolsScaffoldsPinnedDockerfile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if err := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte("[package]\nname = \"demo\"\nversion = \"0.1.0\"\nrust-version = \"1.77.0\"\n"), 0644); err != nil {
		t.Fatalf("write Cargo.toml: %v", err)
	}

	var out bytes.Buffer
	cmd := NewInitCmd()
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(nil)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	configData, err := os.ReadFile(filepath.Join(dir, ".sandman", "config.yaml"))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	if !strings.Contains(string(configData), "build_tools: rust") {
		t.Fatalf("config missing rust build_tools preset, got:\n%s", configData)
	}

	dockerfileData, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile := string(dockerfileData)
	if !strings.Contains(dockerfile, "# sandman build-tools: rust") {
		t.Fatalf("Dockerfile missing build-tools metadata, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "# sandman rust-version: 1.77.0") {
		t.Fatalf("Dockerfile missing rust-version metadata, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "RUN mise use -g --pin rust@1.77.0") {
		t.Fatalf("Dockerfile missing pinned rust install, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "RUN npm install -g opencode-ai@") {
		t.Fatalf("Dockerfile missing opencode install, got:\n%s", dockerfile)
	}
}

func TestInit_GenericBuildToolsStillWinsOnRustRepo(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if err := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte("[package]\nname = \"demo\"\nversion = \"0.1.0\"\nrust-version = \"1.77.0\"\n"), 0644); err != nil {
		t.Fatalf("write Cargo.toml: %v", err)
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

	dockerfileData, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile := string(dockerfileData)
	if !strings.Contains(dockerfile, "# sandman build-tools: generic") {
		t.Fatalf("Dockerfile missing generic metadata, got:\n%s", dockerfile)
	}
	if strings.Contains(dockerfile, "# sandman rust-version:") {
		t.Fatalf("Dockerfile unexpectedly contains rust metadata, got:\n%s", dockerfile)
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

func TestInit_ElixirBuildToolsScaffoldsPinnedDockerfile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	var out bytes.Buffer
	cmd := NewInitCmd()
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{"--build-tools", "elixir"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	configData, err := os.ReadFile(filepath.Join(dir, ".sandman", "config.yaml"))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	if !strings.Contains(string(configData), "build_tools: elixir") {
		t.Fatalf("config missing elixir build_tools preset, got:\n%s", configData)
	}

	dockerfileData, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile := string(dockerfileData)
	if !strings.Contains(dockerfile, "# sandman build-tools: elixir") {
		t.Fatalf("Dockerfile missing build-tools metadata, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "# sandman elixir-version:") {
		t.Fatalf("Dockerfile missing elixir-version metadata, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "# sandman erlang-version:") {
		t.Fatalf("Dockerfile missing erlang-version metadata, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "RUN mise use -g --pin elixir@") {
		t.Fatalf("Dockerfile missing pinned elixir install, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "RUN mise use -g --pin erlang@") {
		t.Fatalf("Dockerfile missing pinned erlang install, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "RUN mix local.hex --force") {
		t.Fatalf("Dockerfile missing hex install, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "RUN mix local.rebar --force") {
		t.Fatalf("Dockerfile missing rebar3 install, got:\n%s", dockerfile)
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

func TestInit_DefaultsToElixirPresetForElixirRepo(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if err := os.WriteFile(filepath.Join(dir, "mix.exs"), []byte("defmodule Demo.MixProject do\n  use Mix.Project\n  def project do\n    [app: :demo, version: \"0.1.0\", elixir: \"~> 1.18\"]\n  end\nend\n"), 0644); err != nil {
		t.Fatalf("write mix.exs: %v", err)
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
	if !strings.Contains(string(configData), "build_tools: elixir") {
		t.Fatalf("config missing elixir build_tools preset, got:\n%s", configData)
	}

	dockerfileData, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile := string(dockerfileData)
	if !strings.Contains(dockerfile, "# sandman build-tools: elixir") {
		t.Fatalf("Dockerfile missing elixir build-tools metadata, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "# sandman elixir-version:") {
		t.Fatalf("Dockerfile missing elixir-version metadata, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "RUN mise use -g --pin elixir@") {
		t.Fatalf("Dockerfile missing pinned elixir install, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "RUN mix local.rebar --force") {
		t.Fatalf("Dockerfile missing rebar3 install, got:\n%s", dockerfile)
	}
}

func TestInit_ExplicitGenericOverridesElixirRepoHint(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if err := os.WriteFile(filepath.Join(dir, "mix.exs"), []byte("defmodule Demo.MixProject do\n  use Mix.Project\n  def project do\n    [app: :demo, version: \"0.1.0\", elixir: \"~> 1.18\"]\n  end\nend\n"), 0644); err != nil {
		t.Fatalf("write mix.exs: %v", err)
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

func TestInit_ElixirBuildToolsFlagHelpTextMentionsElixir(t *testing.T) {
	cmd := NewInitCmd()
	flag := cmd.Flags().Lookup("build-tools")
	if flag == nil {
		t.Fatalf("--build-tools flag missing")
	}
	if !strings.Contains(flag.Usage, "elixir") {
		t.Fatalf("--build-tools help text missing elixir, got: %q", flag.Usage)
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

func TestInit_DefaultsToNodePresetForNodeRepo(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

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

func TestInit_RubyBuildToolsScaffoldsPinnedDockerfile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	var out bytes.Buffer
	cmd := NewInitCmd()
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{"--build-tools", "ruby"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	configData, err := os.ReadFile(filepath.Join(dir, ".sandman", "config.yaml"))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	if !strings.Contains(string(configData), "build_tools: ruby") {
		t.Fatalf("config missing ruby build_tools preset, got:\n%s", configData)
	}

	dockerfileData, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile := string(dockerfileData)
	if !strings.Contains(dockerfile, "# sandman build-tools: ruby") {
		t.Fatalf("Dockerfile missing build-tools metadata, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "# sandman ruby-version:") {
		t.Fatalf("Dockerfile missing ruby-version metadata, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "RUN mise use -g --pin ruby@") {
		t.Fatalf("Dockerfile missing pinned ruby install, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "RUN gem install bundler") {
		t.Fatalf("Dockerfile missing bundler install, got:\n%s", dockerfile)
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

func TestInit_DefaultsToRubyPresetForRubyRepo(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if err := os.WriteFile(filepath.Join(dir, "Gemfile"), []byte("source 'https://rubygems.org'\ngem 'rails'\n"), 0644); err != nil {
		t.Fatalf("write Gemfile: %v", err)
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
	if !strings.Contains(string(configData), "build_tools: ruby") {
		t.Fatalf("config missing ruby build_tools preset, got:\n%s", configData)
	}

	dockerfileData, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile := string(dockerfileData)
	if !strings.Contains(dockerfile, "# sandman build-tools: ruby") {
		t.Fatalf("Dockerfile missing ruby build-tools metadata, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "# sandman ruby-version:") {
		t.Fatalf("Dockerfile missing ruby-version metadata, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "RUN mise use -g --pin ruby@") {
		t.Fatalf("Dockerfile missing pinned ruby install, got:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "RUN gem install bundler") {
		t.Fatalf("Dockerfile missing bundler install, got:\n%s", dockerfile)
	}
}

func TestInit_ExplicitGenericOverridesRubyRepoHint(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if err := os.WriteFile(filepath.Join(dir, "Gemfile"), []byte("source 'https://rubygems.org'\ngem 'rails'\n"), 0644); err != nil {
		t.Fatalf("write Gemfile: %v", err)
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

func TestInit_RubyBuildToolsFlagHelpTextMentionsRuby(t *testing.T) {
	cmd := NewInitCmd()
	flag := cmd.Flags().Lookup("build-tools")
	if flag == nil {
		t.Fatalf("--build-tools flag missing")
	}
	if !strings.Contains(flag.Usage, "ruby") {
		t.Fatalf("--build-tools help text missing ruby, got: %q", flag.Usage)
	}
}

func TestInit_ExplicitGenericOverridesNodeRepoHint(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

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

	var out bytes.Buffer
	cmd := NewInitCmd()
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{"--build-tools", "generic", "--agent", "opencode"})

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
	if !strings.Contains(string(configData), "agent: opencode") {
		t.Errorf("config missing agent preset, got:\n%s", configData)
	}

	dockerfileData, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	if !strings.Contains(string(dockerfileData), "RUN npm install -g opencode-ai@") {
		t.Errorf("Dockerfile missing opencode install, got:\n%s", dockerfileData)
	}
}

func TestInit_InfersToolVersionRepoWhenUnset(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

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
	if !strings.Contains(string(dockerfileData), "# sandman installed-agents: opencode") {
		t.Fatalf("expected installed-agents metadata, got:\n%s", dockerfileData)
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

func TestInit_ParallelReviewsFlagOverridesPersistedDefault(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantInYAML  string
		wantErr     bool
		errContains string
	}{
		{
			name:       "default persists 1",
			args:       []string{"--build-tools", "generic"},
			wantInYAML: "parallel_reviews: 1",
		},
		{
			name:       "explicit 8 persists 8",
			args:       []string{"--build-tools", "generic", "--parallel-reviews", "8"},
			wantInYAML: "parallel_reviews: 8",
		},
		{
			name:       "explicit 0 persists 1",
			args:       []string{"--build-tools", "generic", "--parallel-reviews", "0"},
			wantInYAML: "parallel_reviews: 1",
		},
		{
			name:       "sentinel -1 persists 1",
			args:       []string{"--build-tools", "generic", "--parallel-reviews", "-1"},
			wantInYAML: "parallel_reviews: 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)

			var out bytes.Buffer
			cmd := NewInitCmd()
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetIn(strings.NewReader(""))
			cmd.SetArgs(tt.args)

			err := cmd.Execute()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Fatalf("error should contain %q, got %v", tt.errContains, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			data, err := os.ReadFile(filepath.Join(dir, ".sandman", "config.yaml"))
			if err != nil {
				t.Fatalf("read config.yaml: %v", err)
			}
			if !strings.Contains(string(data), tt.wantInYAML) {
				t.Fatalf("config.yaml missing %q, got:\n%s", tt.wantInYAML, data)
			}
		})
	}
}

func TestInit_ParallelFlagOverridesPersistedDefault(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantInYAML string
	}{
		{
			name:       "default persists 1",
			args:       []string{"--build-tools", "generic"},
			wantInYAML: "parallel: 1",
		},
		{
			name:       "explicit 8 persists 8",
			args:       []string{"--build-tools", "generic", "--parallel", "8"},
			wantInYAML: "parallel: 8",
		},
		{
			name:       "explicit 0 persists 1",
			args:       []string{"--build-tools", "generic", "--parallel", "0"},
			wantInYAML: "parallel: 1",
		},
		{
			name:       "sentinel -1 persists 1",
			args:       []string{"--build-tools", "generic", "--parallel", "-1"},
			wantInYAML: "parallel: 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)

			var out bytes.Buffer
			cmd := NewInitCmd()
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetIn(strings.NewReader(""))
			cmd.SetArgs(tt.args)

			err := cmd.Execute()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			data, err := os.ReadFile(filepath.Join(dir, ".sandman", "config.yaml"))
			if err != nil {
				t.Fatalf("read config.yaml: %v", err)
			}
			if !strings.Contains(string(data), tt.wantInYAML) {
				t.Fatalf("config.yaml missing %q, got:\n%s", tt.wantInYAML, data)
			}
		})
	}
}

func TestInit_RetriesFlagOverridesPersistedDefault(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantInYAML  string
		wantErr     bool
		errContains string
	}{
		{
			name:       "default persists 3",
			args:       []string{"--build-tools", "generic"},
			wantInYAML: "retries: 3",
		},
		{
			name:       "explicit 5 persists 5",
			args:       []string{"--build-tools", "generic", "--retries", "5"},
			wantInYAML: "retries: 5",
		},
		{
			name:       "explicit 0 persists 0",
			args:       []string{"--build-tools", "generic", "--retries", "0"},
			wantInYAML: "retries: 0",
		},
		{
			name:       "sentinel -1 persists 3",
			args:       []string{"--build-tools", "generic", "--retries", "-1"},
			wantInYAML: "retries: 3",
		},
		{
			name:        "below sentinel rejected",
			args:        []string{"--build-tools", "generic", "--retries", "-2"},
			wantErr:     true,
			errContains: "retries",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)

			var out bytes.Buffer
			cmd := NewInitCmd()
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetIn(strings.NewReader(""))
			cmd.SetArgs(tt.args)

			err := cmd.Execute()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Fatalf("error should contain %q, got %v", tt.errContains, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			data, err := os.ReadFile(filepath.Join(dir, ".sandman", "config.yaml"))
			if err != nil {
				t.Fatalf("read config.yaml: %v", err)
			}
			if !strings.Contains(string(data), tt.wantInYAML) {
				t.Fatalf("config.yaml missing %q, got:\n%s", tt.wantInYAML, data)
			}
		})
	}
}

func TestInit_RunIdleTimeoutFlagOverridesPersistedDefault(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantInYAML  string
		wantErr     bool
		errContains string
	}{
		{
			name:       "default persists 1800",
			args:       []string{"--build-tools", "generic"},
			wantInYAML: "run_idle_timeout: 1800",
		},
		{
			name:       "explicit 600 persists 600",
			args:       []string{"--build-tools", "generic", "--run-idle-timeout", "600"},
			wantInYAML: "run_idle_timeout: 600",
		},
		{
			name:       "explicit 0 persists 0",
			args:       []string{"--build-tools", "generic", "--run-idle-timeout", "0"},
			wantInYAML: "run_idle_timeout: 0",
		},
		{
			name:       "sentinel -1 persists 1800",
			args:       []string{"--build-tools", "generic", "--run-idle-timeout", "-1"},
			wantInYAML: "run_idle_timeout: 1800",
		},
		{
			name:        "below sentinel rejected",
			args:        []string{"--build-tools", "generic", "--run-idle-timeout", "-2"},
			wantErr:     true,
			errContains: "run_idle_timeout",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)

			var out bytes.Buffer
			cmd := NewInitCmd()
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetIn(strings.NewReader(""))
			cmd.SetArgs(tt.args)

			err := cmd.Execute()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Fatalf("error should contain %q, got %v", tt.errContains, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			data, err := os.ReadFile(filepath.Join(dir, ".sandman", "config.yaml"))
			if err != nil {
				t.Fatalf("read config.yaml: %v", err)
			}
			if !strings.Contains(string(data), tt.wantInYAML) {
				t.Fatalf("config.yaml missing %q, got:\n%s", tt.wantInYAML, data)
			}
		})
	}
}
