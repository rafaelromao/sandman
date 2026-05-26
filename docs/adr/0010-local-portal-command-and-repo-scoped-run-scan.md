# ADR-0010: Local portal command and repo-scoped run scan

## Status

accepted

## Context

Sandman already exposes long-lived daemon behavior through `sandman run`, which creates per-run sockets under `.sandman/runs/`. Operators need a lightweight way to inspect active launch records from the current repository without attaching to each daemon individually.

The portal must stay repo-scoped so it only shows records for the checked-out project, and it must rescan on each poll so new `sandman run` processes appear without restarting the portal.

## Decision

Add a new `sandman portal` command that binds a local HTTP server to `0.0.0.0`, defaults to port `5000`, and discovers run sockets by scanning the current repository's `.sandman/runs/` tree on every request.

The portal serves a simple HTML view and JSON polling endpoints. Discovery only treats actual UNIX socket files named `run.sock` as live instances.

## Consequences

### Positive

- Operators get a stable local portal for current repo launch records.
- Late-starting daemons appear on the next poll without restarting the portal.
- Socket discovery stays aligned with repo-local Sandman state.

### Negative

- The portal adds another long-lived CLI command and a polling UI.
- Repo root detection becomes a required part of command startup.

### Neutral

- The portal does not manage launch records; it only observes them.
