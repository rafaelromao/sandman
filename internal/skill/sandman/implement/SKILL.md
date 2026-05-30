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
- `sandman-tdd`, `sandman-review`, and `sandman-merge` skills available

## Workflow

You need to follow all steps in this workflow. Make sure you have gone through all items in the checklist in the end of this skill.

### 1. Setup branch

```bash
gh issue view <ID> --json title,number
```

- Checkout `main`/`master`, pull latest
- Create and switch to branch: `issue-<ID>/<slugified-title>`
- Report the issue title and branch name to user

### 2. Plan

- Read the issue body and any linked context
- Load the `sandman-tdd` skill
- Let `sandman-tdd` handle the plan draft and user approval internally, scoped to the repository codebase
- Do NOT add a separate confirmation prompt — `sandman-tdd` already asks for approval before writing code

### 3. Implement (TDD)

- Follow `sandman-tdd` workflow: vertical slices, one test → one implementation, within the repository codebase
- Run project tests and formatting after each cycle
- Do NOT commit during TDD; keep working

### 4. Commit implementation

Once all tests pass and user is satisfied:

```bash
git add -A
git commit -m "feat: <issue title>"
```

### 5. Self-review

- Load the `sandman-review` skill
- Perform a self-review of the changes
- Apply fixes, format the code, run all tests, including smoke and e2e, and commit:

```bash
git add -A
git commit -m "refactor: self-review fixes"
```

- Fix the code in case any of the tests fail. Commit again:

```bash
git add -A
git commit -m "refactor: self-review fixes"
```

- Repeat the Self-review cycle until all tests pass.

### 6. Merge base branch before PR

- Load the `sandman-merge` skill
- Use it to merge the base branch into the current branch before creating the PR
- Resolve conflicts using the `sandman-merge` skill's 3-way workflow
- Run relevant tests and formatting after the merge
- Do NOT rebase
- Do NOT force-push

### 7. Push & create PR

```bash
git push -u origin <branch>
gh pr create --title "<issue title>" --body "Fixes #<ID>"
```

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
- [ ] Implementation committed
- [ ] Self-review performed and fixes committed
- [ ] Base branch merged into current branch with `sandman-merge`
- [ ] PR created with `Fixes #<ID>`
- [ ] Delegate review completed
