package batch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/paths"
)

const (
	opencodeSessionProtocol = "opencode-session/v1"
	opencodeProvider        = "opencode"
)

type opencodeSessionIdentity struct {
	Protocol  string `json:"protocol"`
	Provider  string `json:"provider"`
	SessionID string `json:"session_id"`
}

func validOpenCodeSession(id opencodeSessionIdentity) error {
	if id.Protocol != opencodeSessionProtocol {
		return fmt.Errorf("unsupported session protocol %q", id.Protocol)
	}
	if id.Provider != opencodeProvider {
		return fmt.Errorf("unsupported session provider %q", id.Provider)
	}
	if strings.TrimSpace(id.SessionID) == "" {
		return errors.New("session id is empty")
	}
	return nil
}

func readOpenCodeSession(path string) (opencodeSessionIdentity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return opencodeSessionIdentity{}, err
	}
	var identity opencodeSessionIdentity
	if err := json.Unmarshal(data, &identity); err != nil {
		return opencodeSessionIdentity{}, fmt.Errorf("parse session metadata: %w", err)
	}
	if err := validOpenCodeSession(identity); err != nil {
		return opencodeSessionIdentity{}, err
	}
	return identity, nil
}

func writeOpenCodeSession(path, sessionID string) error {
	identity := opencodeSessionIdentity{
		Protocol:  opencodeSessionProtocol,
		Provider:  opencodeProvider,
		SessionID: strings.TrimSpace(sessionID),
	}
	if err := validOpenCodeSession(identity); err != nil {
		return err
	}
	return atomicfs.WriteAtomicJSON(path, identity, 0644)
}

// priorOpenCodeSession selects canonical metadata first. Legacy lookup is
// deliberately limited to a missing canonical file so a broken canonical
// record cannot be silently replaced by stale evidence.
func priorOpenCodeSession(layout paths.Layout, batchID, runID string) (opencodeSessionIdentity, bool, error) {
	canonical := layout.RunSessionPath(batchID, runID)
	identity, err := readOpenCodeSession(canonical)
	if err == nil {
		return identity, true, nil
	}
	if !os.IsNotExist(err) {
		return opencodeSessionIdentity{}, false, err
	}

	legacy := layout.LegacyRunSessionPath(runID)
	identity, err = readOpenCodeSession(legacy)
	if err == nil {
		return identity, true, nil
	}
	if os.IsNotExist(err) {
		return opencodeSessionIdentity{}, false, nil
	}
	return opencodeSessionIdentity{}, false, err
}

type opencodeOutput struct {
	dst     io.Writer
	warning io.Writer
	capture *opencodeSessionCapture
	stderr  bool
	buf     bytes.Buffer
}

type opencodeSessionCapture struct {
	mu              sync.Mutex
	sessionID       string
	sessionNotFound bool
}

func newOpenCodeOutput(dst, warning io.Writer, stderr bool) *opencodeOutput {
	return newSharedOpenCodeOutput(dst, warning, stderr, &opencodeSessionCapture{})
}

func newSharedOpenCodeOutput(dst, warning io.Writer, stderr bool, capture *opencodeSessionCapture) *opencodeOutput {
	return &opencodeOutput{dst: dst, warning: warning, stderr: stderr, capture: capture}
}

func (w *opencodeOutput) Write(p []byte) (int, error) {
	n, err := w.buf.Write(p)
	if err != nil {
		return n, err
	}
	for {
		line, rest, found := bytes.Cut(w.buf.Bytes(), []byte("\n"))
		if !found {
			break
		}
		line = append([]byte(nil), line...)
		w.buf.Reset()
		_, _ = w.buf.Write(rest)
		if err := w.writeLine(line, true); err != nil {
			return n, err
		}
	}
	return n, nil
}

func (w *opencodeOutput) Flush() error {
	if w.buf.Len() == 0 {
		return nil
	}
	line := append([]byte(nil), w.buf.Bytes()...)
	w.buf.Reset()
	return w.writeLine(line, false)
}

