package review

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
)

func TestDaemonIgnoresAnsweredRequestAndProcessesFreshRequest(t *testing.T) {
	const prNumber = 2737
	now := time.Date(2026, time.September, 26, 13, 0, 0, 0, time.UTC)
	request := github.PRComment{
		ID:          "request-1",
		Body:        "/sandman review",
		AuthorLogin: "sandman",
		CreatedAt:   now,
	}
	response := github.PRComment{
		ID:          "response-1",
		Body:        "## Summary\nThe review is complete.\n\n## Decision\n**APPROVED**",
		AuthorLogin: "sandman",
		CreatedAt:   now.Add(time.Minute),
	}
	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open", UpdatedAt: response.CreatedAt}},
		comments: map[int][]github.PRComment{
			prNumber: {request, response},
		},
		prFetch: map[int]*github.PR{prNumber: {Number: prNumber, Title: "T", Body: "B"}},
	}
	runner := newDecisionRunner()
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.authenticatedLogin = "sandman"

	tickAndWait(t, d, context.Background())
	if got := runner.Calls(); got != 0 {
		t.Fatalf("answered request launched %d review runs, want 0", got)
	}

	newRequest := github.PRComment{
		ID:          "request-2",
		Body:        "/sandman review focus on the new change",
		AuthorLogin: "sandman",
		CreatedAt:   now.Add(2 * time.Minute),
	}
	gh.mu.Lock()
	gh.comments[prNumber] = []github.PRComment{request, response, newRequest}
	gh.prs[0].UpdatedAt = newRequest.CreatedAt
	gh.mu.Unlock()

	tickAndWait(t, d, context.Background())
	if got := runner.Calls(); got != 1 {
		t.Fatalf("fresh request launched %d review runs, want 1", got)
	}
	runner.mu.Lock()
	focus := runner.last.ReviewFocus
	runner.mu.Unlock()
	if focus != "focus on the new change" {
		t.Fatalf("fresh request focus = %q, want %q", focus, "focus on the new change")
	}
}

func TestDaemonRefreshesTerminalStateWrittenByAnotherInstance(t *testing.T) {
	const (
		prNumber  = 2737
		commentID = "request-1"
		batchID   = "20260926-abc-PR2737"
	)
	dir := t.TempDir()
	t.Chdir(dir)
	seedPriorReviewEntry(t, dir, batchID, prNumber, commentID, "pending")
	updatedAt := time.Date(2026, time.September, 26, 13, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open", UpdatedAt: updatedAt}},
		comments: map[int][]github.PRComment{
			prNumber: {{ID: commentID, Body: "/sandman review", AuthorLogin: "sandman", CreatedAt: updatedAt}},
		},
	}
	cfg := &config.Config{DefaultReviewAgent: "opencode", DefaultReviewModel: "opencode/foo"}
	first := New(dir, gh, &prompt.Engine{}, &capturedRequest{}, cfg, &lockedBuffer{}, 0, false, nil)
	secondRunner := &capturedRequest{}
	second := New(dir, gh, &prompt.Engine{}, secondRunner, cfg, &lockedBuffer{}, 0, false, nil)
	second.authenticatedLogin = "sandman"
	second.markCommentsRead(gh.prs[0])

	statePath := filepath.Join(dir, "batches", batchID, "runs", deriveReviewRowID(batchID, prNumber), "review-state.json")
	state, err := NewReviewStateStore(statePath, prNumber, first)
	if err != nil {
		t.Fatalf("open shared review state: %v", err)
	}
	if err := state.MarkSeen(commentID, "success"); err != nil {
		t.Fatalf("persist terminal state from first daemon: %v", err)
	}

	tickAndWait(t, second, context.Background())
	if !second.IsTerminalSeen(prNumber, commentID) {
		t.Fatal("second daemon did not observe terminal state written after its cache was hydrated")
	}
	if err := second.processPR(context.Background(), prNumber); err != nil {
		t.Fatalf("re-evaluate terminal request: %v", err)
	}
	if got := secondRunner.Calls(); got != 0 {
		t.Fatalf("shared terminal request launched %d review runs, want 0", got)
	}
}

func TestDaemonDoesNotTreatUnrelatedLaterCommentAsReviewResponse(t *testing.T) {
	now := time.Date(2026, time.September, 26, 13, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		author string
		body   string
	}{
		{name: "ordinary comment by daemon account", author: "sandman", body: "I will take a look."},
		{name: "review-shaped comment by another account", author: "reviewer", body: "## Summary\nReviewed.\n\n## Decision\n**APPROVED**"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const prNumber = 2738
			gh := &fakeGH{
				prs: []github.PR{{Number: prNumber, State: "open", UpdatedAt: now.Add(time.Minute)}},
				comments: map[int][]github.PRComment{
					prNumber: {
						{ID: "request", Body: "/sandman review", AuthorLogin: "sandman", CreatedAt: now},
						{ID: "later", Body: tc.body, AuthorLogin: tc.author, CreatedAt: now.Add(time.Minute)},
					},
				},
			}
			runner := newDecisionRunner()
			d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
				DefaultReviewAgent: "opencode",
				DefaultReviewModel: "opencode/foo",
			})
			d.authenticatedLogin = "sandman"

			tickAndWait(t, d, context.Background())
			if got := runner.Calls(); got != 1 {
				t.Fatalf("unrelated later comment suppressed the request: RunBatch calls = %d, want 1", got)
			}
		})
	}
}

func TestDaemonProcessesEditedTriggerWhenPriorResponsePredatesEdit(t *testing.T) {
	const prNumber = 2739
	createdAt := time.Date(2026, time.September, 26, 13, 0, 0, 0, time.UTC)
	editedAt := createdAt.Add(2 * time.Minute)
	comment := github.PRComment{
		ID:          "request",
		Body:        "/sandman review focus on the edited request",
		AuthorLogin: "sandman",
		CreatedAt:   createdAt,
		UpdatedAt:   editedAt,
	}
	response := github.PRComment{
		ID:          "response",
		Body:        "## Summary\nReviewed the earlier request.\n\n## Decision\n**APPROVED**",
		AuthorLogin: "sandman",
		CreatedAt:   createdAt.Add(time.Minute),
	}
	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open", UpdatedAt: editedAt}},
		comments: map[int][]github.PRComment{
			prNumber: {comment, response},
		},
	}
	runner := newDecisionRunner()
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.authenticatedLogin = "sandman"

	tickAndWait(t, d, context.Background())
	if got := runner.Calls(); got != 1 {
		t.Fatalf("edited trigger was suppressed by a response to its earlier revision: RunBatch calls = %d, want 1", got)
	}
	runner.mu.Lock()
	focus := runner.last.ReviewFocus
	runner.mu.Unlock()
	if focus != "focus on the edited request" {
		t.Fatalf("edited trigger focus = %q, want %q", focus, "focus on the edited request")
	}
}
