package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/events"
)

// recordingEventLog captures every event handed to Log.
type recordingEventLog struct {
	logged []events.Event
}

func (r *recordingEventLog) Log(event events.Event) error {
	r.logged = append(r.logged, event)
	return nil
}

func (r *recordingEventLog) Read() ([]events.Event, error) { return nil, nil }
func (r *recordingEventLog) RemoveEventsByIssue(int) error { return nil }

func writeManifestFile(t *testing.T, runDir string, manifest BatchManifest) {
	t.Helper()
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := WriteManifest(runDir, manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func TestRecoverStaleRuns_EmitsAbortedForUnterminatedRun(t *testing.T) {
	baseDir := t.TempDir()
	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	started := createdAt.Add(5 * time.Minute)

	runDir := filepath.Join(baseDir, "batches", "dead-1")
	writeManifestFile(t, runDir, BatchManifest{Issues: []int{42}, CreatedAt: createdAt})

	eventLog := &recordingEventLog{}
	existing := []events.Event{
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: started},
	}

	recovered, dirs, err := RecoverStaleRuns(baseDir, existing, eventLog)
	if err != nil {
		t.Fatalf("RecoverStaleRuns: %v", err)
	}
	if recovered != 1 {
		t.Errorf("expected 1 recovered, got %d", recovered)
	}
	if dirs != 1 {
		t.Errorf("expected 1 dead dir, got %d", dirs)
	}
	if len(eventLog.logged) != 1 {
		t.Fatalf("expected 1 logged event, got %d", len(eventLog.logged))
	}
	e := eventLog.logged[0]
	if e.Type != "run.aborted" {
		t.Errorf("expected run.aborted, got %q", e.Type)
	}
	if e.IssueRef == nil || *e.IssueRef != 42 {
		t.Errorf("expected IssueRef=42, got %v", e.IssueRef)
	}
	if v, _ := e.Payload["recovered"].(bool); !v {
		t.Errorf("expected payload.recovered=true, got %v", e.Payload)
	}
}

func TestRecoverStaleRuns_UpdatesRunManifestStatusToAborted(t *testing.T) {
	baseDir := t.TempDir()
	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	started := createdAt.Add(5 * time.Minute)

	batchDir := filepath.Join(baseDir, "batches", "dead-1")
	writeManifestFile(t, batchDir, BatchManifest{Issues: []int{42}, CreatedAt: createdAt})

	runDir := filepath.Join(batchDir, "runs", "run-42")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := batchindex.WriteManifest(runDir, batchindex.RunManifest{
		RunID:     "run-42",
		BatchID:   "dead-1",
		Issue:     42,
		Status:    batchindex.RunManifestStatusActive,
		CreatedAt: started,
	}); err != nil {
		t.Fatalf("write run manifest: %v", err)
	}

	eventLog := &recordingEventLog{}
	existing := []events.Event{
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: started},
	}

	if _, _, err := RecoverStaleRuns(baseDir, existing, eventLog); err != nil {
		t.Fatalf("RecoverStaleRuns: %v", err)
	}

	manifest, err := batchindex.ReadManifest(runDir)
	if err != nil {
		t.Fatalf("read run manifest after recovery: %v", err)
	}
	if manifest.Status != batchindex.RunManifestStatusAborted {
		t.Errorf("run.json status = %q, want %q", manifest.Status, batchindex.RunManifestStatusAborted)
	}
}

func TestRecoverStaleRuns_RecoversRunStartedBeforeCreatedAtAsOrphan(t *testing.T) {
	baseDir := t.TempDir()
	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	started := createdAt.Add(-1 * time.Hour)

	runDir := filepath.Join(baseDir, "batches", "old-1")
	writeManifestFile(t, runDir, BatchManifest{Issues: []int{42}, CreatedAt: createdAt})

	eventLog := &recordingEventLog{}
	existing := []events.Event{
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: started},
	}

	recovered, dirs, err := RecoverStaleRuns(baseDir, existing, eventLog)
	if err != nil {
		t.Fatalf("RecoverStaleRuns: %v", err)
	}
	if recovered != 1 {
		t.Errorf("expected 1 recovered (orphaned run), got %d", recovered)
	}
	if dirs != 1 {
		t.Errorf("expected 1 dead dir, got %d", dirs)
	}
	if len(eventLog.logged) != 1 {
		t.Fatalf("expected 1 logged event, got %d", len(eventLog.logged))
	}
	e := eventLog.logged[0]
	if e.Type != "run.aborted" {
		t.Errorf("expected run.aborted, got %q", e.Type)
	}
	if e.IssueRef == nil || *e.IssueRef != 42 {
		t.Errorf("expected IssueRef=42, got %v", e.IssueRef)
	}
	if v, _ := e.Payload["recovered"].(bool); !v {
		t.Errorf("expected payload.recovered=true, got %v", e.Payload)
	}
}

