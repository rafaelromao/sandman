package review

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
)

// TestDaemon_1949_S6_MissingDecision_RetriesAcrossTicks pins the
// issue #1949 contract for the missing-decision.md branch of
// postDecision. Pre-#1949 this test was
// TestDaemon_S6_MissingDecision_DoesNotRelaunchForever and pinned
// the S6 bounded-retry contract via seen-cache short-circuit. Issue
// #1949 supersedes that carve-out: the missing-decision.md outcome
// is retryable, not terminal. The daemon writes `pending` to
// review-state.json and registers a pendingPost entry, so the next
// tick's rehydrate walker drops the stale entry (decision.md still
// missing) and falls through to the launch path, re-running the
// agent.
func TestDaemon_1949_S6_MissingDecision_RetriesAcrossTicks(t *testing.T) {
	const (
		prNumber  = 7070
		commentID = "c-s6-1"
	)
	now := time.Date(2026, 7, 6, 13, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{
			prNumber: {{ID: commentID, Body: "/sandman review", CreatedAt: now}},
		},
		prFetch: map[int]*github.PR{prNumber: {Number: prNumber, Title: "PR S6", Body: "Body"}},
	}
	runner := &capturedRequest{}
	d, _, dir := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})

	tickAndWait(t, d, context.Background())
	if runner.Calls() != 1 {
		t.Fatalf("first tick should launch exactly 1 batch, got %d", runner.Calls())
	}

	statePath := locateReviewStatePath(t, dir)
	if statePath == "" {
		t.Fatalf("expected review-state.json to exist after launch, got none in %s", dir)
	}
	stateBytes, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read review-state.json: %v", err)
	}
	var state batchindex.ReviewState
	if err := json.Unmarshal(stateBytes, &state); err != nil {
		t.Fatalf("unmarshal review-state.json: %v", err)
	}
	foundPending := false
	for _, sc := range state.SeenComments {
		if sc.CommentID == commentID && sc.Status == "pending" {
			foundPending = true
			break
		}
	}
	if !foundPending {
		t.Errorf("expected MarkSeen(pending) for %s at launch-end (issue #1949), got %+v", commentID, state.SeenComments)
	}
	if d.IsTerminalSeen(prNumber, commentID) {
		t.Errorf("seenCache must NOT mark (%d, %s) terminal-seen under issue #1949, got cache %+v", prNumber, commentID, d.seenCache)
	}

	tickAndWait(t, d, context.Background())
	if runner.Calls() != 2 {
		t.Errorf("second tick should re-launch the agent via rehydrate fallthrough (issue #1949), got %d total RunBatch calls", runner.Calls())
	}
	if d.IsTerminalSeen(prNumber, commentID) {
		t.Errorf("seenCache must remain clean for (%d, %s) across rehydrate retries, got cache %+v", prNumber, commentID, d.seenCache)
	}
}

