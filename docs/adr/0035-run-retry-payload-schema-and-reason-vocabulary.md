# ADR-0035: `run.retry` payload schema and closed `reason` vocabulary

## Status

accepted

## Context

The orchestrator writes one `run.retry` event at the top of every retry iteration (between two attempts that are both actually about to run). The current payload — `attempt`, `max_attempts`, `previous_status`, `last_log_lines`, `branch` — tells operators *what* is happening but not *why*. After three slices of work that surfaced `run.retry` to the portal (PRD #1498, slices 1–4), the `reason` field is the one piece of context that distinguishes `agent-stalled` from `agent-failed` from `sandbox-timeout`. Without it, every retry looks identical in the JSONL event log and the operator has to read the agent log to recover the cause.

The schema is fixed in `internal/batch/orchestrator.go::logRetry` and consumed by:

- **`internal/events/run_state.go::RetriesTotal` / `RetriesDone`** — read `Finished.Payload["retries_total"]` / `["retries_done"]` verbatim. The orchestrator pre-computes both at finish time as `result.RetriesTotal - 1` for `retries_done` and `req.Retries` for `retries_total`. There is no fold of raw `run.retry` events into the finished payload; the count is written once at `run.finished` time.
- **`events.RunState.LiveAttempt()` / `LastRetryReason()`** (slice 1, #1499) — the new state-level helpers that surface the live attempt count and the most recent retry reason for *active* runs. Slice 2's `Retries []Event` projection (slice 2, #1500) feeds the active-row walk.
- **`internal/cmd/portal.html::renderRunMeta`** and the client-side `SandmanPortalDiff` (slice 4, #1503) — the new "attempts N retries" chip on active rows, plus the reason tooltip / subtext.
- **Operator forensics** — anyone reading `.sandman/events.jsonl` to debug a run.

The vocabulary must be closed because (a) the portal chip and the slice-4 tooltip render the value verbatim, so any free-form string would risk layout blowup; (b) the slice-1 helper `LastRetryReason()` returns a stable string keyed off `run.retry.payload.reason`, so a closed set keeps downstream filters and tests honest; (c) ADR-0000 establishes that domain vocabulary changes belong in an ADR, and a vocabulary expansion is the kind of decision that needs a record.

## Decision

### Closed `reason` vocabulary

The `reason` field on `run.retry` payload is a closed string drawn from this initial set. The orchestrator's `logRetry` site picks exactly one value per emit, and the consumer code (portal chip, slice-1 helpers, ADR references) treats anything outside the set as a bug.

| Value | Meaning |
|-------|---------|
| `agent-stalled` | The agent process was alive but produced no log output for `run_idle_timeout` seconds; the heartbeat watchdog killed it. Maps to the upstream `run.idle_timeout` event's `reason: "run_idle_timeout"` but expressed in the retry-loop vocabulary. |
| `agent-failed` | The agent process exited with a non-zero status before the next iteration started; the orchestrator classified the previous iteration as `failure`. |
| `sandbox-timeout` | The sandbox wrapper (container or worktree) reported a timeout that is not the agent's fault — e.g. `docker exec` exceeded the run timeout. |
| `kill-timeout` | The operator or a parent process issued SIGKILL/SIGTERM and the orchestrator promoted the cancellation to a retry. Distinct from `sandbox-timeout` because the kill came from outside the run, not from the sandbox adapter. |
| `manual` | A retry that was triggered by an explicit operator action outside the orchestrator's normal supervisor path — e.g. a `sandman run --continue` against a run whose previous iteration ended in a terminal non-success state. |

This is the slice-3 starting set. New values are added only by amending this ADR with a one-line entry in the table above; silent additions are forbidden.

**Vocabulary → emit-site mapping** is the slice-3 implementation's job (per slice 3 issue #1501): there is currently exactly one `logRetry` emit site (`internal/batch/orchestrator.go:1783`) and slice 3 will lock which `result.Status` and trigger condition maps to each value above. This ADR records the contract; slice 3 is responsible for verifying that the chosen value at each emit site matches one of the rows.

### Schema (the six fields)

The `run.retry` payload is the union of six fields. All are documented in `docs/usage/monitoring.md` and the orchestrator's `logRetry` emit is the only writer.

| Field | Type | Description |
|-------|------|-------------|
| `attempt` | int (1-indexed) | The attempt number that is about to start. |
| `max_attempts` | int | Total attempts the run was budgeted for (`retries + 1`). |
| `previous_status` | string | `result.Status` from the previous iteration, verbatim (`failure` or `aborted` in practice). |
| `last_log_lines` | []string | Up to 3 trailing lines from the per-run agent log at retry time. |
| `branch` | string | Branch the run is operating on. |
| `reason` | string (closed vocabulary) | Why this retry was triggered — see table above. Slice 3 makes this field mandatory on every emit. |

Historical `run.retry` events emitted before slice 3 was merged carry `reason` absent or `null`. They remain on disk untouched (slice 3 must not force-mutate the JSONL log); consumers that read `reason` must tolerate the empty case for historical events.

### Projection rule — `attempts` for active vs. finished runs

A consumer that wants to display "attempts N" for a row follows this rule:

- **Finished run** — read `Finished.Payload["retries_done"]` (the orchestrator writes the count of retry iterations actually executed into the terminal `run.finished` event). This is what `events.RunState.RetriesDone()` already returns today.
- **Active run** — no `run.finished` exists yet. Walk the raw event list for the run and return the maximum `run.retry.payload.attempt` value. If no `run.retry` events exist, return `0` (the run has not retried yet). Slice 1's `LiveAttempt()` helper on `events.RunState` is the canonical implementation; slice 2's `Retries []Event` projection makes that walk cheap by avoiding a second pass over the events list.

The `RetriesTotal()` projection is unchanged: it always reads `Finished.Payload["retries_total"]`, returning `0` for active runs.

### Reciprocal link with `CONTEXT.md`

The glossary entry `Run retry` in `CONTEXT.md` is the user-facing entry point. It links here for the schema and vocabulary source of truth, and this ADR names the glossary as the place consumers should look first. The portal layout doc (`docs/agents/portal-layout.md`) names the slice-4 chip and its `reason` tooltip without restating the schema.

## Consequences

### Positive

- The portal's new active-row `attempts N retries` chip (slice 4, #1503) can render the chip without re-deriving the cause — the slice-1 helper hands back a string from the closed vocabulary.
- Slice-1's `LastRetryReason()` and the underlying `run.retry.payload.reason` key are stable contracts; tests can match the vocabulary without regex wrangling.
- Operator forensics on `.sandman/events.jsonl` gain a one-token discriminator for retry cause.
- The schema and the vocabulary land together in a single document, so a doc reader sees the closed set and the field semantics side-by-side.

### Negative

- Adding a new retry cause requires an ADR amendment, not just a code change. This is intentional but adds ceremony; the alternative (open string) was rejected because it would silently let the portal chip render arbitrary text.

### Neutral

- `internal/events/run_state.go::RetriesTotal()` / `RetriesDone()` continue to read from `Finished.Payload`; they are not affected by the active-run rule above.
- `docs/usage/monitoring.md`'s `run.retry` block will gain a `reason` row once slice 3 lands the orchestrator change. The doc update is a follow-up and not in scope for slice 5.
