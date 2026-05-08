package sandbox

import "context"

// Sandbox provides isolation for one or more AgentRuns.
type Sandbox interface {
	// Start initializes the sandbox environment.
	Start() error
	// Exec runs a command inside the sandbox at the given worktree path.
	Exec(ctx context.Context, worktreePath string, command string) error
	// Stop tears down the sandbox environment.
	Stop() error
}
