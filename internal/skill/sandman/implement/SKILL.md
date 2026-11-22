---
name: sandman-implement
description: Runs the implementation capability from branch creation through verified pull-request publication. Use when a composed workflow needs implementation, testing, self-review, and publication without delegating the standalone review cycle.
---

# Implementation Capability

Implementation and pull-request publication for an open work item. A caller
may compose this capability with `sandman-review-cycle` and `sandman-pr-merge`.
`sandman-review-request` is a separate stateless capability; this capability
does not own the iterative review loop.

After publication, the composing workflow selects the review path. An empty
review command selects `sandman-blind-review`; a configured command selects
`sandman-review-request`. Standalone composition uses `sandman-review-cycle`
for either path. Managed tasks retain runtime-owned waiting and decisions,
and never load the standalone cycle to perform a blind round.

## Scope

This skill implements an open work item by modifying the current repository's source code, tests, and configuration files. It also updates user-facing shared guidance when the work item explicitly requires it; it does not otherwise change capability definitions or repository meta-infrastructure.

## Prerequisites

- `gh` CLI authenticated
- `jq` installed for the bounded CI rerun helper
- Working directory at repo root
- `sandman-plan`, `sandman-tdd`, `sandman-code-review`, and `sandman-back-merge` skills available

## Workflow

You need to follow all steps in this workflow. Make sure you have gone through all items in the checklist in the end of this skill.

## Hard rules

1. **You must NOT exit while any test is failing.** If a test you wrote (or a test that started failing because of your changes) is red, you have two acceptable paths: (a) fix the code so the test goes green, or (b) revert the change that introduced the failure so the test goes green again. Describing the failure, hypothesizing about the cause, and exiting with the failure unresolved is NOT acceptable — a PR with failing tests will not be merged, and any retry will restart on the same broken state. If you truly cannot resolve the failure within the run's context window, commit a known-broken state to a separate diagnostic branch and revert the working branch to the last green commit.

2. **You must commit at meaningful milestones, not only at the very end.** Within a single vertical slice, accumulate the slice's RED→GREEN cycles as uncommitted work — committing after every test produces noisy, undebuggable history. The slice ends with a single commit once the slice is fully green. Across slices, commit one commit per vertical slice. Commit before any step where you might be interrupted — before delegating review, before requesting review, before any action that hands control to another agent. Uncommitted work in the working tree is at risk: if the run is interrupted, retried, or reset, anything that has not been committed is lost. Commits are your durable checkpoint.

3. **You must reach the PR-created state in every run, even with partial implementation.** Only a merged PR counts as success; an open PR is the durable artifact that lets the next run pick up where this one left off. If you cannot complete all vertical slices in the plan within the run's context window, that is not a reason to keep iterating on TDD — it is a reason to commit what you have, create the PR with the platform's exact closing-reference body for the implementor's open work item, and let the review loop surface the gaps. An open PR with partial implementation is recoverable: the next run continues from the same branch. No PR at all means the work lives only in the local working tree, and the next run starts over from a clean branch.

**Closing-reference body is mandatory.** The PR body MUST contain the platform's exact closing-reference line for the selected work item. The reference links the implemented work item to the change request; whether the tracker closes the work item when the change request merges is the repository's own setting, not this workflow's job. A reference buried in prose or using a non-closing keyword is not acceptable and must not be created.

**Closing keywords belong only in the pull-request body.** Never put a closing keyword (`Closes`, `Fixes`, `Resolves`, or any variant) in a commit message, including merge and self-review fix commits. A closing keyword in a commit message can close the work item later than intended, for example when a feature branch lands on the default branch through a merge commit or a rebase.

**A closing reference and footer are not a description.** Before publication, the body MUST contain a meaningful Summary and actual Verification evidence: explain what changed and why, and report what was actually checked. A body containing only the closing reference and footer, headings with no content, or unfilled placeholders is not publishable. Honor the target repository's pull-request template and conventions: retain and complete every required section, use its equivalent headings where available, and include applicable risks or merge-danger guidance. Do not impose Sandman's example headings on another repository.

