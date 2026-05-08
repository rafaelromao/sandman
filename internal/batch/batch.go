package batch

import "context"

// Request describes a batch of AgentRuns to execute.
type Request struct {
	Issues   []int
	Parallel int
}

// Result describes the outcome of a batch.
type Result struct {
	Runs []AgentRunResult
}

// AgentRunResult describes the outcome of a single AgentRun.
type AgentRunResult struct {
	IssueNumber int
	Status      string
	Branch      string
	PRURL       string
}

// Runner coordinates parallel execution of AgentRuns.
type Runner interface {
	RunBatch(ctx context.Context, req Request) (*Result, error)
}
