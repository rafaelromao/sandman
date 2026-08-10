package cmd

import (
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
)

func TestPortalBlockedMessage_DistinguishesExternalGate(t *testing.T) {
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

func TestPortalActiveBatchPreservesExternalGateState(t *testing.T) {
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

	runs, _ := (&portalRunsView{}).runsFromActiveBatch("", active, []events.RunState{state}, nil, nil, nil)
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
