---
name: sandman-implement
description: Automates the full work-item implementation workflow from branch creation to change-request merge in the current repository's codebase. Use when user says sandman implement, wants to implement an open work item end-to-end, or mentions automating the implement workflow.
---

# implement

End-to-end automation for implementing an open work item in the current repository's codebase.

## Scope

This skill implements an open work item by modifying the current repository's source code, tests, and configuration files. It does NOT create or modify skill definitions, documentation, or any meta-infrastructure.

## Prerequisites

- `gh` CLI authenticated
- Working directory at repo root
- `sandman-plan`, `sandman-tdd`, `sandman-self-review`, and `sandman-back-merge` skills available

## Workflow

You need to follow all steps in this workflow. Make sure you have gone through all items in the checklist in the end of this skill.

## Hard rules

1. **You must NOT exit while any test is failing.** If a test you wrote (or a test that started failing because of your changes) is red, you have two acceptable paths: (a) fix the code so the test goes green, or (b) revert the change that introduced the failure so the test goes green again. Describing the failure, hypothesizing about the cause, and exiting with the failure unresolved is NOT acceptable — a PR with failing tests will not be merged, and any retry will restart on the same broken state. If you truly cannot resolve the failure within the run's context window, commit a known-broken state to a separate diagnostic branch and revert the working branch to the last green commit.

2. **You must commit at meaningful milestones, not only at the very end.** Within a single vertical slice, accumulate the slice's RED→GREEN cycles as uncommitted work — committing after every test produces noisy, undebuggable history. The slice ends with a single commit once the slice is fully green. Across slices, commit one commit per vertical slice. Commit before any step where you might be interrupted — before delegating review, before requesting review, before any action that hands control to another agent. Uncommitted work in the working tree is at risk: if the run is interrupted, retried, or reset, anything that has not been committed is lost. Commits are your durable checkpoint.

3. **You must reach the PR-created state in every run, even with partial implementation.** Only a merged PR counts as success; an open PR is the durable artifact that lets the next run pick up where this one left off. If you cannot complete all vertical slices in the plan within the run's context window, that is not a reason to keep iterating on TDD — it is a reason to commit what you have, create the PR with a closing-reference body that links back to the implementor's open work item (one of `Closes #<issue_number>`, `Fixes #<issue_number>`, or `Resolves #<issue_number>`), and let the review loop surface the gaps. An open PR with partial implementation is recoverable: the next run continues from the same branch. No PR at all means the work lives only in the local working tree, and the next run starts over from a clean branch.

    **Closing-reference body is mandatory.** The PR body MUST contain a line of the exact shape `(Closes|Fixes|Resolves) #<issue_number>` so the tracker auto-closes the linked work item when the change request merges. Phrases like `issue #<n>` buried in prose, `Refs #<n>`, `See #<n>`, `Related to #<n>`, or `Part of #<n>` are NOT closing references — they leave the work item open after merge. A change request whose body does not match the closing-reference shape is not acceptable and must not be created.

4. **Never stage Sandman runtime state.** The `.sandman/` directory holds runtime files (config, prompt, Dockerfile, reviews, the per-run task board). It is intentionally gitignored and is untracked by `sandman init`'s pre-commit guard. Do not run `git add` (with or without `-f`) on any path under `.sandman/`, and do not commit changes that include such paths. The pre-commit hook installed by `sandman init` will reject any commit that attempts to put a `.sandman/` path back into the index, but treat that as a last line of defense: do not stage it in the first place. Other Sandman-managed worktrees may not yet have the hook installed, and a force-pushed history rewrite can resurrect ignored paths.

### 1. Setup branch

```bash
gh issue view <ID> --json title,number
```

- Checkout `main`/`master`, pull latest
- Create and switch to branch: `issue-<ID>/<slugified-title>`
- Report the issue title and branch name to user

### 1.5. Pre-flight check

After setting up the branch, determine whether the issue's work is already complete before running `sandman-plan` or `sandman-tdd`.

