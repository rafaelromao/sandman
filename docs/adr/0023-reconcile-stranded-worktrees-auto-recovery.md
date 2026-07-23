# ADR-0023: Auto-recover from stranded worktrees in `--override` and `--continue` flows

## Status

accepted

## Context

A *stranded worktree* is, per the `CONTEXT.md` *Stranded worktree* glossary term, "a sandman-managed git worktree whose HEAD points to a different branch than its directory name expects". The glossary entry currently claims that `sandman run --override reconciles them automatically` and points at `scripts/reconcile-stranded-worktrees.sh` as a standalone detection tool. That claim is only partially true.

`sandman run --override` already cleans the worktree subdirectory under `.sandman/worktrees/` and prunes the git worktree registration, so a stranded directory that lives entirely under `.sandman/worktrees/` is recovered the next time the orchestrator starts. It does not, however, handle the case where the *main repo* itself is checked out on a `sandman/N-…` branch. When that happens, `git worktree add` fails with the "branch used by worktree at …" error and `--override` does not unblock the run. The operator has to remember to `git checkout main` (or whichever base branch applies) in the main repo before re-running.

The glossary promise is therefore wrong for the most common stranded state. Issues #934–#938 close that gap: #934 teaches `--continue` to auto-promote a no-prior-run request to `ModeOverride`, #935 improves the operator-facing error messages, #936 extracts the stranded-worktree detection logic from `scripts/reconcile-stranded-worktrees.sh` into a Go helper (`StrandedWorktree`) so both the CLI and the portal can call it, and #937–#938 wire the helper into `WorktreeSandbox.Start` and `ClearIssueArtifacts` so the main-repo branch mismatch is repaired before the worktree is recreated.

This ADR records the decision to make that auto-repair the default behaviour. It is the contract that the slices in #934–#938 collectively implement. It is filed as `proposed` so the slices can be revised against the contract, not the other way around.

The auto-recovery pattern is not new. ADR-0019 (`docs/adr/0021-portal-auto-runs-clean-stale-on-startup.md`) established that the portal auto-runs `sandman clean --stale` on startup through a package-level function variable seam (`portalStaleCleaner func(string) error`) so the body is shared with the CLI flag and tests can stub the side effect. ~~ADR-0023 (deleted)~~ reuses the same idea for the stranded-worktree case: the detection logic lives in one Go helper, the recovery loop lives in one place per call site, and tests swap the side effect through a function variable seam rather than reaching into the git CLI.

`--continue` is the second call site. ~~ADR-0022 (deleted)~~ (`docs/adr/0022-replace-end-of-session-continuation-with-checkpointed-handoffs.md`) defined `--continue` as "Handoff is the persisted state; Continue is the action that reads it", with the implicit assumption that a prior AgentRun exists for the issue. Slice #934 lifts that assumption: when there is no prior run, `--continue` now falls through to `ModeOverride`, which means it will go through the same auto-recovery path as an explicit `--override`. The new behaviour, in other words, makes the "no prior run" branch of `--continue` indistinguishable from a fresh `--override` — including the stranded-worktree recovery that both flows now perform.

`scripts/reconcile-stranded-worktrees.sh` is the third artefact. After slice #936 lands, the script becomes a thin wrapper around the same Go helper that `--override` and `--continue` use. Operators can still run the script to *detect* stranded worktrees manually; they no longer need to copy-paste the printed remediation commands, because the orchestrator will run them automatically the next time they invoke `--override` or `--continue`.

## Decision

