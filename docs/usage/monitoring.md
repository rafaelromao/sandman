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

## Portal

```bash
sandman portal
```

`sandman portal` is the browser view for the same repo-local run data that powers `status` and `history`. It rescans the current repository on each poll, so live Sandman instances appear without restarting the portal.

Use it when you want one place to inspect active runs, completed runs, and recent logs across multiple instances in the same repo.

## Event log

Sandman writes structured events to `.sandman/events.jsonl` in newline-delimited JSON format. Each event has:

| Field | Description |
|-------|-------------|
| `type` | Event type (`run.started`, `run.continued`, `run.queued`, `run.blocked`, `run.idle_timeout`, `run.warning`, `run.finished`, `run.aborted`) |
| `timestamp` | ISO 8601 timestamp |
| `run_id` | Unique run identifier |
| `issue` | GitHub issue number, or `null` for prompt-only runs |
| `payload` | Event-specific data (see below) |

### Event payloads

#### `run.started` / `run.continued`
Emitted when an agent run begins. `run.continued` replays stored `agent`, `model`, and `review_command` from the original run context (no `previous_run_id` field).

#### `run.queued`
Emitted when an issue enters the wait queue due to unresolved blockers or parallel capacity constraints.

| Field | Description |
|-------|-------------|
| `blocked_by` | List of issue numbers blocking this run |

#### `run.blocked`
Emitted when one or more `BlockedBy` issues failed in the same batch.

| Field | Description |
|-------|-------------|
| `blocked_by` | List of issue numbers that caused the block |

#### `run.idle_timeout`
Emitted when the heartbeat watchdog detects that the agent has produced no log output for the configured `run_idle_timeout` duration. The watchdog then kills the agent process and the run terminates as `aborted`. This event is diagnostic; the terminal status is set by the subsequent `run.aborted` event.

| Field | Description |
|-------|-------------|
| `issue` | GitHub issue number |
| `idle_seconds` | How long the agent was idle before the watchdog fired |
| `idle_timeout_seconds` | The configured idle timeout threshold |
| `attempt` | Which retry attempt this was (1-indexed) |

#### `run.warning`
Emitted for non-fatal issues during sandbox cleanup.

| Field | Description |
|-------|-------------|
| `branch` | Branch name |
| `message` | Warning message |

#### `run.finished`
Emitted when an agent run completes.

| Field | Description |
|-------|-------------|
| `status` | Terminal status (`success`, `failure`) |
| `branch` | Branch name |
| `base_branch` | Base branch name |
| `worktree_state` | Always `preserved` |
| `retries_total` | Total retry attempts configured |
| `retries_done` | Actual retries performed |

#### `run.aborted`
Emitted when a run is aborted via context cancellation (e.g. SIGINT/SIGTERM). Also emitted for runs that were still queued (waiting on the turn gate or the start gate) when the batch was cancelled, and cascaded to dependents whose in-batch blocker finished with status `aborted` (instead of `run.blocked`). For queued/cascaded runs, the `RunID` matches the prior `run.queued` event so projection collapses to a single `RunState`.

Payload shape depends on the abort path:

- **Active run cancelled** (same as `run.finished`): `status`, `branch`, `base_branch`, `worktree_state`, `retries_total`, `retries_done` with `status: aborted`.
- **Queued/blocked run cancelled or cascaded**: minimal payload — `status: aborted`, plus optional `aborted_by` listing the upstream blocker(s) for the cascade case.

Legacy `run.cancelled` events in older `events.jsonl` files project to the same `aborted` status.

## Run logs

Each agent run writes its output to `.sandman/logs/<issue>.log` for issue-driven runs, or a branch-derived filename for prompt-only runs. The file captures both stdout and stderr from the agent process, prefixed with run-specific timestamps.

## Worktree hints

```bash
sandman run 42
```

Every completed run prints `worktree: <path>` on stdout. Worktrees stay on disk until you remove them with `sandman clean`.

Prompt-only runs print the same summary shape, but their issue column appears as `prompt-only` instead of `#<number>`.

## Graceful shutdown

When Sandman receives SIGINT or SIGTERM (e.g., Ctrl+C):

1. Running agents are notified (SIGTERM forwarded to agent process)
2. Sandman waits up to 10 seconds for agents to finish gracefully
3. If agents are still running after the timeout, Sandman sends SIGKILL
4. The control socket (`.sandman/runs/<run-id>/run.sock`) is closed — any connected `sandman attach` clients see EOF and exit
5. Partial results and events are preserved in the event log
6. `sandman run` (or `sandman continue`) prints `batch aborted by operator` to stderr, prints the final summary (with the aborted bucket), and exits with code 130 (the standard Unix code for SIGINT). A real run failure still prints the existing `run batch: ...` message and exits non-zero.

## Idle timeout

The heartbeat watchdog monitors agent log output. If no new output appears for `run_idle_timeout` seconds (default: 1800, i.e., 30 minutes), the watchdog aborts the run.

**What triggers it:**
- Agent blocked on an interactive stdin prompt with no output
- Agent in an infinite loop with no logging
- Agent deadlocked with no progress
- Any situation where the agent process is alive but not producing output

**What the user sees:**
1. A `run.idle_timeout` event is written to `.sandman/events.jsonl` with diagnostic payload (`idle_seconds`, `idle_timeout_seconds`, `attempt`)
2. The agent process is killed
3. The run is emitted as `run.aborted` with status `aborted`
4. The batch summary shows the run in the `aborted` bucket
5. If retries are configured and retries remain, the run is retried

**Disabling:**
Set `run_idle_timeout: 0` in `.sandman/config.yaml` or pass `--run-idle-timeout 0` to disable the watchdog. Use this when running agents that are legitimately silent for long periods (e.g., waiting for external webhooks).

## Understanding run status

### Blocked runs

A run is marked as `blocked` when one or more of its `BlockedBy` issues failed in the same batch with a non-aborted status. Blocked runs do not execute — they are reported in the batch summary:

```
Summary: 3 succeeded, 0 failed, 1 blocked
  #42  success  sandman/42-fix-login
  #43  blocked
```

The event log records a `run.blocked` event for each blocked run, including which blockers caused it. If a blocker finished with status `aborted` instead, the dependent is itself emitted as `run.aborted` (with `aborted_by` listing the upstream blocker) and counted in the aborted total rather than the blocked total.

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

Buckets with a zero count are omitted. A batch interrupted by SIGINT/SIGTERM prints the aborted runs in their own bucket and exits with code 130:

```
Summary: 1 succeeded, 1 aborted
  #42  success  sandman/42-fix-login
  #43  aborted  sandman/43-add-tests
```

Prompt-only runs show the same summary with `prompt-only` in the issue column.
