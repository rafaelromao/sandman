package batch

import (
	"context"
	"io"
	"time"

	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

// Request describes a batch of AgentRuns to execute.
type Request struct {
	Issues []int
	// Dependencies maps each issue to its resolved BlockedBy set.
	Dependencies map[int][]int
	// Blocked marks issues that should be skipped before submission.
	Blocked      map[int][]int
	Agent        string
	Model        string
	BaseBranch   string
	Continuation bool
	// PreviousRunIDs maps each issue number to the run id being continued.
	PreviousRunIDs             map[int]string
	Retries                    int
	Parallel                   int
	StartDelay                 time.Duration
	StartDelaySet              bool
	Branches                   map[int]string
	Sandbox                    string
	ContainerCapacity          int
	ContainerCapacitySet       bool
	MaxContainers              int
	MaxContainersSet           bool
	Force                      bool
	DangerouslySkipPermissions *bool
	PromptConfig               prompt.RenderConfig
	OutputWriter               io.Writer
}

// Result describes the outcome of a batch.
type Result struct {
	Runs []AgentRunResult
}

// AgentRunResult describes the outcome of a single AgentRun.
type AgentRunResult struct {
	IssueNumber  int
	Issue        *int
	Status       string
	RetriesTotal int
	Branch       string
	WorktreePath string
}

// Runnable represents a single agent execution that can be run.
type Runnable interface {
	Run(ctx context.Context, renderer prompt.Renderer, command string, renderCfg prompt.RenderConfig) AgentRunResult
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

// SandboxFactory creates a Sandbox for a given branch.
type SandboxFactory interface {
	NewSandbox(repoPath, worktreeBase, branch, sourceBranch string, container sandbox.Container) sandbox.Sandbox
}

// defaultSandboxFactory creates WorktreeSandbox instances.
type defaultSandboxFactory struct{}

func (d defaultSandboxFactory) NewSandbox(repoPath, worktreeBase, branch, sourceBranch string, container sandbox.Container) sandbox.Sandbox {
	return sandbox.NewWorktreeSandbox(repoPath, worktreeBase, branch, sourceBranch)
}

// ContainerSandboxFactory creates ContainerSandbox instances (isolated mode).
type ContainerSandboxFactory struct {
	Binary   string
	RepoPath string
}

func (f ContainerSandboxFactory) NewSandbox(repoPath, worktreeBase, branch, sourceBranch string, container sandbox.Container) sandbox.Sandbox {
	wt := sandbox.NewWorktreeSandbox(repoPath, worktreeBase, branch, sourceBranch)
	return sandbox.NewContainerSandbox(wt, container, f.Binary, f.RepoPath)
}

// SharedContainerSandboxFactory creates SharedContainerSandbox instances (shared mode).
type SharedContainerSandboxFactory struct {
	Binary   string
	RepoPath string
}

func (f SharedContainerSandboxFactory) NewSandbox(repoPath, worktreeBase, branch, sourceBranch string, container sandbox.Container) sandbox.Sandbox {
	wt := sandbox.NewWorktreeSandbox(repoPath, worktreeBase, branch, sourceBranch)
	return sandbox.NewSharedContainerSandbox(wt, container, f.Binary, f.RepoPath)
}

// ContainerRuntimeFactory creates container starters.
type ContainerRuntimeFactory interface {
	New(binary string) sandbox.ContainerStarter
}

type defaultContainerRuntimeFactory struct{}

func (d defaultContainerRuntimeFactory) New(binary string) sandbox.ContainerStarter {
	return sandbox.NewContainerRuntime(binary)
}

// Runner coordinates parallel execution of AgentRuns.
type Runner interface {
	RunBatch(ctx context.Context, req Request) (*Result, error)
}
