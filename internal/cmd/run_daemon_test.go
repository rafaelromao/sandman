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

func TestRun_CreatesControlSocketInRunDir(t *testing.T) {
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

	runsDir := filepath.Join(sandmanDir, "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		t.Fatalf("read runs dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 run dir, got %d", len(entries))
	}

	sockPath := filepath.Join(runsDir, entries[0].Name(), "run.sock")
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

func TestRun_RemovesRunDirOnCompletion(t *testing.T) {
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

	runsDir := filepath.Join(sandmanDir, "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read runs dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected run dirs to be cleaned up, got %d", len(entries))
	}
}

func TestRun_AllowsConcurrentRuns(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	sandmanDir := filepath.Join(dir, ".sandman")
	if err := os.MkdirAll(sandmanDir, 0755); err != nil {
		t.Fatal(err)
	}

	release := make(chan struct{})
	runner1 := &blockedBatchRunner{
		started: make(chan struct{}),
		release: release,
		result:  &batch.Result{},
	}
	runner2 := &blockedBatchRunner{
		started: make(chan struct{}),
		release: release,
		result:  &batch.Result{},
	}

	done := make(chan error, 2)
	startRun := func(issue string, deps Dependencies) {
		go func() {
			var buf bytes.Buffer
			cmd := NewRunCmd(deps)
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			cmd.SetArgs([]string{issue})
			done <- cmd.Execute()
		}()
	}

	startRun("42", newRunDeps(runner1))
	<-runner1.started

	startRun("43", newRunDeps(runner2))
	<-runner2.started

	runsDir := filepath.Join(sandmanDir, "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		t.Fatalf("read runs dir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 run dirs for concurrent runs, got %d", len(entries))
	}

	close(release)

	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for run to finish")
		}
	}
}

func TestRun_RemovesSocketAndRunDirOnError(t *testing.T) {
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

	runsDir := filepath.Join(sandmanDir, "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read runs dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected run dirs to be cleaned up after error, got %d", len(entries))
	}
}
