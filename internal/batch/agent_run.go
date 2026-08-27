package batch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
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
	batchID                    string
	previousBatchID            string
	previousRunID              string
	reuseSession               bool
	review                     bool
	preset                     string
	model                      string
	modelProvider              string
	modelName                  string
	variant                    string
	dangerouslySkipPermissions *bool
	opencodePermissionMode     string
	sessionName                string
	sandbox                    sandbox.Sandbox
	status                     string
	contextExhausted           bool
	cleanupError               error // distinct cleanup failure from context cancellation
	contextRolloverLiterals    []string
	env                        map[string]string
	outputWriter               io.Writer
	layout                     paths.Layout
	runFolder                  string
	taskWriter                 func(string, []byte, os.FileMode) error
	sessionWarning             io.Writer
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
		issue:      issue,
		branch:     branch,
		sandbox:    sandbox,
		status:     "success",
		layout:     layout,
		taskWriter: atomicfs.WriteAtomic,
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
	if !r.review {
		rendered = prompt.EnsureReviewTimeoutContext(rendered, cfg.ReviewTimeout)
	}

	if err := r.sandbox.WritePrompt(rendered); err != nil {
		return fmt.Errorf("write prompt: %w", err)
	}
	return nil
}

// Execute runs the agent command inside the sandbox, writing prefixed output to the given writers
// and to the run folder's run.log (constant filename, O_APPEND preserved).
//
// When the sandbox reports a cleanup failure (via *sandbox.CleanupError),
// Execute propagates it through the returned error so Run can record it
// on the AgentRunResult (issue #2605 acceptance criterion #4).
func (r *AgentRun) Execute(ctx context.Context, command string, stdout, stderr io.Writer) error {
	return r.execute(ctx, command, stdout, stderr, nil, nil)
}

func (r *AgentRun) execute(ctx context.Context, command string, stdout, stderr io.Writer, parsedStdout, parsedStderr *opencodeOutput) error {
	runFolder := r.runFolder
	if runFolder == "" {
		runFolder = r.sandbox.WorkDir()
	}
	if runFolder == "" {
		return fmt.Errorf("runFolder not set")
	}
	if err := os.MkdirAll(runFolder, 0755); err != nil {
		return fmt.Errorf("create run folder: %w", err)
	}
	logPath := filepath.Join(runFolder, "run.log")
	logFile, err := atomicfs.OpenAppend(logPath, 0644)
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
	if parsedStdout != nil {
		parsedStdout.dst = combinedOut
		combinedOut = parsedStdout
	}
	if parsedStderr != nil {
		parsedStderr.dst = combinedErr
		combinedErr = parsedStderr
	}

	if err := r.sandbox.Exec(ctx, command, combinedOut, combinedErr); err != nil {
		flushOpenCodeOutputs(parsedStdout, parsedStderr)
		return fmt.Errorf("execute agent: %w", err)
	}
	flushOpenCodeOutputs(parsedStdout, parsedStderr)
	_ = prefixedOut.Flush()
	_ = prefixedErr.Flush()
	_ = logPrefixedOut.Flush()
	_ = logPrefixedErr.Flush()
	return nil
}

