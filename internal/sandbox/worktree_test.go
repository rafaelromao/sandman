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

func TestWorktreeSandbox_WritePrompt(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	s := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	if err := s.Start(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := "hello prompt"
	if err := s.WritePrompt(content); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	promptPath := filepath.Join(s.WorkDir(), ".sandman", "prompt.md")
	data, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("read prompt: %v", err)
	}
	if string(data) != content {
		t.Errorf("expected %q, got %q", content, string(data))
	}
}

func TestWorktreeSandbox_ReadRunResult(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	s := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	if err := s.Start(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	runResultPath := filepath.Join(s.WorkDir(), ".sandman", "run-result.json")
	if err := os.MkdirAll(filepath.Dir(runResultPath), 0755); err != nil {
		t.Fatalf("create dir: %v", err)
	}
	if err := os.WriteFile(runResultPath, []byte(`{"title":"Custom Title","body":"Custom Body"}`), 0644); err != nil {
		t.Fatalf("write run result: %v", err)
	}

	result, err := s.ReadRunResult()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected result, got nil")
	}
	if result.Title != "Custom Title" {
		t.Errorf("expected title 'Custom Title', got %q", result.Title)
	}
	if result.Body != "Custom Body" {
		t.Errorf("expected body 'Custom Body', got %q", result.Body)
	}
}
