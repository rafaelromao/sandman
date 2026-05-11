package batch

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

// Orchestrator coordinates parallel AgentRun execution.
type Orchestrator struct {
	githubClient github.Client
	renderer     prompt.Renderer
	configStore  config.Store
	eventLog     events.EventLog
}

// NewOrchestrator creates an Orchestrator with the given dependencies.
func NewOrchestrator(githubClient github.Client, renderer prompt.Renderer, configStore config.Store, eventLog events.EventLog) *Orchestrator {
	return &Orchestrator{
		githubClient: githubClient,
		renderer:     renderer,
		configStore:  configStore,
		eventLog:     eventLog,
	}
}

// RunBatch executes the requested AgentRuns in parallel.
func (o *Orchestrator) RunBatch(ctx context.Context, req Request) (*Result, error) {
	cfg, err := o.configStore.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	var runs []AgentRunResult
	for _, num := range req.Issues {
		issue, err := o.githubClient.FetchIssue(num)
		if err != nil {
			return nil, fmt.Errorf("fetch issue %d: %w", num, err)
		}

		branch := fmt.Sprintf("sandman/%d-%s", issue.Number, slugify(issue.Title))
		// TODO: detect repo root instead of hardcoding "."
		wt := sandbox.NewWorktreeSandbox(".", cfg.WorktreeDir, branch, cfg.Git.DefaultBranch)
		if err := wt.Start(); err != nil {
			return nil, fmt.Errorf("start worktree for issue %d: %w", num, err)
		}

		run := NewAgentRun(issue, branch, wt)

		if err := run.Prepare(o.renderer); err != nil {
			return nil, fmt.Errorf("prepare agent run for issue %d: %w", num, err)
		}

		agentCfg, ok := cfg.AgentProviders[cfg.Agent]
		if !ok {
			return nil, fmt.Errorf("agent provider %q not found in config", cfg.Agent)
		}

		// TODO: respect req.Parallel for concurrent execution.
		// TODO: log run started/finished events to eventLog.
		if err := run.Execute(ctx, agentCfg.Command); err != nil {
			runs = append(runs, AgentRunResult{IssueNumber: issue.Number, Branch: branch, Status: "failure"})
			// TODO: clean up worktree on partial failure.
			return &Result{Runs: runs}, fmt.Errorf("execute agent for issue %d: %w", num, err)
		}

		if err := run.Finalize(o.githubClient, cfg.Git.DefaultBranch); err != nil {
			runs = append(runs, AgentRunResult{IssueNumber: issue.Number, Branch: branch, Status: "failure"})
			return &Result{Runs: runs}, fmt.Errorf("create PR for issue %d: %w", num, err)
		}

		runs = append(runs, run.Result())
	}
	return &Result{Runs: runs}, nil
}

func slugify(title string) string {
	var result []rune
	for _, r := range strings.ToLower(title) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			result = append(result, r)
		} else if r == ' ' || r == '-' || r == '_' {
			if len(result) > 0 && result[len(result)-1] != '-' {
				result = append(result, '-')
			}
		}
	}
	if len(result) > 0 && result[len(result)-1] == '-' {
		result = result[:len(result)-1]
	}
	return string(result)
}

// Ensure Orchestrator implements Runner.
var _ Runner = (*Orchestrator)(nil)
