# ADR-0036: Batches index entry id equals the per-row run id

## Status

superseded

> **Superseded by issue #1917 slice 1 (March 2026):** the
> `manifest.BatchId == per-row RunID` contract is replaced by
> `manifest.BatchId == public BatchId == batch folder basename`.
> See the new contract in `internal/daemon/run_session.go` (lines
> 105–135) and the amended templates in `docs/adr/0030-standardize-run-id-and-run-dir.md`.
> The `idx.Add`-only-called-from-`Prepare` invariant (line 128 of
> this ADR) is preserved across the contract change; only the value
> of `manifest.BatchId` shifted.

## Context

`batches.json` (the portal's discovery index at `.sandman/batches.json`) records one entry per `daemon.RunSession`. Each entry has:

- `ID` — the key the portal uses to address the row, look up `run.json`, drive the archive endpoint, and resolve the per-run socket for the abort handler.
- `Path` — the on-disk batch directory.
- `Kind`, `Status`, `Issues`, `PR`, `CreatedAt` — kind/issue/PR/status bookkeeping.

For issue-driven batches, ADR-0030 distinguishes two identifiers:

- The **batch dir name** `<shortid>-<ts>-<firstIssue>+<N>`, e.g. `abcd-260618113825-42+1`. This is the on-disk `<batch>/` directory under `.sandman/batches/` that holds `batch.json`, `batch.sock`, and `runs/<runID>/`.
- The **per-row RunID** `<shortid>-<ts>-<issueNum>`, e.g. `abcd-260618113825-42`. This is the `run_id` the orchestrator emits in `run.started` / `run.continued`, stores in `run.json`'s `RunID` field, and uses as the per-run folder name `<batch>/runs/<runID>/`.

These two values are distinct for issue-driven batches: the batch dir carries the `+<N>` count suffix; the per-row RunID does not.

Before this ADR, the four batch-kind registration paths in `cmd/run.go`, `cmd/review.go`, `cmd/selection.go`, and `review/daemon.go` registered the batches index entry id inconsistently:

| Path | Old `Entry.ID` (before this ADR) | Orchestrator's emitted `run_id` |
|------|----------------------------------|-------------------------------|
| `sandman run --auto` | `autoIssueRunID` = `<sid>-<ts>-<firstIssue>` (per-row) | `<sid>-<ts>-<firstIssue>` |
| `sandman run --prompt --run-id myid` | `manifest.BatchId = ""` → path basename = `<sid>-<ts>-myid>` | `<sid>-<ts>-prompt-myid>` |
| `sandman run <issue>` | `manifest.BatchId = autoIssueRunID` (empty) → path basename = `<sid>-<ts>-<num>+1` | `<sid>-<ts>-<num>` |
| `sandman run --continue <issue>` | same as `run <issue>` (empty) → path basename = `<sid>-<ts>-<num>+1` | `<sid>-<ts>-<num>` |
| `sandman review` (orphan) | `batchDirName` = `<sid>-<ts>-PR<pr>` (matches per-row by coincidence) | `<sid>-<ts>-PR<pr>` |
| `sandman review` (linked issue) | `batchDirName` = `<sid>-<ts>-PR<pr>` | `<sid>-<ts>-<issue>-PR<pr>` |
| `review` daemon (orphan) | `batchDirName` (matches per-row) | `<sid>-<ts>-PR<pr>` |
| `review` daemon (linked issue) | `batchDirName` = `<sid>-<ts>-PR<pr>` | `<sid>-<ts>-<issue>-PR<pr>` |
| `selection.go` auto-select | `batchID` = `<sid>-<ts>-auto-<N>` (matches per-row by coincidence) | `<sid>-<ts>-auto-<N>` |

The mismatch between `Entry.ID` and the orchestrator's emitted `run_id` forced every portal reader (the abort handler, the archive endpoint, the portal summary view, the dead-batch reconciliation) to maintain a fallback path: when an index entry's `ID` doesn't match a per-row id, fall back to the path basename. That fallback lived at `batchindex.canonicalizeEntryID` (issue #1464) and at `internal/cmd/portal.go:357-361` (the abort-handler discriminator).

The fallback was correct for orphan reviews (where `batchDirName == perRowRunID` by construction) but wrong for issue-driven runs after the per-row RunID diverged from the batch dir name. As soon as the abort handler saw `run.RunID == run.BatchKey` for an issue run (which becomes true once `Entry.ID = perRowRunID`), the fallback picked `filepath.Base(runDir) = <sid>-<ts>-<num>+N` and the abort endpoint stopped finding the per-run socket.

## Decision

**The batches index entry id MUST equal the per-row RunID the orchestrator will emit in `run.started` / `run.continued` and store in `run.json`'s `RunID` field, for every batch kind.**

The five registration sites now compute the entry id by mirroring the orchestrator's per-row RunID formula:

| Path | New `Entry.ID` (after this ADR) |
|------|----------------------------------|
| `sandman run --auto` | `runid.NewRunID(KindIssue, fmt.Sprintf("%d", issues[0]), ts, shortid)` (unchanged — already correct) |
| `sandman run --prompt --run-id myid` | `runid.NewRunID(KindPromptOnly, "prompt-myid", ts, shortid)` |
| `sandman run --prompt` (no userid) | `runid.NewRunID(KindPromptOnly, "prompt", ts, shortid)` |
| `sandman run <issue>` (single) | `runid.NewRunID(KindIssue, fmt.Sprintf("%d", firstIssue), ts, shortid)` |
| `sandman run <issues...>` (multi) | `runid.NewRunID(KindIssue, fmt.Sprintf("%d", firstIssue), ts, shortid)` (canonical row = first row) |
| `sandman run --continue <issue>` | same as `run <issue>` (first issue's per-row RunID) |
| `sandman review` (orphan) | `perRowRunID` from `internal/cmd/review.go` (unchanged — already equal) |
| `sandman review` (linked issue) | `perRowRunID` = `<sid>-<ts>-<issue>-PR<pr>` |
| `review` daemon | `reviewRunIDFor(prNumber, linkedIssue, ts, shortid)` |
| `selection.go` auto-select | unchanged (already correct by coincidence) |

The `Entry.Path` remains the batch dir name (e.g. `<sid>-<ts>-42+N`); only the `Entry.ID` changes. This means `idx.Resolve(perRowRunID)` returns the entry the portal wants, and the per-run folder under `runs/<perRowRunID>/` is now directly keyed by `Entry.ID`.

The discriminator at `internal/cmd/portal.go:357-361` (the abort handler's "runID might actually be the batchID" fallback) is removed. Post-ADR, `manifest.BatchId` ALWAYS equals the per-row RunID, so `run.RunID` is the correct per-run id in every case. The fallback was a workaround for the old contract and is no longer reachable.

The `batchindex.canonicalizeEntryID` path-basename fallback is RETAINED for batches provisioned before this ADR ships. Removing it would orphan legacy entries whose `manifest.BatchId == ""`. The fallback is unreachable for new batches because every registration site now sets `manifest.BatchId` to a non-empty per-row RunID.

`RunSession.Prepare`'s doc comment lists the contract: callers MUST set `manifest.BatchId` to the per-row RunID. A grep test in `internal/daemon/run_session_test.go` (scoped to non-test Go files) pins the invariant that `idx.Add` is only called from `daemon.RunSession.Prepare`, so future batch kinds must go through `Prepare` and inherit the contract.

The companion fix in `docs/adr/0030-standardize-run-id-and-run-dir.md` corrects two stale templates in the per-row table:

- Line 49: `Regular issue` was `<shortid>-<ts>-issue-<issueNum>`, but every call site uses `runid.NewRunID(KindIssue, fmt.Sprintf("%d", issueNum), ts, shortid)` which produces `<shortid>-<ts>-<issueNum>` (no `issue-` prefix). Updated.
- Line 53: `Prompt-only (with user id)` was `<shortid>-<ts>-<userid>`, but the orchestrator at `internal/batch/orchestrator.go` emits `<shortid>-<ts>-prompt-<userid>` (with the `prompt-` prefix). Updated.

## Consequences

### Positive

- Single structural rule replaces four divergent registration paths. Adding a new batch kind only requires emitting a per-row RunID from the orchestrator and setting `manifest.BatchId` to the same string.
- The portal's `run.RunID == run.BatchKey` discriminator is no longer special-cased: both always equal the per-row RunID for issue runs.
- The `batchindex.canonicalizeEntryID` path-basename fallback becomes unreachable for new batches, simplifying the index reader.
- The abort endpoint's `manifest.BatchId == run.BatchKey` fallback is removed entirely. Per-row socket resolution is now direct.
- The contract is auditable: the orchestrator's per-row RunID formula and the registration site's `manifest.BatchId` formula are both expressed via `runid.NewRunID(<kind>, <subject>, <ts>, <shortid>)`, so future drift between them is detectable by a code review.

### Negative

- The batches index `Entry.ID` no longer matches the on-disk batch dir name for issue-driven runs (multi-issue batches) or for reviews with linked issues. Portal code that previously used `filepath.Base(entry.Path)` as a synonym for `entry.ID` must use `entry.ID` directly. The dead-batch recovery, archive endpoint, and run-summary view already read `entry.ID` so the impact is contained.
- Legacy batches provisioned before this ADR continue to use the path-basename fallback. The fallback remains in `batchindex.canonicalizeEntryID` until those batches are archived.

### Neutral

- `manifest.BatchId` semantics shift from "batch dir name" to "per-row RunID (== entry id)". Three downstream portal readers (`portal.go:358`, `portal_index.go:252`, `portal_runs_view.go:874`) all consumed `manifest.BatchId` to recover a canonical id; post-ADR that field equals the per-row RunID, which is the source-of-truth id the portal wants anyway. Regression tests cover each reader.
- `RunSession.Prepare`'s contract is now expressed in a doc comment; future callers must set `manifest.BatchId` to a non-empty per-row RunID. A grep test on production code (excluding `_test.go` files) pins the invariant that `idx.Add` lives only in `Prepare`.

## Blocked by

None — can start immediately.

## Runtime Context

- Issue: #1675
- Parent: #1672
- Supersedes: the ad-hoc discriminator at `internal/cmd/portal.go:357-361` (the "runID might actually be the batchID" fallback)
- Companion fix: `docs/adr/0030-standardize-run-id-and-run-dir.md` lines 49 and 53 (corrected per-row RunID templates)