# Monitoring and Debugging

## Status

```bash
sandman status
```

Displays currently active (in-progress) agent runs with elapsed time. Reads `.sandman/events.jsonl` and filters for runs that have started but not yet finished.

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

## Per-issue logs

Each agent run writes its output to `.sandman/logs/<issue>.log`. This file captures both stdout and stderr from the agent process, prefixed with issue-specific timestamps.

## Debug mode

```bash
sandman run --debug 42
```

When enabled, Sandman prints the worktree path and instructions for manual inspection alongside failure output. Worktrees are always preserved on failure (regardless of `--debug`), so you can examine the agent's partial output and diagnose issues.

## Preserve mode

```bash
sandman run --preserve 42
```

Keeps worktrees on disk even after successful runs. Useful when you want to review the agent's work before cleaning up manually with `sandman clean`.

## Graceful shutdown

When Sandman receives SIGTERM (e.g., Ctrl+C):

1. Running agents are notified (SIGTERM forwarded to agent process)
2. Sandman waits up to 10 seconds for agents to finish gracefully
3. If agents are still running after the timeout, Sandman sends SIGKILL
4. Partial results and events are preserved in the event log

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
