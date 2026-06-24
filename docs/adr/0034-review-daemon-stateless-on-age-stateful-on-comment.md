# ADR-0034: Review daemon — stateless on age, stateful on comment

## Status

accepted

## Context

The original review daemon maintained per-PR state under `.sandman/reviews/<PR>/` (seen comments, claim locks, rendered prompt), regardless of whether a review run actually existed for that PR. This meant daemon-shared state outlived the runs that produced it, and per-run state was co-located with daemon-level state in ways that made cleanup and recovery ambiguous.

The layout redesign (ADR-0032) introduced per-run folders inside batches, which provided a natural home for per-run review state. The review daemon's daemon-level state could be reduced to only the socket and the shared prompt template, eliminating per-PR subdirectories entirely.

Parent: [#1218](https://github.com/rafaelromao/sandman/issues/1218) — Sandman `.sandman/` folder layout redesign, "ADRs to write" section, Phase 6.

Relevant user stories from #1218: 35–43.

## Decision

### Deduplication key: `(prNumber, commentID)`

The daemon deduplicates requests by `(prNumber, commentID)`. If a later `/sandman` comment appears on the same PR, it supersedes an earlier one. The dedup key is the comment ID itself, not a timestamp or sequence number.

This means:
- **Newer requests on the same PR supersede older ones** — the latest unprocessed `/sandman` comment is the one acted upon.
- **Older requests are valid as long as they have not been superseded** — there is no time-based filtering on comment age.

### No time-based filtering

The daemon processes `/sandman` requests older than its own start time. After a restart, backlog requests are not dropped — they are valid as long as they have not been superseded by a newer comment on the same PR.

This is the **stateless-on-age** property: the daemon does not track "I have already processed comments older than T." Instead, it tracks "I have processed this specific `(pr, commentID)` pair" via the per-run `review-state.json`.

### No orphan-PRs rule

The daemon scans **all open PRs** and acts on the latest unprocessed `/sandman` request per PR, even if the PR has no associated review run batch currently active. Orphan PRs — PRs with `/sandman` requests but no live daemon — are valid targets.

This is the **no-orphan-PRs rule**: any open PR with an unprocessed `/sandman` comment is a valid review target, regardless of daemon state.

### Daemon state location

Daemon-level state lives in `.sandman/`:

| File | Purpose |
|------|---------|
| `review.sock` | Daemon command socket at `.sandman/review.sock` |
| `review-prompt.md` | Shared prompt template (no PR data) at `.sandman/review-prompt.md` |

Per-PR state directories (`.sandman/reviews/<PR>/`) exist for tracking per-PR review state. The daemon uses `PRDir(prNumber)` to construct the per-PR path.

### Per-run `review-state.json`

For review runs, the run folder (`.sandman/batches/<batch-id>/runs/<run-id>/`) contains `review-state.json`:

```jsonc
{
  "pr": 1217,
  "seenComments": [
    { "commentID": 12345, "timestamp": "<RFC3339>" }
  ],
  "claims": {
    "<commentID>": { "holder": "<pid>", "since": "<RFC3339>" }
  }
}
```

`seenComments` records which comments have been processed. `claims` tracks which comment is currently being worked on by which process. Claims are stored inline — **no separate `claims/` directory** — so claim state is atomic with the rest of the review state.

### Review daemon workflow

1. Read `.sandman/batches.json` on each scan.
2. Iterate all open PRs from GitHub.
3. For each PR, find the latest `/sandman` comment.
4. If a `review-state.json` already records `(pr, commentID)` with a terminal status (`success`/`failure`/`aborted`), skip.
5. Otherwise, create a new review batch + run folder, index it in `batches.json`, process the comment.
6. Per-run state (seen comments, claim locks) lives inside the run folder. Daemon-shared state is only `review.sock` and `review-prompt.md`.

### `review-prompt.md` is shared

`review-prompt.md` is the same file for all PRs. It contains no PR-specific data — the daemon renders PR context at request time. One prompt template, shared across all review runs.

## Consequences

### Positive

- Daemon state is minimal: `review.sock` + `review-prompt.md` at `.sandman/` root. Per-PR state directories (`PRDir`) exist but are managed by the daemon, not a separate cleanup process.
- Dedup key `(pr, commentID)` is stable and unambiguous — no timestamp drift or clock sync issues.
- Per-run review state is co-located with the run that produced it — cleanup follows the batch/run lifecycle (ADR-0032).
- No orphan PRs: all open PRs with unprocessed `/sandman` comments are scanned, regardless of daemon uptime.
- Claims are atomic with the rest of `review-state.json` — no separate directory to manage.

### Negative

- The daemon must scan all open PRs on every cycle — no per-PR caching of "nothing new here." Acceptable given GitHub API pagination and the single-repo, single-operator workload.
- If `review-state.json` is deleted mid-run, the daemon re-processes the same comment — no external idempotency guard. Recoverable but potentially wasteful.

### Neutral

- `review.sock` and `review-prompt.md` location is shared between this ADR and ADR-0032.
- `review-state.json` schema is defined in ADR-0032 alongside other run-folder artifacts.
- The `Reviewing` status entry in `CONTEXT.md` is unaffected by this ADR.
