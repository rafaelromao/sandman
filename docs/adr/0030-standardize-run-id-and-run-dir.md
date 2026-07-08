# ADR-0030: Standardize Run ID and Run Dir naming

## Status

accepted

## Context

Sandman generates two related but distinct identifiers for every batch run:

- **RunID**: a string that uniquely identifies one AgentRun within a batch. Persisted in the `RunID` field of every event in `events.jsonl` and used as the row key in the portal.
- **RunDir**: a per-batch directory at `<batch>/` that contains the daemon's `batch.json` and `batch.sock`. Each AgentRun within the batch gets its own `<batch>/runs/<runID>/` subdirectory containing `run.json`, `run.log`, and `run.sock`.

Historically, these identifiers were generated ad-hoc in multiple places with inconsistent naming schemes. The `--run-id` flag was used as the RunDir name for prompt-only runs, but not for issue-driven runs. This made it difficult to reason about run identity across the portal, the daemon, and the event log.

Four batch kinds exist: regular issue, review, auto-select, and prompt-only. Each has different structural properties (e.g., review runs link to a PR, auto-select runs have a candidate count, prompt-only runs may or may not carry a user-supplied run id). The naming scheme must accommodate all four while remaining predictable and collision-resistant.

## Decision

### Timestamp + shortid collision guard

Every RunID and RunDir begins with a `<shortid>-<ts>` prefix:

- `<shortid>` is 4 lowercase hex characters derived from `unixNano % 0xFFFF`.
- `<ts>` is `time.Now().Format("060102150405")` (local time, 12 characters, 2-digit year).

When `NewBatch()` finds that a directory already exists at `.sandman/batches/<ts>-<shortid>-...`, it generates a new shortid and retries up to 16 times before returning an error. The timestamp is not changed during retry; only the shortid advances.

The timestamp is placed first to provide chronological ordering and put the human-readable segment at the front of the on-disk path. The shortid provides per-second collision resistance.

### RunDir naming templates

| Kind | Template |
|------|----------|
| Regular issue (single) | `<ts>-<shortid>-<issue>` (no +N suffix) |
| Regular issue (multi)  | `<ts>-<shortid>-<firstIssueNum>+<additionalCount>` |
| Review (without linked issue) | `<ts>-<shortid>-PR<prNum>` |
| Review (with linked issue)    | `<ts>-<shortid>-<linkedIssueNum>-PR<prNum>` |
| Auto-select | `<ts>-<shortid>-auto-<N>` |
| Prompt-only (with user id) | `<ts>-<shortid>-prompt-<userid>` |
| Prompt-only (without user id) | `<ts>-<shortid>-prompt` |

`<N>` is the count (number of issues in a batch, number of candidates in auto-select). `firstIssueNum` is the first issue number for regular issues.

Per issue #1917 (slice 1 of #1916), the issue batch template uses **additional issue count beyond the first** (not total count):

- Single issue (`n==1`): `<ts>-<sid>-<num>` (no `+N` suffix).
- Two issues (`n==2`): `<ts>-<sid>-<firstIssue>+1`.
- Nine issues (`n==9`): `<ts>-<sid>-<firstIssue>+8`.

The `+N` suffix on multi-issue batches therefore means **additional issue count beyond the first** — so `n=2` carries `+1` (one issue beyond the first) and `n=9` carries `+8` (eight issues beyond the first). Single-issue batches omit the plus suffix entirely; there is no `+0` because a single issue carries no additional count to advertise. The omitted suffix on single-issue batches keeps the public BatchId from carrying redundant information. The per-row RunID still uses `<ts>-<sid>-<issueNum>` (no suffix), so the per-row identity is unchanged.

The public BatchId (== batch folder basename) MUST agree with `batch.json.batchId`, `run.json.BatchID`, and the event payload `batch_id` field. The portal Batch label and Details tab render the public BatchId.

