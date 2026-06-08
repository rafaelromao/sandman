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
