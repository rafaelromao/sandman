---
name: sandman-pr-review
description: Automates the GitHub PR review loop with the PR Review Agent. Waits for CI to pass, requests review from the PR Review Agent by posting "{{REVIEW_COMMAND}}" on the PR, then polls for feedback, applies suggestions, commits, pushes, and repeats until approved or max 10 passes. Use when user says sandman pr-review, wants a PR reviewed iteratively by the PR Review Agent, wants to auto-address review feedback, or mentions review loop, {{REVIEW_COMMAND}}, or iterative PR approval.
---

# PR Review

## Hard rules

1. **You must NOT review the PR yourself in this session.**
   Your only job is to delegate the review to the PR Review Agent by posting `{{REVIEW_COMMAND}}` as a PR comment, then wait for the PR Review Agent's feedback and act on it. Under no circumstances should you read the diff and provide your own review comments.

2. **You must NOT finish on ambiguous feedback.** If the reviewer's intent cannot be reduced to a concrete, actionable code change, do not guess, do not change code, and do not stop the loop. Post a new PR comment that includes `{{REVIEW_COMMAND}}` plus a freeform request asking the reviewer to clarify the intended actionable change, then continue polling. The loop only ends on formal approval, explicit user stop, or max passes reached — never on ambiguity.

3. **You must NOT finish before the review timeout or max attempts when no feedback has been provided.** If `reviewDecision` is still `REVIEW_REQUIRED` (or absent), no reviews exist yet, no inline file comments exist, and only boilerplate setup comments are present, keep polling. Do not declare done, do not report success to the user, and do not stop the loop. The only acceptable reasons to exit early are: formal approval (case A or C), explicit user stop, or 10 passes reached.

4. **You must NOT exit the polling loop on a `0/0` count of (formal reviews, inline comments) when the top-level PR conversation has new comments from any non-agent author.** A reviewer who only posts a top-level PR conversation comment (no formal review event, no inline file comments) is still a real reviewer response. Re-classify the state, run the self-check (Step 4), and continue polling — do not give up.

5. **You must NOT request another review while a previous `{{REVIEW_COMMAND}}` is still waiting for a response AND the PR head SHA has not changed.** Only post `{{REVIEW_COMMAND}}` again after either: (a) the reviewer has responded to the previous request, OR (b) a new commit has landed on the PR branch (head SHA changed). If the SHA changed, the previous request is stale — re-request regardless of feedback state. If SHA is unchanged but a response arrived, act on it before re-requesting.

6. **You must NOT request another review before the previous one has produced a response, UNLESS a new commit has landed.** Every iteration that would post a new `{{REVIEW_COMMAND}}` must first check whether the head SHA has changed since the last request. If SHA changed, treat the previous request as consumed and allow re-requesting. If SHA is unchanged, only re-request after a response has arrived.

## Workflow

### Prerequisites

- `gh` CLI authenticated with repo access
- PR is already open, branch is pushed
- Working directory at the repo root

### State tracked across passes

- `.sandman/.<N>.head_sha` — the head commit SHA at which the last `{{REVIEW_COMMAND}}` was posted. If the current head SHA differs, all previous review state is stale and a fresh request is always permitted.
- `.sandman/.<N>.addressed_comments` — one inline comment ID per line, tracking which inline comments have already been acted on. Cleared when head SHA changes (new commit invalidates all old inline comment IDs).

### Iteration loop (max 10 passes)

#### Step 1: Get current PR state

```bash
gh pr view <N> --repo <owner/repo> --json headRefOid,comments,reviewDecision,mergeStateStatus
```

#### Step 2: Wait for CI to pass

> **Prerequisite**: `gh` ≥ 2.0 (released 2021) for `gh pr checks --json ... --jq`. Verify with `gh --version | awk '{print $1, $3}'` before relying on the loop. On older `gh` the `--json` flag is unknown; fall back to plain `gh pr checks <N> --repo <owner/repo>` and parse the first column instead.

