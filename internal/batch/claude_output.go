package batch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"
)

// claudeOutput renders Claude Code's stream-json records into the readable
// lines written to the terminal and run.log, in the same style as the
// OpenCode parser: agent text as-is, tool calls as one-line labels, tool
// errors, permission denials, and a closing result summary. Progress and
// bookkeeping records (thinking-token estimates, command lists, partial
// stream events, long-running tool heartbeats) are dropped. Lines that are not JSON objects, such as
// Claude Code's own stderr warnings, pass through unchanged.
//
// Rendering removes the raw final `result` record from the log, so the
// usage-limit rule is applied here, to the raw record, before rendering.
type claudeOutput struct {
	dst           io.Writer
	progress      func()
	modelProgress func()
	usageLimit    func()
	stopForMode   func()
	stdout        bool
	state         *claudeOutputState
	buf           bytes.Buffer
}

type claudeOutputState struct {
	mu                sync.Mutex
	sessionID         string
	sessionNotFound   bool
	usageLimitReached bool
	permissionDenials []string
	readOnlyMode      *PermissionModeError
	// readOnlyModes are the permission modes that stop a session at its init
	// record. Plan mode is where Claude Code starts a session whose settings
	// carry permissions.defaultMode "plan" when the launch passes no
	// --permission-mode, including when administrator-managed settings
	// disable --dangerously-skip-permissions.
	readOnlyModes []string
}

func claudeReadOnlyModeDiagnostic(mode string) string {
	return "permission mode " + mode + ": Claude Code cannot make changes; set the agent's permission_mode to a mode your administrator allows " +
		"(cause: a defaultMode in user or managed settings, bypass mode disabled)"
}

func newClaudeOutputs(readOnlyModes ...string) (outputParser, outputParser) {
	state := &claudeOutputState{readOnlyModes: readOnlyModes}
	return &claudeOutput{state: state, stdout: true}, &claudeOutput{state: state}
}

func (w *claudeOutput) setDestination(dst io.Writer) { w.dst = dst }

// setProgress registers the callback that reports liveness for records the
// renderer drops. The idle-timeout heartbeat watches run.log's modification
// time, and a long tool call or long thinking emits only progress records, so
// dropping them must still count as activity.
func (w *claudeOutput) setProgress(progress func()) { w.progress = progress }

func (w *claudeOutput) setModelProgress(progress func()) { w.modelProgress = progress }

func (w *claudeOutput) setUsageLimit(onUsageLimit func()) { w.usageLimit = onUsageLimit }

func (w *claudeOutput) setPermissionModeStop(onStop func()) { w.stopForMode = onStop }

// ReadOnlyPermissionMode returns the diagnostic of a session that started in a
// read-only permission mode, or nil.
func (w *claudeOutput) ReadOnlyPermissionMode() *PermissionModeError {
	w.state.mu.Lock()
	defer w.state.mu.Unlock()
	return w.state.readOnlyMode
}

// recordReadOnlyMode records the session's read-only mode once across the
// shared stdout/stderr state and reports whether this call recorded it.
func (w *claudeOutput) recordReadOnlyMode(mode string) (*PermissionModeError, bool) {
	w.state.mu.Lock()
	defer w.state.mu.Unlock()
	if w.state.readOnlyMode != nil {
		return w.state.readOnlyMode, false
	}
	w.state.readOnlyMode = &PermissionModeError{Mode: mode, Diagnostic: claudeReadOnlyModeDiagnostic(mode)}
	return w.state.readOnlyMode, true
}

