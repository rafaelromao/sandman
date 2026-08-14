package prompt

import (
	"strings"
	"testing"
)

func TestContextRecoveryTask(t *testing.T) {
	original := "# Task\n\n## Work\n\nKeep the existing checkpoint.\n"
	got := ContextRecoveryTaskPrompt(original, 600)
	if !strings.Contains(got, "Keep the existing checkpoint.") {
		t.Fatal("recovery Task lost the original content")
	}
	if !strings.Contains(got, "## Continuation Freshness Guard") {
		t.Fatal("recovery Task lacks the continuation freshness guard")
	}
	if !strings.Contains(got, "## Context Recovery Guard") {
		t.Fatal("recovery Task lacks the context recovery guard")
	}
	for _, want := range []string{"reconstruct the handoff", "durable checkpoint", "before implementation"} {
		if !strings.Contains(strings.ToLower(got), want) {
			t.Errorf("recovery Task lacks %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "Delegated review response timeout: `600` seconds") {
		t.Fatal("recovery Task lacks the current review timeout")
	}

	repeated := ContextRecoveryTaskPrompt(got, 600)
	if strings.Count(repeated, "## Context Recovery Guard") != 1 {
		t.Fatalf("recovery guard was not idempotent:\n%s", repeated)
	}
	if repeated != got {
		t.Fatalf("recovery Task changed when composed twice:\nfirst:\n%s\nsecond:\n%s", got, repeated)
	}

	checkpointed := got + "\n## Recovery Checkpoint\n\nThe durable handoff is complete.\n"
	repeatedCheckpoint := ContextRecoveryTaskPrompt(checkpointed, 600)
	if !strings.Contains(repeatedCheckpoint, "The durable handoff is complete.") {
		t.Fatalf("recomposing recovery Task discarded its checkpoint:\n%s", repeatedCheckpoint)
	}
}
