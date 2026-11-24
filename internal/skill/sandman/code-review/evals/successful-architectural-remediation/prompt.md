---
tags: [skill-creator]
max_turns: 8
timeout_seconds: 180
allowed_tools: [Read, Write]
---

Apply the supplied code-review skill in pull-request review context. The fixture below is the authoritative review context and contains all relevant worktree evidence. Review focus is correctness and regression coverage. Local quality rules are unavailable. Historical excerpts may be incomplete. Use only the supplied evidence and write the review to `decision.md`. Do not contact external services.

Acceptance criteria: every generated export reference must round-trip through the parser and enforce the same limits. Earlier reviews found the generator and parser used incompatible validation and asked for a shared boundary.
In the current revision, both public entry points call `exports/reference.ts:9` for normalization and validation before encoding or returning a reference. Supplied diff and production call sites show both entry points use the same inclusive byte limit, reject invalid references, and support all previously valid formats. Tests exercise generated references through the parser with exact-limit and over-limit multibyte values and exercise pre-existing references. The supplied test run passed for this checkout. No other defects are evidenced.

Use the existing report contract. Include Summary, Standards, Spec, Quality check, Findings, Suggested next steps, and Decision. Include Previous review progress only when prior-review context is supplied. The verdict is **CHANGES_REQUESTED** for any Blocking or Important finding, and **APPROVED** otherwise.
