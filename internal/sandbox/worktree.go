package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// WorktreeSandbox provides isolation via git worktree only.
type WorktreeSandbox struct {
	repoPath     string
	worktreeBase string
	branch       string
	sourceBranch string
	workDir      string
	gitName      string
	gitEmail     string
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
	s.workDir = filepath.Join(s.worktreeBase, s.branch)
	if _, err := os.Stat(s.workDir); err == nil {
		return s.configureGitIdentity()
	}

	if err := os.MkdirAll(s.worktreeBase, 0755); err != nil {
		return fmt.Errorf("create worktree base: %w", err)
	}

	if _, err := gitRevParse(s.repoPath, "--verify", "refs/heads/"+s.branch); err == nil {
		return fmt.Errorf(`branch %q already exists — delete it with "git branch -D %s" and re-run`, s.branch, s.branch)
	}

	addCmd := exec.Command("git", "worktree", "add", "-b", s.branch, s.workDir, s.sourceBranch)
	addCmd.Dir = s.repoPath
	if out, err := addCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree add: %w\n%s", err, out)
	}
	return s.configureGitIdentity()
}

// SetGitIdentity configures the identity Sandman should write to worktree-local git config.
func (s *WorktreeSandbox) SetGitIdentity(name, email string) {
	s.gitName = name
	s.gitEmail = email
}

func (s *WorktreeSandbox) configureGitIdentity() error {
	if strings.TrimSpace(s.gitName) == "" || strings.TrimSpace(s.gitEmail) == "" {
		return nil
	}
	if out, err := runGitCommand(s.workDir, "config", "--worktree", "user.name", s.gitName); err != nil {
		return fmt.Errorf("set worktree git user.name: %w\n%s", err, out)
	}
	if out, err := runGitCommand(s.workDir, "config", "--worktree", "user.email", s.gitEmail); err != nil {
		return fmt.Errorf("set worktree git user.email: %w\n%s", err, out)
	}
	return nil
}

// SyncBaseBranch fast-forwards the local base branch from origin.
func SyncBaseBranch(repoPath, sourceBranch string) error {
	if out, err := runGitCommand(repoPath, "fetch", "origin", sourceBranch); err != nil {
		return fmt.Errorf("sync base branch %q: %w\n%s", sourceBranch, err, out)
	}

	remoteRef := "refs/remotes/origin/" + sourceBranch
	localRef := "refs/heads/" + sourceBranch

	remoteHash, err := gitRevParse(repoPath, remoteRef)
	if err != nil {
		return fmt.Errorf("sync base branch %q: resolve %s: %w", sourceBranch, remoteRef, err)
	}

	localHash, err := gitRevParse(repoPath, "--verify", localRef)
	if err != nil {
		if out, updateErr := runGitCommand(repoPath, "update-ref", localRef, remoteHash); updateErr != nil {
			return fmt.Errorf("sync default branch %q: %w\n%s", sourceBranch, updateErr, out)
		}
		return nil
	}

	if out, err := runGitCommand(repoPath, "merge-base", "--is-ancestor", localHash, remoteHash); err != nil {
		return fmt.Errorf("sync default branch %q: %w\n%s", sourceBranch, err, out)
	}

	if out, err := runGitCommand(repoPath, "update-ref", localRef, remoteHash, localHash); err != nil {
		return fmt.Errorf("sync default branch %q: %w\n%s", sourceBranch, err, out)
	}

	return nil
}

func runGitCommand(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

func gitRevParse(dir string, args ...string) (string, error) {
	out, err := runGitCommand(dir, append([]string{"rev-parse"}, args...)...)
	if err != nil {
		return "", fmt.Errorf("%w\n%s", err, out)
	}
	return strings.TrimSpace(string(out)), nil
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

	if err := waitCmd(ctx, cmd); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	return nil
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

	if err := waitCmd(ctx, cmd); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	return nil
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

// WritePrompt writes the prompt content to .sandman/rendered-prompt.md in the worktree.
func (s *WorktreeSandbox) WritePrompt(content string) error {
	promptPath := filepath.Join(s.workDir, ".sandman", "rendered-prompt.md")
	if err := os.MkdirAll(filepath.Dir(promptPath), 0755); err != nil {
		return fmt.Errorf("create prompt dir: %w", err)
	}
	if err := os.WriteFile(promptPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("write prompt: %w", err)
	}
	return nil
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
