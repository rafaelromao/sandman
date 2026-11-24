---
tags: [skill-creator]
max_turns: 8
timeout_seconds: 180
allowed_tools: [Read, Write]
---

Apply the supplied code-review skill in pull-request review context. The fixture below is the authoritative review context and contains all relevant worktree evidence. Review focus is correctness and regression coverage. Local quality rules are unavailable. Historical excerpts may be incomplete. Use only the supplied evidence and write the review to `decision.md`. Do not contact external services.

Acceptance criteria: catalog exports must contain only records for the selected workspace. Existing JSON and CSV exporters both consume `catalog/export_records.ts`. At line 21 the current change replaces a filtered store call with `store.listAll()`; neither exporter filters again. Existing tests use a single workspace. No prior review exists.

Use the existing report contract. Include Summary, Standards, Spec, Quality check, Findings, Suggested next steps, and Decision. Include Previous review progress only when prior-review context is supplied. The verdict is **CHANGES_REQUESTED** for any Blocking or Important finding, and **APPROVED** otherwise.
