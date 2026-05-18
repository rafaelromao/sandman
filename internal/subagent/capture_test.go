package subagent

import (
	"sync"
	"testing"
	"time"
)

func TestEventConstruction(t *testing.T) {
	ts := time.Now()
	e := Event{
		SessionID: "sess-123",
		ParentID:  "parent-456",
		Type:      EventSessionDetected,
		Title:     "Test Session",
		Agent:     "opencode",
		Content:   "hello",
		Timestamp: ts,
	}

	if e.SessionID != "sess-123" {
		t.Errorf("expected SessionID sess-123, got %s", e.SessionID)
	}
	if e.Type != EventSessionDetected {
		t.Errorf("expected type session_detected, got %s", e.Type)
	}
}

func TestSessionOutputConstruction(t *testing.T) {
	so := SessionOutput{
		SessionID: "sess-123",
		Title:     "Fix auth bug",
		Agent:     "opencode",
		Messages: []Message{
			{
				Role: "user",
				Parts: []Part{
					{Type: PartTypeText, Text: "fix the auth bug"},
				},
			},
		},
	}

	if len(so.Messages) != 1 {
		t.Errorf("expected 1 message, got %d", len(so.Messages))
	}
	if so.Messages[0].Parts[0].Type != PartTypeText {
		t.Errorf("expected part type text, got %s", so.Messages[0].Parts[0].Type)
	}
}

func TestCaptureInterfaceExists(t *testing.T) {
	// Verify OpenCodeCapture implements Capture
	var _ Capture = (*OpenCodeCapture)(nil)
}

