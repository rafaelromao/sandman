package prompt

import (
	"fmt"
	"strings"

	"github.com/rafaelromao/sandman/internal/config"
)

const continuationFreshnessGuardHeading = "## Continuation Freshness Guard"
const contextRecoveryGuardHeading = "## Context Recovery Guard"

const (
	reviewTimeoutContextHeading = "## Sandman Runtime Context"
	reviewTimeoutContextPrefix  = "- Delegated review response timeout:"
)

var continuationFreshnessGuard = `## Continuation Freshness Guard

This section is injected for every retry and sandman run --continue and overrides persisted blocker and next-action text from earlier attempts.

Before acting on any persisted blocker or next action:

1. Treat every persisted blocker and next action as historical evidence, never current truth.
2. Re-check its authoritative live source now: Git and worktree state, change-request state and checks, review state, authentication, required tools, or the relevant test command.
3. If the blocker no longer exists, remove or mark it resolved in .sandman/task.md, recompute ## Next Step from current live state and the first unchecked checklist item, and continue automatically.
4. If the blocker still exists, refresh its evidence and next executable action before following it.

Never stop or exit solely because an earlier attempt recorded a blocker.`

var contextRecoveryGuard = `## Context Recovery Guard

This is a clean OpenCode session after the previous session exhausted its context. Do not resume implementation from memory.

Before changing implementation files:

The recovery checkpoint must be durable before implementation resumes.

1. Reconstruct the handoff from the current worktree, git status and diff, recent commits, run log, and this Task.
2. Write a durable checkpoint and handoff summary into this Task, including completed work, remaining work, validation, and the next step.
3. Only after the checkpoint is durable, continue implementation from that reconstructed state.

Preserve both committed and uncommitted work unless the current Task explicitly directs otherwise.`

// ContinuationTaskPrompt returns the resume prompt for `sandman run --continue`
// while preserving the verbatim content of `.sandman/task.md`. The previous
// implementation routed the file through ParseTask → BuildTaskPrompt, which
// rewrote the file into a different scaffold (## Completed / ## Pending /
// ## Blockers / ## Key Decisions / ## Next Step) and silently destroyed the
// original default-task-prompt.md layout (# Task, ## Issue Context, ##
// Runtime Context, ## Execution Checklist). This function preserves that
// content byte-for-byte and appends one canonical freshness guard so historical
// blockers cannot be mistaken for current state, including for task files
// created by older Sandman versions.
//
// When content is empty (e.g. the file existed but was blank), this falls
// back to DefaultPrompt() so the agent still gets a usable scaffold. The
// "no task file" path lives in batch.ReadTaskContent and returns its own
// EmptyTaskTemplate.
func ContinuationTaskPrompt(content string) string {
	return continuationTaskPrompt(content, 0)
}

// ContinuationTaskPromptWithReviewTimeout returns a continuation prompt whose
// runtime context reflects the current AgentRun's effective review budget.
func ContinuationTaskPromptWithReviewTimeout(content string, reviewTimeout int) string {
	return continuationTaskPrompt(content, reviewTimeout)
}

// ContextRecoveryTaskPrompt composes the preserved Task with a canonical,
// checkpoint-first recovery instruction for a fresh OpenCode session. The
// recovery section is replaced rather than duplicated when a retry is itself
// retried.
func ContextRecoveryTaskPrompt(content string, reviewTimeout int) string {
	content = continuationTaskPrompt(content, reviewTimeout)
	content = removeTaskSection(content, contextRecoveryGuardHeading)
	return trimTrailingNewlines(content) + "\n\n" + contextRecoveryGuard + "\n"
}

func continuationTaskPrompt(content string, reviewTimeout int) string {
	if strings.TrimSpace(content) == "" {
		content = DefaultPrompt()
	}
	content = removeCanonicalGuard(content)
	if reviewTimeout > 0 {
		content = EnsureReviewTimeoutContext(content, reviewTimeout)
	}
	guard := continuationFreshnessGuard
	if reviewTimeout > 0 {
		effective := (&config.Config{ReviewTimeout: reviewTimeout}).EffectiveReviewTimeout()
		guard += fmt.Sprintf("\n\nCurrent AgentRun delegated review response timeout is `%d` seconds; it supersedes any persisted timeout wording above.", effective)
	}
	return trimTrailingNewlines(content) + "\n\n" + guard + "\n"
}

