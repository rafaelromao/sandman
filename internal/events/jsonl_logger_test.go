package events

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestJSONLLogger_LogWritesValidJSONLine(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

func TestJSONLLogger_ReadSkipsMalformedLine(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	good := Event{Type: "run.started", Timestamp: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC), RunID: "run-good", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix"}}
	goodLine, err := json.Marshal(good)
	if err != nil {
		t.Fatalf("marshal good: %v", err)
	}
	// A torn line left behind by a cross-process write race: a
	// fragment with no opening brace. Pre-O_APPEND fixes the daemon
	// produced these when two daemons hit the log simultaneously.
	tornLine := []byte(`tection","model":"minimax-coding-plan/MiniMax-M2.7","prompt_source_type":"current","review_command":"@codex review"}}`)

	contents := append([]byte{}, goodLine...)
	contents = append(contents, '\n')
	contents = append(contents, tornLine...)
	contents = append(contents, '\n')
	if err := os.WriteFile(path, contents, 0644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	logger := &JSONLLogger{Path: path}
	got, err := logger.Read()
	if err != nil {
		t.Fatalf("read with malformed line: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 event after skipping malformed line, got %d", len(got))
	}
	if got[0].RunID != "run-good" {
		t.Errorf("expected run-good, got %q", got[0].RunID)
	}

	// RemoveEventsByIssue must also tolerate a torn line so a stuck
	// cleanup can still rewrite the log around it.
	if err := logger.RemoveEventsByIssue(1); err != nil {
		t.Fatalf("remove with malformed line: %v", err)
	}
	got, err = logger.Read()
	if err != nil {
		t.Fatalf("read after remove: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 events after remove, got %d", len(got))
	}
}

// TestJSONLLogger_ReadQuarantinesMalformedLines is the regression test
// for the user-reported log spam: when events.jsonl contains lines
// left behind by a pre-O_APPEND cross-process write race, every Read
// and RemoveEventsByIssue would re-warn on the same bytes. Read must
// quarantine the bad lines to a sidecar so the next Read does not
// re-warn on the same bytes. Read never replaces the main log
// (replacing it via os.Rename would unlink the inode other daemons
// have opened via O_APPEND); the rewrite is the responsibility of
// RemoveEventsByIssue, which truncates the existing in-process file
// descriptor.
func TestJSONLLogger_ReadQuarantinesMalformedLines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	good := Event{Type: "run.started", Timestamp: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC), RunID: "run-good", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix"}}
	goodLine, err := json.Marshal(good)
	if err != nil {
		t.Fatalf("marshal good: %v", err)
	}
	// A 153-byte torn line that looks like a run.queued whose tail was
	// collided with a concurrent run.finished: the parser sees
	// `ftru...` (state "false", got "t" instead of "a").
	tornA := []byte(`{"type":"run.queued","timestamp":"2026-06-16T17:00:36-03:00","run_id":"run-1055-x","issue":1055,"payload":{"blocked_by":[1042]ftrue_padding_padding_padxx`)
	// A 3235-byte torn line that begins with a fragment of a previous
	// run's payload, missing its opening brace.
	tornB := append([]byte(`e context window. Restrict searches to the cwd or explicit sub-paths within it; use the Glob/Grep tools which already scope to the project by default.\n","retries":0,"review":true,"review_focus":"","sandbox":"podman","start_delay":0}}`), bytes.Repeat([]byte(" "), 3001)...)

	contents := append([]byte{}, goodLine...)
	contents = append(contents, '\n')
	contents = append(contents, tornA...)
	contents = append(contents, '\n')
	contents = append(contents, tornB...)
	contents = append(contents, '\n')
	if err := os.WriteFile(path, contents, 0644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	// Pin the sizes to match the original error log: 153 and 3235.
	if len(tornA) != 153 {
		t.Fatalf("tornA must be 153 bytes to mirror the original error, got %d", len(tornA))
	}
	if len(tornB) != 3235 {
		t.Fatalf("tornB must be 3235 bytes to mirror the original error, got %d", len(tornB))
	}

	logger := &JSONLLogger{Path: path}
	got, err := logger.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 1 || got[0].RunID != "run-good" {
		t.Fatalf("expected 1 valid event (run-good), got %d: %+v", len(got), got)
	}

	// The sidecar must hold both torn lines so the user can inspect
	// them. Order is preserved by Read.
	side, err := os.ReadFile(path + ".malformed")
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	sideLines := strings.Split(strings.TrimRight(string(side), "\n"), "\n")
	if len(sideLines) != 2 {
		t.Fatalf("expected 2 quarantined lines, got %d: %q", len(sideLines), side)
	}
	if sideLines[0] != string(tornA) || sideLines[1] != string(tornB) {
		t.Errorf("sidecar lines do not match originals\nwant: %q / %q\ngot:  %q / %q", tornA, tornB, sideLines[0], sideLines[1])
	}

	// The main log is left intact: Read does not replace it (replacing
	// it would unlink the inode other daemons have opened).
	main, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read main log: %v", err)
	}
	if !strings.Contains(string(main), string(tornA)) || !strings.Contains(string(main), string(tornB)) {
		t.Fatalf("main log should still hold the torn lines (Read does not rewrite): %q", main)
	}
	if !strings.Contains(string(main), "run-good") {
		t.Fatalf("main log lost the valid event during read: %q", main)
	}

	// A second Read must be idempotent: the same torn lines are
	// detected again, but filterAlreadyQuarantined skips them, so
	// the sidecar does not grow and no extra "skipping" log lines
	// are produced.
	got, err = logger.Read()
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if len(got) != 1 || got[0].RunID != "run-good" {
		t.Fatalf("second read expected 1 event, got %d", len(got))
	}
	side2, err := os.ReadFile(path + ".malformed")
	if err != nil {
		t.Fatalf("read sidecar again: %v", err)
	}
	if string(side) != string(side2) {
		t.Errorf("sidecar grew on a no-op read: was %d bytes, now %d", len(side), len(side2))
	}
}

// TestJSONLLogger_RemoveQuarantinesMalformedLines ensures the cleanup
// path (RemoveEventsByIssue) also routes torn lines to the sidecar
// instead of dropping them silently. It also asserts the rewrite is
// idempotent on a second pass: the sidecar does not grow and the
// main log stays the same size.
func TestJSONLLogger_RemoveQuarantinesMalformedLines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	good := Event{Type: "run.started", Timestamp: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC), RunID: "run-good", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix"}}
	goodLine, err := json.Marshal(good)
	if err != nil {
		t.Fatalf("marshal good: %v", err)
	}
	torn := []byte(`{"type":"run.queued","timestamp":"2026-06-16T17:00:36-03:00","run_id":"run-x","issue":99,"payload":{"blocked_by":[1]ftrue`)

	contents := append([]byte{}, goodLine...)
	contents = append(contents, '\n')
	contents = append(contents, torn...)
	contents = append(contents, '\n')
	if err := os.WriteFile(path, contents, 0644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	logger := &JSONLLogger{Path: path}
	if err := logger.RemoveEventsByIssue(99); err != nil {
		t.Fatalf("remove: %v", err)
	}

	// The torn line must land in the sidecar even though Remove
	// discards it from the rewrite.
	side, err := os.ReadFile(path + ".malformed")
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if !strings.Contains(string(side), string(torn)) {
		t.Errorf("torn line missing from sidecar: %q", side)
	}
	main, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read main: %v", err)
	}
	if strings.Contains(string(main), string(torn)) {
		t.Errorf("torn line still in main log: %q", main)
	}
	if !strings.Contains(string(main), "run-good") {
		t.Errorf("valid event lost during remove: %q", main)
	}

	// Idempotency: a second Remove on the same issue touches
	// nothing (the issue is already gone), the sidecar does not
	// grow, and the main log is byte-identical.
	sideLenBefore := len(side)
	mainBefore := append([]byte(nil), main...)
	if err := logger.RemoveEventsByIssue(99); err != nil {
		t.Fatalf("second remove: %v", err)
	}
	side2, err := os.ReadFile(path + ".malformed")
	if err != nil {
		t.Fatalf("read sidecar again: %v", err)
	}
	if len(side2) != sideLenBefore {
		t.Errorf("sidecar grew on a no-op remove: was %d bytes, now %d", sideLenBefore, len(side2))
	}
	main2, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read main again: %v", err)
	}
	if !bytes.Equal(main2, mainBefore) {
		t.Errorf("main log changed on a no-op remove\nwas: %q\nnow: %q", mainBefore, main2)
	}
}