func (w *claudeOutput) Write(p []byte) (int, error) {
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

func (w *claudeOutput) Flush() error {
	if w.buf.Len() == 0 {
		return nil
	}
	line := append([]byte(nil), w.buf.Bytes()...)
	w.buf.Reset()
	return w.writeLine(line, false)
}

func (w *claudeOutput) SessionID() string {
	w.state.mu.Lock()
	defer w.state.mu.Unlock()
	return w.state.sessionID
}

func (w *claudeOutput) SessionNotFound() bool {
	w.state.mu.Lock()
	defer w.state.mu.Unlock()
	return w.state.sessionNotFound
}

// claudeSessionNotFoundMessage starts the error Claude Code reports, both in
// the final result record's errors and as plain stderr text, when --resume
// names a session that does not exist.
const claudeSessionNotFoundMessage = "No conversation found with session ID"

func (w *claudeOutput) recordSessionNotFound() {
	w.state.mu.Lock()
	defer w.state.mu.Unlock()
	w.state.sessionNotFound = true
}

func (w *claudeOutput) UsageLimitReached() bool {
	w.state.mu.Lock()
	defer w.state.mu.Unlock()
	return w.state.usageLimitReached
}

// PermissionDenials returns the tools Claude Code refused during the session,
// in first-seen order: the best-effort `permission_denied` system records and
// the authoritative `permission_denials` list on the final result record.
func (w *claudeOutput) PermissionDenials() []string {
	w.state.mu.Lock()
	defer w.state.mu.Unlock()
	return append([]string(nil), w.state.permissionDenials...)
}

func (w *claudeOutput) recordPermissionDenial(tool string) {
	if tool == "" {
		return
	}
	w.state.mu.Lock()
	defer w.state.mu.Unlock()
	if !slices.Contains(w.state.permissionDenials, tool) {
		w.state.permissionDenials = append(w.state.permissionDenials, tool)
	}
}

func (w *claudeOutput) writeLine(line []byte, newline bool) error {
	text := strings.TrimSuffix(string(line), "\r")
	var record map[string]any
	if !strings.HasPrefix(strings.TrimSpace(text), "{") || json.Unmarshal([]byte(text), &record) != nil {
		if strings.HasPrefix(strings.TrimSpace(text), claudeSessionNotFoundMessage) {
			w.recordSessionNotFound()
		}
		return w.writeRaw(line, newline)
	}
	recordType, _ := record["type"].(string)
	// Only the init record names the session that actually started: a failed
	// --resume reports the requested ID on its result record as well.
	if id, _ := record["session_id"].(string); id != "" && recordType == "system" && record["subtype"] == "init" {
		w.state.mu.Lock()
		if w.state.sessionID == "" {
			w.state.sessionID = id
		}
		w.state.mu.Unlock()
	}
	if recordType == "result" && claudeResultReportsMissingSession(record) {
		w.recordSessionNotFound()
	}
	if w.stdout && recordType == "assistant" && claudeAssistantHasText(record) && w.modelProgress != nil {
		w.modelProgress()
	}
	if subtype, _ := record["subtype"].(string); recordType == "system" && subtype == "permission_denied" {
		tool, _ := record["tool_name"].(string)
		w.recordPermissionDenial(tool)
	}
	if denials, _ := record["permission_denials"].([]any); recordType == "result" {
		for _, denial := range denials {
			entry, _ := denial.(map[string]any)
			tool, _ := entry["tool_name"].(string)
			w.recordPermissionDenial(tool)
		}
	}
	if recordType == "result" && claudeUsageLimitLine(text) {
		w.state.mu.Lock()
		alreadyReached := w.state.usageLimitReached
		w.state.usageLimitReached = true
		onUsageLimit := w.usageLimit
		w.state.mu.Unlock()
		if !alreadyReached && onUsageLimit != nil {
			onUsageLimit()
		}
	}
	var lines []string
	switch recordType {
	case "system":
		lines = claudeSystemLines(record)
		if stopped := w.readOnlyModeAtInit(record); stopped != nil {
			lines = append(lines, "Stopped: "+stopped.Diagnostic)
		}
	case "rate_limit_event":
		lines = claudeRateLimitLines(record)
	case "assistant":
		lines = claudeAssistantLines(record)
	case "user":
		lines = claudeToolErrorLines(record)
	case "result":
		lines = claudeResultLines(record)
	case "stream_event", "tool_progress":
	default:
		return w.writeRaw(line, newline)
	}
	if len(lines) == 0 && w.progress != nil {
		w.progress()
	}
	for _, rendered := range lines {
		if _, err := io.WriteString(w.dst, rendered+"\n"); err != nil {
			return err
		}
	}
	return nil
}

// readOnlyModeAtInit recognises a session whose system/init record reports a
// read-only permission mode, stops the launch through the registered callback,
// and returns the diagnostic the first time it is seen.
func (w *claudeOutput) readOnlyModeAtInit(record map[string]any) *PermissionModeError {
	if subtype, _ := record["subtype"].(string); subtype != "init" {
		return nil
	}
	mode, _ := record["permissionMode"].(string)
	if mode == "" || !slices.Contains(w.state.readOnlyModes, mode) {
		return nil
	}
	stopped, first := w.recordReadOnlyMode(mode)
	if !first {
		return nil
	}
	if w.stopForMode != nil {
		w.stopForMode()
	}
	return stopped
}

func (w *claudeOutput) writeRaw(line []byte, newline bool) error {
	if _, err := w.dst.Write(line); err != nil {
		return err
	}
	if newline {
		_, err := io.WriteString(w.dst, "\n")
		return err
	}
	return nil
}

func claudeSystemLines(record map[string]any) []string {
	subtype, _ := record["subtype"].(string)
	switch subtype {
	case "init":
		parts := []string{"Claude Code"}
		if version, _ := record["claude_code_version"].(string); version != "" {
			parts[0] += " " + version
		}
		if model, _ := record["model"].(string); model != "" {
			parts = append(parts, "model "+model)
		}
		if mode, _ := record["permissionMode"].(string); mode != "" {
			parts = append(parts, "permissions "+mode)
		}
		if id, _ := record["session_id"].(string); id != "" {
			parts = append(parts, "session "+id)
		}
		return []string{strings.Join(parts, " · ")}
	case "permission_denied":
		tool, _ := record["tool_name"].(string)
		line := "Permission denied: " + tool
		if reason, _ := record["decision_reason"].(string); reason != "" {
			line += " (" + truncateString(singleLine(reason), 200) + ")"
		}
		return []string{line}
	case "compact_boundary":
		return []string{"Conversation compacted"}
	default:
		return nil
	}
}

func claudeRateLimitLines(record map[string]any) []string {
	info, _ := record["rate_limit_info"].(map[string]any)
	status, _ := info["status"].(string)
	if status == "" || status == "allowed" {
		return nil
	}
	line := "Rate limit: " + status
	if limitType, _ := info["rateLimitType"].(string); limitType != "" {
		line += " (" + limitType + ")"
	}
	if utilization, ok := info["utilization"].(float64); ok {
		line += fmt.Sprintf(", %.0f%% used", utilization*100)
	}
	if resetsAt, ok := info["resetsAt"].(float64); ok && resetsAt > 0 {
		line += ", resets " + time.Unix(int64(resetsAt), 0).UTC().Format("2006-01-02 15:04 UTC")
	}
	return []string{line}
}

func claudeAssistantLines(record map[string]any) []string {
	var lines []string
	for _, block := range claudeContentBlocks(record) {
		switch block["type"] {
		case "text":
			if text, _ := block["text"].(string); strings.TrimSpace(text) != "" {
				lines = append(lines, strings.TrimRight(text, "\n"))
			}
		case "tool_use":
			name, _ := block["name"].(string)
			input, _ := block["input"].(map[string]any)
			lines = append(lines, claudeToolLabel(name, input))
		}
	}
	return lines
}

func claudeAssistantHasText(record map[string]any) bool {
	if claudeAssistantHasError(record) {
		return false
	}
	for _, block := range claudeContentBlocks(record) {
		if block["type"] == "text" {
			if text, _ := block["text"].(string); strings.TrimSpace(text) != "" {
				return true
			}
		}
	}
	return false
}

func claudeAssistantHasError(record map[string]any) bool {
	errorValue, ok := record["error"]
	if !ok || errorValue == nil {
		return false
	}
	if message, ok := errorValue.(string); ok {
		return strings.TrimSpace(message) != ""
	}
	return true
}

func claudeToolErrorLines(record map[string]any) []string {
	var lines []string
	for _, block := range claudeContentBlocks(record) {
		if block["type"] != "tool_result" || block["is_error"] != true {
			continue
		}
		message := claudeToolResultText(block["content"])
		if message == "" {
			message = "tool call failed"
		}
		lines = append(lines, "Tool error: "+truncateString(singleLine(message), 300))
	}
	return lines
}

func claudeResultLines(record map[string]any) []string {
	subtype, _ := record["subtype"].(string)
	isError, _ := record["is_error"].(bool)
	status := "success"
	if isError {
		status = "error"
	}
	summary := "Result: " + status
	if subtype != "" && subtype != "success" {
		summary += " (" + subtype + ")"
	}
	if turns, ok := record["num_turns"].(float64); ok {
		summary += fmt.Sprintf(" · %d turns", int(turns))
	}
	if duration, ok := record["duration_ms"].(float64); ok && duration >= 1000 {
		summary += " · " + (time.Duration(duration) * time.Millisecond).Round(time.Second).String()
	}
	if cost, ok := record["total_cost_usd"].(float64); ok && cost > 0 {
		summary += fmt.Sprintf(" · $%.2f", cost)
	}
	lines := []string{summary}
	if isError {
		if message, _ := record["result"].(string); strings.TrimSpace(message) != "" {
			lines = append(lines, "Error: "+singleLine(message))
		}
	}
	return lines
}

// claudeToolLabel mirrors the OpenCode parser's labels: `$ <command>` for
// shell commands and `→ <Tool> <detail>` for everything else.
func claudeToolLabel(name string, input map[string]any) string {
	detail := ""
	for _, key := range []string{"command", "file_path", "notebook_path", "path", "pattern", "url", "query", "skill", "description", "prompt"} {
		if value, _ := input[key].(string); strings.TrimSpace(value) != "" {
			detail = truncateString(singleLine(value), 200)
			break
		}
	}
	switch {
	case name == "Bash":
		return strings.TrimSpace("$ " + detail)
	case name == "Skill" && detail != "":
		return fmt.Sprintf("→ Skill %q", detail)
	case detail != "":
		return "→ " + name + " " + detail
	default:
		return "→ " + name
	}
}

func claudeContentBlocks(record map[string]any) []map[string]any {
	message, _ := record["message"].(map[string]any)
	content, _ := message["content"].([]any)
	blocks := make([]map[string]any, 0, len(content))
	for _, item := range content {
		if block, ok := item.(map[string]any); ok {
			blocks = append(blocks, block)
		}
	}
	return blocks
}

func claudeToolResultText(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		var parts []string
		for _, item := range value {
			if block, ok := item.(map[string]any); ok {
				if text, _ := block["text"].(string); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}

func singleLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func claudeResultReportsMissingSession(record map[string]any) bool {
	errs, _ := record["errors"].([]any)
	for _, entry := range errs {
		if message, _ := entry.(string); strings.HasPrefix(message, claudeSessionNotFoundMessage) {
			return true
		}
	}
	return false
}
