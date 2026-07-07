package review

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
)

// TestDaemon_LaunchReviewReturnsFastAndRecordsPending is the headline
// regression for issue #1482 acceptance criterion #1:
// `launchReview` returns within `RunBatch_completion + 5s` under all
// documented failure modes (no 15s retry chain). After RunBatch
// returns successfully, launchReview's post step (issue #1846) reads
// decision.md, posts it, and records MarkSeen on the per-run
// review-state.json. The test drives the full tick flow so the
// production post-mark path runs.
//
// The "no 15s retry chain" half of the regression is asserted by
// (a) the elapsed-wall-clock budget (<5s) and (b) a single ListPRComments
// call observable in fakeGH — under the old verifyReviewPosted
// primitive, a missing-comment run would call ListPRComments up to 3
// times; lazy verify records pending after one RunBatch, no further
// ListPRComments happens in this tick.
//
// Issue #1849 (S6): the lazy-verify walker is gone. The post step is
// the SOLE writer to MarkSeen on the launch path; the seen-cache
// short-circuit is the only deduplication gate.
func TestDaemon_LaunchReviewReturnsFastAndRecordsPending(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 7, State: "open"}},
		comments: map[int][]github.PRComment{
			7: {
				{ID: "trigger", Body: "/sandman review", CreatedAt: now},
			},
		},
		prFetch: map[int]*github.PR{7: {Number: 7, Title: "PR 7", Body: "Body"}},
	}
	runner := newDecisionRunner()
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
		WorktreeDir:        ".sandman/worktrees",
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

	// Issue #1846 (S3) and #1849 (S6): launchReview owns MarkSeen
	// via the post step. A successful post records `success`
	// directly; the lazy-verify walker is gone, so the happy path
	// settles as `success` on the launching tick.
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

// TestDaemon_NextTickIsNoOpAfterSuccess pins the post-#1849 dedup
// invariant: a trigger recorded as success on the launching tick must
// NOT be re-launched on subsequent ticks. The seen-cache short-circuit
// (driven by the post step's MarkSeen("success")) keeps processPR from
// re-processing the trigger. This is the new shape of issue #1482
// acceptance criterion #4: verify is no longer inline-blocked because
// the post step owns terminal state at launch-end.
func TestDaemon_NextTickIsNoOpAfterSuccess(t *testing.T) {
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
		WorktreeDir:        ".sandman/worktrees",
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

	// Second tick: the trigger is now terminal-seen in the seen
	// cache (post-step MarkSeen fired MarkTerminalSeen via the
	// SeenCacheInvalidator hook). processPR drops the trigger
	// before launch.
	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if runner.calls != 1 {
		t.Errorf("second tick must not re-launch the review (seen-cache short-circuit), got %d total RunBatch calls", runner.calls)
	}

	// The per-run review-state.json on the launched run folder should
	// already have status=success for the trigger.
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
		t.Errorf("expected trigger recorded as success after first tick, got %+v", state.SeenComments)
	}
}

// TestDaemon_PendingNotTerminalInSeenCache pins the dedup consequence
// from issue #1482 §Notes: shouldSkipDedupStatus("pending") must be
// false (pending is retryable). Post-#1849 the seen-cache does NOT
// short-circuit pending comments because no daemon code path writes
// "pending" to review-state.json anymore; the S4 rehydrate walker
// (issue #1847) is the only mechanism that observes pending rows from
// disk and processes them at tick time.
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
