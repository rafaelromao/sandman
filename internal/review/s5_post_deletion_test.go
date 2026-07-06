package review

import (
	"context"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
)

// TestDaemon_ProcessPR_BotShapedBody_DoesNotTrigger is the post-S5
// behavioural pin for the surviving self-defence gate. The structural
// sniff `LooksLikeBotReviewBody` is now the only gate that drops a
// bot-shaped body (one carrying the `## Previous review progress`
// heading AND the literal `/sandman review` substring) before
// `ParseTrigger` runs. The test was renamed from
// TestDaemon_ProcessPR_SelfPostedReviewBody_DoesNotTrigger
// (issue #1848). The pre-S5 self-post filter is gone; this is the
// only defence.
func TestDaemon_ProcessPR_BotShapedBody_DoesNotTrigger(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	botBody := "## Previous review progress\n/sandman review focus on tests\n\nLGTM, no blockers."

	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {
				{ID: "100", Body: botBody, CreatedAt: now},
			},
		},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})

	tickAndWait(t, d, context.Background())

	if runner.calls != 0 {
		t.Errorf("daemon MUST drop a bot-shaped body via structural sniff (issue #1848 / #1821), got %d batch runs", runner.calls)
	}
}

// TestDaemon_ProcessPR_RecordedReviewBody_DoesNotReTrigger is the
// regression-prevention pin for issue #1702's self-loop failure:
// a bot review-body containing the literal `/sandman review`
// substring in its `## Previous review progress` section is dropped
// by `processPR` on a subsequent tick — even when the body
// structurally matches a previous bot review. Post-S5 the only gate
// is the structural sniff (`LooksLikeBotReviewBody`); the
// SelfPostStore is gone, but the body still trips the sniff.
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
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now.Add(-1 * time.Minute) }

	tickAndWait(t, d, context.Background())

	if runner.calls != 0 {
		t.Errorf("MUST drop a bot-shaped body containing the trigger substring via structural sniff (issue #1702 / #1848), got %d batch runs", runner.calls)
	}
}

// TestDaemon_ProcessPR_ImplementorTriggerStillLaunches_WhenTriggerHashNotRecorded
// pins the happy path post-S5: an implementor's `/sandman review`
// trigger whose body is NOT in any dedup store (the store is gone)
// still launches a review. The structural sniff does not flag a
// bare `/sandman review` body, so it falls through to `ParseTrigger`
// and the launch path.
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

	tickAndWait(t, d, context.Background())

	if runner.calls != 1 {
		t.Fatalf("MUST launch a review for an implementor trigger (issue #1848), got %d batch runs", runner.calls)
	}
}

// TestDaemon_BotShapedBody_DoesNotReactEvenWithEmptySelfPostStore is
// the AC-level regression pin for the surviving structural sniff
// gate (issue #1821 AC #3). The body carries the
// `## Previous review progress` heading AND the literal
// `/sandman review` trigger substring; the structural sniff is the
// only signal that drops it. Post-S5 the SelfPostStore is gone, so
// the test no longer depends on `d.selfPosts` — only the sniff and
// the absence of an eyes reaction matter.
func TestDaemon_BotShapedBody_DoesNotReactEvenWithEmptySelfPostStore(t *testing.T) {
	now := time.Date(2026, 7, 5, 0, 16, 0, 0, time.UTC)
	botBody := "## Previous review progress\nFirst review pass on PR #1809. Prior activity includes a single `/sandman review` trigger."

	gh := &fakeGH{
		prs: []github.PR{{Number: 1809, State: "open"}},
		comments: map[int][]github.PRComment{
			1809: {
				{ID: "stale-bot", Body: botBody, CreatedAt: now},
			},
		},
		prFetch: map[int]*github.PR{1809: {Number: 1809, Title: "T", Body: "B"}},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now.Add(-1 * time.Minute) }

	tickAndWait(t, d, context.Background())

	if runner.calls != 0 {
		t.Errorf("defence-in-depth: bot-shaped body MUST NOT cause a batch run via structural sniff (issue #1821 AC #3 / #1848); got %d", runner.calls)
	}
	gh.mu.Lock()
	defer gh.mu.Unlock()
	for _, c := range gh.reactionCalls {
		if c.kind == "add_comment" && c.commentID == "stale-bot" {
			t.Errorf("defence-in-depth: daemon MUST NOT add an eyes reaction to a bot-shaped body via structural sniff (issue #1821 AC #3 / #1848); got reaction %+v", c)
		}
	}
}
