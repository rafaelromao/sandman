# ADR-0012: Ralph Loop — Agent-Driven Issue Selection

## Status

superseded

Superseded by [ADR-0025](0025-rename-ralph-to-auto-mode.md), which renames the Ralph Loop to Auto Mode. The original decision and its context are preserved here for historical reference.

## Context

The `--ralph` flag (renamed from `--next`) currently sorts issues by issue number in ascending order. This is simple but ignores priority entirely. Issues with high business value or that unblock other work are treated the same as trivial, low-value tasks. As the issue count grows, the probability of wasting agent time on low-value work increases.

We need a mechanism that selects the most valuable issues for the agent to work on, not just the numerically next ones.

## Decision

Replace numeric sorting with an agent-driven two-phase selection process called the Ralph Loop.

### Two-phase flow

1. **Selection phase**: An agent run reads all `ready-for-agent` issues, evaluates them against configured criteria, and writes a JSON file listing the selected issue numbers.
2. **Execution phase**: The selected issues are implemented in order, one by one, as before.

### Key design choices and rationale

#### In-process execution

Selection runs the agent directly (no sandbox, no worktree) — the task is lightweight read-and-decide, not worth spinning up a container.

#### File handoff

The selection agent writes `.sandman/selected-issues.json` in the repository root. This avoids fragile stdout parsing and creates an explicit artifact that can be inspected, logged, or overridden.

#### Truncated issue bodies (~500 chars)

Issue bodies are truncated to approximately 500 characters to provide enough context to evaluate scope and value without bloating the selection prompt beyond useful limits.

#### Mode switching via file existence

When `.sandman/priority-selection-prompt.md` exists, the Ralph Loop is active. When absent, the system falls back to numeric sort order. This allows users to opt out simply by deleting the file.

#### Same agent/model

The selection phase uses the same agent preset and model as the execution phase. No separate "selection" agent.

#### `--next` → `--ralph`

The flag was renamed to signal that the behaviour has changed from simple numeric ordering to agent-driven prioritisation.

### Priority selection prompt

A built-in `priority_selection_prompt.md` is embedded via `//go:embed` in `internal/prompt/` alongside the existing `default-task-prompt.md`. It uses two new substitution keys — `{{CANDIDATE_ISSUES}}` (a formatted list) and `{{MAX_COUNT}}` (the N from `--ralph=N`) — that are populated at render time alongside the existing keys.

The `sandman init` command creates `.sandman/priority-selection-prompt.md` from the built-in default, following the same pattern as `prompt.md`. Running `init` again does not overwrite an existing file (idempotent).

## Consequences

### Positive

- Agent time is spent on the most valuable issues, not just the next ones.
- The selection criteria are explicit and customizable via the prompt template.
- Backward compatible — deleting `.sandman/priority-selection-prompt.md` restores numeric sort order.
- The file-handoff pattern is explicit and debuggable.

### Negative

- Each batch run incurs one extra agent invocation (the selection phase).
- Users must understand the selection prompt to customize criteria effectively.

### Neutral

- The selection phase uses the same agent config as execution — no new infrastructure.
- Issue bodies are truncated, so very long issues may lose nuance in the selection prompt.
