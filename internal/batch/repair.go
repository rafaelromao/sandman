package batch

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/runid"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

type repairSandboxFactory struct {
	repoPath, promptPath string
}

var canonicalRepairID = regexp.MustCompile(`^\d{12}-[0-9a-f]{4}-prompt(?:-[a-zA-Z0-9_-]+)?$`)

func (f repairSandboxFactory) NewSandbox(string, string, string, string, sandbox.Container) sandbox.Sandbox {
	return sandbox.NewRepositoryHostSandbox(f.repoPath, f.promptPath)
}

// RunRepair is the optional production adapter for repository-scoped diagnostic
// repairs. Admission/attempt ownership belongs to its caller; execution uses the
// ordinary prompt-only lifecycle with a reserved identity and no retries.
func (o *Orchestrator) RunRepair(ctx context.Context, req Request, cfg *config.Config) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(req.Issues) != 0 || req.Review || req.IssueNumber != 0 || req.PRNumber != 0 {
		return nil, fmt.Errorf("repair requires prompt-only input without issue or review metadata")
	}
	if req.RunID == "" {
		return nil, fmt.Errorf("repair requires a reserved RunID")
	}
	if err := runid.IsValidUserRunID(req.RunID); err != nil && !canonicalRepairID.MatchString(req.RunID) {
		return nil, fmt.Errorf("repair requires a valid reserved RunID: %w", err)
	}
	if req.PromptConfig.TaskPrompt == "" && req.PromptConfig.PromptFlag == "" && req.PromptConfig.TemplateFlag == "" {
		return nil, fmt.Errorf("repair requires a diagnostic prompt")
	}
	// Use the daemon's already validated snapshot: configuration reload may
	// itself be the blocked operation that this agent needs to repair.
	if cfg == nil {
		return nil, fmt.Errorf("repair requires the daemon configuration snapshot")
	}
	agentName := strings.TrimSpace(req.Agent)
	if agentName == "" {
		agentName = cfg.DefaultAgent
	}
	if agentName == "" {
		agentName = cfg.Agent
	}
	agentCfg, err := cfg.ResolveAgentProvider(agentName)
	if err != nil {
		return nil, err
	}
	if model := strings.TrimSpace(req.Model); model != "" {
		if agentCfg.Preset == "" {
			return nil, fmt.Errorf("model override is only supported for built-in presets")
		}
		agentCfg.Model = model
	}
	if err := sandbox.ValidateAgentConfig(agentName, agentCfg); err != nil {
		return nil, err
	}
	reviewTimeout, err := resolveReviewTimeout(req, cfg)
	if err != nil {
		return nil, err
	}
	req.PromptConfig.ReviewTimeout = reviewTimeout
	// Reserved repair IDs are exact lifecycle identities, not prompt subjects.
	req.BatchTS, req.BatchShortID = "", ""
	req.Retries = 0
	req.Mode, req.PreviousRunIDs, req.PreviousRunBatchIDs, req.ReuseSession = nil, nil, nil, nil
	layout := paths.NewLayout(cfg, o.layout.RepoRoot)
	batchID := req.RunID
	batchDir := layout.BatchDir(batchID)
	if req.RunDir != "" && filepath.Clean(req.RunDir) != filepath.Clean(batchDir) {
		return nil, fmt.Errorf("repair batch directory does not match reserved identity")
	}
	session := daemon.NewRunSession(layout.SandmanDir, batchID)
	defer session.Close()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := session.Prepare(daemon.BatchManifest{BatchId: batchID, CreatedAt: time.Now(), RunKind: "prompt-only", PortalHidden: req.PortalHidden, Issues: []int{}}); err != nil {
		return nil, fmt.Errorf("prepare repair artifacts: %w", err)
	}
	// The prompt-only compatibility adapter derives batch identity from this
	// canonical per-run path, not the batch-root Request documentation.
	req.RunDir = layout.RunFolder(batchID, req.RunID)
	promptPath := filepath.Join(layout.RunFolder(batchID, req.RunID), "task.md")
	renderedPromptPath, err := filepath.Rel(layout.RepoRoot, promptPath)
	if err != nil {
		return nil, fmt.Errorf("resolve repair prompt path: %w", err)
	}
	req.PromptConfig.RenderedPromptFile = renderedPromptPath
	req.PromptConfig.Branch = req.RunID
	// A fresh orchestrator owns its own mutexes and coordinator state. Preserve
	// injected execution/strategy hooks through the existing constructor options.
	opts := o.runSessionOpts
	opts.repairHost = true
	opts.baseBranchSyncMu = nil
	sbFactory := repairSandboxFactory{repoPath: layout.RepoRoot, promptPath: promptPath}
	repair := NewOrchestrator(o.githubClient, o.renderer, o.configStore, o.eventLog,
		WithRunnableFactory(o.runnableFactory), WithSandboxFactory(sbFactory),
		WithErrorLog(o.errorLog), WithRunSessionOpts(opts),
		WithHeartbeatTickInterval(o.heartbeatTickInterval))
	coord := newBatchCoordinator(req.PhaseWriter)
	skipPermissions := req.DangerouslySkipPermissions != nil && *req.DangerouslySkipPermissions
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return repair.runPromptOnly(ctx, cfg, agentName, agentCfg, nil, sbFactory, nil,
		req, "", 0, 1, 0, resolveRunIdleTimeout(req, cfg), "worktree", 0, false, 0, false,
		skipPermissions, false, coord, layout)
}