func TestWrapCommandInjectsFormatJson(t *testing.T) {
	oc := NewOpenCodeCapture()

	wrapped, _, _, err := oc.WrapCommand("opencode run --issue 123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := "opencode run --format json --issue 123"
	if wrapped != expected {
		t.Errorf("expected %q, got %q", expected, wrapped)
	}
}

func TestWrapCommandNonOpencodeUnchanged(t *testing.T) {
	oc := NewOpenCodeCapture()

	wrapped, _, _, err := oc.WrapCommand("echo hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if wrapped != "echo hello" {
		t.Errorf("expected %q, got %q", "echo hello", wrapped)
	}
}

func TestWrapCommandTrimsWhitespace(t *testing.T) {
	oc := NewOpenCodeCapture()

	wrapped, _, _, err := oc.WrapCommand("  opencode run --issue 456  ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := "opencode run --format json --issue 456"
	if wrapped != expected {
		t.Errorf("expected %q, got %q", expected, wrapped)
	}
}

func TestWrapCommandSkipsIfFormatAlreadyPresent(t *testing.T) {
	oc := NewOpenCodeCapture()

	wrapped, _, _, err := oc.WrapCommand("opencode run --format json --issue 123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := "opencode run --format json --issue 123"
	if wrapped != expected {
		t.Errorf("expected %q, got %q", expected, wrapped)
	}
}

func TestWrapCommandHandlesEnvAssignmentPrefix(t *testing.T) {
	oc := NewOpenCodeCapture()

	wrapped, _, _, err := oc.WrapCommand(`PATH=/workspace/.sandman/bin:${PATH} opencode run --issue 123`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := `PATH=/workspace/.sandman/bin:${PATH} opencode run --format json --issue 123`
	if wrapped != expected {
		t.Errorf("expected %q, got %q", expected, wrapped)
	}
}

func TestWrapCommandKeepsCaptureWhenFormatAlreadyPresent(t *testing.T) {
	oc := NewOpenCodeCapture()

	wrapped, stdout, cleanup, err := oc.WrapCommand("opencode run --format json --issue 123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()

	if wrapped != "opencode run --format json --issue 123" {
		t.Fatalf("expected command unchanged, got %q", wrapped)
	}
	if stdout == nil {
		t.Fatal("expected capture stdout writer")
	}

	_, err = stdout.Write([]byte(`{"type":"text","timestamp":"2024-01-01T00:00:00Z","sessionID":"sess-preformatted","part":{"type":"text","text":"hello"}}` + "\n"))
	if err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}

	select {
	case e := <-oc.Events():
		if e.Type != EventSessionDetected {
			t.Fatalf("expected session_detected event, got %s", e.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for session_detected event")
	}

	select {
	case e := <-oc.Events():
		if e.Type != EventText {
			t.Fatalf("expected text event, got %s", e.Type)
		}
		if e.Content != "hello" {
			t.Fatalf("expected content hello, got %q", e.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for text event")
	}
}

func TestSessionOutputKeepsStepAndErrorEvents(t *testing.T) {
	oc := NewOpenCodeCapture()
	_, stdout, cleanup, err := oc.WrapCommand("opencode run --format json --issue 123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()

	_, err = stdout.Write([]byte(`{"type":"step_start","timestamp":"2024-01-01T00:00:00Z","sessionID":"sess-steps"}` + "\n"))
	if err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}
	_, err = stdout.Write([]byte(`{"type":"step_finish","timestamp":"2024-01-01T00:00:01Z","sessionID":"sess-steps"}` + "\n"))
	if err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}
	_, err = stdout.Write([]byte(`{"type":"error","timestamp":"2024-01-01T00:00:02Z","sessionID":"sess-steps","error":{"message":"boom"}}` + "\n"))
	if err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}

	for i := 0; i < 4; i++ {
		<-oc.Events()
	}

	sessions, err := oc.Stop()
	if err != nil {
		t.Fatalf("unexpected stop error: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected one session, got %+v", sessions)
	}
	parts := sessions[0].Messages[0].Parts
	if len(parts) != 3 {
		t.Fatalf("expected 3 parts, got %+v", parts)
	}
	if parts[0].Text != "..." || parts[1].Text != "OK" || parts[2].Text != "✗ boom" {
		t.Fatalf("unexpected parts: %+v", parts)
	}
}

func TestStopClosesChannel(t *testing.T) {
	oc := NewOpenCodeCapture()
	_, _, cleanup, err := oc.WrapCommand("opencode run --issue 123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cleanup()
	_, _ = oc.Stop()

	select {
	case _, ok := <-oc.Events():
		if ok {
			t.Fatal("expected channel to be closed after Stop")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for channel close")
	}
}

func TestStopReturnsParentAndChildSessions(t *testing.T) {
	oc := NewOpenCodeCapture()
	var mu sync.Mutex
	callIndex := 0
	runner := func(args ...string) ([]byte, error) {
		mu.Lock()
		i := callIndex
		callIndex++
		mu.Unlock()
		switch i {
		case 0:
			return []byte("/home/user/.opencode/sessions.db\n"), nil
		case 1:
			return []byte(`[{"id":"child-1","title":"Explore","agent":"opencode","time_updated":100}]`), nil
		case 2:
			return []byte(`[{"id":"m1","session_id":"child-1","data":"{\"role\":\"assistant\"}"}]`), nil
		case 3:
			return []byte(`[{"id":"p1","message_id":"m1","session_id":"child-1","data":"{\"type\":\"text\",\"text\":\"child hello\"}"}]`), nil
		default:
			return []byte(`[]`), nil
		}
	}
	poller := &DBPoller{runner: runner, issue: 42, pollInterval: time.Millisecond, writeFile: func(path string, data []byte) error { return nil }}
	oc.SetDBPoller(poller)

	_, stdout, cleanup, err := oc.WrapCommand("opencode run --issue 123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()

	_, err = stdout.Write([]byte(`{"type":"text","timestamp":"2024-01-01T00:00:00Z","sessionID":"parent-1","part":{"type":"text","text":"parent hello"}}` + "\n"))
	if err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}

	select {
	case e := <-oc.Events():
		if e.Type != EventSessionDetected {
			t.Fatalf("expected session_detected event, got %s", e.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for session_detected event")
	}

	select {
	case e := <-oc.Events():
		if e.Type != EventText || e.Content != "parent hello" {
			t.Fatalf("expected parent text event, got %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for parent text event")
	}

	select {
	case e := <-oc.Events():
		if e.Type != EventSubagentStart {
			t.Fatalf("expected child start event, got %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for child start event")
	}

	sessions, err := oc.Stop()
	if err != nil {
		t.Fatalf("unexpected stop error: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %+v", sessions)
	}
	if sessions[0].SessionID != "parent-1" || len(sessions[0].Messages) == 0 || len(sessions[0].Messages[0].Parts) == 0 {
		t.Fatalf("expected parent session output first, got %+v", sessions[0])
	}
	if sessions[1].SessionID != "child-1" || len(sessions[1].Messages) == 0 || len(sessions[1].Messages[0].Parts) == 0 {
		t.Fatalf("expected child session output second, got %+v", sessions[1])
	}
	if got := sessions[1].Messages[0].Parts[0].Text; got != "child hello" {
		t.Fatalf("expected child message text, got %q", got)
	}
}

func TestJSONParserExtractsSessionID(t *testing.T) {
	oc := NewOpenCodeCapture()
	wrapped, stdout, cleanup, err := oc.WrapCommand("opencode run --issue 123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()

	if wrapped != "opencode run --format json --issue 123" {
		t.Fatalf("expected wrapped command with --format json")
	}

	// Simulate JSON event stream written to stdout
	eventLine := `{"type":"text","timestamp":"2024-01-01T00:00:00Z","sessionID":"sess-abc-123","content":"hello"}`
	_, err = stdout.Write([]byte(eventLine + "\n"))
	if err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}

	// Wait for session_detected event
	select {
	case e := <-oc.Events():
		if e.Type != EventSessionDetected {
			t.Errorf("expected session_detected event, got %s", e.Type)
		}
		if e.SessionID != "sess-abc-123" {
			t.Errorf("expected sessionID sess-abc-123, got %s", e.SessionID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for session_detected event")
	}
}

func TestJSONParserEmitsTextEvent(t *testing.T) {
	oc := NewOpenCodeCapture()
	_, stdout, cleanup, err := oc.WrapCommand("opencode run --issue 123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()

	eventLine := `{"type":"text","timestamp":"2024-01-01T00:00:00Z","sessionID":"sess-abc-123","part":{"type":"text","text":"Hello world"}}`
	_, err = stdout.Write([]byte(eventLine + "\n"))
	if err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}

	// Wait for session_detected first
	<-oc.Events()

	// Now expect a text event
	select {
	case e := <-oc.Events():
		if e.Type != EventText {
			t.Errorf("expected text event, got %s", e.Type)
		}
		if e.Content != "Hello world" {
			t.Errorf("expected content 'Hello world', got %q", e.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for text event")
	}
}

func TestJSONParserEmitsReasoningEvent(t *testing.T) {
	oc := NewOpenCodeCapture()
	_, stdout, cleanup, err := oc.WrapCommand("opencode run --issue 123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()

	eventLine := `{"type":"reasoning","timestamp":"2024-01-01T00:00:00Z","sessionID":"sess-abc-123","part":{"type":"reasoning","text":"Thinking step by step..."}}`
	_, err = stdout.Write([]byte(eventLine + "\n"))
	if err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}

	<-oc.Events()

	select {
	case e := <-oc.Events():
		if e.Type != EventReasoning {
			t.Errorf("expected reasoning event, got %s", e.Type)
		}
		if e.Content != "Thinking step by step..." {
			t.Errorf("expected content 'Thinking step by step...', got %q", e.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for reasoning event")
	}
}

func TestJSONParserEmitsToolEvent(t *testing.T) {
	oc := NewOpenCodeCapture()
	_, stdout, cleanup, err := oc.WrapCommand("opencode run --issue 123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()

	eventLine := `{"type":"tool_use","timestamp":"2024-01-01T00:00:00Z","sessionID":"sess-abc-123","part":{"type":"tool","tool":"Read","state":{"status":"completed","input":{"path":"foo.go"},"output":"content"}}}`
	_, err = stdout.Write([]byte(eventLine + "\n"))
	if err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}

	<-oc.Events()

	select {
	case e := <-oc.Events():
		if e.Type != EventTool {
			t.Errorf("expected tool event, got %s", e.Type)
		}
		if e.Title != "Read" {
			t.Errorf("expected tool name 'Read', got %q", e.Title)
		}
		if e.Content != "completed" {
			t.Errorf("expected status 'completed', got %q", e.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for tool event")
	}
}

func TestJSONParserEmitsErrorEvent(t *testing.T) {
	oc := NewOpenCodeCapture()
	_, stdout, cleanup, err := oc.WrapCommand("opencode run --issue 123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()

	eventLine := `{"type":"error","timestamp":"2024-01-01T00:00:00Z","sessionID":"sess-abc-123","error":{"message":"something went wrong"}}`
	_, err = stdout.Write([]byte(eventLine + "\n"))
	if err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}

	<-oc.Events()

	select {
	case e := <-oc.Events():
		if e.Type != EventError {
			t.Errorf("expected error event, got %s", e.Type)
		}
		if e.Content != "something went wrong" {
			t.Errorf("expected error message 'something went wrong', got %q", e.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for error event")
	}
}

func TestJSONParserEmitsStepEvents(t *testing.T) {
	oc := NewOpenCodeCapture()
	_, stdout, cleanup, err := oc.WrapCommand("opencode run --issue 123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()

	_, err = stdout.Write([]byte(`{"type":"step_start","timestamp":"2024-01-01T00:00:00Z","sessionID":"sess-abc-123","part":{"type":"step-start"}}` + "\n"))
	if err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}

	<-oc.Events()

	select {
	case e := <-oc.Events():
		if e.Type != EventStepStart {
			t.Errorf("expected EventStepStart for step_start, got %s", e.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for step_start event")
	}
}

func TestJSONParserEmitsStepFinishEvent(t *testing.T) {
	oc := NewOpenCodeCapture()
	_, stdout, cleanup, err := oc.WrapCommand("opencode run --issue 123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()

	_, err = stdout.Write([]byte(`{"type":"step_finish","timestamp":"2024-01-01T00:00:00Z","sessionID":"sess-abc-123","part":{"type":"step-finish"}}` + "\n"))
	if err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}

	<-oc.Events()

	select {
	case e := <-oc.Events():
		if e.Type != EventStepFinish {
			t.Errorf("expected EventStepFinish for step_finish, got %s", e.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for step_finish event")
	}
}

func TestJSONParserFiltersBySessionID(t *testing.T) {
	oc := NewOpenCodeCapture()
	_, stdout, cleanup, err := oc.WrapCommand("opencode run --issue 123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()

	parentSession := `{"type":"text","timestamp":"2024-01-01T00:00:00Z","sessionID":"parent-1","part":{"type":"text","text":"parent text"}}`
	subSession := `{"type":"text","timestamp":"2024-01-01T00:00:01Z","sessionID":"child-1","part":{"type":"text","text":"child text"}}`

	_, _ = stdout.Write([]byte(parentSession + "\n"))
	<-oc.Events()
	<-oc.Events()

	_, _ = stdout.Write([]byte(subSession + "\n"))

	select {
	case e := <-oc.Events():
		t.Errorf("expected child session event to be filtered out, got sessionID %s", e.SessionID)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestJSONParserPassthroughNonJSONLines(t *testing.T) {
	oc := NewOpenCodeCapture()
	_, stdout, cleanup, err := oc.WrapCommand("opencode run --issue 123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()

	_, _ = stdout.Write([]byte("Hello from agent\n"))

	select {
	case e := <-oc.Events():
		if e.Type != EventText {
			t.Errorf("expected text event for non-JSON line, got %s", e.Type)
		}
		if e.Content != "Hello from agent" {
			t.Errorf("expected content 'Hello from agent', got %q", e.Content)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for passthrough event")
	}
}

func TestJSONParserPassthroughThenDetectsSession(t *testing.T) {
	oc := NewOpenCodeCapture()
	_, stdout, cleanup, err := oc.WrapCommand("opencode run --issue 123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()

	// Write invalid lines first, then valid event
	_, _ = stdout.Write([]byte("not json\n"))
	_, _ = stdout.Write([]byte("{bad json\n"))
	_, _ = stdout.Write([]byte(`{"type":"text","timestamp":"2024-01-01T00:00:00Z","sessionID":"sess-valid","content":"ok"}` + "\n"))

	// Drain passthrough events for non-JSON lines
	for i := 0; i < 2; i++ {
		select {
		case e := <-oc.Events():
			if e.Type != EventText {
				t.Fatalf("expected passthrough EventText, got %s", e.Type)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for passthrough event")
		}
	}

	select {
	case e := <-oc.Events():
		if e.Type != EventSessionDetected {
			t.Errorf("expected session_detected event, got %s", e.Type)
		}
		if e.SessionID != "sess-valid" {
			t.Errorf("expected sessionID sess-valid, got %s", e.SessionID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for session_detected event")
	}
}
