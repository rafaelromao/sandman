---
tags: [skill-creator]
max_turns: 8
timeout_seconds: 180
allowed_tools: [Read, Write]
---

Apply the supplied code-review skill in pull-request review context. The fixture below is the authoritative review context and contains all relevant worktree evidence. Review focus is correctness and regression coverage. Local quality rules are unavailable. Historical excerpts may be incomplete. Use only the supplied evidence and write the review to `decision.md`. Do not contact external services.

Acceptance criteria: preserve title and filter settings on pagination. Prior review found that pagination dropped filters. Applied fix at `catalog/pager.ts:19` carries the prior filter through all entry points. The supplied regression passes and current edits leave that path intact. The current patch at `catalog/title.ts:8` truncates the last character when copying the title, with a separate helper and no shared cause. The criterion requires the full title.

Use the existing report contract. Include Summary, Standards, Spec, Quality check, Findings, Suggested next steps, and Decision. Include Previous review progress only when prior-review context is supplied. The verdict is **CHANGES_REQUESTED** for any Blocking or Important finding, and **APPROVED** otherwise.
