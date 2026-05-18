package subagent

import (
	"io"
	"time"
)

type EventType string

const (
	EventSessionDetected EventType = "session_detected"
	EventText            EventType = "text"
	EventReasoning       EventType = "reasoning"
	EventTool            EventType = "tool"
	EventError           EventType = "error"
)

type PartType string

const (
	PartTypeText      PartType = "text"
	PartTypeReasoning PartType = "reasoning"
	PartTypeTool      PartType = "tool"
)

type Event struct {
	SessionID string
	ParentID  string
	Type      EventType
	Title     string
	Agent     string
	Content   string
	Timestamp time.Time
}

type Part struct {
	Type       PartType
	Text       string
	ToolName   string
	ToolInput  string
	ToolOutput string
}

type Message struct {
	Role  string
	Parts []Part
}

type SessionOutput struct {
	SessionID string
	Title     string
	Agent     string
	Messages  []Message
}

type Capture interface {
	WrapCommand(command string) (wrapped string, stdout io.Writer, cleanup func(), err error)
	Events() <-chan Event
	Stop() ([]SessionOutput, error)
}
