package review

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
)

// TestDaemon_S6_MissingDecision_DoesNotRelaunchForever pins the S6
// bounded-retry contract via seen-cache short-circuit. The lazy-verify
// walker is gone (issue #1849), so the bounded-retry escape must be
// expressed by postDecision's MarkSeen("failure") writing the
// terminal-seen entry on the in-memory seen cache directly. The
// next tick's processPR short-circuits the trigger via IsTerminalSeen
// and does NOT call RunBatch again.
//
// Pre-S6 the bounded-retry escape was multi-cycle: promotePendingReviews
// ran at the start of every tick, observed no review comment on the PR,
// incremented the cycle counter, and only after `pendingMaxCycles` ticks
// promoted the entry to failure and called MarkTerminalSeen. With the
// walker gone, the contract collapses to a single-shot at launch-end:
// postDecision records MarkSeen("failure") and the launch goroutine
// fires MarkTerminalSeen so the next tick short-circuits.
func TestDaemon_S6_MissingDecision_DoesNotRelaunchForever(t *testing.T) {
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
	// capturedRequest writes no decision.md, so postDecision's
	// missing-decision.md branch fires.
	runner := &capturedRequest{}
	d, _, dir := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})

	// First tick: launches the review; postDecision records
	// MarkSeen("failure") at launch-end (issue #1846 + #1849).
	tickAndWait(t, d, context.Background())
	if runner.Calls() != 1 {
		t.Fatalf("first tick should launch exactly 1 batch, got %d", runner.Calls())
	}

	// The per-run review-state.json records failure for the trigger.
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
	foundFailure := false
	for _, sc := range state.SeenComments {
		if sc.CommentID == commentID && sc.Status == "failure" {
			foundFailure = true
			break
		}
	}
	if !foundFailure {
		t.Errorf("expected MarkSeen(failure) for %s at launch-end, got %+v", commentID, state.SeenComments)
	}

	// Second tick: the seen-cache short-circuit must keep the
	// trigger from re-launching. This pins the bounded-retry
	// contract preserved by S6: failure is now terminal-seen and
	// processPR drops the trigger before launch.
	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if runner.Calls() != 1 {
		t.Errorf("second tick must not re-launch the trigger (bounded-retry via seen-cache short-circuit), got %d total RunBatch calls", runner.Calls())
	}
	if !d.IsTerminalSeen(prNumber, commentID) {
		t.Errorf("seenCache should mark (%d, %s) terminal-seen after launch-end failure, got cache %+v", prNumber, commentID, d.seenCache)
	}

	// Third tick: still no re-launch. The bounded-retry contract is
	// single-shot, not multi-cycle.
	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("third tick: %v", err)
	}
	if runner.Calls() != 1 {
		t.Errorf("third tick must not re-launch the trigger (single-shot bounded retry), got %d total RunBatch calls", runner.Calls())
	}
}

// TestDaemon_S6_FailedPost_DoesNotRelaunchForever pins the same
// bounded-retry contract for the post-failure branch of postDecision:
// when CommentPoster.PostComment returns a non-ctx error, the daemon
// records MarkSeen("failure") at launch-end. The seen-cache hook
// (MarkTerminalSeen) fires so the next tick's processPR short-circuits
// the trigger via IsTerminalSeen and does NOT call RunBatch again.
func TestDaemon_S6_FailedPost_DoesNotRelaunchForever(t *testing.T) {
	const (
		prNumber  = 7071
		commentID = "c-s6-2"
	)
	now := time.Date(2026, 7, 6, 13, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{
			prNumber: {{ID: commentID, Body: "/sandman review", CreatedAt: now}},
		},
		prFetch: map[int]*github.PR{prNumber: {Number: prNumber, Title: "PR S6 post", Body: "Body"}},
	}
	// decisionCapturingRunner writes decision.md before RunBatch returns,
	// so postDecision reaches PostComment.
	runner := &decisionCapturingRunner{capturedRequest: &capturedRequest{}, body: "ok"}
	poster := &fakeCommentPoster{err: os.ErrNotExist} // post failure
	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}
	d, _, _ := newDaemonForTestS3(t, gh, runner, cfg, poster)

	// First tick: launches the review; postDecision records
	// MarkSeen("failure") because PostComment returned an error.
	tickAndWait(t, d, context.Background())
	if runner.Calls() != 1 {
		t.Fatalf("first tick should launch exactly 1 batch, got %d", runner.Calls())
	}
	if poster.Calls() != 1 {
		t.Errorf("expected 1 PostComment call, got %d", poster.Calls())
	}

	// Second tick: the seen-cache short-circuit must keep the
	// trigger from re-launching.
	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if runner.Calls() != 1 {
		t.Errorf("second tick must not re-launch the trigger (bounded-retry via seen-cache short-circuit), got %d total RunBatch calls", runner.Calls())
	}
	if !d.IsTerminalSeen(prNumber, commentID) {
		t.Errorf("seenCache should mark (%d, %s) terminal-seen after launch-end post failure, got cache %+v", prNumber, commentID, d.seenCache)
	}
}
