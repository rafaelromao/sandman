package subagent

import (
	"io"
	"time"
)

// EventType represents the kind of capture event from an agent session.
type EventType string

const (
	EventSessionDetected EventType = "session_detected"
	EventText            EventType = "text"
	EventReasoning       EventType = "reasoning"
	EventTool            EventType = "tool"
	EventError           EventType = "error"
	EventStepStart       EventType = "step_start"
	EventStepFinish      EventType = "step_finish"
)

// PartType represents the kind of content in a message part.
type PartType string

const (
	PartTypeText      PartType = "text"
	PartTypeReasoning PartType = "reasoning"
	PartTypeTool      PartType = "tool"
)

// Event is a single structured event emitted during agent session capture.
type Event struct {
	SessionID string
	ParentID  string
	Type      EventType
	Title     string
	Agent     string
	Content   string
	Timestamp time.Time
}

// Part is a single content unit within a message.
type Part struct {
	Type       PartType
	Text       string
	ToolName   string
	ToolInput  string
	ToolOutput string
}

// Message represents a single message in a captured session.
type Message struct {
	Role  string
	Parts []Part
}

// SessionOutput holds the captured output for a single agent session.
type SessionOutput struct {
	SessionID string
	Title     string
	Agent     string
	Messages  []Message
}

// Capture wraps an agent command to intercept its output stream.
type Capture interface {
	WrapCommand(command string) (wrapped string, stdout io.Writer, cleanup func(), err error)
	Events() <-chan Event
	Stop() ([]SessionOutput, error)
}