// TestDaemon_1949_MissingDecision_RetriesAcrossTicks pins the issue
// #1949 contract for the missing-decision.md branch of postDecision:
// when the agent fails to write decision.md after RunBatch returns,
// the daemon must NOT mark the comment terminal-seen. It writes
// `pending` to review-state.json and registers a pendingPost entry,
// so the next tick's rehydrate walker drops the stale entry
// (decision.md still missing) and falls through to the launch path,
// re-running the agent.
//
// Pre-#1949 this branch was terminal-seen via MarkSeen("failure") +
// MarkTerminalSeen, which short-circuited subsequent ticks. The fix
// treats "agent failed to produce a review" as retryable from the
// daemon's perspective: the next tick re-launches the agent and the
// trigger is not lost.
func TestDaemon_1949_MissingDecision_RetriesAcrossTicks(t *testing.T) {
	const (
		prNumber  = 19490
		commentID = "c-1949-missing"
	)
	now := time.Date(2026, 7, 7, 10, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{
			prNumber: {{ID: commentID, Body: "/sandman review", CreatedAt: now}},
		},
		prFetch: map[int]*github.PR{prNumber: {Number: prNumber, Title: "PR 1949", Body: "Body"}},
	}
	runner := &capturedRequest{} // never writes decision.md
	d, _, dir := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})

	tickAndWait(t, d, context.Background())
	if runner.Calls() != 1 {
		t.Fatalf("first tick should launch exactly 1 batch, got %d", runner.Calls())
	}

	statePath := locateReviewStatePath(t, dir)
	if statePath == "" {
		t.Fatalf("expected review-state.json to exist after launch, got none in %s", dir)
	}
	stateBytes, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read review-state.json: %v", err)
	}
	var state batchindex.ReviewState
	if err := json.Unmarshal(stateBytes, &state); err != nil {
		t.Fatalf("unmarshal review-state.json: %v", err)
	}
	foundPending := false
	for _, sc := range state.SeenComments {
		if sc.CommentID == commentID && sc.Status == "pending" {
			foundPending = true
			break
		}
	}
	if !foundPending {
		t.Errorf("expected MarkSeen(pending) for %s after missing-decision.md outcome (issue #1949), got %+v", commentID, state.SeenComments)
	}
	if d.IsTerminalSeen(prNumber, commentID) {
		t.Errorf("seenCache must NOT mark (%d, %s) terminal-seen under issue #1949, got cache %+v", prNumber, commentID, d.seenCache)
	}
	d.pendingPostMu.Lock()
	_, hasPending := d.pendingPost[prNumber][commentID]
	d.pendingPostMu.Unlock()
	if !hasPending {
		t.Errorf("pendingPost should register an entry for (%d, %s) under issue #1949, got map %+v", prNumber, commentID, d.pendingPost)
	}

	// Second tick: rehydrate walker drops the stale pending entry
	// (decision.md still missing) and falls through to launch path.
	tickAndWait(t, d, context.Background())
	if runner.Calls() != 2 {
		t.Errorf("second tick should re-launch the agent (rehydrate fallthrough), got %d total RunBatch calls", runner.Calls())
	}
	if d.IsTerminalSeen(prNumber, commentID) {
		t.Errorf("seenCache must remain clean for (%d, %s) across rehydrate retries, got cache %+v", prNumber, commentID, d.seenCache)
	}
}

// TestDaemon_PostRetriesOnTransientFailure pins the issue #1891 contract:
// transient gh pr comment failures are retried in-process up to
// PostStepMaxAttempts before the daemon falls back to the rehydrate path.
// The fake poster returns errors for attempts 1–3 then succeeds on
// attempt 4. Asserts: 4 PostComment calls, on-disk status is `success`,
// the seen cache IS marked terminal-seen (because the post landed), no
// pendingPost entry remains.
func TestDaemon_PostRetriesOnTransientFailure(t *testing.T) {
	const (
		prNumber  = 7072
		commentID = "c-1891-transient"
	)
	now := time.Date(2026, 7, 6, 13, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{
			prNumber: {{ID: commentID, Body: "/sandman review", CreatedAt: now}},
		},
		prFetch: map[int]*github.PR{prNumber: {Number: prNumber, Title: "PR 1891", Body: "Body"}},
	}
	runner := &decisionCapturingRunner{capturedRequest: &capturedRequest{}, body: "ok"}
	transient := errors.New("gh pr comment: transient network failure")
	poster := &fakeCommentPoster{
		errs: []error{transient, transient, transient, nil, nil},
	}
	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
		WorktreeDir:        ".sandman/worktrees",
	}
	d, _, _ := newDaemonForTestS3(t, gh, runner, cfg, poster)

	start := time.Now()
	tickAndWait(t, d, context.Background())
	elapsed := time.Since(start)

	if runner.Calls() != 1 {
		t.Fatalf("first tick should launch exactly 1 batch, got %d", runner.Calls())
	}
	if poster.Calls() != 4 {
		t.Errorf("expected 4 PostComment calls (3 failures + 1 success), got %d", poster.Calls())
	}
	if elapsed < 1*time.Second+2*time.Second+4*time.Second {
		t.Errorf("post retry should honour backoff schedule; elapsed=%v is faster than 1+2+4=7s minimum", elapsed)
	}
	if elapsed > 10*time.Second {
		t.Errorf("post retry elapsed=%v is slower than expected bound (1+2+4=7s + jitter)", elapsed)
	}

	if !d.IsTerminalSeen(prNumber, commentID) {
		t.Errorf("seenCache should mark (%d, %s) terminal-seen after a successful retry, got cache %+v", prNumber, commentID, d.seenCache)
	}

	d.pendingPostMu.Lock()
	_, hasPending := d.pendingPost[prNumber][commentID]
	d.pendingPostMu.Unlock()
	if hasPending {
		t.Errorf("pendingPost should NOT have an entry for (%d, %s) after a successful retry", prNumber, commentID)
	}
}

