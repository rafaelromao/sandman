package events

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
)

// JSONLLogger writes events to a JSONL file.
//
// Log, Read, and RemoveEventsByIssue are safe to call concurrently with
// each other. The mutex serialises in-process callers; the underlying
// file is opened with O_APPEND so the kernel guarantees that
// concurrent writes from different processes (e.g. multiple sandman
// daemons writing to the same repo-scoped log) never interleave
// bytes from a single Log call.
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

	f, err := l.ensureOpen()
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

	f, err := l.ensureOpen()
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
			// A single torn line should not poison the whole read
			// (and take down the portal). Drop it with a warning so
			// the rest of the log stays usable; the next Log call
			// from a healthy daemon will append after the EOF and
			// the bad line stays quarantined above the new tail.
			log.Printf("events: skipping malformed event line (%d bytes): %v", len(line), err)
			continue
		}
		events = append(events, e)
	}
	return events, nil
}

// RemoveEventsByIssue removes all events matching the given issue number.
func (l *JSONLLogger) RemoveEventsByIssue(issueNumber int) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	f, err := l.ensureOpen()
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
				// Mirror Read's tolerance so a torn line does not
				// block RemoveEventsByIssue from rewriting the log
				// around it.
				log.Printf("events: skipping malformed event line during remove (%d bytes): %v", len(line), err)
				continue
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

func (l *JSONLLogger) ensureOpen() (*os.File, error) {
	if l.file != nil {
		return l.file, nil
	}
	// O_APPEND is mandatory: multiple sandman daemons (and the portal)
	// can write to the same repo-scoped events.jsonl. Without
	// O_APPEND, write(2) goes at the FD's current position, which is
	// independent per process, so two processes' writes interleave
	// at the byte level and tear every line longer than a single
	// pipe-sized write. O_APPEND makes the kernel position every
	// write at the current EOF atomically, which is exactly the
	// guarantee a JSONL log needs.
	f, err := os.OpenFile(l.Path, os.O_APPEND|os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, fmt.Errorf("open event log: %w", err)
	}
	l.file = f
	return f, nil
}

// Ensure JSONLLogger implements EventLog.
var _ EventLog = (*JSONLLogger)(nil)
