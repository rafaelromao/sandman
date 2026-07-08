# ADR-0036: Batches index entry id equals the per-row run id

## Status

superseded

> **Superseded by issue #1917 slice 1 (March 2026), in turn refined by
> issue #1922 slice 6 of parent PRD #1916.** The public BatchId
> contract this ADR defended (`manifest.BatchId == per-row RunID`)
> is replaced by `public BatchId == batch folder basename ==
> batch.json.batchId == run.json.BatchID == event payload batch_id`,
> with the per-row RunID documented separately as the row identity
> surfaced in the portal. The new contract lives in:
>
> - `docs/adr/0030-standardize-run-id-and-run-dir.md` §RunDir naming
>   templates, §Per-row RunID templates, §Prompt BatchId and RunID
>   use the `prompt` segment, §Linked review BatchId and RunID
>   include the linked issue
> - `docs/adr/0032-sandman-layout-redesign.md` §`batch.json` schema
>   (BatchId headline definition) and §Row-level action resolution
>   identity table
>
> Do **not** implement from the body of this ADR. The body is retained
> for historical audit only and contradicts the current public BatchId
> rule. The `idx.Add`-only-called-from-`Prepare` invariant described
> below is preserved across the contract change; only the value of
> `manifest.BatchId` shifted.

## Context