func (w *opencodeOutput) writeLine(line []byte, newline bool) error {
	text := string(line)
	stderrLine := strings.TrimSuffix(text, "\r")
	if w.stderr && (stderrLine == "Session not found" || stderrLine == "Error: Session not found") {
		w.capture.mu.Lock()
		w.capture.sessionNotFound = true
		w.capture.mu.Unlock()
	}

	var value any
	if err := json.Unmarshal(line, &value); err != nil {
		if strings.TrimSpace(text) != "" {
			w.warn(fmt.Sprintf("malformed OpenCode event: %v", err))
		}
		return w.writeRaw(line, newline)
	}
	event, ok := value.(map[string]any)
	if !ok {
		return w.writeText(formatUnhandledOpenCodeJSON(value))
	}
	if id, ok := event["sessionID"].(string); ok && strings.TrimSpace(id) != "" {
		id = strings.TrimSpace(id)
		w.capture.mu.Lock()
		previous := w.capture.sessionID
		if previous == "" {
			w.capture.sessionID = id
		}
		w.capture.mu.Unlock()
		if previous != "" && previous != id {
			w.warn(fmt.Sprintf("conflicting OpenCode session IDs %q and %q", previous, id))
		}
	}

	eventType, _ := event["type"].(string)
	switch eventType {
	case "step_start", "step_finish":
		// These delimit OpenCode's internal tool loop and had no counterpart in
		// its previous human-oriented output.
		return nil
	case "error":
		message := eventMessage(event)
		normalizedMessage := normalizeSessionMessage(message)
		if normalizedMessage == "Session not found" {
			w.capture.mu.Lock()
			w.capture.sessionNotFound = true
			w.capture.mu.Unlock()
		}
		if normalizedMessage != "" {
			return w.writeText("Error: " + normalizedMessage)
		}
	case "text":
		if text := eventText(event); text != "" {
			return w.writeText(text)
		}
	case "tool", "tool_use", "tool_result":
		if tool := eventTool(event); tool != "" {
			return w.writeText(formatToolEvent(event, tool))
		}
	}
	return w.writeText(formatUnhandledOpenCodeJSON(value))
}

// formatUnhandledOpenCodeJSON keeps valid protocol additions readable while
// preserving their complete payload in the canonical run log.
func formatUnhandledOpenCodeJSON(value any) string {
	formatted, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(formatted)
}

func formatToolEvent(event map[string]any, tool string) string {
	input := toolInput(event)
	state := toolState(event)
	status, _ := state["status"].(string)
	var detail string
	switch tool {
	case "read":
		if v, _ := input["filePath"].(string); v != "" {
			detail = v
			pagination := make([]string, 0, 2)
			if offset, ok := input["offset"]; ok && offset != nil {
				pagination = append(pagination, "offset "+formatToolInputValue(offset))
			}
			if limit, ok := input["limit"]; ok {
				pagination = append(pagination, "limit "+formatToolInputValue(limit))
			}
			if len(pagination) > 0 {
				detail += " (" + strings.Join(pagination, ", ") + ")"
			}
		}
	case "grep":
		pattern, _ := input["pattern"].(string)
		path, _ := input["path"].(string)
		if path == "" {
			path, _ = input["include"].(string)
		}
		if pattern != "" && path != "" {
			detail = fmt.Sprintf("%q in %s", truncateString(pattern, 60), path)
		} else if pattern != "" {
			detail = fmt.Sprintf("%q", truncateString(pattern, 60))
		} else if path != "" {
			detail = path
		}
	case "glob":
		pattern, _ := input["pattern"].(string)
		path, _ := input["path"].(string)
		if pattern != "" && path != "" {
			detail = fmt.Sprintf("%q in %s", truncateString(pattern, 60), path)
		} else if pattern != "" {
			detail = fmt.Sprintf("%q", truncateString(pattern, 60))
		} else if path != "" {
			detail = path
		}
	case "bash":
		if cmd, _ := input["command"].(string); cmd != "" {
			detail = truncateString(strings.TrimSpace(cmd), 120)
			if workdir, _ := input["workdir"].(string); workdir != "" && workdir != "." && workdir != "/tmp" {
				detail += " @ " + workdir
			}
		} else if cmd, _ := input["cmd"].(string); cmd != "" {
			detail = truncateString(strings.TrimSpace(cmd), 120)
		}
	case "skill":
		if name, _ := input["name"].(string); name != "" {
			detail = name
		} else if name, _ := input["skill"].(string); name != "" {
			detail = name
		}
	case "edit", "write", "apply_patch":
		if fp, _ := input["filePath"].(string); fp != "" {
			detail = fp
		} else if fp, _ := input["path"].(string); fp != "" {
			detail = fp
		} else if patch, _ := input["patchText"].(string); patch != "" {
			detail = patchTargets(patch)
		}
	case "todowrite":
		if todos, ok := input["todos"].([]any); ok {
			detail = fmt.Sprintf("%d todos", len(todos))
			detail += todoStatusSummary(todos)
		} else if todos, ok := input["todos"].([]map[string]any); ok {
			detail = fmt.Sprintf("%d todos", len(todos))
			items := make([]any, len(todos))
			for i := range todos {
				items[i] = todos[i]
			}
			detail += todoStatusSummary(items)
		}
	case "question":
		if q, _ := input["question"].(string); q != "" {
			detail = truncateString(q, 80)
		} else if header, _ := input["header"].(string); header != "" {
			detail = truncateString(header, 80)
		}
	case "pty_spawn":
		detail = formatPTYSpawn(input, state)
	case "pty_read", "pty_write", "pty_kill":
		detail = formatPTYEvent(tool, input, state)
	case "task":
		detail, _ = input["description"].(string)
	default:
		if input != nil && len(input) > 0 {
			if b, err := json.Marshal(input); err == nil {
				detail = truncateString(string(b), 120)
			}
		}
	}
	base := formatToolLabel(tool, detail)
	if status == "error" {
		if msg, _ := state["error"].(string); msg != "" {
			base += fmt.Sprintf(" (error: %s)", truncateString(strings.TrimSpace(msg), 80))
		} else if msg := toolErrorMessage(state); msg != "" {
			base += fmt.Sprintf(" (error: %s)", truncateString(msg, 80))
		} else {
			base += " (error)"
		}
	}
	return base
}

