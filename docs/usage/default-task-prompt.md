# Default Task Prompt

Sandman's canonical task prompt lives in `internal/prompt/default-task-prompt.md`. `sandman init` copies it to `.sandman/prompt.md`, which becomes the Project Prompt Template you customize per repo. Sandman then renders that template into `.sandman/task.md` and passes the rendered Task to the agent.

The long workflow now lives in the shared Sandman skill. This page describes the bootstrap prompt that passes issue context and runtime values to that skill. See [Sandman Skills](skills.md) for the shared workflow itself.

## Canonical prompt

<!-- default-task-prompt:start -->
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

    The worktree MUST be checked out on `{{BRANCH}}` when the run finishes. Do not switch to `{{BASE_BRANCH}}` or any other branch before exiting.

    ## Execution Checklist

    - [ ] Create branch
    - [ ] Plan (sandman-plan)
    - [ ] Implement (sandman-implement: execute TDD + commit + self-review + back-merge + create PR + delegate review)
    - [ ] PR-Review (sandman-pr-review)
    - [ ] PR-Merge (sandman-pr-merge)

    After completing each item, update `.sandman/task.md` in place by checking that item off.

    ## Mandatory Execution Contract

    This task must be executed through the Sandman skill workflow, not by ad-hoc implementation.

    1. Load the `sandman` skill.
    2. Use mode `sandman implement`.
    3. When `sandman` routes to a subskill, load that subskill and follow its full workflow, checklist, guardrails, hard rules, preconditions, and stop conditions before moving on.
    4. Treat every `Workflow`, `Checklist`, `Guardrails`, `Hard rule`, `Preconditions`, and `Stop conditions` section in each loaded Sandman subskill as mandatory.
    5. Do not skip, summarize, or replace skill steps with your own shortcut.
    6. If a skill says to load another skill, load it and follow it end to end.
    7. If a step cannot be completed, stop only when the relevant skill says to stop, report the blocker, then still run the continuation step below.

    ## AFK Rule — Absolute

    This is a fully automated Away From Keyboard workflow. **The user will never be available to answer questions, give approval, or make decisions during execution.**

    ### Hard Ban

    You MUST NEVER:
    - Ask the user for approval, confirmation, permission, or decisions.
    - Ask the user "should I proceed?", "ready for next step?", "want me to continue?", or any variant.
    - Ask the user for clarification, feedback, or review.
    - Pause, prompt, or block waiting for user input — **including yes/no questions, confirmations, and rhetorical check-ins**.
    - Stop mid-workflow to report status to the user unless the workflow has reached a terminal stop condition defined by a loaded skill.

    ### Subagent Escape Hatch

    If you genuinely cannot decide what to do next (ambiguous result, conflicting skill instructions, unclear failure mode), do not ask the user. Instead:
    1. **Spawn a subagent** with full context of the decision point.
    2. Ask the subagent to analyze and recommend.
    3. Reach consensus with the subagent.
    4. Proceed automatically.

    This is your only allowed second-opinion mechanism. Never fall back to asking the user.

    ### Satisfying "User Approval" Gates in Skills

    When any loaded skill refers to user approval, user confirmation, or user satisfaction, satisfy that gate by proceeding automatically once tests, formatting, CI, and review gates pass.

    The Required Skill Chain defines specific tools for each review type:

    | Step | Designated Mechanism | Notes |
    |------|-------------------|-------|
    | Plan approval (TDD) | Subagent review + consensus | Only step that explicitly requires subagent review |
    | Self-review | `sandman-self-review` skill | |
    | PR review | `sandman-pr-review` skill | **Must NOT use subagent** |

    **PR review is the only step where subagent review is banned.** Use the `sandman-pr-review` skill instead. Subagent review is recommended for plan approval.

    ### Examples of Banned Questions

    These are all forbidden (non-exhaustive):

    > "Ready for PR review step. Want me to proceed?"
    > "Should I create the PR now?"
    > "Does this look good to you?"
    > "Can I merge?"
    > "What should I do about this test failure?"
    > "The review returned feedback. Should I apply it?"

    All of these MUST be handled autonomously. Use the Subagent Escape Hatch only for genuine decision ambiguity (conflicting instructions, unclear failure mode), not for routine steps that have designated skills.

    ## Search Scope Restriction

    Never run grep, rg, find, or any recursive content/file search against directories outside the current working directory (e.g. /tmp, /var, /usr, /etc, /opt, /home, node_modules, .git, target, dist, build, vendor). Such searches return massive output that floods the context window. Restrict searches to the cwd or explicit sub-paths within it; use the Glob/Grep tools which already scope to the project by default.

    This restriction applies to the current agent and to every subagent invoked in the current session, including subagents launched directly and subagents launched by any Sandman or other skill loaded during the run. When spawning, delegating to, or handing work off to a subagent, pass this Search Scope Restriction into the subagent's instructions verbatim, or reference this section by name, so the subagent obeys the same rule.

    ## Required Skill Chain

    During `sandman implement`, follow all delegated subskills it calls:

    - `sandman-tdd` for planning, subagent-reviewed plan consensus, vertical red-green TDD, and refactor-after-green.
    - `sandman-self-review` for self-review.
    - `sandman-back-merge` before PR creation, with no rebase and no force-push.
    - `sandman-pr-review` for delegated PR review. Do not review the PR yourself.
    - `sandman-pr-merge` only if the PR is fully approved, required checks are green, and GitHub reports it mergeable.

    ## Required Order

    1. Complete checklist items in order: Create branch, Plan, Implement, PR-Review, PR-Merge.
    2. For plan-approval, use subagent review. For self-review, use `sandman-self-review` skill. For PR-review, use `sandman-pr-review` skill — subagent review is banned there. Proceed after consensus/completion. Do not ask the user.
    3. **PR creation is not PR review.** A PR existing does not mean it has been reviewed or is ready to merge. Before loading `sandman-pr-merge`, the agent MUST confirm that `sandman-pr-review` was actually executed and produced a reviewed/approved state. If the last completed step is "PR Created" and the PR is not approved or not mergeable, the agent MUST call `sandman-pr-review` before `sandman-pr-merge` — do not skip the review step. If any merge gate is false or ambiguous, call `sandman-pr-review` and continue the review loop instead of reporting blockers to the user.
    4. If `PR-Review` completes with full approval and all merge gates are true, load and run `sandman-pr-merge`.
    5. If a `sandman-pr-review` pass times out or returns without approval, do not mark `PR-Review` complete and do not advance to `PR-Merge` on the next retry. Re-enter `sandman-pr-review` and keep the review loop open until approval is observed or a stop condition is reached.

    ## Completion Requirements

    Before final response, verify and report:

    - Whether each required skill checklist was completed.
    - Test/format commands run and outcomes.
    - PR URL and review status, if a PR was created.
    - Whether PR merge was performed or skipped, with reason.
