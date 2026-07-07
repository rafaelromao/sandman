---
name: sandman-pr-review
description: Automates the GitHub PR review loop with the PR Review Agent. Waits for CI to pass, requests review from the PR Review Agent by posting "{{REVIEW_COMMAND}}" on the PR, then polls for feedback, applies suggestions, commits, pushes, and repeats until approved or max 10 passes. Use when user says sandman pr-review, wants a PR reviewed iteratively by the PR Review Agent, wants to auto-address review feedback, or mentions review loop, {{REVIEW_COMMAND}}, or iterative PR approval.
---

# PR Review

## Hard rules

1. **You must NOT review the PR yourself in this session.**
   Your only job is to delegate the review to the PR Review Agent by posting `{{REVIEW_COMMAND}}` as a PR comment, then wait for the PR Review Agent's feedback and act on it. Under no circumstances should you read the diff and provide your own review comments.

2. **You must NOT finish on ambiguous feedback.** If the reviewer's intent cannot be reduced to a concrete, actionable code change, do not guess, do not change code, and do not stop the loop. Post a new PR comment that includes `{{REVIEW_COMMAND}}` plus a freeform request asking the reviewer to clarify the intended actionable change, then continue polling. The loop only ends on approval (formal case A or informal case C), explicit user stop, or max passes reached — never on ambiguity.

3. **You must NOT finish before the review timeout or max attempts when no feedback has been provided.** If `reviewDecision` is still `REVIEW_REQUIRED` (or absent), no reviews exist yet, no inline file comments exist, and only boilerplate setup comments are present, keep polling. Do not declare done, do not report success to the user, and do not stop the loop. The only acceptable reasons to exit early are: approval (formal case A or informal case C), explicit user stop, or 10 passes reached.

4. **You must NOT exit the polling loop on a `0/0` count of (formal reviews, inline comments) when the top-level PR conversation has new comments from any non-agent author.** A reviewer who only posts a top-level PR conversation comment (no formal review event, no inline file comments) is still a real reviewer response. Re-classify the state, run the self-check (Step 4), and continue polling — do not give up.

5. **You must NOT request another review while a previous `{{REVIEW_COMMAND}}` is still waiting for a response AND the PR head SHA has not changed.** Only post `{{REVIEW_COMMAND}}` again after either: (a) the reviewer has responded to the previous request, OR (b) a new commit has landed on the PR branch (head SHA changed). If the SHA changed, the previous request is stale — re-request regardless of feedback state. If SHA is unchanged but a response arrived, act on it before re-requesting.

6. **You must NOT request another review before the previous one has produced a response, UNLESS a new commit has landed.** Every iteration that would post a new `{{REVIEW_COMMAND}}` must first check whether the head SHA has changed since the last request. If SHA changed, treat the previous request as consumed and allow re-requesting. If SHA is unchanged, only re-request after a response has arrived.

7. **You must NOT request review until CI is green.** If CI is still pending or failing, keep polling Step 2 and do not post `{{REVIEW_COMMAND}}` yet.

8. **You must NOT give up on a `CHANGES_REQUESTED` review when the reviewer's request maps to the issue description or acceptance criteria.** When the reviewer flags a requirement that comes from the issue body or its acceptance criteria (the same criteria the implementor agent was asked to satisfy), you have exactly two acceptable paths:
   - **Implement the requested change.** Read the issue description and its acceptance criteria, confirm the reviewer's interpretation is consistent with them, then make the change, commit, push, and re-request review.
   - **Convince the reviewer the requirement is out of scope.** Post a PR comment that quotes the issue's own acceptance criteria verbatim, explains why the requested change falls outside the issue's stated scope, and asks the reviewer to either accept the narrowed scope or correct the implementor's interpretation. Then **wait for the reviewer's explicit agreement** before considering the `CHANGES_REQUESTED` resolved. If the reviewer reaffirms the change is required, you must implement it on the next pass — you cannot keep asserting your own interpretation against theirs.
   
   It is NEVER acceptable to assert "this is out of scope" unilaterally and exit the loop with a `CHANGES_REQUESTED` still pending. If max passes are reached with the deadlock unresolved, exit the loop with a clearly-documented `CHANGES_REQUESTED_UNRESOLVED` reason in the run log so the failure is visible in the run history — do not silently terminate as if the work were complete.

9. **You must use `codeindex` before `grep` or `glob` when looking for symbols, blast radius, dependencies, or other broad code locations.** Load the `sandman-index` sub-skill first — it encapsulates all codeindex guidance including the hard rule, command reference, query refinement strategies, and read discipline.