func formatToolLabel(tool, detail string) string {
	if tool == "bash" {
		if detail == "" {
			return "$"
		}
		return "$ " + detail
	}
	if tool == "skill" && detail != "" {
		return fmt.Sprintf("→ Skill %q", detail)
	}
	ptyLabels := map[string]string{
		"pty_spawn": "→ PTY Spawn",
		"pty_read":  "→ PTY Read",
		"pty_write": "→ PTY Write",
		"pty_kill":  "→ PTY Kill",
		"pty_list":  "→ PTY List",
	}
	if label := ptyLabels[tool]; label != "" {
		if detail == "" {
			return label
		}
		return label + " " + detail
	}
	labels := map[string]string{
		"read":        "→ Read",
		"grep":        "✱ Grep",
		"glob":        "✱ Glob",
		"edit":        "→ Edit",
		"write":       "→ Write",
		"apply_patch": "→ Edit",
		"todowrite":   "→ Task",
		"question":    "→ Task",
		"task":        "→ Task",
	}
	label := labels[tool]
	if label == "" {
		label = tool
	}
	if detail == "" {
		return label
	}
	return label + " " + detail
}

func formatPTYSpawn(input, state map[string]any) string {
	parts := make([]string, 0, 4)
	if command, _ := input["command"].(string); command != "" {
		parts = append(parts, command)
	}
	if args, ok := input["args"]; ok {
		if rendered := formatToolInputValue(args); rendered != "" && rendered != "null" && rendered != "[]" {
			parts = append(parts, rendered)
		}
	}
	if description, _ := input["description"].(string); description != "" {
		parts = append(parts, "("+truncateString(description, 80)+")")
	}
	if session := ptySessionID(input, state); session != "" {
		parts = append(parts, "["+session+"]")
	}
	return strings.Join(parts, " ")
}

func formatPTYEvent(tool string, input, state map[string]any) string {
	parts := make([]string, 0, 3)
	if session := ptySessionID(input, state); session != "" {
		parts = append(parts, session)
	}
	if tool == "pty_write" {
		if data, _ := input["data"].(string); data != "" {
			parts = append(parts, fmt.Sprintf("%q", truncateString(data, 120)))
		}
	}
	if tool == "pty_read" {
		if output := ptyOutput(input, state); output != "" {
			parts = append(parts, "output "+fmt.Sprintf("%q", truncateString(output, 120)))
		}
	}
	return strings.Join(parts, " ")
}

func ptySessionID(input, state map[string]any) string {
	for _, values := range []map[string]any{input, state} {
		for _, key := range []string{"id", "session", "sessionID"} {
			if value, _ := values[key].(string); value != "" {
				return value
			}
		}
		if output, _ := values["output"].(map[string]any); output != nil {
			for _, key := range []string{"id", "session", "sessionID"} {
				if value, _ := output[key].(string); value != "" {
					return value
				}
			}
		}
	}
	return ""
}

func ptyOutput(input, state map[string]any) string {
	for _, values := range []map[string]any{state, input} {
		for _, key := range []string{"output", "data", "result"} {
			if value, _ := values[key].(string); value != "" {
				return value
			}
		}
	}
	return ""
}

func patchTargets(patch string) string {
	const marker = "*** "
	targets := make([]string, 0, 1)
	for _, line := range strings.Split(patch, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, marker) {
			continue
		}
		line = strings.TrimPrefix(line, marker)
		for _, action := range []string{"Update File: ", "Add File: ", "Delete File: "} {
			if target := strings.TrimPrefix(line, action); target != line {
				if target != "" && !containsString(targets, target) {
					targets = append(targets, target)
				}
				break
			}
		}
	}
	return strings.Join(targets, ", ")
}

