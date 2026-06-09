package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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
