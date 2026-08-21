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
| `type` | Event type (`run.started`, `run.continued`, `run.queued`, `run.blocked`, `run.retry`, `run.idle_timeout`, `run.warning`, `run.await`, `run.resumed`, `run.finished`, `run.aborted`) |
| `timestamp` | ISO 8601 timestamp |
| `run_id` | Per-row RunID. For review runs the shape is `<ts>-<sid>-<linkedIssue?>-PR<pr>`. This is the row-level identifier; the batch-level identifier (public BatchId) is the `batch_id` field on the `run.started` / `run.finished` payloads, not this row. |
| `batch_id` | Public BatchId (batch-level identifier). Always present on `run.started` and `run.continued` payloads; mirrors the batch folder basename. |
| `issue` | GitHub issue number, or `null` for prompt-only runs |
| `payload` | Event-specific data (see below) |

The `run_id` (per-row RunID) and the `payload.batch_id` (public BatchId) identify different things. For multi-issue batches the two diverge — the public BatchId carries the `+N` additional count suffix and the per-row RunID does not. For every other kind (single-issue, prompt-only, review) the two are identical.

## Canonical identifiers

`batch_id` (public BatchId) and `run_id` (per-row RunID) are the canonical identifiers throughout the event log.

### Event payloads

#### `run.started` / `run.continued`
Emitted when an agent run begins. `run.continued` carries the same fields as `run.started` (branch, base_branch, parallel, start_delay, review_timeout, retries, sandbox, container_capacity, container_capacity_set, max_containers, max_containers_set, agent, model, review_command) plus `previous_run_id`. The full payload shape is identical to `run.started`; `previous_run_id` links the continuation to the original run.

| Field | Description |
|-------|-------------|
| `batch_id` | Public BatchId. Equals the batch folder basename and is the batch-level identifier (not the per-row `run_id` above). |
| `run_kind` | Optional taxonomy tag. `"review"` is signalled via the boolean `review` field. Issue-driven and prompt-only runs leave it absent. |
| `review_timeout` | Effective delegated review response budget in integer seconds for this AgentRun. |

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

#### `run.retry`
Emitted at the top of each retry iteration in the orchestrator's `for attempt` loop, between two attempts that are both actually about to run. The first attempt and the final attempt do not emit `run.retry`; the terminal `run.finished` (or `run.aborted`) covers those cases. Symmetric across the issue-driven and prompt-only retry loops; prompt-only runs use `issue: null` to match the existing prompt-only convention.

| Field | Description |
|-------|-------------|
| `attempt` | 1-indexed; matches the heartbeat's attempt indexing |
| `max_attempts` | Total attempts the run was budgeted for (`retries + 1`) |
| `previous_status` | `result.Status` from the previous iteration, verbatim (`failure` or `aborted` in practice; the spec's `idle_timeout` value is unreachable today because `withHeartbeat` flips non-success to `aborted` before the next attempt's `run.retry` fires) |
| `branch` | Branch the run is operating on |
| `last_log_lines` | `["line 1", "line 2", "line 3"]` — Up to 3 trailing lines from the agent log at retry time |
| `reason` | Closed retry cause: `agent-stalled`, `agent-failed`, `sandbox-timeout`, `kill-timeout`, `manual`, or `context-exhausted` |

#### `run.idle_timeout`
Emitted when the heartbeat watchdog detects that the agent has produced no log output for the configured `run_idle_timeout` duration. The watchdog then kills the agent process and the run terminates as `aborted`. This event is diagnostic; the terminal status is set by the subsequent `run.aborted` event.

| Field | Description |
|-------|-------------|
| `issue` | GitHub issue number |
| `idle_seconds` | How long the agent was idle before the watchdog fired |
| `idle_timeout_seconds` | The configured idle timeout threshold |
| `attempt` | Which retry attempt this was (1-indexed) |
| `reason` | `"run_idle_timeout"` — Constant string identifying the trigger |
| `last_log_lines` | `["line 1", "line 2", "line 3"]` — Up to 3 trailing lines from the agent log at timeout |

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
| `status` | Terminal status (`success`, `failure`, or `blocked`; `blocked` is reserved for dependency outcomes) |
| `branch` | Branch name |
| `base_branch` | Base branch name |
| `worktree_state` | Always `preserved` |
| `retries_total` | Total retry attempts configured |
| `retries_done` | Actual retries performed |
| `context_exhausted` | Present as `true` when the final attempt exhausted the OpenCode context and no clean retry remained |
| `run_kind` | Mirrors the `run.started` payload so projection sees a consistent kind on both events. |
| `reason` | Short string built from the error returned by the selection phase. |
| `blocker` | Optional dependency blocker classification and upstream issue list. |
| `completion` | Completion diagnostic used when a merged pull request needs closing-reference repair. |
| `review_request` | Present for retained delegated-review outcomes; retains the confirmed request identity, current head, deadline, budget, elapsed time, response counters, validated request-scoped classification, outcome, and next action. |

