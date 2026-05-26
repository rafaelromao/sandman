# Sandman Skill

## Mission

Implement the current GitHub issue using Sandman's repository workflow.

## Workflow

1. Read the issue body and any linked context from the bootstrap prompt.
1. Stay on the branch Sandman already prepared.
1. Use parallel readers for independent issue/docs and codebase/test exploration.
1. Build one behavior at a time with TDD: one test, one minimal fix, repeat.
1. Keep tests focused on public behavior, not implementation details.
1. Run the relevant tests and formatting after each green step.
1. Commit the TDD result before starting self-review.
1. Self-review the diff against the issue, apply fixes, then rerun the full relevant test set.
1. Use the review command supplied in the bootstrap prompt when entering the delegated review loop.
1. Merge only after approval, green checks, and a mergeable PR.

## Guardrails

- Do not re-bootstrap Sandman.
- Do not restart the issue discovery flow.
- Do not widen scope beyond the current repository codebase.
- Keep the workflow behavior-first and sequential inside the TDD loop.
