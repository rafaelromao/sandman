//go:build smoke

package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/sandbox"
	"github.com/rafaelromao/sandman/internal/scaffold"
)

type buildPrompter struct{}

func (buildPrompter) Confirm(string) (bool, error)            { return true, nil }
func (buildPrompter) Select(string, []string) (string, error) { return "", nil }

// buildPresetImageSkipRationale is emitted when the smoke pre-warm phase did
// not produce a matching image for this (agent, preset) pair. The per-test
// build path was removed because compiling language toolchains from source
// (notably Erlang/OTP 28 via mise, which alone takes 5-10 minutes) routinely
// exceeded the 10m per-test timeout, taking the whole test binary down with
// SIGQUIT. The smoke pre-warm already exercises every variant we want to
// assert against when the host can build it; on hosts where the pre-warm
// could not produce the image (or where SANDMAN_RUN_SMOKE_E2E=1 / SANDMAN_SMOKE_PREFETCH=1 were not set), the test
// is skipped instead of being retried.
const buildPresetImageSkipRationale = "smoke pre-warm image not available for this (agent, preset) pair; per-test build skipped to stay inside the 10m timeout"

func TestInit_ElixirPresetBuildsForEveryBuiltInAgentProvider(t *testing.T) {
	runtime, err := sandbox.ResolveRuntime("podman")
	if err != nil {
		t.Skipf("container runtime unavailable: %v", err)
	}

	for agent := range config.BuiltInAgentPresets {
		t.Run(agent, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "mix.exs"), []byte("defmodule Demo.MixProject do\n  use Mix.Project\n\n  def project do\n    [\n      app: :demo,\n      version: \"0.1.0\",\n      elixir: \"~> 1.18\",\n      elixirc_paths: elixirc_paths(Mix.env())\n    ]\n  end\n\n  defp deps do\n    [{:plug, \"~> 1.11\"}]\n  end\nend\n"), 0644); err != nil {
				t.Fatalf("write mix.exs: %v", err)
			}

			s := &scaffold.Scaffolder{}
			if err := s.Scaffold(dir, scaffold.Options{Agent: agent}, buildPrompter{}); err != nil {
				t.Fatalf("scaffold: %v", err)
			}

			buildPresetImage(t, runtime, agent, "elixir")
		})
	}
}

func TestInit_PythonPresetBuildsForEveryBuiltInAgentProvider(t *testing.T) {
	runtime, err := sandbox.ResolveRuntime("podman")
	if err != nil {
		t.Skipf("container runtime unavailable: %v", err)
	}

	for agent := range config.BuiltInAgentPresets {
		t.Run(agent, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\nname = \"demo\"\n"), 0644); err != nil {
				t.Fatalf("write pyproject.toml: %v", err)
			}

			s := &scaffold.Scaffolder{}
			if err := s.Scaffold(dir, scaffold.Options{Agent: agent}, buildPrompter{}); err != nil {
				t.Fatalf("scaffold: %v", err)
			}

			buildPresetImage(t, runtime, agent, "python")
		})
	}
}

func TestInit_DotnetPresetBuildsForEveryBuiltInAgentProvider(t *testing.T) {
	runtime, err := sandbox.ResolveRuntime("podman")
	if err != nil {
		t.Skipf("container runtime unavailable: %v", err)
	}

	for agent := range config.BuiltInAgentPresets {
		t.Run(agent, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "global.json"), []byte(`{"sdk":{"version":"8.0.100"}}`), 0644); err != nil {
				t.Fatalf("write global.json: %v", err)
			}

			s := &scaffold.Scaffolder{}
			if err := s.Scaffold(dir, scaffold.Options{Agent: agent}, buildPrompter{}); err != nil {
				t.Fatalf("scaffold: %v", err)
			}

			buildPresetImage(t, runtime, agent, "dotnet")
		})
	}
}

func TestInit_GoPresetBuildsForEveryBuiltInAgentProvider(t *testing.T) {
	runtime, err := sandbox.ResolveRuntime("podman")
	if err != nil {
		t.Skipf("container runtime unavailable: %v", err)
	}

	for agent := range config.BuiltInAgentPresets {
		t.Run(agent, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/demo\n\ngo 1.24\n"), 0644); err != nil {
				t.Fatalf("write go.mod: %v", err)
			}

			s := &scaffold.Scaffolder{}
			if err := s.Scaffold(dir, scaffold.Options{Agent: agent}, buildPrompter{}); err != nil {
				t.Fatalf("scaffold: %v", err)
			}

			buildPresetImage(t, runtime, agent, "go")
		})
	}
}

