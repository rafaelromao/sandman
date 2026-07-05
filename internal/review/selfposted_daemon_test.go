package review

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/testenv"
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

// TestDaemon_PromotePendingComment_DoesNotRecordObservedComment pins the
// post-#1722 contract: promotePendingComment detects success from any
// non-empty comment posted after `since`, but it MUST NOT write that
// comment into SelfPostStore. The defensive observation that used to
// live here recorded every observed comment — including the
// implementor's repeated `/sandman review` trigger body — and once
// that hash entered the store, processPR's IsSelfPosted-first filter
// dropped every future review request (permanent blindness). The
// self-post store is now populated ONLY by processPR itself
// (issue #1757), which records every non-trigger comment it observes
// and deliberately skips trigger bodies.
//
// Both the first and any subsequent call return "success" (any
// comment after `since` settles the lazy-verify entry); neither call
// poisons the store.
func TestDaemon_PromotePendingComment_DoesNotRecordObservedComment(t *testing.T) {
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

	// SelfPostStore starts empty — the daemon must keep it empty.
	spPath := filepath.Join(dir, "reviews", "self-posted.json")
	sp, err := NewSelfPostStore(spPath)
	if err != nil {
		t.Fatalf("NewSelfPostStore: %v", err)
	}
	d.selfPosts = sp

	// First observation: success, but NO recording.
	status, err := d.promotePendingComment(context.Background(), 1, "trigger", now.Add(-30*time.Second), nil)
	if err != nil {
		t.Fatalf("promotePendingComment: %v", err)
	}
	if status != "success" {
		t.Errorf("expected status 'success' on observation, got %q", status)
	}
	if d.selfPosts.IsSelfPosted(1, reviewBody) {
		t.Error("promotePendingComment MUST NOT record the observed comment (#1722), poisoned SelfPostStore")
	}

	// Second observation: still success. The entry is not poisoned, so
	// re-evaluation is stable (and in the real flow the entry is
	// dropped after the first success anyway).
	status, err = d.promotePendingComment(context.Background(), 1, "trigger", now.Add(-30*time.Second), nil)
	if err != nil {
		t.Fatalf("second promotePendingComment: %v", err)
	}
	if status != "success" {
		t.Errorf("expected status 'success' on second observation, got %q", status)
	}
	if d.selfPosts.IsSelfPosted(1, reviewBody) {
		t.Error("second observation MUST NOT poison SelfPostStore either (#1722)")
	}
}

// TestDaemon_PromotePendingComment_NoPoisoning_CrossTickSuccessSettlesWithoutLoop
// pins the post-#1722 cross-tick contract end-to-end via the full
// tick() ordering, after the #1757 recording-source move.
//
// The bot's review body follows the #1709 prompt rule: it does NOT
// contain the literal `/sandman review` substring, so ParseTrigger
// cannot match it. The wrapper (Step 4b) is gone (issue #1757); the
// daemon's processPR is the sole authoritative record site.
//
//   - Tick N: implementor's trigger lands; processPR launches the
//     review and registers the pending entry.
//   - Between ticks: the bot posts its review body (no trigger
//     substring).
//   - Tick N+1: promotePendingReviews runs first, observes the body
//     after `since`, returns success. processPR then runs: the body
//     is not in SelfPostStore yet so IsSelfPosted is false; the
//     trigger is filtered by the seen cache; the new review body is
//     recorded as a non-trigger body so any later re-observation is
//     suppressed. No review is re-launched.
//
// Assertions: runner.calls stays at 1 across both ticks (no
// re-launch). The bot review body IS in SelfPostStore after tick
// N+1 — processPR now records every non-trigger body it observes
// (issue #1757). Trigger bodies (the implementor's /sandman review)
// are deliberately NOT recorded, which is why no blindness occurs.
func TestDaemon_PromotePendingComment_NoPoisoning_CrossTickSuccessSettlesWithoutLoop(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	// Post-#1709 review body: paraphrases prior requests, no literal
	// trigger substring.
	reviewBody := "## Previous review progress\nOpen review requests: none.\n\nLGTM, no blockers."

	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {
				{ID: "trigger", Body: "/sandman review", CreatedAt: now},
			},
		},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now.Add(-1 * time.Minute) }

	// Tick N: implementor's trigger lands; processPR launches the
	// review and registers the pending entry.
	tickAndWait(t, d, context.Background())
	if runner.calls != 1 {
		t.Fatalf("tick N: expected 1 batch run, got %d", runner.calls)
	}

	// Between ticks: the bot posts its review body (no trigger
	// substring). Lock fakeGH.mu so the test stays race-safe.
	gh.mu.Lock()
	gh.comments[1] = append(gh.comments[1], github.PRComment{
		ID:        "bot-review",
		Body:      reviewBody,
		CreatedAt: now.Add(1 * time.Minute),
	})
	gh.mu.Unlock()

	// Tick N+1: promote sees the body → success (no recording);
	// processPR does not re-launch.
	tickAndWait(t, d, context.Background())

	if runner.calls != 1 {
		t.Errorf("tick N+1 MUST NOT re-launch on the bot review body (#1722), got %d total batch runs", runner.calls)
	}
	if d.selfPosts.IsSelfPosted(1, "/sandman review") {
		t.Error("tick N+1 MUST NOT record the implementor's trigger body (issue #1757); trigger bodies are deliberately skipped to keep future trigger lookups live")
	}
	if !d.selfPosts.IsSelfPosted(1, reviewBody) {
		t.Error("tick N+1 MUST record the bot's review body (issue #1757); processPR is the sole authoritative record site for non-trigger bodies")
	}
}

