package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStrandedWorktree_MissingBaseReturnsFalse(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	missingBase := filepath.Join(repoDir, "does-not-exist")

	info, stranded := StrandedWorktree(repoDir, missingBase, "sandman/907-foo")
	if stranded {
		t.Fatalf("expected false (no-op) when worktreeBase is missing, got info=%+v stranded=%v", info, stranded)
	}
	if info != (StrandedWorktreeInfo{}) {
		t.Fatalf("expected zero-value StrandedWorktreeInfo, got %+v", info)
	}
}

func TestStrandedWorktree_NoWorktreeAtPathReturnsFalse(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	worktreeBase := filepath.Join(repoDir, ".sandman", "worktrees")
	if err := os.MkdirAll(worktreeBase, 0755); err != nil {
		t.Fatalf("mkdir worktreeBase: %v", err)
	}

	info, stranded := StrandedWorktree(repoDir, worktreeBase, "sandman/907-foo")
	if stranded {
		t.Fatalf("expected false when no worktree exists at <worktreeBase>/<branch>, got info=%+v", info)
	}
	if info != (StrandedWorktreeInfo{}) {
		t.Fatalf("expected zero-value StrandedWorktreeInfo, got %+v", info)
	}
}

func TestStrandedWorktree_CleanWorktreeOnExpectedBranchReturnsFalse(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	const branch = "sandman/907-foo"
	runGit(t, repoDir, "branch", branch)

	worktreeBase := filepath.Join(repoDir, ".sandman", "worktrees")
	if err := os.MkdirAll(worktreeBase, 0755); err != nil {
		t.Fatalf("mkdir worktreeBase: %v", err)
	}
	wtPath := filepath.Join(worktreeBase, branch)
	runGit(t, repoDir, "worktree", "add", wtPath, branch)

	info, stranded := StrandedWorktree(repoDir, worktreeBase, branch)
	if stranded {
		t.Fatalf("expected false when worktree is on expected branch, got info=%+v", info)
	}
	if info != (StrandedWorktreeInfo{}) {
		t.Fatalf("expected zero-value StrandedWorktreeInfo, got %+v", info)
	}
}

func TestStrandedWorktree_MismatchedBranchReturnsTrue(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	const expected = "sandman/907-foo"
	const actual = "sandman/42-other-branch"
	runGit(t, repoDir, "branch", expected)
	runGit(t, repoDir, "branch", actual)

	worktreeBase := filepath.Join(repoDir, ".sandman", "worktrees")
	if err := os.MkdirAll(worktreeBase, 0755); err != nil {
		t.Fatalf("mkdir worktreeBase: %v", err)
	}
	wtPath := filepath.Join(worktreeBase, expected)
	runGit(t, repoDir, "worktree", "add", "--force", wtPath, actual)

	info, stranded := StrandedWorktree(repoDir, worktreeBase, expected)
	if !stranded {
		t.Fatalf("expected true when worktree HEAD is on a different branch, got info=%+v", info)
	}
	if info.Path != wtPath {
		t.Errorf("Path: got %q, want %q", info.Path, wtPath)
	}
	if info.ActualBranch != "refs/heads/"+actual {
		t.Errorf("ActualBranch: got %q, want %q", info.ActualBranch, "refs/heads/"+actual)
	}
	if info.ExpectedBranch != "refs/heads/"+expected {
		t.Errorf("ExpectedBranch: got %q, want %q", info.ExpectedBranch, "refs/heads/"+expected)
	}
}

func TestStrandedWorktree_DetachedHeadReturnsTrue(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	const branch = "sandman/77-detached"
	runGit(t, repoDir, "branch", branch)

	worktreeBase := filepath.Join(repoDir, ".sandman", "worktrees")
	if err := os.MkdirAll(worktreeBase, 0755); err != nil {
		t.Fatalf("mkdir worktreeBase: %v", err)
	}
	wtPath := filepath.Join(worktreeBase, branch)
	runGit(t, repoDir, "worktree", "add", wtPath, branch)
	runGit(t, wtPath, "checkout", "--detach", "HEAD")

	info, stranded := StrandedWorktree(repoDir, worktreeBase, branch)
	if !stranded {
		t.Fatalf("expected true when worktree is in detached HEAD state, got info=%+v", info)
	}
	if info.Path != wtPath {
		t.Errorf("Path: got %q, want %q", info.Path, wtPath)
	}
	if info.ActualBranch != "" {
		t.Errorf("ActualBranch: got %q, want empty (detached)", info.ActualBranch)
	}
	if info.ExpectedBranch != "refs/heads/"+branch {
		t.Errorf("ExpectedBranch: got %q, want %q", info.ExpectedBranch, "refs/heads/"+branch)
	}
}

