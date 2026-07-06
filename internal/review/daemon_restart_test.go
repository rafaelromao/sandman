package review

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rafaelromao/sandman/internal/batchindex"
)

// findPendingReviewState walks the on-disk review-state.json files
// under BaseDir and returns the path of the first one that records
// (prNumber, commentID) with status "pending". Returns (path, true)
// on hit, ("", false) on miss. Mirrors the per-row layout from
// ADR-0030 / issue #1551. Read errors on the batches dir are fatal;
// read errors on individual review-state.json files are non-fatal
// (a row may legitimately have no state file yet, depending on the
// test's pre-seed).
//
// Issue #1849 (S6): the `pending` status remains in the on-disk
// schema only as the S4 rehydrate-on-startup (issue #1847) source
// of truth. No daemon code path writes "pending" to review-state.json
// anymore; the helper is retained for tests that pre-seed a pending
// row to drive the S4 walker.
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
