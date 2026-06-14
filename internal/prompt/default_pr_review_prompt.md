# PR Review

Review pull request #{{PR_NUMBER}}: {{PR_TITLE}}

## PR Context

{{PR_BODY}}

## Review Focus

{{REVIEW_FOCUS}}

## Runtime Context

- You are running inside a Sandman-created worktree on a dedicated review branch.
- The current PR is #{{PR_NUMBER}}. Title and body are pre-fetched above.
- When the focus section is empty, perform a general code review. When it contains guidance, treat it as the reviewer's stated priorities.

## Review Procedure

### Pre-flight checks

Before performing the review, ensure the PR is in a healthy state:

1. **Resolve merge conflicts.** Check the PR's mergeability:
   ```bash
   gh pr view {{PR_NUMBER}} --json mergeable
   ```
   If the `mergeable` field is `CONFLICTING`, resolve conflicts by checking out the PR branch, merging the base branch into it, resolving conflicts, committing, and pushing. Use `gh pr view` to find the head and base refs.

2. **Fix broken CI.** After ensuring there are no merge conflicts, check CI status:
   ```bash
   gh pr checks {{PR_NUMBER}}
   ```
   If any required checks are failing, investigate the failure logs, fix the issue on the PR branch, commit, and push. Repeat until checks are green. If the failure is outside the scope of the PR (e.g. a flaky unrelated test), note it but proceed with the review. If you lack push access to the PR branch, note the failure but proceed with the review.

### Core review

3. Fetch the PR diff with `gh pr diff {{PR_NUMBER}}` and read it end to end.

4. **Analyse previous review progress.** Fetch prior review comments and reviews on this PR:
   ```bash
   gh api "/repos/{owner}/{repo}/pulls/{{PR_NUMBER}}/comments" --paginate
   gh api "/repos/{owner}/{repo}/pulls/{{PR_NUMBER}}/reviews" --paginate
   ```
   Compare the prior feedback against what has changed since the last review cycle. Report which items were addressed, partially resolved, or remain outstanding. If there are no prior reviews, skip this step entirely and **omit** the `## Previous review progress` section from the posted comment.

5. **Cross-reference against the original task specification.** Look for an issue reference in the PR body (e.g. `Fixes #N`, `Closes #N`, `#N`, or `refs N`). If found, fetch the issue body:
   ```bash
   gh issue view <N> --json title,body
   ```
   Verify that the implementation matches the issue's requirements and acceptance criteria. If the issue body references a spec or design doc, check those too. If no issue reference is found, skip this step gracefully.

6. **Cross-reference parent issue acceptance criteria.** Look for a `## Parent` section in the PR body or the linked issue body that references another issue (e.g. `Parent: #N` or `## Parent\n#N`). If found, fetch that parent issue:
   ```bash
   gh issue view <N> --json title,body
   ```
   Verify that the implementation satisfies the parent issue's acceptance criteria. This mirrors the existing issue-reference logic but handles the structured parent reference used in this repo's issue templates. If no parent reference is found, skip this step gracefully.

7. Read the repo's documented coding standards in `CLAUDE.md` (or `AGENTS.md` if present) and domain vocabulary in `CONTEXT.md`, plus the ADRs in `docs/adr/` that overlap with the changed code. Check for:
   - Coding style and conventions documented in `CLAUDE.md`.
   - Domain terminology defined in `CONTEXT.md` — flag names, file paths, function names, and error messages should match.
   - ADR decisions that constrain the area being modified.

8. For every file in the diff, compare the change against those standards and the surrounding code. Look for:
   - Behaviour that breaks an ADR or a documented invariant in `CONTEXT.md`.
   - Bugs, race conditions, or error paths the diff did not cover.
   - Missing tests for new behaviour, edge cases, or failure modes.
   - Inconsistencies with the repo's language and naming (domain vocabulary, flag terms, file paths).
   - Inconsistencies with existing patterns in the surrounding code — if neighbouring functions use a certain style or abstraction, the new code should follow suit.
   - Unsafe, destructive, or surprising operations (force pushes, hard deletes, broad `chmod`, unanchored curls, etc.).

9. When you find an issue, cite the file and line range, quote the offending snippet, and describe the concrete fix.

## Posting the Review

Post your findings as a single PR comment using the GitHub CLI:

```bash
gh pr comment {{PR_NUMBER}} --body "..."
```

Format the body as Markdown with the following sections:

- `## Summary` — one paragraph describing what the PR does.
- `## Previous review progress` — **only include this section if previous reviews exist**. List each prior finding and its status: **resolved**, **partially addressed**, or **still outstanding**. If there were no prior reviews, **omit this section entirely** (do not write "No previous reviews found.").
- `## Findings` — bulleted list. Group by severity (`Blocking`, `Important`, `Nit`). If there are no findings in a group, omit it.
- `## Suggested next steps` — the minimum set of follow-ups for the author.
- `## Decision` — If there are zero `Blocking` or `Important` findings, place a single line: `**APPROVED**`. Otherwise, place `**CHANGES_REQUESTED**`.

Keep the comment terse and actionable. Do not post review commentary outside the single `gh pr comment` invocation.

## AFK Rule

This is an Away From Keyboard workflow. Do not ask the user for approval, confirmation, or decisions during execution. Produce the comment, post it, and exit.

## Search Scope Restriction

Never run grep, rg, find, or any recursive content/file search against directories outside the current working directory (e.g. /tmp, /var, /usr, /etc, /opt, /home, node_modules, .git, target, dist, build, vendor). Such searches return massive output that floods the context window. Restrict searches to the cwd or explicit sub-paths within it; use the Glob/Grep tools which already scope to the project by default.
