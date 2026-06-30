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

Daemon-level state lives in `.sandman/reviews/`:

| File | Purpose |
|------|---------|
| `review.sock` | Daemon command socket at `.sandman/reviews/review.sock` |
| `review-prompt.md` | Shared prompt template (no PR data) at `.sandman/reviews/review-prompt.md` |

No per-PR subdirectories are created under `.sandman/reviews/`. Per-run state lives inside the batch run folder.

### Per-run `review-state.json`

For review runs, the run folder (`.sandman/batches/<batch-id>/runs/<run-id>/`) contains `review-state.json`:

```jsonc
{
  "pr": 1217,
  "seenComments": [
    { "commentID": 12345, "status": "success", "timestamp": "<RFC3339>" }
  ],
  "claims": {
    "<commentID>": { "holder": "<pid>", "since": "<RFC3339>" }
  }
}
```

`seenComments` records which comments have been processed along with their terminal status (`success`/`failure`/`superseded`/`aborted`). `claims` tracks which comment is currently being worked on by which process. Claims are stored inline — **no separate `claims/` directory** — so claim state is atomic with the rest of the review state.

### Review daemon workflow

1. Read `.sandman/batches.json` on each scan.
2. Iterate all open PRs from GitHub.
3. For each PR, find the latest `/sandman` comment.
4. If a `review-state.json` already records `(pr, commentID)` with a terminal status (`success`/`failure`/`aborted`), skip.
5. Otherwise, create a new review batch + run folder, index it in `batches.json`, process the comment.
6. Per-run state (seen comments, claim locks) lives inside the run folder. Daemon-shared state is only `review.sock` and `review-prompt.md`.

### `review-prompt.md` is shared

`review-prompt.md` is the same file for all PRs. It contains no PR-specific data — the daemon renders PR context at request time. One prompt template, shared across all review runs.

### Per-PR slot table

Replace the per-tick inner `sem := make(chan struct{}, parallel_reviews)` allocation in `tick` with a daemon-scoped slot table that survives across ticks. The slot table is built from two pieces: a shared buffered channel (`slotPool`) sized to `EffectiveReviewParallel()`, and a per-PR map (`map[prNumber]struct{}`) recording which PR holds a slot.

The slot is acquired by `processPR` before launching, after the newest unseen trigger is identified but before the eyes reactions are added. `acquirePRSlot` first checks the per-PR map — if the PR already has a held slot, the function returns `false` and `processPR` exits silently (no reactions, no launch, no `MarkSeen`); the trigger survives in `ListPRComments` and is processed on the next tick. If the per-PR map has no entry, it attempts a non-blocking send on `slotPool`; if the pool is saturated, `processPR` also exits silently. On either path no trigger is dropped.

Two design choices, recorded here:

1. **Where the slot is held across the tick boundary** — the slot is held by `processPR`'s deferred `releasePRSlot`, which runs after `MarkSeen` persists the trigger as terminal-seen. The slot reflects on-disk truth, not in-flight truth. This closes the "slot dropped because the tick is faster than `MarkSeen`" failure mode: the next tick cannot free the slot until the review has been fully terminal-seen.

2. **How a new trigger is expressed while a review is in flight** — the trigger is re-scanned from `ListPRComments` on the next tick. The trigger stays in the seen cache as non-terminal-seen (slice A's hydration is unaffected), so the next `processPR` for the same PR re-identifies it as unprocessed. This avoids a bounded queue on the slot channel and keeps the table semantics simple: a per-PR slot is binary (held or free), and triggers either get the slot now or wait for the next tick.

The slice-B claim lock (`busy=1` + `TryClaim` in `processPR`) is a precondition: it guarantees that two `processPR` calls for the same PR never enter `launchReview` simultaneously, which makes the per-PR slot bookkeeping single-owner (the in-flight `launchReview`) and trivially correct. The slot table is in-memory only — a daemon restart abandons the slot, and the trigger is re-discovered via `ListPRComments` on the next tick. Per-run `review-state.json` (already existing) is unaffected.

## Consequences

### Positive

- Daemon state is minimal: `review.sock` + `review-prompt.md` at `.sandman/reviews/`. No per-PR directories.
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
