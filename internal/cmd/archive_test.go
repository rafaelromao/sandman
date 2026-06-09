package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/rafaelromao/sandman/internal/daemon"
)

func TestArchiveRun_NonexistentRunReturnsError(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"run", "missing-1"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for nonexistent run, got nil")
	}

	archiveDir := filepath.Join(dir, ".sandman", "archive")
	if _, err := os.Stat(archiveDir); !os.IsNotExist(err) {
		t.Errorf("expected archive dir to NOT be created when source does not exist, got stat err: %v", err)
	}
	if _, err := os.Stat(filepath.Join(archiveDir, "missing-1")); !os.IsNotExist(err) {
		t.Errorf("expected no archive/missing-1, got stat err: %v", err)
	}
}

func TestArchiveRun_LiveRunReturnsError(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	runDir := filepath.Join(dir, ".sandman", "runs", "live-1")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "batch.json"), []byte("{}"), 0644); err != nil {
		t.Fatalf("write batch.json: %v", err)
	}

	cmdServer := daemon.NewCommandServer(runDir, nil)
	if err := cmdServer.Start(); err != nil {
		t.Fatalf("start command server: %v", err)
	}
	defer cmdServer.Stop()

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"run", "live-1"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for live run, got nil")
	}

	if _, err := os.Stat(runDir); err != nil {
		t.Errorf("expected live run dir to be preserved on rejection, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "archive", "live-1")); !os.IsNotExist(err) {
		t.Errorf("expected no archive/live-1 after rejection, got: %v", err)
	}
}

func TestArchiveRun_DeadRunMovesDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	runDir := filepath.Join(dir, ".sandman", "runs", "dead-1")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "batch.json"), []byte(`{"issues":[42]}`), 0644); err != nil {
		t.Fatalf("write batch.json: %v", err)
	}

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"run", "dead-1"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	archiveRunDir := filepath.Join(dir, ".sandman", "archive", "dead-1")
	if _, err := os.Stat(archiveRunDir); err != nil {
		t.Fatalf("expected archived run dir to exist: %v", err)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Errorf("expected source run dir to be gone after archive, got: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(archiveRunDir, "batch.json"))
	if err != nil {
		t.Fatalf("read archived batch.json: %v", err)
	}
	if string(data) != `{"issues":[42]}` {
		t.Errorf("archived batch.json content = %q, want %q", string(data), `{"issues":[42]}`)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "archive")); err != nil {
		t.Errorf("expected .sandman/archive/ to exist after archive: %v", err)
	}
}

func TestArchiveRun_CollisionWithExistingArchiveDirReturnsError(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	runDir := filepath.Join(dir, ".sandman", "runs", "dead-2")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "batch.json"), []byte("source"), 0644); err != nil {
		t.Fatalf("write batch.json: %v", err)
	}

	existingArchive := filepath.Join(dir, ".sandman", "archive", "dead-2")
	if err := os.MkdirAll(existingArchive, 0755); err != nil {
		t.Fatalf("mkdir existing archive: %v", err)
	}
	if err := os.WriteFile(filepath.Join(existingArchive, "sentinel.txt"), []byte("preserved"), 0644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"run", "dead-2"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when destination already exists, got nil")
	}

	if _, err := os.Stat(runDir); err != nil {
		t.Errorf("expected source run dir preserved on collision, got: %v", err)
	}
	sentinel, err := os.ReadFile(filepath.Join(existingArchive, "sentinel.txt"))
	if err != nil {
		t.Fatalf("expected existing archive sentinel preserved, got: %v", err)
	}
	if string(sentinel) != "preserved" {
		t.Errorf("expected existing archive sentinel untouched, got %q", string(sentinel))
	}
}
