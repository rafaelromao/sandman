package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/paths"
)

// WorktreeSandbox provides isolation via git worktree only.
type WorktreeSandbox struct {
	repoPath          string
	worktreeBase      string
	branch            string
	sourceBranch      string
	override          bool
	strandedReconcile bool
	workDir           string
	gitName           string
	gitEmail          string
	mu                sync.Mutex
	cmd               *exec.Cmd
	processWrapper    *processWrapper
	errorLog          io.Writer
}

// NewWorktreeSandbox creates a WorktreeSandbox for the given repo and branch.
func NewWorktreeSandbox(repoPath, worktreeBase, branch, sourceBranch string) *WorktreeSandbox {
	return &WorktreeSandbox{
		repoPath:          repoPath,
		worktreeBase:      worktreeBase,
		branch:            branch,
		sourceBranch:      sourceBranch,
		strandedReconcile: true,
	}
}

// SetOverride enables override behavior for orphan worktree recovery.
func (s *WorktreeSandbox) SetOverride(override bool) {
	s.override = override
}

// SetStrandedReconcile enables or disables auto-recovery from stranded
// worktrees during Start. When enabled (the default), a "git branch -D"
// failure with the "checked out at" error is recovered by either
// deleting the branch from a stranded worktree's cwd or by force-checking
// out the base branch in the main repo. When disabled, the original
// error surfaces unchanged.
func (s *WorktreeSandbox) SetStrandedReconcile(enabled bool) {
	s.strandedReconcile = enabled
}