10. **Any PR comment intended to be read by the reviewer MUST start with the review command.** A comment that does not begin with the review command is treated as boilerplate by the daemon and ignored — it does not reach the reviewer and does not advance the loop. Concretely:
    - When posting the trigger comment (Step 4), the body must be exactly the review command on its own (e.g. via the platform's "post change-request comment" CLI, passing the change-request identifier and the review-command body).
    - When posting a clarification request, a follow-up after a stalled poll, or any other reviewer-facing message, the body must begin with the review command and may include additional freeform text afterwards (e.g. `{{REVIEW_COMMAND}} — please clarify which file you mean`). The leading review-command substring is what the daemon's trigger filter matches on; the trailing freeform text is read by the reviewer but ignored by the trigger filter.
    - When posting the bot's own review-body, do NOT prefix it with the review command. The review-body is the substance the reviewer writes back to you — prefixing it would cause the daemon to mis-classify the body as a duplicate trigger on the next tick and drop the actual review content.

## Workflow

### Prerequisites

- `gh` CLI authenticated with repo access
- PR is already open, branch is pushed
- Working directory at the repo root

### State tracked across passes

- `.sandman/state/<N>.head_sha` — the head commit SHA at which the last `{{REVIEW_COMMAND}}` was posted. If the current head SHA differs, all previous review state is stale and a fresh request is always permitted.
- `.sandman/state/<N>.addressed_comments` — one inline comment ID per line, tracking which inline comments have already been acted on. Cleared when head SHA changes (new commit invalidates all old inline comment IDs).

### Iteration loop (max 10 passes)

#### Step 1: Get current PR state

```bash
pr_data=$(gh pr view <N> --repo <owner/repo> --json headRefOid,comments,reviewDecision,mergeStateStatus)
mergeStateStatus=$(echo "$pr_data" | jq -r '.mergeStateStatus')
headRefOid=$(echo "$pr_data" | jq -r '.headRefOid')
reviewDecision=$(echo "$pr_data" | jq -r '.reviewDecision')
comments=$(echo "$pr_data" | jq -r '.comments')
```

#### Step 2: Wait for CI to pass

> **Prerequisite**: `gh` ≥ 2.0 (released 2021) for `gh pr checks --json ... --jq`. Verify with `gh --version | awk '{print $1, $3}'` before relying on the loop. On older `gh` the `--json` flag is unknown; fall back to plain `gh pr checks <N> --repo <owner/repo>` and parse the first column instead.

```bash
# Step 2 must wait for CI, but CI cannot run on a DIRTY (conflicting) PR.
# Step 1 already fetched mergeStateStatus — use it directly. If DIRTY, trigger back-merge first.
if [ "$mergeStateStatus" = "DIRTY" ]; then
  echo "PR is in 'DIRTY' state (merge conflicts). CI cannot run. Running sandman-back-merge to resolve."
  # Load and run back-merge: merges the base branch into the current branch, resolves conflicts, pushes.
  # This can recover fixes that landed on main after the branch was created.
  # If back-merge fails, the PR remains unmergeable — keep polling so we re-enter this block.
  if sandman-back-merge; then
    echo "Back-merge succeeded, pushing and re-checking CI."
    git push
    continue  # restart CI wait loop after push triggers new CI run
  else
    echo "Back-merge failed or unresolved conflicts — CI still blocked. Continuing to poll."
    sleep 20
    continue
  fi
fi

# gh pr checks --json returns state values in uppercase:
#   SUCCESS, FAILURE, PENDING, IN_PROGRESS, QUEUED, NEUTRAL,
#   CANCELLED, TIMED_OUT, ACTION_REQUIRED, STARTUP_FAILURE, STALE, SKIPPED.
# We classify each state into "fail", "pending", or "pass" and loop until
# no "pending" remains (with "fail" taking priority).
while true; do
  states=$(gh pr checks <N> --repo <owner/repo> --json name,state \
    --jq '.[] | select(.state != "SKIPPED") | .state' 2>/dev/null)
  if [ -z "$states" ]; then sleep 20; continue; fi
  # Fail: read logs, fix, push, then continue waiting for the new CI run.
  if echo "$states" | grep -qE '^(FAILURE|STARTUP_FAILURE|TIMED_OUT|ACTION_REQUIRED|CANCELLED)$'; then
    echo "CI failed:"; gh pr checks <N> --repo <owner/repo>
    # Fetch failure reason from job logs.
    job_id=$(gh api repos/<owner>/<repo>/actions/runs \
      --jq '.workflow_runs[0].jobs[] | select(.conclusion == "failure") | .id' 2>/dev/null)
    if [ -n "$job_id" ]; then
      gh api repos/<owner>/<repo>/actions/jobs/<job_id>/logs \
        --jq '.text' 2>/dev/null | tail -50
    fi
    # If the failure looks like base-branch drift, use sandman-back-merge to pull in fixes before retrying.
    # This can recover fixes that landed after the task started.
    # Fix it. Read relevant source files, make minimal changes.
    git add -A && git commit -m "fix: resolve CI failure on <N>" && git push
    # After pushing, the old CI run is irrelevant.
    # Continue to wait for the NEW CI run triggered by the push.
    continue
  fi
  # Pending: keep waiting.
  if echo "$states" | grep -qE '^(PENDING|IN_PROGRESS|QUEUED)$'; then
    sleep 20; continue
  fi
  # All remaining states are terminal non-fail (SUCCESS, NEUTRAL, STALE).
  break
done
```

On CI failure, `continue` the outer loop to wait for the newly-triggered CI run after pushing the fix — not `exit 1`, which would fall through to posting a review on a broken PR. If `mergeStateStatus` is `DIRTY`/`CONFLICTING` (the PR has merge conflicts), CI cannot run at all: Step 2 detects that upfront and delegates to `sandman-back-merge` to resolve the conflict before waiting for CI. This prevents the agent from spinning forever on empty check results or declaring the PR "requires manual resolution."

#### Step 3: Check if SHA changed (stale request check)

Read `.sandman/state/<N>.head_sha` if it exists and compare against the current head SHA from Step 1.

- **SHA changed** (new commit landed since last request): mark all previous review state stale. Delete `.sandman/state/<N>.addressed_comments` if it exists, because inline comment IDs from the old commit are no longer relevant. A fresh review request is always permitted.
- **SHA unchanged**: apply the "previous request still pending" logic before posting again.

#### Step 4: Delegate review to the PR Review Agent (trigger post)

If SHA changed since the last request, always allow re-requesting. If SHA is unchanged, skip this step if no review response has arrived yet.

Only post `{{REVIEW_COMMAND}}` after CI has reached a green terminal state in Step 2.

```bash
gh pr comment <N> --repo <owner/repo> --body "{{REVIEW_COMMAND}}"
```

After posting, write the current head SHA to `.sandman/state/<N>.head_sha` so subsequent passes can detect staleness.

#### Step 5: Wait for review (timeout: 15 minutes)

| Iteration | Sleep before this poll |
|-----------|------------------------|
| 1         | `sleep 120`            |
| 2         | `sleep 60`             |
| 3         | `sleep 60`             |
| 4+        | `sleep 30` (repeated until cumulative sleep budget of 900s is exhausted) |

Total polling budget: **900s = 15 minutes** of cumulative sleep (120 + 60 + 60 + N×30).

**Hard rule — observed-response fast path.** If any poll iteration observes a new top-level PR conversation comment whose author is not the agent itself, the very next sleep MUST be ≤ 60s.

**Hard rule — DIRTY mid-poll must trigger back-merge, not be observed and ignored.** A PR whose `mergeStateStatus` was CLEAN at Step 1 can drift to `DIRTY` mid-poll once a new commit lands on the base branch and conflicts with the PR. The DIRTY pre-check at Step 2 only catches the initial state; subsequent polls MUST detect and resolve this. See Step 5a.

On **every** poll iteration run all three commands separately:

```bash
gh pr view <N> --repo <owner/repo> --json comments,reviewDecision,mergeStateStatus
gh api repos/<owner>/<repo>/pulls/<N>/reviews
gh api repos/<owner>/<repo>/pulls/<N>/comments --paginate
```

Counter: `top=<count> reviews=<count> inline=<count>`

Counter definitions:
- `top` = top-level change-request comments from the change-request view (the JSON `comments` field) whose author is not the agent itself AND whose body is not the `{{REVIEW_COMMAND}}` request
- `reviews` = entries returned by the reviews endpoint (full entry, not truncated `latestReviews`)
- `inline` = entries returned by the inline comments endpoint with pagination

A reviewer response is **any** of:
- A new top-level comment whose author is not the agent itself and whose body is not the `{{REVIEW_COMMAND}}` request
- A formal review with `state: COMMENTED`, `APPROVED`, or `CHANGES_REQUESTED`
- An inline file comment

**Self-check (after every poll, before classifying):**
If `top > 0` AND `reviews == 0` AND `inline == 0`, AND no previous `{{REVIEW_COMMAND}}` is already pending without response, post a follow-up comment with `{{REVIEW_COMMAND}}` plus a freeform clarification request. If a request is already pending, skip — do not pile on.

If no reviewer response arrives within 15 minutes, stop and exit the loop with a `REVIEW_TIMEOUT` reason documented in the run log so the failure is visible in the run history.

#### Step 5a: DIRTY handling — every poll iteration

> **Prerequisite**: a DIRTY (`mergeable == CONFLICTING`) PR cannot run CI, cannot be reviewed on its diff cleanly, and cannot be merged. The Step 2 pre-check catches the initial state, but a PR can drift to DIRTY mid-poll once new commits land on the base branch. This section is the per-poll guard.

On **every** poll iteration, after running the three commands above, inspect the `mergeStateStatus` field already returned by the first command (do **not** make a separate change-request view call). If `mergeStateStatus == "DIRTY"`:

1. Stop polling for review feedback. The PR is unmergeable until the conflict is resolved; reviewer comments posted on a DIRTY PR do not produce a usable review.
2. Load `sandman-back-merge` (see the `sandman-back-merge` skill). Run it on the current branch. It performs the disciplined 3-way merge of the base branch into the working branch and resolves conflicts without history rewrites.
3. If back-merge succeeds, push the updated branch with `git push`. Update `.sandman/state/<N>.head_sha` with the new head SHA so Step 3's stale-request check sees the new commit and re-evaluates.
4. Restart polling from Step 1 — a fresh CI run will be triggered by the push, and the review agent may have already posted feedback on the prior SHA that the polling loop should classify on the next pass.
5. If back-merge fails to resolve conflicts (e.g. semantic conflict, merge helper rejected a hunk), exit the loop with a distinct `REVIEW_CONFLICT_UNRESOLVED` reason in the run log. This is **never** a `REVIEW_TIMEOUT`. It is also **never** a silent success — the PR remains unmergeable and a future run must continue from this state.

**Hard rule — DIRTY is not REVIEW_TIMEOUT.** A DIRTY PR that back-merge cannot resolve is a structured failure with a downstream signal in the run payload. Do not collapse it into the generic review-timeout bucket: the two failures have different remediation paths and different downstream tooling.

**Hard rule — DIRTY is not silent success.** Observing a DIRTY PR and continuing to poll for reviewer comments is the failure mode the skill exists to prevent. The fix is action, not observation.

#### Step 6: Read and classify feedback

**A. Formal approval detected?**
- `reviewDecision: APPROVED`, OR any entry returned by the reviews endpoint with `state: "APPROVED"`
→ **Approve** — done, exit the loop and document the approval in the run log.

**B. Formal changes requested?**
- `reviewDecision: CHANGES_REQUESTED`, OR any entry with `state: "CHANGES_REQUESTED"`
→ **Blockers** — must fix before continuing. Apply Hard Rule 7 (issue ACs): if the reviewer's request maps to a requirement from the issue body or acceptance criteria, you must either implement the change or get the reviewer's explicit agreement that the scope is narrower. Posting a "this is out of scope" comment and exiting the loop is NOT an acceptable resolution — it leaves the `CHANGES_REQUESTED` pending and the PR unmerged.

**C. Informal approval (implicit approval without formal review)?**
- No pending `CHANGES_REQUESTED` reviews, AND
- A `COMMENTED` review OR top-level comment with approval keywords
→ **Approve — DONE. Stop the loop.** An informal approval is sufficient.

Approval keywords: `lgtm`, `looks good`, `looks great`, `nice work`, `good work`, `approved`, `ship it`, `+1`, `thumbs up`, `all good`, `all set`, `good to go`, `no major issues`, `minor issues only`, etc.

**D. Still pending?**
- `reviewDecision: "REVIEW_REQUIRED"` or absent, AND
- `top == 0`, AND
- No `APPROVED` / `CHANGES_REQUESTED` reviews, AND
- No inline comments with concrete requested changes, AND
- All bodies are boilerplate-only
→ **Still waiting** — continue polling

**E. Real feedback detected?**
An inline file comment OR top-level comment OR review body contains concrete code feedback (specific file paths, function names, variable names, line numbers):
→ **Has blockers or suggestions** — apply fixes, commit, push. Only re-request after fix+push if previous `{{REVIEW_COMMAND}}` already received a response. If no response yet, keep polling.

**F. Ambiguous feedback with unclear actionable intent only?**
- Comments exist but none specify a concrete code change
→ **Clarification** — ask for clarification if no request is pending; otherwise keep polling.

**G. Only nits or suggestions?**
- Comments are nits or optional improvements, no `CHANGES_REQUESTED`
→ **Suggestions** — fix if straightforward; skip if redesign required. Only re-request after fix+push if previous request received a response.

#### Step 7: Apply fixes

**Hard rule — never exit after pushing a fix.** After `git push` in Step 7, the agent MUST continue to Step 5 to poll for the reviewer's next response.

**Hard rule — never exit with `CHANGES_REQUESTED` unresolved.** If a `CHANGES_REQUESTED` review exists after applying fixes, do not declare the run done. Re-request review (Step 4) and continue the loop. Only approval (formal case A or informal case C), explicit user stop, or max passes reached may end the loop. Applying a fix that you believe addresses the reviewer's concern does NOT close the loop — the reviewer must explicitly approve.

- Read `.sandman/state/<N>.addressed_comments` — skip any inline comment IDs already present.
- Read relevant source files, make minimal changes.
- Run project tests and formatting (e.g., `go test ./...`, `gofmt -w .`).
- Commit: `git add -A && git commit -m "refactor: address review feedback on #<N>"`
- Push: `git push`
- Append acted-on inline comment IDs to `.sandman/state/<N>.addressed_comments`.
- **After pushing, loop back to Step 2 to wait for the new CI run triggered by the push.** Do not proceed to Step 8 until CI reaches a terminal state.

#### Step 8: Repeat

Go to Step 1 for the next pass. Before re-requesting in Step 4: if head SHA changed → always allow re-request; if SHA unchanged and previous request received no response → keep polling.

### State files

- `.sandman/state/<N>.head_sha` — rewritten on every new review request post. SHA change = all prior review state stale, fresh request always permitted.
- `.sandman/state/<N>.addressed_comments` — cleared when head SHA changes (new commit invalidates old inline comment IDs). One inline comment ID per line.

### Same comment ID 3+ passes without resolution

If an inline comment ID appears in 3+ consecutive passes without resolution, treat it as unresolvable without a larger redesign:
- Do not keep looping on the same comment ID
- Re-request review instead, noting the comment ID

## Never give up conditions

Stop only when:
- Formal approval (A or C)
- User explicitly asks to stop
- Max 10 passes reached with unresolved blockers
- **`REVIEW_CONFLICT_UNRESOLVED` — back-merge failed to resolve a DIRTY PR; not a `REVIEW_TIMEOUT`, never silent**

Continue polling when:
- Review pending / no reviews yet
- Only boilerplate comments exist
- Only nits/suggestions remain
- CI is still running
- **The PR is DIRTY mid-poll — Step 5a triggers `sandman-back-merge`, then restarts polling from Step 1 after a successful push. Keep going while back-merge is making progress; exit with `REVIEW_CONFLICT_UNRESOLVED` only when back-merge itself fails.**
- Any `CHANGES_REQUESTED` review exists but is addressable
- Only already-addressed inline comment IDs remain
- Top-level PR conversation has new comments from non-agent author
- **A new commit has landed (head SHA changed) — re-request always permitted regardless of prior response state**
- **A `CHANGES_REQUESTED` review references the issue's acceptance criteria and you have not yet implemented the change OR obtained the reviewer's explicit agreement on the narrowed scope (Hard Rule 7)**

## Tips

- Use the change-request view's `state` and `mergeStateStatus` JSON fields to check merge readiness after approval.
- Always include `top=<count> reviews=<count> inline=<count>` in the final report.
- Never force-push or amend commits.
- Keep commits focused: one commit per review round.
- When feedback is ambiguous, ask for clarification with `{{REVIEW_COMMAND}}` in the same comment.
- Review agents may post feedback as: top-level comments, inline diff comments, or formal `COMMENT` reviews. Always check all three sources.
- When CI is broken and the failure may be base-branch drift, load `sandman-back-merge` first so any fix that landed on the base branch can be merged before retrying.
- When CI is failing, fix it first — CI must be green before any review feedback can be meaningfully addressed.
- **DIRTY PR handling is a hard per-poll guard.** If `mergeable == CONFLICTING` is observed on ANY poll iteration, Step 5a triggers `sandman-back-merge` automatically. Do not treat a DIRTY PR as a manual-resolution situation, do not classify it as `REVIEW_TIMEOUT`, and do not exit the loop with silent success. The only acceptable outcomes are: (a) back-merge succeeded → push → restart polling; (b) back-merge failed → exit with `REVIEW_CONFLICT_UNRESOLVED`.
