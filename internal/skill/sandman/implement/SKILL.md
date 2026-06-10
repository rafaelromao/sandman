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
- `sandman-tdd`, `sandman-self-review`, `sandman-back-merge`, and `sandman-handoff` skills available

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

### 3. Handoff (plan-approved)

After the TDD plan is approved via subagent consensus:

- Load the `sandman-handoff` skill
- Follow its workflow to assemble completed, pending, blockers, key decisions, and next step
- Substitute `<STAGE>` in the skill template's `## Stage:` line with `plan-approved`
- Re-read `.sandman/rendered-prompt.md` and record `## Source Prompt: .sandman/rendered-prompt.md` (fixed path, unchanged)
- Set `## Last Skill` to `sandman-tdd`
- Set `## Last Skill Status` to `complete`
- Write the result to `.sandman/handoff.md` in the current worktree
- If `.sandman/handoff.md` already exists, overwrite it (only one handoff file is kept per worktree)

### 4. Implement (TDD)

- Follow `sandman-tdd` workflow: vertical slices, one test → one implementation, within the repository codebase
- Run project tests and formatting after each cycle
- Do NOT commit during TDD; keep working

### 5. Commit implementation

Once all tests pass and user is satisfied:

```bash
git add -A
git commit -m "feat: <issue title>"
```

### 6. Handoff (implementation-committed)

- Load the `sandman-handoff` skill
- Follow its workflow to assemble completed, pending, blockers, key decisions, and next step
- Substitute `<STAGE>` in the skill template's `## Stage:` line with `implementation-committed`
- Re-read `.sandman/rendered-prompt.md` and record `## Source Prompt: .sandman/rendered-prompt.md` (fixed path, unchanged)
- Set `## Last Skill` to `sandman-tdd`
- Set `## Last Skill Status` to `complete`
- Write the result to `.sandman/handoff.md` in the current worktree
- If `.sandman/handoff.md` already exists, overwrite it (only one handoff file is kept per worktree)

### 7. Self-review

- Load the `sandman-self-review` skill
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

### 8. Merge base branch before PR

- Load the `sandman-back-merge` skill
- Use it to merge the base branch into the current branch before creating the PR
- Resolve conflicts using the `sandman-back-merge` skill's 3-way workflow
- Run relevant tests and formatting after the merge
- Do NOT rebase
- Do NOT force-push

### 9. Push & create PR

```bash
git push -u origin <branch>
gh pr create --title "<issue title>" --body "Fixes #<ID>"
```

Capture the PR URL and number.

### 10. Handoff (pr-created)

- Load the `sandman-handoff` skill
- Follow its workflow to assemble completed, pending, blockers, key decisions, and next step
- Substitute `<STAGE>` in the skill template's `## Stage:` line with `pr-created`
- Set `## Source Prompt: .sandman/rendered-prompt.md` (fixed path, unchanged)
- Set `## Last Skill` to `sandman-implement`
- Set `## Last Skill Status` to `complete`
- Write the result to `.sandman/handoff.md` in the current worktree
- If `.sandman/handoff.md` already exists, overwrite it (only one handoff file is kept per worktree)

### 11. Delegate review

- Load the `sandman-pr-review` skill
- Run the delegated review loop on the PR
- Address all review feedback from the PR, including requests, suggestions, recommendations, and nits, unless there is a strong reason to ignore a specific item.
- If you do ignore feedback, explain why in the PR thread before continuing.
- Stop when the PR Review Agent approves or after max passes

### 12. Handoff (pr-review-finished)

When the delegated review result is either PR approval or a hard blocker:

- Load the `sandman-handoff` skill
- Follow its workflow to assemble completed, pending, blockers, key decisions, and next step
- Substitute `<STAGE>` in the skill template's `## Stage:` line with `pr-review-finished`
- Set `## Source Prompt: .sandman/rendered-prompt.md` (fixed path, unchanged)
- Set `## Last Skill` to `sandman-pr-review`
- Set `## Last Skill Status` to `complete` if the PR was approved, or `incomplete` if a hard blocker was encountered
- If the review returned a hard blocker, fill the `## Blockers` section with the blocker; otherwise leave `## Blockers` empty
- Write the result to `.sandman/handoff.md` in the current worktree
- If `.sandman/handoff.md` already exists, overwrite it (only one handoff file is kept per worktree)

> **Hard rule**: If the PR was already merged (e.g. `sandman-pr-merge` already ran and succeeded), do NOT write a handoff document. Skip step 12 entirely. A post-merge handoff is misleading — it suggests work remains, but there is none. The orchestrator also enforces this by deleting `.sandman/handoff.md` from the worktree when the PR merge gate passes.

## Checklist

- [ ] Branch created from latest main
- [ ] Changes confined to the repository codebase (not meta-infrastructure)
- [ ] User confirmed plan before TDD
- [ ] Implementation committed
- [ ] Self-review performed and fixes committed
- [ ] Base branch merged into current branch with `sandman-back-merge`
- [ ] PR created with `Fixes #<ID>`
- [ ] Delegate review completed
- [ ] Handoff written after each checkpoint (4 stages: plan-approved, implementation-committed, pr-created, pr-review-finished)
