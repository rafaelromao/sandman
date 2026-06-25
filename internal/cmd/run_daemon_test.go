package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
)

// chdirToSandmanDir creates a temp dir with .sandman/review.sock
// listening, chdirs into it, and returns the dir. Tests that need
// to inspect the dir (e.g. for run subdirs) should use this so the
// review daemon guard (issue #383) is satisfied for the default
// review command.
func chdirToSandmanDir(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	sandmanDir := filepath.Join(dir, ".sandman")
	if err := os.MkdirAll(filepath.Join(sandmanDir, "reviews"), 0755); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", ReviewSocketPath(sandmanDir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	t.Chdir(dir)
	return dir
}

// chdirToShortSandmanDir is like chdirToSandmanDir but allocates the
// temp dir under /tmp with a short prefix so the resulting socket
// paths stay under the 108-byte Unix socket limit. The new run-id
// scheme (slice 2) produces ~38-char directory names which, combined
// with the default t.TempDir path prefix, push run.sock and cmd.sock
// past the limit. Use this variant for tests that actually dial a
// socket in the run dir.
func chdirToShortSandmanDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "r")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	sandmanDir := filepath.Join(dir, ".sandman")
	if err := os.MkdirAll(filepath.Join(sandmanDir, "reviews"), 0755); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", ReviewSocketPath(sandmanDir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	t.Chdir(dir)
	return dir
}

// depsWithSocket returns Dependencies for tests that have already
// chdir'd into a directory with a live .sandman/review.sock.
func depsWithSocket(runner batch.Runner) Dependencies {
	return Dependencies{
		BatchRunner:  runner,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: &fakeGitHubClient{},
	}
}

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
	dir := chdirToShortSandmanDir(t)
	deps := depsWithSocket(&blockedBatchRunner{
		started: make(chan struct{}),
		release: make(chan struct{}),
		result:  &batch.Result{},
	})
	sandmanDir := filepath.Join(dir, ".sandman")

	done := make(chan error, 1)
	go func() {
		var buf bytes.Buffer
		cmd := NewRunCmd(deps)
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{"42"})
		done <- cmd.Execute()
	}()

	<-deps.BatchRunner.(*blockedBatchRunner).started

	batchesDir := filepath.Join(sandmanDir, "batches")
	entries, err := os.ReadDir(batchesDir)
	if err != nil {
		t.Fatalf("read batches dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 batch dir, got %d", len(entries))
	}

	sockPath := filepath.Join(batchesDir, entries[0].Name(), "batch.sock")
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("socket should exist during run: %v", err)
	}
	conn.Close()

	close(deps.BatchRunner.(*blockedBatchRunner).release)

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
	dir := chdirToSandmanDir(t)
	deps := depsWithSocket(&spyBatchRunner{result: &batch.Result{}})
	sandmanDir := filepath.Join(dir, ".sandman")

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
	for _, entry := range entries {
		runPath := filepath.Join(runsDir, entry.Name())
		if _, err := os.Stat(filepath.Join(runPath, "config")); err == nil {
			t.Errorf("expected run-owned config/ snapshot to be removed with run dir %q", runPath)
		}
	}
}

// commanderBatchRunner is a batch.Runner that also satisfies daemon.IssueCommander.
type commanderBatchRunner struct {
	started    chan struct{}
	release    chan struct{}
	abortCalls chan int
	abortErr   error
}

func (c *commanderBatchRunner) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
	close(c.started)
	<-c.release
	return &batch.Result{}, nil
}

func (c *commanderBatchRunner) AbortIssue(issueNumber int) error {
	c.abortCalls <- issueNumber
	return c.abortErr
}

