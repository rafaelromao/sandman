package batch

import (
	"context"
	"fmt"
)

// Orchestrator coordinates parallel AgentRun execution.
type Orchestrator struct{}

// RunBatch executes the requested AgentRuns in parallel.
func (o *Orchestrator) RunBatch(ctx context.Context, req Request) (*Result, error) {
	return nil, fmt.Errorf("batch orchestration not yet implemented")
}

// Ensure Orchestrator implements Runner.
var _ Runner = (*Orchestrator)(nil)
