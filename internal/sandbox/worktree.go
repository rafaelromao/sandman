package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// WorktreeSandbox provides isolation via git worktree only.
type WorktreeSandbox struct {
	repoPath     string
	worktreeBase string
	branch       string
	sourceBranch string
	workDir      string
}

// NewWorktreeSandbox creates a WorktreeSandbox for the given repo and branch.
func NewWorktreeSandbox(repoPath, worktreeBase, branch, sourceBranch string) *WorktreeSandbox {
	return &WorktreeSandbox{
		repoPath:     repoPath,
		worktreeBase: worktreeBase,
		branch:       branch,
		sourceBranch: sourceBranch,
	}
}

// Start initializes the worktree.
func (s *WorktreeSandbox) Start() error {
	if err := os.MkdirAll(s.worktreeBase, 0755); err != nil {
		return fmt.Errorf("create worktree base: %w", err)
	}

	s.workDir = filepath.Join(s.worktreeBase, s.branch)
	cmd := exec.Command("git", "worktree", "add", "-b", s.branch, s.workDir, s.sourceBranch)
	cmd.Dir = s.repoPath
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree add: %w\n%s", err, out)
	}
	return nil
}

// Exec runs a command in the worktree.
func (s *WorktreeSandbox) Exec(ctx context.Context, worktreePath string, command string) error {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = s.workDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	return nil
}

// Stop cleans up the worktree.
func (s *WorktreeSandbox) Stop() error {
	return fmt.Errorf("worktree sandbox stop not yet implemented")
}

// WorkDir returns the working directory path of the sandbox.
func (s *WorktreeSandbox) WorkDir() string {
	return s.workDir
}

// Ensure WorktreeSandbox implements Sandbox.
var _ Sandbox = (*WorktreeSandbox)(nil)
