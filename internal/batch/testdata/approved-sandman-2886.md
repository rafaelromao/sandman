## Summary

This PR fixes the four latent defects from #2885:

1. **Lease renewal.** `newWaitOwnerWithFailure` retries renewal I/O failures on later ticks and reports each one through `onRetry`. It calls `onFailure` only when the next renewal would land after the uncapped `RunRecoveryGrace` of the last successful renewal. Errors classified as `daemon.ErrRunWaitInvalid` (foreign identity, changed deadline, corrupt record) still abort immediately.
2. **Claim files.** `ClaimRun` checks after locking that the path still names the inode it locked, and retries otherwise. The new `PruneRunClaims`, wired into `sandman clean --stale` and `--all` (never under dry-run), unlinks claim files only for event-terminal Runs, and only while holding their claim.
3. **Git guard.** The guard now runs `git -c gc.worktreePruneExpire=never`. Since the last review, each guard also removes its own directory from `PATH`, so nested guards delegate instead of looping.
4. **PR-head reconciliation.** Deleting the temporary ref uses a 10-second timeout derived with `context.WithoutCancel`.

The docs are updated in AGENTS.md, CONTEXT.md, `docs/architecture/*`, `docs/development/architecture-guidelines.md` and `docs/usage/commands.md`.

## Quality check

### Scope
18 files: 6 production Go files (one with an embedded POSIX shell script), 6 Go test files and 6 Markdown docs. 883 lines added, 56 removed. Modules touched: `internal/batch`, `internal/daemon`, `internal/cmd`, `internal/sandbox`, plus repository docs.

`cross-cutting` — the change spans four packages and shared infrastructure: RunID claim and wait-lease persistence under `.sandman/state`, and the Git guard used by every agent sandbox.

### Metrics
- Cognitive complexity:
  - `(*Orchestrator).RunBatch`: 753 (threshold 15). Flag: `internal/batch/orchestrator.go:1493`. Already over on `main`; the PR adds only a three-line `leaseRetry` closure and two extra arguments.
  - `NewCleanCmd`: 98 (threshold 15). Flag: `internal/cmd/clean.go:149`. Already over on `main`; the PR adds two one-line calls.
  - `newWaitOwnerWithFailure`: 30 (threshold 15). Flag: `internal/batch/waiting_owner.go:50`.
  - `PruneRunClaims`: 20 (threshold 15). Flag: `internal/daemon/run_wait.go:123`.
  - `claimRun`: 16 (threshold 15). Flag: `internal/daemon/run_wait.go:56`.
  - All other changed functions are at or below 15. No flag.
- Cyclomatic complexity:
  - `RunBatch`: 290 (threshold 20). Flag: `internal/batch/orchestrator.go:1493`. Already over on `main`.
  - `NewCleanCmd`: 49 (threshold 20). Flag: `internal/cmd/clean.go:149`. Already over on `main`.
  - Every function the PR adds or rewrites is at or below 19 (threshold 20). No flag.
- None of these breaches comes with a demonstrated readability concern. Since the last review, the retry decision has been extracted into the named predicate `renewalRetryable`. Per `.sandman/reviews/quality-rules.md` ("metric breach without a demonstrated readability or change-risk concern"), the breaches are reported here only.
- Prior coverage of the blast radius not measured: repository has no configured coverage tool.

### Findings
None.

### Tools used
`gocognit` and `gocyclo` on the changed production files at the PR head (`8594a826`), plus a manual review of the diff against the construct-tag rules.

## Findings

No Blocking, Important or Nit findings.

How each acceptance criterion from #2885 is met:
- **One transient failure, then success, keeps the row running:** `renewalRetryable` plus the retry tests in `internal/batch/waiting_owner_test.go`.
- **Failures that persist until the lease would lapse call the failure callback exactly once:** the give-up test, the deadline-capped lease test and the invalid-ownership test in the same file.
- **`ClaimRun` retries when the locked inode no longer matches the path:** `claimRun` with its `beforeLock` test hook. The revalidation in `claimFileIsPath` uses `os.SameFile`, and a missing path counts as a mismatch.
- **`sandman clean` removes claims for terminal Runs only:** `PruneRunClaims` skips non-terminal Runs, Runs with no events, non-regular files and live claims (`ErrRunOwned`). The `--all` path skips it under dry-run, and `--stale` already rejects `--dry-run`. If the event log cannot be read, every claim is kept.
- **The Git guard runs with `-c gc.worktreePruneExpire=never`, and `git gc` keeps a stale registration:** `TestWorktreeSandboxGitGuardKeepsSiblingRegistrationsThroughGC`.
- **Bounded cleanup context:** `removeTemporaryRef`.

I also checked the race analysis for claim pruning. A claimant on the old inode either gets `ErrRunOwned` while the prune holds the lock, or locks the unlinked inode, fails revalidation and retries onto a fresh file. Two claimants can never hold different inodes for one RunID. Wrapping the `validateRunWait`/`RenewRunWait` errors with `invalidRunWait` keeps their error text and their `Unwrap` chain.

## Suggested next steps

- None required.
- For information only, this is not a code finding: an agent sandbox whose outer guard predates this PR still hangs `TestWorktreeSandboxExecBlocksGlobalWorktreePrune`. I saw this here because the `/workspace/.sandman/agent-bin/git` guard has no `self_dir` handling. The nested-guard fix takes effect once the host Sandman binary ships the new guard. With the outer guard removed from `PATH`, the full `internal/sandbox` package passes, including `TestWorktreeSandboxGitGuardDelegatesInsideAnotherGuard`.
- Verified locally at `8594a826`:
  - Targeted `go test -race` passed for `internal/batch` (WaitOwner, Waiting and ReconcileWorktreeToPRHead tests), all of `internal/daemon`, `internal/cmd` (Clean tests) and all of `internal/sandbox`.
  - `make fmt-check vet` passed, and all 11 CI checks are green.
  - I did not run the smoke or e2e tiers.

## Decision

**APPROVED**

## Previous review progress

- Nit `[control-flow]`: name the renewal retry-or-give-up predicate in `newWaitOwnerWithFailure`. **Resolved.** It is extracted as `renewalRetryable(err, at, renewedAt)` in `internal/batch/waiting_owner.go`.
- Environment note: the guard tests hang inside an agent sandbox. **Resolved in this PR.** Each guard now also strips its own `$0` directory from `PATH`, and the new regression test is `TestWorktreeSandboxGitGuardDelegatesInsideAnotherGuard`. Sandboxes still running a pre-PR outer guard keep hanging until the host binary updates, as described above.

<!-- sandman-publication:3dba578f4da223545cf37a04c03e8c35f7f3332196b03455557c77eb775427c9 -->
