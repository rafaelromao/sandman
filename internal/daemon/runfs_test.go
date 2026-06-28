package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
)

func TestDeadBatch_RunTimestamp_PrefersManifestCreatedAt(t *testing.T) {
	manifestTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	batch := DeadBatch{
		RunDir:   t.TempDir(),
		Manifest: BatchManifest{Issues: []int{42}, CreatedAt: manifestTime},
	}

	if err := os.Chtimes(batch.RunDir, time.Now(), time.Now()); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if got := batch.RunTimestamp(); !got.Equal(manifestTime) {
		t.Errorf("RunTimestamp = %v, want %v", got, manifestTime)
	}
}

func TestDeadBatch_RunTimestamp_FallsBackToMtime(t *testing.T) {
	dir := t.TempDir()
	mtime := time.Now().Add(-45 * 24 * time.Hour).UTC().Round(time.Second)
	if err := os.Chtimes(dir, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	batch := DeadBatch{RunDir: dir}

	got := batch.RunTimestamp()
	if !got.Equal(mtime) {
		t.Errorf("RunTimestamp = %v, want %v (dir mtime)", got, mtime)
	}
}

func TestDeadBatch_RunTimestamp_ZeroWhenNoSource(t *testing.T) {
	batch := DeadBatch{RunDir: filepath.Join(t.TempDir(), "missing")}
	if got := batch.RunTimestamp(); !got.IsZero() {
		t.Errorf("RunTimestamp = %v, want zero", got)
	}
}

func TestRunFolder(t *testing.T) {
	batchDir := "/some/batches/batch123"
	runID := "run456"
	got := RunFolder(batchDir, runID)
	want := "/some/batches/batch123/runs/run456"
	if got != want {
		t.Errorf("RunFolder(%q, %q) = %q, want %q", batchDir, runID, got, want)
	}
}

func TestBatchSocketPath(t *testing.T) {
	batchDir := "/some/batches/batch123"
	got := BatchSocketPath(batchDir)
	want := "/some/batches/batch123/batch.sock"
	if got != want {
		t.Errorf("BatchSocketPath(%q) = %q, want %q", batchDir, got, want)
	}
}

func TestRunSocketPath(t *testing.T) {
	batchDir := "/some/batches/batch123"
	runID := "run456"
	got := RunSocketPath(batchDir, runID)
	want := "/some/batches/batch123/runs/run456/run.sock"
	if got != want {
		t.Errorf("RunSocketPath(%q, %q) = %q, want %q", batchDir, runID, got, want)
	}
}

func TestWriteRunManifest(t *testing.T) {
	tmp := t.TempDir()
	batchDir := filepath.Join(tmp, "batch123")
	runID := "run456"

	manifest := batchindex.RunManifest{
		RunID:   runID,
		BatchID: "batch123",
		Issue:   1213,
		Branch:  "sandman/1213-foo",
	}

	if err := WriteRunManifest(batchDir, runID, manifest); err != nil {
		t.Fatalf("WriteRunManifest failed: %v", err)
	}

	wantPath := filepath.Join(batchDir, "runs", runID, "run.json")
	if _, err := os.Stat(wantPath); os.IsNotExist(err) {
		t.Fatalf("run.json not created at %s", wantPath)
	}

	got, err := ReadRunManifest(batchDir, runID)
	if err != nil {
		t.Fatalf("ReadRunManifest failed: %v", err)
	}
	if got.RunID != manifest.RunID {
		t.Errorf("RunID = %q, want %q", got.RunID, manifest.RunID)
	}
	if got.BatchID != manifest.BatchID {
		t.Errorf("BatchID = %q, want %q", got.BatchID, manifest.BatchID)
	}
	if got.Issue != manifest.Issue {
		t.Errorf("Issue = %d, want %d", got.Issue, manifest.Issue)
	}
}

func TestReadRunManifest_NotFound(t *testing.T) {
	tmp := t.TempDir()
	batchDir := filepath.Join(tmp, "batch123")
	runID := "nonexistent"

	_, err := ReadRunManifest(batchDir, runID)
	if err == nil {
		t.Error("expected error for nonexistent run")
	}
}

func TestUpdateRunManifestStatus_WritesTerminalStatus(t *testing.T) {
	tmp := t.TempDir()
	batchDir := filepath.Join(tmp, "batch123")
	runID := "run456"

	manifest := batchindex.RunManifest{
		RunID:     runID,
		BatchID:   "batch123",
		Status:    batchindex.RunManifestStatusActive,
		CreatedAt: time.Now(),
	}
	if err := WriteRunManifest(batchDir, runID, manifest); err != nil {
		t.Fatalf("WriteRunManifest failed: %v", err)
	}

	if err := UpdateRunManifestStatus(batchDir, runID, batchindex.RunManifestStatusSuccess); err != nil {
		t.Fatalf("UpdateRunManifestStatus failed: %v", err)
	}

	got, err := ReadRunManifest(batchDir, runID)
	if err != nil {
		t.Fatalf("ReadRunManifest failed: %v", err)
	}
	if got.Status != batchindex.RunManifestStatusSuccess {
		t.Errorf("Status = %q, want %q", got.Status, batchindex.RunManifestStatusSuccess)
	}
}

func TestIsRunActive_ProbesBatchSock(t *testing.T) {
	tmp := t.TempDir()
	batchPath := filepath.Join(tmp, "batch1")
	if err := os.MkdirAll(batchPath, 0755); err != nil {
		t.Fatal(err)
	}

	if IsRunActive(batchPath) {
		t.Error("expected IsRunActive to return false when no batch.sock exists")
	}

	ctlSock := NewControlSocket(batchPath, NewBroadcaster())
	if err := ctlSock.Start(); err != nil {
		t.Fatal(err)
	}
	defer ctlSock.Stop()

	if !IsRunActive(batchPath) {
		t.Error("expected IsRunActive to return true when batch.sock is live")
	}
}

func TestIsRunActive_RejectsStaleOnlyDirs(t *testing.T) {
	tmp := t.TempDir()
	batchPath := filepath.Join(tmp, "batch1")
	if err := os.MkdirAll(batchPath, 0755); err != nil {
		t.Fatal(err)
	}

	if IsRunActive(batchPath) {
		t.Error("expected IsRunActive to return false for empty batch dir")
	}
}
