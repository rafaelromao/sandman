package sandbox

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// StrandedWorktreeInfo describes a worktree that does not match its expected branch.
type StrandedWorktreeInfo struct {
	Path           string // absolute path to the worktree directory
	ActualBranch   string // the ref the worktree's HEAD actually points to (or "" if detached)
	ExpectedBranch string // the ref the directory name implies (e.g. "refs/heads/907-..." for a default-base worktree, or "refs/heads/<feature>/907-..." for a feature-branch-prefixed worktree)
}

// resolveBase resolves a worktreeBase path to an absolute, symlink-free form
// so path comparisons against `git worktree list --porcelain` output match on
// platforms where the working directory is reached through a symlink (notably
// macOS, where /tmp is a symlink to /private/tmp and `t.TempDir()` returns
// paths under /tmp).
func resolveBase(worktreeBase string) string {
	if resolved, err := filepath.EvalSymlinks(worktreeBase); err == nil {
		return resolved
	}
	return worktreeBase
}

// StrandedWorktree checks whether a worktree for the given branch exists under
// worktreeBase in a stranded state (HEAD does not match the expected ref).
// Returns the StrandedWorktreeInfo and true if stranded, zero value and false otherwise.
//
// worktreeBase is resolved against repoPath when it is a relative
// path, so callers can pass the configured WorktreeDir (typically
// `.sandman/worktrees`) without pre-resolving it to an absolute path.
func StrandedWorktree(repoPath, worktreeBase, branch string) (StrandedWorktreeInfo, bool) {
	if !filepath.IsAbs(worktreeBase) {
		worktreeBase = filepath.Join(repoPath, worktreeBase)
	}
	if _, err := os.Stat(worktreeBase); err != nil {
		return StrandedWorktreeInfo{}, false
	}
	worktreeBase = resolveBase(worktreeBase)

	expectedRef := "refs/heads/" + branch
	target := filepath.Join(worktreeBase, branch)

	entries, err := listWorktrees(repoPath)
	if err != nil {
		return StrandedWorktreeInfo{}, false
	}

	for _, e := range entries {
		if e.path != target {
			continue
		}
		if e.prunable {
			return StrandedWorktreeInfo{}, false
		}
		if e.detached || e.branch != expectedRef {
			return StrandedWorktreeInfo{
				Path:           e.path,
				ActualBranch:   e.branch,
				ExpectedBranch: expectedRef,
			}, true
		}
		return StrandedWorktreeInfo{}, false
	}
	return StrandedWorktreeInfo{}, false
}

// ForeignStrandedWorktree scans all registered worktrees for any live
// (non-prunable) worktree whose HEAD is on `branch` but whose path is NOT
// the canonical `<worktreeBase>/<branch>`. Such a worktree blocks
// `git branch -D <branch>` because the branch is checked out elsewhere, and
// `StrandedWorktree` cannot detect it because that helper only inspects the
// canonical path.
//
// Returns the foreign worktree's StrandedWorktreeInfo and true when one is
// found, zero value and false otherwise. worktreeBase is resolved against
// repoPath when relative, mirroring StrandedWorktree.
//
// Issue #2140: `sandman run --override` must be able to clear this state.
func ForeignStrandedWorktree(repoPath, worktreeBase, branch string) (StrandedWorktreeInfo, bool) {
	if !filepath.IsAbs(worktreeBase) {
		worktreeBase = filepath.Join(repoPath, worktreeBase)
	}
	if _, err := os.Stat(worktreeBase); err != nil {
		return StrandedWorktreeInfo{}, false
	}
	worktreeBase = resolveBase(worktreeBase)

	expectedRef := "refs/heads/" + branch
	target := filepath.Join(worktreeBase, branch)

	entries, err := listWorktrees(repoPath)
	if err != nil {
		return StrandedWorktreeInfo{}, false
	}

	for _, e := range entries {
		if e.detached || e.branch != expectedRef {
			continue
		}
		if e.prunable {
			continue
		}
		if e.path == target {
			continue
		}
		return StrandedWorktreeInfo{
			Path:           e.path,
			ActualBranch:   e.branch,
			ExpectedBranch: expectedRef,
		}, true
	}
	return StrandedWorktreeInfo{}, false
}

// ReclaimableWorktree checks whether a git-worktree-list entry exists at
// <worktreeBase>/<branch>, regardless of whether git considers it prunable.
// Unlike StrandedWorktree, this helper does not filter on the prunable field;
// it is used to detect worktree registrations that could be reattached after
// a slice-2 cleanup pass.
//
// worktreeBase is resolved against repoPath when it is a relative
// path, so callers can pass the configured WorktreeDir without pre-resolving it.
func ReclaimableWorktree(repoPath, worktreeBase, branch string) (StrandedWorktreeInfo, bool) {
	if !filepath.IsAbs(worktreeBase) {
		worktreeBase = filepath.Join(repoPath, worktreeBase)
	}
	if _, err := os.Stat(worktreeBase); err != nil {
		return StrandedWorktreeInfo{}, false
	}
	worktreeBase = resolveBase(worktreeBase)

	expectedRef := "refs/heads/" + branch
	target := filepath.Join(worktreeBase, branch)

	entries, err := listWorktrees(repoPath)
	if err != nil {
		return StrandedWorktreeInfo{}, false
	}

	for _, e := range entries {
		if e.path != target {
			continue
		}
		return StrandedWorktreeInfo{
			Path:           e.path,
			ActualBranch:   e.branch,
			ExpectedBranch: expectedRef,
		}, true
	}
	return StrandedWorktreeInfo{}, false
}

