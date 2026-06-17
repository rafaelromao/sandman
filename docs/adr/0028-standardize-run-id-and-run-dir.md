# ADR-0028: Standardize Run ID and Run Dir naming

## Status

accepted

## Context

Sandman generates two related but distinct identifiers for every batch run:

- **RunID**: a string that uniquely identifies one AgentRun within a batch. Persisted in the `RunID` field of every event in `events.jsonl` and used as the row key in the portal.
- **RunDir**: a per-batch directory under `.sandman/runs/` that contains the control socket, command server, broadcaster, and `batch.json` manifest for the daemon process.

Historically, these identifiers were generated ad-hoc in multiple places with inconsistent naming schemes. The `--run-id` flag was used as the RunDir name for prompt-only runs, but not for issue-driven runs. This made it difficult to reason about run identity across the portal, the daemon, and the event log.

Four batch kinds exist: regular issue, review, auto-select, and prompt-only. Each has different structural properties (e.g., review runs link to a PR, auto-select runs have a candidate count, prompt-only runs may or may not carry a user-supplied run id). The naming scheme must accommodate all four while remaining predictable and collision-resistant.

## Decision

### Timestamp + shortid collision guard

Every RunID and RunDir begins with a `<ts>-<shortid>` prefix:

- `<ts>` is `time.Now().Format("20060102-150405")` (local time, 15 characters).
- `<shortid>` is 4 lowercase hex characters derived from `unixNano % 0xFFFF`.

When `NewBatch()` finds that a directory already exists at `.sandman/runs/<ts>-<shortid>-...`, it generates a new shortid and retries up to 16 times before returning an error. The timestamp is not changed during retry; only the shortid advances.

The timestamp is placed first so that filesystem tools that sort by name naturally chronological-order runs. The shortid provides collision resistance within the same second.

### RunDir naming templates

| Kind | Template |
|------|----------|
| Regular issue | `<ts>-<shortid>-<N>-issues-first-<firstIssueNum>` |
| Review | `<ts>-<shortid>-review-<firstSubject>` where `firstSubject` is the PR number prefixed `PR` |
| Auto-select | `<ts>-<shortid>-auto-select-<N>-candidates` |
| Prompt-only (with user id) | `<ts>-<shortid>-prompt-only-ID-<userid>` |
| Prompt-only (without user id) | `<ts>-<shortid>-prompt-only` |

`<N>` is the count (number of issues in a batch, number of candidates in auto-select, 1 for review, 1 for prompt-only). `firstSubject` is the first issue number for regular issues, the PR number for reviews, and the user-supplied run id for prompt-only runs.

### Per-row RunID templates

Each AgentRun within a batch receives a unique RunID built from the batch's `<ts>-<shortid>` prefix plus a per-row subject:

| Kind | Template |
|------|----------|
| Regular issue | `<ts>-<shortid>-issue-<issueNum>` |
| Review (with linked issue) | `<ts>-<shortid>-issue-<linkedIssueNum>-review-<prNum>` |
| Review (without linked issue) | `<ts>-<shortid>-review-<prNum>` |
| Auto-select | `<ts>-<shortid>-auto-select-<count>c` |
| Prompt-only (with user id) | `<ts>-<shortid>-prompt-<userid>` |
| Prompt-only (without user id) | `<ts>-<shortid>-prompt` |

### `--run-id` flag preserved

The `--run-id` flag on `sandman run` is retained for prompt-only runs where the user may want a self-chosen identifier. However, it no longer influences the RunDir name. The RunDir is always generated fresh via `NewBatchID`. This keeps the naming scheme uniform while preserving the flag for its original purpose: giving prompt-only runs a human-meaningful identity in the portal.

### User-supplied run ID validation

When `--run-id` is provided for a prompt-only run, it is validated by `IsValidUserRunID`:

- Length: 1 to 64 characters
- Allowed characters: alphanumerics, hyphens, underscores
- No leading-letter requirement (unlike the previous strict rule)

### Portal recovery via `KindFromDirName`

The portal uses `KindFromDirName(name string) (Kind, bool)` to recover the batch kind from a RunDir name during same-second collision recovery. It matches by pattern:

- `*-issues-first-*` → `KindIssue`
- `*-review-*` → `KindReview`
- `*-auto-select-*-candidates` → `KindAutoSelect`
- `*-prompt-only*` → `KindPromptOnly`

## Consequences

### Positive

- Single source of truth for RunID and RunDir generation. All four batch kinds share the same `internal/runid` package.
- Collision guard eliminates race conditions when multiple runs start within the same second.
- Timestamp-first ordering means filesystem browsing tools naturally show runs in chronological order.
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
