package sandbox

import "context"

// RunResult captures structured output produced by an agent run.
type RunResult struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Sandbox provides isolation for one or more AgentRuns.
type Sandbox interface {
	// Start initializes the sandbox environment.
	Start() error
	// Exec runs a command inside the sandbox.
	Exec(ctx context.Context, command string) error
	// Stop tears down the sandbox environment.
	Stop() error
	// WorkDir returns the working directory path of the sandbox.
	WorkDir() string
	// WritePrompt writes the prompt content to the sandbox.
	WritePrompt(content string) error
	// ReadRunResult reads the run result produced by the agent.
	ReadRunResult() (*RunResult, error)
}
