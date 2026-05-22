package cmd

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/daemon"
)

func TestAttach_NoLiveRunsReturnsError(t *testing.T) {
	var buf bytes.Buffer
	cmd := NewAttachCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when no live run exists")
	}
	if !strings.Contains(err.Error(), "no sandman daemon is running") {
		t.Fatalf("expected no-live-run error, got: %v", err)
	}
}

func TestAttach_TargetsRunID(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	store := daemon.NewLiveRunStore(filepath.Join(dir, ".sandman"))
	runID := "run-42-123"
	startLiveRun(t, store, runID, []int{42}, "hello from run 42")

	var buf bytes.Buffer
	cmd := NewAttachCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{runID})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if buf.String() != "hello from run 42" {
		t.Fatalf("expected attachment output, got %q", buf.String())
	}
}

func TestAttach_AutoAttachesSingleLiveRun(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	store := daemon.NewLiveRunStore(filepath.Join(dir, ".sandman"))
	runID := "run-43-123"
	startLiveRun(t, store, runID, []int{43}, "hello from single run")

	var buf bytes.Buffer
	cmd := NewAttachCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if buf.String() != "hello from single run" {
		t.Fatalf("expected attachment output, got %q", buf.String())
	}
}

func TestAttach_PromptsForMultipleLiveRuns(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	store := daemon.NewLiveRunStore(filepath.Join(dir, ".sandman"))
	startLiveRun(t, store, "run-41-123", []int{41}, "first run")
	startLiveRun(t, store, "run-42-123", []int{42}, "second run")

	var outBuf bytes.Buffer
	var errBuf bytes.Buffer
	cmd := NewAttachCmd()
	cmd.SetIn(strings.NewReader("2\n"))
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outBuf.String() != "second run" {
		t.Fatalf("expected second run output, got %q", outBuf.String())
	}
}

func startLiveRun(t *testing.T, store *daemon.LiveRunStore, runID string, issues []int, output string) {
	t.Helper()
	if err := store.Register(daemon.LiveRun{
		RunID:     runID,
		PID:       os.Getpid(),
		Issues:    append([]int(nil), issues...),
		StartedAt: time.Now().UTC().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("register live run: %v", err)
	}

	listener, err := net.Listen("unix", store.SocketPath(runID))
	if err != nil {
		t.Fatalf("listen on run socket: %v", err)
	}

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		_, _ = conn.Write([]byte(output))
		_ = conn.Close()
		_ = listener.Close()
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		_ = store.Remove(runID)
	})
}
