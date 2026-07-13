package batch

import (
	"github.com/rafaelromao/sandman/internal/sandbox"
)

// T1SandboxFactory returns a sandbox.WorktreeSandbox whose source
// branch is pinned to `origin/main` HEAD. The T1 decision oracle
// uses this factory to ensure every `go test -run ...` line runs
// against the canonical main-line code, not the branch's own diff.
//
// The factory is wired into the orchestrator through the
// `Orchestrator.verifyPath` seam (see verify.go); production code
// passes a `*T1SandboxFactory` into the T1 oracle's `Runner`
// callback when constructing a non-default chain. Tests pass a
// literal VerifyPathFunc to bypass the factory entirely.
type T1SandboxFactory struct {
	// RepoPath is the git working copy whose `origin/main` the
	// oracle verifies against.
	RepoPath string
	// WorktreeBase is the directory under which the verifier
	// creates its ephemeral worktree.
	WorktreeBase string
	// Branch is the local branch the verifier checks out. It must
	// be unique per run so concurrent verifications don't
	// clobber each other's worktrees.
	Branch string
}

// NewSandbox implements the batch.SandboxFactory seam. The
// sourceBranch is overridden to "origin/main" — every other
// argument flows through from the orchestrator's call site.
func (f *T1SandboxFactory) NewSandbox(repoPath, worktreeBase, branch, _ string, _ sandbox.Container) sandbox.Sandbox {
	repo := f.RepoPath
	if repo == "" {
		repo = repoPath
	}
	base := f.WorktreeBase
	if base == "" {
		base = worktreeBase
	}
	branchName := f.Branch
	if branchName == "" {
		branchName = branch
	}
	return sandbox.NewWorktreeSandbox(repo, base, branchName, "origin/main")
}

// ReplaySandboxFactory is the slice-4 equivalent for T3: a
// SandboxFactory whose source branch is pinned to the PR's head
// (or the run's current branch when no PR exists). The T3
// transitional fallback uses this factory so the
// `ok: <cmd> -> <sentinel>` lines replay against the actual
// change, not main.
type ReplaySandboxFactory struct {
	// RepoPath is the git working copy whose HEAD the oracle
	// replays against.
	RepoPath string
	// WorktreeBase is the directory under which the replay
	// worktree is created.
	WorktreeBase string
	// Branch is the local branch the replay worktree checks out.
	Branch string
	// SourceBranch overrides the worktree's source ref. When
	// empty, the orchestrator's call-site source branch is used.
	SourceBranch string
}

// NewSandbox implements the batch.SandboxFactory seam. Unlike the
// T1 factory, the source branch defaults to the run's own branch
// (so evidence replays see the change the user is verifying), with
// an explicit override when the PR head ref is known.
func (f *ReplaySandboxFactory) NewSandbox(repoPath, worktreeBase, branch, sourceBranch string, _ sandbox.Container) sandbox.Sandbox {
	repo := f.RepoPath
	if repo == "" {
		repo = repoPath
	}
	base := f.WorktreeBase
	if base == "" {
		base = worktreeBase
	}
	branchName := f.Branch
	if branchName == "" {
		branchName = branch
	}
	src := f.SourceBranch
	if src == "" {
		src = sourceBranch
	}
	if src == "" {
		src = "HEAD"
	}
	return sandbox.NewWorktreeSandbox(repo, base, branchName, src)
}
