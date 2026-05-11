package batch

import (
	"context"

	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

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

// Runnable represents a single agent execution that can be run.
type Runnable interface {
	Run(ctx context.Context, renderer prompt.Renderer, command string, client github.Client, defaultBranch string) AgentRunResult
}

// RunnableFactory creates a Runnable for a given issue.
type RunnableFactory interface {
	NewRunnable(issue *github.Issue, branch string, sb sandbox.Sandbox) Runnable
}

// defaultRunnableFactory creates AgentRun instances.
type defaultRunnableFactory struct{}

func (d defaultRunnableFactory) NewRunnable(issue *github.Issue, branch string, sb sandbox.Sandbox) Runnable {
	return NewAgentRun(issue, branch, sb)
}

// Runner coordinates parallel execution of AgentRuns.
type Runner interface {
	RunBatch(ctx context.Context, req Request) (*Result, error)
}
