---
tags: [skill-creator]
max_turns: 8
timeout_seconds: 180
allowed_tools: [Read, Write]
---

Apply the supplied code-review skill in pull-request review context. The fixture below is the authoritative review context and contains all relevant worktree evidence. Review focus is correctness and regression coverage. Local quality rules are unavailable. Historical excerpts may be incomplete. Use only the supplied evidence and write the review to `decision.md`. Do not contact external services.

Acceptance criteria: a pending report load must never replace the view after superseding navigation, including leaving a view and returning. An earlier review identified stale loads on navigation, then requested architecture-level remediation at the shared result application boundary. Revision A adopted a centralized URL-equality guard. The current revision still applies results if `currentURL === startedURL` in `report/navigation.ts:34`.
Given A → B → A while the first A load remains pending, the shared guard accepts the obsolete A result. Immediate completion and A → B are already tested. The router exposes an existing navigation generation that changes on each navigation.

Use the existing report contract. Include Summary, Standards, Spec, Quality check, Findings, Suggested next steps, and Decision. Include Previous review progress only when prior-review context is supplied. The verdict is **CHANGES_REQUESTED** for any Blocking or Important finding, and **APPROVED** otherwise.
