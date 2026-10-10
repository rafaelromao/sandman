# Architecture Overview

The reasoning behind Sandman's shape, in one page. For the canonical on-disk inventory see [Disk Layout](disk-layout.md).

## The filesystem is the database

Sandman has no database. Every artifact that needs to survive a process restart lives in a flat file under `.sandman/` and is written atomically (temp-file + `os.Rename`). Coordination between processes happens over Unix domain sockets:

- Batch control socket: `<batch>/batch.sock`
- Per-row command socket: `<batch>/runs/<runID>/run.sock`
- Review daemon socket: `.sandman/reviews/review.sock`

Atomic writes mean a torn read can never produce a corrupt document. The portal, the CLI, and the daemon all read the same flat files; no single process owns the canonical copy.

## Run status is a projection, not a record

The append-only `.sandman/events.jsonl` is the sole lifecycle authority. Status/history, Portal/API, archive/cleanup and recovery use its shared terminality projection. Initial `run.queued` admissions tagged `initial_admission: true` are unfinished; untagged historical records and explicit `terminal_placeholder` records retain terminal skipped-placeholder meaning. A started `run.capacity_queued` continuation remains waiting: readiness/admission are not a return to initial queue. Same RunID retains active duration across waits and batches; new RunID starts fresh. The full [AgentRun state machine](run-state-machine.md) is the canonical transition and budget contract.

Artifact state is independent: indexes record location/availability; socket and advisory RunID claims establish ownership. Missing artifacts cannot revise outcomes. Atomic wait/schedule leases preserve valid ownerless intent for five minutes capped by its operation deadline; normal admission rehydrates within grace. Explicit abort persists terminal cancellation and cannot be reclaimed. Expired recovery appends `run.aborted`; neither socket death nor lease data is itself a lifecycle outcome. Unknown artifact-only rows have no fabricated finish time.

`run.json` is an atomically replaced artifact manifest: identity, branch, worktree, kind, and creation metadata support artifact lookup and ownership validation. Its legacy `status` field is a best-effort execution snapshot written on start/finish/recovery for compatibility and inspection. It may lag events, disagree after interruption, or be missing; it is never a fallback lifecycle authority and never authorizes archiving or reclamation. No schema migration is required. Missing events mean unknown lifecycle and cannot authorize terminal-only operations; event-read failures fail closed. Missing manifests may prevent age/ownership-dependent operations without changing lifecycle.

Individual archive moves only an event-terminal Run. Whole-batch archive additionally requires a dead batch daemon and terminal projections for all members known from Run directories, index records, and event batch identity. Cleanup retains its manifest/path ownership checks in addition to event terminality. If a run's displayed status looks wrong, start by tracing its events.

The event types are documented in [Monitoring](../usage/monitoring.md#event-log).

External-wait bounds and execution allowances are distinct. Managed CI/review
keep hard deadlines; standalone CI retains its 60-minute/three-fix invocation-local
bounds. Implementation relaunches are session-local, not cumulative repair
reservations. Quota preserves completed polling across restart, with a diagnostic
exhaustion estimate and unchanged ownerless grace. See the canonical state machine
and [AFK exit baseline](../development/afk-exit-regression-baseline.md).

## Top-down dependency injection at the command boundary

`cmd.Dependencies` is the single composition root. It owns the wiring between the concrete adapters (`gh` CLI client, file-backed config store, JSONL event store, Docker / Podman container starter,…) and the in-process interfaces they implement. The orchestrator only knows about interfaces and does not construct a concrete dependency.

In tests, fakes are injected at the interface boundary (`Runner`, `Sandbox`, `Store`, `Client`, `EventLog`, `Renderer`) instead of mocking deep concrete types. This keeps the orchestrator testable without poking holes through its invariants.

## Two factory seams

`batch.Request` is the public batch input (issues, config, flags) and does not carry factories. Factories are orchestrator dependencies held in `runDeps` / `OrchestratorOpt`:

- `RunnableFactory` — produces the per-row `Runnable` (one per AgentRun).
- `SandboxFactory` — produces the `Sandbox` for each AgentRun (`WorktreeSandbox` or `ContainerSandbox`).

See `internal/batch/row_spec.go:RunExecutor` / `runDeps` and `internal/cmd/root.go:Dependencies`. New `Runnable` or `Sandbox` implementations plug in by satisfying the interface; nothing else in the orchestrator changes.

## The `Sandbox.Exec` Setpgid invariant

`Sandbox.Exec` requires the spawned OS command to be its own process-group leader: `Setpgid: true` on the spawn. Without it, the shared `waitCmd` helper's `syscall.Kill(-cmd.Process.Pid, …)` lands on a non-existent PGID on context cancel, returns `ESRCH`, and `cmd.Wait()` blocks forever — surfacing to the user as "I clicked Abort but the run is still `active`."

Any new `Sandbox` implementation must set `Setpgid: true` on the spawned command.

> Note: killing the host-side `docker exec` / `podman exec` wrapper does not yet propagate to the in-container AgentRun. This is a known limitation.

The production GitHub CLI adapter follows the same cancellation rule. Its
`realRunner` starts `gh` as a process-group leader and cancels the negative PGID
so descendants do not survive a timeout or context cancellation. Tests may
replace that runner through the existing `WithRunner` seam.

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
- [CONTRIBUTING](../../CONTRIBUTING.md) — project structure and key interfaces
