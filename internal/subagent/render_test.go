package subagent

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestRenderEventsText(t *testing.T) {
	var buf bytes.Buffer
	events := make(chan Event, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go RenderEvents(ctx, 42, events, &buf, nil)

	events <- Event{
		Type:      EventText,
		Content:   "Hello world",
		Timestamp: time.Date(2024, 1, 1, 10, 30, 0, 0, time.UTC),
	}

	time.Sleep(50 * time.Millisecond)
	cancel()

	output := buf.String()
	if !strings.Contains(output, "[issue-42]") {
		t.Errorf("expected [issue-42] prefix, got %q", output)
	}
	if !strings.Contains(output, "10:30:00") {
		t.Errorf("expected timestamp 10:30:00, got %q", output)
	}
	if !strings.Contains(output, "Hello world") {
		t.Errorf("expected content 'Hello world', got %q", output)
	}
}

func TestRenderEventsMultipleEvents(t *testing.T) {
	var buf bytes.Buffer
	events := make(chan Event, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go RenderEvents(ctx, 7, events, &buf, nil)

	events <- Event{Type: EventText, Content: "first", Timestamp: time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)}
	events <- Event{Type: EventText, Content: "second", Timestamp: time.Date(2024, 1, 1, 10, 0, 1, 0, time.UTC)}
	events <- Event{Type: EventText, Content: "third", Timestamp: time.Date(2024, 1, 1, 10, 0, 2, 0, time.UTC)}

	time.Sleep(50 * time.Millisecond)
	cancel()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Errorf("expected 3 lines, got %d", len(lines))
	}
}