// TestDaemon_PromotePendingComment_CountsWrapperRecordedBotBodyAsSuccess
// pins the post-#1722 contract that a bot review-body recorded by the
// pr-review skill wrapper (Step 4b) IS the success signal for the
// pending entry that launched it.
//
// Pre-#1722, promotePendingComment ran an IsSelfPosted check that
// SKIPPED wrapper-recorded bodies, so a review whose body was
// correctly recorded by the wrapper was never detected as success and
// was mislabelled `failure` after pendingMaxCycles. The check existed
// only to support the defensive-record double-count avoidance; with
// the defensive record gone (#1722), the check is gone too, and the
// bot's own recorded body correctly settles the entry as success.
func TestDaemon_PromotePendingComment_CountsWrapperRecordedBotBodyAsSuccess(t *testing.T) {
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

	// Pre-seed: the review body is ALREADY in selfPosts, as if the
	// skill wrapper recorded it at posting time (Step 4b).
	spPath := filepath.Join(dir, "reviews", "self-posted.json")
	sp, err := NewSelfPostStore(spPath)
	if err != nil {
		t.Fatalf("NewSelfPostStore: %v", err)
	}
	if err := sp.Record(1, reviewBody, ""); err != nil {
		t.Fatalf("seed Record: %v", err)
	}
	d.selfPosts = sp

	status, err := d.promotePendingComment(context.Background(), 1, "trigger", now.Add(-30*time.Second), nil)
	if err != nil {
		t.Fatalf("expected success for a wrapper-recorded bot body, got error: %v", err)
	}
	if status != "success" {
		t.Errorf("expected status 'success' (the bot's recorded body is the success signal, #1722), got %q", status)
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

// TestDaemon_ProcessPR_RecordsNonTriggerBotReviewBody pins the post-#1757
// contract: processPR itself is the sole authoritative record site
// (replacing both the pr-review SKILL.md Step 4b wrapper and the
// promotePendingComment defensive observation). For every non-trigger
// comment it observes, processPR records the body into SelfPostStore
// so the next tick's IsSelfPosted check suppresses it. Trigger bodies
// are deliberately NOT recorded — recording them would mask future
// legitimate trigger lookups (every re-request shares one body hash).
//
// The bot's review body has no literal trigger substring (post-#1709
// prompt rule), so the first tick of processPR falls through to the
// new Record call. The second tick finds the body in the store and
// skips it before ParseTrigger, so no review is launched.
//
// Issue #1757 acceptance criterion: the daemon exclusively owns the
// SelfPostStore, the skill-side wrapper is gone.
func TestDaemon_ProcessPR_RecordsNonTriggerBotReviewBody(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	// Post-#1709 body: paraphrases prior requests, no literal trigger
	// substring.
	reviewBody := "## Previous review progress\nOpen review requests: none.\n\nLGTM, no blockers."

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

	spPath := filepath.Join(dir, "reviews", "self-posted.json")
	sp, err := NewSelfPostStore(spPath)
	if err != nil {
		t.Fatalf("NewSelfPostStore: %v", err)
	}
	d.selfPosts = sp

	tickAndWait(t, d, context.Background())

	if runner.calls != 0 {
		t.Errorf("MUST NOT launch on a post-#1709 bot review body; the recorded body must be suppressed on the next tick (issue #1757), got %d batch runs", runner.calls)
	}
	if !d.selfPosts.IsSelfPosted(1, reviewBody) {
		t.Error("processPR MUST record every non-trigger body it observes (issue #1757); the daemon is the sole authoritative record site")
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

// TestDaemon_NoBlindness_TriggerReRequestAfterPendingWindow_Launches is
// the definitive regression test for issue #1722: the daemon MUST NOT
// go blind to a legit `/sandman review` re-request that lands while (or
// after) a previous review is still pending verification.
//
// The pr-review skill posts `/sandman review` repeatedly across passes
// (Step 4), so every re-request shares one body and therefore one
// SHA-256 hash. Pre-#1722, promotePendingComment defensively recorded
// the first re-request it observed after `since`; from that tick on,
// processPR's IsSelfPosted-first filter dropped EVERY `/sandman review`
// comment and the daemon was permanently blind.
//
// Sequence:
//  1. C1 = `/sandman review` (trigger). Tick N launches a review and
//     registers a pending entry with since = tick N's clock.
//  2. Between ticks, a second `/sandman review` re-request (C2, same
//     body, new comment ID) lands after `since`.
//  3. Tick N+1: promotePendingReviews observes C2, returns success for
//     C1 (WITHOUT recording C2), and MarkSeens C1. processPR then
//     skips C1 via the seen cache and launches C2 — the re-request is
//     honoured, not swallowed.
//
// Assertions:
//   - runner.calls == 2 (C1 on tick N, C2 on tick N+1). Pre-#1722 this
//     was 1: C2 was poisoned into SelfPostStore and dropped.
//   - hashBody("/sandman review") is NOT in SelfPostStore. The store
//     must never contain the trigger body.
func TestDaemon_NoBlindness_TriggerReRequestAfterPendingWindow_Launches(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	triggerBody := "/sandman review"

	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {
				{ID: "t1", Body: triggerBody, CreatedAt: now},
			},
		},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	// d.now() is the `since` recorded for the pending entry on tick N.
	// Keep it at `now` so the re-request at now+1m is observed after it.
	d.Clock = func() time.Time { return now }

	// Tick N: launch the review for C1, register the pending entry.
	tickAndWait(t, d, context.Background())
	if runner.calls != 1 {
		t.Fatalf("tick N: expected 1 batch run, got %d", runner.calls)
	}

	// Between ticks: a legit `/sandman review` re-request (C2) lands.
	// Same body, new comment ID — exactly what the pr-review skill
	// posts on its next pass.
	gh.mu.Lock()
	gh.comments[1] = append(gh.comments[1], github.PRComment{
		ID:        "t2",
		Body:      triggerBody,
		CreatedAt: now.Add(1 * time.Minute),
	})
	gh.mu.Unlock()

	// Tick N+1: C1 is promoted to success (via C2) and dropped from
	// pending; processPR must launch C2. Pre-#1722 the defensive
	// record poisoned triggerBody and C2 was dropped (runner.calls
	// stayed at 1 — the blindness).
	tickAndWait(t, d, context.Background())

	if runner.calls != 2 {
		t.Errorf("tick N+1 MUST launch the re-request C2 (no blindness, #1722); expected 2 total batch runs, got %d", runner.calls)
	}
	if d.selfPosts.IsSelfPosted(1, triggerBody) {
		t.Errorf("tick N+1 MUST NOT record the trigger body in SelfPostStore (#1722); a poisoned trigger hash blinds the daemon to all future /sandman review requests")
	}
}

// TestDaemon_New_SeedsSelfPostStoreFromReviewRunLogs is the slice-2
// pin for issue #1821: a daemon freshly constructed against a
// BaseDir that already contains a review-kind batch's run.log with
// a `gh pr comment <N> --body <body>` invocation MUST observe the
// bot body in its SelfPostStore. This closes the across-restart
// failure mode on operators who have no on-disk self-posted.json
// to reload (greenfield cases where slice 1's loader has nothing
// to recover) — the run log is the only authoritative source for
// what the bot posted, and the seed step is the structural fix.
//
// Without this slice, a `sandman` operator who restarts the daemon
// after the bot posted a review body loses the entry on restart,
// the bot's body re-matches the trigger regex on the next tick
// (because it contains the literal `/sandman review` substring in
// its `## Previous review progress` section), and the daemon
// launches a self-loop review (live failure on PR #1809).
func TestDaemon_New_SeedsSelfPostStoreFromReviewRunLogs(t *testing.T) {
	// Pre-populate a review-kind batch with a run.log that the
	// daemon's run-log reader (extractBodiesFromLog) can parse.
	dir := testenv.MkdirShort(t, "sm-seed-selfposts-")
	t.Chdir(dir)

	const prNumber = 1809
	const botBody = "## Summary\nThis is the bot's review body for PR #1809. It contains /sandman review as a quoted literal substring so the previous-review-progress failure mode reproduces if SelfPostStore forgets it."

	// Layout:
	//   <dir>/batches/<batch>/runs/<row>/run.json  (kind=review)
	//   <dir>/batches/<batch>/runs/<row>/run.log  (one gh pr comment line)
	//   <dir>/batches.json                        (index entry)
	batchID := "abcd-20260701-PR1809"
	rowID := deriveReviewRowID(batchID, prNumber)
	runDir := filepath.Join(dir, "batches", batchID, "runs", rowID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "run.log"), []byte(
		"[abcd-20260701-PR1809] 12:00:00 $ gh pr comment 1809 --body \""+botBody+"\"\n",
	), 0o644); err != nil {
		t.Fatalf("write run.log: %v", err)
	}
	if err := batchindex.WriteManifest(runDir, batchindex.RunManifest{
		RunID:     rowID,
		BatchID:   batchID,
		PR:        prNumber,
		Kind:      batchindex.KindReview,
		CreatedAt: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
		Status:    batchindex.RunManifestStatusActive,
	}); err != nil {
		t.Fatalf("write run manifest: %v", err)
	}
	batchDir := filepath.Join(dir, "batches", batchID)
	indexPath := daemon.BatchesIndexPath(dir)
	idx, err := batchindex.Load(indexPath)
	if err != nil {
		t.Fatalf("load batches index: %v", err)
	}
	idx.Add(batchindex.Entry{
		ID:   batchID,
		Path: batchDir,
		Kind: batchindex.KindReview,
		PR:   prNumber,
	})
	if err := idx.Save(indexPath); err != nil {
		t.Fatalf("save batches index: %v", err)
	}

	// Construct a daemon. After New() returns, the bot body
	// must be in SelfPostStore for PR 1809 — observable via
	// IsSelfPosted on the public field.
	gh := &fakeGH{}
	runner := &capturedRequest{}
	d := New(dir, gh, &prompt.Engine{}, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}, &lockedBuffer{}, 0, false)

	if d.selfPosts == nil {
		t.Fatal("daemon.selfPosts must be non-nil after New")
	}
	if !d.selfPosts.IsSelfPosted(prNumber, botBody) {
		t.Errorf("after Daemon.New, the bot body extracted from the review run log MUST be in SelfPostStore (issue #1821 seed step); IsSelfPosted(%d, %q) returned false", prNumber, botBody)
	}
}

// TestDaemon_StaleBotBody_DoesNotRetriggerAcrossRestart is the
// end-to-end AC regression for issue #1821. A fresh daemon —
// constructed against a BaseDir that already contains the bot's
// own previous-run review body both in the seeded run.log AND on
// the fake GitHub PR comments endpoint — does NOT launch a batch
// run and does NOT add an eyes reaction to that body when ticked.
//
// The body is the exact shape that broke PR #1809: the bot's
// previous-review body on PR #1809 (comment 4884234243, posted
// 2026-07-05T00:15:54Z). It quotes the original trigger
// substring in its `## Previous review progress` section, so
// `ParseTrigger` matches it. Without the slice-1 loader and the
// slice-2 seed step, the bot body is not in SelfPostStore when
// the daemon starts — the IsSelfPosted check returns false on
// this tick, ParseTrigger matches, the daemon launches a fresh
// review run for this comment, and the daemon adds an eyes
// reaction to its own comment (live failure observed on PR
// #1809's run `4f33-260705092651-PR1809`).
//
// Pre-#1821 this test MUST fail (got 1 batch run AND 1 reaction
// on the bot's comment). Post-#1821 the daemon must:
//   1. Find the bot body in SelfPostStore via the slice-2 seed
//      step (the run.log contains a gh pr comment invocation
//      with this exact body),
//   2. Drop the comment via the IsSelfPosted-first filter in
//      processPR (issue #1702),
//   3. Launch zero batch runs and add zero eyes reactions for
//      this PR.
func TestDaemon_StaleBotBody_DoesNotRetriggerAcrossRestart(t *testing.T) {
	now := time.Date(2026, 7, 5, 0, 16, 0, 0, time.UTC)

	// This is the bot's review body verbatim from PR #1809,
	// comment 4884234243. The previous-review-progress section
	// quotes the literal `/sandman review` substring so
	// ParseTrigger would match if SelfPostStore forgot the
	// entry.
	botBody := "## Summary\n\nThis is the bot's previous-review body for PR #1809. The original /sandman review trigger was posted at 2026-07-05T00:11:28Z. No prior review findings exist to track."

	dir := testenv.MkdirShort(t, "sm-ac-selfposts-")
	t.Chdir(dir)

	const prNumber = 1809

	// The daemons run log for a prior-session review of PR #1809
	// (the on-disk source of truth per the issue #1821 seed
	// step). The daemon's `extractBodiesFromLog` must be able to
	// read this exactly the same way it reads live run logs.
	batchID := "abcd-20260705-PR1809"
	rowID := deriveReviewRowID(batchID, prNumber)
	priorRunDir := filepath.Join(dir, "batches", batchID, "runs", rowID)
	if err := os.MkdirAll(priorRunDir, 0o755); err != nil {
		t.Fatalf("mkdir prior run dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(priorRunDir, "run.log"), []byte(
		"[abcd-20260705-PR1809] 09:28:20 $ gh pr comment 1809 --body \""+botBody+"\"\n",
	), 0o644); err != nil {
		t.Fatalf("write prior run.log: %v", err)
	}
	if err := batchindex.WriteManifest(priorRunDir, batchindex.RunManifest{
		RunID:     rowID,
		BatchID:   batchID,
		PR:        prNumber,
		Kind:      batchindex.KindReview,
		CreatedAt: now.Add(-1 * time.Hour),
		Status:    batchindex.RunManifestStatusActive,
	}); err != nil {
		t.Fatalf("write prior run manifest: %v", err)
	}
	priorBatchDir := filepath.Join(dir, "batches", batchID)
	indexPath := daemon.BatchesIndexPath(dir)
	idx, err := batchindex.Load(indexPath)
	if err != nil {
		t.Fatalf("load batches index: %v", err)
	}
	idx.Add(batchindex.Entry{
		ID:   batchID,
		Path: priorBatchDir,
		Kind: batchindex.KindReview,
		PR:   prNumber,
	})
	if err := idx.Save(indexPath); err != nil {
		t.Fatalf("save batches index: %v", err)
	}

	// The fake GitHub endpoint serves the PR with two comments:
	// the original implementor trigger and the bot's previous-run
	// review body. The daemon's processPR must drop the bot body
	// via the IsSelfPosted-first filter and treat only the
	// implementor trigger as a fresh trigger to launch against.
	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{
			prNumber: {
				{ID: "trigger", Body: "/sandman review", CreatedAt: now.Add(-1 * time.Hour)},
				{ID: "stale-bot", Body: botBody, CreatedAt: now.Add(-30 * time.Minute)},
			},
		},
		prFetch: map[int]*github.PR{prNumber: {Number: prNumber, Title: "T", Body: "B"}},
	}
	runner := &capturedRequest{}

	// Construct the fresh daemon AFTER seeding the run-log
	// fixture, so the slice-2 seed step has the prior-session
	// run log available at construction time.
	d := New(dir, gh, &prompt.Engine{}, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}, &lockedBuffer{}, 0, false)
	d.Clock = func() time.Time { return now.Add(-1 * time.Minute) }

	if !d.selfPosts.IsSelfPosted(prNumber, botBody) {
		t.Fatalf("setup invariant: seed step should have placed the bot body in SelfPostStore; IsSelfPosted(%d, %q) = false", prNumber, botBody)
	}

	tickAndWait(t, d, context.Background())

	if runner.calls != 1 {
		t.Errorf("AC FAIL: stale bot body MUST NOT cause a self-loop batch run (issue #1821 / #1809); expected 1 run (the implementor trigger), got %d", runner.calls)
	}
	// AC: the daemon must NOT add an eyes reaction to the
	// bot's own comment. This is the exact symptom reported on
	// the screenshot for PR #1809 (👀 eyes count = 1 on the
	// bot's previous-review comment).
	gh.mu.Lock()
	defer gh.mu.Unlock()
	for _, c := range gh.reactionCalls {
		if c.kind == "add_comment" && c.commentID == "stale-bot" {
			t.Errorf("AC FAIL: daemon MUST NOT add an eyes reaction to the bot's own comment (issue #1821 / #1809); got reaction %+v", c)
		}
	}
}