func TestRecoverStaleRuns_SkipsTerminatedRun(t *testing.T) {
	baseDir := t.TempDir()
	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	started := createdAt.Add(5 * time.Minute)

	runDir := filepath.Join(baseDir, "batches", "done-1")
	writeManifestFile(t, runDir, BatchManifest{Issues: []int{42}, CreatedAt: createdAt})

	eventLog := &recordingEventLog{}
	existing := []events.Event{
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: started},
		{Type: "run.finished", RunID: "run-42", Issue: 42, Timestamp: started.Add(time.Hour), Payload: map[string]any{"status": "success"}},
	}

	recovered, _, err := RecoverStaleRuns(baseDir, existing, eventLog)
	if err != nil {
		t.Fatalf("RecoverStaleRuns: %v", err)
	}
	if recovered != 0 {
		t.Errorf("expected 0 recovered (already terminated), got %d", recovered)
	}
}

func TestRecoverStaleRuns_LiveRunExcluded(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("daemon socket liveness probing depends on Unix socket semantics; tracked by #1720")
	}
	baseDir := t.TempDir()
	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	started := createdAt.Add(5 * time.Minute)

	runDir := filepath.Join(baseDir, "batches", "live-1")
	writeManifestFile(t, runDir, BatchManifest{Issues: []int{42}, CreatedAt: createdAt})

	// Bind a live control socket so IsRunActive returns true.
	ctlSocket := NewControlSocket(runDir, NewBroadcaster())
	if err := ctlSocket.Start(); err != nil {
		t.Fatalf("start control socket: %v", err)
	}
	defer ctlSocket.Stop()

	eventLog := &recordingEventLog{}
	existing := []events.Event{
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: started},
	}

	recovered, dirs, err := RecoverStaleRuns(baseDir, existing, eventLog)
	if err != nil {
		t.Fatalf("RecoverStaleRuns: %v", err)
	}
	if recovered != 0 {
		t.Errorf("expected 0 recovered (live run), got %d", recovered)
	}
	if dirs != 0 {
		t.Errorf("expected 0 dead dirs (live run), got %d", dirs)
	}
	if len(eventLog.logged) != 0 {
		t.Errorf("expected no logged events, got %d", len(eventLog.logged))
	}
}

func TestRecoverStaleRuns_ContinuedResetsStartedTimestamp(t *testing.T) {
	baseDir := t.TempDir()
	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	firstStart := createdAt.Add(-2 * time.Hour)
	continuedAt := createdAt.Add(5 * time.Minute)

	runDir := filepath.Join(baseDir, "batches", "cont-1")
	writeManifestFile(t, runDir, BatchManifest{Issues: []int{42}, CreatedAt: createdAt})

	eventLog := &recordingEventLog{}
	existing := []events.Event{
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: firstStart},
		{Type: "run.continued", RunID: "run-42", Issue: 42, Timestamp: continuedAt},
	}

	recovered, _, err := RecoverStaleRuns(baseDir, existing, eventLog)
	if err != nil {
		t.Fatalf("RecoverStaleRuns: %v", err)
	}
	if recovered != 1 {
		t.Errorf("expected 1 recovered (continued inside window), got %d", recovered)
	}
}

