package review

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
)

// TestDaemon_ProcessPR_SelfPostedReviewBody_DoesNotTrigger pins the
// new contract introduced by issue #1702: when the only comment on a
// PR matches the trigger regex AND its body is in the SelfPostStore,
// the daemon MUST NOT launch a review. The self-post filter runs
// before ParseTrigger in processPR, so a body that has been recorded
// as a self-post is dropped before it can match the trigger regex.
// This protects the daemon from re-triggering a review on the bot's
// own review-body (which contains the literal `/sandman review`
// substring in its `## Previous review progress` section).
//
// Issue #1682's ordering (ParseTrigger before IsSelfPosted) is
// reversed here. The implementor's `/sandman review` trigger still
// launches a review on a fresh tick because the trigger body is NOT
// in SelfPostStore — see TestDaemon_ProcessPR_StillTriggersOnNonSelfComment.
func TestDaemon_ProcessPR_SelfPostedReviewBody_DoesNotTrigger(t *testing.T) {
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

	if runner.calls != 0 {
		t.Errorf("daemon MUST drop a self-posted trigger-shaped body before parsing (#1702), got %d batch runs", runner.calls)
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

	// After issue #1702, the self-post filter runs BEFORE
	// ParseTrigger in processPR. The self-posted trigger comment
	// (c1) is therefore dropped at the IsSelfPosted check and
	// never reaches the trigger-parse path. Only c2 (realBody,
	// "focus beta") is parsed and queued as a trigger, and c2
	// is the only trigger — no supersede happens because c1
	// never entered the trigger slice. tickAndWait blocks until
	// the async review goroutine completes (RunBatch now
	// launches in a background goroutine).
	tickAndWait(t, d, context.Background())

	if runner.calls != 1 {
		t.Fatalf("expected 1 batch run (newest non-self trigger wins), got %d", runner.calls)
	}
	if runner.last.ReviewFocus != "focus beta" {
		t.Errorf("expected focus 'focus beta', got %q", runner.last.ReviewFocus)
	}
}

// TestDaemon_ProcessPR_RecordedReviewBody_DoesNotReTrigger pins the
// contract that a bot review-body recorded as a self-post is dropped
// by processPR on a subsequent tick — even when the body contains
// the trigger substring. This is the regression-prevention pin for
// issue #1702's self-loop failure: without the new ordering, the
// bot's review body, which contains the literal `/sandman review`
// substring in its `## Previous review progress` section, would
// match the trigger regex and re-launch a review.
//
// The PR has exactly one comment: the bot's review-body. The body
// contains the trigger substring and is pre-seeded in SelfPostStore.
// Under the OLD ordering, ParseTrigger would match the substring
// and the daemon would launch a review — a self-loop. Under the
// NEW ordering, IsSelfPosted runs first, drops the body, and no
// review is launched.
func TestDaemon_ProcessPR_RecordedReviewBody_DoesNotReTrigger(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	reviewBody := "## Previous review progress\n/sandman review\n\nLGTM, no blockers."

	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {
				{ID: "review", Body: reviewBody, CreatedAt: now},
			},
		},
	}
	runner := &capturedRequest{}
	d, _, dir := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now.Add(-1 * time.Minute) }

	// Pre-seed the SelfPostStore with the review-body — the
	// defensive record path. The body must be in the store
	// BEFORE processPR iterates the comments; otherwise it
	// would be treated as a fresh trigger on this tick.
	spPath := filepath.Join(dir, "reviews", "self-posted.json")
	sp, err := NewSelfPostStore(spPath)
	if err != nil {
		t.Fatalf("NewSelfPostStore: %v", err)
	}
	if err := sp.Record(1, reviewBody, ""); err != nil {
		t.Fatalf("seed Record: %v", err)
	}
	d.selfPosts = sp

	tickAndWait(t, d, context.Background())

	if runner.calls != 0 {
		t.Errorf("MUST drop a recorded review-body that contains the trigger substring (#1702), got %d batch runs", runner.calls)
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
