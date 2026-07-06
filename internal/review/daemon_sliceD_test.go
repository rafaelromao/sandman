package review

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
)

// TestDaemon_PromotePendingComment_ReturnsSuccessWhenReviewFound pins
// the new lazy-verify primitive's success path: when a non-trigger
// comment has been posted at or after `since`, promotePendingComment
// returns "success" with no error. This is the minimal RED for the
// slice-D helper that replaces the inline retry chain.
func TestDaemon_PromotePendingComment_ReturnsSuccessWhenReviewFound(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	after := now.Add(1 * time.Minute)
	gh := &fakeGH{
		comments: map[int][]github.PRComment{
			42: {
				{ID: "100", Body: "/sandman review", CreatedAt: now},
				{ID: "101", Body: "## Summary\nLGTM", CreatedAt: after},
			},
		},
	}
	d, _, _ := newDaemonForTest(t, gh, &capturedRequest{}, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now }

	status, err := d.promotePendingComment(context.Background(), 42, "100", now, nil)
	if err != nil {
		t.Fatalf("expected no error when review comment is present, got: %v", err)
	}
	if status != "success" {
		t.Errorf("expected status success, got %q", status)
	}
}

// TestDaemon_PromotePendingComment_ReturnsErrorWhenMissing pins the
// negative path: when no non-trigger comment exists at or after
// `since`, promotePendingComment returns an error so the caller can
// decide whether to record failure (after the bounded cycle count) or
// leave the comment pending.
func TestDaemon_PromotePendingComment_ReturnsErrorWhenMissing(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		comments: map[int][]github.PRComment{
			42: {
				{ID: "100", Body: "/sandman review", CreatedAt: now},
			},
		},
	}
	d, _, _ := newDaemonForTest(t, gh, &capturedRequest{}, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now }

	_, err := d.promotePendingComment(context.Background(), 42, "100", now, nil)
	if err == nil {
		t.Fatal("expected error when no review comment is present")
	}
	if !strings.Contains(err.Error(), "no review comment") {
		t.Errorf("expected error to mention missing review comment, got: %v", err)
	}
}

// TestDaemon_PromotePendingComment_IgnoresTriggerComment pins the
// trigger-exclusion rule from the inline verify: the trigger comment
// itself does NOT count as a posted review. A follow-up trigger-only
// PR (e.g. operator re-posts the same comment) is still treated as
// "no review found".
func TestDaemon_PromotePendingComment_IgnoresTriggerComment(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	after := now.Add(1 * time.Minute)
	gh := &fakeGH{
		comments: map[int][]github.PRComment{
			42: {
				{ID: "100", Body: "/sandman review", CreatedAt: now},
				// Trigger reposted later; should NOT count as the
				// agent's review.
				{ID: "100", Body: "/sandman review again", CreatedAt: after},
			},
		},
	}
	d, _, _ := newDaemonForTest(t, gh, &capturedRequest{}, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now }

	_, err := d.promotePendingComment(context.Background(), 42, "100", now, nil)
	if err == nil {
		t.Fatal("expected error when only the trigger comment exists")
	}
}

