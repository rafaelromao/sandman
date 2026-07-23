package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// addStrandedWorktree registers a worktree at <worktreeBase>/<expectedBranch>
// whose HEAD points at <otherBranch>, so the detector sees it as stranded.
// It avoids `git worktree add --force <path> <branch>`, which fails on macOS
// git when <branch> is already checked out elsewhere. Instead, it uses
// `git worktree add --detach` (allowed on both Linux and macOS even when the
// branch is checked out elsewhere) and then rewrites the worktree's HEAD to
// <otherBranch> via `git symbolic-ref HEAD`. The helper does not create
// <expectedBranch> (callers decide — needed for the "missing locally"
// precondition); <otherBranch> must already exist.
//
// The returned path is symlink-resolved so it matches what `git worktree list
// --porcelain` reports (necessary on macOS where /tmp is a symlink to
// /private/tmp). See #1738.
func addStrandedWorktree(t *testing.T, repoDir, worktreeBase, expectedBranch, otherBranch string) string {
	t.Helper()
	wtPath := filepath.Join(worktreeBase, expectedBranch)
	runGit(t, repoDir, "worktree", "add", "--detach", wtPath, otherBranch)
	runGit(t, wtPath, "symbolic-ref", "HEAD", "refs/heads/"+otherBranch)
	if resolved, err := filepath.EvalSymlinks(wtPath); err == nil {
		return resolved
	}
	return wtPath
}

// resolveWorktreePath resolves a path the same way `git worktree list
// --porcelain` reports it (symlinks resolved). Tests use this when comparing
// info.Path against an expected worktree path, so the comparison matches on
// macOS where /tmp is a symlink to /private/tmp. See #1738.
func resolveWorktreePath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

