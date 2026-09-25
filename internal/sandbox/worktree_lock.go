package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

const gitWorktreeAdminLockName = "sandman-worktree-admin.lock"

// withGitWorktreeAdminLock serializes Sandman-managed Git worktree
// administration for a repository across concurrent runs and processes.
// Git creates each linked-worktree registration in several steps; another Git
// process can otherwise inspect the shared worktrees directory between those
// steps and encounter a partially written commondir file.
func withGitWorktreeAdminLock(repoPath string, operation func() error) error {
	commonDir := filepath.Join(repoPath, ".git")
	if info, err := os.Stat(commonDir); err != nil || !info.IsDir() {
		commonDirCmd := exec.Command("git", "rev-parse", "--git-common-dir")
		commonDirCmd.Dir = repoPath
		commonDirOutput, err := commonDirCmd.Output()
		if err != nil {
			return fmt.Errorf("resolve common Git directory for worktree lock: %w", err)
		}
		commonDir = strings.TrimSpace(string(commonDirOutput))
		if commonDir == "" {
			return fmt.Errorf("resolve common Git directory for worktree lock: empty path")
		}
		if !filepath.IsAbs(commonDir) {
			commonDir = filepath.Join(repoPath, commonDir)
		}
	}
	commonDir, err := filepath.Abs(commonDir)
	if err != nil {
		return fmt.Errorf("resolve common Git directory for worktree lock: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(commonDir); resolveErr == nil {
		commonDir = resolved
	}

	lock, err := os.OpenFile(filepath.Join(commonDir, gitWorktreeAdminLockName), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open worktree administration lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock Git worktree administration: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	return operation()
}
