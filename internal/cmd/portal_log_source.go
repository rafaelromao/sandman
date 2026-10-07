package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"time"
)

const portalLogPollInterval = 100 * time.Millisecond
const portalLogSnapshotLimit = 256 * 1024

// portalLogCursor identifies a position in one Saved Run Log. Offset is the
// raw byte end of the last accepted complete record, before display cleaning.
type portalLogCursor struct {
	RunID      string `json:"runId"`
	Generation string `json:"generation"`
	Offset     int64  `json:"offset"`
}

type portalLogRecord struct {
	RunID      string `json:"runId"`
	Generation string `json:"generation"`
	Start      int64  `json:"start"`
	End        int64  `json:"end"`
	Text       string `json:"text"`
}

type portalLogBatch struct {
	RunID      string            `json:"runId"`
	Generation string            `json:"generation"`
	Start      int64             `json:"start"`
	End        int64             `json:"end"`
	Records    []portalLogRecord `json:"records"`
	Cursor     portalLogCursor   `json:"cursor"`
	Bounded    bool              `json:"bounded,omitempty"`
	Reset      bool              `json:"reset,omitempty"`
	Reason     string            `json:"reason,omitempty"`
}

type portalLogStreamEvent struct {
	Type   string          `json:"type"`
	Batch  *portalLogBatch `json:"batch,omitempty"`
	Reason string          `json:"reason,omitempty"`
}

type portalLogSource struct {
	path  string
	runID string
	file  *os.File
	gen   string
	pos   int64
}

func newPortalLogSource(path, runID string) (*portalLogSource, error) {
	if strings.TrimSpace(path) == "" {
		return nil, os.ErrNotExist
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &portalLogSource{path: path, runID: runID, file: f, gen: portalLogGeneration(info)}, nil
}

func (s *portalLogSource) Close() error {
	if s == nil || s.file == nil {
		return nil
	}
	return s.file.Close()
}

func (s *portalLogSource) cursor() portalLogCursor {
	return portalLogCursor{RunID: s.runID, Generation: s.gen, Offset: s.pos}
}

func (s *portalLogSource) snapshot() (portalLogBatch, error) {
	info, err := s.file.Stat()
	if err != nil {
		return portalLogBatch{}, err
	}
	mark := info.Size()
	start, err := s.snapshotStart(mark)
	if err != nil {
		return portalLogBatch{}, err
	}
	records, end, err := s.records(start, mark, false)
	if err != nil {
		return portalLogBatch{}, err
	}
	s.pos = end
	return portalLogBatch{RunID: s.runID, Generation: s.gen, Start: start, End: end, Records: records, Cursor: s.cursor(), Bounded: start > 0}, nil
}

func (s *portalLogSource) snapshotStart(mark int64) (int64, error) {
	if mark <= portalLogSnapshotLimit {
		return 0, nil
	}
	candidate := mark - portalLogSnapshotLimit
	window := make([]byte, mark-candidate)
	if _, err := s.file.ReadAt(window, candidate); err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	if newline := bytes.IndexByte(window, '\n'); newline >= 0 {
		return candidate + int64(newline) + 1, nil
	}
	return 0, nil
}

func (s *portalLogSource) refreshPathIdentity() (bool, error) {
	info, err := os.Stat(s.path)
	if err != nil {
		// A rename/archive removes the original path but does not invalidate
		// the open descriptor or its generation.
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	generation := portalLogGeneration(info)
	if generation == s.gen {
		return false, nil
	}
	file, err := os.Open(s.path)
	if err != nil {
		return false, err
	}
	_ = s.file.Close()
	s.file = file
	s.gen = generation
	s.pos = 0
	return true, nil
}

func (s *portalLogSource) appendBatch(terminal bool) (portalLogBatch, bool, error) {
	replaced, err := s.refreshPathIdentity()
	if err != nil {
		return portalLogBatch{}, false, err
	}
	if replaced {
		return portalLogBatch{RunID: s.runID, Generation: s.gen, Reset: true, Reason: "source-replaced"}, true, nil
	}
	info, err := s.file.Stat()
	if err != nil {
		return portalLogBatch{}, false, err
	}
	if info.Size() < s.pos {
		return portalLogBatch{RunID: s.runID, Generation: s.gen, Reset: true, Reason: "source-truncated"}, true, nil
	}
	if info.Size() == s.pos {
		if terminal {
			// A final unterminated fragment is a record once lifecycle state says
			// the writer has flushed and the AgentRun is terminal.
			records, end, err := s.records(s.pos, info.Size(), true)
			if err != nil {
				return portalLogBatch{}, false, err
			}
			if len(records) > 0 {
				s.pos = end
				return portalLogBatch{RunID: s.runID, Generation: s.gen, Start: records[0].Start, End: end, Records: records, Cursor: s.cursor()}, true, nil
			}
		}
		return portalLogBatch{}, false, nil
	}
	records, end, err := s.records(s.pos, info.Size(), terminal)
	if err != nil {
		return portalLogBatch{}, false, err
	}
	if len(records) == 0 {
		return portalLogBatch{}, false, nil
	}
	s.pos = end
	return portalLogBatch{RunID: s.runID, Generation: s.gen, Start: records[0].Start, End: end, Records: records, Cursor: s.cursor()}, true, nil
}

func (s *portalLogSource) records(start, limit int64, includePartial bool) ([]portalLogRecord, int64, error) {
	if start < 0 || limit < start {
		return nil, start, io.ErrUnexpectedEOF
	}
	data := make([]byte, limit-start)
	if len(data) > 0 {
		if _, err := s.file.ReadAt(data, start); err != nil && !errors.Is(err, io.EOF) {
			return nil, start, err
		}
	}
	reader := bufio.NewReader(strings.NewReader(string(data)))
	records := make([]portalLogRecord, 0)
	offset := start
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			complete := strings.HasSuffix(line, "\n")
			if complete || includePartial {
				end := offset + int64(len(line))
				records = append(records, portalLogRecord{RunID: s.runID, Generation: s.gen, Start: offset, End: end, Text: cleanPortalSavedRecord(line)})
				offset = end
			} else {
				break
			}
		}
		if err != nil {
			break
		}
	}
	return records, offset, nil
}