func TestRecoverStaleRuns_RecoversQueuedFromDeadBatch(t *testing.T) {
	baseDir := t.TempDir()
	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	queuedAt := createdAt.Add(5 * time.Minute)

	runDir := filepath.Join(baseDir, "batches", "queued-1")
	writeManifestFile(t, runDir, BatchManifest{Issues: []int{42}, CreatedAt: createdAt})

	eventLog := &recordingEventLog{}
	existing := []events.Event{
		{Type: "run.queued", RunID: "run-42", Issue: 42, Timestamp: queuedAt},
	}

	recovered, dirs, err := RecoverStaleRuns(baseDir, existing, eventLog)
	if err != nil {
		t.Fatalf("RecoverStaleRuns: %v", err)
	}
	if recovered != 1 {
		t.Errorf("expected 1 recovered, got %d", recovered)
	}
	if dirs != 1 {
		t.Errorf("expected 1 dead dir, got %d", dirs)
	}
	if len(eventLog.logged) != 1 {
		t.Fatalf("expected 1 logged event, got %d", len(eventLog.logged))
	}
	e := eventLog.logged[0]
	if e.Type != "run.aborted" {
		t.Errorf("expected run.aborted, got %q", e.Type)
	}
	if e.IssueRef == nil || *e.IssueRef != 42 {
		t.Errorf("expected IssueRef=42, got %v", e.IssueRef)
	}
	if v, _ := e.Payload["recovered"].(bool); !v {
		t.Errorf("expected payload.recovered=true, got %v", e.Payload)
	}
}

func TestRecoverStaleRuns_RecoversBlockedFromDeadBatch(t *testing.T) {
	baseDir := t.TempDir()
	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	blockedAt := createdAt.Add(5 * time.Minute)

	runDir := filepath.Join(baseDir, "batches", "blocked-1")
	writeManifestFile(t, runDir, BatchManifest{Issues: []int{42}, CreatedAt: createdAt})

	eventLog := &recordingEventLog{}
	existing := []events.Event{
		{Type: "run.blocked", RunID: "run-42", Issue: 42, Timestamp: blockedAt, Payload: map[string]any{"blocked_by": []int{1}}},
	}

	recovered, dirs, err := RecoverStaleRuns(baseDir, existing, eventLog)
	if err != nil {
		t.Fatalf("RecoverStaleRuns: %v", err)
	}
	if recovered != 1 {
		t.Errorf("expected 1 recovered, got %d", recovered)
	}
	if dirs != 1 {
		t.Errorf("expected 1 dead dir, got %d", dirs)
	}
	if len(eventLog.logged) != 1 {
		t.Fatalf("expected 1 logged event, got %d", len(eventLog.logged))
	}
	e := eventLog.logged[0]
	if e.Type != "run.aborted" {
		t.Errorf("expected run.aborted, got %q", e.Type)
	}
	if e.IssueRef == nil || *e.IssueRef != 42 {
		t.Errorf("expected IssueRef=42, got %v", e.IssueRef)
	}
	if v, _ := e.Payload["recovered"].(bool); !v {
		t.Errorf("expected payload.recovered=true, got %v", e.Payload)
	}
}

func TestRecoverStaleRuns_RecoversOrphanActiveRun(t *testing.T) {
	baseDir := t.TempDir()
	startedAt := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)

	// No batch directories created — the run is orphaned.
	eventLog := &recordingEventLog{}
	existing := []events.Event{
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: startedAt},
	}

	recovered, dirs, err := RecoverStaleRuns(baseDir, existing, eventLog)
	if err != nil {
		t.Fatalf("RecoverStaleRuns: %v", err)
	}
	if recovered != 1 {
		t.Errorf("expected 1 recovered, got %d", recovered)
	}
	if dirs != 0 {
		t.Errorf("expected 0 dead dirs, got %d", dirs)
	}
	if len(eventLog.logged) != 1 {
		t.Fatalf("expected 1 logged event, got %d", len(eventLog.logged))
	}
	e := eventLog.logged[0]
	if e.Type != "run.aborted" {
		t.Errorf("expected run.aborted, got %q", e.Type)
	}
	if e.IssueRef == nil || *e.IssueRef != 42 {
		t.Errorf("expected IssueRef=42, got %v", e.IssueRef)
	}
	if v, _ := e.Payload["recovered"].(bool); !v {
		t.Errorf("expected payload.recovered=true, got %v", e.Payload)
	}
}