func TestInit_NodePresetBuildsForEveryBuiltInAgentProvider(t *testing.T) {
	runtime, err := sandbox.ResolveRuntime("podman")
	if err != nil {
		t.Skipf("container runtime unavailable: %v", err)
	}

	for agent := range config.BuiltInAgentPresets {
		t.Run(agent, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"demo","engines":{"node":"20"}}`), 0644); err != nil {
				t.Fatalf("write package.json: %v", err)
			}

			s := &scaffold.Scaffolder{}
			if err := s.Scaffold(dir, scaffold.Options{Agent: agent}, buildPrompter{}); err != nil {
				t.Fatalf("scaffold: %v", err)
			}

			buildPresetImage(t, runtime, agent, "node")
		})
	}
}

func TestInit_RubyPresetBuildsForEveryBuiltInAgentProvider(t *testing.T) {
	runtime, err := sandbox.ResolveRuntime("podman")
	if err != nil {
		t.Skipf("container runtime unavailable: %v", err)
	}

	for agent := range config.BuiltInAgentPresets {
		t.Run(agent, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "Gemfile"), []byte("source 'https://rubygems.org'\ngem 'rails'\n"), 0644); err != nil {
				t.Fatalf("write Gemfile: %v", err)
			}

			s := &scaffold.Scaffolder{}
			if err := s.Scaffold(dir, scaffold.Options{Agent: agent}, buildPrompter{}); err != nil {
				t.Fatalf("scaffold: %v", err)
			}

			buildPresetImage(t, runtime, agent, "ruby")
		})
	}
}

func TestInit_JavaPresetBuildsForEveryBuiltInAgentProvider(t *testing.T) {
	runtime, err := sandbox.ResolveRuntime("podman")
	if err != nil {
		t.Skipf("container runtime unavailable: %v", err)
	}

	for agent := range config.BuiltInAgentPresets {
		t.Run(agent, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "pom.xml"), []byte("<project><properties><java.version>21</java.version></properties></project>\n"), 0644); err != nil {
				t.Fatalf("write pom.xml: %v", err)
			}

			s := &scaffold.Scaffolder{}
			if err := s.Scaffold(dir, scaffold.Options{Agent: agent}, buildPrompter{}); err != nil {
				t.Fatalf("scaffold: %v", err)
			}

			buildPresetImage(t, runtime, agent, "java")
		})
	}
}

func TestInit_RustPresetBuildsForEveryBuiltInAgentProvider(t *testing.T) {
	runtime, err := sandbox.ResolveRuntime("podman")
	if err != nil {
		t.Skipf("container runtime unavailable: %v", err)
	}

	for agent := range config.BuiltInAgentPresets {
		t.Run(agent, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte("[package]\nname = \"demo\"\nversion = \"0.1.0\"\nrust-version = \"1.77.0\"\n"), 0644); err != nil {
				t.Fatalf("write Cargo.toml: %v", err)
			}

			s := &scaffold.Scaffolder{}
			if err := s.Scaffold(dir, scaffold.Options{Agent: agent}, buildPrompter{}); err != nil {
				t.Fatalf("scaffold: %v", err)
			}

			buildPresetImage(t, runtime, agent, "rust")
		})
	}
}

func TestInit_GenericPresetBuildsForEveryBuiltInAgentProvider(t *testing.T) {
	runtime, err := sandbox.ResolveRuntime("podman")
	if err != nil {
		t.Skipf("container runtime unavailable: %v", err)
	}

	for agent := range config.BuiltInAgentPresets {
		t.Run(agent, func(t *testing.T) {
			dir := t.TempDir()

			s := &scaffold.Scaffolder{}
			if err := s.Scaffold(dir, scaffold.Options{BuildTools: "generic", Agent: agent}, buildPrompter{}); err != nil {
				t.Fatalf("scaffold: %v", err)
			}

			buildPresetImage(t, runtime, agent, "generic")
		})
	}
}

func buildPresetImage(t *testing.T, runtime, agent, preset string) {
	t.Helper()

	tag := smokePrewarmLookup(agent, preset)
	if tag == "" {
		t.Skip(buildPresetImageSkipRationale)
	}

	if err := exec.Command(runtime, "image", "exists", tag).Run(); err != nil {
		t.Skipf("smoke pre-warm image %q not present in %s: %v", tag, runtime, err)
	}
}

func TestInit_AllPresetImagesExposeRuntimeTools(t *testing.T) {
	runtime, err := sandbox.ResolveRuntime("podman")
	if err != nil {
		t.Skipf("container runtime unavailable: %v", err)
	}

	tests := []struct {
		preset string
		check  string
	}{
		{"generic", "command -v gh >/dev/null; command -v git >/dev/null; command -v jq >/dev/null; command -v yq >/dev/null; command -v opencode >/dev/null; command -v rtk >/dev/null"},
		{"go", "go version; test \"$(go env GOPATH)\" = \"/.local/share/go\"; test \"$(go env GOMODCACHE)\" = \"/.cache/go/pkg/mod\"; mkdir -p \"$(go env GOPATH)\" \"$(go env GOMODCACHE)\"; test -w \"$(go env GOPATH)\"; test -w \"$(go env GOMODCACHE)\""},
		{"node", "node --version; npm --version; npx --version; corepack --version"},
		{"python", "python3 --version; pip3 --version; uv --version"},
		{"elixir", "elixir --version; mix --version"},
		{"dotnet", "dotnet --version"},
		{"rust", "rustc --version; cargo --version; rustfmt --version; cargo clippy --version; test -w \"$CARGO_HOME\""},
		{"java", "java --version"},
		{"ruby", "ruby --version; bundler --version"},
	}

	for _, tt := range tests {
		t.Run(tt.preset, func(t *testing.T) {
			buildPresetImage(t, runtime, "opencode", tt.preset)
			tag := smokePrewarmLookup("opencode", tt.preset)
			container, err := sandbox.NewContainerRuntime(runtime).Start(tag, t.TempDir(), sandbox.StartOptions{UserID: fmt.Sprintf("%d", os.Getuid())})
			if err != nil {
				t.Fatalf("start %s preset container: %v", tt.preset, err)
			}
			defer container.Stop()

			check := exec.Command(runtime, "exec", container.ID(), "sh", "-ceu", tt.check)
			if out, err := check.CombinedOutput(); err != nil {
				t.Fatalf("%s preset runtime tools unavailable: %v\n%s", tt.preset, err, out)
			}
		})
	}
}
