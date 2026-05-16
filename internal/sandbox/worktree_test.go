package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func initGitRepoWithRemote(t *testing.T, dir string) string {
	t.Helper()
	initGitRepo(t, dir)
	remoteDir := t.TempDir()
	runGit(t, remoteDir, "init", "--bare")
	runGit(t, dir, "remote", "add", "origin", remoteDir)
	runGit(t, dir, "push", "-u", "origin", "main")
	return remoteDir
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func writeGitFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func commitGitFile(t *testing.T, dir, name, content, message string) {
	t.Helper()
	writeGitFile(t, dir, name, content)
	runGit(t, dir, "add", name)
	runGit(t, dir, "commit", "-m", message)
}

func TestWorktreeSandbox_CreatesBranchFromSourceBranchWithTrackedFilesOnly(t *testing.T) {
	seedDir := t.TempDir()
	remoteDir := initGitRepoWithRemote(t, seedDir)
	commitGitFile(t, seedDir, "tracked.txt", "initial\n", "first tracked file")
	commitGitFile(t, seedDir, "another.txt", "data\n", "second tracked file")
	runGit(t, seedDir, "push", "origin", "main")

	localDir := t.TempDir()
	runGit(t, localDir, "clone", "-b", "main", remoteDir, ".")
	runGit(t, localDir, "config", "user.email", "test@test.com")
	runGit(t, localDir, "config", "user.name", "Test")
	writeGitFile(t, localDir, "untracked.txt", "should not appear\n")

	if err := SyncDefaultBranch(localDir, "main"); err != nil {
		t.Fatalf("sync error: %v", err)
	}

	s := NewWorktreeSandbox(localDir, filepath.Join(localDir, ".sandman", "worktrees"), "sandman/99-test", "main")
	if err := s.Start(); err != nil {
		t.Fatalf("start error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(s.WorkDir(), "tracked.txt")); err != nil {
		t.Errorf("expected tracked.txt in worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.WorkDir(), "another.txt")); err != nil {
		t.Errorf("expected another.txt in worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.WorkDir(), "untracked.txt")); !os.IsNotExist(err) {
		t.Errorf("expected untracked.txt to NOT be in worktree, got %v", err)
	}

	branchesOut := runGit(t, localDir, "branch", "--list", "sandman/99-test")
	if branchesOut == "" {
		t.Error("expected branch sandman/99-test to exist")
	}
}

func TestWorktreeSandbox_StartCreatesWorktree(t *testing.T) {
	dir := t.TempDir()
	_ = initGitRepoWithRemote(t, dir)

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

func TestSyncDefaultBranchFastForwardsDefaultBranchBeforeAddingWorktree(t *testing.T) {
	seedDir := t.TempDir()
	remoteDir := initGitRepoWithRemote(t, seedDir)
	commitGitFile(t, seedDir, "tracked.txt", "base\n", "base")
	runGit(t, seedDir, "push", "origin", "main")

	localDir := t.TempDir()
	runGit(t, localDir, "clone", "-b", "main", remoteDir, ".")
	runGit(t, localDir, "config", "user.email", "test@test.com")
	runGit(t, localDir, "config", "user.name", "Test")
	runGit(t, localDir, "checkout", "-b", "feature")
	commitGitFile(t, localDir, "feature-only.txt", "feature\n", "feature")
	writeGitFile(t, localDir, "untracked.txt", "keep me out of the worktree\n")

	commitGitFile(t, seedDir, "tracked.txt", "remote\n", "remote update")
	runGit(t, seedDir, "push", "origin", "main")

	if err := SyncDefaultBranch(localDir, "main"); err != nil {
		t.Fatalf("unexpected sync error: %v", err)
	}

	localMain := runGit(t, localDir, "rev-parse", "main")
	remoteMain := runGit(t, remoteDir, "rev-parse", "main")
	if localMain != remoteMain {
		t.Fatalf("expected local main to sync to remote main, got %q and %q", localMain, remoteMain)
	}

	s := NewWorktreeSandbox(localDir, filepath.Join(localDir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	if err := s.Start(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	trackedPath := filepath.Join(s.WorkDir(), "tracked.txt")
	data, err := os.ReadFile(trackedPath)
	if err != nil {
		t.Fatalf("read synced tracked file: %v", err)
	}
	if string(data) != "remote\n" {
		t.Fatalf("expected worktree to be created from synced default branch, got %q", string(data))
	}

	if _, err := os.Stat(filepath.Join(s.WorkDir(), "untracked.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected untracked file to stay out of the worktree, got %v", err)
	}
}

func TestSyncDefaultBranchFailsWhenDefaultBranchCannotFastForward(t *testing.T) {
	seedDir := t.TempDir()
	remoteDir := initGitRepoWithRemote(t, seedDir)
	commitGitFile(t, seedDir, "tracked.txt", "base\n", "base")
	runGit(t, seedDir, "push", "origin", "main")

	localDir := t.TempDir()
	runGit(t, localDir, "clone", "-b", "main", remoteDir, ".")
	runGit(t, localDir, "config", "user.email", "test@test.com")
	runGit(t, localDir, "config", "user.name", "Test")
	commitGitFile(t, localDir, "local-only.txt", "local\n", "local divergence")
	runGit(t, localDir, "checkout", "-b", "feature")

	commitGitFile(t, seedDir, "remote-only.txt", "remote\n", "remote divergence")
	runGit(t, seedDir, "push", "origin", "main")

	if err := SyncDefaultBranch(localDir, "main"); err == nil {
		t.Fatal("expected sync failure when default branch cannot fast-forward")
	} else if !strings.Contains(err.Error(), "sync default branch") {
		t.Fatalf("expected sync error, got %v", err)
	}

	s := NewWorktreeSandbox(localDir, filepath.Join(localDir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")

	if _, err := os.Stat(s.WorkDir()); !os.IsNotExist(err) {
		t.Fatalf("expected no worktree to be created, got %v", err)
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
	_ = initGitRepoWithRemote(t, dir)

	s := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	if err := s.Start(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := "hello prompt"
	if err := s.WritePrompt(content); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	promptPath := filepath.Join(s.WorkDir(), ".sandman", "rendered-prompt.md")
	data, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("read prompt: %v", err)
	}
	if string(data) != content {
		t.Errorf("expected %q, got %q", content, string(data))
	}
}

func TestWorktreeSandbox_StopRemovesWorktree(t *testing.T) {
	dir := t.TempDir()
	_ = initGitRepoWithRemote(t, dir)

	s := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	if err := s.Start(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	worktreePath := s.WorkDir()
	if err := s.Stop(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Errorf("expected worktree to be removed, but it still exists")
	}
}

func TestWorktreeSandbox_StartReusesExistingWorktree(t *testing.T) {
	dir := t.TempDir()
	_ = initGitRepoWithRemote(t, dir)

	s := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	if err := s.Start(); err != nil {
		t.Fatalf("unexpected error on first start: %v", err)
	}

	worktreePath := s.WorkDir()
	if err := os.WriteFile(filepath.Join(worktreePath, "marker.txt"), []byte("preserved"), 0644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	s2 := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	if err := s2.Start(); err != nil {
		t.Fatalf("unexpected error on second start: %v", err)
	}

	if s2.WorkDir() != worktreePath {
		t.Errorf("expected same workdir, got %q", s2.WorkDir())
	}

	data, err := os.ReadFile(filepath.Join(worktreePath, "marker.txt"))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if string(data) != "preserved" {
		t.Errorf("expected preserved marker, got %q", string(data))
	}
}

func TestWorktreeSandbox_ExecInteractive_RunsCommand(t *testing.T) {
	dir := t.TempDir()
	_ = initGitRepoWithRemote(t, dir)

	s := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	if err := s.Start(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := s.ExecInteractive(context.Background(), "touch interactive-ran.txt"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	markerPath := filepath.Join(s.WorkDir(), "interactive-ran.txt")
	if _, err := os.Stat(markerPath); err != nil {
		t.Errorf("expected interactive marker file to exist: %v", err)
	}
}
