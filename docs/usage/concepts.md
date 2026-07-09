# Concepts

The model behind Sandman, in prose. The canonical glossary lives at [`../../CONTEXT.md`](../../CONTEXT.md) and uses precise vocabulary with `_Avoid_` notes; this page is its human-readable companion.

## The CLI invocation produces a Batch, not a Run

When you run `sandman run 42 43 44`, three things happen:

1. The CLI parses flags, selects issues, and resolves `BlockedBy` (via body text + the GitHub REST API).
2. A **Batch** is created at `.sandman/batches/<batch-id>/`. The Batch is the long-lived artifact: a daemon process owns it, a control socket lives on it, and the per-row Run folders hang off it.
3. For each issue, an **AgentRun** is scheduled and executed. An AgentRun targets exactly one Issue and produces exactly one Branch (`sandman/<issue-number>-<slugified-title>` for issue-driven runs, `sandman/<slug>-<timestamp>` for prompt-only runs).

`Run` is the load-bearing term for everything you see in the portal: a Run is one row in the runs table, with its own RunID, log file, command socket, and (for review runs) `review-state.json`. A Run is to a Batch as a row is to a sheet.

> For the exact identity rules, see ADR-0030 (RunID templates) and ADR-0032 (batch layout + per-row RunID).

## Sandbox is the abstract isolation contract

A **Sandbox** is whatever isolates one AgentRun from the rest of the host. Two adapters ship:

- **WorktreeSandbox** — git worktree only. One worktree per AgentRun. Lightest weight, no filesystem or process isolation.
- **ContainerSandbox** — a Podman or Docker container hosting one or more worktrees. Pool size and per-container concurrency are tuned via `container_capacity` and `max_containers`.

Callers must not assume the sandbox is a worktree; the abstraction exists precisely so the orchestrator stays decoupled from the isolation strategy.

## Prompt-only mode skips the GitHub lookup

If `sandman run` is invoked with `--prompt` or `--template` and no issue selectors (no positional numbers, no `--label`, no `--query`, no `--auto`) and the final prompt omits the issue placeholders, Sandman enters prompt-only mode:

- No `gh` call. The branch is named `sandman/<slug>-<timestamp>`.
- The issue in events and history is `null`, summarised as `prompt-only` for human readers.
- Useful for ad-hoc scripted tasks ("return only OK") and as a continuation target for `sandman run --continue --run-id`.

## BlockedBy is a union, with strict and auto-expand modes

For each issue, Sandman builds a `BlockedBy` set from two sources:

1. Body references (`blocked by #N`, `depends on #N`, `## Blocked by` bullet list).
2. The GitHub REST API's native dependency surface (`blocked_by` on the issue response, plus dependency events).

Union, then validate. **In strict mode** (the default), any in-batch blocker that's missing produces `Error: missing blockers: #N`. **Auto-expand mode** (`--include-dependencies`) recursively includes transitive blockers, erroring on cycles with the cycle path. Dependencies on external blockers still require the upstream issue to be closed on GitHub right before the dependent starts; in-batch blocker success releases dependents immediately (ADR-0018).

## PRDs are detected by body shape, not labels

A **PRD** (Product Requirements Document) is a GitHub issue whose body contains the H2 sections `## Problem Statement`, `## Solution`, and `## User Stories`. Detection is structural. Sandman resolves a PRD into its child issues before execution; children are accepted only when their bodies contain a `## Parent` backlink to the PRD, and nested PRDs are rejected (ADR-0025). User-typed issue numbers skip the validation — the operator owns the choice.

## Sandman Review is the AFK review gate, not a separate agent

The review daemon listens for `/sandman review` comments on open PRs and launches a reviewer AgentRun against each. The reviewer agent writes its body to `<runDir>/decision.md`; the daemon reads the file, runs every leading-slash `sandman` substring through `RedactBody` (so the bot's body cannot re-trigger itself), and posts the redacted body via `gh pr comment`. The trust boundary is the daemon transform, not the prompt. A structural sniff survives as defence-in-depth: bodies that look like previous bot reviews (carry `## Previous review progress` *and* the literal `/sandman review` substring) are dropped before `ParseTrigger` runs.

## Auto Mode picks the issues for you

`sandman run --auto --count 3` lets a selection agent choose which of the eligible issues to run, up to the cap (default 50, configurable via `auto_max_count`). The conservative defaults (`--retries=3`, `--parallel=1`, `--container-capacity=1`, `--max-containers=1`) apply silently when those flags are not provided. The portal surfaces the candidate list as a chip; the selected subset appears in `run.finished.payload.selected`.

## Events are the source of truth, not mutable status

Run status is a projection over the append-only `.sandman/events.jsonl`. There is no mutable `Status` field anywhere on disk. The portal, the `status` command, the `history` command, and the per-row HTTP endpoints all derive state by folding event types (`run.started`, `run.queued`, `run.blocked`, `run.retry`, `run.idle_timeout`, `run.warning`, `run.finished`, `run.aborted`) through `events.RunState`. If a run's status looks wrong, start by tracing its events, not by looking for a mutable record.

## Two identifiers per run: the BatchId and the RunID

- **Public BatchId** — the batch-level identifier rendered in the Batch label and Details tab. Equals the batch folder basename, `batch.json.batchId`, `run.json.BatchID`, and `events.run.started.payload.batch_id`.
- **Per-row RunID** — the row-level identifier rendered per row and used by row-level actions (archive, abort, log download). Equals `run.json.runID` and `event.payload.run_id`.

For multi-issue batches they diverge (the BatchId carries the `+N` additional-count suffix, the RunID does not). For single-issue, prompt-only, review, and auto-select batches they're identical. See [ADR-0030](../adr/0030-standardize-run-id-and-run-dir.md) and [ADR-0032](../adr/0032-sandman-layout-redesign.md) for the kind-by-kind identity table.

## Worktrees carry the prompt and the progress

Each Run gets its own git worktree. Inside it, `.sandman/task.md` is:

- on a fresh run: the rendered Project Prompt Template with `{{ISSUE_NUMBER}}`, `{{ISSUE_TITLE}}`, etc. substituted.
- on `--continue`: read verbatim and passed to the agent as the resume prompt.

It is also the place the agent records checkpoint state (the Execution Checklist, the registered `## Next Step`), so a continued run picks up where it stopped instead of restarting.

## The portal is a visualization layer, not a control plane

`sandman portal` starts a local HTTP server that polls the current repository's `.sandman/batches/` tree on each request. It does not start daemons, does not stop daemons, and does not hold state. The two row-level writes it exposes (`POST /api/runs/abort`, `POST /api/runs/archive`) are thin wrappers over the per-row command socket and the CLI's `archive run` contract. Whole-batch archive is CLI-only.

## Where to read more

- [`../../CONTEXT.md`](../../CONTEXT.md) — canonical glossary (with `_Avoid_` and `_See_` notes)
- [Disk Layout](../architecture/disk-layout.md) — canonical on-disk tree, per-artifact table
- [Architecture Overview](../architecture/overview.md) — event-sourced state, DI seams, factory model
- [ADR-0030](../adr/0030-standardize-run-id-and-run-dir.md) — RunID and per-row identity
- [ADR-0032](../adr/0032-sandman-layout-redesign.md) — batch layout redesign
