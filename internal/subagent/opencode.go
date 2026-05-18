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
	events    chan Event
	mu        sync.Mutex
	sessionID string
	stopped   bool
	wg        sync.WaitGroup
}

// NewOpenCodeCapture creates a new OpenCodeCapture instance.
func NewOpenCodeCapture() *OpenCodeCapture {
	return &OpenCodeCapture{
		events: make(chan Event, 64),
	}
}

// SessionID returns the detected session ID, if any.
func (o *OpenCodeCapture) SessionID() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.sessionID
}

func (o *OpenCodeCapture) WrapCommand(command string) (string, io.Writer, func(), error) {
	trimmed := strings.TrimSpace(command)
	if !strings.HasPrefix(trimmed, "opencode run") {
		return command, nil, func() {}, nil
	}

	if strings.Contains(trimmed, "--format") {
		return trimmed, nil, func() {}, nil
	}

	wrapped := strings.Replace(trimmed, "opencode run", "opencode run --format json", 1)

	pr, pw := io.Pipe()
	o.wg.Add(1)
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
	if o.stopped {
		o.mu.Unlock()
		return nil, nil
	}
	o.stopped = true
	o.mu.Unlock()
	o.wg.Wait()
	close(o.events)
	return nil, nil
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

		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue
		}

		var sessionID, tsStr string
		if v, ok := raw["sessionID"]; ok {
			json.Unmarshal(v, &sessionID)
		}
		if v, ok := raw["timestamp"]; ok {
			json.Unmarshal(v, &tsStr)
		}

		var timestamp time.Time
		if tsStr != "" {
			timestamp, _ = time.Parse(time.RFC3339, tsStr)
		}

		o.mu.Lock()
		isNew := o.sessionID == "" && sessionID != ""
		if isNew {
			o.sessionID = sessionID
		}
		o.mu.Unlock()

		if isNew {
			select {
			case o.events <- Event{
				SessionID: sessionID,
				Type:      EventSessionDetected,
				Timestamp: timestamp,
			}:
			default:
			}
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("opencode capture: scanner error: %v", err)
	}
}
