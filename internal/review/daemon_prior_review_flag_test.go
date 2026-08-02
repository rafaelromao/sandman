package review

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
)

// TestDaemon_PriorReviewFlag_NoComments_RendersNO pins the case where the
// PR has only the triggering implementor comment (no prior reviews). The
// daemon must render {{PRIOR_REVIEW_EXISTS}} → "NO" so the review agent
// omits the `## Previous review progress` section (issue #1892).
func TestDaemon_PriorReviewFlag_NoComments_RendersNO(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 42, State: "open"}},
		comments: map[int][]github.PRComment{
			42: {
				{ID: "trigger-1", Body: "/sandman review", CreatedAt: now.Add(1 * time.Hour)},
			},
		},
	}
	runner := &decisionCapturingRunner{capturedRequest: &capturedRequest{}, body: "ok"}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now.Add(-1 * time.Minute) }

	tickAndWait(t, d, context.Background())

	if runner.calls != 1 {
		t.Fatalf("expected 1 batch run, got %d", runner.calls)
	}
	prompt := runner.last.PromptConfig.PromptFlag
	if strings.Contains(prompt, "{{PRIOR_REVIEW_EXISTS}}") {
		t.Errorf("rendered prompt must not retain unfilled {{PRIOR_REVIEW_EXISTS}} placeholder, got prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "deterministic prior-review flag is `NO`") {
		t.Errorf("rendered prompt must substitute {{PRIOR_REVIEW_EXISTS}}=NO when the only comment is the implementor trigger (issue #1892), got prompt:\n%s", prompt)
	}
}

// TestDaemon_PriorReviewFlag_HumanReview_RendersYES pins the case where
// the PR has both the implementor trigger and a prior human review. The
// daemon must render {{PRIOR_REVIEW_EXISTS}} → "YES" (issue #1892).
func TestDaemon_PriorReviewFlag_HumanReview_RendersYES(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 42, State: "open"}},
		comments: map[int][]github.PRComment{
			42: {
				{ID: "human-1", Body: "LGTM, no blockers.", CreatedAt: now.Add(1 * time.Hour)},
				{ID: "trigger-1", Body: "/sandman review focus on tests", CreatedAt: now.Add(2 * time.Hour)},
			},
		},
	}
	runner := &decisionCapturingRunner{capturedRequest: &capturedRequest{}, body: "ok"}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now.Add(-1 * time.Minute) }

	tickAndWait(t, d, context.Background())

	if runner.calls != 1 {
		t.Fatalf("expected 1 batch run, got %d", runner.calls)
	}
	prompt := runner.last.PromptConfig.PromptFlag
	if strings.Contains(prompt, "{{PRIOR_REVIEW_EXISTS}}") {
		t.Errorf("rendered prompt must not retain unfilled {{PRIOR_REVIEW_EXISTS}} placeholder, got prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "deterministic prior-review flag is `YES`") {
		t.Errorf("rendered prompt must substitute {{PRIOR_REVIEW_EXISTS}}=YES when a human review exists (issue #1892), got prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "## Authoritative prior review entries\n\nLGTM, no blockers.") {
		t.Errorf("rendered prompt must supply prior review entries to the daemon-review agent, got prompt:\n%s", prompt)
	}
}

// TestDaemon_PriorReviewFlag_BotSelfPost_RendersYES pins the asymmetric
// contract that bot self-posts (which are excluded by LooksLikeBotReviewBody
// from the trigger list) STILL count as prior reviews for the purpose of
// the `## Previous review progress` section (issue #1892). A PR with only
// a bot-shaped self-post and the implementor trigger renders YES, because
// the bot self-post is a review of the PR (just not a fresh trigger).
func TestDaemon_PriorReviewFlag_BotSelfPost_RendersYES(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 42, State: "open"}},
		comments: map[int][]github.PRComment{
			42: {
				{
					ID:        "bot-1",
					Body:      "## Previous review progress\nFirst pass.\n\n## Summary\nLGTM.",
					CreatedAt: now.Add(1 * time.Hour),
				},
				{ID: "trigger-1", Body: "/sandman review", CreatedAt: now.Add(2 * time.Hour)},
			},
		},
	}
	runner := &decisionCapturingRunner{capturedRequest: &capturedRequest{}, body: "ok"}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now.Add(-1 * time.Minute) }

	tickAndWait(t, d, context.Background())

	if runner.calls != 1 {
		t.Fatalf("expected 1 batch run, got %d", runner.calls)
	}
	prompt := runner.last.PromptConfig.PromptFlag
	if !strings.Contains(prompt, "deterministic prior-review flag is `YES`") {
		t.Errorf("rendered prompt must substitute {{PRIOR_REVIEW_EXISTS}}=YES when a bot self-post exists (counts as a prior review, issue #1892), got prompt:\n%s", prompt)
	}
}

func TestDaemon_PriorReviewContext_IncludesFormalAndInlineHistory(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs:      []github.PR{{Number: 42, State: "open"}},
		comments: map[int][]github.PRComment{42: {{ID: "trigger-1", Body: "/sandman review", CreatedAt: now}}},
		formalReviews: map[int][]github.PRReview{42: {{
			ID: "review-1", State: "CHANGES_REQUESTED", Body: "Please add coverage.", CreatedAt: now,
		}}},
		inlineReviewComments: map[int][]github.PRReviewComment{42: {{
			ID: "inline-1", Path: "internal/review/daemon.go", Line: 42, Body: "Handle this error.", CreatedAt: now,
		}}},
	}
	runner := &decisionCapturingRunner{capturedRequest: &capturedRequest{}, body: "ok"}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now.Add(-time.Minute) }

	tickAndWait(t, d, context.Background())

	prompt := runner.last.PromptConfig.PromptFlag
	for _, want := range []string{
		"### Formal review (CHANGES_REQUESTED)",
		"Please add coverage.",
		"### Inline review (internal/review/daemon.go:42)",
		"Handle this error.",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("rendered prompt must include prior review history %q, got:\n%s", want, prompt)
		}
	}
}

func TestDaemon_PriorReviewContext_FailsClosedOnHistoryError(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	for name, configure := range map[string]func(*fakeGH){
		"formal": func(gh *fakeGH) { gh.formalReviewErr = errors.New("formal history unavailable") },
		"inline": func(gh *fakeGH) { gh.inlineReviewErr = errors.New("inline history unavailable") },
	} {
		t.Run(name, func(t *testing.T) {
			gh := &fakeGH{
				prs:      []github.PR{{Number: 42, State: "open"}},
				comments: map[int][]github.PRComment{42: {{ID: "trigger-1", Body: "/sandman review", CreatedAt: now}}},
			}
			configure(gh)
			runner := &decisionCapturingRunner{capturedRequest: &capturedRequest{}, body: "ok"}
			d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
				DefaultReviewAgent: "opencode",
				DefaultReviewModel: "opencode/foo",
			})
			d.Clock = func() time.Time { return now.Add(-time.Minute) }

			tickAndWait(t, d, context.Background())
			if runner.calls != 0 {
				t.Fatalf("review launched with incomplete %s history", name)
			}
		})
	}
}