func TestJSONLLogger_ConcurrentAppendIsAtomic(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// TestJSONLLogger_LogVsRemoveRace is the #2224 regression test: N
// concurrent Log calls run in parallel with one RemoveEventsByIssue
// and no non-matching event is lost.
func TestJSONLLogger_LogVsRemoveRace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	logger := &JSONLLogger{Path: path}

	const (
		totalLogs    = 100
		removedIssue = 3
		matchModulo  = 10
	)

	if err := logger.Log(Event{Type: "run.warmup", RunID: "warmup", Issue: 0}); err != nil {
		t.Fatalf("warmup log: %v", err)
	}

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

	for id := range expectedNonMatching {
		count := seen[id]
		if count == 0 {
			t.Errorf("non-matching run_id %q was lost in the race", id)
		}
		if count > 1 {
			t.Errorf("run_id %q appeared %d times after the race", id, count)
		}
	}

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

// jsonlCrossProcessChildFlag is the env var a forked child reads to
// discover its worker id, event count, payload size, and shared file
// path. The child writes that many events and exits.
const jsonlCrossProcessChildFlag = "SANDMAN_JSONL_CROSS_PROCESS_CHILD"

func TestJSONLLogger_CrossProcessConcurrentAppendIsAtomic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	const (
		workers       = 6
		eventsPerWork = 200
		payloadSize   = 7000
	)
	// Mix one long-payload worker (mimics a PR-review run.started) with
	// short-payload workers (mimic normal issue runs). Without an
	// atomic cross-process append, the long writer's chunk can be
	// interleaved with a short writer's line and produce a torn record.
	mix := func(id int) (count int, payload string) {
		if id == 0 {
			return eventsPerWork, strings.Repeat("a", payloadSize)
		}
		return eventsPerWork, "short"
	}

	expected := workers * eventsPerWork
	ready := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(workers)
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		count, payload := mix(i)
		// Each worker blocks on a private "go" file. Parent writes 1
		// byte to every worker's go file after the ready channel is
		// closed, so all workers start hammering the shared log
		// within a small scheduling window.
		goPath := filepath.Join(dir, fmt.Sprintf("go-%d", i))
		if err := os.WriteFile(goPath, nil, 0644); err != nil {
			t.Fatalf("create go file: %v", err)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestJSONLLogger_CrossProcessChild$")
		cmd.Env = append(os.Environ(),
			jsonlCrossProcessChildFlag+"=1",
			"SANDMAN_JSONL_PATH="+path,
			"SANDMAN_JSONL_WORKER_ID="+strconv.Itoa(i),
			"SANDMAN_JSONL_COUNT="+strconv.Itoa(count),
			"SANDMAN_JSONL_PAYLOAD="+payload,
			"SANDMAN_JSONL_GO_FILE="+goPath,
		)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatalf("start worker %d: %v", i, err)
		}
		go func(i int, goPath string) {
			defer wg.Done()
			<-ready
			// Signal "go" by writing a single byte to the worker's
			// go file. The child polls for any change to the file's
			// mtime/size, so a write of any length works.
			f, err := os.OpenFile(goPath, os.O_WRONLY, 0644)
			if err != nil {
				errs <- fmt.Errorf("worker %d open go: %w", i, err)
				return
			}
			if _, err := f.Write([]byte("go")); err != nil {
				_ = f.Close()
				errs <- fmt.Errorf("worker %d signal: %w", i, err)
				return
			}
			_ = f.Close()
			if err := cmd.Wait(); err != nil {
				errs <- fmt.Errorf("worker %d: %w", i, err)
			}
		}(i, goPath)
	}

	close(ready)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("%v", err)
	}

	// Every line in the file must be valid JSON. A torn record from a
	// cross-process write race surfaces here as a json decode error
	// tied to the offset where the second writer overwrote the first.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read shared log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != expected {
		t.Fatalf("expected %d lines, got %d", expected, len(lines))
	}
	seen := make(map[string]int, expected)
	for i, line := range lines {
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d invalid JSON (%d bytes): %v\nline: %q", i+1, len(line), err, line)
		}
		if e.Type != "run.started" {
			t.Errorf("line %d unexpected type %q", i+1, e.Type)
		}
		seen[e.RunID]++
	}
	for i := 0; i < workers; i++ {
		count, _ := mix(i)
		for k := 0; k < count; k++ {
			id := fmt.Sprintf("w%d-r%d", i, k)
			if seen[id] != 1 {
				t.Errorf("expected run_id %q exactly once, got %d", id, seen[id])
			}
		}
	}
}

