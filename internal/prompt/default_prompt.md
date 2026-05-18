# Task

Implement issue #{{ISSUE_NUMBER}}: {{ISSUE_TITLE}}

## Context

{{ISSUE_BODY}}

## Operating Rules

- Use parallel subagents whenever a stage has independent reads or fixes.
- Never parallelize the TDD implementation loop.
- Wait for all required subagents before synthesizing a plan or making changes.
- Dedupe overlapping findings before acting on them.
- Prioritize blockers first, then suggestions, then nits.
- The agent is only done when the PR is merged or merge is impossible after exhausting the review loop.

## Approach

### 1. Setup
- Sandman already rendered the issue content and created the current worktree/branch.
- Work in the current Sandman-created worktree on `{{BRANCH}}`.
- Do not run `gh issue view {{ISSUE_NUMBER}}`, `git checkout main`, `git pull`, or create a new branch.
- If a toolchain is missing, use mise first before adding ad hoc installs.

### 2. Plan
- Read the issue body and linked context.
- Run parallel readers:
  - Reader A: issue/spec/docs plus domain glossary.
  - Reader B: codebase and test surface.
- Wait for both readers.
- Synthesize one implementation plan from the combined results.
- Keep the plan behavior-first, not implementation-first.
- Design testable public interfaces.
- List behaviors to test, not implementation steps.
- No horizontal slicing: one test -> one impl -> repeat.

### 3. Implement (TDD)
- RED: write one test for one behavior -> fails.
- GREEN: write minimal code to pass -> passes.
- Repeat for each behavior.
- Keep tests at public interface level, not implementation details.
- Run project tests and formatting after each cycle.
- Do NOT commit during TDD.

### 4. Commit
```bash
git add -A
git commit -m "feat: <issue title>"
```

### 5. Self-review
Review the diff against the originating issue. For each file/hunk:
- Run parallel reviewers:
  - Standards reviewer: repo standards, glossary, ADRs, and coding guidance.
  - Spec reviewer: issue, linked context, and expected behavior.
- Wait for both reviewers.
- Synthesize their findings into one fix list.
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

### 6. Push & PR
```bash
git push -u origin {{BRANCH}}
gh pr create --base {{DEFAULT_BRANCH}} --head {{BRANCH}} --title "{{ISSUE_TITLE}}" --body "Fixes #{{ISSUE_NUMBER}}"
```

- Capture the PR URL and number.

### 7. Delegate review (max 10 passes)
- Poll `gh pr checks <N>` until CI passes.
- If checks fail, fix them, commit, and push.
- Post `gh pr comment <N> --body "{{REVIEW_COMMAND}}"`
- Poll `gh pr view <N> --comments` every 30-60s, with a 5 minute timeout.
- If no review response arrives in time, stop and report the PR as still open.
- When feedback arrives, cluster independent comments and fix them in parallel where possible.
- Address blockers first, then suggestions, then nits.
- Apply fixes, run tests/formatting, commit, and push.
- Repeat this loop until approved or max 10 passes.
- Do NOT review your own PR.
- Delegate exclusively to `{{REVIEW_COMMAND}}`.

### 8. Merge and finish
- Only merge when all of these are true:
  - opencode has approved the PR
  - required checks are green
  - GitHub reports the PR is mergeable
- Merge with squash.
- Verify the PR actually merged.
- Delete the branch after merge.
- If approval is not achieved after 10 review cycles, leave the PR open and report the final blockers.

## Final Result

Return:
- PR URL
- status
- number of review cycles used
- last blocking feedback if the PR stopped after 10 cycles
