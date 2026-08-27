# ADR-0055: Opt into OpenCode session reuse

## Status

accepted

## Context

An AgentRun that waits for external pull-request progress must retain enough
runtime context for a later re-entry to continue the same OpenCode conversation.
Ordinary continuation has a different safety goal: it should preserve the
worktree and Task without silently carrying forward an old conversation.

## Decision

The built-in OpenCode preset requests structured output and atomically stores the
first valid session identity in each Run's `session.json`. Runtime-owned
re-entry after an external wait and in-session lifecycle relaunch select that
identity automatically. Ordinary `--continue` is fresh; an operator can opt in
with `--reuse-session`.

Exact-session launches may use one fallback to OpenCode `--continue` only when
the selected identity is absent or OpenCode reports the exact standalone
`Session not found` error. Cancellation, permission, authentication, provider,
model, and generic command failures do not enter this fallback. Custom commands
and non-OpenCode agents remain unchanged.

## Consequences

Await re-entry retains conversational context without keeping an agent process
alive. Explicit continuation is predictable by default and deliberate when it
reuses history. Runtime metadata is separate from event-sourced lifecycle
status, and old Runs without valid metadata remain recoverable through the
single narrow fallback.
