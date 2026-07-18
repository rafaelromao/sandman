package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
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
		if err := f.Sync(); err != nil {
			return fmt.Errorf("sync event log: %w", err)
		}
		return nil
	})
}

// Read returns all valid events from the log, quarantining malformed lines
// without rewriting the main file.
func (l *JSONLLogger) Read() ([]Event, error) {
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

// RemoveEventsByIssue removes events matching either issue representation.
// Rewrites retain the main log inode so already-open O_APPEND descriptors do
// not become ghost writers.
func (l *JSONLLogger) RemoveEventsByIssue(issueNumber int) error {
	return l.withLock(func() error {
		f, err := l.ensureOpen()
		if err != nil {
			return err
		}
		raw, err := readAll(f)
		if err != nil {
			return err
		}
		all, bad := parseLogLines(string(raw))
		kept := make([]Event, 0, len(all))
		for _, event := range all {
			if event.Issue == issueNumber || (event.IssueRef != nil && *event.IssueRef == issueNumber) {
				continue
			}
			kept = append(kept, event)
		}
		if len(bad) == 0 && len(kept) == len(all) {
			return nil
		}

		var rewritten []byte
		for _, event := range kept {
			data, err := json.Marshal(event)
			if err != nil {
				return fmt.Errorf("marshal event: %w", err)
			}
			rewritten = append(rewritten, data...)
			rewritten = append(rewritten, '\n')
		}

		if err := l.beginTransaction(raw); err != nil {
			return err
		}
		failed := func(primary error) error {
			return l.abortTransaction(raw, primary)
		}

		toQuarantine := l.filterAlreadyQuarantined(bad)
		if len(toQuarantine) > 0 {
			if err := l.quarantineMalformed(toQuarantine); err != nil {
				return failed(fmt.Errorf("quarantine %d malformed line(s): %w", len(toQuarantine), err))
			}
			l.markQuarantined(toQuarantine)
		}
		if err := l.hit("truncate"); err != nil {
			return failed(err)
		}
		if err := f.Truncate(0); err != nil {
			return failed(fmt.Errorf("truncate event log: %w", err))
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return failed(fmt.Errorf("rewind event log: %w", err))
		}
		if err := l.hit("write"); err != nil {
			return failed(err)
		}
		if _, err := f.Write(rewritten); err != nil {
			return failed(fmt.Errorf("write event log: %w", err))
		}
		if err := l.hit("sync"); err != nil {
			return failed(err)
		}
		if err := f.Sync(); err != nil {
			return failed(fmt.Errorf("sync event log: %w", err))
		}
		if err := l.finishTransaction(); err != nil {
			return failed(err)
		}
		return nil
	})
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

	if err := l.recoverTransaction(); err != nil {
		return err
	}
	return operation()
}

func (l *JSONLLogger) beginTransaction(raw []byte) error {
	if err := l.writeSynced(l.snapshotPath(), raw); err != nil {
		return fmt.Errorf("write recovery snapshot: %w", err)
	}
	if err := l.hit("snapshot"); err != nil {
		return l.abortTransaction(raw, err)
	}
	if err := l.writeSynced(l.markerPath(), []byte("pending\n")); err != nil {
		return l.abortTransaction(raw, fmt.Errorf("write recovery marker: %w", err))
	}
	if err := l.hit("marker"); err != nil {
		return l.abortTransaction(raw, err)
	}
	return nil
}

func (l *JSONLLogger) finishTransaction() error {
	// Remove the marker first. If snapshot cleanup then fails, restore uses the
	// in-memory raw bytes and leaves the snapshot for operator inspection.
	if err := l.hit("cleanup-marker"); err != nil {
		return err
	}
	if err := os.Remove(l.markerPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove recovery marker: %w", err)
	}
	if err := l.hit("cleanup-snapshot"); err != nil {
		return err
	}
	if err := os.Remove(l.snapshotPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove recovery snapshot: %w", err)
	}
	return nil
}

func (l *JSONLLogger) recoverTransaction() error {
	if _, err := os.Stat(l.markerPath()); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat recovery marker: %w", err)
	}
	raw, err := os.ReadFile(l.snapshotPath())
	if err != nil {
		return fmt.Errorf("read recovery snapshot: %w", err)
	}
	if err := l.restore(raw); err != nil {
		return fmt.Errorf("recover interrupted event log transaction: %w", err)
	}
	if err := l.clearTransaction(); err != nil {
		return fmt.Errorf("cleanup recovered event log transaction: %w", err)
	}
	return nil
}

func (l *JSONLLogger) abortTransaction(raw []byte, primary error) error {
	if recoveryErr := l.restore(raw); recoveryErr != nil {
		return errors.Join(primary, fmt.Errorf("restore event log: %w", recoveryErr))
	}
	if err := l.clearTransaction(); err != nil {
		return errors.Join(primary, fmt.Errorf("cleanup recovered event log transaction: %w", err))
	}
	return primary
}

func (l *JSONLLogger) clearTransaction() error {
	if err := os.Remove(l.markerPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove recovery marker: %w", err)
	}
	if err := syncDir(filepath.Dir(l.Path)); err != nil {
		return fmt.Errorf("sync recovery marker removal: %w", err)
	}
	if err := os.Remove(l.snapshotPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove recovery snapshot: %w", err)
	}
	if err := syncDir(filepath.Dir(l.Path)); err != nil {
		return fmt.Errorf("sync recovery snapshot removal: %w", err)
	}
	return nil
}

func (l *JSONLLogger) restore(raw []byte) error {
	f, err := l.ensureOpen()
	if err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		return err
	}
	return f.Sync()
}

func (l *JSONLLogger) writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (l *JSONLLogger) snapshotPath() string { return l.Path + ".recovery" }
func (l *JSONLLogger) markerPath() string   { return l.Path + ".txn" }

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
