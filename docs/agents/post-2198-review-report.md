# Post-#2198 Code-Quality & Robustness Review

## 1. Setup

- **Reviewed commit**: `92d0f6ba` (Merge PR #2198 — slice 8 T3 retirement).
- **Branch**: `review/post-2198-merge` at `/home/romao/projects/sandman-review`.
- **PR diff scope**: `internal/batch/{ac_parser,ac_parser_test,oracles,oracles_test,orchestrator,t3_retirement_guards_test,verify,verify_orchestrator_test,verify_sandbox,verify_sandbox_test,verify_test}.go` and `internal/sandbox/worktree.go`.
- **PR character**: code-only retirement of the T3 transitional fallback (and `ReplaySandboxFactory`, `T3EvidenceOracle`, `ParseSandmanEvidence`, `EvidenceLine`) plus a documentation-comments cleanup. Net `-18` lines of production code.

## 2. Test runs

| Suite | Gate / env vars | Result |
|---|---|---|
| Default unit (`go test ./...`) | none | **3001 passed, 0 failed** across 20 packages |
| Smoke (`-tags smoke`) | `SANDMAN_TEST_FAST=1` | **3016 passed, 1 failed, 17 skipped** — single browser-timing flake, passes 3/3 in isolation |
| PR-2198 scope (re-run) | unit, focused on `internal/batch/...` and `internal/sandbox/...` | **684 passed, 0 failed** across 2 packages |
| E2e (`-tags e2e`) | `SANDMAN_E2E_GATES=all SANDMAN_TEST_PROVIDERS=all SANDMAN_TEST_FAST=1` | **1314 passed, 16 failed, 14 skipped** in `internal/cmd` |
| `go vet ./...` | — | clean |
| `gofmt -l .` | — | clean |

### E2e failures

Two distinct categories, **both pre-existing on `92f5240e` (the commit immediately before #2198)** — verified by running the same tests against `92f5240e` and reproducing every failure:

1. **`unexpected gh api path: repos/example/sandbox/issues/{1,150}/comments?per_page=100&sort=created&directi...`** — 14 of the 16 failures. The local `gh` CLI rejects `repos/example/sandbox/...` URLs the test's fake shim does not handle (its switch matches `repos/example/sandbox/issues/1` but the failing cases call `gh api issue comments` which produces a different arg shape than the shim parses). Hits `TestPRFlow_*`, `TestPortal_E2E_AbortStopsOneIssueAndBatchContinues*`, `TestE2E_QueuedIssuesPersistAfterBatchCompletes`, `TestOpencodeSubagentPermissionAllowAll`.
2. **`TestSlice10_PromptOnlyBatchIdentity`** — the `want` short ID is captured from `spyNoID.req.BatchShortID` (set in the runner) while the actual `idx.Batches[0].Path` was written earlier in `cmd/run.go` from `runid.NewBatchID(...)`. The mismatch `d944` vs `a00e` indicates the short IDs are minted at different call sites and never reconciled. Pre-existing bug from slice 10.

Neither is touched by #2198's diff; both fail identically at `92f5240e`.

### Smoke flake

`TestPortalRefresh_ExpandedDetailsTabShowsLoadingCursorWhileDetailFetchPending` is a chromium-driven DOM-timing test with hard-coded `setTimeout(..., 50)` and `setTimeout(..., 700)` deadlines. Passes 3/3 in isolation and was last touched by `10a4b9f4` (May). Pre-existing flake, not in #2198's diff.

## 3. Code-review findings

### Executive summary

- The slice is **surgically clean**: every retired symbol (`T3EvidenceOracle`, `ParseSandmanEvidence`, `EvidenceLine`, `ReplaySandboxFactory`, `T3Oracle`, `VerifyInput.T3`) is fully removed from `internal/batch/` and `internal/sandbox/`. No orphan references.
- All imports trimmed; no dead code. `t3_retirement_guards_test.go` adds a reflection-based regression net that catches future re-introductions of a `T3` field on `VerifyInput`.
- Tests inject at the documented seams (`VerifyPathFunc`, `SandboxFactory`, `&fakeOracle{}`). No deep mocks introduced. Skill-hygiene preserved (zero references to retired symbols in `internal/skill/sandman/`).
- **No blockers or majors.** All findings are documentation polish and one pre-existing structural duplication.

### Findings table

| Sev | File:Line | Category | Description | Suggested fix |
|---|---|---|---|---|
| Minor | `internal/batch/orchestrator.go:2239-2330` | Code-quality (pre-existing) | Verify-then-close block duplicated in both `mergeRequired` and `!mergeRequired` branches. Functionally identical but with subtle gating differences (issue close at 2277-2280 vs 2291-2294). Each T3-comment cleanup left both copies; future maintainers may edit one and miss the other. | Extract helper `applyVerifyThenClose(ctx, o, branch, issue, wt, pr)`. Out of scope for #2198. |
| Minor | `internal/batch/verify_orchestrator_test.go:18-20` | Docs (pre-existing) | Comment "closes the open PR if any (today's `hasBlockingOpenPR` backstop is bypassed)" reads as if the orchestrator closes the PR. It only bypasses the backstop and closes the issue. | Reword: "closes the issue if open; bypasses the `hasBlockingOpenPR` open-PR backstop on verified outcomes". |
| Minor | `internal/batch/verify_sandbox_test.go:24-27` | Test (pre-existing) | `TestT1SandboxFactory_PinsSourceToOriginMain` doc promises "we re-run … with a different source and asserting the same source is used" but the test never re-runs (`_ = wt` no-op). Source branch field unexported; meaningful test needs an unexported accessor. | Either drop the misleading sentence or expose an unexported helper. |
| Minor | `internal/batch/t3_retirement_guards_test.go:8-21` | Test (cosmetic) | `TestRunVerifyPath_NoT3FieldReflectsStructShape` matches `TestRunVerifyPath` prefix but the function under test is the *type* `VerifyInput`. Other tests use `Test<Type>_<Field>` for struct-shape assertions. | Rename to `TestVerifyInput_HasNoT3Field`. Pure rename, zero behaviour risk. |
| Nit | `internal/batch/verify.go:97-101` | Docs | `DefaultVerifyPath` doc lists T1/T2/T4 oracles but the chain literal at 117-118 is `T2 → T4 → T1`; the doc re-orders. | Reorder doc to `(T2 → T4 → T1)` and drop the redundant second listing. |
| Nit | `internal/batch/verify.go:75-77` | Docs | `Oracle` doc says "a nil oracle is treated as OracleAbstain so a test or partial deployment can elide individual oracles", but `RunVerifyPath` chain at 125-132 hard-codes all three slots. Comment overpromises. | Reword to require `&fakeOracle{outcome: OracleAbstain}` to elide. |
| Block | `internal/batch/verify.go` `RunVerifyPath` chain slice (post-#2198) | Correctness | The three-oracle chain is correct, but `t3_retirement_guards_test.go:TestRunVerifyPath_NoT3FieldReflectsStructShape` is the only thing preventing a future `T3` field from silently being added to `VerifyInput` without being wired into the chain. If a contributor adds a `T3` field to the struct and *also* adds it to the chain, the test still passes (because the test only inspects struct fields, not the chain). | Optional: extend the guard test to also assert the chain slice contains exactly `{T1, T2, T4}`. Defensive but the slice 8 PR is small enough to accept this gap. |
| Block | (none) | — | — | — |

### What I specifically checked and found clean

- **No orphan references**: `rg -rn 'T3EvidenceOracle|ReplaySandboxFactory|ParseSandmanEvidence|EvidenceLine|sandman-evidence' internal/` returns zero hits.
- **`internal/batch/oracles.go`** — imports still all used; `truncate`, `defaultT1Runner`, `isSubset` are retained and justified.
- **`internal/batch/ac_parser.go`** — no dead code, no half-deleted blocks.
- **`internal/batch/verify.go`** — `DefaultVerifyPath` correctly wires `T2 / T4 / T1`; `RunVerifyPath` chain reduced to three entries; the `_ = step.orc; continue` early-exit preserved.
- **`internal/batch/verify_sandbox.go`** — `T1SandboxFactory` structurally clean; `SandboxFactory` interface unchanged.
- **`internal/batch/orchestrator.go`** — only doc-comment changes from self-review commit `69c55509`; no logic touched.
- **`internal/batch/orchestrator.go:1995-2007`** — `runVerifyPath` seam preserves the `o.verifyPath != nil` test injection hook with the same `VerifyPathFunc` shape, the DI seam AGENTS.md requires.
- **`internal/sandbox/worktree.go`** — only a one-line comment change; `GitMergeBaseIsAncestor` and `gitMergeBaseIsAncestor` unchanged. No race or cleanup leak touched.
- **Tests inject at the documented seams**: every test in `verify_test.go`, `verify_orchestrator_test.go`, `verify_sandbox_test.go`, `oracles_test.go`, `t3_retirement_guards_test.go` uses `&fakeOracle{...}` or `VerifyPathFunc(...)`. No deep concrete mocks.
- **Skill-hygiene**: `internal/skill/sandman/` and `internal/skill/skill*.go` contain zero references to any retired symbol.
- **Event-sourced reasoning preserved**: no mutable status shortcuts introduced; `events.RunState` untouched.
- **Atomic filesystem semantics preserved**: no `.sandman/` writers touched in this PR.
- **Tests cover what they claim**: traced every test in `verify_test.go` (`AllAbstain`, `T1VerifiedTriggersAutoClose`, `T1FailedReturnsFailed`, `T4DefersToT1`, `T2RejectsSkipsRest`, `RunsOraclesInOrder`) and all six `TestRunSingle_AlreadyResolved_*` cases in `verify_orchestrator_test.go` — each asserts the documented behavior correctly.

### What I was unable to verify

- The reflection guard's resilience to a hypothetical `T3 → T3Oracle` rename: a rename would also break `TestRunVerifyPath_RunsOraclesInOrder` at compile-time, so the guard is defence-in-depth, not the primary safety net.
- The `internal/sandbox/worktree.go` change is a single comment edit (`four-oracle chain` → `verify chain`); I did not exhaustively re-validate worktree recovery logic that is unrelated to the PR diff.

## 4. Recommended fix order (smallest blast radius first)

1. Rename `t3_retirement_guards_test.go` test from `TestRunVerifyPath_NoT3FieldReflectsStructShape` → `TestVerifyInput_HasNoT3Field`. Pure rename.
2. Reorder + dedupe the `verify.go:97` oracle-list doc to `(T2 → T4 → T1)`. Pure docs.
3. Reword the misleading sentence in `verify_sandbox_test.go:24-27`. Pure docs.
4. Reword the `Oracle` doc in `verify.go:75-77` to clarify the abstain-fake pattern. Pure docs.
5. Reword the ambiguous "closes the open PR" sentence in `verify_orchestrator_test.go:18-20`. Pure docs.
6. (Out of scope) De-duplicate the verify-then-close blocks at `orchestrator.go:2243-2282` and `2300-2327`.

## 5. Bottom line

- **PR #2198 is a clean, well-scoped retirement slice.** Nothing in the slice 8 diff introduces a behavioural change or a regression risk.
- **The 16 e2e test failures are pre-existing, environment-related** (`gh api` rejecting the shim's URL shape, and the slice-10 short-ID mismatch). Reproduced verbatim at `92f5240e` (the commit immediately before #2198).
- **The single smoke flake is pre-existing**, a chromium DOM-timing test that passes 3/3 in isolation.
- **Recommended action**: fix items 1-5 in a small follow-up PR for documentation polish. Items 1 (rename) is the only one with any code-shape impact, and it is zero risk.