`batches.json` (the portal's discovery index at `.sandman/batches.json`) recorded one entry per `daemon.RunSession`. Each entry had:

- `ID` — the key the portal used to address the row, look up `run.json`, drive the archive endpoint, and resolve the per-run socket for the abort handler.
- `Path` — the on-disk batch directory.
- `Kind`, `Status`, `Issues`, `PR`, `CreatedAt` — kind/issue/PR/status bookkeeping.

For issue-driven batches, ADR-0030 distinguished two identifiers:

- The **batch dir name** `<ts>-<sid>-<firstIssue>+<N>`, e.g. `260618113825-abcd-42+1`. This is the on-disk `<batch>/` directory under `.sandman/batches/` that holds `batch.json`, `batch.sock`, and `runs/<runID>/`.
- The **per-row RunID** `<ts>-<sid>-<issueNum>`, e.g. `260618113825-abcd-42`. This is the `run_id` the orchestrator emitted in `run.started` / `run.continued`, stored in `run.json`'s `RunID` field, and used as the per-run folder name `<batch>/runs/<runID>/`.

These two values were distinct for issue-driven batches: the batch dir carried the `+<N>` count suffix; the per-row RunID did not.

The four batch-kind registration paths in `cmd/run.go`, `cmd/review.go`, `cmd/selection.go`, and `review/daemon.go` registered the batches index entry id inconsistently, and this ADR set the rule that `Entry.ID == perRowRunID` for every batch kind to remove the inconsistency.

## Decision (historical — superseded)

> **The text under this heading is preserved verbatim for historical
> audit only.** The bolded "MUST" rule below was the contract this
> ADR defended at adoption time; it has been replaced by the
> slice-1 contract (`public BatchId == batch folder basename`), as
> described in ADR-0030 §RunDir naming templates and ADR-0032
> §`batch.json` schema. Do not implement from this section.

> **Historical rule (no longer current):** The batches index entry id
> MUST equal the per-row RunID the orchestrator will emit in
> `run.started` / `run.continued` and store in `run.json`'s `RunID`
> field, for every batch kind.

This rule is the historical artifact preserved for audit. It is **no longer the public BatchId contract**: public BatchId is the batch folder basename (see ADR-0030 §RunDir naming templates and ADR-0032 §`batch.json` schema). The five registration sites below computed the entry id by mirroring the orchestrator's per-row RunID formula:

| Path | Historical `Entry.ID` |
|------|------------------------|
| `sandman run --auto` | `runid.NewRunID(KindIssue, fmt.Sprintf("%d", issues[0]), ts, shortid)` (unchanged — already correct) |
| `sandman run --prompt --run-id myid` | `runid.NewRunID(KindPromptOnly, "prompt-myid", ts, shortid)` |
| `sandman run --prompt` (no userid) | `runid.NewRunID(KindPromptOnly, "prompt", ts, shortid)` |
| `sandman run <issue>` (single) | `runid.NewRunID(KindIssue, fmt.Sprintf("%d", firstIssue), ts, shortid)` |
| `sandman run <issues...>` (multi) | `runid.NewRunID(KindIssue, fmt.Sprintf("%d", firstIssue), ts, shortid)` (canonical row = first row) |
| `sandman run --continue <issue>` | same as `run <issue>` (first issue's per-row RunID) |
| `sandman review` (orphan) | `perRowRunID` from `internal/cmd/review.go` (unchanged — already equal) |
| `sandman review` (linked issue) | `perRowRunID` = `<ts>-<sid>-<issue>-PR<pr>` |
| `review` daemon | `reviewRunIDFor(prNumber, linkedIssue, ts, shortid)` |
| `selection.go` auto-select | unchanged (already correct by coincidence) |

The discriminator at `internal/cmd/portal.go:357-361` (the abort handler's "runID might actually be the batchID" fallback) was removed once `manifest.BatchId == perRowRunID` was true. After the slice-1 contract change (issue #1917), `manifest.BatchId` returned to meaning "public BatchId == batch folder basename," so the discriminator no longer applies.

## Consequences (historical)

### Positive (at the time of adoption)

- Single structural rule replaced four divergent registration paths.
- The portal's `run.RunID == run.BatchKey` discriminator was no longer special-cased.
- The `batchindex.canonicalizeEntryID` path-basename fallback became unreachable for new batches.

### Why the rule no longer holds

The slice-1 contract change (issue #1917) deliberately re-separated the per-row RunID from the public BatchId for multi-issue batches: the public BatchId is `<ts>-<sid>-<firstIssue>+<N>` (carrying the additional count) and the per-row RunID is `<ts>-<sid>-<firstIssue>` (no suffix). Re-asserting `Entry.ID == perRowRunID` after slice 1 would re-introduce the mismatch this ADR was originally written to fix, but now in the opposite direction (the batch folder basename would no longer match `Entry.ID`). The slice-1 + slice-6 model is:

- Public BatchId == batch folder basename (`batches/<id>/`'s last path segment).
- Per-row RunID == row identity surfaced in the portal (`run.json.runID`).
- `Entry.ID` is an internal index key, distinct from both for multi-issue batches, and may match either for single-issue / prompt-only / review / auto-select batches (see ADR-0032 §Row-level action resolution identity table).

### Legacy fallback (preserved)

`batchindex.canonicalizeEntryID` path-basename fallback is RETAINED for batches provisioned before the slice-1 contract change. Removing it would orphan legacy entries whose `manifest.BatchId == ""`. The fallback is unreachable for new batches because every registration site now sets `manifest.BatchId` to the public BatchId (== batch folder basename).

The companion fix in `docs/adr/0030-standardize-run-id-and-run-dir.md` corrected two stale templates in the per-row table (lines 49 and 53 at the time of this ADR's adoption). The templates are now governed by ADR-0030 §Per-row RunID templates and ADR-0032 §Row-level action resolution identity table.

## Blocked by

None — can start immediately.

## Runtime Context

- Issue: #1675
- Parent: #1672
- Supersedes: the ad-hoc discriminator at `internal/cmd/portal.go:357-361` (the "runID might actually be the batchID" fallback)
- Companion fix: `docs/adr/0030-standardize-run-id-and-run-dir.md` lines 49 and 53 (corrected per-row RunID templates)
- **Superseded by**: issue #1917 slice 1 (March 2026), refined by issue #1922 slice 6 of parent PRD #1916 — public BatchId is the batch folder basename; per-row RunID is the row identity. See the new contract in ADR-0030 and ADR-0032.
