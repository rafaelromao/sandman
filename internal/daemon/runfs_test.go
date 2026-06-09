package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDeadBatch_RunTimestamp_PrefersManifestCreatedAt(t *testing.T) {
	dir := t.TempDir()
	manifestTime := time.Now().Add(-10 * 24 * time.Hour).UTC().Round(time.Second)
	batch := DeadBatch{
		RunDir: dir,
		Manifest: BatchManifest{
			Issues:    []int{42},
			CreatedAt: manifestTime,
		},
	}
	if err := os.WriteFile(filepath.Join(dir, "batch.json"), mustMarshal(BatchManifest{Issues: []int{42}, CreatedAt: manifestTime}), 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.Chtimes(dir, time.Now(), time.Now()); err != nil {
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

func mustMarshal(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}
