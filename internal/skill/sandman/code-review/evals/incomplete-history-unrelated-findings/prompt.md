---
tags: [skill-creator]
max_turns: 8
timeout_seconds: 180
allowed_tools: [Read, Write]
---

Apply the supplied code-review skill in pull-request review context. The fixture below is the authoritative review context and contains all relevant worktree evidence. Review focus is correctness and regression coverage. Local quality rules are unavailable. Historical excerpts may be incomplete. Use only the supplied evidence and write the review to `decision.md`. Do not contact external services.

Acceptance criteria: the selected view loads the saved filter and displays the selected sort order. Prior-review context includes two copies of “view settings can disagree” with no route, commit, cause, or reproduction. A separate earlier note confirms a stale label was corrected. At `catalog/sort_label.ts:8` the current diff maps descending order to the “Ascending” label. The saved filter path has no evidence of a defect. No other information establishes the earlier root cause.

Use the existing report contract. Include Summary, Standards, Spec, Quality check, Findings, Suggested next steps, and Decision. Include Previous review progress only when prior-review context is supplied. The verdict is **CHANGES_REQUESTED** for any Blocking or Important finding, and **APPROVED** otherwise.
