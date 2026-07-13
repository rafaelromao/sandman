package scaffold

import (
	"fmt"
	"os/exec"
	"strings"
)

// GitOps abstracts the small slice of git functionality Scaffolder needs
// during init: probe whether a directory is a git worktree, and untrack a
// path that should remain on disk but disappear from the index. Production
// code uses NewDefaultGitOps, which shells out to the real `git` binary.
type GitOps interface {
	IsRepo(repoRoot string) bool
	Untrack(repoRoot, path string) error
}

// NewDefaultGitOps returns the production GitOps backed by exec.Command.
func NewDefaultGitOps() GitOps {
	return &defaultGitOps{}
}

type defaultGitOps struct{}

func (g *defaultGitOps) IsRepo(repoRoot string) bool {
	cmd := exec.Command("git", "-C", repoRoot, "rev-parse", "--git-dir")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return len(strings.TrimSpace(string(out))) > 0
}

func (g *defaultGitOps) Untrack(repoRoot, path string) error {
	cmd := exec.Command("git", "-C", repoRoot, "rm", "--cached", "-r", "--ignore-unmatch", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git rm --cached %s: %s: %w", path, strings.TrimSpace(string(out)), err)
	}
	return nil
}
