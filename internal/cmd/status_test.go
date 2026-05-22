package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
)

func TestStatus_ShowsLiveRunsWithRunIDAndIssues(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	store := daemon.NewLiveRunStore(filepath.Join(dir, ".sandman"))
	if err := store.Register(daemon.LiveRun{
		RunID:     "run-42-123",
		PID:       os.Getpid(),
		Issues:    []int{42, 43},
		StartedAt: time.Now().Add(-5 * time.Minute).UTC(),
	}); err != nil {
		t.Fatalf("register live run: %v", err)
	}

	log := &fakeEventLog{
		events: []events.Event{{Type: "run.started", Timestamp: time.Now().Add(-10 * time.Minute), RunID: "run-99-old", Issue: 99}},
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
	if !strings.Contains(out, "run-42-123") {
		t.Fatalf("expected live run id in output, got:\n%s", out)
	}
	if !strings.Contains(out, "#42, #43") {
		t.Fatalf("expected live issue list in output, got:\n%s", out)
	}
	if !strings.Contains(out, "#99") {
		t.Fatalf("expected event-log-only active run to survive merge, got:\n%s", out)
	}
}

func TestStatus_NoActiveRuns(t *testing.T) {
	var buf bytes.Buffer
	cmd := NewStatusCmd(&fakeEventLog{
		events: []events.Event{
			{Type: "run.started", Timestamp: time.Now().Add(-10 * time.Minute), RunID: "run-42", Issue: 42},
			{Type: "run.finished", Timestamp: time.Now().Add(-5 * time.Minute), RunID: "run-42", Issue: 42, Payload: map[string]any{"status": "success"}},
		},
	})
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "No active runs") {
		t.Fatalf("expected no active runs, got:\n%s", out)
	}
}
