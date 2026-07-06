package review

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rafaelromao/sandman/internal/batchindex"
)

// TestDaemon_RestartRecoversPendingFromDisk pins acceptance criterion
// from issue #1635: a daemon restart between `launchReview` and the
// first post-launch `promotePendingReviews` tick must NOT cause the
// new daemon instance to re-launch a review for the same trigger.
//
// Issue #1846 (S3) supersedes the lazy-verify restart recovery path:
// launchReview now owns MarkSeen directly via the decision.md post
// step, so the pending-across-restart contract is no longer
// exercised in production. The on-disk MarkSeen("success") state is
// persisted by the post step (via ReviewStateStore), so a daemon
// restart that re-reads review-state.json will short-circuit the
// trigger through the existing seen-cache hydration path. That
// invariant is verified by TestDaemon_ReviewStateStore_HydratesSuccessCacheFromDisk.
func TestDaemon_RestartRecoversPendingFromDisk(t *testing.T) {
	t.Skip("issue #1846 (S3) supersedes the lazy-verify restart recovery path. The post step now records MarkSeen directly on the launching tick, so the pending-across-restart contract is no longer exercised; the seen-cache hydration invariant is still verified by TestDaemon_ReviewStateStore_HydratesSuccessCacheFromDisk.")
}

// findPendingReviewState walks the on-disk review-state.json files
// under BaseDir and returns the path of the first one that records
// (prNumber, commentID) with status "pending". Returns (path, true)
// on hit, ("", false) on miss. Mirrors the per-row layout from
// ADR-0030 / issue #1551. Read errors on the batches dir are fatal;
// read errors on individual review-state.json files are non-fatal
// (a row may legitimately have no state file yet, depending on the
// test's pre-seed).
func findPendingReviewState(t *testing.T, baseDir string, prNumber int, commentID string) (string, bool) {
	t.Helper()
	batchesDir := filepath.Join(baseDir, "batches")
	entries, err := os.ReadDir(batchesDir)
	if err != nil {
		t.Fatalf("read batches dir: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		rowID := deriveReviewRowID(e.Name(), prNumber)
		state, err := batchindex.ReadReviewState(filepath.Join(batchesDir, e.Name(), "runs", rowID))
		if err != nil {
			continue
		}
		for _, sc := range state.SeenComments {
			if sc.CommentID == commentID && sc.Status == "pending" {
				return filepath.Join(batchesDir, e.Name(), "runs", rowID, "review-state.json"), true
			}
		}
	}
	return "", false
}
