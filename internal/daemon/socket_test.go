package daemon

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestControlSocket_StartCreatesSocket(t *testing.T) {
	dir := t.TempDir()
	sock := NewControlSocket(dir, NewBroadcaster())

	if err := sock.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer sock.Stop()

	info, err := net.Dial("unix", filepath.Join(dir, "batch.sock"))
	if err != nil {
		t.Fatalf("connect to socket: %v", err)
	}
	info.Close()
}

func TestControlSocket_StartSetsSocketMode0600(t *testing.T) {
	dir := t.TempDir()
	sock := NewControlSocket(dir, NewBroadcaster())

	if err := sock.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer sock.Stop()

	info, err := os.Stat(filepath.Join(dir, "batch.sock"))
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("socket mode = %o, want 0o600", got)
	}
}

func TestControlSocket_StartSetsRunDirMode0700(t *testing.T) {
	dir := t.TempDir()
	sock := NewControlSocket(dir, NewBroadcaster())

	if err := sock.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer sock.Stop()

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat run dir: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("run dir mode = %o, want 0o700", got)
	}
}

func TestControlSocket_CustomFilename(t *testing.T) {
	dir := t.TempDir()
	sock := NewControlSocketWithName(dir, "review.sock", NewBroadcaster())

	if err := sock.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer sock.Stop()

	info, err := net.Dial("unix", filepath.Join(dir, "review.sock"))
	if err != nil {
		t.Fatalf("connect to review.sock: %v", err)
	}
	info.Close()

	if _, err := os.Stat(filepath.Join(dir, "batch.sock")); err == nil {
		t.Fatalf("default batch.sock should not exist when custom name is used")
	}
}

func TestControlSocket_StartWithCustomNameSetsSocketMode0600(t *testing.T) {
	dir := t.TempDir()
	sock := NewControlSocketWithName(dir, "review.sock", NewBroadcaster())

	if err := sock.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer sock.Stop()

	info, err := os.Stat(filepath.Join(dir, "review.sock"))
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("socket mode = %o, want 0o600", got)
	}
}

func TestControlSocket_StopsAcceptingAfterClose(t *testing.T) {
	dir := t.TempDir()
	sock := NewControlSocket(dir, NewBroadcaster())

	if err := sock.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	if err := sock.Stop(); err != nil {
		t.Fatalf("Stop() failed: %v", err)
	}

	_, err := net.Dial("unix", filepath.Join(dir, "batch.sock"))
	if err == nil {
		t.Fatal("expected dial error after Stop()")
	}
}

func TestControlSocket_Stop_RemovesSocketFile(t *testing.T) {
	dir := t.TempDir()
	sock := NewControlSocket(dir, NewBroadcaster())

	if err := sock.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	sockPath := filepath.Join(dir, "batch.sock")
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("socket file should exist after Start(): %v", err)
	}

	if err := sock.Stop(); err != nil {
		t.Fatalf("Stop() failed: %v", err)
	}

	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Fatalf("socket file should not exist after Stop(), got err: %v", err)
	}
}

func TestControlSocket_RemovesStaleSocketOnStart(t *testing.T) {
	dir := t.TempDir()
	oldSock := NewControlSocket(dir, NewBroadcaster())
	if err := oldSock.Start(); err != nil {
		t.Fatal(err)
	}
	oldSock.Stop()

	// Start again — should remove stale socket
	newSock := NewControlSocket(dir, NewBroadcaster())
	if err := newSock.Start(); err != nil {
		t.Fatalf("Start() with stale socket should succeed: %v", err)
	}
	defer newSock.Stop()

	conn, err := net.Dial("unix", filepath.Join(dir, "batch.sock"))
	if err != nil {
		t.Fatalf("connect after restart: %v", err)
	}
	conn.Close()
}

func TestIsRunActive(t *testing.T) {
	dir := t.TempDir()
	if IsRunActive(dir) {
		t.Fatal("expected dir without sockets to be inactive")
	}

	sock := NewControlSocket(dir, NewBroadcaster())
	if err := sock.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	if !IsRunActive(dir) {
		t.Fatal("expected dir with live batch.sock to be active")
	}

	sock.Stop()

	if IsRunActive(dir) {
		t.Fatal("expected dir without sockets to be inactive after stop")
	}
}