func TestStrandedWorktree_MissingBaseReturnsFalse(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	missingBase := filepath.Join(repoDir, "does-not-exist")

	info, stranded := StrandedWorktree(repoDir, missingBase, "907-foo")
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

	info, stranded := StrandedWorktree(repoDir, worktreeBase, "907-foo")
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

	const branch = "907-foo"
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

	const expected = "907-foo"
	const actual = "42-other-branch"
	runGit(t, repoDir, "branch", expected)
	runGit(t, repoDir, "branch", actual)

	worktreeBase := filepath.Join(repoDir, ".sandman", "worktrees")
	if err := os.MkdirAll(worktreeBase, 0755); err != nil {
		t.Fatalf("mkdir worktreeBase: %v", err)
	}
	wtPath := addStrandedWorktree(t, repoDir, worktreeBase, expected, actual)

	info, stranded := StrandedWorktree(repoDir, worktreeBase, expected)
	if !stranded {
		t.Fatalf("expected true when worktree HEAD is on a different branch, got info=%+v", info)
	}
	if info.Path != resolveWorktreePath(wtPath) {
		t.Errorf("Path: got %q, want %q", info.Path, resolveWorktreePath(wtPath))
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

	const branch = "77-detached"
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
	if info.Path != resolveWorktreePath(wtPath) {
		t.Errorf("Path: got %q, want %q", info.Path, resolveWorktreePath(wtPath))
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

	const expected = "907-foo"
	const actual = "42-other-branch"
	runGit(t, repoDir, "branch", actual)
	if BranchExists(repoDir, expected) {
		t.Fatalf("precondition: expected ref %q should not exist locally", expected)
	}

	worktreeBase := filepath.Join(repoDir, ".sandman", "worktrees")
	if err := os.MkdirAll(worktreeBase, 0755); err != nil {
		t.Fatalf("mkdir worktreeBase: %v", err)
	}
	wtPath := addStrandedWorktree(t, repoDir, worktreeBase, expected, actual)

	info, stranded := StrandedWorktree(repoDir, worktreeBase, expected)
	if !stranded {
		t.Fatalf("expected true when worktree is stranded and expected ref is missing, got info=%+v", info)
	}
	if info.Path != resolveWorktreePath(wtPath) {
		t.Errorf("Path: got %q, want %q", info.Path, resolveWorktreePath(wtPath))
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

	const healthy = "1-healthy"
	const stranded = "2-bad"
	runGit(t, repoDir, "branch", healthy)
	runGit(t, repoDir, "branch", stranded)

	worktreeBase := filepath.Join(repoDir, ".sandman", "worktrees")
	if err := os.MkdirAll(worktreeBase, 0755); err != nil {
		t.Fatalf("mkdir worktreeBase: %v", err)
	}
	runGit(t, repoDir, "worktree", "add", filepath.Join(worktreeBase, healthy), healthy)
	addStrandedWorktree(t, repoDir, worktreeBase, stranded, healthy)

	info, ok := StrandedWorktree(repoDir, worktreeBase, stranded)
	if !ok {
		t.Fatalf("expected stranded=true for %q, got false (info=%+v)", stranded, info)
	}
	if info.Path != resolveWorktreePath(filepath.Join(worktreeBase, stranded)) {
		t.Errorf("Path: got %q, want %q", info.Path, resolveWorktreePath(filepath.Join(worktreeBase, stranded)))
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

	runGit(t, repoDir, "branch", "1-healthy")
	runGit(t, repoDir, "branch", "2-wrong")
	runGit(t, repoDir, "branch", "3-detached")

	worktreeBase := filepath.Join(repoDir, ".sandman", "worktrees")
	if err := os.MkdirAll(worktreeBase, 0755); err != nil {
		t.Fatalf("mkdir worktreeBase: %v", err)
	}

	runGit(t, repoDir, "branch", "no-issue-branch")
	runGit(t, repoDir, "worktree", "add", filepath.Join(worktreeBase, "1-healthy"), "1-healthy")
	addStrandedWorktree(t, repoDir, worktreeBase, "2-wrong", "1-healthy")
	runGit(t, repoDir, "worktree", "add", filepath.Join(worktreeBase, "3-detached"), "3-detached")
	runGit(t, filepath.Join(worktreeBase, "3-detached"), "checkout", "--detach", "HEAD")
	runGit(t, repoDir, "worktree", "add", filepath.Join(worktreeBase, "no-issue-prefix"), "no-issue-branch")

	results := ListStrandedWorktrees(repoDir, worktreeBase)
	if len(results) != 2 {
		var names []string
		for _, r := range results {
			names = append(names, r.Path)
		}
		t.Fatalf("expected 2 stranded worktrees (2-wrong, 3-detached), got %d: %v", len(results), names)
	}

	byName := map[string]StrandedWorktreeInfo{}
	for _, r := range results {
		byName[r.ExpectedBranch] = r
	}

	wrong, ok := byName["refs/heads/2-wrong"]
	if !ok {
		t.Fatalf("expected refs/heads/2-wrong in results, got %+v", results)
	}
	if wrong.ActualBranch != "refs/heads/1-healthy" {
		t.Errorf("2-wrong ActualBranch: got %q, want %q", wrong.ActualBranch, "refs/heads/1-healthy")
	}
	if wrong.ExpectedBranch != "refs/heads/2-wrong" {
		t.Errorf("2-wrong ExpectedBranch: got %q, want %q", wrong.ExpectedBranch, "refs/heads/2-wrong")
	}

	detached, ok := byName["refs/heads/3-detached"]
	if !ok {
		t.Fatalf("expected refs/heads/3-detached in results, got %+v", results)
	}
	if detached.ActualBranch != "" {
		t.Errorf("3-detached ActualBranch: got %q, want empty (detached)", detached.ActualBranch)
	}
	if detached.ExpectedBranch != "refs/heads/3-detached" {
		t.Errorf("3-detached ExpectedBranch: got %q, want %q", detached.ExpectedBranch, "refs/heads/3-detached")
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

	const branch = "77-prunable"
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

func TestReclaimableWorktree_PrunableWorktreeAtPathReturnsTrue(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	const branch = "77-prunable"
	runGit(t, repoDir, "branch", branch)

	worktreeBase := filepath.Join(repoDir, ".sandman", "worktrees")
	if err := os.MkdirAll(worktreeBase, 0755); err != nil {
		t.Fatalf("mkdir worktreeBase: %v", err)
	}
	wtPath := filepath.Join(worktreeBase, branch)
	runGit(t, repoDir, "worktree", "add", wtPath, branch)
	wantPath := resolveWorktreePath(wtPath)

	if err := os.RemoveAll(wtPath); err != nil {
		t.Fatalf("remove wtPath: %v", err)
	}

	info, reclaimable := ReclaimableWorktree(repoDir, worktreeBase, branch)
	if !reclaimable {
		t.Fatalf("expected reclaimable=true for prunable worktree at path, got info=%+v", info)
	}
	if info.Path != wantPath {
		t.Errorf("Path: got %q, want %q", info.Path, wantPath)
	}
	if info.ExpectedBranch != "refs/heads/"+branch {
		t.Errorf("ExpectedBranch: got %q, want %q", info.ExpectedBranch, "refs/heads/"+branch)
	}
}

func TestReclaimableWorktree_NoWorktreeAtPathReturnsFalse(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	worktreeBase := filepath.Join(repoDir, ".sandman", "worktrees")
	if err := os.MkdirAll(worktreeBase, 0755); err != nil {
		t.Fatalf("mkdir worktreeBase: %v", err)
	}

	info, reclaimable := ReclaimableWorktree(repoDir, worktreeBase, "907-foo")
	if reclaimable {
		t.Fatalf("expected reclaimable=false when no worktree exists at path, got info=%+v", info)
	}
	if info != (StrandedWorktreeInfo{}) {
		t.Fatalf("expected zero-value StrandedWorktreeInfo, got %+v", info)
	}
}

func TestReclaimableWorktree_MissingBaseReturnsFalse(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	missingBase := filepath.Join(repoDir, "does-not-exist")

	info, reclaimable := ReclaimableWorktree(repoDir, missingBase, "907-foo")
	if reclaimable {
		t.Fatalf("expected false (no-op) when worktreeBase is missing, got info=%+v", info)
	}
	if info != (StrandedWorktreeInfo{}) {
		t.Fatalf("expected zero-value StrandedWorktreeInfo, got %+v", info)
	}
}

func TestReclaimableWorktree_NonPrunableWorktreeAtPathReturnsTrue(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	const branch = "77-healthy"
	runGit(t, repoDir, "branch", branch)

	worktreeBase := filepath.Join(repoDir, ".sandman", "worktrees")
	if err := os.MkdirAll(worktreeBase, 0755); err != nil {
		t.Fatalf("mkdir worktreeBase: %v", err)
	}
	wtPath := filepath.Join(worktreeBase, branch)
	runGit(t, repoDir, "worktree", "add", wtPath, branch)

	info, reclaimable := ReclaimableWorktree(repoDir, worktreeBase, branch)
	if !reclaimable {
		t.Fatalf("expected reclaimable=true for non-prunable worktree at path, got info=%+v", info)
	}
	if info.Path != resolveWorktreePath(wtPath) {
		t.Errorf("Path: got %q, want %q", info.Path, resolveWorktreePath(wtPath))
	}
	if info.ExpectedBranch != "refs/heads/"+branch {
		t.Errorf("ExpectedBranch: got %q, want %q", info.ExpectedBranch, "refs/heads/"+branch)
	}
}

// TestForeignStrandedWorktree_DetectsBranchHeldElsewhere pins the
// regression guard for issue #2140: a live (non-prunable) worktree at
// a DIFFERENT path than the canonical <worktreeBase>/<branch> that
// currently has `branch` checked out must be detected so the override
// path can free it before retrying `git branch -D`.
func TestForeignStrandedWorktree_DetectsBranchHeldElsewhere(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	const branch = "30-foo"
	runGit(t, repoDir, "branch", branch)

	worktreeBase := filepath.Join(repoDir, ".sandman", "worktrees")
	if err := os.MkdirAll(worktreeBase, 0755); err != nil {
		t.Fatalf("mkdir worktreeBase: %v", err)
	}
	// Foreign worktree at a path that does NOT match worktreeBase/branch.
	foreignPath := filepath.Join(worktreeBase, "review-148-foreign")
	runGit(t, repoDir, "worktree", "add", foreignPath, branch)

	info, ok := ForeignStrandedWorktree(repoDir, worktreeBase, branch)
	if !ok {
		t.Fatalf("expected foreign stranded=true for branch %q held at %q, got false (info=%+v)", branch, foreignPath, info)
	}
	if info.Path != resolveWorktreePath(foreignPath) {
		t.Errorf("Path: got %q, want %q", info.Path, resolveWorktreePath(foreignPath))
	}
	if info.ActualBranch != "refs/heads/"+branch {
		t.Errorf("ActualBranch: got %q, want %q", info.ActualBranch, "refs/heads/"+branch)
	}
	if info.ExpectedBranch != "refs/heads/"+branch {
		t.Errorf("ExpectedBranch: got %q, want %q", info.ExpectedBranch, "refs/heads/"+branch)
	}
}

// TestForeignStrandedWorktree_IgnoresWorktreeAtExpectedPath pins that
// a worktree registered at the canonical <worktreeBase>/<branch> path
// is NOT flagged as foreign (it's the expected location, handled by
// StrandedWorktree instead).
func TestForeignStrandedWorktree_IgnoresWorktreeAtExpectedPath(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	const branch = "30-foo"
	runGit(t, repoDir, "branch", branch)

	worktreeBase := filepath.Join(repoDir, ".sandman", "worktrees")
	if err := os.MkdirAll(worktreeBase, 0755); err != nil {
		t.Fatalf("mkdir worktreeBase: %v", err)
	}
	wtPath := filepath.Join(worktreeBase, branch)
	runGit(t, repoDir, "worktree", "add", wtPath, branch)

	info, ok := ForeignStrandedWorktree(repoDir, worktreeBase, branch)
	if ok {
		t.Fatalf("expected foreign=false for worktree at canonical path, got info=%+v", info)
	}
	if info != (StrandedWorktreeInfo{}) {
		t.Fatalf("expected zero-value StrandedWorktreeInfo, got %+v", info)
	}
}

// TestForeignStrandedWorktree_IgnoresPrunableWorktree pins that prunable
// worktrees are skipped — they will be cleaned up by `git worktree prune`,
// not by the foreign-detach recovery path.
func TestForeignStrandedWorktree_IgnoresPrunableWorktree(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	const branch = "30-foo"
	runGit(t, repoDir, "branch", branch)

	worktreeBase := filepath.Join(repoDir, ".sandman", "worktrees")
	if err := os.MkdirAll(worktreeBase, 0755); err != nil {
		t.Fatalf("mkdir worktreeBase: %v", err)
	}
	foreignPath := filepath.Join(worktreeBase, "review-148-prunable")
	runGit(t, repoDir, "worktree", "add", foreignPath, branch)
	// Delete the directory so the worktree becomes prunable.
	if err := os.RemoveAll(foreignPath); err != nil {
		t.Fatalf("remove foreign worktree dir: %v", err)
	}

	info, ok := ForeignStrandedWorktree(repoDir, worktreeBase, branch)
	if ok {
		t.Fatalf("expected foreign=false for prunable worktree, got info=%+v", info)
	}
	if info != (StrandedWorktreeInfo{}) {
		t.Fatalf("expected zero-value StrandedWorktreeInfo, got %+v", info)
	}
}

// TestForeignStrandedWorktree_MissingBaseReturnsFalse pins the no-op
// behavior when worktreeBase itself is missing.
func TestForeignStrandedWorktree_MissingBaseReturnsFalse(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	missingBase := filepath.Join(repoDir, "does-not-exist")
	info, ok := ForeignStrandedWorktree(repoDir, missingBase, "30-foo")
	if ok {
		t.Fatalf("expected foreign=false when worktreeBase is missing, got info=%+v", info)
	}
	if info != (StrandedWorktreeInfo{}) {
		t.Fatalf("expected zero-value StrandedWorktreeInfo, got %+v", info)
	}
}
