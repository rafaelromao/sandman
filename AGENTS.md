# Sandman

CLI tool for orchestrating AFK coding agents in isolated sandboxes.

## Purpose

This file provides operating instructions for coding agents working in this repository. Follow the architecture notes and workflow rules below before making changes.

## Architecture

- **Event-sourced state**: Run status is a projection over the append-only `.sandman/events.jsonl`, not mutable records. `events.RunState` folds events into current state. If a status looks wrong, start by tracing the relevant event types and the fold/projection logic rather than searching for mutable status fields.
- **Factory seams**: `cmd.Dependencies` (`internal/cmd/root.go`) is the top-level dependency injection struct. The per-run seam is `RunExecutor{Execute(ctx, row RowSpec)}` (`internal/batch/row_spec.go`); its implementation holds a `runDeps` struct (github.Client, prompt.IssueRenderer, events.EventLog, RunnableFactory, SandboxFactory, paths.Layout, heartbeatTickInterval, errorLog, runSessionOptions, verifyPath) and a narrow `runCoordination` interface (`*Orchestrator` implements it; RunBatch owns the state). Test injection of the orchestrator's behavior goes through `OrchestratorOpt` options on `NewOrchestrator` — `WithRunnableFactory`, `WithSandboxFactory`, `WithContainerRuntimeFactory`, `WithErrorLog`, `WithRunSessionOpts`, `WithHeartbeatTickInterval`, `WithVerifyPath` (and `WithBadgeHooker` for production). `batch.Request` is the public batch input (issues, config, flags) — it does **not** carry factories. Inject fakes at the `runDeps`/`OrchestratorOpt` seams rather than mocking deep concrete types.
- **Filesystem as data store**: There is no database. State lives in flat files under `.sandman/` (manifests, logs, review state), written atomically via temp-file + `os.Rename`. IPC uses Unix domain sockets.

## Sandman task routing

Canonical on-disk layout reference: `docs/architecture/disk-layout.md`.

Start from the most likely architectural seam for the problem.

### Status and run-state bugs

If a bug involves status, lifecycle, or current run state:

- Start with event types and `events.RunState`.
- Trace the fold/projection path from `.sandman/events.jsonl`.
- Do not assume there is a canonical mutable status record.

### Command wiring and dependency injection

If the task involves CLI wiring, root command setup, or substitution in tests:

- Start with `cmd.Dependencies` in `internal/cmd/root.go`.
- Then inspect the relevant command constructors and injected dependencies.
- Prefer changing seams over introducing new hidden globals.

### Batch and sandbox orchestration

If a task involves execution flow, runners, or sandbox lifecycle:

- Start with `RunExecutor` and `RowSpec` (`internal/batch/row_spec.go`); the per-run seam is `Execute(ctx, row RowSpec)`. `batch.Request` is the public batch input that feeds it.
- Trace `runDeps` for the test/constructor seam and `OrchestratorOpt` (`WithRunnableFactory`, `WithSandboxFactory`, `WithContainerRuntimeFactory`, `WithErrorLog`, `WithRunSessionOpts`, ...) for test injection. `RunnableFactory` and `SandboxFactory` are the factory interface types inside `runDeps`.
- Keep orchestration logic testable by preserving the `RunExecutor`/`runDeps`/`OrchestratorOpt` interface seams.

### Persistence and file safety

If the task involves manifests, logs, review state, or other persisted data:

- Inspect `.sandman/` writers and readers first.
- Preserve atomic write behavior using temp-file + `os.Rename`.
- Do not introduce partial-write or in-place mutation patterns where existing code expects atomic replacement.

### IPC and socket behavior

If the task involves coordination between processes:

- Inspect Unix domain socket code paths first.
- Prefer changes that preserve clear ownership and cleanup of socket lifecycle.

## Change-safety rules

Before editing shared or central code, assess downstream impact first.

Always run dependency or blast-radius checks before changing:

- Event definitions or event-fold/projection logic.
- Shared command wiring or `cmd.Dependencies`.
- The per-run seam: `RunExecutor` interface, `RowSpec`/`BatchConfig`, `runDeps` (incl. `RunnableFactory`/`SandboxFactory` and their implementations), and `runCoordination`.
- `OrchestratorOpt` options on `NewOrchestrator` (`WithRunnableFactory`, `WithSandboxFactory`, `WithContainerRuntimeFactory`, `WithErrorLog`, `WithRunSessionOpts`, `WithHeartbeatTickInterval`, `WithVerifyPath`, `WithBadgeHooker`).
- Files with high blast radius.
- Persistence code under `.sandman/`.
- IPC or socket lifecycle code.

If a file has high blast radius, prefer the smallest safe change and inspect dependent paths before editing.

## Testing guidance

When writing or updating tests:

- Mock or fake at the documented seams: `cmd.Dependencies` (top level); `runDeps` fields and `OrchestratorOpt` options (`WithRunnableFactory`, `WithSandboxFactory`, `WithContainerRuntimeFactory`, `WithErrorLog`, `WithRunSessionOpts`, `WithHeartbeatTickInterval`, `WithVerifyPath`) for orchestrator-level injection; `RunnableFactory`/`SandboxFactory` fakes that satisfy the factory interfaces.
- Do not mock deep concrete internals when an interface seam already exists.
- Preserve event-sourced behavior in tests; verify projections/folds rather than inventing alternate mutable-state shortcuts.

## Implementation constraints

- Preserve event-sourced reasoning. Do not add mutable status fields as shortcuts if status should be derived from events.
- Preserve atomic filesystem semantics. Prefer temp-file + rename over direct in-place writes.
- Preserve DI seams. Prefer injecting dependencies over hard-coding globals or constructing deep dependencies inline.
- Keep IPC changes compatible with Unix domain socket assumptions already used in the repo.

## Skill content constraints

Skills under `internal/skill/sandman/` describe how coding agents work with the **user-facing** concepts (`.sandman/` state files, public CLI, review commands, worktrees, ADRs). They must not reference Sandman's **internals** — Go package paths under `internal/`, Go type and function names like `processPR` / `MarkSeen` / `launchReview`, or other implementation details that may shift.

Skills also must not mention the GitHub issue tracker directly (issue numbers, kanban labels, or triage vocabulary). When a skill needs to refer to a project decision, link to the relevant ADR (`docs/adr/`) or `CONTEXT.md` instead; when it needs to refer to the user's work item, describe it behaviorally ("the implementor's open work item") rather than by tracker coordinates.

Concretely: a contributor reviewing a skill should be able to read it without knowing Sandman's package layout or workflow automation. If a paragraph needs re-reading after the user-facing vocab is internalized, it shouldn't name internals.

**Violations** to be caught during `sandman-self-review` and `sandman-pr-review`. The regression net is `internal/skill/sandman/skill_hygiene_test.go`, which scans all skill prose for forbidden internal package paths, forbidden internal Go identifiers, and tracker jargon.

## Before committing

Run:

```bash
gofmt -w . && go vet ./...
```

If the change affects behavior materially, also run the most relevant targeted tests for the touched package(s) before finalizing.

## Agent skills and repository references

Use these repository-specific references when appropriate:

- Issue tracker: `docs/agents/issue-tracker.md`
- Triage labels: `docs/agents/triage-labels.md`
- Domain vocabulary: `CONTEXT.md`
- ADRs: `docs/adr/`

## Preferred operating pattern

For most non-trivial tasks, follow this order:

1. Read this file.
2. Read only the narrowed code paths.
3. Read `CONTEXT.md` or ADRs if domain or architectural intent matters.
4. Make the smallest coherent change.
5. Run formatting, vetting, and relevant tests.
6. Summarize what changed, what was verified, and any remaining risk.

