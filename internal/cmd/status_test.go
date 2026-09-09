package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
)

func TestStatus_ShowsRunningAgent(t *testing.T) {
	log := &fakeEventLog{
		events: []events.Event{
			{Type: "run.started", Timestamp: time.Now().Add(-5 * time.Minute), RunID: "run-42", Issue: 42},
		},
	}

	var buf bytes.Buffer
	cmd := NewStatusCmd(log)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "#42") {
		t.Errorf("expected output to contain #42, got:\n%s", out)
	}
	if strings.Contains(out, "not yet implemented") {
		t.Errorf("should not show placeholder, got:\n%s", out)
	}
}

func TestStatus_NoActiveRuns(t *testing.T) {
	log := &fakeEventLog{
		events: []events.Event{
			{Type: "run.started", Timestamp: time.Now().Add(-10 * time.Minute), RunID: "run-42", Issue: 42},
			{Type: "run.finished", Timestamp: time.Now().Add(-5 * time.Minute), RunID: "run-42", Issue: 42, Payload: map[string]any{"status": "success"}},
		},
	}

	var buf bytes.Buffer
	cmd := NewStatusCmd(log)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, "#42") {
		t.Errorf("expected no active runs, but output contains #42:\n%s", out)
	}
}

func TestStatus_ShowsPromptOnlyRun(t *testing.T) {
	log := &fakeEventLog{
		events: []events.Event{
			{Type: "run.started", Timestamp: time.Now().Add(-5 * time.Minute), RunID: "run-prompt-only", Payload: map[string]any{"branch": "return-only-ok-123"}},
		},
	}

	var buf bytes.Buffer
	cmd := NewStatusCmd(log)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "prompt-only") {
		t.Fatalf("expected prompt-only label, got:\n%s", out)
	}
}

func TestStatus_ShowsReviewRunWithPRID(t *testing.T) {
	log := &fakeEventLog{
		events: []events.Event{
			{Type: "run.started", Timestamp: time.Now().Add(-5 * time.Minute), RunID: "PR42", Payload: map[string]any{"review": true, "pr_number": 42, "branch": "review-PR42"}},
		},
	}

	var buf bytes.Buffer
	cmd := NewStatusCmd(log)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "PR42") {
		t.Fatalf("expected review run to show PR42, got:\n%s", out)
	}
	if strings.Contains(out, "prompt-only") {
		t.Fatalf("expected review run NOT to show prompt-only, got:\n%s", out)
	}
}

func TestStatus_ExcludesBlockedRuns(t *testing.T) {
	log := &fakeEventLog{
		events: []events.Event{
			{Type: "run.blocked", Timestamp: time.Now().Add(-5 * time.Minute), RunID: "run-408", Issue: 408, Payload: map[string]any{"blocked_by": []int{42}}},
		},
	}

	var buf bytes.Buffer
	cmd := NewStatusCmd(log)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, "#408") {
		t.Fatalf("expected blocked run to be excluded, got:\n%s", out)
	}
	if !strings.Contains(out, "No active runs") {
		t.Fatalf("expected no active runs message, got:\n%s", out)
	}
}

func TestStatus_UsesActiveDurationWhileAwaiting(t *testing.T) {
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	awaitAt := startedAt.Add(5 * time.Minute)
	log := &fakeEventLog{
		events: []events.Event{
			{Type: "run.started", Timestamp: startedAt, RunID: "run-status-await", Issue: 42},
			{Type: "run.await", Timestamp: awaitAt, RunID: "run-status-await", Issue: 42},
		},
	}

	var buf bytes.Buffer
	cmd := newStatusCmd(log, func() time.Time { return awaitAt.Add(time.Hour) })
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, "elapsed 5m0s") {
		t.Fatalf("status duration = %q, want frozen 5m0s", got)
	}
}
