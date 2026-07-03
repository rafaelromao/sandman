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

// TestDaemon_ProcessPR_BotReviewBodyWithTriggerSubstring_DoesNotLoop
// is the regression-prevention pin for the PR #1671 failure: a bot
// review body that contains the literal `/sandman review` substring
// (in its `## Previous review progress` section, per the pr-review
// skill's review-body format) MUST be dropped by processPR and MUST
// NOT launch a fresh review.
//
// The PR has exactly one comment: the bot's review-body. The body
// contains the trigger substring and is pre-seeded in SelfPostStore
// (this is the path the pr-review skill wrapper takes at Step 4b —
// it records the review-body hash at posting time so the daemon's
// IsSelfPosted check, which runs BEFORE ParseTrigger, drops the
// body before it can match the trigger regex).
//
// Without the new ordering (ParseTrigger before IsSelfPosted, the
// pre-#1702 behavior), the body would match the trigger regex and
// the daemon would launch a self-loop review — this is the live
// failure observed on PR #1671. With the new ordering, the body is
// filtered as a self-post and the daemon launches zero reviews.
//
// Issue #1703 acceptance criterion #1.
func TestDaemon_ProcessPR_BotReviewBodyWithTriggerSubstring_DoesNotLoop(t *testing.T) {
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

	// Pre-seed the SelfPostStore with the bot's review-body hash.
	// This is the path the pr-review skill wrapper takes at Step
	// 4b: it hashes the review body and appends it to the store
	// at posting time.
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
		t.Errorf("MUST drop a bot review-body containing the trigger substring (PR #1671 / issue #1703), got %d batch runs", runner.calls)
	}
}

// TestDaemon_ProcessPR_BotReviewBodyWithoutSelfPostHash_StillNotLoop_AfterDefensiveRecord
// pins the cross-tick defensive-record path: even when the bot's
// review-body hash is NOT pre-seeded in SelfPostStore (the skill
// wrapper's Step 4b was bypassed or the bot was restarted before
// posting), the daemon's promotePendingComment records the body
// defensively on a tick, and a subsequent tick drops the body
// because its hash is now in the store.
//
// Sequence:
//  1. PR has one comment: the bot's review-body (with the trigger
//     substring). SelfPostStore is empty.
//  2. Test calls d.promotePendingComment to defensively record the
//     body (this is the "previous tick" observation path).
//  3. tickAndWait runs processPR. Because the body hash is now in
//     the store, IsSelfPosted drops it before ParseTrigger can
//     match the trigger substring.
//  4. Assert: zero batch runs.
//
// Issue #1703 acceptance criterion #2.
func TestDaemon_ProcessPR_BotReviewBodyWithoutSelfPostHash_StillNotLoop_AfterDefensiveRecord(t *testing.T) {
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

	// SelfPostStore starts empty. The bot's body is NOT pre-seeded
	// (simulating the wrapper-bypass failure mode).
	spPath := filepath.Join(dir, "reviews", "self-posted.json")
	sp, err := NewSelfPostStore(spPath)
	if err != nil {
		t.Fatalf("NewSelfPostStore: %v", err)
	}
	d.selfPosts = sp

	// Drive the defensive observation directly: a previous tick's
	// promotePendingComment would have observed this comment and
	// recorded it. We call promotePendingComment with since=now-30s
	// so the review comment (created at `now`) is observed after
	// `since` and the Record fires inside promotePendingComment.
	if _, err := d.promotePendingComment(context.Background(), 1, "trigger", now.Add(-30*time.Second)); err != nil {
		t.Fatalf("promotePendingComment (defensive record): %v", err)
	}
	if !d.selfPosts.IsSelfPosted(reviewBody) {
		t.Fatal("expected reviewBody to be recorded as self-post after defensive observation")
	}

	// Now run a tick. processPR should drop the body because
	// IsSelfPosted returns true for it (the hash is in the store
	// after the defensive record above).
	tickAndWait(t, d, context.Background())

	if runner.calls != 0 {
		t.Errorf("MUST drop a bot review-body whose hash was defensively recorded on a prior tick (issue #1703), got %d batch runs", runner.calls)
	}
}

// TestDaemon_ProcessPR_ImplementorTriggerStillLaunches_WhenTriggerHashNotRecorded
// pins the happy path after the trigger-hash recording is removed
// (issue #1700): an implementor's `/sandman review` trigger whose
// body is NOT in SelfPostStore (because the skill no longer records
// the trigger command) still launches a review. This is the contract
// that the new "IsSelfPosted first" ordering in processPR does not
// regress the implementor's review-request flow.
//
// The test pre-seeds the SelfPostStore with NOTHING (so the trigger
// body is not in the store) and the only PR comment is a fresh
// implementor trigger. The daemon must launch one batch run.
//
// Issue #1703 acceptance criterion #3.
func TestDaemon_ProcessPR_ImplementorTriggerStillLaunches_WhenTriggerHashNotRecorded(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	triggerBody := "/sandman review focus on tests"

	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {
				{ID: "trigger", Body: triggerBody, CreatedAt: now},
			},
		},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now.Add(-1 * time.Minute) }

	// SelfPostStore is empty: the implementor's trigger is NOT
	// recorded (per #1700 trigger-hash removal). The daemon must
	// still launch a review.
	tickAndWait(t, d, context.Background())

	if runner.calls != 1 {
		t.Fatalf("MUST launch a review for an implementor trigger whose hash is not recorded (issue #1703), got %d batch runs", runner.calls)
	}
}
