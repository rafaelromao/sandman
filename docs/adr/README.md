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
| 0010 | Local portal command and repo-scoped run scan | superseded |
| 0011 | Remove interactive agent mode | accepted |
| 0012 | Rename delegate-review to pr-review | accepted |
| 0013 | Sandman Review - Daemon-Monitored PR Reviews | accepted |
| 0014 | Store container config snapshots under the run dir | accepted |
| 0015 | Split OpenCode config snapshot from mutable state | accepted |
| 0016 | Unblock dependents from same-batch success | accepted |
| 0017 | Canonical test env vars for provider allowlists and e2e scenario gates | accepted |
| 0018 | Per-agent env vars to parameterize the model used by smoke and e2e tests | accepted |
| 0019 | Portal auto-runs `clean --stale` on startup | accepted |
| 0020 | Remove Pi agent support | accepted |
| 0021 | Specification expansion to child issues | accepted |
| 0022 | Rename Ralph Loop to Auto Mode | accepted |
| 0023 | Auto-recover from stranded worktrees in `--override` and `--continue` flows | accepted |
| 0024 | PR review prompt — omit `## Previous review progress` when there are no prior reviews | accepted |
| 0025 | Portal secondary-row chips for run context | accepted |
| 0026 | Standardize per-row RunID and run directory naming | accepted |
| 0027 | Portal read-only — commands panel removed | accepted |
| 0028 | Per-run command sockets for command and abort | accepted |
| 0029 | Review daemon — stateless on age, stateful on comment | accepted |
| 0030 | `run.retry` payload schema and closed `reason` vocabulary | accepted |
| 0031 | [placeholder] | accepted |
| 0032 | `.sandman/` layout redesign — batches, runs, and the master index | accepted |
| 0033 | Opencode host/sandbox version drift — warning + in-ARB retry | accepted |
| 0034 | Empty-child Specification runs as a regular issue | accepted |
| 0037 | Hermetic `gh` in pr-flow e2e tests | accepted |
| 0038 | Badge marker — paginated idempotency check | accepted |
| 0039 | Roll back Auto Mode (`--auto`, `--count`, `auto_max_count`) | accepted |
