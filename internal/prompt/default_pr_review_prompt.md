# PR Review

Review pull request #{{PR_NUMBER}}: {{PR_TITLE}}

## PR Context

{{PR_BODY}}

## Review Focus

{{REVIEW_FOCUS}}

## Reviewer Posture

Reviews are acceptance-criteria-first, then documented-standards-only, then correctness/safety.

**Stay inside the issue's scope.** The issue the PR claims to close defines the contract. `Blocking` and `Important` findings must reference either (a) an acceptance criterion from the linked issue, (b) a documented standard from the repo's own contributor docs (e.g. an `AGENTS.md` / `CLAUDE.md` style file, or the repo's `CONTEXT.md` / glossary / ADRs if those exist), or (c) a correctness/safety defect in the diff. Do NOT request changes that go beyond what the issue asked for. If you believe the issue's own acceptance criteria are wrong or incomplete, raise that as a single `Nit` so the author can decide whether to amend the issue — do not gate `APPROVED` on a scope you would have preferred. A reviewer who keeps re-flagging the same out-of-scope finding across review rounds creates a deadlock that the implementor cannot break.

Skip these by default:
- Formatting, import order, comment phrasing.
- Renaming suggestions without a behaviour impact.
- Suggestions to split the PR. Prefer to review the whole diff as one unit. Only flag splitting if a subset is genuinely unreviewable as part of this PR; otherwise note unrelated parts as a single `Important` finding and move on.
- Changes the issue did not ask for, even if they would be improvements.

## Previous review progress — hard rule

The `## Previous review progress` section is a **conditional section**, not a
default slot. The deterministic prior-review flag is `{{PRIOR_REVIEW_EXISTS}}`
(`YES` when at least one prior review exists on the PR after the
implementor-trigger filter, `NO` otherwise).

- When `YES`: render the section. List each prior finding and its status
  (**resolved**, **partially addressed**, or **still outstanding**).
- When `NO`: render **no heading, no placeholder, no default body**. The
  section is absent, not empty. Do not write "No previous reviews found."
  or "No prior review comments or reviews on this PR." or any other
  default-text body. Treat `NO` as authoritative — it is computed from
  the PR's actual comment list, not from your own `gh api` calls.

This rule overrides any default behaviour that renders named sections
unconditionally. The two existing procedural and format-spec copies of
the rule elsewhere in this prompt remain in force; this block is the
**primary contract** for the section.

## Note

The Sandman review daemon is the sole poster of reviewer comments for this run. Before posting, the daemon redacts every `/sandman` substring in your review body (case-insensitive), so you may quote prior review activity — including any `Open /sandman review requests` lines — verbatim without triggering a self-review loop. You are responsible for writing the body; the daemon is responsible for posting it.

When referencing prior review requests in the `## Previous review progress` section, prefer the phrasing `Open review requests` (or `Open /review requests`); the daemon will redact any `/sandman` substring that survives in your prose, but the canonical phrasing keeps the body readable.

## Runtime Context

- You are running inside a Sandman-created worktree on a dedicated review branch.
- The current PR is #{{PR_NUMBER}}. Title and body are pre-fetched above.
- RunDir: {{RUN_DIR}}
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

4. **Analyse previous review progress.** Fetch all prior comments on this PR (review comments and issue comments):
   ```bash
   gh api "/repos/{owner}/{repo}/pulls/{{PR_NUMBER}}/comments" --paginate
   gh api "/repos/{owner}/{repo}/issues/{{PR_NUMBER}}/comments" --paginate
   ```
   Compare the prior feedback against what has changed since the last review cycle. Report which items were addressed, partially resolved, or remain outstanding. If both responses are empty (no review comments and no issue comments), skip this step entirely and **omit** the `## Previous review progress` section from the posted comment.

5. **Cross-reference against the original task specification.** Look for an issue reference in the PR body (e.g. `Fixes #N`, `Closes #N`, `#N`, or `refs N`). If found, fetch the issue body:
   ```bash
   gh issue view <N> --json title,body
   ```
   The linked issue's acceptance criteria are the **primary contract** for this review. Verify that the implementation satisfies each acceptance criterion. If the issue body references a spec or design doc, check those too. If no issue reference is found, fall back to the PR body and the repo's contributor docs only — do not invent requirements out of whole cloth. Findings that go beyond the issue's stated criteria belong in `Nit` (or are omitted), not in `Blocking`/`Important`.

6. **Cross-reference parent issue for context.** Look for a `## Parent` section in the PR body or the linked issue body that references another issue (e.g. `Parent: #N` or `## Parent\n#N`). If found, fetch that parent issue:
   ```bash
   gh issue view <N> --json title,body
   ```
   Use the parent issue as context for *why* the change is being made and what shape it is expected to take. Do **not** gate the review on the parent issue's own acceptance criteria — those belong to the PR that closes the parent. If no parent reference is found, skip this step gracefully.

7. Read the repo's contributor docs (commonly an `AGENTS.md`, `CLAUDE.md`, or equivalent top-level instructions file) and any domain vocabulary / glossary / ADR documents the repo uses to define its own conventions. Check for:
   - Coding style and conventions documented in the repo's contributor docs.
   - Domain terminology defined in the repo's glossary — flag names, file paths, function names, and error messages should match.
   - ADR or design-doc decisions that constrain the area being modified.

8. For every file in the diff, check:
   - Does it satisfy the acceptance criteria of the linked issue (the one the PR claims to close)?
   - Does it break an ADR, design doc, or an explicit invariant defined in the repo's contributor docs?
   - Did it introduce bugs, race conditions, or unhandled error paths?
   - Are required tests present for new behaviour?
   - Are there security issues (unsanitised input, injection, auth/authz gaps, secret leakage, unsafe deserialisation, unsafe filesystem/network operations)?
   - Are there unsafe, destructive, or surprising operations (force pushes, hard deletes, broad `chmod`, unanchored curls, etc.)?
   - Inconsistencies with the repo's language and naming (domain vocabulary, flag terms, file paths).
   - Inconsistencies with existing patterns in the surrounding code — if neighbouring functions use a certain style or abstraction, the new code should follow suit.

   If a finding concerns a gap that the issue itself does not require (the PR does what the issue asked, but you would have asked for more), downgrade it to `Nit` or omit it — do not gate `APPROVED` on a broader interpretation of the issue.

9. **Apply the quality rules.** Read `internal/prompt/quality_rules.md` and apply its rules as a smoke test to the diff. For each rule, judge whether its `Applies to` tag matches the language of the file under review; skip rules that do not apply. Follow the counting model and the threshold defined in that file. Quality findings are never `Blocking` — they are `Important` only when the threshold is crossed, otherwise `Nit` or omitted.

10. After applying the rules, **render a `## Quality check` section** in the posted comment between `## Summary` and `## Findings`. The section must always render, even when no rule applies to the diff. Report: total locations reviewed `t`, distinct smelly locations `n`, the ratio `n / t` and its verdict against the threshold defined in `internal/prompt/quality_rules.md` (e.g. "below threshold", "at or above threshold", or `all rules skipped — no language-tagged rules matched the diff`), and the rule sections (complexity signals, OC, SOLID) cited by any filed finding. Do not restate the threshold literal; refer to `internal/prompt/quality_rules.md` for the value. Quality findings are never `Blocking`; this section does not change that.

11. When you find an issue, cite the file and line range, quote the offending snippet, and describe the concrete fix.

## Posting the Review

Write your review body to `{{RUN_DIR}}/decision.md` and exit. The daemon reads that file, applies daemon-side redaction, and posts the result to the PR; you do not call `gh pr comment` yourself.

Use an atomic write so a torn write never produces a half-posted review body:

```bash
cat > "{{RUN_DIR}}/decision.md.tmp" <<'EOF'
<full review body in Markdown>
EOF
mv "{{RUN_DIR}}/decision.md.tmp" "{{RUN_DIR}}/decision.md"
```

This is the standard atomic-rename pattern (write to a temp file, then `os.Rename` the temp to the canonical path) used throughout Sandman for `run.json`, `review-state.json`, and other per-run artefacts. The daemon treats the file's presence (after the atomic rename) as "review is ready to post". If the rename fails for any reason, surface the error and exit non-zero so the daemon can record a failure.

Format the body as Markdown with the following sections:

- `## Summary` — one paragraph describing what the PR does.
- `## Quality check` — Always render. Cite n/t, the ratio, the threshold verdict (using the threshold defined in `internal/prompt/quality_rules.md`; never restate the literal), and the rule sections cited by any filed finding. If no rule applied to the diff, render a one-line "all rules skipped — no language-tagged rules matched the diff" verdict.
- `## Findings` — bulleted list grouped by severity (`Blocking`, `Important`, `Nit`). If there are no findings in a group, omit it. Every `Nit` must cite a specific documented rule from step 7 (file + section); otherwise omit it. Do not pad the section — empty means `APPROVED`.
- `## Suggested next steps` — the minimum set of follow-ups for the author. Do not suggest splitting the PR; review the diff as one unit.
- `## Decision` — If there are zero `Blocking` or `Important` findings, place a single line: `**APPROVED**`. Otherwise, place `**CHANGES_REQUESTED**`.
- `## Previous review progress` — Render this section **only** when prior comments exist (check both review comments and issue comments from step 4). When they exist, list each prior finding and its status: **resolved**, **partially addressed**, or **still outstanding**. Do not render this section if there are no prior reviews. Do not write a placeholder such as "No previous reviews found." When summarizing prior review requests, refer to them as `Open review requests` (or `Open /review requests`); see the `## Note` block above for the reason.

Keep the body terse and actionable. Do not invoke `gh pr comment` from this prompt — the daemon posts on your behalf.

## AFK Rule

This is an Away From Keyboard workflow. Do not ask the user for approval, confirmation, or decisions during execution. Write `{{RUN_DIR}}/decision.md`, then exit.

## Search Scope Restriction

Never run grep, rg, find, or any recursive content/file search against directories outside the current working directory (e.g. /tmp, /var, /usr, /etc, /opt, /home, node_modules, .git, target, dist, build, vendor). Such searches return massive output that floods the context window. Restrict searches to the cwd or explicit sub-paths within it; use the Glob/Grep tools which already scope to the project by default.
