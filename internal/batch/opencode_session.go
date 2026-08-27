package batch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

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
	dst             io.Writer
	warning         io.Writer
	sessionID       string
	sessionNotFound bool
	stderr          bool
	buf             bytes.Buffer
}

func newOpenCodeOutput(dst, warning io.Writer, stderr bool) *opencodeOutput {
	return &opencodeOutput{dst: dst, warning: warning, stderr: stderr}
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
	if w.stderr && (strings.TrimSpace(text) == "Session not found" || strings.TrimSpace(text) == "Error: Session not found") {
		w.sessionNotFound = true
	}

	var event map[string]any
	if err := json.Unmarshal(line, &event); err != nil {
		return w.writeRaw(line, newline)
	}
	if id, ok := event["sessionID"].(string); ok && strings.TrimSpace(id) != "" {
		if w.sessionID == "" {
			w.sessionID = strings.TrimSpace(id)
		} else if w.sessionID != strings.TrimSpace(id) {
			w.warn(fmt.Sprintf("conflicting OpenCode session IDs %q and %q", w.sessionID, strings.TrimSpace(id)))
		}
	}

	eventType, _ := event["type"].(string)
	switch eventType {
	case "error":
		message := eventMessage(event)
		if normalizeSessionMessage(message) == "Session not found" {
			w.sessionNotFound = true
		}
		if message != "" {
			return w.writeText(message)
		}
	case "text":
		if text := eventText(event); text != "" {
			return w.writeText(text)
		}
	case "tool", "tool_use", "tool_result":
		if tool := eventTool(event); tool != "" {
			return w.writeText("tool: " + tool)
		}
	}
	return w.writeRaw(line, newline)
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

func (w *opencodeOutput) SessionID() string { return w.sessionID }

func (w *opencodeOutput) SessionNotFound() bool { return w.sessionNotFound }

func flushOpenCodeOutputs(outputs ...*opencodeOutput) {
	for _, output := range outputs {
		if output != nil {
			_ = output.Flush()
		}
	}
}
