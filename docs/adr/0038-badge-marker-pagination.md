# ADR-0038: Badge marker — paginated idempotency check

## Status

proposed

## Context

The post-batch badge sidecar (`internal/batch/badge_hook.go`, `MaybeSuggestBadge`) uses a marker-comment idempotency check as its authoritative gate before suggesting a Built with Sandman badge PR. The check calls `gh pr list --state all --limit 1000 --json number,body` once.

On a repository with more than 1000 historical PRs, the marker-bearing badge PR can be truncated off the page, causing the sidecar to re-suggest a duplicate badge. The contract documented in `docs/usage/badge.md` (§ "The marker comment (source of truth)") establishes the marker-comment query as the single source of truth — a truncated scan breaks this contract.

The in-agent idempotency check (`internal/prompt/badge_prompt.md:7`) uses `gh pr list --state all --limit 100` and is kept as defence-in-depth only. It operates on a different surface (the agent's own `gh pr list` call inside its worktree) and is explicitly documented as not the authoritative gate.

## Decision

Replace the one-shot `--limit 1000` call with a paginating scan that walks every page until the marker is found or the API reports no further pages.

### Pagination contract

- `gh pr list --state all --limit 100 --json number,body` is called per page.
- The `Link: <url>; rel="next"` header (written to stderr in combined output) is parsed to extract the `&after=<cursor>` cursor for the next page.
- Pagination stops when: (a) the marker `<!-- sandman-badge-pr -->` is found in any page, (b) the API returns no `rel="next"` link, or (c) an error is raised.
- The scan is silent on success: no per-page log lines are emitted so that the operator-visible stdout from `sandman run` is not polluted with pagination progress. Failures still surface through the existing `Badge PR suggestion skipped: ...` warn-line on the hook writer.

### Marker check stays authoritative

The marker-comment check in `MaybeSuggestBadge` remains the single source of truth for idempotency. The control file at `.sandman/state/.built_with_sandman` remains a performance optimisation subordinate to the marker check.

### In-agent prompt stays as defence-in-depth

The `--limit 100` check in `internal/prompt/badge_prompt.md:7` is unchanged. It protects against a wasted agent run in the rare case the Go-side hook's query was truncated (which this ADR eliminates, but the defence-in-depth is kept for robustness).

### Failure modes

- **API error mid-scan**: `findBadgeMarkerPR` returns a wrapped error naming the page, e.g. `badge marker scan page 3: gh pr list: ...`. `MaybeSuggestBadge` emits a warning and skips the badge suggestion.
- **Network error mid-scan**: Same as API error — `gh` returns a non-zero exit code captured by `runGh`.
- **Mid-scan daemon crash**: No durable state is written until the badge PR is created. A restart resumes from the same `gh` pagination state; no idempotency guarantee is broken.
- **GitHub rate limit**: `gh` surfaces this as an error through `runGh`. The sidecar emits a warning and skips the suggestion; the next batch retries.

## Decision recorded in

- `internal/batch/badge_hook.go` — `findBadgeMarkerPR` helper and `HasBadgePR` delegation
- `docs/usage/badge.md` § "The marker comment (source of truth)" — pagination contract

## Consequences

### Positive

- The marker-comment idempotency check can no longer be truncated by repository size. The documented contract is upheld.
- The hook stays quiet on the success path; only the operator-visible summary line (`Sandman suggested a Built with Sandman badge PR: ...`) or the existing warn-lines appear on the writer.
- The pagination boundary (`--limit 100`) is explicit and auditable.

### Negative

- A repository with very many PRs will take longer to scan on first badge check. The scan is bounded by the marker or by the API's last page.
- Operators lose the in-progress `page N, total_seen M` trace. Mid-scan errors are still surfaced via the existing `Badge PR suggestion skipped: ...` warn-line, which is sufficient for diagnosing the failure.

### Neutral

- The `ghCommander` seam is preserved; no new interface introduced.
- `internal/prompt/badge_prompt.md` is unchanged — no agent behaviour modification.
- The ADR documents the failure modes but does not change the failure handling strategy (warnings + skip, consistent with existing best-effort semantics in `docs/usage/badge.md` § "Failure modes").
