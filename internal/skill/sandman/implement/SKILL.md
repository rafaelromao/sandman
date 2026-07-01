---
name: sandman-implement
description: Automates the full issue implementation workflow from branch creation to PR merge in the current repository's codebase. Use when user says sandman implement, /work_on_issue, wants to implement an issue end-to-end, or mentions automating the issue workflow.
---

# implement

End-to-end automation for implementing a GitHub issue in the current repository's codebase.

## Scope

This skill implements a GitHub issue by modifying the current repository's source code, tests, and configuration files. It does NOT create or modify skill definitions, documentation, or any meta-infrastructure.

## Prerequisites

- `gh` CLI authenticated
- Working directory at repo root
- `sandman-plan`, `sandman-tdd`, `sandman-self-review`, and `sandman-back-merge` skills available

## Workflow

You need to follow all steps in this workflow. Make sure you have gone through all items in the checklist in the end of this skill.

## Hard rules

1. **You must NOT exit while any test is failing.** If a test you wrote (or a test that started failing because of your changes) is red, you have two acceptable paths: (a) fix the code so the test goes green, or (b) revert the change that introduced the failure so the test goes green again. Describing the failure, hypothesizing about the cause, and exiting with the failure unresolved is NOT acceptable — a PR with failing tests will not be merged, and any retry will restart on the same broken state. If you truly cannot resolve the failure within the run's context window, commit a known-broken state to a separate diagnostic branch and revert the working branch to the last green commit.

2. **You must commit at meaningful milestones, not only at the very end.** Within a single vertical slice, accumulate the slice's RED→GREEN cycles as uncommitted work — committing after every test produces noisy, undebuggable history. The slice ends with a single commit once the slice is fully green. Across slices, commit one commit per vertical slice. Commit before any step where you might be interrupted — before delegating review, before requesting review, before any action that hands control to another agent. Uncommitted work in the working tree is at risk: if the run is interrupted, retried, or reset, anything that has not been committed is lost. Commits are your durable checkpoint.

3. **You must reach the PR-created state in every run, even with partial implementation.** Only a merged PR counts as success; an open PR is the durable artifact that lets the next run pick up where this one left off. If you cannot complete all vertical slices in the plan within the run's context window, that is not a reason to keep iterating on TDD — it is a reason to commit what you have, create the PR with `Fixes #<issue_number>`, and let the review loop surface the gaps. An open PR with partial implementation is recoverable: the next run continues from the same branch. No PR at all means the work lives only in the local working tree, and the next run starts over from a clean branch.

4. **You must use `codeindex` before `grep` or `glob` when looking for symbols, blast radius, dependencies, or other broad code locations.** Load the `sandman-index` skill first — it encapsulates all codeindex guidance including the hard rule, command reference, query refinement strategies, and read discipline.

### 1. Setup branch

```bash
gh issue view <ID> --json title,number
```

- Checkout `main`/`master`, pull latest
- Create and switch to branch: `issue-<ID>/<slugified-title>`
- Report the issue title and branch name to user

### 1.5. Pre-flight check

After setting up the branch, determine whether the issue's work is already complete before running `sandman-plan` or `sandman-tdd`.

A merged PR that closes an issue will, by GitHub rules, automatically close the issue — so there is no need to search for a closing PR separately.

1. Run: `gh issue view <ID> --json state`
2. Decision matrix:
   - **Issue is closed** → verify the issue acceptance criteria against the current state of the base branch after fetching `origin/<base>`; only write `## Status: already resolved` and stop if the base branch actually satisfies every criterion. If the base branch does not satisfy every criterion, continue to step 2 (Plan) as normal.
    - **Issue is open** → read the issue acceptance criteria and compare against the current state of the base branch after fetching `origin/<base>`. If all acceptance criteria are already met in the base branch, write `## Status: already resolved` to `.sandman/task.md` and stop without running Plan/TDD. Otherwise, proceed to step 2 (Plan) as normal.

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

Once all tests pass and user is satisfied:

```bash
git add -A
git commit -m "feat: <issue title>"
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

```bash
git push -u origin <branch>
gh pr create --title "<issue title>" --body "Fixes #<issue_number>"
```

Before running `gh pr create`, set `body` to exactly `Fixes #<issue_number>`.
Verify the final `body` string is exactly `Fixes #<issue_number>` and contains no other issue references or extra text.
If the body is wrong, do NOT create the PR. Instead, report the exact wrong body to the user and stop.

Capture the PR URL and number.

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
- [ ] PR created with `Fixes #<issue_number>` (Hard Rule 3 — even with partial implementation)
- [ ] Delegate review completed