Per issue #1919 (slice 3 of #1916), the review RunDir naming and the review per-row RunID template agree exactly: the review RunDir is whichever of the two review templates above (linked vs. orphan) the PR's `LinkedIssueNumber()` resolves to. The on-disk batch directory name therefore equals the per-row RunID for both orphan and linked reviews, the per-row run folder is `<perRowRunID>/runs/<perRowRunID>/`, and the event payload's `batch_id` matches `run.json.BatchID` and the folder basename for both flavours. Review rows whose `LinkedIssueNumber()` resolves group under their linked issue row in the portal; orphan reviews stay as standalone "Review of PR <n>" rows.

### Per-row RunID templates

Each AgentRun within a batch receives a unique RunID built from the batch's `<ts>-<shortid>` prefix plus a per-row subject:

| Kind | Template |
|------|----------|
| Regular issue | `<ts>-<shortid>-<issueNum>` |
| Review (with linked issue) | `<ts>-<shortid>-<linkedIssueNum>-PR<prNum>` |
| Review (without linked issue) | `<ts>-<shortid>-PR<prNum>` |
| Auto-select | `<ts>-<shortid>-auto-<N>` |
| Prompt-only (with user id) | `<ts>-<shortid>-prompt-<userid>` |
| Prompt-only (without user id) | `<ts>-<shortid>-prompt` |

### Prompt BatchId and RunID use the `prompt` segment

Per issue #1920 (slice 4 of parent PRD #1916), every prompt-only public BatchId and every prompt-only per-row RunID carries the literal `prompt` segment. The two templates are:

- With `--run-id <userid>`: `<ts>-<sid>-prompt-<userid>` — the `<userid>` is appended after the literal `prompt-` prefix.
- Without `--run-id`: `<ts>-<sid>-prompt` — the bare `prompt` segment terminates the id.

The literal `prompt` segment is hard-coded in both `runid.NewBatchID(KindPromptOnly, …)` and `runid.NewRunID(KindPromptOnly, …)` so callers cannot drift back to a bare `<userid>` shape. The segment is also the disambiguator between prompt-only batches and issue batches: a numeric `--run-id` (e.g. `--run-id 42`) would otherwise produce `<ts>-<sid>-42`, which is also a valid single-issue BatchId. The literal `prompt` is unparseable as an issue number, so the canonical shape is unambiguous on disk and `KindFromDirName` matches `-prompt` literally before the numeric-issue-tail check.

Because the prompt-only RunDir template and the per-row RunID template are the same string, the public BatchId (== batch folder basename == `batch.json.batchId` == event payload `batch_id` == per-row RunID) is a single identifier for every prompt-only run. There is no BatchId-vs-RunID split for prompt-only batches.

### Linked review BatchId and RunID include the linked issue

Per issue #1919 (slice 3 of parent PRD #1916), every review with a `LinkedIssueNumber()` carries the linked issue in both the public BatchId and the per-row RunID. The template is:

- Review with linked issue: `<ts>-<sid>-<linkedIssueNum>-PR<prNum>` — e.g. `<ts>-<sid>-1551-PR42`.

This applies to both the RunDir name (== public BatchId == batch folder basename) and the per-row RunID (== `run.json.runID` == event payload `run_id`). Orphan reviews (no `LinkedIssueNumber()`) stay PR-only and use `<ts>-<sid>-PR<prNum>` so they do not incorrectly group under any issue. See ADR-0032 §Row-level action resolution identity table for the full kind-by-kind breakdown.

### `--run-id` flag preserved

The `--run-id` flag on `sandman run` is retained for prompt-only runs where the user may want a self-chosen identifier. However, it no longer influences the RunDir name. The RunDir is always generated fresh via `NewBatchID`. This keeps the naming scheme uniform while preserving the flag for its original purpose: giving prompt-only runs a human-meaningful identity in the portal.

### User-supplied run ID validation

When `--run-id` is provided for a prompt-only run, it is validated by `IsValidUserRunID`:

- Length: 1 to 64 characters
- Allowed characters: alphanumerics, hyphens, underscores
- No leading-letter requirement (unlike the previous strict rule)

### Portal recovery via `KindFromDirName`

