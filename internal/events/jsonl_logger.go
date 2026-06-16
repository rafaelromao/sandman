package events

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
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

	events, bad := parseLogLines(string(buf))
	if len(bad) > 0 {
		if err := l.quarantineAndRewrite(buf, bad); err != nil {
			log.Printf("events: failed to quarantine %d malformed line(s): %v", len(bad), err)
		}
	}
	return events, nil
}

// parseLogLines splits the raw JSONL buffer into valid events and the
// raw bytes of any malformed lines. A malformed line is a record that
// the JSON decoder could not consume end-to-end; the rest of the log
// stays usable regardless.
//
// Only trailing newlines and a final empty record are trimmed.
// Per-line content is preserved verbatim so a torn line that ends in
// a run of spaces (a common shape for a payload fragment) is still
// identified as a single bad line and quarantined in full.
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
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			bad = append(bad, []byte(line))
			continue
		}
		events = append(events, e)
	}
	return events, bad
}

// quarantineAndRewrite moves the malformed lines into a sidecar file
// and rewrites the main log without them. The sidecar preserves the
// raw bytes for forensic inspection while ensuring the next Read does
// not re-encounter the same corruption.
//
// Quarantine path: <Path>.malformed. Each call appends to the sidecar
// so multiple quarantines accumulate instead of overwriting prior
// forensic data.
//
// The rewrite uses an atomic temp-file + rename so a crash mid-rewrite
// cannot leave a half-written events.jsonl behind. The temp file lives
// in the same directory as the log so the rename(2) stays on a single
// filesystem.
func (l *JSONLLogger) quarantineAndRewrite(raw []byte, bad [][]byte) error {
	for _, line := range bad {
		log.Printf("events: skipping malformed event line (%d bytes)", len(line))
	}
	if err := l.quarantineMalformed(bad); err != nil {
		return err
	}

	kept := stripLines(raw, bad)
	tmp, err := os.CreateTemp(filepath.Dir(l.Path), filepath.Base(l.Path)+".rewrite-*.tmp")
	if err != nil {
		return fmt.Errorf("create rewrite temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if len(kept) > 0 {
		if _, err := tmp.Write(kept); err != nil {
			_ = tmp.Close()
			cleanup()
			return fmt.Errorf("write rewrite temp file: %w", err)
		}
		if err := tmp.Sync(); err != nil {
			_ = tmp.Close()
			cleanup()
			return fmt.Errorf("sync rewrite temp file: %w", err)
		}
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close rewrite temp file: %w", err)
	}
	if err := os.Rename(tmpName, l.Path); err != nil {
		cleanup()
		return fmt.Errorf("rename rewrite temp file: %w", err)
	}
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
	return nil
}

// quarantineMalformed appends bad lines to the .malformed sidecar.
// Used by RemoveEventsByIssue, which rewrites the main log via
// Truncate and only needs the sidecar write (no file replacement).
func (l *JSONLLogger) quarantineMalformed(bad [][]byte) error {
	sidecar := l.Path + ".malformed"
	side, err := os.OpenFile(sidecar, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		return fmt.Errorf("open quarantine sidecar: %w", err)
	}
	for _, line := range bad {
		if _, err := side.Write(append(line, '\n')); err != nil {
			_ = side.Close()
			return fmt.Errorf("append to quarantine sidecar: %w", err)
		}
	}
	if err := side.Close(); err != nil {
		return fmt.Errorf("close quarantine sidecar: %w", err)
	}
	return nil
}

// stripLines returns raw with every line in bad removed. Lines are
// matched by full-content equality; malformed lines from the same
// log are guaranteed unique in practice (each torn line is a one-off
// collision of concurrent writes) but if duplicates exist we drop all
// occurrences so a future Read cannot regress.
//
// Only trailing newlines are stripped from the buffer; per-line
// trailing whitespace is part of the line identity and must be
// preserved so a torn line that ends in spaces is dropped in full.
func stripLines(raw []byte, bad [][]byte) []byte {
	if len(bad) == 0 {
		return raw
	}
	drop := make(map[string]struct{}, len(bad))
	for _, line := range bad {
		drop[string(line)] = struct{}{}
	}
	trimmed := strings.TrimRight(string(raw), "\n")
	if trimmed == "" {
		return nil
	}
	var out []byte
	for _, line := range strings.Split(trimmed, "\n") {
		if _, ok := drop[line]; ok {
			continue
		}
		if len(out) > 0 {
			out = append(out, '\n')
		}
		out = append(out, line...)
	}
	if len(out) == 0 {
		return nil
	}
	out = append(out, '\n')
	return out
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
	var bad [][]byte
	if size > 0 {
		buf := make([]byte, size)
		if _, err := io.ReadFull(f, buf); err != nil {
			return fmt.Errorf("read event log: %w", err)
		}
		raw := strings.TrimRight(string(buf), "\n")
		if raw != "" {
			for _, line := range strings.Split(raw, "\n") {
				if line == "" {
					continue
				}
				var e Event
				if err := json.Unmarshal([]byte(line), &e); err != nil {
					// Mirror Read's tolerance so a torn line does not
					// block RemoveEventsByIssue from rewriting the log
					// around it. The bad line is quarantined below.
					bad = append(bad, []byte(line))
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
	}

	if len(bad) > 0 {
		if err := l.quarantineMalformed(bad); err != nil {
			log.Printf("events: failed to quarantine %d malformed line(s) during remove: %v", len(bad), err)
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