// Start initializes the worktree.
func (s *WorktreeSandbox) Start() error {
	s.workDir = filepath.Join(s.worktreeBase, s.branch)
	overrideRecreate := false
	if s.workDirIsValidWorktree() {
		currentRef, err := CurrentBranchRef(s.workDir)
		if err != nil {
			if !s.override {
				return fmt.Errorf("worktree at %q HEAD is not on a branch: %w; re-run with --override to reconcile", s.workDir, err)
			}
			overrideRecreate = true
			goto overrideCleanup
		}
		expectedRef := "refs/heads/" + s.branch
		if currentRef == expectedRef {
			if !s.override {
				return s.configureGitIdentity()
			}
			overrideRecreate = true
		} else {
			if !s.override {
				return fmt.Errorf("worktree at %q is on branch %q, expected %q; re-run with --override to reconcile",
					s.workDir, strings.TrimPrefix(currentRef, "refs/heads/"), s.branch)
			}
			overrideRecreate = true
		}
	}
	if !s.override && s.strandedReconcile {
		if _, reclaimable := ReclaimableWorktree(s.repoPath, s.worktreeBase, s.branch); reclaimable {
			if s.workDirExists() {
				gitlinkPath := filepath.Join(s.workDir, ".git")
				data, err := os.ReadFile(gitlinkPath)
				if err == nil {
					content := strings.TrimSpace(string(data))
					const prefix = "gitdir: "
					if strings.HasPrefix(content, prefix) {
						gitdir := strings.TrimSpace(strings.TrimPrefix(content, prefix))
						if !filepath.IsAbs(gitdir) {
							gitdir = filepath.Join(s.workDir, gitdir)
						}
						if _, err := os.Stat(gitdir); err != nil {
							worktreeDirName := filepath.Base(s.workDir)
							correctGitdir := filepath.Join(s.repoPath, ".git", "worktrees", worktreeDirName)
							if info, err := os.Stat(correctGitdir); err == nil && info.IsDir() {
								if err := os.WriteFile(gitlinkPath, []byte("gitdir: "+correctGitdir+"\n"), 0644); err != nil {
									return fmt.Errorf("fix broken gitlink: %w", err)
								}
								return s.configureGitIdentity()
							}
						}
					}
				}
			}
			pruneCmd := exec.Command("git", "worktree", "prune")
			pruneCmd.Dir = s.repoPath
			if out, err := pruneCmd.CombinedOutput(); err != nil {
				return fmt.Errorf("git worktree prune: %w\n%s", err, out)
			}
			if s.workDirExists() {
				addCmd := exec.Command("git", "worktree", "add", "-f", s.workDir, s.branch)
				addCmd.Dir = s.repoPath
				if out, err := addCmd.CombinedOutput(); err != nil {
					return fmt.Errorf("git worktree add (reattach): %w\n%s", err, out)
				}
			} else {
				addCmd := exec.Command("git", "worktree", "add", s.workDir, s.branch)
				addCmd.Dir = s.repoPath
				if out, err := addCmd.CombinedOutput(); err != nil {
					return fmt.Errorf("git worktree add (recreate): %w\n%s", err, out)
				}
			}
			return s.configureGitIdentity()
		}
	}
overrideCleanup:
	if s.override {
		// Capture stranded-worktree state up front so the recovery
		// loop can attempt the `git -C <strandedPath> branch -D`
		// strategy from inside the worktree's cwd before the
		// override block prunes the worktree registration.
		strandedPath := ""
		if s.strandedReconcile {
			if info, stranded := StrandedWorktree(s.repoPath, s.worktreeBase, s.branch); stranded {
				strandedPath = info.Path
			}
		}

		if strandedPath != "" {
			delCmd := exec.Command("git", "branch", "-D", s.branch)
			delCmd.Dir = strandedPath
			if out, err := delCmd.CombinedOutput(); err == nil {
				strandedPath = ""
			} else if !isBranchCheckedOutError(out) {
				return fmt.Errorf("delete branch from stranded worktree at %q: %w\n%s", strandedPath, err, out)
			}
		}

		// Issue #2140: also clear any foreign live worktree at a different
		// path that currently holds s.branch. Without this, `git branch -D`
		// at line 174 below fails with "checked out at '<path>'" and the
		// fallback reconcileStrandedBranch cannot free the branch because
		// it force-checks out sourceBranch in repoPath, not in the foreign
		// worktree.
		if s.strandedReconcile {
			if info, foreign := ForeignStrandedWorktree(s.repoPath, s.worktreeBase, s.branch); foreign {
				if err := detachForeignWorktree(info.Path, s.branch); err != nil {
					s.warn("issue #2140: detach foreign worktree at %q: %v\n", info.Path, err)
				}
			}
		}

		if overrideRecreate {
			removeCmd := exec.Command("git", "worktree", "remove", "--force", s.workDir)
			removeCmd.Dir = s.repoPath
			if out, err := removeCmd.CombinedOutput(); err != nil {
				return fmt.Errorf("remove stale worktree: %w\n%s", err, out)
			}
		} else if s.workDirExists() {
			if err := os.RemoveAll(s.workDir); err != nil {
				return fmt.Errorf("clean forced worktree dir: %w", err)
			}
		}
		pruneCmd := exec.Command("git", "worktree", "prune")
		pruneCmd.Dir = s.repoPath
		if out, err := pruneCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("prune stale worktree registration: %w\n%s", err, out)
		}
		if BranchExists(s.repoPath, s.branch) {
			delCmd := exec.Command("git", "branch", "-D", s.branch)
			delCmd.Dir = s.repoPath
			if out, err := delCmd.CombinedOutput(); err != nil {
				if s.strandedReconcile && isBranchCheckedOutError(out) {
					// Issue #2140: parse the offending path from the error
					// message and attempt to detach the foreign worktree
					// directly before falling back to the main-repo
					// force-checkout strategy. The fallback below only
					// recovers the case where the branch is checked out
					// in repoPath itself; foreign worktrees require the
					// explicit detach.
					if foreignPath, ok := parseCheckedOutPath(out); ok && foreignPath != "" {
						if detachErr := detachForeignWorktree(foreignPath, s.branch); detachErr == nil {
							retryCmd := exec.Command("git", "branch", "-D", s.branch)
							retryCmd.Dir = s.repoPath
							if retryOut, retryErr := retryCmd.CombinedOutput(); retryErr == nil {
								goto afterBranchDelete
							} else {
								out = retryOut
								err = retryErr
							}
						} else {
							s.warn("issue #2140: detach foreign worktree at %q failed: %v\n", foreignPath, detachErr)
						}
					}
					if recErr := s.reconcileStrandedBranch(); recErr != nil {
						return fmt.Errorf("delete stale branch %q: %w\n%s", s.branch, recErr, out)
					}
				} else {
					return fmt.Errorf("delete stale branch %q: %w\n%s", s.branch, err, out)
				}
			}
		}
	}
