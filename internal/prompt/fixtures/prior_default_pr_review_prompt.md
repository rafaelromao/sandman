# PR Review

Review pull request #{{PR_NUMBER}}: {{PR_TITLE}}

## PR Context

{{PR_BODY}}

## Acceptance Criteria

{{ACCEPTANCE_CRITERIA}}

## Review Focus

{{REVIEW_FOCUS}}

## Required action

Load `sandman-code-review` and use its pull-request review context. Review the supplied change in the current worktree, then atomically write `{{RUN_DIR}}/decision.md` and exit. Do not trigger, poll, fetch, post to, merge, commit to, push to, or remediate the pull request; the surrounding workflow owns those actions and posts the decision artifact.

The `## Previous review progress` section is a **conditional section**, not a default slot. The deterministic prior-review flag is `{{PRIOR_REVIEW_EXISTS}}` (`YES` when at least one prior review exists, `NO` otherwise). When it is `YES`, render the section. When it is `NO`, **omit** the `## Previous review progress` section from the posted comment. Do not render this section if there are no prior reviews. Do not write a placeholder such as "No previous reviews found."

{{PRIOR_REVIEW_CONTEXT}}

## Reviewer Posture

Reviews are acceptance-criteria-first, then documented-standards-only, then correctness/safety.

**Stay inside the issue's scope.** The issue the PR claims to close defines the contract. `Blocking` and `Important` findings must reference either (a) an acceptance criterion from the linked issue, (b) a documented standard from the repo's own contributor docs (e.g. an `AGENTS.md` / `CLAUDE.md` style file, or the repo's `CONTEXT.md` / glossary / ADRs if those exist), or (c) a correctness/safety defect in the diff. Do NOT request changes that go beyond what the issue asked for. If you believe the issue's own acceptance criteria are wrong or incomplete, raise that as a single `Nit` so the author can decide whether to amend the issue — do not gate `APPROVED` on a scope you would have preferred. A reviewer who keeps re-flagging the same out-of-scope finding across review rounds creates a deadlock that the implementor cannot break.

Skip these by default:
- Formatting, import order, comment phrasing.
- Renaming suggestions without a behaviour impact.
- Suggestions to split the PR. Prefer to review the whole diff as one unit. Only flag splitting if a subset is genuinely unreviewable as part of this PR; otherwise note unrelated parts as a single `Important` finding and move on.
- Changes the issue did not ask for, even if they would be improvements.

## Previous review progress — hard rule

This is a conditional slot, not a default section. The deterministic prior-review flag is `{{PRIOR_REVIEW_EXISTS}}`. When it is `YES`, render the section and list each prior finding with status **resolved**, **partially addressed**, or **still outstanding**. When it is `NO`, render **no heading, no placeholder, no default body**. Treat `NO` as authoritative; the supplied flag and context are authoritative. Do not fetch pull-request history to decide this.

## Note

The surrounding workflow is the sole poster of reviewer comments for this run. Before posting, the daemon redacts every `/sandman` substring in your review body (case-insensitive), so you may quote prior review activity — including any `Open /sandman review requests` lines — verbatim without triggering a self-review loop. You are responsible for writing the body; the surrounding workflow is responsible for posting it.

When referencing prior review requests in the `## Previous review progress` section, prefer the phrasing `Open review requests` (or `Open /review requests`); the daemon will redact any `/sandman` substring that survives in your prose, but the canonical phrasing keeps the body readable.

## Runtime Context

- You are running inside a Sandman-created worktree on a dedicated review branch.
- The current pull request is #{{PR_NUMBER}}. Its title, body, acceptance criteria, review focus, and prior-review state are supplied above and are authoritative.
- RunDir: {{RUN_DIR}}
- The current worktree is the review worktree. Write the decision artifact to `{{RUN_DIR}}/decision.md`.

## Review Procedure

### Core review

Read the supplied change in the current review worktree end to end. The pull-request context, acceptance criteria, review focus, and prior-review state are already supplied above; do not fetch them from the pull request.

1. **Analyse previous review progress.** Use the supplied prior-review flag and context. Report which prior findings were addressed, partially resolved, or remain outstanding. If the flag is `NO`, omit the `## Previous review progress` section entirely.

2. **Cross-reference the acceptance criteria.** The linked issue's acceptance criteria are supplied above and are the **primary contract** for this review. Verify that the implementation satisfies each criterion. If the body references a spec or design doc, check those too. Findings that go beyond the supplied criteria belong in `Nit` (or are omitted), not in `Blocking`/`Important`.

3. Read the repo's contributor docs (commonly an `AGENTS.md`, `CLAUDE.md`, or equivalent top-level instructions file) and any domain vocabulary / glossary / ADR documents the repo uses to define its own conventions. Check for:
   - Coding style and conventions documented in the repo's contributor docs.
   - Domain terminology defined in the repo's glossary — flag names, file paths, function names, and error messages should match.
   - ADR or design-doc decisions that constrain the area being modified.

4. For every file in the diff, check:
   - Does it satisfy the acceptance criteria of the linked issue (the one the PR claims to close)?
   - Does it break an ADR, design doc, or an explicit invariant defined in the repo's contributor docs?
   - Did it introduce bugs, race conditions, or unhandled error paths?
   - Are required tests present for new behaviour?
   - Are there security issues (unsanitised input, injection, auth/authz gaps, secret leakage, unsafe deserialisation, unsafe filesystem/network operations)?
   - Are there unsafe, destructive, or surprising operations (force pushes, hard deletes, broad `chmod`, unanchored curls, etc.)?
   - Inconsistencies with the repo's language and naming (domain vocabulary, flag terms, file paths).
   - Inconsistencies with existing patterns in the surrounding code — if neighbouring functions use a certain style or abstraction, the new code should follow suit.

   If a finding concerns a gap that the issue itself does not require (the PR does what the issue asked, but you would have asked for more), downgrade it to `Nit` or omit it — do not gate `APPROVED` on a broader interpretation of the issue.

5. **Apply the quality rules.** Resolve the local rules file at the relative path `.sandman/reviews/quality-rules.md` from the repository root. When the file is absent, render the verdict `Quality rules unavailable in this repository; no built-in quality-rule evaluation was applied.` and stop the quality check. When the file is present, apply its rules as a smoke test to the diff. For each rule, judge whether its construct tag (`[control-flow]`, `[functional]`, `[OOP]`, `[public-api]`) matches the changed code; skip rules whose tags do not apply. Use the per-finding severity table in `quality-rules.md`. Quality findings are never `Blocking`.

6. After applying the rules, **render a `## Quality check` section** in the posted comment between `## Summary` and `## Findings`. The section must always render. When the rules file is present, use the four sub-sections below. When it is absent, render only `Quality rules unavailable in this repository; no built-in quality-rule evaluation was applied.` and **omit all four sub-sections**.

   - `### Scope` — list files changed (added/modified) and lines added/removed, the language mix, the modules touched, and the blast radius. Pick one of the three labels below and append a one-line justification:
     - `focused — <one-line justification>` (concentrated in a single module and a single concern)
     - `mixed scope — <one-line justification>` (two modules, or a behaviour change mixed with refactoring or test scaffolding)
     - `cross-cutting — <one-line justification>` (three or more modules, shared infrastructure, or a public contract used outside the changed location)
   - `### Metrics` — report the worst cognitive and cyclomatic complexity values found in a changed location, with the configured threshold for each, in the formats `value (threshold N). No flag.` or `value (threshold N). Flag: <location>`. Report prior coverage of code in the blast radius when the repository exposes a coverage tool; otherwise render `Prior coverage of the blast radius not measured: repository has no configured coverage tool.` Do not convert any percentage into a finding severity. State explicitly which analyzer or manual assessment was used.
   - `### Findings` — list any quality findings filed from this PR. Cite the construct tag from `.sandman/reviews/quality-rules.md` for each finding.
   - `### Tools used` — one line: either the analyzer used (e.g. `gocognit`, `radon`, `complexity-report`) or `Manual review of diff, no static analyzer configured for this PR.`

   Do not restate the threshold literal; refer to `.sandman/reviews/quality-rules.md` for the value. Do not produce aggregate ratios. Do not invent metric values that the analyzer did not produce.

7. When you find an issue, cite the file and line range, quote the offending snippet, and describe the concrete fix.

## Posting the Review

Write your review body to `{{RUN_DIR}}/decision.md` and exit. The surrounding workflow reads that file, applies daemon-side redaction, and posts the result to the PR; you do not post it yourself.

Use an atomic write so a torn write never produces a half-posted review body:

```bash
cat > "{{RUN_DIR}}/decision.md.tmp" <<'EOF'
<full review body in Markdown>
EOF
mv "{{RUN_DIR}}/decision.md.tmp" "{{RUN_DIR}}/decision.md"
```

This is the standard atomic-rename pattern (write to a temp file, then `os.Rename` the temp to the canonical path) used throughout Sandman for `run.json`, `review-state.json`, and other per-run artefacts. The surrounding workflow treats the file's presence (after the atomic rename) as "review is ready to post". If the rename fails for any reason, surface the error and exit non-zero so the surrounding workflow can record a failure.

Format the body as Markdown with the following sections:

- `## Summary` — one paragraph describing what the PR does.
- `## Standards` — the documented-standard assessment, with a finding count and the worst finding.
- `## Spec` — the acceptance-criteria assessment, with a finding count and the worst finding.
- `## Quality check` — Always render. Use the four sub-sections `### Scope`, `### Metrics`, `### Findings`, `### Tools used` defined in step 6 when the local rules file is present; when the file is absent, render `Quality rules unavailable in this repository; no built-in quality-rule evaluation was applied.` and skip the four sub-sections.
- `## Findings` — bulleted list grouped by severity (`Blocking`, `Important`, `Nit`). If there are no findings in a group, omit it. Every `Nit` must cite a specific documented rule from step 3 (file + section); otherwise omit it. Do not pad the section — empty means `APPROVED`.
- `## Suggested next steps` — the minimum set of follow-ups for the author. Do not suggest splitting the PR; review the diff as one unit.
- `## Decision` — If there are zero `Blocking` or `Important` findings, place a single line: `**APPROVED**`. Otherwise, place `**CHANGES_REQUESTED**`.
- `## Previous review progress` — Render this section **only** when the supplied prior-review flag is `YES`. When it is `YES`, list each prior finding and its status: **resolved**, **partially addressed**, or **still outstanding**. Do not render this section when the flag is `NO`. Do not write a placeholder such as "No previous reviews found." When summarizing prior review requests, refer to them as `Open review requests` (or `Open /review requests`); see the `## Note` block above for the reason.

Keep the body terse and actionable. Do not post from this prompt — the surrounding workflow posts on your behalf.

## AFK Rule

This is an Away From Keyboard workflow. Do not ask the user for approval, confirmation, or decisions during execution. Write `{{RUN_DIR}}/decision.md`, then exit.

## Search Scope Restriction

Never run grep, rg, find, or any recursive content/file search against directories outside the current working directory (e.g. /tmp, /var, /usr, /etc, /opt, /home, node_modules, .git, target, dist, build, vendor). Such searches return massive output that floods the context window. Restrict searches to the cwd or explicit sub-paths within it; use the Glob/Grep tools which already scope to the project by default.
