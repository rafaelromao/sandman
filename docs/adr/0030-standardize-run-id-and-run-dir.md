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
| Regular issue | `<shortid>-<ts>-<firstIssueNum>+<N>` |
| Review | `<shortid>-<ts>-PR<prNum>` |
| Auto-select | `<shortid>-<ts>-auto-<N>` |
| Prompt-only (with user id) | `<shortid>-<ts>-<userid>` |
| Prompt-only (without user id) | `<shortid>-<ts>` |

`<N>` is the count (number of issues in a batch, number of candidates in auto-select). `firstIssueNum` is the first issue number for regular issues. For issue batches, `+<N>` means "primary issue + N more issues" (e.g., `42+2` = 3 issues total).

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

The `BatchManifest` persisted at `<batchDir>/batch.json` (see `internal/daemon/runfs.go`) carries the batch's `<shortid>` and `<ts>` primitives as top-level fields, alongside the existing `BatchId`:

- `RunTS` — the `time.Now().Format("060102150405")` timestamp (12 chars, local time, 2-digit year) that the orchestrator generated when it called `NewBatch` at session start. See "Timestamp + shortid collision guard" above for the primitive definition.
- `RunShortID` — the 4 lowercase hex characters (`%04x` of `unixNano % 0xFFFF`) that acted as the same-second collision guard during `NewBatch`. See "Timestamp + shortid collision guard" above for the primitive definition.
- `BatchId` — the full assembled batch identifier (`<shortid>-<ts>-<kindSuffix>`), retained for backward compatibility.

Every manifest creation site (`internal/cmd/run.go` for issue-driven runs, `internal/cmd/review.go` for one-shot review, `internal/cmd/selection.go` for auto-select, and `internal/review/daemon.go` for the review daemon) populates both fields. The JSON tags use `runTs` and `runShortId` with `omitempty`, so manifests written before this amendment decode as zero values and remain readable.

### Per-row RunID derivation contract

Event-log-less consumers (most prominently the portal's nil-state path, before `run.started` fires in `events.jsonl`) MUST derive per-row RunIDs from manifest fields via `runid.NewRunID`. Synthesizing ad-hoc formats such as `<shortid>-<issue>-<N>` or any string containing the literal word `"issue"` is forbidden, because such strings violate this ADR's per-row RunID templates and break the portal's row-key contract.

The canonical call shape for a regular issue batch is:

```go
runid.NewRunID(runid.KindIssue, fmt.Sprintf("%d", issueNum), manifest.RunTS, manifest.RunShortID)
```

The four arguments are positional and named here for clarity: `KindIssue` selects the issue template, `fmt.Sprintf("%d", issueNum)` is the per-row subject (`<issueNum>`, never `"issue-<issueNum>"`), `manifest.RunTS` is the `<ts>` primitive from the manifest, and `manifest.RunShortID` is the `<shortid>` primitive.

The portal implements this contract through two helpers in `internal/cmd/portal_runs_view.go`:

- `perRowRunIDForManifest(runTS, runShortID, prNumber, issueNumber, queued *events.Event)` — the pure derivation helper. When `(runTS, runShortID)` are both non-empty it returns `runid.NewRunID(runid.KindReview, …)` for PR runs (with or without a linked issue) and `runid.NewRunID(runid.KindIssue, …)` for regular issue batches. When either manifest field is empty it falls back to `queued.RunID` if a queued event is available, or returns the empty string.
- `perRowRunIDForActive(active portalActiveRun, issueNumber int, queued *events.Event)` — the active-batch wrapper that pulls `(RunTS, RunShortID, PRNumber)` from a `portalActiveRun` snapshot and delegates to `perRowRunIDForManifest`. This is the entry point used by `runFromActiveBatchIssue`.

Any future consumer that needs per-row RunIDs without consulting the event log (for example, a new CLI command that lists a live batch before any `run.started` event lands) MUST route through these helpers or call `runid.NewRunID` with the same `(KindIssue, fmt.Sprintf("%d", issueNum), manifest.RunTS, manifest.RunShortID)` shape. Other derivations are out of contract.

## Consequences

### Positive

- Single source of truth for RunID and RunDir generation. All four batch kinds share the same `internal/runid` package.
- Collision guard eliminates race conditions when multiple runs start within the same second.
- Shortid-first ordering maximises collision resistance within the same timestamp.
- Pattern-based `KindFromDirName` enables the portal to recover batch kind from directory names alone.
- Validation is loosened to accept the full range of identifiers users are likely to choose.

### Negative

- Existing RunDir names generated before this ADR are not renamed. Old runs continue to use their original names.
- The `--run-id` flag no longer influences the RunDir name. Scripts that relied on this behavior must be updated.

### Neutral

- The `internal/runid` package is wire-agnostic: it performs no I/O beyond checking for directory existence during collision detection. Callers remain responsible for creating the RunDir after calling `NewBatch()`.
- The collision guard retries up to 16 times, which is sufficient for the 65536-value shortid space. In practice, collisions within the same second are rare.

## Blocked by

None - can start immediately

## Runtime Context

- Issue: #984
- Parent: #982
