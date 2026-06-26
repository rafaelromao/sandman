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

## AFK Rule

This is an Away From Keyboard workflow. Do not ask the user for approval, confirmation, or decisions during execution unless a skill stop condition makes progress impossible.

When any Sandman skill refers to user approval, user confirmation, or user satisfaction, satisfy that gate by:

- Asking a subagent to review the plan or result.
- Reaching consensus with the subagent.
- Proceeding automatically once tests, formatting, CI, and review gates pass.

For TDD planning, load `sandman-tdd`, draft the plan, ask a subagent to review it, revise until consensus, then proceed automatically. Do not wait for human approval.

## Search Scope Restriction

Never run grep, rg, find, or any recursive content/file search against directories outside the current working directory (e.g. /tmp, /var, /usr, /etc, /opt, /home, node_modules, .git, target, dist, build, vendor). Such searches return massive output that floods the context window. Restrict searches to the cwd or explicit sub-paths within it; use the Glob/Grep tools which already scope to the project by default.

This restriction applies to the current agent and to every subagent invoked in the current session, including subagents launched directly and subagents launched by any Sandman or other skill loaded during the run. When spawning, delegating to, or handing work off to a subagent, pass this Search Scope Restriction into the subagent's instructions verbatim, or reference this section by name, so the subagent obeys the same rule.

## Required Skill Chain

During `sandman implement`, follow all delegated subskills it calls:

- `sandman-tdd` for planning, subagent-reviewed plan consensus, vertical red-green TDD, and refactor-after-green.
- `sandman-self-review` for self-review.
- `sandman-back-merge` before PR creation, with no rebase and no force-push.
- `sandman-pr-review` for delegated PR review. Do not review the PR yourself. Use the configured review command and collect all top-level, review-summary, and inline feedback.
- `sandman-pr-merge` only if the PR is fully approved, required checks are green, and GitHub reports it mergeable.

## Required Order

1. Complete checklist items in order: Create branch, Plan, Implement, PR-Review, PR-Merge.
2. For any plan-approval, confirmation, or satisfaction step, use subagent review and proceed after consensus. Do not ask the user.
3. **PR creation is not PR review.** A PR existing does not mean it has been reviewed or is ready to merge. Before loading `sandman-pr-merge`, the agent MUST confirm that `sandman-pr-review` was actually executed and produced a reviewed/approved state. If the last completed step is "PR Created" and the PR is not approved or not mergeable, the agent MUST call `sandman-pr-review` before `sandman-pr-merge` — do not skip the review step. If any merge gate is false or ambiguous, do not merge. Leave the PR open and report blockers.
4. If `PR-Review` completes with full approval and all merge gates are true, load and run `sandman-pr-merge`.

## Completion Requirements

Before final response, verify and report:

- Whether each required skill checklist was completed.
- Test/format commands run and outcomes.
- PR URL and review status, if a PR was created.
- Whether PR merge was performed or skipped, with reason.
