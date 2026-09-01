package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/shellenv"
)

// TestContainerSandbox_Exec_KillAgentFnFailureWrapsCleanupError verifies the
// real wrapping path: when context is cancelled and KillAgentFn fails, Exec
// must return an error wrapping CleanupError so the batch layer can persist
// payload.cleanup_error. A regression that changes the fmt.Errorf wrapping or
// the error variable name would silently drop the field.
func TestContainerSandbox_Exec_KillAgentFnFailureWrapsCleanupError(t *testing.T) {
	if err := exec.Command("sleep", "0").Run(); err != nil {
		t.Skipf("sleep command not available: %v", err)
	}

	wt := &fakeWorktreeForContainer{workDir: "/host/repo/.sandman/worktrees/branch"}
	ctr := &fakeContainer{id: "cleanup-fail-123"}
	sb := NewContainerSandbox(wt, ctr, "docker", "/host/repo")

	prevExec := ExecCommandFn
	prevKill := KillAgentFn
	defer func() {
		ExecCommandFn = prevExec
		KillAgentFn = prevKill
	}()

	readyPath := filepath.Join(t.TempDir(), "child.ready")
	KillAgentFn = func(containerID, binary string) error {
		return fmt.Errorf("kill failed for %s", containerID)
	}
	ExecCommandFn = func(name string, arg ...string) *exec.Cmd {
		return exec.Command("sh", "-c", fmt.Sprintf("touch %s && sleep 5", shellenv.Quote(readyPath)))
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		errCh <- sb.Exec(ctx, "echo hello", io.Discard, io.Discard)
	}()

	waitForChildReadyTB(t, readyPath, 2*time.Second)
	cancel()

	select {
	case err := <-errCh:
		// Wait for goroutine to exit, but bound the wait so a regression that
		// leaves Exec hanging does not deadlock the test's cleanup.
		select {
		case <-doneCh:
		case <-time.After(5 * time.Second):
			t.Fatalf("Exec goroutine did not exit after returning")
		}
		if err == nil {
			t.Fatal("expected error from context cancellation with cleanup failure")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled in chain, got %v", err)
		}
		var ce *CleanupError
		if !errors.As(err, &ce) {
			t.Fatalf("expected CleanupError in chain, got %T: %v", err, err)
		}
		if ce.CleanupFail == nil || ce.CleanupFail.Error() == "" {
			t.Fatalf("expected non-empty CleanupFail, got %v", ce.CleanupFail)
		}
		if ce.Err == nil || !errors.Is(ce.Err, context.Canceled) {
			t.Fatalf("CleanupError.Err should wrap context.Canceled, got %v", ce.Err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Exec did not unblock after context cancel")
		// No wait for doneCh here; the test must fail independently without
		// blocking on the goroutine that is the regression being detected.
	}
}
