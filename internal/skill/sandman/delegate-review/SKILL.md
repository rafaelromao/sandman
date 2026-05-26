---
name: sandman-delegate-review
description: Automates the GitHub PR review loop with opencode. Waits for CI to pass, requests review from opencode by posting "{{REVIEW_COMMAND}}" on the PR, then polls for feedback, applies suggestions, commits, pushes, and repeats until approved or max 10 passes. Use when user says sandman delegate-review, wants a PR reviewed iteratively by opencode, wants to auto-address review feedback, or mentions review loop, {{REVIEW_COMMAND}}, or iterative PR approval.
---

# delegate_review

## Quick start

```bash
gh pr checks <N> --repo <owner/repo>
gh pr comment <N> --body "{{REVIEW_COMMAND}}" --repo <owner/repo>
gh pr view <N> --repo <owner/repo> --comments
```

## Hard rule

**You must NOT review the PR yourself in this session.**
Your only job is to delegate the review to opencode by posting `{{REVIEW_COMMAND}}` as a PR comment, then wait for opencode's feedback and act on it. Under no circumstances should you read the diff and provide your own review comments.

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

2. **Delegate review to opencode**
   ```bash
   gh pr comment <N> --repo <owner/repo> --body "{{REVIEW_COMMAND}}"
   ```
   **Do NOT read the PR diff or write review comments yourself.** The review must come exclusively from opencode.

3. **Wait for review** (timeout: 10 minutes)
   ```bash
   gh pr view <N> --repo <owner/repo> --comments
   ```
   Poll every 30–60s until a new comment or review from opencode appears.
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
- Always delegate the review request to opencode via `{{REVIEW_COMMAND}}` — you are strictly forbidden from reviewing your own PR in the same session.