Verification MUST distinguish completed local checks (name the commands and observed outcomes), known failures or skips (name them and explain them), and externally observed CI (report its status for the current pull-request head and when it was observed). Never invent a green result, treat an unrun check as passing, or present stale CI as current. If CI has not been observed for the current head, say so rather than implying it passed.

4. **Never stage Sandman runtime state.** The `.sandman/` directory holds runtime files (config, prompt, Dockerfile, reviews, the per-run task document). It is intentionally gitignored and is untracked by `sandman init`'s pre-commit guard. Do not run `git add` (with or without `-f`) on any path under `.sandman/`, and do not commit changes that include such paths. The pre-commit hook installed by `sandman init` will reject any commit that attempts to put a `.sandman/` path back into the index, but treat that as a last line of defense: do not stage it in the first place. Other Sandman-managed worktrees may not yet have the hook installed, and a force-pushed history rewrite can resurrect ignored paths.

5. **Never yield for work this workflow owns.** Finish back-merge, commit/push, pull-request creation, and post-create body verification autonomously. Do not ask whether those actions are authorized; the user's request to implement the work item already authorizes them. A checklist transition such as `PR-Review` is not permission to stop before the PR exists. A missing PR or unpushed branch is not an external wait. If an owned action cannot be completed after bounded recovery, record the exact failure and next action and fail the run; do not report success or ask the operator to resume it manually.

6. **Yield only to an operation already in progress.** A confirmed review request is ongoing from successful delivery, even before a reviewer begins; current-head CI may also resolve while you wait when checks are queued or running. In a Sandman-managed run, once your owned work is complete (see Handoff boundary), checkpoint that confirmed request or current-head CI and yield for runtime observation. Do not yield for a generic pending gate, missing checks, a stale head, an unconfirmed review comment, a lookup failure, or an exhausted operation budget. When the reviewer responds, continue the implementation/merge work autonomously; if no external operation is actively resolving a blocker, fail with a specific next action instead of waiting.

### Handoff boundary

Local verification and publication are yours; external observation belongs to the runtime in a Sandman-managed run. A required local test, format, vet or static-analysis run (including a background command that is still running), an unresolved test failure, an unpushed commit, and an incomplete publication, review-request delivery or blind review round all remain your work. Wait for required verification to finish and fix its failures; never skip, interrupt, weaken or abbreviate it to hand off sooner.

Publishing durable progress early (push, open the pull request, deliver the review request) stays allowed and does not end your turn. If local work remains after early publication, say so explicitly instead of implying that the runtime is already observing an external wait.

Once required verification and your publication or request-delivery work are complete, record the durable checkpoint in `.sandman/task.md` and return immediately in a managed run. Do not launch redundant verification, and do not poll CI or review from the agent session to wait for them. A standalone composition continues into the review cycle, which owns its own polling.

Report truthfully. Keep local verification in progress, external CI or review awaiting observation, and ready for merge apart in every checkpoint and final summary. Never present an earlier "CI is running" or "review is pending" statement as current: re-read the live state and report what you verified, or label it as the last observation with its timestamp. In a managed run the runtime, not your summary, makes the post-attempt decision.

### Failed current-head CI recovery

A failed required check on the current pull-request head remains implementation work, even when another check is pending or a reviewer has approved. Diagnose the actual failure first. Repair and validate deterministic source, test, configuration, or workflow failures; update the coordinated pins for a verified provider deprecation. Do not rerun deterministic failures.

For a diagnosed transient provider timeout, network timeout, or provider usage limit, one authorized rerun of the identified failed workflow job is a bounded recovery action. The canary's single `opencode run` invocation and no-retry-loop rule applies to each job invocation; it does not prohibit a rerun of the existing failed execution. `workflow_dispatch` is not required. Keep the PR source head distinct from the workflow's tested SHA, and use the job database ID reported by GitHub rather than a job URL number.

