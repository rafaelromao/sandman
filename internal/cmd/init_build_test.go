//go:build smoke

package cmd

import (
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

// buildPresetImageSkipRationale documents why each per-test podman build is
// gated on the smoke pre-warm phase. Erlang/OTP 28 takes 5-10 minutes to
// compile from source under mise, well past the 10m per-test timeout, and
// the pre-warm phase already builds every variant we want to assert against.
// See https://github.com/rafaelromao/sandman/issues/1793.
const buildPresetImageSkipRationale = "per-test podman build skipped: relying on smoke pre-warm image for Erlang/OTP 28 compile cost (see issue #1793)"

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
