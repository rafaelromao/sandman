# ADR-0048: External-gate state for clean exits with open pull requests

## Status

proposed

## Context

An issue-driven agent can finish successfully after preparing a pull request
while CI or delegated review is still running. The old completion check treated
the not-yet-merged pull request as an agent failure, emitted `run.retry`, and
relaunched the agent against the same work. That consumed retry budget for an
external condition and made the portal report an agent failure.

The external gate is asynchronous, so it needs a bounded polling policy and a
durable terminal record when the gate does not resolve during the current run.

## Decision

When an agent exits successfully and its branch has an open pull request,
Sandman classifies the pull request gate before applying the ordinary failure
fallback:

- Pending CI or review is polled with exponential backoff. Production polling
  starts at 120 seconds, caps an individual wait at 600 seconds, and uses an
  1800-second wall-clock budget. The poll context cancels GitHub calls at the
  same deadline.
- A merged pull request is accepted only when it retains closing intent for the
  issue. A merged pull request without that intent remains actionable rather
  than becoming an unverified success.
- Failed CI, rejected review, an unresolved gate, or an unavailable gate ends
  the run as `run.finished` with payload `status: "blocked"`,
  `blocker: "external-gate"`, and a `gate` reason. This is distinct from the
  dependency `run.blocked` event through the payload blocker and the portal
  message.
- An unanswered, current-head delegated-review request that reaches its own
  absolute deadline is handed to the same gate with `gate: "review-timeout"`
  and `reason: "REVIEW_TIMEOUT"`. The terminal payload retains the confirmed
  request identity, deadline, budget, elapsed time, response counters, and
  next action. A malformed or stale request is blocked as a state error rather
  than becoming an agent failure or retry.
- Gate waiting never emits `run.retry` and never increments the agent retry
  count. Actual agent failure, idle timeout, and cancellation retain their
  existing retry behavior.
- The blocker and next executable action are appended to the per-run log and
  atomically persisted under `## External Gate` in the worktree task document.
  Fresh and continuation runs use the same gate handling.

## Consequences

### Positive

- CI and review latency no longer consumes agent retry budget.
- Operators can distinguish an external gate from agent failure in events,
  portal status, task state, and the run log.
- A later continuation has a durable action while still rechecking live PR
  state before acting.

### Negative

- A run may remain active while it waits for the bounded external-gate poll.
- A gate that exceeds its budget requires a later continuation or intervention.
- GitHub polling adds bounded external API activity after the agent exits.
