# Project Structure

This page is for contributors modifying Sandman itself. For using Sandman, see [Get Started](../get-started/README.md) and [Using Sandman](../usage/README.md).

Sandman is a Go CLI with most application code under `internal/`, public documentation under `docs/`, and the binary entry point under `cmd/`.

## Top-level paths

| Path | Purpose |
|------|---------|
| `cmd/sandman/` | CLI entry point for the `sandman` binary |
| `internal/` | Sandman's application packages |
| `docs/` | Public docs, contributor docs, internal agent docs, and ADR files |
| `assets/` | Static assets used by docs and badges |
| `testdata/` | Shared fixture files |
| `Makefile` | Common local build and test targets |

## Core internal packages

| Path | Purpose |
|------|---------|
| `internal/cmd/` | Cobra command construction and command-level orchestration |
| `internal/batch/` | Batch execution, dependency ordering, and agent run lifecycle |
| `internal/sandbox/` | Worktree and container sandbox implementations |
| `internal/events/` | Append-only event log and projected run state |
| `internal/config/` | Config model, config file store, and built-in agent presets |
| `internal/github/` | GitHub access through the `gh` CLI boundary |
| `internal/prompt/` | Prompt template loading and rendering |
| `internal/daemon/` | Process coordination through Unix domain sockets |
| `internal/review/` | Review-loop support and redaction behavior |
| `internal/scaffold/` | Files written by `sandman init` |
| `internal/skill/` | Embedded Sandman skill assets synced during project setup |
| `internal/testenv/` | Shared helpers for portable tests |

## Documentation paths

| Path | Audience |
|------|----------|
| `docs/get-started/` | New users |
| `docs/usage/` | Users running Sandman in their own repositories |
| `docs/architecture/` | Developers who need the system model without changing internals |
| `docs/help/` | Troubleshooting and product-level explanations |
| `docs/development/` | Contributors changing Sandman itself |
| `docs/agents/` | Internal agent operating context; excluded from the public docs sidebar |
| `docs/adr/` | Decision records; excluded from the public docs sidebar |

## Where to start

For command behavior, start in `internal/cmd/`, then follow the injected dependencies into the relevant package.

For run lifecycle or batch behavior, start in `internal/batch/` and trace how sandboxes and run events are produced.

For status or history behavior, start with the event log and run-state projection rather than looking for a mutable status record.

For filesystem layout questions, use [Disk Layout](../architecture/disk-layout.md) first, then inspect the package that owns the specific file.
