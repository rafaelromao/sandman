package batch

import (
	"bytes"
	"io"
	"strings"
	"sync"
)

// runLogWriter tracks the immediately preceding entry in one run's log while
// forwarding every non-duplicate write to the configured log destination.
type runLogWriter struct {
	mu        sync.Mutex
	writer    io.Writer
	lastEntry string
	pending   []byte
}

func (w *runLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	n, err := w.writer.Write(p)
	if n > 0 {
		w.record(p[:n])
	}
	return n, err
}

func (w *runLogWriter) WriteEntry(entry string, p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(w.pending) == 0 && entry == w.lastEntry {
		return len(p), nil
	}
	line := p
	prefixNewline := len(w.pending) > 0
	if prefixNewline {
		line = append([]byte("\n"), p...)
	}
	n, err := w.writer.Write(line)
	if err == nil && n == len(line) {
		w.pending = w.pending[:0]
		w.lastEntry = entry
	} else if n > 0 {
		w.record(line[:n])
	}
	written := n
	if prefixNewline && written > 0 {
		written--
	}
	if written > len(p) {
		written = len(p)
	}
	if err == nil && n != len(line) {
		err = io.ErrShortWrite
	}
	return written, err
}

func (w *runLogWriter) record(p []byte) {
	combined := append(append([]byte(nil), w.pending...), p...)
	lastNewline := bytes.LastIndexByte(combined, '\n')
	if lastNewline < 0 {
		w.pending = combined
		w.lastEntry = ""
		return
	}
	complete := combined[:lastNewline]
	if previousNewline := bytes.LastIndexByte(complete, '\n'); previousNewline >= 0 {
		complete = complete[previousNewline+1:]
	}
	w.lastEntry = strings.TrimSuffix(string(complete), "\r")
	w.pending = append(w.pending[:0], combined[lastNewline+1:]...)
	if len(w.pending) > 0 {
		w.lastEntry = ""
	}
}

func (s *runSession) runLogWriter() *runLogWriter {
	if s == nil || s.deps.errorLog == nil {
		return nil
	}
	if writer, ok := s.deps.errorLog.(*runLogWriter); ok {
		return writer
	}
	writer := &runLogWriter{writer: s.deps.errorLog}
	s.deps.errorLog = writer
	return writer
}
