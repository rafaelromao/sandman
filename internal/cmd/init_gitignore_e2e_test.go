package cmd

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/skill"
)

// requireGitForInitRegression skips when git is not on PATH so the
// regression test for issue #2148 remains runnable on minimal build hosts.
func requireGitForInitRegression(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not on PATH; skipping issue #2148 regression test")
	}
}

// TestInit_GitignoreAndUntrackRegression is the end-to-end regression test
// required by issue #2148: starting from a repo where .sandman/task.md is
// already tracked, sandman init must remove the file from the index (without
// deleting the on-disk copy), AND `git add -f .sandman/task.md && git commit`
// must be rejected — even though -f bypasses the .gitignore rule, the
// installed pre-commit hook must refuse the commit because the staged path
// is under .sandman/.
func TestInit_GitignoreAndUntrackRegression(t *testing.T) {
	requireGitForInitRegression(t)

	oldSync := syncSandmanSkill
	syncSandmanSkill = func(skill.SyncOptions) error { return nil }
	t.Cleanup(func() { syncSandmanSkill = oldSync })

	dir := t.TempDir()
	t.Chdir(dir)

	initRunIntegrationRepo(t, dir)

	sandmanDir := filepath.Join(dir, ".sandman")
	if err := os.MkdirAll(sandmanDir, 0755); err != nil {
		t.Fatalf("mkdir .sandman: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sandmanDir, "task.md"), []byte("# pre-existing task\n"), 0644); err != nil {
		t.Fatalf("write task.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sandmanDir, "config.yaml"), []byte("placeholder: true\n"), 0644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	runGit(t, dir, "add", ".sandman")
	runGit(t, dir, "commit", "-m", "track sandboxed sandman")

	if got := strings.TrimSpace(runGit(t, dir, "ls-files", ".sandman")); got == "" {
		t.Fatalf("pre-condition: .sandman must be tracked, but ls-files is empty")
	}

	var out bytes.Buffer
	cmd := NewInitCmd()
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader("y\n"))
	cmd.SetArgs([]string{"--build-tools", "generic"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("init command failed: %v\noutput: %s", err, out.String())
	}

	if got := strings.TrimSpace(runGit(t, dir, "ls-files", ".sandman")); got != "" {
		t.Fatalf("after init, git ls-files .sandman/ must be empty, got:\n%s", got)
	}
	t.Logf("ls-files after init: %q", runGit(t, dir, "ls-files", ".sandman/"))
	t.Logf("status after init:")
	t.Logf("%s", runGit(t, dir, "status"))

	if _, err := os.Stat(filepath.Join(sandmanDir, "task.md")); err != nil {
		t.Fatalf("on-disk .sandman/task.md must be preserved, got: %v", err)
	}

	gi, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.Contains(string(gi), ".sandman/") {
		t.Fatalf(".gitignore must contain .sandman/ rule, got:\n%s", gi)
	}

	hookPath := filepath.Join(dir, ".git", "hooks", "pre-commit")
	hookContent, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("pre-commit hook must be installed at %s: %v", hookPath, err)
	}
	t.Logf("hook contents:\n%s", hookContent)
	hookInfo, err := os.Stat(hookPath)
	if err != nil {
		t.Fatalf("stat hook: %v", err)
	}
	t.Logf("hook perms: %v", hookInfo.Mode())

	runGit(t, dir, "add", "--force", ".sandman/task.md")

	commitCmd := exec.Command("git", "commit", "-m", "force-add sandman task")
	commitCmd.Dir = dir
	commitOut, commitErr := commitCmd.CombinedOutput()
	if commitErr == nil {
		t.Fatalf("git commit must be rejected by pre-commit hook, got success and output:\n%s", commitOut)
	}

	if !strings.Contains(string(commitOut), ".sandman/task.md") {
		t.Fatalf("hook output must mention the blocked .sandman/ path, got:\n%s", commitOut)
	}

	if strings.TrimSpace(runGit(t, dir, "log", "-1", "--pretty=%s")) == "force-add sandman task" {
		t.Fatalf("blocked commit must not appear in git log")
	}
}