// TestDaemon_LaunchReviewReturnsFastAndRecordsPending is the headline
// regression for issue #1482 acceptance criterion #1:
// `launchReview` returns within `RunBatch_completion + 5s` under all
// documented failure modes (no 15s retry chain). After RunBatch
// returns successfully and processPR writes the lazy-verify pending
// mark, review-state.json records the trigger as pending so a
// follow-up tick can promote it. The test drives the full tick flow
// so the production pending-mark path runs.
//
// The "no 15s retry chain" half of the regression is asserted by
// (a) the elapsed-wall-clock budget (<5s) and (b) a single ListPRComments
// call observable in fakeGH — under the old verifyReviewPosted
// primitive, a missing-comment run would call ListPRComments up to 3
// times; lazy verify records pending after one RunBatch, no further
// ListPRComments happens in this tick.
func TestDaemon_LaunchReviewReturnsFastAndRecordsPending(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 7, State: "open"}},
		comments: map[int][]github.PRComment{
			7: {
				// No review comment present yet. Under the old
				// synchronous verify, this would retry for ~15s.
				{ID: "trigger", Body: "/sandman review", CreatedAt: now},
			},
		},
		prFetch: map[int]*github.PR{7: {Number: 7, Title: "PR 7", Body: "Body"}},
	}
	runner := newDecisionRunner()
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now }

	start := time.Now()
	tickAndWait(t, d, context.Background())
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("tick took %v, expected well under 5s (no 15s retry chain)", elapsed)
	}
	if runner.calls != 1 {
		t.Fatalf("first tick should launch exactly 1 batch, got %d", runner.calls)
	}

	// The 3 × ListPRComments retry chain is gone. After this tick the
	// per-PR fake's ListPRComments must have been called exactly once:
	// one call from processPR's scan + zero from the (removed)
	// inline verify. We also assert the comment count is at 1 here
	// (a tighter bound would couple us to refactors of processPR's
	// scan ordering).
	if got := gh.commentCalls[7]; got != 1 {
		t.Errorf("ListPRComments should be called exactly once during the first tick (no missing-comment retry chain), got %d calls", got)
	}

	// Issue #1846 (S3): launchReview now owns MarkSeen. A
	// successful post records `success` directly; the previous
	// `pending` -> next-tick-promotion flow still exists as a
	// safety net for the pre-S3 lazy-verify semantics but the
	// happy path settles as `success` on the launching tick.
	runDir := runner.last.RunDir
	statePath := filepath.Join(runDir, "review-state.json")
	data, readErr := os.ReadFile(statePath)
	if readErr != nil {
		t.Fatalf("read review-state.json: %v", readErr)
	}
	var state batchindex.ReviewState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("decode review-state.json: %v", err)
	}
	foundStatus := ""
	for _, sc := range state.SeenComments {
		if sc.CommentID == "trigger" {
			foundStatus = sc.Status
			break
		}
	}
	if foundStatus != "success" {
		t.Errorf("expected trigger recorded as success in review-state.json (S3 happy path), got %q (SeenComments=%+v)", foundStatus, state.SeenComments)
	}
}

// TestDaemon_NextTickPromotesPendingCommentToSuccess pins issue #1482
// acceptance criterion #4: a successful review comment posted after
// launchReview returns is still detected and recorded as terminal-seen
// on the next tick — verify is no longer inline-blocked. The next tick
// also MUST NOT re-launch the review.
func TestDaemon_NextTickPromotesPendingCommentToSuccess(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	afterReview := now.Add(1 * time.Minute)
	gh := &fakeGH{
		prs: []github.PR{{Number: 11, State: "open"}},
		comments: map[int][]github.PRComment{
			11: {
				{ID: "trigger", Body: "/sandman review", CreatedAt: now},
				// The agent's review comment arrives between ticks.
				{ID: "review-reply", Body: "## Summary\nApproved", CreatedAt: afterReview},
			},
		},
		prFetch: map[int]*github.PR{11: {Number: 11, Title: "PR 11", Body: "Body"}},
	}
	runner := newDecisionRunner()
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now }

	// First tick: launch the review and settle it on success
	// (issue #1846 happy path: post step reads decision.md
	// written by the test runner and MarkSeen("success") is
	// recorded directly by launchReview).
	tickAndWait(t, d, context.Background())
	if runner.calls != 1 {
		t.Fatalf("first tick should launch exactly 1 batch, got %d", runner.calls)
	}

	// Second tick: the agent's review comment is now visible. The
	// promotePendingReviews step should mark the trigger as success
	// WITHOUT calling RunBatch again.
	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if runner.calls != 1 {
		t.Errorf("second tick must not re-launch the review, got %d total RunBatch calls", runner.calls)
	}

	// The per-run review-state.json on the launched run folder should
	// now have status=success for the trigger.
	runDir := runner.last.RunDir
	state, err := batchindex.ReadReviewState(runDir)
	if err != nil {
		t.Fatalf("read review state: %v", err)
	}
	foundSuccess := false
	for _, sc := range state.SeenComments {
		if sc.CommentID == "trigger" && sc.Status == "success" {
			foundSuccess = true
			break
		}
	}
	if !foundSuccess {
		t.Errorf("expected trigger promoted to success after second tick, got %+v", state.SeenComments)
	}
}

