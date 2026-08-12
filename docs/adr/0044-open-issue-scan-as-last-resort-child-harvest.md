# ADR-0044: Open-issue scan as last-resort Specification child harvest

## Status

proposed

## Context

Issue 2476 / ADR-0042 generalised the parent-section matcher so that candidates whose `## Parent` / `## Parent area` / `## Parent spec` heading cites the spec are accepted. The generalization lives entirely on the **verifier** side. The **harvest** side — what gets fed into the verifier in the first place — still depends on a cheap traversal of the spec body, its comments, the native sub-issue edge, and a mention-search fallback gated on `len(order) == 0`.

A spike (`spike/upstream-spec-shape`, worktree at `/home/romao/projects/sandman-spike-upstream-spec-shape/`) prototyped the gap: a spec whose body is silent (no `## Children`, no bullet references), no `## Parent` in the candidates' bodies, but candidates that **do** cite the spec in their `## Parent` section is the threeterm-#58 pattern in disguise, and the current resolver misses it. The prototype found that the `## Parent` signal is strong (1.00 precision, 1.00 recall on every fixture where it applies) when it actually gets fed to the verifier — it just doesn't, because the harvest phase never looks for it.

The upstream mattpocock `to-tickets` convention produces every ticket with `## Parent` already wired, but operators regularly migrate, hand-write, or import tickets from external trackers where the spec body is silent and the relationship only lives in the child side. The spike's keyword-only and umbrella-style fixtures reproduce exactly this case.

The cost of one full-repo `gh api repos/<owner>/<repo>/issues?state=open --paginate` is bounded by repo size and paid only when the cheaper sources are silent — same gating as the existing search fallback. The REST endpoint returns both issues and pull requests, so a client-side `pull_request`-field filter keeps the scan issue-only.

## Decision

1. **New harvest step: open-issue scan.** When `collectCandidates` (internal/batch/spec.go) has produced zero candidates after body, comments, native sub-issues, and the mention-search fallback, sandman lists every open issue in the current repo and keeps the ones whose body carries a `## Parent`-style H2 section (broadened per ADR-0042) that cites the spec. The scan reuses `HasParentSectionBacklinkTo` (the widened matcher), so every harvested candidate passes the existing verifier by construction.
2. **Persistence via auto-posted comment.** For every spec where the scan produced candidates, sandman posts a single comment on the spec with the body:
   ```
   <!-- sandman-discovered-children -->

   ## Discovered children

   - #<n1>
   - #<n2>
   ```
    The hidden HTML marker identifies the comment as auto-generated. The `## Discovered children` heading plus `- #N` bullets is recognised by the existing structured comment harvest (`ParseChildrenFromBody`) on subsequent runs, so the operator gains a persistent record and the expensive scan only runs once per spec per missing-marker state.
3. **Idempotency.** Before posting, sandman lists the spec's existing comments. If any carries the `<!-- sandman-discovered-children -->` marker, sandman skips posting. Operators who want to force a re-scan delete the marker comment; sandman then re-runs the harvest and re-posts on the next call.
4. **Optional interfaces for additive growth.** The two new GitHub operations are exposed as optional interfaces (`github.OpenIssueLister`, `github.IssueCommentPoster`) on `internal/github/github.go`. The resolver type-asserts `r.client.(github.OpenIssueLister)` and `r.client.(github.IssueCommentPoster)`; production `CLIClient` implements both, existing test fakes do not. This means the new step is a no-op on every existing fake — no test changes are required. The hardening guarantee: existing tests stay unchanged because they pre-date the optional interface.
5. **Implementation seams.**
   - `internal/github/github.go` gains the `OpenIssueLister` and `IssueCommentPoster` interfaces.
   - `internal/github/cli_client.go` implements both via `gh api repos/<owner>/<repo>/issues?state=open&per_page=100 --paginate` (streaming JSON decoder, client-side `pull_request` filter) and `gh issue comment <n> --body <body>`. `gh issue list --paginate` is intentionally NOT used because `--paginate` is a `gh api` flag, not a `gh issue list` flag.
   - `internal/batch/spec.go` gains `discoverChildrenViaOpenIssueScan`, `postDiscoveredChildrenComment`, `markerCommentExists`, and `buildDiscoveredChildrenComment`. `collectCandidates` extends with a new step gated by `len(order) == 0` (same gate as the existing search fallback).
   - `internal/batch/spec_discovery_test.go` is a new test file with eight unit tests pinning the behaviour; the existing `spec_test.go` is not modified.
   - `internal/github/cli_client_test.go` adds `TestCLIClient_ListOpenIssues_Success` / `_MultiPage` / `_Error` pinning the constructed `gh api ... --paginate` invocation, the PR filter, the multi-page decoder, and the error path.
