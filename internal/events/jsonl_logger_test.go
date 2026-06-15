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

func TestJSONLLogger_LogWritesNullIssue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	logger := &JSONLLogger{Path: path}

	event := Event{
		Type:      "run.started",
		Timestamp: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC),
		RunID:     "run-prompt-only",
		Payload:   map[string]any{"branch": "sandman/return-only-ok-123"},
	}

	if err := logger.Log(event); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}

	if !strings.Contains(string(data), `"issue":null`) {
		t.Fatalf("expected null issue in JSON, got %s", string(data))
	}

	got, err := logger.Read()
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	if got[0].Issue != 0 {
		t.Fatalf("expected zero issue value after read, got %d", got[0].Issue)
	}
	if got[0].IssueRef != nil {
		t.Fatalf("expected nil issue ref after read, got %v", *got[0].IssueRef)
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

func TestJSONLLogger_RemoveEventsByIssue_FiltersByIssueRef(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	logger := &JSONLLogger{Path: path}

	ptr := 7
	events := []Event{
		{Type: "run.started", RunID: "r-1", Issue: 1},
		{Type: "run.finished", RunID: "r-1", Issue: 1},
		{Type: "run.started", RunID: "r-7", IssueRef: &ptr},
		{Type: "run.finished", RunID: "r-7", IssueRef: &ptr},
		{Type: "run.started", RunID: "r-8", Issue: 8},
	}
	for _, e := range events {
		if err := logger.Log(e); err != nil {
			t.Fatalf("log %q: %v", e.RunID, err)
		}
	}

	if err := logger.RemoveEventsByIssue(7); err != nil {
		t.Fatalf("remove issue 7: %v", err)
	}

	got, err := logger.Read()
	if err != nil {
		t.Fatalf("read after remove: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 events after remove (Issue=1 kept x2, Issue=8 kept x1), got %d", len(got))
	}
	for _, e := range got {
		if e.IssueRef != nil && *e.IssueRef == 7 {
			t.Errorf("issue ref 7 should have been removed, got %+v", e)
		}
		if e.Issue == 7 {
			t.Errorf("issue 7 should have been removed, got %+v", e)
		}
	}
}

// TestJSONLLogger_LogVsRemoveRace is the regression test for the
// slice 6 race: N concurrent Log calls run in parallel with one
// RemoveEventsByIssue. The post-condition is:
//
//  1. No event with Issue != removedIssue is lost in the race. With the
//     pre-slice-6 implementation, an O_APPEND between Read and O_TRUNC
//     in RemoveEventsByIssue dropped the appended line, so a non-
//     matching RunID would be missing from the final file.
//  2. No RunID appears more than once.
//  3. After the concurrent batch settles, a subsequent
//     RemoveEventsByIssue call removes any matching events that were
//     logged after the first remove completed, leaving the warmup event
//     and the 90 non-matching events from the 100 logs.
func TestJSONLLogger_LogVsRemoveRace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	logger := &JSONLLogger{Path: path}

	const (
		totalLogs    = 100
		removedIssue = 3
		matchModulo  = 10
	)

	// Warm-up Log opens the held file handle before the race window
	// starts, so the test exercises the held-handle path rather than
	// the lazy-open path.
	if err := logger.Log(Event{Type: "run.warmup", RunID: "warmup", Issue: 0}); err != nil {
		t.Fatalf("warmup log: %v", err)
	}

	// expectedNonMatching is the set of RunIDs we will log that are
	// NOT supposed to be removed. The post-condition asserts every one
	// of these is present after the race and after a final remove.
	expectedNonMatching := make(map[string]bool, totalLogs)
	for i := 0; i < totalLogs; i++ {
		if i%matchModulo == removedIssue {
			continue
		}
		expectedNonMatching[fmt.Sprintf("run-%d", i)] = true
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(totalLogs + 1)

	for i := 0; i < totalLogs; i++ {
		go func(n int) {
			defer wg.Done()
			e := Event{Type: "run.started", RunID: fmt.Sprintf("run-%d", n), Issue: n % matchModulo}
			<-start
			if err := logger.Log(e); err != nil {
				t.Errorf("log %d: %v", n, err)
			}
		}(i)
	}

	go func() {
		defer wg.Done()
		<-start
		if err := logger.RemoveEventsByIssue(removedIssue); err != nil {
			t.Errorf("remove: %v", err)
		}
	}()

	close(start)
	wg.Wait()

	got, err := logger.Read()
	if err != nil {
		t.Fatalf("read after race: %v", err)
	}

	seen := make(map[string]int)
	for _, e := range got {
		if e.RunID == "warmup" {
			continue
		}
		seen[e.RunID]++
	}

	// Post-condition 1: every non-matching RunID is present exactly once.
	// A missing non-matching RunID is the original race-induced loss.
	for id := range expectedNonMatching {
		count := seen[id]
		if count == 0 {
			t.Errorf("non-matching run_id %q was lost in the race", id)
		}
		if count > 1 {
			t.Errorf("run_id %q appeared %d times after the race", id, count)
		}
	}

	// Post-condition 2: a final remove cleans up any matching events
	// that were logged after the concurrent remove completed, leaving
	// exactly the warmup event plus the 90 non-matching events.
	if err := logger.RemoveEventsByIssue(removedIssue); err != nil {
		t.Fatalf("final remove: %v", err)
	}
	got, err = logger.Read()
	if err != nil {
		t.Fatalf("read after final remove: %v", err)
	}
	wantAfterFinal := 1 + len(expectedNonMatching)
	if len(got) != wantAfterFinal {
		t.Fatalf("expected %d events after final remove (warmup + %d non-matching), got %d", wantAfterFinal, len(expectedNonMatching), len(got))
	}
	gotIDs := make(map[string]bool)
	for _, e := range got {
		gotIDs[e.RunID] = true
	}
	if !gotIDs["warmup"] {
		t.Errorf("warmup event missing after final remove")
	}
	for id := range expectedNonMatching {
		if !gotIDs[id] {
			t.Errorf("non-matching run_id %q missing after final remove", id)
		}
	}
	for _, e := range got {
		if e.Issue == removedIssue && e.RunID != "warmup" {
			t.Errorf("matching event %q still in file after final remove", e.RunID)
		}
	}
}
