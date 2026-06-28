# ADR-0033: Per-run command sockets for command and abort

## Status

accepted

## Context

In the original layout, the daemon created one command server (`cmd.sock`) at the batch root, multiplexed across all AgentRuns in the batch. External abort tools had no per-run address — they could only signal the batch as a whole, not a specific run within it.

The layout redesign (ADR-0032) introduced per-run folders inside each batch. With that structural change, it became possible and desirable to give each AgentRun its own named command socket, addressable by path.

Parent: [#1218](https://github.com/rafaelromao/sandman/issues/1218) — Sandman `.sandman/` folder layout redesign, "ADRs to write" section, Phase 6.

Relevant user stories from #1218: 27–30.

## Decision

### Socket topology

Each batch has two socket types:

| Socket | Location | Purpose |
|--------|----------|---------|
| `batch.sock` | Batch root `.sandman/batches/<batch-id>/batch.sock` | Batch-level attach/streaming; `IsRunActive` liveness probe |
| `run.sock` | Run folder `.sandman/batches/<batch-id>/runs/<run-id>/run.sock` | Per-run command/abort; addressed by path from external tools |

The daemon creates one command server (`run.sock`) **per AgentRun** inside each run folder. The command server dispatches to the orchestrator's per-issue cancel API, which maps an external abort request to a specific `AgentRun`.

`RunSocketPath()` (`internal/daemon/runfs.go:247`) returns the per-run socket path and is used by the portal to probe run liveness.

### Why per-run sockets

**External abort tools address runs by path.** An external tool (e.g., a human operator or automation script) that wants to abort a specific AgentRun without affecting siblings needs a stable, path-based handle. With `run.sock` inside each run folder, the tool connects to `<batch>/runs/<runID>/run.sock` — the path is the address and encodes the run identity directly.

**One command server per AgentRun.** The command server is created per-AgentRun inside its run folder and dispatches to the orchestrator's per-issue cancel API (the `IssueCommander` seam), which maps an external cancel to a single `AgentRun`.

**`batch.sock` remains for batch-level operations.** The control socket at the batch root is used for attach/streaming of the entire batch output and for batch-level liveness checks (`IsRunActive` probes `batch.sock` only).

### `IsRunActive` semantics

`IsRunActive` probes `batch.sock` at the batch root — it checks whether the daemon is alive, not the individual run. Run-level liveness (whether a specific AgentRun is still running) is determined by the event log: a run is active if it has a `run.started` event without a terminal `run.finished`, `run.aborted`, or `run.cancelled` event.

This separation means:
- `IsRunActive` returning `true` means the daemon is alive (batch could have live runs)
- Run-level status is read from the event log, not from socket probes

### Command server interface

The `run.sock` accepts JSON command requests. The first supported command is:

```json
{"action": "abort", "issue": <issue-number>}
```

Each `CommandServer` (one per AgentRun, bound at `<batch>/runs/<runID>/run.sock`) dispatches this to the `IssueCommander` interface on the orchestrator, which cancels the context for that specific `AgentRun` without affecting siblings.

### Schema changes

`run.json` (ADR-0032) adds no socket-specific fields — socket paths are derived deterministically from the folder layout:

- `batch.sock` is at `<batch>/batch.sock`
- `run.sock` is at `<batch>/runs/<runID>/run.sock` (per-run folder)

The `Command Server` entry in `CONTEXT.md` is updated to reflect the per-run socket decision.

## Consequences

### Positive

- External abort tools address each run by path — stable, discoverable, no encoding of identity into payload.
- One command server per AgentRun dispatches aborts to the `IssueCommander` seam, cancelling a specific `AgentRun` without affecting siblings.
- Path-based addressing aligns with the filesystem layout: the socket lives inside the run folder alongside `run.json` and `run.log`.
- The `IssueCommander` seam is clean: one interface, one implementation per orchestrator, dispatch by issue number.

### Negative

- The daemon manages one socket listener per active run. File descriptor usage is O(active runs), not O(batches) — acceptable given Sandman's single-repo, single-operator workload.
- External tools must know both the batch path and run ID to connect. The run ID is derivable from the batch index and batch identifier; the full socket path is computed by `RunSocketPath()`.

### Neutral

- The batch-level `batch.sock` remains for attach/streaming. No change to the attach workflow.
- `IsRunActive` semantics are unchanged — it still probes `batch.sock` only.
- The per-run socket decision is orthogonal to the batch/run folder layout (ADR-0032) but enabled by it.