func TestStrandedWorktree_ExpectedRefMissingLocallyReturnsTrue(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	const expected = "sandman/907-foo"
	const actual = "sandman/42-other-branch"
	runGit(t, repoDir, "branch", actual)
	if BranchExists(repoDir, expected) {
		t.Fatalf("precondition: expected ref %q should not exist locally", expected)
	}

	worktreeBase := filepath.Join(repoDir, ".sandman", "worktrees")
	if err := os.MkdirAll(worktreeBase, 0755); err != nil {
		t.Fatalf("mkdir worktreeBase: %v", err)
	}
	wtPath := filepath.Join(worktreeBase, expected)
	runGit(t, repoDir, "worktree", "add", "--force", wtPath, actual)

	info, stranded := StrandedWorktree(repoDir, worktreeBase, expected)
	if !stranded {
		t.Fatalf("expected true when worktree is stranded and expected ref is missing, got info=%+v", info)
	}
	if info.Path != wtPath {
		t.Errorf("Path: got %q, want %q", info.Path, wtPath)
	}
	if info.ActualBranch != "refs/heads/"+actual {
		t.Errorf("ActualBranch: got %q, want %q", info.ActualBranch, "refs/heads/"+actual)
	}
	if info.ExpectedBranch != "refs/heads/"+expected {
		t.Errorf("ExpectedBranch: got %q, want %q", info.ExpectedBranch, "refs/heads/"+expected)
	}
	if BranchExists(repoDir, expected) {
		t.Errorf("postcondition: expected ref %q should still be missing locally", expected)
	}
}

func TestStrandedWorktree_IgnoresSiblingWorktrees(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	const healthy = "sandman/1-healthy"
	const stranded = "sandman/2-bad"
	runGit(t, repoDir, "branch", healthy)
	runGit(t, repoDir, "branch", stranded)

	worktreeBase := filepath.Join(repoDir, ".sandman", "worktrees")
	if err := os.MkdirAll(worktreeBase, 0755); err != nil {
		t.Fatalf("mkdir worktreeBase: %v", err)
	}
	runGit(t, repoDir, "worktree", "add", filepath.Join(worktreeBase, healthy), healthy)
	runGit(t, repoDir, "worktree", "add", "--force", filepath.Join(worktreeBase, stranded), healthy)

	info, ok := StrandedWorktree(repoDir, worktreeBase, stranded)
	if !ok {
		t.Fatalf("expected stranded=true for %q, got false (info=%+v)", stranded, info)
	}
	if info.Path != filepath.Join(worktreeBase, stranded) {
		t.Errorf("Path: got %q, want %q", info.Path, filepath.Join(worktreeBase, stranded))
	}
	if info.ActualBranch != "refs/heads/"+healthy {
		t.Errorf("ActualBranch: got %q, want %q", info.ActualBranch, "refs/heads/"+healthy)
	}

	cleanInfo, cleanOK := StrandedWorktree(repoDir, worktreeBase, healthy)
	if cleanOK {
		t.Fatalf("expected stranded=false for healthy %q, got info=%+v", healthy, cleanInfo)
	}
	if cleanInfo != (StrandedWorktreeInfo{}) {
		t.Errorf("expected zero-value info for healthy worktree, got %+v", cleanInfo)
	}
}

