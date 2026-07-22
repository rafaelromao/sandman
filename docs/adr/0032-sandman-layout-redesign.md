# ADR-0032: `.sandman/` layout redesign — batches, runs, and the master index

## Status

accepted

## Context

The original `.sandman/` layout used a flat directory structure. As the system scaled, this created ambiguity between active and archived state.

## Decision

Redesign the `.sandman/` layout with an explicit hierarchy:
- `.sandman/batches/<batchID>/runs/<runID>/` — per-run artifacts
- `.sandman/archive/<batchID>/` — archived batches
- `batches.json` as the canonical master index.

## Consequences

### Positive

- Clear separation between active and archived state.
- Deterministic artifact paths.

### Negative

- Migration path required.

### Neutral

- Portal and CLI both read from `batches.json`.
