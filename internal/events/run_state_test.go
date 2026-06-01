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