// TestDaemon_PostFinalFailure_RegistersPendingPost pins the issue #1891
// fallback: when the retry budget is exhausted, the daemon writes
// status="pending" on disk AND registers a pendingPost entry so the
// same-process next tick (or the S4 rehydrate walker after a restart)
// re-attempts the post. Asserts: 5 PostComment calls, on-disk status is
// `pending`, the seen cache is NOT marked terminal-seen, and the
// pendingPost map carries a registered entry keyed by (pr, comment).
func TestDaemon_PostFinalFailure_RegistersPendingPost(t *testing.T) {
	const (
		prNumber  = 7073
		commentID = "c-1891-final"
	)
	now := time.Date(2026, 7, 6, 13, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{
			prNumber: {{ID: commentID, Body: "/sandman review", CreatedAt: now}},
		},
		prFetch: map[int]*github.PR{prNumber: {Number: prNumber, Title: "PR 1891", Body: "Body"}},
	}
	runner := &decisionCapturingRunner{capturedRequest: &capturedRequest{}, body: "ok"}
	sustained := errors.New("gh pr comment: sustained failure")
	poster := &fakeCommentPoster{
		errs: []error{sustained, sustained, sustained, sustained, sustained},
	}
	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
		WorktreeDir:        ".sandman/worktrees",
	}
	d, _, dir := newDaemonForTestS3(t, gh, runner, cfg, poster)

	tickAndWait(t, d, context.Background())

	if runner.Calls() != 1 {
		t.Fatalf("first tick should launch exactly 1 batch, got %d", runner.Calls())
	}
	if poster.Calls() != PostStepMaxAttempts {
		t.Errorf("expected %d PostComment calls (full retry budget), got %d", PostStepMaxAttempts, poster.Calls())
	}

	statePath := locateReviewStatePath(t, dir)
	if statePath == "" {
		t.Fatalf("expected review-state.json to exist after launch, got none in %s", dir)
	}
	stateBytes, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read review-state.json: %v", err)
	}
	var state batchindex.ReviewState
	if err := json.Unmarshal(stateBytes, &state); err != nil {
		t.Fatalf("unmarshal review-state.json: %v", err)
	}
	foundPending := false
	for _, sc := range state.SeenComments {
		if sc.CommentID == commentID && sc.Status == "pending" {
			foundPending = true
			break
		}
	}
	if !foundPending {
		t.Errorf("expected MarkSeen(pending) for %s after retry budget exhausted, got %+v", commentID, state.SeenComments)
	}

	if d.IsTerminalSeen(prNumber, commentID) {
		t.Errorf("seenCache must NOT mark (%d, %s) terminal-seen after final post failure (issue #1891), got cache %+v", prNumber, commentID, d.seenCache)
	}

	d.pendingPostMu.Lock()
	entry, ok := d.pendingPost[prNumber][commentID]
	d.pendingPostMu.Unlock()
	if !ok {
		t.Fatalf("pendingPost should have an entry for (%d, %s) after final post failure, got map %+v", prNumber, commentID, d.pendingPost)
	}
	if entry.runDir == "" {
		t.Errorf("pendingPost entry for (%d, %s) should have a non-empty runDir, got %+v", prNumber, commentID, entry)
	}
	if entry.reviewState == "" {
		t.Errorf("pendingPost entry for (%d, %s) should have a non-empty reviewState path, got %+v", prNumber, commentID, entry)
	}
}

