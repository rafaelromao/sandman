# ADR-0043: Query the Dedicated `/dependencies/blocked_by` Endpoint for Native Blockers

## Status

accepted

## Context

Sandman's GitHub client (`internal/github/cli_client.go`) reads native issue
dependencies to compute the `BlockedBy` set that gates a dependent AgentRun.
ADR-0004 contemplated two sources: the `gh api repos/{o}/{r}/issues/{n}` payload
(which exposes `issue_dependencies_summary` counts but not the issue numbers)
and the events API (`/events`) as a fallback for `blocked_by_added` /
`cross-referenced` events.

The events path is event-time state, not current state. For an issue that is
*created* with all of its native blockers already attached, the
`blocked_by_added` events are not in the timeline at the moment the run starts
— they only appear later as the dependency surface is reconciled against the
issue view in the UI. The same gap shows up for any case where a project
relies on the dedicated dependency surface (the GitHub web UI's
"Blocked by" / "Add blocked by" picker) instead of the events stream.

Real repro (ThreeTerm #278, 2026-07-31, batch `260731094731-e131-278`):

- `GET /repos/rafaelromao/threeterm/issues/278` returned
  `blocked_by: null`, `issue_dependencies: null`,
  `issue_dependencies_summary: {blocked_by: 4, total_blocked_by: 5, ...}`.
- The `/events` timeline carried only `labeled` and `blocked_by_added`
  entries, all with timestamps after the batch started
  (`2026-07-31T12:24:09Z..12:24:12Z` vs batch start at `09:47:31Z`).
- The dedicated endpoint
  `GET /repos/rafaelromao/threeterm/issues/278/dependencies/blocked_by`
  returned the full current list `[232, 246, 127, 128, 129]`.

Without the dedicated endpoint the client returns an empty `BlockedBy` set and
the dependent runs, exactly as it did for ThreeTerm #278.

A second, latent defect was discovered while writing the regression test:
`issueEventPayload` tagged the inner object as `blocking_issue` (`json:"blocking_issue"`),
but the real GitHub `blocked_by_added` event uses the key `blocked_by`. The
parser therefore returned `nil` for every real GitHub event, and the bug
was codified by the existing
`TestCLIClient_FetchIssueDependencies_FallsBackToEvents` fixture.

## Decision

`CLIClient.fetchIssueDependencies` consults three sources, in order:

1. The issue payload's own `blocked_by` and `issue_dependencies.blocked_by`
   fields, when the public REST endpoint populates them (preserves
   ADR-0004's fast path).
2. The dedicated
   `GET /repos/{owner}/{repo}/issues/{number}/dependencies/blocked_by`
   endpoint, which returns the *current* set of native blockers. This is
   the authoritative source when the issue payload carries the dependency
   counts but not the numbers.
3. The events endpoint
   (`GET /repos/{owner}/{repo}/issues/{number}/events`) as a final
   fallback for older event-driven blockers that the dedicated endpoint
   does not yet surface. `parseDependencyEvents` reads the real GitHub
   shape (`event.blocked_by.number`); the legacy `blocking_issue` key is
   gone.

The body parser (`parseBlockedBy`) is unchanged. The heading-only contract
documented in `docs/usage/issue-body-formats.md` and the
`SpecificationResolver` carve-out (ADR-0042) both continue to hold.

## Consequences

### Positive

- Native blockers declared via the GitHub UI's dependency surface are
  recognised immediately, including at issue-creation time and when the
  events timeline is sparse.
- The events-fallback shape matches the public REST payload, so the
  parser no longer silently drops every real GitHub event.
- Body and event sources remain in place, so existing repos that rely on
  `## Blocked by` headings or on the events timeline continue to work.

### Negative

- One extra `gh api` call per FetchIssue (the dedicated endpoint). The
  per-call cost is bounded by the existing 30s default timeout and the
  same connection reuse used by other `gh api` calls.
- The dedicated endpoint may be rate-limited independently; the fallback
  to events (with its own retry) absorbs the failure mode.

### Neutral

- The public `github.Client.FetchIssueDependencies` and `FetchIssue`
  signatures are unchanged. The new behaviour is observable only through
  the returned `BlockedBy` set.
- The vocabulary in `CONTEXT.md` is unchanged: `BlockedBy` remains the
  union of body references and GitHub native dependency fields, sourced
  from the new endpoint as well as the previous sources.
