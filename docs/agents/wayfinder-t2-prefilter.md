# Wayfinder T2 — Pre-filter: DiffSubset + L1 predicate wiring

> Research asset for [T2 of map #2151](https://github.com/rafaelromao/sandman/issues/2151). Captures the L1 + DiffSubset pre-filter that gates entry to the more expensive oracles.

## TL;DR

T2 is the cheap oracle that runs *first*. It combines a `git merge-base --is-ancestor B M` L1 check with a `DiffSubset` comparison on the (file, hunk) level. When L1 is true (the head is a descendant of main), the oracle abstains — the change is on main, no further verification needed. When L1 is false, the DiffSubset compares the branch's diff against main's diff: if the branch's set is a subset of main's, the oracle abstains (the change is part of main); otherwise it rejects (the branch has lines that are not in main, so no oracle can prove the issue is already on main).

## Implementation shape

- **L1 predicate**: `internal/sandbox/worktree.go::gitMergeBaseIsAncestor` (existing). T2 uses the public wrapper `internal/sandbox/worktree.go::GitMergeBaseIsAncestor`.
- **Diff helper**: `internal/sandbox/diff_subset.go::DiffSubset(repoDir, a, b string) (DiffSet, error)`. Pure helper; no I/O outside `git diff`.
- **Pre-filter**: `internal/batch/oracles.go::T2PreFilter`. Wires the L1 predicate + DiffSubset; returns `OracleAbstain` (L1 true or L1 false + subset) or `OracleReject` (L1 false + not a subset). Errors are treated as abstain so transient git failures do not block the run.

## Runtime cost

Sub-second, zero REST. The L1 check is one `git merge-base --is-ancestor`; the L2 fallback is one `git diff` round-trip in each direction.

## Why reject (not fail) on a divergent diff

T2 cannot prove the issue is already on main if the branch's diff is not a subset of main's. T2's job is to gate entry, not to produce a signal; rejecting the branch is equivalent to abstaining (the next oracles still run) but it records *why* we abstained so the operator sees the diff divergence in the run log.
