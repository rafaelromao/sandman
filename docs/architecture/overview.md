# Architecture Overview

The reasoning behind Sandman's shape, in one page. For the canonical on-disk inventory see [Disk Layout](disk-layout.md). For the domain vocabulary see [`../../CONTEXT.md`](../../CONTEXT.md). For each load-bearing decision in isolation, see [`../adr/`](../adr/).

## Three load-bearing properties

### 1. The filesystem is the database

Sandman has no database. Every artifact that needs to survive a process restart lives in a flat file under `.sandman/` and is written atomically (temp-file + `os.Rename`). Coordination between processes happens over Unix domain sockets:

- Batch control socket: `<batch>/batch.sock`
- Per-row command socket: `<batch>/runs/<runID>/run.sock`
- Review daemon socket: `.sandman/reviews/review.sock`

Atomic writes mean a torn read can never produce a corrupt document. The portal, the CLI, and the daemon all read the same flat files; no single process owns the canonical copy.

### 2. Run status is a projection, not a record

The append-only `.sandman/events.jsonl` is the source of truth. There is no mutable `Status` field anywhere on disk. Every status a user sees (`status`, `history`, the portal table, the HTTP API) is computed by folding events through `events.RunState`.

This is why status bugs in Sandman are almost always bugs in projection logic or in the event types the projector consumes, not in a status field that needs to be set. If a run's status looks wrong, start by tracing its events.

The event types are documented in [Monitoring and Debugging > Event log](monitoring.md#event-log) and the projection rules are scattered across the event-projection tests under `internal/events/`.

### 3. Top-down dependency injection at the command boundary

`cmd.Dependencies` (`internal/cmd/root.go`) is the single composition root. It owns the wiring between the concrete adapters (`gh` CLI client, file-backed config store, JSONL event store, Docker / Podman container starter,…) and the in-process interfaces they implement. The orchestrator only knows about interfaces; nothing inside `internal/batch/` constructs a concrete dependency.

In tests, fakes are injected at the interface boundary (`Runner`, `Sandbox`, `Store`, `Client`, `EventLog`, `Renderer`) instead of mocking deep concrete types. This keeps the orchestrator testable without poking holes through its invariants.

## Two factory seams

`batch.Request` accepts two factory interfaces:

- `RunnableFactory` — produces the per-row `Runnable` (one per AgentRun).
- `SandboxFactory` — produces the `Sandbox` for each AgentRun (`WorktreeSandbox` or `ContainerSandbox`).

New `Runnable` or `Sandbox` implementations plug in by satisfying the interface; nothing else in the orchestrator changes. The `Sandbox.Exec` Setpgid invariant (see below) is the one contract every new implementation must honour.

## The `Sandbox.Exec` Setpgid invariant

`Sandbox.Exec` requires the spawned OS command to be its own process-group leader: `Setpgid: true` on the spawn. Without it, the shared `waitCmd` helper's `syscall.Kill(-cmd.Process.Pid, …)` lands on a non-existent PGID on context cancel, returns `ESRCH`, and `cmd.Wait()` blocks forever — surfacing to the user as "I clicked Abort but the run is still `active`."

Any new `Sandbox` implementation must set `Setpgid: true` on the spawned command. A separate follow-up issue tracks the related problem that killing the host-side `docker exec` / `podman exec` wrapper does not yet propagate to the in-container AgentRun.

## The daemon-as-poster trust boundary for review

The review pipeline does not let the LLM post to PRs directly. Instead:

1. The reviewer agent writes its body to `<runDir>/decision.md` (atomic temp-file + `os.Rename`).
2. The daemon reads the file, applies `RedactBody` (a regex `(?i)/sandman` → `sandman`), and posts the redacted body via `gh pr comment`.

The redactor is the load-bearing safety net for the no-self-loop invariant: it runs out-of-band of the LLM, so the bot's body can never contain the trigger substring regardless of what the prompt rule says. The structural sniff `LooksLikeBotReviewBody` is the surviving belt-and-braces backstop — bodies that look like previous bot reviews are dropped before `ParseTrigger` runs. See [ADR-0014 §Daemon-side redaction](../adr/0014-sandman-review-daemon-and-guard.md#daemon-side-redaction).

## The no-migration rule across the slice-1 contract change

The slice-1 contract change (issue #1917) renamed the public BatchId surface and the per-row RunID templates. Pre-upgrade batches carry old id shapes and are not migrated in place. Operators upgrading across #1917 are expected to delete `.sandman` and rebuild. The slice-by-slice migration notes are listed in [Disk Layout > Migration note](disk-layout.md#migration-note).

This is honest friction: it avoids a class of bugs where a partial migration produces ambiguous identifiers, and it keeps every release's on-disk reader linear.

## Project structure

```
cmd/sandman/main.go          # Composition root — wires interfaces to concrete adapters
internal/
  adr/                       # ADR test utilities (slice regression tests)
  atomicfs/                  # Atomic-write helpers: WriteAtomic, WriteAtomicJSON, OpenAppend
  batch/                     # Core domain: Orchestrator, AgentRun, DependencyResolver, factories
  batchindex/                # Batch index types and persistence (Index, Entry, Batch, RunManifest, ReviewState)
  cmd/                       # Cobra CLI commands
  config/                    # Config model, file store, built-in agent presets
  daemon/                    # Per-batch and per-run control sockets
  events/                    # Event log interface + JSONL implementation + RunState projection
  github/                    # GitHub client interface + gh CLI implementation
  paths/                     # Layout struct for all on-disk path resolution
  prompt/                    # Prompt template engine and renderers
  review/                    # Daemon-side redaction layer + review daemon
  runid/                     # NewRunID, Kind, batch/run ID generation
  sandbox/                   # Sandbox interface + WorktreeSandbox and ContainerSandbox adapters
  scaffold/                  # sandman init scaffolding logic
  shellenv/                  # Single-quoted `sh -c` env-prefix builder
  skill/                     # Sync function for embedded sandman skill tree
  testenv/                   # MkdirShort + SANDMAN_TEST_* env var helpers
```

## Where to read next

- [Disk Layout](disk-layout.md) — canonical on-disk tree, per-artifact table, migration notes
- [`../../AGENTS.md`](../../AGENTS.md) — operating rules for coding agents in this repo
- [`../../CONTEXT.md`](../../CONTEXT.md) — domain glossary
- [Positioning](../POSITIONING.md) — what Sandman is and isn't, in plain language
