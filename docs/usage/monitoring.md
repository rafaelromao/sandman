# Monitoring and Debugging

## Status

```bash
sandman status
```

Displays currently active live runs with `run-id`, issue list, and elapsed time. Reads `.sandman/runs/<run-id>/run.json` and uses live metadata as the source of truth.

## History

```bash
sandman history
```

Displays all completed agent runs with status, duration, and branch name. Useful for checking what happened in previous batches.

## Event log

Sandman writes structured events to `.sandman/events.jsonl` in newline-delimited JSON format. Each event has:

| Field | Description |
|-------|-------------|
| `type` | Event type (e.g. `run.started`, `run.finished`, `run.blocked`, `run.warning`) |
| `timestamp` | ISO 8601 timestamp |
| `run_id` | Unique run identifier |
| `issue` | GitHub issue number |
| `payload` | Event-specific data (status, branch, error message, etc.) |

Because `sandman retry` replays prompt inputs, `run.started` payloads may also include raw prompt-affecting values such as `prompt_source_type`, `prompt_source_value`, `prompt_args`, and `review_command`.

## Per-issue logs

Each agent run writes its output to `.sandman/logs/<issue>.log`. This file captures both stdout and stderr from the agent process, prefixed with issue-specific timestamps.

## Worktree hints

```bash
sandman run 42
```

Every completed run prints `worktree: <path>` on stdout. Worktrees stay on disk until you remove them with `sandman clean`.

## Graceful shutdown

When Sandman receives SIGINT or SIGTERM (e.g., Ctrl+C):

1. Running agents are notified (SIGTERM forwarded to agent process)
2. Sandman waits up to 10 seconds for agents to finish gracefully
3. If agents are still running after the timeout, Sandman sends SIGKILL
4. The control socket (`.sandman/runs/<run-id>/run.sock`) is closed — any connected `sandman attach` clients see EOF and exit
5. The live run directory (`.sandman/runs/<run-id>/`) is removed
6. Partial results and events are preserved in the event log

## Understanding run status

### Blocked runs

A run is marked as `blocked` when one or more of its `BlockedBy` issues failed in the same batch. Blocked runs do not execute — they are reported in the batch summary:

```
Summary: 3 succeeded, 0 failed, 1 blocked
  #42  success  sandman/42-fix-login
  #43  blocked
```

The event log records a `run.blocked` event for each blocked run, including which blockers caused it.

### Queued runs

When all container slots are full (container capacity reached and max containers limit hit), eligible `AgentRun`s wait in a queue. The event log records queue-related events. Runs are dispatched as capacity frees up within the same batch.

## Summary output

After a batch completes, Sandman prints a summary:

```
Summary: 2 succeeded, 1 failed
  #42  success  sandman/42-fix-login
  #43  failure  sandman/43-add-tests
  #44  success  sandman/44-update-docs
```
