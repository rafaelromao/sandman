package batch

import (
	"context"
	"io"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

// RowSpec carries the per-row-varying inputs for one AgentRun — the inputs that
// differ across issues/runs within a batch. It is the elevated seam's input:
// runSingleRow / runPromptOnlyRow take a RowSpec instead of the historical
// 28-/27-positional-argument packers (wayfinder map #2226, slice 1 #2228).
//
// Path discrimination is implicit, matching today's runSession: issue-driven
// when IssueNumber>0 (RunTS/RunShortID carry the ID components); prompt-only /
// review otherwise (BatchTS/BatchShortID/RunID carry them; Review/PRNumber/
// ReviewFocus set for review runs).
type RowSpec struct {
	IssueNumber      int
	Mode             IssueMode
	Branches         map[int]string
	PreviousRunIDs   map[int]string
	BaseBranch       string
	ExternalBlockers []int
	RenderCfg        prompt.RenderConfig
	OutputWriter     io.Writer
	// ID minting — issue-driven path.
	RunTS      string
	RunShortID string
	// ID minting — prompt-only path.
	BatchTS           string
	BatchShortID      string
	RunID             string
	UserProvidedRunID string
	// Pre-derived at the call site (issue-driven: from buildRunID;
	// prompt-only: from batchIDForPromptOnly).
	BatchID string
	// Review / prompt-only fields.
	Review      bool
	PRNumber    int
	ReviewFocus string
}

// BatchConfig carries the batch-constant knobs — inputs that are the same for
// every row in a batch. It is set once and passed alongside each RowSpec.
type BatchConfig struct {
	Cfg                        *config.Config
	AgentName                  string
	AgentCfg                   config.Agent
	IdentityResolver           *gitIdentityResolver
	Parallel                   int
	StartDelay                 time.Duration
	Retries                    int
	RunIdleTimeout             int
	SandboxMode                string
	ContainerCapacity          int
	ContainerCapacitySet       bool
	MaxContainers              int
	MaxContainersSet           bool
	DangerouslySkipPermissions bool
	StrandedReconcile          bool
}

// runDeps bundles the read-only dependencies the elevated executor reaches
// through today's *Orchestrator ref. It is the test seam: behaviour is
// injected by constructing runDeps directly rather than poking Orchestrator
// private fields (wayfinder map #2226, slice 2 #2234). Every field is
// batch-constant — set once per RunBatch and shared by every row.
type runDeps struct {
	githubClient          github.Client
	renderer              prompt.IssueRenderer
	eventLog              events.EventLog
	runnableFactory       RunnableFactory
	sandboxFactory        SandboxFactory
	layout                paths.Layout
	heartbeatTickInterval time.Duration
	errorLog              io.Writer
	runSessionOpts        runSessionOptions
	verifyPath            VerifyPathFunc
}

// runCoordination is the narrow interface the elevated executor uses for the
// shared, mutable coordination state that RunBatch owns (the active-runs map,
// the shutdown-supervisor fan-in, the per-batch phase writer, and the
// first-sandbox-start once). *Orchestrator implements it; RunBatch stays the
// owner. Keeping this an interface (rather than a raw struct handle) preserves
// the executor's testability — the constructor is the seam.
type runCoordination interface {
	registerActiveRun(key int, sb sandbox.Sandbox)
	unregisterActiveRun(key int)
	trackShutdownSupervisor(done <-chan struct{})
	currentPhaseWriter() io.Writer
	firstSandboxStart(sandboxStarted time.Time)
}

// RunExecutor is the elevated seam for one AgentRun's lifecycle behind a
// single method (wayfinder #2233). Path is implicit — issue-driven when
// IssueNumber>0, prompt-only/review otherwise — matching today's
// discrimination via issueNumber and the review/prNumber fields.
type RunExecutor interface {
	Execute(ctx context.Context, row RowSpec) (AgentRunResult, bool)
}

// runExecutor is the RunExecutor implementation. It holds the batch-constant
// state (deps + coordination + commander + BatchConfig + factories + the
// RunBatch parent ctx) and builds a fresh runSession per Execute call. It has
// no *Orchestrator field: every formerly-reached Orchestrator dependency is
// an explicit constructor input (wide constructor, narrow Execute = deep
// module).
type runExecutor struct {
	deps           runDeps
	coord          runCoordination
	commander      daemon.IssueCommander
	bc             BatchConfig
	sbFactory      SandboxFactory
	containerAlloc containerAllocator
	parentCtx      context.Context
}

// newRunExecutor is the constructor/test seam: it snapshots the
// batch-constant dependencies off the orchestrator (which still owns the
// coordination state and implements runCoordination / IssueCommander) and
// freezes them on the returned executor.
func (o *Orchestrator) newRunExecutor(parentCtx context.Context, bc BatchConfig, sbFactory SandboxFactory, containerAlloc containerAllocator) *runExecutor {
	coord := newBatchCoordinator(nil)
	return o.newRunExecutorWith(parentCtx, bc, sbFactory, containerAlloc, coord, coord, o.layout)
}

func (o *Orchestrator) newRunExecutorWith(parentCtx context.Context, bc BatchConfig, sbFactory SandboxFactory, containerAlloc containerAllocator, coord runCoordination, commander daemon.IssueCommander, layout paths.Layout) *runExecutor {
	return &runExecutor{
		deps: runDeps{
			githubClient:          o.githubClient,
			renderer:              o.renderer,
			eventLog:              o.eventLog,
			runnableFactory:       o.runnableFactory,
			sandboxFactory:        o.sandboxFactory,
			layout:                layout,
			heartbeatTickInterval: o.heartbeatTickInterval,
			errorLog:              o.errorLog,
			runSessionOpts:        o.runSessionOpts,
			verifyPath:            o.verifyPath,
		},
		coord:          coord,
		commander:      commander,
		bc:             bc,
		sbFactory:      sbFactory,
		containerAlloc: containerAlloc,
		parentCtx:      parentCtx,
	}
}

// Execute dispatches one row through the issue-driven or prompt-only
// lifecycle, discriminated by row.IssueNumber>0. parentCtx (the RunBatch ctx)
// is batch-constant and held on the executor; ctx is the per-row ctx
// (per-issue for issue-driven, the RunBatch ctx for prompt-only).
func (e *runExecutor) Execute(ctx context.Context, row RowSpec) (AgentRunResult, bool) {
	s := newRunSession(e, row)
	if row.IssueNumber > 0 {
		return s.execute(ctx)
	}
	return s.executePromptOnly(ctx)
}

// newRunSession builds a runSession from the elevated seam's inputs. It
// centralizes the runSession construction that was previously duplicated
// verbatim between runSingle and runPromptOnlySingle. Behaviour-preserving:
// every field maps 1:1 to the prior literal assignments.
func newRunSession(e *runExecutor, row RowSpec) *runSession {
	bc := e.bc
	return &runSession{
		deps:                       e.deps,
		coord:                      e.coord,
		commander:                  e.commander,
		issueNumber:                row.IssueNumber,
		cfg:                        bc.Cfg,
		agentName:                  bc.AgentName,
		agentCfg:                   bc.AgentCfg,
		mode:                       row.Mode,
		previousRunIDs:             row.PreviousRunIDs,
		identityResolver:           bc.IdentityResolver,
		branches:                   row.Branches,
		renderCfg:                  row.RenderCfg,
		outputWriter:               row.OutputWriter,
		sbFactory:                  e.sbFactory,
		containerAlloc:             e.containerAlloc,
		baseBranch:                 row.BaseBranch,
		externalBlockers:           row.ExternalBlockers,
		parallel:                   bc.Parallel,
		startDelay:                 bc.StartDelay,
		retries:                    bc.Retries,
		runIdleTimeout:             bc.RunIdleTimeout,
		sandboxMode:                bc.SandboxMode,
		containerCapacity:          bc.ContainerCapacity,
		containerCapacitySet:       bc.ContainerCapacitySet,
		maxContainers:              bc.MaxContainers,
		maxContainersSet:           bc.MaxContainersSet,
		dangerouslySkipPermissions: bc.DangerouslySkipPermissions,
		strandedReconcile:          bc.StrandedReconcile,
		runTS:                      row.RunTS,
		runShortID:                 row.RunShortID,
		batchID:                    row.BatchID,
		batchTS:                    row.BatchTS,
		batchShortID:               row.BatchShortID,
		runID:                      row.RunID,
		userProvidedRunID:          row.UserProvidedRunID,
		review:                     row.Review,
		prNumber:                   row.PRNumber,
		reviewFocus:                row.ReviewFocus,
		parentCtx:                  e.parentCtx,
		opts:                       e.deps.runSessionOpts,
	}
}
