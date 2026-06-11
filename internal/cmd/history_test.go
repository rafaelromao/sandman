package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
)

func TestHistory_ShowsCompletedRun(t *testing.T) {
	log := &fakeEventLog{
		events: []events.Event{
			{Type: "run.started", Timestamp: time.Now().Add(-10 * time.Minute), RunID: "run-42", Issue: 42},
			{Type: "run.finished", Timestamp: time.Now().Add(-5 * time.Minute), RunID: "run-42", Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-fix-bug"}},
		},
	}

	var buf bytes.Buffer
	cmd := NewHistoryCmd(log)
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
	if !strings.Contains(out, "success") {
		t.Errorf("expected output to contain success, got:\n%s", out)
	}
	if !strings.Contains(out, "sandman/42-fix-bug") {
		t.Errorf("expected output to contain branch, got:\n%s", out)
	}
	if strings.Contains(out, "not yet implemented") {
		t.Errorf("should not show placeholder, got:\n%s", out)
	}
}

func TestHistory_ExcludesIncompleteRuns(t *testing.T) {
	log := &fakeEventLog{
		events: []events.Event{
			{Type: "run.started", Timestamp: time.Now().Add(-10 * time.Minute), RunID: "run-42", Issue: 42},
		},
	}

	var buf bytes.Buffer
	cmd := NewHistoryCmd(log)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, "#42") {
		t.Errorf("expected no completed runs, but output contains #42:\n%s", out)
	}
}

func TestHistory_ShowsPromptOnlyRun(t *testing.T) {
	log := &fakeEventLog{
		events: []events.Event{
			{Type: "run.started", Timestamp: time.Now().Add(-10 * time.Minute), RunID: "run-prompt-only", Payload: map[string]any{"branch": "sandman/return-only-ok-123"}},
			{Type: "run.finished", Timestamp: time.Now().Add(-5 * time.Minute), RunID: "run-prompt-only", Payload: map[string]any{"status": "success", "branch": "sandman/return-only-ok-123"}},
		},
	}

	var buf bytes.Buffer
	cmd := NewHistoryCmd(log)
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
	if !strings.Contains(out, "success") {
		t.Fatalf("expected success status, got:\n%s", out)
	}
}

func TestHistory_ShowsReviewRunWithPRID(t *testing.T) {
	log := &fakeEventLog{
		events: []events.Event{
			{Type: "run.started", Timestamp: time.Now().Add(-10 * time.Minute), RunID: "PR42", Payload: map[string]any{"review": true, "pr_number": 42, "branch": "sandman/review-PR42"}},
			{Type: "run.finished", Timestamp: time.Now().Add(-5 * time.Minute), RunID: "PR42", Payload: map[string]any{"review": true, "pr_number": 42, "status": "success", "branch": "sandman/review-PR42"}},
		},
	}

	var buf bytes.Buffer
	cmd := NewHistoryCmd(log)
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

func TestHistory_ShowsBlockedRun(t *testing.T) {
	log := &fakeEventLog{
		events: []events.Event{
			{Type: "run.blocked", Timestamp: time.Now().Add(-10 * time.Minute), RunID: "run-408", Issue: 408, Payload: map[string]any{"blocked_by": []int{42}}},
		},
	}

	var buf bytes.Buffer
	cmd := NewHistoryCmd(log)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "#408") {
		t.Fatalf("expected blocked run to be listed, got:\n%s", out)
	}
	if !strings.Contains(out, "blocked") {
		t.Fatalf("expected blocked status, got:\n%s", out)
	}
}