Auto-recovery from stranded and prunable worktrees is on by default in the two flows that can create a worktree: `sandman run --override` and `sandman run --continue` (after the #934 auto-promotion falls through). Both flows call the same Go helper before they call `git worktree add`, and the helper performs detection and recovery in a single pass. Recovery includes prunable-reattach (strategy 0): when a worktree registration exists but the `.git` gitlink points to a non-existent directory, the registration is pruned and the existing directory is re-registered without deleting its contents.

The two new CLI flags share a single boolean (`reconcileStranded`) with a sensible default:

- `--reconcile-stranded` — set the boolean to `true`. This is the default; the flag is provided for symmetry with `--no-reconcile-stranded` and for documentation purposes.
- `--no-reconcile-stranded` — set the boolean to `false`. Operators opt out of the auto-repair entirely; the flow surfaces the same "branch used by worktree at …" error it surfaced before this ADR landed.

`StrandedWorktree` (the Go helper introduced in #936) is the canonical detection source. Both `--override` and `--continue` call it; `scripts/reconcile-stranded-worktrees.sh` is reduced to a wrapper that shells out to the same helper. Detection returns a structured value that the recovery loop can iterate over without re-parsing git output.

The recovery loop tries four strategies, in order, and stops as soon as one succeeds:

0. **Reclaim prunable worktree.** If a worktree registration exists at `.sandman/worktrees/<issue>` but the `.git` gitlink points to a non-existent directory (git marks it prunable), the helper detects the stale registration via `ReclaimableWorktree`, runs `git worktree prune` to remove the stale entry, and re-registers the existing directory with `git worktree add <dir> <branch>` (no `-b`). This is the cheapest recovery and runs before the branch-delete strategies. It preserves any uncommitted work in the directory.

1. **Delete the branch from the stranded worktree's cwd.** If the worktree at `.sandman/worktrees/<issue>` is checked out on the wrong branch but that branch has no commits ahead of the base, the helper `git branch -D`s the wrong branch from inside the worktree directory. This is the cheapest fix and is sufficient when the stranded branch is genuinely unreferenced.
2. **Detect a foreign live worktree holder at a non-canonical path.** When the parent itself does not hold the branch but a sibling sandbox run's worktree at a non-canonical path does, the helper detaches HEAD in that foreign worktree via `git -C <foreignPath> checkout --detach`. The foreign worktree's directory, `.git` gitlink, and `.git/worktrees/<dir>` registration all stay intact (issue #2187 contract); only the foreign HEAD becomes detached. This step runs BEFORE the parent ref-drop so the parent never touches a branch a foreign worktree holds.
3. **Detach HEAD in the main repo and delete the branch.** If the main repo itself is checked out on `sandman/N-…`, the helper runs `git -C <repoRoot> checkout --detach` followed by `git -C <repoRoot> branch -D <branch>`. The detach leaves the working tree contents unchanged at the same commit (so the operator's editor / uncommitted changes are preserved); `git branch -D` re-checks worktree holders atomically with the ref-drop — closing the TOCTOU window where a sibling worktree could check out the branch between the parent's detach and a raw `git update-ref -d`. The branch reference is re-created later in the run by the canonical `git worktree add -b <branch>` call, so the effective loss is bounded to a transient detached-HEAD window in the parent repo.
   This is the most common case after a `Ctrl+C` mid-run on the main repo. It supersedes the previous "force-checkout base branch" strategy (issue: the operator's working copy was being silently switched to a branch they did not choose).
4. **Refuse to detach and drop the ref.** If none of the strategies above applies (parent HEAD is on a different branch AND no foreign worktree holds the branch, OR parent HEAD is already detached, OR the parent is not the verified holder), the recovery loop refuses to detach the parent and refuses to drop the ref. This guards against a foreign-worktree-holder race that would silently detach the parent HEAD and leave a foreign worktree's symbolic HEAD dangling against a deleted ref. The orchestrator surfaces the failure to the operator instead of forcing recovery.

All four strategies are local to the worktree base and the main repo. They never touch refs that the operator has not touched in this session, and they never force-push. The ref-drop in strategy (3) is bounded to a single Start() invocation; the branch is re-created immediately after by the canonical `git worktree add -b <branch>` call downstream.

The package-level function variable seam that ADR-0019 introduced is replicated here. The recovery loop calls a package-level `reconcileStrandedFn func(repoRoot, worktreeBase string) error` (or the equivalent closure captured by the orchestrator) so tests can stub the side effect without spawning a real git repo. The default closure calls the real helper; tests inject a stub that records the call and returns success.

## Consequences

### Positive

- Operators no longer need to remember to `git checkout main` (or whichever base branch applies) in the main repo before re-running `sandman run --override`. The most common stranded-worktree case is repaired automatically, and the run proceeds.
- The same auto-recovery now fires on `sandman run --continue` for issues that have no prior run, because #934 promotes those requests to `ModeOverride`. Operators who relied on the "no prior run" error to catch typos in issue numbers will need to use `--no-reconcile-stranded` plus a deliberate issue-number check, but the common case of "I forgot which branch I was on" no longer needs a manual fix.
- `StrandedWorktree` is the single detection source. The bash script, the CLI, and the portal all call the same Go helper, so detection rules cannot drift between surfaces.
- `--no-reconcile-stranded` is the documented escape hatch. Operators who want the old "fail loudly" behaviour can opt out per invocation without having to revert a global setting.
- The parent's working-tree commit is preserved across the recovery. The detach-and-ref-drop strategy leaves the operator's files, editor state, and uncommitted changes exactly where they were; only the symbolic-ref form of HEAD changes (transient detached state, immediately reattached by the worktree-add call). Operators can `git checkout <branch>` from the detached HEAD to recover their branch.

### Negative

- The parent's HEAD becomes transiently detached during recovery. Operators who observe `git status` during the recovery will see "HEAD detached at <commit>" instead of "on branch <branch>". The branch reference is re-created by `git worktree add -b <branch>` later in `Start()`, so the operator's view of their files is restored within the same invocation; this is a UI surprise, not a data loss.
- Strategy (3)'s `git branch -D` is destructive in the sense that it drops a ref. The branch reference is re-created by `git worktree add -b <branch>` immediately after, so the effective loss is bounded to a transient detached-HEAD window. If `Start` fails between the ref-drop and the re-creation, the operator is left with a detached HEAD on a deleted branch ref; the reflog preserves the commit so `git checkout -b <branch> <reflog-sha>` recovers.
- The auto-promotion in `--continue` (slice #934) breaks the implicit assumption from ~~ADR-0022 (deleted)~~ that a prior AgentRun exists. The previous error path — "no AgentRun for issue N; run with `--override`" — is no longer the only way out. Operators who relied on that error to catch wrong issue numbers will need to add an explicit guard, since the orchestrator will now treat a typo'd issue as a fresh `--override` and try to auto-recover whatever stranded state happens to exist.

### Phase 5 follow-up

Slices #934–#938 wired the auto-recovery for the `--override` path only. The prunable-reattach (strategy 0) for the `--continue` path was not implemented in those slices: when `--continue` encountered a prunable worktree registration, it would fall through to the orphan-dir cleanup and branch-exists error, failing loudly instead of auto-recovering. Slices 1–3 of issue #1006 close this gap by extending the `--no-reconcile-stranded` gate to also cover the prunable-reattach block, so the same `--reconcile-stranded` / `--no-reconcile-stranded` contract applies to both the override and continue flows.

### Neutral

- `scripts/reconcile-stranded-worktrees.sh` still works for manual detection. After slice #936 lands it is a thin wrapper around the Go helper, but operators can still run it to see which worktrees are stranded without invoking `sandman run`. The script's printed remediation commands are no longer needed in the common case, but the detection output is unchanged.
- The function variable seam is a `func(...) error`, not a struct field. This mirrors ADR-0019's `portalStaleCleaner` choice. Future work that wants to inject a stub from a `Dependencies` object can wrap the seam; today the orchestrator does not take a `Dependencies`.
- This ADR is filed as `accepted`. The original contract (slices #934–#938) and the extension (slices 1–3 of #1006) collectively implement the auto-recovery for both `--override` and `--continue`, including prunable-reattach for the `--continue` path. If any future slice diverges from the contract — for example, if `StrandedWorktree` returns a different shape, or if the recovery strategies are reordered — the ADR is the artefact that has to change to match, not the code.
