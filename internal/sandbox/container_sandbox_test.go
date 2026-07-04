package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/shellenv"
)

type fakeWorktreeForContainer struct {
	startCalled                bool
	startError                 error
	stopCalled                 bool
	stopError                  error
	workDir                    string
	writePromptCalled          bool
	writePromptContent         string
	writePromptError           error
	execCalled                 bool
	execCommand                string
	execError                  error
	process                    Process
	setOverrideCalled          bool
	setOverrideValue           bool
	setStrandedReconcileCalled bool
	setStrandedReconcileValue  bool
	setIdentityName            string
	setIdentityEmail           string
}

func (f *fakeWorktreeForContainer) Start() error {
	f.startCalled = true
	return f.startError
}

func (f *fakeWorktreeForContainer) Stop() error {
	f.stopCalled = true
	return f.stopError
}

func (f *fakeWorktreeForContainer) WorkDir() string {
	return f.workDir
}

func (f *fakeWorktreeForContainer) RepoPath() string {
	return ""
}

func (f *fakeWorktreeForContainer) WritePrompt(content string) error {
	f.writePromptCalled = true
	f.writePromptContent = content
	return f.writePromptError
}

func (f *fakeWorktreeForContainer) Exec(ctx context.Context, command string, stdout, stderr io.Writer) error {
	f.execCalled = true
	f.execCommand = command
	return f.execError
}

func (f *fakeWorktreeForContainer) ExecInteractive(ctx context.Context, command string) error {
	f.execCalled = true
	f.execCommand = command
	return f.execError
}

func (f *fakeWorktreeForContainer) Process() Process {
	return f.process
}

func (f *fakeWorktreeForContainer) SetOverride(override bool) {
	f.setOverrideCalled = true
	f.setOverrideValue = override
}

func (f *fakeWorktreeForContainer) SetStrandedReconcile(enabled bool) {
	f.setStrandedReconcileCalled = true
	f.setStrandedReconcileValue = enabled
}

func (f *fakeWorktreeForContainer) SetGitIdentity(name, email string) {
	f.setIdentityName = name
	f.setIdentityEmail = email
}

// Ensure fakeWorktreeForContainer satisfies Sandbox.
var _ Sandbox = (*fakeWorktreeForContainer)(nil)

type fakeContainer struct {
	id         string
	stopCalled bool
	stopError  error
}

func (f *fakeContainer) ID() string {
	return f.id
}

func (f *fakeContainer) Stop() error {
	f.stopCalled = true
	return f.stopError
}

