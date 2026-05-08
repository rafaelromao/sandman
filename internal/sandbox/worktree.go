package sandbox

import (
	"context"
	"fmt"
)

// WorktreeSandbox provides isolation via git worktree only.
type WorktreeSandbox struct{}

// Start initializes the worktree.
func (s *WorktreeSandbox) Start() error {
	return fmt.Errorf("worktree sandbox not yet implemented")
}

// Exec runs a command in the worktree.
func (s *WorktreeSandbox) Exec(ctx context.Context, worktreePath string, command string) error {
	return fmt.Errorf("worktree sandbox not yet implemented")
}

// Stop cleans up the worktree.
func (s *WorktreeSandbox) Stop() error {
	return fmt.Errorf("worktree sandbox not yet implemented")
}

// Ensure WorktreeSandbox implements Sandbox.
var _ Sandbox = (*WorktreeSandbox)(nil)
