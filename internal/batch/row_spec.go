package batch

import (
	"context"
	"io"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/prompt"
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

// newRunSession builds a runSession from the elevated seam's inputs. It
// centralizes the runSession construction that was previously duplicated
// verbatim between runSingle and runPromptOnlySingle. Behaviour-preserving:
// every field maps 1:1 to the prior literal assignments.
func newRunSession(o *Orchestrator, row RowSpec, bc BatchConfig, sbFactory SandboxFactory, containerAlloc containerAllocator, parentCtx context.Context) *runSession {
	return &runSession{
		o:                          o,
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
		sbFactory:                  sbFactory,
		containerAlloc:             containerAlloc,
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
		parentCtx:                  parentCtx,
		opts:                       o.runSessionOpts,
	}
}
