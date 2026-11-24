---
tags: [skill-creator]
max_turns: 8
timeout_seconds: 180
allowed_tools: [Read, Write]
---

Apply the supplied code-review skill in pull-request review context. The fixture below is the authoritative review context and contains all relevant worktree evidence. Review focus is correctness and regression coverage. Local quality rules are unavailable. Historical excerpts may be incomplete. Use only the supplied evidence and write the review to `decision.md`. Do not contact external services.

Acceptance criteria: optional filters remain selected when the user changes sort order or page. Supplied history contains two unattributed copies of a prior finding that the sort action rebuilt state without the filter. It also contains a comment reporting the sort handler now preserves it. No record establishes which comments belong to separate review rounds or which commits they reviewed.
The current checkout confirms the sort handler now spreads prior state. The unchanged pagination handler at `catalog/pager.ts:19` rebuilds the same state without the selected filter. Both write to the same selection store. Current tests cover sort with a filter, but pagination tests use no filter.

Use the existing report contract. Include Summary, Standards, Spec, Quality check, Findings, Suggested next steps, and Decision. Include Previous review progress only when prior-review context is supplied. The verdict is **CHANGES_REQUESTED** for any Blocking or Important finding, and **APPROVED** otherwise.
