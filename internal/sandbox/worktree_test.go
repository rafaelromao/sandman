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

func removeBranch(t *testing.T, dir, branch string) {
	t.Helper()
	list := exec.Command("git", "branch", "--list", branch)
	list.Dir = dir
	out, err := list.CombinedOutput()
	if err != nil {
		t.Fatalf("git branch --list %s: %v: %s", branch, err, out)
	}
	if strings.TrimSpace(string(out)) == "" {
		return
	}
	deleteCmd := exec.Command("git", "branch", "-D", branch)
	deleteCmd.Dir = dir
	out, err = deleteCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git branch -D %s: %v: %s", branch, err, out)
	}
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

	if err := SyncBaseBranch(localDir, "main"); err != nil {
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
	removeBranch(t, dir, "sandman/42-fix-bug")

	s := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	if err := s.Start(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(func() {
		s.Stop()
		removeBranch(t, dir, "sandman/42-fix-bug")
	})

	worktreePath := s.WorkDir()
	if worktreePath == "" {
		t.Fatal("expected workdir to be set")
	}

	if _, err := os.Stat(worktreePath); err != nil {
		t.Errorf("worktree directory does not exist: %v", err)
	}
}

func TestSyncBaseBranchFastForwardsBaseBranchBeforeAddingWorktree(t *testing.T) {
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
	removeBranch(t, localDir, "sandman/42-fix-bug")

	commitGitFile(t, seedDir, "tracked.txt", "remote\n", "remote update")
	runGit(t, seedDir, "push", "origin", "main")

	if err := SyncBaseBranch(localDir, "main"); err != nil {
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
	t.Cleanup(func() {
		s.Stop()
		removeBranch(t, localDir, "sandman/42-fix-bug")
	})

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

func TestSyncBaseBranchFailsWhenDiverged(t *testing.T) {
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

	if err := SyncBaseBranch(localDir, "main"); err == nil {
		t.Fatal("expected sync failure when local and remote have diverged")
	} else if !strings.Contains(err.Error(), "diverged") {
		t.Fatalf("expected diverged error, got %v", err)
	}

	s := NewWorktreeSandbox(localDir, filepath.Join(localDir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")

	if _, err := os.Stat(s.WorkDir()); !os.IsNotExist(err) {
		t.Fatalf("expected no worktree to be created, got %v", err)
	}
}

func TestSyncBaseBranchAheadOfRemote(t *testing.T) {
	seedDir := t.TempDir()
	remoteDir := initGitRepoWithRemote(t, seedDir)
	commitGitFile(t, seedDir, "tracked.txt", "base\n", "base")
	runGit(t, seedDir, "push", "origin", "main")

	localDir := t.TempDir()
	runGit(t, localDir, "clone", "-b", "main", remoteDir, ".")
	runGit(t, localDir, "config", "user.email", "test@test.com")
	runGit(t, localDir, "config", "user.name", "Test")
	// Add a commit locally that exists only on local, not on remote.
	commitGitFile(t, localDir, "local-only.txt", "local\n", "local ahead")

	if err := SyncBaseBranch(localDir, "main"); err != nil {
		t.Fatalf("expected sync to succeed when local is ahead of remote, got: %v", err)
	}
}

func TestSyncBaseBranchAtSameCommit(t *testing.T) {
	seedDir := t.TempDir()
	remoteDir := initGitRepoWithRemote(t, seedDir)

	localDir := t.TempDir()
	runGit(t, localDir, "clone", "-b", "main", remoteDir, ".")
	runGit(t, localDir, "config", "user.email", "test@test.com")
	runGit(t, localDir, "config", "user.name", "Test")

	if err := SyncBaseBranch(localDir, "main"); err != nil {
		t.Fatalf("expected sync to succeed when local equals remote, got: %v", err)
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
	removeBranch(t, dir, "sandman/42-fix-bug")

	s := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	if err := s.Start(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(func() {
		s.Stop()
		removeBranch(t, dir, "sandman/42-fix-bug")
	})

	content := "hello prompt"
	if err := s.WritePrompt(content); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	promptPath := filepath.Join(s.WorkDir(), ".sandman", "task.md")
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
	removeBranch(t, dir, "sandman/42-fix-bug")

	s := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	if err := s.Start(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(func() {
		_ = s.Stop()
		removeBranch(t, dir, "sandman/42-fix-bug")
	})

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
	removeBranch(t, dir, "sandman/42-fix-bug")

	s := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	if err := s.Start(); err != nil {
		t.Fatalf("unexpected error on first start: %v", err)
	}
	t.Cleanup(func() {
		s.Stop()
		removeBranch(t, dir, "sandman/42-fix-bug")
	})

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

func TestWorktreeSandbox_OverrideRecreatesDirtyWorktreeFromScratch(t *testing.T) {
	dir := t.TempDir()
	_ = initGitRepoWithRemote(t, dir)
	removeBranch(t, dir, "sandman/42-fix-bug")

	s := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	if err := s.Start(); err != nil {
		t.Fatalf("unexpected error on first start: %v", err)
	}
	t.Cleanup(func() {
		s.Stop()
		removeBranch(t, dir, "sandman/42-fix-bug")
	})

	markerPath := filepath.Join(s.WorkDir(), "stale-marker.txt")
	if err := os.WriteFile(markerPath, []byte("stale\n"), 0644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	s2 := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	s2.SetOverride(true)
	if err := s2.Start(); err != nil {
		t.Fatalf("expected no error with --override, got: %v", err)
	}

	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("expected stale marker to be removed by --override, got: %v", err)
	}
	if status := runGit(t, s2.WorkDir(), "status", "--short"); strings.Contains(status, "stale-marker.txt") {
		t.Fatalf("expected stale marker to be absent from worktree status after --override, got:\n%s", status)
	}
}

func TestWorktreeSandbox_StartFailsWhenBranchAlreadyExists(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	removeBranch(t, dir, "sandman/42-fix-bug")
	runGit(t, dir, "checkout", "-b", "sandman/42-fix-bug")

	s := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	err := s.Start()
	if err == nil {
		t.Fatal("expected error when branch already exists")
	}
	want := `branch "sandman/42-fix-bug" already exists`
	if !strings.Contains(err.Error(), want) {
		t.Errorf("expected error to contain %q, got %q", want, err.Error())
	}
	if !strings.Contains(err.Error(), `git branch -D sandman/42-fix-bug`) {
		t.Errorf("expected error to contain actionable delete command, got %q", err.Error())
	}
	runGit(t, dir, "checkout", "main")
	t.Cleanup(func() { removeBranch(t, dir, "sandman/42-fix-bug") })
}

func TestBranchExists(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	removeBranch(t, dir, "sandman/42-fix-bug")

	if BranchExists(dir, "sandman/42-fix-bug") {
		t.Fatal("expected missing branch to return false")
	}

	runGit(t, dir, "checkout", "-b", "sandman/42-fix-bug")
	if !BranchExists(dir, "sandman/42-fix-bug") {
		t.Fatal("expected existing branch to return true")
	}
}

func TestWorktreeSandbox_ExecInteractive_RunsCommand(t *testing.T) {
	dir := t.TempDir()
	_ = initGitRepoWithRemote(t, dir)
	removeBranch(t, dir, "sandman/42-fix-bug")

	s := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	if err := s.Start(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(func() {
		s.Stop()
		removeBranch(t, dir, "sandman/42-fix-bug")
	})

	if err := s.ExecInteractive(context.Background(), "touch interactive-ran.txt"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	markerPath := filepath.Join(s.WorkDir(), "interactive-ran.txt")
	if _, err := os.Stat(markerPath); err != nil {
		t.Errorf("expected interactive marker file to exist: %v", err)
	}
}

func TestWorktreeSandbox_Start_RecreatesOrphanWorktreeDirectory(t *testing.T) {
	// Simulate a previous run that crashed inside `git worktree add` after the
	// directory was created but before git registered it. The directory exists
	// on disk with throwaway content; git has no record of it. Start() must
	// detect this and re-create a real worktree. See #545.
	dir := t.TempDir()
	initGitRepoWithRemote(t, dir)
	commitGitFile(t, dir, "tracked.txt", "base\n", "base")
	runGit(t, dir, "push", "origin", "main")

	worktreeBase := filepath.Join(dir, ".sandman", "worktrees")
	branch := "sandman/42-fix-bug"
	orphanPath := filepath.Join(worktreeBase, branch)
	if err := os.MkdirAll(orphanPath, 0755); err != nil {
		t.Fatalf("create orphan dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(orphanPath, "stale.txt"), []byte("left over from a previous run\n"), 0644); err != nil {
		t.Fatalf("write stale file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(orphanPath, "README.md"), []byte("# orphan\n"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}

	listCmd := exec.Command("git", "worktree", "list")
	listCmd.Dir = dir
	if out, err := listCmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree list: %v: %s", err, out)
	} else if strings.Contains(string(out), orphanPath) {
		t.Fatalf("git should not know about the orphan dir, got:\n%s", out)
	}

	s := NewWorktreeSandbox(dir, worktreeBase, branch, "main")
	s.SetOverride(true)
	if err := s.Start(); err != nil {
		t.Fatalf("Start() failed on orphan dir: %v", err)
	}
	t.Cleanup(func() {
		s.Stop()
		removeBranch(t, dir, branch)
	})

	if _, err := os.Stat(filepath.Join(s.WorkDir(), "stale.txt")); !os.IsNotExist(err) {
		t.Errorf("expected stale orphan content to be gone, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(s.WorkDir(), "tracked.txt")); err != nil {
		t.Errorf("expected tracked.txt from source branch, got err=%v", err)
	}
	revParse := exec.Command("git", "rev-parse", "--git-dir")
	revParse.Dir = s.WorkDir()
	if out, err := revParse.CombinedOutput(); err != nil {
		t.Fatalf("worktree dir is not a real git worktree: %v: %s", err, out)
	}
}

func TestWorktreeSandbox_StartErrorsOnWrongBranch(t *testing.T) {
	dir := t.TempDir()
	_ = initGitRepoWithRemote(t, dir)
	removeBranch(t, dir, "sandman/42-fix-bug")

	s := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	if err := s.Start(); err != nil {
		t.Fatalf("unexpected error on first start: %v", err)
	}
	t.Cleanup(func() {
		s.Stop()
		removeBranch(t, dir, "sandman/42-fix-bug")
	})

	runGit(t, s.WorkDir(), "checkout", "-b", "wrong-branch")

	s2 := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	err := s2.Start()
	if err == nil {
		t.Fatal("expected error when worktree is on wrong branch")
	}
	if !strings.Contains(err.Error(), "wrong-branch") {
		t.Errorf("expected error to contain actual branch name 'wrong-branch', got: %v", err)
	}
	if !strings.Contains(err.Error(), "sandman/42-fix-bug") {
		t.Errorf("expected error to contain expected branch name 'sandman/42-fix-bug', got: %v", err)
	}
	if !strings.Contains(err.Error(), s.WorkDir()) {
		t.Errorf("expected error to contain worktree path %q, got: %v", s.WorkDir(), err)
	}
	if !strings.Contains(err.Error(), "--override") {
		t.Errorf("expected error to contain '--override' hint, got: %v", err)
	}

	headRef := runGit(t, s.WorkDir(), "symbolic-ref", "HEAD")
	if !strings.Contains(headRef, "wrong-branch") {
		t.Errorf("expected worktree HEAD to remain on wrong-branch, got %q", headRef)
	}
}

func TestWorktreeSandbox_OverrideFalseStillErrorsOnWrongBranch(t *testing.T) {
	dir := t.TempDir()
	_ = initGitRepoWithRemote(t, dir)
	removeBranch(t, dir, "sandman/42-fix-bug")

	s := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	if err := s.Start(); err != nil {
		t.Fatalf("unexpected error on first start: %v", err)
	}
	t.Cleanup(func() {
		s.Stop()
		removeBranch(t, dir, "sandman/42-fix-bug")
	})

	runGit(t, s.WorkDir(), "checkout", "-b", "wrong-branch")

	s2 := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	s2.SetOverride(false)
	err := s2.Start()
	if err == nil {
		t.Fatal("expected error when override is false")
	}
	if !strings.Contains(err.Error(), "wrong-branch") {
		t.Errorf("expected error to contain wrong branch name, got: %v", err)
	}
}

func TestWorktreeSandbox_StartErrorsOnDetachedHead(t *testing.T) {
	dir := t.TempDir()
	_ = initGitRepoWithRemote(t, dir)
	removeBranch(t, dir, "sandman/42-fix-bug")

	s := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	if err := s.Start(); err != nil {
		t.Fatalf("unexpected error on first start: %v", err)
	}
	t.Cleanup(func() {
		s.Stop()
		removeBranch(t, dir, "sandman/42-fix-bug")
	})

	runGit(t, s.WorkDir(), "checkout", "--detach", "HEAD")

	s2 := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	err := s2.Start()
	if err == nil {
		t.Fatal("expected error when worktree HEAD is detached")
	}
	if !strings.Contains(err.Error(), "not on a branch") {
		t.Errorf("expected error to mention detached HEAD, got: %v", err)
	}
	if !strings.Contains(err.Error(), s.WorkDir()) {
		t.Errorf("expected error to contain worktree path %q, got: %v", s.WorkDir(), err)
	}
	if !strings.Contains(err.Error(), "--override") {
		t.Errorf("expected error to contain '--override' hint, got: %v", err)
	}

	headRef := runGit(t, s.WorkDir(), "rev-parse", "--short", "HEAD")
	if headRef == "" {
		t.Error("expected HEAD to still point to a commit after error")
	}
}

func TestWorktreeSandbox_StartSelfHealsBrokenWorktreeWithStaleDotGitFile(t *testing.T) {
	// A stale .git gitlink (pointing to non-existent gitdir) is detected
	// and the worktree is recreated from scratch when override is set.
	dir := t.TempDir()
	initGitRepoWithRemote(t, dir)
	commitGitFile(t, dir, "tracked.txt", "base\n", "base")
	runGit(t, dir, "push", "origin", "main")

	worktreeBase := filepath.Join(dir, ".sandman", "worktrees")
	branch := "sandman/42-fix-bug"

	// Create a real worktree first so the branch exists in the parent repo.
	s1 := NewWorktreeSandbox(dir, worktreeBase, branch, "main")
	if err := s1.Start(); err != nil {
		t.Fatalf("create initial worktree: %v", err)
	}

	// Corrupt the .git gitlink to simulate a stale gitdir (e.g. container destroyed).
	gitPath := filepath.Join(s1.WorkDir(), ".git")
	if err := os.WriteFile(gitPath, []byte("gitdir: /tmp/nonexistent-worktree-gitdir\n"), 0644); err != nil {
		t.Fatalf("corrupt .git file: %v", err)
	}

	// Now try Start() with override, simulating the review daemon's behavior.
	s2 := NewWorktreeSandbox(dir, worktreeBase, branch, "main")
	s2.SetOverride(true)
	err := s2.Start()
	if err != nil {
		t.Fatalf("Start() should self-heal stale gitlink, got: %v", err)
	}
	t.Cleanup(func() {
		s2.Stop()
		removeBranch(t, dir, branch)
	})

	// Verify the worktree was recreated properly.
	if _, err := os.Stat(filepath.Join(s2.WorkDir(), "tracked.txt")); err != nil {
		t.Errorf("expected tracked.txt from source branch, got err=%v", err)
	}
	newGitlink, err := os.ReadFile(filepath.Join(s2.WorkDir(), ".git"))
	if err != nil {
		t.Fatalf("read new gitlink: %v", err)
	}
	if strings.Contains(string(newGitlink), "nonexistent-worktree") {
		t.Error("expected new gitlink to point to valid gitdir, got corrupted gitlink")
	}
	revParse := exec.Command("git", "rev-parse", "--git-dir")
	revParse.Dir = s2.WorkDir()
	if out, err := revParse.CombinedOutput(); err != nil {
		t.Fatalf("worktree dir is not a real git worktree: %v: %s", err, out)
	}
}

func TestWorktreeSandbox_OverrideReconcileWrongBranch(t *testing.T) {
	dir := t.TempDir()
	_ = initGitRepoWithRemote(t, dir)
	removeBranch(t, dir, "sandman/42-fix-bug")

	s := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	if err := s.Start(); err != nil {
		t.Fatalf("unexpected error on first start: %v", err)
	}
	t.Cleanup(func() {
		s.Stop()
		removeBranch(t, dir, "sandman/42-fix-bug")
	})

	runGit(t, s.WorkDir(), "checkout", "-b", "wrong-branch")

	s2 := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	s2.SetOverride(true)
	if err := s2.Start(); err != nil {
		t.Fatalf("expected no error with --override, got: %v", err)
	}

	headRef := runGit(t, s2.WorkDir(), "symbolic-ref", "HEAD")
	if !strings.Contains(headRef, "sandman/42-fix-bug") {
		t.Errorf("expected HEAD to be on sandman/42-fix-bug after force-checkout, got %q", headRef)
	}
}

func TestWorktreeSandbox_OverrideReconcileMissingBranch(t *testing.T) {
	dir := t.TempDir()
	_ = initGitRepoWithRemote(t, dir)
	removeBranch(t, dir, "sandman/42-fix-bug")

	s := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	if err := s.Start(); err != nil {
		t.Fatalf("unexpected error on first start: %v", err)
	}
	t.Cleanup(func() {
		s.Stop()
	})

	runGit(t, s.WorkDir(), "checkout", "-b", "wrong-branch")

	runGit(t, dir, "branch", "-D", "sandman/42-fix-bug")

	s2 := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	s2.SetOverride(true)
	s2.SetStrandedReconcile(false)
	if err := s2.Start(); err != nil {
		t.Fatalf("expected no error with --override, got: %v", err)
	}

	headRef := runGit(t, s2.WorkDir(), "symbolic-ref", "HEAD")
	if !strings.Contains(headRef, "sandman/42-fix-bug") {
		t.Errorf("expected HEAD to be on sandman/42-fix-bug after recreation, got %q", headRef)
	}
}

func TestWorktreeSandbox_OverrideReconcileDetachedHead(t *testing.T) {
	dir := t.TempDir()
	_ = initGitRepoWithRemote(t, dir)
	removeBranch(t, dir, "sandman/42-fix-bug")

	s := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	if err := s.Start(); err != nil {
		t.Fatalf("unexpected error on first start: %v", err)
	}
	t.Cleanup(func() {
		s.Stop()
		removeBranch(t, dir, "sandman/42-fix-bug")
	})

	runGit(t, s.WorkDir(), "checkout", "--detach", "HEAD")

	s2 := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	s2.SetOverride(true)
	if err := s2.Start(); err != nil {
		t.Fatalf("expected no error with --override on detached HEAD, got: %v", err)
	}

	headRef := runGit(t, s2.WorkDir(), "symbolic-ref", "HEAD")
	if !strings.Contains(headRef, "sandman/42-fix-bug") {
		t.Errorf("expected HEAD to be on sandman/42-fix-bug after force-checkout, got %q", headRef)
	}
}

func TestWorktreeSandbox_StartPreservesErrorWhenReconcileDisabled(t *testing.T) {
	// The main repo is checked out on the very branch we need to delete.
	// `git branch -D` from the main-repo cwd refuses with the
	// "Cannot delete branch ... checked out at ..." error. With
	// --no-reconcile-stranded, that error must surface unchanged.
	dir := t.TempDir()
	initGitRepo(t, dir)
	const branch = "sandman/42-fix-bug"
	runGit(t, dir, "checkout", "-b", branch)

	s := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), branch, "main")
	s.SetOverride(true)
	s.SetStrandedReconcile(false)

	err := s.Start()
	if err == nil {
		t.Fatal("expected error when main repo is checked out on branch and reconcile is disabled")
	}
	if !strings.Contains(err.Error(), "delete stale branch") {
		t.Errorf("expected error to mention 'delete stale branch', got %q", err.Error())
	}
	if !strings.Contains(err.Error(), branch) {
		t.Errorf("expected error to mention branch %q, got %q", branch, err.Error())
	}

	t.Cleanup(func() {
		runGit(t, dir, "checkout", "main")
		removeBranch(t, dir, branch)
	})
}

func TestWorktreeSandbox_RecoversFromMainRepoBranch_StrandedWorktreePath(t *testing.T) {
	// The main repo is checked out on the branch we need to delete. A
	// stranded worktree lives at <worktreeBase>/<branch> on a different
	// branch. Start() should detect the stranded worktree, run
	// `git -C <strandedPath> branch -D <branch>` from inside the worktree,
	// and then create the worktree as normal.
	dir := t.TempDir()
	_ = initGitRepoWithRemote(t, dir)
	commitGitFile(t, dir, "tracked.txt", "base\n", "base")
	runGit(t, dir, "push", "origin", "main")

	worktreeBase := filepath.Join(dir, ".sandman", "worktrees")
	if err := os.MkdirAll(worktreeBase, 0755); err != nil {
		t.Fatalf("mkdir worktreeBase: %v", err)
	}
	branch := "sandman/42-fix-bug"
	const otherBranch = "sandman/9-other"

	runGit(t, dir, "branch", branch)
	runGit(t, dir, "branch", otherBranch)
	strandedPath := filepath.Join(worktreeBase, branch)
	runGit(t, dir, "worktree", "add", "--force", strandedPath, otherBranch)

	runGit(t, dir, "checkout", branch)

	s := NewWorktreeSandbox(dir, worktreeBase, branch, "main")
	s.SetOverride(true)
	s.SetStrandedReconcile(true)

	if err := s.Start(); err != nil {
		t.Fatalf("expected stranded-worktree recovery to succeed, got: %v", err)
	}
	t.Cleanup(func() {
		s.Stop()
		removeBranch(t, dir, branch)
		removeBranch(t, dir, otherBranch)
	})

	if _, err := os.Stat(filepath.Join(s.WorkDir(), "tracked.txt")); err != nil {
		t.Errorf("expected tracked.txt in worktree after recovery, got: %v", err)
	}
	headRef := runGit(t, s.WorkDir(), "symbolic-ref", "HEAD")
	if !strings.Contains(headRef, branch) {
		t.Errorf("expected worktree HEAD to be on %q, got %q", branch, headRef)
	}
	mainHeadRef := runGit(t, dir, "symbolic-ref", "HEAD")
	if !strings.Contains(mainHeadRef, "main") {
		t.Errorf("expected main repo to be back on main after recovery, got %q", mainHeadRef)
	}
}

func TestWorktreeSandbox_RecoversFromMainRepoBranch_MainRepoCheckoutPath(t *testing.T) {
	// The main repo is checked out on the branch we need to delete, and
	// there is NO stranded worktree at <worktreeBase>/<branch>. Start()
	// should force-checkout the source branch in the main repo and
	// retry the delete, then create the worktree as normal.
	dir := t.TempDir()
	_ = initGitRepoWithRemote(t, dir)
	commitGitFile(t, dir, "tracked.txt", "base\n", "base")
	runGit(t, dir, "push", "origin", "main")

	worktreeBase := filepath.Join(dir, ".sandman", "worktrees")
	if err := os.MkdirAll(worktreeBase, 0755); err != nil {
		t.Fatalf("mkdir worktreeBase: %v", err)
	}
	branch := "sandman/42-fix-bug"

	runGit(t, dir, "branch", branch)
	runGit(t, dir, "checkout", branch)

	s := NewWorktreeSandbox(dir, worktreeBase, branch, "main")
	s.SetOverride(true)
	s.SetStrandedReconcile(true)

	if err := s.Start(); err != nil {
		t.Fatalf("expected main-repo-checkout recovery to succeed, got: %v", err)
	}
	t.Cleanup(func() {
		s.Stop()
		removeBranch(t, dir, branch)
	})

	if _, err := os.Stat(filepath.Join(s.WorkDir(), "tracked.txt")); err != nil {
		t.Errorf("expected tracked.txt in worktree after recovery, got: %v", err)
	}
	headRef := runGit(t, s.WorkDir(), "symbolic-ref", "HEAD")
	if !strings.Contains(headRef, branch) {
		t.Errorf("expected worktree HEAD to be on %q, got %q", branch, headRef)
	}
	mainHeadRef := runGit(t, dir, "symbolic-ref", "HEAD")
	if !strings.Contains(mainHeadRef, "main") {
		t.Errorf("expected main repo to be back on main after recovery, got %q", mainHeadRef)
	}
}

func TestIsBranchCheckedOutError_OnlyMatchesCheckedOutOrWorktreeMessages(t *testing.T) {
	// The recovery loop must only trigger on the "checked out at" /
	// "used by worktree at" patterns. A generic branch-deletion error
	// (for example, a permission denied, a missing ref, or an I/O
	// failure) must not be classified as recoverable, otherwise the
	// recovery loop would silently swallow unrelated failures.
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{
			name: "modern checked out message",
			out:  "error: Cannot delete branch 'sandman/42-fix-bug' checked out at '/tmp/repo'\n",
			want: true,
		},
		{
			name: "legacy used by worktree message",
			out:  "error: cannot delete branch 'sandman/42-fix-bug' used by worktree at '/tmp/repo'\n",
			want: true,
		},
		{
			name: "branch not found",
			out:  "error: branch 'sandman/42-fix-bug' not found.\n",
			want: false,
		},
		{
			name: "permission denied",
			out:  "error: cannot remove '.git/refs/heads/sandman/42-fix-bug': Permission denied\n",
			want: false,
		},
		{
			name: "empty output",
			out:  "",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isBranchCheckedOutError([]byte(tc.out)); got != tc.want {
				t.Errorf("isBranchCheckedOutError(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}

func TestStrandedWorktree_ResolvesRelativeWorktreeBase(t *testing.T) {
	// The StrandedWorktree helper resolves a relative worktreeBase
	// against repoPath internally, so callers can pass the configured
	// (typically relative) WorktreeDir directly. Without this, the
	// stranded-worktree recovery in WorktreeSandbox.Start silently
	// never fires in default deployments where the configured
	// WorktreeDir is relative (`.sandman/worktrees`).
	dir := t.TempDir()
	initGitRepo(t, dir)

	worktreeBase := ".sandman/worktrees"
	if err := os.MkdirAll(filepath.Join(dir, worktreeBase), 0755); err != nil {
		t.Fatalf("mkdir worktreeBase: %v", err)
	}

	const expected = "sandman/907-foo"
	const actual = "sandman/42-other-branch"
	runGit(t, dir, "branch", expected)
	runGit(t, dir, "branch", actual)
	strandedPath := filepath.Join(dir, worktreeBase, expected)
	runGit(t, dir, "worktree", "add", "--force", strandedPath, actual)

	info, stranded := StrandedWorktree(dir, worktreeBase, expected)
	if !stranded {
		t.Fatalf("expected stranded=true for relative worktreeBase, got info=%+v", info)
	}
	if info.Path != strandedPath {
		t.Errorf("Path: got %q, want %q", info.Path, strandedPath)
	}
}

func TestWorktreeSandbox_StartCallsReconcileStrandedFnSeam(t *testing.T) {
	// The recovery loop is dispatched through the package-level
	// reconcileStrandedFn seam (ADR-0027). When a `git branch -D`
	// failure matches the "checked out at" pattern, the seam is
	// invoked with the expected (repoPath, worktreeBase, branch,
	// sourceBranch) tuple.
	prev := reconcileStrandedFn
	defer func() { reconcileStrandedFn = prev }()

	var captured struct {
		repoPath     string
		worktreeBase string
		branch       string
		sourceBranch string
		called       bool
	}
	reconcileStrandedFn = func(repoPath, worktreeBase, branch, sourceBranch string) error {
		captured.repoPath = repoPath
		captured.worktreeBase = worktreeBase
		captured.branch = branch
		captured.sourceBranch = sourceBranch
		captured.called = true
		return nil
	}

	dir := t.TempDir()
	initGitRepo(t, dir)
	const branch = "sandman/42-fix-bug"
	runGit(t, dir, "checkout", "-b", branch)

	worktreeBase := filepath.Join(dir, ".sandman", "worktrees")
	s := NewWorktreeSandbox(dir, worktreeBase, branch, "main")
	s.SetOverride(true)
	s.SetStrandedReconcile(true)

	// The seam will silently succeed but no worktree exists yet at
	// the worktreeBase, so the worktree-add step still fails. We
	// only need the recovery to fire and the seam to be invoked.
	_ = s.Start()

	if !captured.called {
		t.Fatal("expected reconcileStrandedFn seam to be called during Start")
	}
	if captured.repoPath != dir {
		t.Errorf("seam repoPath: got %q, want %q", captured.repoPath, dir)
	}
	if captured.worktreeBase != worktreeBase {
		t.Errorf("seam worktreeBase: got %q, want %q", captured.worktreeBase, worktreeBase)
	}
	if captured.branch != branch {
		t.Errorf("seam branch: got %q, want %q", captured.branch, branch)
	}
	if captured.sourceBranch != "main" {
		t.Errorf("seam sourceBranch: got %q, want %q", captured.sourceBranch, "main")
	}

	t.Cleanup(func() {
		runGit(t, dir, "checkout", "main")
		removeBranch(t, dir, branch)
	})
}

func TestWorktreeSandbox_StartReattachesPrunableWorktree_DirIntact(t *testing.T) {
	// A worktree exists and is registered, but its .git gitlink points to a
	// non-existent gitdir, making it show as "prunable" in git worktree list.
	// Start() must detect the prunable registration, prune it, re-register the
	// existing directory (no -b flag), preserve content, and return success.
	dir := t.TempDir()
	initGitRepoWithRemote(t, dir)
	commitGitFile(t, dir, "tracked.txt", "base\n", "base")
	runGit(t, dir, "push", "origin", "main")

	worktreeBase := filepath.Join(dir, ".sandman", "worktrees")
	branch := "sandman/42-fix-bug"

	s1 := NewWorktreeSandbox(dir, worktreeBase, branch, "main")
	if err := s1.Start(); err != nil {
		t.Fatalf("create initial worktree: %v", err)
	}

	branchTipBefore := runGit(t, dir, "rev-parse", "refs/heads/"+branch)

	uncommittedPath := filepath.Join(s1.WorkDir(), "uncommitted.txt")
	if err := os.WriteFile(uncommittedPath, []byte("work in progress\n"), 0644); err != nil {
		t.Fatalf("write uncommitted file: %v", err)
	}

	gitPath := filepath.Join(s1.WorkDir(), ".git")
	if err := os.WriteFile(gitPath, []byte("gitdir: /tmp/nonexistent-worktree-gitdir\n"), 0644); err != nil {
		t.Fatalf("corrupt .git file: %v", err)
	}

	if _, reclaimable := ReclaimableWorktree(dir, worktreeBase, branch); !reclaimable {
		t.Fatalf("expected ReclaimableWorktree to return true after gitlink corruption")
	}

	s2 := NewWorktreeSandbox(dir, worktreeBase, branch, "main")
	if err := s2.Start(); err != nil {
		t.Fatalf("Start() should reattach prunable worktree, got: %v", err)
	}
	t.Cleanup(func() {
		s2.Stop()
		removeBranch(t, dir, branch)
	})

	if _, err := os.Stat(filepath.Join(s2.WorkDir(), "tracked.txt")); err != nil {
		t.Errorf("expected tracked.txt in reattached worktree, got err=%v", err)
	}

	if _, err := os.Stat(uncommittedPath); err != nil {
		t.Errorf("expected uncommitted.txt to be preserved in reattached worktree, got err=%v", err)
	}

	branchTipAfter := runGit(t, dir, "rev-parse", "refs/heads/"+branch)
	if branchTipAfter != branchTipBefore {
		t.Errorf("branch tip changed: before=%q, after=%q", branchTipBefore, branchTipAfter)
	}

	revParse := exec.Command("git", "rev-parse", "--git-dir")
	revParse.Dir = s2.WorkDir()
	if out, err := revParse.CombinedOutput(); err != nil {
		t.Fatalf("worktree dir is not a real git worktree: %v: %s", err, out)
	}
}

func TestWorktreeSandbox_StartReattachesPrunableWorktree_DirGone(t *testing.T) {
	// A worktree registration exists (prunable due to broken gitlink) but the
	// worktree directory itself has been deleted from disk. Start() must
	// detect the prunable registration, prune it, and create a fresh worktree
	// on the existing branch tip. No error, branch tip preserved.
	dir := t.TempDir()
	initGitRepoWithRemote(t, dir)
	commitGitFile(t, dir, "tracked.txt", "base\n", "base")
	runGit(t, dir, "push", "origin", "main")

	worktreeBase := filepath.Join(dir, ".sandman", "worktrees")
	branch := "sandman/42-fix-bug"

	s1 := NewWorktreeSandbox(dir, worktreeBase, branch, "main")
	if err := s1.Start(); err != nil {
		t.Fatalf("create initial worktree: %v", err)
	}

	branchTipBefore := runGit(t, dir, "rev-parse", "refs/heads/"+branch)
	worktreePath := s1.WorkDir()

	gitPath := filepath.Join(worktreePath, ".git")
	if err := os.WriteFile(gitPath, []byte("gitdir: /tmp/nonexistent-worktree-gitdir\n"), 0644); err != nil {
		t.Fatalf("corrupt .git file: %v", err)
	}

	if _, reclaimable := ReclaimableWorktree(dir, worktreeBase, branch); !reclaimable {
		t.Fatalf("expected ReclaimableWorktree to return true after gitlink corruption")
	}

	if err := os.RemoveAll(worktreePath); err != nil {
		t.Fatalf("remove worktree dir: %v", err)
	}

	s2 := NewWorktreeSandbox(dir, worktreeBase, branch, "main")
	if err := s2.Start(); err != nil {
		t.Fatalf("Start() should recreate prunable worktree with dir gone, got: %v", err)
	}
	t.Cleanup(func() {
		s2.Stop()
		removeBranch(t, dir, branch)
	})

	if _, err := os.Stat(filepath.Join(s2.WorkDir(), "tracked.txt")); err != nil {
		t.Errorf("expected tracked.txt in recreated worktree, got err=%v", err)
	}

	branchTipAfter := runGit(t, dir, "rev-parse", "refs/heads/"+branch)
	if branchTipAfter != branchTipBefore {
		t.Errorf("branch tip changed: before=%q, after=%q", branchTipBefore, branchTipAfter)
	}

	revParse := exec.Command("git", "rev-parse", "--git-dir")
	revParse.Dir = s2.WorkDir()
	if out, err := revParse.CombinedOutput(); err != nil {
		t.Fatalf("worktree dir is not a real git worktree: %v: %s", err, out)
	}
}

func TestWorktreeSandbox_StartSkipsPrunableReattachWhenReconcileDisabled(t *testing.T) {
	// A worktree exists and is registered, but its .git gitlink points to a
	// non-existent gitdir, making it show as "prunable" in git worktree list.
	// With SetStrandedReconcile(false), Start() must NOT attempt reattach;
	// instead it must fall through to the "branch already exists" error,
	// preserving the fail-loudly contract for operators who pass
	// --no-reconcile-stranded.
	dir := t.TempDir()
	initGitRepoWithRemote(t, dir)
	commitGitFile(t, dir, "tracked.txt", "base\n", "base")
	runGit(t, dir, "push", "origin", "main")

	worktreeBase := filepath.Join(dir, ".sandman", "worktrees")
	branch := "sandman/42-fix-bug"

	s1 := NewWorktreeSandbox(dir, worktreeBase, branch, "main")
	if err := s1.Start(); err != nil {
		t.Fatalf("create initial worktree: %v", err)
	}
	t.Cleanup(func() {
		s1.Stop()
		removeBranch(t, dir, branch)
	})

	gitPath := filepath.Join(s1.WorkDir(), ".git")
	if err := os.WriteFile(gitPath, []byte("gitdir: /tmp/nonexistent-worktree-gitdir\n"), 0644); err != nil {
		t.Fatalf("corrupt .git file: %v", err)
	}

	if _, reclaimable := ReclaimableWorktree(dir, worktreeBase, branch); !reclaimable {
		t.Fatalf("expected ReclaimableWorktree to return true after gitlink corruption")
	}

	s2 := NewWorktreeSandbox(dir, worktreeBase, branch, "main")
	s2.SetStrandedReconcile(false)
	err := s2.Start()
	if err == nil {
		t.Fatal("expected error when reconcile is disabled and worktree is prunable")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected 'already exists' error, got: %v", err)
	}
}
