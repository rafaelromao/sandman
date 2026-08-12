# ADR-0052: Request-scoped delegated review response classification

## Status

proposed

## Context

The implementor-side delegated review wait reads top-level pull-request comments,
formal reviews, and inline comments from one pull request. Treating those
surfaces as one unbounded response pool allows evidence from an older trigger,
another head, or a later trigger to affect the current request. Deadline
hardening also needs a stable evidence handoff so a response observed exactly at
the deadline is not lost.

The existing response surfaces and downstream informal-feedback rules are
backward-compatibility constraints. This change must structure their evidence,
not add free-form language inference or an author-based response filter.

## Decision

1. The versioned observer emits an additive `review-classification/v1` object
   alongside the existing raw snapshot and response counts.
2. Classification is keyed by repository, pull request, head SHA, configured
   trigger prefix, confirmed trigger identity and timestamp, and the absolute
   deadline. Only records strictly after the active trigger and before the next
   configured-prefix trigger belong to that request. Records at the deadline
   remain eligible; later records do not.
3. Top-level comments are excluded only when their body begins with the
   configured trigger prefix. Formal `COMMENTED`, `APPROVED`, and
   `CHANGES_REQUESTED` reviews and inline comments remain accepted response
   surfaces.
4. Structured source evidence carries a canonical timestamp, source ID, and
   head status. Current-head formal approvals are mechanical approval evidence;
   stale or unknown approvals remain audit evidence and cannot approve. Any
   active-window `CHANGES_REQUESTED` review remains blocking.
5. The wait validates the classification identity, boundary evidence, source
   arrays, counts, timestamps, and formal precedence before persisting or
   returning a response. Missing or inconsistent classification fails closed as
   unavailable. A later trigger is surfaced as a superseded response boundary,
   including when no response exists between the triggers.

## Consequences

### Positive

- Review evidence cannot approve a different confirmed request or stale head.
- The deadline handoff has structured source evidence and preserves exact-boundary
  records.
- Existing raw snapshots and response formats remain available for audit and
  downstream feedback handling.

### Negative

- Malformed or incomplete API records can make a wait unavailable rather than
  silently accepting partial evidence.
- Consumers must use request-scoped classification instead of pull-request-wide
  aggregate review state for approval decisions.

### Neutral

- Natural-language approval and concrete-feedback interpretation remain in the
  existing review skill step; this protocol does not infer new prose meanings.
