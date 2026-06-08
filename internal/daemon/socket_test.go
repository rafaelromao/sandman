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

	info, err := net.Dial("unix", filepath.Join(dir, "run.sock"))
	if err != nil {
		t.Fatalf("connect to socket: %v", err)
	}
	info.Close()
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

	_, err := net.Dial("unix", filepath.Join(dir, "run.sock"))
	if err == nil {
		t.Fatal("expected dial error after Stop()")
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

	conn, err := net.Dial("unix", filepath.Join(dir, "run.sock"))
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

	cmdServer := NewCommandServer(dir, nil)
	if err := cmdServer.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer cmdServer.Stop()

	if !IsRunActive(dir) {
		t.Fatal("expected dir with live cmd.sock to be active")
	}

	cmdServer.Stop()

	sock := NewControlSocket(dir, NewBroadcaster())
	if err := sock.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer sock.Stop()

	if !IsRunActive(dir) {
		t.Fatal("expected dir with live run.sock but no cmd.sock to be active (continue runs)")
	}
}

func TestCleanupStaleRunSnapshots_RemovesOnlyInactive(t *testing.T) {
	baseDir := t.TempDir()
	runsDir := filepath.Join(baseDir, "runs")
	if err := os.MkdirAll(runsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Inactive run with config/
	inactive := filepath.Join(runsDir, "inactive-1")
	if err := os.MkdirAll(filepath.Join(inactive, "config"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inactive, "config", "host.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inactive, "batch.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	// Active run with config/
	active := filepath.Join(runsDir, "active-1")
	if err := os.MkdirAll(filepath.Join(active, "config"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(active, "config", "host.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(active, "batch.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	cmdServer := NewCommandServer(active, nil)
	if err := cmdServer.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer cmdServer.Stop()

	// Inactive run with manifest but no config/ (should be a no-op)
	manifestOnly := filepath.Join(runsDir, "manifest-only")
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
		t.Errorf("expected 1 inactive run snapshot removed, got %d", removed)
	}

	if _, err := os.Stat(filepath.Join(inactive, "config")); !os.IsNotExist(err) {
		t.Errorf("expected inactive run config/ to be removed, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(inactive, "batch.json")); err != nil {
		t.Errorf("expected inactive run manifest to be preserved (active-set is only the config/ snapshot): %v", err)
	}

	if _, err := os.Stat(filepath.Join(active, "config")); err != nil {
		t.Errorf("expected active run config/ to be preserved, got: %v", err)
	}

	if _, err := os.Stat(filepath.Join(manifestOnly, "batch.json")); err != nil {
		t.Errorf("expected manifest-only run to be untouched: %v", err)
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
	runsDir := filepath.Join(baseDir, "runs")
	if err := os.MkdirAll(runsDir, 0755); err != nil {
		t.Fatal(err)
	}

	active := filepath.Join(runsDir, "active-1")
	if err := os.MkdirAll(active, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(active, "batch.json"), []byte(`{"issues":[42]}`), 0644); err != nil {
		t.Fatal(err)
	}
	cmdServer := NewCommandServer(active, nil)
	if err := cmdServer.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer cmdServer.Stop()

	batches, err := FindDeadRunBatches(baseDir)
	if err != nil {
		t.Fatalf("FindDeadRunBatches: %v", err)
	}
	if len(batches) != 0 {
		t.Errorf("expected active run to be excluded, got %d dead batches", len(batches))
	}
	for _, b := range batches {
		if b.RunDir == active {
			t.Errorf("active run %q should not appear in dead batches", b.RunDir)
		}
	}
}

func TestFindDeadRunBatches_DeadSocketIncluded(t *testing.T) {
	baseDir := t.TempDir()
	runsDir := filepath.Join(baseDir, "runs")
	if err := os.MkdirAll(runsDir, 0755); err != nil {
		t.Fatal(err)
	}

	dead := filepath.Join(runsDir, "dead-1")
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
	runsDir := filepath.Join(baseDir, "runs")
	if err := os.MkdirAll(runsDir, 0755); err != nil {
		t.Fatal(err)
	}

	dead := filepath.Join(runsDir, "no-manifest")
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
	runsDir := filepath.Join(baseDir, "runs")
	if err := os.MkdirAll(runsDir, 0755); err != nil {
		t.Fatal(err)
	}

	deadA := filepath.Join(runsDir, "alpha")
	if err := os.MkdirAll(deadA, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deadA, "batch.json"), []byte(`{"issues":[1]}`), 0644); err != nil {
		t.Fatal(err)
	}

	live := filepath.Join(runsDir, "bravo")
	if err := os.MkdirAll(live, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "batch.json"), []byte(`{"issues":[2]}`), 0644); err != nil {
		t.Fatal(err)
	}
	cmdServer := NewCommandServer(live, nil)
	if err := cmdServer.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer cmdServer.Stop()

	deadC := filepath.Join(runsDir, "charlie")
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
			t.Errorf("live run %q should not appear in dead batches", live)
		}
	}
}