// TestDaemon_NextTickRejectsPendingCommentToFailureAfterBound pins
// acceptance criterion #4's negative half: if the new review comment
// never appears, the daemon promotes the pending comment to failure
// after `pendingMaxCycles` ticks, instead of retrying forever.
func TestDaemon_NextTickRejectsPendingCommentToFailureAfterBound(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 13, State: "open"}},
		comments: map[int][]github.PRComment{
			13: {
				// Only the trigger; never any agent reply.
				{ID: "trigger", Body: "/sandman review", CreatedAt: now},
			},
		},
		prFetch: map[int]*github.PR{13: {Number: 13, Title: "PR 13", Body: "Body"}},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now }

	tickAndWait(t, d, context.Background())
	runDir := runner.last.RunDir

	// Drive enough ticks to exhaust the bounded retry budget.
	for i := 0; i < pendingMaxCycles; i++ {
		if err := d.tick(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", i+2, err)
		}
	}

	// After pendingMaxCycles additional ticks the trigger should be
	// marked failure (not pending), and no further RunBatch launches.
	if runner.calls != 1 {
		t.Errorf("expected exactly 1 RunBatch call across %d ticks (no retry of pending), got %d", pendingMaxCycles+1, runner.calls)
	}
	state, err := batchindex.ReadReviewState(runDir)
	if err != nil {
		t.Fatalf("read review state: %v", err)
	}
	foundFailure := false
	for _, sc := range state.SeenComments {
		if sc.CommentID == "trigger" && sc.Status == "failure" {
			foundFailure = true
			break
		}
	}
	if !foundFailure {
		t.Errorf("expected trigger promoted to failure after bounded cycles, got %+v", state.SeenComments)
	}
}

// TestDaemon_PendingCommentIsNotRelaunchedMidCycle pins the
// "next tick processes pending comments without re-launching the
// review" invariant on a fresh pending entry whose cycle counter is
// strictly below the bound (issue #1482 acceptance criterion #4,
// negative half). Driving two ticks with the agent's review comment
// never arriving must keep RunBatch calls at exactly 1 — neither tick
// may launch a new batch.
func TestDaemon_PendingCommentIsNotRelaunchedMidCycle(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 19, State: "open"}},
		comments: map[int][]github.PRComment{
			19: {
				{ID: "trigger", Body: "/sandman review", CreatedAt: now},
			},
		},
		prFetch: map[int]*github.PR{19: {Number: 19, Title: "PR 19", Body: "Body"}},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now }

	// First tick: launch + record pending.
	tickAndWait(t, d, context.Background())
	if runner.calls != 1 {
		t.Fatalf("first tick should launch exactly 1 batch, got %d", runner.calls)
	}

	// Second tick with no review comment yet — must NOT relaunch.
	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if runner.calls != 1 {
		t.Errorf("second tick must not re-launch the pending review (cycle 1 < pendingMaxCycles), got %d RunBatch calls", runner.calls)
	}

	// Third tick: still pending, cycle 2 — also must not relaunch.
	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("third tick: %v", err)
	}
	if runner.calls != 1 {
		t.Errorf("third tick must not re-launch the pending review (cycle 2 < pendingMaxCycles), got %d RunBatch calls", runner.calls)
	}
}