#### `run.await`
Emitted when an issue-driven run ends its agent session while recoverable pull-request work remains (CI, review, mergeability, or decision publication). Non-terminal: the run does not finish or consume a retry; foreground observation retains the active agent and sandbox capacity until lifecycle completion or cancellation.

| Field | Description |
|-------|-------------|
| `await` | Always `true` |
| `await_reason` | Lifecycle reason such as `"pending"`, `"failed"`, `"review-timeout"`, `"ready-to-merge"`, or `"actionable-feedback"` |
| `gate` | Lifecycle state at await time |
| `branch` | Branch name |
| `base_branch` | Base branch name |
| `retries_total` | Total retry attempts configured |
| `review_request` | Present for retained delegated-review outcomes; retains the confirmed request identity, current head, deadline, validated request-scoped classification, outcome, and next action |

#### `run.resumed`
Emitted when the runtime relaunches the agent session in-session with request-scoped review evidence — actionable review feedback (`reason: "feedback"`) or a current-head approval with green CI and clean mergeability (`reason: "approval"`). Non-terminal: the run stays active, keeps its RunID, and does not consume a retry.

Entry re-evaluation (session start) relaunches with the same evidence but is recorded as `run.continued` by the existing continuation flow — `run.resumed` marks only in-session relaunches.

