package events

import (
	"testing"
	"time"
)

func TestProjectRunStates_PreservesPromptOnlyRun(t *testing.T) {
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Minute)

	runs := ProjectRunStates([]Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-prompt", Payload: map[string]any{"branch": "sandman/prompt-only-123"}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: "run-prompt", Payload: map[string]any{"status": "success", "branch": "sandman/prompt-only-123"}},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	run := runs[0]
	if got := run.IssueLabel(); got != "prompt-only" {
		t.Fatalf("expected prompt-only label, got %q", got)
	}
	if got := run.Status(); got != "success" {
		t.Fatalf("expected success status, got %q", got)
	}
	if got := run.Branch(); got != "sandman/prompt-only-123" {
		t.Fatalf("expected branch sandman/prompt-only-123, got %q", got)
	}
	if got := run.Duration(); got != 2*time.Minute {
		t.Fatalf("expected 2m duration, got %s", got)
	}
}

func TestProjectRunStates_IncludesContinuedRun(t *testing.T) {
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	continuedAt := startedAt.Add(5 * time.Minute)
	finishedAt := continuedAt.Add(2 * time.Minute)

	runs := ProjectRunStates([]Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-1", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.continued", Timestamp: continuedAt, RunID: "run-2", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: "run-2", Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-fix"}},
	})

	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(runs))
	}

	byID := map[string]RunState{}
	for _, run := range runs {
		byID[run.RunID] = run
	}

	continued, ok := byID["run-2"]
	if !ok {
		t.Fatal("expected continued run in projected set")
	}
	if got := continued.IssueLabel(); got != "#42" {
		t.Fatalf("expected issue label #42, got %q", got)
	}
	if got := continued.Status(); got != "success" {
		t.Fatalf("expected success status, got %q", got)
	}
	if got := continued.Branch(); got != "sandman/42-fix" {
		t.Fatalf("expected branch sandman/42-fix, got %q", got)
	}
	if got := continued.Duration(); got != 2*time.Minute {
		t.Fatalf("expected 2m duration, got %s", got)
	}
}

