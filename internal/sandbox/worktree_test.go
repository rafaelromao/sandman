package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	cmds := [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "checkout", "-b", "main"},
		{"git", "commit", "--allow-empty", "-m", "init"},
	}
	for _, cmd := range cmds {
		c := exec.Command(cmd[0], cmd[1:]...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
}

func TestWorktreeSandbox_StartCreatesWorktree(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	s := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	if err := s.Start(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	worktreePath := s.WorkDir()
	if worktreePath == "" {
		t.Fatal("expected workdir to be set")
	}

	if _, err := os.Stat(worktreePath); err != nil {
		t.Errorf("worktree directory does not exist: %v", err)
	}
}

func TestWorktreeSandbox_StartFailsOutsideGitRepo(t *testing.T) {
	dir := t.TempDir()

	s := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	if err := s.Start(); err == nil {
		t.Fatal("expected error when not inside a git repo")
	}
}

func TestWorktreeSandbox_WorkDirBeforeStart(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	s := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	if s.WorkDir() != "" {
		t.Error("expected workdir to be empty before Start")
	}
}
