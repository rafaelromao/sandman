package batch

import (
	"encoding/json"
	"io"
	"strings"

	"github.com/rafaelromao/sandman/internal/shellenv"
)

// claudeStrategy runs the unmodified Claude Code CLI in print mode
// (`claude -p --output-format stream-json --verbose`). It reuses sessions
// through Claude Code's working-directory-scoped --continue, relies on Claude
// Code's automatic compaction instead of context rollover, renders the
// stream-json records as readable lines, and recognises subscription usage
// limits on the final result record.
type claudeStrategy struct {
	builtInCommand bool
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

func (claudeStrategy) LaunchEnv(env map[string]string, _, _ string) map[string]string {
	return env
}

// PrepareLaunch resumes with `claude -p --continue`, which reopens the most
// recent conversation in the working directory, print-mode sessions
// included. Each row owns one worktree, so the worktree identifies the
// conversation: no session ID is parsed and no session.json is written.
func (s claudeStrategy) PrepareLaunch(run *AgentRun) (string, bool) {
	return "", s.builtInCommand && run.reuseSession
}

func (claudeStrategy) RetryAfterMissingSession(*AgentRun, string, error, outputParser, outputParser) bool {
	return false
}

func (claudeStrategy) PersistSession(*AgentRun, outputParser, outputParser) {}

// NewOutputParsers renders the built-in command's stream-json into readable
// lines. Custom commands keep their own output untouched.
func (s claudeStrategy) NewOutputParsers(io.Writer) (outputParser, outputParser) {
	if !s.builtInCommand {
		return nil, nil
	}
	return newClaudeOutputs()
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
