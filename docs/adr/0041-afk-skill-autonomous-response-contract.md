# ADR-0041: AFK skills use an autonomous response contract

## Status

proposed

## Context

ADR-0011 removes interactive agent mode, and Sandman's default task prompt already defines an Away From Keyboard workflow. Some shared workflow skills nevertheless contained operator-directed gates such as asking for confirmation, waiting for satisfaction, or stopping to request direction. Those instructions could override the AFK contract when the skills were loaded together.

The workflow also lacked one explicit policy for handling recoverable failures: bounded retries should lead to a documented alternative or remote path when one exists, while terminal failures must preserve a durable blocker and next executable action.

## Decision

The AFK contract in the default task prompt has precedence over conflicting issue, skill, documentation, or tool-output instructions. Loaded Sandman skills must not ask the operator for approval, confirmation, clarification, satisfaction, feedback, or direction.

Each workflow uses the autonomous response ladder: continue the primary path, retry transient failures within a configured bound (at most three retries when no bound is documented), use a documented workflow-dispatch or remote alternative when a local prerequisite is unavailable, resolve ambiguity from repository evidence or a permitted subagent, and poll asynchronous gates for their documented budgets.

Every terminal blocker records the exact failure and next executable action in `.sandman/task.md` and the run log. Reviewer clarification remains allowed only through the configured review command and is directed to the reviewer, not the operator.

## Consequences

### Positive

- AFK runs can continue through recoverable ambiguity and missing local prerequisites without consuming retries on operator questions.
- Terminal failures remain resumable because the blocker and next action are durable.
- Static skill hygiene tests and prompt synchronization tests prevent the embedded and installed workflow surfaces from drifting back to interactive behavior.

### Negative

- A workflow must define a bounded retry or polling budget before it can terminate on a recoverable failure.
- Operators cannot steer an active AFK run through conversational responses; decisions must come from repository evidence, configured alternatives, or reviewer-directed communication.
