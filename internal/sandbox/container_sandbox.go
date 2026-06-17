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
	"syscall"
)

// ContainerSandbox provides isolation via a container and a git worktree.
// In isolated mode each sandbox owns its container; in shared mode the container
// lifecycle is managed by the caller.
type ContainerSandbox struct {
	worktree       Sandbox
	container      Container
	binary         string
	repoPath       string
	ownsContainer  bool
	cmd            *exec.Cmd
	processWrapper *processWrapper
}

// ExecCommandFn is the function-variable seam for the *exec.Cmd factory used
// by ContainerSandbox.Exec and ContainerSandbox.ExecInteractive. The default
// delegates to exec.Command; tests may substitute a stub that returns a real
// long-running *exec.Cmd (e.g. `sleep 60`) so the production waitCmd path can
// be exercised end-to-end without a real container runtime. The pattern
// mirrors the unexported reconcileStrandedFn seam in worktree.go (ADR-0027),
// but is exported so cross-package tests (e.g. the orchestrator abort tests
// in package batch) can drive the waitCmd path. Save and restore around the
// test:
//
//	prev := sandbox.ExecCommandFn
//	defer func() { sandbox.ExecCommandFn = prev }()
//	sandbox.ExecCommandFn = func(name string, arg ...string) *exec.Cmd { ... }
var ExecCommandFn = exec.Command

// KillAgentFn is the function-variable seam for the container-side kill signal
// sent when an exec is aborted via context cancellation. The default reads the
// agent's pidfile (/tmp/agent-pgid) inside the container and sends SIGKILL to
// that process group via `docker exec <id> sh -c 'kill -KILL -<pgid>'`. Tests may
// substitute a stub to verify the call is made with the expected container ID.
// Save and restore around the test:
//
//	prev := sandbox.KillAgentFn
//	defer func() { sandbox.KillAgentFn = prev }()
//	sandbox.KillAgentFn = func(containerID string) error { ... }
var KillAgentFn = func(containerID string) error {
	cmd := exec.Command("docker", "exec", containerID, "sh", "-c",
		"pgid=$(cat /tmp/agent-pgid 2>/dev/null); [ -n \"$pgid\" ] && kill -KILL -\"$pgid\"")
	return cmd.Run()
}

// NewContainerSandbox creates a ContainerSandbox that owns the given container.
func NewContainerSandbox(worktree Sandbox, container Container, binary, repoPath string) *ContainerSandbox {
	return &ContainerSandbox{
		worktree:      worktree,
		container:     container,
		binary:        binary,
		repoPath:      repoPath,
		ownsContainer: true,
	}
}

// NewSharedContainerSandbox creates a SharedContainerSandbox that borrows the given container.
func NewSharedContainerSandbox(worktree Sandbox, container Container, binary, repoPath string) *ContainerSandbox {
	return &ContainerSandbox{
		worktree:      worktree,
		container:     container,
		binary:        binary,
		repoPath:      repoPath,
		ownsContainer: false,
	}
}

// SetGitIdentity forwards worktree-local git identity configuration to the underlying worktree.
func (s *ContainerSandbox) SetGitIdentity(name, email string) {
	s.worktree.SetGitIdentity(name, email)
}

// SetOverride forwards override behavior to the underlying worktree.
func (s *ContainerSandbox) SetOverride(override bool) {
	s.worktree.SetOverride(override)
}

// SetStrandedReconcile forwards stranded-worktree auto-recovery to the underlying worktree.
func (s *ContainerSandbox) SetStrandedReconcile(enabled bool) {
	s.worktree.SetStrandedReconcile(enabled)
}

func (s *ContainerSandbox) containerWorkDir() string {
	wd := s.worktree.WorkDir()
	rel, err := filepath.Rel(s.repoPath, wd)
	if err != nil {
		return wd
	}
	return filepath.Join("/workspace", rel)
}

// Start initializes the worktree and rewrites paths so git commands
// issued inside the container resolve correctly.
func (s *ContainerSandbox) Start() error {
	if err := s.worktree.Start(); err != nil {
		return err
	}
	return s.rewriteGitPaths()
}

