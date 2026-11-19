# ADR-0060: Managed handoff boundary and 30-second observation

## Status

accepted

## Context

A managed implementation attempt hands a pull request to the runtime once the
agent has nothing left to do: Sandman then observes current-head CI and review,
releases the execution slot while it waits, and resumes the same RunID when the
evidence selects work.

A confirmed incident showed that the boundary between the two owners was
ambiguous. While the agent was still running required local verification
(a full suite and end-to-end tests), the pull request's request-scoped review
approval and its current-head CI both completed. The agent kept reporting that
CI was still running, long after it had finished, and the runtime resumed merge
work about 11 seconds after the attempt returned. The runtime's decision path
was correct and prompt: the delay was an unclear agent handoff and stale
reporting, not a slow poller or a misclassified approval.

Separately, ordinary managed observation waited 120, 60 and 60 seconds before it
settled at 30 seconds. The two-minute first interval is avoidable latency after
a genuine external handoff.

## Decision

**Ownership.** The implementor owns local verification and publication; the
runtime owns observation of external CI and review. A required local test or
static-analysis run (including a background command that is still running), an
unresolved test failure, an unpushed commit, and an incomplete publication,
review-request delivery or blind review round remain implementor-owned work.
Required verification is never skipped, interrupted or weakened to hand off
sooner. Early publication of durable progress stays allowed; when local work
remains after it, the agent says so rather than implying that the runtime is
observing a wait. Once owned work is complete the agent records its checkpoint
and returns immediately, without redundant verification or agent-side polling.
The same canonical `Sandman Managed Handoff Context` is composed idempotently into
fresh Tasks, continuations, retries and context recovery, and the implementation
capability carries the same contract. The review-request capability stays
stateless: it delivers and confirms one trigger and never observes.

**Truthful reporting.** Checkpoints and final summaries keep three states apart:
local verification in progress, external CI or review awaiting runtime
observation, and ready for merge. An earlier "CI is running" or "review is
pending" statement is never carried forward as present fact; the agent reports a
live verified observation or labels the statement as the last observation with
its timestamp. The runtime, not the summary, makes the post-attempt decision.

**Cadence.** Ordinary managed observation, in both the batch and the foreground
execution path, uses a 30-second interval from the first poll. The interval is
the time between observations: API-call duration and execution-capacity
admission delay are not part of it. The faster default applies to default and new
waits. A retained, valid explicit plan (a persisted review-request `poll_plan`,
or an explicit test override) keeps its meaning, and a registration created
before this decision remains valid. Provider-quota polling (ten minutes), hourly
GitHub recovery attempts and the outage gate, the standalone review cycle's own
plan and budgets, and the rollups that count as passing, failing or pending are
unchanged.

> **Later change (annotated 2026-11-19, #2989):** The ten-minute provider-quota
> cadence remains the implementation fallback and the reviewer daemon's fixed
> schedule. Implementation waits use a validated provider reset time when
> available, capped by the remaining five-hour completed-wait allowance; see
> [ADR-0062](0062-provider-reset-aware-quota-recovery.md).

**No new controller.** One runtime-owned lifecycle decision-maker remains. Events
stay the authority for lifecycle and terminality; no event schema, mutable
status or merge-on-green shortcut is introduced. A ready CI or review result
never preempts an executing attempt. The 30-minute CI bound of one identified
check execution and the configured review deadline are not renewed by polling; a
verified new head or same-head rerun may establish a new CI wait generation.
Polling does not reset retry, relaunch or stalled-resume accounting, and the
scheduler's fairness, start delay and quota rules are unchanged.

## Consequences

### Positive

- A genuine external handoff is observed within 30 seconds rather than up to two
  minutes, and the agent's final report distinguishes what it verified from what
  it last saw.
- Required local verification cannot be traded away for a faster handoff, while
  early publication keeps local and remote validation parallel.
- One canonical guidance section replaces scattered yield sentences, so retries
  and continuations cannot diverge from fresh Tasks.

### Negative

- Waiting rows make up to four times as many GitHub lookups during the first
  minutes of a wait. Every wait remains bounded by its existing deadline and by
  the GitHub-outage gate.
- Prompt and skill contract tests prove that the guidance is present and stable;
  they cannot prove that an agent follows it.
- Registrations that predate this decision keep their explicit plan until their
  wait ends, so their first interval is still longer than 30 seconds.
