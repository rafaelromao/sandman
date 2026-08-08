package cmd

import (
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (&portalRunsView{}).portalBlockedMessage(tt.payload); got != tt.want {
				t.Fatalf("portalBlockedMessage() = %q, want %q", got, tt.want)
			}
		})
	}
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
