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

## Commit and PR Title

The change-request title (and the commit subject) must follow the Conventional Commits format. Pick the most accurate type for the change from `feat`, `fix`, `perf`, `docs`, `refactor`, `test`, `build`, `ci`, `chore`, `revert`. Append `!` to the type for breaking changes. The full regex and allowed-types list are documented in `AGENTS.md`'s "Branching and versioning rules" section — read it before opening the change request. The `CI / semantic-pull-request` status check on `{{BASE_BRANCH}}` enforces the regex; titles that fall outside the format will block merge. Reuse the same Conventional Commits header for both the commit and the change request so the merge button sees one coherent signal.

## Execution Checklist

- [ ] Create branch
- [ ] Plan (Load `sandman-plan`)
- [ ] Implement (Load `sandman-implement`)
- [ ] PR-Review (Load `sandman-pr-review`)
- [ ] PR-Merge (Load `sandman-pr-merge`)

Each checklist step is a skill-load directive. The agent MUST emit a `Skill "sandman-<name>"` invocation in its transcript before doing any work for that step; the step is not considered started until the skill is loaded. Steps completed without their skill loaded are invalid — for example, a `gh pr create` body composed without first loading `sandman-implement` does not satisfy the closing-reference body requirement (one of `Closes #<issue_number>`, `Fixes #<issue_number>`, `Resolves #<issue_number>`) and is not acceptable.

Before moving on, check which checklist items are already complete in `.sandman/task.md`. If an item is already checked, treat it as complete and skip it instead of repeating the work.

After checking off an item, update `.sandman/task.md` in place and rewrite the registered `## Next Step` so it points at the next unchecked checklist item.

## Next Step

The registered next step is the first unchecked item in the Execution Checklist.

## Already Resolved

If the issue is already implemented on `{{BASE_BRANCH}}`, after fetching and checking the current `origin/{{BASE_BRANCH}}` HEAD against the issue acceptance criteria, update `.sandman/task.md` so it contains the exact line `## Status: already resolved`.

Write `## Status: already resolved` only if every AC has a corresponding test that exists on `origin/{{BASE_BRANCH}}`; otherwise the orchestrator cannot verify and the run will fail.

Do not paraphrase this line. Do not use `already implemented`, `no action required`, or any other wording for this marker.

If a PR is open for the current branch, the orchestrator will run an independent verification pass against `origin/{{BASE_BRANCH}}` before declaring the run successful.

## Success-Blocking Conditions

The run is NOT considered successful (and `## Status: already resolved` MUST NOT be written) while any of the following are true:

