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

1. **Wait for CI**
   ```bash
   gh pr checks <N> --repo <owner/repo>
   ```
   Poll until status is `pass`. If `fail`:
   - Read the failed job logs to identify the root cause.
   - Fix the error in the codebase.
   - Run local tests/formatting to verify the fix.
   - Commit and push: `git add -A && git commit -m "fix: resolve CI failure" && git push`
   - **Repeat Step 1** (wait for CI again).

2. **Delegate review to the PR Review Agent**

   Request a review with this exact command. Do not change the body of the request.

   ```bash
   gh pr comment <N> --repo <owner/repo> --body "{{REVIEW_COMMAND}}"
   ```
   **Do NOT read the PR diff or write review comments yourself.** The review must come exclusively from the PR Review Agent.

3. **Wait for review** (timeout: 10 minutes)
  Poll every 30–60s using all three commands:
  ```bash
  gh pr view <N> --repo <owner/repo> --comments
  gh pr view <N> --repo <owner/repo> --json latestReviews,reviews,comments,reviewDecision,mergeStateStatus
  gh api repos/<owner>/<repo>/pulls/<N>/comments
  gh api repos/<owner>/<repo>/pulls/<N>/reviews
  ```
  Merge all sources before classifying feedback.
  Read every new PR Review Agent comment from all sources, including inline file comments.
  Do not overlook comments attached to a file diff instead of the top-level conversation.
  Treat any requested change in an inline file comment as actionable feedback.
  If no response arrives within 10 minutes, stop and report to the user.

4. **Read and classify feedback**
   - **Approve** → done, report to user
   - **Blockers** → must fix before continuing
   - **Suggestions** → fix if straightforward; ask user if requires major redesign
   - **Nits** → fix if trivial; skip if purely stylistic disagreement

5. **Apply fixes**
   - Read relevant source files
   - Make minimal changes to address feedback
   - Run project tests and formatting (e.g., `go test ./...`, `gofmt -w .`)
   - Commit: `git add -A && git commit -m "refactor: address review feedback on ..."`
   - Push: `git push`

6. **Repeat** from step 2. After the final pass, request one last review and report the outcome.

## Tips

- Use `gh pr view <N> --repo <owner/repo> --json state,mergeStateStatus` to check merge readiness after approval.
- Keep commits focused: one commit per review round is fine.
- If the reviewer asks a question rather than giving actionable feedback, answer in a PR comment and re-request review.
- Never force-push or amend commits.
- Always delegate the review request to the PR Review Agent via `{{REVIEW_COMMAND}}` — you are strictly forbidden from reviewing your own PR in the same session.
