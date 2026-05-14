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
	issue         *github.Issue
	branch        string
	defaultBranch string
	sandbox       sandbox.Sandbox
	status        string
	env           map[string]string
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
func (r *AgentRun) Prepare(renderer prompt.Renderer, cfg prompt.RenderConfig) error {
	rendered, err := renderer.Render(cfg, prompt.IssueData{
		Number:       r.issue.Number,
		Title:        r.issue.Title,
		Body:         r.issue.Body,
		SourceBranch: r.branch,
		TargetBranch: r.defaultBranch,
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
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
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
	_ = prefixedOut.Flush()
	_ = prefixedErr.Flush()
	return nil
}

// Run executes the full lifecycle of the AgentRun and returns the result.
func (r *AgentRun) Run(ctx context.Context, renderer prompt.Renderer, command string, interactive bool, renderCfg prompt.RenderConfig) AgentRunResult {
	if err := r.Prepare(renderer, renderCfg); err != nil {
		r.status = "failure"
		return r.Result()
	}

	renderedCmd, err := RenderCommand(command, CommandData{
		PromptFile: renderCfg.PromptFile,
	})
	if err != nil {
		r.status = "failure"
		return r.Result()
	}
	renderedCmd = applyAgentEnv(renderedCmd, r.env)

	if interactive {
		if err := r.sandbox.ExecInteractive(ctx, renderedCmd); err != nil {
			r.status = "failure"
			return r.Result()
		}
	} else {
		if err := r.Execute(ctx, renderedCmd, os.Stdout, os.Stderr); err != nil {
			r.status = "failure"
			return r.Result()
		}
	}
	return r.Result()
}

// Result returns the current outcome of the AgentRun.
func (r *AgentRun) Result() AgentRunResult {
	return AgentRunResult{
		IssueNumber: r.issue.Number,
		Status:      r.status,
		Branch:      r.branch,
	}
}
