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

	"github.com/rafaelromao/sandman/internal/atomicfs"
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
	continueRun       bool
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

// RestoreHostPaths is a no-op on a worktree-only sandbox: there is no
// /workspace-visible gitlink to restore. Container sandboxes override the
// behavior to rewrite the preserved worktree's .git pointer back to host
// paths. Issue #2189.
func (s *WorktreeSandbox) RestoreHostPaths() error {
	return nil
}

// Start initializes the worktree. opts carries the pre-Start configuration
// (override / continue / stranded-reconcile / git-identity) that used to
// ride on four independent setters; this method is the only configuration
// entry point.
func (s *WorktreeSandbox) Start(opts SandboxStart) error {
	s.override = opts.Override
	s.continueRun = opts.Continue
	s.strandedReconcile = opts.StrandedReconcile
	s.gitName = opts.Identity.Name
	s.gitEmail = opts.Identity.Email
	s.workDir = filepath.Join(s.worktreeBase, s.branch)
	if s.continueRun && !s.override {
		if err := RestoreWorktreeGitPaths(s.repoPath, s.workDir); err != nil {
			return fmt.Errorf("normalize preserved worktree gitlink for continuation: %w", err)
		}
		if state, actualRef := ContinuationWorktreeStatus(s.repoPath, s.worktreeBase, s.branch); state != ContinuationWorktreeReusable {
			if err := s.refuseContinuation(state, actualRef); err != nil {
				return err
			}
		}
	}
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
								if err := atomicfs.WriteAtomic(gitlinkPath, []byte("gitdir: "+correctGitdir+"\n"), 0644); err != nil {
									return fmt.Errorf("fix broken gitlink: %w", err)
								}
								return s.configureGitIdentity()
							}
						}
					}
				}
			}
			// Issue #2187: scoped reattach for the canonical `s.workDir`
			// only. We drop the registration for `<ownWorkdir>` (the
			// canonical path), removing the worktree directory, then
			// re-register via `git worktree add`. No global
			// `git worktree prune` (which would also drop sibling
			// registrations owned by parallel sandbox runs sharing
			// the same `.git`).
			if err := s.removePrunableWorktreeRegistration(); err != nil {
				return fmt.Errorf("drop canonical worktree registration: %w", err)
			}
			if s.workDirExists() {
				if err := os.RemoveAll(s.workDir); err != nil {
					return fmt.Errorf("remove canonical worktree dir %q: %w", s.workDir, err)
				}
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

		// Issue #2187: also release any foreign live worktree at a different
		// path that currently holds s.branch so the upcoming `git branch -D`
		// can succeed. The release is `git -C <path> checkout --detach` only
		// — the foreign worktree's directory, `.git` gitlink, and
		// `.git/worktrees/<dir>` registration are left intact. Parallel
		// sandbox runs share the same `.git` bind mount, so destroying
		// sibling worktree registrations here would leave them with a
		// broken `.git` pointer.
		if s.strandedReconcile {
			if info, foreign := ForeignStrandedWorktree(s.repoPath, s.worktreeBase, s.branch); foreign {
				// Only release FOREIGN worktrees (a worktree whose
				// path is not the main repo). The main repo holding
				// the branch is recovered via the worktree-add step
				// further below — the override path detaches the
				// parent (if on the branch) and reuses the branch
				// via `git worktree add -f <path> <branch>` instead
				// of the previous delete-and-recreate flow.
				if !SamePath(info.Path, s.repoPath) {
					if err := ReleaseBranchInWorktree(info.Path); err != nil {
						s.warn("issue #2187: release foreign worktree at %q: %v\n", info.Path, err)
					}
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
			// Issue #2187: scoped reattach for the canonical
			// `s.workDir`. Drop the registration BEFORE clearing
			// the directory so subsequent recovery does
			// not see a "checked out at" pointer. No global
			// `git worktree prune`.
			if err := s.removePrunableWorktreeRegistration(); err != nil {
				return fmt.Errorf("drop canonical worktree registration: %w", err)
			}
			if err := os.RemoveAll(s.workDir); err != nil {
				return fmt.Errorf("clean forced worktree dir: %w", err)
			}
		}
		// Issue #2187: no global `git worktree prune`. The canonical
		// `s.workDir` may have a prunable registration (broken `.git`
		// gitlink), but recovery for that case is handled earlier via the
		// ReclaimableWorktree path or by the `git worktree remove --force
		// <s.workDir>` above. We deliberately do not prune sibling
		// registrations.
		//
		// The override path no longer deletes the branch here. The
		// worktree-add step (further below) reuses the branch if it
		// exists via `git worktree add -f <path> <branch>`, avoiding
		// the delete-and-recreate TOCTOU window where a sibling
		// worktree could attach between the parent's detach and the
		// ref-drop. See ADR-0023 strategy 3.
	}
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
			// Issue #2187: scoped cleanup of the canonical
			// `s.workDir` registration after branch delete. No
			// global `git worktree prune`.
			if err := s.removePrunableWorktreeRegistration(); err != nil {
				return fmt.Errorf("drop canonical worktree registration: %w", err)
			}
			if s.workDirExists() {
				if err := os.RemoveAll(s.workDir); err != nil {
					return fmt.Errorf("remove canonical worktree dir %q: %w", s.workDir, err)
				}
			}
		} else {
			return fmt.Errorf(`branch %q already exists — delete it with "git branch -D %s" and re-run`, s.branch, s.branch)
		}
	}

	// Avoid the delete-and-recreate TOCTOU window. `git branch -D`
	// re-checks worktree holders at the moment of the call, but its
	// `refs_delete_refs` transaction is a separate step that can race
	// with a sibling worktree attaching `refs/heads/<branch>` in
	// between — leaving the sibling with a dangling symbolic HEAD.
	// Instead, reuse the existing branch via `git worktree add -f
	// <path> <branch>` when it is present; only fall back to creating
	// a new branch from source when the branch does not exist. This
	// eliminates the delete-recreate window entirely (the existing
	// branch is never deleted) and shrinks the attach window to the
	// single git command boundary.
	var addCmd *exec.Cmd
	if BranchExists(s.repoPath, s.branch) {
		// If the parent is on the branch, detach so the branch is
		// not "checked out" in the parent when we add the worktree.
		headRef, err := CurrentBranchRef(s.repoPath)
		if err == nil && headRef == "refs/heads/"+s.branch {
			detachCmd := exec.Command("git", "checkout", "--detach")
			detachCmd.Dir = s.repoPath
			if out, err := detachCmd.CombinedOutput(); err != nil {
				return fmt.Errorf("detach HEAD in main repo to release %s: %w\n%s", s.branch, err, out)
			}
		}
		addCmd = exec.Command("git", "worktree", "add", "-f", s.workDir, s.branch)
	} else {
		addCmd = exec.Command("git", "worktree", "add", "-b", s.branch, s.workDir, s.sourceBranch)
	}
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