- **Open PR with no verification path** — An open PR exists for the branch AND the issue's ACs do not map to tests on `origin/{{BASE_BRANCH}}` (the orchestrator's verifier cannot decide the run).
- **`mergeable: CONFLICTING`** — the branch's open PR is in a conflict state with the base branch.
- **Unpushed commits** — `git log @{u}..HEAD` (or `git log origin/{{BASE_BRANCH}}..HEAD` for a new branch) is non-empty; the local branch has commits the remote does not.
- **Unresolved AC blocker** — any acceptance criterion in the issue body is unmet, contested, or marked blocked by another open issue.
- **PR not approved** — an open PR exists for the branch AND the PR does not have Approval (`reviewDecision !== 'APPROVED'` AND no informal approval per `sandman-pr-review` Step 6 case C). The orchestrator must not declare the run successful while review is unresolved.
- **PR not approved for the current diff** — even when `reviewDecision` or a top-level comment shows an old APPROVED, the approval is stale if it was posted against a prior head SHA (issue #2309). The approval must be against the head SHA recorded at the last `{{REVIEW_COMMAND}}` post — see `sandman-pr-review` Step 6 Case C's approval-recency gate.
- **Unanswered `/sandman review` trigger** — an open PR exists for the branch AND the most recent top-level comment is an implementor `{{REVIEW_COMMAND}}` trigger that has not yet received a response (no formal review, no inline file comment, no other top-level body from a non-agent author). An older APPROVED comment sitting below an unanswered trigger is not sufficient; the trigger is a fresh request and must be answered before the run is considered approved.

Re-check this block immediately before writing `## Status: already resolved`. If any condition is true, abort the marker and address the underlying problem (close orphan PR, back-merge, push commits, or resolve the blocker).

## Mandatory Execution Contract

This task must be executed through the Sandman skill workflow, not by ad-hoc implementation.

1. Load the `sandman` skill.
2. Use mode `sandman implement`.
3. Load `sandman-implement` itself. The closing-reference body rule (Hard Rule 3 of `sandman-implement`), the back-merge step, and the closing-reference verification step live inside that skill; do not attempt to recreate them from memory.
4. When `sandman` routes to a subskill, load that subskill and follow its full workflow, checklist, guardrails, hard rules, preconditions, and stop conditions before moving on.
5. Treat every `Workflow`, `Checklist`, `Guardrails`, `Hard rule`, `Preconditions`, and `Stop conditions` section in each loaded Sandman subskill as mandatory.
6. Do not skip, summarize, or replace skill steps with your own shortcut.
7. If a skill says to load another skill, load it and follow it end to end.
8. If a step cannot be completed, stop only when the relevant skill says to stop, report the blocker, then still run the continuation step below.
9. **Skill-loading gate.** Before running the first command of each Execution Checklist step, the agent MUST emit a `Skill "sandman-<name>"` invocation in its transcript. A step is not considered started until that skill is loaded. Steps completed without their skill loaded are invalid and must be re-done with the skill loaded.

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
| Self-review | `sandman-self-review` skill |
| PR review | `sandman-pr-review` skill | **Must NOT use subagent**

**PR review is the only step where subagent review is banned.** Use the `sandman-pr-review` skill instead. Subagent review is recommended for plan approval.

### Examples of Banned Questions

These are all forbidden (non-exhaustive):

> "Ready for PR review step. Want me to proceed?"
> "Should I create the PR now?"
> "Does this look good to you?"
> "Can I merge?"
> "What should I do about this test failure?"
> "The review returned feedback. Should I apply it?"

All of these MUST be handled autonomously. Use the Subagent Escape Hatch for genuine decision ambiguity or as delegated in the table above.

## Search Scope Restriction

Never run grep, rg, find, or any recursive content/file search against directories outside the current working directory (e.g. /tmp, /var, /usr, /etc, /opt, /home, node_modules, .git, target, dist, build, vendor). Such searches return massive output that floods the context window. Restrict searches to the cwd or explicit sub-paths within it; use the Glob/Grep tools which already scope to the project by default.

This restriction applies to the current agent and to every subagent invoked in the current session, including subagents launched directly and subagents launched by any Sandman or other skill loaded during the run. When spawning, delegating to, or handing work off to a subagent, pass this Search Scope Restriction into the subagent's instructions verbatim, or reference this section by name, so the subagent obeys the same rule.

## Required Skill Chain

Load `sandman-implement` first; it owns the end-to-end implement workflow (TDD, commits, self-review, back-merge, PR creation, review delegation, merge) and delegates to the subskills below. Follow each delegated subskill in the order it is called:

- `sandman-implement` — end-to-end implement workflow. Must be loaded before any implementation work begins. Owns the closing-reference body rule, the back-merge step, and the post-create body verification.
- `sandman-tdd` for planning, subagent-reviewed plan consensus, vertical red-green TDD, and refactor-after-green.
- `sandman-self-review` for self-review.
- `sandman-back-merge` before PR creation, with no rebase and no force-push.
- `sandman-pr-review` for delegated PR review. Do not review the PR yourself.
- `sandman-pr-merge` only if the PR is fully approved, required checks are green, and GitHub reports it mergeable.

## Required Order

1. Complete checklist items in order: Create branch, Plan, Implement, PR-Review, PR-Merge.
2. For plan-approval, use subagent review. For self-review, use `sandman-self-review` skill. For PR-review, use `sandman-pr-review` skill — subagent review is banned there. Proceed after consensus/completion. Do not ask the user.
3. **PR creation is not PR review.** A PR existing does not mean it has been reviewed or is ready to merge. Before loading `sandman-pr-merge`, the agent MUST confirm that `sandman-pr-review` was actually executed and produced a reviewed/approved state. If the last completed step is "PR Created" and the PR is not approved or not mergeable, the agent MUST call `sandman-pr-review` before `sandman-pr-merge` — do not skip the review step. If any merge gate is false or ambiguous, call `sandman-pr-review` and continue the review loop instead of reporting blockers to the user.
4. **PR-Review is `[x]` only when the PR has Approval against the current diff.** `PR-Review` cannot be marked complete on the basis of exhausted review passes, timeouts, or zero reviewer responses. The check is a concrete signal: the PR has Approval (`reviewDecision === 'APPROVED'` OR informal approval per `sandman-pr-review` Step 6 case C) **and that approval was posted against the current head SHA** (issue #2309). An APPROVED comment from a prior SHA is stale — the prior approval was issued against a different diff and does not authorize merging the current one. Until a fresh Approval is observed against the head SHA recorded at the last `{{REVIEW_COMMAND}}` post, leave the checkbox unchecked and keep the review loop open — even if every other item is checked. Marking `PR-Review` `[x]` on a stale approval is the failure mode that strands a run at PR-Merge after a back-merge with no path forward.
5. If `PR-Review` completes with full approval and all merge gates are true, load and run `sandman-pr-merge`.
6. If a `sandman-pr-review` pass times out or returns without approval, do not mark `PR-Review` complete and do not advance to `PR-Merge` on the next retry. Re-enter `sandman-pr-review` and keep the review loop open until approval is observed or a stop condition is reached.
7. **A new commit resets the review pass counter.** If the agent pushed a new commit to the PR branch (head SHA changed) after the last review post, the prior exhausted pass budget is stale — the reviewer is being asked to evaluate a new diff. Re-enter `sandman-pr-review` with a fresh 10-pass budget for the new SHA, regardless of how many passes the prior SHA consumed. This applies intra- and inter-session: any SHA change restarts the counter.
8. **On retry, the prior pass budget does not carry over.** Each new agent session (e.g., `sandman run --continue` or any re-entry of the run) starts with a fresh 10-pass budget for `sandman-pr-review` — the in-session pass counter is not persisted across sessions. Treat any prior `[x] PR-Review` in `.sandman/task.md` as untrusted state from a prior session: re-verify the PR has Approval NOW before accepting the box as complete. If no Approval is observed, uncheck the box and re-enter `sandman-pr-review`.

## Completion Requirements

Before final response, verify and report:

- Whether each required skill checklist was completed.
- Test/format commands run and outcomes.
- PR URL and review status, if a PR was created.
- Whether PR merge was performed or skipped, with reason.

