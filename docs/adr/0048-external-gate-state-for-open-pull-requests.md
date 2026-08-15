# ADR-0048: External-gate state for clean exits with open pull requests

## Status

rejected

## Context

An issue-driven agent can finish successfully after preparing a pull request
while CI or delegated review is still running. The old completion check treated
the not-yet-merged pull request as an agent failure and consumed retry budget.

This ADR proposed a terminal `external-gate` state as the remedy. Its
implementation made recoverable CI, review, and publication waits terminal
`blocked` results, retained sandbox capacity while polling, required manual
continuations, and allowed incomplete or stale local review artifacts to
override the live pull-request state. `blocked` consequently stopped
representing only dependency outcomes.

## Decision

Reject the terminal external-gate design. It must not be introduced or
preserved.

The replacement is a runtime-owned implementation pull-request lifecycle:

- A clean agent exit with an open pull request enters a non-terminal bounded
  await state without consuming agent retries or holding agent execution
  capacity.
- Live pull-request state and durable review-publication evidence determine the
  next lifecycle action. Incomplete or stale local request records cannot
  terminalize a run or override verified merged completion.
- Actionable feedback and verified merge readiness start a fresh lifecycle
  session automatically. A verified merged pull request with closing intent is
  the only normal success path.
- `blocked` is reserved for dependency outcomes. PR lifecycle waiting,
  resumption, and terminal policy outcomes have distinct event and portal
  projections.

This replacement is planned by the implementation pull-request lifecycle
initiative. A follow-up ADR will record its final event schema and lifecycle
boundary when implemented.

## Consequences

### Positive

- CI and review latency no longer consumes agent retry budget.
- Recoverable PR states no longer require an operator continuation.
- Sandboxes and agent capacity are released while the lifecycle awaits external
  progress.
- Dependency blocking regains one unambiguous meaning.

### Negative

- The terminal external-gate implementation, its projections, and duplicate
  arbitration paths must be removed.
- The runtime must own a testable await/resume lifecycle and its event schema.
