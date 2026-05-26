# Default Prompt

Sandman's canonical prompt lives in `internal/prompt/default_prompt.md`. `sandman init` copies it to `.sandman/prompt.md`, which becomes the Project Prompt Template you customize per repo. Sandman then renders that template into `.sandman/rendered-prompt.md` and passes the rendered Prompt to the agent.

The long workflow now lives in the shared Sandman skill. This page describes the bootstrap prompt that passes issue context and runtime values to that skill. See [Sandman Skills](skills.md) for the shared workflow itself.

## Canonical prompt

<!-- default-prompt:start -->
    # Task

    Implement issue #{{ISSUE_NUMBER}}: {{ISSUE_TITLE}}

    ## Context

    {{ISSUE_BODY}}

    ## Runtime Context

    - Work in the current Sandman-created worktree on `{{BRANCH}}` (`{{SOURCE_BRANCH}}`).
    - Base branch: `{{BASE_BRANCH}}`.
    - Review command: `{{REVIEW_COMMAND}}`.
    - Follow the installed `sandman` skill for the full plan, implementation, review, merge, and continuation workflow.
<!-- default-prompt:end -->

## What each part does

- `Task` names the work and injects the issue number/title.
- `Context` passes the raw issue body through unchanged.
- `Runtime Context` passes the current worktree branch, base branch, and review command into the shared skill.
- `{{BRANCH}}` and `{{SOURCE_BRANCH}}` identify the run branch.
- `{{REVIEW_COMMAND}}` resolves from config or `--review-command` and defaults to `/oc review`.

## Prompt lifecycle

- **Default Prompt**: Sandman's embedded bootstrap prompt.
- **Project Prompt Template**: `.sandman/prompt.md`, created from the Default Prompt during `sandman init` and materialized on run when missing.
- **Sandman Skill**: the shared workflow installed into `~/.agents/skills/sandman/SKILL.md` by `sandman init`.
- **Prompt**: `.sandman/rendered-prompt.md`, the rendered instruction file handed to the agent.
- **Continue replay**: `sandman continue` reuses stored branch, base branch, agent, model, and review command from the prior run. It ignores current base-branch config changes, then prepends `.sandman/continuation-context.md` to `.sandman/continue-prompt.md` when present.
