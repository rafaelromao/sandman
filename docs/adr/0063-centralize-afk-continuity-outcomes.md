# ADR-0063: Centralize AFK continuity outcomes

## Status

accepted

## Context

AFK continuity outcomes have been selected at several decision sites: lifecycle
failure classification, unchanged-resume stalls, exhausted relaunch allowances,
owned-work holds, and expired review requests. That duplication makes a local
change capable of silently altering a different gate's behavior.

The parent lifecycle work changes the policy in controlled steps. Work the
implementor cannot resolve should end the Run terminally after its bounded
attempts. External operations that are still resolving keep waiting: current-head
CI, an in-deadline or still-running review, the GitHub recovery window, provider
quota, and capacity. This refactor itself preserves every existing outcome;
the AFK continuity contract records the current table rows so each policy
change can flip only its assigned row.

## Decision

- One outcome table, keyed by gate, reason, and cause, decides whether a
  lifecycle exit waits for an external operation, enters a continuity hold, or
  ends terminally. Causes are resolving operation, failure reason, stall,
  exhausted relaunch allowance, expired operation, and unreadable stall ledger.
- Failure reasons are classified in that table. Unknown reasons remain
  terminal, and the contract test requires every emitted failure reason to be
  classified.
- Decision sites derive the cause and evidence, consult the table, then retain
  their existing payload-building and event-writing responsibilities. A table
  outcome is not re-inferred from a gate string.
- The current outcomes, including sibling-owned holds, are enumerated by
  `TestAFKContinuityContract_OutcomeTableRows` and exercised at their decision
  sites. Rows without a table-owning decision site remain pinned by the
  operation's existing lifecycle or event-fold scenario.
- The following sibling changes own the listed rows. Until each change lands,
  the contract retains the current outcome shown in the last column.

| Sibling | Gate / reason / cause | Current outcome | Revised outcome |
| --- | --- | --- | --- |
| #2995 | No continuity-row flip: descriptive merge prose no longer contradicts an otherwise approving daemon verdict | Existing approval classification | Keep approval eligible for merge |
| #2997 | `actionable-feedback` / `REVIEW_CHANGES_REQUESTED` / allowance used | Terminal after the configured round is exhausted | Keep terminal; scope review-feedback rounds to the head |
| #2998 | `actionable-feedback` / `REVIEW_CHANGES_REQUESTED` / allowance used for a contradicted daemon approval | Terminal after the configured round is exhausted | Treat the approval as requested changes and keep terminal after the bound |
| #2999 | `actionable-feedback` / `REVIEW_INFORMAL_FEEDBACK` / stall | Hold before #2999; terminal since it landed | Terminal (`REVIEW_FEEDBACK_STALLED`, classified permanent as a failure reason) |
| #3000 | `review-timeout` / `REVIEW_TIMEOUT` / expired operation with no active resolver | Hold | Terminal (`REVIEW_REQUEST_EXPIRED`) |
| #3001 | `ready-to-merge` / any merge reason / stall or allowance used | Hold | Terminal with the exhausted-work reason |
| #3001 | `ci-failure` / `CI_FAILURE` / stall or allowance used | Hold | Terminal with the exhausted-work reason |
| #3001 | `implementor-work-required` / `REVIEW_REQUEST_REQUIRED` / stall | Hold | Terminal with the exhausted-work reason |
| #3001 | `blind-review-required` / `BLIND_REVIEW_REQUIRED` / stall or allowance used | Hold | Terminal with the exhausted-work reason |
| #3001 | `actionable-feedback` / `REVIEW_INFORMAL_FEEDBACK` / allowance used | Hold (until #3001, the hold ends `REVIEW_FEEDBACK_STALLED` on its next observation; see below) | Terminal with the exhausted-work reason |

The #2999 row ends a Run whose informal-feedback relaunch left the head, the
review request and its responses unchanged. Its terminal evidence carries the
retained feedback (source, locator, body), the stalled resume signature
(`stalled_resume`) and the next action `sandman run --continue`. A Run an earlier
binary parked on that hold ends with the same reason on its next observation,
because its recorded stalled signatures prove the stall. The #3001 row for used-up
informal-feedback allowance keeps its hold, but that hold records its signature
like any stalled wait, and a recorded signature cannot be told apart from one an
earlier binary wrote. Until #3001 flips that row, the hold therefore ends with
`REVIEW_FEEDBACK_STALLED` on its next observation. Genuine requested changes
carried as informal feedback (a daemon `changes_requested` verdict, an approval
override) are keyed as `REVIEW_CHANGES_REQUESTED` for the stall decision, so they
keep their row and their per-head allowance.

These changes do not alter waits for resolving operations or the unrelated
current holds: in-deadline CI and review, active review execution, the
GitHub-recovery window before its bound, provider quota, capacity, expired CI
wait (`CI_WAIT_TIMEOUT`), and the idle gate when no review command is configured.
Formal requested changes left unrepaired remain terminal.

## Consequences

### Positive

- A lifecycle policy change has one explicit row and one contract update.
- Adding a failure reason without classifying it fails the contract; unknown
  reasons still fail closed as terminal.
- Tests distinguish an external operation that is resolving from a hold that
  has no operation deadline.

### Negative

- A table and its exhaustive contract require deliberate maintenance together
  when a sibling changes policy.
- The policy is staged across sibling changes, so temporary hold rows remain
  until their assigned changes land.

### Neutral

- This ADR's refactor introduces no behavior change, mutable lifecycle state,
  or new event type.
- Existing external-operation owners continue to decide their own deadlines;
  the table documents and tests those waits without duplicating their runtime
  scheduling decisions.