func TestRenderEventsToolPlain(t *testing.T) {
	var buf bytes.Buffer
	events := make(chan Event, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go RenderEvents(ctx, 1, events, &buf, nil)

	events <- Event{
		Type:      EventTool,
		Title:     "Read",
		Content:   "completed",
		Timestamp: time.Date(2024, 1, 1, 10, 30, 0, 0, time.UTC),
	}

	time.Sleep(50 * time.Millisecond)
	cancel()

	output := buf.String()
	if !strings.Contains(output, "\u2500 Read completed") {
		t.Errorf("expected '─ Read completed', got %q", output)
	}
}

func TestRenderEventsStepStart(t *testing.T) {
	var buf bytes.Buffer
	events := make(chan Event, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go RenderEvents(ctx, 1, events, &buf, nil)

	events <- Event{
		Type:      EventStepStart,
		Timestamp: time.Date(2024, 1, 1, 10, 30, 0, 0, time.UTC),
	}

	time.Sleep(50 * time.Millisecond)
	cancel()

	output := buf.String()
	if !strings.HasSuffix(strings.TrimSpace(output), "...") {
		t.Errorf("expected '...' suffix, got %q", output)
	}
}

func TestRenderSubagentStart(t *testing.T) {
	var buf bytes.Buffer
	events := make(chan Event, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go RenderEvents(ctx, 42, events, &buf, nil)

	events <- Event{
		Type:      EventSubagentStart,
		SessionID: "child-1",
		Agent:     "explore",
		Title:     "Explore auth codebase",
		Timestamp: time.Date(2024, 1, 1, 10, 15, 35, 0, time.UTC),
	}

	time.Sleep(50 * time.Millisecond)
	cancel()

	output := strings.TrimSpace(buf.String())
	want := "[issue-42] 10:15:35  └─ @explore subagent: Explore auth codebase"
	if output != want {
		t.Errorf("got  %q\nwant %q", output, want)
	}
}

func TestRenderSubagentTextIndented(t *testing.T) {
	var buf bytes.Buffer
	events := make(chan Event, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go RenderEvents(ctx, 42, events, &buf, nil)

	events <- Event{
		Type:      EventSubagentStart,
		SessionID: "child-1",
		ParentID:  "parent-1",
		Agent:     "explore",
		Title:     "Explore auth codebase",
		Timestamp: time.Date(2024, 1, 1, 10, 15, 35, 0, time.UTC),
	}
	events <- Event{
		Type:      EventText,
		SessionID: "child-1",
		ParentID:  "parent-1",
		Content:   "Found something important",
		Timestamp: time.Date(2024, 1, 1, 10, 15, 36, 0, time.UTC),
	}

	time.Sleep(50 * time.Millisecond)
	cancel()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	want := "[issue-42] 10:15:36     └─ Found something important"
	if lines[1] != want {
		t.Errorf("got  %q\nwant %q", lines[1], want)
	}
}

func TestRenderSubagentToolCall(t *testing.T) {
	var buf bytes.Buffer
	events := make(chan Event, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go RenderEvents(ctx, 42, events, &buf, nil)

	events <- Event{
		Type:      EventSubagentStart,
		SessionID: "child-1",
		Agent:     "explore",
		Title:     "Explore",
		Timestamp: time.Date(2024, 1, 1, 10, 15, 35, 0, time.UTC),
	}
	events <- Event{
		Type:      EventTool,
		SessionID: "child-1",
		Title:     "Read",
		Content:   "middleware/auth.go",
		Timestamp: time.Date(2024, 1, 1, 10, 15, 36, 0, time.UTC),
	}

	time.Sleep(50 * time.Millisecond)
	cancel()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	want := "[issue-42] 10:15:36     └─ [Read] middleware/auth.go"
	if lines[1] != want {
		t.Errorf("got  %q\nwant %q", lines[1], want)
	}
}

func TestRenderSubagentFinish(t *testing.T) {
	var buf bytes.Buffer
	events := make(chan Event, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go RenderEvents(ctx, 42, events, &buf, nil)

	events <- Event{
		Type:      EventSubagentStart,
		SessionID: "child-1",
		Agent:     "explore",
		Title:     "Explore auth codebase",
		Timestamp: time.Date(2024, 1, 1, 10, 15, 35, 0, time.UTC),
	}
	events <- Event{
		Type:      EventSubagentFinish,
		SessionID: "child-1",
		Agent:     "explore",
		Timestamp: time.Date(2024, 1, 1, 10, 15, 42, 0, time.UTC),
	}

	time.Sleep(50 * time.Millisecond)
	cancel()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	want := "[issue-42] 10:15:42  └─ @explore subagent: finished"
	if lines[1] != want {
		t.Errorf("got  %q\nwant %q", lines[1], want)
	}
}

func TestRenderSubagentReasoningNonTTY(t *testing.T) {
	var buf bytes.Buffer
	events := make(chan Event, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go RenderEvents(ctx, 42, events, &buf, nil)

	events <- Event{
		Type:      EventSubagentStart,
		SessionID: "child-1",
		Agent:     "explore",
		Title:     "Explore",
		Timestamp: time.Date(2024, 1, 1, 10, 15, 35, 0, time.UTC),
	}
	events <- Event{
		Type:      EventReasoning,
		SessionID: "child-1",
		Content:   "thinking about the problem",
		Timestamp: time.Date(2024, 1, 1, 10, 15, 36, 0, time.UTC),
	}

	time.Sleep(50 * time.Millisecond)
	cancel()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	want := "[issue-42] 10:15:36     └─ [thinking] thinking about the problem"
	if lines[1] != want {
		t.Errorf("got  %q\nwant %q", lines[1], want)
	}
}

func TestRenderSubagentInterleaved(t *testing.T) {
	var buf bytes.Buffer
	events := make(chan Event, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go RenderEvents(ctx, 42, events, &buf, nil)

	events <- Event{
		Type:      EventText,
		Content:   "Starting work",
		Timestamp: time.Date(2024, 1, 1, 10, 15, 30, 0, time.UTC),
	}
	events <- Event{
		Type:      EventSubagentStart,
		SessionID: "child-1",
		Agent:     "explore",
		Title:     "Explore",
		Timestamp: time.Date(2024, 1, 1, 10, 15, 35, 0, time.UTC),
	}
	events <- Event{
		Type:      EventTool,
		SessionID: "child-1",
		Title:     "read",
		Content:   "auth.go",
		Timestamp: time.Date(2024, 1, 1, 10, 15, 36, 0, time.UTC),
	}
	events <- Event{
		Type:      EventSubagentFinish,
		SessionID: "child-1",
		Agent:     "explore",
		Timestamp: time.Date(2024, 1, 1, 10, 15, 42, 0, time.UTC),
	}

	time.Sleep(50 * time.Millisecond)
	cancel()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "Starting work") {
		t.Errorf("line 0 should be main agent output: %q", lines[0])
	}
	if !strings.Contains(lines[1], "└─ @explore subagent: Explore") {
		t.Errorf("line 1 should be subagent start: %q", lines[1])
	}
	if !strings.Contains(lines[2], "    └─ [read] auth.go") {
		t.Errorf("line 2 should be indented tool: %q", lines[2])
	}
	if !strings.Contains(lines[3], "└─ @explore subagent: finished") {
		t.Errorf("line 3 should be subagent finish: %q", lines[3])
	}
}

func TestRenderSubagentMultiLineText(t *testing.T) {
	var buf bytes.Buffer
	events := make(chan Event, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go RenderEvents(ctx, 42, events, &buf, nil)

	events <- Event{
		Type:      EventSubagentStart,
		SessionID: "child-1",
		Agent:     "explore",
		Title:     "Explore",
		Timestamp: time.Date(2024, 1, 1, 10, 15, 35, 0, time.UTC),
	}
	events <- Event{
		Type:      EventText,
		SessionID: "child-1",
		Content:   "first line\nsecond line\nthird line",
		Timestamp: time.Date(2024, 1, 1, 10, 15, 36, 0, time.UTC),
	}

	time.Sleep(50 * time.Millisecond)
	cancel()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines (start + 3 text lines), got %d", len(lines))
	}
	want1 := "[issue-42] 10:15:36     └─ first line"
	if lines[1] != want1 {
		t.Errorf("line 1: got %q, want %q", lines[1], want1)
	}
	want2 := "[issue-42] 10:15:36     └─ second line"
	if lines[2] != want2 {
		t.Errorf("line 2: got %q, want %q", lines[2], want2)
	}
	want3 := "[issue-42] 10:15:36     └─ third line"
	if lines[3] != want3 {
		t.Errorf("line 3: got %q, want %q", lines[3], want3)
	}
}

func TestRenderEventsChannelClose(t *testing.T) {
	var buf bytes.Buffer
	events := make(chan Event, 10)
	ctx := context.Background()

	go RenderEvents(ctx, 1, events, &buf, nil)

	events <- Event{
		Type:      EventText,
		Content:   "last message",
		Timestamp: time.Date(2024, 1, 1, 10, 30, 0, 0, time.UTC),
	}
	close(events)

	time.Sleep(50 * time.Millisecond)

	output := buf.String()
	if !strings.Contains(output, "last message") {
		t.Errorf("expected 'last message' in output, got %q", output)
	}
}

func TestRenderEventsWritesToLogWriter(t *testing.T) {
	var outBuf bytes.Buffer
	var logBuf bytes.Buffer
	events := make(chan Event, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go RenderEvents(ctx, 42, events, &outBuf, &logBuf)

	events <- Event{
		Type:      EventText,
		Content:   "Hello world",
		Timestamp: time.Date(2024, 1, 1, 10, 30, 0, 0, time.UTC),
	}

	time.Sleep(50 * time.Millisecond)
	cancel()

	if !strings.Contains(outBuf.String(), "[issue-42]") {
		t.Errorf("expected [issue-42] prefix in stdout, got %q", outBuf.String())
	}
	if strings.Contains(logBuf.String(), "[issue-42]") {
		t.Errorf("expected no issue prefix in log, got %q", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "Hello world") {
		t.Errorf("expected 'Hello world' in log, got %q", logBuf.String())
	}
}

func TestRenderEventsIgnoresSessionDetected(t *testing.T) {
	var buf bytes.Buffer
	events := make(chan Event, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go RenderEvents(ctx, 42, events, &buf, nil)

	events <- Event{
		Type:      EventSessionDetected,
		SessionID: "sess-123",
		Timestamp: time.Date(2024, 1, 1, 10, 30, 0, 0, time.UTC),
	}
	events <- Event{
		Type:      EventText,
		Content:   "Hello world",
		Timestamp: time.Date(2024, 1, 1, 10, 30, 1, 0, time.UTC),
	}

	time.Sleep(50 * time.Millisecond)
	cancel()

	output := buf.String()
	if strings.Contains(output, "Session sess-123 started") {
		t.Errorf("expected session banner to be hidden, got %q", output)
	}
	if !strings.Contains(output, "Hello world") {
		t.Errorf("expected text output, got %q", output)
	}
}
