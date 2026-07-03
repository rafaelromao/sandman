package review

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
)

// TestDaemon_ProcessPR_SelfPostedTriggerStillLaunches pins the fix
// for issue #1682: when the only `/sandman review` trigger on a PR
// has a body that is in the SelfPostStore, the daemon STILL launches
// a review for it. ParseTrigger runs before the self-post check so
// trigger commands are always detected regardless of self-post status.
// The self-post filter applies only to non-trigger comments (review
// responses), preventing the bot's own review from being mistaken
// for a human reply during promotion.
func TestDaemon_ProcessPR_SelfPostedTriggerStillLaunches(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	triggerBody := "/sandman review focus on tests"

	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {
				{ID: "100", Body: triggerBody, CreatedAt: now},
			},
		},
	}
	runner := &capturedRequest{}
	d, _, dir := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})

	// Pre-seed the SelfPostStore with the trigger body so the
	// daemon would have treated it as a self-post before #1682.
	spPath := filepath.Join(dir, "reviews", "self-posted.json")
	sp, err := NewSelfPostStore(spPath)
	if err != nil {
		t.Fatalf("NewSelfPostStore: %v", err)
	}
	if err := sp.Record(1, triggerBody, ""); err != nil {
		t.Fatalf("seed Record: %v", err)
	}
	d.selfPosts = sp

	tickAndWait(t, d, context.Background())

	if runner.calls != 1 {
		t.Errorf("daemon SHOULD launch a review for a self-posted trigger (#1682 fix), got %d batch runs", runner.calls)
	}
}

// TestDaemon_ProcessPR_StillTriggersOnNonSelfComment pins the
// invariant that the self-post filter does not regress the happy
// path: a normal `/sandman review` comment whose body is NOT in the
// SelfPostStore still triggers a review.
func TestDaemon_ProcessPR_StillTriggersOnNonSelfComment(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {
				{ID: "100", Body: "/sandman review", CreatedAt: now},
			},
		},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now.Add(-1 * time.Minute) }

	// SelfPostStore is empty (no prior posts). tickAndWait blocks
	// until the async review goroutine completes (RunBatch now
	// launches in a background goroutine).
	tickAndWait(t, d, context.Background())

	if runner.calls != 1 {
		t.Fatalf("expected 1 batch run for a fresh trigger, got %d", runner.calls)
	}
}

// TestDaemon_ProcessPR_SkipsOnlySelfPostedAmongTriggers pins the
// mixed case: a PR has two trigger comments, one whose body is
// self-posted and one whose body is not. The daemon processes
// only the non-self one.
func TestDaemon_ProcessPR_SkipsOnlySelfPostedAmongTriggers(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	selfBody := "/sandman review focus alpha"
	realBody := "/sandman review focus beta"

	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {
				{ID: "c1", Body: selfBody, CreatedAt: now},
				{ID: "c2", Body: realBody, CreatedAt: now.Add(1 * time.Minute)},
			},
		},
	}
	runner := &capturedRequest{}
	d, _, dir := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now.Add(-2 * time.Minute) }

	spPath := filepath.Join(dir, "reviews", "self-posted.json")
	sp, err := NewSelfPostStore(spPath)
	if err != nil {
		t.Fatalf("NewSelfPostStore: %v", err)
	}
	if err := sp.Record(1, selfBody, ""); err != nil {
		t.Fatalf("seed Record: %v", err)
	}
	d.selfPosts = sp

	// After issue #1682, both trigger comments are detected (the
	// self-post filter now applies only to non-trigger comments).
	// The daemon picks the newest trigger and marks older ones
	// superseded: realBody (focus beta) is newer than selfBody, so
	// realBody wins. tickAndWait blocks until the async review
	// goroutine completes (RunBatch now launches in a background
	// goroutine).
	tickAndWait(t, d, context.Background())

	if runner.calls != 1 {
		t.Fatalf("expected 1 batch run (newest non-self trigger wins), got %d", runner.calls)
	}
	if runner.last.ReviewFocus != "focus beta" {
		t.Errorf("expected focus 'focus beta', got %q", runner.last.ReviewFocus)
	}
}

