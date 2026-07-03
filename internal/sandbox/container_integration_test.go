package sandbox

import (
	"bytes"
	"context"
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
	c, err := rt.Start(DefaultContainerImage, dir, StartOptions{})
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
	c, err := rt.Start(DefaultContainerImage, dir, StartOptions{})
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
	_, err := rt.Start("nonexistent-image-12345", dir, StartOptions{})
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
	c, err := rt.Start(DefaultContainerImage, dir, StartOptions{})
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
	c, err := rt.Start(DefaultContainerImage, dir, StartOptions{})
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
	c, err := rt.Start(DefaultContainerImage, dir, StartOptions{})
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