```bash
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

**Key change vs before**: On CI failure we `continue` the outer loop to wait for the newly-triggered CI run after pushing the fix — NOT `exit 1` which would fall through to posting a review on a broken PR.

#### Step 3: Check if SHA changed (stale request check)

Read `.sandman/.<N>.head_sha` if it exists and compare against the current head SHA from Step 1.

- **SHA changed** (new commit landed since last request): mark all previous review state stale. Delete `.sandman/.<N>.addressed_comments` if it exists, because inline comment IDs from the old commit are no longer relevant. A fresh review request is always permitted.
- **SHA unchanged**: apply the "previous request still pending" logic before posting again.

#### Step 4: Delegate review to the PR Review Agent

If SHA changed since the last request, always allow re-requesting. If SHA is unchanged, skip this step if no review response has arrived yet.

```bash
gh pr comment <N> --repo <owner/repo> --body "{{REVIEW_COMMAND}}"
```

After posting, write the current head SHA to `.sandman/.<N>.head_sha` so subsequent passes can detect staleness.

**Do NOT read the PR diff or write review comments yourself.** The review must come exclusively from the PR Review Agent.

#### Step 5: Wait for review (timeout: 30 minutes)

| Iteration | Sleep before this poll |
|-----------|------------------------|
| 1         | (no sleep — first poll fires immediately after the `gh pr comment` post) |
| 2         | `sleep 30`             |
| 3         | `sleep 60`             |
| 4         | `sleep 60`             |
| 5         | `sleep 90`             |
| 6         | `sleep 90`             |
| 7         | `sleep 120`            |

Total polling budget: 30 + 60 + 60 + 90 + 90 + 120 = **450s = 7.5 min** of cumulative sleep.

**Hard rule — observed-response fast path.** If any poll iteration observes a new top-level PR conversation comment whose author is not the agent itself, the very next sleep MUST be ≤ 60s.

On **every** poll iteration run all three commands separately:

```bash
gh pr view <N> --repo <owner/repo> --json comments,reviewDecision,mergeStateStatus
gh api repos/<owner>/<repo>/pulls/<N>/reviews
gh api repos/<owner>/<repo>/pulls/<N>/comments --paginate
```

Counter: `top=<count> reviews=<count> inline=<count>`

Counter definitions:
- `top` = top-level PR comments from `gh pr view --json comments` whose author is not the agent itself AND whose body is not the `{{REVIEW_COMMAND}}` request
- `reviews` = entries returned by `gh api .../reviews` (full entry, not truncated `latestReviews`)
- `inline` = entries returned by `gh api .../comments --paginate`

A reviewer response is **any** of:
- A new top-level comment whose author is not the agent itself and whose body is not the `{{REVIEW_COMMAND}}` request
- A formal review with `state: COMMENTED`, `APPROVED`, or `CHANGES_REQUESTED`
- An inline file comment

**Self-check (after every poll, before classifying):**
If `top > 0` AND `reviews == 0` AND `inline == 0`, AND no previous `{{REVIEW_COMMAND}}` is already pending without response, post a follow-up comment with `{{REVIEW_COMMAND}}` plus a freeform clarification request. If a request is already pending, skip — do not pile on.

If no reviewer response arrives within 30 minutes, stop and report to the user.

#### Step 6: Read and classify feedback

**A. Formal approval detected?**
- `reviewDecision: APPROVED`, OR any entry in `gh api .../reviews` with `state: "APPROVED"`
→ **Approve** — done, report to user

**B. Formal changes requested?**
- `reviewDecision: CHANGES_REQUESTED`, OR any entry with `state: "CHANGES_REQUESTED"`
→ **Blockers** — must fix before continuing

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

- Read `.sandman/.<N>.addressed_comments` — skip any inline comment IDs already present.
- Read relevant source files, make minimal changes.
- Run project tests and formatting (e.g., `go test ./...`, `gofmt -w .`).
- Commit: `git add -A && git commit -m "refactor: address review feedback on #<N>"`
- Push: `git push`
- Append acted-on inline comment IDs to `.sandman/.<N>.addressed_comments`.
- **After pushing, loop back to Step 2 to wait for the new CI run triggered by the push.** Do not proceed to Step 8 until CI reaches a terminal state.

#### Step 8: Repeat

Go to Step 1 for the next pass. Before re-requesting in Step 4: if head SHA changed → always allow re-request; if SHA unchanged and previous request received no response → keep polling.

### State files

- `.sandman/.<N>.head_sha` — rewritten on every new review request post. SHA change = all prior review state stale, fresh request always permitted.
- `.sandman/.<N>.addressed_comments` — cleared when head SHA changes (new commit invalidates old inline comment IDs). One inline comment ID per line.

### Same comment ID 3+ passes without resolution

If an inline comment ID appears in 3+ consecutive passes without resolution, treat it as unresolvable without a larger redesign:
- Do not keep looping on the same comment ID
- Re-request review instead, noting the comment ID

## Never give up conditions

Stop only when:
- Formal approval (A or C)
- User explicitly asks to stop
- Max 10 passes reached with unresolved blockers

Continue polling when:
- Review pending / no reviews yet
- Only boilerplate comments exist
- Only nits/suggestions remain
- CI is still running
- Any `CHANGES_REQUESTED` review exists but is addressable
- Only already-addressed inline comment IDs remain
- Top-level PR conversation has new comments from non-agent author
- **A new commit has landed (head SHA changed) — re-request always permitted regardless of prior response state**

## Tips

- Use `gh pr view --json state,mergeStateStatus` to check merge readiness after approval.
- Always include `top=<count> reviews=<count> inline=<count>` in the final report.
- Never force-push or amend commits.
- Keep commits focused: one commit per review round.
- When feedback is ambiguous, ask for clarification with `{{REVIEW_COMMAND}}` in the same comment.
- Review agents may post feedback as: top-level comments, inline diff comments, or formal `COMMENT` reviews. Always check all three sources.
- When CI is failing, fix it first — CI must be green before any review feedback can be meaningfully addressed.
