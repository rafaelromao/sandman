# Wayfinder T4 — Cheap gate: reviewDecision + mergeStateStatus + statusCheckRollup

> Research asset for [T4 of map #2151](https://github.com/rafaelromao/sandman/issues/2151). Captures the API-side pre-check that runs after T2 and before T1: a one-REST call to GitHub that defers to T1 only when the PR is already in a mergeable, approved state.

## TL;DR

T4 reads `reviewDecision`, `mergeStateStatus`, and `statusCheckRollup` off the PR snapshot the orchestrator already has. When all three are positive (`APPROVED` + `CLEAN` + all-green), T4 returns `OracleDeferT1` — the chain continues to T1, which makes the final call. On any other state (no review, dirty checks, dirty merge, `CHANGES_REQUESTED`), T4 abstains. `CHANGES_REQUESTED` is a hard abstain because `sandman-pr-review` Hard Rule 8 owns that path.

## Implementation shape

- **PR struct extension**: `internal/github/github.go::PR` now carries `ReviewDecision`, `MergeStateStatus`, `StatusCheckRollup` (slice 1).
- **FindPRByBranch**: `internal/github/cli_client.go::FindPRByBranch` fetches the three new columns in its single `gh pr list --json ...` invocation. No additional REST calls.
- **Gate**: `internal/batch/oracles.go::T4CheapGate`. Pure over the PR snapshot; no shell-out, no second REST. Returns `OracleDeferT1` or `OracleAbstain`.

## Runtime cost

~150ms per run (one extra REST call, already in the existing `gh pr list` invocation). No new REST endpoints.

## Why T4 is a "defer to T1" rather than "verify"

T4 cannot prove the issue is already on main — the PR being approved + clean + green means a human has accepted the change, but it does not mean the change has reached main. T4's job is to gate T1: when GitHub already says the PR is in a mergeable state, the chain is "warmer" and T1's verifier is more likely to find the test plan on `origin/main` HEAD. T4 abstains (does not defer) when the PR is in any other state, including dirty — the issue is still in flight and T1 should not run on a non-canonical HEAD.
