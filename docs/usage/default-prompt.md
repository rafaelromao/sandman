# Default Prompt

Sandman's canonical prompt lives in `internal/prompt/default_prompt.md`. `sandman init` copies it to `.sandman/prompt.md`, which becomes the Project Prompt Template you customize per repo. Sandman then renders that template into `.sandman/rendered-prompt.md` and passes the rendered Prompt to the agent.

## Canonical prompt

<!-- default-prompt:start -->
    # Task

    Implement issue #{{ISSUE_NUMBER}}: {{ISSUE_TITLE}}

    ## Context

    {{ISSUE_BODY}}

    ## Approach

    ### 1. Setup
    - Sandman already rendered the issue content and created the current worktree/branch.
    - Work in the current Sandman-created worktree on `{{BRANCH}}`.
    - Do not run `gh issue view {{ISSUE_NUMBER}}`, `git checkout main`, `git pull`, or create a new branch.
    - If a toolchain is missing, use mise first before adding ad hoc installs.

    ### 2. Plan (TDD)
    - Read the issue body and linked context, respecting ADRs and domain glossary
    - Design testable public interfaces (deep modules, small interface)
    - List behaviors to test (not implementation steps)
    - **No horizontal slicing**: one test -> one impl -> repeat

    ### 3. Implement (TDD — vertical tracer bullets)
    - RED: write one test for one behavior -> fails
    - GREEN: write minimal code to pass -> passes
    - Repeat for each behavior
    - Keep tests at public interface level, not implementation details
    - Run project tests and formatting after each cycle
    - Do NOT commit during TDD

    ### 4. Commit
    ```bash
    git add -A
    git commit -m "feat: <issue title>"
    ```

    ### 5. Self-review
    Review the diff against the originating issue. For each file/hunk:
    - Does it implement what the issue asked for? (Spec)
    - Does it follow repo conventions? (Standards — CLAUDE.md, CONTRIBUTING.md, CONTEXT.md, ADRs, tooling configs)
    - Apply fixes, run tests/formatting, commit:
    ```bash
    git add -A
    git commit -m "refactor: self-review fixes"
    ```

    ### 6. Push & PR
    ```bash
    git push -u origin {{BRANCH}}
    gh pr create --base {{DEFAULT_BRANCH}} --head {{BRANCH}} --title "{{ISSUE_TITLE}}" --body "Fixes #{{ISSUE_NUMBER}}"
    ```

    ### 7. Delegate review (max 10 passes)
    - Poll `gh pr checks <N>` until CI passes (fix failures if needed, commit & push)
    - Post `gh pr comment <N> --body "{{REVIEW_COMMAND}}"`
    - Poll `gh pr view <N> --comments` every 30-60s (5 min timeout)
    - Classify feedback: blockers (must fix), suggestions (fix if straightforward), nits (fix if trivial)
    - Apply fixes, run tests/formatting, commit, push
    - Repeat from step 2 until approved or max 10 passes
    - **Do NOT review your own PR** — delegate exclusively to opencode
<!-- default-prompt:end -->

## What each part does

- `Task` names the work and injects the issue number/title.
- `Context` passes the raw issue body through unchanged.
- `Setup` keeps the agent inside the current Sandman worktree and forbids re-bootstrap.
- `Plan (TDD)` asks for behavior-first planning.
- `Implement (TDD)` keeps the loop vertical: one test, one fix, repeat.
- `Commit`, `Self-review`, `Push & PR`, and `Delegate review` encode Sandman's PR workflow.
- `{{BRANCH}}` and `{{DEFAULT_BRANCH}}` are branch aliases filled from the run branch and default branch.
- `{{REVIEW_COMMAND}}` resolves from config or `--review-command` and defaults to `/oc review`.

## Prompt lifecycle

- **Default Prompt**: Sandman's embedded source of truth.
- **Project Prompt Template**: `.sandman/prompt.md`, created from the Default Prompt during `sandman init` and materialized on run when missing.
- **Prompt**: `.sandman/rendered-prompt.md`, the rendered instruction file handed to the agent.
- **Retry replay**: `sandman retry` reuses stored prompt inputs from the prior run when they were recorded.
