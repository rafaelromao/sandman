package events

import "fmt"

// JSONLLogger writes events to a JSONL file.
type JSONLLogger struct {
	Path string
}

// Log appends a single event atomically.
func (l *JSONLLogger) Log(event Event) error {
	return fmt.Errorf("event logging not yet implemented")
}

// Read returns all events from the log.
func (l *JSONLLogger) Read() ([]Event, error) {
	return nil, fmt.Errorf("event reading not yet implemented")
}

// Ensure JSONLLogger implements both interfaces.
var _ Logger = (*JSONLLogger)(nil)
var _ Reader = (*JSONLLogger)(nil)
