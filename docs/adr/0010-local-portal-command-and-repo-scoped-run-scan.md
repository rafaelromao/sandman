# ADR-0010: Local portal command and repo-scoped run scan

## Status

superseded by ADR-0023

## Context

Sandman already exposes long-lived daemon behavior through `sandman run`, which creates per-run sockets under `.sandman/runs/`. Operators need a lightweight way to inspect active runs from the current repository without attaching to each daemon individually.

The portal must stay repo-scoped so it only shows runs for the checked-out project, and it must rescan on each poll so new `sandman run` processes appear without restarting the portal.

## Decision

Add a new `sandman portal` command that binds a local HTTP server to `127.0.0.1`, defaults to port `5000`, discovers run sockets by scanning the current repository's `.sandman/runs/` tree on every request, and exposes a `/api/commands` preset launcher surface for repo-scoped Sandman commands. The bind interface is configurable via the `--host` flag or the `SANDMAN_PORTAL_HOST` env var, with `0.0.0.0` as the documented opt-in for exposing the portal on all interfaces.

The portal serves a simple HTML view and a JSON polling endpoint. Discovery only treats actual UNIX socket files named `run.sock` as live instances.

## Consequences

### Positive

- Operators get a stable localhost dashboard for current repo runs.
- Late-starting daemons appear on the next poll without restarting the portal.
- Socket discovery stays aligned with repo-local Sandman state.
- The default loopback bind keeps `sandman portal` from accidentally exposing a dev server on every interface.

### Negative

- The portal adds another long-lived CLI command and a polling UI.
- Repo root detection becomes a required part of command startup.

### Neutral

- The portal observes runs and can launch repo-scoped Sandman commands from typed presets.
- The default implementation stays local-only; the operator can opt in to a wider bind via `--host` / `SANDMAN_PORTAL_HOST`.
