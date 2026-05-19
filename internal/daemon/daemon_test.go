package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestPIDLock_AcquireCreatesPIDFile(t *testing.T) {
	dir := t.TempDir()
	lock := NewPIDLock(dir)

	err := lock.Acquire()
	if err != nil {
		t.Fatalf("Acquire() failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "run.pid"))
	if err != nil {
		t.Fatalf("run.pid not created: %v", err)
	}

	pid := string(data)
	if pid == "" {
		t.Fatal("run.pid is empty")
	}
}

func TestPIDLock_AcquireFailsOnLivePID(t *testing.T) {
	dir := t.TempDir()
	livePID := os.Getpid()
	pidPath := filepath.Join(dir, "run.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(livePID)), 0644); err != nil {
		t.Fatal(err)
	}

	lock := NewPIDLock(dir)
	err := lock.Acquire()
	if err == nil {
		t.Fatal("expected error when lock held by live process")
	}
}

func TestPIDLock_AcquireCleansStalePID(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "run.pid")
	if err := os.WriteFile(pidPath, []byte("999999999"), 0644); err != nil {
		t.Fatal(err)
	}

	lock := NewPIDLock(dir)
	err := lock.Acquire()
	if err != nil {
		t.Fatalf("Acquire() should clean stale PID, got: %v", err)
	}

	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("run.pid should exist after acquire: %v", err)
	}
	if string(data) == "999999999" {
		t.Fatal("run.pid should have been overwritten with current PID")
	}
}

func TestPIDLock_ReleaseRemovesPIDFile(t *testing.T) {
	dir := t.TempDir()
	lock := NewPIDLock(dir)

	if err := lock.Acquire(); err != nil {
		t.Fatal(err)
	}

	if err := lock.Release(); err != nil {
		t.Fatalf("Release() failed: %v", err)
	}

	pidPath := filepath.Join(dir, "run.pid")
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatal("run.pid should be deleted after Release()")
	}
}

func TestPIDLock_ReleaseNoFile(t *testing.T) {
	dir := t.TempDir()
	lock := NewPIDLock(dir)

	err := lock.Release()
	if err != nil {
		t.Fatalf("Release() on non-existent file should succeed: %v", err)
	}
}
