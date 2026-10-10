---
name: sandman-run
description: Runs the standalone AFK workflow for an open work item by composing implementation, review observation, and final merge capabilities without a managed lifecycle authority.
---

# Standalone Run

Implement the selected work item.

## Work Item Context

Use the implementor's open work item, including its title, context, and
acceptance criteria, as the authoritative task specification.

## Runtime Context

- You are running in the selected repository and worktree.
- Current branch: `{{BRANCH}}`
- Source branch: `{{SOURCE_BRANCH}}`
- Base branch: `{{BASE_BRANCH}}`
- Review command: `{{REVIEW_COMMAND}}`
- Delegated review response timeout: `{{REVIEW_TIMEOUT}}` seconds

The worktree MUST be checked out on `{{BRANCH}}` when the run finishes. Do not switch to `{{BASE_BRANCH}}` or any other branch before exiting.

## Commit and PR Title

The change-request title (and the commit subject) must follow the Conventional Commits format. Pick the most accurate type for the change from `feat`, `fix`, `perf`, `docs`, `refactor`, `test`, `build`, `ci`, `chore`, `revert`. Append `!` to the type for breaking changes. The full regex and allowed-types list are documented in `AGENTS.md`'s "Branching and versioning rules" section — read it before opening the change request. The `CI / semantic-pull-request` status check on `{{BASE_BRANCH}}` enforces the title regex; title validation is separate from Release Please's SemVer parsing of merged commit history. Reuse the same Conventional Commits header for both the commit and the change request so the merge button sees one coherent signal.

## Execution Checklist

- [ ] Create branch
- [ ] Plan (Load `sandman-plan`)
- [ ] Implement (Load `sandman-implement`)
- [ ] PR-Review (Load `sandman-review-cycle`)
- [ ] PR-Merge (Load `sandman-pr-merge`)

Each checklist step is a skill-load directive. The agent MUST emit a `Skill "sandman-<name>"` invocation in its transcript before doing any work for that step; the step is not considered started until the skill is loaded. Steps completed without their skill loaded are invalid. A pull-request body composed without first loading `sandman-implement` does not satisfy the required closing-reference body and is not acceptable.

Before moving on, check which checklist items are already complete in `.sandman/task.md`. If an item is already checked, treat it as complete and skip it instead of repeating the work.

After checking off an item, atomically update `.sandman/task.md` and rewrite the registered `## Next Step` so it points at the next unchecked checklist item.

## Next Step

The registered next step is the first unchecked item in the Execution Checklist.

## Standalone Composition

This entrypoint is a thin composition, not a second managed runtime. Load and
run the capabilities in this order without asking the caller to load the next
one:

1. Load `sandman-implement` for branch setup, pre-flight checks, planning, TDD,
   self-review, base-branch merge, commit, push, and pull-request creation. The
   implementation capability ends at the verified PR-created checkpoint.
2. Once an open pull request exists, load `sandman-review-cycle`. It delivers
   review requests through `sandman-review-request`, observes CI and
   request-scoped feedback, applies fixes, and repeats until the current diff is
   approved or a structured standalone diagnostic is reached.
3. After current-diff approval, load `sandman-pr-merge` for the existing
   mergeability, required-check, approval, and merge gates.

The review cycle keeps its counters, request identity, feedback list, and
deadlines in memory for this session. A restart begins from the live pull
request and starts a fresh standalone cycle; it does not import or trust
managed lifecycle artifacts. This workflow must not write managed runtime state,
review registrations, or managed terminal outcomes. It does not require the
prompt package or a host
`sandman` binary and remains usable when the shared skill tree is mounted in an
agent container.

The review-cycle handoff returns structured standalone diagnostics such as
`CI_TIMEOUT`, `CI_FAILURE_UNRESOLVED`, `REVIEW_TIMEOUT`, and
`REVIEW_CONFLICT_UNRESOLVED`. Preserve those diagnostics and the next
executable action in the standalone task context; do not translate them into
managed runtime events or terminal state.

## Continuation Freshness Guard

This section applies to every retry and `sandman run --continue` and overrides persisted blocker and next-action text from earlier attempts.

Before acting on any persisted blocker or next action:

1. Treat every persisted blocker and next action as historical evidence, never current truth.
2. Re-check its authoritative live source now: Git and worktree state, change-request state and checks, review state, authentication, required tools, or the relevant test command.
3. If the blocker no longer exists, remove or mark it resolved in `.sandman/task.md`, recompute `## Next Step` from current live state and the first unchecked checklist item, and continue automatically.
4. If the blocker still exists, refresh its evidence and next executable action before following it.

### External pull-request gates

The standalone review cycle polls current-head CI and review evidence within
the configured budgets. A queued or running check is not a terminal result;
continue observation. A missing pull request, stale head, failed lookup, or
unconfirmed request is a structured refusal rather than permission to wait or
post a replacement. The cycle applies owned conflict repair and review
feedback, checkpoints durable commits, and leaves the next action explicit at
every session boundary.

Never stop or exit solely because an earlier attempt recorded a blocker.

