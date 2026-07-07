package events

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/rafaelromao/sandman/internal/atomicfs"
)

// JSONLLogger writes events to a JSONL file.
//
// Log, Read, and RemoveEventsByIssue are safe to call concurrently with
// each other. The mutex serialises in-process callers; the underlying
// file is opened with O_APPEND so the kernel guarantees that
// concurrent writes from different processes (e.g. multiple sandman
// daemons writing to the same repo-scoped log) never interleave
// bytes from a single Log call.
//
// The sidecar events.jsonl.malformed accumulates malformed lines from
// pre-O_APPEND torn writes. It is never trimmed by the logger; the
// operator is responsible for rolling it over.
type JSONLLogger struct {
	Path string

	mu   sync.Mutex
	file *os.File

	// quarantined tracks the content of bad lines that have already
	// been written to the .malformed sidecar. Read skips re-quarantine
	// for lines it has already seen, preventing the sidecar from
	// growing without bound when a static corrupted log is polled
	// repeatedly (e.g. by the portal every few seconds).
	quarantined map[string]struct{}
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

	stat, err := os.Stat(l.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return []Event{}, nil
		}
		return nil, fmt.Errorf("stat event log: %w", err)
	}

	f, err := l.ensureOpen()
	if err != nil {
		return nil, err
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind event log: %w", err)
	}

	events, bad, err := l.classifyFile(stat.Size())
	if err != nil {
		return nil, err
	}

	if len(bad) > 0 {
		// Filter out bad lines we've already quarantined so the
		// sidecar does not grow without bound when a static
		// corrupted log is polled repeatedly (e.g. by the portal).
		bad = l.filterAlreadyQuarantined(bad)
		if len(bad) > 0 {
			// Quarantine only — never replace the main log from
			// Read.  Renaming the main log would unlink the inode
			// that other sandman daemons opened via O_APPEND,
			// leaving their next Log() writing to a file no reader
			// can see by name (a "ghost" tail).  The rewrite is the
			// responsibility of RemoveEventsByIssue, which
			// truncates the existing in-process file descriptor
			// instead of replacing it.
			if err := l.quarantineMalformed(bad); err != nil {
				return events, fmt.Errorf("quarantine %d malformed line(s): %w", len(bad), err)
			}
		}
	}
	return events, nil
}

// classifyFile reads the full event log at the given size, returning
// parsed events and any malformed lines.  The caller must hold l.mu
// and the file position must be at offset 0.
func (l *JSONLLogger) classifyFile(size int64) ([]Event, [][]byte, error) {
	if size == 0 {
		return nil, nil, nil
	}
	f := l.file
	buf := make([]byte, size)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, nil, fmt.Errorf("read event log: %w", err)
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return nil, nil, fmt.Errorf("restore event log position: %w", err)
	}
	events, bad := parseLogLines(string(buf))
	return events, bad, nil
}

// filterAlreadyQuarantined drops bad lines whose content has already
// been written to the .malformed sidecar in a previous call.
func (l *JSONLLogger) filterAlreadyQuarantined(bad [][]byte) [][]byte {
	if l.quarantined == nil {
		l.quarantined = make(map[string]struct{}, len(bad))
	}
	first := bad[:0]
	for _, line := range bad {
		key := string(line)
		if _, seen := l.quarantined[key]; seen {
			continue
		}
		l.quarantined[key] = struct{}{}
		first = append(first, line)
	}
	if len(first) == 0 {
		return nil
	}
	return first
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

// quarantineMalformed appends bad lines to the .malformed sidecar and
// logs one "skipping" message per bad line.  Called by Read
// (quarantine-only, no main-log rewrite) and by RemoveEventsByIssue,
// which truncates the existing in-process file descriptor in addition
// to the sidecar write.
//
// Sidecar path: <Path>.malformed. Each call appends to the sidecar
// so multiple quarantines accumulate instead of overwriting prior
// forensic data. The sidecar is never trimmed by the logger; the
// operator is responsible for rolling it over if it grows without
// bound.
func (l *JSONLLogger) quarantineMalformed(bad [][]byte) error {
	sidecar := l.Path + ".malformed"
	side, err := atomicfs.OpenAppend(sidecar, 0644)
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
		allEvents, malformed := parseLogLines(string(buf))
		bad = malformed
		for _, e := range allEvents {
			if e.Issue == issueNumber {
				continue
			}
			if e.IssueRef != nil && *e.IssueRef == issueNumber {
				continue
			}
			kept = append(kept, e)
		}
	}

	if len(bad) > 0 {
		// Apply the same dedup as Read so that a prior Read on the
		// same dirty file does not produce duplicate sidecar entries
		// before the truncate-rewrite cleans the main log below.
		bad = l.filterAlreadyQuarantined(bad)
		if len(bad) > 0 {
			if err := l.quarantineMalformed(bad); err != nil {
				log.Printf("events: failed to quarantine %d malformed line(s) during remove: %v", len(bad), err)
			}
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
	//
	// O_RDWR is required alongside O_APPEND because Read and
	// RemoveEventsByIssue seek back to offset 0 and read the full
	// file through the same descriptor (no second open). atomicfs
	// does not expose an RDWR variant, so this site keeps os.OpenFile
	// while the sidecar uses atomicfs.OpenAppend (WRONLY).
	f, err := os.OpenFile(l.Path, os.O_APPEND|os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, fmt.Errorf("open event log: %w", err)
	}
	l.file = f
	return f, nil
}

// Ensure JSONLLogger implements EventLog.
var _ EventLog = (*JSONLLogger)(nil)