afterBranchDelete:
	if s.workDirExists() {
		// Directory exists on disk but is not a registered git worktree.
		// This can happen when a previous run crashed after the directory
		// was created but before `git worktree add` finished registering
		// it. `git rev-parse --git-dir` from such a dir walks up to the
		// parent repo's `.git`, so we cannot use that to detect the orphan
		// state — instead we check for the `.git` file that a real worktree
		// has. See #545.
		if err := os.RemoveAll(s.workDir); err != nil {
			return fmt.Errorf("clean orphan worktree dir: %w", err)
		}
	}

	if err := os.MkdirAll(s.worktreeBase, 0755); err != nil {
		return fmt.Errorf("create worktree base: %w", err)
	}

	if !s.override && BranchExists(s.repoPath, s.branch) && !s.workDirExists() {
		if s.strandedReconcile {
			headRef, headErr := CurrentBranchRef(s.repoPath)
			if headErr == nil && headRef == "refs/heads/"+s.branch {
				return fmt.Errorf(`branch %q already exists — delete it with "git branch -D %s" and re-run`, s.branch, s.branch)
			}
			delCmd := exec.Command("git", "branch", "-D", s.branch)
			delCmd.Dir = s.repoPath
			if out, err := delCmd.CombinedOutput(); err != nil {
				return fmt.Errorf(`delete stale branch %q: %w\n%s`, s.branch, err, out)
			}
			pruneCmd := exec.Command("git", "worktree", "prune")
			pruneCmd.Dir = s.repoPath
			if out, err := pruneCmd.CombinedOutput(); err != nil {
				return fmt.Errorf("git worktree prune: %w\n%s", err, out)
			}
		} else {
			return fmt.Errorf(`branch %q already exists — delete it with "git branch -D %s" and re-run`, s.branch, s.branch)
		}
	}

	addCmd := exec.Command("git", "worktree", "add", "-b", s.branch, s.workDir, s.sourceBranch)
	addCmd.Dir = s.repoPath
	if out, err := addCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree add: %w\n%s", err, out)
	}
	return s.configureGitIdentity()
}

// workDirIsValidWorktree reports whether s.workDir is a registered git worktree.
// A worktree has a `.git` file (not directory) at its root pointing to the
// real git dir. A regular subdir of the parent repo has no `.git` at all.
func (s *WorktreeSandbox) workDirIsValidWorktree() bool {
	info, err := os.Stat(s.workDir)
	if err != nil || !info.IsDir() {
		return false
	}
	gitPath := filepath.Join(s.workDir, ".git")
	info, err = os.Stat(gitPath)
	if err != nil {
		return false
	}
	if !info.IsDir() {
		data, err := os.ReadFile(gitPath)
		if err != nil {
			return false
		}
		content := strings.TrimSpace(string(data))
		const prefix = "gitdir: "
		if !strings.HasPrefix(content, prefix) {
			return false
		}
		gitDir := strings.TrimSpace(strings.TrimPrefix(content, prefix))
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(s.workDir, gitDir)
		}
		if info, err := os.Stat(gitDir); err != nil || !info.IsDir() {
			return false
		}
		return true
	}
	return false
}

// CurrentBranchRef returns the full ref that HEAD points to in the given worktree.
func CurrentBranchRef(workDir string) (string, error) {
	out, err := runGitCommand(workDir, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve HEAD symbolic-ref: %w\n%s", err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// forceCheckoutBranch runs git checkout -f in the given workdir to switch to branch.
func forceCheckoutBranch(workDir, branch string) error {
	out, err := runGitCommand(workDir, "checkout", "-f", branch)
	if err != nil {
		return fmt.Errorf("git checkout -f %s: %w\n%s", branch, err, out)
	}
	return nil
}

// warn writes a warning line to the operator log (s.errorLog or os.Stderr).
func (s *WorktreeSandbox) warn(format string, args ...interface{}) {
	w := s.errorLog
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintf(w, "warning: "+format, args...)
}

// workDirExists reports whether s.workDir is an existing directory.
func (s *WorktreeSandbox) workDirExists() bool {
	info, err := os.Stat(s.workDir)
	return err == nil && info.IsDir()
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
			return fmt.Errorf("sync base branch %q: create local branch: %w\n%s", sourceBranch, updateErr, out)
		}
		return nil
	}

	// Local is at or ahead of remote — nothing to do.
	if ok, err := gitMergeBaseIsAncestor(repoPath, remoteHash, localHash); err != nil {
		return fmt.Errorf("sync base branch %q: check if remote is ancestor of local: %w", sourceBranch, err)
	} else if ok {
		return nil
	}

	// Local is behind remote — fast-forward.
	if ok, err := gitMergeBaseIsAncestor(repoPath, localHash, remoteHash); err != nil {
		return fmt.Errorf("sync base branch %q: check if local is ancestor of remote: %w", sourceBranch, err)
	} else if ok {
		if out, err := runGitCommand(repoPath, "update-ref", localRef, remoteHash, localHash); err != nil {
			return fmt.Errorf("sync base branch %q: fast-forward: %w\n%s", sourceBranch, err, out)
		}
		return nil
	}

	if out, err := runGitCommand(repoPath, "update-ref", localRef, remoteHash); err != nil {
		return fmt.Errorf("sync base branch %q: diverged, reset to remote: %w\n%s", sourceBranch, err, out)
	}
	return nil
}

