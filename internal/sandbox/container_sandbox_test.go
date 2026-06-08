package sandbox

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"testing"
)

type fakeWorktreeForContainer struct {
	startCalled        bool
	startError         error
	stopCalled         bool
	stopError          error
	workDir            string
	writePromptCalled  bool
	writePromptContent string
	writePromptError   error
	execCalled         bool
	execCommand        string
	execError          error
	process            Process
	setForceCalled     bool
	setForceValue      bool
	setIdentityName    string
	setIdentityEmail   string
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

func (f *fakeWorktreeForContainer) SetForce(force bool) {
	f.setForceCalled = true
	f.setForceValue = force
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
	if captured[8] != "echo hello" {
		t.Errorf("expected command echo hello, got %q", captured[8])
	}
}

func TestContainerSandbox_Exec_ReturnsErrorOnStartFailure(t *testing.T) {
	wt := &fakeWorktreeForContainer{workDir: "/host/repo/.sandman/worktrees/branch"}
	ctr := &fakeContainer{id: "abc123"}
	sb := NewContainerSandbox(wt, ctr, "docker", "/host/repo")

	sb.execFn = func(name string, arg ...string) *exec.Cmd {
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
	sb.execFn = func(name string, arg ...string) *exec.Cmd {
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

	sb.execFn = func(name string, arg ...string) *exec.Cmd {
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

func TestContainerSandbox_SetForce_ForwardsToWorktree(t *testing.T) {
	wt := &fakeWorktreeForContainer{}
	ctr := &fakeContainer{id: "abc123"}
	sb := NewContainerSandbox(wt, ctr, "docker", "/host/repo")

	sb.SetForce(true)

	if !wt.setForceCalled {
		t.Fatal("expected worktree.SetForce to be called")
	}
	if !wt.setForceValue {
		t.Error("expected SetForce(true) to forward value true to worktree")
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

func TestSharedContainerSandbox_SetForce_ForwardsToWorktree(t *testing.T) {
	wt := &fakeWorktreeForContainer{}
	ctr := &fakeContainer{id: "shared123"}
	sb := NewSharedContainerSandbox(wt, ctr, "docker", "/host/repo")

	sb.SetForce(false)

	if !wt.setForceCalled {
		t.Fatal("expected worktree.SetForce to be called")
	}
	if wt.setForceValue {
		t.Error("expected SetForce(false) to forward value false to worktree")
	}
}
