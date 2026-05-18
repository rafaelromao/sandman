package subagent

import (
	"context"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"
	"unsafe"
)

const ansiDim = "\033[2m"
const ansiItalic = "\033[3m"
const ansiBold = "\033[1m"
const ansiReset = "\033[0m"

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	var termios syscall.Termios
	_, _, err := syscall.Syscall6(
		syscall.SYS_IOCTL, f.Fd(), syscall.TCGETS,
		uintptr(unsafe.Pointer(&termios)), 0, 0, 0,
	)
	return err == 0
}

func RenderEvents(ctx context.Context, issue int, events <-chan Event, w io.Writer) {
	useANSI := isTerminal(w)
	prefix := fmt.Sprintf("[issue-%d] ", issue)

	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-events:
			if !ok {
				return
			}
			ts := e.Timestamp
			if ts.IsZero() {
				ts = time.Now()
			}
			timeStr := ts.Format("15:04:05")
			line := formatEvent(e, useANSI)
			if line == "" {
				continue
			}
			fmt.Fprintf(w, "%s%s %s\n", prefix, timeStr, line)
		}
	}
}

func formatEvent(e Event, useANSI bool) string {
	switch e.Type {
	case EventText:
		return e.Content
	case EventReasoning:
		if useANSI {
			return ansiDim + ansiItalic + e.Content + ansiReset
		}
		return e.Content
	case EventTool:
		if useANSI {
			return fmt.Sprintf("\u2500 %s%s%s", ansiBold, e.Title, ansiReset)
		}
		return fmt.Sprintf("\u2500 %s", e.Title)
	case EventStepStart:
		return "..."
	case EventStepFinish:
		return "OK"
	case EventSessionDetected:
		return fmt.Sprintf("\u2550 Session %s started \u2550", e.SessionID)
	case EventError:
		return fmt.Sprintf("\u2717 %s", e.Content)
	default:
		return e.Content
	}
}
