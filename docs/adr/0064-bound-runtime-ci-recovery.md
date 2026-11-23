# ADR-0064: Bound runtime CI recovery

## Status

accepted

## Context

[ADR-0060](0060-managed-handoff-boundary-and-thirty-second-observation.md)
assigns CI diagnosis and repair to the implementor and external observation to
the runtime. An implementor can diagnose a transient failure and confirm one
targeted rerun, yet still exhaust repair while the provider remains unavailable.
Ending immediately loses a recovery opportunity; rerunning every failed check
would hide deterministic failures and risk duplicate mutations.

## Decision

- Extend the managed handoff narrowly: stalled or exhausted CI repair enters
  runtime recovery only with a confirmed transient rerun receipt matching the
  current repository, issue/branch, PR, head, workflow and failed job. All other
  failures keep their existing outcomes.
- Reuse the bounded recovery window with hourly attempts and a 24-hour deadline.
  Skip attempts during a reported Actions outage; an unreachable status page
  is not an outage. At the deadline allow only one read-only PR/check observation
  before `CI_RECOVERY_EXHAUSTED`.
- Revalidate the PR and latest failed job, record intent before sending the
  targeted rerun, and confirm the replacement attempt contains the job and
  quality gate. Reconcile ambiguous intent without blindly replaying the request.
- Restart preserves the bound and next attempt. Manual continuation starts a
  fresh RunID and can attempt immediately only with matching confirmed evidence
  from its continuation lineage. Abort ends recovery.

## Consequences

Managed Runs can outlast a transient provider incident without occupying an
agent slot or suppressing repair of a different failure. Recovery depends on
readable event history and current workflow identity; uncertainty may leave a
Run waiting without another mutation. Standalone and ordinary CI observation
keep their existing cadence and bounds.
