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
	if poller != nil {
		poller.events = o.events
	}
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
	prefix, core := splitEnvAssignments(trimmed)
	if !strings.HasPrefix(core, "opencode run ") && core != "opencode run" {
		return command, nil, func() {}, nil
	}

	wrappedCore := core
	if !(strings.Contains(core, " --format ") || strings.HasSuffix(core, " --format") || strings.Contains(core, " --format=")) {
		wrappedCore = strings.Replace(core, "opencode run", "opencode run --format json", 1)
	}
	wrapped := strings.TrimSpace(prefix + wrappedCore)

	pr, pw := io.Pipe()
	o.pw = pw
	o.wg.Add(1)
	go o.parseStream(pr)

	cleanup := func() {
		_ = pw.Close()
	}

	return wrapped, pw, cleanup, nil
}

func splitEnvAssignments(command string) (string, string) {
	remaining := strings.TrimSpace(command)
	var prefix strings.Builder
	for remaining != "" {
		space := strings.IndexByte(remaining, ' ')
		if space < 0 {
			break
		}
		token := remaining[:space]
		if !looksLikeEnvAssignment(token) {
			break
		}
		prefix.WriteString(token)
		prefix.WriteByte(' ')
		remaining = strings.TrimLeft(remaining[space+1:], " ")
	}
	return prefix.String(), remaining
}

func looksLikeEnvAssignment(token string) bool {
	idx := strings.IndexByte(token, '=')
	return idx > 0 && !strings.HasPrefix(token, "--")
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

	var childOutputs []SessionOutput
	if poller != nil {
		childOutputs = poller.Stop()
	}
	if pw != nil {
		_ = pw.Close()
	}
	o.wg.Wait()
	close(o.events)

	o.mu.Lock()
	result := o.sessionOutput
	o.mu.Unlock()

	sessions := make([]SessionOutput, 0, 1+len(childOutputs))
	if result.SessionID != "" || len(result.Messages) > 0 {
		sessions = append(sessions, result)
	}
	sessions = append(sessions, childOutputs...)

	if len(sessions) == 0 {
		return nil, nil
	}
	return sessions, nil
}

func (o *OpenCodeCapture) parseStream(reader io.Reader) {
	defer o.wg.Done()
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
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
	case EventStepStart:
		part = Part{Type: PartTypeText, Text: "..."}
	case EventStepFinish:
		part = Part{Type: PartTypeText, Text: "OK"}
	case EventError:
		part = Part{Type: PartTypeText, Text: "✗ " + event.Content}
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
	Type      string          `json:"type"`
	Timestamp json.RawMessage `json:"timestamp"`
	SessionID string          `json:"sessionID"`
	Part      *openCodePart   `json:"part,omitempty"`
	Error     *openCodeError  `json:"error,omitempty"`
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
	if len(raw.Timestamp) > 0 && string(raw.Timestamp) != "null" {
		var tsString string
		if err := json.Unmarshal(raw.Timestamp, &tsString); err == nil {
			timestamp, _ = time.Parse(time.RFC3339, tsString)
		} else {
			var tsInt int64
			if err := json.Unmarshal(raw.Timestamp, &tsInt); err == nil {
				timestamp = time.UnixMilli(tsInt)
			}
		}
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
