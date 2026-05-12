package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// WorktreeSandbox provides isolation via git worktree only.
type WorktreeSandbox struct {
	repoPath     string
	worktreeBase string
	branch       string
	sourceBranch string
	workDir      string
	cmd          *exec.Cmd
}

// NewWorktreeSandbox creates a WorktreeSandbox for the given repo and branch.
func NewWorktreeSandbox(repoPath, worktreeBase, branch, sourceBranch string) *WorktreeSandbox {
	return &WorktreeSandbox{
		repoPath:     repoPath,
		worktreeBase: worktreeBase,
		branch:       branch,
		sourceBranch: sourceBranch,
	}
}

// Start initializes the worktree.
func (s *WorktreeSandbox) Start() error {
	if err := os.MkdirAll(s.worktreeBase, 0755); err != nil {
		return fmt.Errorf("create worktree base: %w", err)
	}

	s.workDir = filepath.Join(s.worktreeBase, s.branch)
	if _, err := os.Stat(s.workDir); err == nil {
		return nil
	}

	cmd := exec.Command("git", "worktree", "add", "-b", s.branch, s.workDir, s.sourceBranch)
	cmd.Dir = s.repoPath
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree add: %w\n%s", err, out)
	}
	return nil
}

// Exec runs a command in the worktree, writing stdout and stderr to the given writers.
func (s *WorktreeSandbox) Exec(ctx context.Context, command string, stdout, stderr io.Writer) error {
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = s.workDir
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("exec start: %w", err)
	}
	s.cmd = cmd

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("exec: %w", err)
		}
		return nil
	case <-ctx.Done():
		<-done
		return ctx.Err()
	}
}

// ExecInteractive runs a command in the worktree attached to the user's terminal.
func (s *WorktreeSandbox) ExecInteractive(ctx context.Context, command string) error {
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = s.workDir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("exec start: %w", err)
	}
	s.cmd = cmd

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("exec: %w", err)
		}
		return nil
	case <-ctx.Done():
		<-done
		return ctx.Err()
	}
}

// Stop cleans up the worktree.
func (s *WorktreeSandbox) Stop() error {
	cmd := exec.Command("git", "worktree", "remove", "--force", s.workDir)
	cmd.Dir = s.repoPath
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree remove: %w\n%s", err, out)
	}
	return nil
}

// WritePrompt writes the prompt content to .sandman/prompt.md in the worktree.
func (s *WorktreeSandbox) WritePrompt(content string) error {
	promptPath := filepath.Join(s.workDir, ".sandman", "prompt.md")
	if err := os.MkdirAll(filepath.Dir(promptPath), 0755); err != nil {
		return fmt.Errorf("create prompt dir: %w", err)
	}
	if err := os.WriteFile(promptPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("write prompt: %w", err)
	}
	return nil
}

// ReadRunResult reads .sandman/run-result.json from the worktree.
func (s *WorktreeSandbox) ReadRunResult() (*RunResult, error) {
	runResultPath := filepath.Join(s.workDir, ".sandman", "run-result.json")
	data, err := os.ReadFile(runResultPath)
	if err != nil {
		return nil, err
	}
	var rr RunResult
	if err := json.Unmarshal(data, &rr); err != nil {
		return nil, err
	}
	return &rr, nil
}

// WorkDir returns the working directory path of the sandbox.
func (s *WorktreeSandbox) WorkDir() string {
	return s.workDir
}

// Process returns the running OS process, or nil if no process is active.
func (s *WorktreeSandbox) Process() Process {
	if s.cmd == nil || s.cmd.Process == nil {
		return nil
	}
	return s.cmd.Process
}

// Ensure WorktreeSandbox implements Sandbox.
var _ Sandbox = (*WorktreeSandbox)(nil)
