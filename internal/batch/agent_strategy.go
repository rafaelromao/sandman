package batch

import (
	"io"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/shellenv"
)

// agentStrategy is the per-preset behaviour bundle the run loop selects once
// per launch. Every agent-specific decision (command flags, launch
// environment, session reuse, output parsing, and failure classification) is
// a method here, so adding or removing an agent touches a registry entry and
// one strategy type instead of the run loop.
//
// strategyFor is the only place in this package that inspects a preset name
// or compares a command to a preset template; a source-scanning test enforces
// that no agent-name comparison appears anywhere else.
type agentStrategy interface {
	// ModelFlag renders the model selector passed to the command template as
	// {{.ModelFlag}}, or "" when the model is empty or not wired for the launch.
	ModelFlag(model string) string
	// VariantFlag renders the model-variant selector passed to the command
	// template as {{.VariantFlag}}, or "".
	VariantFlag(variant string) string
	// LaunchEnv returns the environment exported in front of renderedCmd.
	// permissionMode is the resolved agent's OpencodePermissionMode.
	LaunchEnv(env map[string]string, permissionMode, renderedCmd string) map[string]string

	// PrepareLaunch chooses how the first launch selects a prior session: an
	// exact session ID rendered as {{.SessionFlag}}, the agent's own continue
	// flag rendered as {{.ContinueFlag}}, or neither.
	PrepareLaunch(run *AgentRun) (sessionID string, continueFlag bool)
	// RetryAfterMissingSession reports whether a failed launch that selected
	// priorSession is relaunched once with only the continue flag.
	RetryAfterMissingSession(run *AgentRun, priorSession string, execErr error, stdout, stderr outputParser) bool
	// PersistSession records the session identity observed by the parsers of
	// the launch that counts for the Run.
	PersistSession(run *AgentRun, stdout, stderr outputParser)

	// NewOutputParsers returns the stdout and stderr parsers that rewrite the
	// agent's structured stream into readable output, or nil parsers to keep
	// the raw stream.
	NewOutputParsers(warnings io.Writer) (stdout, stderr outputParser)

	// ContextRolloverDetector returns the detector that stops an attempt on a
	// repeated context-limit failure, or nil when the agent handles context
	// limits itself.
	ContextRolloverDetector(literals []string, onTrigger func()) *contextRolloverDetector
	// UsageLimitRule returns the per-line rule that recognises the agent's
	// usage-limit response, or nil when the agent has none.
	UsageLimitRule() func(line string) bool
	// AwaitsUsageLimit reports whether a recognised usage limit enters
	// run.await with usage-limit polling instead of the ordinary retry path.
	AwaitsUsageLimit() bool
}

// outputParser is a structured-output parser placed between the sandbox and
// the prefixed terminal/run.log writers. It writes readable lines to its
// destination and captures session facts on the way.
type outputParser interface {
	io.Writer
	Flush() error
	SessionID() string
	SessionNotFound() bool
	setDestination(dst io.Writer)
}

// agentStrategies maps each built-in agent preset to its strategy
// constructor. The constructor learns whether the launch runs the preset's
// own command template: a custom command under a built-in preset keeps the
// preset's failure classification and environment rules, but receives no
// agent flags, session selection, output parsing, or usage-limit awaiting.
var agentStrategies = map[string]func(builtInCommand bool) agentStrategy{
	"opencode": func(builtInCommand bool) agentStrategy { return opencodeStrategy{builtInCommand: builtInCommand} },
	"claude":   func(builtInCommand bool) agentStrategy { return claudeStrategy{builtInCommand: builtInCommand} },
}

// strategyFor returns the behaviour bundle for a launch of command under
// preset. Presets without a registered strategy, including preset-less custom
// providers, get the passthrough strategy.
func strategyFor(preset, command string) agentStrategy {
	newStrategy, ok := agentStrategies[preset]
	if !ok {
		return passthroughStrategy{}
	}
	builtIn, ok := config.BuiltInAgentPresets[preset]
	if !ok {
		return passthroughStrategy{}
	}
	return newStrategy(command == builtIn.Command)
}

// strategyForPreset returns the strategy of a preset running its own built-in
// command. Callers that know only an agent name use it.
func strategyForPreset(preset string) agentStrategy {
	builtIn, ok := config.BuiltInAgentPresets[preset]
	if !ok {
		return passthroughStrategy{}
	}
	return strategyFor(preset, builtIn.Command)
}

// AwaitsUsageLimit reports whether a run of the named built-in agent preset
// waits for a recognised usage limit to reset instead of retrying. The review
// daemon, which knows only the review agent's name, uses it to gate its
// daemon-wide quota pause with the same rule as the run loop.
func AwaitsUsageLimit(agentName string) bool {
	return strategyForPreset(strings.TrimSpace(agentName)).AwaitsUsageLimit()
}

