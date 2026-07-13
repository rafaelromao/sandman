# Wayfinder T1 — GitHub rate-limit impact of the lazy `ListIssueComments` probe

> Research asset for [T1 of map #2142](https://github.com/rafaelromao/sandman/issues/2143). Captures the per-batch API cost ceiling for the broadened-spec detector, against GitHub's documented REST primary and search rate limits, so that the rename + detector broadening (T2, T3, T4) ship with a quantified budget rather than a guess.

## TL;DR

| Path | Per-input-issue cost in the broadened-detector world |
|---|---|
| Section-shape present (canonical Specification body) | **0 extra calls** — the cheap path short-circuits before `ListIssueComments`. |
| Section-shape absent, has children | **1–N REST requests**, where `N = ceil(comments_on_issue / 100)`, for one lazy `ListIssueComments` probe. |
| Section-shape absent, no children | 1 REST request (cached after that) — the lazy probe that returns empty. |
| Search fallback (unchanged from today) | 0 or 1 additional `gh issue list --search` request, only when body+comments yielded no candidates. |

The lazy probe fires **only** when section-shape is absent. For typical batch sizes (1–10 typed issues) the worst-case budget stays well inside both authenticated (5,000 req/hr) and even unauthenticated (60 req/hr) primary limits. The unauthenticated case **only** blows the search-endpoint budget (10 req/min) when many input issues simultaneously hit the search fallback — possible but not common.

The broadened detector **does not** introduce a new rate-limit risk that justifies a different design.

## Cost model in detail

### What changes in `internal/batch/spec.go` (T3's "lazy probe")

Today, every input issue is fetched with `client.FetchIssue` and the body is checked against three H2-section regexes (`Problem Statement`, `Solution`, `User Stories`). If matched, the resolver issues **at most one** `ListIssueComments` per typed issue (only as part of `collectCandidates`, line 150 of the current file).

After broadening: the same body check runs first. **If** it fails AND the issue is in fact a parent of children, the resolver needs an extra signal to discover that fact. The chosen signal reuses `ListIssueComments` and asks "does the body of any conversation comment reference another issue number?"

A naive implementation that probes every input would add one call per non-section-shape issue. With `cachedGitHubClient` already memoising `ListIssueComments` per issue number per run, this collapses to:

- **First time the resolver sees issue `#N`** → 1 REST request to list comments (cached).
- **Second time** (e.g. a candidate also being filtered) → 0 requests; served from `c.comments[N]`.

So the extra API cost attributable to the lazy probe is **at most one REST `repos/.../issues/N/comments` call per non-section-shape input issue, per `sandman run` invocation**.

### REST endpoint costs

`internal/github/cli_client.go:472` wraps the call as:

```go
gh api "repos/%s/%s/issues/%d/comments?per_page=100&sort=created&direction=asc" --paginate
```

with `prCommentPageSize = "100"` (`cli_client.go:414`).

**Paginated count per call**: a 100-comment issue → 1 request; a 1,000-comment issue → 10 requests (one per page); a 10,000-comment issue → 100 requests. The worst-case per-issue REST budget scales linearly with comment count.

| Comments on issue | REST requests per probe |
|---|---|
| 0–100 | 1 |
| 101–500 | 2–5 |
| 501–1000 | 6–10 |
| 1001–5000 | 11–50 |
| 5001–10000 | 51–100 |

In practice, GitHub issues used as Specification-shaped specs in this repo carry tens, occasionally hundreds of comments. The lazy probe on a typical Specification likely lands at 1–2 REST requests. The 10k-comment long tail is improbable for this domain (real-world Specifications rarely attract that conversation volume).

### Primary rate limits (REST, doc-of-record)

From [`docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api`](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api):

| Auth tier | Primary REST limit |
|---|---|
| Unauthenticated (per IP) | **60 req/hr** |
| Authenticated PAT (per user) | **5,000 req/hr** |
| GitHub App installation (per installation) | 5,000 req/hr base; scales with repo & user count; cap 12,500 |
| `GITHUB_TOKEN` in CI (per repo) | 1,000 req/hr |

### Search rate limit (unchanged from today)

`/search/issues` carries a separate bucket (verified from [`docs.github.com/en/rest/search/search`](https://docs.github.com/en/rest/search/search)):

| Auth tier | `/search/issues` limit |
|---|---|
| Unauthenticated | **10 req/min** |
| Authenticated | **30 req/min** |
| Semantic / hybrid search | 10 req/min (auth required) |

The existing `SearchIssues` path (`cli_client.go:759`) uses `gh issue list --search … --limit 1000`. `gh` makes one underlying API call. Today, `collectCandidates` only falls back to search when body+comments yielded zero candidates — and, importantly, only for issues that already matched section-shape (because that's the only branch that calls `collectCandidates`). After broadening, the search fallback still applies only to section-shape-matched parents, not to lazy-probed non-section-shape parents. **This is the existing behaviour unchanged.** A future ticket could decide whether the lazy probe should also fall back to search; per T3 charting that decision lives outside T1's scope.

## Worst-case budget over a single `sandman run`

Let `k` = number of typed input issues. Let `m` ≤ k be the number that don't carry canonical section-shape bodies (so the lazy probe fires for each).

**Cost attributable to the broadened detector**, assuming the cache is cold:

```
extra_rest = sum_over_probed_issues_of ceil(comments_on_issue_i / 100)
```

against a primary budget of:

- **Unauthenticated** — 60 req/hr total. If a single run does 30 comment-page fetches in a few minutes, the user has used ~50% of the hour. Two such runs back-to-back burn the bucket. (But unauthenticated use of this CLI is exceptional.)
- **Authenticated** — 5,000 req/hr. A run that probes all 10 typed issues each with 1000 comments = 100 extra requests = 2% of the hour. Comfortably fine.

**Stress scenarios** (none are within reasonable operating territory but enumerated for completeness):

- **100 typed issues, all comment-heavy (~500 comments each) → 500 REST requests in one run.** Within 5,000/hr authenticated budget; saturates the unauthenticated 60/hr bucket.
- **Search-fallback thrash (older issue, before T3 confirmed scoping):** if the lazy probe ever reached `SearchIssues`, the unauthenticated 10 req/min search ceiling means 10 simultaneous lazy-probed parents in the same minute = exactly hit the ceiling, second-minute additional = blocked.

## Conclusion

The broadened detector fits comfortably inside the rate-limit budget under realistic operating parameters. The cache (`cachedGitHubClient`) is doing the heavy lifting: per issue, per run, exactly one cold `ListIssueComments` call regardless of how many re-entries the resolver does.

**Recommendation to T4**: ship the lazy probe as-is, no second-order guardrails needed. The union of section-shape + children is cheap.

**Caveat for future tracking**: the unbounded recursion (T3's "flatten with warnings") does not change this picture. Recursion expands already-discovered children, not new ones — and the cache covers them too. Worst case shifts from "k input issues pay m probes" to "k plus m dot k probes in pathological trees" but is still capped by the cache and by paginated comment counts.

## Primary sources

- [`docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api`](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api) — primary limits table, secondary limits, points table.
- [`docs.github.com/en/rest/search/search`](https://docs.github.com/en/rest/search/search) — `/search/issues` rate limit (10/min unauth, 30/min auth).

## Local artefacts that grounded the model

- `internal/batch/spec.go:55-96` — current `Resolve` flow that the broadened detector sits inside.
- `internal/batch/spec.go:134-166` — `collectCandidates`, the path that already calls `ListIssueComments`.
- `internal/github/cli_client.go:472-519` — implementation of `ListIssueComments` (paginated at 100/page).
- `internal/github/cli_client.go:414` — `prCommentPageSize = "100"`.
- `internal/github/cli_client.go:759-786` — implementation of `SearchIssues` via `gh issue list --search`.
- `internal/cmd/run.go:27-78` — `cachedGitHubClient` wrapping both `FetchIssue` and `ListIssueComments` per run, memoising by issue number — the critical efficiency boundary that caps the broadened-detector cost.
