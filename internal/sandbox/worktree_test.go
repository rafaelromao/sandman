package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/shellenv"
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
	if err := s.Start(SandboxStart{StrandedReconcile: true}); err != nil {
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
	if err := s.Start(SandboxStart{StrandedReconcile: true}); err != nil {
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
	if err := s.Start(SandboxStart{StrandedReconcile: true}); err != nil {
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

func TestSyncBaseBranchResetsWhenDiverged(t *testing.T) {
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

	if err := SyncBaseBranch(localDir, "main"); err != nil {
		t.Fatalf("expected sync to succeed by resetting to remote, got %v", err)
	}

	mainHead, err := gitRevParse(localDir, "refs/heads/main")
	if err != nil {
		t.Fatalf("failed to get main ref: %v", err)
	}
	remoteMain, err := gitRevParse(remoteDir, "refs/heads/main")
	if err != nil {
		t.Fatalf("failed to get remote main: %v", err)
	}
	if mainHead != remoteMain {
		t.Fatalf("expected local main to be reset to remote main (%s), got %s", remoteMain[:7], mainHead[:7])
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
	if err := s.Start(SandboxStart{StrandedReconcile: true}); err == nil {
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
	if err := s.Start(SandboxStart{StrandedReconcile: true}); err != nil {
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
	if err := s.Start(SandboxStart{StrandedReconcile: true}); err != nil {
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
	if err := s.Start(SandboxStart{StrandedReconcile: true}); err != nil {
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
	if err := s2.Start(SandboxStart{StrandedReconcile: true}); err != nil {
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
	if err := s.Start(SandboxStart{Override: true, StrandedReconcile: true}); err != nil {
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
	if err := s2.Start(SandboxStart{Override: true, StrandedReconcile: true}); err != nil {
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
	err := s.Start(SandboxStart{StrandedReconcile: true})
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
	if err := s.Start(SandboxStart{StrandedReconcile: true}); err != nil {
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

// TestWorktreeSandbox_ExecInteractive_IsProcessGroupLeader asserts that
// ExecInteractive spawns its command as a process-group leader (PGID == PID),
// so waitCmd's negative-PID SIGKILL on context cancel targets only the
// agent's group. Regression for #1782.
func TestWorktreeSandbox_ExecInteractive_IsProcessGroupLeader(t *testing.T) {
	if err := exec.Command("sh", "-c", "true").Run(); err != nil {
		t.Skipf("sh not available: %v", err)
	}

	dir := t.TempDir()
	_ = initGitRepoWithRemote(t, dir)
	removeBranch(t, dir, "sandman/42-fix-bug")

	s := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	if err := s.Start(SandboxStart{StrandedReconcile: true}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(func() {
		s.Stop()
		removeBranch(t, dir, "sandman/42-fix-bug")
	})

	pidFile := filepath.Join(t.TempDir(), "child.pid")
	command := fmt.Sprintf("echo $$ > %s; sleep 30", shellenv.Quote(pidFile))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.ExecInteractive(ctx, command)
	}()

	waitForChildReadyFileTB(t, pidFile, 2*time.Second)
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatalf("parse pid %q: %v", strings.TrimSpace(string(pidBytes)), err)
	}

	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatalf("getpgid(%d): %v", pid, err)
	}
	if pgid != pid {
		t.Fatalf("expected spawned shell PID %d to equal its PGID (be a process-group leader); got PGID %d — missing Setpgid on WorktreeSandbox.ExecInteractive?", pid, pgid)
	}

	cancel()
	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("ExecInteractive did not return within 3s of cancel")
	}
}

func waitForChildReadyFileTB(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(20 * time.Millisecond)
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
	if err := s.Start(SandboxStart{Override: true, StrandedReconcile: true}); err != nil {
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
	if err := s.Start(SandboxStart{StrandedReconcile: true}); err != nil {
		t.Fatalf("unexpected error on first start: %v", err)
	}
	t.Cleanup(func() {
		s.Stop()
		removeBranch(t, dir, "sandman/42-fix-bug")
	})

	runGit(t, s.WorkDir(), "checkout", "-b", "wrong-branch")

	s2 := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	err := s2.Start(SandboxStart{StrandedReconcile: true})
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
	if err := s.Start(SandboxStart{StrandedReconcile: true}); err != nil {
		t.Fatalf("unexpected error on first start: %v", err)
	}
	t.Cleanup(func() {
		s.Stop()
		removeBranch(t, dir, "sandman/42-fix-bug")
	})

	runGit(t, s.WorkDir(), "checkout", "-b", "wrong-branch")

	s2 := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	err := s2.Start(SandboxStart{StrandedReconcile: true})
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
	if err := s.Start(SandboxStart{StrandedReconcile: true}); err != nil {
		t.Fatalf("unexpected error on first start: %v", err)
	}
	t.Cleanup(func() {
		s.Stop()
		removeBranch(t, dir, "sandman/42-fix-bug")
	})

	runGit(t, s.WorkDir(), "checkout", "--detach", "HEAD")

	s2 := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	err := s2.Start(SandboxStart{StrandedReconcile: true})
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
	if err := s1.Start(SandboxStart{StrandedReconcile: true}); err != nil {
		t.Fatalf("create initial worktree: %v", err)
	}

	// Corrupt the .git gitlink to simulate a stale gitdir (e.g. container destroyed).
	gitPath := filepath.Join(s1.WorkDir(), ".git")
	if err := os.WriteFile(gitPath, []byte("gitdir: /tmp/nonexistent-worktree-gitdir\n"), 0644); err != nil {
		t.Fatalf("corrupt .git file: %v", err)
	}

	// Now try Start() with override, simulating the review daemon's behavior.
	s2 := NewWorktreeSandbox(dir, worktreeBase, branch, "main")
	err := s2.Start(SandboxStart{Override: true, StrandedReconcile: true})
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
	if err := s.Start(SandboxStart{Override: true, StrandedReconcile: true}); err != nil {
		t.Fatalf("unexpected error on first start: %v", err)
	}
	t.Cleanup(func() {
		s.Stop()
		removeBranch(t, dir, "sandman/42-fix-bug")
	})

	runGit(t, s.WorkDir(), "checkout", "-b", "wrong-branch")

	s2 := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	if err := s2.Start(SandboxStart{Override: true, StrandedReconcile: true}); err != nil {
		t.Fatalf("expected no error with --override, got: %v", err)
	}

	headRef := runGit(t, s2.WorkDir(), "symbolic-ref", "HEAD")
	if !strings.Contains(headRef, "sandman/42-fix-bug") {
		t.Errorf("expected HEAD to be on sandman/42-fix-bug after force-checkout, got %q", headRef)
	}
}

// TestWorktreeSandbox_OverrideBypassesContinueRefusal pins that
// --override combined with --continue still enters the override
// reconciliation path. The continue-mode pre-validation is gated on
// `continueRun && !override` so a user explicitly opting into override
// can recover even from a preserved worktree that would otherwise be
// refused by continue alone. Issue #2189.
func TestWorktreeSandbox_OverrideBypassesContinueRefusal(t *testing.T) {
	dir := t.TempDir()
	_ = initGitRepoWithRemote(t, dir)
	removeBranch(t, dir, "sandman/42-fix-bug")

	s := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	if err := s.Start(SandboxStart{Override: true, StrandedReconcile: true}); err != nil {
		t.Fatalf("create initial worktree: %v", err)
	}

	runGit(t, s.WorkDir(), "checkout", "-b", "wrong-branch")

	s2 := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	if err := s2.Start(SandboxStart{Override: true, Continue: true, StrandedReconcile: true}); err != nil {
		t.Fatalf("expected --continue --override to succeed via override reconciliation, got: %v", err)
	}
	t.Cleanup(func() {
		s2.Stop()
		removeBranch(t, dir, "sandman/42-fix-bug")
	})

	headRef := runGit(t, s2.WorkDir(), "symbolic-ref", "HEAD")
	if !strings.Contains(headRef, "sandman/42-fix-bug") {
		t.Errorf("expected HEAD to be on sandman/42-fix-bug after override reconciliation, got %q", headRef)
	}
}

func TestWorktreeSandbox_OverrideReconcileMissingBranch(t *testing.T) {
	dir := t.TempDir()
	_ = initGitRepoWithRemote(t, dir)
	removeBranch(t, dir, "sandman/42-fix-bug")

	s := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	if err := s.Start(SandboxStart{Override: true, StrandedReconcile: true}); err != nil {
		t.Fatalf("unexpected error on first start: %v", err)
	}
	t.Cleanup(func() {
		s.Stop()
	})

	runGit(t, s.WorkDir(), "checkout", "-b", "wrong-branch")

	runGit(t, dir, "branch", "-D", "sandman/42-fix-bug")

	s2 := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	if err := s2.Start(SandboxStart{Override: true, StrandedReconcile: false}); err != nil {
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
	if err := s.Start(SandboxStart{Override: true, StrandedReconcile: true}); err != nil {
		t.Fatalf("unexpected error on first start: %v", err)
	}
	t.Cleanup(func() {
		s.Stop()
		removeBranch(t, dir, "sandman/42-fix-bug")
	})

	runGit(t, s.WorkDir(), "checkout", "--detach", "HEAD")

	s2 := NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "sandman/42-fix-bug", "main")
	if err := s2.Start(SandboxStart{Override: true, StrandedReconcile: true}); err != nil {
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

	err := s.Start(SandboxStart{Override: true, StrandedReconcile: false})
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
	addStrandedWorktree(t, dir, worktreeBase, branch, otherBranch)

	runGit(t, dir, "checkout", branch)

	s := NewWorktreeSandbox(dir, worktreeBase, branch, "main")

	if err := s.Start(SandboxStart{Override: true, StrandedReconcile: true}); err != nil {
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

	if err := s.Start(SandboxStart{Override: true, StrandedReconcile: true}); err != nil {
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

// TestParseCheckedOutPath_ExtractsWorktreePath pins issue #2140: the
// override path uses this helper to recover from `git branch -D` failures
// caused by the branch being checked out in a foreign worktree. The
// helper must extract the worktree path from both modern and legacy
// error message formats so it can be passed to releaseBranchInWorktree.
func TestParseCheckedOutPath_ExtractsWorktreePath(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want string
		ok   bool
	}{
		{
			name: "modern checked out message",
			out:  "error: Cannot delete branch 'sandman/30-foo' checked out at '/tmp/repo/.sandman/worktrees/review-148'\n",
			want: "/tmp/repo/.sandman/worktrees/review-148",
			ok:   true,
		},
		{
			name: "legacy used by worktree message",
			out:  "error: cannot delete branch 'sandman/30-foo' used by worktree at '/tmp/repo/.sandman/worktrees/review-148'\n",
			want: "/tmp/repo/.sandman/worktrees/review-148",
			ok:   true,
		},
		{
			name: "branch not found",
			out:  "error: branch 'sandman/30-foo' not found.\n",
			want: "",
			ok:   false,
		},
		{
			name: "permission denied",
			out:  "error: cannot remove '.git/refs/heads/sandman/30-foo': Permission denied\n",
			want: "",
			ok:   false,
		},
		{
			name: "empty output",
			out:  "",
			want: "",
			ok:   false,
		},
		{
			name: "path with spaces inside quotes",
			out:  "error: Cannot delete branch 'sandman/30-foo' checked out at '/tmp/path with spaces/wt'\n",
			want: "/tmp/path with spaces/wt",
			ok:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseCheckedOutPath([]byte(tc.out))
			if ok != tc.ok {
				t.Errorf("parseCheckedOutPath(%q) ok = %v, want %v", tc.out, ok, tc.ok)
			}
			if got != tc.want {
				t.Errorf("parseCheckedOutPath(%q) = %q, want %q", tc.out, got, tc.want)
			}
		})
	}
}

// TestWorktreeSandbox_OverrideReclaimsForeignStrandedWorktree pins the
// regression guard for issues #2140 and #2187: when a foreign live
// worktree (e.g. a leftover review worktree) holds the target branch
// at a path that differs from the canonical <worktreeBase>/<branch>,
// `Start()` with --override must:
//
//  1. Detach HEAD in the foreign worktree so the branch becomes
//     deletable. The foreign worktree's directory, `.git` gitlink,
//     and `.git/worktrees/<dir>` registration all stay intact.
//  2. Delete the branch from the main repo.
//  3. Create a fresh canonical worktree holding the new branch on
//     the source branch.
//
// The #2140 contract had `Start()` force-remove the foreign worktree
// entirely. That destroyed sibling worktrees in parallel sandbox runs
// because all runs share the same `.git` bind mount. The #2187
// contract preserves the foreign worktree and only detaches its HEAD.
func TestWorktreeSandbox_OverrideReclaimsForeignStrandedWorktree(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	const branch = "sandman/30-foo"
	runGit(t, repoDir, "branch", branch)

	worktreeBase := filepath.Join(repoDir, ".sandman", "worktrees")
	if err := os.MkdirAll(worktreeBase, 0755); err != nil {
		t.Fatalf("mkdir worktreeBase: %v", err)
	}

	// Foreign worktree at a path that does NOT match worktreeBase/branch.
	foreignPath := filepath.Join(worktreeBase, "review-148-foreign")
	runGit(t, repoDir, "worktree", "add", foreignPath, branch)

	// Sanity: branch is currently held by the foreign worktree.
	info, ok := ForeignStrandedWorktree(repoDir, worktreeBase, branch)
	if !ok {
		t.Fatalf("precondition: expected ForeignStrandedWorktree to detect the foreign worktree at %q, got info=%+v", foreignPath, info)
	}

	// Override-start: must release the branch from the foreign worktree
	// (without destroying it) and create a fresh worktree at the
	// canonical path.
	sb := NewWorktreeSandbox(repoDir, worktreeBase, branch, "main")
	if err := sb.Start(SandboxStart{Override: true, StrandedReconcile: true}); err != nil {
		t.Fatalf("Start with --override failed to reclaim foreign worktree: %v", err)
	}

	// After Start, the canonical worktree exists and points at branch.
	canonicalPath := filepath.Join(worktreeBase, branch)
	if _, err := os.Stat(canonicalPath); err != nil {
		t.Fatalf("expected canonical worktree dir at %q after Start, got: %v", canonicalPath, err)
	}
	headRef := strings.TrimSpace(runGit(t, canonicalPath, "symbolic-ref", "--quiet", "HEAD"))
	if !strings.HasSuffix(headRef, branch) {
		t.Errorf("expected canonical worktree HEAD on %q, got %q", branch, headRef)
	}

	// Issue #2187: foreign worktree must still exist (scoped recovery).
	if _, err := os.Stat(foreignPath); err != nil {
		t.Errorf("expected foreign worktree dir to remain at %q under scoped recovery, got err=%v", foreignPath, err)
	}
	// Foreign worktree registration must still exist.
	listAfter := runGit(t, repoDir, "worktree", "list", "--porcelain")
	if !strings.Contains(listAfter, foreignPath) {
		t.Errorf("expected foreign worktree registration at %q to remain, list after=\n%s", foreignPath, listAfter)
	}
	// Foreign worktree's HEAD must now be detached (the branch has been
	// released from there). The branch is then re-created at the
	// canonical path via `git worktree add -b <branch>` below, so we
	// assert the foreign's HEAD is detached rather than asserting the
	// branch is gone from the repo.
	detachedCmd := exec.Command("git", "symbolic-ref", "--quiet", "HEAD")
	detachedCmd.Dir = foreignPath
	detachedOut, _ := detachedCmd.CombinedOutput()
	if strings.TrimSpace(string(detachedOut)) != "" {
		t.Errorf("expected foreign worktree HEAD to be detached after recovery, got %q", strings.TrimSpace(string(detachedOut)))
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
	strandedPath := addStrandedWorktree(t, dir, filepath.Join(dir, worktreeBase), expected, actual)

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

	// The seam will silently succeed but no worktree exists yet at
	// the worktreeBase, so the worktree-add step still fails. We
	// only need the recovery to fire and the seam to be invoked.
	_ = s.Start(SandboxStart{Override: true, StrandedReconcile: true})

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
	if err := s1.Start(SandboxStart{StrandedReconcile: true}); err != nil {
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
	if err := s2.Start(SandboxStart{StrandedReconcile: true}); err != nil {
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
	if err := s1.Start(SandboxStart{StrandedReconcile: true}); err != nil {
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
	if err := s2.Start(SandboxStart{StrandedReconcile: true}); err != nil {
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
	// With SandboxStart{StrandedReconcile: false}, Start() must NOT attempt
	// reattach; instead it must fall through to the "branch already exists"
	// error, preserving the fail-loudly contract for operators who pass
	// --no-reconcile-stranded.
	dir := t.TempDir()
	initGitRepoWithRemote(t, dir)
	commitGitFile(t, dir, "tracked.txt", "base\n", "base")
	runGit(t, dir, "push", "origin", "main")

	worktreeBase := filepath.Join(dir, ".sandman", "worktrees")
	branch := "sandman/42-fix-bug"

	s1 := NewWorktreeSandbox(dir, worktreeBase, branch, "main")
	if err := s1.Start(SandboxStart{StrandedReconcile: true}); err != nil {
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
	err := s2.Start(SandboxStart{StrandedReconcile: false})
	if err == nil {
		t.Fatal("expected error when reconcile is disabled and worktree is prunable")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected 'already exists' error, got: %v", err)
	}
}

// TestReleaseBranchInWorktree_NoOpOnMissingPath pins acceptance criterion
// #8: a missing path is a no-op (returns nil, writes nothing). This is
// the safety guarantee that lets the recovery code call the helper
// without checking whether a foreign worktree directory still exists.
func TestReleaseBranchInWorktree_NoOpOnMissingPath(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist")

	if err := releaseBranchInWorktree(missing); err != nil {
		t.Fatalf("expected nil for missing path, got: %v", err)
	}

	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("expected missing path to remain non-existent, got err=%v", err)
	}
}

// TestReleaseBranchInWorktree_DetachesBranchWithoutRemovingWorktree
// pins the core behaviour: calling releaseBranchInWorktree detaches
// the worktree's HEAD so `git branch -D` becomes legal from the main
// repo, while leaving the worktree directory, `.git` gitlink, and
// `.git/worktrees/<dir>` registration intact.
func TestReleaseBranchInWorktree_DetachesBranchWithoutRemovingWorktree(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	const branch = "sandman/2187-release-target"
	runGit(t, repoDir, "branch", branch)

	worktreeBase := filepath.Join(repoDir, ".sandman", "worktrees")
	if err := os.MkdirAll(worktreeBase, 0755); err != nil {
		t.Fatalf("mkdir worktreeBase: %v", err)
	}
	wtPath := filepath.Join(worktreeBase, "foreign-2187-review")
	runGit(t, repoDir, "worktree", "add", wtPath, branch)

	listBefore := runGit(t, repoDir, "worktree", "list", "--porcelain")
	if !strings.Contains(listBefore, wtPath) {
		t.Fatalf("precondition: expected worktree list to include %q, got:\n%s", wtPath, listBefore)
	}

	if err := releaseBranchInWorktree(wtPath); err != nil {
		t.Fatalf("releaseBranchInWorktree failed: %v", err)
	}

	if _, err := os.Stat(wtPath); err != nil {
		t.Errorf("expected worktree directory to remain at %q, got: %v", wtPath, err)
	}

	listAfter := runGit(t, repoDir, "worktree", "list", "--porcelain")
	if !strings.Contains(listAfter, wtPath) {
		t.Errorf("expected worktree registration at %q to remain, list after=\n%s", wtPath, listAfter)
	}

	detachedCmd := exec.Command("git", "symbolic-ref", "--quiet", "HEAD")
	detachedCmd.Dir = wtPath
	detachedOut, _ := detachedCmd.CombinedOutput()
	if strings.TrimSpace(string(detachedOut)) != "" {
		t.Errorf("expected HEAD to be detached after release, got %q", strings.TrimSpace(string(detachedOut)))
	}

	deleteCmd := exec.Command("git", "branch", "-D", branch)
	deleteCmd.Dir = repoDir
	if out, err := deleteCmd.CombinedOutput(); err != nil {
		t.Errorf("expected `git branch -D %s` to succeed after release, got: %v\n%s", branch, err, out)
	}
}

// TestWorktreeSandbox_OverrideDoesNotDeleteForeignLiveWorktree pins
// acceptance criterion #5 from issue #2187: a foreign live worktree
// that holds the target branch on a NON-canonical path must NOT be
// deleted by `WorktreeSandbox.Start(override)`. Only the branch needs
// to be released from the foreign (via detached HEAD) so the canonical
// worktree can be created.
func TestWorktreeSandbox_OverrideDoesNotDeleteForeignLiveWorktree(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	const branch = "sandman/2187-foreign-live"
	runGit(t, repoDir, "branch", branch)

	worktreeBase := filepath.Join(repoDir, ".sandman", "worktrees")
	if err := os.MkdirAll(worktreeBase, 0755); err != nil {
		t.Fatalf("mkdir worktreeBase: %v", err)
	}

	// Foreign live worktree at non-canonical path, holds the branch.
	foreignPath := filepath.Join(worktreeBase, "review-2187-foreign-live")
	runGit(t, repoDir, "worktree", "add", foreignPath, branch)

	// Capture foreign worktree state to confirm it survives Start.
	listBefore := runGit(t, repoDir, "worktree", "list", "--porcelain")
	if !strings.Contains(listBefore, foreignPath) {
		t.Fatalf("precondition: foreign worktree at %q must be registered, list=\n%s", foreignPath, listBefore)
	}

	// Override Start — this is the recovery path that previously
	// destroyed the foreign worktree via `git worktree remove --force`.
	sb := NewWorktreeSandbox(repoDir, worktreeBase, branch, "main")
	if err := sb.Start(SandboxStart{Override: true, StrandedReconcile: true}); err != nil {
		t.Fatalf("Start with override failed: %v", err)
	}
	t.Cleanup(func() {
		sb.Stop()
		removeBranch(t, repoDir, branch)
	})

	// 1. Foreign worktree directory MUST still exist.
	if _, err := os.Stat(foreignPath); err != nil {
		t.Errorf("foreign worktree at %q must still exist after Start, got: %v", foreignPath, err)
	}

	// 2. Foreign worktree .git gitlink MUST still point to a valid dir.
	gitPath := filepath.Join(foreignPath, ".git")
	data, err := os.ReadFile(gitPath)
	if err != nil {
		t.Errorf("foreign worktree .git gitlink must still be readable at %q, got: %v", gitPath, err)
	}
	if !strings.HasPrefix(string(data), "gitdir: ") {
		t.Errorf("foreign worktree .git content unexpected: %q", string(data))
	}

	// 3. Foreign worktree registration MUST still exist in
	// .git/worktrees/<dir>.
	wtName := filepath.Base(foreignPath)
	regPath := filepath.Join(repoDir, ".git", "worktrees", wtName)
	if _, err := os.Stat(regPath); err != nil {
		t.Errorf("foreign worktree registration at %q must still exist, got: %v", regPath, err)
	}

	// 4. HEAD in foreign worktree MUST now be detached.
	detachedCmd := exec.Command("git", "symbolic-ref", "--quiet", "HEAD")
	detachedCmd.Dir = foreignPath
	detachedOut, _ := detachedCmd.CombinedOutput()
	if strings.TrimSpace(string(detachedOut)) != "" {
		t.Errorf("expected foreign worktree HEAD to be detached, got %q", strings.TrimSpace(string(detachedOut)))
	}
}

// TestWorktreeSandbox_OverrideDoesNotPruneUnrelatedWorktrees pins
// acceptance criterion #6: a foreign worktree that holds a DIFFERENT
// branch from `s.branch` must be left completely untouched by the
// override recovery path. No reattach, no prune, no removal.
func TestWorktreeSandbox_OverrideDoesNotPruneUnrelatedWorktrees(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	runGit(t, repoDir, "branch", "sandman/2187-target")
	runGit(t, repoDir, "branch", "sandman/2187-unrelated")

	worktreeBase := filepath.Join(repoDir, ".sandman", "worktrees")
	if err := os.MkdirAll(worktreeBase, 0755); err != nil {
		t.Fatalf("mkdir worktreeBase: %v", err)
	}

	// Foreign worktree on an UNRELATED branch at non-canonical path.
	foreignPath := filepath.Join(worktreeBase, "review-2187-unrelated")
	runGit(t, repoDir, "worktree", "add", foreignPath, "sandman/2187-unrelated")

	beforeDirInfo, err := os.Stat(foreignPath)
	if err != nil {
		t.Fatalf("foreign path missing: %v", err)
	}
	beforeList := runGit(t, repoDir, "worktree", "list", "--porcelain")
	beforeBranch := runGit(t, foreignPath, "symbolic-ref", "HEAD")

	// Override Start on the TARGET branch. The unrelated foreign
	// worktree must be invisible to the recovery logic.
	sb := NewWorktreeSandbox(repoDir, worktreeBase, "sandman/2187-target", "main")
	if err := sb.Start(SandboxStart{Override: true, StrandedReconcile: true}); err != nil {
		t.Fatalf("Start with override failed: %v", err)
	}
	t.Cleanup(func() {
		sb.Stop()
		// Tear down the unrelated foreign worktree before removing
		// its branch.
		wtCmd := exec.Command("git", "worktree", "remove", "--force", foreignPath)
		wtCmd.Dir = repoDir
		_, _ = wtCmd.CombinedOutput()
		removeBranch(t, repoDir, "sandman/2187-target")
		removeBranch(t, repoDir, "sandman/2187-unrelated")
	})

	// Foreign worktree directory MUST be unchanged (mtime preserved).
	afterDirInfo, err := os.Stat(foreignPath)
	if err != nil {
		t.Fatalf("foreign worktree disappeared: %v", err)
	}
	if beforeDirInfo.ModTime() != afterDirInfo.ModTime() {
		t.Errorf("foreign worktree mtime changed: before=%v after=%v", beforeDirInfo.ModTime(), afterDirInfo.ModTime())
	}

	// Foreign worktree registration MUST be in the worktree list.
	afterList := runGit(t, repoDir, "worktree", "list", "--porcelain")
	if !strings.Contains(afterList, foreignPath) {
		t.Errorf("foreign worktree at %q was removed from list:\nbefore:\n%s\nafter:\n%s", foreignPath, beforeList, afterList)
	}

	// HEAD ref MUST be unchanged.
	afterBranch := runGit(t, foreignPath, "symbolic-ref", "HEAD")
	if beforeBranch != afterBranch {
		t.Errorf("foreign worktree HEAD changed: before=%q after=%q", beforeBranch, afterBranch)
	}
}

// TestWorktreeSandbox_ParallelStartsDoNotDestroyEachOther pins the
// regression guard for issue #2187: two parallel `WorktreeSandbox.Start`
// cycles on different branches in the same repo must not destroy each
// other's `.git/worktrees/<otherBranch>` registration. This is the
// behavioural contract that protects batch `260713123820-89cd-2165+11`
// and similar from the prunable-entry symptom.
func TestWorktreeSandbox_ParallelStartsDoNotDestroyEachOther(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	const branchA = "sandman/2187-parallel-a"
	const branchB = "sandman/2187-parallel-b"
	runGit(t, repoDir, "branch", branchA)
	runGit(t, repoDir, "branch", branchB)

	worktreeBase := filepath.Join(repoDir, ".sandman", "worktrees")

	// Run A's Start on branchA with override. Assert worktree A is
	// registered, then immediately run B's Start on branchB with
	// override. Neither run should remove the other's registration.
	sbA := NewWorktreeSandbox(repoDir, worktreeBase, branchA, "main")
	if err := sbA.Start(SandboxStart{Override: true, StrandedReconcile: true}); err != nil {
		t.Fatalf("Start A failed: %v", err)
	}

	wtAPath := sbA.WorkDir()
	if _, err := os.Stat(filepath.Join(repoDir, ".git", "worktrees", filepath.Base(wtAPath))); err != nil {
		t.Fatalf("expected A registration to exist after A.Start, got: %v", err)
	}

	sbB := NewWorktreeSandbox(repoDir, worktreeBase, branchB, "main")
	if err := sbB.Start(SandboxStart{Override: true, StrandedReconcile: true}); err != nil {
		t.Fatalf("Start B failed: %v", err)
	}

	wtBPath := sbB.WorkDir()

	// A's registration must still be present after B's Start.
	if _, err := os.Stat(filepath.Join(repoDir, ".git", "worktrees", filepath.Base(wtAPath))); err != nil {
		t.Errorf("expected A registration at .git/worktrees/%s to survive B.Start, got: %v",
			filepath.Base(wtAPath), err)
	}
	// B's registration must be present after B's Start.
	if _, err := os.Stat(filepath.Join(repoDir, ".git", "worktrees", filepath.Base(wtBPath))); err != nil {
		t.Errorf("expected B registration at .git/worktrees/%s after B.Start, got: %v",
			filepath.Base(wtBPath), err)
	}

	// A's worktree HEAD must still point at branchA.
	listAll := runGit(t, repoDir, "worktree", "list", "--porcelain")
	if !strings.Contains(listAll, branchA) || !strings.Contains(listAll, branchB) {
		t.Errorf("expected both branchA and branchB in worktree list, got:\n%s", listAll)
	}
}

// TestWorktreeSandbox_ContinueNormalizesWorkspaceGitlink pins behavior 1 of
// issue #2189: when a preserved worktree's `.git` file points at a
// `/workspace/...` gitdir (left over from a container attempt that exited
// before its RestoreHostPaths ran), a continue-mode Start() must rewrite
// the pointer back to the host-visible gitdir before any other validation.
func TestWorktreeSandbox_ContinueNormalizesWorkspaceGitlink(t *testing.T) {
	dir := t.TempDir()
	initGitRepoWithRemote(t, dir)
	commitGitFile(t, dir, "tracked.txt", "base\n", "base")
	runGit(t, dir, "push", "origin", "main")

	worktreeBase := filepath.Join(dir, ".sandman", "worktrees")
	branch := "sandman/42-fix-bug"

	s1 := NewWorktreeSandbox(dir, worktreeBase, branch, "main")
	if err := s1.Start(SandboxStart{StrandedReconcile: true}); err != nil {
		t.Fatalf("create initial worktree: %v", err)
	}
	t.Cleanup(func() {
		s1.Stop()
		removeBranch(t, dir, branch)
	})

	absRepo, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("resolve repo path: %v", err)
	}
	worktreePath := s1.WorkDir()
	worktreeDirName := filepath.Base(worktreePath)
	containerGitdir := "/workspace/.git/worktrees/" + worktreeDirName
	hostGitdir := filepath.Join(absRepo, ".git", "worktrees", worktreeDirName)

	gitlinkPath := filepath.Join(worktreePath, ".git")
	if err := os.WriteFile(gitlinkPath, []byte("gitdir: "+containerGitdir+"\n"), 0644); err != nil {
		t.Fatalf("rewrite .git to container-visible path: %v", err)
	}

	s2 := NewWorktreeSandbox(dir, worktreeBase, branch, "main")
	if err := s2.Start(SandboxStart{StrandedReconcile: true, Continue: true}); err != nil {
		t.Fatalf("continue-mode Start() should normalize and succeed, got: %v", err)
	}
	t.Cleanup(func() {
		s2.Stop()
	})

	rewritten, err := os.ReadFile(gitlinkPath)
	if err != nil {
		t.Fatalf("read .git after continue Start: %v", err)
	}
	if !strings.Contains(string(rewritten), hostGitdir) {
		t.Errorf("expected .git to point at host gitdir %q, got %q", hostGitdir, string(rewritten))
	}
	if strings.Contains(string(rewritten), "/workspace") {
		t.Errorf("expected .git to no longer contain /workspace, got %q", string(rewritten))
	}
}

// TestWorktreeSandbox_ContinueReusesPreservedWorktree pins behavior 2 of
// issue #2189: when normalization succeeds, the continue-mode Start() must
// reuse the existing worktree and branch without deleting the branch,
// removing the worktree, pruning sibling registrations, or recreating the
// worktree directory. A marker file written into the worktree before the
// second Start() must survive untouched.
func TestWorktreeSandbox_ContinueReusesPreservedWorktree(t *testing.T) {
	dir := t.TempDir()
	initGitRepoWithRemote(t, dir)
	commitGitFile(t, dir, "tracked.txt", "base\n", "base")
	runGit(t, dir, "push", "origin", "main")

	worktreeBase := filepath.Join(dir, ".sandman", "worktrees")
	branch := "sandman/42-fix-bug"

	s1 := NewWorktreeSandbox(dir, worktreeBase, branch, "main")
	if err := s1.Start(SandboxStart{StrandedReconcile: true}); err != nil {
		t.Fatalf("create initial worktree: %v", err)
	}

	branchTipBefore := runGit(t, dir, "rev-parse", "refs/heads/"+branch)

	markerPath := filepath.Join(s1.WorkDir(), "preserved-marker.txt")
	if err := os.WriteFile(markerPath, []byte("untouched\n"), 0644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	worktreeDirName := filepath.Base(s1.WorkDir())
	containerGitdir := "/workspace/.git/worktrees/" + worktreeDirName
	if err := os.WriteFile(filepath.Join(s1.WorkDir(), ".git"), []byte("gitdir: "+containerGitdir+"\n"), 0644); err != nil {
		t.Fatalf("rewrite .git to /workspace: %v", err)
	}

	s2 := NewWorktreeSandbox(dir, worktreeBase, branch, "main")
	if err := s2.Start(SandboxStart{StrandedReconcile: true, Continue: true}); err != nil {
		t.Fatalf("continue-mode Start() should reuse preserved worktree, got: %v", err)
	}
	t.Cleanup(func() {
		s2.Stop()
		removeBranch(t, dir, branch)
	})

	if s2.WorkDir() != s1.WorkDir() {
		t.Errorf("expected reused workdir %q, got %q", s1.WorkDir(), s2.WorkDir())
	}

	data, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("expected preserved marker to survive, got: %v", err)
	}
	if string(data) != "untouched\n" {
		t.Errorf("expected marker contents untouched, got %q", string(data))
	}

	branchTipAfter := runGit(t, dir, "rev-parse", "refs/heads/"+branch)
	if branchTipAfter != branchTipBefore {
		t.Errorf("expected branch tip unchanged (%q), got %q", branchTipBefore, branchTipAfter)
	}

	if _, err := os.Stat(filepath.Join(s2.WorkDir(), "tracked.txt")); err != nil {
		t.Errorf("expected tracked.txt from preserved worktree, got: %v", err)
	}

	if _, reclaimable := ReclaimableWorktree(dir, worktreeBase, branch); !reclaimable {
		t.Error("expected preserved worktree to remain registered after reuse")
	}
}

// TestWorktreeSandbox_ContinueIsIdempotentOnHostVisibleGitlink pins behavior 4
// of issue #2189: a continue-mode run whose preserved worktree already has a
// host-visible `.git` pointer must not modify the file. The test must drive
// WorktreeSandbox.Start(SandboxStart{Continue: true}) (not call the free
// function directly) so the assertion proves Start()'s routing is idempotent.
func TestWorktreeSandbox_ContinueIsIdempotentOnHostVisibleGitlink(t *testing.T) {
	dir := t.TempDir()
	initGitRepoWithRemote(t, dir)
	commitGitFile(t, dir, "tracked.txt", "base\n", "base")
	runGit(t, dir, "push", "origin", "main")

	worktreeBase := filepath.Join(dir, ".sandman", "worktrees")
	branch := "sandman/42-fix-bug"

	s1 := NewWorktreeSandbox(dir, worktreeBase, branch, "main")
	if err := s1.Start(SandboxStart{StrandedReconcile: true}); err != nil {
		t.Fatalf("create initial worktree: %v", err)
	}
	t.Cleanup(func() {
		s1.Stop()
		removeBranch(t, dir, branch)
	})

	gitlinkPath := filepath.Join(s1.WorkDir(), ".git")
	beforeBytes, err := os.ReadFile(gitlinkPath)
	if err != nil {
		t.Fatalf("read .git: %v", err)
	}
	beforeInfo, err := os.Stat(gitlinkPath)
	if err != nil {
		t.Fatalf("stat .git: %v", err)
	}
	beforeMtime := beforeInfo.ModTime()

	time.Sleep(20 * time.Millisecond)

	s2 := NewWorktreeSandbox(dir, worktreeBase, branch, "main")
	if err := s2.Start(SandboxStart{StrandedReconcile: true, Continue: true}); err != nil {
		t.Fatalf("continue-mode Start() with host-visible pointer should succeed, got: %v", err)
	}
	t.Cleanup(func() {
		s2.Stop()
	})

	afterBytes, err := os.ReadFile(gitlinkPath)
	if err != nil {
		t.Fatalf("read .git after Start: %v", err)
	}
	if string(beforeBytes) != string(afterBytes) {
		t.Errorf("expected .git bytes unchanged, before=%q after=%q", string(beforeBytes), string(afterBytes))
	}
	afterInfo, err := os.Stat(gitlinkPath)
	if err != nil {
		t.Fatalf("stat .git after Start: %v", err)
	}
	if !afterInfo.ModTime().Equal(beforeMtime) {
		t.Errorf("expected .git mtime unchanged, before=%v after=%v", beforeMtime, afterInfo.ModTime())
	}
}

// TestWorktreeSandbox_ContinueRecoversFromCrashedContainerAttempt reproduces
// the exact sequence observed for issues #2186 / #2187: a container attempt
// rewrote the worktree's .git pointer to /workspace/... and exited before
// its RestoreHostPaths ran. The next --continue run must reuse the existing
// worktree and branch without invoking git branch -D, git worktree remove,
// git worktree prune, or recreating the worktree directory.
//
// This is the end-to-end regression for behavior 6 of issue #2189.
func TestWorktreeSandbox_ContinueRecoversFromCrashedContainerAttempt(t *testing.T) {
	dir := t.TempDir()
	initGitRepoWithRemote(t, dir)
	commitGitFile(t, dir, "tracked.txt", "base\n", "base")
	runGit(t, dir, "push", "origin", "main")

	worktreeBase := filepath.Join(dir, ".sandman", "worktrees")
	branch := "sandman/42-fix-bug"

	s1 := NewWorktreeSandbox(dir, worktreeBase, branch, "main")
	if err := s1.Start(SandboxStart{StrandedReconcile: true}); err != nil {
		t.Fatalf("create initial worktree: %v", err)
	}

	branchTipBefore := runGit(t, dir, "rev-parse", "refs/heads/"+branch)

	markerPath := filepath.Join(s1.WorkDir(), "agent-marker.txt")
	if err := os.WriteFile(markerPath, []byte("agent was here\n"), 0644); err != nil {
		t.Fatalf("write agent marker: %v", err)
	}

	absRepo, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("resolve repo path: %v", err)
	}
	worktreeDirName := filepath.Base(s1.WorkDir())
	containerGitdir := "/workspace/.git/worktrees/" + worktreeDirName
	hostGitdir := filepath.Join(absRepo, ".git", "worktrees", worktreeDirName)

	gitlinkPath := filepath.Join(s1.WorkDir(), ".git")
	if err := os.WriteFile(gitlinkPath, []byte("gitdir: "+containerGitdir+"\n"), 0644); err != nil {
		t.Fatalf("simulate crashed container rewrite of .git: %v", err)
	}

	s2 := NewWorktreeSandbox(dir, worktreeBase, branch, "main")
	if err := s2.Start(SandboxStart{StrandedReconcile: true, Continue: true}); err != nil {
		t.Fatalf("--continue Start() after crashed container must reuse worktree, got: %v", err)
	}
	t.Cleanup(func() {
		s2.Stop()
		removeBranch(t, dir, branch)
	})

	if s2.WorkDir() != s1.WorkDir() {
		t.Errorf("expected reused workdir %q, got %q", s1.WorkDir(), s2.WorkDir())
	}

	afterGitlink, err := os.ReadFile(gitlinkPath)
	if err != nil {
		t.Fatalf("read .git after --continue: %v", err)
	}
	if !strings.Contains(string(afterGitlink), hostGitdir) {
		t.Errorf("expected .git to point at host gitdir %q after --continue, got %q", hostGitdir, string(afterGitlink))
	}
	if strings.Contains(string(afterGitlink), "/workspace") {
		t.Errorf("expected .git to no longer reference /workspace after --continue, got %q", string(afterGitlink))
	}

	markerData, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("expected agent marker to survive --continue, got: %v", err)
	}
	if string(markerData) != "agent was here\n" {
		t.Errorf("expected agent marker contents untouched, got %q", string(markerData))
	}

	branchTipAfter := runGit(t, dir, "rev-parse", "refs/heads/"+branch)
	if branchTipAfter != branchTipBefore {
		t.Errorf("expected branch tip unchanged after --continue, before=%q after=%q", branchTipBefore, branchTipAfter)
	}

	listOutput := runGit(t, dir, "worktree", "list", "--porcelain")
	resolvedWorkDir, err := filepath.EvalSymlinks(s1.WorkDir())
	if err != nil {
		resolvedWorkDir = s1.WorkDir()
	}
	if !strings.Contains(listOutput, "worktree "+resolvedWorkDir) && !strings.Contains(listOutput, "worktree "+s1.WorkDir()) {
		t.Errorf("expected exactly one worktree entry for %q (or its symlink-resolved form %q) in porcelain, got:\n%s", s1.WorkDir(), resolvedWorkDir, listOutput)
	}
}

// TestWorktreeSandbox_ContinueReturnsTargetedErrorOnUncontinuableState pins
// behavior 3 of issue #2189: when normalization succeeds but the preserved
// worktree is uncontinuable, continue-mode Start() must return a targeted
// error before running any destructive reconciliation. --override remains
// the only recovery mode that bypasses the early return.
func TestWorktreeSandbox_ContinueReturnsTargetedErrorOnUncontinuableState(t *testing.T) {
	cases := []struct {
		name          string
		setup         func(t *testing.T, dir, worktreePath string)
		wantErrSubstr string
	}{
		{
			name: "missing registration",
			setup: func(t *testing.T, dir, worktreePath string) {
				name := filepath.Base(worktreePath)
				if err := os.RemoveAll(filepath.Join(dir, ".git", "worktrees", name)); err != nil {
					t.Fatalf("remove registration: %v", err)
				}
			},
			wantErrSubstr: "no live registration",
		},
		{
			name: "detached HEAD",
			setup: func(t *testing.T, dir, worktreePath string) {
				runGit(t, worktreePath, "checkout", "--detach")
			},
			wantErrSubstr: "detached HEAD",
		},
		{
			name: "wrong branch",
			setup: func(t *testing.T, dir, worktreePath string) {
				runGit(t, worktreePath, "checkout", "-b", "sandman/other-branch")
			},
			wantErrSubstr: "is checked out on branch \"sandman/other-branch\"",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			initGitRepoWithRemote(t, dir)
			commitGitFile(t, dir, "tracked.txt", "base\n", "base")
			runGit(t, dir, "push", "origin", "main")

			worktreeBase := filepath.Join(dir, ".sandman", "worktrees")
			branch := "sandman/42-fix-bug"

			s1 := NewWorktreeSandbox(dir, worktreeBase, branch, "main")
			if err := s1.Start(SandboxStart{StrandedReconcile: true}); err != nil {
				t.Fatalf("create initial worktree: %v", err)
			}

			branchTipBefore := runGit(t, dir, "rev-parse", "refs/heads/"+branch)

			tc.setup(t, dir, s1.WorkDir())

			worktreeDirName := filepath.Base(s1.WorkDir())
			containerGitdir := "/workspace/.git/worktrees/" + worktreeDirName
			if err := os.WriteFile(filepath.Join(s1.WorkDir(), ".git"), []byte("gitdir: "+containerGitdir+"\n"), 0644); err != nil {
				t.Fatalf("rewrite .git to /workspace: %v", err)
			}

			s2 := NewWorktreeSandbox(dir, worktreeBase, branch, "main")
			err := s2.Start(SandboxStart{StrandedReconcile: true, Continue: true})
			if err == nil {
				t.Fatalf("expected targeted error for %s, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("expected error to contain %q, got %q", tc.wantErrSubstr, err.Error())
			}

			branchTipAfter := runGit(t, dir, "rev-parse", "refs/heads/"+branch)
			if branchTipAfter != branchTipBefore {
				t.Errorf("expected branch tip unchanged in %s case (%q), got %q", tc.name, branchTipBefore, branchTipAfter)
			}

			t.Cleanup(func() {
				s1.Stop()
				removeBranch(t, dir, branch)
				removeBranch(t, dir, "sandman/other-branch")
			})
		})
	}
}
