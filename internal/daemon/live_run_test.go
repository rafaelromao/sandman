package daemon

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestLiveRunStore_RegisterListAndRemove(t *testing.T) {
	dir := t.TempDir()
	store := NewLiveRunStore(dir)
	run := LiveRun{
		RunID:     "run-42-123",
		PID:       os.Getpid(),
		Issues:    []int{42, 43},
		StartedAt: time.Unix(123, 0).UTC(),
	}

	if err := store.Register(run); err != nil {
		t.Fatalf("Register() failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "runs", run.RunID, "run.json"))
	if err != nil {
		t.Fatalf("run.json not written: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("run.json is empty")
	}

	got, err := store.List()
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 live run, got %d", len(got))
	}
	if got[0].RunID != run.RunID || got[0].PID != run.PID || !reflect.DeepEqual(got[0].Issues, run.Issues) || !got[0].StartedAt.Equal(run.StartedAt) {
		t.Fatalf("List() mismatch: got %+v want %+v", got[0], run)
	}

	if err := store.Remove(run.RunID); err != nil {
		t.Fatalf("Remove() failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "runs", run.RunID)); !os.IsNotExist(err) {
		t.Fatalf("run dir should be removed, got err=%v", err)
	}
}
