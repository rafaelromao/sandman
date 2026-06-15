# ADR-0027: Auto-recover from stranded worktrees in `--override` and `--continue` flows

## Status

proposed

## Context

A *stranded worktree* is, per the `CONTEXT.md` *Stranded worktree* glossary term, "a sandman-managed git worktree whose HEAD points to a different branch than its directory name expects". The glossary entry currently claims that `sandman run --override reconciles them automatically` and points at `scripts/reconcile-stranded-worktrees.sh` as a standalone detection tool. That claim is only partially true.

`sandman run --override` already cleans the worktree subdirectory under `.sandman/worktrees/` and prunes the git worktree registration, so a stranded directory that lives entirely under `.sandman/worktrees/` is recovered the next time the orchestrator starts. It does not, however, handle the case where the *main repo* itself is checked out on a `sandman/N-…` branch. When that happens, `git worktree add` fails with the "branch used by worktree at …" error and `--override` does not unblock the run. The operator has to remember to `git checkout main` (or whichever base branch applies) in the main repo before re-running.

The glossary promise is therefore wrong for the most common stranded state. Issues #934–#938 close that gap: #934 teaches `--continue` to auto-promote a no-prior-run request to `ModeOverride`, #935 improves the operator-facing error messages, #936 extracts the stranded-worktree detection logic from `scripts/reconcile-stranded-worktrees.sh` into a Go helper (`StrandedWorktree`) so both the CLI and the portal can call it, and #937–#938 wire the helper into `WorktreeSandbox.Start` and `ClearIssueArtifacts` so the main-repo branch mismatch is repaired before the worktree is recreated.

This ADR records the decision to make that auto-repair the default behaviour. It is the contract that the slices in #934–#938 collectively implement. It is filed as `proposed` so the slices can be revised against the contract, not the other way around.

The auto-recovery pattern is not new. ADR-0021 (`docs/adr/0021-portal-auto-runs-clean-stale-on-startup.md`) established that the portal auto-runs `sandman clean --stale` on startup through a package-level function variable seam (`portalStaleCleaner func(string) error`) so the body is shared with the CLI flag and tests can stub the side effect. ADR-0027 reuses the same idea for the stranded-worktree case: the detection logic lives in one Go helper, the recovery loop lives in one place per call site, and tests swap the side effect through a function variable seam rather than reaching into the git CLI.

`--continue` is the second call site. ADR-0022 (`docs/adr/0022-replace-end-of-session-continuation-with-checkpointed-handoffs.md`) defined `--continue` as "Handoff is the persisted state; Continue is the action that reads it", with the implicit assumption that a prior AgentRun exists for the issue. Slice #934 lifts that assumption: when there is no prior run, `--continue` now falls through to `ModeOverride`, which means it will go through the same auto-recovery path as an explicit `--override`. The new behaviour, in other words, makes the "no prior run" branch of `--continue` indistinguishable from a fresh `--override` — including the stranded-worktree recovery that both flows now perform.

`scripts/reconcile-stranded-worktrees.sh` is the third artefact. After slice #936 lands, the script becomes a thin wrapper around the same Go helper that `--override` and `--continue` use. Operators can still run the script to *detect* stranded worktrees manually; they no longer need to copy-paste the printed remediation commands, because the orchestrator will run them automatically the next time they invoke `--override` or `--continue`.

## Decision

