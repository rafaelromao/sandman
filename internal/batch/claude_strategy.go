package batch

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/rafaelromao/sandman/internal/shellenv"
)

// claudeStrategy runs the unmodified Claude Code CLI in print mode
// (`claude -p --output-format stream-json --verbose`). It reuses the exact
// session recorded in session.json through --resume, relies on Claude
// Code's automatic compaction instead of context rollover, renders the
// stream-json records as readable lines, and recognises subscription usage
// limits on the final result record.
type claudeStrategy struct {
	builtInCommand bool
	// readOnlyModes are the preset's permission modes in which a session
	// cannot change anything; a session starting in one is stopped.
	readOnlyModes []string
}

func (s claudeStrategy) ModelFlag(model string) string {
	if model == "" || !s.builtInCommand {
		return ""
	}
	return "--model " + shellenv.Quote(model)
}

// VariantFlag maps Sandman's model variant to Claude Code's effort level.
// Claude Code validates the value itself.
func (s claudeStrategy) VariantFlag(variant string) string {
	if variant == "" || !s.builtInCommand {
		return ""
	}
	return "--effort " + shellenv.Quote(variant)
}

// PermissionFlag renders the launch's Claude Code permission mode.
//
// An edit scope allows Edit (which also covers Write and the other
// file-editing tools) under editDir and keeps every other prompting call
// denied. In command-line rules a leading "/" anchors at the working
// directory. --allowedTools takes several values, so the single-value
// --permission-mode follows it and the prompt is never read as a rule;
// dontAsk is explicit so neither a configured mode nor a settings defaultMode
// can widen the scope.
//
// Otherwise a configured mode is rendered as --permission-mode. On the command
// line it overrides a permissions.defaultMode from user or managed settings,
// but Claude Code still refuses a mode the administrator disabled.
func (s claudeStrategy) PermissionFlag(mode, editDir string) string {
	if !s.builtInCommand {
		return ""
	}
	if editDir != "" {
		return "--allowedTools " + shellenv.Quote("Edit(/"+editDir+"/**)") + " --permission-mode dontAsk"
	}
	if mode != "" {
		return "--permission-mode " + shellenv.Quote(mode)
	}
	return ""
}

func (claudeStrategy) LaunchEnv(env map[string]string, _, _ string) map[string]string {
	return env
}

// PrepareLaunch selects the exact session recorded in the prior Run's
// session.json, rendered as --resume. Claude Code's working-directory-scoped
// --continue is never used: every container worktree lives under the same
// /workspace path, and the host transcript directory is shared, so the most
// recent conversation in a directory may belong to another repository. With
// no valid recorded session the launch starts a fresh session.
func (s claudeStrategy) PrepareLaunch(run *AgentRun) (string, bool) {
	if !s.builtInCommand || !run.reuseSession {
		return "", false
	}
	identity, found, err := priorAgentSession(run.layout, run.previousBatchID, run.previousRunID, claudeSessionKind)
	if found && !claudeSessionIDPattern.MatchString(identity.SessionID) {
		found, err = false, fmt.Errorf("invalid Claude Code session id %q", identity.SessionID)
	}
	if err != nil {
		run.warnSession(err)
	}
	if !found {
		return "", false
	}
	return identity.SessionID, false
}

// claudeSessionIDPattern is the UUID shape of a Claude Code session ID. A
// recorded value of any other shape is never rendered into the command.
var claudeSessionIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// RetryAfterMissingSession relaunches once when the recorded session no
// longer exists. The relaunch renders no session selector, so it starts a
// fresh session; the Claude preset has no --continue fallback.
func (s claudeStrategy) RetryAfterMissingSession(run *AgentRun, priorSession string, execErr error, stdout, stderr outputParser) bool {
	return execErr != nil && s.builtInCommand && run.reuseSession && priorSession != "" && sessionNotFound(stdout, stderr)
}

