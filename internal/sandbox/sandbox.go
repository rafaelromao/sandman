package sandbox

import (
	"context"
	"io"
	"os"
	"os/exec"
	"syscall"
)

// Process represents a running OS process that can be signalled.
type Process interface {
	Signal(sig os.Signal) error
	Kill() error
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
	// WritePrompt writes the prompt content to the sandbox.
	WritePrompt(content string) error
	// Process returns the running OS process, or nil if no process is active.
	Process() Process
	// SetOverride enables override behavior for orphan worktree recovery.
	// Must be safe to call before Start.
	SetOverride(override bool)
	// SetGitIdentity configures the identity Sandman should write to worktree-local git config.
	// Must be safe to call before Start.
	SetGitIdentity(name, email string)
}

// waitCmd waits for cmd to finish, returning ctx.Err() if the context is cancelled first.
// When the context is cancelled, the process is killed so the wait unblocks.
func waitCmd(ctx context.Context, cmd *exec.Cmd) error {
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		if cmd.Process != nil {
			// Kill the entire process group (negative PID) to ensure child
			// processes such as agent scripts and their background tasks
			// are terminated, not just the immediate sh -c parent.
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		<-done
		return ctx.Err()
	}
}
