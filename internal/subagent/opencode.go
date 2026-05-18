package subagent

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"strings"
	"sync"
	"time"
)

// OpenCodeCapture captures output from OpenCode agent sessions.
type OpenCodeCapture struct {
	events        chan Event
	mu            sync.Mutex
	sessionID     string
	stopped       bool
	wg            sync.WaitGroup
	pw            io.WriteCloser
	dbPoller      *DBPoller
	sessionOutput SessionOutput
}

// NewOpenCodeCapture creates a new OpenCodeCapture instance.
func NewOpenCodeCapture() *OpenCodeCapture {
	return &OpenCodeCapture{
		events: make(chan Event, 64),
	}
}

// SetDBPoller sets the DB poller for discovering subagent sessions.
func (o *OpenCodeCapture) SetDBPoller(poller *DBPoller) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.dbPoller = poller
}

// SessionID returns the detected session ID, if any.
func (o *OpenCodeCapture) SessionID() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.sessionID
}

func (o *OpenCodeCapture) WrapCommand(command string) (string, io.Writer, func(), error) {
	trimmed := strings.TrimSpace(command)
	if !strings.HasPrefix(trimmed, "opencode run ") && trimmed != "opencode run" {
		return command, nil, func() {}, nil
	}

	if strings.Contains(trimmed, " --format ") || strings.HasSuffix(trimmed, " --format") || strings.Contains(trimmed, " --format=") {
		return trimmed, nil, func() {}, nil
	}

	wrapped := strings.Replace(trimmed, "opencode run", "opencode run --format json", 1)

	pr, pw := io.Pipe()
	o.pw = pw
	o.wg.Add(1)
	go o.parseStream(pr)

	cleanup := func() {
		_ = pw.Close()
	}

	return wrapped, pw, cleanup, nil
}

func (o *OpenCodeCapture) Events() <-chan Event {
	return o.events
}

func (o *OpenCodeCapture) Stop() ([]SessionOutput, error) {
	o.mu.Lock()
	if o.stopped {
		o.mu.Unlock()
		return nil, nil
	}
	o.stopped = true
	pw := o.pw
	poller := o.dbPoller
	o.mu.Unlock()
	if poller != nil {
		poller.Stop()
	}
	if pw != nil {
		_ = pw.Close()
	}
	o.wg.Wait()
	close(o.events)

	o.mu.Lock()
	result := o.sessionOutput
	o.mu.Unlock()

	if result.SessionID == "" && len(result.Messages) == 0 {
		return nil, nil
	}
	return []SessionOutput{result}, nil
}

func (o *OpenCodeCapture) parseStream(reader io.Reader) {
	defer o.wg.Done()
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		o.mu.Lock()
		stopped := o.stopped
		o.mu.Unlock()
		if stopped {
			return
		}

		line := scanner.Text()
		if line == "" {
			continue
		}

		event := o.parseJSONLine(line)
		if event == nil {
			continue
		}
		o.mu.Lock()
		isNew := o.sessionID == "" && event.SessionID != ""
		if isNew {
			o.sessionID = event.SessionID
			if o.dbPoller != nil {
				o.dbPoller.Start(event.SessionID)
			}
		}
		matches := o.sessionID == event.SessionID || event.SessionID == ""
		o.mu.Unlock()

		if isNew {
			select {
			case o.events <- Event{
				SessionID: event.SessionID,
				Type:      EventSessionDetected,
				Timestamp: event.Timestamp,
			}:
			default:
			}
		}
		if matches {
			select {
			case o.events <- *event:
			default:
			}
		}
		o.accumulateEvent(event)
	}
	if err := scanner.Err(); err != nil {
		log.Printf("opencode capture: scanner error: %v", err)
	}
}

func (o *OpenCodeCapture) accumulateEvent(event *Event) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if event.SessionID != "" && o.sessionOutput.SessionID == "" {
		o.sessionOutput.SessionID = event.SessionID
	}

	var part Part
	switch event.Type {
	case EventText:
		part = Part{Type: PartTypeText, Text: event.Content}
	case EventReasoning:
		part = Part{Type: PartTypeReasoning, Text: event.Content}
	case EventTool:
		part = Part{Type: PartTypeTool, ToolName: event.Title, ToolOutput: event.Content}
	default:
		return
	}

	if len(o.sessionOutput.Messages) == 0 {
		o.sessionOutput.Messages = append(o.sessionOutput.Messages, Message{Role: "assistant"})
	}
	o.sessionOutput.Messages[0].Parts = append(o.sessionOutput.Messages[0].Parts, part)
}

type openCodeState struct {
	Status string `json:"status"`
}

type openCodePart struct {
	Type  string         `json:"type"`
	Text  string         `json:"text"`
	Tool  string         `json:"tool"`
	State *openCodeState `json:"state,omitempty"`
}

type openCodeError struct {
	Message string `json:"message"`
}

type openCodeEvent struct {
	Type      string         `json:"type"`
	Timestamp string         `json:"timestamp"`
	SessionID string         `json:"sessionID"`
	Part      *openCodePart  `json:"part,omitempty"`
	Error     *openCodeError `json:"error,omitempty"`
}

func (o *OpenCodeCapture) parseJSONLine(line string) *Event {
	var raw openCodeEvent
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		textEvent := Event{
			Type:    EventText,
			Content: line,
		}
		return &textEvent
	}

	var timestamp time.Time
	if raw.Timestamp != "" {
		timestamp, _ = time.Parse(time.RFC3339, raw.Timestamp)
	}

	ev := Event{
		SessionID: raw.SessionID,
		Timestamp: timestamp,
	}

	switch raw.Type {
	case "text":
		ev.Type = EventText
		if raw.Part != nil {
			ev.Content = raw.Part.Text
		}
	case "reasoning":
		ev.Type = EventReasoning
		if raw.Part != nil {
			ev.Content = raw.Part.Text
		}
	case "tool_use":
		ev.Type = EventTool
		if raw.Part != nil {
			ev.Title = raw.Part.Tool
			if raw.Part.State != nil {
				ev.Content = raw.Part.State.Status
			}
		}
	case "step_start":
		ev.Type = EventStepStart
	case "step_finish":
		ev.Type = EventStepFinish
	case "error":
		ev.Type = EventError
		if raw.Error != nil {
			ev.Content = raw.Error.Message
		}
	default:
		return nil
	}

	return &ev
}
