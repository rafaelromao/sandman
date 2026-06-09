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

1. Fetch the PR diff with `gh pr diff {{PR_NUMBER}}` and read it end to end.
2. Read the repo's documented standards in `AGENTS.md` and `CONTEXT.md`, plus the ADRs in `docs/adr/` that overlap with the changed code.
3. For every file in the diff, compare the change against those standards and the surrounding code. Look for:
   - Behaviour that breaks an ADR or a documented invariant in `CONTEXT.md`.
   - Bugs, race conditions, or error paths the diff did not cover.
   - Missing tests for new behaviour, edge cases, or failure modes.
   - Inconsistencies with the repo's language and naming (domain vocabulary, flag terms, file paths).
   - Unsafe, destructive, or surprising operations (force pushes, hard deletes, broad `chmod`, unanchored curls, etc.).
4. When you find an issue, cite the file and line range, quote the offending snippet, and describe the concrete fix.

## Posting the Review

Post your findings as a single PR comment using the GitHub CLI:

```bash
gh pr comment {{PR_NUMBER}} --body "..."
```

Format the body as Markdown with the following sections:

- `## Summary` — one paragraph describing what the PR does.
- `## Findings` — bulleted list. Group by severity (`Blocking`, `Important`, `Nit`). If there are no findings in a group, omit it.
- `## Suggested next steps` — the minimum set of follow-ups for the author.
- `## Decision` — If there are zero `Blocking` or `Important` findings, place a single line: `**APPROVED**`. Otherwise, place `**CHANGES_REQUESTED**`.

Keep the comment terse and actionable. Do not post review commentary outside the single `gh pr comment` invocation.

## AFK Rule

This is an Away From Keyboard workflow. Do not ask the user for approval, confirmation, or decisions during execution. Produce the comment, post it, and exit.

## Search Scope Restriction

Never run grep, rg, find, or any recursive content/file search against directories outside the current working directory (e.g. /tmp, /var, /usr, /etc, /opt, /home, node_modules, .git, target, dist, build, vendor). Such searches return massive output that floods the context window. Restrict searches to the cwd or explicit sub-paths within it; use the Glob/Grep tools which already scope to the project by default.
