package subagent

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
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
			lines := formatEventWithHierarchy(e, useANSI, active)
			if len(lines) == 0 {
				continue
			}
			for _, line := range lines {
				fmt.Fprintf(w, "%s%s %s\n", prefix, timeStr, line)
			}
		}
	}
}

func formatEventWithHierarchy(e Event, useANSI bool, active map[string]bool) []string {
	switch e.Type {
	case EventSubagentStart:
		active[e.SessionID] = true
		return []string{fmt.Sprintf(" └─ @%s subagent: %s", e.Agent, e.Title)}
	case EventSubagentFinish:
		delete(active, e.SessionID)
		return []string{fmt.Sprintf(" └─ @%s subagent: finished", e.Agent)}
	default:
		if _, ok := active[e.SessionID]; ok {
			return formatSubagentEvent(e, useANSI)
		}
		line := formatEvent(e, useANSI)
		if line == "" {
			return nil
		}
		return []string{line}
	}
}

func formatSubagentEvent(e Event, useANSI bool) []string {
	indent := "    └─ "
	switch e.Type {
	case EventText:
		return indentLines(indent, e.Content)
	case EventReasoning:
		prefix := indent + "[thinking] "
		if useANSI {
			prefix = fmt.Sprintf("%s%s%s", indent, ansiDim+ansiItalic, "[thinking] ")
			suffix := ansiReset
			return []string{prefix + firstLine(e.Content) + suffix}
		}
		return []string{prefix + firstLine(e.Content)}
	case EventTool:
		line := indent + "[" + e.Title + "]"
		if e.Content != "" {
			line += " " + firstLine(e.Content)
		}
		return []string{line}
	default:
		return []string{indent + e.Content}
	}
}

func indentLines(indent, content string) []string {
	lines := strings.Split(content, "\n")
	result := make([]string, len(lines))
	for i, l := range lines {
		result[i] = indent + l
	}
	return result
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
