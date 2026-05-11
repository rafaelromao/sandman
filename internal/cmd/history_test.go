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
			{Type: "run.finished", Timestamp: time.Now().Add(-5 * time.Minute), RunID: "run-42", Issue: 42, Payload: map[string]any{"status": "success", "pr_url": "https://github.com/owner/repo/pull/123"}},
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
	if !strings.Contains(out, "https://github.com/owner/repo/pull/123") {
		t.Errorf("expected output to contain PR URL, got:\n%s", out)
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
