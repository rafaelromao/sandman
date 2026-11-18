# ADR-0061: Close the work item after a merge into its configured base

## Status

rejected

Reverted because work item closure belongs to the team's delivery flow; the
replacement decision follows in this Specification.

## Context

ADR-0054 gates every dependent AgentRun on its blockers being `closed` on
GitHub, and the implementor relies on the pull request's closing reference
(`Closes #N`) for that closure. GitHub honours closing references only when the
pull request targets the repository's **default** branch. A Run whose configured
base is anything else (a `feat/<name>` feature branch selected with
`--base-branch` or `git.base_branch`) merges successfully and completes
(ADR-0049 accepts the body's closing reference), yet its work item stays open.
Every in-batch or external dependent of that item then finishes `run.blocked`,
so dependency chains cannot run on feature branches.

## Decision

Sandman closes the implementor's own work item after a verified merge into its
configured base branch, instead of depending on GitHub's default-branch
auto-close.

- **Primary closer: the `sandman-pr-merge` skill.** Right after verifying the
  merge (before deleting the remote branch) it reads the merged pull request's
  own base. When a configured base is known and differs, it does not close and
  records the mismatch; with no configured base, the pull request's base is the
  configured base. It closes only the implementor's own work item, never other
  referenced items, and only while it is still open, with the comment
  `Closed by sandman — merged in #<pr> into <base>.` It reads the state back,
  retries once, and otherwise records the blocker and the exact close command
  in the Task and run log. The merge result always stands. Because the skill is
  shared, the standalone `sandman-run` composition and managed runs behave the
  same.
- **Managed fallback in the runtime.** When a managed issue Run reaches terminal
  success, before its terminal event is written (and therefore before any
  in-batch dependent reads the blocker's state), the runtime closes the issue
  if the merged pull request whose observation decided success carries this
  issue's closing reference, its base equals the Run's configured base (an
  empty configured base accepts the pull request's base), and a fresh state
  read reports the issue open. It reuses that observed pull request instead of
  performing another lookup, uses the same comment wording, and skips after
  cancellation or explicit abort. A missing or mismatched base, or a failed
  close, is only written to the error log.
- "Closed" therefore means merged into the configured base branch, not
  necessarily into the default branch. The feature branch's own pull request
  into the default branch remains the release vehicle.

The ADR-0054 gate is unchanged: dependents still require `success` and
`closed`. This decision only makes the gate satisfiable on non-default bases.

## Consequences

### Positive

- Dependency chains work on feature branches, in managed and standalone flows.
- On the default branch the close is a no-op: GitHub already closed the item,
  so no duplicate comment is posted.
- The runtime fallback recovers a skill that did not close the item (for
  example an older skill installation) without another agent launch.

### Negative

- A work item closes when its change lands on the feature branch, possibly long
  before it reaches the default branch.
- Two writers can close the item; they share one comment wording and both skip
  an already closed item.

### Neutral

- No new event type, payload field, wait state or retry. A failed fallback
  close does not start a `github-unavailable` wait and leaves dependents on the
  existing ADR-0054 `run.blocked` path.
- The branch lookup now also requests the pull request's `baseRefName`.
- Existing already-resolved closes keep their `Closed by sandman — issue
  already completed.` wording.