func cleanPortalSavedRecord(raw string) string {
	cleaned := portalANSISequence.ReplaceAllString(raw, "")
	cleaned = strings.TrimSuffix(cleaned, "\n")
	cleaned = strings.TrimSuffix(cleaned, "\r")
	return stripLogLabel(cleaned)
}

func portalLogGeneration(info os.FileInfo) string {
	if info == nil || info.Sys() == nil {
		return "unknown"
	}
	sys := reflect.ValueOf(info.Sys())
	for sys.Kind() == reflect.Pointer {
		if sys.IsNil() {
			return "unknown"
		}
		sys = sys.Elem()
	}
	if sys.Kind() != reflect.Struct {
		return fmt.Sprintf("%T", info.Sys())
	}
	parts := make([]string, 0, 3)
	for _, name := range []string{"Dev", "Ino", "VolumeSerialNumber", "FileIndexHigh", "FileIndexLow"} {
		field := sys.FieldByName(name)
		if field.IsValid() && field.CanInterface() {
			parts = append(parts, fmt.Sprintf("%s=%v", name, field.Interface()))
		}
	}
	if len(parts) == 0 {
		return fmt.Sprintf("%T:%s", info.Sys(), info.Name())
	}
	return fmt.Sprintf("%T:%s", info.Sys(), strings.Join(parts, ","))
}

func encodePortalLogCursor(cursor portalLogCursor) string {
	data, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodePortalLogCursor(value string) (portalLogCursor, error) {
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return portalLogCursor{}, err
	}
	var cursor portalLogCursor
	if err := json.Unmarshal(data, &cursor); err != nil {
		return portalLogCursor{}, err
	}
	if cursor.RunID == "" || cursor.Generation == "" || cursor.Offset < 0 {
		return portalLogCursor{}, errors.New("invalid portal log cursor")
	}
	return cursor, nil
}

func writePortalLogEvent(w io.Writer, event string, payload any, id string) error {
	if id != "" {
		if _, err := fmt.Fprintf(w, "id: %s\n", id); err != nil {
			return err
		}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return err
	}
	if f, ok := w.(interface{ Flush() }); ok {
		f.Flush()
	}
	return nil
}

func portalLogSourcePath(run portalRun) string {
	return strings.TrimSpace(run.LogPath)
}

func portalLogTerminal(run portalRun) bool {
	// Kind describes the Portal row shape, not lifecycle. In particular, a
	// queued or waiting row may not be active while its event-derived run is
	// still unfinished. Only terminal lifecycle outcomes authorize end.
	return isTerminalStatus(run.Status)
}

func streamPortalSavedLog(ctx context.Context, w io.Writer, source *portalLogSource, initial *portalLogCursor, terminal func() bool) error {
	defer source.Close()
	reset := false
	if initial != nil {
		if initial.RunID != source.runID || initial.Generation != source.gen || initial.Offset < 0 {
			reset = true
		} else {
			info, statErr := source.file.Stat()
			if statErr != nil || initial.Offset > info.Size() {
				reset = true
			} else {
				retainedStart, startErr := source.snapshotStart(info.Size())
				if startErr != nil || initial.Offset < retainedStart {
					reset = true
				}
				source.pos = initial.Offset
				if !reset {
					_, end, err := source.records(0, source.pos, false)
					if err != nil || end != source.pos {
						reset = true
					}
				}
			}
		}
	}
	if reset || initial == nil {
		batch, err := source.snapshot()
		if err != nil {
			return err
		}
		if reset {
			if err := writePortalLogEvent(w, "reset", map[string]any{"runId": source.runID, "generation": source.gen, "reason": "invalid-cursor"}, ""); err != nil {
				return err
			}
		}
		if err := writePortalLogEvent(w, "snapshot", batch, encodePortalLogCursor(batch.Cursor)); err != nil {
			return err
		}
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		// Observe lifecycle once and use that same observation for both the
		// final drain and the end event. A second observation here can race a
		// terminal transition and close the stream before its final fragment is
		// accepted.
		terminalNow := terminal()
		batch, changed, err := source.appendBatch(terminalNow)
		if err != nil {
			return err
		}
		if changed && batch.Reset {
			if err := writePortalLogEvent(w, "reset", batch, ""); err != nil {
				return err
			}
			fresh, snapshotErr := source.snapshot()
			if snapshotErr != nil {
				return snapshotErr
			}
			if err := writePortalLogEvent(w, "snapshot", fresh, encodePortalLogCursor(fresh.Cursor)); err != nil {
				return err
			}
			continue
		}
		if changed {
			if err := writePortalLogEvent(w, "append", batch, encodePortalLogCursor(batch.Cursor)); err != nil {
				return err
			}
		}
		if terminalNow {
			return writePortalLogEvent(w, "end", map[string]any{"runId": source.runID, "generation": source.gen, "cursor": source.cursor()}, "")
		}
		timer := time.NewTimer(portalLogPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
