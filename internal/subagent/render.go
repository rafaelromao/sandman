package subagent

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
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
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return false
	}
	return st.Mode&syscall.S_IFMT == syscall.S_IFCHR
}

func RenderEvents(ctx context.Context, issue int, events <-chan Event, w io.Writer) {
	useANSI := isTerminal(w)
	prefix := fmt.Sprintf("[issue-%d] ", issue)
	active := make(map[string]bool)
	var mu sync.Mutex

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
			line := formatEventWithHierarchy(e, useANSI, active, &mu)
			if line == "" {
				continue
			}
			fmt.Fprintf(w, "%s%s %s\n", prefix, timeStr, line)
		}
	}
}

func formatEventWithHierarchy(e Event, useANSI bool, active map[string]bool, mu sync.Locker) string {
	switch e.Type {
	case EventSubagentStart:
		mu.Lock()
		active[e.SessionID] = true
		mu.Unlock()
		return fmt.Sprintf(" └─ @%s subagent: %s", e.Agent, e.Title)
	case EventSubagentFinish:
		mu.Lock()
		delete(active, e.SessionID)
		mu.Unlock()
		return fmt.Sprintf(" └─ @%s subagent: finished", e.Agent)
	default:
		mu.Lock()
		_, isSub := active[e.SessionID]
		mu.Unlock()
		if isSub {
			return formatSubagentEvent(e, useANSI)
		}
		return formatEvent(e, useANSI)
	}
}

func formatSubagentEvent(e Event, useANSI bool) string {
	switch e.Type {
	case EventText:
		return fmt.Sprintf("    └─ %s", firstLine(e.Content))
	case EventReasoning:
		prefix := "    └─ [thinking] "
		if useANSI {
			return fmt.Sprintf("    └─ %s%s%s", ansiDim+ansiItalic, "[thinking] "+firstLine(e.Content), ansiReset)
		}
		return prefix + firstLine(e.Content)
	case EventTool:
		line := "    └─ [" + e.Title + "]"
		if e.Content != "" {
			line += " " + firstLine(e.Content)
		}
		return line
	default:
		return fmt.Sprintf("    └─ %s", firstLine(e.Content))
	}
}

func firstLine(s string) string {
	if idx := strings.Index(s, "\n"); idx >= 0 {
		return s[:idx]
	}
	return s
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
			return fmt.Sprintf("\u2500 %s%s%s %s", ansiBold, e.Title, ansiReset, e.Content)
		}
		return fmt.Sprintf("\u2500 %s %s", e.Title, e.Content)
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
