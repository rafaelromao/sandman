package batch

import (
	"context"
	"fmt"
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

// RunSpec is the immutable identity and policy input for one admitted Run. The
// pointer payload is an immutable snapshot of row-specific rendering and review
// settings; lifecycle identity is explicit and never rewritten during a wait.
type RunSpec struct {
	RunID       string
	IssueNumber int
	BatchID     string
	BatchDir    string
	Branch      string
	BaseBranch  string

	RunTS             string
	RunShortID        string
	BatchTS           string
	BatchShortID      string
	UserProvidedRunID string
	details           *runSpecDetails
}

// ContinuationCursor is the state that advances while the same Run is driven.
// Its RunID and BatchID references identify the prior continuation source;
// they do not replace the stable identity in RunSpec.
type ContinuationCursor struct {
	Mode               IssueMode
	ReuseSession       bool
	PreviousRunID      string
	PreviousRunBatchID string
	quotaAllowance     usageLimitAllowance
}

type runSpecDetails struct {
	externalBlockers []int
	renderCfg        prompt.RenderConfig
	outputWriter     io.Writer
	review           bool
	reviewHeadSHA    string
	prNumber         int
	reviewFocus      string
	portalHidden     bool
	qualityRulesFile string
}

func runSpecFromRow(row RowSpec) (RunSpec, ContinuationCursor) {
	branch := row.Branches[row.IssueNumber]
	if branch == "" {
		branch = row.Branches[0]
	}
	return RunSpec{
			RunID: row.RunID, IssueNumber: row.IssueNumber, BatchID: row.BatchID,
			BatchDir: row.BatchDir, Branch: branch, BaseBranch: row.BaseBranch,
			RunTS: row.RunTS, RunShortID: row.RunShortID, BatchTS: row.BatchTS,
			BatchShortID: row.BatchShortID, UserProvidedRunID: row.UserProvidedRunID,
			details: &runSpecDetails{
				externalBlockers: append([]int(nil), row.ExternalBlockers...),
				renderCfg:        cloneRowRenderConfig(row.RenderCfg), outputWriter: row.OutputWriter, review: row.Review,
				reviewHeadSHA: row.ReviewHeadSHA, prNumber: row.PRNumber, reviewFocus: row.ReviewFocus,
				portalHidden: row.PortalHidden, qualityRulesFile: row.QualityRulesFile,
			},
		}, ContinuationCursor{
			Mode: row.Mode, ReuseSession: row.ReuseSession,
			PreviousRunID:      row.PreviousRunIDs[row.IssueNumber],
			PreviousRunBatchID: row.PreviousRunBatchIDs[row.IssueNumber],
			quotaAllowance:     row.quotaAllowance,
		}
}

func cloneRowRenderConfig(cfg prompt.RenderConfig) prompt.RenderConfig {
	if cfg.PromptArgs != nil {
		args := make(map[string]string, len(cfg.PromptArgs))
		for key, value := range cfg.PromptArgs {
			args[key] = value
		}
		cfg.PromptArgs = args
	}
	return cfg
}

func (run RunSpec) row(cursor ContinuationCursor) RowSpec {
	branches := map[int]string{run.IssueNumber: run.Branch}
	if run.IssueNumber == 0 || run.details != nil && run.details.review {
		branches[0] = run.Branch
	}
	row := RowSpec{
		IssueNumber: run.IssueNumber, Mode: cursor.Mode,
		Branches:            branches,
		PreviousRunIDs:      map[int]string{run.IssueNumber: cursor.PreviousRunID},
		PreviousRunBatchIDs: map[int]string{run.IssueNumber: cursor.PreviousRunBatchID},
		ReuseSession:        cursor.ReuseSession, quotaAllowance: cursor.quotaAllowance,
		BaseBranch: run.BaseBranch, RunTS: run.RunTS, RunShortID: run.RunShortID,
		BatchID: run.BatchID, BatchDir: run.BatchDir, BatchTS: run.BatchTS,
		BatchShortID: run.BatchShortID, RunID: run.RunID, UserProvidedRunID: run.UserProvidedRunID,
	}
	if run.details != nil {
		row.ExternalBlockers = append([]int(nil), run.details.externalBlockers...)
		row.RenderCfg, row.OutputWriter = cloneRowRenderConfig(run.details.renderCfg), run.details.outputWriter
		row.Review, row.ReviewHeadSHA = run.details.review, run.details.reviewHeadSHA
		row.PRNumber, row.ReviewFocus = run.details.prNumber, run.details.reviewFocus
		row.PortalHidden, row.QualityRulesFile = run.details.portalHidden, run.details.qualityRulesFile
	}
	return row
}

// RowSpec is the batch-assembly and compatibility value converted into an
// immutable RunSpec plus ContinuationCursor before it crosses RunExecutor.
// It groups the per-row-varying inputs that differ across a batch.
//
// Path discrimination is implicit, matching today's runSession: review rows
// remain prompt-only even when IssueNumber carries a linked issue; ordinary
// issue rows use RunTS/RunShortID while prompt-only/review rows use
// BatchTS/BatchShortID/RunID.
type RowSpec struct {
	IssueNumber         int
	Mode                IssueMode
	Branches            map[int]string
	PreviousRunIDs      map[int]string
	PreviousRunBatchIDs map[int]string
	ReuseSession        bool
	// quotaAllowance carries the implementation quota recovery state across row
	// execution, polling, and scheduler re-entry.
	quotaAllowance   usageLimitAllowance
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
	// prompt-only: from promptOnlyBatchLocation).
	BatchID string
	// BatchDir is BatchID's frozen directory when known (issue-driven: the
	// resolved RunDir). Empty means the canonical directory for BatchID.
	BatchDir string
	// Review / prompt-only fields.
	Review        bool
	ReviewHeadSHA string
	PRNumber      int
	ReviewFocus   string
	// PortalHidden preserves internal prompt-only visibility metadata through
	// the per-run execution seam.
	PortalHidden bool
	// QualityRulesFile is the host-absolute path of the
	// `.sandman/reviews/quality-rules.md` file the review daemon has
	// materialised. The orchestrator copies it into the per-row worktree
	// after the sandbox starts so the agent can read the rules at the
	// relative path the review prompt points at. Empty for non-review
	// runs.
	QualityRulesFile string
}

// BatchConfig carries the batch-constant knobs — inputs that are the same for
// every row in a batch. It is set once and used to build each runSession.
type BatchConfig struct {
	Cfg                        *config.Config
	AgentName                  string
	AgentCfg                   config.Agent
	Variant                    string
	IdentityResolver           *gitIdentityResolver
	Parallel                   int
	StartDelay                 time.Duration
	Retries                    int
	RunIdleTimeout             int
	ContextRolloverLiterals    []string
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
// private fields (wayfinder map #2226, #2234). Every field is
// batch-constant — set once per RunBatch and shared by every row.
type runDeps struct {
	githubClient             github.Client
	renderer                 prompt.IssueRenderer
	eventLog                 events.EventLog
	runnableFactory          RunnableFactory
	sandboxFactory           SandboxFactory
	layout                   paths.Layout
	heartbeatTickInterval    time.Duration
	closingGuardTickInterval time.Duration
	errorLog                 io.Writer
	runSessionOpts           runSessionOptions
	verifyPath               VerifyPathFunc
	quotaProgress            func(int)
	quotaLimit               func(int)
}

// runCoordination is the narrow interface the elevated executor uses for the
// shared, mutable coordination state that RunBatch owns (the active-runs map,
// the shutdown-supervisor fan-in, the per-batch phase writer, and the
// first-sandbox-start once). The per-batch *batchCoordinator implements it;
// RunBatch creates and owns one coordinator per batch. Keeping this an
// interface (rather than a raw struct handle) preserves the executor's
// testability — the constructor is the seam.
type runCoordination interface {
	registerActiveRun(key int, sb sandbox.Sandbox)
	unregisterActiveRun(key int)
	startCommandServer(issueNumber int, dir string, commander daemon.IssueCommander) error
	stopCommandServer(issueNumber int) error
	trackShutdownSupervisor(done <-chan struct{})
	currentPhaseWriter() io.Writer
	firstSandboxStart(sandboxStarted time.Time)
}

// RunExecutor is the attempt seam behind one immutable Run identity and its
// advancing continuation cursor. Path is implicit — Review rows are prompt-only
// even when they carry a linked IssueNumber; all other rows with an issue
// number are issue-driven.
type RunExecutor interface {
	ExecuteRun(ctx context.Context, run RunSpec, cursor ContinuationCursor) (AgentRunResult, bool)
}

// runExecutor is the RunExecutor implementation. It holds the batch-constant
// state (deps + coordination + commander + BatchConfig + factories + the
// RunBatch parent ctx) and builds a fresh runSession per ExecuteRun call. It has
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
// batch-constant dependencies off the orchestrator, pairs them with a fresh
// *batchCoordinator (which implements runCoordination / IssueCommander), and
// freezes them on the returned executor.
func (o *Orchestrator) newRunExecutor(parentCtx context.Context, bc BatchConfig, sbFactory SandboxFactory, containerAlloc containerAllocator) *runExecutor {
	coord := newBatchCoordinator(nil)
	return o.newRunExecutorWith(parentCtx, bc, sbFactory, containerAlloc, coord, coord, o.layout)
}

func (o *Orchestrator) newRunExecutorWith(parentCtx context.Context, bc BatchConfig, sbFactory SandboxFactory, containerAlloc containerAllocator, coord runCoordination, commander daemon.IssueCommander, layout paths.Layout) *runExecutor {
	return &runExecutor{
		deps: runDeps{
			githubClient:             o.githubClient,
			renderer:                 o.renderer,
			eventLog:                 o.eventLog,
			runnableFactory:          o.runnableFactory,
			sandboxFactory:           o.sandboxFactory,
			layout:                   layout,
			heartbeatTickInterval:    o.heartbeatTickInterval,
			closingGuardTickInterval: o.closingGuardTickInterval,
			errorLog:                 o.errorLog,
			runSessionOpts:           o.runSessionOpts,
			verifyPath:               o.verifyPath,
		},
		coord:          coord,
		commander:      commander,
		bc:             bc,
		sbFactory:      sbFactory,
		containerAlloc: containerAlloc,
		parentCtx:      parentCtx,
	}
}

// Execute runs one row through the shared lifecycle. Input policies are
// discriminated by the row's Review policy and IssueNumber. parentCtx (the RunBatch ctx)
// is batch-constant and held on the executor; ctx is the per-row ctx
// (per-issue for issue-driven, the RunBatch ctx for prompt-only).
func (e *runExecutor) ExecuteRun(ctx context.Context, run RunSpec, cursor ContinuationCursor) (AgentRunResult, bool) {
	return e.executeRow(ctx, run.row(cursor))
}

func (e *runExecutor) ObserveRun(ctx context.Context, run RunSpec, cursor ContinuationCursor) lifecycleVerdict {
	return e.observeRowLifecycle(ctx, run.row(cursor))
}

func (e *runExecutor) PersistAwait(ctx context.Context, run RunSpec, cursor ContinuationCursor, evidence map[string]any) error {
	return e.persistObservedAwait(ctx, run.row(cursor), evidence)
}

func (e *runExecutor) FinishObserved(ctx context.Context, run RunSpec, cursor ContinuationCursor, verdict lifecycleVerdict) AgentRunResult {
	return e.finishObserved(ctx, run.row(cursor), verdict)
}

// Execute is retained as a focused test/adapter helper while callers migrate
// to the immutable RunExecutor seam.
func (e *runExecutor) Execute(ctx context.Context, row RowSpec) (AgentRunResult, bool) {
	return e.executeRow(ctx, row)
}

func (e *runExecutor) executeRow(ctx context.Context, row RowSpec) (AgentRunResult, bool) {
	s := newRunSession(e, row)
	if s.quotaAllowance.probing() {
		if err := s.restoreQuotaAccounting(); err != nil {
			if s.deps.errorLog != nil {
				fmt.Fprintf(s.deps.errorLog, "warning: quota recovery accounting unavailable; continuing without additional quota waits: %v\n", err)
			}
		}
	}
	s.reuseSession = s.quotaAllowance.allowSessionReuse(s.reuseSession)
	if s.isIssueDriven() {
		if s.quotaAllowance.probing() {
			// A quota probe is not a terminal lifecycle authority. Verified
			// completion still wins without another agent/container launch.
			if ctx.Err() == nil {
				verdict := e.observeRowLifecycle(ctx, row)
				if verdict.supersedesQuotaProbe() {
					result := e.finishObserved(ctx, row, verdict)
					result.UsageLimitReached = true
					return result, false
				}
			}
			// The estimate does not veto the final quota probe or the
			// configured ordinary retry path. Poll accounting governs waits.
		}
		return s.execute(ctx)
	}
	return s.execute(ctx)
}

// newRunSession builds a runSession from the elevated seam's inputs. It
// centralizes the runSession construction that was previously duplicated
// verbatim between runSingle and runPromptOnlySingle. Behaviour-preserving:
// every field maps 1:1 to the prior literal assignments.
func newRunSession(e *runExecutor, row RowSpec) *runSession {
	bc := e.bc
	opts := e.deps.runSessionOpts
	if !opts.contextRolloverLiteralsSet {
		opts.contextRolloverLiterals = append([]string(nil), bc.ContextRolloverLiterals...)
	}
	renderCfg := row.RenderCfg
	if renderCfg.ReviewTimeout < config.MinReviewTimeout {
		if bc.Cfg != nil {
			renderCfg.ReviewTimeout = bc.Cfg.EffectiveReviewTimeout()
		} else {
			renderCfg.ReviewTimeout = config.DefaultReviewTimeout
		}
	}
	session := &runSession{
		deps:                       e.deps,
		coord:                      e.coord,
		commander:                  e.commander,
		issueNumber:                row.IssueNumber,
		cfg:                        bc.Cfg,
		agentName:                  bc.AgentName,
		agentCfg:                   bc.AgentCfg,
		variant:                    bc.Variant,
		mode:                       row.Mode,
		previousRunIDs:             row.PreviousRunIDs,
		previousRunBatchIDs:        row.PreviousRunBatchIDs,
		reuseSession:               row.ReuseSession,
		quotaAllowance:             row.quotaAllowance,
		identityResolver:           bc.IdentityResolver,
		branches:                   row.Branches,
		renderCfg:                  renderCfg,
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
		batchDir:                   row.BatchDir,
		batchTS:                    row.BatchTS,
		batchShortID:               row.BatchShortID,
		runID:                      row.RunID,
		userProvidedRunID:          row.UserProvidedRunID,
		review:                     row.Review,
		reviewHeadSHA:              row.ReviewHeadSHA,
		prNumber:                   row.PRNumber,
		reviewFocus:                row.ReviewFocus,
		portalHidden:               row.PortalHidden,
		qualityRulesFile:           row.QualityRulesFile,
		modelProgress: func() {
			if e.deps.quotaProgress != nil {
				e.deps.quotaProgress(row.IssueNumber)
			}
		},
		usageLimit: func() {
			if e.deps.quotaLimit != nil {
				e.deps.quotaLimit(row.IssueNumber)
			}
		},
		parentCtx:               e.parentCtx,
		opts:                    opts,
		reviewRegistrationStore: opts.reviewRegistrationStore,
		reviewRegistrationNow:   opts.reviewRegistrationNow,
	}
	return session
}
