package cmd

import (
	"testing"
	"time"
)

func TestAttemptsAndLastRetryReasonFromEvents_NoRetriesReturnsZero(t *testing.T) {
	got, reason := attemptsAndLastRetryReasonFromEvents(nil)
	if got != 0 {
		t.Fatalf("attempts = %d, want 0 for empty slice", got)
	}
	if reason != "" {
		t.Fatalf("reason = %q, want empty for empty slice", reason)
	}
}

func TestAttemptsAndLastRetryReasonFromEvents_SingleRetryReturnsOne(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	events := []portalEvent{
		{Type: "run.retry", Timestamp: now, Payload: map[string]any{"attempt": 2, "reason": "agent-stalled"}},
	}
	got, reason := attemptsAndLastRetryReasonFromEvents(events)
	if got != 1 {
		t.Fatalf("attempts = %d, want 1 (one retry has occurred; payload attempt=2 maps to retry count 1)", got)
	}
	if reason != "agent-stalled" {
		t.Fatalf("reason = %q, want %q", reason, "agent-stalled")
	}
}

func TestAttemptsAndLastRetryReasonFromEvents_MalformedZeroAttemptClampsAtZero(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	events := []portalEvent{
		{Type: "run.retry", Timestamp: now, Payload: map[string]any{"attempt": 0}},
	}
	got, reason := attemptsAndLastRetryReasonFromEvents(events)
	if got != 0 {
		t.Fatalf("attempts = %d, want 0 (malformed attempt=0 must clamp to non-negative)", got)
	}
	if got < 0 {
		t.Fatalf("attempts = %d, must never return a negative value", got)
	}
	if reason != "" {
		t.Fatalf("reason = %q, want empty when retry payload omits reason", reason)
	}
}

func TestAttemptsAndLastRetryReasonFromEvents_TwoRetriesReturnTwo(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	events := []portalEvent{
		{Type: "run.retry", Timestamp: now, Payload: map[string]any{"attempt": 2, "reason": "agent-stalled"}},
		{Type: "run.retry", Timestamp: now.Add(1 * time.Minute), Payload: map[string]any{"attempt": 3, "reason": "build-failed"}},
	}
	got, reason := attemptsAndLastRetryReasonFromEvents(events)
	if got != 2 {
		t.Fatalf("attempts = %d, want 2 (two retries have occurred; max(attempt)=3, retry count = 3-1 = 2)", got)
	}
	if reason != "build-failed" {
		t.Fatalf("reason = %q, want %q (most recent retry reason)", reason, "build-failed")
	}
}