6. **Test plan (all in the new file).**
   - `TestOpenIssueScan_FiresWhenCheaperSourcesEmpty` — body, comments, sub-issues, and search are silent; the scan fires, finds candidates, and posts the marker comment exactly once.
   - `TestOpenIssueScan_SkippedWhenCheaperSourcesReturnCandidates` — body declares a child; the scan does not fire.
   - `TestOpenIssueScan_FilterUsesBroadenedParentMatcher` — `## Parent area` and `## parent` (case-insensitive) are accepted; `### Parent` (H3) is rejected.
   - `TestOpenIssueScan_FilterAcceptsMultiRefParentSection` — a candidate citing both the intermediate parent area and the umbrella spec is accepted when the spec is among the references.
   - `TestOpenIssueScan_ExcludesSpecItself` — even when the spec appears in the open-issue list (it is open), it is not harvested as its own child.
   - `TestOpenIssueScan_IdempotentMarkerComment` — pre-existing marker comment blocks re-posting.
   - `TestOpenIssueScan_PostFailureDoesNotAbortResolver` — failed `PostIssueComment` is logged but does not abort the resolver; candidates are still accepted this run.
   - `TestOpenIssueScan_NoOpWhenClientLacksOptionalInterfaces` — fakes that satisfy only `Client` (no `OpenIssueLister` / `IssueCommentPoster`) skip the new step, confirming the additive contract that keeps existing tests unchanged.
   - `TestBuildDiscoveredChildrenComment` — pins the comment-body format (hidden marker + `## Discovered children` H2 + sorted `- #N` bullets).
7. **Failure mode.** A comment-post failure logs a warning via `r.warningWriter` and the resolver continues. Candidates harvested in memory are still accepted this run; the next run re-scans and re-attempts the post.
8. **CLI surface unchanged.** No new flags. No new config keys. The strategy is always on; it only fires when cheaper sources are silent.

## Consequences

### Positive

- Closes the spike's harvest-side gap: candidates whose `## Parent` section cites the spec are recovered even when the spec body, its comments, native sub-issues, and the mention-search fallback are all silent.
- The auto-posted comment gives operators a persistent, reviewable record of what sandman auto-discovered. They can edit the marker comment to remove false positives, add missing candidates, or delete the marker to force a re-scan.
- Future runs automatically pick up the marker comment via the existing comment-harvest path (`ListIssueComments` + `ParseChildrenFromBody`), so the expensive scan only fires when the marker is absent — the comment is the cache.
- The widened `HasParentSectionBacklinkTo` matcher (ADR-0042) is reused as the filter, so the spike's `## Parent area` umbrella pattern (threeterm #232 / #234) is covered for free without a second matcher.
- Existing tests in `internal/batch/spec_test.go`, `internal/batch/orchestrator_test.go`, `internal/batch/dependencies_test.go`, `internal/batch/badge_e2e_test.go`, and `internal/cmd/run_test.go` are unchanged because the new operations live on optional interfaces — the type-assertion falls through, and the new step is a no-op on fakes that do not implement the optional interfaces.

### Negative

- One full-repo open-issue scan is more expensive than the existing search fallback, especially on repos with thousands of open issues. Gating by `len(order) == 0` keeps it off the hot path; a future change may need a per-batch cap or opt-out flag if any real repo hits the budget.
- The auto-posted comment is a side effect on the spec issue. Operators who run sandman repeatedly without deleting the marker will never see a re-scan, even after opening new child issues that reference the spec. The trade-off favours persistence over freshness; the operator's "delete the marker" escape hatch is documented in the marker comment itself.
- The strategy relies on every child carrying a `## Parent` H2 section that cites the spec. Children written without that backlink (only body keywords, only sub-issue edges, only range references) are not recovered by the scan. The first category is handled by the existing body-keyword + sub-issue harvests; the range-reference gap surfaced in the spike remains open as a separate problem.
- A comment-post failure leaves the candidates harvested in memory accepted this run but unpersisted; the next run will re-scan and re-attempt. Operators who care about durability should run sandman twice or fix the underlying post failure.

### Neutral

- `internal/github/cli_client.go` gains `ListOpenIssues` and `PostIssueComment` alongside the existing `ListSubIssues` and `ListIssueComments`. The pattern of one `gh` call per logical operation is unchanged.
- The discoverer's comment-post failure path mirrors the existing search-fallback failure path (`fmt.Fprintf(r.warningWriter, ...)` and continue).
- ADR-0021 step 2 (child discovery) and ADR-0034 (empty-child carve-out) are unchanged at the high level; this ADR adds a single additional harvest step between them.
