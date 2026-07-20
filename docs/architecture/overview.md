# Architecture Overview

The reasoning behind Sandman's shape, in one page. For the canonical on-disk inventory see [Disk Layout](disk-layout.md).

## The filesystem is the database

Sandman has no database. Every artifact that needs to survive a process restart lives in a flat file under `.sandman/` and is written atomically (temp-file + `os.Rename`). Coordination between processes happens over Unix domain sockets:

- Batch control socket: `<batch>/batch.sock`
- Per-row command socket: `<batch>/runs/<runID>/run.sock`
- Review daemon socket: `.sandman/reviews/review.sock`

Atomic writes mean a torn read can never produce a corrupt document. The portal, the CLI, and the daemon all read the same flat files; no single process owns the canonical copy.

## Run status is a projection, not a record

The append-only `.sandman/events.jsonl` is the source of truth. There is no mutable `Status` field anywhere on disk. Every status a user sees (`status`, `history`, the portal table, the HTTP API) is computed by folding events through `events.RunState`.

This is why status bugs in Sandman are almost always bugs in projection logic or in the event types the projector consumes, not in a status field that needs to be set. If a run's status looks wrong, start by tracing its events.

The event types are documented in [Monitoring](../usage/monitoring.md#event-log).

## Top-down dependency injection at the command boundary

`cmd.Dependencies` is the single composition root. It owns the wiring between the concrete adapters (`gh` CLI client, file-backed config store, JSONL event store, Docker / Podman container starter,…) and the in-process interfaces they implement. The orchestrator only knows about interfaces and does not construct a concrete dependency.

In tests, fakes are injected at the interface boundary (`Runner`, `Sandbox`, `Store`, `Client`, `EventLog`, `Renderer`) instead of mocking deep concrete types. This keeps the orchestrator testable without poking holes through its invariants.

## Two factory seams

`batch.Request` accepts two factory interfaces:

- `RunnableFactory` — produces the per-row `Runnable` (one per AgentRun).
- `SandboxFactory` — produces the `Sandbox` for each AgentRun (`WorktreeSandbox` or `ContainerSandbox`).

New `Runnable` or `Sandbox` implementations plug in by satisfying the interface; nothing else in the orchestrator changes.

## The `Sandbox.Exec` Setpgid invariant

`Sandbox.Exec` requires the spawned OS command to be its own process-group leader: `Setpgid: true` on the spawn. Without it, the shared `waitCmd` helper's `syscall.Kill(-cmd.Process.Pid, …)` lands on a non-existent PGID on context cancel, returns `ESRCH`, and `cmd.Wait()` blocks forever — surfacing to the user as "I clicked Abort but the run is still `active`."

Any new `Sandbox` implementation must set `Setpgid: true` on the spawned command.

> Note: killing the host-side `docker exec` / `podman exec` wrapper does not yet propagate to the in-container AgentRun. This is a known limitation.

## The daemon-as-poster trust boundary for review

The review pipeline does not let the LLM post to PRs directly. Instead:

1. The reviewer agent writes its body to the review worktree's `decision.md` (atomic temp-file + `os.Rename`).
2. The daemon reads the file, applies `RedactBody` (`(?i)/sandman` → `sandman`), and posts the redacted body via `gh pr comment`.

The redactor is the load-bearing safety net for the no-self-loop invariant: it runs out-of-band of the LLM, so the bot's body can never contain the trigger substring regardless of what the prompt rule says. The structural sniff `LooksLikeBotReviewBody` is defence-in-depth — bodies that look like previous bot reviews are dropped before `ParseTrigger` runs.

## No state migration across version upgrades

Sandman does not migrate on-disk state across version upgrades. After upgrading, clear `.sandman/` and re-run `sandman init`. This avoids ambiguous identifiers and keeps the on-disk reader linear. See [Troubleshooting](../help/troubleshooting.md#portal-shows-unknown-rows-after-upgrading-sandman) for the symptom and fix.

## Project structure

```
cmd/sandman/main.go          # Composition root — wires interfaces to concrete adapters
internal/
  atomicfs/                  # Atomic-write helpers: WriteAtomic, WriteAtomicJSON, OpenAppend
  batch/                     # Core domain: Orchestrator, AgentRun, DependencyResolver, factories
  batchindex/                # Batch index types and persistence
  cmd/                       # Cobra CLI commands
  config/                    # Config model, file store, built-in agent presets
  daemon/                    # Per-batch and per-run control sockets
  events/                    # Event log interface + JSONL implementation + RunState projection
  github/                    # GitHub client interface + gh CLI implementation
  paths/                     # Layout struct for all on-disk path resolution
  prompt/                    # Prompt template engine and renderers
  review/                    # Daemon-side redaction layer + review daemon
  runid/                     # Run ID generation
  sandbox/                   # Sandbox interface + WorktreeSandbox and ContainerSandbox adapters
  scaffold/                  # sandman init scaffolding logic
  shellenv/                  # Single-quoted sh -c env-prefix builder
  skill/                     # Sync function for embedded sandman skill tree
  testenv/                   # Test environment helpers
```

## See also

- [Disk Layout](disk-layout.md) — canonical on-disk tree and per-artifact table
- [Concepts](../get-started/concepts.md) — the Batch / AgentRun / Sandbox model in prose
- [Positioning](../help/positioning.md) — what Sandman is and isn't
- [CONTRIBUTING](../../CONTRIBUTING.md) — project structure and key interfaces
