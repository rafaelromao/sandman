package batch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	sandbox      sandbox.Sandbox
	eventLog     events.EventLog
}

// NewOrchestrator creates an Orchestrator with the given dependencies.
func NewOrchestrator(githubClient github.Client, renderer prompt.Renderer, configStore config.Store, sandbox sandbox.Sandbox, eventLog events.EventLog) *Orchestrator {
	return &Orchestrator{
		githubClient: githubClient,
		renderer:     renderer,
		configStore:  configStore,
		sandbox:      sandbox,
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

		rendered, err := o.renderer.Render(prompt.RenderConfig{}, prompt.IssueData{
			Number: issue.Number,
			Title:  issue.Title,
			Body:   issue.Body,
		})
		if err != nil {
			return nil, fmt.Errorf("render prompt for issue %d: %w", num, err)
		}

		branch := fmt.Sprintf("sandman/%d-%s", issue.Number, slugify(issue.Title))
		wt := sandbox.NewWorktreeSandbox(".", cfg.WorktreeDir, branch, "main")
		if err := wt.Start(); err != nil {
			return nil, fmt.Errorf("start worktree for issue %d: %w", num, err)
		}

		promptPath := filepath.Join(wt.WorkDir(), ".sandman", "prompt.md")
		if err := os.MkdirAll(filepath.Dir(promptPath), 0755); err != nil {
			return nil, fmt.Errorf("create prompt dir: %w", err)
		}
		if err := os.WriteFile(promptPath, []byte(rendered), 0644); err != nil {
			return nil, fmt.Errorf("write prompt: %w", err)
		}

		agentCfg, ok := cfg.AgentProviders[cfg.Agent]
		if !ok {
			return nil, fmt.Errorf("agent provider %q not found in config", cfg.Agent)
		}

		status := "success"
		if err := wt.Exec(ctx, wt.WorkDir(), agentCfg.Command); err != nil {
			status = "failure"
			runs = append(runs, AgentRunResult{IssueNumber: issue.Number, Branch: branch, Status: status})
			return nil, fmt.Errorf("execute agent for issue %d: %w", num, err)
		}

		runs = append(runs, AgentRunResult{IssueNumber: issue.Number, Branch: branch, Status: status})
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