Before acting, verify the live PR source head, workflow run, tested SHA, current attempt, failed job, and repository linkage. Replace every angle-bracket value below with the verified literal value,
keeping its single quotes. Managed invocations must contain those literal
arguments in the tool command itself, not shell variables or substitutions:
the receipt is checked against the recorded command without shell expansion.
Keep the invocation as one shell command on one line. Escape any literal apostrophe as `'\''`. Invoke the helper once:

```bash
sh "$HOME/.agents/skills/sandman/implement/ci-rerun-v1.sh" --repository '<repository>' --pull-request '<pull-request-number>' --head-sha '<source-head-sha>' --tested-sha '<tested-sha>' --run-id '<workflow-run-id>' --job-id '<failed-job-database-id>' --diagnosis transient-provider-timeout
```

Run the helper exactly as shown in a Sandman-managed run. In a standalone session whose skills are installed only in the repository, run the same helper from that installation instead (`.agents/skills/sandman/implement/ci-rerun-v1.sh`).

Use `transient-network-timeout` only for a verified network timeout. Use `transient-provider-quota` only for an explicit hosted-provider usage-limit or quota error in the failed job's log (for example, a message that a free usage limit was reached); that failure is external and transient even though it is not a timeout. Judge the log, not the job's name or a guess: any other failure of a job that merely calls a provider stays deterministic and is never rerun. A usage limit may still be in effect when the rerun starts, so rerun once per failed execution and do not invoke the helper again for the same failure. The helper performs at most one mutation attempt and checks GitHub read-back. A `confirmed` result means a new attempt was observed; `waiting` means the workflow/recovery is already active and should be left to runtime observation in a managed run; `uncertain` or `refused` is not permission to submit again. Resolve ambiguous evidence by re-reading live GitHub state before any further action. Never invent credentials or permissions, rerun a source failure, dispatch a replacement workflow, or weaken a required check.

In a Sandman-managed run, the rerun you spend through the helper is your only one for that failed execution. If it fails the same way and nothing else needs repair, return at once with the failure and your diagnosis recorded in the Task. Do not invoke the helper again for that failure, do not look for another way to rerun it, and do not stop the Run: the runtime reruns the diagnosed job itself, once an hour, while the Run waits, and returns it to the normal review and merge flow when CI is green. A new head or a different failing check is a new failure; diagnose it from the start.

Only after the failure is diagnosed and the corrective action has actually completed may you yield to runtime observation. A saved blocker or a promise to repair is not a corrective action.

### 1. Setup branch

Resolve the base branch before anything else; this skill is shared by every
repository on the machine, so it never assumes one. Use the first source that
provides it:

1. The caller. In a Sandman-managed run, the Task's `Base branch` is that value
   and always wins.
2. The base of the open pull request for the current branch.
3. `git.base_branch` in the repository's `.sandman/config.yaml`.
4. The repository's default branch.

```bash
gh issue view <ID> --json title,number
git branch --show-current                                         # current branch
gh pr view --json baseRefName --jq .baseRefName                   # open pull request base, if any
gh repo view --json defaultBranchRef --jq .defaultBranchRef.name  # repository default branch
base_branch='<resolved base branch>'
git fetch origin "$base_branch"
```

