package events

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// JSONLLogger writes events to a JSONL file.
type JSONLLogger struct {
	Path string
}

// Log appends a single event atomically.
func (l *JSONLLogger) Log(event Event) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}

	f, err := os.OpenFile(l.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open event log: %w", err)
	}
	defer f.Close()

	data = append(data, '\n')
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write event: %w", err)
	}
	return nil
}

// Read returns all events from the log.
func (l *JSONLLogger) Read() ([]Event, error) {
	data, err := os.ReadFile(l.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return []Event{}, nil
		}
		return nil, fmt.Errorf("read event log: %w", err)
	}

	var events []Event
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("unmarshal event line: %w", err)
		}
		events = append(events, e)
	}
	return events, nil
}

// Ensure JSONLLogger implements EventLog.
var _ EventLog = (*JSONLLogger)(nil)
