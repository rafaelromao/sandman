package cmd

import (
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
)

// TestKindForRun_BlockedStateReturnsCompleted is the unit-level tracer
// bullet for issue #1699. A run whose only event is a `run.blocked`
// must project as kind="completed" so the portal's "Active Batches"
// filter and active-row CSS chrome do not light up on a row that has no
// live daemon. The status field stays "blocked" so the wait-state badge
// and the `row-non-expandable` class (keyed off status, not kind) still
// render.
func TestKindForRun_BlockedStateReturnsCompleted(t *testing.T) {
	ts := time.Date(2026, 7, 2, 21, 44, 14, 0, time.UTC)
	blocked := events.RunState{
		RunID: "b7cf-260702214402-1641",
		Started: events.Event{
			Type:      "run.blocked",
			RunID:     "b7cf-260702214402-1641",
			Timestamp: ts,
			Issue:     1641,
			Payload:   map[string]any{"blocked_by": []int{1640}},
		},
		Finished: &events.Event{
			Type:      "run.blocked",
			RunID:     "b7cf-260702214402-1641",
			Timestamp: ts,
			Issue:     1641,
			Payload:   map[string]any{"blocked_by": []int{1640}},
		},
	}
	if got := (&portalRunsView{}).kindForRun(blocked); got != "completed" {
		t.Fatalf("kindForRun(blocked) = %q, want %q", got, "completed")
	}
}

// TestKindForRun_QueuedStateReturnsCompleted mirrors the blocked case for
// the queued status. A run that is sitting in the wait queue (no
// terminal event yet, no live socket) is not active; kind must be
// "completed" so the active batches filter excludes it.
func TestKindForRun_QueuedStateReturnsCompleted(t *testing.T) {
	ts := time.Date(2026, 7, 2, 21, 44, 13, 0, time.UTC)
	queued := events.RunState{
		RunID: "b7cf-260702214402-1640",
		Started: events.Event{
			Type:      "run.queued",
			RunID:     "b7cf-260702214402-1640",
			Timestamp: ts,
			Issue:     1640,
			Payload:   map[string]any{"blocked_by": nil},
		},
		Finished: &events.Event{
			Type:      "run.queued",
			RunID:     "b7cf-260702214402-1640",
			Timestamp: ts,
			Issue:     1640,
			Payload:   map[string]any{"blocked_by": nil},
		},
	}
	if got := (&portalRunsView{}).kindForRun(queued); got != "completed" {
		t.Fatalf("kindForRun(queued) = %q, want %q", got, "completed")
	}
}

// TestKindForRun_TrulyActiveStateReturnsActive locks in the positive
// path: a run whose state has no Finished event is still active and
// must project as kind="active". The fix for #1699 must not regress
// this contract.
func TestKindForRun_TrulyActiveStateReturnsActive(t *testing.T) {
	ts := time.Date(2026, 7, 2, 21, 44, 16, 0, time.UTC)
	active := events.RunState{
		RunID: "b7cf-260702214402-1640",
		Started: events.Event{
			Type:      "run.started",
			RunID:     "b7cf-260702214402-1640",
			Timestamp: ts,
			Issue:     1640,
			Payload:   map[string]any{"branch": "sandman/1640-fix"},
		},
	}
	if got := (&portalRunsView{}).kindForRun(active); got != "active" {
		t.Fatalf("kindForRun(active) = %q, want %q", got, "active")
	}
}

// TestKindForRun_TerminalSuccessReturnsCompleted locks in the terminal
// positive path: a run whose Finished event has status="success" is
// completed and must project as kind="completed". Without this guard a
// future refactor could conflate "completed" with "aborted" or drop the
// default branch entirely.
func TestKindForRun_TerminalSuccessReturnsCompleted(t *testing.T) {
	started := time.Date(2026, 7, 2, 21, 44, 16, 0, time.UTC)
	finished := started.Add(2 * time.Minute)
	terminal := events.RunState{
		RunID: "b7cf-260702214402-1640",
		Started: events.Event{
			Type:      "run.started",
			RunID:     "b7cf-260702214402-1640",
			Timestamp: started,
			Issue:     1640,
		},
		Finished: &events.Event{
			Type:      "run.finished",
			RunID:     "b7cf-260702214402-1640",
			Timestamp: finished,
			Issue:     1640,
			Payload:   map[string]any{"status": "success"},
		},
	}
	if got := (&portalRunsView{}).kindForRun(terminal); got != "completed" {
		t.Fatalf("kindForRun(success) = %q, want %q", got, "completed")
	}
}