func TestProjectRunStates_TreatsBlockedRunAsTerminal(t *testing.T) {
	blockedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	runs := ProjectRunStates([]Event{
		{Type: "run.blocked", Timestamp: blockedAt, RunID: "run-blocked", Issue: 408, Payload: map[string]any{"blocked_by": []int{123}}},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	run := runs[0]
	if run.IsActive() {
		t.Fatal("expected blocked run to be terminal")
	}
	if got := run.Status(); got != "blocked" {
		t.Fatalf("expected blocked status, got %q", got)
	}
	if got := run.IssueLabel(); got != "#408" {
		t.Fatalf("expected issue label #408, got %q", got)
	}
}

func TestProjectRunStates_TreatsAbortedRunAsTerminalAborted(t *testing.T) {
	abortedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	runs := ProjectRunStates([]Event{
		{Type: "run.started", Timestamp: abortedAt.Add(-1 * time.Minute), RunID: "run-aborted", Issue: 408},
		{Type: "run.aborted", Timestamp: abortedAt, RunID: "run-aborted", Issue: 408, Payload: map[string]any{"status": "aborted"}},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	run := runs[0]
	if run.IsActive() {
		t.Fatal("expected aborted run to be terminal")
	}
	if got := run.Status(); got != "aborted" {
		t.Fatalf("expected aborted status, got %q", got)
	}
	if got := run.IssueLabel(); got != "#408" {
		t.Fatalf("expected issue label #408, got %q", got)
	}
}

func TestProjectRunStates_LegacyCancelledEventStillProjectsAsAborted(t *testing.T) {
	legacyAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	runs := ProjectRunStates([]Event{
		{Type: "run.started", Timestamp: legacyAt.Add(-1 * time.Minute), RunID: "run-cancelled", Issue: 408},
		{Type: "run.cancelled", Timestamp: legacyAt, RunID: "run-cancelled", Issue: 408, Payload: map[string]any{"status": "failure"}},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	run := runs[0]
	if run.IsActive() {
		t.Fatal("expected legacy cancelled run to be terminal")
	}
	if got := run.Status(); got != "aborted" {
		t.Fatalf("expected aborted status, got %q", got)
	}
	if got := run.IssueLabel(); got != "#408" {
		t.Fatalf("expected issue label #408, got %q", got)
	}
}

func TestProjectRunStates_UnfinishedRunHasEmptyStatus(t *testing.T) {
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	runs := ProjectRunStates([]Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-active", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	run := runs[0]
	if !run.IsActive() {
		t.Fatal("expected unfinished run to be active")
	}
	if got := run.Status(); got != "" {
		t.Fatalf("expected empty status for unfinished run, got %q", got)
	}
}

func TestProjectRunStates_UnknownPayloadStatusRoundTripsThroughStatus(t *testing.T) {
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Minute)

	t.Run("named unknown status round-trips verbatim", func(t *testing.T) {
		runs := ProjectRunStates([]Event{
			{Type: "run.started", Timestamp: startedAt, RunID: "run-quirky", Issue: 99, Payload: map[string]any{"branch": "sandman/99-fix"}},
			{Type: "run.finished", Timestamp: finishedAt, RunID: "run-quirky", Issue: 99, Payload: map[string]any{"status": "timeout", "branch": "sandman/99-fix"}},
		})

		if len(runs) != 1 {
			t.Fatalf("expected 1 run, got %d", len(runs))
		}
		if got := runs[0].Status(); got != "timeout" {
			t.Fatalf("expected payload status to round-trip verbatim, got %q", got)
		}
	})

	t.Run("missing status key maps to empty string", func(t *testing.T) {
		runs := ProjectRunStates([]Event{
			{Type: "run.started", Timestamp: startedAt, RunID: "run-nokey", Issue: 100, Payload: map[string]any{"branch": "sandman/100-fix"}},
			{Type: "run.finished", Timestamp: finishedAt, RunID: "run-nokey", Issue: 100, Payload: map[string]any{"branch": "sandman/100-fix"}},
		})

		if len(runs) != 1 {
			t.Fatalf("expected 1 run, got %d", len(runs))
		}
		if got := runs[0].Status(); got != "" {
			t.Fatalf("expected empty status when payload has no status key, got %q", got)
		}
	})
}

func TestProjectRunStates_ReviewRunLabel(t *testing.T) {
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Minute)

	runs := ProjectRunStates([]Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "PR42", Payload: map[string]any{"review": true, "pr_number": 42, "branch": "sandman/review-PR42"}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: "PR42", Payload: map[string]any{"review": true, "pr_number": 42, "status": "success", "branch": "sandman/review-PR42"}},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	run := runs[0]
	if got := run.IssueLabel(); got != "PR42" {
		t.Fatalf("expected review run label PR42, got %q", got)
	}
	if got := run.IsReview(); !got {
		t.Fatal("expected IsReview() == true for review run")
	}
	if got := run.IssueNumber(); got != 0 {
		t.Fatalf("expected IssueNumber 0 for review run, got %d", got)
	}
	if got := run.RunID; got != "PR42" {
		t.Fatalf("expected RunID PR42, got %q", got)
	}
}

func TestProjectRunStates_IdleTimeoutEventDoesNotBreakProjection(t *testing.T) {
	idleTimeoutAt := time.Date(2025, 1, 1, 12, 5, 0, 0, time.UTC)
	abortedAt := time.Date(2025, 1, 1, 12, 6, 0, 0, time.UTC)

	runs := ProjectRunStates([]Event{
		{Type: "run.started", Timestamp: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC), RunID: "run-idle", Issue: 42},
		{Type: "run.idle_timeout", Timestamp: idleTimeoutAt, RunID: "run-idle", Issue: 42, Payload: map[string]any{
			"issue":                42,
			"idle_seconds":         300.0,
			"idle_timeout_seconds": 1800,
			"attempt":              1,
		}},
		{Type: "run.aborted", Timestamp: abortedAt, RunID: "run-idle", Issue: 42, Payload: map[string]any{"status": "aborted"}},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	run := runs[0]
	if run.IsActive() {
		t.Fatal("expected run to be terminal after run.aborted")
	}
	if got := run.Status(); got != "aborted" {
		t.Fatalf("expected aborted status, got %q", got)
	}
	if got := run.IssueLabel(); got != "#42" {
		t.Fatalf("expected issue label #42, got %q", got)
	}
	if got := run.Duration(); got != 6*time.Minute {
		t.Fatalf("expected 6m duration, got %s", got)
	}
}
