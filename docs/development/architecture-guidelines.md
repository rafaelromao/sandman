# Architecture Guidelines for Contributors

This page is for contributors modifying Sandman itself. For using Sandman, see [Get Started](../get-started/README.md) and [Using Sandman](../usage/README.md).

Use these rules when changing Sandman's implementation. They describe the current architecture; they are not a decision-history index.

## Preserve event-sourced state

Run status is derived from the append-only event log. Do not add mutable status fields as shortcuts when status should be projected from events.

When changing lifecycle behavior:

1. Identify the event types involved.
2. Trace the fold/projection path that produces current state.
3. Add tests against the projected behavior.

## Preserve dependency-injection seams

Command code should stay testable through explicit dependencies. Prefer changing existing seams over introducing hidden globals or constructing deep concrete dependencies inline.

When writing tests, fake the documented boundary rather than mocking deep implementation details.

## Preserve atomic filesystem writes

Sandman stores state in flat files under `.sandman/`. Writers should use atomic replacement patterns: write a temp file, flush it as appropriate, then rename it into place.

Avoid in-place mutation for files that other commands may read concurrently.

## Preserve socket ownership

Process coordination uses Unix domain sockets. Socket changes should keep ownership and cleanup clear:

- The process that creates a socket should own its lifecycle.
- Cleanup should tolerate already-removed sockets.
- Tests that bind sockets should use short temp paths; see [Test Infrastructure](test-infrastructure.md#short-unix-socket-paths).

## Check blast radius before central changes

Before editing shared or central code, check what depends on it and prefer the smallest safe change.

Pay special attention to:

- Event definitions and run-state projection
- Command dependency wiring
- Batch and sandbox interfaces
- Persistence helpers
- IPC and socket lifecycle code

## Keep public behavior and internal mechanics separate

User-facing docs should explain current behavior and commands. Contributor docs can name packages and seams when that helps people change Sandman safely.

Embedded skills are different: they should describe user-facing workflows and avoid unstable implementation details. See [Documentation](documentation.md).
