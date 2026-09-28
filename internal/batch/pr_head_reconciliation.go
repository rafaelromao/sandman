package batch

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/rafaelromao/sandman/internal/github"
)

const gatePRHeadChanged = "pull-request-head-changed"

func (s *runSession) livePRHeadForLifecycle(ctx context.Context, workDir, branch string, pr *github.PR, currentHead string) (string, string, error) {
	if pr == nil {
		return currentHead, currentHead, nil
	}
	prHead := strings.TrimSpace(pr.HeadRefOid)
	if prHead == "" {
		return currentHead, currentHead, nil
	}
	if s.opts.currentHead != nil {
		// Tests may inject a symbolic head without constructing a Git worktree.
		// The injected value remains the test's local-head authority; without a
		// real Git worktree, a mismatch must stay a stale-head pending decision.
		if strings.TrimSpace(currentHead) == "" || !strings.EqualFold(currentHead, prHead) {
			return currentHead, currentHead, nil
		}
		return prHead, currentHead, nil
	}
	localHead, err := currentBranchHead(workDir)
	if err != nil {
		return "", "", nil
	}
	if strings.EqualFold(localHead, prHead) {
		return prHead, localHead, nil
	}
	updatedHead, _, err := reconcileWorktreeToPRHead(ctx, workDir, branch, pr.Number, prHead)
	if err != nil {
		return prHead, localHead, err
	}
	return prHead, updatedHead, nil
}

// reconcileWorktreeToPRHead advances a clean issue worktree to the exact live
// PR head. It refuses branch changes, dirty trees, non-matching remote heads,
// and non-fast-forward updates so a lifecycle re-entry never overwrites agent
// work or silently switches to a different revision.
func reconcileWorktreeToPRHead(ctx context.Context, workDir, branch string, prNumber int, prHead string) (string, bool, error) {
	workDir = strings.TrimSpace(workDir)
	branch = strings.TrimSpace(branch)
	prHead = strings.TrimSpace(prHead)
	if workDir == "" || branch == "" || prNumber <= 0 || !isGitObjectID(prHead) {
		return "", false, fmt.Errorf("pull-request head reconciliation identity is incomplete")
	}

	currentRef, err := runGitAt(ctx, workDir, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return "", false, fmt.Errorf("inspect worktree branch: %w", err)
	}
	expectedRef := "refs/heads/" + branch
	if currentRef != expectedRef {
		return "", false, fmt.Errorf("worktree branch is %q, want %q", strings.TrimPrefix(currentRef, "refs/heads/"), branch)
	}
	currentHead, err := currentBranchHead(workDir)
	if err != nil {
		return "", false, fmt.Errorf("inspect worktree head: %w", err)
	}
	if strings.EqualFold(currentHead, prHead) {
		return currentHead, false, nil
	}

	status, err := runGitAt(ctx, workDir, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return currentHead, false, fmt.Errorf("inspect worktree changes: %w", err)
	}
	if status != "" {
		return currentHead, false, fmt.Errorf("worktree has uncommitted changes")
	}

	temporaryRef := fmt.Sprintf("refs/sandman/pr-head-reconcile/%d/%s", prNumber, prHead)
	refspec := "+refs/heads/" + branch + ":" + temporaryRef
	if _, err := runGitAt(ctx, workDir, "fetch", "--no-tags", "origin", refspec); err != nil {
		return currentHead, false, fmt.Errorf("fetch pull-request branch: %w", err)
	}
	defer func() {
		_, _ = runGitAt(context.Background(), workDir, "update-ref", "-d", temporaryRef)
	}()

	fetchedHead, err := runGitAt(ctx, workDir, "rev-parse", "--verify", temporaryRef+"^{commit}")
	if err != nil {
		return currentHead, false, fmt.Errorf("verify fetched pull-request head: %w", err)
	}
	if !strings.EqualFold(fetchedHead, prHead) {
		return currentHead, false, fmt.Errorf("fetched branch head %s does not match live pull-request head %s", fetchedHead, prHead)
	}
	if _, err := runGitAt(ctx, workDir, "merge", "--ff-only", "--no-edit", prHead); err != nil {
		return currentHead, false, fmt.Errorf("fast-forward worktree to pull-request head: %w", err)
	}
	updatedHead, err := currentBranchHead(workDir)
	if err != nil {
		return currentHead, false, fmt.Errorf("verify reconciled worktree head: %w", err)
	}
	if !strings.EqualFold(updatedHead, prHead) {
		return updatedHead, false, fmt.Errorf("worktree head %s does not match live pull-request head %s after fast-forward", updatedHead, prHead)
	}
	return updatedHead, true, nil
}

func isGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
			return false
		}
	}
	return true
}

func runGitAt(ctx context.Context, workDir string, args ...string) (string, error) {
	cmdArgs := append([]string{"-C", workDir}, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}