// TestDaemon_NextTickRejectsPendingCommentTwiceNoOp pins that once a
// pending comment is promoted to failure, the daemon does not keep
// retrying it on later ticks. This is the corollary of acceptance
// criterion "without re-launching the review" applied to the failure
// half of the bounded retry escape.
func TestDaemon_NextTickRejectsPendingCommentTwiceNoOp(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 17, State: "open"}},
		comments: map[int][]github.PRComment{
			17: {
				{ID: "trigger", Body: "/sandman review", CreatedAt: now},
			},
		},
		prFetch: map[int]*github.PR{17: {Number: 17, Title: "PR 17", Body: "Body"}},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now }

	tickAndWait(t, d, context.Background())
	for i := 0; i < pendingMaxCycles+2; i++ {
		if err := d.tick(context.Background()); err != nil {
			t.Fatalf("tick: %v", err)
		}
	}
	if runner.calls != 1 {
		t.Errorf("post-failure ticks should not launch new reviews, got %d RunBatch calls", runner.calls)
	}
}

// TestDaemon_PendingNotTerminalInSeenCache pins the dedup consequence
// from issue #1482 §Notes: shouldSkipDedupStatus("pending") must be
// false (pending is retryable). The seen cache therefore does NOT
// short-circuit pending comments — a follow-up tick that processes
// the PR sees the pending entry in MarkSeen runs and the new
// promotePendingReviews step can still operate on it.
func TestDaemon_PendingNotTerminalInSeenCache(t *testing.T) {
	if shouldSkipDedupStatus("pending") {
		t.Error("shouldSkipDedupStatus(pending) must be false; pending comments must remain retryable")
	}
	if shouldSkipDedupStatus("success") != true {
		t.Error("shouldSkipDedupStatus(success) must be true")
	}
	if shouldSkipDedupStatus("superseded") != true {
		t.Error("shouldSkipDedupStatus(superseded) must be true")
	}
}

// TestDaemon_LaunchReviewReturnsFastOnRunBatchError pins that the
// critical-path latency budget holds even when RunBatch errors: the
// launch returns quickly and processPR records the trigger as
// failure (not pending), because the failure is unambiguously "the
// agent did not start" — there is no review comment to look for.
// The test drives a full tick so the production-shaped flow runs:
// prepare -> launch (error) -> processPR records failure.
func TestDaemon_LaunchReviewReturnsFastOnRunBatchError(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 21, State: "open"}},
		comments: map[int][]github.PRComment{
			21: {{ID: "trigger", Body: "/sandman review", CreatedAt: now}},
		},
		prFetch: map[int]*github.PR{21: {Number: 21, Title: "PR 21", Body: "Body"}},
	}
	var runsMu sync.Mutex
	var runDirs []string
	errRunner := batchFunc(func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		runsMu.Lock()
		runDirs = append(runDirs, req.RunDir)
		runsMu.Unlock()
		return nil, errors.New("batch exploded")
	})
	d, _, _ := newDaemonForTest(t, gh, errRunner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now }

	start := time.Now()
	tickAndWait(t, d, context.Background())
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Errorf("tick took %v on RunBatch error, expected under 5s", elapsed)
	}

	// Read the per-run review-state.json from the FIRST run folder
	// (the second tick's prepareReviewRun may create a new batch
	// folder on retry; the first-runner's folder is the one we
	// expect to carry status=failure).
	runsMu.Lock()
	capturedRunDir := runDirs[0]
	runsMu.Unlock()
	state, readErr := batchindex.ReadReviewState(capturedRunDir)
	if readErr != nil {
		t.Fatalf("read review state at %s: %v", capturedRunDir, readErr)
	}
	foundFailure := false
	for _, sc := range state.SeenComments {
		if sc.CommentID == "trigger" && sc.Status == "failure" {
			foundFailure = true
			break
		}
	}
	if !foundFailure {
		t.Errorf("expected trigger recorded as failure when RunBatch errors, got %+v", state.SeenComments)
	}
}

// ensure pendingMaxCycles is referenced at compile time
var _ = pendingMaxCycles
