package review

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
)

// ClearReviewArtifacts removes the review worktree directory and branch
// created by RunBatch for a single review launch. It is the review
// daemon's counterpart to batch.ClearIssueArtifacts: same idempotency
// guarantees (missing artifacts do not error), same log-and-continue
// contract (best-effort cleanup never blocks the daemon), but with a
// narrower scope — review runs do not touch the event log, the batches
// index, or the per-run batch directory under .sandman/batches/.
//
// The branch is computed by reviewBranchName(pr, commentID) and the
// worktree path is <worktreeDir>/<branch>, matching the layout
// created by WorktreeSandbox.Start. Empty inputs are treated as a
// no-op so callers can always invoke this unconditionally from a
// defer without an empty-config check.
func ClearReviewArtifacts(branch, worktreeDir string, logWriter io.Writer) {
	if strings.TrimSpace(branch) == "" || strings.TrimSpace(worktreeDir) == "" {
		return
	}
	wtPath := filepath.Join(worktreeDir, branch)

	if out, err := exec.Command("git", "worktree", "remove", "--force", wtPath).CombinedOutput(); err != nil && !isMissingWorktreeErr(out) {
		fmt.Fprintf(logWriter, "error: remove review worktree %s: %v: %s\n", wtPath, err, out)
	}
	if out, err := exec.Command("git", "branch", "-D", branch).CombinedOutput(); err != nil && !isMissingBranchErr(out) {
		fmt.Fprintf(logWriter, "error: delete review branch %s: %v: %s\n", branch, err, out)
	}
}

// isMissingWorktreeErr reports whether a `git worktree remove --force`
// failure is caused by the worktree not existing — i.e. a no-op the
// caller should suppress, not a real error. Git prints either
// "not a working tree" (when the path is not registered as a
// worktree) or "fatal: '<path>' is not a working tree" depending on
// version. We treat both as missing.
func isMissingWorktreeErr(out []byte) bool {
	return bytes.Contains(out, []byte("not a working tree"))
}

// isMissingBranchErr reports whether a `git branch -D` failure is
// caused by the branch not existing — a no-op the caller should
// suppress, not a real error.
func isMissingBranchErr(out []byte) bool {
	return bytes.Contains(out, []byte("not found"))
}