func TestRecoverStaleRuns_QueuedRunWithoutBatchDir_Recovered(t *testing.T) {
	baseDir := t.TempDir()
	queuedAt := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)

	// No batch directories — the queued run is orphaned and should be recovered.
	eventLog := &recordingEventLog{}
	existing := []events.Event{
		{Type: "run.queued", RunID: "run-42", Issue: 42, Timestamp: queuedAt},
	}

	recovered, dirs, err := RecoverStaleRuns(baseDir, existing, eventLog)
	if err != nil {
		t.Fatalf("RecoverStaleRuns: %v", err)
	}
	if recovered != 1 {
		t.Errorf("expected 1 recovered, got %d", recovered)
	}
	if dirs != 0 {
		t.Errorf("expected 0 dead dirs, got %d", dirs)
	}
	if len(eventLog.logged) != 1 {
		t.Fatalf("expected 1 logged event, got %d", len(eventLog.logged))
	}
	e := eventLog.logged[0]
	if e.Type != "run.aborted" {
		t.Errorf("expected run.aborted, got %q", e.Type)
	}
	if e.IssueRef == nil || *e.IssueRef != 42 {
		t.Errorf("expected IssueRef=42, got %v", e.IssueRef)
	}
}

func TestRecoverStaleRuns_BlockedRunWithoutBatchDir_Recovered(t *testing.T) {
	baseDir := t.TempDir()
	blockedAt := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)

	// No batch directories — the blocked run is orphaned and should be recovered.
	eventLog := &recordingEventLog{}
	existing := []events.Event{
		{Type: "run.blocked", RunID: "run-42", Issue: 42, Timestamp: blockedAt, Payload: map[string]any{"blocked_by": []int{1}}},
	}

	recovered, dirs, err := RecoverStaleRuns(baseDir, existing, eventLog)
	if err != nil {
		t.Fatalf("RecoverStaleRuns: %v", err)
	}
	if recovered != 1 {
		t.Errorf("expected 1 recovered, got %d", recovered)
	}
	if dirs != 0 {
		t.Errorf("expected 0 dead dirs, got %d", dirs)
	}
	if len(eventLog.logged) != 1 {
		t.Fatalf("expected 1 logged event, got %d", len(eventLog.logged))
	}
	e := eventLog.logged[0]
	if e.Type != "run.aborted" {
		t.Errorf("expected run.aborted, got %q", e.Type)
	}
	if e.IssueRef == nil || *e.IssueRef != 42 {
		t.Errorf("expected IssueRef=42, got %v", e.IssueRef)
	}
}

func TestRecoverStaleRuns_RecoversOrphanPromptOnlyRun(t *testing.T) {
	baseDir := t.TempDir()
	startedAt := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)

	// No batch directories created — the prompt-only run is orphaned.
	eventLog := &recordingEventLog{}
	existing := []events.Event{
		{Type: "run.started", RunID: "run-prompt", Timestamp: startedAt},
	}

	recovered, dirs, err := RecoverStaleRuns(baseDir, existing, eventLog)
	if err != nil {
		t.Fatalf("RecoverStaleRuns: %v", err)
	}
	if recovered != 1 {
		t.Errorf("expected 1 recovered, got %d", recovered)
	}
	if dirs != 0 {
		t.Errorf("expected 0 dead dirs, got %d", dirs)
	}
	if len(eventLog.logged) != 1 {
		t.Fatalf("expected 1 logged event, got %d", len(eventLog.logged))
	}
	e := eventLog.logged[0]
	if e.Type != "run.aborted" {
		t.Errorf("expected run.aborted, got %q", e.Type)
	}
	if e.IssueRef != nil {
		t.Errorf("expected nil IssueRef for prompt-only, got %v", *e.IssueRef)
	}
	if v, _ := e.Payload["recovered"].(bool); !v {
		t.Errorf("expected payload.recovered=true, got %v", e.Payload)
	}
}

