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
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
	"github.com/rafaelromao/sandman/internal/shellenv"
)

// AgentRun orchestrates the lifecycle of a single agent execution for an issue.
type AgentRun struct {
	issue                      *github.Issue
	branch                     string
	baseBranch                 string
	runID                      string
	review                     bool
	preset                     string
	model                      string
	modelProvider              string
	modelName                  string
	dangerouslySkipPermissions *bool
	opencodePermissionMode     string
	sessionName                string
	sandbox                    sandbox.Sandbox
	status                     string
	env                        map[string]string
	outputWriter               io.Writer
	layout                     paths.Layout
}

// NewAgentRun creates an AgentRun for the given issue, branch, and sandbox.
// The run uses a Layout rooted at the current working directory, matching
// the pre-Layout behaviour for callers that have not migrated yet.
func NewAgentRun(issue *github.Issue, branch string, sandbox sandbox.Sandbox) *AgentRun {
	return NewAgentRunWithLayout(issue, branch, sandbox, paths.NewLayout(&config.Config{}, "."))
}

// NewAgentRunWithLayout creates an AgentRun that resolves its log directory
// and filename through the supplied paths.Layout, so the run is rooted at
// the layout's RepoRoot regardless of the current working directory.
func NewAgentRunWithLayout(issue *github.Issue, branch string, sandbox sandbox.Sandbox, layout paths.Layout) *AgentRun {
	return &AgentRun{
		issue:   issue,
		branch:  branch,
		sandbox: sandbox,
		status:  "success",
		layout:  layout,
	}
}

// Prepare renders the prompt for the issue and writes it to the sandbox.
func (r *AgentRun) Prepare(renderer prompt.IssueRenderer, cfg prompt.RenderConfig) error {
	issue := r.issueData()
	rendered, err := renderer.Render(cfg, prompt.IssueData{
		Number:       issue.Number,
		Title:        issue.Title,
		Body:         issue.Body,
		SourceBranch: r.branch,
		BaseBranch:   r.baseBranch,
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
// and to the layout's LogDir (saved log file is also prefixed).
func (r *AgentRun) Execute(ctx context.Context, command string, stdout, stderr io.Writer) error {
	logDir := r.layout.LogDir
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	var logName string
	if r.issue != nil {
		logName = fmt.Sprintf("%d.log", r.issue.Number)
	} else {
		logName = r.layout.SafeLogFilename(strings.TrimSpace(r.runID)) + ".log"
	}
	logPath := filepath.Join(logDir, logName)
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
	logPrefixedOut := NewLinePrefixWriter(r.prefixLabel(), logFile)
	logPrefixedErr := NewLinePrefixWriter(r.prefixLabel(), logFile)

	combinedOut := io.MultiWriter(prefixedOut, logPrefixedOut)
	combinedErr := io.MultiWriter(prefixedErr, logPrefixedErr)

	if err := r.sandbox.Exec(ctx, command, combinedOut, combinedErr); err != nil {
		return fmt.Errorf("execute agent: %w", err)
	}
	_ = prefixedOut.Flush()
	_ = prefixedErr.Flush()
	_ = logPrefixedOut.Flush()
	_ = logPrefixedErr.Flush()
	return nil
}

// Run executes the full lifecycle of the AgentRun and returns the result.
func (r *AgentRun) Run(ctx context.Context, renderer prompt.IssueRenderer, command string, renderCfg prompt.RenderConfig) AgentRunResult {
	renderedPromptFile := renderCfg.RenderedPromptFile
	if renderedPromptFile == "" {
		renderedPromptFile = filepath.Join(".", ".sandman", "task.md")
	}

	if renderCfg.TaskPrompt != "" {
		if err := r.writeTaskPrompt(renderedPromptFile, renderCfg.TaskPrompt); err != nil {
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
		PromptFile:                 renderedPromptFile,
		ModelFlag:                  r.modelFlag(command),
		ModelProvider:              r.modelProvider,
		ModelName:                  r.modelName,
		DangerouslySkipPermissions: r.dangerouslySkipPermissions != nil && *r.dangerouslySkipPermissions,
		SessionName:                r.sessionName,
	})
	if err != nil {
		r.status = "failure"
		return r.Result()
	}
	renderedCmd, err = r.prependEnv(renderedCmd)
	if err != nil {
		// The typed *shellenv.InvalidKeyError from shellenv.Build is
		// intentionally not surfaced to the run result: the run is
		// marked "failure" with a generic status. A future caller
		// that wants per-key error reporting can inspect the typed
		// error returned by prependEnv directly.
		r.status = "failure"
		return r.Result()
	}

	if err := r.Execute(ctx, renderedCmd, os.Stdout, os.Stderr); err != nil {
		r.status = "failure"
		return r.Result()
	}
	return r.Result()
}

// prependEnv returns command prefixed with `export KEY=VALUE; ...` entries
// for r.env. The opencode permission skip rule still applies: the
// OPENCODE_PERMISSION entry is dropped when the opencode preset is in
// "builtin" mode and the rendered command does not request
// --dangerously-skip-permissions. The *shellenv.InvalidKeyError returned
// by shellenv.Build is propagated unchanged so the caller can surface a
// typed failure.
func (r *AgentRun) prependEnv(command string) (string, error) {
	if len(r.env) == 0 {
		return command, nil
	}
	applyOpencodePermission := strings.Contains(command, "--dangerously-skip-permissions")
	filtered := make(map[string]string, len(r.env))
	for key, value := range r.env {
		if key == "OPENCODE_PERMISSION" && r.opencodePermissionMode == "builtin" && !applyOpencodePermission {
			continue
		}
		filtered[key] = value
	}
	if len(filtered) == 0 {
		return command, nil
	}
	return shellenv.Build(filtered, command)
}

func (r *AgentRun) writeTaskPrompt(renderedPromptFile, content string) error {
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
	var issueRefPtr *int
	if r.issue != nil {
		issueRefPtr = issueRef(issue.Number)
	}
	return AgentRunResult{
		IssueNumber:  issue.Number,
		Issue:        issueRefPtr,
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

func (r *AgentRun) prefixLabel() string {
	if r.issue != nil {
		return fmt.Sprintf("issue-%d", r.issue.Number)
	}
	if r.review && r.runID != "" {
		return r.runID
	}
	return "prompt-only"
}
