# Sandman

CLI tool for orchestrating AFK coding agents in isolated sandboxes.

## Purpose

This file provides operating instructions for coding agents working in this repository. Follow the architecture notes and workflow rules below before making changes.

## Architecture

- **Event-sourced state**: Run status is a projection over the append-only `.sandman/events.jsonl`, not mutable records. `events.RunState` folds events into current state. If a status looks wrong, start by tracing the relevant event types and the fold/projection logic rather than searching for mutable status fields.
- **Factory seams**: `cmd.Dependencies` (`internal/cmd/root.go`) is the top-level dependency injection struct. `batch.Request` accepts `RunnableFactory` and `SandboxFactory` interfaces. In tests, inject fakes at these seams rather than mocking deep concrete types.
- **Filesystem as data store**: There is no database. State lives in flat files under `.sandman/` (manifests, logs, review state), written atomically via temp-file + `os.Rename`. IPC uses Unix domain sockets.

## Mandatory workflow

For all code discovery, symbol lookup, dependency tracing, and blast-radius analysis, use `codeindex` before broad grep or opening many files.

Required sequence:

1. If the task touches code, start with `codeindex`, not repo-wide grep.
2. Use `codeindex` to narrow the candidate files or symbols first.
3. Only open source files after `codeindex` identifies the most relevant targets.
4. Before changing shared or high-risk code, assess dependencies or blast radius first.
5. If `codeindex` is unavailable, incomplete, or stale, state that explicitly and fall back to targeted reads only.

Do not start by scanning the repository broadly when a `codeindex` command can answer the question.

## Symbol lookup and impact analysis

Use `codeindex` CLI before grepping for symbols or assessing blast radius.

### Core commands

| Command | Purpose |
|---------|---------|
| `codeindex lookup <symbol>` | Find where a function, type, method, or symbol is defined (file + line). |
| `codeindex impact <file>` | Blast-radius report showing how many files are affected if this file changes. |
| `codeindex dependencies <file>` | Show imports and imported-by relationships for a file. |
| `codeindex high-blast --threshold N` | List the riskiest files with blast score greater than or equal to `N`. |
| `codeindex search "natural language"` | Hybrid semantic + keyword + graph search for cross-cutting concepts. |

### Command selection rules

Use the smallest command that answers the question:

- Need a definition location for a symbol, interface, type, method, or function: use `codeindex lookup <symbol>`.
- Need to understand what depends on a file before editing it: use `codeindex dependencies <file>` or `codeindex impact <file>`.
- Need to understand risky change areas during refactors: use `codeindex high-blast --threshold N`.
- Need to find a concept that may span multiple files or is easier to describe than to name precisely: use `codeindex search "..."`.

### Read discipline

After a `codeindex` query:

- Read only the files that the query identifies as likely relevant.
- Avoid opening many adjacent files “just in case”.
- Avoid repo-wide grep unless `codeindex` cannot answer or the task concerns non-indexed content.

## Sandman task routing

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

If the task involves execution flow, runners, or sandbox lifecycle:

- Start with `batch.Request`.
- Trace `RunnableFactory` and `SandboxFactory`.
- Keep orchestration logic testable by preserving interface seams.

### Persistence and file safety

If the task involves manifests, logs, review state, or other persisted data:

- Inspect `.sandman/` writers and readers first.
- Preserve atomic write behavior using temp-file + `os.Rename`.
- Do not introduce partial-write or in-place mutation patterns where existing code expects atomic replacement.

### IPC and socket behavior

If the task involves coordination between processes:

- Inspect Unix domain socket code paths first.
- Prefer changes that preserve clear ownership and cleanup of socket lifecycle.

## Direct-read exceptions

Use direct file reads without waiting on `codeindex` only when the task is mainly about:

- `CONTEXT.md` and domain terminology.
- ADRs under `docs/adr/`.
- Agent guidance under `docs/agents/`.
- Shell scripts, JSON, YAML, logs, fixtures, or generated artifacts.
- Exact implementation details after `codeindex` has already narrowed the search.

For architecture or domain questions, read the relevant docs early instead of inferring intent from code alone.

## Change-safety rules

Before editing shared or central code, assess downstream impact first.

Always run dependency or blast-radius checks before changing:

- Event definitions or event-fold/projection logic.
- Shared command wiring or `cmd.Dependencies`.
- `RunnableFactory` or `SandboxFactory` interfaces and implementations.
- Files with high blast radius.
- Persistence code under `.sandman/`.
- IPC or socket lifecycle code.

If a file has high blast radius, prefer the smallest safe change and inspect dependent paths before editing.

## Testing guidance

When writing or updating tests:

- Mock or fake at the documented seams: `cmd.Dependencies`, `RunnableFactory`, and `SandboxFactory`.
- Do not mock deep concrete internals when an interface seam already exists.
- Preserve event-sourced behavior in tests; verify projections/folds rather than inventing alternate mutable-state shortcuts.

## Implementation constraints

- Preserve event-sourced reasoning. Do not add mutable status fields as shortcuts if status should be derived from events.
- Preserve atomic filesystem semantics. Prefer temp-file + rename over direct in-place writes.
- Preserve DI seams. Prefer injecting dependencies over hard-coding globals or constructing deep dependencies inline.
- Keep IPC changes compatible with Unix domain socket assumptions already used in the repo.

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
2. Use `codeindex` to locate the relevant symbol, file, dependency set, or blast radius.
3. Read only the narrowed code paths.
4. Read `CONTEXT.md` or ADRs if domain or architectural intent matters.
5. Make the smallest coherent change.
6. Run formatting, vetting, and relevant tests.
7. Summarize what changed, what was verified, and any remaining risk.
