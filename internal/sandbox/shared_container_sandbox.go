package sandbox

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
)

// SharedContainerSandbox provides isolation via a shared container and a git worktree.
// The container lifecycle is managed by the caller (shared mode).
type SharedContainerSandbox struct {
	worktree  Sandbox
	container Container
	binary    string
	repoPath  string
	cmd       *exec.Cmd
	execFn    func(name string, arg ...string) *exec.Cmd
}

// NewSharedContainerSandbox creates a SharedContainerSandbox that borrows the given container.
func NewSharedContainerSandbox(worktree Sandbox, container Container, binary, repoPath string) *SharedContainerSandbox {
	return &SharedContainerSandbox{
		worktree:  worktree,
		container: container,
		binary:    binary,
		repoPath:  repoPath,
		execFn:    exec.Command,
	}
}

func (s *SharedContainerSandbox) containerWorkDir() string {
	wd := s.worktree.WorkDir()
	rel, err := filepath.Rel(s.repoPath, wd)
	if err != nil {
		return wd
	}
	return filepath.Join("/workspace", rel)
}

// Start initializes the worktree.
func (s *SharedContainerSandbox) Start() error {
	return s.worktree.Start()
}

// Exec runs a command inside the shared container, writing stdout and stderr to the given writers.
func (s *SharedContainerSandbox) Exec(ctx context.Context, command string, stdout, stderr io.Writer) error {
	cmd := s.execFn(s.binary, "exec", "-w", s.containerWorkDir(), s.container.ID(), "sh", "-c", command)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("container exec start: %w", err)
	}
	s.cmd = cmd

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("container exec: %w", err)
		}
		return nil
	case <-ctx.Done():
		<-done
		return ctx.Err()
	}
}

// Stop tears down only the worktree (the container is managed by the caller).
func (s *SharedContainerSandbox) Stop() error {
	return s.worktree.Stop()
}

// WorkDir returns the working directory path of the sandbox.
func (s *SharedContainerSandbox) WorkDir() string {
	return s.worktree.WorkDir()
}

// WritePrompt writes the prompt content to the sandbox.
func (s *SharedContainerSandbox) WritePrompt(content string) error {
	return s.worktree.WritePrompt(content)
}

// ReadRunResult reads the run result produced by the agent.
func (s *SharedContainerSandbox) ReadRunResult() (*RunResult, error) {
	return s.worktree.ReadRunResult()
}

// Process returns the running OS process, or nil if no process is active.
func (s *SharedContainerSandbox) Process() Process {
	if s.cmd == nil || s.cmd.Process == nil {
		return nil
	}
	return s.cmd.Process
}

// Ensure SharedContainerSandbox implements Sandbox.
var _ Sandbox = (*SharedContainerSandbox)(nil)
