//go:build e2e

package cmd

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/sandbox"
	"github.com/rafaelromao/sandman/internal/scaffold"
)

// Exercise the generated Python RUN instruction with a local mise stand-in:
// authentication must reach the installer but never the next image layer.
func TestPythonBuildSecretIsTransient(t *testing.T) {
	if !podmanAvailable(t) {
		return
	}
	for _, authenticated := range []bool{false, true} {
		t.Run(fmt.Sprintf("authenticated=%t", authenticated), func(t *testing.T) {
			t.Setenv("GH_TOKEN", "")
			t.Setenv("GITHUB_TOKEN", "")
			t.Setenv("MISE_GITHUB_TOKEN", "")
			token := ""
			if authenticated {
				token = "ephemeral-build-fixture"
				t.Setenv("MISE_GITHUB_TOKEN", token)
			}
			repo := t.TempDir()
			s := &scaffold.Scaffolder{}
			if err := s.Scaffold(repo, scaffold.Options{BuildTools: "python", ToolVersion: "3.12.9"}, smokePrompter{}); err != nil {
				t.Fatal(err)
			}
			dockerfile := filepath.Join(repo, ".sandman", "Dockerfile")
			data, err := os.ReadFile(dockerfile)
			if err != nil {
				t.Fatal(err)
			}
			var install string
			for _, line := range strings.Split(string(data), "\n") {
				if strings.Contains(line, "mise use -g --pin python@") {
					install = line
				}
			}
			if install == "" {
				t.Fatal("generated Python install instruction missing")
			}
			// The stand-in stores only a digest of the expected fixture token.
			mise := fmt.Sprintf("#!/bin/sh\nset -eu\ntest \"$*\" = 'use -g --pin python@3.12.9'\ntest \"${MISE_GITHUB_TOKEN:-}\" = \"${GITHUB_TOKEN:-}\"\ntest \"$(printf '%%s' \"${GITHUB_TOKEN:-}\" | sha256sum | cut -d ' ' -f 1)\" = '%x'\n", sha256.Sum256([]byte(token)))
			if err := os.WriteFile(filepath.Join(repo, "mise"), []byte(mise), 0755); err != nil {
				t.Fatal(err)
			}
			checkClean := `test ! -e /run/secrets/github_token && test -z "${GITHUB_TOKEN:-}" && test -z "${MISE_GITHUB_TOKEN:-}"`
			fixture := "FROM alpine\nCOPY mise /usr/local/bin/mise\n" + install + "\nRUN " + checkClean + "\n"
			if err := os.WriteFile(dockerfile, []byte(fixture), 0644); err != nil {
				t.Fatal(err)
			}
			runtime := sandbox.NewContainerRuntime("podman")
			tag, err := runtime.BuildImage(t.Context(), repo)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = exec.Command("podman", "rmi", "-f", tag).Run() })
			if out, err := exec.Command("podman", "run", "--rm", tag, "sh", "-c", checkClean).CombinedOutput(); err != nil {
				t.Fatalf("build secret persisted at runtime: %v\n%s", err, out)
			}
			history, err := exec.Command("podman", "history", "--no-trunc", tag).Output()
			if err != nil {
				t.Fatal(err)
			}
			if token != "" && strings.Contains(string(history), token) {
				t.Fatal("build secret persisted in image history")
			}
		})
	}
}
