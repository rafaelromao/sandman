package batch

import (
	"net"
	"os"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/reviewpub"
)

func TestExpiredReviewRequestResolutionUsesConfirmedTriggerWithoutClassification(t *testing.T) {
	layout := paths.NewLayout(nil, t.TempDir())
	if err := os.MkdirAll(layout.ReviewsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", layout.ReviewSocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	trigger := github.PRComment{ID: "trigger-1001", UpdatedAt: time.Unix(1000, 0)}
	log := &spyEventLog{}
	if err := log.Log(events.Event{
		Type: "run.started", RunID: "review-run", Timestamp: time.Unix(1200, 0),
		Payload: map[string]any{
			"review": true, "pr_number": 17, "branch": "review-17-" + reviewpub.TriggerKey(trigger.ID, trigger.UpdatedAt),
		},
	}); err != nil {
		t.Fatal(err)
	}
	session := &runSession{
		deps: runDeps{
			eventLog: log, layout: layout,
			githubClient: &registrationGitHubClient{comments: []github.PRComment{trigger}},
		},
		opts: runSessionOptions{now: func() time.Time { return time.Unix(3000, 0) }},
	}
	evidence := map[string]any{"review_request": confirmedExpiryProbeRequest()}
	active := session.expiredReviewRequestResolution(t.Context(), 17, "current-sha", evidence)
	if !active.expired || !active.reviewRunObserved || !active.reviewResolverLive {
		t.Fatalf("active review resolution = %+v, want matching live review without a response classification", active)
	}

	session.deps.eventLog = unreadableEventLog{}
	uncertain := session.expiredReviewRequestResolution(t.Context(), 17, "current-sha", evidence)
	if !uncertain.expired || uncertain.reviewRunObserved || !uncertain.reviewResolverLive {
		t.Fatalf("unreadable review resolution = %+v, want to preserve the wait without claiming to have seen a review Run", uncertain)
	}
}
