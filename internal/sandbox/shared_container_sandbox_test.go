package sandbox

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"testing"
)

func TestSharedContainerSandbox_Start_DelegatesToWorktree(t *testing.T) {
	wt := &fakeWorktreeForContainer{}
	ctr := &fakeContainer{id: "shared123"}
	sb := NewSharedContainerSandbox(wt, ctr, "docker", "/host/repo")

	if err := sb.Start(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !wt.startCalled {
		t.Error("expected worktree.Start to be called")
	}
}

func TestSharedContainerSandbox_Start_ReturnsWorktreeError(t *testing.T) {
	wt := &fakeWorktreeForContainer{startError: errors.New("worktree failed")}
	ctr := &fakeContainer{id: "shared123"}
	sb := NewSharedContainerSandbox(wt, ctr, "docker", "/host/repo")

	if err := sb.Start(); err == nil {
		t.Fatal("expected error when worktree.Start fails")
	}
}

func TestSharedContainerSandbox_Stop_DoesNotStopContainer(t *testing.T) {
	wt := &fakeWorktreeForContainer{}
	ctr := &fakeContainer{id: "shared123"}
	sb := NewSharedContainerSandbox(wt, ctr, "docker", "/host/repo")

	if err := sb.Stop(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ctr.stopCalled {
		t.Error("expected container.Stop NOT to be called in shared mode")
	}
	if !wt.stopCalled {
		t.Error("expected worktree.Stop to be called")
	}
}

func TestSharedContainerSandbox_Exec_RunsContainerExec(t *testing.T) {
	wt := &fakeWorktreeForContainer{workDir: "/host/repo/.sandman/worktrees/branch"}
	ctr := &fakeContainer{id: "shared123"}
	sb := NewSharedContainerSandbox(wt, ctr, "docker", "/host/repo")

	var captured []string
	sb.execFn = func(name string, arg ...string) *exec.Cmd {
		captured = append([]string{name}, arg...)
		return exec.Command("true")
	}

	if err := sb.Exec(context.Background(), "echo hello", io.Discard, io.Discard); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(captured) == 0 {
		t.Fatal("expected exec command to be built")
	}
	if captured[3] != "/workspace/.sandman/worktrees/branch" {
		t.Errorf("expected workdir /workspace/.sandman/worktrees/branch, got %q", captured[3])
	}
	if captured[4] != "shared123" {
		t.Errorf("expected container id shared123, got %q", captured[4])
	}
}
