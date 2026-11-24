---
tags: [skill-creator]
max_turns: 8
timeout_seconds: 180
allowed_tools: [Read, Write]
---

Apply the supplied code-review skill in pull-request review context. The fixture below is the authoritative review context and contains all relevant worktree evidence. Review focus is correctness and regression coverage. Local quality rules are unavailable. Historical excerpts may be incomplete. Use only the supplied evidence and write the review to `decision.md`. Do not contact external services.

Acceptance criteria: every token successfully emitted by the public encoder must decode to the same value. Max payload length is 8 bytes inclusive. Existing token formats must remain readable. No prior review exists.
At `tokens/encode.ts:12`, the encoder counts UTF-16 code units and rejects values with a length of 8 or more. At `tokens/decode.ts:18`, the decoder counts bytes and rejects values exceeding 8. These are the only canonical encoder and decoder. Tests exercise lengths 1 and 9 with ASCII only. The patch introduces the encoder check. No code execution is available.

Use the existing report contract. Include Summary, Standards, Spec, Quality check, Findings, Suggested next steps, and Decision. Include Previous review progress only when prior-review context is supplied. The verdict is **CHANGES_REQUESTED** for any Blocking or Important finding, and **APPROVED** otherwise.
