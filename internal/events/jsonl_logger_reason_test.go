package events

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestJSONLLogger_PreservesHistoricalRunRetryEventsWithoutReason
// asserts that pre-#1501 run.retry events written to the JSONL log
// (which lack a "reason" key, or carry reason: null) are preserved
// verbatim by the logger. #1501 must not
// force-mutate the historical event log; consumers that read reason
// must tolerate the empty case for historical events.
//
// Two historical shapes are tested because issue #1501 notes
// "the ones currently sitting in this repo's .sandman/events.jsonl
// already with reason: null" — the pre-existing production log
// contains a mix of no-reason and reason:null lines depending on
// which orchestrator version wrote them, and both must round-trip
// without the logger backfilling or rewriting the line.
func TestJSONLLogger_PreservesHistoricalRunRetryEventsWithoutReason(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	historicalWithoutReason := []byte(`{"type":"run.retry","timestamp":"2025-01-01T12:00:00Z","run_id":"run-1","issue":42,"payload":{"attempt":2,"max_attempts":3,"previous_status":"failure","branch":"sandman/42-fix-bug","last_log_lines":["line1"]}}` + "\n")
	historicalWithNullReason := []byte(`{"type":"run.retry","timestamp":"2025-01-01T12:01:00Z","run_id":"run-1","issue":42,"payload":{"attempt":3,"max_attempts":3,"previous_status":"failure","branch":"sandman/42-fix-bug","last_log_lines":["line2"],"reason":null}}` + "\n")
	if err := os.WriteFile(path, append(historicalWithoutReason, historicalWithNullReason...), 0644); err != nil {
		t.Fatalf("seed events.jsonl: %v", err)
	}

	logger := &JSONLLogger{Path: path}
	logs, err := logger.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("expected 2 events on disk, got %d", len(logs))
	}

	if _, present := logs[0].Payload["reason"]; present {
		t.Errorf("historical run.retry (no reason key) gained a reason key on read: %+v", logs[0].Payload)
	}

	raw, present := logs[1].Payload["reason"]
	if !present {
		t.Errorf("historical run.retry (reason: null) lost the reason key on read: %+v", logs[1].Payload)
	}
	if raw != nil {
		t.Errorf("historical run.retry (reason: null) reason rewritten to %v, want nil", raw)
	}

	diskBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read events.jsonl after Read: %v", err)
	}
	if !strings.Contains(string(diskBytes), `"reason":null`) {
		t.Errorf("historical reason:null line was rewritten on disk; bytes now: %s", string(diskBytes))
	}
	if !strings.Contains(string(diskBytes), `"last_log_lines":["line1"]}`) {
		t.Errorf("historical no-reason line was rewritten on disk; bytes now: %s", string(diskBytes))
	}
	if _, err := os.Stat(path + ".malformed"); err == nil {
		t.Errorf("historical lines were quarantined to .malformed sidecar; this must not happen for valid pre-slice-3 lines")
	}

	var firstJSON map[string]any
	if err := json.Unmarshal(historicalWithoutReason, &firstJSON); err != nil {
		t.Fatalf("re-marshal baseline: %v", err)
	}
	if _, present := firstJSON["payload"].(map[string]any)["reason"]; present {
		t.Errorf("baseline historical line unexpectedly has a reason key: %s", historicalWithoutReason)
	}
}
