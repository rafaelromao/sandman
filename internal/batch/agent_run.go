package batch

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

// AgentRun orchestrates the lifecycle of a single agent execution for an issue.
type AgentRun struct {
	issue         *github.Issue
	branch        string
	defaultBranch string
	preset        string
	model         string
	modelProvider string
	modelName     string
	sandbox       sandbox.Sandbox
	status        string
	env           map[string]string
	outputWriter  io.Writer
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
	issue := r.issueData()
	rendered, err := renderer.Render(cfg, prompt.IssueData{
		Number:       issue.Number,
		Title:        issue.Title,
		Body:         issue.Body,
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
	logPath := filepath.Join(logDir, r.logFileName())
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("create log file: %w", err)
	}
	defer logFile.Close()

	if r.outputWriter != nil {
		stdout = io.MultiWriter(stdout, r.outputWriter)
		stderr = io.MultiWriter(stderr, r.outputWriter)
	}

	prefixedOut := NewLinePrefixWriter(r.prefixLabel(), stdout)
	prefixedErr := NewLinePrefixWriter(r.prefixLabel(), stderr)

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
func (r *AgentRun) Run(ctx context.Context, renderer prompt.Renderer, command string, renderCfg prompt.RenderConfig) AgentRunResult {
	renderedPromptFile := renderCfg.RenderedPromptFile
	if renderedPromptFile == "" {
		renderedPromptFile = filepath.Join(".", ".sandman", "rendered-prompt.md")
	}

	if renderCfg.ContinuePrompt != "" {
		renderedPromptFile = filepath.Join(".", ".sandman", "continue-prompt.md")
		if err := r.writeContinuePrompt(renderedPromptFile, renderCfg.ContinuePrompt); err != nil {
			r.status = "failure"
			return r.Result()
		}
	} else {
		if err := r.Prepare(renderer, renderCfg); err != nil {
			r.status = "failure"
			return r.Result()
		}
	}

	renderedCmd, err := RenderCommand(command, CommandData{
		PromptFile:    renderedPromptFile,
		ModelFlag:     r.modelFlag(command),
		ModelProvider: r.modelProvider,
		ModelName:     r.modelName,
	})
	if err != nil {
		r.status = "failure"
		return r.Result()
	}
	renderedCmd = applyAgentEnv(renderedCmd, r.env)

	if err := r.Execute(ctx, renderedCmd, os.Stdout, os.Stderr); err != nil {
		r.status = "failure"
		return r.Result()
	}
	return r.Result()
}

func (r *AgentRun) writeContinuePrompt(renderedPromptFile, content string) error {
	promptPath := filepath.Join(r.sandbox.WorkDir(), renderedPromptFile)
	if err := os.MkdirAll(filepath.Dir(promptPath), 0755); err != nil {
		return fmt.Errorf("create prompt dir: %w", err)
	}
	if err := os.WriteFile(promptPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("write prompt: %w", err)
	}
	return nil
}

func (r *AgentRun) modelFlag(command string) string {
	model := strings.TrimSpace(r.model)
	if model == "" || r.preset == "" {
		return ""
	}
	preset, ok := config.BuiltInAgentPresets[r.preset]
	if !ok || preset.Command != command {
		return ""
	}
	switch r.preset {
	case "opencode":
		return "-m " + model
	default:
		return ""
	}
}

// Result returns the current outcome of the AgentRun.
func (r *AgentRun) Result() AgentRunResult {
	issue := r.issueData()
	return AgentRunResult{
		IssueNumber:  issue.Number,
		Issue:        r.issuePointer(),
		Status:       r.status,
		Branch:       r.branch,
		WorktreePath: r.sandbox.WorkDir(),
	}
}

func (r *AgentRun) issueData() github.Issue {
	if r.issue != nil {
		return *r.issue
	}
	return github.Issue{}
}

func (r *AgentRun) issuePointer() *int {
	if r.issue == nil {
		return nil
	}
	n := r.issue.Number
	return &n
}

func (r *AgentRun) prefixLabel() string {
	if r.issue != nil {
		return fmt.Sprintf("issue-%d", r.issue.Number)
	}
	return "prompt-only"
}

func (r *AgentRun) logFileName() string {
	if r.issue != nil {
		return fmt.Sprintf("%d.log", r.issue.Number)
	}
	name := strings.NewReplacer("/", "-", string(os.PathSeparator), "-", " ", "-").Replace(r.branch)
	if name == "" {
		name = "prompt-only"
	}
	return name + ".log"
}
