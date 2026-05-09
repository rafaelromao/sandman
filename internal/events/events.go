package events

import "time"

// Event is a single structured entry in the append-only JSONL event log.
type Event struct {
	Type      string         `json:"type"`
	Timestamp time.Time      `json:"timestamp"`
	RunID     string         `json:"run_id,omitempty"`
	Issue     int            `json:"issue,omitempty"`
	Payload   map[string]any `json:"payload,omitempty"`
}

// Logger writes events to the event log.
type Logger interface {
	Log(event Event) error
}

// Reader reads events from the event log.
type Reader interface {
	Read() ([]Event, error)
}
