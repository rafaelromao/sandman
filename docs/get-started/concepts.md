# Concepts

The model behind Sandman, in prose. This page is the human-readable companion to the canonical glossary in [`CONTEXT.md`](../../CONTEXT.md).

## Batch, AgentRun, and Run

When you run `sandman run 42 43 44`, three things happen:

1. The CLI parses flags, selects issues, and resolves `BlockedBy` relationships.
2. A **Batch** is created at `.sandman/batches/<batch-id>/`. The Batch is the long-lived artifact: a daemon process owns it, a control socket lives on it, and the per-row Run folders hang off it.
3. For each issue, an **AgentRun** is scheduled and executed. An AgentRun targets exactly one Issue and produces exactly one Branch (`sandman/<issue-number>-<slugified-title>` for issue-driven runs, `sandman/<slug>-<timestamp>` for prompt-only runs).

**Run** is the load-bearing term for everything you see in the portal: one row in the runs table, with its own RunID, log file, command socket, and (for review runs) `review-state.json`.

## Sandbox

A **Sandbox** is whatever isolates one AgentRun from the rest of the host. Two adapters ship:

- **WorktreeSandbox** — git worktree only. One worktree per AgentRun. Lightest weight, no filesystem or process isolation.
- **ContainerSandbox** — a Podman or Docker container hosting one or more worktrees. Pool size and per-container concurrency are tuned via `container_capacity` and `max_containers`.

Callers must not assume the sandbox is a worktree; the abstraction keeps orchestration decoupled from the isolation strategy.

## Prompt-only mode

If `sandman run` is invoked with `--prompt` or `--template` and no issue selectors, and the final prompt omits the issue placeholders, Sandman enters prompt-only mode:

- No `gh` call. The branch is named `sandman/<slug>-<timestamp>`.
- The issue in events and history is `null`, summarised as `prompt-only` for human readers.
- Useful for ad-hoc scripted tasks and as a continuation target for `sandman run --continue --run-id`.

## BlockedBy

For each issue, Sandman builds a `BlockedBy` set from two sources:

1. Body references (`## Blocked by` / `## Depends on` / `## Blocked-by` heading sections, with bare `#N` shorthand, link bullet, titled link, or trailing-annotation bullet entries).
2. The GitHub REST API's native dependency surface (`blocked_by` on the issue response).

Union, then validate. **Strict mode** (the default) errors if any in-batch blocker is missing. **Auto-expand mode** (`--include-dependencies`) recursively includes transitive blockers and errors on cycles. In-batch blocker success releases dependents immediately; external blockers must be closed on GitHub before the dependent starts. Inline phrases such as `Blocked by #10` or `Depends on #10` outside a heading are not recognized — they were retired because prose mentions were sensitive noise (see ADR-0025 and `docs/usage/issue-body-formats.md`).

## Specifications

A **Specification** is a GitHub issue whose body declares children — in any of the supported forms (body heading, body prose, issue comments, native GitHub sub-issues, mention-search fallback) — OR whose body carries the canonical Specification shape (`## Problem Statement` + `## Solution` + optional `## User Stories`). Detection is structural, not label-based, and the no-other-gate contract drops the body-shape as the identification gate: child existence alone is sufficient. Sandman resolves a Specification into its child issues before execution; children are accepted only when their bodies contain a `## Parent` backlink to the Specification (user-typed children bypass that check). Nested Specifications are flattened recursively rather than rejected. Inline phrases like `Children: #10`, `Child Issues: #10`, `Blocked by #10`, and `Depends on #10` outside a heading are deprecated — prose mentions were sensitive noise.

## Sandman Review

The review daemon listens for `/sandman review` comments on open PRs and launches a reviewer AgentRun against each. The reviewer agent writes its body to the review worktree's `decision.md`; the daemon reads the file, strips every leading-slash `sandman` substring via `RedactBody` (so the bot's body cannot re-trigger itself), and posts the redacted body via `gh pr comment`. The trust boundary is the daemon transform, not the prompt.

## Events as the source of truth

Run status is a projection over the append-only `.sandman/events.jsonl`. There is no mutable `Status` field anywhere on disk. The portal, the `status` command, the `history` command, and the per-row HTTP endpoints all derive state by folding event types through `events.RunState`. If a run's status looks wrong, start by tracing its events.

## BatchId and RunID

Every run carries two identifiers:

- **Public BatchId** — the batch-level identifier. Equals the batch folder basename.
- **Per-row RunID** — the row-level identifier used by row-level actions (archive, abort, log download).

For multi-issue batches they diverge (the BatchId carries the `+N` additional-count suffix, the RunID does not). For single-issue, prompt-only, and review batches they are identical.

## See also

- [Overview](overview.md) — what Sandman is and the delivery loop
- [Architecture](../architecture/overview.md) — event-sourced state, DI seams, factory model
- [Disk Layout](../architecture/disk-layout.md) — canonical on-disk tree
- [Commands](../usage/commands.md) — every CLI flag
