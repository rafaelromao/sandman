package batch

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"unicode"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

// Orchestrator coordinates parallel AgentRun execution.
type Orchestrator struct {
	githubClient    github.Client
	renderer        prompt.Renderer
	configStore     config.Store
	eventLog        events.EventLog
	runnableFactory RunnableFactory
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

	parallel := req.Parallel
	if parallel == 0 {
		parallel = 4
	}

	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	results := make([]AgentRunResult, len(req.Issues))
	var mu sync.Mutex
	failureCount := 0

	for i, num := range req.Issues {
		wg.Add(1)
		go func(idx, issueNum int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			res := o.runSingle(ctx, issueNum, cfg)
			mu.Lock()
			results[idx] = res
			if res.Status == "failure" {
				failureCount++
			}
			mu.Unlock()
		}(i, num)
	}

	wg.Wait()

	if failureCount > 0 {
		return &Result{Runs: results}, fmt.Errorf("%d of %d runs failed", failureCount, len(req.Issues))
	}
	return &Result{Runs: results}, nil
}

func (o *Orchestrator) runSingle(ctx context.Context, num int, cfg *config.Config) AgentRunResult {
	issue, err := o.githubClient.FetchIssue(num)
	if err != nil {
		return AgentRunResult{IssueNumber: num, Status: "failure"}
	}

	branch := fmt.Sprintf("sandman/%d-%s", issue.Number, slugify(issue.Title))
	// TODO: detect repo root instead of hardcoding "."
	wt := sandbox.NewWorktreeSandbox(".", cfg.WorktreeDir, branch, cfg.Git.DefaultBranch)
	if err := wt.Start(); err != nil {
		return AgentRunResult{IssueNumber: num, Status: "failure", Branch: branch}
	}

	factory := o.runnableFactory
	if factory == nil {
		factory = defaultRunnableFactory{}
	}
	runnable := factory.NewRunnable(issue, branch, wt)

	agentCfg, ok := cfg.AgentProviders[cfg.Agent]
	if !ok {
		return AgentRunResult{IssueNumber: num, Status: "failure", Branch: branch}
	}

	// TODO: log run started/finished events to eventLog.
	result := runnable.Run(ctx, o.renderer, agentCfg.Command, o.githubClient, cfg.Git.DefaultBranch)
	return result
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