A merged change request will, by the tracker's merge rules, automatically close its work item — so there is no need to search for a closing change request separately.

1. Run the platform's "view work item" CLI to read the current state of the open work item.
2. Run branch freshness check:

   ```bash
   git fetch origin main
   git merge-base --is-ancestor HEAD origin/main
   ```

   If `merge-base --is-ancestor HEAD origin/main` returns false, **STOP**. Do NOT write `## Status: already resolved`. Load `sandman-back-merge` and merge the base branch into the current branch before re-evaluating. A branch that is behind `origin/main` is stale and cannot reliably be claimed as "already resolved" — the issue's AC may be partially met by local work that is not yet on `main`, or the AC check will race with the next main-line push.
3. Run open-PR check:

   ```bash
   gh pr list --head <branch> --state open
   ```

   If a PR is open for the current branch, the orchestrator will run an independent verification pass against `origin/main` before declaring the run successful. Write `## Status: already resolved` only if every AC has a corresponding test that exists on `origin/main`; otherwise the orchestrator cannot verify and the run will fail. Agents that prefer the explicit close path can pick one of:
   - **(a) Close the orphan PR** with the platform's "close change request" CLI, preserving the branch and commenting that the work is superseded by the new run, before writing the marker, OR
   - **(b) Stop without writing the marker** and let the existing PR drive the run — this is the safer default; the open PR is itself durable evidence of partial or pending work.
4. Decision matrix (after branch freshness and open-PR checks pass):
   - **Issue is closed** → verify the issue acceptance criteria against the current state of the base branch after fetching `origin/<base>`; only write `## Status: already resolved` and stop if the base branch actually satisfies every criterion. If the base branch does not satisfy every criterion, continue to step 2 (Plan) as normal.
   - **Issue is open** → read the issue acceptance criteria and compare against the current state of the base branch after fetching `origin/<base>`. If all acceptance criteria are already met in the base branch, write `## Status: already resolved` to `.sandman/task.md` and stop without running plan or TDD. Otherwise, proceed to step 2 (Plan) as normal.

Writing `## Status: already resolved` while a PR is open without a verification path fails the run, so step 3 is not optional.

### 2. Plan

- Read the issue body and any linked context
- Load the `sandman-plan` skill
- Let `sandman-plan` create the plan sketch
- Then load `sandman-tdd` and let it execute the plan from `## Plan` in `.sandman/task.md`
- Do NOT add a separate confirmation prompt

### 3. Implement (TDD)

- Follow `sandman-tdd` workflow: vertical slices, one test → one implementation, within the repository codebase
- Run project tests and formatting after each cycle
- **Do NOT commit individual RED→GREEN cycles within a single vertical slice.** Keep working within a slice until the slice is fully green, then commit one commit per vertical slice (see Hard Rule 2). This gives you atomic, reviewable history without the noise of micro-commits per test.
- **Always end a run in a committable state.** If context is running low and you have a green slice but not a complete implementation, commit what you have (one commit per finished slice) before moving to Step 4. Do not let the run end with multiple slices of uncommitted work — if it does, the next retry restarts from a clean branch.

### 4. Commit implementation

Once all tests pass and user is satisfied, derive a Conventional Commits subject that the PR title and the commit will share.

Pick the most accurate type for the change. Allowed types: `feat`, `fix`, `perf`, `docs`, `refactor`, `test`, `build`, `ci`, `chore`, `revert`. Append `!` to the type for breaking changes (for example, `feat!:`). Keep the subject to one imperative sentence with no trailing period.

```bash
COMMIT_TYPE="<type>"
COMMIT_SCOPE="<scope-or-empty>"
COMMIT_SUBJECT="<one-line imperative description of the change>"

if [ -n "$COMMIT_SCOPE" ]; then
  COMMIT_HEADER="$COMMIT_TYPE($COMMIT_SCOPE): $COMMIT_SUBJECT"
else
  COMMIT_HEADER="$COMMIT_TYPE: $COMMIT_SUBJECT"
fi

git add -A
git commit -m "$COMMIT_HEADER"
```

