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

When `NewBatch()` finds that a directory already exists at `.sandman/batches/<shortid>-<ts>-...`, it generates a new shortid and retries up to 16 times before returning an error. The timestamp is not changed during retry; only the shortid advances.

The shortid is placed first to maximise collision resistance within the same timestamp. The timestamp provides chronological ordering.

### RunDir naming templates

| Kind | Template |
|------|----------|
| Regular issue (single) | `<shortid>-<ts>-<issue>` (no +N suffix) |
| Regular issue (multi)  | `<shortid>-<ts>-<firstIssueNum>+<additionalCount>` |
| Review | `<shortid>-<ts>-PR<prNum>` |
| Auto-select | `<shortid>-<ts>-auto-<N>` |
| Prompt-only (with user id) | `<shortid>-<ts>-<userid>` |
| Prompt-only (without user id) | `<shortid>-<ts>` |

`<N>` is the count (number of issues in a batch, number of candidates in auto-select). `firstIssueNum` is the first issue number for regular issues.

Per issue #1917 (slice 1 of #1916), the issue batch template uses **additional count** (not total count):

- Single issue (`n==1`): `<sid>-<ts>-<num>` (no `+N` suffix).
- Two issues (`n==2`): `<sid>-<ts>-<firstIssue>+1`.
- Nine issues (`n==9`): `<sid>-<ts>-<firstIssue>+8`.

The omitted suffix on single-issue batches keeps the public BatchId from carrying redundant information; `+<additionalCount>` makes the suffix meaningful (it counts the issues *beyond* the first). The per-row RunID still uses `<sid>-<ts>-<issueNum>` (no suffix), so the per-row identity is unchanged.

The public BatchId (== batch folder basename) MUST agree with `batch.json.batchId`, `run.json.BatchID`, and the event payload `batch_id` field. The portal Batch label and Details tab render the public BatchId.

### Per-row RunID templates

Each AgentRun within a batch receives a unique RunID built from the batch's `<shortid>-<ts>` prefix plus a per-row subject:

| Kind | Template |
|------|----------|
| Regular issue | `<shortid>-<ts>-<issueNum>` |
| Review (with linked issue) | `<shortid>-<ts>-<linkedIssueNum>-PR<prNum>` |
| Review (without linked issue) | `<shortid>-<ts>-PR<prNum>` |
| Auto-select | `<shortid>-<ts>-auto-<N>` |
| Prompt-only (with user id) | `<shortid>-<ts>-prompt-<userid>` |
| Prompt-only (without user id) | `<shortid>-<ts>-prompt` |

### `--run-id` flag preserved

The `--run-id` flag on `sandman run` is retained for prompt-only runs where the user may want a self-chosen identifier. However, it no longer influences the RunDir name. The RunDir is always generated fresh via `NewBatchID`. This keeps the naming scheme uniform while preserving the flag for its original purpose: giving prompt-only runs a human-meaningful identity in the portal.

### User-supplied run ID validation

When `--run-id` is provided for a prompt-only run, it is validated by `IsValidUserRunID`:

- Length: 1 to 64 characters
- Allowed characters: alphanumerics, hyphens, underscores
- No leading-letter requirement (unlike the previous strict rule)

### Portal recovery via `KindFromDirName`

The portal uses `KindFromDirName(name string) (Kind, bool)` to recover the batch kind from a RunDir name during same-second collision recovery. It matches by pattern:

- `*-auto-*` → `KindAutoSelect`
- `*-PR*` → `KindReview`
- `*+*` → `KindIssue` (batch dirs only; `+N` suffix)
- `*-issues-first-*` → `KindIssue` (old format backwards compat)
- `*-review-*` → `KindReview` (old format backwards compat)
- `*-auto-select-*-candidates` → `KindAutoSelect` (old format backwards compat)
- `*-prompt-only*` → `KindPromptOnly` (old format backwards compat)
- New format with no other marker → `KindPromptOnly` (fallback)

### Manifest carries `RunTS` and `RunShortID`

The `BatchManifest` persisted at `<batchDir>/batch.json` carries the batch's `<shortid>` and `<ts>` primitives as top-level fields, alongside the existing `BatchId`:

- `RunTS` — the timestamp from "Timestamp + shortid collision guard" above, repeated here for self-containment. Format: `time.Now().Format("060102150405")` (12 chars, local time, 2-digit year).
- `RunShortID` — the collision guard from "Timestamp + shortid collision guard" above, repeated here for self-containment. Format: `%04x` of `unixNano % 0xFFFF` (4 lowercase hex chars).
- `BatchId` — the full assembled batch identifier (`<shortid>-<ts>-<kindSuffix>`), retained for backward compatibility.

The manifest's JSON schema is therefore:

```json
{
  "batchId": "<shortid>-<ts>-<kindSuffix>",
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

When either manifest field is absent, callers should fall back to the queued event's `RunID` if one is available, or return the empty string. The portal implements both paths through `perRowRunIDForManifest(runTS, runShortID, prNumber, issueNumber, queued)` (pure helper) and its active-batch wrapper `perRowRunIDForActive(active, issueNumber, queued)`.

## Consequences

### Positive

- Single source of truth for RunID and RunDir generation. All four batch kinds share the same `internal/runid` package.
- Collision guard eliminates race conditions when multiple runs start within the same second.
- Shortid-first ordering maximises collision resistance within the same timestamp.
- Pattern-based `KindFromDirName` enables the portal to recover batch kind from directory names alone.
- Validation is loosened to accept the full range of identifiers users are likely to choose.
- Carrying `RunTS` and `RunShortID` on the manifest lets event-log-less consumers (the portal, before `run.started` lands) derive canonical per-row RunIDs without synthesizing ad-hoc formats.
- The `runid.NewRunID` derivation contract pins the canonical call shape so future consumers cannot drift back to synthetic "issue-N" RunIDs.

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
