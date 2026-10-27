package batch

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/reviewpub"
)

func TestObserveCurrentReviewEvidence_IgnoresMismatchedDaemonPublication(t *testing.T) {
	triggeredAt := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	trigger := github.PRComment{
		ID:        "https://github.com/owner/repo/pull/17#issuecomment-1001",
		Body:      "/sandman review",
		CreatedAt: triggeredAt,
		UpdatedAt: triggeredAt,
	}
	triggerKey := reviewpub.TriggerKey(trigger.ID, trigger.UpdatedAt)
	stale := github.PRComment{
		ID:          "stale-report",
		AuthorLogin: "sandman-bot",
		Body:        reviewpub.Body("## Summary\nStale approval.\n\n## Decision\n**APPROVED**", triggerKey, "older-head"),
		CreatedAt:   triggeredAt.Add(time.Minute),
	}
	current := github.PRComment{
		ID:          "current-report",
		AuthorLogin: "sandman-bot",
		Body: reviewpub.Body("## Summary\nThe change needs a correction.\n\n## Findings\n- `worker.go:12`: fix the retry condition.\n\n## Decision\n**CHANGES_REQUESTED**",
			triggerKey, "head-1"),
		CreatedAt: triggeredAt.Add(2 * time.Minute),
	}
	client := &registrationGitHubClient{comments: []github.PRComment{trigger, stale, current}}
	session := &runSession{
		deps: runDeps{githubClient: client},
		opts: runSessionOptions{reviewRegistrationNow: func() time.Time { return triggeredAt.Add(3 * time.Minute) }},
	}
	request := reviewRequestEnvelope{
		Protocol:            "review-wait/v1",
		Repository:          "owner/repo",
		PullRequest:         17,
		HeadSHA:             "head-1",
		TriggerID:           trigger.ID,
		TriggerPrefix:       "/sandman review",
		TriggerCreatedAt:    triggeredAt.Format(time.RFC3339Nano),
		ConfirmedAt:         triggeredAt.Format(time.RFC3339Nano),
		StartedAt:           triggeredAt.Format(time.RFC3339Nano),
		DeadlineAt:          fmt.Sprintf("unix:%d", triggeredAt.Add(30*time.Minute).Unix()),
		StartedUnixSeconds:  int(triggeredAt.Unix()),
		DeadlineUnixSeconds: int(triggeredAt.Add(30 * time.Minute).Unix()),
		EffectiveTimeout:    1800,
		PollPlan:            []int{120, 60, 60, 30},
	}

	handoff, err := session.observeCurrentReviewEvidence(context.Background(), request, &github.PR{Number: 17, HeadRefOid: "head-1"}, client.comments)
	if err != nil {
		t.Fatalf("observe review evidence: %v", err)
	}
	if handoff == nil || handoff.Classification == nil {
		t.Fatal("review handoff = nil, want current-head report evidence")
	}
	evidence := handoff.Classification.informalFeedbackEvidenceFor(request, handoff.Classification.WindowEnd)
	if len(evidence) != 1 || evidence[0].ID != current.ID {
		t.Fatalf("informal feedback = %+v, want only the current-head report %q", evidence, current.ID)
	}
}