| Field | Description |
|-------|-------------|
| `reason` | Resume cause: `"feedback"` or `"approval"` |
| `gate` | External-gate state that triggered the resume: `"actionable-feedback"` or `"ready-to-merge"` |
| `branch` | Branch name |
| `base_branch` | Base branch name |
| `retries_total` | Total retry attempts configured |
| `run_id` | RunID continuity marker (equals the event's own `run_id`; the resume keeps the RunID) |
| `review_request` | Present for retained delegated-review outcomes; request-scoped review evidence attached to the resumed session |

#### Implementation pull-request lifecycle
At a clean agent exit, the runtime consults the pull request once and applies one
lifecycle decision. Verified merged completion wins over retained review
evidence: a closing reference produces `success`, while an unverifiable or
missing closing reference produces `failure` with completion diagnostics.

Recoverable open-pull-request states produce `run.await` without consuming an
agent retry while foreground observation retains sandbox capacity. A continuation or in-session relaunch
re-evaluates the same facts. Retained review records and daemon decisions are
evidence only: they can supply the await reason and request-scoped prompt
evidence, but cannot terminalize a run or override merged completion.

Closed pull requests without a merge are terminal `failure`. Terminal
`blocked` remains exclusively the dependency outcome emitted by `run.blocked`.

#### `run.aborted`
Emitted when a run is aborted via context cancellation (e.g. SIGINT/SIGTERM). Also emitted for runs that were still queued (waiting on the turn gate or the start gate) when the batch was cancelled, and cascaded to dependents whose in-batch blocker finished with status `aborted` (instead of `run.blocked`). For queued/cascaded runs, the `RunID` matches the prior `run.queued` event so projection collapses to a single `RunState`.

Payload shape depends on the abort path:

- **Active run cancelled** (same as `run.finished`): `status`, `branch`, `base_branch`, `worktree_state`, `retries_total`, `retries_done` with `status: aborted`.
- **Queued/blocked run cancelled or cascaded**: minimal payload — `status: aborted`, plus optional `aborted_by` listing the upstream blocker(s) for the cascade case.

## Run logs

Each agent run writes its output to the run's log file inside the batch directory. The file captures both stdout and stderr from the agent process, prefixed with run-specific timestamps.

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
4. The control socket is closed — any connected `sandman attach` clients see EOF and exit
5. Partial results and events are preserved in the event log
6. `sandman run` (or `sandman run --continue`) prints `batch aborted by operator` to stderr, prints the final summary (with the aborted bucket), and exits with code 130 (the standard Unix code for SIGINT). A real run failure still prints the existing `run batch: ...` message and exits non-zero.

## Idle timeout

The heartbeat watchdog monitors agent log output. If no new output appears for `run_idle_timeout` seconds (default: 3600, i.e., 60 minutes), the watchdog aborts the run.

**What triggers it:**
- Agent blocked on an interactive stdin prompt with no output
- Agent in an infinite loop with no logging
- Agent deadlocked with no progress
- Any situation where the agent process is alive but not producing output

**What the user sees:**
1. A `run.idle_timeout` event is written to `.sandman/events.jsonl` with diagnostic payload (`idle_seconds`, `idle_timeout_seconds`, `attempt`, `reason`, `last_log_lines`)
2. The agent process is killed
3. The run is emitted as `run.aborted` with status `aborted`
4. The batch summary shows the run in the `aborted` bucket
5. If retries are configured and retries remain, the run is retried

**Disabling:**
Set `run_idle_timeout: 0` in `.sandman/config.yaml` or pass `--run-idle-timeout 0` to disable the watchdog. Use this when running agents that are legitimately silent for long periods (e.g., waiting for external webhooks).

## Understanding run status

### Blocked runs

- A dependency-blocked run has one or more `BlockedBy` issues that failed in the same batch with a non-aborted status. It does not execute and emits `run.blocked`, including the upstream blockers.

Only dependency-blocked runs appear in the blocked bucket of the batch summary:

```
Summary: 3 succeeded, 0 failed, 1 blocked
  #42  success  42-fix-login
  #43  blocked
```

If a dependency blocker finished with status `aborted`, the dependent is itself emitted as `run.aborted` (with `aborted_by` listing the upstream blocker) and counted in the aborted total rather than the blocked total.

### Queued runs

When all container slots are full (container capacity reached and max containers limit hit), eligible `AgentRun`s wait in a queue. The event log records queue-related events. Runs are dispatched as capacity frees up within the same batch.

## Summary output

After a batch completes, Sandman prints a summary:

```
Summary: 2 succeeded, 1 failed
  #42  success  42-fix-login
  #43  failure  43-add-tests
  #44  success  44-update-docs
```

Buckets with a zero count are omitted. A batch interrupted by SIGINT/SIGTERM prints the aborted runs in their own bucket and exits with code 130:

```
Summary: 1 succeeded, 1 aborted
  #42  success  42-fix-login
  #43  aborted  43-add-tests
```

Prompt-only runs show the same summary with `prompt-only` in the issue column.

## Archive

`sandman archive` ships four subcommands and every one of them is per-row aware.

| Subcommand | Behaviour |
|------------|-----------|
| `sandman archive run <runId>` | Move `runs/<runId>/` from `.sandman/batches/<batchId>/` to `.sandman/archive/<batchId>/runs/<runId>/`. The targeted row's `run.json.Status` must be terminal; sibling rows and the batch daemon stay untouched. Persists a per-row `Runs[]` record carrying `status: "archived"` and `archivePath` for crash recovery. |
| `sandman archive batch <batchId>` | Move the whole batch dir from `.sandman/batches/<batchId>/` to `.sandman/archive/<batchId>/`. The batch daemon must be gone. Flips the batch-level `status` to `archived`. CLI-only — not exposed via HTTP. |
| `sandman archive older-than <days>` | Walk every `runs/<runID>/run.json` across all batches and archive each terminal row older than the cutoff. Already-archived rows are skipped via the per-row `Runs[]` record. |
| `sandman archive stale` | Run the same stale-recovery pass as `clean --stale` (emit `run.aborted` for unterminated runs in dead batches), then walk every `runs/<runID>/run.json` and archive each terminal row. |

Per-row archive does not edit `events.jsonl` and does not touch `.sandman/worktrees/`. The HTTP `POST /api/runs/archive` endpoint shares the `archive run` contract: per-row, empty `200` on success, structured `409` (with `archivePath`) on collision or non-terminal, `404` when the row id is unknown.

Bulk commands process each row individually; a batch with multiple terminal rows archives them one at a time and leaves any still-active row live. Whole-batch archive is the only path that moves the batch root.

## Upgrades

Sandman does not migrate on-disk state across version upgrades. Clear `.sandman/` and re-run `sandman init` after upgrading. See [Troubleshooting](../help/troubleshooting.md#portal-shows-unknown-rows-after-upgrading-sandman) if status or portal rows look inconsistent after an upgrade.
