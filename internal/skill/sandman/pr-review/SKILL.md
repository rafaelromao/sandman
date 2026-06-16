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

5. **You must NOT request another review while a previous `{{REVIEW_COMMAND}}` is still waiting for a response.** Only post `{{REVIEW_COMMAND}}` again after the reviewer has responded to the previous request (approval, changes requested, comments, or inline feedback). If no response has arrived yet, keep polling — do not re-request.

6. **You must NOT request another review before the previous one has produced a response.** Every iteration of the loop that would post a new `{{REVIEW_COMMAND}}` must first check whether the previous request has gotten a response. If it hasn't, skip the request and go back to polling.

## Workflow

### Prerequisites

- `gh` CLI authenticated with repo access
- PR is already open, branch is pushed
- Working directory at the repo root

### Iteration loop (max 10 passes)

1. **Check CI status**

  ```bash
  gh pr checks <N> --repo <owner/repo>
  ```

2. **Wait for CI**

  Run this single deterministic bash loop as one standalone command (do NOT pipe through `tail`/`head`/`&&` and do NOT use `gh pr checks --watch` — the loop below is the only mechanism; the model must not invent its own polling). The loop polls `gh pr checks` every 20 seconds and exits the moment CI reaches a terminal state. This is the lock that prevents the 15-minute model-side gap observed when the agent chose `--watch` and then took minutes to fire the next command.

  > **Prerequisite**: `gh` ≥ 2.0 (released 2021) for `gh pr checks --json ... --jq`. Verify with `gh --version | awk '{print $1, $3}'` before relying on the loop. On older `gh` the `--json` flag is unknown and the loop will fail; fall back to plain `gh pr checks <N> --repo <owner/repo>` and parse the first column instead.

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
    # Fail: bail out.
    if echo "$states" | grep -qE '^(FAILURE|STARTUP_FAILURE|TIMED_OUT|ACTION_REQUIRED|CANCELLED)$'; then
      echo "CI failed:"; gh pr checks <N> --repo <owner/repo>; exit 1
    fi
    # Pending: keep waiting.
    if echo "$states" | grep -qE '^(PENDING|IN_PROGRESS|QUEUED)$'; then
      sleep 20; continue
    fi
    # All remaining states are terminal non-fail (SUCCESS, NEUTRAL, STALE).
    break
  done
  ```

  If the loop exits non-zero (CI `fail`):

    - If there are merge conflicts, load the `sandman-back-merge` skill and merge the base branch into the local branch.
    - Read the failed job logs to identify the root cause.
    - Fix the error in the codebase.
    - Run local tests/formatting to verify the fix.
    - Commit and push: `git add -A && git commit -m "fix: resolve CI failure" && git push`
    - **Repeat Step 2** (re-run the loop above).

3. **Delegate review to the PR Review Agent**

  Before posting, check if a previous `{{REVIEW_COMMAND}}` has already been sent and is still awaiting a response. If it has (no review comments, no formal reviews, no inline feedback since that post), skip this step and go back to Step 2 — do not pile on a new request.

  Request a review with this exact command. Do not change the body of the request for the initial review request.

  ```bash
  gh pr comment <N> --repo <owner/repo> --body "{{REVIEW_COMMAND}}"
  ```
  **Do NOT read the PR diff or write review comments yourself.** The review must come exclusively from the PR Review Agent.

4. **Wait for review** (timeout: 30 minutes)

  Poll on this **explicit cadence** — the model must NOT invent its own sleep durations. Use the sleep values below in order; after the sixth sleep, stop and report to the user (timeout reached):

  | Iteration | Sleep before this poll |
  |-----------|------------------------|
  | 1         | (no sleep — first poll fires immediately after the `gh pr comment` post) |
  | 2         | `sleep 30`             |
  | 3         | `sleep 60`             |
  | 4         | `sleep 60`             |
  | 5         | `sleep 90`             |
  | 6         | `sleep 90`             |
  | 7         | `sleep 120`            |

  Total polling budget: 30 + 60 + 60 + 90 + 90 + 120 = **450s = 7.5 min** of cumulative sleep between the `/sandman review` post and the last poll, inside a **30 min overall ceiling**. Per-poll worst-case latency is **120s** (the longest single sleep in the curve), so a response that arrives during a sleep is detected within 120s. The 7.5 min figure is the cumulative sleep if you run every iteration to its full sleep with no observed response. The cadence is chosen to fit the 2-10 min typical review-agent runtime and to bound the response-detection latency.

  **Hard rule — observed-response fast path.** If any poll iteration observes a new top-level PR conversation comment whose author is not the agent itself (i.e., the review agent has started responding), the very next sleep MUST be ≤ 60s. The 90s and 120s sleeps are only allowed when no new comment has been observed since the request was posted. This is the lock that prevents a 5-minute sleep from missing a response that arrived during the sleep.

  On **every** poll iteration you MUST run all three commands below — never skip one. Run each command as a fully separate, standalone invocation — do NOT chain commands in any way, including with `&&`, `||`, `;`, pipes (`|`), or subshells. Each command must be executed on its own so its full output is captured before processing the next one.

  After every poll, print a counter line in this exact format (the agent must read this counter out loud to the user on every iteration and include the final iteration's counter in the final report):

  ```
  top=<count> reviews=<count> inline=<count>
  ```

  ```bash
  gh pr view <N> --repo <owner/repo> --json comments,reviewDecision,mergeStateStatus
  gh api repos/<owner>/<repo>/pulls/<N>/reviews
  gh api repos/<owner>/<repo>/pulls/<N>/comments --paginate
  ```

  Counter definitions (apply to a single poll iteration):
  - `top` = number of top-level PR conversation comments from `gh pr view --json comments` whose author is not the agent itself AND whose body is not the `{{REVIEW_COMMAND}}` request the agent posted. Capture the agent's own GitHub author login from the `{{REVIEW_COMMAND}}` post in step 3 and reuse it as the "self" filter for every subsequent poll
  - `reviews` = number of entries returned by `gh api .../reviews` (use the full entry, not the truncated `latestReviews`)
  - `inline` = number of entries returned by `gh api .../comments --paginate` (inline file-level review comments on the diff; use `--paginate` to fetch all pages)

  What each command returns:
  - `gh pr view --json comments` — top-level PR conversation comments only (NOT inline diff comments)
  - `gh api .../reviews` — all reviews with their state, body, and commit. Use this for review content, NOT `latestReviews` which is a truncated preview with empty fields
  - `gh api .../comments --paginate` — inline file-level review comments on the diff. Use `--paginate` to fetch all pages; inline comments can span many files and are often truncated without it

  A reviewer response is **any** of:
  - A new top-level comment whose author is not the agent itself and whose body is not the `{{REVIEW_COMMAND}}` request the agent posted
  - A formal review with `state: COMMENTED`, `APPROVED`, or `CHANGES_REQUESTED`
  - An inline file comment

  **Self-check (run after every poll, before classifying state):**
  If `top > 0` AND `reviews == 0` AND `inline == 0`, AND no previous `{{REVIEW_COMMAND}}` is already pending without response, post a follow-up PR comment that includes `{{REVIEW_COMMAND}}` plus a freeform request asking the reviewer to clarify the intended actionable change, then continue polling. If a request is already pending, skip — do not pile on. This guarantees that a reviewer who only posts a top-level conversation comment is never silently dropped.

  Read every new PR Review Agent comment from all three sources, including inline file comments. Do not overlook comments attached to a file diff instead of the top-level conversation. Treat any requested concrete change in an inline file comment as actionable feedback. If no reviewer response arrives within 30 minutes, stop and report to the user.

5. **Read and classify feedback**

    Merge data from all three sources (top-level PR conversation comments, formal reviews from `gh api .../reviews`, inline file comments from `gh api .../comments`) and apply this decision tree to the **union** of those sources. Every case A through G below is evaluated against the union — a single signal from any of the three sources is enough to trigger the corresponding case. When multiple cases match, `APPROVED` and `CHANGES_REQUESTED` win first, then actionable feedback, then ambiguity, then pending/boilerplate:

   **A. Formal approval detected?**
   - `reviewDecision: APPROVED` in the JSON from `gh pr view`, OR
   - Any entry in `gh api .../reviews` with `state: "APPROVED"`
   → **Approve** — done, report to user

   **B. Formal changes requested?**
   - `reviewDecision: CHANGES_REQUESTED` from `gh pr view`, OR
   - Any entry in `gh api .../reviews` with `state: "CHANGES_REQUESTED"`
   → **Blockers** — must fix before continuing

   **C. Informal approval (implicit approval without formal review)?**
   - No pending `CHANGES_REQUESTED` reviews, AND
   - At least one of these conditions:
     - An entry in `gh api .../reviews` with `state: "COMMENTED"` whose body contains approval keywords (see list below), OR
     - A top-level PR conversation comment (from `gh pr view --json comments`, not attached to a diff line) from a known reviewer whose body contains approval keywords
   → **Approve — DONE. Report to user. Stop the loop. An informal approval is sufficient — do not wait for a formal APPROVE review.**

   An informal approval (Case C) is just as valid as a formal approval for the purpose of merging. When this case is reached, the PR is ready. Do NOT continue polling or re-request review.

   Approval keywords to search for (case-insensitive, partial match):
   `lgtm`, `looks good`, `looks good to me`, `looks great`, `looks nice`,
   `nice work`, `good work`, `great work`, `approved`, `ship it`, `+1`,
   `thumbs up`, `all good`, `all set`, `good to go`, `go ahead`,
   `didn't find any major issues`, `no major issues`, `minor issues only`,
   `only minor`, `no major concerns`

    **D. Still pending?**
    - `reviewDecision: "REVIEW_REQUIRED"` or absent, AND
    - `top == 0` (no top-level comments from non-agent authors), AND
    - No entries in `gh api .../reviews` with `state: "APPROVED"` or `state: "CHANGES_REQUESTED"`, AND
    - No inline file comments with a concrete requested change exist (from `gh api .../comments`), AND
    - All review bodies are boilerplate-only (see below)
    → **Still waiting** — continue polling, do not give up

  **E. Real feedback detected?**
  An inline file comment exists from `gh api .../comments` with a concrete requested code change, OR a top-level PR conversation comment contains concrete code feedback, OR a review body contains concrete code feedback beyond boilerplate:
  - Boilerplate pattern: body starts with "### 💡" or "### Codex Review" and only contains "Here are some automated review suggestions" without mentioning specific files, functions, variable names, or line numbers
  - Real feedback: mentions specific file paths, function names, variable names, or requests concrete code changes
  → **Has blockers or suggestions** — treat as actionable feedback, apply fixes, commit, push. Only re-request review after the fix+push cycle if the previous `{{REVIEW_COMMAND}}` has already received a response. If no response was received yet, keep polling — do not re-request.

  **F. Ambiguous feedback with unclear actionable intent only?**
  - Top-level comments, inline file comments, or review bodies exist, but none of them specify a concrete code change
  - The reviewer's intended action cannot be reduced to a concrete code change
  - The feedback is not concrete enough to classify as a specific fix, blocker, or suggestion
  - No `APPROVED` or `CHANGES_REQUESTED` review is present
  → **Clarification** — do not guess, do not change code. Only post a new `{{REVIEW_COMMAND}}` plus clarification request if no previous request is still pending without response. If one is already pending, keep polling — do not pile on.

  **G. Only nits or suggestions?**
  - Comments that are nits (minor stylistic) or suggestions (optional improvements)
  - No `CHANGES_REQUESTED` reviews
  → **Suggestions** — fix if straightforward; skip if requires non-trivial redesign. Only re-request review after the fix+push cycle if the previous `{{REVIEW_COMMAND}}` has already received a response. If not, keep polling — do not re-request.