func TestRecoverStaleRuns_RecoversPromptOnlyRunWithDeadBatchDir(t *testing.T) {
	baseDir := t.TempDir()
	createdAt := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	startedAt := createdAt.Add(5 * time.Minute)

	// Create a dead 0-issue batch dir (no sockets — IsRunActive returns false).
	runDir := filepath.Join(baseDir, "batches", "prompt-dead")
	writeManifestFile(t, runDir, BatchManifest{Issues: nil, CreatedAt: createdAt})

	eventLog := &recordingEventLog{}
	existing := []events.Event{
		{Type: "run.started", RunID: "run-prompt", Timestamp: startedAt},
	}

	recovered, dirs, err := RecoverStaleRuns(baseDir, existing, eventLog)
	if err != nil {
		t.Fatalf("RecoverStaleRuns: %v", err)
	}
	if recovered != 1 {
		t.Errorf("expected 1 recovered (dead prompt-only dir), got %d", recovered)
	}
	if dirs != 1 {
		t.Errorf("expected 1 dead dir, got %d", dirs)
	}
	if len(eventLog.logged) != 1 {
		t.Fatalf("expected 1 logged event, got %d", len(eventLog.logged))
	}
	e := eventLog.logged[0]
	if e.Type != "run.aborted" {
		t.Errorf("expected run.aborted, got %q", e.Type)
	}
	if v, _ := e.Payload["recovered"].(bool); !v {
		t.Errorf("expected payload.recovered=true, got %v", e.Payload)
	}
}

func TestRecoverStaleRuns_ManifestIssueWithoutRunIsSkipped(t *testing.T) {
	baseDir := t.TempDir()
	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)

	runDir := filepath.Join(baseDir, "batches", "no-run-1")
	writeManifestFile(t, runDir, BatchManifest{Issues: []int{42}, CreatedAt: createdAt})

	eventLog := &recordingEventLog{}
	// No events for issue 42.

	recovered, dirs, err := RecoverStaleRuns(baseDir, nil, eventLog)
	if err != nil {
		t.Fatalf("RecoverStaleRuns: %v", err)
	}
	if recovered != 0 {
		t.Errorf("expected 0 recovered (no matching run), got %d", recovered)
	}
	if dirs != 1 {
		t.Errorf("expected 1 dead dir to be processed, got %d", dirs)
	}
}

func TestRecoverStaleRuns_TwoQueuedRunsSameIssue_DeadBatch_RecoversBoth(t *testing.T) {
	baseDir := t.TempDir()
	createdAt := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	queuedA := createdAt.Add(1 * time.Minute)
	queuedB := createdAt.Add(5 * time.Minute)

	// One dead batch dir with issue 42. Two queued runs for the same issue
	// (from different batches/batch dead+re-queue). The dead batch loop
	// should recover both — earlier queued runs are not superseded by
	// later queued/blocked placeholders, only by actual run.started.
	runDir := filepath.Join(baseDir, "batches", "batch-1")
	writeManifestFile(t, runDir, BatchManifest{Issues: []int{42}, CreatedAt: createdAt})

	eventLog := &recordingEventLog{}
	existing := []events.Event{
		{Type: "run.queued", RunID: "first-queue", Issue: 42, Timestamp: queuedA, Payload: map[string]any{"blocked_by": []int{1}}},
		{Type: "run.queued", RunID: "second-queue", Issue: 42, Timestamp: queuedB, Payload: map[string]any{"blocked_by": []int{1}}},
	}

	recovered, dirs, err := RecoverStaleRuns(baseDir, existing, eventLog)
	if err != nil {
		t.Fatalf("RecoverStaleRuns: %v", err)
	}
	if recovered != 2 {
		t.Errorf("expected 2 recovered, got %d", recovered)
	}
	if dirs != 1 {
		t.Errorf("expected 1 dead dir, got %d", dirs)
	}
	if len(eventLog.logged) != 2 {
		t.Fatalf("expected 2 logged events, got %d", len(eventLog.logged))
	}
	for _, e := range eventLog.logged {
		if e.Type != "run.aborted" {
			t.Errorf("expected run.aborted, got %q", e.Type)
		}
		if e.IssueRef == nil || *e.IssueRef != 42 {
			t.Errorf("expected IssueRef=42, got %v", e.IssueRef)
		}
	}
}

