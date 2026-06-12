package batch

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

// ErrAborted is returned (wrapped) by RunBatch when context cancellation interrupted an in-flight AgentRun.
var ErrAborted = errors.New("batch aborted by context cancellation")

// ErrNoSuchIssue is returned by Orchestrator.AbortIssue when the named issue is
// not currently tracked (already finished, never started, or unknown to this batch).
var ErrNoSuchIssue = errors.New("batch: no such issue")

// IssueMode routes one issue through the batch orchestrator.
type IssueMode int

const (
	ModeFresh IssueMode = iota
	ModeOverride
	ModeContinue
)

// Request describes a batch of AgentRuns to execute.
type Request struct {
	Issues []int
	// Dependencies maps each issue to its resolved BlockedBy set.
	Dependencies map[int][]int
	// Blocked marks issues that should be skipped before submission.
	Blocked    map[int][]int
	Agent      string
	Model      string
	BaseBranch string
	// Mode maps each issue number to its routing mode. Missing entries default
	// to ModeFresh.
	Mode map[int]IssueMode
	// PreviousRunIDs maps each issue number to the run id being continued.
	PreviousRunIDs map[int]string
	// BaseBranches maps each issue number to its base branch.
	BaseBranches map[int]string
	// HandoffPrompts maps each issue number to its rendered handoff prompt.
	HandoffPrompts    map[int]string
	Retries           int
	Parallel          int
	StartDelay        time.Duration
	StartDelaySet     bool
	RunIdleTimeout    int
	RunIdleTimeoutSet bool
	Branches          map[int]string
	Sandbox           string
	// RequireDockerfile enforces a .sandman/Dockerfile preflight for container runs.
	RequireDockerfile          bool
	ContainerCapacity          int
	ContainerCapacitySet       bool
	MaxContainers              int
	MaxContainersSet           bool
	Override                   bool
	DangerouslySkipPermissions *bool
	PromptConfig               prompt.RenderConfig
	OutputWriter               io.Writer
	// RunDir is the per-batch run directory (typically `.sandman/runs/<run-id>`)
	// under which container config snapshots are stored for the lifetime of
	// the batch. When empty, snapshots fall back to a temp directory.
	RunDir string
	// Review marks this batch as a review-agent run (one-shot or daemon).
	// The orchestrator surfaces it as `payload.review = true` on
	// `run.started` and `run.finished` events so the event log and portal
	// can distinguish review runs from implementation runs.
	Review bool
	// PRNumber is the GitHub PR number being reviewed. Only meaningful
	// when Review is true. Implementation runs leave it zero.
	PRNumber int
	// ReviewFocus is the free-form text that followed `/sandman review`
	// in the trigger comment. May be empty. Only meaningful when Review
	// is true.
	ReviewFocus string
	// RunID is an optional batch-level identifier for prompt-only runs.
	// When set, it is used as the run directory name and as the RunID in
	// run.started events instead of the auto-generated run-0-<timestamp>.
	// Must be validated by the caller to match [a-zA-Z][a-zA-Z0-9_-]*.
	RunID string
}

// IssueMode returns the mode for num, defaulting to ModeFresh.
func (r Request) IssueMode(num int) IssueMode {
	if r.Mode == nil {
		return ModeFresh
	}
	if mode, ok := r.Mode[num]; ok {
		return mode
	}
	return ModeFresh
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
	Review       bool
	RunID        string
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
