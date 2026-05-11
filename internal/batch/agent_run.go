package batch

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

// AgentRun orchestrates the lifecycle of a single agent execution for an issue.
type AgentRun struct {
	issue   *github.Issue
	branch  string
	sandbox sandbox.Sandbox
	status  string
	prURL   string
}

// NewAgentRun creates an AgentRun for the given issue, branch, and sandbox.
func NewAgentRun(issue *github.Issue, branch string, sandbox sandbox.Sandbox) *AgentRun {
	return &AgentRun{
		issue:   issue,
		branch:  branch,
		sandbox: sandbox,
		status:  "success",
	}
}

// Prepare renders the prompt for the issue and writes it to the sandbox.
func (r *AgentRun) Prepare(renderer prompt.Renderer) error {
	rendered, err := renderer.Render(prompt.RenderConfig{}, prompt.IssueData{
		Number: r.issue.Number,
		Title:  r.issue.Title,
		Body:   r.issue.Body,
	})
	if err != nil {
		return fmt.Errorf("render prompt: %w", err)
	}

	if err := r.sandbox.WritePrompt(rendered); err != nil {
		return fmt.Errorf("write prompt: %w", err)
	}
	return nil
}

// Execute runs the agent command inside the sandbox, writing prefixed output to the given writers
// and un-prefixed output to .sandman/logs/<issue>.log.
func (r *AgentRun) Execute(ctx context.Context, command string, stdout, stderr io.Writer) error {
	logDir := ".sandman/logs"
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	logPath := filepath.Join(logDir, fmt.Sprintf("%d.log", r.issue.Number))
	logFile, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("create log file: %w", err)
	}
	defer logFile.Close()

	prefixedOut := NewLinePrefixWriter(r.issue.Number, stdout)
	prefixedErr := NewLinePrefixWriter(r.issue.Number, stderr)

	combinedOut := io.MultiWriter(logFile, prefixedOut)
	combinedErr := io.MultiWriter(logFile, prefixedErr)

	if err := r.sandbox.Exec(ctx, command, combinedOut, combinedErr); err != nil {
		return fmt.Errorf("execute agent: %w", err)
	}
	return nil
}

// Finalize reads the run result, creates a PR, and records the PR URL.
func (r *AgentRun) Finalize(client github.Client, defaultBranch string) error {
	prTitle := r.issue.Title
	prBody := r.issue.Body
	if rr, err := r.sandbox.ReadRunResult(); err == nil && rr != nil {
		if rr.Title != "" {
			prTitle = rr.Title
		}
		if rr.Body != "" {
			prBody = rr.Body
		}
	}
	if r.issue.Number > 0 {
		prBody += fmt.Sprintf("\n\nFixes #%d", r.issue.Number)
	}

	prURL, err := client.CreatePR(r.branch, defaultBranch, prTitle, prBody)
	if err != nil {
		return fmt.Errorf("create PR: %w", err)
	}
	r.prURL = prURL
	return nil
}

// Run executes the full lifecycle of the AgentRun and returns the result.
func (r *AgentRun) Run(ctx context.Context, renderer prompt.Renderer, command string, client github.Client, defaultBranch string) AgentRunResult {
	if err := r.Prepare(renderer); err != nil {
		r.status = "failure"
		return r.Result()
	}
	if err := r.Execute(ctx, command, os.Stdout, os.Stderr); err != nil {
		r.status = "failure"
		return r.Result()
	}
	if err := r.Finalize(client, defaultBranch); err != nil {
		r.status = "failure"
		return r.Result()
	}
	return r.Result()
}

// Result returns the current outcome of the AgentRun.
func (r *AgentRun) Result() AgentRunResult {
	return AgentRunResult{
		IssueNumber: r.issue.Number,
		Status:      r.status,
		Branch:      r.branch,
		PRURL:       r.prURL,
	}
}
