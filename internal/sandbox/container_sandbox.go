package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
)

// ContainerSandbox provides isolation via a container and a git worktree.
// In isolated mode each sandbox owns its container; in shared mode the container
// lifecycle is managed by the caller.
type ContainerSandbox struct {
	worktree      Sandbox
	container     Container
	binary        string
	repoPath      string
	ownsContainer bool
	cmd           *exec.Cmd
	execFn        func(name string, arg ...string) *exec.Cmd
}

// NewContainerSandbox creates a ContainerSandbox that owns the given container.
func NewContainerSandbox(worktree Sandbox, container Container, binary, repoPath string) *ContainerSandbox {
	return &ContainerSandbox{
		worktree:      worktree,
		container:     container,
		binary:        binary,
		repoPath:      repoPath,
		ownsContainer: true,
		execFn:        exec.Command,
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
		execFn:        exec.Command,
	}
}

func (s *ContainerSandbox) containerWorkDir() string {
	wd := s.worktree.WorkDir()
	rel, err := filepath.Rel(s.repoPath, wd)
	if err != nil {
		return wd
	}
	return filepath.Join("/workspace", rel)
}

// Start initializes the worktree.
func (s *ContainerSandbox) Start() error {
	return s.worktree.Start()
}

// Exec runs a command inside the container, writing stdout and stderr to the given writers.
func (s *ContainerSandbox) Exec(ctx context.Context, command string, stdout, stderr io.Writer) error {
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

// Stop tears down the worktree and, in isolated mode, the container.
func (s *ContainerSandbox) Stop() error {
	if s.ownsContainer {
		return errors.Join(s.container.Stop(), s.worktree.Stop())
	}
	return s.worktree.Stop()
}

// WorkDir returns the working directory path of the sandbox.
func (s *ContainerSandbox) WorkDir() string {
	return s.worktree.WorkDir()
}

// WritePrompt writes the prompt content to the sandbox.
func (s *ContainerSandbox) WritePrompt(content string) error {
	return s.worktree.WritePrompt(content)
}

// ReadRunResult reads the run result produced by the agent.
func (s *ContainerSandbox) ReadRunResult() (*RunResult, error) {
	return s.worktree.ReadRunResult()
}

// Process returns the running OS process, or nil if no process is active.
func (s *ContainerSandbox) Process() Process {
	if s.cmd == nil || s.cmd.Process == nil {
		return nil
	}
	return s.cmd.Process
}

// Ensure ContainerSandbox implements Sandbox.
var _ Sandbox = (*ContainerSandbox)(nil)
