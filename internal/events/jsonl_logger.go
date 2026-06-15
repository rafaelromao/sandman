package events

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// JSONLLogger writes events to a JSONL file.
//
// Log, Read, and RemoveEventsByIssue are safe to call concurrently with each
// other. All three serialise their work through a single mutex over a
// long-lived *os.File so that an interleaved Log between Read and the
// rewrite in RemoveEventsByIssue cannot lose the appended line.
type JSONLLogger struct {
	Path string

	mu   sync.Mutex
	file *os.File
}

// Log appends a single event atomically.
func (l *JSONLLogger) Log(event Event) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	data = append(data, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()

	f, err := l.openLocked()
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write event: %w", err)
	}
	return nil
}

// Read returns all events from the log.
func (l *JSONLLogger) Read() ([]Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if _, err := os.Stat(l.Path); err != nil {
		if os.IsNotExist(err) {
			return []Event{}, nil
		}
		return nil, fmt.Errorf("stat event log: %w", err)
	}

	f, err := l.openLocked()
	if err != nil {
		return nil, err
	}

	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, fmt.Errorf("seek event log: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind event log: %w", err)
	}

	var buf []byte
	if size > 0 {
		buf = make([]byte, size)
		if _, err := io.ReadFull(f, buf); err != nil {
			return nil, fmt.Errorf("read event log: %w", err)
		}
	}

	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return nil, fmt.Errorf("restore event log position: %w", err)
	}

	if len(buf) == 0 {
		return []Event{}, nil
	}

	var events []Event
	lines := strings.Split(strings.TrimSpace(string(buf)), "\n")
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

// RemoveEventsByIssue removes all events matching the given issue number.
//
// An event matches if its Issue field equals issueNumber, or if its
// IssueRef pointer is non-nil and points to issueNumber. Read, filter,
// and rewrite all happen under the same mutex over the same held file
// handle, so a concurrent Log is either fully recorded before the
// rewrite starts or fully recorded after it completes.
func (l *JSONLLogger) RemoveEventsByIssue(issueNumber int) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	f, err := l.openLocked()
	if err != nil {
		return err
	}

	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("seek event log: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind event log: %w", err)
	}

	var kept []Event
	if size > 0 {
		buf := make([]byte, size)
		if _, err := io.ReadFull(f, buf); err != nil {
			return fmt.Errorf("read event log: %w", err)
		}
		lines := strings.Split(strings.TrimSpace(string(buf)), "\n")
		for _, line := range lines {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var e Event
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				return fmt.Errorf("unmarshal event line: %w", err)
			}
			if e.Issue == issueNumber {
				continue
			}
			if e.IssueRef != nil && *e.IssueRef == issueNumber {
				continue
			}
			kept = append(kept, e)
		}
	}

	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncate event log: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind event log: %w", err)
	}
	for _, e := range kept {
		data, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("marshal event: %w", err)
		}
		data = append(data, '\n')
		if _, err := f.Write(data); err != nil {
			return fmt.Errorf("write event: %w", err)
		}
	}
	return nil
}

// Close releases the held *os.File. Safe to call multiple times.
func (l *JSONLLogger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

// openLocked returns the held *os.File, opening it on first use. The
// caller must hold l.mu.
//
// If the file does not exist yet, openLocked creates it so the held
// handle is always writable. Callers that need to read a potentially
// absent log should check for IsNotExist before calling.
func (l *JSONLLogger) openLocked() (*os.File, error) {
	if l.file != nil {
		return l.file, nil
	}
	f, err := os.OpenFile(l.Path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, fmt.Errorf("open event log: %w", err)
	}
	l.file = f
	return f, nil
}

// Ensure JSONLLogger implements EventLog.
var _ EventLog = (*JSONLLogger)(nil)
