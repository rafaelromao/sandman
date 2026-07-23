# ADR-0022: Rename Ralph Loop to Auto Mode

## Status

superseded by [ADR-0039](0039-rollback-auto-mode.md). Auto Mode was rolled back before any release shipped the renamed flag; this ADR is preserved as the historical record of the rename direction.

## Context

The "Ralph Loop" agent-driven issue selection mode was named after an internal codename. The name caused confusion with the user-facing `--ralph` flag.

## Decision

The Ralph Loop was renamed to **Auto Mode** and the `--ralph` flag was renamed to `--auto`. All internal references were updated. The behaviour was unchanged at the time of the rename.

## Consequences

### Positive

- Mode name described the behaviour rather than implying a personal or system association.
- Consistent naming across CLI flags and user-facing documentation (until the rollback).

### Negative

- Existing scripts using `--ralph` had to be updated to `--auto`. (The same scripts were then forced to drop `--auto` again when [ADR-0039](0039-rollback-auto-mode.md) rolled the entire Auto Mode surface back before any release shipped.)

### Neutral

- ~~ADR-0012 (deleted)~~ was superseded by this ADR for the naming change. (Both ADRs are now historical record: [ADR-0039](0039-rollback-auto-mode.md) records the rollback that supersedes this rename.)
