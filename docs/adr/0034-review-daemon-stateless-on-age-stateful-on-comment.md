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

### Tick coordination

For each PR scanned by `tick` / `processPR`, the dedup check against terminal-seen comments reads from a per-process in-memory cache (`Daemon.seenCache`) keyed `prNumber → map[commentID]bool`. The cache is hydrated eagerly at daemon construction by replaying the on-disk batches index and every terminal-seen entry it references, then refreshed on every successful `ReviewStateStore.MarkSeen` (status ∈ `{success, superseded}`) and forgotten on every `Release`. The on-disk `review-state.json` files remain the source of truth — the cache only short-circuits the per-tick on-disk scan, it does not introduce a new persistence boundary. `Daemon.InvalidateSeenCache()` re-runs the on-disk scan and replaces the cache; production calls this when an out-of-band change to `batches.json` or a `review-state.json` is observed (e.g. via fsnotify in a later slice, or as a slow-tick recovery). Issue [#1480](https://github.com/rafaelromao/sandman/issues/1480) slice A.

### Per-PR slot table

Replace the per-tick inner `sem := make(chan struct{}, parallel_reviews)` allocation in `tick` with a daemon-scoped slot table that survives across ticks. The slot table is built from two pieces: a shared buffered channel (`slotPool`) sized to `EffectiveReviewParallel()`, and a per-PR map (`map[prNumber]struct{}`) recording which PR holds a slot.

The slot is acquired by `processPR` before launching, after the newest unseen trigger is identified but before the eyes reactions are added. `acquirePRSlot` first checks the per-PR map — if the PR already has a held slot, the function returns `false` and `processPR` exits silently (no reactions, no launch, no `MarkSeen`); the trigger survives in `ListPRComments` and is processed on the next tick. If the per-PR map has no entry, it attempts a non-blocking send on `slotPool`; if the pool is saturated, `processPR` also exits silently. On either path no trigger is dropped.

Two design choices, recorded here:

1. **Where the slot is held across the tick boundary** — the slot is held by `processPR`'s deferred `releasePRSlot`, which runs after `MarkSeen` persists the trigger as terminal-seen. The slot reflects on-disk truth, not in-flight truth. This closes the "slot dropped because the tick is faster than `MarkSeen`" failure mode: the next tick cannot free the slot until the review has been fully terminal-seen.

2. **How a new trigger is expressed while a review is in flight** — the trigger is re-scanned from `ListPRComments` on the next tick. The trigger stays in the seen cache as non-terminal-seen (slice A's hydration is unaffected), so the next `processPR` for the same PR re-identifies it as unprocessed. This avoids a bounded queue on the slot channel and keeps the table semantics simple: a per-PR slot is binary (held or free), and triggers either get the slot now or wait for the next tick.

The slice-B claim lock (`busy=1` + `TryClaim` in `processPR`) is a precondition: it guarantees that two `processPR` calls for the same PR never enter `launchReview` simultaneously, which makes the per-PR slot bookkeeping single-owner (the in-flight `launchReview`) and trivially correct. The slot table is in-memory only — a daemon restart abandons the slot, and the trigger is re-discovered via `ListPRComments` on the next tick. Per-run `review-state.json` (already existing) is unaffected.

### Verify off the critical path (slice D)

The original implementation of `launchReview` ran `verifyReviewPosted` synchronously after `RunBatch` returned: three `ListPRComments` polls at 5-second backoff (up to ~15s wall) to wait for the agent's review comment to appear on GitHub. Combined with agent startup and `RunBatch` execution, this routinely overrun the 30s `PollingInterval` and triggered "previous tick still running, skipping" — which dropped incoming `/sandman review` comments. Issue [#1482](https://github.com/rafaelromao/sandman/issues/1482) (slice D) moves the verify step off the critical path.

**Chosen option:** Option 1 (lazy verify).

**Critical-path latency budget:** `launchReview` returns within `RunBatch_completion + small_constant`. The `~15s` retry chain is removed. Slice B's `busy=1` semantic still bounds the outer critical path; under lazy verify the daemon's `busy` semaphore is held for a bounded, predictable window.

**Promotion step:** every `tick` first runs `promotePendingReviews` (after acquiring `busy`, before `ListOpenPRs`). For each in-memory pending entry the daemon calls `ListPRComments` once. Three outcomes:

- A non-trigger comment is observed at-or-after the launch timestamp → `MarkSeen("success")` on the per-run store; the cache hook fires; the entry is dropped.
- No review comment yet → the entry's cycle counter increments. After `pendingMaxCycles = 3` cycles (~90s at default `PollingInterval`) the entry is promoted to `MarkSeen("failure")`; the daemon also calls `MarkTerminalSeen` on the seen cache hook so the next tick skips the trigger without re-launching. This is a slice-D-only path; `MarkSeen("failure")` invoked from processPR's RunBatch-error branch does NOT fire `MarkTerminalSeen` (failure remains retryable per slice A's contract).
- `ListPRComments` errors are logged and the entry is kept — a temporary GitHub outage does not silently promote an in-flight review to failure.

**Post-failure carve-out (issue #1891, amended by #1949):** the S6 single-shot escape above is preserved for the RunBatch-error branch only (`recordLaunchFailure` writes `MarkSeen("failure")`, which does not fire the SeenCacheInvalidator hook since `failure` is not in `shouldSkipDedupStatus`). The missing-decision.md and decision.md-is-a-directory branches are amended by #1949 to write `pending` to `review-state.json` and register a `pendingPost` entry, matching the post-failure branch. The trigger is NOT marked terminal-seen in any of these branches. The post-failure branch (`PostComment` returned a non-ctx error) is also amended under #1891: postDecision retries the `gh pr comment` call up to 5 times with exponential backoff (1s/2s/4s/8s/16s; worst case ~31s). On final failure, postDecision writes `pending` to `review-state.json` and registers a `pendingPost` entry so the S4 rehydrate walker recovers the trigger on the next tick (same process or after restart). See ADR-0014 §Retry the post step on transient gh failures for the full contract.

**Retryable failure recovery (issue #1949):** for the missing-decision.md and decision.md-is-a-directory branches, the next tick's rehydrate walker (`tryRehydratePost`) finds decision.md still missing or still a directory at tick time, drops the stale pending entry, and falls through to the launch path. The launch path's `ClearReviewArtifacts` defer removes the worktree directory (which removes any directory-shaped `decision.md`) so the next `postDecision` observes a clean slate. For the post-failure branch, decision.md is on disk and rehydrate actually retries the post; the trigger stays in `pending` until the post lands or an operator intervenes. Operators who want guaranteed lossless recovery across restarts during the post-failure pending window should re-post `/sandman review` on the PR after the daemon restarts; this trade-off is bounded (a 90s window at default `PollingInterval`) and acceptable.

**New status — `pending`:** `review-state.json` can now carry `pending` in `seenComments`. `pending` is NOT in `shouldSkipDedupStatus` (it remains retryable from the per-run store's perspective); the in-process `pendingReviews` map and the per-tick pending-set filter in `processPR` are what guard against double-launching while pending.

**Dedup consequence for `(prNumber, commentID)`:** the seen cache continues to short-circuit only `success`/`superseded` entries; pending comments are still subject to in-memory dedup via the daemon's `pendingReviews` map keyed by PR number plus a per-PR-set filter in `processPR`. A follow-up tick that processes the same PR sees pending filters drop the trigger from `unprocessed`, and `promotePendingReviews` resolves the entry toward `success`/`failure`. There is no observable window in which a trigger is double-launched.

**Restart semantics:** `pendingReviews` is in-process only. A daemon restart drops the pending window — a `pending` `review-state.json` from a prior run will be picked up on a future tick by `loadSeenCache` only if it has reached `success`/`failure`/`superseded`. Operators who want guaranteed lossless recovery across restarts during the pending window should re-post `/sandman review` on the PR after the daemon restarts. This trade-off is bounded (a 90s window at default `PollingInterval`) and acceptable for slice D.

## Consequences

### Positive

- Daemon state is minimal: `review.sock` + `review-prompt.md` at `.sandman/reviews/`. No per-PR directories.
- Dedup key `(pr, commentID)` is stable and unambiguous — no timestamp drift or clock sync issues.
- Per-run review state is co-located with the run that produced it — cleanup follows the batch/run lifecycle (ADR-0032).
- No orphan PRs: all open PRs with unprocessed `/sandman` comments are scanned, regardless of daemon uptime.
- Claims are atomic with the rest of `review-state.json` — no separate directory to manage.
- Per-PR seen cache (issue #1480 slice A) keeps tick latency constant as the number of historical review batches grows — `processPR` does not re-read `.sandman/batches.json` or any `review-state.json` for cached PRs. Regression tests: `TestDaemon_SeenCacheHydratedAtConstruction`, `TestDaemon_ProcessPRScalesConstantlyWithPriorBatches`, `TestDaemon_ReviewStateStore_MarkSeenInvalidatesCacheMidProcess`, `TestReviewStateStore_MarkSeenFiresCacheHook`, `TestReviewStateStore_MarkSeenSaveErrorLeavesCacheUntouched`, `TestDaemon_ReleaseForgetsCacheEntry` (in `internal/review/daemon_sliceA_test.go` and `internal/review/state_test.go`).
- Per-PR slot table (issue #1481 slice C) holds a slot for the in-flight review across ticks, so new `/sandman review` triggers on a busy PR are not silently dropped while the previous review is still running. Regression tests: `TestDaemon_ParallelReviews_HoldsPerPRSlotAcrossTicks`, `TestDaemon_ParallelReviews_HonorsGlobalCap`, `TestDaemon_MidFlightCommentOnBusyPR_IsProcessedAfterRelease`, `TestDaemon_HeldSlotLeavesSeenCacheNonTerminal` (in `internal/review/daemon_sliceC_test.go`).
- Lazy verify (issue #1482 slice D) keeps `launchReview`'s critical path proportional to `RunBatch` and removes the ~15s inline retry window. Regression tests: `TestDaemon_PromotePendingComment_ReturnsSuccessWhenReviewFound`, `TestDaemon_PromotePendingComment_ReturnsErrorWhenMissing`, `TestDaemon_PromotePendingComment_IgnoresTriggerComment`, `TestDaemon_LaunchReviewReturnsFastAndRecordsPending`, `TestDaemon_NextTickPromotesPendingCommentToSuccess`, `TestDaemon_NextTickRejectsPendingCommentToFailureAfterBound`, `TestDaemon_NextTickRejectsPendingCommentTwiceNoOp`, `TestDaemon_PendingNotTerminalInSeenCache`, `TestDaemon_LaunchReviewReturnsFastOnRunBatchError` (in `internal/review/daemon_sliceD_test.go`).

### Negative

- The daemon must scan all open PRs on every cycle — no per-PR caching of "nothing new here." Acceptable given GitHub API pagination and the single-repo, single-operator workload.
- If `review-state.json` is deleted mid-run, the daemon re-processes the same comment — no external idempotency guard. Recoverable but potentially wasteful.
- The seen cache must be invalidated on every successful `MarkSeen` (which slice A handles via the `SeenCacheInvalidator` seam) and reconciled on out-of-band disk writes (which slice A recovers via `InvalidateSeenCache`); missing an invalidation or recovery leads to a stale-skip bug. The contract is enforced by tests above plus a save-error test that pins the advisory invariant.
- The slot table is in-memory only; if a daemon restart occurs while a review is in flight, the slot is abandoned. The next tick re-discovers the trigger in `ListPRComments` and re-launches — bounded retry cost, not a correctness issue. Slice D's `pendingReviews` map carries the same in-memory-only restart trade-off.

### Neutral

- `review.sock` and `review-prompt.md` location is shared between this ADR and ADR-0032.
- `review-state.json` schema is defined in ADR-0032 alongside other run-folder artifacts.
- The `Reviewing` status entry in `CONTEXT.md` is unaffected by this ADR.