// ContinuationWorktreeState describes whether the preserved worktree at
// <worktreeBase>/<branch> is fit to be reused by a continue-mode run.
// Issue #2189: the host-side validation must distinguish a missing
// registration from a detached HEAD from a wrong-branch checkout so the
// orchestrator can return a targeted error instead of dropping into
// destructive reconciliation.
type ContinuationWorktreeState int

const (
	// ContinuationWorktreeMissing indicates no live worktree-list entry
	// exists at the canonical path. The worktree directory may still be
	// on disk; the registration's gitdir is absent.
	ContinuationWorktreeMissing ContinuationWorktreeState = iota
	// ContinuationWorktreeDetached indicates the registration exists but
	// the worktree's HEAD is detached (not on any local branch).
	ContinuationWorktreeDetached
	// ContinuationWorktreeWrongBranch indicates the registration exists
	// but HEAD points at a different ref than refs/heads/<branch>.
	ContinuationWorktreeWrongBranch
	// ContinuationWorktreeReusable indicates the registration exists and
	// HEAD points at refs/heads/<branch>; the continue-mode happy path
	// may proceed without destructive reconciliation.
	ContinuationWorktreeReusable
)

// ContinuationWorktreeStatus inspects the preserved worktree at
// <worktreeBase>/<branch> and reports whether a continue-mode Start()
// can reuse it. worktreeBase is resolved against repoPath when relative,
// mirroring ReclaimableWorktree / StrandedWorktree.
//
// Returns ContinuationWorktreeMissing when no worktree-list entry exists
// at the canonical path, the entry's detached flag is set, or the entry's
// branch differs from the expected refs/heads/<branch>. The string
// return value reports the actual branch ref when the worktree is in
// the wrong-branch state; it is empty for the other states.
func ContinuationWorktreeStatus(repoPath, worktreeBase, branch string) (ContinuationWorktreeState, string) {
	if !filepath.IsAbs(worktreeBase) {
		worktreeBase = filepath.Join(repoPath, worktreeBase)
	}
	if _, err := os.Stat(worktreeBase); err != nil {
		return ContinuationWorktreeMissing, ""
	}
	worktreeBase = resolveBase(worktreeBase)

	expectedRef := "refs/heads/" + branch
	target := filepath.Join(worktreeBase, branch)

	entries, err := listWorktrees(repoPath)
	if err != nil {
		return ContinuationWorktreeMissing, ""
	}

	for _, e := range entries {
		if e.path != target {
			continue
		}
		if e.prunable {
			return ContinuationWorktreeMissing, ""
		}
		if e.detached {
			return ContinuationWorktreeDetached, ""
		}
		if e.branch != expectedRef {
			return ContinuationWorktreeWrongBranch, e.branch
		}
		return ContinuationWorktreeReusable, ""
	}
	return ContinuationWorktreeMissing, ""
}

// issueDrivenDir matches directory names that come from issue-driven
// branches (e.g. "907-foo" or "feat/foo/907-bar" stored as the
// worktree directory name). The basename of a feature-branch-prefixed
// worktree directory is still "907-<slug>".
var issueDrivenDir = regexp.MustCompile(`^[0-9]+-`)

// ListStrandedWorktrees scans all worktrees under worktreeBase whose directory
// name matches the issue-driven branch pattern (N-slug) and returns those whose
// HEAD ref does not match the expected ref (refs/heads/<dirname>).
// Returns nil if worktreeBase is missing or git fails; callers that need error
// visibility on a `git worktree list` failure should call listWorktrees
// directly.
func ListStrandedWorktrees(repoPath, worktreeBase string) []StrandedWorktreeInfo {
	if _, err := os.Stat(worktreeBase); err != nil {
		return nil
	}
	worktreeBase = resolveBase(worktreeBase)

	prefix := worktreeBase
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}

	entries, err := listWorktrees(repoPath)
	if err != nil {
		return nil
	}

	var stranded []StrandedWorktreeInfo
	for _, e := range entries {
		if !strings.HasPrefix(e.path, prefix) {
			continue
		}
		if e.prunable {
			continue
		}
		dir := filepath.Base(e.path)
		if !issueDrivenDir.MatchString(dir) {
			continue
		}
		expectedRef := "refs/heads/" + dir
		if e.detached || e.branch != expectedRef {
			stranded = append(stranded, StrandedWorktreeInfo{
				Path:           e.path,
				ActualBranch:   e.branch,
				ExpectedBranch: expectedRef,
			})
		}
	}
	return stranded
}

type worktreeEntry struct {
	path     string
	branch   string
	detached bool
	prunable bool
}

func listWorktrees(repoPath string) ([]worktreeEntry, error) {
	cmd := exec.Command("git", "worktree", "list", "--porcelain")
	cmd.Dir = repoPath
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git worktree list: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	var entries []worktreeEntry
	var cur worktreeEntry
	flush := func() {
		if cur.path != "" {
			entries = append(entries, cur)
		}
		cur = worktreeEntry{}
	}

	scanner := bufio.NewScanner(&stdout)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			cur.path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch "):
			cur.branch = strings.TrimPrefix(line, "branch ")
		case line == "detached":
			cur.detached = true
		case strings.HasPrefix(line, "prunable "):
			cur.prunable = true
		}
	}
	flush()
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan worktree list: %w", err)
	}
	return entries, nil
}
