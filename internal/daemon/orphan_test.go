package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
)

type fakeEventLog struct {
	events []events.Event
	err    error
}

func (f *fakeEventLog) Log(event events.Event) error              { return nil }
func (f *fakeEventLog) Read() ([]events.Event, error)            { return f.events, f.err }
func (f *fakeEventLog) RemoveEventsByIssue(int) error             { return nil }

func writeBatchManifest(t *testing.T, batchDir string) {
	t.Helper()
	if err := os.MkdirAll(batchDir, 0o755); err != nil {
		t.Fatalf("mkdir batch dir: %v", err)
	}
	if err := WriteManifest(batchDir, BatchManifest{BatchId: filepath.Base(batchDir), CreatedAt: time.Now()}); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func writeRunSubdir(t *testing.T, batchDir, runID string) {
	t.Helper()
	runDir := filepath.Join(batchDir, "runs", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
}

func TestCleanupOrphanedTestBatches_NoMatchAndNoSocket_RemovesDir(t *testing.T) {
	baseDir := t.TempDir()
	orphanDir := filepath.Join(baseDir, "batches", "orphan-1")
	writeBatchManifest(t, orphanDir)

	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "some-other-run", Timestamp: time.Now()},
	}}

	removed, err := CleanupOrphanedTestBatches(baseDir, log, func(string) bool { return false })
	if err != nil {
		t.Fatalf("CleanupOrphanedTestBatches: %v", err)
	}

	if len(removed) != 1 || removed[0] != orphanDir {
		t.Fatalf("removed = %v, want [%s]", removed, orphanDir)
	}
	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Errorf("orphan dir still exists: %v", err)
	}
}

func TestCleanupOrphanedTestBatches_DirNameMatchesRunStarted_KeepsDir(t *testing.T) {
	baseDir := t.TempDir()
	batchDir := filepath.Join(baseDir, "batches", "live-batch")
	writeBatchManifest(t, batchDir)

	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "live-batch", Timestamp: time.Now()},
	}}

	removed, err := CleanupOrphanedTestBatches(baseDir, log, func(string) bool { return false })
	if err != nil {
		t.Fatalf("CleanupOrphanedTestBatches: %v", err)
	}

	if len(removed) != 0 {
		t.Fatalf("removed = %v, want none", removed)
	}
	if _, err := os.Stat(batchDir); err != nil {
		t.Errorf("live batch dir removed unexpectedly: %v", err)
	}
}

func TestCleanupOrphanedTestBatches_RunSubdirMatchesRunStarted_KeepsDir(t *testing.T) {
	baseDir := t.TempDir()
	batchDir := filepath.Join(baseDir, "batches", "parent-batch")
	writeBatchManifest(t, batchDir)
	writeRunSubdir(t, batchDir, "child-run-42")

	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "child-run-42", Timestamp: time.Now()},
	}}

	removed, err := CleanupOrphanedTestBatches(baseDir, log, func(string) bool { return false })
	if err != nil {
		t.Fatalf("CleanupOrphanedTestBatches: %v", err)
	}

	if len(removed) != 0 {
		t.Fatalf("removed = %v, want none", removed)
	}
	if _, err := os.Stat(batchDir); err != nil {
		t.Errorf("batch dir removed unexpectedly: %v", err)
	}
}

func TestCleanupOrphanedTestBatches_LiveBatchSock_KeepsDirEvenWithNoMatchingEvents(t *testing.T) {
	baseDir := t.TempDir()
	batchDir := filepath.Join(baseDir, "lb")
	writeBatchManifest(t, batchDir)

	ctl := NewControlSocket(batchDir, NewBroadcaster())
	if err := ctl.Start(); err != nil {
		t.Fatalf("start control socket: %v", err)
	}
	defer ctl.Stop()

	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "unrelated", Timestamp: time.Now()},
	}}

	removed, err := CleanupOrphanedTestBatches(baseDir, log, IsRunActive)
	if err != nil {
		t.Fatalf("CleanupOrphanedTestBatches: %v", err)
	}

	if len(removed) != 0 {
		t.Fatalf("removed = %v, want none (live batch.sock must be kept)", removed)
	}
	if _, err := os.Stat(batchDir); err != nil {
		t.Errorf("live batch dir removed unexpectedly: %v", err)
	}
}

func TestCleanupOrphanedTestBatches_LiveRunSock_KeepsDirEvenWithNoMatchingEvents(t *testing.T) {
	baseDir := t.TempDir()
	batchDir := filepath.Join(baseDir, "lr")
	writeBatchManifest(t, batchDir)
	writeRunSubdir(t, batchDir, "rr")

	ctl := NewControlSocketWithName(filepath.Join(batchDir, "runs", "rr"), "run.sock", NewBroadcaster())
	if err := ctl.Start(); err != nil {
		t.Fatalf("start run control socket: %v", err)
	}
	defer ctl.Stop()

	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "unrelated", Timestamp: time.Now()},
	}}

	removed, err := CleanupOrphanedTestBatches(baseDir, log, IsRunActive)
	if err != nil {
		t.Fatalf("CleanupOrphanedTestBatches: %v", err)
	}

	if len(removed) != 0 {
		t.Fatalf("removed = %v, want none (live run.sock must be kept)", removed)
	}
}

