# ADR-0025: Portal secondary-row chips for run context

## Status

accepted

## Context

The portal's run list view shows run state via colour-coded badges, but there is no way to see per-run context without expanding the row. A user reviewing a batch of runs needs this context at a glance.

## Decision

Add a secondary row below each run row in the portal, surfaced as chips showing: attempt count, retry reason (when non-empty), and model name.

## Consequences

### Positive

- Run context is visible without expanding each row.

### Negative

- More DOM nodes per run.

### Neutral

- No changes to the underlying run state model.
