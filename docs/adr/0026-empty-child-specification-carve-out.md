# ADR-0026: Empty-child Specification carve-out

## Status

proposed

## Context

ADR-0025 §5 ("No-children rejection") established that a Specification with no accepted children (no candidates harvested, or every candidate dropped at `## Parent` verification) fails the resolution with `no child issues for specification #<n>`. This was the original design.

In practice, this creates a sharp edge: a user who types `sandman run #<spec>` expecting to implement the Specification as a single-row batch receives an opaque error instead. The most helpful behavior would be to run the Specification as a regular issue, logging the decision so the operator understands what happened.

## Decision

We replace ADR-0025 §5 with a carve-out:

A Specification whose accepted-child set is empty (no candidates, or every candidate dropped at `## Parent` verification) runs as a single-row batch. The resolver emits `running specification #N as a regular issue (no children)` on stderr and returns the Specification's number in the output batch instead of failing.

Hard errors (fetch failures, context cancellation) retain their existing error returns.

## Consequences

### Positive

- `sandman run #<spec>` with no children is no longer a hard error; users get the intended single-row batch.
- The log line makes the decision transparent to the operator.

### Negative

- A Specification that genuinely cannot resolve its children now silently runs instead of alerting the operator. This could mask a misconfigured Specification or a network failure in `collectCandidates`. Mitigated by the fact that hard errors (fetch failures, ctx cancellation) still surface.
- The broadened-detector path (`HasChildren` + `ListSubIssues`) is affected: a non-spec issue whose `ListSubIssues` candidates all fail `## Parent` verification also now passes through as a single-row batch. This is acceptable because the broadened-detector's own pass-through logic (lines 218-227 in spec.go) already handles the zero-candidates case; the all-filtered case is analogous.

### Neutral

- ADR-0025 §5 is superseded by this decision.
