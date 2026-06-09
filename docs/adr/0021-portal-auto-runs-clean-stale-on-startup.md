# ADR-0021: Portal auto-runs `clean --stale` on startup

## Status

accepted

## Context

ADR-0010 introduced `sandman portal` as a repo-scoped local HTTP dashboard that rescans `.sandman/runs/` on each browser poll. The portal observes run state passively; it does not modify anything on disk.

Operators frequently see a `Portal` row that shows an issue as `active` or `blocked` even though the underlying Daemon Process died (host reboot, OOM kill, manual `kill`). Those rows project from the JSONL event log, where the most recent event for the issue is a non-terminal `run.started` or `run.blocked`. The `sandman clean --stale` command (PR #690, `daemon.RecoverStaleRuns`) already exists to repair this state by emitting a `run.aborted` event for each issue whose RunState has not reached a terminal event in a dead batch.

The portal is the first surface operators open after a crash. Having to remember to also run `sandman clean --stale` to get accurate state defeats the purpose of a "live" dashboard. The first browser poll after portal startup should already reflect corrected statuses.

## Decision

`newPortalHandler` spawns a single fire-and-forget goroutine that calls the same body as `sandman clean --stale` once per server lifetime. The goroutine is the only place in the portal process that mutates the events log; the request handlers stay read-only.

Concretely:

- A package-level `portalStaleCleaner func(string) error` variable (mirrors the existing `portalRunAborter` / `portalPeerPID` / `portalSignalProcess` seams in `internal/cmd/portal.go`) wraps the body of the `--stale` flag. The default closure reads the events log under `repoRoot` and calls the new `runCleanStale(eventsList, log)` helper, which in turn calls `daemon.RecoverStaleRuns`. Tests swap the closure for a stub; production never needs to.
- The goroutine is spawned as the last step of `newPortalHandler`, after all `mux.HandleFunc` registrations and before `return mux`. `runPortalServer` calls `newPortalHandler` once, so the cleaner is invoked exactly once per server lifetime — never on a poll, never on a `clean` HTTP request.
- The body is shared with the CLI flag. The `--stale` branch in `internal/cmd/clean.go` is rewritten to call `runCleanStale`, so there is exactly one code path that calls `daemon.RecoverStaleRuns`. Duplication across the CLI and the portal is not introduced.
- Errors are logged via `log.Printf` and never block portal startup or serving. A panic in the helper is recovered inside the goroutine so a malformed events log cannot tear down the HTTP server.
- The default closure logs the recovered count on success (matching the CLI's stdout summary line) so operators can tell that the sweep ran.

The `baseDir` for the recovery stays hardcoded as `".sandman"` in the helper, matching the CLI flag's existing assumption. The portal passes `repoRoot` to the events log path (which is resolved up front by `runPortalServer`) so the goroutine reads the right file even when the user launched `sandman portal` from a subdirectory of the worktree.

## Consequences

### Positive

- Operators get accurate state on the first browser poll after portal startup without an extra CLI invocation.
- The cleaner body is shared with the `--stale` flag through `runCleanStale`, so the two surfaces cannot drift.
- A panicking cleaner cannot crash the server. The HTTP server keeps serving even when the events log is corrupted or unreadable.
- The seam (`portalStaleCleaner`) is the only behaviour swap, so the test surface is small and race-free under `-race`.

### Negative

- The portal mutates the events log on startup. The handlers remain read-only, but operators who treat `.sandman/events.jsonl` as a passive observation target will see a `run.aborted` event appear the moment the portal boots. This is the same write the CLI performs, but it now happens implicitly.
- The goroutine has no cancellation context. On `Ctrl+C` the cleaner may continue writing to the events log for a few extra milliseconds. The clean body is short (one `Read` + one `FindDeadRunBatches` + a handful of `Log` calls), so the window is small; the recovered events are still correct.
- `runCleanStale` discards the recovered-count return value to the seam. The default closure logs the count, but any test stub that does not inspect the count will silently miss the summary. Tests that need to assert the count use the seam and read it directly (none do today).

### Neutral

- The default closure constructs its own `events.JSONLLogger` from the `repoRoot` path. The portal handlers do the same per request, so no new shared state is introduced.
- The seam is a `func(string) error`, not a struct field. Future work that wants to inject a stub from a `Dependencies` object can wrap this seam; today the portal does not take a `Dependencies`.
