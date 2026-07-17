package sandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
)

// Process represents a running OS process that can be signalled and waited on.
//
// The sandbox package owns the only goroutine that calls the underlying
// exec.Cmd.Wait. Callers must not call Wait themselves — they observe
// process exit via WaitDone, which closes when the package's internal
// Wait goroutine returns. WaitDone is the canonical exit signal;
// supervisors should select on it (or on their own context / timeout)
// rather than sleeping.
type Process interface {
	Signal(sig os.Signal) error
	Kill() error
	WaitDone() <-chan struct{}
}

// SandboxIdentity pairs the git author identity that a WorktreeSandbox
// writes to the worktree-local git config during Start. Name and Email
// travel together everywhere; this named sub-struct mirrors the shape
// returned by the per-row git-identity resolve in internal/batch.
type SandboxIdentity struct {
	Name  string
	Email string
}

// SandboxStart carries the configuration absorbed from the pre-Start Set*
// protocol that used to live as four independent setters on the Sandbox
// interface. Constructed per-row by the runSession helper in internal/batch
// and handed to Sandbox.Start(opts) at the seam. The zero value
// SandboxStart{} is a safe "configure nothing" — production always sets
// all four fields explicitly, but tests that want a partial configure
// (e.g. only Identity) can rely on zero-default behaviour for the others.
type SandboxStart struct {
	// Override enables override behaviour for orphan worktree recovery.
	Override bool
	// Continue signals that this Start is a continuation of a previous run
	// that may have left a /workspace-visible gitlink behind in the
	// preserved worktree's .git file.
	Continue bool
	// StrandedReconcile enables or disables auto-recovery from stranded
	// worktrees during Start.
	StrandedReconcile bool
	// Identity is the worktree-local git identity to write during Start.
	Identity SandboxIdentity
}

// Sandbox provides isolation for one or more AgentRuns.
type Sandbox interface {
	// Start initializes the sandbox environment.
	Start() error
	// Exec runs a command inside the sandbox, writing stdout and stderr to the given writers.
	Exec(ctx context.Context, command string, stdout, stderr io.Writer) error
	// ExecInteractive runs a command inside the sandbox attached to the user's terminal.
	ExecInteractive(ctx context.Context, command string) error
	// Stop tears down the sandbox environment.
	Stop() error
	// WorkDir returns the working directory path of the sandbox.
	WorkDir() string
	// RepoPath returns the parent repository path that owns the sandbox.
	// Used to run git commands (e.g. `git checkout -f`) that must target
	// the repo, not the worktree.
	RepoPath() string
	// WritePrompt writes the prompt content to the sandbox.
	WritePrompt(content string) error
	// Process returns the running OS process, or nil if no process is active.
	Process() Process
	// SetOverride enables override behavior for orphan worktree recovery.
	// Must be safe to call before Start.
	SetOverride(override bool)
	// SetStrandedReconcile enables or disables auto-recovery from a
	// stranded worktree or a "branch used by worktree at" error during
	// Start. Must be safe to call before Start.
	SetStrandedReconcile(enabled bool)
	// SetGitIdentity configures the identity Sandman should write to worktree-local git config.
	// Must be safe to call before Start.
	SetGitIdentity(name, email string)
	// SetContinue signals that this Start is a continuation of a
	// previous run that may have left a /workspace-visible gitlink
	// behind. When enabled, Start normalizes the preserved worktree's
	// .git pointer back to host-visible paths before validation and
	// reuses the existing worktree. Must be safe to call before Start.
	// Issue #2189.
	SetContinue(c bool)
	// RestoreHostPaths returns the sandbox to host-visible state without
	// removing the worktree. For container sandboxes this rewrites the
	// preserved worktree's .git pointer from /workspace/... back to the
	// host repository path. For worktree-only sandboxes it is a no-op.
	// It is safe to call any number of times; subsequent calls without
	// intermediate rewriting are no-ops. Issue #2189.
	RestoreHostPaths() error
}

// waitCmd waits for cmd to finish via its owning processWrapper's
// WaitDone channel, returning ctx.Err() if the context is cancelled
// first. When the context is cancelled, the process is killed so the
// wait unblocks.
//
// waitCmd does not call cmd.Wait itself — the wrapper owns that single
// Wait. Calling cmd.Wait twice triggers Go's "Wait was already called"
// error. cmdWrapper is the wrapper created when cmd was started; the
// caller passes it in so waitCmd can block on the right channel.
//
// onAbort is an optional callback invoked synchronously when the context
// is cancelled, after the process group kill is sent but before waitCmd
// waits for the process to exit. It is intended for propagating the abort
// signal into container namespaces where the host-side process group kill
// does not reach.
func waitCmd(ctx context.Context, cmd *exec.Cmd, cmdWrapper *processWrapper, onAbort func()) error {
	if cmdWrapper == nil {
		return errors.New("waitCmd: cmdWrapper is nil")
	}
	waitDone := cmdWrapper.WaitDone()

	select {
	case <-waitDone:
		return cmdWrapper.exitErr()
	case <-ctx.Done():
		if cmd.Process != nil {
			// Kill the entire process group (negative PID) to ensure child
			// processes such as agent scripts and their background tasks
			// are terminated, not just the immediate sh -c parent.
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		if onAbort != nil {
			onAbort()
		}
		<-waitDone
		return ctx.Err()
	}
}
