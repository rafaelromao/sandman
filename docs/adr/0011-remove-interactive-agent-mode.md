# ADR-0011: Remove Interactive Agent Mode

## Status

accepted

## Context

Sandman previously exposed `sandman run --interactive` to attach agent execution directly to the user's terminal. That mode duplicated the normal sandbox execution path, complicated flag validation, and made `run` behavior depend on a flag that only changed transport, not batch selection.

## Decision

We will remove interactive agent mode from `sandman run`.

1. `sandman run` will always execute agents through the non-interactive sandbox command path.
2. No-args TTY invocation still uses the issue picker to choose issues before execution.
3. `--interactive` is no longer a supported flag and old invocations fail as unknown args.

## Consequences

- One execution model for `run`, which keeps selection orthogonal to execution.
- Less CLI surface area and fewer mode-specific code paths.
- Users who want issue selection without explicit numbers still get the TTY picker.