func TestContainerSandbox_Start_DelegatesToWorktree(t *testing.T) {
	wt := &fakeWorktreeForContainer{}
	ctr := &fakeContainer{id: "abc123"}
	sb := NewContainerSandbox(wt, ctr, "docker", "/host/repo")

	if err := sb.Start(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !wt.startCalled {
		t.Error("expected worktree.Start to be called")
	}
}

func TestContainerSandbox_Start_ReturnsWorktreeError(t *testing.T) {
	wt := &fakeWorktreeForContainer{startError: errors.New("worktree failed")}
	ctr := &fakeContainer{id: "abc123"}
	sb := NewContainerSandbox(wt, ctr, "docker", "/host/repo")

	if err := sb.Start(); err == nil {
		t.Fatal("expected error when worktree.Start fails")
	}
}

func TestContainerSandbox_Stop_StopsContainerAndWorktree(t *testing.T) {
	wt := &fakeWorktreeForContainer{}
	ctr := &fakeContainer{id: "abc123"}
	sb := NewContainerSandbox(wt, ctr, "docker", "/host/repo")

	if err := sb.Stop(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ctr.stopCalled {
		t.Error("expected container.Stop to be called")
	}
	if !wt.stopCalled {
		t.Error("expected worktree.Stop to be called")
	}
}

func TestContainerSandbox_Stop_ReturnsContainerError(t *testing.T) {
	wt := &fakeWorktreeForContainer{}
	ctr := &fakeContainer{id: "abc123", stopError: errors.New("container stop failed")}
	sb := NewContainerSandbox(wt, ctr, "docker", "/host/repo")

	if err := sb.Stop(); err == nil {
		t.Fatal("expected error when container.Stop fails")
	}
}

func TestContainerSandbox_WorkDir_DelegatesToWorktree(t *testing.T) {
	wt := &fakeWorktreeForContainer{workDir: "/host/repo/.sandman/worktrees/branch"}
	ctr := &fakeContainer{id: "abc123"}
	sb := NewContainerSandbox(wt, ctr, "docker", "/host/repo")

	if got := sb.WorkDir(); got != wt.workDir {
		t.Errorf("expected workdir %q, got %q", wt.workDir, got)
	}
}

func TestContainerSandbox_WritePrompt_DelegatesToWorktree(t *testing.T) {
	wt := &fakeWorktreeForContainer{}
	ctr := &fakeContainer{id: "abc123"}
	sb := NewContainerSandbox(wt, ctr, "docker", "/host/repo")

	if err := sb.WritePrompt("prompt content"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !wt.writePromptCalled {
		t.Error("expected worktree.WritePrompt to be called")
	}
	if wt.writePromptContent != "prompt content" {
		t.Errorf("expected prompt content %q, got %q", "prompt content", wt.writePromptContent)
	}
}

func TestContainerSandbox_Exec_RunsContainerExec(t *testing.T) {
	wt := &fakeWorktreeForContainer{workDir: "/host/repo/.sandman/worktrees/branch"}
	ctr := &fakeContainer{id: "abc123"}
	sb := NewContainerSandbox(wt, ctr, "docker", "/host/repo")

	var captured []string
	prev := ExecCommandFn
	defer func() { ExecCommandFn = prev }()
	ExecCommandFn = func(name string, arg ...string) *exec.Cmd {
		captured = append([]string{name}, arg...)
		return exec.Command("true")
	}

	if err := sb.Exec(context.Background(), "echo hello", io.Discard, io.Discard); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(captured) == 0 {
		t.Fatal("expected exec command to be built")
	}
	if captured[0] != "docker" {
		t.Errorf("expected binary docker, got %q", captured[0])
	}
	if captured[1] != "exec" {
		t.Errorf("expected subcommand exec, got %q", captured[1])
	}
	if captured[2] != "-it" {
		t.Errorf("expected flag -it, got %q", captured[2])
	}
	if captured[3] != "-w" {
		t.Errorf("expected flag -w, got %q", captured[3])
	}
	if captured[4] != "/workspace/.sandman/worktrees/branch" {
		t.Errorf("expected workdir /workspace/.sandman/worktrees/branch, got %q", captured[4])
	}
	if captured[5] != "abc123" {
		t.Errorf("expected container id abc123, got %q", captured[5])
	}
	if captured[6] != "sh" {
		t.Errorf("expected shell sh, got %q", captured[6])
	}
	if captured[7] != "-c" {
		t.Errorf("expected flag -c, got %q", captured[7])
	}
	if !strings.Contains(captured[8], "/tmp/agent-pgid") {
		t.Errorf("expected command to contain pidfile mechanism, got %q", captured[8])
	}
	if !strings.Contains(captured[8], "echo hello") {
		t.Errorf("expected command to contain original command echo hello, got %q", captured[8])
	}
}

func TestContainerSandbox_Exec_ReturnsErrorOnStartFailure(t *testing.T) {
	wt := &fakeWorktreeForContainer{workDir: "/host/repo/.sandman/worktrees/branch"}
	ctr := &fakeContainer{id: "abc123"}
	sb := NewContainerSandbox(wt, ctr, "docker", "/host/repo")

	prev := ExecCommandFn
	defer func() { ExecCommandFn = prev }()
	ExecCommandFn = func(name string, arg ...string) *exec.Cmd {
		return exec.Command("nonexistent-command-that-fails")
	}

	if err := sb.Exec(context.Background(), "echo hello", io.Discard, io.Discard); err == nil {
		t.Fatal("expected error when exec start fails")
	}
}

func TestContainerSandbox_Process_ReturnsNilWhenNoExec(t *testing.T) {
	wt := &fakeWorktreeForContainer{}
	ctr := &fakeContainer{id: "abc123"}
	sb := NewContainerSandbox(wt, ctr, "docker", "/host/repo")

	if sb.Process() != nil {
		t.Error("expected Process to be nil when no exec has run")
	}
}

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

func TestContainerSandbox_ExecInteractive_RunsContainerExec(t *testing.T) {
	wt := &fakeWorktreeForContainer{workDir: "/host/repo/.sandman/worktrees/branch"}
	ctr := &fakeContainer{id: "abc123"}
	sb := NewContainerSandbox(wt, ctr, "docker", "/host/repo")

	var captured []string
	prev := ExecCommandFn
	defer func() { ExecCommandFn = prev }()
	ExecCommandFn = func(name string, arg ...string) *exec.Cmd {
		captured = append([]string{name}, arg...)
		return exec.Command("true")
	}

	if err := sb.ExecInteractive(context.Background(), "echo hello"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(captured) == 0 {
		t.Fatal("expected exec command to be built")
	}
	if captured[0] != "docker" {
		t.Errorf("expected binary docker, got %q", captured[0])
	}
	if captured[1] != "exec" {
		t.Errorf("expected subcommand exec, got %q", captured[1])
	}
	if captured[2] != "-it" {
		t.Errorf("expected flag -it, got %q", captured[2])
	}
	if captured[3] != "-w" {
		t.Errorf("expected flag -w, got %q", captured[3])
	}
	if captured[4] != "/workspace/.sandman/worktrees/branch" {
		t.Errorf("expected workdir /workspace/.sandman/worktrees/branch, got %q", captured[4])
	}
	if captured[5] != "abc123" {
		t.Errorf("expected container id abc123, got %q", captured[5])
	}
}

func TestContainerSandbox_ExecInteractive_ReturnsErrorOnStartFailure(t *testing.T) {
	wt := &fakeWorktreeForContainer{workDir: "/host/repo/.sandman/worktrees/branch"}
	ctr := &fakeContainer{id: "abc123"}
	sb := NewContainerSandbox(wt, ctr, "docker", "/host/repo")

	prev := ExecCommandFn
	defer func() { ExecCommandFn = prev }()
	ExecCommandFn = func(name string, arg ...string) *exec.Cmd {
		return exec.Command("nonexistent-command-that-fails")
	}

	if err := sb.ExecInteractive(context.Background(), "echo hello"); err == nil {
		t.Fatal("expected error when exec start fails")
	}
}

func TestSharedContainerSandbox_Exec_RunsContainerExec(t *testing.T) {
	wt := &fakeWorktreeForContainer{workDir: "/host/repo/.sandman/worktrees/branch"}
	ctr := &fakeContainer{id: "shared123"}
	sb := NewSharedContainerSandbox(wt, ctr, "docker", "/host/repo")

	var captured []string
	prev := ExecCommandFn
	defer func() { ExecCommandFn = prev }()
	ExecCommandFn = func(name string, arg ...string) *exec.Cmd {
		captured = append([]string{name}, arg...)
		return exec.Command("true")
	}

	if err := sb.Exec(context.Background(), "echo hello", io.Discard, io.Discard); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(captured) == 0 {
		t.Fatal("expected exec command to be built")
	}
	if captured[2] != "-it" {
		t.Errorf("expected flag -it, got %q", captured[2])
	}
	if captured[3] != "-w" {
		t.Errorf("expected flag -w, got %q", captured[3])
	}
	if captured[4] != "/workspace/.sandman/worktrees/branch" {
		t.Errorf("expected workdir /workspace/.sandman/worktrees/branch, got %q", captured[4])
	}
	if captured[5] != "shared123" {
		t.Errorf("expected container id shared123, got %q", captured[5])
	}
}

func TestContainerSandbox_SetOverride_ForwardsToWorktree(t *testing.T) {
	wt := &fakeWorktreeForContainer{}
	ctr := &fakeContainer{id: "abc123"}
	sb := NewContainerSandbox(wt, ctr, "docker", "/host/repo")

	sb.SetOverride(true)

	if !wt.setOverrideCalled {
		t.Fatal("expected worktree.SetOverride to be called")
	}
	if !wt.setOverrideValue {
		t.Error("expected SetOverride(true) to forward value true to worktree")
	}
}

func TestContainerSandbox_SetGitIdentity_ForwardsToWorktree(t *testing.T) {
	wt := &fakeWorktreeForContainer{}
	ctr := &fakeContainer{id: "abc123"}
	sb := NewContainerSandbox(wt, ctr, "docker", "/host/repo")

	sb.SetGitIdentity("Sandman", "[email protected]")

	if wt.setIdentityName != "Sandman" {
		t.Errorf("expected worktree name %q, got %q", "Sandman", wt.setIdentityName)
	}
	if wt.setIdentityEmail != "[email protected]" {
		t.Errorf("expected worktree email %q, got %q", "[email protected]", wt.setIdentityEmail)
	}
}

func TestSharedContainerSandbox_SetOverride_ForwardsToWorktree(t *testing.T) {
	wt := &fakeWorktreeForContainer{}
	ctr := &fakeContainer{id: "shared123"}
	sb := NewSharedContainerSandbox(wt, ctr, "docker", "/host/repo")

	sb.SetOverride(false)

	if !wt.setOverrideCalled {
		t.Fatal("expected worktree.SetOverride to be called")
	}
	if wt.setOverrideValue {
		t.Error("expected SetOverride(false) to forward value false to worktree")
	}
}

func TestContainerSandbox_Exec_KillAgentFnCalledOnAbort(t *testing.T) {
	if err := exec.Command("sleep", "0").Run(); err != nil {
		t.Skipf("sleep command not available: %v", err)
	}

	wt := &fakeWorktreeForContainer{workDir: "/host/repo/.sandman/worktrees/branch"}
	ctr := &fakeContainer{id: "test-container-123"}
	sb := NewContainerSandbox(wt, ctr, "docker", "/host/repo")

	prevExec := ExecCommandFn
	prevKill := KillAgentFn
	defer func() {
		ExecCommandFn = prevExec
		KillAgentFn = prevKill
	}()

	readyPath := filepath.Join(t.TempDir(), "child.ready")
	var killCalls []string
	KillAgentFn = func(containerID string) error {
		killCalls = append(killCalls, containerID)
		return nil
	}
	ExecCommandFn = func(name string, arg ...string) *exec.Cmd {
		return exec.Command("sh", "-c", fmt.Sprintf("touch %s && sleep 60", shellenv.Quote(readyPath)))
	}

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- sb.Exec(ctx, "echo hello", io.Discard, io.Discard)
	}()

	waitForChildReadyTB(t, readyPath, 2*time.Second)
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error from context cancellation")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Exec did not unblock after context cancel")
	}

	if len(killCalls) != 1 {
		t.Fatalf("expected KillAgentFn to be called exactly once, got %d calls", len(killCalls))
	}
	if killCalls[0] != "test-container-123" {
		t.Errorf("expected containerID=test-container-123, got %q", killCalls[0])
	}
}

func TestContainerSandbox_Exec_CancelsViaContext(t *testing.T) {
	if err := exec.Command("sleep", "0").Run(); err != nil {
		t.Skipf("sleep command not available: %v", err)
	}

	wt := &fakeWorktreeForContainer{workDir: "/host/repo/.sandman/worktrees/branch"}
	ctr := &fakeContainer{id: "abc123"}
	sb := NewContainerSandbox(wt, ctr, "docker", "/host/repo")

	prev := ExecCommandFn
	defer func() { ExecCommandFn = prev }()
	readyPath := filepath.Join(t.TempDir(), "child.ready")
	ExecCommandFn = func(name string, arg ...string) *exec.Cmd {
		return exec.Command("sh", "-c", fmt.Sprintf("touch %s && sleep 60", shellenv.Quote(readyPath)))
	}

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- sb.Exec(ctx, "echo hello", io.Discard, io.Discard)
	}()

	waitForChildReadyTB(t, readyPath, 2*time.Second)
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error from context cancellation")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Exec did not unblock after context cancel — missing Setpgid on container sandbox?")
	}
}