// Run executes the full lifecycle of the AgentRun and returns the result.
//
// The rendered prompt file path is worktree-relative. When the caller does
// not supply a RenderedPromptFile, the default is `./.sandman/task.md` —
// the agent reads this file from inside the worktree, not from the host
// repo's `.sandman/`. The path is intentionally literal because the
// worktree has its own `.sandman/` directory that the layout does not own.
func (r *AgentRun) Run(ctx context.Context, renderer prompt.IssueRenderer, command string, renderCfg prompt.RenderConfig) AgentRunResult {
	renderedPromptFile := renderCfg.RenderedPromptFile
	if renderedPromptFile == "" {
		renderedPromptFile = filepath.Join(".", ".sandman", "task.md")
	}

	if renderCfg.TaskPrompt != "" {
		if ctx.Err() != nil {
			r.status = "aborted"
			return r.Result()
		}
		taskPrompt := renderCfg.TaskPrompt
		if !r.review {
			taskPrompt = prompt.EnsureReviewTimeoutContext(taskPrompt, renderCfg.ReviewTimeout)
		}
		if err := r.writeTaskPrompt(renderedPromptFile, taskPrompt); err != nil {
			// A clean recovery session must not start without its checkpoint-first
			// Task, even though atomic replacement preserved the prior Task.
			// Retain the rollover cause so the ordinary retry boundary retries
			// against that unchanged Task rather than treating this as a generic
			// failure and replacing it with a continuation prompt.
			r.contextExhausted = renderCfg.ContextRecovery
			r.status = "failure"
			return r.Result()
		}
	} else {
		if err := r.Prepare(renderer, renderCfg); err != nil {
			r.status = "failure"
			return r.Result()
		}
	}

	builtInOpenCode := r.preset == "opencode" && command == config.BuiltInAgentPresets["opencode"].Command
	priorSession := ""
	useContinue := false
	if builtInOpenCode && r.reuseSession {
		identity, found, lookupErr := priorOpenCodeSession(r.layout, r.previousBatchID, r.previousRunID)
		if lookupErr != nil {
			r.warnSession(lookupErr)
		}
		if found {
			priorSession = identity.SessionID
		} else {
			useContinue = true
		}
	}
	render := func(sessionID string, continueFlag bool) (string, error) {
		sessionFlag := ""
		if sessionID != "" {
			sessionFlag = shellenv.Quote(sessionID)
		}
		return RenderCommand(command, CommandData{
			PromptFile:                 renderedPromptFile,
			ModelFlag:                  r.modelFlag(command),
			VariantFlag:                r.variantFlag(command),
			ModelProvider:              r.modelProvider,
			ModelName:                  r.modelName,
			DangerouslySkipPermissions: r.dangerouslySkipPermissions != nil && *r.dangerouslySkipPermissions,
			SessionName:                r.sessionName,
			SessionFlag:                sessionFlag,
			ContinueFlag:               continueFlag,
		})
	}
	renderedCmd, err := render(priorSession, useContinue)
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

	attemptCtx, cancelAttempt := context.WithCancel(ctx)
	defer cancelAttempt()
	var detector *contextRolloverDetector
	if r.preset == "opencode" {
		detector = newContextRolloverDetector(time.Now, r.contextRolloverLiterals, func() {
			if ctx.Err() == nil {
				cancelAttempt()
			}
		})
	}
	stdout := io.Writer(os.Stdout)
	stderr := io.Writer(os.Stderr)
	if detector != nil {
		stdout = io.MultiWriter(stdout, detector)
		stderr = io.MultiWriter(stderr, detector)
	}
	var parsedStdout, parsedStderr *opencodeOutput
	if builtInOpenCode {
		parsedStdout = newOpenCodeOutput(nil, r.warningWriter(), false)
		parsedStderr = newOpenCodeOutput(nil, r.warningWriter(), true)
	}
	execErr := r.execute(attemptCtx, renderedCmd, stdout, stderr, parsedStdout, parsedStderr)
	if detector != nil {
		detector.Flush()
	}
	if detector != nil && detector.Triggered() {
		r.contextExhausted = true
		r.status = "failure"
		// Extract any cleanup failure so the orchestrator can record it
		// before marking the run as terminal (issue #2605 criterion #4).
		var cleanupErr *sandbox.CleanupError
		if errors.As(execErr, &cleanupErr) {
			r.cleanupError = cleanupErr.CleanupFail
		}
		return r.Result()
	}
	sessionNotFound := (parsedStdout != nil && parsedStdout.SessionNotFound()) || (parsedStderr != nil && parsedStderr.SessionNotFound())
	fallbackUsed := false
	if execErr != nil && builtInOpenCode && r.reuseSession && priorSession != "" && sessionNotFound {
		if ctx.Err() == nil {
			renderedCmd, err = render("", true)
			if err == nil {
				fallbackUsed = true
				fallbackOut := newOpenCodeOutput(nil, r.warningWriter(), false)
				fallbackErr := newOpenCodeOutput(nil, r.warningWriter(), true)
				execErr = r.execute(attemptCtx, renderedCmd, stdout, stderr, fallbackOut, fallbackErr)
				if fallbackOut.SessionID() != "" {
					r.persistSession(fallbackOut.SessionID())
				}
			}
		}
	}
	if !fallbackUsed && parsedStdout != nil && parsedStdout.SessionID() != "" {
		r.persistSession(parsedStdout.SessionID())
	}
	if !fallbackUsed && parsedStderr != nil && parsedStderr.SessionID() != "" {
		r.persistSession(parsedStderr.SessionID())
	}
	if execErr != nil {
		r.status = "failure"
		return r.Result()
	}
	return r.Result()
}

func (r *AgentRun) warningWriter() io.Writer {
	return r.sessionWarning
}

func (r *AgentRun) warnSession(err error) {
	if err != nil && r.warningWriter() != nil {
		fmt.Fprintf(r.warningWriter(), "warning: OpenCode session metadata: %v\n", err)
	}
}

func (r *AgentRun) persistSession(sessionID string) {
	runFolder := r.runFolder
	if runFolder == "" {
		runFolder = r.sandbox.WorkDir()
	}
	path := filepath.Join(runFolder, "session.json")
	if err := writeOpenCodeSession(path, sessionID); err != nil {
		r.warnSession(err)
	}
}

func (r *AgentRun) variantFlag(command string) string {
	if r.variant == "" || r.preset != "opencode" || command != config.BuiltInAgentPresets["opencode"].Command {
		return ""
	}
	return "--variant " + shellenv.Quote(r.variant)
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
	taskWriter := r.taskWriter
	if taskWriter == nil {
		taskWriter = atomicfs.WriteAtomic
	}
	if err := taskWriter(promptPath, []byte(content), 0644); err != nil {
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
		IssueNumber:      issue.Number,
		Issue:            issueRefPtr,
		Status:           r.status,
		Branch:           r.branch,
		WorktreePath:     r.sandbox.WorkDir(),
		ContextExhausted: r.contextExhausted,
		CleanupError:     r.cleanupError,
	}
}

func (r *AgentRun) issueData() github.Issue {
	if r.issue != nil {
		return *r.issue
	}
	return github.Issue{}
}

func (r *AgentRun) prefixLabel() string {
	return r.runID
}
