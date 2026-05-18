package subagent

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"
)

type OpenCodeCapture struct {
	events    chan Event
	mu        sync.Mutex
	sessionID string
	stopped   bool
}

func NewOpenCodeCapture() *OpenCodeCapture {
	return &OpenCodeCapture{
		events: make(chan Event, 64),
	}
}

func (o *OpenCodeCapture) WrapCommand(command string) (string, io.Writer, func(), error) {
	if !strings.Contains(command, "opencode run") {
		return command, nil, func() {}, nil
	}

	wrapped := strings.Replace(command, "opencode run", "opencode run --format json", 1)

	pr, pw := io.Pipe()
	go o.parseStream(pr)

	cleanup := func() {
		o.mu.Lock()
		defer o.mu.Unlock()
		o.stopped = true
		pw.Close()
	}

	return wrapped, pw, cleanup, nil
}

func (o *OpenCodeCapture) Events() <-chan Event {
	return o.events
}

func (o *OpenCodeCapture) Stop() ([]SessionOutput, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.stopped = true
	close(o.events)
	return nil, nil
}

func (o *OpenCodeCapture) parseStream(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue
		}

		var sessionID, typ, tsStr, content string
		if v, ok := raw["sessionID"]; ok {
			json.Unmarshal(v, &sessionID)
		}
		if v, ok := raw["type"]; ok {
			json.Unmarshal(v, &typ)
		}
		if v, ok := raw["timestamp"]; ok {
			json.Unmarshal(v, &tsStr)
		}
		if v, ok := raw["content"]; ok {
			json.Unmarshal(v, &content)
		}

		var timestamp time.Time
		if tsStr != "" {
			timestamp, _ = time.Parse(time.RFC3339, tsStr)
		}

		o.mu.Lock()
		if o.sessionID == "" && sessionID != "" {
			o.sessionID = sessionID
			o.events <- Event{
				SessionID: sessionID,
				Type:      EventSessionDetected,
				Timestamp: timestamp,
			}
		}
		o.mu.Unlock()
	}
}
