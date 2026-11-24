---
tags: [skill-creator]
max_turns: 8
timeout_seconds: 180
allowed_tools: [Read, Write]
---

Apply the supplied code-review skill in pull-request review context. The fixture below is the authoritative review context and contains all relevant worktree evidence. Review focus is correctness and regression coverage. Local quality rules are unavailable. Historical excerpts may be incomplete. Use only the supplied evidence and write the review to `decision.md`. Do not contact external services.

Acceptance criteria: preview and final totals must use the same currency rounding policy. The supplied review of revision A found the preview rounded half-up and the finalizer rounded half-even. The prior root-cause change request identified inconsistent rounding and asked the caller to pass that policy consistently. The implementer passed the policy in the interactive path.
The current revision B uses the agreed policy in both interactive paths, but `payments/retry.ts:32` directly constructs a finalizer with an implicit default. Retried final totals can still differ from the preview on midpoint values. Current tests cover interactive flows only.

Use the existing report contract. Include Summary, Standards, Spec, Quality check, Findings, Suggested next steps, and Decision. Include Previous review progress only when prior-review context is supplied. The verdict is **CHANGES_REQUESTED** for any Blocking or Important finding, and **APPROVED** otherwise.
