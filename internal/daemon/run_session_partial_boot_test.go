package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
)

// TestRunSession_Prepare_BatchesIndexInvalid_FailsBeforeSocket ensures a
// partial-boot failure at the batches-index update is fenced: batch.json is
// written but batch.sock is not bound, no run.started event is implied, and
// the retained partial batch directory is handled safely by stale recovery.
func TestRunSession_Prepare_BatchesIndexInvalid_FailsBeforeSocket(t *testing.T) {
	baseDir := t.TempDir()
	// Poison batches.json with invalid JSON and no .bak fallback.
	indexPath := BatchesIndexPath(baseDir)
	if err := os.MkdirAll(filepath.Dir(indexPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, []byte("invalid json{"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Ensure no .bak exists so Load cannot fall back.
	_ = os.Remove(indexPath + ".bak")

	rs := NewRunSession(baseDir, "partial-1")
	manifest := BatchManifest{Issues: []int{42}, CreatedAt: time.Now().UTC(), BatchId: "partial-1"}
	err := rs.Prepare(manifest)
	if err == nil {
		t.Cleanup(func() { _ = rs.Close() })
		t.Fatal("Prepare must fail when batches.json is invalid")
	}
	if !errors.Is(err, ErrStepBatchesIndex) {
		t.Fatalf("Prepare error = %v, want wrap of ErrStepBatchesIndex", err)
	}

	// batch.json was written before the index step.
	batchDir := filepath.Join(baseDir, "batches", "partial-1")
	if _, err := os.Stat(filepath.Join(batchDir, "batch.json")); err != nil {
		t.Fatalf("batch.json should exist after partial boot, stat err=%v", err)
	}
	// batch.sock must NOT be live: Prepare failed before binding.
	if IsRunActive(batchDir) {
		t.Fatalf("partial batch must not be considered live (no batch.sock)")
	}
	if _, err := os.Stat(BatchSocketPath(batchDir)); !os.IsNotExist(err) {
		t.Fatalf("batch.sock should not exist after failed Prepare, stat err=%v", err)
	}

	// Stale recovery must not crash on the retained partial directory and
	// must not emit a run.aborted when no run.started exists.
	eventLog := &recordingEventLog{}
	if recovered, _, err := RecoverStaleRuns(baseDir, nil, eventLog); err != nil {
		t.Fatalf("RecoverStaleRuns on partial dir: %v", err)
	} else if recovered != 0 {
		t.Fatalf("expected 0 recovered with no events, got %d", recovered)
	}
	// FindDeadRunBatches should still return the partial batch as dead.
	dead, err := FindDeadRunBatches(baseDir)
	if err != nil {
		t.Fatalf("FindDeadRunBatches: %v", err)
	}
	found := false
	for _, d := range dead {
		if filepath.Base(d.RunDir) == "partial-1" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("FindDeadRunBatches should include partial batch dir, got %v", dead)
	}

	// Portal-like handling: reading events with no run.started should be safe.
	// Projecting empty events must not produce an active run for the partial dir.
	states := events.ProjectRunStates(nil)
	if len(states) != 0 {
		t.Fatalf("empty events should project to 0 states, got %d", len(states))
	}
}

// TestRunSession_Prepare_BatchesIndexUnwritable_FailsBeforeSocket covers the
// unwritable-index variant: batches.json path is a directory so Update cannot
// open the lock file.
func TestRunSession_Prepare_BatchesIndexUnwritable_FailsBeforeSocket(t *testing.T) {
	baseDir := t.TempDir()
	indexPath := BatchesIndexPath(baseDir)
	if err := os.MkdirAll(indexPath, 0o755); err != nil {
		t.Fatal(err)
	}
	// Add a file inside so it looks like a directory, not a file.
	if err := os.WriteFile(filepath.Join(indexPath, "blocker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	rs := NewRunSession(baseDir, "partial-2")
	manifest := BatchManifest{Issues: []int{43}, CreatedAt: time.Now().UTC(), BatchId: "partial-2"}
	err := rs.Prepare(manifest)
	if err == nil {
		t.Cleanup(func() { _ = rs.Close() })
		t.Fatal("Prepare must fail when batches.json is a directory")
	}
	if !errors.Is(err, ErrStepBatchesIndex) {
		t.Fatalf("Prepare error = %v, want wrap of ErrStepBatchesIndex", err)
	}
	batchDir := filepath.Join(baseDir, "batches", "partial-2")
	if _, err := os.Stat(filepath.Join(batchDir, "batch.json")); err != nil {
		t.Fatalf("batch.json should exist even after index failure, stat err=%v", err)
	}
	if IsRunActive(batchDir) {
		t.Fatalf("partial batch must not be live")
	}
}
