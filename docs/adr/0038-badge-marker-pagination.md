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
- Per-page log line: `badge marker scan: page N, total_seen M` is emitted to standard error so operators can see how deep the scan went.

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
- Operators can observe how deep the scan went via the per-page log line.
- The pagination boundary (`--limit 100`) is explicit and auditable.

### Negative

- A repository with very many PRs will take longer to scan on first badge check. The scan is bounded by the marker or by the API's last page.
- The per-page `os.Stderr` logging is coarse — no structured logging. This is consistent with the existing `fmt.Fprintf(h.writer, ...)` pattern in the hook.

### Neutral

- The `ghCommander` seam is preserved; no new interface introduced.
- `internal/prompt/badge_prompt.md` is unchanged — no agent behaviour modification.
- The ADR documents the failure modes but does not change the failure handling strategy (warnings + skip, consistent with existing best-effort semantics in `docs/usage/badge.md` § "Failure modes").
