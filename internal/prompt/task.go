package prompt

import (
	"strings"
)

const continuationFreshnessGuardHeading = "## Continuation Freshness Guard"

var continuationFreshnessGuard = `## Continuation Freshness Guard

This section is injected for every retry and sandman run --continue and overrides persisted blocker and next-action text from earlier attempts.

Before acting on any persisted blocker or next action:

1. Treat every persisted blocker and next action as historical evidence, never current truth.
2. Re-check its authoritative live source now: Git and worktree state, change-request state and checks, review state, authentication, required tools, or the relevant test command.
3. If the blocker no longer exists, remove or mark it resolved in .sandman/task.md, recompute ## Next Step from current live state and the first unchecked checklist item, and continue automatically.
4. If the blocker still exists, refresh its evidence and next executable action before following it.

Never stop or exit solely because an earlier attempt recorded a blocker.`

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
	if strings.TrimSpace(content) == "" {
		return DefaultPrompt()
	}
	content = removeCanonicalGuard(content)
	return trimTrailingNewlines(content) + "\n\n" + continuationFreshnessGuard + "\n"
}

func removeCanonicalGuard(content string) string {
	if idx := strings.Index(content, continuationFreshnessGuardHeading); idx >= 0 {
		end := idx + len(continuationFreshnessGuard) + 1
		if end > len(content) {
			end = len(content)
		}
		content = content[:idx] + content[end:]
		return removeCanonicalGuard(content)
	}
	return content
}

func trimTrailingNewlines(content string) string {
	for len(content) > 0 && (content[len(content)-1] == '\n' || content[len(content)-1] == '\r') {
		content = content[:len(content)-1]
	}
	return content
}
