package batch

import (
	"bytes"
	"fmt"
	"io"
	"time"
)

// LinePrefixWriter prefixes every line written to it with the issue identifier and current timestamp.
type LinePrefixWriter struct {
	w     io.Writer
	label string
	buf   []byte
}

// NewLinePrefixWriter creates a writer that prefixes each line with a label and HH:MM:SS.
func NewLinePrefixWriter(label string, w io.Writer) *LinePrefixWriter {
	return &LinePrefixWriter{w: w, label: label}
}

func (lp *LinePrefixWriter) prefix() string {
	return fmt.Sprintf("[%s] %s ", lp.label, time.Now().Format("15:04:05"))
}

// Write buffers input until a newline is found, then writes the prefixed line.
func (lp *LinePrefixWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		idx := bytes.IndexByte(p, '\n')
		if idx == -1 {
			lp.buf = append(lp.buf, p...)
			break
		}
		lp.buf = append(lp.buf, p[:idx]...)
		line := string(lp.buf)
		lp.buf = lp.buf[:0]
		if _, err := fmt.Fprintf(lp.w, "%s%s\n", lp.prefix(), line); err != nil {
			return total, err
		}
		p = p[idx+1:]
	}
	return total, nil
}

// Flush writes any remaining buffered content with the prefix.
func (lp *LinePrefixWriter) Flush() error {
	if len(lp.buf) > 0 {
		if _, err := fmt.Fprintf(lp.w, "%s%s", lp.prefix(), string(lp.buf)); err != nil {
			return err
		}
		lp.buf = lp.buf[:0]
	}
	return nil
}
