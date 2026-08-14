package events

import (
	"testing"
	"time"
)

func TestContextRolloverRetryProjection(t *testing.T) {
	now := time.Date(2026, time.August, 14, 12, 0, 0, 0, time.UTC)
	runs := ProjectRunStates([]Event{
		{Type: "run.started", Timestamp: now, RunID: "run-context", Issue: 42, IssueRef: intRef(42), Payload: map[string]any{"branch": "42-context"}},
		{Type: "run.retry", Timestamp: now.Add(time.Second), RunID: "run-context", Issue: 42, IssueRef: intRef(42), Payload: map[string]any{
			"attempt": 2, "max_attempts": 2, "previous_status": "failure", "reason": "context-exhausted", "branch": "42-context",
		}},
		{Type: "run.finished", Timestamp: now.Add(2 * time.Second), RunID: "run-context", Issue: 42, IssueRef: intRef(42), Payload: map[string]any{
			"status": "failure", "branch": "42-context", "retries_total": 1, "retries_done": 1, "context_exhausted": true,
		}},
	})
	if len(runs) != 1 {
		t.Fatalf("projected runs = %d, want 1", len(runs))
	}
	run := runs[0]
	if !run.ContextExhausted() {
		t.Fatal("ContextExhausted() = false, want true")
	}
	if got := run.LastRetryReason(); got != "context-exhausted" {
		t.Fatalf("LastRetryReason() = %q, want context-exhausted", got)
	}
	if got := run.RetriesDone(); got != 1 {
		t.Fatalf("RetriesDone() = %d, want 1", got)
	}
}

func intRef(value int) *int { return &value }