func TestRecoverStaleRuns_TwoQueuedRunsSameIssue_NoBatchDirs_RecoversBoth(t *testing.T) {
	baseDir := t.TempDir()
	queuedA := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	queuedB := queuedA.Add(5 * time.Minute)

	// No batch directories. Two queued runs for the same issue — neither
	// is superseded because the later run is also queued, not started.
	eventLog := &recordingEventLog{}
	existing := []events.Event{
		{Type: "run.queued", RunID: "first-queue", Issue: 42, Timestamp: queuedA, Payload: map[string]any{"blocked_by": []int{1}}},
		{Type: "run.queued", RunID: "second-queue", Issue: 42, Timestamp: queuedB, Payload: map[string]any{"blocked_by": []int{99}}},
	}

	recovered, dirs, err := RecoverStaleRuns(baseDir, existing, eventLog)
	if err != nil {
		t.Fatalf("RecoverStaleRuns: %v", err)
	}
	if recovered != 2 {
		t.Errorf("expected 2 recovered, got %d", recovered)
	}
	if dirs != 0 {
		t.Errorf("expected 0 dead dirs, got %d", dirs)
	}
	if len(eventLog.logged) != 2 {
		t.Fatalf("expected 2 logged events, got %d", len(eventLog.logged))
	}
	for _, e := range eventLog.logged {
		if e.Type != "run.aborted" {
			t.Errorf("expected run.aborted, got %q", e.Type)
		}
	}
}

func TestRecoverStaleRuns_QueuedSupersededByLaterStarted_Skipped(t *testing.T) {
	baseDir := t.TempDir()
	queuedAt := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	startedAt := queuedAt.Add(5 * time.Minute)
	finishedAt := startedAt.Add(10 * time.Minute)

	// No batch directories — the queued placeholder was superseded by
	// a later actual run for the same issue (completed normally).
	eventLog := &recordingEventLog{}
	existing := []events.Event{
		{Type: "run.queued", RunID: "placeholder-42", Issue: 42, Timestamp: queuedAt, Payload: map[string]any{"blocked_by": []int{99}}},
		{Type: "run.started", RunID: "actual-run-42", Issue: 42, Timestamp: startedAt},
		{Type: "run.finished", RunID: "actual-run-42", Issue: 42, Timestamp: finishedAt, Payload: map[string]any{"status": "success"}},
	}

	recovered, dirs, err := RecoverStaleRuns(baseDir, existing, eventLog)
	if err != nil {
		t.Fatalf("RecoverStaleRuns: %v", err)
	}
	if recovered != 0 {
		t.Errorf("expected 0 recovered (superseded by actual run), got %d", recovered)
	}
	if dirs != 0 {
		t.Errorf("expected 0 dead dirs, got %d", dirs)
	}
	if len(eventLog.logged) != 0 {
		t.Errorf("expected 0 logged events, got %d", len(eventLog.logged))
	}
}

func TestRecoverStaleRuns_BlockedSupersededByLaterStarted_Skipped(t *testing.T) {
	baseDir := t.TempDir()
	blockedAt := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	startedAt := blockedAt.Add(5 * time.Minute)
	finishedAt := startedAt.Add(10 * time.Minute)

	// No batch directories — the blocked placeholder was superseded by
	// a later actual run for the same issue (completed normally).
	eventLog := &recordingEventLog{}
	existing := []events.Event{
		{Type: "run.blocked", RunID: "placeholder-42", Issue: 42, Timestamp: blockedAt, Payload: map[string]any{"blocked_by": []int{1}}},
		{Type: "run.started", RunID: "actual-run-42", Issue: 42, Timestamp: startedAt},
		{Type: "run.finished", RunID: "actual-run-42", Issue: 42, Timestamp: finishedAt, Payload: map[string]any{"status": "success"}},
	}

	recovered, dirs, err := RecoverStaleRuns(baseDir, existing, eventLog)
	if err != nil {
		t.Fatalf("RecoverStaleRuns: %v", err)
	}
	if recovered != 0 {
		t.Errorf("expected 0 recovered (superseded by actual run), got %d", recovered)
	}
	if dirs != 0 {
		t.Errorf("expected 0 dead dirs, got %d", dirs)
	}
	if len(eventLog.logged) != 0 {
		t.Errorf("expected 0 logged events, got %d", len(eventLog.logged))
	}
}

