---
name: sandman-review-request
description: Deliver one stateless delegated review request for a change request without owning review state, polling, or lifecycle decisions.
---

# Review Request

Use this capability when a caller needs one review trigger delivered and
confirmed. The caller supplies the repository, pull-request number, current
head, configured review command, and any request-scoped context.

1. Verify that the current pull request is open, the branch is published, and
   its live head matches the supplied head.
2. Run the shared `review-trigger-guard-v1.sh` read-only trigger-delivery guard
   immediately before posting. Consume its `review-trigger/v1` decision;
   do not copy or reinterpret the guard in this capability.
   A refusal caused by an unanswered trigger, malformed evidence, ambiguous
   ordering, stale head, or failed lookup is a delivery refusal.
3. Post exactly one `{{REVIEW_COMMAND}}` trigger. Never silently retry by
   posting a replacement trigger.
4. Re-read the pull request and confirm the returned comment against the live
   head, command prefix, comment identity, and valid server timestamp.
5. Return one `review-request/v1` structured confirmed-request envelope containing the repository,
   pull-request number, head, comment URL or identity, command prefix, server
   timestamp, and confirmation time to the caller.
6. Do not poll for feedback, classify approval or requested changes, or invoke
   the standalone review cycle. Do not create, edit, or delete review state
   files. Do not write lifecycle, await, dependency, terminal, or event state.

If validation, guarded delivery, or confirmation fails, return the concrete
delivery refusal and do not post a replacement request. `REVIEW_TIMEOUT` is
run context supplied by the caller; it is not shared skill state and this
capability does not start its deadline or observation loop.
