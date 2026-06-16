# ADR-0029: Portal secondary-row chips for run context

Issue #973 introduced a small inline status-pill chip (`.badge.kind-chip`) to label `auto-select` and `review` runs. This was a misinterpretation — the chip used status-pill visual language (dot + label, same family as the `● running` / `● reviewing` badge) for a semantic label, and was redundant with the adjacent status badge. The chip should have followed — and this ADR introduces — the same pattern as the `Part of Batch` chip: a full-width muted block in a secondary row, carrying the *subject* of the run rather than the *category*.

## Decision

Three mutually exclusive chips, each rendered in a secondary `tr.context-row` below the run row, using the same muted block style as the existing `Part of Batch` chip (`display: block`, `var(--surface-3)`, `var(--muted)` text, `border-radius: 6px`):

| Run kind | Secondary-row chip text | Data source |
|---|---|---|
| Issue in batch (>1 issues) | `Part of batch: #N, #N` | `portalRun.BatchIssues` *(inline in title cell for now; pending #1055)* |
| Review | `Reviewing PR #N for issue #M` | `portalRun.PRNumber` + issue lookup |
| Auto-select | `Auto-select candidates: #N, #N` | `portalRun.Candidates` (new field) |
| Regular issue / prompt-only / continuation | *(no chip)* | — |

Chips are mutually exclusive: a row is in exactly one of these categories and shows at most one chip. A review run in a batch shows the review chip (`Reviewing PR #N for issue #M`), not the batch chip. An auto-select run in a batch shows the auto-select chip, not the batch chip. This avoids stacking multiple muted blocks in the secondary row and keeps the chip's meaning unambiguous.

The status vocabulary is extended:

- `statusOrDefault` adds: `active && isAutoSelect` → `"auto-selecting"`
- `statusOrDefault` keeps: `active && isReview` → `"reviewing"`
- Terminal statuses stay `success` / `failure` / `aborted` for all run kinds (no kind-specific terminal statuses).

The `portalRun` struct gains a `Candidates []int` field (`json:"candidates,omitempty"`), populated from `run.started.payload.candidates` in the event log.

The `run.started` event for review runs gains an `issue_number` field in the payload (in addition to the existing `pr_number`). The review daemon resolves the issue from the PR before emitting the event. The portal reads `issue_number` from the payload and populates `portalRun.IssueNumber` and `portalRun.IssueLabel` (as `#N`) for review runs, replacing the current behavior of setting `IssueLabel` to the literal `PR42` and `IssueNumber` to 0.

The inline `.badge.kind-chip` chip in the title cell (introduced by #973 / PR #1047) is removed entirely.

## Consequences

- The `● review` / `● auto-select` inline chip (PR #1047) is deleted and replaced with the secondary-row pattern.
- The secondary row now carries three possible chip types: batch membership, review target, and auto-select candidates. Each has a distinct label prefix (`Part of batch:`, `Reviewing PR`, `Auto-select candidates:`).
- The `statusOrDefault` function gains a single special-case branch for `auto-selecting`, mirroring the existing `reviewing` branch.
- The `Candidates []int` field is added to `portalRun` — a small, additive JSON contract change (omitted when empty, so no breakage for non-auto-select rows).
- The `run.started` event for review runs gains `issue_number` in the payload — a breaking change to the event contract for review runs only. Older event logs without this field will fall back to the current behavior (`IssueNumber = 0`, `IssueLabel = PR42`).
- The review chip renders `Reviewing PR #N for issue #M` using `portalRun.PRNumber` and `portalRun.IssueNumber`, both now populated for review runs.
- Selected items from a successful auto-select run are surfaced in the agent log, not in the chip. The chip shows candidates only, keeping the chip's information density stable across active and terminal states.
