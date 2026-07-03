package review

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
)

// TestDaemon_RestartRecoversPendingFromDisk pins acceptance criterion
// #1 from issue #1635: a daemon restart between `launchReview` and
// the first post-launch `promotePendingReviews` tick must NOT cause
// the new daemon instance to re-launch a review for the same trigger.
//
// The bug: `(*Daemon).pendingReviews` is in-memory only, and the
// seen-cache hydration at construction deliberately excludes
// `pending` entries (`shouldSkipDedupStatus("pending") == false`).
// After a restart, a trigger that was already in flight (status
// `pending` on disk) is invisible to the new instance, so the tick
// re-launches the review.
//
// The fix: rehydrate `pendingReviews` from on-disk `review-state.json`
// at construction time, mirroring the existing seen-cache hydration.
func TestDaemon_RestartRecoversPendingFromDisk(t *testing.T) {
	skipIfNotAsyncLaunchSupported(t)
	const (
		prNumber  = 17
		commentID = "pending-on-disk"
	)
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	after := now.Add(2 * time.Minute)

	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{
			prNumber: {
				{ID: commentID, Body: "/sandman review", CreatedAt: now},
				// A reviewer reply that the second daemon's
				// promotePendingReviews step should pick up on
				// its first tick (since the reply's CreatedAt
				// is after the recorded `since`).
				{ID: "review-reply", Body: "## Summary\nLGTM", CreatedAt: after},
			},
		},
		prFetch: map[int]*github.PR{prNumber: {Number: prNumber, Title: "T", Body: "B"}},
	}
	runner := &capturedRequest{}

	// Step 1: launch a daemon, tick once, and capture the on-disk
	// side effect: a run folder with `review-state.json` recording
	// the trigger as `pending`. This mirrors the production
	// "daemon launched the review, agent is still working" state.
	dir := t.TempDir()
	t.Chdir(dir)
	d1 := New(dir, gh, &prompt.Engine{}, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}, &lockedBuffer{}, 0, false)
	d1.Clock = func() time.Time { return now }
	tickAndWait(t, d1, context.Background())
	if runner.calls != 1 {
		t.Fatalf("first daemon should launch the review once, got %d calls", runner.calls)
	}

	// Sanity check: the on-disk review-state.json for the row
	// that was just launched must record the trigger as `pending`.
	if _, ok := findPendingReviewState(t, dir, prNumber, commentID); !ok {
		t.Fatalf("first daemon should have persisted a pending entry for (%d, %s); no review-state.json with that entry was found", prNumber, commentID)
	}

	// Step 2: simulate a daemon restart by constructing a fresh
	// daemon against the same BaseDir. The in-memory pendingReviews
	// map and the in-memory seenCache are both empty on the new
	// instance; only the on-disk state survives.
	runner2 := &capturedRequest{}
	d2 := New(dir, gh, &prompt.Engine{}, runner2, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}, &lockedBuffer{}, 0, false)
	d2.Clock = func() time.Time { return now.Add(1 * time.Minute) }

	// Step 3: drive one tick on the fresh daemon. The replay
	// guard under test (pendingReviews rehydrated from disk) must
	// prevent processPR from launching a second batch for the
	// already-in-flight trigger.
	if err := d2.tick(context.Background()); err != nil {
		t.Fatalf("second daemon tick: %v", err)
	}
	if runner2.calls != 0 {
		t.Errorf("second daemon must not re-launch the in-flight trigger after restart, got %d RunBatch calls", runner2.calls)
	}
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
