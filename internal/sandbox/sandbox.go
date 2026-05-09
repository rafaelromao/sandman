package sandbox

import "context"

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
}
