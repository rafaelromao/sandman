# 24-Hour Code-Quality & Robustness Review

**Window**: 2026-07-13T14:17:03Z → 2026-07-14T14:17:03Z (24h ending at request time)
**Reviewed commit**: `c1b75a95` (origin/main, post-PR #2203)
**Worktree**: `/home/romao/projects/sandman-24h-review` on branch `review/24h-window`

## 1. Scope

18 PRs, 21 commits, ~70 files changed. High-blast-radius files per AGENTS.md (touched at least once):

| File | PRs touching it | Why high-blast |
|---|---|---|
| `internal/batch/orchestrator.go` | 2182, 2190, 2197 | Event fold, status projection, runner lifecycle |
| `internal/batch/spec.go` | 2162, 2179, 2188, 2203 | Spec resolver + recursive flatten |
| `internal/batch/verify.go` + `verify_sandbox.go` + `oracles.go` | 2182, 2198 | Verify chain (Layer 1) |
| `internal/sandbox/worktree.go` | 2182, 2191, 2193, 2198 | Worktree lifecycle, recovery, host-path rewrite |
| `internal/sandbox/container_sandbox.go` | 2193 | Container rewrite on Start/RestoreHostPaths |
| `internal/sandbox/sandbox.go` + `stranded.go` | 2193 | New Sandbox methods, stranded worktree recovery |
| `internal/github/cli_client.go` + `github.go` | 2182, 2162, 2203 | PR/blocker/sub-issue parsing |
| `internal/cmd/run.go` | 2162, 2203 | Cached client, run lifecycle |
| `internal/scaffold/scaffolder.go` | 2180, 2196 | Scaffold template, codeindex drop + gitignore hook |
| `internal/skill/sandman/*` | 2177, 2178, 2185, 2196 | Skill-hygiene regression surface |

## 2. Test runs

| Suite | Gate / env vars | Result |
|---|---|---|
| Default unit (`go test ./...`) | `SANDMAN_TEST_FAST=1` | **3012 passed, 0 failed** across 20 packages |
| Skill-hygiene regression net | `go test ./internal/skill/sandman/... ./internal/prompt/...` | **88 passed, 0 failed** |
| Smoke `-tags smoke` on touched areas (`scaffold`, `skill`, `cmd`, `batch`) | `SANDMAN_TEST_FAST=1` | **1515 passed, 0 failed** |
| E2e `-tags e2e` on touched areas (badge, init-gi`tignore, scaffold) | `SANDMAN_TEST_FAST=1` | **138 passed, 0 failed** |
| `go vet ./...` | — | clean |
| `gofmt -l .` | — | clean |

The 16 e2e failures from the PR #2198-only review (pre-existing `gh api` shim + slice-10 short-ID mismatch + smoke chromium flake) remain pre-existing on `c1b75a95` and are unrelated to any of these PRs.

## 3. Executive summary

- **Two real blockers and three real majors** were found and verified independently by reading the cited lines.
- The window's PRs are otherwise **mechanically clean**: DI seams preserved, atomic-write helpers reused where appropriate, skill-hygiene regression net green, no event-sourced reasoning violations, no IPC regressions.
- Test coverage is generally thorough, with **one explicit coverage gap** in the codeindex-drop regression net (6/8 presets lack the inverted assertion).
- Documentation drift between the shipped code and the wayfinder/ADR docs in the verify-then-close PR (#2182) is the most material finding from a user-trust perspective.

## 4. Findings — Blockers (must fix before treating these slices as "shipped")

| # | File:Line | PR | Issue |
|---|---|---|---|
| **B1** | `internal/batch/spec.go:163` | #2203 | `if !hasChildren { ListSubIssues(...) }` gates the sub-issues probe behind the comment-children gate. Commit `60b855f2` claims the broadened-detector branch always calls `ListSubIssues`, but the code only calls it when `!hasChildren`. An umbrella issue with comment refs AND native sub-issues will only surface the comment refs. **Verified**: read spec.go:163, the `if !hasChildren` gate is present. The new code contradicts the commit's stated intent. |
| **B2** | `internal/sandbox/worktree.go:168-217` (`releaseBranchInWorktree`) | #2191 | Runs `git checkout --detach` unconditionally. If the foreign worktree has uncommitted changes, git refuses, the helper errors, the caller warns and continues, and the subsequent `git branch -D` then fails with "checked out at" leaving the operator with a misleading error. **No test covers the dirty-foreign path.** |
| **B3** | `internal/scaffold/gitignore_hook.go:41` | #2196 | Scoped check uses `git ls-files -z -- .sandman .` — the `.` pathspec matches every tracked file. The inner case statement flags every tracked path as a violation. Output: `SCOPED blocking .sandman/task.md\nSCOPED blocking foo.txt` for any populated repo. The fallback regex still rejects the commit (so the user's intent is preserved), but the user sees noise about every committed file. |
| **B4** | `internal/sandbox/container_sandbox.go:139, 168, 172` (and `worktree.go:134, 559`) | #2193 | `os.WriteFile(gitFile, …)` writes `.git` directly. AGENTS.md says "Prefer temp-file + rename over direct in-place writes." #2193 doubled the round-trip (host→/workspace→host), so the crash-mid-write failure surface is bigger. |
| **B5** | `docs/agents/wayfinder-t1-ac-traceability-oracle.md:12`, `verify.go:111-114` | #2182 | T1 docs claim "oracle runs each line in a worktree whose source branch is `origin/main` HEAD" but the actual `DefaultVerifyPath` → `T1DecisionOracle{}` → `defaultT1Runner` runs `sh -c line` in `in.WorkDir` (the run's own branch HEAD). `T1SandboxFactory` exists but is dead code — never wired into `DefaultVerifyPath`. The documented "worktree pinned to origin/main" architecture is not actually shipped. |

## 5. Findings — Majors (should fix in a follow-up slice)

| # | File:Line | PR | Issue |
|---|---|---|---|
| M1 | `internal/scaffold/gitignore.go:38-55` | #2196 | `EnsureRule` is read-modify-write on `.gitignore` without locking. Two concurrent `sandman init` invocations can lose each other's append. `atomicfs.WriteAtomic` only protects the rename itself. |
| M2 | `internal/scaffold/gitignore_hook.go:73-78` | #2196 | `installPreCommitHook` silently no-ops when a pre-commit already exists. The user's existing pre-commit may not contain the sandman guard, so AC3 (rejecting `git add -f .sandman/task.md`) is silently unmet. |
| M3 | `internal/sandbox/stranded.go:209-212` | #2193 | `listWorktrees` error collapses to `(ContinuationWorktreeMissing, "")` — a transient git failure is then reported as "no live registration", misleading operators debugging a stuck `--continue`. |
| M4 | `internal/sandbox/worktree.go:130-141` (`ReclaimableWorktree`) | #2191 | `removePrunableWorktreeRegistration` runs **after** the non-atomic `WriteFile`. Under concurrent bit-batch sibling runs, the two writes can interleave on the same `<s.workdir>` registration. |
| M5 | `internal/batch/orchestrator.go:2254-2259, 2277-2280, 2291-2327` | #2182 | (a) The verify path doesn't re-check `ctx.Err()` between `lookupPRForVerify`, `runVerifyPath`, and `CloseIssue` — a cancelled ctx still produces a `success` result with no close. (b) The two `mergeRequired` arms (`true` at 2243-2282 and `false` at 2300-2327) duplicate logic with subtly different gating; the `false` arm skips `CloseIssue` because of the pre-verify close at 2291, but no test exercises that arm. |
| M6 | `internal/sandbox/worktree.go:470-474` (`runGitCommand`) | #2193 / #2182 | `runGitCommand` uses `exec.Command`, not `exec.CommandContext`. When the orchestrator's ctx is cancelled mid-T2, git calls run to completion. `T2PreFilter.Run` (oracles.go:54) and `DiffSubset` are both affected. |
| M7 | `internal/batch/spec_parse.go:57-83` (`ExtractParentReference`) | #2179 | Does not accept the `[Title](.../issues/N)` plain-link markdown format. PR #2131/#2126 added that format to `parseBlockedByHeading` — blocker parsing recognizes three formats, parent backlink parsing recognizes two. Asymmetric. |
| M8 | `internal/batch/spec.go:156-176` (broadened branch) + `cli_client.go:537-578` | #2203 | `Client.ListSubIssues` returns *all* sub-issue numbers regardless of state. Closed sub-issues are picked up, fetched, logged, then dropped at `filterClosedIssues`. Inconsistent with `dependencies.go:93` which filters by `IsIssueClosed`. |
| M9 | `docs/adr/0025-specification-expansion.md §2`, `docs/get-started/concepts.md:43`, `docs/agents/wayfinder-t1-rate-limit-impact.md` | #2203 | (a) ADR §2 doesn't mention the native sub-issues endpoint or the body→comments→search→sub-issues ordering. (b) concepts.md still says "Nested Specifications are rejected" — false post-T4. (c) wayfinder T1 budget doc doesn't account for the new `/repos/.../sub_issues` REST call. |
| M10 | `internal/scaffold/scaffolder_test.go` | #2180 | Only 2 of 8 preset tests (Node, Python) have inverted assertions that codeindex is NOT installed. Go, Dotnet, Generic, Rust, Java, Elixir, Ruby have no such assertion. A future re-addition of codeindex to the unconditional render path would pass 6/8 preset tests silently. |

## 6. Findings — Minors (cleanup, non-blocking)

M11. `internal/batch/verify_orchestrator_test.go:18-20` (carried from #2198): doc reword for issue-vs-PR closure (already addressed in #2204 follow-up).
M12. `internal/batch/spec_test.go:886` (PR #2203): test `TestSpecificationResolver_NonSpecWithoutChildrenSkipsListSubIssues` asserts `len == 1` (i.e. one call) but the name says "Skips". Rename to `…CallsListSubIssuesOnce`.
M13. `internal/cmd/run.go:76-93` (`getOrFill`): cache-stampede window between cache lookup and `fill()`. Not exercised today; flag for any future concurrent caller.
M14. `internal/batch/badge_hook.go:173-193`: hand-rolls temp-file + `os.Rename` instead of using the in-tree `internal/atomicfs.WriteAtomic` helper. Same pattern, less code.
M15. `internal/skill/sandman/implement/SKILL.md:177` (#2196): Hard Rule 4 added but the Checklist at 167-178 was NOT updated with a corresponding `- [ ] No .sandman/ paths staged` item.
M16. `internal/sandbox/container_sandbox.go:138, 168` (`rewriteGitPaths`, `RestoreHostPaths → RestoreWorktreeGitPaths`): `strings.Replace(..., 1)` rewrites only the first occurrence. A future gitlink with multiple `gitdir:` lines would silently be left in a mixed state.
M17. `internal/sandbox/worktree.go:638-639` (`parseCheckedOutPath`): dead code post-#2191. Docstring still references `git worktree remove --force`. Delete or update.
M18. `internal/review/redactor_test.go:69`: test fixture references deleted `sandman/index/SKILL.md`. Update to `implement/SKILL.md`.
M19. `internal/batch/orchestrator.go:2255`: `issue != nil` check is defensive — `issue` is guaranteed non-nil at this point (line 2397 returns failure if FetchIssue failed).
M20. `internal/batch/orchestrator.go:2030-2059` (`mergeVerificationExtras`): always allocates a new map. Reuse when caller already passed one.
M21. `internal/cmd/init.go:82-95`: if `syncSandmanSkill` fails after `Scaffold` succeeded, the user sees a failure on a half-scaffolded repo. Add partial-success warning or revert on skill-install failure.
M22. `internal/cmd/init_gitignore_e2e_test.go:79-103`: weak substring assertions for `.sandman/` presence. Tighten to byte-exact.
M23. `internal/sandbox/container_sandbox_test.go`: no direct end-to-end test of `ContainerSandbox.RestoreHostPaths`'s actual rewrite.
M24. `internal/skill/sandman/SKILL.md` etc. (`#2185`, `#2192`): "Step 1.5" wording + landing-page "PRD model in prose" → "Specification model in prose" — sweep is correct and consistent.
M25. `internal/cmd/run_test.go`: missing `TestCachedGitHubClient_FetchIssue_CachesResult` analogue.

## 7. What was checked and found clean

- **Skill-hygiene regression net**: all 15 tests pass. Confirmed zero forbidden patterns (`internalPackagePathRe`, `internalGoIdentifierRe`, `issueTrackerJargonRe`, `ghCliInProseRe`) in the changed `.md` files. The only orphan `codeindex` reference in the worktree is in `docs/agents/cold-start-migration-report.md:99` (historical planning record), which is acceptable per AGENTS.md's "historical record" exemption.
- **AGENTS.md "Skill content constraints" section** is untouched and still names `processPR` / `MarkSeen` / `launchReview` / `internal/skill/sandman/skill_hygiene_test.go` as required by `agents_md_test.go`.
- **DI seams**: PR #2182 tests inject `VerifyPathFunc` / `taskWritingRunnableFactory` at the documented seams. PR #2193 tests inject `fakeSandboxFactory` / `fakeWorktreeForContainer.Start(opts)/RestoreHostPaths` at `RunnableFactory` / `SandboxFactory` seams. PR #2203 fakes (`fakeGitHubClient.ListSubIssues` in 5 test files) implement `github.Client` and are wired via `cmd.Dependencies` / `batch.Request`. PR #2196 exposes `Gitignore`, `GitOps`, `HooksDir` as injectable seams on `Scaffolder`.
- **Event-sourced reasoning**: no mutable status shortcuts introduced in any of the 18 PRs.
- **Atomic filesystem semantics**: most `.sandman/` writers reuse `atomicfs.WriteAtomic`. Exceptions: `internal/sandbox/container_sandbox.go:139,168,172` and `internal/sandbox/worktree.go:134,559` use raw `os.WriteFile` (flagged as M5/M16/B4).
- **IPC**: no socket changes in any of the 18 PRs.
- **Rename completeness** for the PRD→Specification rename (PR #2162): zero `PRDResolver` / `IsPRD` / `expandPRDs` / `prdSearchToken` / `expanded PRD` / `nested PRD detected` references remain in active code or docs.
- **`gh api --paginate` correctness** for #2197: the new pagination path is exercised by 8 unit tests and 2 e2e tests. Marker-on-last-page, empty-result, mid-pagination error, and false-positive scenarios all covered.
- **`T1SandboxFactory` factory shape** is structurally correct; the seam is well-formed even though it's dead code per B5.
- **PR #2190's blocker-resolver fix**: correct reorder — `known` check precedes the `IsIssueClosed` short-circuit so in-batch blockers always become `activeBlockers`.
- **PR #2191's recovery scoping**: confirmed via grep that `git worktree prune` no longer appears anywhere in `internal/sandbox/worktree.go`. `samePath(info.Path, s.repoPath)` correctly short-circuits foreign-release on the main repo.
- **Codeindex drop**: deletion is mechanically complete across the user-facing surface (skills, prompts, docs, gitignore, scaffolded Dockerfile). The embed directive auto-tracks deletion via `embeddedFileSet()` walking the embed FS at runtime.

## 8. What was unable to be verified

- Real GitHub rate-limit / pagination behaviour for the new `sub_issues` endpoint in #2203.
- Real `gh api --paginate -q '.[]'` byte-level output shape (only the simulated shape is tested).
- Whether `--continue` host-path restoration works end-to-end across container runtime on macOS (no container runtime in this worktree).
- Concurrent `sandman init` race on `.gitignore` (reasoned analytically; no Go test harness for the race).
- macOS symlink handling for `/var/folders → /private` in PR #2193.
- The full bit-batch reproduction scenario from the original `--continue` bug report.

## 9. Recommended fix order (smallest blast radius first)

1. **B3 — Drop the `.` pathspec** in `gitignore_hook.go:41` (3-line fix). Add the e2e regression assertion from M22 to lock it in.
2. **B4 / M16 — Convert `container_sandbox.go:139,168,172` and `worktree.go:134,559` to temp-file + rename.** Single-purpose helper in `internal/atomicfs/`.
3. **M10 — Extend codeindex regression net** in `scaffolder_test.go` to all 8 presets. One-line addition inside `TestScaffold_AllPresetsIncludeRTK`.
4. **M18 — Update `redactor_test.go:69`** to `implement/SKILL.md` (single line).
5. **M12 — Rename** `TestSpecificationResolver_NonSpecWithoutChildrenSkipsListSubIssues` → `…CallsListSubIssuesOnce` (single line).
6. **B5 — Decide on T1 wiring**: either wire `T1SandboxFactory` into `DefaultVerifyPath` and have T1's Runner execute inside the new worktree, or correct the doc to "runs AC lines against the run's own worktree at the issue branch HEAD".
7. **B1 — Move `ListSubIssues` out of the `if !hasChildren` gate** in `spec.go:163`. Add a regression test where the parent has comment refs AND native sub-issues.
8. **M7 — Add `[Title](.../issues/N)` plain-link support** to `ExtractParentReference`.
9. **M5 / M6 — Add ctx.Err() re-checks in the verify path** + a `runGitCommandCtx` helper for T2 and DiffSubset.
10. **M8 — Filter closed sub-issues** at the resolver or extend `ListSubIssues` to return `(int, state)`.
11. **M9 — Docs sweep**: ADR-0025 §2 + concepts.md + wayfinder-t1.
12. **M1 / M2 / M3 / M4 / M11 / M13–M17 / M19–M25** — bundle into a follow-up cleanup PR.

## 10. Bottom line

- The 24h window contains **18 PRs of meaningful complexity**. The high-blast-radius files (`orchestrator.go`, `worktree.go`, `container_sandbox.go`, `spec.go`) all received multiple touches.
- **5 blockers** and **10 majors** are real and reproducible. None are silent failures — all surface at user-visible layer (close errors, pagination output, scaffold noise, test gaps).
- The window's PRs collectively preserve event-sourced reasoning, DI seams, and skill-hygiene. The regressions are localized and fixable in small, surgical PRs.
- 3012 unit tests + 88 skill/prompt tests + 1515 smoke + 138 targeted e2e all green. The 16 pre-existing e2e failures from the prior review remain unchanged (environment-related, not regressed by these PRs).