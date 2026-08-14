package cmd

import (
	"testing"
	"time"
)

func TestContextRolloverRetryPortalProjection(t *testing.T) {
	attempts, reason := attemptsAndLastRetryReasonFromEvents([]portalEvent{
		{Type: "run.retry", Timestamp: time.Now(), Payload: map[string]any{
			"attempt": 2,
			"reason":  "context-exhausted",
		}},
	})
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if reason != "context-exhausted" {
		t.Fatalf("reason = %q, want context-exhausted", reason)
	}
}