// rewriteGitPaths rewrites the worktree's .git pointer so paths resolve
// inside the container (/workspace/...) instead of the host checkout.
func (s *ContainerSandbox) rewriteGitPaths() error {
	absRepo, err := filepath.Abs(s.repoPath)
	if err != nil {
		return nil
	}

	// Rewrite the worktree .git pointer so git resolves inside the container.
	gitFile := filepath.Join(s.worktree.WorkDir(), ".git")
	if data, readErr := os.ReadFile(gitFile); readErr == nil {
		updated := strings.Replace(string(data), absRepo, "/workspace", 1)
		if werr := os.WriteFile(gitFile, []byte(updated), 0644); werr != nil {
			return werr
		}
	}

	return nil
}

// RestoreHostPaths puts host-visible worktree metadata back after container use.
func (s *ContainerSandbox) RestoreHostPaths() error {
	return RestoreWorktreeGitPaths(s.repoPath, s.worktree.WorkDir())
}

// RestoreWorktreeGitPaths rewrites a preserved worktree's .git pointer back to host paths.
func RestoreWorktreeGitPaths(repoPath, worktreePath string) error {
	absRepo, err := filepath.Abs(repoPath)
	if err != nil {
		return nil
	}

	gitFile := filepath.Join(worktreePath, ".git")
	data, err := os.ReadFile(gitFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	updated := strings.Replace(string(data), "/workspace", absRepo, 1)
	if updated == string(data) {
		return nil
	}
	return os.WriteFile(gitFile, []byte(updated), 0644)
}

const execWrapperScript = `setsid sh -c 'echo \$\$ > /tmp/agent-pgid; exec %s' & SPID=$!; wait $SPID; rm -f /tmp/agent-pgid
`

// Exec runs a command inside the container, writing stdout and stderr to the given writers.
func (s *ContainerSandbox) Exec(ctx context.Context, command string, stdout, stderr io.Writer) error {
	wrapperCmd := fmt.Sprintf(execWrapperScript, command)
	cmd := ExecCommandFn(s.binary, "exec", "-it", "-w", s.containerWorkDir(), s.container.ID(), "sh", "-c", wrapperCmd)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("container exec start: %w", err)
	}
	s.cmd = cmd
	s.processWrapper = newProcessWrapper(cmd)

	onAbort := func() {
		_ = KillAgentFn(s.container.ID())
	}

	if err := waitCmd(ctx, cmd, s.processWrapper, onAbort); err != nil {
		return fmt.Errorf("container exec: %w", err)
	}
	return nil
}

// ExecInteractive runs a command inside the container attached to the user's terminal.
func (s *ContainerSandbox) ExecInteractive(ctx context.Context, command string) error {
	wrapperCmd := fmt.Sprintf(execWrapperScript, command)
	cmd := ExecCommandFn(s.binary, "exec", "-it", "-w", s.containerWorkDir(), s.container.ID(), "sh", "-c", wrapperCmd)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("exec start: %w", err)
	}
	s.cmd = cmd
	s.processWrapper = newProcessWrapper(cmd)

	onAbort := func() {
		_ = KillAgentFn(s.container.ID())
	}

	if err := waitCmd(ctx, cmd, s.processWrapper, onAbort); err != nil {
		return fmt.Errorf("container exec: %w", err)
	}
	return nil
}

// Stop tears down the worktree and, in isolated mode, the container.
func (s *ContainerSandbox) Stop() error {
	restoreErr := s.RestoreHostPaths()
	if s.ownsContainer {
		return errors.Join(restoreErr, s.container.Stop(), s.worktree.Stop())
	}
	return errors.Join(restoreErr, s.worktree.Stop())
}

// WorkDir returns the working directory path of the sandbox.
func (s *ContainerSandbox) WorkDir() string {
	return s.worktree.WorkDir()
}

// RepoPath returns the parent repository path that owns this sandbox.
func (s *ContainerSandbox) RepoPath() string {
	return s.repoPath
}

// WritePrompt writes the prompt content to the sandbox.
func (s *ContainerSandbox) WritePrompt(content string) error {
	return s.worktree.WritePrompt(content)
}

// Process returns the running OS process, or nil if no process is active.
func (s *ContainerSandbox) Process() Process {
	if s.cmd == nil || s.cmd.Process == nil {
		return nil
	}
	if s.processWrapper == nil {
		s.processWrapper = newProcessWrapper(s.cmd)
	}
	return s.processWrapper
}

// Ensure ContainerSandbox implements Sandbox.
var _ Sandbox = (*ContainerSandbox)(nil)
