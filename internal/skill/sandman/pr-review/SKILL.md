---
name: sandman-pr-review
description: Automates the GitHub PR review loop with the PR Review Agent. Waits for CI to pass, requests review from the PR Review Agent by posting "{{REVIEW_COMMAND}}" on the PR, then polls for feedback, applies suggestions, commits, pushes, and repeats until approved or max 10 passes. Use when user says sandman pr-review, wants a PR reviewed iteratively by the PR Review Agent, wants to auto-address review feedback, or mentions review loop, {{REVIEW_COMMAND}}, or iterative PR approval.
---

# PR Review

## Hard rule

**You must NOT review the PR yourself in this session.**
Your only job is to delegate the review to the PR Review Agent by posting `{{REVIEW_COMMAND}}` as a PR comment, then wait for the PR Review Agent's feedback and act on it. Under no circumstances should you read the diff and provide your own review comments.

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

      Poll until status is `pass`. If `fail`:

      - If there are merge conflicts, load the `sandman-merge` skill and merge the base branch into the local branch.
      - Read the failed job logs to identify the root cause.
      - Fix the error in the codebase.
      - Run local tests/formatting to verify the fix.
      - Commit and push: `git add -A && git commit -m "fix: resolve CI failure" && git push`
      - **Repeat Step 2** (wait for CI again).

3. **Delegate review to the PR Review Agent**

    Request a review with this exact command. Do not change the body of the request.

    ```bash
    gh pr comment <N> --repo <owner/repo> --body "{{REVIEW_COMMAND}}"
    ```
    **Do NOT read the PR diff or write review comments yourself.** The review must come exclusively from the PR Review Agent.

4. **Wait for review** (timeout: 10 minutes)
      Poll every 30–60s using all three commands below. Each API returns different data — merge all three before classifying feedback:

    ```bash
    gh pr view <N> --repo <owner/repo> --json comments,reviewDecision,mergeStateStatus
    gh api repos/<owner>/<repo>/pulls/<N>/reviews
    gh api repos/<owner>/<repo>/pulls/<N>/comments
    ```

   What each command returns:
   - `gh pr view --json comments` — top-level PR conversation comments only (NOT inline diff comments)
   - `gh api .../reviews` — all reviews with their state, body, and commit. Use this for review content, NOT `latestReviews` which is a truncated preview with empty fields
   - `gh api .../comments` — inline file-level review comments on the diff (these are separate from top-level comments)

   Read every new PR Review Agent comment from all sources, including inline file comments.
   Do not overlook comments attached to a file diff instead of the top-level conversation.
   Treat any requested change in an inline file comment as actionable feedback.
   If no response arrives within 10 minutes, stop and report to the user.

5. **Read and classify feedback**

   Merge data from all three sources above, then apply this decision tree:

   **A. Formal approval detected?**
   - `reviewDecision: APPROVED` in the JSON, OR
   - Any review entry with `state: "APPROVED"`
   → **Approve** — done, report to user

   **B. Formal changes requested?**
   - `reviewDecision: CHANGES_REQUESTED`, OR
   - Any review entry with `state: "CHANGES_REQUESTED"`
   → **Blockers** — must fix before continuing

   **C. Informal approval (implicit approval without formal review)?**
   - No pending `CHANGES_REQUESTED` reviews, AND
   - At least one of these conditions:
     - A review with `state: "COMMENTED"` whose body contains approval keywords (see list below), OR
     - A PR comment (not attached to a diff line) from a known reviewer whose body contains approval keywords
   → **Approve** — report as informal approval

   Approval keywords to search for (case-insensitive, partial match):
   `lgtm`, `looks good`, `looks good to me`, `looks great`, `looks nice`,
   `nice work`, `good work`, `great work`, `approved`, `ship it`, `+1`,
   `thumbs up`, `all good`, `all set`, `good to go`, `go ahead`

   **D. Still pending?**
   - `reviewDecision: "REVIEW_REQUIRED"` or absent, AND
   - No reviews with `state: "APPROVED"` or `state: "CHANGES_REQUESTED"`, AND
   - Either:
     - No review comments exist at all (first poll), OR
     - All reviews are boilerplate-only (see below), AND
     - No inline file comments exist
   → **Still waiting** — continue polling, do not give up

   **E. Real feedback detected?**
   Inline file comments exist (from `gh api .../pulls/<N>/comments`), OR review body contains concrete code feedback beyond boilerplate:
   - Boilerplate pattern: body starts with "### 💡" or "### Codex Review" and only contains "Here are some automated review suggestions" without mentioning specific files, functions, variable names, or line numbers
   - Real feedback: mentions specific file paths, function names, variable names, or requests concrete code changes
   → **Has blockers or suggestions** — treat as actionable feedback, apply fixes, commit, push, and re-request review

   **F. Only nits, suggestions, or questions?**
   - Comments that are nits (minor stylistic), suggestions (optional improvements), or questions (not blocking)
   - No `CHANGES_REQUESTED` reviews
   → **Suggestions** — fix if straightforward; skip if requires non-trivial redesign and re-request review after addressing what is straightforward

6. **Apply fixes**
   - Read relevant source files
   - Make minimal changes to address feedback
   - Run project tests and formatting (e.g., `go test ./...`, `gofmt -w .`)
   - Commit: `git add -A && git commit -m "refactor: address review feedback on ..."`
   - Push: `git push`

7. **Repeat** from step 2. After the final pass, request one last review and report the outcome.

## Never give up conditions

Stop polling and report to the user **only** when:
- Formal approval is detected (A or C above)
- User explicitly asks to stop
- Max 10 passes reached with unresolved blockers

Continue polling (do NOT stop) when:
- Review is pending (`reviewDecision: "REVIEW_REQUIRED"` or no reviews yet)
- Only boilerplate setup comments exist — the review has not yet produced real feedback
- Only nits or suggestions remain (E above) — still should wait for formal or informal approval
- CI is still running
- Any `CHANGES_REQUESTED` review exists but can be addressed

Note: Some review agents (e.g., Codex) post an initial boilerplate comment ("Here are some automated review suggestions / ℹ️ About Codex...") that is not real feedback. Continue polling after seeing this — wait for actual inline comments or non-boilerplate review body.

## Tips

- Use `gh pr view <N> --repo <owner/repo> --json state,mergeStateStatus` to check merge readiness after approval.
- Keep commits focused: one commit per review round is fine.
- If the reviewer asks a question rather than giving actionable feedback, answer in a PR comment and re-request review.
- Never force-push or amend commits.
- Always delegate the review request to the PR Review Agent via `{{REVIEW_COMMAND}}` — you are strictly forbidden from reviewing your own PR in the same session.
- Review agents may post feedback as: top-level PR comments, inline diff comments, or formal reviews with `COMMENT` event. Always check all three sources.