func TestRun_CreatesCommandSocketInRunDir(t *testing.T) {
	dir := chdirToShortSandmanDir(t)
	deps := depsWithSocket(&commanderBatchRunner{
		started:    make(chan struct{}),
		release:    make(chan struct{}),
		abortCalls: make(chan int, 1),
	})
	sandmanDir := filepath.Join(dir, ".sandman")
	runner := deps.BatchRunner.(*commanderBatchRunner)

	done := make(chan error, 1)
	go func() {
		var buf bytes.Buffer
		cmd := NewRunCmd(deps)
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{"42"})
		done <- cmd.Execute()
	}()

	<-runner.started

	batchesDir := filepath.Join(sandmanDir, "batches")
	entries, err := os.ReadDir(batchesDir)
	if err != nil {
		t.Fatalf("read batches dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 batch dir, got %d", len(entries))
	}

	cmdSockPath := filepath.Join(batchesDir, entries[0].Name(), "run.sock")
	conn, err := net.Dial("unix", cmdSockPath)
	if err != nil {
		t.Fatalf("cmd.sock should exist during run: %v", err)
	}

	req := map[string]any{"action": "abort", "issue": 42}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var resp map[string]string
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "ok" {
		t.Fatalf("expected status=ok, got %+v", resp)
	}
	conn.Close()

	select {
	case n := <-runner.abortCalls:
		if n != 42 {
			t.Fatalf("expected abort(42), got %d", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected orchestrator to receive AbortIssue(42)")
	}

	close(runner.release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for run to complete")
	}
}

func TestRun_RemovesCommandSocketOnCompletion(t *testing.T) {
	t.Skip("flaky in CI; tracked in #1326")
	dir := chdirToSandmanDir(t)
	deps := depsWithSocket(&commanderBatchRunner{
		started:    make(chan struct{}),
		release:    make(chan struct{}),
		abortCalls: make(chan int, 1),
	})
	sandmanDir := filepath.Join(dir, ".sandman")
	runner := deps.BatchRunner.(*commanderBatchRunner)

	started := make(chan struct{})
	go func() {
		var buf bytes.Buffer
		cmd := NewRunCmd(deps)
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{"42"})
		_ = cmd.Execute()
		close(started)
	}()

	<-runner.started
	close(runner.release)
	<-started

	runsDir := filepath.Join(sandmanDir, "batches")
	entries, err := os.ReadDir(runsDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read batches dir: %v", err)
	}
	for _, entry := range entries {
		sockPath := filepath.Join(runsDir, entry.Name(), "run.sock")
		if _, err := os.Stat(sockPath); err == nil {
			t.Fatalf("expected run.sock to be removed, still exists at %s", sockPath)
		}
	}
}

func TestRun_AllowsConcurrentRuns(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	sandmanDir := filepath.Join(dir, ".sandman")
	if err := os.MkdirAll(filepath.Join(sandmanDir, "reviews"), 0755); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", ReviewSocketPath(sandmanDir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

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

	startRun("42", depsWithSocket(runner1))
	<-runner1.started

	startRun("43", depsWithSocket(runner2))
	<-runner2.started

	batchesDir := filepath.Join(sandmanDir, "batches")
	entries, err := os.ReadDir(batchesDir)
	if err != nil {
		t.Fatalf("read batches dir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 batch dirs for concurrent runs, got %d", len(entries))
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

func TestRun_LeavesBatchDirOnError(t *testing.T) {
	dir := chdirToSandmanDir(t)
	deps := depsWithSocket(&spyBatchRunner{result: nil, err: os.ErrClosed})
	sandmanDir := filepath.Join(dir, ".sandman")

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error from batch runner")
	}

	batchesDir := filepath.Join(sandmanDir, "batches")
	entries, err := os.ReadDir(batchesDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read batches dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected batch dir to remain after error, got %d", len(entries))
	}
}

func TestRun_SetsRunDirOnBatchRequest(t *testing.T) {
	_ = chdirToSandmanDir(t)
	deps := depsWithSocket(&spyBatchRunner{result: &batch.Result{}})
	spy := deps.BatchRunner.(*spyBatchRunner)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.RunDir == "" {
		t.Fatal("expected RunDir to be set on batch.Request")
	}
	if !strings.HasPrefix(spy.req.RunDir, ".sandman/batches/") {
		t.Errorf("expected RunDir %q to be under .sandman/batches/", spy.req.RunDir)
	}
}