func gitMergeBaseIsAncestor(dir, a, b string) (bool, error) {
	cmd := exec.Command("git", "merge-base", "--is-ancestor", a, b)
	cmd.Dir = dir
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

// GitMergeBaseIsAncestor is the exported wrapper around
// gitMergeBaseIsAncestor. The four-oracle chain in `internal/batch`
// uses it from the T2 pre-filter; the original lowercase helper stays
// for internal callers that already shell out from the sandbox
// package.
func GitMergeBaseIsAncestor(dir, a, b string) (bool, error) {
	return gitMergeBaseIsAncestor(dir, a, b)
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

// BranchExists reports whether the given branch already exists in refs/heads of the repo at repoPath.
func BranchExists(repoPath, branch string) bool {
	_, err := gitRevParse(repoPath, "--verify", "refs/heads/"+branch)
	return err == nil
}

// IsGitDir reports whether the given path is inside a valid git directory
// (worktree or bare repo). It runs `git rev-parse --git-dir` and checks
// whether the command succeeds, which is the canonical way to test this.
func IsGitDir(path string) bool {
	_, err := gitRevParse(path, "--git-dir")
	return err == nil
}

// Exec runs a command in the worktree, writing stdout and stderr to the given writers.
func (s *WorktreeSandbox) Exec(ctx context.Context, command string, stdout, stderr io.Writer) error {
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = s.workDir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("exec start: %w", err)
	}
	s.mu.Lock()
	s.cmd = cmd
	s.processWrapper = newProcessWrapper(cmd)
	s.mu.Unlock()

	if err := waitCmd(ctx, cmd, s.processWrapper, nil); err != nil {
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
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("exec start: %w", err)
	}
	s.mu.Lock()
	s.cmd = cmd
	s.processWrapper = newProcessWrapper(cmd)
	s.mu.Unlock()

	if err := waitCmd(ctx, cmd, s.processWrapper, nil); err != nil {
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

// WritePrompt writes the prompt content to .sandman/task.md in the worktree.
func (s *WorktreeSandbox) WritePrompt(content string) error {
	promptPath := filepath.Join(paths.NewLayout(&config.Config{}, s.workDir).SandmanDir, "task.md")
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

// RepoPath returns the parent repository path that owns this sandbox.
func (s *WorktreeSandbox) RepoPath() string {
	return s.repoPath
}

// Process returns the running OS process, or nil if no process is active.
func (s *WorktreeSandbox) Process() Process {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == nil || s.cmd.Process == nil {
		return nil
	}
	if s.processWrapper == nil {
		s.processWrapper = newProcessWrapper(s.cmd)
	}
	return s.processWrapper
}

// Ensure WorktreeSandbox implements Sandbox.
var _ Sandbox = (*WorktreeSandbox)(nil)

// reconcileStrandedFn is the function variable seam for the stranded-worktree
// recovery loop. It is invoked by WorktreeSandbox.Start when the initial
// `git branch -D` fails with a "checked out at" error and reconciliation is
// enabled. The default implementation calls the real recovery logic;
// tests can substitute a stub to record the call and return success
// without spawning a real git repo. See ADR-0027 for the pattern.
var reconcileStrandedFn = func(repoPath, worktreeBase, branch, sourceBranch string) error {
	return defaultReconcileStrandedBranch(repoPath, worktreeBase, branch, sourceBranch)
}

// defaultReconcileStrandedBranch is the default implementation of the
// recovery loop. It force-checks out sourceBranch in repoPath and
// retries the branch delete. Exposed as a free function so the
// function-variable seam can be substituted independently of the
// receiver-bound method on WorktreeSandbox.
func defaultReconcileStrandedBranch(repoPath, worktreeBase, branch, sourceBranch string) error {
	checkoutCmd := exec.Command("git", "checkout", "-f", sourceBranch)
	checkoutCmd.Dir = repoPath
	if out, err := checkoutCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("checkout -f %s in main repo: %w\n%s", sourceBranch, err, out)
	}
	delCmd := exec.Command("git", "branch", "-D", branch)
	delCmd.Dir = repoPath
	if out, err := delCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("delete stale branch %q after force-checkout: %w\n%s", branch, err, out)
	}
	return nil
}

// isBranchCheckedOutError reports whether the given git output indicates
// the branch cannot be deleted because it is checked out somewhere
// (e.g. the main repo or another worktree). Matches both "Cannot delete
// branch 'X' checked out at '...'" and the older "cannot delete branch
// 'X' used by worktree at '...'" wording.
func isBranchCheckedOutError(out []byte) bool {
	msg := string(out)
	return strings.Contains(msg, "checked out at") || strings.Contains(msg, "used by worktree at")
}

// parseCheckedOutPath extracts the absolute worktree path from a
// `git branch -D` failure of the form
//
//	error: Cannot delete branch 'X' checked out at '<path>'
//	error: cannot delete branch 'X' used by worktree at '<path>'
//
// Returns the parsed path and true on success; empty string and false
// when the output does not match either form. The path is returned
// verbatim (single quotes stripped) so callers can pass it directly to
// `git worktree remove --force` or `git -C <path> checkout --detach`.
//
// Issue #2140: the override path uses this helper to recover from
// foreign worktrees that hold the target branch.
func parseCheckedOutPath(out []byte) (string, bool) {
	msg := string(out)
	for _, prefix := range []string{"checked out at '", "used by worktree at '"} {
		idx := strings.Index(msg, prefix)
		if idx < 0 {
			continue
		}
		start := idx + len(prefix)
		end := strings.IndexByte(msg[start:], '\'')
		if end < 0 {
			continue
		}
		return msg[start : start+end], true
	}
	return "", false
}

// detachForeignWorktree releases `branch` from a live worktree at `path` so
// the branch becomes deletable from the main repo. Both `git worktree remove
// --force` (the preferred, simpler path) and the fallback `git -C <path>
// checkout --detach` are attempted in order; the first that succeeds wins.
//
// Returns nil on success or a non-nil error that wraps the underlying git
// output from the last attempted strategy. The caller is expected to retry
// `git branch -D <branch>` afterwards. Idempotent: a no-op when the worktree
// at `path` no longer exists.
//
// Issue #2140.
func detachForeignWorktree(path, branch string) error {
	if path == "" {
		return fmt.Errorf("detach foreign worktree: empty path")
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat foreign worktree %q: %w", path, err)
	}
	removeCmd := exec.Command("git", "worktree", "remove", "--force", path)
	removeCmd.Dir = filepath.Dir(path)
	if out, err := removeCmd.CombinedOutput(); err == nil {
		return nil
	} else if !isBranchCheckedOutError(out) && !strings.Contains(string(out), "contains a repository") {
		// Hard failure on remove — fall through to detach only when the
		// error pattern suggests the worktree cannot be removed cleanly
		// (e.g. untracked files would be lost) but the branch itself
		// can still be freed via `checkout --detach`.
		return fmt.Errorf("git worktree remove --force %q: %w\n%s", path, err, out)
	}
	detachCmd := exec.Command("git", "checkout", "--detach")
	detachCmd.Dir = path
	if out, err := detachCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("detach HEAD in foreign worktree %q: %w\n%s", path, err, out)
	}
	return nil
}

// reconcileStrandedBranch attempts to remove the stale branch after the
// initial "git branch -D" failed because the branch is checked out
// somewhere. The caller has already attempted the stranded-worktree
// strategy (delete from the stranded worktree's cwd) and removed any
// stale worktree registration; the recovery path here is therefore
// the "main repo on the branch" case, dispatched through the
// package-level reconcileStrandedFn seam (see ADR-0027).
//
// Returns nil on success, or a non-nil error on hard failure.
func (s *WorktreeSandbox) reconcileStrandedBranch() error {
	return reconcileStrandedFn(s.repoPath, s.worktreeBase, s.branch, s.sourceBranch)
}