// configureGitIdentity writes the identity captured during Start() to the
// worktree-local git config. Called from Start once the worktree is in place.
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
// gitMergeBaseIsAncestor. The verify chain in `internal/batch`
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
	if err := atomicfs.WriteAtomic(promptPath, []byte(content), 0644); err != nil {
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
// without spawning a real git repo. See ADR-0023 for the pattern.
var reconcileStrandedFn = func(repoPath, worktreeBase, branch, sourceBranch string) error {
	return defaultReconcileStrandedBranch(repoPath, worktreeBase, branch, sourceBranch)
}

// defaultReconcileStrandedBranch is the default implementation of the
// recovery loop. The previous version force-checked out sourceBranch in
// repoPath, which silently mutated the operator's working-copy HEAD and
// left them on the source branch instead of the issue branch they were
// inspecting. The new version detaches HEAD in the parent repo (working
// tree untouched) and then deletes the branch via `git branch -D`,
// which re-checks worktree holders at the moment of the delete — this
// closes the TOCTOU window where a sibling worktree could check out
// the branch between the parent's `git checkout --detach` and the
// delete. The branch is re-created later in Start() by the worktree-add
// step (which reuses the branch if it exists, or creates a fresh
// branch from source if it does not), so the effective loss is bounded
// to a transient detached-HEAD window in the parent repo.
//
// ADR-0023 documented `git update-ref -d` as strategy (3) "last resort";
// this commit elevates it to the default for the main-repo case so that
// operators who keep their working copy on the issue branch while
// orchestrating runs from the same checkout no longer have their HEAD
// switched to the source branch under them. The implementation uses
// `git branch -D` (rather than the documented `update-ref -d`) so the
// delete re-checks worktree holders atomically with the ref-drop,
// closing the TOCTOU window where a sibling worktree could check out
// the branch between the parent's detach and a raw update-ref.
// Foreign-worktree recovery still goes through `ReleaseBranchInWorktree`
// and the stranded-worktree path, both of which only detach a foreign
// HEAD.
//
// Guard: the parent HEAD must point at refs/heads/<branch> before this
// function detaches it. Without this guard, a race with a foreign
// worktree holder (between detection in Start() and the recovery
// branch) would leave the parent HEAD silently detached even though
// the parent was on, e.g., main, and the foreign worktree's symbolic
// HEAD would dangle against a deleted ref. The guard restores the
// invariant: this function only ever mutates HEAD when the parent is
// the verified holder.
//
// TOCTOU note: `git checkout --detach` releases Git's
// checked-out-branch guard. A sibling worktree that checks out the
// branch between the detach and the delete would otherwise see the
// ref deleted out from under it. Using `git branch -D` (rather than
// `git update-ref -d`) makes the delete re-check worktree holders
// atomically with the ref-drop, so a freshly-checked-out sibling is
// preserved.
func defaultReconcileStrandedBranch(repoPath, worktreeBase, branch, sourceBranch string) error {
	headRef, err := CurrentBranchRef(repoPath)
	if err != nil {
		// HEAD is detached or unreadable. We cannot tell whether the
		// parent holds the branch; refuse to drop the ref to avoid
		// deleting a branch a foreign worktree still holds.
		return fmt.Errorf("drop branch refs/heads/%s: main repo HEAD is not a symbolic ref (%v) — refusing to detach (foreign worktree holder race?)", branch, err)
	}
	if headRef != "refs/heads/"+branch {
		return fmt.Errorf("drop branch refs/heads/%s: main repo HEAD is %q, not the target branch — refusing to detach (foreign worktree holder race?)", branch, headRef)
	}
	detachCmd := exec.Command("git", "checkout", "--detach")
	detachCmd.Dir = repoPath
	if out, err := detachCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("detach HEAD in main repo: %w\n%s", err, out)
	}
	// Use `git branch -D` rather than `git update-ref -d` so the
	// delete re-checks worktree holders atomically with the
	// ref-drop. This closes the TOCTOU window where a sibling
	// worktree could check out the branch between the parent's
	// detach and a raw update-ref.
	delCmd := exec.Command("git", "branch", "-D", branch)
	delCmd.Dir = repoPath
	if out, err := delCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("drop branch refs/heads/%s in main repo via 'git branch -D': %w\n%s", branch, err, out)
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

// removePrunableWorktreeRegistration drops the registration directory that
// corresponds to the canonical worktree, without touching any other
// worktree registration. It is the scoped equivalent of
// `git worktree prune` for a single path.
//
// Use this in recovery paths to drop a stale registration (the worktree
// directory was removed out-of-band, or the `.git` gitlink was rewritten)
// without running a global prune — that would also drop registrations
// owned by sibling sandbox runs sharing the same `.git`.
//
// Returns nil when there is no registration to remove.
//
// Issue #2187.
func (s *WorktreeSandbox) removePrunableWorktreeRegistration() error {
	return RemoveWorktreeRegistration(s.repoPath, s.workDir)
}

// RemoveWorktreeRegistration removes only the registration corresponding to
// worktreePath. It is the target-scoped alternative to `git worktree prune`,
// which can remove live sibling registrations when host paths are not visible
// from a container.
func RemoveWorktreeRegistration(repoPath, worktreePath string) error {
	registrations := filepath.Join(repoPath, ".git", "worktrees")
	entries, err := os.ReadDir(registrations)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read worktree registrations %q: %w", registrations, err)
	}

	want, err := filepath.Abs(worktreePath)
	if err != nil {
		return fmt.Errorf("resolve worktree path %q: %w", worktreePath, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(registrations, entry.Name())
		gitdir, err := os.ReadFile(filepath.Join(candidate, "gitdir"))
		if err != nil {
			continue
		}
		registeredPath, err := filepath.Abs(filepath.Dir(strings.TrimSpace(string(gitdir))))
		if err != nil {
			continue
		}
		if registeredPath != want {
			continue
		}
		if err := os.RemoveAll(candidate); err != nil {
			return fmt.Errorf("remove registration %q: %w", candidate, err)
		}
		return nil
	}

	return nil
}

// refuseContinuation returns a targeted error explaining why the preserved
// worktree at s.workDir cannot be reused by a continue-mode Start. It runs
// before any destructive reconciliation so the orchestrator can surface
// the failure to the operator; --override remains the only recovery mode
// that bypasses the early return. Issue #2189.
func (s *WorktreeSandbox) refuseContinuation(state ContinuationWorktreeState, actualRef string) error {
	switch state {
	case ContinuationWorktreeMissing:
		return fmt.Errorf("cannot continue: preserved worktree at %q has no live registration in the parent repo; re-run with --override to reconcile", s.workDir)
	case ContinuationWorktreeDetached:
		return fmt.Errorf("cannot continue: preserved worktree at %q has a detached HEAD; re-run with --override to reconcile", s.workDir)
	case ContinuationWorktreeWrongBranch:
		actualBranch := strings.TrimPrefix(actualRef, "refs/heads/")
		return fmt.Errorf("cannot continue: preserved worktree at %q is checked out on branch %q, expected %q; re-run with --override to reconcile",
			s.workDir, actualBranch, s.branch)
	}
	return nil
}

// SamePath reports whether two paths refer to the same directory after
// symlink resolution. Used to exclude the main repo from being treated
// as a "foreign stranded worktree" by recovery helpers. Exported so
// call sites in other packages (e.g. recoverBranchDeleteFromMainRepo
// in internal/batch) can apply the same guard without depending on
// WorktreeSandbox's full overrideCleanup flow.
func SamePath(a, b string) bool {
	if a == b {
		return true
	}
	ar, err := filepath.EvalSymlinks(a)
	if err != nil {
		ar = a
	}
	br, err := filepath.EvalSymlinks(b)
	if err != nil {
		br = b
	}
	if ar == br {
		return true
	}
	if info1, err1 := os.Stat(ar); err1 == nil {
		if info2, err2 := os.Stat(br); err2 == nil {
			if os.SameFile(info1, info2) {
				return true
			}
		}
	}
	return false
}

// ReleaseBranchInWorktree detaches HEAD in the worktree at `path` so the
// branch it currently holds is no longer checked out there. After this
// runs, `git branch -D` from the main repo succeeds even though the
// worktree's directory, `.git` gitlink, and `.git/worktrees/<dir>`
// registration are all left intact. This is the recovery primitive used
// to free a branch that is held by a foreign worktree (e.g. a sibling
// sandbox run's worktree) without destroying that worktree.
//
// Idempotent: a no-op when the path does not exist (returns nil),
// because the caller can't always tell whether a foreign worktree
// directory is still around between detection and the recovery step.
// Issue #2187: scoped to the offending worktree only; never runs
// `git worktree remove` or `git worktree prune` against any path.
//
// Exported so call sites in other packages (e.g. ClearIssueArtifacts
// in internal/batch) can recover from a "branch checked out in a
// foreign worktree" failure without depending on WorktreeSandbox's
// full overrideCleanup flow.
func ReleaseBranchInWorktree(path string) error {
	if path == "" {
		return fmt.Errorf("release branch in worktree: empty path")
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat worktree %q: %w", path, err)
	}
	detachCmd := exec.Command("git", "checkout", "--detach")
	detachCmd.Dir = path
	if out, err := detachCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("detach HEAD in worktree %q: %w\n%s", path, err, out)
	}
	return nil
}

// reconcileStrandedBranch attempts to remove the stale branch after the
// initial "git branch -D" failed because the branch is checked out
// somewhere. The caller has already attempted the stranded-worktree
// strategy (delete from the stranded worktree's cwd) and removed any
// stale worktree registration; the recovery path here is therefore
// the "main repo on the branch" case, dispatched through the
// package-level reconcileStrandedFn seam (see ADR-0023).
//
// The default seam implementation drops the stray ref via
// `git branch -D` rather than force-checking out the source branch
// in the parent repo, so the operator's working-tree HEAD is preserved
// across the recovery. `git branch -D` re-checks worktree holders at
// the moment of the delete; a sibling worktree that acquires the
// branch after the parent detach but before the delete is preserved.
// The branch is re-created later in Start() by the worktree-add step
// (which reuses the branch if it exists, or creates a fresh branch
// from source if it does not), so the operator's local view of the
// branch is restored within the same Start().
//
// Returns nil on success, or a non-nil error on hard failure.
func (s *WorktreeSandbox) reconcileStrandedBranch() error {
	return reconcileStrandedFn(s.repoPath, s.worktreeBase, s.branch, s.sourceBranch)
}
