package review

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
)

// TestPromotePendingReviews_BoundedRetryGrepsLogBeforeFailing pins
// B13: when the cycle counter reaches pendingMaxCycles but the run
// log contains bodies the bot posted, the entry settles as success
// (not failure). The bodies are recorded into SelfPostStore. This
// closes the "bot was slower than 90s and the daemon would have
// wrongly labelled success as failure" race.
//
// Issue #1846 (S3) rewrote launchReview to own MarkSeen directly
// via the decision.md post step. The lazy-verify promotion path is
// preserved unchanged as a safety net, but the S3 happy path
// MarkSeens success on the launching tick — so a real launch never
// leaves a pending entry alive long enough for the bounded-retry
// grace to fire. This test exercises the preserved code by
// pre-registering a `pending` entry and seeding the in-memory
// `runLogPath` so the grace path reads the fixture directly,
// bypassing the launch path entirely. The pre-#1846
// TestDaemon_BoundedRetry_GrepsLogBeforeFailing test was rewritten
// to this form (issue #1846 self-review noted that skipping it
// would weaken the regression net for the retained
// promotePendingReviews grace code).
func TestPromotePendingReviews_BoundedRetryGrepsLogBeforeFailing(t *testing.T) {
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	botBody := "## Summary\nLGTM, no blockers."

	dir := t.TempDir()
	runLogPath := filepath.Join(dir, "run.log")
	logContent := "[run-1] 12:00:00 $ gh pr comment 7 --body \"" + botBody + "\"\n"
	if err := os.WriteFile(runLogPath, []byte(logContent), 0644); err != nil {
		t.Fatalf("write run.log: %v", err)
	}

	// No post-`since` PR comments — the bot's body is in the log
	// but GitHub's eventual consistency hasn't surfaced it yet.
	gh := &fakeGH{
		prs: []github.PR{{Number: 7, State: "open"}},
		comments: map[int][]github.PRComment{
			7: {
				{ID: "trigger", Body: "/sandman review", CreatedAt: now.Add(-1 * time.Minute)},
			},
		},
		prFetch: map[int]*github.PR{7: {Number: 7, Title: "PR 7", Body: "B"}},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now }
	d.selfPosts = mustSelfPostStore(t, d.BaseDir)

	// Drive enough ticks to reach the bounded-retry boundary.
	// On every tick we re-seed a pending entry pointing at the
	// run.log fixture, simulating the pre-S3 condition where the
	// launch goroutine had registered `pending` and the next
	// tick's promote step would walk it.
	runDir := filepath.Join(d.BaseDir, "fake-state")
	for i := 0; i < pendingMaxCycles; i++ {
		_ = os.MkdirAll(runDir, 0755)
		// Open a fresh per-run store so the promote path has
		// somewhere to write the final MarkSeen.
		statePath := filepath.Join(runDir, "review-state.json")
		store, err := NewReviewStateStore(statePath, 7, d)
		if err != nil {
			t.Fatalf("open review state: %v", err)
		}
		d.registerPendingReview(7, "trigger", now.Add(-1*time.Minute), statePath, runLogPath, store)
		if err := d.tick(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", i+2, err)
		}
	}

	state, err := batchindex.ReadReviewState(runDir)
	if err != nil {
		t.Fatalf("read review state: %v", err)
	}
	foundSuccess := false
	for _, sc := range state.SeenComments {
		if sc.CommentID == "trigger" && sc.Status == "success" {
			foundSuccess = true
		}
		if sc.CommentID == "trigger" && sc.Status == "failure" {
			t.Errorf("expected success (grace path), got failure (issue #1759 B13)")
		}
	}
	if !foundSuccess {
		t.Errorf("expected trigger recorded as success via bounded-retry grace, got %+v", state.SeenComments)
	}
	if !d.selfPosts.IsSelfPosted(7, botBody) {
		t.Errorf("expected bot body to be recorded via grace path")
	}
}

// TestDaemon_BoundedRetry_FailsWhenLogIsEmpty pins the negative half
// of B13: when the cycle counter reaches pendingMaxCycles AND the
// run log is empty (the bot truly failed), the entry settles as
// failure. This is the regression of the old behavior for the
// truly-failed case.
// TestPromotePendingReviews_BoundedRetryFailsWhenLogIsEmpty pins
// the negative half of B13: when the cycle counter reaches
// pendingMaxCycles AND the run log is empty (the bot truly
// failed), the entry settles as failure. Issue #1846 preserves the
// code but the launch goroutine no longer registers an entry under
// S3 (post step owns MarkSeen); the test now exercises the
// preserved code by re-seeding the in-memory pending entry on each
// tick so the bounded-retry grace path fires.
func TestPromotePendingReviews_BoundedRetryFailsWhenLogIsEmpty(t *testing.T) {
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

	dir := t.TempDir()
	runLogPath := filepath.Join(dir, "run.log")
	// Empty log — the bot never posted anything.
	if err := os.WriteFile(runLogPath, []byte(""), 0644); err != nil {
		t.Fatalf("write run.log: %v", err)
	}

	gh := &fakeGH{
		prs: []github.PR{{Number: 7, State: "open"}},
		comments: map[int][]github.PRComment{
			7: {
				{ID: "trigger", Body: "/sandman review", CreatedAt: now.Add(-1 * time.Minute)},
			},
		},
		prFetch: map[int]*github.PR{7: {Number: 7, Title: "PR 7", Body: "B"}},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now }
	d.selfPosts = mustSelfPostStore(t, d.BaseDir)

	// Drive enough ticks to exhaust the bounded retry budget.
	// Issue #1846 (S3) preserves the lazy-verify path as a safety
	// net; this test exercises the preserved code by re-seeding
	// the in-memory pending entry on each tick so the bounded-
	// retry grace path reaches the empty-log branch.
	runDir := filepath.Join(d.BaseDir, "fake-state-empty")
	for i := 0; i < pendingMaxCycles; i++ {
		_ = os.MkdirAll(runDir, 0755)
		statePath := filepath.Join(runDir, "review-state.json")
		store, err := NewReviewStateStore(statePath, 7, d)
		if err != nil {
			t.Fatalf("open review state: %v", err)
		}
		d.registerPendingReview(7, "trigger", now.Add(-1*time.Minute), statePath, runLogPath, store)
		if err := d.tick(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", i+2, err)
		}
	}

	state, err := batchindex.ReadReviewState(runDir)
	if err != nil {
		t.Fatalf("read review state: %v", err)
	}
	foundFailure := false
	for _, sc := range state.SeenComments {
		if sc.CommentID == "trigger" && sc.Status == "failure" {
			foundFailure = true
		}
		if sc.CommentID == "trigger" && sc.Status == "success" {
			t.Errorf("expected failure (empty log), got success")
		}
	}
	if !foundFailure {
		t.Errorf("expected trigger recorded as failure after bounded cycles, got %+v", state.SeenComments)
	}

	// Verify the trigger's review-state.json is well-formed JSON.
	data, err := os.ReadFile(filepath.Join(runDir, "review-state.json"))
	if err != nil {
		t.Fatalf("read review-state.json: %v", err)
	}
	var s batchindex.ReviewState
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("decode review-state.json: %v", err)
	}
}
