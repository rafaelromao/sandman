package cmd

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
)

// blockedBatchRunner blocks RunBatch until released.
type blockedBatchRunner struct {
	started chan struct{}
	release chan struct{}
	result  *batch.Result
	err     error
}

func (b *blockedBatchRunner) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
	close(b.started)
	<-b.release
	return b.result, b.err
}

func TestRun_AcquiresPIDLock(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	sandmanDir := filepath.Join(dir, ".sandman")
	if err := os.MkdirAll(sandmanDir, 0755); err != nil {
		t.Fatal(err)
	}

	blocked := &blockedBatchRunner{
		started: make(chan struct{}),
		release: make(chan struct{}),
		result:  &batch.Result{},
	}
	deps := newRunDeps(blocked)

	done := make(chan error, 1)
	go func() {
		var buf bytes.Buffer
		cmd := NewRunCmd(deps)
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{"42"})
		done <- cmd.Execute()
	}()

	<-blocked.started

	pidPath := filepath.Join(sandmanDir, "run.pid")
	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("run.pid should exist during run: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("run.pid should contain PID")
	}

	close(blocked.release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for run to complete")
	}
}

func TestRun_ReleasesPIDLockOnCompletion(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	sandmanDir := filepath.Join(dir, ".sandman")
	if err := os.MkdirAll(sandmanDir, 0755); err != nil {
		t.Fatal(err)
	}

	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	pidPath := filepath.Join(sandmanDir, "run.pid")
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatal("run.pid should be deleted after run completes")
	}
}

func TestRun_CreatesControlSocket(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	sandmanDir := filepath.Join(dir, ".sandman")
	if err := os.MkdirAll(sandmanDir, 0755); err != nil {
		t.Fatal(err)
	}

	blocked := &blockedBatchRunner{
		started: make(chan struct{}),
		release: make(chan struct{}),
		result:  &batch.Result{},
	}
	deps := newRunDeps(blocked)

	done := make(chan error, 1)
	go func() {
		var buf bytes.Buffer
		cmd := NewRunCmd(deps)
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{"42"})
		done <- cmd.Execute()
	}()

	<-blocked.started

	sockPath := filepath.Join(sandmanDir, "run.sock")
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("socket should exist during run: %v", err)
	}
	conn.Close()

	close(blocked.release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for run to complete")
	}
}

func TestRun_ClosesControlSocketOnCompletion(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	sandmanDir := filepath.Join(dir, ".sandman")
	if err := os.MkdirAll(sandmanDir, 0755); err != nil {
		t.Fatal(err)
	}

	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sockPath := filepath.Join(sandmanDir, "run.sock")
	_, err = net.Dial("unix", sockPath)
	if err == nil {
		t.Fatal("socket should be closed after run completes")
	}
}

func TestRun_FailsOnExistingLiveDaemon(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	sandmanDir := filepath.Join(dir, ".sandman")
	if err := os.MkdirAll(sandmanDir, 0755); err != nil {
		t.Fatal(err)
	}

	pidPath := filepath.Join(sandmanDir, "run.pid")
	if err := os.WriteFile(pidPath, []byte("999999999"), 0644); err != nil {
		t.Fatal(err)
	}

	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.called {
		t.Fatal("expected batch runner to be called after stale PID cleanup")
	}
}

func TestRun_RemovesSocketAndPIDOnError(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	sandmanDir := filepath.Join(dir, ".sandman")
	if err := os.MkdirAll(sandmanDir, 0755); err != nil {
		t.Fatal(err)
	}

	spy := &spyBatchRunner{result: nil, err: os.ErrClosed}
	deps := newRunDeps(spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error from batch runner")
	}

	pidPath := filepath.Join(sandmanDir, "run.pid")
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatal("run.pid should be deleted even on error")
	}

	sockPath := filepath.Join(sandmanDir, "run.sock")
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Fatal("run.sock should be deleted even on error")
	}
}
