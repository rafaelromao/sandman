package batch

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/prompt"
)

// This file holds the legacy positional packer shims, retained for the direct
// test call sites in *_test.go (49 sites: 41 runSingle + 8 runPromptOnlySingle).
//
// Production routes exclusively through the RowSpec seams (runSingleRow /
// runPromptOnlyRow in orchestrator.go); no production caller references these.
// They live in a _test.go file so they do not ship in the binary. They are
// removed when the test call sites migrate to construct RowSpec+BatchConfig
// directly (wayfinder #2231).

// runSingle is the legacy positional entry point that collapses to a RowSpec +
// BatchConfig and delegates to runSingleRow.
func (o *Orchestrator) runSingle(ctx context.Context, parentCtx context.Context, num int, cfg *config.Config, agentName string, agentCfg config.Agent, continuation bool, previousRunIDs map[int]string, identityResolver *gitIdentityResolver, branches map[int]string, renderCfg prompt.RenderConfig, outputWriter io.Writer, sbFactory SandboxFactory, containerAlloc containerAllocator, override bool, baseBranch string, externalBlockers []int, parallel int, startDelay time.Duration, retries int, runIdleTimeout int, sandboxMode string, containerCapacity int, containerCapacitySet bool, maxContainers int, maxContainersSet bool, dangerouslySkipPermissions bool, strandedReconcile bool, runTS string, runShortID string, batchID ...string) (AgentRunResult, bool) {
	issueBatchID := ""
	if len(batchID) > 0 {
		issueBatchID = strings.TrimSpace(batchID[0])
	}
	if issueBatchID == "" && (runTS != "" || runShortID != "") {
		issueBatchID = batchIDFromRunID(buildRunID(num, runTS, runShortID))
	}
	mode := ModeFresh
	if continuation {
		mode = ModeContinue
	} else if override {
		mode = ModeOverride
	}
	row := RowSpec{
		IssueNumber:      num,
		Mode:             mode,
		Branches:         branches,
		PreviousRunIDs:   previousRunIDs,
		BaseBranch:       baseBranch,
		ExternalBlockers: externalBlockers,
		RenderCfg:        renderCfg,
		OutputWriter:     outputWriter,
		RunTS:            runTS,
		RunShortID:       runShortID,
		BatchID:          issueBatchID,
	}
	bc := BatchConfig{
		Cfg:                        cfg,
		AgentName:                  agentName,
		AgentCfg:                   agentCfg,
		IdentityResolver:           identityResolver,
		Parallel:                   parallel,
		StartDelay:                 startDelay,
		Retries:                    retries,
		RunIdleTimeout:             runIdleTimeout,
		SandboxMode:                sandboxMode,
		ContainerCapacity:          containerCapacity,
		ContainerCapacitySet:       containerCapacitySet,
		MaxContainers:              maxContainers,
		MaxContainersSet:           maxContainersSet,
		DangerouslySkipPermissions: dangerouslySkipPermissions,
		StrandedReconcile:          strandedReconcile,
	}
	return o.runSingleRow(ctx, parentCtx, row, bc, sbFactory, containerAlloc)
}

// runPromptOnlySingle is the legacy positional entry point that collapses to a
// RowSpec + BatchConfig and delegates to runPromptOnlyRow.
func (o *Orchestrator) runPromptOnlySingle(ctx context.Context, cfg *config.Config, agentName string, agentCfg config.Agent, identityResolver *gitIdentityResolver, branch string, renderCfg prompt.RenderConfig, outputWriter io.Writer, sbFactory SandboxFactory, containerAlloc containerAllocator, mode IssueMode, baseBranch string, startDelay time.Duration, parallel int, retries int, sandboxMode string, containerCapacity int, containerCapacitySet bool, maxContainers int, maxContainersSet bool, dangerouslySkipPermissions bool, strandedReconcile bool, review bool, prNumber int, reviewFocus string, runID string, previousRunIDs map[int]string, reviewIssueNumber int, batchTS string, batchShortID string, runDir string) (AgentRunResult, bool) {
	row := RowSpec{
		IssueNumber:       reviewIssueNumber,
		Mode:              mode,
		Branches:          map[int]string{0: branch},
		PreviousRunIDs:    previousRunIDs,
		BaseBranch:        baseBranch,
		RenderCfg:         renderCfg,
		OutputWriter:      outputWriter,
		BatchID:           batchIDForPromptOnly(batchTS, batchShortID, runID, runDir),
		BatchTS:           batchTS,
		BatchShortID:      batchShortID,
		RunID:             runID,
		UserProvidedRunID: runID,
		Review:            review,
		PRNumber:          prNumber,
		ReviewFocus:       reviewFocus,
	}
	bc := BatchConfig{
		Cfg:                        cfg,
		AgentName:                  agentName,
		AgentCfg:                   agentCfg,
		IdentityResolver:           identityResolver,
		Parallel:                   parallel,
		StartDelay:                 startDelay,
		Retries:                    retries,
		SandboxMode:                sandboxMode,
		ContainerCapacity:          containerCapacity,
		ContainerCapacitySet:       containerCapacitySet,
		MaxContainers:              maxContainers,
		MaxContainersSet:           maxContainersSet,
		DangerouslySkipPermissions: dangerouslySkipPermissions,
		StrandedReconcile:          strandedReconcile,
	}
	return o.runPromptOnlyRow(ctx, row, bc, sbFactory, containerAlloc)
}