Auto-recovery from stranded worktrees is on by default in the two flows that can create a worktree: `sandman run --override` and `sandman run --continue` (after the #934 auto-promotion falls through). Both flows call the same Go helper before they call `git worktree add`, and the helper performs detection and recovery in a single pass.

The two new CLI flags share a single boolean (`reconcileStranded`) with a sensible default:

- `--reconcile-stranded` — set the boolean to `true`. This is the default; the flag is provided for symmetry with `--no-reconcile-stranded` and for documentation purposes.
- `--no-reconcile-stranded` — set the boolean to `false`. Operators opt out of the auto-repair entirely; the flow surfaces the same "branch used by worktree at …" error it surfaced before this ADR landed.

`StrandedWorktree` (the Go helper introduced in #936) is the canonical detection source. Both `--override` and `--continue` call it; `scripts/reconcile-stranded-worktrees.sh` is reduced to a wrapper that shells out to the same helper. Detection returns a structured value that the recovery loop can iterate over without re-parsing git output.

The recovery loop tries three strategies, in order, and stops as soon as one succeeds:

1. **Delete the branch from the stranded worktree's cwd.** If the worktree at `.sandman/worktrees/<issue>` is checked out on the wrong branch but that branch has no commits ahead of the base, the helper `git branch -D`s the wrong branch from inside the worktree directory. This is the cheapest fix and is sufficient when the stranded branch is genuinely unreferenced.
2. **Check out the base branch in the main repo.** If the main repo itself is on `sandman/N-…`, the helper runs `git -C <repoRoot> checkout <baseBranch>` so the worktree registration can be removed and re-created. This is the most common case after a `Ctrl+C` mid-run on the main repo.
3. **`git update-ref -d` (last resort).** If strategies (1) and (2) fail — for example because the operator has uncommitted changes in the main repo that block the checkout — the helper deletes the stray ref with `git update-ref -d refs/heads/sandman/N-…`. This is a destructive last resort: it does not touch the working tree, but it does drop the ref so the orchestrator can re-create the worktree. The helper logs a warning before this branch runs and only proceeds if the ref has no commits ahead of the base.

All three strategies are local to the worktree base and the main repo. They never touch refs that the operator has not touched in this session, and they never force-push.

The package-level function variable seam that ADR-0021 introduced is replicated here. The recovery loop calls a package-level `reconcileStrandedFn func(repoRoot, worktreeBase string) error` (or the equivalent closure captured by the orchestrator) so tests can stub the side effect without spawning a real git repo. The default closure calls the real helper; tests inject a stub that records the call and returns success.

## Consequences

### Positive

- Operators no longer need to remember to `git checkout main` (or whichever base branch applies) in the main repo before re-running `sandman run --override`. The most common stranded-worktree case is repaired automatically, and the run proceeds.
- The same auto-recovery now fires on `sandman run --continue` for issues that have no prior run, because #934 promotes those requests to `ModeOverride`. Operators who relied on the "no prior run" error to catch typos in issue numbers will need to use `--no-reconcile-stranded` plus a deliberate issue-number check, but the common case of "I forgot which branch I was on" no longer needs a manual fix.
- `StrandedWorktree` is the single detection source. The bash script, the CLI, and the portal all call the same Go helper, so detection rules cannot drift between surfaces.
- `--no-reconcile-stranded` is the documented escape hatch. Operators who want the old "fail loudly" behaviour can opt out per invocation without having to revert a global setting.

### Negative

- Auto-recovery mutates the main repo's checked-out branch. Operators who treat `sandman run --override` as a "scoped to `.sandman/worktrees/`" command will be surprised the first time their main repo's branch changes underneath them. The CLI logs the recovery and the strategy used so the surprise is at least visible in the same terminal session.
- Strategy (c) (`git update-ref -d`) is destructive. It is a deliberate last resort, but it does drop a ref. The helper logs a warning before this branch runs and refuses to delete a ref that has commits ahead of the base, so the destruction is bounded; still, an operator who expected the recovery to be no-op-on-main-repo will see the ref disappear and may not realise why.
- The auto-promotion in `--continue` (slice #934) breaks the implicit assumption from ADR-0022 that a prior AgentRun exists. The previous error path — "no AgentRun for issue N; run with `--override`" — is no longer the only way out. Operators who relied on that error to catch wrong issue numbers will need to add an explicit guard, since the orchestrator will now treat a typo'd issue as a fresh `--override` and try to auto-recover whatever stranded state happens to exist.

### Neutral

- `scripts/reconcile-stranded-worktrees.sh` still works for manual detection. After slice #936 lands it is a thin wrapper around the Go helper, but operators can still run it to see which worktrees are stranded without invoking `sandman run`. The script's printed remediation commands are no longer needed in the common case, but the detection output is unchanged.
- The function variable seam is a `func(...) error`, not a struct field. This mirrors ADR-0021's `portalStaleCleaner` choice. Future work that wants to inject a stub from a `Dependencies` object can wrap the seam; today the orchestrator does not take a `Dependencies`.
- This ADR is filed as `proposed`. The contract described above is the contract the slices in #934–#938 implement. If any of those slices diverge from the contract — for example, if `StrandedWorktree` returns a different shape, or if the recovery strategies are reordered — the ADR is the artefact that has to change to match, not the code.
