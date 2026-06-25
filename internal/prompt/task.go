package prompt

import (
	"strings"
)

// ContinuationTaskPrompt returns the resume prompt for `sandman run --continue`
// from the verbatim content of `.sandman/task.md`. The previous
// implementation routed the file through ParseTask → BuildTaskPrompt, which
// rewrote the file into a different scaffold (## Completed / ## Pending /
// ## Blockers / ## Key Decisions / ## Next Step) and silently destroyed the
// original default-task-prompt.md layout (# Task, ## Issue Context, ##
// Runtime Context, ## Execution Checklist). The continuation contract from
// docs/usage/default-task-prompt.md is "pass the contents of the task file
// verbatim" — this function is the single seam that enforces it.
//
// When content is empty (e.g. the file existed but was blank), this falls
// back to DefaultPrompt() so the agent still gets a usable scaffold. The
// "no task file" path lives in batch.ReadTaskContent and returns its own
// EmptyTaskTemplate.
func ContinuationTaskPrompt(content string) string {
	if strings.TrimSpace(content) == "" {
		return DefaultPrompt()
	}
	return content
}