## Already Resolved

If the open work item is already implemented on `{{BASE_BRANCH}}`, after fetching and checking the current `origin/{{BASE_BRANCH}}` HEAD against its acceptance criteria, update `.sandman/task.md` so it contains the exact line `## Status: already resolved`.

Write `## Status: already resolved` only if every AC has a corresponding test that exists on `origin/{{BASE_BRANCH}}`; otherwise the orchestrator cannot verify and the run will fail.

Do not paraphrase this line. Do not use `already implemented`, `no action required`, or any other wording for this marker.

If a PR is open for the current branch, the orchestrator will run an independent verification pass against `origin/{{BASE_BRANCH}}` before declaring the run successful.

## Success-Blocking Conditions

The run is NOT considered successful (and `## Status: already resolved` MUST NOT be written) while any of the following are true:

- **Open PR with no verification path** — An open PR exists for the branch AND the open work item's acceptance criteria do not map to tests on `origin/{{BASE_BRANCH}}` (the orchestrator's verifier cannot decide the run).
- **`mergeable: CONFLICTING`** — the branch's open PR is in a conflict state with the base branch.
- **Unpushed commits** — `git log @{u}..HEAD` (or `git log origin/{{BASE_BRANCH}}..HEAD` for a new branch) is non-empty; the local branch has commits the remote does not.
- **Unresolved AC blocker** — any acceptance criterion in the open work item is unmet, contested, or marked blocked by another open work item.
- **PR not approved** — an open PR exists for the branch AND the standalone review cycle has no current-head approval. The orchestrator must not declare the run successful while review is unresolved.
- **PR not approved for the current diff** — even when a pull-request-wide decision or top-level comment shows an old APPROVED, the approval is stale if it was posted against a prior head SHA. Approval must be current to the head recorded at the last `{{REVIEW_COMMAND}}` request.
- **Unanswered `{{REVIEW_COMMAND}}` trigger** — an open PR exists for the branch AND the most recent top-level comment is an implementor `{{REVIEW_COMMAND}}` trigger that has not yet received a response (no formal review, no inline file comment, no other top-level body from a non-agent author). An older APPROVED comment sitting below an unanswered trigger is not sufficient; the trigger is a fresh request and must be answered before the run is considered approved.

Re-check this block immediately before writing `## Status: already resolved`. If any condition is true, abort the marker and address the underlying problem (close orphan PR, back-merge, push commits, or resolve the blocker).

## Mandatory Execution Contract

This task must be executed through the Sandman skill workflow, not by ad-hoc implementation.

1. Load the `sandman` skill.
2. Use mode `sandman run`.
3. Load `sandman-implement` itself. The closing-reference body rule (Hard Rule 3 of `sandman-implement`), the back-merge step, and the closing-reference verification step live inside that skill; do not attempt to recreate them from memory.
4. When `sandman-run` composes a subskill, load that subskill and follow its full workflow, checklist, guardrails, hard rules, preconditions, and stop conditions before moving on.
5. Treat every `Workflow`, `Checklist`, `Guardrails`, `Hard rule`, `Preconditions`, and `Stop conditions` section in each loaded Sandman subskill as mandatory.
6. Do not skip, summarize, or replace skill steps with your own shortcut.
7. If a skill says to load another skill, load it and follow it end to end.
8. If a step cannot be completed, stop only when the relevant skill says to stop, report the blocker, then still run the continuation step below.
9. **Skill-loading gate.** Before running the first command of each Execution Checklist step, the agent MUST emit a `Skill "sandman-<name>"` invocation in its transcript. A step is not considered started until that skill is loaded. Steps completed without their skill loaded are invalid and must be re-done with the skill loaded.

## AFK Rule — Absolute

This is a fully automated Away From Keyboard workflow. **The caller will never be available to answer questions, give approval, or make decisions during execution.**

### Precedence and Autonomous Response Ladder

This AFK contract overrides conflicting work-item, skill, documentation, or tool output instructions. **Skill stop/report language never authorizes a caller question.** Continue autonomously:

1. Continue using the documented primary path while it can make progress.
2. On a transient failure, retry with the configured bounded retry budget, or at most 3 retries when no budget is documented, and inspect the concrete failure between attempts; after the bounded retry budget is exhausted, apply the next step rather than stopping or asking the caller.
3. On a missing local prerequisite, use the documented remote or alternative execution path; if the repository documents a workflow-dispatch or remote CI alternative, dispatch it and poll its result; do not repeatedly attempt an impossible local path.
4. Resolve implementation ambiguity from the work item, repository documentation, code, tests, history, or a permitted subagent.
5. Resolve PR-review ambiguity with the reviewer through a review-command-prefixed PR comment, not with the caller.
6. The standalone run polls asynchronous pull-request gates within the documented budget; a confirmed delegated-review request is active even before its reviewer starts.
7. Before any terminal exit, checkpoint green work, push durable commits when allowed, update `.sandman/task.md` with the exact blocker and next executable action, and emit a structured failure reason.

