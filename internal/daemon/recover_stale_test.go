package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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

	runDir := filepath.Join(baseDir, "runs", "dead-1")
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

func TestRecoverStaleRuns_RecoversRunStartedBeforeCreatedAtAsOrphan(t *testing.T) {
	baseDir := t.TempDir()
	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	started := createdAt.Add(-1 * time.Hour)

	runDir := filepath.Join(baseDir, "runs", "old-1")
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

	runDir := filepath.Join(baseDir, "runs", "done-1")
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
	baseDir := t.TempDir()
	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	started := createdAt.Add(5 * time.Minute)

	runDir := filepath.Join(baseDir, "runs", "live-1")
	writeManifestFile(t, runDir, BatchManifest{Issues: []int{42}, CreatedAt: createdAt})

	// Bind a live command server so IsRunActive returns true.
	cmdServer := NewCommandServer(runDir, nil)
	if err := cmdServer.Start(); err != nil {
		t.Fatalf("start command server: %v", err)
	}
	defer cmdServer.Stop()

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

	runDir := filepath.Join(baseDir, "runs", "cont-1")
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

	runDir := filepath.Join(baseDir, "runs", "queued-1")
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

	runDir := filepath.Join(baseDir, "runs", "blocked-1")
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

func TestRecoverStaleRuns_QueuedRunWithoutBatchDir_Skipped(t *testing.T) {
	baseDir := t.TempDir()
	queuedAt := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)

	// No batch directories — the orphan pass skips queued runs because it
	// cannot distinguish this from a completed batch whose dir was cleaned up.
	eventLog := &recordingEventLog{}
	existing := []events.Event{
		{Type: "run.queued", RunID: "run-42", Issue: 42, Timestamp: queuedAt},
	}

	recovered, dirs, err := RecoverStaleRuns(baseDir, existing, eventLog)
	if err != nil {
		t.Fatalf("RecoverStaleRuns: %v", err)
	}
	if recovered != 0 {
		t.Errorf("expected 0 recovered (queued run not orphan-recoverable), got %d", recovered)
	}
	if dirs != 0 {
		t.Errorf("expected 0 dead dirs, got %d", dirs)
	}
	if len(eventLog.logged) != 0 {
		t.Errorf("expected 0 logged events, got %d", len(eventLog.logged))
	}
}

func TestRecoverStaleRuns_BlockedRunWithoutBatchDir_Skipped(t *testing.T) {
	baseDir := t.TempDir()
	blockedAt := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)

	// No batch directories — the orphan pass skips blocked runs because it
	// cannot distinguish this from a completed batch whose dir was cleaned up.
	eventLog := &recordingEventLog{}
	existing := []events.Event{
		{Type: "run.blocked", RunID: "run-42", Issue: 42, Timestamp: blockedAt, Payload: map[string]any{"blocked_by": []int{1}}},
	}

	recovered, dirs, err := RecoverStaleRuns(baseDir, existing, eventLog)
	if err != nil {
		t.Fatalf("RecoverStaleRuns: %v", err)
	}
	if recovered != 0 {
		t.Errorf("expected 0 recovered (blocked run not orphan-recoverable), got %d", recovered)
	}
	if dirs != 0 {
		t.Errorf("expected 0 dead dirs, got %d", dirs)
	}
	if len(eventLog.logged) != 0 {
		t.Errorf("expected 0 logged events, got %d", len(eventLog.logged))
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
	runDir := filepath.Join(baseDir, "runs", "prompt-dead")
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

	runDir := filepath.Join(baseDir, "runs", "no-run-1")
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