6. **Apply fixes**
   - Read `.sandman/.<N>.addressed_comments` to get the set of already-addressed inline comment IDs
   - When reading inline comments from `gh api .../comments`, filter out any whose `id` field appears in the addressed_comments file — treat those as already resolved
   - Track which inline comment IDs you acted on during this pass
   - Read relevant source files
   - Make minimal changes to address feedback
   - Run project tests and formatting (e.g., `go test ./...`, `gofmt -w .`)
   - Commit: `git add -A && git commit -m "refactor: address review feedback on ..."`
   - Push: `git push`
   - Append the acted-on inline comment IDs to `.sandman/.<N>.addressed_comments` so they do not re-trigger in future passes

7. **Repeat** from step 2. Before re-requesting a review in step 3, check whether the previous `{{REVIEW_COMMAND}}` has already received a response. If it hasn't, skip the re-request and go directly to polling (step 4). After the final pass, request one last review and report the outcome.

### State files

- `.sandman/.<N>.addressed_comments` — one inline comment ID per line, tracking which inline comments have already been acted on and should not trigger the fix loop again. State files for this skill MUST be saved under the `.sandman/` directory; never at the repo root or any other location.
- Initialized empty on first pass; appended to after each fix+push cycle
- If only already-addressed inline comment IDs remain (no new IDs), re-request review instead of declaring done