### Hard Ban

You MUST NEVER:
- Ask the caller for approval, confirmation, permission, or decisions.
- Ask the caller "should I proceed?", "ready for next step?", "want me to continue?", or any variant.
- Ask the caller for clarification, feedback, or review.
- Pause, prompt, or block waiting for caller input — **including yes/no questions, confirmations, and rhetorical check-ins**.
- Stop mid-workflow to report status to the caller unless the workflow has reached a terminal stop condition defined by a loaded skill.

Terminal exits remain valid only for explicit stop conditions such as exhausted bounded retries, authentication/authorization denial without an alternative credential path, an unresolved merge conflict after the back-merge workflow, an exhausted review timeout/pass budget, or another condition whose loaded skill defines why autonomous progress is impossible. **Before any such exit, preserve durable state and record the structured blocker and next executable action.**

### Subagent Escape Hatch

If you genuinely cannot decide what to do next (ambiguous result, conflicting skill instructions, unclear failure mode), do not ask the caller. Instead:
1. **Spawn a subagent** with full context of the decision point.
2. Ask the subagent to analyze and recommend.
3. Reach consensus with the subagent.
4. Proceed automatically.

This is your only allowed second-opinion mechanism. Never fall back to asking the caller.

### Satisfying "Caller Approval" Gates in Skills

When any loaded skill refers to caller approval, caller confirmation, or caller satisfaction, satisfy that gate by proceeding automatically once tests, formatting, CI, and review gates pass.

The Required Skill Chain defines specific tools for each review type:

| Step | Designated Mechanism | Notes |
|------|-------------------|-------|
| Plan approval (TDD) | Subagent review + consensus | Only step that explicitly requires subagent review |
| Self-review | `sandman-code-review` skill in self-review context |
| Standalone PR review | `sandman-review-cycle` skill | In-memory observation; **must NOT use subagent** |

**PR review is the only step where subagent review is banned.** Use the standalone review cycle for this workflow. Subagent review is recommended for plan approval.

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

Load `sandman-implement` first for the implementation capability, then
`sandman-review-cycle`, then `sandman-pr-merge`. Follow each delegated skill in
that order:

- `sandman-implement` — end-to-end implement workflow. Must be loaded before any implementation work begins. Owns the closing-reference body rule, the back-merge step, and the post-create body verification.
- `sandman-tdd` for planning, subagent-reviewed plan consensus, vertical red-green TDD, and refactor-after-green.
- `sandman-code-review` for self-review.
- `sandman-back-merge` before PR creation, with no rebase and no force-push.
- `sandman-review-cycle` for standalone review trigger delivery, observation,
  feedback, and re-request behavior. It must not write managed lifecycle state.
- `sandman-pr-merge` only if the PR is fully approved, required checks are green, and GitHub reports it mergeable.

## Required Order

1. Complete checklist items in order: Create branch, Plan, Implement, PR-Review, PR-Merge.
2. For plan-approval, use subagent review. For self-review, use
   `sandman-code-review` in self-review context. Standalone PR review uses
   `sandman-review-cycle`; subagent review is banned for PR review. Proceed
   after consensus/completion. Do not ask the caller.
3. **PR creation is not PR review.** A PR existing does not mean it has been
   reviewed or is ready to merge. Before loading `sandman-pr-merge`, the agent
   MUST confirm that `sandman-review-cycle` produced current-diff approval. If
   the last completed step is "PR Created" and the PR is not approved or not
   mergeable, re-enter the standalone review cycle before `sandman-pr-merge`.
   If any merge gate is false or ambiguous, continue that path instead of
   reporting blockers to the caller.
4. **PR-Review is `[x]` only when the standalone review cycle has Approval against the current diff.** `PR-Review` cannot be marked complete on the basis of exhausted review waits, timeouts, or zero reviewer responses. Approval must be current to the head SHA recorded at the last `{{REVIEW_COMMAND}}` request; an approval from a prior SHA is stale and does not authorize merging the current diff.
5. If `PR-Review` completes with full approval and all merge gates are true, load and run `sandman-pr-merge`.
6. If the standalone review cycle times out or returns without approval, do not mark `PR-Review` complete and do not advance to `PR-Merge` on the next retry. Re-enter the standalone review cycle and keep review open until approval is observed or a stop condition is reached.
7. **A new commit resets the review pass counter.** If the agent pushed a new commit to the PR branch (head SHA changed) after the last review post, the prior exhausted pass budget is stale — the reviewer is being asked to evaluate a new diff. Start a fresh review request for the new SHA; standalone callers receive a fresh 10-pass budget. This applies intra- and inter-session: any SHA change restarts the counter.
8. **On retry, prior review budget does not carry over.** Each new agent session revalidates approval against the current head before accepting the review step as complete. If no approval is observed, re-enter the standalone review cycle.

## Completion Requirements

Before final response, verify and report:

- Whether each required skill checklist was completed.
- Test/format commands run and outcomes.
- PR URL and review status, if a PR was created.
- Whether PR merge was performed or skipped, with reason.
