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
    - To implement the issue, call `sandman implement` first. Write continuation context by loading the `sandman-continuation`. Do this even if the previous step failed. If the PR is fully approved, call `sandman pr-merge` next.
<!-- default-prompt:end -->

## What each part does

- `Task` names the work and injects the issue number/title.
- `Context` passes the raw issue body through unchanged.
- `Runtime Context` passes the current worktree branch and the continuation instructions into the shared skill.
- `{{BRANCH}}` and `{{SOURCE_BRANCH}}` identify the run branch.
- The continuation-context instruction tells the agent to write `.sandman/continuation-context.md` after implement, even on failure.

## Prompt lifecycle

- **Default Prompt**: Sandman's embedded bootstrap prompt.
- **Project Prompt Template**: `.sandman/prompt.md`, created from the Default Prompt during `sandman init` and materialized on run when missing.
- **Sandman Skill**: the shared skill folder installed into `~/.agents/skills/sandman/` by `sandman init`.
- **Prompt**: `.sandman/rendered-prompt.md`, the rendered instruction file handed to the agent.
- **Continue replay**: `sandman continue` reuses stored branch, base branch, agent, model, and review command from the prior run. It ignores current base-branch config changes, then prepends `.sandman/continuation-context.md` to `.sandman/continue-prompt.md` when present.