func TestRecoverStaleRuns_QueuedNotSupersededByAbortedStarted_Recovered(t *testing.T) {
	baseDir := t.TempDir()
	queuedAt := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	startedAt := queuedAt.Add(5 * time.Minute)
	abortedAt := startedAt.Add(2 * time.Minute)

	// No batch directories. The earlier queued placeholder was followed
	// by a run.started (real work began) but the daemon died — emitting
	// run.aborted. The issue was never actually completed, so the queued
	// placeholder is still an orphan that should be recovered.
	eventLog := &recordingEventLog{}
	existing := []events.Event{
		{Type: "run.queued", RunID: "placeholder-42", Issue: 42, Timestamp: queuedAt, Payload: map[string]any{"blocked_by": []int{99}}},
		{Type: "run.started", RunID: "actual-run-42", Issue: 42, Timestamp: startedAt},
		{Type: "run.aborted", RunID: "actual-run-42", Issue: 42, Timestamp: abortedAt, Payload: map[string]any{"recovered": true}},
	}

	recovered, _, err := RecoverStaleRuns(baseDir, existing, eventLog)
	if err != nil {
		t.Fatalf("RecoverStaleRuns: %v", err)
	}
	if recovered != 1 {
		t.Errorf("expected 1 recovered (aborted started run does not supersede), got %d", recovered)
	}
	if len(eventLog.logged) != 1 {
		t.Fatalf("expected 1 logged event, got %d", len(eventLog.logged))
	}
	e := eventLog.logged[0]
	if e.RunID != "placeholder-42" {
		t.Errorf("expected run.aborted for placeholder-42, got RunID=%q", e.RunID)
	}
}

