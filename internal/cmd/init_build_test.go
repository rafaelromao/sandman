//go:build smoke

package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/sandbox"
	"github.com/rafaelromao/sandman/internal/scaffold"
)

type buildPrompter struct{}

func (buildPrompter) Confirm(string) (bool, error)            { return true, nil }
func (buildPrompter) Select(string, []string) (string, error) { return "", nil }

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

			tag := fmt.Sprintf("sandman-python-preset-%s-%d:latest", agent, time.Now().UnixNano())
			buildPresetImage(t, runtime, tag, filepath.Join(dir, ".sandman", "Dockerfile"), dir)
			t.Cleanup(func() {
				_ = exec.Command(runtime, "rmi", "-f", tag).Run()
			})
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

			tag := fmt.Sprintf("sandman-dotnet-preset-%s-%d:latest", agent, time.Now().UnixNano())
			buildPresetImage(t, runtime, tag, filepath.Join(dir, ".sandman", "Dockerfile"), dir)
			t.Cleanup(func() {
				_ = exec.Command(runtime, "rmi", "-f", tag).Run()
			})
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

			tag := fmt.Sprintf("sandman-go-preset-%s-%d:latest", agent, time.Now().UnixNano())
			buildPresetImage(t, runtime, tag, filepath.Join(dir, ".sandman", "Dockerfile"), dir)
			t.Cleanup(func() {
				_ = exec.Command(runtime, "rmi", "-f", tag).Run()
			})
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

			tag := fmt.Sprintf("sandman-node-preset-%s-%d:latest", agent, time.Now().UnixNano())
			buildPresetImage(t, runtime, tag, filepath.Join(dir, ".sandman", "Dockerfile"), dir)
			t.Cleanup(func() {
				_ = exec.Command(runtime, "rmi", "-f", tag).Run()
			})
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

			tag := fmt.Sprintf("sandman-generic-preset-%s-%d:latest", agent, time.Now().UnixNano())
			buildPresetImage(t, runtime, tag, filepath.Join(dir, ".sandman", "Dockerfile"), dir)
			t.Cleanup(func() {
				_ = exec.Command(runtime, "rmi", "-f", tag).Run()
			})
		})
	}
}

func buildPresetImage(t *testing.T, runtime, tag, dockerfile, contextDir string) {
	t.Helper()

	var lastErr error
	var lastOut []byte
	for attempt := 1; attempt <= 3; attempt++ {
		cmd := exec.Command(runtime, "build", "-t", tag, "-f", dockerfile, contextDir)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return
		}
		lastErr = err
		lastOut = out
		t.Logf("container build failed (attempt %d/3): %v", attempt, err)
		time.Sleep(time.Duration(attempt) * time.Second)
	}

	t.Fatalf("%s build failed after retries: %v\n%s", runtime, lastErr, lastOut)
}
