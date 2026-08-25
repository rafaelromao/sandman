package events

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"syscall"
)

// JSONLLogger writes events to a JSONL file. Every public operation takes an
// advisory flock on Path+".lock". The lock file is deliberately permanent:
// unlinking it can split mutual exclusion between processes holding old and
// newly-created lock inodes.
type JSONLLogger struct {
	Path string

	mu          sync.Mutex
	file        *os.File
	quarantined map[string]struct{}
	hooks       *jsonlLoggerHooks
}

// jsonlLoggerHooks is deliberately package-local. Tests use it to make
// otherwise rare filesystem failures deterministic.
type jsonlLoggerHooks struct {
	fail func(stage string) error
}

func (l *JSONLLogger) Log(event Event) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	data = append(data, '\n')

	return l.withLock(func() error {
		f, err := l.ensureOpen()
		if err != nil {
			return err
		}
		if _, err := f.Write(data); err != nil {
			return fmt.Errorf("write event: %w", err)
		}
		return nil
	})
}

// Read returns all valid events from the log, quarantining malformed lines
// without rewriting the main file.
func (l *JSONLLogger) Read() ([]Event, error) {
	// A repository without .sandman has no event log yet. Preserve Read's
	// historical empty result instead of creating runtime state for a probe.
	if _, err := os.Stat(l.Path); err != nil {
		if os.IsNotExist(err) {
			return []Event{}, nil
		}
		return nil, fmt.Errorf("stat event log: %w", err)
	}

	var events []Event
	err := l.withLock(func() error {
		f, err := l.ensureOpen()
		if err != nil {
			return err
		}
		raw, err := readAll(f)
		if err != nil {
			return err
		}
		var bad [][]byte
		events, bad = parseLogLines(string(raw))
		bad = l.filterAlreadyQuarantined(bad)
		if len(bad) == 0 {
			return nil
		}
		if err := l.quarantineMalformed(bad); err != nil {
			return fmt.Errorf("quarantine %d malformed line(s): %w", len(bad), err)
		}
		l.markQuarantined(bad)
		return nil
	})
	if err != nil {
		return events, err
	}
	return events, nil
}

func (l *JSONLLogger) withLock(operation func() error) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	lock, err := os.OpenFile(l.Path+".lock", os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("open event log lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock event log: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	return operation()
}

func (l *JSONLLogger) hit(stage string) error {
	if l.hooks != nil && l.hooks.fail != nil {
		return l.hooks.fail(stage)
	}
	return nil
}

func readAll(f *os.File) ([]byte, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind event log: %w", err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read event log: %w", err)
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return nil, fmt.Errorf("restore event log position: %w", err)
	}
	return data, nil
}

func (l *JSONLLogger) filterAlreadyQuarantined(bad [][]byte) [][]byte {
	if l.quarantined == nil {
		l.quarantined = make(map[string]struct{}, len(bad))
	}
	first := bad[:0]
	for _, line := range bad {
		if _, seen := l.quarantined[string(line)]; !seen {
			first = append(first, line)
		}
	}
	return first
}

func (l *JSONLLogger) markQuarantined(lines [][]byte) {
	if l.quarantined == nil {
		l.quarantined = make(map[string]struct{}, len(lines))
	}
	for _, line := range lines {
		l.quarantined[string(line)] = struct{}{}
	}
}

func parseLogLines(raw string) ([]Event, [][]byte) {
	var events []Event
	var bad [][]byte
	raw = strings.TrimRight(raw, "\n")
	if raw == "" {
		return nil, nil
	}
	for _, line := range strings.Split(raw, "\n") {
		if line == "" {
			continue
		}
		var event Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			bad = append(bad, []byte(line))
			continue
		}
		events = append(events, event)
	}
	return events, bad
}

func (l *JSONLLogger) quarantineMalformed(bad [][]byte) error {
	if err := l.hit("quarantine"); err != nil {
		return err
	}
	side, err := os.OpenFile(l.Path+".malformed", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open quarantine sidecar: %w", err)
	}
	for _, line := range bad {
		if _, err := side.Write(append(line, '\n')); err != nil {
			_ = side.Close()
			return fmt.Errorf("append to quarantine sidecar: %w", err)
		}
	}
	if err := side.Sync(); err != nil {
		_ = side.Close()
		return fmt.Errorf("sync quarantine sidecar: %w", err)
	}
	if err := side.Close(); err != nil {
		return fmt.Errorf("close quarantine sidecar: %w", err)
	}
	for _, line := range bad {
		log.Printf("events: skipping malformed event line (%d bytes)", len(line))
	}
	return nil
}

func (l *JSONLLogger) ensureOpen() (*os.File, error) {
	if l.file != nil {
		return l.file, nil
	}
	f, err := os.OpenFile(l.Path, os.O_APPEND|os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, fmt.Errorf("open event log: %w", err)
	}
	l.file = f
	return f, nil
}

var _ EventLog = (*JSONLLogger)(nil)