<!-- default-task-prompt:end -->

## What each part does

- `Task` names the work and injects the issue number/title.
- `Issue Context` passes the raw issue body through unchanged.
- `Runtime Context` passes branch, base, and review metadata into the shared workflow.
- `Mandatory Execution Contract` forces the agent to load and obey the Sandman skill chain.
- `AFK Rule — Absolute` replaces human approval with subagent consensus for plan approval, and bans subagent use for PR review (must use `sandman-pr-review` skill). Self-review uses `sandman-self-review` skill.
- `Search Scope Restriction` keeps recursive search (grep, rg, find) bounded to the working directory and explicitly named sub-paths, so agent context is not flooded by scans of system folders. The rule propagates: the agent must forward it to every subagent it spawns or hands work off to, including subagents launched by Sandman or other loaded skills.
- `Required Skill Chain` names the mandatory subskills the agent must follow.
- `Required Order` makes the sequence explicit, including continuation before exit and merge only when gates are true.
- `Completion Requirements` define what the agent must report at the end.

## Prompt lifecycle

- **Default Task Prompt**: Sandman's embedded bootstrap prompt.
- **Project Prompt Template**: `.sandman/prompt.md`, created from the Default Task Prompt during `sandman init` and materialized on run when missing.
- **Sandman Skill**: the shared skill folder installed into `~/.agents/skills/sandman/` by `sandman init`.
- **Prompt**: `.sandman/task.md`, the rendered instruction file handed to the agent.
- **Continue replay**: `sandman run --continue` reuses the stored branch, base branch, agent, and review command from the prior run. It reads the task file (`.sandman/task.md`) from the worktree and passes its contents verbatim as the agent's resume prompt. When no task file exists, an empty task template is used with a warning on stderr.