func TestRecoverStaleRuns_OrphanAfterFinishedBatch(t *testing.T) {
	baseDir := t.TempDir()
	createdAt := time.Date(2026, 6, 8, 11, 38, 0, 0, time.UTC)
	batchFinishedAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	candidateStartedAt := time.Date(2026, 6, 8, 16, 0, 0, 0, time.UTC)

	runDir := filepath.Join(baseDir, "batches", "dead-batch")
	writeManifestFile(t, runDir, BatchManifest{
		Issues:    []int{960, 961, 962, 963, 964, 965, 966, 967, 968},
		CreatedAt: createdAt,
	})

	eventLog := &recordingEventLog{}
	existing := []events.Event{
		{Type: "run.started", RunID: "run-960", Issue: 960, Timestamp: createdAt.Add(2 * time.Minute)},
		{Type: "run.finished", RunID: "run-960", Issue: 960, Timestamp: batchFinishedAt, Payload: map[string]any{"status": "success"}},
		{Type: "run.started", RunID: "run-964", Issue: 964, Timestamp: candidateStartedAt},
	}

	recovered, _, err := RecoverStaleRuns(baseDir, existing, eventLog)
	if err != nil {
		t.Fatalf("RecoverStaleRuns: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("expected 1 recovered (candidate started after batch died), got %d", recovered)
	}
	if len(eventLog.logged) != 1 {
		t.Fatalf("expected 1 logged event, got %d", len(eventLog.logged))
	}
	e := eventLog.logged[0]
	if e.Type != "run.aborted" {
		t.Errorf("expected run.aborted, got %q", e.Type)
	}
	if e.RunID != "run-964" {
		t.Errorf("expected RunID=run-964, got %q", e.RunID)
	}
	if e.IssueRef == nil || *e.IssueRef != 964 {
		t.Errorf("expected IssueRef=964, got %v", e.IssueRef)
	}
	if v, _ := e.Payload["recovered"].(bool); !v {
		t.Errorf("expected payload.recovered=true, got %v", e.Payload)
	}
}

func TestRecoverStaleRuns_CoveredWithinBatchWindow(t *testing.T) {
	baseDir := t.TempDir()
	createdAt := time.Date(2026, 6, 8, 11, 38, 0, 0, time.UTC)
	batchFinishedAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	candidateStartedAt := time.Date(2026, 6, 8, 11, 50, 0, 0, time.UTC)

	runDir := filepath.Join(baseDir, "batches", "dead-batch")
	writeManifestFile(t, runDir, BatchManifest{
		Issues:    []int{960, 961, 962, 963, 964, 965, 966, 967, 968},
		CreatedAt: createdAt,
	})

	eventLog := &recordingEventLog{}
	existing := []events.Event{
		{Type: "run.started", RunID: "run-960", Issue: 960, Timestamp: createdAt.Add(2 * time.Minute)},
		{Type: "run.finished", RunID: "run-960", Issue: 960, Timestamp: batchFinishedAt, Payload: map[string]any{"status": "success"}},
		{Type: "run.started", RunID: "run-964", Issue: 964, Timestamp: candidateStartedAt},
	}

	recovered, _, err := RecoverStaleRuns(baseDir, existing, eventLog)
	if err != nil {
		t.Fatalf("RecoverStaleRuns: %v", err)
	}
	if recovered != 0 {
		t.Fatalf("expected 0 recovered (candidate started during batch window, covered), got %d", recovered)
	}
	if len(eventLog.logged) != 0 {
		t.Fatalf("expected 0 logged events, got %d", len(eventLog.logged))
	}
}

func TestRecoverStaleRuns_RecoversAutoSelectFromDeadBatch(t *testing.T) {
	baseDir := t.TempDir()
	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	started := createdAt.Add(5 * time.Minute)

	autoSelectRunID := "20260608-120000-abcd-auto-select-5c"
	runDir := filepath.Join(baseDir, "batches", autoSelectRunID+"-candidates")
	writeManifestFile(t, runDir, BatchManifest{
		RunKind:    "auto-select",
		BatchId:    autoSelectRunID,
		Candidates: []int{1, 2, 3, 4, 5},
		Count:      5,
		Query:      "label:ready-for-agent is:open",
		CreatedAt:  createdAt,
	})

	eventLog := &recordingEventLog{}
	existing := []events.Event{
		{
			Type:      "run.started",
			RunID:     autoSelectRunID,
			Issue:     0,
			Timestamp: started,
			Payload:   map[string]any{"run_kind": "auto-select", "count": 5},
		},
	}

	recovered, dirs, err := RecoverStaleRuns(baseDir, existing, eventLog)
	if err != nil {
		t.Fatalf("RecoverStaleRuns: %v", err)
	}
	if recovered != 1 {
		t.Errorf("expected 1 recovered, got %d", recovered)
	}
	if dirs != 1 {
		t.Errorf("expected 1 dead dir, got %d", dirs)
	}
	if len(eventLog.logged) != 1 {
		t.Fatalf("expected 1 logged event, got %d", len(eventLog.logged))
	}
	e := eventLog.logged[0]
	if e.Type != "run.aborted" {
		t.Errorf("expected run.aborted, got %q", e.Type)
	}
	if e.RunID != autoSelectRunID {
		t.Errorf("expected RunID %q, got %q", autoSelectRunID, e.RunID)
	}
	if e.Issue != 0 {
		t.Errorf("expected Issue=0 for auto-select, got %d", e.Issue)
	}
	if v, _ := e.Payload["recovered"].(bool); !v {
		t.Errorf("expected payload.recovered=true, got %v", e.Payload)
	}
}

func TestRecoverStaleRuns_SkipsAutoSelectWithTerminalStatus(t *testing.T) {
	baseDir := t.TempDir()
	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	started := createdAt.Add(5 * time.Minute)
	finished := started.Add(10 * time.Minute)

	autoSelectRunID := "20260608-120000-abcd-auto-select-5c"
	runDir := filepath.Join(baseDir, "batches", autoSelectRunID+"-candidates")
	writeManifestFile(t, runDir, BatchManifest{
		RunKind:    "auto-select",
		BatchId:    autoSelectRunID,
		Candidates: []int{1, 2, 3, 4, 5},
		Count:      5,
		CreatedAt:  createdAt,
	})

	eventLog := &recordingEventLog{}
	existing := []events.Event{
		{Type: "run.started", RunID: autoSelectRunID, Issue: 0, Timestamp: started, Payload: map[string]any{"run_kind": "auto-select"}},
		{Type: "run.finished", RunID: autoSelectRunID, Issue: 0, Timestamp: finished, Payload: map[string]any{"run_kind": "auto-select", "status": "success"}},
	}

	recovered, dirs, err := RecoverStaleRuns(baseDir, existing, eventLog)
	if err != nil {
		t.Fatalf("RecoverStaleRuns: %v", err)
	}
	if recovered != 0 {
		t.Errorf("expected 0 recovered (run already terminated), got %d", recovered)
	}
	if dirs != 1 {
		t.Errorf("expected 1 dead dir, got %d", dirs)
	}
	if len(eventLog.logged) != 0 {
		t.Fatalf("expected 0 logged events for terminal auto-select run, got %d", len(eventLog.logged))
	}
}
