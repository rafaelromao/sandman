package review

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
)

// TestDaemon_PromotePendingComment_RecordsBotAuthoredBody_FromReviewLog
// pins B12: when the post-`since` PR comment body matches a body the
// bot posted (as discovered by the run-log grep), promotePendingComment
// records it into SelfPostStore. The pending entry is then settled as
// success on the next tick.
//
// Setup: a pending entry is registered with a runLogPath that
// contains a `gh pr comment --body` invocation. The PR has the bot's
// review body as a post-`since` comment. After the promote call,
// SelfPostStore.IsSelfPosted(prNumber, body) must be true.
func TestDaemon_PromotePendingComment_RecordsBotAuthoredBody_FromReviewLog(t *testing.T) {
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	botBody := "## Summary\nBot review body content."

	dir := t.TempDir()
	runLogPath := filepath.Join(dir, "run.log")
	logContent := "[run-1] 12:00:00 $ gh pr comment 7 --body \"" + botBody + "\"\n"
	if err := os.WriteFile(runLogPath, []byte(logContent), 0644); err != nil {
		t.Fatalf("write run.log: %v", err)
	}

	gh := &fakeGH{
		comments: map[int][]github.PRComment{
			7: {
				{ID: "trigger", Body: "/sandman review", CreatedAt: now.Add(-1 * time.Minute)},
				{ID: "bot-review", Body: botBody, CreatedAt: now},
			},
		},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now }
	d.selfPosts = mustSelfPostStore(t, d.BaseDir)
	d.registerPendingReview(7, "trigger", now.Add(-30*time.Second), filepath.Join(dir, "review-state.json"), runLogPath, nil)

	bodies, err := extractBodiesFromLog(runLogPath)
	if err != nil {
		t.Fatalf("extractBodiesFromLog: %v", err)
	}
	status, err := d.promotePendingComment(context.Background(), 7, "trigger", now.Add(-30*time.Second), bodies)
	if err != nil {
		t.Fatalf("promotePendingComment: %v", err)
	}
	if status != "success" {
		t.Errorf("status: got %q, want %q", status, "success")
	}
	if !d.selfPosts.IsSelfPosted(7, botBody) {
		t.Errorf("expected bot body to be recorded in SelfPostStore (issue #1759 B12); IsSelfPosted=false")
	}
}

// TestDaemon_PromotePendingComment_DoesNotRecordHumanAuthoredBody
// pins the human-reply branch of B12: when the post-`since` PR
// comment body does NOT match any body the bot posted, the comment is
// treated as a human reply and NOT recorded into SelfPostStore. The
// entry still settles as success on a non-empty post-`since` comment.
func TestDaemon_PromotePendingComment_DoesNotRecordHumanAuthoredBody(t *testing.T) {
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	botBody := "## Summary\nBot review body content."
	humanReply := "Thanks, this looks good to me."

	dir := t.TempDir()
	runLogPath := filepath.Join(dir, "run.log")
	logContent := "[run-1] 12:00:00 $ gh pr comment 7 --body \"" + botBody + "\"\n"
	if err := os.WriteFile(runLogPath, []byte(logContent), 0644); err != nil {
		t.Fatalf("write run.log: %v", err)
	}

	gh := &fakeGH{
		comments: map[int][]github.PRComment{
			7: {
				{ID: "trigger", Body: "/sandman review", CreatedAt: now.Add(-1 * time.Minute)},
				{ID: "human-reply", Body: humanReply, CreatedAt: now},
			},
		},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now }
	d.selfPosts = mustSelfPostStore(t, d.BaseDir)
	d.registerPendingReview(7, "trigger", now.Add(-30*time.Second), filepath.Join(dir, "review-state.json"), runLogPath, nil)

	bodies, err := extractBodiesFromLog(runLogPath)
	if err != nil {
		t.Fatalf("extractBodiesFromLog: %v", err)
	}
	status, err := d.promotePendingComment(context.Background(), 7, "trigger", now.Add(-30*time.Second), bodies)
	if err != nil {
		t.Fatalf("promotePendingComment: %v", err)
	}
	if status != "success" {
		t.Errorf("status: got %q, want %q", status, "success")
	}
	if d.selfPosts.IsSelfPosted(7, humanReply) {
		t.Errorf("human reply must NOT be recorded into SelfPostStore (issue #1759 B12 / #1722); IsSelfPosted=true")
	}
}