// TestDaemon_PromotePendingComment_DefensivelyRecordsObservedComment
// pins the contract that promotePendingComment records the first
// non-trigger comment it observes so the next tick treats it as a
// self-post. The first observation still counts as success (it is
// the agent's review or a non-self reviewer reply), but the next
// tick will skip it as a trigger.
func TestDaemon_PromotePendingComment_DefensivelyRecordsObservedComment(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	reviewBody := "## Summary\nLGTM, no blockers."

	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {
				{ID: "trigger", Body: "/sandman review", CreatedAt: now.Add(-1 * time.Minute)},
				{ID: "review", Body: reviewBody, CreatedAt: now},
			},
		},
	}
	runner := &capturedRequest{}
	d, _, dir := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now.Add(-2 * time.Minute) }

	// Pre-seed the SelfPostStore with nothing — the daemon
	// should defensively record the observed review.
	spPath := filepath.Join(dir, "reviews", "self-posted.json")
	sp, err := NewSelfPostStore(spPath)
	if err != nil {
		t.Fatalf("NewSelfPostStore: %v", err)
	}
	d.selfPosts = sp

	// First tick: launch the review. tickAndWait blocks until the
	// async review goroutine completes (RunBatch now launches in a
	// background goroutine).
	tickAndWait(t, d, context.Background())
	if runner.calls != 1 {
		t.Fatalf("first tick: expected 1 batch run, got %d", runner.calls)
	}
	// Drive the defensive observation directly. A real
	// second tick would do this via promotePendingReviews;
	// we call promotePendingComment with since=now so the
	// review comment is observed after `since` and the
	// Record fires.
	status, err := d.promotePendingComment(context.Background(), 1, "trigger", now.Add(-30*time.Second))
	if err != nil {
		t.Fatalf("promotePendingComment: %v", err)
	}
	if status != "success" {
		t.Errorf("expected status 'success' on first observation, got %q", status)
	}
	if !d.selfPosts.IsSelfPosted(reviewBody) {
		t.Error("expected reviewBody to be recorded as self-post after defensive observation")
	}

	// Second observation (simulating a later tick): the same
	// body is now self-posted and must NOT count as success.
	status, err = d.promotePendingComment(context.Background(), 1, "trigger", now.Add(-30*time.Second))
	if err == nil {
		t.Fatalf("expected pending error on second observation of self-post, got status=%q", status)
	}
	if status != "pending" {
		t.Errorf("expected status 'pending' on second observation, got %q", status)
	}
}

// TestDaemon_PromotePendingComment_DoesNotCountSelfPostAsSuccess
// pins the contract that when the only comment observed after
// `since` is already in the SelfPostStore, the function returns
// ("pending", err). This is the case where the bot's own
// review-comment body is observed on a tick that did not do the
// defensive Record yet (because the defensive Record only fires
// in the same call when the body is NOT already a self-post).
func TestDaemon_PromotePendingComment_DoesNotCountSelfPostAsSuccess(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	reviewBody := "## Summary\nself-review by the bot"

	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {
				{ID: "trigger", Body: "/sandman review", CreatedAt: now.Add(-1 * time.Minute)},
				{ID: "review", Body: reviewBody, CreatedAt: now},
			},
		},
	}
	runner := &capturedRequest{}
	d, _, dir := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now.Add(-2 * time.Minute) }

	// Pre-seed: the review body is ALREADY in selfPosts (as
	// if the skill wrapper recorded it at posting time).
	spPath := filepath.Join(dir, "reviews", "self-posted.json")
	sp, err := NewSelfPostStore(spPath)
	if err != nil {
		t.Fatalf("NewSelfPostStore: %v", err)
	}
	if err := sp.Record(1, reviewBody, ""); err != nil {
		t.Fatalf("seed Record: %v", err)
	}
	d.selfPosts = sp

	status, err := d.promotePendingComment(context.Background(), 1, "trigger", now.Add(-30*time.Second))
	if err == nil {
		t.Fatalf("expected pending error when only observation is a self-post, got status=%q", status)
	}
	if status != "pending" {
		t.Errorf("expected status 'pending', got %q", status)
	}
}
