# ADR-0029: Portal secondary-row chips for run context

Issue #973 introduced a small inline status-pill chip (`.badge.kind-chip`) to label `auto-select` and `review` runs. This was a misinterpretation — the chip used status-pill visual language (dot + label, same family as the `● running` / `● reviewing` badge) for a semantic label, and was redundant with the adjacent status badge. The chip should have followed — and this ADR introduces — the same pattern as the `Part of Batch` chip: a full-width muted block in a secondary row, carrying the *subject* of the run rather than the *category*.

## Decision

Three mutually exclusive chips, each rendered in a secondary `tr.context-row` below the run row, using the same muted block style as the existing `Part of Batch` chip (`display: block`, `var(--surface-3)`, `var(--muted)` text, `border-radius: 6px`):

| Run kind | Secondary-row chip text | Data source |
|---|---|---|
| Issue in batch (>1 issues) | `Part of batch: #N, #N` | `portalRun.BatchIssues` *(inline in title cell for now; pending #1055)* |
| Regular issue / prompt-only / continuation | *(no chip)* | — |

(Subsequent change: the auto-select candidates chip was removed — see the slice notes below. The `Candidates` field on `portalRun` remains for downstream consumers; only its portal chip text goes away.)

Chips are mutually exclusive: a row is in exactly one of these categories and shows at most one chip. The auto-select chip is intentionally absent — the active status badge already labels the row kind, and the long list of candidate issue numbers adds noise without informing the user.

The status vocabulary is extended:

- `statusOrDefault` adds: `active && isAutoSelect` → `"auto-select"`
- `statusOrDefault` keeps: `active && isReview` → `"reviewing"`
- Terminal statuses stay `success` / `failure` / `aborted` for all run kinds (no kind-specific terminal statuses).

The `portalRun` struct retains the `Candidates []int` field (`json:"candidates,omitempty"`), populated from `run.started.payload.candidates` in the event log. The field stays on the wire so downstream consumers (e.g. the agent log filter, future external dashboards) can still inspect the candidate set; the portal's secondary-row chip no longer renders it.

The `run.started` event for review runs gains an `issue_number` field in the payload (in addition to the existing `pr_number`). The review daemon resolves the issue from the PR before emitting the event. The portal reads `issue_number` from the payload and populates `portalRun.IssueNumber` and `portalRun.IssueLabel` (as `#N`) for review runs, replacing the current behavior of setting `IssueLabel` to the literal `PR42` and `IssueNumber` to 0.

### Review-only orphan label

When only review child rows exist for an issue (no canonical implementation row), the portal `visibleRunForIssueGroup` orphan fallback renders an explicit `Review of PR <prNumber> (#<issueNumber>)` label in the row's name cell (e.g. `Review of PR 1508 (#1472)`). The PR the review targeted is surfaced first; the linked issue is shown as a parenthesised reference. The row uses the review run's own `run_id` as the visible row identity (`data-run-key`), does not fabricate implementation-run metadata such as `batchKey` or `issueTitle`, and remains expandable via the subject selector. This replaces the earlier behavior of synthesizing a parent-shaped issue row from the newest review source (issue #1489) with a row whose `issueLabel` mirrored the source PR or fell back to a bare `#N`. References issue #1526.

When an orphan review run lacks an `issueNumber` entirely (the issue could not be resolved from the PR, or older event logs without the `issue_number` field), the Go-side projection (`portalRunsView.runFromState` and `runFromActiveMatch`) renders the same shape without the parenthesised issue reference: `Review of PR <prNumber>` (e.g. `Review of PR 1508`). The `reviewOrphanIssueLabel` helper centralises the rule; the exotic fallback (`runID` with neither an issue nor a PR) is preserved. References issue #1667.

The inline `.badge.kind-chip` chip in the title cell (introduced by #973 / PR #1047) is removed entirely.

## Consequences

- The `● review` / `● auto-select` inline chip (PR #1047) is deleted and replaced with the secondary-row pattern.
- The secondary row currently carries one chip type: batch membership. The earlier auto-select candidates chip (label prefix `Auto-select candidates:`) was dropped in a subsequent slice because the long `#N, #N, ...` text overlaps information already conveyed by the active status badge.
- The `statusOrDefault` function gains a single special-case branch for `auto-select`, mirroring the existing `reviewing` branch.
- The `Candidates []int` field is added to `portalRun` — a small, additive JSON contract change (omitted when empty, so no breakage for non-auto-select rows). The field remains on the wire even though the portal no longer renders a chip for it.
- The `run.started` event for review runs gains `issue_number` in the payload — a breaking change to the event contract for review runs only. Older event logs without this field will fall back to the current behavior (`IssueNumber = 0`, `IssueLabel = PR42`).
- Selected items from a successful auto-select run are surfaced in the agent log; the long candidates list is not rendered anywhere in the row chrome.
