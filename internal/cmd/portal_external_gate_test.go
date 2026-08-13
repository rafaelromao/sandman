package cmd

import (
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
)

func TestExternalGate_PortalBlockedMessageDistinguishesGate(t *testing.T) {
	tests := []struct {
		name    string
		payload map[string]any
		want    string
	}{
		{
			name:    "pending",
			payload: map[string]any{"blocker": "external-gate", "gate": "pending"},
			want:    "Blocked while waiting for the external CI/review gate.",
		},
		{
			name:    "failed",
			payload: map[string]any{"blocker": "external-gate", "gate": "failed"},
			want:    "Blocked by a failed external gate.",
		},
		{
			name:    "unavailable",
			payload: map[string]any{"blocker": "external-gate", "gate": "unavailable"},
			want:    "External gate unavailable; verify the pull request and its CI/review state.",
		},
		{
			name:    "unverified",
			payload: map[string]any{"blocker": "external-gate", "gate": "unverified"},
			want:    "Merged pull request could not be verified; confirm its closing reference.",
		},
		{
			name:    "ready to merge",
			payload: map[string]any{"blocker": "external-gate", "gate": "ready-to-merge"},
			want:    "Pull request ready to merge; revalidate current-head approval, CI, and mergeability before executing the normal merge gate.",
		},
		{
			name:    "review timeout",
			payload: map[string]any{"blocker": "external-gate", "gate": "review-timeout"},
			want:    "Delegated review request timed out; inspect the retained request and continue after a new confirmed trigger or resolved pull-request gate.",
		},
		{
			name:    "review timeout state error",
			payload: map[string]any{"blocker": "external-gate", "gate": "review-timeout-state-error"},
			want:    "Retained delegated-review request state is invalid; repair or remove it and confirm a new review trigger before continuing.",
		},
		{
			name:    "actionable feedback",
			payload: map[string]any{"blocker": "external-gate", "gate": "actionable-feedback"},
			want:    "Delegated review requested changes; inspect the retained evidence, address the feedback, and continue after pushing a new current head.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (&portalRunsView{}).portalBlockedMessage(tt.payload); got != tt.want {
				t.Fatalf("portalBlockedMessage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAggregateReviewChildren_PreservesExternalGateParent(t *testing.T) {
	startedAt := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	liveReview := portalRun{
		IssueNumber: 2496, Review: true, RunID: "review-live", Key: "review-live",
		Kind: "active", Status: "reviewing", StartedAt: startedAt,
	}
	parent := portalRun{
		IssueNumber: 2496, RunID: "implementation", Key: "implementation",
		Kind: "completed", Status: "blocked", StartedAt: startedAt.Add(-time.Minute),
		externalGate: true,
	}

	runs := (&portalRunsView{}).aggregateReviewChildren(paths.NewLayout(nil, t.TempDir()), []portalRun{parent, liveReview})
	for _, run := range runs {
		if run.RunID == parent.RunID {
			if run.Status != "blocked" {
				t.Fatalf("external-gate parent status = %q, want blocked", run.Status)
			}
			return
		}
	}
	t.Fatalf("external-gate parent missing from aggregate output: %#v", runs)
}

func TestExternalGate_PortalActiveBatchPreservesState(t *testing.T) {
	startedAt := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(time.Minute)
	state := events.RunState{
		RunID: "run-42",
		Started: events.Event{
			Timestamp: startedAt,
			Issue:     42,
			Payload:   map[string]any{},
		},
		Finished: &events.Event{
			Type:      "run.finished",
			Timestamp: finishedAt,
			Issue:     42,
			Payload: map[string]any{
				"status":  "blocked",
				"blocker": "external-gate",
				"gate":    "pending",
			},
		},
	}
	active := portalActiveRun{
		RunID:        "run-42",
		IssueNumbers: []int{42},
		StartedAt:    startedAt,
		ModTime:      startedAt,
	}

	eventsByRun := map[string][]portalEvent{
		"run-42": {{Type: "run.finished", Timestamp: startedAt.Add(time.Minute), Payload: state.Finished.Payload}},
	}
	runs, _ := (&portalRunsView{}).runsFromActiveBatch("", active, []events.RunState{state}, nil, eventsByRun, nil)
	if len(runs) != 1 {
		t.Fatalf("runsFromActiveBatch() returned %d runs, want 1", len(runs))
	}
	if runs[0].Status != "blocked" {
		t.Fatalf("active external-gate status = %q, want blocked", runs[0].Status)
	}
	if runs[0].Log != "Blocked while waiting for the external CI/review gate." {
		t.Fatalf("active external-gate log = %q, want external-gate message", runs[0].Log)
	}
}

func TestExternalGate_PortalReviewTimeoutPreservesRequestDetails(t *testing.T) {
	startedAt := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	state := events.RunState{
		RunID: "run-42",
		Started: events.Event{
			Timestamp: startedAt,
			Issue:     42,
			Payload:   map[string]any{"branch": "42-fix-bug"},
		},
		Finished: &events.Event{
			Type:      "run.finished",
			Timestamp: startedAt.Add(time.Minute),
			Issue:     42,
			Payload: map[string]any{
				"status":      "blocked",
				"blocker":     "external-gate",
				"gate":        "review-timeout",
				"reason":      "REVIEW_TIMEOUT",
				"next_action": "inspect the retained request",
				"review_request": map[string]any{
					"pull_request":              17,
					"head_sha":                  "current-sha",
					"trigger_id":                "trigger-1001",
					"deadline_unix_seconds":     2800,
					"effective_timeout_seconds": 1800,
					"response_counts":           map[string]any{"top_level": 0, "formal_reviews": 0, "inline_comments": 0},
				},
			},
		},
	}
	active := portalActiveRun{RunID: "run-42", IssueNumbers: []int{42}, StartedAt: startedAt, ModTime: startedAt}

	eventsByRun := map[string][]portalEvent{
		"run-42": {{Type: "run.finished", Timestamp: startedAt.Add(time.Minute), Payload: state.Finished.Payload}},
	}
	runs, _ := (&portalRunsView{}).runsFromActiveBatch("", active, []events.RunState{state}, nil, eventsByRun, nil)
	if len(runs) != 1 {
		t.Fatalf("runsFromActiveBatch() returned %d runs, want 1", len(runs))
	}
	if runs[0].Status != "blocked" || runs[0].Log != "Delegated review request timed out; inspect the retained request and continue after a new confirmed trigger or resolved pull-request gate." {
		t.Fatalf("portal timeout projection = status %q/log %q", runs[0].Status, runs[0].Log)
	}
	if !runs[0].externalGate {
		t.Fatal("portal timeout projection did not retain external-gate marker")
	}
	if len(runs[0].Events) != 1 || runs[0].Events[0].Payload["review_request"] == nil {
		t.Fatalf("portal timeout events = %#v, want retained review_request payload", runs[0].Events)
	}
}
