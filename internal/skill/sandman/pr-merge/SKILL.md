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

## What "fully approved" means

The change request is fully approved when the current head has an eligible
approval for the latest review request and nothing requests changes. Either
of these is full approval on its own:

- a formal APPROVED review on the current head;
- an eligible informal approval: a top-level response to the latest review
  request on the current head whose decision is APPROVED, with no Blocking or
  Important findings and no author changes requested.

A formal review is never required. The author and the reviewer may share one
account, and the tracker does not let an author formally approve their own
change request. When an eligible informal approval exists, do not ask the
reviewer or a maintainer for a formal review, a review decision, or a
publication workaround, and do not post another review request for the same
head.

## Workflow

1. Confirm the change request is fully approved, formally or informally, as
   defined above.
2. Confirm required checks are green.
3. Confirm the tracker reports the change request mergeable.
4. Merge with squash: `gh pr merge --squash`. Do not pass `--delete-branch`; the local branch must remain in this worktree for downstream sandman tooling (next run, --continue, --override).
5. Verify the PR actually merged.
6. After verifying, delete the remote branch from a different worktree — never from this worktree: `git push origin --delete <branch>`.
7. If approval is not achieved after 10 review cycles, leave the PR open and record final blockers plus the next executable action in `.sandman/task.md` and the run log.

## Stop conditions

- Do not merge if any gate is false.
- Do not merge if mergeability is ambiguous.
