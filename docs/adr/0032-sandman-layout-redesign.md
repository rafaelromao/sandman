# ADR-0032: `.sandman/` layout redesign — batches, runs, and the master index

## Status

accepted

## Context

The original `.sandman/` layout grew organically and became incoherent:

- What was called a "run directory" (`.sandman/runs/<id>/`) was actually a **batch** — it held the daemon sockets, the `batch.json` manifest, and the host config snapshot, while the N AgentRuns inside shared one folder with no per-run home.
- Log files lived in a flat `.sandman/logs/` named by issue number or branch slug, disconnected from the run that produced them.
- Worktrees under `.sandman/worktrees/` were linked to runs only by convention, with no entry in `batch.json` pointing to them.
- `batch.json` had a field literally named `RunID` that actually held a per-row identifier — the name was wrong and there was no field identifying the batch itself.
- There was no master index. The portal, `archive`, and `clean` rebuilt their view on every invocation by walking the filesystem. Renaming `.sandman/` or `runs/` would silently break every tool.
- At end of run, log files and the run dir were deleted automatically — no archive step, no operator control over artifact retention.
- The old `.sandman/runs/` and `.sandman/logs/` directories were to be wiped on the redesign.

Parent: [#1218](https://github.com/rafaelromao/sandman/issues/1218) — Sandman `.sandman/` folder layout redesign, "ADRs to write" section, Phase 6.

Relevant user stories from #1218: 1–26, 44–47.

## Decision

### Directory structure

The new layout is organized around three ideas:

**1. Batches own runs.** A batch is a folder (`.sandman/batches/<batch-id>/`) containing N run folders. The daemon, the control socket (`batch.sock`), the `batch.json` manifest, and the `config/` host snapshot live at the batch root. Each run folder (`.sandman/batches/<batch-id>/runs/<run-id>/`) holds its own `run.json`, `run.log`, per-run command socket (`run.sock`), and (for review runs) `review-state.json`.

**2. A master index with paths as data.** `.sandman/batches.json` records every batch ever created with its on-disk path frozen at write time. Renaming `.sandman/` or `batches/` requires only a batched index rewrite — no code changes. Batches carry `status` (`active` / `archived` / `unavailable`) and lifecycle is driven by `archive`, `clean`, and lazy on-read filesystem probes.

**3. Explicit lifecycle.** Runs do not delete their artifacts on close. `sandman archive` moves a batch folder to `.sandman/archive/<id>/` (mirroring `batches/` 1:1). `sandman clean` deletes active batches; `sandman clean --archived` deletes archived batches; both reap `unavailable` entries. The portal reads the index first and only probes the filesystem for live-socket state.

### Index schema (`batches.json`)

```jsonc
{
  "version": 1,
  "entries": [
    {
      "id":         "<batch-id>",
      "path":       ".sandman/batches/<batch-id>",
      "kind":       "issue" | "auto-select" | "review" | "prompt-only",
      "status":     "active" | "archived" | "unavailable",
      "createdAt":  "<RFC3339>",
      "issues":     [1213, 1214],          // issue/auto-select/review; empty for prompt-only
      "pr":         1217,                   // review only
      "archivedAt": "<RFC3339>",            // present when batch-level status == "archived"
      "runs": [                              // per-row state; absent on legacy batches
        {
          "runId":       "<per-row RunID>",
          "status":      "active" | "archived" | "unavailable",
          "archivePath": ".sandman/archive/<batch-id>/runs/<per-row RunID>"  // present when per-row status == "archived"
        }
      ]
    }
  ]
}
```

`version: 1` reserves a hook for future schema migrations. `path` is frozen at write time. Renaming folders requires only a batched index rewrite. The `runs` array is the per-row projection of the batch: each row carries its own `status` and `archivePath` so multi-row batches can archive one row at a time without flipping the batch-level state. Per-row archive moves `runs/<runID>/` to `.sandman/archive/<batchId>/runs/<runID>/` and writes a `runs[runID]` record carrying `status: "archived"` and the new `archivePath`; the batch-level `status` stays `active` until every row is archived AND the batch daemon is gone.

### `batch.json` schema

```jsonc
{
  "batchId":   "<batch-id>",
  "issues":    [1213, 1214],
  "createdAt": "<RFC3339>",
  "runKind":   "issue" | "auto-select" | "review" | "prompt-only",
  "candidates":[...],     // auto-select only
  "query":     "...",     // auto-select only
  "count":     0,         // auto-select only
  "pr":        1217       // review only
}
```

The legacy `BatchManifest.RunID` field is renamed to `BatchId`. **`BatchId` is the public batch id and equals the batch folder basename** (== `batch.json.batchId` == `run.json.BatchID` == event payload `batch_id`). This is the canonical contract for the public identity of a batch; the per-row RunID in `run.json.runID` is the per-row identity and is documented separately under "Row-level action resolution" below. The field semantics shift from "per-row RunID" (misnamed) to "the batch identifier generated by `internal/runid.NewBatchID`." `BatchId` is written into `batch.json`, the index entry, and every `run.json` in the batch.

### `run.json` schema

```jsonc
{
  "runID":        "<per-row RunID>",
  "batchId":      "<batch-id>",
  "issue":        1213,
  "branch":       "sandman/1213-...",
  "baseBranch":   "main",
  "worktreePath": ".sandman/worktrees/sandman/1213-...",  // frozen at write time
  "kind":         "issue" | "auto-select" | "review" | "prompt-only",
  "createdAt":    "<RFC3339>",
  "pr":           1217,                                     // review only
  "status":       "active" | "success" | "failure" | "aborted" | "blocked"
}
```

`status` is written as `"active"` at run start and rewritten to the terminal value (`"success"`, `"failure"`, `"aborted"`, or `"blocked"`) when the run finishes. The event log remains authoritative for projections; `run.json` is a read-only snapshot after completion.

`worktreePath` is captured at write time. Renaming `.sandman/worktrees/` later does not break historical references; only new runs use the new path.

### Lifecycle transitions

| Transition | Action |
|------------|--------|
| Batch creation | Write `batch.json` at batch root, write index entry, create `runs/` directory, start daemon |
| Run start | Create `<batch>/runs/<run>/`, open `run.sock` inside run folder, open `run.log` with `O_APPEND`, write `run.json` via `WriteRunManifest` |
| Run end | Update `run.json.status` to the terminal status, close sockets, close log file. **No deletion.** Folder persists |
| Per-row archive | `os.Rename(<batch>/runs/<runID>/, archive/<batchId>/runs/<runID>/)` first, then append/update `runs[runID]` record to `status:"archived"` with `archivePath`. Batch-level `status` stays `active` until every row is archived |
| Whole-batch archive | `os.Rename(<batch>/, archive/<id>/)` first, then update index entry to `status:"archived"`. The existing `archive batch <batchId>` CLI subcommand |
| Clean (no flag) | Remove `active` and `unavailable` entries + their folders |
| Clean `--archived` | Remove `archived` and `unavailable` entries + their folders |
| Clean `--dry-run` | Preview deletions without removing; same targeting as `clean` or `clean --archived` |
| Clean `--stale` | Recover stale runs as `aborted` via event log; no index change |
| Lazy unavailable | Any code reading the index stats each batch path; only `ENOENT` flips status to `unavailable` |
| Lazy per-row reconcile | After `MarkUnavailable`, `ReconcileRuns` walks every `runs[]` record; a row whose `archivePath` is non-empty but whose live folder is gone AND whose archive folder is gone flips to `status:"unavailable"` |

Folder rename always happens before the index update during archive (both per-row and whole-batch). If the index write fails, the inconsistency is detectable on next read: the live folder is gone, the archive folder exists, and the per-row record still carries the path.

### Row-level action resolution

Row-level actions (archive, abort, log download) MUST resolve by per-row RunID, not by the public BatchId (the batch folder basename). The portal UI sends the per-row RunID because that is the id the orchestrator emits in `run.started` and stores in `run.json.runID` — the per-row RunID is the id the operator sees on the row, not the public BatchId they see in the Batch label and Details tab.

| Action | Input | Resolver | Per-run artifact |
|--------|-------|----------|------------------|
| Per-row archive | `runId` (per-row RunID) | `resolveBatchEntryForRunID` -> index entry | `archive/<batch-id>/runs/<perRowRunID>/` |
| Abort | `runKey` (per-row RunID) | `portalRunForKey` -> `portalRun.RunID` -> `<batchDir>/runs/<perRowRunID>/run.sock` | `<batchDir>/runs/<perRowRunID>/run.sock` |
| Log download | `/api/runs?runKey=<perRowRunID>` -> `/api/logs?path=<per-row-log-path>` | keyed row lookup -> `runLogPath` -> `handleLogs` | `<batchDir>/runs/<perRowRunID>/run.log` |
| Whole-batch archive | `batchId` (CLI only) | `idx.Resolve(batchId)` -> index entry | `archive/<batch-id>/` |

Two distinct helpers resolve row-action inputs because their fallback contracts differ (issue #1923 slice 7):

- `resolveBatchEntryForRunID` (package-level, `internal/cmd/portal.go`) is the archive-endpoint hot path. The fast path is `idx.Resolve(runID)`, which matches the batch entry id directly; the second path walks each entry's `Runs[]` records so already-archived rows resolve from the index without an on-disk probe; the final fallback is a stat-only scan that returns the first entry whose `runs/<runID>/run.json` exists on disk.
- `resolveBatchFromRowID` (method, `internal/cmd/portal_runs_view.go`) is the log/portal runs-view path. Its fast path is also `idx.Resolve(runID)`, but the fallback parses each `runs/<runID>/run.json`, reads `run.json.batchId`, and re-resolves that id in the index so the log path follows the manifest's declared owner.

The fast path succeeds for every batch kind in production except multi-issue issue runs (`<ts>-<sid>-<firstIssue>+<N>` vs `<ts>-<sid>-<firstIssue>`); the fallback path covers multi-issue batches, where the public BatchId and the per-row RunID differ. The resolver contract is "accept either form, return the right entry" — operators do not need to know which kind they are working against, and each helper hides that detail behind its own focused fallback contract. Per issue #1944, only the canonical `<ts>-<sid>-...` shape is recognised; legacy `<sid>-<ts>-...` on-disk batches are no longer resolvable and must be re-provisioned after upgrade.

The per-run folder is the canonical target for every row action. The archive directory is named after the batch entry id (the public BatchId), so the on-disk tree after per-row archive mirrors the active tree 1:1 (`archive/<batch-id>/runs/<perRowRunID>/run.json` corresponds to `batches/<batch-id>/runs/<perRowRunID>/run.json`). Per-row archive moves ONLY `runs/<perRowRunID>/`, leaves sibling rows and the batch daemon live, and writes a `Runs[runID]` record carrying `status: "archived"` and the relative `archivePath` so the row survives crash recovery. The portal archive endpoint returns empty `200` on success and `409` with `archivePath` echoed in the body when the row is already archived.

Whole-batch archive (CLI `archive batch <batchId>` only - not exposed via HTTP) moves the entire batch dir to `.sandman/archive/<batchId>/` and flips the batch-level `Status` to `archived`. The daemon control socket must be gone for whole-batch archive to succeed; per-row archive has no daemon liveness requirement because the targeted row is terminal by contract.

#### Identity table: when per-row RunID equals the public BatchId

The slice-1 contract (`manifest.BatchId == public BatchId == batch folder basename == batch.json.batchId == run.json.BatchID == event payload batch_id`) deliberately makes the per-row RunID and the public BatchId equal for some batch kinds and deliberately distinct for others. The row-action resolver handles both forms transparently; the identity table is the public specification of which is which. The public BatchId is **always** the batch folder basename; the per-row RunID is the row's identity surfaced in the portal.

| Batch kind | Public BatchId (== batch folder basename) | Per-row RunID (== `run.json.runID`) | Equal? |
|------------|-------------------------------------------|-------------------------------------|--------|
| Issue single | `<ts>-<sid>-<num>` | `<ts>-<sid>-<num>` | yes (no `+N` suffix on single-issue) |
| Issue multi (canonical row = first issue) | `<ts>-<sid>-<firstIssue>+<N>` | `<ts>-<sid>-<firstIssue>` | no (carries `+<N>`) |
| Review (orphan) | `<ts>-<sid>-PR<pr>` | `<ts>-<sid>-PR<pr>` | yes |
| Review (linked to issue) | `<ts>-<sid>-<linkedIssue>-PR<pr>` | `<ts>-<sid>-<linkedIssue>-PR<pr>` | yes |
| Auto-select | `<ts>-<sid>-auto-<N>` | `<ts>-<sid>-auto-<N>` | yes |
| Prompt-only (no user id) | `<ts>-<sid>-prompt` | `<ts>-<sid>-prompt` | yes |
| Prompt-only (with user id) | `<ts>-<sid>-prompt-<userid>` | `<ts>-<sid>-prompt-<userid>` | yes |

The "no" row in the table is the only production case where the row-action resolver MUST exercise its fallback path. The fast path (`idx.ResolveBatch(runID)`) succeeds for every other kind in production: single-issue, orphan review, linked review, auto-select, and prompt-only all produce identical strings for the per-row RunID and the public BatchId, so the public BatchId is enough. The fallback path (`runs/<runID>/run.json` scan — stat-only for `resolveBatchFromRunIDFastOrScan`, parse-then-resolve for `resolveBatchFromRowID`) is automatic and exists to cover multi-issue batches and any legacy batches provisioned before the #1917 slice-1 contract change pinned `manifest.BatchId == public BatchId`. The resolver's contract is "accept either form, return the right batch" — operators do not need to know which kind they are working against, and the resolver hides that detail.

Multi-issue batch row actions target the selected row, not a sibling row. Each per-row RunID is a distinct string, and the resolver's on-disk scan picks the entry whose `runs/<runID>/run.json` exists; two sibling rows in the same batch produce two distinct `runs/<runID>/run.json` files, so the resolver always returns the entry that owns the selected row. The archive endpoint then walks the per-run folder, the abort endpoint dials `<batchDir>/runs/<perRowRunID>/run.sock`, and the log endpoint serves `<batchDir>/runs/<perRowRunID>/run.log` — three independent code paths that all key on the same per-row RunID.

#### Continuation runs

`sandman run --continue` creates a new batch and new per-row RunIDs. It reuses the previous run's branch and worktree path, but it does not reuse the previous public BatchId or previous per-row RunID. The new batch is a sibling of the previous batch under `.sandman/batches/`, and the previous batch directory, run folder, manifest, and event log remain unchanged.

The continuation row emits `run.continued` using the new per-row RunID. Its event payload includes `previous_run_id`, pointing to the immediate prior per-row RunID. The event fold treats `run.continued` as the start event for a fresh `RunState` keyed by the new RunID, so the continuation can be archived, aborted, and viewed independently from the previous row.

The continuation prompt is rendered from the previous worktree's `.sandman/task.md` through the normal task prompt path. The original task file is not modified as part of constructing the continuation request.

Error semantics are preserved: archive returns 404 when no entry hosts the per-row manifest, 409 when the resolved batch is not active or the daemon socket is still live, and 500 only on filesystem failure. Abort returns 404 when the per-run socket does not exist, 409 when the daemon is no longer live or the orchestrator rejects the abort, and 502 only on dial / read / write failure. The 404/409 paths are observable per kind and per row, not collapsed to a single batch-level status.

### Index writer

One `batches.json` document. Atomic write: write to `batches.json.tmp`, then `os.Rename` over `batches.json`. Keep `batches.json.bak` on each successful write for recovery.

Concurrent daemons writing different entries are safe — full document rewrite, last writer wins. No file lock.

Bulk operations (`archive older-than`, `archive stale`, `clean`, `clean --archived`) do a single read-modify-write per invocation.

### What is deleted

- `.sandman/logs/` directory and all `layout.LogDir` references
- `SafeLogFilename` helper (every log is `run.log`)
- `os.RemoveAll(s.runDir)` in `RunSession.Close` (artifacts persist until archive/clean)
- `ClearIssueArtifacts` log-file deletion in the orchestrator
- `--success` and `--failed` flags on `clean`
- Backward-compatibility shims for old paths

### What is added

- `clean --dry-run` to preview deletions before destructive operations
- `clean --stale` to recover stale runs as `aborted` without changing the index

### What is kept

- `.sandman/portal/` orphan directory left untouched (no production-code references)
- `events.jsonl`, `config.yaml`, `Dockerfile`, `prompt.md`, `auto-selection-prompt.md`
- Worktrees under `<cfg.WorktreeDir>/<branch>` unchanged on disk

## Consequences

### Positive

- On-disk structure matches the conceptual structure: batches own runs.
- Master index survives folder renames — paths are data, not convention.
- Artifact lifecycle is operator-controlled: explicit `archive` and `clean` steps.
- Portal reads index first; filesystem walks only for live-socket state.
- Schema version on `batches.json` reserves a migration hook.
- No dual paths — greenfield redesign is complete.

### Negative

- Historical runs referencing old `.sandman/runs/` paths are not automatically migrated.
- No automatic archiving; artifacts accumulate until operator runs `archive` or `clean`.
- Concurrent index writers use last-wins semantics; some batch metadata may be lost under heavy write contention (acceptable for single-repo, single-operator workload).

### Migration out of scope

**Existing `.sandman` migration is out of scope.** The slice-1 contract change (issue #1917) and the identity alignment that followed (slices 2–6 of parent PRD #1916) rename the public BatchId surface and the per-row RunID templates. Batches provisioned before the contract change carry old id shapes (legacy `+1` single-issue, total-count `+N`, prompt-only without the `prompt` segment, etc.) and are not rewritten in place. The operator is expected to delete `.sandman` after rebuilding; no migration tool ships for the old layout. This is a greenfield redesign, not a back-compat release.

### Neutral

- ADR-0030 (Standardize Run ID and Run Dir naming) is extended by this ADR's batch/run split.
- The `RunDir` and `Archive` glossary entries in `CONTEXT.md` are updated to reflect the new layout.
- The review daemon state shape is covered by ADR-0034; the `.sandman/reviews/` layout is shared between this ADR and ADR-0034.
