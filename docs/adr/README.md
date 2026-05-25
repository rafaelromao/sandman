# Architectural Decision Records (ADRs)

This directory contains Architectural Decision Records (ADRs) for Sandman.

## What is an ADR?

An ADR is a document that captures an important architectural decision made along with its context and consequences. ADRs are immutable: once accepted or rejected, they are not modified (though they may be superseded by a new ADR).

## When to Write an ADR

Write an ADR when:

- A decision affects more than one module or package
- A decision is difficult or expensive to reverse
- A decision introduces a new dependency or technology
- A decision changes the project's domain vocabulary or agent behavior

## Format

All ADRs follow the MADR-style template established in `0000-use-adr-template.md`:

- Title and number
- Status (`proposed`, `accepted`, `rejected`, `superseded`)
- Context (what forces are at play)
- Decision (what we decided)
- Consequences (what becomes easier or harder)

## Numbering

ADRs are numbered sequentially starting from `0001`. The template ADR (`0000`) is reserved and should not be reused.

## Workflow

1. Create a new ADR file with the next sequential number and status `proposed`.
2. Open a discussion for feedback.
3. After review, update the status to `accepted` or `rejected`.
4. Merge the change.

## Index

| Number | Title | Status |
|--------|-------|--------|
| 0000 | Use MADR-style ADR template | accepted |
| 0001 | Remove PR creation from agent workflow | accepted |
| 0002 | Make shared container execution the default sandbox mode | accepted |
| 0003 | Dependency-aware batch execution | accepted |
| 0004 | Use GitHub REST API via `gh api` for native dependency queries | accepted |
| 0005 | Replace isolated container toggle with container capacity | accepted |
| 0006 | Built-in agent presets and Claude Code naming | accepted |
| 0007 | BuildToolsPreset and pinned init scaffolding | accepted |
| 0008 | Config mount resolution via temporary copy | accepted |
| 0009 | Stabilize container-backed smoke and e2e tests | accepted |
| 0010 | Local portal command and repo-scoped run scan | accepted |