func TestContainerSandbox_ExecInteractive_CancelsViaContext(t *testing.T) {
	if err := exec.Command("sleep", "0").Run(); err != nil {
		t.Skipf("sleep command not available: %v", err)
	}

	wt := &fakeWorktreeForContainer{workDir: "/host/repo/.sandman/worktrees/branch"}
	ctr := &fakeContainer{id: "abc123"}
	sb := NewContainerSandbox(wt, ctr, "docker", "/host/repo")

	prev := ExecCommandFn
	defer func() { ExecCommandFn = prev }()
	readyPath := filepath.Join(t.TempDir(), "child.ready")
	ExecCommandFn = func(name string, arg ...string) *exec.Cmd {
		return exec.Command("sh", "-c", fmt.Sprintf("touch %s && sleep 60", shellenv.Quote(readyPath)))
	}

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- sb.ExecInteractive(ctx, "echo hello")
	}()

	waitForChildReadyTB(t, readyPath, 2*time.Second)
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error from context cancellation")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ExecInteractive did not unblock after context cancel — missing Setpgid on container sandbox?")
	}
}

// waitForChildReadyTB polls for a child-process readiness marker
// file; fails the test on timeout. The container-exec tests inject
// an ExecCommandFn whose underlying command writes the marker
// before sleeping, so the marker observing the file means the OS
// has actually forked+exec'd the child — the same guarantee the
// previous fixed time.Sleep(200ms) provided, with a deadline
// instead of a wall-clock guess.
func waitForChildReadyTB(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for child readiness marker %s", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestContainerSandbox_SetOverride_FalseAlsoForwardsToWorktree(t *testing.T) {
	wt := &fakeWorktreeForContainer{}
	ctr := &fakeContainer{id: "abc123"}
	sb := NewContainerSandbox(wt, ctr, "docker", "/host/repo")

	sb.SetOverride(false)

	if !wt.setOverrideCalled {
		t.Fatal("expected worktree.SetOverride to be called")
	}
	if wt.setOverrideValue {
		t.Error("expected SetOverride(false) to forward value false to worktree")
	}
}