// IsUsageLimitOutput reports whether output contains the named agent's
// usage-limit response under that agent's own detection rule. Callers that
// receive an error instead of an AgentRunResult use this to preserve the same
// detection boundary.
func IsUsageLimitOutput(agentName, output string) bool {
	rule := strategyForPreset(strings.TrimSpace(agentName)).UsageLimitRule()
	if rule == nil {
		return false
	}
	detector := newUsageLimitDetector(rule)
	_, _ = detector.Write([]byte(output))
	detector.Flush()
	return detector.Triggered()
}

// passthroughStrategy runs custom commands and preset-less providers exactly
// as configured: no injected flags, no session handling, no parsing, and no
// agent-specific failure classification.
type passthroughStrategy struct{}

func (passthroughStrategy) ModelFlag(string) string   { return "" }
func (passthroughStrategy) VariantFlag(string) string { return "" }

func (passthroughStrategy) LaunchEnv(env map[string]string, _, _ string) map[string]string {
	return env
}

func (passthroughStrategy) PrepareLaunch(*AgentRun) (string, bool) { return "", false }

func (passthroughStrategy) RetryAfterMissingSession(*AgentRun, string, error, outputParser, outputParser) bool {
	return false
}

func (passthroughStrategy) PersistSession(*AgentRun, outputParser, outputParser) {}

func (passthroughStrategy) NewOutputParsers(io.Writer) (outputParser, outputParser) {
	return nil, nil
}

func (passthroughStrategy) ContextRolloverDetector([]string, func()) *contextRolloverDetector {
	return nil
}

func (passthroughStrategy) UsageLimitRule() func(string) bool { return nil }
func (passthroughStrategy) AwaitsUsageLimit() bool            { return false }

// opencodeStrategy runs the OpenCode CLI (`opencode run --format json`). It
// owns exact session reuse through session.json with one narrow --continue
// fallback, the JSON event parser, context-rollover detection, and the
// OpenCode usage-limit rule.
type opencodeStrategy struct {
	builtInCommand bool
}

func (s opencodeStrategy) ModelFlag(model string) string {
	if model == "" || !s.builtInCommand {
		return ""
	}
	return "-m " + model
}

func (s opencodeStrategy) VariantFlag(variant string) string {
	if variant == "" || !s.builtInCommand {
		return ""
	}
	return "--variant " + shellenv.Quote(variant)
}

// LaunchEnv drops the preset's OPENCODE_PERMISSION allow-list ("builtin"
// mode) unless the rendered command skips permission prompts. A value the
// operator configured ("custom" mode) is always exported.
func (opencodeStrategy) LaunchEnv(env map[string]string, permissionMode, renderedCmd string) map[string]string {
	applyPermission := strings.Contains(renderedCmd, "--dangerously-skip-permissions")
	filtered := make(map[string]string, len(env))
	for key, value := range env {
		if key == "OPENCODE_PERMISSION" && permissionMode == "builtin" && !applyPermission {
			continue
		}
		filtered[key] = value
	}
	return filtered
}

// PrepareLaunch selects the exact prior session recorded in session.json.
// When reuse is requested but no identity was recorded, the launch falls back
// to OpenCode's own --continue.
func (s opencodeStrategy) PrepareLaunch(run *AgentRun) (string, bool) {
	if !s.builtInCommand || !run.reuseSession {
		return "", false
	}
	identity, found, lookupErr := priorOpenCodeSession(run.layout, run.previousBatchID, run.previousRunID)
	if lookupErr != nil {
		run.warnSession(lookupErr)
	}
	if found {
		return identity.SessionID, false
	}
	return "", true
}

func (s opencodeStrategy) RetryAfterMissingSession(run *AgentRun, priorSession string, execErr error, stdout, stderr outputParser) bool {
	return execErr != nil && s.builtInCommand && run.reuseSession && priorSession != "" && sessionNotFound(stdout, stderr)
}

func (opencodeStrategy) PersistSession(run *AgentRun, stdout, stderr outputParser) {
	run.persistSession(firstSessionID(stdout, stderr))
}

func (s opencodeStrategy) NewOutputParsers(warnings io.Writer) (outputParser, outputParser) {
	if !s.builtInCommand {
		return nil, nil
	}
	capture := &opencodeSessionCapture{}
	return newSharedOpenCodeOutput(nil, warnings, false, capture), newSharedOpenCodeOutput(nil, warnings, true, capture)
}

func (opencodeStrategy) ContextRolloverDetector(literals []string, onTrigger func()) *contextRolloverDetector {
	return newContextRolloverDetector(time.Now, literals, onTrigger)
}

func (opencodeStrategy) UsageLimitRule() func(string) bool { return opencodeUsageLimitLine }

func (s opencodeStrategy) AwaitsUsageLimit() bool { return s.builtInCommand }

func firstSessionID(outputs ...outputParser) string {
	for _, output := range outputs {
		if output != nil && output.SessionID() != "" {
			return output.SessionID()
		}
	}
	return ""
}

func sessionNotFound(outputs ...outputParser) bool {
	for _, output := range outputs {
		if output != nil && output.SessionNotFound() {
			return true
		}
	}
	return false
}

func flushOutputParsers(outputs ...outputParser) {
	for _, output := range outputs {
		if output != nil {
			_ = output.Flush()
		}
	}
}
