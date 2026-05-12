package sandbox

import (
	"context"
	"io"
	"os"
	"os/exec"
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
}

// waitCmd waits for cmd to finish, returning ctx.Err() if the context is cancelled first.
func waitCmd(ctx context.Context, cmd *exec.Cmd) error {
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		<-done
		return ctx.Err()
	}
}