// claudeSessionKind is the Claude Code session identity recorded in a Run's
// session.json.
var claudeSessionKind = agentSessionKind{protocol: "claude-session/v1", provider: "claude"}

// PersistSession records the session ID Claude Code reported so the Run's
// details can name the conversation to resume from the host.
func (s claudeStrategy) PersistSession(run *AgentRun, stdout, stderr outputParser) {
	if !s.builtInCommand {
		return
	}
	run.persistSession(claudeSessionKind, firstSessionID(stdout, stderr))
}

// NewOutputParsers renders the built-in command's stream-json into readable
// lines. Custom commands keep their own output untouched.
func (s claudeStrategy) NewOutputParsers(io.Writer) (outputParser, outputParser) {
	if !s.builtInCommand {
		return nil, nil
	}
	return newClaudeOutputs(s.readOnlyModes...)
}

func (claudeStrategy) ContextRolloverDetector([]string, func()) *contextRolloverDetector {
	return nil
}

func (claudeStrategy) UsageLimitRule() func(string) bool { return claudeUsageLimitLine }

func (s claudeStrategy) AwaitsUsageLimit() bool { return s.builtInCommand }

// claudeUsageLimitLiterals are the documented Claude Code messages for limits
// that reset within Sandman's usage-limit waiting window. Spend and budget
// limits ("monthly spend limit", "shared budget", ...) are deliberately
// absent: they do not reset within that window.
var claudeUsageLimitLiterals = []string{
	"hit your session limit",
	"hit your weekly limit",
	"hit your opus limit",
	"hit your sonnet limit",
	"fable limit reached",
	// Wording of earlier Claude Code releases, still seen on older installs.
	"claude ai usage limit reached",
}

// claudeUsageLimitLine recognises Claude Code's usage-limit response. In print
// mode Claude Code ends the run instead of waiting for the reset, and the
// terminal stream-json record is a top-level `result` record with
// `is_error: true`. Only that record qualifies: the agent's own text can echo
// the same phrase, but stream-json carries it inside assistant records, which
// never have a top-level `is_error: true`. The literal may sit in any field of
// the result record.
func claudeUsageLimitLine(line string) bool {
	line = normalizeContextRolloverLine(line)
	if !strings.HasPrefix(line, "{") {
		return false
	}
	var record struct {
		Type    string `json:"type"`
		IsError bool   `json:"is_error"`
	}
	if err := json.Unmarshal([]byte(line), &record); err != nil {
		return false
	}
	if record.Type != "result" || !record.IsError {
		return false
	}
	lower := strings.ToLower(line)
	for _, literal := range claudeUsageLimitLiterals {
		if strings.Contains(lower, literal) {
			return true
		}
	}
	return false
}

// claudeAuthFailureLiterals are Claude Code's messages for a session that
// cannot authenticate: missing or invalid credentials, and a subscription
// OAuth session whose refresh token was already spent by another copy.
var claudeAuthFailureLiterals = []string{
	"failed to authenticate",
	"oauth session expired",
	"oauth token has been revoked",
	"invalid api key",
	"please run /login",
}

// claudeAuthFailureLine recognises an authentication failure on the terminal
// stream-json `result` record with `is_error: true`, under the same echo
// rule as claudeUsageLimitLine.
func claudeAuthFailureLine(line string) bool {
	line = normalizeContextRolloverLine(line)
	var record struct {
		Type    string `json:"type"`
		IsError bool   `json:"is_error"`
	}
	if json.Unmarshal([]byte(line), &record) != nil || record.Type != "result" || !record.IsError {
		return false
	}
	return claudeAuthFailureText(line)
}

// claudeAuthFailureText reports whether text carries an authentication
// failure literal. Applied to plain lines it covers Claude Code's own stderr
// diagnostics, which are never agent text.
func claudeAuthFailureText(text string) bool {
	lower := strings.ToLower(text)
	for _, literal := range claudeAuthFailureLiterals {
		if strings.Contains(lower, literal) {
			return true
		}
	}
	return false
}
