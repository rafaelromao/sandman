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

	go RenderEvents(ctx, 42, events, &buf)

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

	go RenderEvents(ctx, 7, events, &buf)

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

	go RenderEvents(ctx, 1, events, &buf)

	events <- Event{
		Type:      EventTool,
		Title:     "Read",
		Timestamp: time.Date(2024, 1, 1, 10, 30, 0, 0, time.UTC),
	}

	time.Sleep(50 * time.Millisecond)
	cancel()

	output := buf.String()
	if !strings.Contains(output, "\u2500 Read") {
		t.Errorf("expected '─ Read', got %q", output)
	}
}

func TestRenderEventsStepStart(t *testing.T) {
	var buf bytes.Buffer
	events := make(chan Event, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go RenderEvents(ctx, 1, events, &buf)

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

func TestRenderEventsChannelClose(t *testing.T) {
	var buf bytes.Buffer
	events := make(chan Event, 10)
	ctx := context.Background()

	go RenderEvents(ctx, 1, events, &buf)

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
