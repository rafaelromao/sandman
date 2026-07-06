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

// TestReviewDaemon_FullLoopPastLaunchReview exercised the lazy-verify
// path that issue #1846 (S3) supersedes: launchReview registered the
// trigger as `pending` on the launching tick, the next tick's
// promotePendingReviews → promotePendingComment observed the bot's
// review body via ListPRComments and promoted to `success`, and
// SelfPostStore recorded the body via the promote-path grace hook.
//
// The seam-1 contract for the S3 slice is pinned directly by
// TestDaemon_S3_HappyPath_PostsRedactedDecision,
// TestDaemon_S3_MissingDecision_FailsClosed,
// TestDaemon_S3_FailedPost_FailsClosed, and
// TestDaemon_S3_CtxCancelDuringPost_StaysPending. The new path moves
// MarkSeen ownership into launchReview itself, so the lazy-verify
// promotion path is preserved unchanged as a safety net (see
// promotePendingReviews) but is no longer the primary terminal-state
// path. Skipping this test does not weaken the S3 contract.
// TestPromotePendingReviews_FullLoopPastLaunch exercises the
// preserved promotePendingReviews → promotePendingComment path end
// to end. The pre-#1846 TestReviewDaemon_FullLoopPastLaunchReview
// drove this through launchReview + the launch goroutine, but
// issue #1846 rewrote launchReview to own MarkSeen directly via the
// decision.md post step, so the `pending` → `promote` flow is no
// longer exercised in production. The lazy-verify code is preserved
// as a safety net; this test exercises the preserved code directly
// by pre-registering a `pending` entry pointing at a run.log that
// contains a bot-shaped body, then ticking promotePendingReviews.
func TestPromotePendingReviews_FullLoopPastLaunch(t *testing.T) {
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	botTime := now.Add(1 * time.Minute)
	const (
		triggerCommentID = "100"
		botCommentID     = "2000"
		botBody          = "## Summary LGTM no blockers — bot reviewer"
		prNumber         = 1
	)

	dir := t.TempDir()
	runLogPath := filepath.Join(dir, "run.log")
	logContent := "[run-1] 12:00:00 $ gh pr comment 1 --body \"" + botBody + "\"\n"
	if err := os.WriteFile(runLogPath, []byte(logContent), 0644); err != nil {
		t.Fatalf("write run.log: %v", err)
	}

	runDir := filepath.Join(dir, "fake-full-loop")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	statePath := filepath.Join(runDir, "review-state.json")

	gh := &fakeGH{
		prs: []github.PR{
			{Number: prNumber, State: "open", HeadRefName: "feature-x", HeadRefOid: "0000000000000000000000000000000000000000"},
		},
		comments: map[int][]github.PRComment{
			prNumber: {
				{ID: triggerCommentID, Body: "/sandman review", CreatedAt: now},
				{ID: botCommentID, Body: botBody, CreatedAt: botTime},
			},
		},
		prFetch: map[int]*github.PR{prNumber: {Number: prNumber, Title: "Test PR", Body: "A test pull request"}},
	}
	runner := &capturedRequest{}
	var logBuf lockedBuffer
	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}
	cfg.AgentProviders = map[string]config.Agent{
		"opencode": {Preset: "opencode", Command: "opencode"},
	}

	d := New(dir, gh, &prompt.Engine{}, runner, cfg, &logBuf, 0, false, nil)
	d.PollInterval = 0
	d.Clock = func() time.Time { return now }
	d.selfPosts = mustSelfPostStore(t, d.BaseDir)

	// Pre-seed a `pending` entry that points at the run.log
	// fixture. The first tick's promotePendingReviews walks it,
	// observes the bot body in the log, and MarkSeens success.
	store, err := NewReviewStateStore(statePath, prNumber, d)
	if err != nil {
		t.Fatalf("open review state: %v", err)
	}
	d.registerPendingReview(prNumber, triggerCommentID, now, statePath, runLogPath, store)

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	// Poll the per-run review-state.json until success or timeout.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s, err := batchindex.ReadReviewState(runDir)
		if err == nil {
			for _, sc := range s.SeenComments {
				if sc.CommentID == triggerCommentID && sc.Status == "success" {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	s, _ := batchindex.ReadReviewState(runDir)
	t.Fatalf("timed out waiting for success on %s; last state: %+v", triggerCommentID, s)
}
