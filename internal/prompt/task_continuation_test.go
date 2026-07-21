package prompt

import (
	"strings"
	"testing"
)

// TestContinuationTaskPrompt_PreservesOriginalTaskTemplate documents the
// --continue invariant: when an issue's .sandman/task.md was originally
// rendered from default-task-prompt.md (and therefore starts with "# Task"
// and contains ## Issue Context, ## Runtime Context, and ## Execution
// Checklist), the resume prompt must be the file's verbatim content. The
// earlier round-trip through ParseTask → BuildTaskPrompt rewrote the file
// into a different scaffold (## Completed / ## Pending / ## Blockers /
// ## Key Decisions / ## Next Step), destroying the user-facing Execution
// Checklist and breaking the in-place checklist semantics described in
// default-task-prompt.md.
func TestContinuationTaskPrompt_PreservesOriginalTaskTemplate(t *testing.T) {
	original := `# Task

Implement GitHub issue #1193: Uniform log prefix -- always [<runID>]

## Issue Context

Slice 2 of issue #1193.

## Runtime Context

- You are running inside a Sandman-created worktree.
- Current branch: 'sandman/1193-slice-2-uniform-log-prefix-always-runid'
- Source branch: 'sandman/1193-slice-2-uniform-log-prefix-always-runid'
- Base branch: 'main'
- Review command: '/sandman review'

The worktree MUST be checked out on 'sandman/1193-slice-2-uniform-log-prefix-always-runid' when the run finishes. Do not switch to 'main' or any other branch before exiting.

## Execution Checklist

- [x] Create branch
- [x] Plan (sandman-plan)
- [x] Implement (sandman-implement: execute TDD + commit + self-review + back-merge + create PR + delegate review)
- [ ] PR-Review (sandman-pr-review)
- [ ] PR-Merge (sandman-pr-merge)

After completing each item, update '.sandman/task.md' in place by checking that item off.
`

	got := ContinuationTaskPrompt(original)

	if got != original {
		t.Fatalf("expected continuation prompt to be the verbatim task.md content, got a rewritten scaffold.\n\n--- diff (first 40 lines) ---\nwant:\n%s\n\ngot:\n%s", firstLines(original, 40), firstLines(got, 40))
	}
}

// TestContinuationTaskPrompt_PreservesBlockersSection verifies that
// ContinuationTaskPrompt does NOT strip ## Blockers sections from the
// content. No Sandman skill writes a ## Blockers section to task.md, so
// stripping is unnecessary and the content should be returned verbatim.
func TestContinuationTaskPrompt_PreservesBlockersSection(t *testing.T) {
	withBlockers := `# Task

Implement GitHub issue #1193.

## Execution Checklist

- [x] Create branch
- [x] Plan
- [x] Implement

## Blockers

- PR #1208 remains open, awaiting unrelated CI failure to be resolved before merge.

## Next Step

Wait for CI to be green.
`

	got := ContinuationTaskPrompt(withBlockers)

	if !strings.Contains(got, "## Blockers") {
		t.Fatalf("expected continuation prompt to preserve ## Blockers section, got:\n%s", got)
	}
	if !strings.Contains(got, "PR #1208 remains open, awaiting unrelated CI failure") {
		t.Fatalf("expected continuation prompt to preserve ## Blockers content, got:\n%s", got)
	}
	if !strings.Contains(got, "## Execution Checklist") {
		t.Fatalf("expected continuation prompt to preserve ## Execution Checklist, got:\n%s", got)
	}
}

// TestContinuationTaskPrompt_EmptyTaskFallsBackToTemplate verifies that the
// empty-file path (when .sandman/task.md does not exist) still produces a
// usable resume prompt — it should use the embedded DefaultPrompt as a
// fallback so the agent has the original Execution Checklist to work from.
func TestContinuationTaskPrompt_EmptyTaskFallsBackToTemplate(t *testing.T) {
	got := ContinuationTaskPrompt("")

	if !strings.Contains(got, "# Task") {
		t.Fatalf("expected fallback to include '# Task' heading from default-task-prompt.md, got:\n%s", firstLines(got, 20))
	}
	if !strings.Contains(got, "## Execution Checklist") {
		t.Fatalf("expected fallback to include '## Execution Checklist' from default-task-prompt.md, got:\n%s", firstLines(got, 40))
	}
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n") + "\n... (truncated)"
}
