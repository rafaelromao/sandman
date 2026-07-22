# ADR-0022: Rename Ralph Loop to Auto Mode

## Status

accepted

## Context

The "Ralph Loop" agent-driven issue selection mode was named after an internal codename. The name caused confusion with the user-facing `--ralph` flag.

## Decision

Rename the Ralph Loop to **Auto Mode**. The `--ralph` flag is renamed to `--auto`. All internal references are updated. The behaviour is unchanged.

## Consequences

### Positive

- Mode name now describes the behaviour rather than implying a personal or system association.
- Consistent naming across CLI flags and user-facing documentation.

### Negative

- Existing scripts using `--ralph` must be updated to `--auto`.

### Neutral

- ~~ADR-0012 (deleted)~~ is superseded by this ADR for the naming change.
