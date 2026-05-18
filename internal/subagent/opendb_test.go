package subagent

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestDBPollerDiscoverDBPath(t *testing.T) {
	var calls [][]string
	runner := func(args ...string) ([]byte, error) {
		calls = append(calls, args)
		return []byte("/home/user/.opencode/sessions.db\n"), nil
	}

	p := &DBPoller{runner: runner}
	path, err := p.discoverDBPath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "/home/user/.opencode/sessions.db" {
		t.Errorf("expected /home/user/.opencode/sessions.db, got %q", path)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if len(calls[0]) != 2 || calls[0][0] != "db" || calls[0][1] != "path" {
		t.Errorf("expected ['db', 'path'], got %v", calls[0])
	}
}

func TestDBPollerDiscoverDBPathError(t *testing.T) {
	runner := func(args ...string) ([]byte, error) {
		return nil, errors.New("opencode not found")
	}

	p := &DBPoller{runner: runner}
	_, err := p.discoverDBPath()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestDBPollerDiscoverDBPathCaches(t *testing.T) {
	var callCount int
	runner := func(args ...string) ([]byte, error) {
		callCount++
		return []byte("/home/user/.opencode/sessions.db\n"), nil
	}

	p := &DBPoller{runner: runner}
	_, _ = p.discoverDBPath()
	_, _ = p.discoverDBPath()

	if callCount != 1 {
		t.Errorf("expected 1 call (cached), got %d", callCount)
	}
}

func TestDBPollerQuerySessions(t *testing.T) {
	runner := func(args ...string) ([]byte, error) {
		return []byte(`[{"id":"child-1","title":"Fix auth","agent":"opencode","time_created":"2024-01-01T00:00:00Z"}]`), nil
	}

	p := &DBPoller{runner: runner}
	sessions, err := p.querySessions("parent-123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].SessionID != "child-1" {
		t.Errorf("expected SessionID child-1, got %q", sessions[0].SessionID)
	}
	if sessions[0].Title != "Fix auth" {
		t.Errorf("expected Title 'Fix auth', got %q", sessions[0].Title)
	}
	if sessions[0].Agent != "opencode" {
		t.Errorf("expected Agent 'opencode', got %q", sessions[0].Agent)
	}
}

func TestDBPollerQuerySessionsEmpty(t *testing.T) {
	runner := func(args ...string) ([]byte, error) {
		return []byte(`[]`), nil
	}

	p := &DBPoller{runner: runner}
	sessions, err := p.querySessions("parent-123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(sessions))
	}
}

func TestDBPollerQuerySessionsError(t *testing.T) {
	runner := func(args ...string) ([]byte, error) {
		return nil, errors.New("db error")
	}

	p := &DBPoller{runner: runner}
	_, err := p.querySessions("parent-123")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestDBPollerExtractSessionMessages(t *testing.T) {
	var mu sync.Mutex
	callIndex := 0
	responses := [][]byte{
		[]byte(`[{"id":"m1","session_id":"child-1","data":"{\"role\":\"user\"}"},{"id":"m2","session_id":"child-1","data":"{\"role\":\"assistant\"}"}]`),
		[]byte(`[{"id":"p1","message_id":"m1","session_id":"child-1","data":"{\"type\":\"text\",\"text\":\"Hello\"}"},{"id":"p2","message_id":"m2","session_id":"child-1","data":"{\"type\":\"reasoning\",\"text\":\"thinking...\"}"},{"id":"p3","message_id":"m2","session_id":"child-1","data":"{\"type\":\"text\",\"text\":\"Here is the answer\"}"}]`),
	}
	runner := func(args ...string) ([]byte, error) {
		mu.Lock()
		i := callIndex
		callIndex++
		mu.Unlock()
		return responses[i], nil
	}

	p := &DBPoller{runner: runner}
	messages, err := p.extractSession("child-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}
	if messages[0].Role != "user" {
		t.Errorf("expected role 'user', got %q", messages[0].Role)
	}
	if len(messages[0].Parts) != 1 {
		t.Fatalf("expected 1 part in msg0, got %d", len(messages[0].Parts))
	}
	if messages[0].Parts[0].Type != PartTypeText {
		t.Errorf("expected PartTypeText, got %s", messages[0].Parts[0].Type)
	}
	if messages[0].Parts[0].Text != "Hello" {
		t.Errorf("expected Text 'Hello', got %q", messages[0].Parts[0].Text)
	}
	if messages[1].Role != "assistant" {
		t.Errorf("expected role 'assistant', got %q", messages[1].Role)
	}
	if len(messages[1].Parts) != 2 {
		t.Fatalf("expected 2 parts in msg1, got %d", len(messages[1].Parts))
	}
	if messages[1].Parts[0].Type != PartTypeReasoning {
		t.Errorf("expected PartTypeReasoning, got %s", messages[1].Parts[0].Type)
	}
	if messages[1].Parts[1].Type != PartTypeText {
		t.Errorf("expected PartTypeText, got %s", messages[1].Parts[1].Type)
	}
}

func TestDBPollerExtractSessionEmpty(t *testing.T) {
	runner := func(args ...string) ([]byte, error) {
		return []byte(`[]`), nil
	}

	p := &DBPoller{runner: runner}
	messages, err := p.extractSession("child-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(messages) != 0 {
		t.Errorf("expected 0 messages, got %d", len(messages))
	}
}

func TestDBPollerExtractSessionError(t *testing.T) {
	runner := func(args ...string) ([]byte, error) {
		return nil, errors.New("db error")
	}

	p := &DBPoller{runner: runner}
	_, err := p.extractSession("child-1")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestDBPollerParseToolPart(t *testing.T) {
	var mu sync.Mutex
	callIndex := 0
	responses := [][]byte{
		[]byte(`[{"id":"m1","session_id":"child-1","data":"{\"role\":\"assistant\"}"}]`),
		[]byte(`[{"id":"p1","message_id":"m1","session_id":"child-1","data":"{\"type\":\"tool\",\"tool\":\"Read\",\"input\":\"file.go\",\"output\":\"file content\"}"}]`),
	}
	runner := func(args ...string) ([]byte, error) {
		mu.Lock()
		i := callIndex
		callIndex++
		mu.Unlock()
		return responses[i], nil
	}

	p := &DBPoller{runner: runner}
	messages, err := p.extractSession("child-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(messages) != 1 || len(messages[0].Parts) != 1 {
		t.Fatalf("expected 1 message with 1 part")
	}
	part := messages[0].Parts[0]
	if part.Type != PartTypeTool {
		t.Errorf("expected PartTypeTool, got %s", part.Type)
	}
	if part.ToolName != "Read" {
		t.Errorf("expected ToolName 'Read', got %q", part.ToolName)
	}
	if part.ToolInput != "file.go" {
		t.Errorf("expected ToolInput 'file.go', got %q", part.ToolInput)
	}
	if part.ToolOutput != "file content" {
		t.Errorf("expected ToolOutput 'file content', got %q", part.ToolOutput)
	}
}

func TestDBPollerStartStop(t *testing.T) {
	events := make(chan Event, 64)
	runner := func(args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "db" && args[1] == "path" {
			return []byte("/home/user/.opencode/sessions.db"), nil
		}
		return []byte(`[]`), nil
	}

	p := &DBPoller{
		runner:       runner,
		events:       events,
		issue:        42,
		writeFile:    func(path string, data []byte) error { return nil },
		pollInterval: 50 * time.Millisecond,
	}

	p.Start("parent-123")

	time.Sleep(100 * time.Millisecond)

	p.Stop()

	select {
	case e := <-events:
		t.Errorf("unexpected event: %+v", e)
	default:
	}
}

func TestDBPollerDBPathErrorDisablesCapture(t *testing.T) {
	events := make(chan Event, 64)
	callCount := 0
	runner := func(args ...string) ([]byte, error) {
		callCount++
		return nil, errors.New("opencode not available")
	}

	p := &DBPoller{
		runner:    runner,
		events:    events,
		issue:     42,
		writeFile: func(path string, data []byte) error { return nil },
	}

	p.Start("parent-123")
	p.Stop()

	// Since DB path discovery failed, poller should not start polling
	// runner should have been called only once (for discoverDBPath)
	if callCount != 1 {
		t.Errorf("expected 1 call (discoverDBPath), got %d", callCount)
	}
}

func TestDBPollerEmitsEventsForChildContent(t *testing.T) {
	events := make(chan Event, 64)

	type step struct {
		output []byte
		err    error
	}

	steps := []step{
		{output: []byte("/home/user/.opencode/sessions.db")},
		{output: []byte(`[]`)},
		{output: []byte(`[{"id":"child-1","title":"Fix bug","agent":"opencode","time_created":"2024-01-01T00:00:00Z"}]`)},
		{output: []byte(`[{"id":"m1","session_id":"child-1","data":"{\"role\":\"assistant\"}"}]`)},
		{output: []byte(`[{"id":"p1","message_id":"m1","session_id":"child-1","data":"{\"type\":\"text\",\"text\":\"Hello from child\"}"}]`)},
	}

	var mu sync.Mutex
	callIndex := 0
	runner := func(args ...string) ([]byte, error) {
		mu.Lock()
		i := callIndex
		callIndex++
		mu.Unlock()
		if i < len(steps) {
			return steps[i].output, steps[i].err
		}
		return []byte(`[]`), nil
	}

	writeFile := func(path string, data []byte) error { return nil }

	p := &DBPoller{
		runner:       runner,
		events:       events,
		issue:        42,
		writeFile:    writeFile,
		pollInterval: 50 * time.Millisecond,
	}

	p.Start("parent-123")
	defer p.Stop()

	var gotEvent bool
	timeout := time.After(2 * time.Second)
	for !gotEvent {
		select {
		case e := <-events:
			if e.Type != EventText {
				t.Errorf("expected EventText, got %s", e.Type)
			}
			if e.SessionID != "child-1" {
				t.Errorf("expected SessionID child-1, got %s", e.SessionID)
			}
			if e.ParentID != "parent-123" {
				t.Errorf("expected ParentID parent-123, got %s", e.ParentID)
			}
			if e.Content != "Hello from child" {
				t.Errorf("expected content 'Hello from child', got %q", e.Content)
			}
			gotEvent = true
		case <-timeout:
			t.Fatal("timeout waiting for text event from child")
		}
	}
}