// TestDaemon_PromotePendingComment_RecordsMultipleBotBodies pins the
// multi-post branch of B12: when the bot posts two bodies (a follow-up
// comment after the first review), both bodies are recorded. A
// post-`since` human reply is still not recorded.
//
// The test exercises the cache→match loop across two consecutive
// promote calls so the second body (which the bot posted AFTER the
// first promote success) is recorded on the second cycle.
func TestDaemon_PromotePendingComment_RecordsMultipleBotBodies(t *testing.T) {
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	firstBody := "## First review"
	secondBody := "## Follow-up review"
	humanReply := "Acknowledged."

	dir := t.TempDir()
	runLogPath := filepath.Join(dir, "run.log")
	logContent := "[run-1] 12:00:00 $ gh pr comment 7 --body \"" + firstBody + "\"\n" +
		"[run-1] 12:00:30 $ gh pr comment 7 --body \"" + secondBody + "\"\n"
	if err := os.WriteFile(runLogPath, []byte(logContent), 0644); err != nil {
		t.Fatalf("write run.log: %v", err)
	}

	gh := &fakeGH{
		comments: map[int][]github.PRComment{
			7: {
				{ID: "trigger", Body: "/sandman review", CreatedAt: now.Add(-1 * time.Minute)},
				{ID: "human-reply", Body: humanReply, CreatedAt: now},
			},
		},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now }
	d.selfPosts = mustSelfPostStore(t, d.BaseDir)
	d.registerPendingReview(7, "trigger", now.Add(-30*time.Second), filepath.Join(dir, "review-state.json"), runLogPath, nil)

	// Cycle 1: the bot's first body is in the log, but the only
	// post-`since` PR comment is the human reply. The entry settles
	// as success; the human reply is NOT recorded.
	bodies, err := extractBodiesFromLog(runLogPath)
	if err != nil {
		t.Fatalf("extractBodiesFromLog: %v", err)
	}
	status, err := d.promotePendingComment(context.Background(), 7, "trigger", now.Add(-30*time.Second), bodies)
	if err != nil {
		t.Fatalf("promotePendingComment (cycle 1): %v", err)
	}
	if status != "success" {
		t.Errorf("status: got %q, want %q", status, "success")
	}
	if d.selfPosts.IsSelfPosted(7, firstBody) {
		t.Errorf("first body must NOT be recorded when no PR comment matches (issue #1759 B12: only matched comments are recorded)")
	}
	if d.selfPosts.IsSelfPosted(7, secondBody) {
		t.Errorf("second body must NOT be recorded when no PR comment matches (issue #1759 B12)")
	}
	if d.selfPosts.IsSelfPosted(7, humanReply) {
		t.Errorf("human reply must NOT be recorded (issue #1759 B12 / #1722)")
	}

	// Reset the store and the entry to exercise cycle 2: a matched
	// post-`since` comment for each bot body is added to the PR.
	d.selfPosts = mustSelfPostStore(t, d.BaseDir)
	gh.comments[7] = append(gh.comments[7],
		github.PRComment{ID: "first-post", Body: firstBody, CreatedAt: now.Add(10 * time.Second)},
		github.PRComment{ID: "second-post", Body: secondBody, CreatedAt: now.Add(40 * time.Second)},
	)

	// Cycle 2: the daemon re-runs the promote step. With a fresh
	// registerPendingReview (simulating a second cycle), the bodies
	// are re-extracted and matched against the new post-`since`
	// comments. Both bot bodies are recorded; the human reply is
	// not.
	d.registerPendingReview(7, "trigger", now.Add(-30*time.Second), filepath.Join(dir, "review-state.json"), runLogPath, nil)
	status, err = d.promotePendingComment(context.Background(), 7, "trigger", now.Add(-30*time.Second), bodies)
	if err != nil {
		t.Fatalf("promotePendingComment (cycle 2): %v", err)
	}
	if status != "success" {
		t.Errorf("status: got %q, want %q", status, "success")
	}
	if !d.selfPosts.IsSelfPosted(7, firstBody) {
		t.Errorf("first body must be recorded when PR comment matches (issue #1759 B5/B12)")
	}
	if !d.selfPosts.IsSelfPosted(7, secondBody) {
		t.Errorf("second body must be recorded when PR comment matches (issue #1759 B5/B12)")
	}
	if d.selfPosts.IsSelfPosted(7, humanReply) {
		t.Errorf("human reply must NOT be recorded (issue #1759 B12 / #1722)")
	}
}

// mustSelfPostStore creates a SelfPostStore at <baseDir>/reviews/self-posted.json
// and fails the test if it cannot be created.
func mustSelfPostStore(t *testing.T, baseDir string) *SelfPostStore {
	t.Helper()
	sp, err := NewSelfPostStore(filepath.Join(baseDir, "reviews", "self-posted.json"))
	if err != nil {
		t.Fatalf("NewSelfPostStore: %v", err)
	}
	return sp
}
