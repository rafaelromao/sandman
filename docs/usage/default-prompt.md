# Default Prompt

Sandman's canonical prompt lives in `internal/prompt/default_prompt.md`. `sandman init` copies it to `.sandman/prompt.md`, which becomes the Project Prompt Template you customize per repo. Sandman then renders that template into `.sandman/rendered-prompt.md` and passes the rendered Prompt to the agent.

## Canonical prompt

<!-- default-prompt:start -->
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
    - Commit all changes from the TDD cycle before moving on. Do not proceed to self-review, push, or PR creation until the current TDD changes are committed.
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
    - If `{{DEFAULT_BRANCH}}` was rewritten after this branch diverged, rebase onto it before opening the PR.
    - If that rebase causes conflicts or the branch is already published and would require force-push, stop and report it instead of forcing the push.
    ```bash
    git rebase {{DEFAULT_BRANCH}}
    git push -u origin {{BRANCH}}
    gh pr create --base {{DEFAULT_BRANCH}} --head {{BRANCH}} --title "{{ISSUE_TITLE}}" --body "Fixes #{{ISSUE_NUMBER}}"
    ```

    - Capture the PR URL and number.

    ### 6.5 Completion Gate
    - PR creation is a checkpoint, not completion.
    - Do not stop, summarize, or return a final result after `gh pr create`.
    - Immediately continue into steps 7 and 8.
    - The only valid end states are:
      - PR merged and verified
      - merge impossible after exhausting the review loop, with the PR left open
    - If you feel done after PR creation, ignore that impulse and continue.

    ### 7. Delegate review (max 10 passes)
    - Poll `gh pr checks <N>` until CI passes. Do not exit while checks are pending.
    - If checks fail, fix them, commit, and push.
    - Post `gh pr comment <N> --body "{{REVIEW_COMMAND}}"`
    - Poll `gh pr view <N> --comments` every 30-60s, with a 10 minute timeout.
    - If no review response arrives in time, stop and report the PR as still open.
    - When feedback arrives, cluster independent comments and fix them in parallel where possible.
    - Address blockers first, then suggestions, then nits.
    - Apply fixes, run tests/formatting, commit, and push.
    - Repeat this loop until approved or max 10 passes.
    - Do NOT review your own PR, delegate exclusively to `{{REVIEW_COMMAND}}`.

    ### 7.5 Completion Gate
    - Review is a checkpoint, not completion.
    - Do not stop, summarize, or return a final result after the review loop.
    - Immediately continue into step 8.
    - If the PR is not fully approved, or checks are not green, or GitHub does not report it mergeable, do not merge.
    - The only valid end states are:
      - PR merged and verified
      - merge impossible after exhausting the review loop, with the PR left open

    ### 8. Merge and finish
    - Only merge when all of these are true:
      - the PR is fully approved
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
<!-- default-prompt:end -->

## What each part does

- `Task` names the work and injects the issue number/title.
- `Context` passes the raw issue body through unchanged.
- `Setup` keeps the agent inside the current Sandman worktree and forbids re-bootstrap.
- `Plan (TDD)` asks for behavior-first planning and parallel independent reads.
- `Implement (TDD)` keeps the loop vertical and strictly sequential: one test, one fix, repeat.
- `Commit`, `Self-review`, `Push & PR`, `Delegate review`, and `Merge & wrap-up` encode Sandman's PR workflow.
- `Push & PR` now asks for a rebase onto `{{DEFAULT_BRANCH}}` before opening the PR, to reduce merge conflict risk.
- `{{BRANCH}}` and `{{DEFAULT_BRANCH}}` are branch aliases filled from the run branch and default branch.
- `{{REVIEW_COMMAND}}` resolves from config or `--review-command` and defaults to `/oc review`.

## Prompt lifecycle

- **Default Prompt**: Sandman's embedded source of truth.
- **Project Prompt Template**: `.sandman/prompt.md`, created from the Default Prompt during `sandman init` and materialized on run when missing.
- **Prompt**: `.sandman/rendered-prompt.md`, the rendered instruction file handed to the agent.
- **Continue replay**: `sandman continue` reuses stored branch, agent, model, and review command from the prior run, then writes a raw prompt to `.sandman/continue-prompt.md`.
