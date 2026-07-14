# ADR-0038: Badge marker — paginated idempotency check (supersedes the proposed REST `Link`-header pagination contract)

## Status

accepted — supersedes the original proposed wording in this file's
prior revision, which documented a REST `Link: …; rel="next"`
pagination contract that was never actually implemented against the
real `gh` CLI. The current implementation (issue #2195) uses `gh api
--paginate` against the REST pulls endpoint instead, and the prior
proposal is preserved below as a `Superseded notes` section so the
historical context remains in the repo.

## Context

The post-batch badge sidecar (`internal/batch/badge_hook.go`,
`MaybeSuggestBadge`) is the single source of truth for the "Built
with Sandman" badge idempotency. The marker comment
`<!-- sandman-badge-pr -->` lives in the body of the badge PR and
applies in any PR state — open, closed, merged. Finding that marker
across the entire PR history of the repo is non-negotiable: missing
the marker triggers a duplicate badge PR, which is rejected by the
operator-visible post-batch hook since the spawn would fire again on
every subsequent batch.

### Why the prior pagination contract failed

The hand-rolled REST `Link: …; rel="next"` parser that the original
revision of this ADR proposed was correct in theory but never worked
against the `gh` CLI in practice, because `gh pr list --json …`
routes through GitHub's GraphQL API and produces **no** `Link: …;
rel="next"` header. The cursor lives in `pageInfo { hasNextPage,
endCursor }` inside the GraphQL response body instead. The
production code looked for a header that `gh` never emits, so the
cursor loop terminated after page 1; on any repo with >100 PRs the
marker scan returned `false` (issue #2195 repro: 2,180 PRs in
`rafaelromao/sandman`, marker PRs at #1617 and #1913 both outside
the first 100 returned).

## Decision

Use `gh api --paginate` against the REST pulls endpoint and walk the
stream sequentially. The REST endpoint's `Link` header pagination is
honoured by `gh` itself — the Go code never sees a cursor.

### Wire shape

- The lister calls
  `gh api --paginate -q '.[]' repos/{owner}/{repo}/pulls?state=all&per_page=100`
  through the existing `ghCommander` seam.
- The endpoint is passed as a **single positional** argument after
  `--paginate` and `-q '.[]'` — `gh` rejects multiple positionals
  with `accepts 1 arg(s), received 2`, so the earlier `"/repos/…"`
  prefix form fails outright. The `gh api` host (api.github.com) is
  implicit.
- The `-q '.[]'` flattens the per-page `[…]` response into one PR
  object per line of stdout. Without it `gh` emits
  `[…][…][…]` (concatenated JSON arrays) which the `json.Decoder`
  rejects on the first page. The Go code streams the JSON-Lines
  payload through `json.Decoder` and returns `true` on the first
  body that matches `<!-- sandman-badge-pr -->`; otherwise `false`
  at EOF.
- `gh api --paginate` does not accept `--limit` — page size is fixed
  by `&per_page=100` in the query string, which matches the original
  ADR's per-page boundary.

### Failure modes

- **`gh` exits non-zero**: `runGh` returns the wrapped error and the
  hook short-circuits silently. Next batch retries harmlessly.
- **Truncated JSON**: `json.Decoder.Decode` returns a non-EOF error
  that is wrapped with `badge marker scan: decode prs:` and the hook
  short-circuits silently.
- **Network / rate-limit error mid-pagination**: raised by `gh`
  before the stream is consumed; same silent short-circuit.
- **Concurrent operator / mid-batch crash**: no durable state is
  written until the badge PR is created; the next batch resumes from
  the same scan and either finds the marker on a fresh run or
  re-spawns the sidecar.

### Why the control file replaces the "opt-in" fast path

The control file at `.sandman/state/.built_with_sandman` was
originally a perf optimisation subordinate to the marker-comment
check. After issue #2195 it became the **primary API gate**: when
the file is present on disk, neither the marker scan nor the spawn
runs. The sidecar writes the file synchronously after `RunPrompt`
returns a non-empty PR URL, so subsequent batches short-circuit at
the file check on every invocation without making any API calls.

### Silent on every path

The hook writes nothing to the operator-visible stream under any
condition. The success summary line
`Sandman suggested a Built with Sandman badge PR: …` that prior
revisions of this hook emitted on the success path is removed; the
hook is fire-and-forget. The user's invariant is "be silent even in
case of errors", which is satisfied by removing the per-failure
`Badge PR suggestion skipped: …` lines too. Operator visibility for
badge sidecar outcomes lives in `.sandman/events.jsonl` and the
child batch's own `run.log`, not the parent batch's writer.

### In-agent prompt alignment

The defense-in-depth `gh pr list --state all --limit 100` check in
`internal/prompt/badge_prompt.md` is replaced with the same
`gh api --paginate …` shape the Go-side hook uses, so the agent's
own pre-flight scan walks every page too. The agent no longer
authors the control file — the sidecar is the sole authority on its
lifecycle.

## Decision recorded in

- `internal/batch/badge_hook.go` — `HasBadgePR` using `gh api --paginate`,
  `defaultBadgeControlFileWriter` for the synchronous write, the
  silent hook contract.
- `internal/prompt/badge_prompt.md` — defense-in-depth check aligned
  with the new `gh api --paginate` pagination contract.
- `docs/usage/badge.md` § "The control file" — file is the API gate,
  not a perf optimisation.
- `internal/batch/badge_hook_test.go`,
  `internal/batch/badge_e2e_test.go`,
  `internal/cmd/badge_e2e_test.go` — pinned behaviour.

## Consequences

### Positive

- The marker-comment idempotency check traverses every PR regardless
  of repo size, restoring the documented contract that the prior
  GraphQL path silently broke.
- The hook is silent on every path; the operator-facing writer is
  only touched by the parent batch itself, never by the sidecar.
- The control file makes the "already proposed" decision O(1) on
  disk for every batch in this checkout after the first successful
  spawn.
- The pagination contract is implemented entirely in `gh` rather
  than hand-rolled — there is no marker-parser bug surface left.

### Negative

- A first badge check on a large repo takes longer (we no longer
  rely on a hard `--limit` cap to bound latency). The cost is paid
  at most once per checkout; subsequent batches short-circuit on the
  control file.
- Operators lose per-page visibility into the scan. The only signal
  is the eventual success/failure of the sidecar as recorded in
  events.jsonl and the child batch's run.log.
- The hook no longer surfaces API errors to the operator. Recovery
  is now: wait for the next batch (which retries), or `gh` audit.

### Neutral

- The `ghCommander` seam is preserved; no new interface was needed
  for the wire-shape change.
- The `BadgeControlFileWriter` interface was added so the synchronous
  write is testable in isolation. The existing
  `BadgeControlFileReader` is unchanged.

## Superseded notes (kept for historical context)

The previous revision of this ADR proposed hand-rolled cursor
parsing against the `Link: …; rel="next"` header emitted by `gh pr
list`. That proposal was correct for the REST transport but did not
match `gh pr list --json`'s actual GraphQL transport, which emits
no such header. The implementation that resulted from that proposal
stopped paginating after page 1 in production (`issue #2195`), so
the proposal is replaced by the `gh api --paginate` decision above.
