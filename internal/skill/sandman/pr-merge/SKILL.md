---
name: sandman-pr-merge
description: Merges a fully approved change request only after checks are green and the tracker reports it mergeable. Use when user says sandman pr-merge, wants to merge an approved change request, or needs the final change-request merge gate from Sandman's default prompt.
---

# PR Merge

## Goal

Merge the PR only when all merge gates pass, then close the implementor's own
work item.

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
- when no review command is configured, a confirmed blind-review publication
  for the current head whose verdict is approved, with no Blocking or Important
  findings or outstanding formal requested changes. The newest current-head
  blind report supersedes earlier rounds; a trigger or daemon author is not
  required. With a configured command, blind publications do not satisfy its
  delegated-review gate.

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
4. Select the merge method from the repository's documented policy and the hosting service's allowed methods. Prefer squash only when no policy selects another method and squash is allowed; otherwise use an allowed merge or rebase method. If policy and allowed methods have no intersection, record the concrete blocker and stop. Invoke the selected method explicitly (`gh pr merge --squash`, `gh pr merge --merge`, or `gh pr merge --rebase`). Do not pass `--delete-branch`; the local branch must remain in this worktree for downstream sandman tooling (next run, --continue, --override).
5. Verify the PR actually merged.
6. Close the implementor's own work item after the verified merge, as described in "Close the work item after merge" below. The tracker closes a work item from the closing reference only when the change request targets the repository's default branch, so a merge into any other configured base branch would leave it open.
7. After verifying, delete the remote branch from a different worktree — never from this worktree: `git push origin --delete <branch>`.
8. If approval is not achieved after 10 review cycles, leave the PR open and record final blockers plus the next executable action in `.sandman/task.md` and the run log.

## Close the work item after merge

Close only the implementor's own work item,
never another work item the change request references.

1. Read the merged change request's own state and base. After the merge it is
   no longer open, so read it by number:

   ```bash
   gh pr view <pr-number> --json state,mergedAt,baseRefName
   ```

2. Resolve the configured base branch from the first source that provides it:
   the caller (in a Sandman-managed run, the Task's `Base branch`), then
   `git.base_branch` in `.sandman/config.yaml`. When a configured base branch
   is known and differs from the merged change request's base,
   do not close the work item; record the mismatch as a blocker in
   `.sandman/task.md` and the run log. When none is configured, the change
   request's base is the configured base branch.
3. Read the work item's state and close it only while it is still open. A work
   item the tracker already closed needs nothing more:

   ```bash
   gh issue view <work-item-number> --json state --jq .state
   gh issue close <work-item-number> --comment "Closed by sandman — merged in #<pr-number> into <base>."
   ```

   Use exactly the comment `Closed by sandman — merged in #<pr-number> into <base>.`,
   where `<base>` is the merged change request's base.
4. Read the state back and accept only a confirmed closed state. Otherwise
   retry the close once. If it is still not closed,
   record the blocker and the exact close command in `.sandman/task.md` and the run log,
   then finish. The merge result stands: a failed close never undoes, repeats
   or blocks the merge.

## Stop conditions

- Do not merge if any gate is false.
- Do not merge if mergeability is ambiguous.

If required CI is failing, return to implementation-owned diagnosis and repair; do not treat a pending sibling check, review approval, or a saved blocker as permission to merge or hand off an untouched failure. A narrow source, test, configuration, or workflow change needed to repair the required checks is within implementation scope. For a diagnosed transient provider/network timeout, the implementation agent may use the bounded existing-run recovery action in the implementation capability; never bypass a required check.
