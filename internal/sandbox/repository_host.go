package sandbox

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/rafaelromao/sandman/internal/atomicfs"
)

// RepositoryHostSandbox executes repair work in the owning repository without
// preparing a worktree, changing git configuration, or requiring a container.
// Its process protocol is shared with host worktree execution.
type RepositoryHostSandbox struct {
	*WorktreeSandbox
	promptPath string
}

func NewRepositoryHostSandbox(repoPath, promptPath string) *RepositoryHostSandbox {
	return &RepositoryHostSandbox{
		WorktreeSandbox: &WorktreeSandbox{repoPath: repoPath, workDir: repoPath},
		promptPath:      promptPath,
	}
}

func (s *RepositoryHostSandbox) Start(SandboxStart) error {
	info, err := os.Stat(s.workDir)
	if err != nil {
		return fmt.Errorf("open repair repository: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("repair repository %q is not a directory", s.workDir)
	}
	return nil
}

func (s *RepositoryHostSandbox) WritePrompt(content string) error {
	if err := os.MkdirAll(filepath.Dir(s.promptPath), 0755); err != nil {
		return err
	}
	return atomicfs.WriteAtomic(s.promptPath, []byte(content), 0644)
}

// Stop never removes repository files or git refs. Exec and the lifecycle
// supervisor own process shutdown and wait for the sole process waiter.
func (s *RepositoryHostSandbox) Stop() error { return nil }

var _ Sandbox = (*RepositoryHostSandbox)(nil)