// EnsureReviewTimeoutContext keeps the effective delegated review budget in
// the current task even when a custom template omits REVIEW_TIMEOUT.
func EnsureReviewTimeoutContext(content string, reviewTimeout int) string {
	effective := (&config.Config{ReviewTimeout: reviewTimeout}).EffectiveReviewTimeout()
	line := fmt.Sprintf("%s `%d` seconds", reviewTimeoutContextPrefix, effective)
	lines := strings.Split(content, "\n")
	sectionStart, sectionEnd := runtimeContextSection(lines)
	if sectionStart >= 0 {
		for i := sectionStart + 1; i < sectionEnd; i++ {
			if strings.HasPrefix(strings.TrimSpace(lines[i]), reviewTimeoutContextPrefix) {
				lines[i] = line
				return strings.Join(lines, "\n")
			}
		}
		lines = insertLine(lines, sectionEnd, line)
		return strings.Join(lines, "\n")
	}

	return trimTrailingNewlines(content) + "\n\n" + reviewTimeoutContextHeading + "\n\n" + line + "\n"
}

// runtimeContextSection returns the last runtime-context section so an issue
// body containing similarly named text is never treated as Sandman metadata.
func runtimeContextSection(lines []string) (start, end int) {
	start = -1
	for i, line := range lines {
		switch strings.TrimSpace(line) {
		case reviewTimeoutContextHeading, "## Runtime Context":
			start = i
		}
	}
	if start < 0 {
		return -1, -1
	}

	end = len(lines)
	for i := start + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "## ") || (strings.HasPrefix(trimmed, "# ") && !strings.HasPrefix(trimmed, "##")) {
			end = i
			break
		}
	}
	return start, end
}

func insertLine(lines []string, index int, line string) []string {
	if index == len(lines) && index > 0 && lines[index-1] == "" {
		index--
	}
	lines = append(lines, "")
	copy(lines[index+1:], lines[index:])
	lines[index] = line
	return lines
}

func removeCanonicalGuard(content string) string {
	for {
		idx := strings.Index(content, continuationFreshnessGuardHeading)
		if idx < 0 {
			return content
		}
		end := guardSectionEnd(content, idx)
		content = content[:idx] + content[end:]
	}
}

func removeTaskSection(content, heading string) string {
	for {
		idx := strings.Index(content, heading)
		if idx < 0 {
			return content
		}
		end := guardSectionEnd(content, idx)
		content = content[:idx] + content[end:]
	}
}

// guardSectionEnd returns the byte offset that follows the H2 section opened
// by the guard heading at the given offset. The section terminates at the
// next H2 heading (a line beginning with "## "), at the next H1 ("# "), or
// at the end of the content. Anchoring on Markdown headings ensures stale or
// short-bodied guards cannot delete the persisted sections that follow
// them — a regression that previously sliced into the next H2 body.
func guardSectionEnd(content string, headingIdx int) int {
	lineStart := strings.LastIndex(content[:headingIdx], "\n") + 1
	scan := lineStart
	for scan < len(content) {
		nl := strings.IndexByte(content[scan:], '\n')
		var end int
		if nl < 0 {
			end = len(content)
		} else {
			end = scan + nl
		}
		line := content[scan:end]
		trimmed := strings.TrimSpace(line)
		if scan != lineStart {
			if strings.HasPrefix(trimmed, "## ") || (scan+1 < len(content) && strings.HasPrefix(trimmed, "# ") && !strings.HasPrefix(trimmed, "##")) {
				return scan
			}
		}
		if nl < 0 {
			return len(content)
		}
		scan = end + 1
	}
	return len(content)
}

func trimTrailingNewlines(content string) string {
	for len(content) > 0 && (content[len(content)-1] == '\n' || content[len(content)-1] == '\r') {
		content = content[:len(content)-1]
	}
	return content
}
