# ADR-0040: Runtime branch naming drops the `sandman/` prefix

## Status

accepted

## Context

The runtime has always emitted `sandman/<issue-number>-<slugified-title>` as the worktree branch for issue-driven AgentRuns. The shape is built by `BranchName` in `internal/batch/orchestrator.go` and parsed by `ListStrandedWorktrees` in `internal/sandbox/stranded.go`. The hardcoded `sandman/` prefix was a namespace marker that made Sandman-managed branches discoverable in `git branch` output.

`AGENTS.md` "### Feature branches" and `CONTRIBUTING.md` "### Feature branches" describe the `feat/<feature-name>` initiative convention: when a feature branch is the configured base, issue branches should be cut from the feature branch and target it on PR. The hardcoded `sandman/` prefix collides with that convention — every issue branch is forced into a single sub-namespace, and the feature-branch ownership is invisible in the branch name.

Issue #2376's "Notes" section observes that `sandman-implement`'s outside-Sandman manual branch (`issue-<num>/<slug>`) and the runtime's `sandman/<n>-<slug>` coexist in the repo because they do not collide. The coexistence is tolerated, not by design — the runtime branch name does not align with the feature-branch convention the docs describe.

## Decision

Change `BranchName(issueNumber, title)` to `BranchName(issueNumber, title, sourceBranch)` returning the issue-driven default shape `<n>-<slug>`.

The `sourceBranch` argument is accepted for API symmetry with the orchestrator's resolved base branch (the same value threaded through the orchestrator's `req.BaseBranch` and `s.baseBranch` plumbing). The runtime branch name does not encode the source branch because git's ref-namespace model forbids encoding a feature branch (e.g. `feat/release-pipeline-2026q3`) as a prefix of another branch (e.g. `feat/release-pipeline-2026q3/955-...`) when the feature branch itself exists: `git worktree add -b feat/release-pipeline-2026q3/955-...` fails with `cannot lock ref 'refs/heads/feat/release-pipeline-2026q3/955-...': 'refs/heads/feat/release-pipeline-2026q3' exists`. The feature branch relationship is carried through the source branch parameter, not the branch name.

The hardcoded `sandman/` prefix at `internal/sandbox/stranded.go:270` drops in lockstep, so the expected ref becomes `refs/heads/<dirname>` for directory names matching `^[0-9]+-`. The regex still matches issue-driven worktree directories at the basename level regardless of the source branch.

The sidecar branches (`sandman/built-with-sandman` for the badge, `sandman/review-<pr>-<commentID>` for the review daemon) and the prompt-only branch (`sandman/<slug>-<timestamp>`) keep their `sandman/` prefix because they have no `<n>-<slug>` shape and no base-branch relationship. Migrating them would either rename the badge PR branch or break the convention's symmetry.

The `internal/skill/sandman/implement/SKILL.md` line that mentions `issue-<ID>/<slugified-title>` is the outside-Sandman manual-invocation convention and is unaffected — it stays as the user-facing skill's documented branch shape for the manual path.

## Consequences

### Positive

- The runtime's branch shape no longer carries a hardcoded `sandman/` namespace marker. The branch name is now driven by the issue number and slug, which is the operator's mental model.
- The hardcoded `sandman/` namespace prefix is gone from the runtime, reducing the surface of `internal/` that needs to know about runtime namespace plumbing.
- The source branch is threaded through `BranchName` as a passive argument, so a future extension (different SCM, different naming convention) can pick up the base branch without a signature change.

### Negative

- Existing on-disk worktrees named `sandman/<n>-<slug>` are not migrated. They will be reported as stranded by `sandman stranded` after the upgrade and require operator cleanup with `sandman clean` (or manual `git branch -D` and `git worktree remove`).
- `BranchName` is now a 3-arg function. Every call site must thread the resolved base branch through. The change is mechanical — every call site already has `s.baseBranch` (on `runSession`) or the resolved `req.BaseBranch` (with `cfg.Git.BaseBranch` fallback) to hand.
- The blast radius across tests is large (~500 string literals across 60+ test files). Each literal is a cosmetic update to track the new convention; the test behavior is unchanged.
- The runtime cannot encode the feature branch in the issue branch name. Operators who want to see feature-branch ownership at PR-review time must inspect the worktree's source branch (via `git worktree list`) or the open PR's `--base` flag. The `AGENTS.md#feature-branches` "example initiative" tree draws the relationship between the feature branch and its issue branches through the change-request target, not the branch name.

### Neutral

- The sidecar and prompt-only branches keep their `sandman/` prefix. The new convention strictly applies to issue-driven branches where the branch name is the issue identifier.
- The container-side T1 oracle (`internal/batch/verify_sandbox.go`) is unaffected — it uses `origin/main` as the source for verification, not the runtime branch name.
- The orphan worktree at `/home/romao/projects/sandman/internal/batch/.sandman/worktrees/sandman/42-fix-bug/` is a stale test artifact unrelated to the runtime change. Operator cleanup is `rm -rf` when convenient; out of scope for this ADR.
