# Wayfinder T5 — Layer 1 disposition: verify-then-close path for alreadyResolved runs

> ADR-candidate for [T5 of map #2151](https://github.com/rafaelromao/sandman/issues/2151). Captures the final disposition of the verify-then-close architecture: four oracles in series, conservative backstop last, T3 sunset on cold-start migration.

## TL;DR

Replace the conservative Layer-1 guard in `internal/batch/orchestrator.go`'s `alreadyResolved` arm with a four-oracle verify-then-close path:

```
alreadyResolved + open PR:
  T2 (pre-filter)   → abstain | reject
  T4 (cheap gate)   → abstain | defer to T1
  T1 (decision)     → Verified | Failed | No signal
  T3 (transitional) → Verified | Failed | No signal
  conservative guard (backstop) — today's behaviour
```

Only T1's `Verified` aggregate triggers auto-close of the orphan PR plus the issue. The conservative guard remains as the backstop for any "no signal" case; its trigger changes from first to last.

## Decision

### Auto-close comment template

When T1 returns `Verified` and the orchestrator closes the orphan PR + issue, the auto-close comment is:

```
Closed by sandman — issue already completed.
```

This wording is fixed (matches the existing `CloseIssue` call). It is not customised by the verify path; T1's `Verified` outcome means the same thing as the existing `alreadyResolved` short-circuit. The terminal `run.finished` event surfaces the new `verification: {outcome, checks: [...]}` payload separately so operators can see *which* oracle produced the signal.

### Architecture summary

- **T1 (decision oracle)**: structured `## Acceptance criteria` section in the issue body, each `- [ ]` line a `go test -run` shell line. Verifier reuses `sandbox.NewWorktreeSandbox` via a dedicated factory. Three-state aggregate. 1–3 min/run, zero REST.
- **T2 (pre-filter)**: predicate L1 (`git merge-base --is-ancestor B M`) gates entry; on L1-false, `DiffSubset` compares `(file, hunk)` SHAs against `origin/main`. Sub-second, zero REST.
- **T4 (cheap gate)**: `reviewDecision == APPROVED` + `statusCheckRollup` all-green + `mergeStateStatus == CLEAN`. Abstains on `CHANGES_REQUESTED` (pr-review Hard Rule 8 owns that). ~150ms/run, one REST.
- **T3 (transitional fallback)**: fenced ` ```sandman-evidence ` block with `ok: <cmd> -> <sentinel>` lines. Strict subset of T1 at higher authoring cost; sunset when cold-start migration lands. ~5s/run when evidence is present.
- **Conservative guard (backstop)**: today's behaviour — flip to `failure` with `blocker: 'open-pr-blocks-already-resolved'`. Trigger moves from first to last.

### Why four oracles, not one

A single decision oracle (T1) would force every run to either run tests (expensive) or abstain. The four-oracle chain lets cheap signals short-circuit:

- **T2** (sub-second) rejects branches whose diff is not a subset of `origin/main` — if your branch has lines that aren't in main, no oracle can prove the issue is already on main. ~0ms cost.
- **T4** (~150ms) defers to T1 only when GitHub already says APPROVED + CLEAN + green checks. Otherwise abstains and lets T1 run anyway.
- **T1** (1–3 min) is the only oracle that produces `Verified` — it parses `## Acceptance criteria` and runs each line as `go test -run` in a worktree pinned to `origin/main` HEAD.
- **T3** (~5s when present) is the transitional fallback for issues whose ACs aren't yet T1-mappable. It runs only when T1 abstained.

The conservative guard remains the backstop for any "no signal" case, preserving the existing `blocker: 'open-pr-blocks-already-resolved'` payload verbatim.

## Implementation

| Slice | Title | Issue | Status |
|---|---|---|---|
| 1 | PR struct extension | [#2170](https://github.com/rafaelromao/sandman/issues/2170) | Shipped in this PR |
| 2 | T2 pre-filter: DiffSubset + L1 wiring | [#2169](https://github.com/rafaelromao/sandman/issues/2169) | Shipped in this PR |
| 3 | T1 decision oracle: AC parser + verifier | [#2171](https://github.com/rafaelromao/sandman/issues/2171) | Shipped in this PR |
| 4 | T3 transitional fallback: evidence-block + replay | [#2172](https://github.com/rafaelromao/sandman/issues/2172) | Shipped in this PR |
| 5 | Refactor alreadyResolved arm to layering | [#2173](https://github.com/rafaelromao/sandman/issues/2173) | Shipped in this PR |
| 6 | Step 1.5 wording + task template update | [#2174](https://github.com/rafaelromao/sandman/issues/2174) | Already aligned (no change needed) |
| 7 | Test suite: all five paths + #1684 smoke | [#2175](https://github.com/rafaelromao/sandman/issues/2175) | Shipped in this PR |
| 8 | Cold-start migration — T3 sunset trigger | [#2176](https://github.com/rafaelromao/sandman/issues/2176) | Open (this PR is the parent spec) |

The parent PR ships slices 1–5 + 7. Slice 6's wording was already aligned with T5's grill decision (no ticket or oracle names in user-facing vocabulary). Slice 8 remains open and is the *sole* trigger for T3's sunset.

## Out of scope

- Renaming the `blocker` payload string (preserved verbatim per the original #1684 contract).
- Changing `sandman-pr-review`'s DIRTY handling (#1685).
- Replacing `gh pr list --head <branch> --state open` plumbing with GraphQL.
- Retroactive application of the new oracle to historical alreadyResolved runs.
- Subsuming `merge_conflict: true` under T2's diff equivalence.

## Rollback

`git revert <slice-5-sha>` (or the parent PR's squash commit) plus the existing #1684 test suite (`go test ./internal/batch/ -run TestRunSingle_AlreadyResolved`) all remain green. The conservative backstop is restored to first position; auto-close stops working.
