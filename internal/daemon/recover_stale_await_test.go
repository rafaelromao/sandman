package daemon

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/events"
)

// TestRecoverStaleRuns_RecoversAwaitingRun proves a dead awaited run becomes
// one recovered run.aborted, updates its manifest, and no longer projects as waiting.
func TestRecoverStaleRuns_RecoversAwaitingRun(t *testing.T) {
	baseDir := t.TempDir()
	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	started := createdAt.Add(5 * time.Minute)
	awaited := started.Add(2 * time.Minute)

	batchDir := filepath.Join(baseDir, "batches", "dead-await-1")
	writeManifestFile(t, batchDir, BatchManifest{Issues: []int{42}, CreatedAt: createdAt})

	runDir := filepath.Join(batchDir, "runs", "run-await-42")
	if err := batchindex.WriteManifest(runDir, batchindex.RunManifest{
		RunID:     "run-await-42",
		BatchID:   "dead-await-1",
		Issue:     42,
		Status:    batchindex.RunManifestStatusActive,
		CreatedAt: started,
	}); err != nil {
		t.Fatalf("write run manifest: %v", err)
	}

	eventLog := &recordingEventLog{}
	existing := []events.Event{
		{Type: "run.started", RunID: "run-await-42", Issue: 42, Timestamp: started},
		{Type: "run.await", RunID: "run-await-42", Issue: 42, Timestamp: awaited, Payload: map[string]any{"gate": "pending", "await": true}},
	}

	// Sanity: before recovery the run projects as waiting.
	statesBefore := events.ProjectRunStates(existing)
	if len(statesBefore) != 1 || !statesBefore[0].IsAwaiting() {
		t.Fatalf("before recovery expected awaiting run, got %+v", statesBefore)
	}

	recovered, dirs, err := RecoverStaleRuns(baseDir, existing, eventLog)
	if err != nil {
		t.Fatalf("RecoverStaleRuns: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("expected 1 recovered awaiting run, got %d", recovered)
	}
	if dirs != 1 {
		t.Fatalf("expected 1 dead dir, got %d", dirs)
	}
	if len(eventLog.logged) != 1 || eventLog.logged[0].Type != "run.aborted" {
		t.Fatalf("expected one run.aborted, got %v", eventLog.logged)
	}

	// Manifest must be flipped to aborted.
	manifest, err := batchindex.ReadManifest(runDir)
	if err != nil {
		t.Fatalf("read run manifest after recovery: %v", err)
	}
	if manifest.Status != batchindex.RunManifestStatusAborted {
		t.Fatalf("run.json status = %q, want aborted", manifest.Status)
	}

	// After recovery the projection must NOT be waiting; it is terminal aborted.
	combined := append([]events.Event(nil), existing...)
	combined = append(combined, eventLog.logged...)
	finalStates := events.ProjectRunStates(combined)
	var final *events.RunState
	for i := range finalStates {
		if finalStates[i].RunID == "run-await-42" {
			final = &finalStates[i]
			break
		}
	}
	if final == nil {
		t.Fatalf("no run state for run-await-42 after recovery")
	}
	if final.IsActive() {
		t.Fatalf("recovered awaiting run should be inactive, status=%q awaiting=%v", final.Status(), final.IsAwaiting())
	}
	if final.IsAwaiting() {
		t.Fatalf("recovered run must not project as waiting")
	}
	if final.Status() != "aborted" {
		t.Fatalf("status = %q, want aborted", final.Status())
	}
}

// TestRecoverStaleRuns_RecoversResumedRun proves a dead resumed implementation run
// is recovered exactly like an awaiting run: one run.aborted, manifest aborted,
// not portal-visible as waiting.
func TestRecoverStaleRuns_RecoversResumedRun(t *testing.T) {
	baseDir := t.TempDir()
	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	started := createdAt.Add(5 * time.Minute)
	awaited := started.Add(2 * time.Minute)
	resumed := awaited.Add(1 * time.Minute)

	batchDir := filepath.Join(baseDir, "batches", "dead-resumed-1")
	writeManifestFile(t, batchDir, BatchManifest{Issues: []int{42}, CreatedAt: createdAt})

	runDir := filepath.Join(batchDir, "runs", "run-resumed-42")
	if err := batchindex.WriteManifest(runDir, batchindex.RunManifest{
		RunID:     "run-resumed-42",
		BatchID:   "dead-resumed-1",
		Issue:     42,
		Status:    batchindex.RunManifestStatusActive,
		CreatedAt: started,
	}); err != nil {
		t.Fatalf("write run manifest: %v", err)
	}

	eventLog := &recordingEventLog{}
	existing := []events.Event{
		{Type: "run.started", RunID: "run-resumed-42", Issue: 42, Timestamp: started},
		{Type: "run.await", RunID: "run-resumed-42", Issue: 42, Timestamp: awaited, Payload: map[string]any{"gate": "pending"}},
		{Type: "run.resumed", RunID: "run-resumed-42", Issue: 42, Timestamp: resumed, Payload: map[string]any{"reason": "actionable-feedback"}},
	}

	// Resumed runs are active and not awaiting.
	statesBefore := events.ProjectRunStates(existing)
	if len(statesBefore) != 1 || !statesBefore[0].IsActive() {
		t.Fatalf("before recovery expected active resumed run, got %+v", statesBefore)
	}
	if statesBefore[0].IsAwaiting() {
		t.Fatalf("resumed run should not be awaiting before recovery")
	}

	recovered, dirs, err := RecoverStaleRuns(baseDir, existing, eventLog)
	if err != nil {
		t.Fatalf("RecoverStaleRuns: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("expected 1 recovered resumed run, got %d", recovered)
	}
	if dirs != 1 {
		t.Fatalf("expected 1 dead dir, got %d", dirs)
	}

	manifest, err := batchindex.ReadManifest(runDir)
	if err != nil {
		t.Fatalf("read run manifest after recovery: %v", err)
	}
	if manifest.Status != batchindex.RunManifestStatusAborted {
		t.Fatalf("run.json status = %q, want aborted", manifest.Status)
	}

	combined := append([]events.Event(nil), existing...)
	combined = append(combined, eventLog.logged...)
	finalStates := events.ProjectRunStates(combined)
	var final *events.RunState
	for i := range finalStates {
		if finalStates[i].RunID == "run-resumed-42" {
			final = &finalStates[i]
			break
		}
	}
	if final == nil {
		t.Fatalf("no run state for run-resumed-42 after recovery")
	}
	if final.IsActive() || final.IsAwaiting() {
		t.Fatalf("recovered resumed run should be terminal not awaiting: active=%v awaiting=%v status=%q", final.IsActive(), final.IsAwaiting(), final.Status())
	}
}
