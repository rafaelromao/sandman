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
