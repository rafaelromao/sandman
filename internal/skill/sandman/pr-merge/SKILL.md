---
name: sandman-pr-merge
description: Merges a fully approved change request only after checks are green and the tracker reports it mergeable. Use when user says sandman pr-merge, wants to merge an approved change request, or needs the final change-request merge gate from Sandman's default prompt.
---

# PR Merge

## Goal

Merge the PR only when all merge gates pass.

## Preconditions

- PR fully approved
- Required checks green
- The tracker reports the change request mergeable

## Workflow

1. Confirm the change request is fully approved.
2. Confirm required checks are green.
3. Confirm the tracker reports the change request mergeable.
4. Merge with squash: `gh pr merge --squash`. Do not pass `--delete-branch`; the local branch must remain in this worktree for downstream sandman tooling (next run, --continue, --override).
5. Verify the PR actually merged.
6. After verifying, delete the remote branch from a different worktree — never from this worktree: `git push origin --delete <branch>`.
7. If approval is not achieved after 10 review cycles, leave the PR open and report final blockers.

## Stop conditions

- Do not merge if any gate is false.
- Do not merge if mergeability is ambiguous.
