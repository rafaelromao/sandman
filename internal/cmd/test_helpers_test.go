package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/testenv"
)

func shortTempDir(t *testing.T) string {
	t.Helper()
	return testenv.MkdirShort(t, "sm-")
}

func newRunDepsAuto(t *testing.T, runner batch.Runner) Dependencies {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".sandman"), 0o755); err != nil {
		t.Fatalf("mkdir .sandman: %v", err)
	}
	initCmd := exec.Command("git", "init", "-q", dir)
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	t.Chdir(dir)
	return Dependencies{
		BatchRunner:  runner,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: &fakeGitHubClient{},
		RepoRoot:     ".",
	}
}