// TestDaemon_PostFinalFailure_NextTickRehydrates pins the issue #1891
// recovery loop: after a final post failure registers a pendingPost
// entry, the next tick fires tryRehydratePost which re-attempts the
// post via the same decision.md on disk. The fake poster is wired to
// fail on the first tick's 5 attempts and succeed on the second tick's
// single attempt. Asserts: the second tick lands the review as `success`
// and clears the pendingPost entry, without re-launching the agent.
func TestDaemon_PostFinalFailure_NextTickRehydrates(t *testing.T) {
	const (
		prNumber  = 7074
		commentID = "c-1891-rehydrate"
	)
	now := time.Date(2026, 7, 6, 13, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{
			prNumber: {{ID: commentID, Body: "/sandman review", CreatedAt: now}},
		},
		prFetch: map[int]*github.PR{prNumber: {Number: prNumber, Title: "PR 1891", Body: "Body"}},
	}
	runner := &decisionCapturingRunner{capturedRequest: &capturedRequest{}, body: "ok"}
	sustained := errors.New("gh pr comment: sustained failure")
	poster := &fakeCommentPoster{
		errs: []error{sustained, sustained, sustained, sustained, sustained, nil, nil},
	}
	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
		WorktreeDir:        ".sandman/worktrees",
	}
	d, _, _ := newDaemonForTestS3(t, gh, runner, cfg, poster)

	tickAndWait(t, d, context.Background())
	if poster.Calls() != PostStepMaxAttempts {
		t.Fatalf("first tick should hit the full retry budget, got %d calls", poster.Calls())
	}

	tickAndWait(t, d, context.Background())
	if poster.Calls() != PostStepMaxAttempts+1 {
		t.Errorf("second tick should fire tryRehydratePost and call PostComment exactly once, got total %d calls", poster.Calls())
	}
	if runner.Calls() != 1 {
		t.Errorf("second tick must NOT re-launch the agent (the post is recovered via rehydrate), got %d total RunBatch calls", runner.Calls())
	}
	if !d.IsTerminalSeen(prNumber, commentID) {
		t.Errorf("seenCache should mark (%d, %s) terminal-seen after rehydrate success, got cache %+v", prNumber, commentID, d.seenCache)
	}

	d.pendingPostMu.Lock()
	_, hasPending := d.pendingPost[prNumber][commentID]
	d.pendingPostMu.Unlock()
	if hasPending {
		t.Errorf("pendingPost should drop the entry after a successful rehydrate post")
	}
}

// TestDaemon_PostRetry_CtxCancelStopsRetrying pins that context
// cancellation between attempts short-circuits the retry loop and
// returns ctx.Err() before the budget is exhausted. The fake poster
// blocks on a release channel so the test can cancel the context
// mid-retry. Asserts: PostComment is called at most twice (no further
// attempts after ctx cancel), the on-disk status is NOT touched
// (no MarkSeen call), and the seen cache is not mutated.
func TestDaemon_PostRetry_CtxCancelStopsRetrying(t *testing.T) {
	const (
		prNumber  = 7075
		commentID = "c-1891-cancel"
	)
	now := time.Date(2026, 7, 6, 13, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{
			prNumber: {{ID: commentID, Body: "/sandman review", CreatedAt: now}},
		},
		prFetch: map[int]*github.PR{prNumber: {Number: prNumber, Title: "PR 1891", Body: "Body"}},
	}
	runner := &decisionCapturingRunner{capturedRequest: &capturedRequest{}, body: "ok"}
	transient := errors.New("gh pr comment: transient")
	release := make(chan struct{})
	poster := &fakeCommentPoster{
		errs:    []error{transient, transient, transient, transient, transient},
		release: release,
	}
	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
		WorktreeDir:        ".sandman/worktrees",
	}
	d, _, _ := newDaemonForTestS3(t, gh, runner, cfg, poster)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := d.tick(ctx); err != nil {
			t.Logf("tick returned: %v", err)
		}
	}()

	for i := 0; i < 50; i++ {
		if poster.Calls() >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	close(release)
	<-done

	if poster.Calls() > 2 {
		t.Errorf("ctx cancellation should stop retries promptly; got %d PostComment calls", poster.Calls())
	}

	if d.IsTerminalSeen(prNumber, commentID) {
		t.Errorf("seenCache must NOT mark terminal-seen on ctx cancel (issue #1846), got cache %+v", d.seenCache)
	}
}