### 5. Self-review

- Load the `sandman-self-review` skill
- Perform a self-review of the changes
- Apply fixes, format the code, run all tests, including smoke and e2e, and commit:

```bash
git add -A
git commit -m "refactor: self-review fixes"
```

- **If any test fails during self-review, you must NOT exit with the failure unresolved** (see Hard Rule 1). Diagnose the failure, fix the code, and re-run until the test is green. If the failing test is a pre-existing flake unrelated to your changes, isolate it with `git stash`, re-run to confirm green, and document the flake in the commit message. If you cannot resolve the failure within the run's context window, do not proceed to Steps 6-8 — stop and leave the failure documented in the commit message and task.md so the next attempt has a clear starting point.

- Fix the code in case any of the tests fail. Commit again:

```bash
git add -A
git commit -m "refactor: self-review fixes"
```

- Repeat the Self-review cycle until all tests pass.

### 6. Merge base branch before PR

- Load the `sandman-back-merge` skill
- Use it to merge the base branch into the current branch before creating the PR
- Resolve conflicts using the `sandman-back-merge` skill's 3-way workflow
- Run relevant tests and formatting after the merge
- Do NOT rebase
- Do NOT force-push

### 7. Push & create PR

1. Push the branch.

   ```bash
   git push -u origin <branch>
   ```
2. Build the closing-reference body. The body MUST contain a line of the exact shape `(Closes|Fixes|Resolves) #<issue_number>` so the tracker auto-closes the linked work item on merge. The recommended body is exactly that single line:

   ```bash
   BODY="Closes #<issue_number>"
   ```

   `Closes`, `Fixes`, and `Resolves` are all accepted closing keywords on GitHub. Do NOT use `Refs`, `See`, `Related to`, `Part of`, or any other phrasing — those do not auto-close the issue, and a change request that does not auto-close its work item is not acceptable.
3. Create the change request with that body. The PR title must use the same Conventional Commits shape as the commit subject from step 4 (same `<type>(<scope>)?:` header; same subject). The title is gated by the CI status check that scans for Conventional Commits, so do not bypass the format with the literal issue title.

   ```bash
   gh pr create --title "$COMMIT_HEADER" --body "$BODY"
   ```
4. Verify the body that landed on the PR. Pull it back from the tracker and confirm it matches the closing-reference shape — do not trust that the create call succeeded, because the API may accept variants silently.

   ```bash
   gh pr view <new-pr-number> --json body --jq -r .body
   ```

   The first non-empty line of the returned body MUST match `^(Closes|Fixes|Resolves) #<issue_number>\s*$`. If it does not — for example, the body is a long description with only `issue #<n>` buried in prose — STOP. Update the body in place so it is exactly `Closes #<issue_number>` (or `Fixes` / `Resolves`), then re-verify. If the body still cannot be made to match after one re-edit attempt, stop without delegating review and report the exact wrong body to the user.
5. Capture the PR URL and number.

### 8. Delegate review

- Load the `sandman-pr-review` skill
- Run the delegated review loop on the PR
- Address all review feedback from the PR, including requests, suggestions, recommendations, and nits, unless there is a strong reason to ignore a specific item.
- If you do ignore feedback, explain why in the PR thread before continuing.
- Stop when the PR Review Agent approves or after max passes

## Checklist

- [ ] Branch created from latest main
- [ ] Changes confined to the repository codebase (not meta-infrastructure)
- [ ] User confirmed plan before TDD
- [ ] Each vertical slice committed before moving to the next (Hard Rule 2)
- [ ] All tests green at exit (Hard Rule 1) — no failing tests left unresolved
- [ ] Implementation committed
- [ ] Self-review performed and fixes committed
- [ ] Base branch merged into current branch with `sandman-back-merge`
- [ ] PR created with a closing-reference body that links back to the implementor's open work item, and verified post-create that the body matches `^(Closes|Fixes|Resolves) #<issue_number>\s*$` (Hard Rule 3 — even with partial implementation)
- [ ] Delegate review completed
