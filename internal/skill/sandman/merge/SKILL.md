---
name: sandman-merge
description: Safely merges a base branch into the current branch and resolves merge conflicts with a disciplined 3-way workflow that avoids history rewrites. Use when user says sandman merge, when preparing a branch to catch up with its base branch, when the user asks to merge main/master/develop into the current branch, or when avoiding force-push is a hard rule.
---

# Merge

## Quick start

Use this workflow to merge `<base-branch>` into the current branch without rebasing:

```bash
git status --short
git fetch origin
git merge-base --is-ancestor "origin/<base-branch>" HEAD
git merge "origin/<base-branch>"
```

If `git merge-base --is-ancestor` succeeds, the current branch already contains the base branch and no merge is needed.

## Guardrails

- Never rebase as part of this skill.
- Never force-push.
- Never merge with uncommitted or unstaged changes. Stop and ask for direction instead.
- Never use `git merge -X ours`, `git merge -X theirs`, or file-wide `--ours` / `--theirs` unless the user explicitly asks for that tradeoff.
- Never resolve conflicts from markers alone when the behavior is non-trivial.

## Workflow

1. Confirm you are on the intended feature branch, not the base branch.
2. Check `git status --short`. If the worktree is dirty, stop.
3. Run `git fetch origin`.
4. Check whether the merge is already present:
   `git merge-base --is-ancestor "origin/<base-branch>" HEAD`
5. If not already merged, run:
   `git merge "origin/<base-branch>"`
6. If the merge succeeds cleanly, run relevant tests and formatters, then push with a normal `git push`.
7. If conflicts occur, follow the conflict workflow below before committing the merge.

## Conflict workflow

1. Find the common ancestor:
   `MERGE_BASE=$(git merge-base HEAD "origin/<base-branch>")`
2. List unresolved files:
   `git diff --name-only --diff-filter=U`
3. For each conflicted path, inspect all 3 versions:
   - ancestor: `git show "$MERGE_BASE:<path>"`
   - current branch: `git show "HEAD:<path>"`
   - incoming base branch: `git show "origin/<base-branch>:<path>"`
4. Read surrounding code in the working tree, including conflict markers, to place the hunk in context.
5. If intent is still unclear, inspect nearby history:
   - `git log --oneline --left-right HEAD..."origin/<base-branch>" -- <path>`
   - `git blame -- <path>`
6. Resolve semantically:
   - keep both changes when they are independent
   - preserve behavior intentionally added on each side
   - prefer the version that matches surrounding code and current APIs
   - rewrite the hunk cleanly when neither side can be copied as-is
7. After editing, stage the file and verify no conflicts remain:
   - `git add <path>`
   - `git diff --name-only --diff-filter=U`
8. Run targeted tests for affected code first, then broader tests if the conflict was structural.

## Conflict heuristics

- Use the merge base as the baseline for intent. Ask: what changed on our branch, what changed on the incoming base, and how should both changes coexist now?
- Prefer manual reconciliation over choosing one whole side.
- If one side only reformats or renames while the other changes behavior, carry over both.
- For deleted vs modified files, verify whether the delete is still valid before keeping it.
- For renamed files, inspect history with `git log --follow -- <path>` if needed.
- Add or update tests when the merge changes behavior or fixes a regression exposed by the conflict.

## Stop conditions

- Stop and report if the worktree was dirty before merge.
- Stop and report if you cannot determine the correct behavior from code, tests, and history.
- Stop and report if tests fail and the correct post-merge behavior is ambiguous.
