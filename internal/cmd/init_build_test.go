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
			cmd := exec.Command(runtime, "build", "-t", tag, "-f", filepath.Join(dir, ".sandman", "Dockerfile"), dir)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("podman build: %v\n%s", err, out)
			}
			t.Cleanup(func() {
				_ = exec.Command(runtime, "rmi", "-f", tag).Run()
			})
		})
	}
}