func todoStatusSummary(todos []any) string {
	counts := make(map[string]int)
	for _, todo := range todos {
		item, ok := todo.(map[string]any)
		if !ok {
			continue
		}
		status, _ := item["status"].(string)
		if status != "" {
			counts[status]++
		}
	}
	if len(counts) == 0 {
		return ""
	}
	parts := make([]string, 0, len(counts))
	for _, status := range []string{"pending", "in_progress", "completed", "cancelled"} {
		if count := counts[status]; count > 0 {
			parts = append(parts, fmt.Sprintf("%s: %d", status, count))
			delete(counts, status)
		}
	}
	for status, count := range counts {
		parts = append(parts, fmt.Sprintf("%s: %d", status, count))
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func toolInput(event map[string]any) map[string]any {
	if part, ok := event["part"].(map[string]any); ok {
		if state, ok := part["state"].(map[string]any); ok {
			if input, ok := state["input"].(map[string]any); ok {
				return input
			}
		}
		if input, ok := part["input"].(map[string]any); ok {
			return input
		}
	}
	if input, ok := event["input"].(map[string]any); ok {
		return input
	}
	return nil
}

func toolState(event map[string]any) map[string]any {
	if part, ok := event["part"].(map[string]any); ok {
		if state, ok := part["state"].(map[string]any); ok {
			return state
		}
	}
	return nil
}

func toolErrorMessage(state map[string]any) string {
	if state == nil {
		return ""
	}
	if msg, _ := state["error"].(string); msg != "" {
		return msg
	}
	if errObj, ok := state["error"].(map[string]any); ok {
		if msg, _ := errObj["message"].(string); msg != "" {
			return msg
		}
	}
	return ""
}

func formatToolInputValue(v any) string {
	switch x := v.(type) {
	case string:
		return truncateString(x, 40)
	case float64:
		return fmt.Sprintf("%v", x)
	default:
		if b, err := json.Marshal(v); err == nil {
			return truncateString(string(b), 40)
		}
		return fmt.Sprintf("%v", v)
	}
}

func truncateString(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 3 {
		return string(runes[:max])
	}
	return string(runes[:max-3]) + "..."
}

func (w *opencodeOutput) writeText(text string) error {
	_, err := io.WriteString(w.dst, text+"\n")
	return err
}

func (w *opencodeOutput) writeRaw(line []byte, newline bool) error {
	if _, err := w.dst.Write(line); err != nil {
		return err
	}
	if newline {
		_, err := io.WriteString(w.dst, "\n")
		return err
	}
	return nil
}

func (w *opencodeOutput) warn(message string) {
	if w.warning != nil {
		fmt.Fprintf(w.warning, "warning: %s\n", message)
	}
}

func eventText(event map[string]any) string {
	if text, _ := event["text"].(string); text != "" {
		return text
	}
	return nestedString(event, "part", "text")
}

func eventTool(event map[string]any) string {
	for _, key := range []string{"tool", "name"} {
		if value, _ := event[key].(string); value != "" {
			return value
		}
	}
	for _, key := range []string{"tool", "name"} {
		if value := nestedString(event, "part", key); value != "" {
			return value
		}
	}
	return ""
}

func eventMessage(event map[string]any) string {
	if value := messageValue(event); value != "" {
		return value
	}
	return ""
}

func messageValue(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	if object, ok := value.(map[string]any); ok {
		if message, ok := object["message"].(string); ok && message != "" {
			return message
		}
		for _, key := range []string{"error", "data"} {
			if message := messageValue(object[key]); message != "" {
				return message
			}
		}
	}
	return ""
}

func nestedString(event map[string]any, parent, child string) string {
	nested, _ := event[parent].(map[string]any)
	value, _ := nested[child].(string)
	return value
}

func normalizeSessionMessage(message string) string {
	return strings.Join(strings.Fields(message), " ")
}

func (w *opencodeOutput) SessionID() string {
	w.capture.mu.Lock()
	defer w.capture.mu.Unlock()
	return w.capture.sessionID
}

func (w *opencodeOutput) SessionNotFound() bool {
	w.capture.mu.Lock()
	defer w.capture.mu.Unlock()
	return w.capture.sessionNotFound
}

func flushOpenCodeOutputs(outputs ...*opencodeOutput) {
	for _, output := range outputs {
		if output != nil {
			_ = output.Flush()
		}
	}
}
