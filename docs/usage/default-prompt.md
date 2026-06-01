# Default Prompt

Sandman's canonical prompt lives in `internal/prompt/default_prompt.md`. `sandman init` copies it to `.sandman/prompt.md`, which becomes the Project Prompt Template you customize per repo. Sandman then renders that template into `.sandman/rendered-prompt.md` and passes the rendered Prompt to the agent.

The long workflow now lives in the shared Sandman skill. This page describes the bootstrap prompt that passes issue context and runtime values to that skill. See [Sandman Skills](skills.md) for the shared workflow itself.

## Canonical prompt

<!-- default-prompt:start -->
    # Task

    Implement GitHub issue #{{ISSUE_NUMBER}}: {{ISSUE_TITLE}}

    ## Issue Context

    {{ISSUE_BODY}}

    ## Runtime Context

    - You are running inside a Sandman-created worktree.
    - Current branch: `{{BRANCH}}`
    - Source branch: `{{SOURCE_BRANCH}}`
    - Base branch: `{{BASE_BRANCH}}`
    - Review command: `{{REVIEW_COMMAND}}`

    ## Mandatory Execution Contract

    This task must be executed through the Sandman skill workflow, not by ad-hoc implementation.

    1. Load the `sandman` skill.
    2. Use mode `sandman implement`.
    3. When `sandman` routes to a subskill, load that subskill and follow its full workflow, checklist, guardrails, hard rules, preconditions, and stop conditions before moving on.
    4. Treat every `Workflow`, `Checklist`, `Guardrails`, `Hard rule`, `Preconditions`, and `Stop conditions` section in each loaded Sandman subskill as mandatory.
    5. Do not skip, summarize, or replace skill steps with your own shortcut.
    6. If a skill says to load another skill, load it and follow it end to end.
    7. If a step cannot be completed, stop only when the relevant skill says to stop, report the blocker, then still run the continuation step below.

    ## AFK Rule

    This is an Away From Keyboard workflow. Do not ask the user for approval, confirmation, or decisions during execution unless a skill stop condition makes progress impossible.

    When any Sandman skill refers to user approval, user confirmation, or user satisfaction, satisfy that gate by:

    - Asking a subagent to review the plan or result.
    - Reaching consensus with the subagent.
    - Proceeding automatically once tests, formatting, CI, and review gates pass.

    For TDD planning, load `sandman-tdd`, draft the plan, ask a subagent to review it, revise until consensus, then proceed automatically. Do not wait for human approval.

    ## Required Skill Chain

    During `sandman implement`, follow all delegated subskills it calls:

    - `sandman-tdd` for planning, subagent-reviewed plan consensus, vertical red-green TDD, and refactor-after-green.
    - `sandman-review` for self-review.
    - `sandman-merge` before PR creation, with no rebase and no force-push.
    - `sandman-pr-review` for delegated PR review. Do not review the PR yourself. Use the configured review command and collect all top-level, review-summary, and inline feedback.
    - `sandman-continuation` before exit.
    - `sandman-pr-merge` only if the PR is fully approved, required checks are green, and GitHub reports it mergeable.

    ## Required Order

    1. Run `sandman implement` and complete every required step in `sandman-implement` and all subskills it loads.
    2. For any plan-approval, confirmation, or satisfaction step, use subagent review and proceed after consensus. Do not ask the user.
    3. Always load and run `sandman-continuation` before exiting, even if implementation, tests, CI, review, or merge failed.
    4. If the delegated PR review completed with full approval and all merge gates are true, load and run `sandman-pr-merge`.
    5. If any merge gate is false or ambiguous, do not merge. Leave the PR open and report blockers in the continuation context.

    ## Completion Requirements

    Before final response, verify and report:

    - Which Sandman subskills were loaded.
    - Whether each required skill checklist was completed.
    - Test/format commands run and outcomes.
    - PR URL and review status, if a PR was created.
    - Whether `.sandman/continuation-context.md` was written.
    - Whether PR merge was performed or skipped, with reason.
<!-- default-prompt:end -->

## What each part does

- `Task` names the work and injects the issue number/title.
- `Issue Context` passes the raw issue body through unchanged.
- `Runtime Context` passes branch, base, and review metadata into the shared workflow.
- `Mandatory Execution Contract` forces the agent to load and obey the Sandman skill chain.
- `AFK Rule` replaces human approval with subagent consensus.
- `Required Skill Chain` names the mandatory subskills the agent must follow.
- `Required Order` makes the sequence explicit, including continuation before exit and merge only when gates are true.
- `Completion Requirements` define what the agent must report at the end.

## Prompt lifecycle

- **Default Prompt**: Sandman's embedded bootstrap prompt.
- **Project Prompt Template**: `.sandman/prompt.md`, created from the Default Prompt during `sandman init` and materialized on run when missing.
- **Sandman Skill**: the shared skill folder installed into `~/.agents/skills/sandman/` by `sandman init`.
- **Prompt**: `.sandman/rendered-prompt.md`, the rendered instruction file handed to the agent.
- **Continue replay**: `sandman continue` reuses stored branch, base branch, agent, and review command from the prior run. It ignores current base-branch config changes, resolves the model from `--model` or `default_model`, then prepends `.sandman/continuation-context.md` to `.sandman/continue-prompt.md` when present.