The portal uses `KindFromDirName(name string) (Kind, bool)` to recover the batch kind from a RunDir name during same-second collision recovery. It matches by pattern. The classifier recognises only the canonical `<ts>-<sid>-...` shape; any name that does not start with `<digits>-<hex>` returns `(0, false)`. Among the canonical shapes it classifies by suffix:

- `^\d{12}-[0-9a-f]{4}-auto-\d+$` → `KindAutoSelect`
- `^\d{12}-[0-9a-f]{4}(-\d+-)?-PR\d+$` → `KindReview`
- `^\d{12}-[0-9a-f]{4}-\d+\+\d+$` → `KindIssue` (batch dirs only; `+N` suffix)
- `^\d{12}-[0-9a-f]{4}-prompt(-|$)` → `KindPromptOnly` (literal `prompt` segment, matched before the numeric-issue-tail check so `--run-id <num>` does not collide with issue `<ts>-<sid>-<num>`)
- `^\d{12}-[0-9a-f]{4}-\d+$` → `KindIssue` (single-issue batch)
- `^\d{12}-[0-9a-f]{4}($|-)` with no other marker → `KindPromptOnly` (fallback)

### Manifest carries `RunTS` and `RunShortID`

The `BatchManifest` persisted at `<batchDir>/batch.json` carries the batch's `<ts>` and `<shortid>` primitives as top-level fields, alongside the existing `BatchId`:

- `RunTS` — the timestamp from "Timestamp + shortid collision guard" above, repeated here for self-containment. Format: `time.Now().Format("060102150405")` (12 chars, local time, 2-digit year).
- `RunShortID` — the collision guard from "Timestamp + shortid collision guard" above, repeated here for self-containment. Format: `%04x` of `unixNano % 0xFFFF` (4 lowercase hex chars).
- `BatchId` — the full assembled batch identifier (`<ts>-<shortid>-<kindSuffix>`), retained for backward compatibility.

The manifest's JSON schema is therefore:

```json
{
  "batchId": "<ts>-<shortid>-<kindSuffix>",
  "runTs": "<ts, 060102150405>",
  "runShortId": "<shortid, 4 hex chars>"
}
```

Both fields are tagged `omitempty`, so manifests written before this amendment decode as zero values and remain readable.

### Per-row RunID derivation contract