// TestJSONLLogger_CrossProcessChild is the worker entry point invoked
// by TestJSONLLogger_CrossProcessConcurrentAppendIsAtomic. It is
// hidden from the regular test run by its leading TestJSONL prefix
// gate; exec.Command invokes it directly.
func TestJSONLLogger_CrossProcessChild(t *testing.T) {
	t.Parallel()
	if os.Getenv(jsonlCrossProcessChildFlag) != "1" {
		t.Skip("cross-process worker; only runs when " + jsonlCrossProcessChildFlag + "=1")
	}
	path := os.Getenv("SANDMAN_JSONL_PATH")
	workerID, err := strconv.Atoi(os.Getenv("SANDMAN_JSONL_WORKER_ID"))
	if err != nil {
		t.Fatalf("worker id: %v", err)
	}
	count, err := strconv.Atoi(os.Getenv("SANDMAN_JSONL_COUNT"))
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	payload := os.Getenv("SANDMAN_JSONL_PAYLOAD")
	// Wait for the parent's "go" signal: poll the go file every 5ms
	// until its size is non-zero.
	goFile := os.Getenv("SANDMAN_JSONL_GO_FILE")
	if goFile == "" {
		t.Fatalf("SANDMAN_JSONL_GO_FILE not set")
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		info, err := os.Stat(goFile)
		if err == nil && info.Size() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	logger := &JSONLLogger{Path: path}
	for k := 0; k < count; k++ {
		e := Event{
			Type:      "run.started",
			Timestamp: time.Now(),
			RunID:     fmt.Sprintf("w%d-r%d", workerID, k),
			Issue:     workerID,
			Payload:   map[string]any{"branch": "sandman/cross-process", "payload": payload},
		}
		if err := logger.Log(e); err != nil {
			t.Fatalf("log %d/%d: %v", k, count, err)
		}
	}
}

func TestJSONLLogger_RemoveFailureRestoresAndNextOperationRecovers(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	seed := &JSONLLogger{Path: path}
	for _, event := range []Event{{Type: "run.started", RunID: "remove", Issue: 7}, {Type: "run.finished", RunID: "keep", Issue: 8}} {
		if err := seed.Log(event); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	logger := &JSONLLogger{
		Path: path,
		hooks: &jsonlLoggerHooks{fail: func(stage string) error {
			if stage == "write" {
				return fmt.Errorf("injected write failure")
			}
			return nil
		}},
	}
	if err := logger.RemoveEventsByIssue(7); err == nil {
		t.Fatal("RemoveEventsByIssue succeeded after injected failure")
	}
	gotRaw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotRaw, raw) {
		t.Fatalf("failed removal changed main log\nwant %q\n got %q", raw, gotRaw)
	}

	// Restoration also clears completed recovery artifacts, so a fresh logger
	// sees the original projection rather than an interrupted transaction.
	got, err := (&JSONLLogger{Path: path}).Read()
	if err != nil {
		t.Fatalf("recovery read: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("recovery lost events: %+v", got)
	}
	if _, err := os.Stat(path + ".txn"); !os.IsNotExist(err) {
		t.Fatalf("transaction marker remains after restoration: %v", err)
	}
}

func TestJSONLLogger_InterruptedTransactionRecoversBeforeLog(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	original := []byte(`{"type":"run.started","timestamp":"2025-01-01T00:00:00Z","run_id":"old","issue":1}` + "\n")
	if err := os.WriteFile(path, []byte("partial rewrite\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".recovery", original, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".txn", []byte("pending\n"), 0644); err != nil {
		t.Fatal(err)
	}
	logger := &JSONLLogger{Path: path}
	if err := logger.Log(Event{Type: "run.finished", RunID: "new", Issue: 2}); err != nil {
		t.Fatalf("log after interruption: %v", err)
	}
	got, err := logger.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].RunID != "old" || got[1].RunID != "new" {
		t.Fatalf("interrupted transaction was not restored: %+v", got)
	}
}

func TestJSONLLogger_CommittedTransactionCleansUpWithoutRestoring(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	filtered := []byte(`{"type":"run.started","timestamp":"2025-01-01T00:00:00Z","run_id":"keep","issue":2}` + "\n")
	previous := []byte(`{"type":"run.started","timestamp":"2025-01-01T00:00:00Z","run_id":"removed","issue":1}` + "\n")
	if err := os.WriteFile(path, filtered, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".recovery", previous, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".txn", []byte("committed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	logger := &JSONLLogger{Path: path}
	if err := logger.Log(Event{Type: "run.finished", RunID: "new", Issue: 3}); err != nil {
		t.Fatalf("log after committed cleanup interruption: %v", err)
	}
	got, err := logger.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].RunID != "keep" || got[1].RunID != "new" {
		t.Fatalf("committed transaction was incorrectly restored: %+v", got)
	}
	for _, artifact := range []string{path + ".recovery", path + ".txn"} {
		if _, err := os.Stat(artifact); !os.IsNotExist(err) {
			t.Fatalf("transaction artifact remains after committed recovery: %s: %v", artifact, err)
		}
	}
}

func TestJSONLLogger_QuarantineFailureDoesNotRewriteMainLog(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	raw := []byte(`{"type":"run.started","timestamp":"2025-01-01T00:00:00Z","run_id":"keep","issue":2}` + "\nnot json\n")
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatal(err)
	}
	logger := &JSONLLogger{Path: path, hooks: &jsonlLoggerHooks{fail: func(stage string) error {
		if stage == "quarantine" {
			return fmt.Errorf("injected quarantine failure")
		}
		return nil
	}}}
	if err := logger.RemoveEventsByIssue(99); err == nil {
		t.Fatal("RemoveEventsByIssue succeeded after quarantine failure")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("quarantine failure rewrote main log\nwant %q\n got %q", raw, got)
	}
}

func TestJSONLLogger_LockBlocksOtherInstancesAndReleasesAfterFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	done := make(chan error, 1)
	go func() { done <- (&JSONLLogger{Path: path}).Log(Event{Type: "run.started", RunID: "blocked", Issue: 1}) }()
	select {
	case err := <-done:
		t.Fatalf("Log escaped held advisory lock: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Log after lock release: %v", err)
	}

	failing := &JSONLLogger{Path: path, hooks: &jsonlLoggerHooks{fail: func(stage string) error {
		if stage == "snapshot" {
			return fmt.Errorf("injected failure")
		}
		return nil
	}}}
	if err := failing.RemoveEventsByIssue(1); err == nil {
		t.Fatal("expected removal failure")
	}
	if err := (&JSONLLogger{Path: path}).Log(Event{Type: "run.finished", RunID: "after-failure", Issue: 2}); err != nil {
		t.Fatalf("lock was not released after failure: %v", err)
	}
}

func TestJSONLLogger_RemovalPreservesRemainingRunStateProjection(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logger := &JSONLLogger{Path: filepath.Join(dir, "events.jsonl")}
	for _, event := range []Event{
		{Type: "run.started", RunID: "remove", Issue: 7},
		{Type: "run.finished", RunID: "remove", Issue: 7, Payload: map[string]any{"status": "success"}},
		{Type: "run.started", RunID: "keep", Issue: 8},
		{Type: "run.finished", RunID: "keep", Issue: 8, Payload: map[string]any{"status": "failure"}},
	} {
		if err := logger.Log(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := logger.RemoveEventsByIssue(7); err != nil {
		t.Fatal(err)
	}
	got, err := logger.Read()
	if err != nil {
		t.Fatal(err)
	}
	states := ProjectRunStates(got)
	if len(states) != 1 || states[0].RunID != "keep" || states[0].Status() != "failure" {
		t.Fatalf("remaining projection changed: %+v", states)
	}
}

const jsonlRemoveChildFlag = "SANDMAN_JSONL_REMOVE_CHILD"

func TestJSONLLogger_CrossProcessLogWaitsForRemoveAndSurvivesFilter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	ready := filepath.Join(dir, "ready")
	release := filepath.Join(dir, "release")
	const removedIssue = 7
	issueRef := removedIssue
	logger := &JSONLLogger{Path: path}
	for _, event := range []Event{
		{Type: "run.started", RunID: "remove-issue", Issue: removedIssue},
		{Type: "run.finished", RunID: "remove-ref", IssueRef: &issueRef},
		{Type: "run.started", RunID: "keep-existing", Issue: 8},
	} {
		if err := logger.Log(event); err != nil {
			t.Fatalf("seed %q: %v", event.RunID, err)
		}
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestJSONLLogger_CrossProcessRemoveChild$")
	cmd.Env = append(os.Environ(),
		jsonlRemoveChildFlag+"=1",
		"SANDMAN_JSONL_PATH="+path,
		"SANDMAN_JSONL_ISSUE="+strconv.Itoa(removedIssue),
		"SANDMAN_JSONL_READY="+ready,
		"SANDMAN_JSONL_RELEASE="+release,
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := os.Stat(ready); err != nil {
		t.Fatalf("remove worker did not become ready: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- (&JSONLLogger{Path: path}).Log(Event{Type: "run.started", RunID: "keep-appended", Issue: 8})
	}()
	select {
	case err := <-done:
		t.Fatalf("Log ran while RemoveEventsByIssue was still active: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if err := os.WriteFile(release, []byte("go"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("remove child: %v", err)
	}
	got, err := (&JSONLLogger{Path: path}).Read()
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]int, len(got))
	for _, event := range got {
		seen[event.RunID]++
		if event.Issue == removedIssue || (event.IssueRef != nil && *event.IssueRef == removedIssue) {
			t.Errorf("matching event survived removal: %+v", event)
		}
	}
	for _, runID := range []string{"keep-existing", "keep-appended"} {
		if seen[runID] != 1 {
			t.Errorf("expected non-matching event %q exactly once, got %d", runID, seen[runID])
		}
	}
	if len(got) != 2 {
		t.Fatalf("expected two non-matching events after removal, got %+v", got)
	}
}

func TestJSONLLogger_CrossProcessRemoveChild(t *testing.T) {
	if os.Getenv(jsonlRemoveChildFlag) != "1" {
		t.Skip("cross-process remove worker")
	}
	path := os.Getenv("SANDMAN_JSONL_PATH")
	issue, err := strconv.Atoi(os.Getenv("SANDMAN_JSONL_ISSUE"))
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	logger := &JSONLLogger{Path: path, hooks: &jsonlLoggerHooks{fail: func(stage string) error {
		if stage != "truncate" {
			return nil
		}
		if err := os.WriteFile(os.Getenv("SANDMAN_JSONL_READY"), []byte("ready"), 0644); err != nil {
			return err
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if info, err := os.Stat(os.Getenv("SANDMAN_JSONL_RELEASE")); err == nil && info.Size() > 0 {
				return nil
			}
			time.Sleep(time.Millisecond)
		}
		return fmt.Errorf("parent did not release remove worker")
	}}}
	if err := logger.RemoveEventsByIssue(issue); err != nil {
		t.Fatal(err)
	}
}

const jsonlLockHolderChildFlag = "SANDMAN_JSONL_LOCK_HOLDER_CHILD"

func TestJSONLLogger_CrossProcessLockCoordinatesRemoveAndLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	ready := filepath.Join(dir, "ready")
	release := filepath.Join(dir, "release")
	cmd := exec.Command(os.Args[0], "-test.run=^TestJSONLLogger_CrossProcessLockHolder$")
	cmd.Env = append(os.Environ(), jsonlLockHolderChildFlag+"=1", "SANDMAN_JSONL_PATH="+path, "SANDMAN_JSONL_READY="+ready, "SANDMAN_JSONL_RELEASE="+release)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := os.Stat(ready); err != nil {
		t.Fatalf("lock holder did not become ready: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- (&JSONLLogger{Path: path}).Log(Event{Type: "run.started", RunID: "after-remove", Issue: 2})
	}()
	select {
	case err := <-done:
		t.Fatalf("Log ran while another process held the lock: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if err := os.WriteFile(release, []byte("go"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("lock holder: %v", err)
	}
	got, err := (&JSONLLogger{Path: path}).Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RunID != "after-remove" {
		t.Fatalf("cross-process removal/log result: %+v", got)
	}
}

func TestJSONLLogger_CrossProcessLockHolder(t *testing.T) {
	if os.Getenv(jsonlLockHolderChildFlag) != "1" {
		t.Skip("cross-process lock holder")
	}
	path := os.Getenv("SANDMAN_JSONL_PATH")
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err := os.WriteFile(os.Getenv("SANDMAN_JSONL_READY"), []byte("ready"), 0644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(os.Getenv("SANDMAN_JSONL_RELEASE")); err == nil && info.Size() > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("parent did not release lock holder")
}
