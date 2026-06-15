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

// SetGitIdentity forwards worktree-local git identity configuration to the underlying worktree.
func (s *ContainerSandbox) SetGitIdentity(name, email string) {
	s.worktree.SetGitIdentity(name, email)
}

// SetOverride forwards override behavior to the underlying worktree.
func (s *ContainerSandbox) SetOverride(override bool) {
	s.worktree.SetOverride(override)
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

// Exec runs a command inside the container, writing stdout and stderr to the given writers.
func (s *ContainerSandbox) Exec(ctx context.Context, command string, stdout, stderr io.Writer) error {
	cmd := s.execFn(s.binary, "exec", "-it", "-w", s.containerWorkDir(), s.container.ID(), "sh", "-c", command)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("container exec start: %w", err)
	}
	s.cmd = cmd

	if err := waitCmd(ctx, cmd); err != nil {
		return fmt.Errorf("container exec: %w", err)
	}
	return nil
}

// ExecInteractive runs a command inside the container attached to the user's terminal.
func (s *ContainerSandbox) ExecInteractive(ctx context.Context, command string) error {
	cmd := s.execFn(s.binary, "exec", "-it", "-w", s.containerWorkDir(), s.container.ID(), "sh", "-c", command)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("container exec start: %w", err)
	}
	s.cmd = cmd

	if err := waitCmd(ctx, cmd); err != nil {
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
	return s.cmd.Process
}

// Ensure ContainerSandbox implements Sandbox.
var _ Sandbox = (*ContainerSandbox)(nil)