- If the current branch is already a non-base branch (a Sandman-managed worktree's branch, or one the caller named), stay on it.
- Otherwise create and switch to `issue-<ID>/<slugified-title>` from the latest base: `git switch -c "issue-<ID>/<slugified-title>" "origin/$base_branch"`.
- Record the issue title, branch name, and base branch in `.sandman/task.md` or the run log, then continue automatically.
- Later commands that use the base branch start with a guard that stops when it is missing. If your shell does not keep variables between commands, set `base_branch` again, to the same value, before each of them.

### 1.5. Pre-flight check

After setting up the branch, determine whether the issue's work is already complete before running `sandman-plan` or `sandman-tdd`.

The tracker's merge rules, not Sandman, decide whether a merged change request also closes its work item, so an open work item does not prove that the work is missing. Compare its acceptance criteria with the fetched base branch below instead of relying on the work item's tracker state alone.

1. Run the platform's "view work item" CLI to read the current state of the open work item.
2. Run branch freshness check:

   ```bash
   : "${base_branch:?resolve the base branch in step 1 first}"
   git fetch origin "$base_branch"
   git merge-base --is-ancestor HEAD "origin/$base_branch"
   ```

   If `merge-base --is-ancestor HEAD "origin/$base_branch"` returns false, **STOP**. Do NOT write `## Status: already resolved`. Load `sandman-back-merge` and merge the base branch into the current branch before re-evaluating. A branch that is behind `origin/<base-branch>` is stale and cannot reliably be claimed as "already resolved" — the issue's AC may be partially met by local work that is not yet on the base branch, or the AC check will race with the next push to the base branch.
3. Run open-PR check:

   ```bash
   gh pr list --head <branch> --state open
   ```

   If a PR is open for the current branch, the orchestrator will run an independent verification pass against `origin/<base-branch>` before declaring the run successful. Write `## Status: already resolved` only if every AC has a corresponding test that exists on `origin/<base-branch>`; otherwise the orchestrator cannot verify and the run will fail. Agents that prefer the explicit close path can pick one of:
   - **(a) Close the orphan PR** with the platform's "close change request" CLI, preserving the branch and commenting that the work is superseded by the new run, before writing the marker, OR
   - **(b) Stop without writing the marker** and let the existing PR drive the run — this is the safer default; the open PR is itself durable evidence of partial or pending work.
4. Decision matrix (after branch freshness and open-PR checks pass):
   - **Issue is closed** → verify the issue acceptance criteria against the current state of the base branch after fetching `origin/<base-branch>`; only write `## Status: already resolved` and stop if the base branch actually satisfies every criterion. If the base branch does not satisfy every criterion, continue to step 2 (Plan) as normal.
   - **Issue is open** → read the issue acceptance criteria and compare against the current state of the base branch after fetching `origin/<base-branch>`. If all acceptance criteria are already met in the base branch, write `## Status: already resolved` to `.sandman/task.md` and stop without running plan or TDD. Otherwise, proceed to step 2 (Plan) as normal.

Writing `## Status: already resolved` while a PR is open without a verification path fails the run, so step 3 is not optional.

### 2. Plan

- Read the issue body and any linked context
- Load the `sandman-plan` skill
- Let `sandman-plan` create the plan sketch
- Then load `sandman-tdd` and let it execute the plan from `## Plan` in `.sandman/task.md`
- Do NOT add a separate confirmation prompt

### 3. Implement (TDD)

- Follow `sandman-tdd` workflow: vertical slices, one test → one implementation, within the repository codebase
- Run project tests and formatting after each cycle
- **Do NOT commit individual RED→GREEN cycles within a single vertical slice.** Keep working within a slice until the slice is fully green, then commit one commit per vertical slice (see Hard Rule 2). This gives you atomic, reviewable history without the noise of micro-commits per test.
- **Always end a run in a committable state.** If context is running low and you have a green slice but not a complete implementation, commit what you have (one commit per finished slice) before moving to Step 4. Do not let the run end with multiple slices of uncommitted work — if it does, the next retry restarts from a clean branch.

### 4. Commit implementation

Once all tests pass and formatting is clean, derive a subject that the PR title and the commit will share.

Follow the repository's documented commit and title conventions. When none are documented, use Conventional Commits. Pick the most accurate type for the change. Default types: `feat`, `fix`, `perf`, `docs`, `refactor`, `test`, `build`, `ci`, `chore`, `revert`. Append `!` to the type for breaking changes (for example, `feat!:`). Keep the subject to one imperative sentence with no trailing period.

Commit messages never contain closing keywords: the closing reference belongs only in the pull-request body (Hard Rule 3).

For literal shell text, use single quotes and replace each apostrophe with `'\''`; alternatively write the commit message with a file-writing tool and use `git commit -F`. The example below uses the fallback format.

```bash
COMMIT_TYPE="<type>"
COMMIT_SCOPE="<scope-or-empty>"
COMMIT_BREAKING="0"   # 1 if this is a breaking change, else 0
COMMIT_SUBJECT='<one-line imperative description of the change>'

# Conventional Commits headers place the breaking marker (`!`) AFTER the
# optional scope, never between the type and the scope. So
# `feat(scope)!: subject`, not `feat!(scope): subject`.
BREAKING_MARK=""
if [ "$COMMIT_BREAKING" = "1" ]; then
  BREAKING_MARK="!"
fi

if [ -n "$COMMIT_SCOPE" ]; then
  COMMIT_HEADER="$COMMIT_TYPE($COMMIT_SCOPE)${BREAKING_MARK}: $COMMIT_SUBJECT"
else
  COMMIT_HEADER="$COMMIT_TYPE${BREAKING_MARK}: $COMMIT_SUBJECT"
fi

git add -A
git commit -m "$COMMIT_HEADER"
```

### 5. Self-review

- Load the `sandman-code-review` skill in self-review context
- Before delegating, capture one immutable common packet containing the fixed-point identity, commit list, committed branch diff, staged and unstaged tracked diffs, and every untracked path with its contents. The packet must include `git diff <fixed-point>...HEAD`, `git diff`, and `git diff --cached`; do not describe a bare `git diff` as the complete uncommitted state.
- The parent must enumerate the changed paths, select only standards sources relevant to those paths, and supply those standards sources explicitly to the Standards reviewer.
- Supply the authoritative task/specification context explicitly to the Spec reviewer. Launch separate Standards and Spec reviewers with the same common packet, keep their findings separate, and do not ask reviewers to explore the repository or discover more context.
- If any packet component is missing or malformed, record a durable blocked self-review and stop instead of reconstructing context through repository-wide discovery.
- Perform a self-review of the changes
- Apply fixes, format the code, run all tests, including smoke and e2e, and commit:

```bash
git add -A
git commit -m "refactor: self-review fixes"
```

- **If any test fails during self-review, you must NOT exit with the failure unresolved** (see Hard Rule 1). Diagnose the failure, fix the code, and re-run until the test is green. If the failing test is a pre-existing flake unrelated to your changes, isolate it with `git stash`, re-run to confirm green, and document the flake in the commit message. If you cannot resolve the failure within the run's context window, checkpoint the last green implementation, preserve the exact failure, structured blocker, and next executable action in `.sandman/task.md` and the run log, commit the known-broken state only on a separate diagnostic branch, restore the implementation branch to its last green commit, and do not proceed to Steps 6-8.

- Fix the code in case any of the tests fail. Commit again:

```bash
git add -A
git commit -m "refactor: self-review fixes"
```

- Repeat the Self-review cycle until all tests pass.

### 6. Merge base branch before PR

- Load the `sandman-back-merge` skill
- Use it to merge the base branch (`origin/$base_branch`) into the current branch before creating the PR
- Resolve conflicts using the `sandman-back-merge` skill's 3-way workflow
- Run relevant tests and formatting after the merge
- Do NOT rebase
- Do NOT force-push

### 7. Push & create PR

1. Push the branch.

   ```bash
   git push -u origin <branch>
   ```
2. Inspect the target repository's pull-request template and publication conventions before composing the body. Preserve all required template headings, checkboxes, and content; put the meaningful Summary and Verification in the appropriate sections or use their equivalent headings. Semantics matter, not labels: a target template might call these sections "What changed" and "Checks run"; use those existing headings rather than adding or renaming sections. When no template applies, use clear Summary and Verification sections. The example headings below are illustrative, not a requirement for every repository. Start with the platform's exact closing-reference line for the selected work item, include substantive change and verification details, and append the Built with Sandman footer as the final content:

   ```bash
   BODY=$(cat <<'PR_BODY'
   <platform-closing-reference-for-selected-work-item>

   ## Summary
   - Preserve saved search filters when the date range changes.
   - Explain the new empty-results behavior when no matching records remain.

   ## Verification
   - Local checks: `<command actually run>` — `<observed outcome>`.
   - Failures/skips: `<list actual failures or skips; say none only after checking>`.
   - CI: `<status for the current pull-request head SHA and observation time, or "not yet observed">`.
   - Risks/merge danger: `<applicable risk and mitigation, or none identified after review>`.

   <a href="https://github.com/rafaelromao/sandman"><img src="https://raw.githubusercontent.com/rafaelromao/sandman/main/assets/badge-built-with-sandman.svg" alt="Built with Sandman" width="154" /></a>
   PR_BODY
   )
   ```

   Replace every illustrative example and placeholder with facts from this change; the sample is not evidence and must never be copied as a claim. The Summary must say what this change actually does and why, not merely restate its title. Verification must report the actual checks and results, plus known failures/skips and the current-head CI observation (or state that CI is not yet observed). A "not run" entry is an honest status, not a passing check. Include applicable risks or merge-danger guidance and complete any template-required checklist items truthfully. Use the platform's accepted closing-reference form. Do not use a non-closing reference or explanatory prose in its place. The reference identifies the implemented work item; the repository decides whether the tracker closes it when the change request merges.
3. Create the change request with that body. The PR title follows the repository conventions selected in step 4; for the fallback, use the same Conventional Commits header and subject. Apply the repository's title requirements rather than assuming a particular CI check exists.

   ```bash
   : "${base_branch:?resolve the base branch in step 1 first}"
   gh pr create --base "$base_branch" --title "$COMMIT_HEADER" --body "$BODY"
   ```
4. Verify the body that landed on the PR. Pull it back from the change-request service and confirm it matches the closing-reference shape — do not trust that the create call succeeded, because the API may accept variants silently.

   ```bash
   gh pr view <new-pr-number> --json body --jq .body
   ```

   The first non-empty line of the returned body MUST match the platform's closing-reference form. The final content MUST be exactly one linked Built with Sandman logo using the footer above, separated from the preceding content by one blank line. On creation, continuation, and body edits, preserve the selected work-item reference, existing substantive description, and every required template section; move an existing footer to the end rather than duplicating it. This PR footer is independent of the optional repository README badge.

   The publication checkpoint is semantic as well as structural: confirm the body has meaningful Summary content explaining what changed and why, and actual Verification content—not just headings, placeholders, the closing reference, and footer. Confirm local commands and outcomes, known failures/skips, and externally observed CI are clearly distinguished; CI claims must match the current head and a fresh observation. Include applicable risks or merge-danger guidance and verify repository-required sections/checklists remain present and truthful. A reference-and-footer-only body cannot pass this checkpoint. If the check cannot be satisfied, update the body and re-verify. If the body still cannot be made to match after one re-edit attempt, do not delegate review: persist the exact body, structured blocker, and next executable action in `.sandman/task.md` and the run log, then exit this attempt with a structured failure reason so the next run continues from the durable blocker.
5. Capture the PR URL and number.

### 8. Verified publication checkpoint

- Stop this capability only after the branch is pushed, the pull request exists,
  its closing reference and final Built with Sandman footer are verified, and `.sandman/task.md` records the
  PR-created checkpoint plus the next action.
- Do not load the review-cycle or merge capabilities from this capability. The
  composing workflow selects the next capability after publication.
- A partial implementation or open pull request is durable progress. Leave the
  exact blocker and next executable action for the next standalone session.

## Checklist

- [ ] Base branch resolved; branch created from the latest base branch, or the existing implementation branch kept
- [ ] Changes confined to the repository codebase (not meta-infrastructure)
- [ ] Plan reviewed and recorded before TDD
- [ ] Each vertical slice committed before moving to the next (Hard Rule 2)
- [ ] All tests green at exit (Hard Rule 1) — no failing tests left unresolved
- [ ] Implementation committed
- [ ] Self-review performed and fixes committed
- [ ] Base branch merged into current branch with `sandman-back-merge`
- [ ] PR created with a closing-reference body that links back to the implementor's open work item, and verified post-create that the body matches the platform's closing-reference form (Hard Rule 3 — even with partial implementation)
- [ ] PR-created checkpoint recorded for the composing workflow
