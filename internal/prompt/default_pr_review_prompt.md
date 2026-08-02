# PR Review

Review pull request #{{PR_NUMBER}}: {{PR_TITLE}}

## PR Context

{{PR_BODY}}

## Acceptance Criteria

{{ACCEPTANCE_CRITERIA}}

## Review Focus

{{REVIEW_FOCUS}}

## Runtime Context

- You are running inside a Sandman-created worktree on a dedicated review branch.
 - The current pull request is #{{PR_NUMBER}}. Its title, body, acceptance criteria, review focus, and prior-review state are supplied above and are authoritative.
 - RunDir: {{RUN_DIR}}
 - The current worktree is the review worktree. Write the decision artifact to `{{RUN_DIR}}/decision.md`.

## Required action

Load `sandman-code-review` and use its daemon-review context. Review the supplied change in the current worktree, then atomically write `{{RUN_DIR}}/decision.md` and exit. Do not trigger, poll, fetch, post to, merge, commit to, push to, or remediate the pull request; the daemon owns those actions and posts the decision artifact.

The `## Previous review progress` section is conditional. The deterministic prior-review flag is `{{PRIOR_REVIEW_EXISTS}}`: render the section only for `YES`; for `NO`, render no heading, placeholder, or default body.

{{PRIOR_REVIEW_CONTEXT}}

The code-review skill applies `.sandman/reviews/quality-rules.md` using applicable `[control-flow]`, `[functional]`, `[OOP]`, and `[public-api]` tags. If unavailable, it records `Quality rules unavailable in this repository; no built-in quality-rule evaluation was applied.` Its `## Quality check` contains `### Scope`, `### Metrics`, `### Findings`, and `### Tools used`; Scope uses `focused`, `mixed scope`, or `cross-cutting`.

## Search Scope Restriction

Never run grep, rg, find, or any recursive content/file search against directories outside the current working directory (e.g. /tmp, /var, /usr, /etc, /opt, /home, node_modules, .git, target, dist, build, vendor). Such searches return massive output that floods the context window. Restrict searches to the cwd or explicit sub-paths within it; use the Glob/Grep tools which already scope to the project by default.
