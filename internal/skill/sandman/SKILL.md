# Sandman

Use this skill when Sandman delegates a GitHub issue to an agent.

## Objective

Implement the issue in the current Sandman worktree. Sandman already prepared the branch, prompt, and runtime context.

## Rules

- Stay inside the current Sandman worktree.
- Do not create a new branch.
- Prefer parallel reads when they are independent.
- Keep the review command parameterized by Sandman.
- Continue until the PR is merged or merge is impossible after exhausting review.

## Workflow

### 1. Setup

- Read the issue body and linked context.
- Confirm the current branch and worktree are Sandman-managed.
- Do not re-bootstrap the repository.

### 2. Plan

- Read the issue, supporting docs, and relevant code.
- Use parallel readers when the reads do not depend on each other.
- Synthesize one behavior-first plan from the results.
- Design for testability.

### 3. Implement

- Use a vertical TDD loop: one test, one fix, repeat.
- Keep the test surface public and behavior-focused.
- Run tests and formatting after each slice.
- Do not commit during the TDD loop.

### 4. Commit

- Commit the TDD changes before moving on.

### 5. Review

- Review the diff against the issue.
- Fix blockers first, then suggestions, then nits.
- Run tests again after any fixup.

### 6. Push and PR

- Push the branch and open the PR when the implementation is ready.
- Use the review command Sandman provides when delegating PR review.

### 7. Merge or stop

- Continue review until the PR is merged or merge is impossible after exhausting review.
- If the workflow requires it, write continuation context before exiting.
