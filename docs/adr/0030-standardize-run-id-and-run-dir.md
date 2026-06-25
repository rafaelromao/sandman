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
| Regular issue | `<shortid>-<ts>-issue-<issueNum>` |
| Review (with linked issue) | `<shortid>-<ts>-<linkedIssueNum>-PR<prNum>` |
| Review (without linked issue) | `<shortid>-<ts>-PR<prNum>` |
| Auto-select | `<shortid>-<ts>-auto-<N>` |
| Prompt-only (with user id) | `<shortid>-<ts>-<userid>` |
| Prompt-only (without user id) | `<shortid>-<ts>` |

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
