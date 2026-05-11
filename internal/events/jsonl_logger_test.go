package events

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestJSONLLogger_LogWritesValidJSONLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	logger := &JSONLLogger{Path: path}

	event := Event{
		Type:      "run.started",
		Timestamp: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC),
		RunID:     "run-42",
		Issue:     42,
		Payload:   map[string]any{"branch": "sandman/42-fix-bug"},
	}

	if err := logger.Log(event); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}

	var got Event
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("unmarshal JSON line: %v", err)
	}

	if got.Type != "run.started" {
		t.Errorf("expected type run.started, got %q", got.Type)
	}
	if got.RunID != "run-42" {
		t.Errorf("expected run_id run-42, got %q", got.RunID)
	}
	if got.Issue != 42 {
		t.Errorf("expected issue 42, got %d", got.Issue)
	}
	branch, _ := got.Payload["branch"].(string)
	if branch != "sandman/42-fix-bug" {
		t.Errorf("expected branch sandman/42-fix-bug, got %q", branch)
	}
}

func TestJSONLLogger_ReadParsesEvents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	logger := &JSONLLogger{Path: path}

	want := []Event{
		{Type: "run.started", RunID: "run-1", Issue: 1},
		{Type: "run.finished", RunID: "run-1", Issue: 1, Payload: map[string]any{"status": "success"}},
	}
	for _, e := range want {
		if err := logger.Log(e); err != nil {
			t.Fatalf("log event: %v", err)
		}
	}

	got, err := logger.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if len(got) != len(want) {
		t.Fatalf("expected %d events, got %d", len(want), len(got))
	}
	for i := range want {
		if got[i].Type != want[i].Type {
			t.Errorf("event %d type: expected %q, got %q", i, want[i].Type, got[i].Type)
		}
		if got[i].RunID != want[i].RunID {
			t.Errorf("event %d run_id: expected %q, got %q", i, want[i].RunID, got[i].RunID)
		}
		if got[i].Issue != want[i].Issue {
			t.Errorf("event %d issue: expected %d, got %d", i, want[i].Issue, got[i].Issue)
		}
	}
}

func TestJSONLLogger_ReadEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	logger := &JSONLLogger{Path: path}

	got, err := logger.Read()
	if err != nil {
		t.Fatalf("read empty file: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 events, got %d", len(got))
	}
}

func TestJSONLLogger_ConcurrentAppendIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	logger := &JSONLLogger{Path: path}

	const workers = 100
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(n int) {
			defer wg.Done()
			e := Event{Type: "run.started", RunID: fmt.Sprintf("run-%d", n), Issue: n}
			if err := logger.Log(e); err != nil {
				t.Errorf("log event %d: %v", n, err)
			}
		}(i)
	}
	wg.Wait()

	events, err := logger.Read()
	if err != nil {
		t.Fatalf("read after concurrent write: %v", err)
	}
	if len(events) != workers {
		t.Fatalf("expected %d events, got %d", workers, len(events))
	}

	seen := make(map[string]bool)
	for _, e := range events {
		if seen[e.RunID] {
			t.Errorf("duplicate run_id %q", e.RunID)
		}
		seen[e.RunID] = true
		if e.Type != "run.started" {
			t.Errorf("expected type run.started, got %q", e.Type)
		}
	}
}
