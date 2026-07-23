package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/testenv"
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
		Branch:  "1213-foo",
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

// TestWriteRunManifest_PublicBatchIdRoundTrip pins the public BatchId
// round-trip for issue batches (issue #1917):
//
//   - run.json.batchId equals the public BatchId (= batch folder
//     basename = batch.json.batchId = event payload batch_id) for both
//     single-issue (no +N) and multi-issue (+<additionalCount>) batches.
//
// The orchestrator writes run.json with BatchID = s.batchID (= public
// BatchId); this test exercises the on-disk round-trip directly so a
// future regression that overwrites the field with the per-row RunID is
// caught without needing to run the orchestrator.
func TestWriteRunManifest_PublicBatchIdRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		publicID string
		runID    string
	}{
		{name: "single issue", publicID: "260618113825-abcd-42", runID: "260618113825-abcd-42"},
		{name: "two issues", publicID: "260618113825-abcd-42+1", runID: "260618113825-abcd-42"},
		{name: "nine issues", publicID: "260618113825-abcd-42+8", runID: "260618113825-abcd-42"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmp := t.TempDir()
			batchDir := filepath.Join(tmp, tt.publicID)
			manifest := batchindex.RunManifest{
				RunID:     tt.runID,
				BatchID:   tt.publicID,
				Issue:     42,
				Branch:    "42-fix",
				CreatedAt: time.Now(),
				Status:    batchindex.RunManifestStatusActive,
			}
			if err := WriteRunManifest(batchDir, tt.runID, manifest); err != nil {
				t.Fatalf("WriteRunManifest: %v", err)
			}
			got, err := ReadRunManifest(batchDir, tt.runID)
			if err != nil {
				t.Fatalf("ReadRunManifest: %v", err)
			}
			if got.BatchID != tt.publicID {
				t.Errorf("run.json.batchId = %q, want %q (public BatchId)", got.BatchID, tt.publicID)
			}
			// Saved log path remains <batchId>/runs/<runId>/run.log.
			logPath := filepath.Join(batchDir, "runs", tt.runID, "run.log")
			expectedDir := filepath.Join(tmp, tt.publicID, "runs", tt.runID)
			if filepath.Dir(logPath) != expectedDir {
				t.Errorf("log path dir = %q, want %q", filepath.Dir(logPath), expectedDir)
			}
		})
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
	tmp := testenv.MkdirShort(t, "sm-daemon-")
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

func TestBatchManifest_RunTSAndRunShortIDRoundTrip(t *testing.T) {
	dir := t.TempDir()
	pr := 42
	manifest := BatchManifest{
		Issues:     []int{7, 8},
		CreatedAt:  time.Date(2026, 6, 18, 11, 38, 25, 0, time.UTC),
		RunKind:    "issue",
		BatchId:    "260618113825-abcd-7+2",
		RunTS:      "260618113825",
		RunShortID: "abcd",
		PR:         &pr,
	}
	if err := WriteManifest(dir, manifest); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	got, err := ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if got.RunTS != "260618113825" {
		t.Errorf("RunTS = %q, want %q", got.RunTS, "260618113825")
	}
	if got.RunShortID != "abcd" {
		t.Errorf("RunShortID = %q, want %q", got.RunShortID, "abcd")
	}
	if got.BatchId != "260618113825-abcd-7+2" {
		t.Errorf("BatchId = %q, want preserved value", got.BatchId)
	}
	if got.RunKind != "issue" {
		t.Errorf("RunKind = %q, want %q", got.RunKind, "issue")
	}
}
