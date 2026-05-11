package sandbox

import (
	"context"
	"io"
	"os"
)

// RunResult captures structured output produced by an agent run.
type RunResult struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

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
	// Stop tears down the sandbox environment.
	Stop() error
	// WorkDir returns the working directory path of the sandbox.
	WorkDir() string
	// WritePrompt writes the prompt content to the sandbox.
	WritePrompt(content string) error
	// ReadRunResult reads the run result produced by the agent.
	ReadRunResult() (*RunResult, error)
	// Process returns the running OS process, or nil if no process is active.
	Process() Process
}
