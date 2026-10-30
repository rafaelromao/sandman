package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var podmanWarmupOnce sync.Once

func podmanAvailable(t *testing.T) bool {
	t.Helper()
	cmd := exec.Command("podman", "version")
	if err := cmd.Run(); err != nil {
		t.Skip("podman not available")
		return false
	}
	podmanWarmupOnce.Do(func() {
		// Warm up Podman to avoid first-container delays in CI.
		_ = exec.Command("podman", "run", "--rm", DefaultContainerImage, "echo", "ok").Run()
	})
	return true
}

func waitForContainer(t *testing.T, id string) {
	t.Helper()
	for i := 0; i < 600; i++ {
		cmd := exec.Command("podman", "inspect", "-f", "{{.State.Running}}", id)
		out, err := cmd.CombinedOutput()
		if err == nil && strings.TrimSpace(string(out)) == "true" {
			return
		}
		if err != nil && strings.Contains(string(out), "No such container") {
			t.Fatalf("container %s was removed before becoming ready", id)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("container did not become ready")
}

func TestContainerRuntime_Start_CreatesRunningContainer(t *testing.T) {
	if !podmanAvailable(t) {
		return
	}
	dir := t.TempDir()
	rt := NewContainerRuntime("podman")
	c, err := rt.Start(t.Context(), DefaultContainerImage, dir, StartOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer c.Stop()
	waitForContainer(t, c.ID())

	verifyCmd := exec.Command("podman", "inspect", "-f", "{{.State.Running}}", c.ID())
	out, err := verifyCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("container not running: %v", err)
	}
	if strings.TrimSpace(string(out)) != "true" {
		t.Fatalf("expected container to be running, got %q", out)
	}
}

func TestContainerRuntime_Start_MountsRepo(t *testing.T) {
	if !podmanAvailable(t) {
		return
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	rt := NewContainerRuntime("podman")
	c, err := rt.Start(t.Context(), DefaultContainerImage, dir, StartOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer c.Stop()
	waitForContainer(t, c.ID())

	execCmd := exec.Command("podman", "exec", c.ID(), "cat", "/workspace/marker.txt")
	out, err := execCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != "hello" {
		t.Errorf("expected hello, got %q", out)
	}
}

func TestContainerRuntime_Start_ReturnsErrorForInvalidImage(t *testing.T) {
	if !podmanAvailable(t) {
		return
	}
	dir := t.TempDir()
	rt := NewContainerRuntime("podman")
	_, err := rt.Start(t.Context(), "nonexistent-image-12345", dir, StartOptions{})
	if err == nil {
		t.Fatal("expected error for invalid image")
	}
}

func TestContainer_Stop_StopsContainer(t *testing.T) {
	if !podmanAvailable(t) {
		return
	}
	dir := t.TempDir()
	rt := NewContainerRuntime("podman")
	c, err := rt.Start(t.Context(), DefaultContainerImage, dir, StartOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = c.Stop() })
	waitForContainer(t, c.ID())

	if err := c.Stop(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	verifyCmd := exec.Command("podman", "inspect", "-f", "{{.State.Running}}", c.ID())
	out, err := verifyCmd.CombinedOutput()
	if err != nil {
		return // container removed, which means it stopped
	}
	if strings.TrimSpace(string(out)) == "true" {
		t.Error("expected container to be stopped")
	}
}

func TestContainerSandbox_Exec_Integration(t *testing.T) {
	if !podmanAvailable(t) {
		return
	}
	dir := t.TempDir()
	rt := NewContainerRuntime("podman")
	c, err := rt.Start(t.Context(), DefaultContainerImage, dir, StartOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer c.Stop()

	waitForContainer(t, c.ID())

	wt := &fakeWorktreeForContainer{workDir: dir}
	sb := NewContainerSandbox(wt, c, "podman", dir)

	var out bytes.Buffer
	if err := sb.Exec(context.Background(), "echo hello from container", &out, io.Discard); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "hello from container") {
		t.Errorf("expected output to contain hello, got %q", out.String())
	}
}

func TestSharedContainerSandbox_Exec_Integration(t *testing.T) {
	if !podmanAvailable(t) {
		return
	}
	dir := t.TempDir()
	rt := NewContainerRuntime("podman")
	c, err := rt.Start(t.Context(), DefaultContainerImage, dir, StartOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer c.Stop()

	waitForContainer(t, c.ID())

	wt := &fakeWorktreeForContainer{workDir: dir}
	sb := NewSharedContainerSandbox(wt, c, "podman", dir)

	var out bytes.Buffer
	if err := sb.Exec(context.Background(), "echo hello from shared", &out, io.Discard); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "hello from shared") {
		t.Errorf("expected output to contain hello, got %q", out.String())
	}
}

// Aborting one agent exec must stop its whole process group inside a real
// container, under the image's own shell, and leave a sibling run's agent in
// the same container running.
func TestSharedContainerSandbox_Exec_AbortKillsOnlyItsAgent_Integration(t *testing.T) {
	if !podmanAvailable(t) {
		return
	}
	dir := t.TempDir()
	rt := NewContainerRuntime("podman")
	c, err := rt.Start(t.Context(), DefaultContainerImage, dir, StartOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer c.Stop()
	waitForContainer(t, c.ID())

	type agentExec struct {
		cancel context.CancelFunc
		done   chan error
		agent  string
	}
	var started []*agentExec
	defer func() {
		for _, e := range started {
			e.cancel()
			select {
			case <-e.done:
			case <-time.After(30 * time.Second):
				t.Error("Exec did not return after cancel")
			}
		}
	}()
	start := func(name, seconds string) *agentExec {
		t.Helper()
		workDir := filepath.Join(dir, name)
		if err := os.MkdirAll(workDir, 0755); err != nil {
			t.Fatal(err)
		}
		sb := NewSharedContainerSandbox(&fakeWorktreeForContainer{workDir: workDir}, c, "podman", dir)
		ctx, cancel := context.WithCancel(context.Background())
		e := &agentExec{cancel: cancel, done: make(chan error, 1), agent: "sleep " + seconds}
		started = append(started, e)
		go func() { e.done <- sb.Exec(ctx, e.agent+" & wait", io.Discard, io.Discard) }()
		waitForContainerProcess(t, c.ID(), e.agent, true)
		return e
	}
	abort := func(e *agentExec) {
		t.Helper()
		e.cancel()
		select {
		case err := <-e.done:
			e.done <- err
			var cleanupErr *CleanupError
			if !errors.Is(err, context.Canceled) || errors.As(err, &cleanupErr) {
				t.Fatalf("expected a clean cancellation, got: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("Exec did not return after cancel")
		}
		waitForContainerProcess(t, c.ID(), e.agent, false)
	}

	a, b := start("a", "301"), start("b", "302")
	abort(a)
	waitForContainerProcess(t, c.ID(), b.agent, true)
	abort(b)
}

// waitForContainerProcess waits until a process whose command line is args
// runs inside the container (want) or none does. The exec script carries the
// agent command as an argument, so only an exact command line identifies the
// agent itself.
func waitForContainerProcess(t *testing.T, id, args string, want bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		out, err := exec.Command("podman", "exec", id, "ps").CombinedOutput()
		if err != nil {
			t.Fatalf("list container processes: %v\n%s", err, out)
		}
		found := false
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasSuffix(strings.TrimSpace(line), " "+args) {
				found = true
			}
		}
		if found == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("container process %q present=%t, want %t:\n%s", args, !want, want, out)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
