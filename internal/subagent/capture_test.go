package subagent

import (
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

func TestJSONParserIgnoresInvalidLines(t *testing.T) {
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

	select {
	case e := <-oc.Events():
		if e.SessionID != "sess-valid" {
			t.Errorf("expected sessionID sess-valid, got %s", e.SessionID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for session_detected event")
	}
}