Event-log-less consumers (most prominently the portal's nil-state path, before `run.started` fires in `events.jsonl`) MUST derive per-row RunIDs from manifest fields via `runid.NewRunID`. Synthesizing ad-hoc formats that violate this ADR's per-row RunID templates (above) is forbidden, because such strings break the portal's row-key contract.

For a regular issue batch, the canonical call shape is:

```go
runid.NewRunID(runid.KindIssue, fmt.Sprintf("%d", issueNum), manifest.RunTS, manifest.RunShortID)
```

The four arguments are positional and named here for clarity: `KindIssue` selects the issue template, `fmt.Sprintf("%d", issueNum)` is the per-row subject (`<issueNum>`, never `"issue-<issueNum>"`), `manifest.RunTS` is the `<ts>` primitive from the manifest, and `manifest.RunShortID` is the `<shortid>` primitive.

For review (PR) batches, the portal uses `runid.NewRunID(runid.KindReview, …)` with the same `(RunTS, RunShortID)` arguments — see the per-row RunID templates above for the PR subject shape. This is included here so consumers do not have to re-derive it independently, but the issue-driven contract above is the canonical entry point called out by the issue body.

For prompt-only batches (issue #1920 slice 4), the per-row RunID is derived from the user-supplied `--run-id` (or `""` if absent) plus the `(RunTS, RunShortID)` primitives:

```go
// With --run-id <userid>: <ts>-<sid>-prompt-<userid>
runid.NewRunID(runid.KindPromptOnly, userRunID, manifest.RunTS, manifest.RunShortID)
// Without --run-id:        <ts>-<sid>-prompt
runid.NewRunID(runid.KindPromptOnly, "", manifest.RunTS, manifest.RunShortID)
```

For prompt-only the per-row RunID equals the public BatchId, so the on-disk batch folder basename, `batch.json.batchId`, the event payload `batch_id`, and the per-row RunID all carry the same string. The manifest's `RunKind` field is `"prompt-only"` for these batches.

When either manifest field is absent, callers should fall back to the queued event's `RunID` if one is available, or return the empty string. The portal implements both paths through `perRowRunIDForManifest(runTS, runShortID, prNumber, issueNumber, queued)` (pure helper) and its active-batch wrapper `perRowRunIDForActive(active, issueNumber, queued)`.

### Auto-select and post-selection issue batch

Auto mode is a two-phase flow: an auto-select selector run that picks the issues, followed by a normal issue batch that runs the selected issues. These two phases are **two distinct batches** with **two distinct identities** and must never be conflated.

The auto-select selector run uses the auto-select naming template:

- BatchId: `<shortid>-<ts>-auto-<N>` where `<N>` is the candidate count
- RunID: the same string (`<shortid>-<ts>-auto-<N>`)
- batch_id in the run.started and run.finished event payloads: the selector BatchId

The post-selection phase is a **normal issue batch** and follows the regular issue rules above. It does **not** inherit the auto-select selector's `<shortid>-<ts>` pair, does **not** carry an `-auto-` marker in its BatchId or RunID, and does **not** have `run_kind: "auto-select"` in any of its event payloads. The post-selection issue batch's per-row RunIDs are issue-specific (per the regular issue template), not auto-select RunIDs.

Concretely, when `--auto` runs to completion, two batch directories appear under `.sandman/batches/`:

1. `<sid1>-<ts1>-auto-N` (the auto-select selector)
2. `<sid2>-<ts2>-<firstIssue>[+<additionalCount>]` (the post-selection issue batch)

with `<sid1>` ≠ `<sid2>` and `<ts1>` ≠ `<ts2>` in general. The portal renders the selector as an "auto-selecting" row (one row per selector run) and the post-selection issue batch as a normal "running" row (one row per issue), and never shows the auto-selecting chip on the post-selection rows.

## Consequences

### Positive

- Single source of truth for RunID and RunDir generation. All four batch kinds share the same `internal/runid` package.
- Collision guard eliminates race conditions when multiple runs start within the same second.
- Shortid-first ordering maximises collision resistance within the same timestamp.
- Pattern-based `KindFromDirName` enables the portal to recover batch kind from directory names alone.
- Validation is loosened to accept the full range of identifiers users are likely to choose.
- Carrying `RunTS` and `RunShortID` on the manifest lets event-log-less consumers (the portal, before `run.started` lands) derive canonical per-row RunIDs without synthesizing ad-hoc formats.
- The `runid.NewRunID` derivation contract pins the canonical call shape so future consumers cannot drift back to synthetic "issue-N" RunIDs.
- The auto-select vs post-selection identity split (issue #1918 slice 2) prevents the portal from showing the auto-selecting chip on the post-selection issue batch rows, and lets downstream consumers resolve the selector's batch identity from the event stream without back-deriving it from the RunID.

### Negative

- Existing RunDir names generated before this ADR are not renamed. Old runs continue to use their original names.
- The `--run-id` flag no longer influences the RunDir name. Scripts that relied on this behavior must be updated.
- Manifests written before this amendment cannot supply `RunTS`/`RunShortID`; consumers must keep the queued-event fallback for old batches.

### Neutral

- The `internal/runid` package is wire-agnostic: it performs no I/O beyond checking for directory existence during collision detection. Callers remain responsible for creating the RunDir after calling `NewBatch()`.
- The collision guard retries up to 16 times, which is sufficient for the 65536-value shortid space. In practice, collisions within the same second are rare.
- The manifest's `omitempty` tags keep the new fields backward-compatible at the wire level: older manifests decode without errors, and newer manifests are still readable by older readers that ignore unknown fields.

## Blocked by

None - can start immediately

## Runtime Context

- Issue: #984
- Parent: #982
- Slice 2 amendment: #1918 (auto-select selector id and post-selection issue batch identity)