### Same comment ID 3+ passes without resolution

If an inline comment ID appears in 3+ consecutive passes without resolution (i.e., it keeps appearing in the inline comments after each fix+push cycle), treat it as unresolvable without a larger redesign. In this case:
- Do not keep looping on the same comment ID
- Re-request review instead, noting the comment ID in the PR comment

## Never give up conditions

Stop polling and report to the user **only** when:
- Formal approval is detected (A or C above)
- User explicitly asks to stop
- Max 10 passes reached with unresolved blockers

Continue polling (do NOT stop) when:
- Review is pending (`reviewDecision: "REVIEW_REQUIRED"` or no reviews yet)
- Only boilerplate setup comments exist in the formal reviews — the top-level PR conversation may still carry a real reviewer response, so keep polling and apply the Step 4 self-check
- Only nits or suggestions remain (F above) that have not yet been addressed
- CI is still running
- Any `CHANGES_REQUESTED` review exists but can be addressed
- Only already-addressed inline comment IDs remain and no new feedback has arrived — re-request review instead of concluding done
- The top-level PR conversation has new comments from a non-agent author even when `reviews == 0` and `inline == 0` — this is a reviewer response, not a no-signal state (see Hard rule 4 and the Step 4 self-check)

Note: Some review agents (e.g., Codex) post an initial boilerplate comment ("Here are some automated review suggestions / ℹ️ About Codex...") that is not real feedback. Continue polling after seeing this — wait for actual inline comments or non-boilerplate review body.

## Tips

- Use `gh pr view <N> --repo <owner/repo> --json state,mergeStateStatus` to check merge readiness after approval.
- Always include the final `top=<count> reviews=<count> inline=<count>` counter line in the final report to the user.
- Keep commits focused: one commit per review round is fine.
    - If feedback is ambiguous because actionable intent is unclear, use the ambiguity branch: ask for clarification in a PR comment with `{{REVIEW_COMMAND}}`, then keep polling without changing code.
- Never force-push or amend commits.
- Always delegate the review request to the PR Review Agent via `{{REVIEW_COMMAND}}` — you are strictly forbidden from reviewing your own PR in the same session.
- Review agents may post feedback as: top-level PR comments, inline diff comments, or formal reviews with `COMMENT` event. Always check all three sources.
- When an inline comment's ID appears in `.sandman/.<N>.addressed_comments`, skip it — it has already been addressed and should not re-trigger the fix loop.
- If only already-addressed IDs remain (no new inline comments), re-request review rather than declaring done.
- If the same inline comment ID appears across 3+ consecutive passes, treat it as unresolvable without a larger redesign — re-request review instead of continuing to loop.