func TestIsRunActive_ProbesPerRunSocket(t *testing.T) {
	dir := t.TempDir()
	runSockDir := filepath.Join(dir, "runs", "run-1")
	if err := os.MkdirAll(runSockDir, 0755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(runSockDir, "run.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	if !IsRunActive(dir) {
		t.Fatal("expected dir with live per-run socket to be active")
	}
}

func TestCleanupStaleRunSnapshots_RemovesOnlyInactive(t *testing.T) {
	baseDir := t.TempDir()
	batchesDir := filepath.Join(baseDir, "batches")
	if err := os.MkdirAll(batchesDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Inactive batch with config/
	inactive := filepath.Join(batchesDir, "inactive-1")
	if err := os.MkdirAll(filepath.Join(inactive, "config"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inactive, "config", "host.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inactive, "batch.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	// Active batch with config/
	active := filepath.Join(batchesDir, "active-1")
	if err := os.MkdirAll(filepath.Join(active, "config"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(active, "config", "host.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(active, "batch.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	ctlSock := NewControlSocket(active, NewBroadcaster())
	if err := ctlSock.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer ctlSock.Stop()

	// Inactive batch with manifest but no config/ (should be a no-op)
	manifestOnly := filepath.Join(batchesDir, "manifest-only")
	if err := os.MkdirAll(manifestOnly, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestOnly, "batch.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	removed, err := CleanupStaleRunSnapshots(baseDir)
	if err != nil {
		t.Fatalf("CleanupStaleRunSnapshots: %v", err)
	}
	if removed != 1 {
		t.Errorf("expected 1 inactive batch snapshot removed, got %d", removed)
	}

	if _, err := os.Stat(filepath.Join(inactive, "config")); !os.IsNotExist(err) {
		t.Errorf("expected inactive batch config/ to be removed, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(inactive, "batch.json")); err != nil {
		t.Errorf("expected inactive batch manifest to be preserved (active-set is only the config/ snapshot): %v", err)
	}

	if _, err := os.Stat(filepath.Join(active, "config")); err != nil {
		t.Errorf("expected active batch config/ to be preserved, got: %v", err)
	}

	if _, err := os.Stat(filepath.Join(manifestOnly, "batch.json")); err != nil {
		t.Errorf("expected manifest-only batch to be untouched: %v", err)
	}
}

func TestCleanupStaleRunSnapshots_MissingRunsDir(t *testing.T) {
	baseDir := t.TempDir()
	removed, err := CleanupStaleRunSnapshots(baseDir)
	if err != nil {
		t.Fatalf("CleanupStaleRunSnapshots: %v", err)
	}
	if removed != 0 {
		t.Errorf("expected 0 removed, got %d", removed)
	}
}

func TestFindDeadRunBatches_NoRunsDir(t *testing.T) {
	baseDir := t.TempDir()
	batches, err := FindDeadRunBatches(baseDir)
	if err != nil {
		t.Fatalf("FindDeadRunBatches: %v", err)
	}
	if len(batches) != 0 {
		t.Errorf("expected 0 dead batches, got %d", len(batches))
	}
}

func TestFindDeadRunBatches_LiveSocketExcluded(t *testing.T) {
	baseDir := t.TempDir()
	batchesDir := filepath.Join(baseDir, "batches")
	if err := os.MkdirAll(batchesDir, 0755); err != nil {
		t.Fatal(err)
	}

	active := filepath.Join(batchesDir, "active-1")
	if err := os.MkdirAll(active, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(active, "batch.json"), []byte(`{"issues":[42]}`), 0644); err != nil {
		t.Fatal(err)
	}
	ctlSock := NewControlSocket(active, NewBroadcaster())
	if err := ctlSock.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer ctlSock.Stop()

	batches, err := FindDeadRunBatches(baseDir)
	if err != nil {
		t.Fatalf("FindDeadRunBatches: %v", err)
	}
	if len(batches) != 0 {
		t.Errorf("expected active batch to be excluded, got %d dead batches", len(batches))
	}
	for _, b := range batches {
		if b.RunDir == active {
			t.Errorf("active batch %q should not appear in dead batches", b.RunDir)
		}
	}
}

func TestFindDeadRunBatches_DeadSocketIncluded(t *testing.T) {
	baseDir := t.TempDir()
	batchesDir := filepath.Join(baseDir, "batches")
	if err := os.MkdirAll(batchesDir, 0755); err != nil {
		t.Fatal(err)
	}

	dead := filepath.Join(batchesDir, "dead-1")
	if err := os.MkdirAll(dead, 0755); err != nil {
		t.Fatal(err)
	}
	manifestJSON := `{"issues":[101,202],"createdAt":"2026-06-08T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(dead, "batch.json"), []byte(manifestJSON), 0644); err != nil {
		t.Fatal(err)
	}

	batches, err := FindDeadRunBatches(baseDir)
	if err != nil {
		t.Fatalf("FindDeadRunBatches: %v", err)
	}
	if len(batches) != 1 {
		t.Fatalf("expected 1 dead batch, got %d", len(batches))
	}
	got := batches[0]
	if got.RunDir != dead {
		t.Errorf("RunDir = %q, want %q", got.RunDir, dead)
	}
	if len(got.Manifest.Issues) != 2 || got.Manifest.Issues[0] != 101 || got.Manifest.Issues[1] != 202 {
		t.Errorf("Manifest.Issues = %v, want [101 202]", got.Manifest.Issues)
	}
}

func TestFindDeadRunBatches_NoManifestFile(t *testing.T) {
	baseDir := t.TempDir()
	batchesDir := filepath.Join(baseDir, "batches")
	if err := os.MkdirAll(batchesDir, 0755); err != nil {
		t.Fatal(err)
	}

	dead := filepath.Join(batchesDir, "no-manifest")
	if err := os.MkdirAll(dead, 0755); err != nil {
		t.Fatal(err)
	}

	batches, err := FindDeadRunBatches(baseDir)
	if err != nil {
		t.Fatalf("FindDeadRunBatches: %v", err)
	}
	if len(batches) != 1 {
		t.Fatalf("expected 1 dead batch (dead dir without manifest), got %d", len(batches))
	}
	got := batches[0]
	if got.RunDir != dead {
		t.Errorf("RunDir = %q, want %q", got.RunDir, dead)
	}
	if len(got.Manifest.Issues) != 0 {
		t.Errorf("expected zero-value BatchManifest.Issues, got %v", got.Manifest.Issues)
	}
	if !got.Manifest.CreatedAt.IsZero() {
		t.Errorf("expected zero-value CreatedAt, got %v", got.Manifest.CreatedAt)
	}
}

func TestFindDeadRunBatches_MultipleDeadBatches(t *testing.T) {
	baseDir := t.TempDir()
	batchesDir := filepath.Join(baseDir, "batches")
	if err := os.MkdirAll(batchesDir, 0755); err != nil {
		t.Fatal(err)
	}

	deadA := filepath.Join(batchesDir, "alpha")
	if err := os.MkdirAll(deadA, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deadA, "batch.json"), []byte(`{"issues":[1]}`), 0644); err != nil {
		t.Fatal(err)
	}

	live := filepath.Join(batchesDir, "bravo")
	if err := os.MkdirAll(live, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "batch.json"), []byte(`{"issues":[2]}`), 0644); err != nil {
		t.Fatal(err)
	}
	ctlSock := NewControlSocket(live, NewBroadcaster())
	if err := ctlSock.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer ctlSock.Stop()

	deadC := filepath.Join(batchesDir, "charlie")
	if err := os.MkdirAll(deadC, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deadC, "batch.json"), []byte(`{"issues":[3,4]}`), 0644); err != nil {
		t.Fatal(err)
	}

	batches, err := FindDeadRunBatches(baseDir)
	if err != nil {
		t.Fatalf("FindDeadRunBatches: %v", err)
	}
	if len(batches) != 2 {
		t.Fatalf("expected 2 dead batches, got %d", len(batches))
	}
	if batches[0].RunDir != deadA {
		t.Errorf("batches[0].RunDir = %q, want %q (sorted lexicographically)", batches[0].RunDir, deadA)
	}
	if batches[1].RunDir != deadC {
		t.Errorf("batches[1].RunDir = %q, want %q (sorted lexicographically)", batches[1].RunDir, deadC)
	}
	if len(batches[0].Manifest.Issues) != 1 || batches[0].Manifest.Issues[0] != 1 {
		t.Errorf("batches[0].Manifest.Issues = %v, want [1]", batches[0].Manifest.Issues)
	}
	if len(batches[1].Manifest.Issues) != 2 || batches[1].Manifest.Issues[0] != 3 || batches[1].Manifest.Issues[1] != 4 {
		t.Errorf("batches[1].Manifest.Issues = %v, want [3 4]", batches[1].Manifest.Issues)
	}
	for _, b := range batches {
		if b.RunDir == live {
			t.Errorf("live batch %q should not appear in dead batches", live)
		}
	}
}