func TestCleanupOrphanedTestBatches_NonDirEntries_Ignored(t *testing.T) {
	baseDir := t.TempDir()
	batchesDir := filepath.Join(baseDir, "batches")
	if err := os.MkdirAll(batchesDir, 0o755); err != nil {
		t.Fatalf("mkdir batches: %v", err)
	}
	strayFile := filepath.Join(batchesDir, "stray.txt")
	if err := os.WriteFile(strayFile, []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("write stray file: %v", err)
	}

	log := &fakeEventLog{}

	removed, err := CleanupOrphanedTestBatches(baseDir, log, func(string) bool { return false })
	if err != nil {
		t.Fatalf("CleanupOrphanedTestBatches: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want none", removed)
	}
	if _, err := os.Stat(strayFile); err != nil {
		t.Errorf("stray file removed unexpectedly: %v", err)
	}
}

func TestCleanupOrphanedTestBatches_MissingBatchesDir_ReturnsEmptyNoError(t *testing.T) {
	baseDir := t.TempDir()
	log := &fakeEventLog{}

	removed, err := CleanupOrphanedTestBatches(baseDir, log, func(string) bool { return false })
	if err != nil {
		t.Fatalf("CleanupOrphanedTestBatches: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want empty", removed)
	}
}

func TestCleanupOrphanedTestBatches_EventReadError_FailsClosed(t *testing.T) {
	baseDir := t.TempDir()
	batchDir := filepath.Join(baseDir, "batches", "would-remove")
	writeBatchManifest(t, batchDir)

	log := &fakeEventLog{err: errors.New("boom")}

	removed, err := CleanupOrphanedTestBatches(baseDir, log, func(string) bool { return false })
	if err == nil {
		t.Fatal("expected error from log.Read, got nil")
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want none on read error (fail-closed)", removed)
	}
	if _, err := os.Stat(batchDir); err != nil {
		t.Errorf("batch dir removed despite event-read failure: %v", err)
	}
}

func TestCleanupOrphanedTestBatches_ArchiveDirNotTouched(t *testing.T) {
	baseDir := t.TempDir()
	batchesDir := filepath.Join(baseDir, "batches")
	archiveDir := filepath.Join(baseDir, "archive")
	if err := os.MkdirAll(batchesDir, 0o755); err != nil {
		t.Fatalf("mkdir batches: %v", err)
	}
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		t.Fatalf("mkdir archive: %v", err)
	}

	orphanName := "orphan-mirror"
	orphanBatchDir := filepath.Join(batchesDir, orphanName)
	writeBatchManifest(t, orphanBatchDir)
	archiveBatchDir := filepath.Join(archiveDir, orphanName)
	writeBatchManifest(t, archiveBatchDir)

	log := &fakeEventLog{}

	removed, err := CleanupOrphanedTestBatches(baseDir, log, func(string) bool { return false })
	if err != nil {
		t.Fatalf("CleanupOrphanedTestBatches: %v", err)
	}

	if len(removed) != 1 || removed[0] != orphanBatchDir {
		t.Fatalf("removed = %v, want only [%s]", removed, orphanBatchDir)
	}
	if _, err := os.Stat(archiveBatchDir); err != nil {
		t.Errorf("archive dir touched unexpectedly: %v", err)
	}
}

func TestCleanupOrphanedTestBatches_DeterministicOrder(t *testing.T) {
	baseDir := t.TempDir()
	batchesDir := filepath.Join(baseDir, "batches")
	if err := os.MkdirAll(batchesDir, 0o755); err != nil {
		t.Fatalf("mkdir batches: %v", err)
	}
	names := []string{"zzz-orphan", "aaa-orphan", "mmm-orphan"}
	for _, n := range names {
		writeBatchManifest(t, filepath.Join(batchesDir, n))
	}

	log := &fakeEventLog{}

	removed, err := CleanupOrphanedTestBatches(baseDir, log, func(string) bool { return false })
	if err != nil {
		t.Fatalf("CleanupOrphanedTestBatches: %v", err)
	}

	bases := make([]string, len(removed))
	for i, p := range removed {
		bases[i] = filepath.Base(p)
	}
	want := append([]string(nil), bases...)
	sort.Strings(want)
	for i := range want {
		if bases[i] != want[i] {
			t.Errorf("removed order = %v, want sorted ascending", bases)
			break
		}
	}
}