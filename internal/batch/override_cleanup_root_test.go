package batch

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// Override cleanup started from a repository subdirectory retires a prunable
// worktree's registration in the repository it belongs to. Git's own
// removal prunes it today; the path-scoped fallback is rooted at the same
// repository so it cannot look beneath the process directory instead.
func TestClearIssueArtifacts_FromASubdirectoryRemovesThePrunableRegistration(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root)
	const branch = "42-fix"
	worktreeDir := filepath.Join(root, ".sandman", "worktrees")
	if err := os.MkdirAll(worktreeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wtPath := filepath.Join(worktreeDir, branch)
	runGit(t, root, "worktree", "add", "-b", branch, wtPath)
	if err := os.RemoveAll(wtPath); err != nil {
		t.Fatal(err)
	}
	registrations, err := filepath.Glob(filepath.Join(root, ".git", "worktrees", "*"))
	if err != nil || len(registrations) != 1 {
		t.Fatalf("registrations = %v (err %v), want the prunable worktree", registrations, err)
	}
	subdir := filepath.Join(root, "nested")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(subdir)

	reconcile := true
	ClearIssueArtifacts(42, branch, worktreeDir, &spyEventLog{}, &bytes.Buffer{}, "main", &reconcile, "", root)

	if _, err := os.Stat(registrations[0]); !os.IsNotExist(err) {
		t.Fatalf("worktree registration %s err = %v, want it removed", registrations[0], err)
	}
}