func TestListStrandedWorktrees(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	runGit(t, repoDir, "branch", "sandman/1-healthy")
	runGit(t, repoDir, "branch", "sandman/2-wrong")
	runGit(t, repoDir, "branch", "sandman/3-detached")

	worktreeBase := filepath.Join(repoDir, ".sandman", "worktrees")
	if err := os.MkdirAll(worktreeBase, 0755); err != nil {
		t.Fatalf("mkdir worktreeBase: %v", err)
	}

	runGit(t, repoDir, "branch", "no-issue-branch")
	runGit(t, repoDir, "worktree", "add", filepath.Join(worktreeBase, "sandman/1-healthy"), "sandman/1-healthy")
	runGit(t, repoDir, "worktree", "add", "--force", filepath.Join(worktreeBase, "sandman/2-wrong"), "sandman/1-healthy")
	runGit(t, repoDir, "worktree", "add", filepath.Join(worktreeBase, "sandman/3-detached"), "sandman/3-detached")
	runGit(t, filepath.Join(worktreeBase, "sandman/3-detached"), "checkout", "--detach", "HEAD")
	runGit(t, repoDir, "worktree", "add", filepath.Join(worktreeBase, "no-issue-prefix"), "no-issue-branch")

	results := ListStrandedWorktrees(repoDir, worktreeBase)
	if len(results) != 2 {
		var names []string
		for _, r := range results {
			names = append(names, r.Path)
		}
		t.Fatalf("expected 2 stranded worktrees (sandman/2-wrong, sandman/3-detached), got %d: %v", len(results), names)
	}

	byName := map[string]StrandedWorktreeInfo{}
	for _, r := range results {
		byName[r.ExpectedBranch] = r
	}

	wrong, ok := byName["refs/heads/sandman/2-wrong"]
	if !ok {
		t.Fatalf("expected refs/heads/sandman/2-wrong in results, got %+v", results)
	}
	if wrong.ActualBranch != "refs/heads/sandman/1-healthy" {
		t.Errorf("sandman/2-wrong ActualBranch: got %q, want %q", wrong.ActualBranch, "refs/heads/sandman/1-healthy")
	}
	if wrong.ExpectedBranch != "refs/heads/sandman/2-wrong" {
		t.Errorf("sandman/2-wrong ExpectedBranch: got %q, want %q", wrong.ExpectedBranch, "refs/heads/sandman/2-wrong")
	}

	detached, ok := byName["refs/heads/sandman/3-detached"]
	if !ok {
		t.Fatalf("expected refs/heads/sandman/3-detached in results, got %+v", results)
	}
	if detached.ActualBranch != "" {
		t.Errorf("sandman/3-detached ActualBranch: got %q, want empty (detached)", detached.ActualBranch)
	}
	if detached.ExpectedBranch != "refs/heads/sandman/3-detached" {
		t.Errorf("sandman/3-detached ExpectedBranch: got %q, want %q", detached.ExpectedBranch, "refs/heads/sandman/3-detached")
	}
}

func TestListStrandedWorktrees_MissingBaseReturnsEmpty(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	missingBase := filepath.Join(repoDir, "does-not-exist")
	results := ListStrandedWorktrees(repoDir, missingBase)
	if len(results) != 0 {
		t.Fatalf("expected empty results when worktreeBase is missing, got %+v", results)
	}
}

func TestStrandedWorktree_PrunableWorktreeIsNotFlagged(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	const branch = "sandman/77-prunable"
	runGit(t, repoDir, "branch", branch)

	worktreeBase := filepath.Join(repoDir, ".sandman", "worktrees")
	if err := os.MkdirAll(worktreeBase, 0755); err != nil {
		t.Fatalf("mkdir worktreeBase: %v", err)
	}
	wtPath := filepath.Join(worktreeBase, branch)
	runGit(t, repoDir, "worktree", "add", wtPath, branch)

	// Manually remove the worktree directory without telling git; the
	// entry becomes "prunable" in `git worktree list --porcelain`.
	if err := os.RemoveAll(wtPath); err != nil {
		t.Fatalf("remove wtPath: %v", err)
	}

	info, stranded := StrandedWorktree(repoDir, worktreeBase, branch)
	if stranded {
		t.Fatalf("expected prunable worktree to be skipped, got info=%+v", info)
	}
	if info != (StrandedWorktreeInfo{}) {
		t.Errorf("expected zero-value info, got %+v", info)
	}

	results := ListStrandedWorktrees(repoDir, worktreeBase)
	if len(results) != 0 {
		t.Fatalf("expected prunable worktree to be excluded from ListStrandedWorktrees, got %+v", results)
	}
}
