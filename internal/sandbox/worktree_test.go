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

func TestSyncBaseBranchFailsWhenBaseBranchCannotFastForward(t *testing.T) {
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
	err := s2.Start()
	if err == nil {
		t.Fatal("expected error when branch does not exist locally")
	}
	if !strings.Contains(err.Error(), "does not exist locally") {
		t.Errorf("expected error to mention branch not existing, got: %v", err)
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
