package batch

import (
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
}

func (w *runLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	n, err := w.writer.Write(p)
	if n > 0 {
		w.lastEntry = lastLogEntry(p[:n])
	}
	return n, err
}

func (w *runLogWriter) WriteEntry(entry string, p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if entry == w.lastEntry {
		return len(p), nil
	}
	n, err := w.writer.Write(p)
	if err == nil && n == len(p) {
		w.lastEntry = entry
	}
	return n, err
}

func lastLogEntry(p []byte) string {
	text := strings.TrimRight(string(p), "\r\n")
	if text == "" {
		return ""
	}
	if index := strings.LastIndexByte(text, '\n'); index >= 0 {
		text = text[index+1:]
	}
	return strings.TrimSuffix(text, "\r")
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
