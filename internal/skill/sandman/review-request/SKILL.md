---
name: sandman-review-request
description: Deliver one stateless delegated review request for a managed change request without owning review state or polling.
---

# Review Request

Use this mode only for a Sandman-managed change request.

1. Verify that the current branch is already published and that the change request is open.
2. Post exactly `/sandman review` as a change-request comment.
3. Return the created comment URL and its server timestamp to the runtime.
4. Do not create, edit, or delete review state files. Do not write lifecycle state.
5. Do not poll for feedback, classify responses, or invoke the standalone review composition flow. The runtime owns observation, deadlines, evidence validation, and re-entry.

If delivery or confirmation fails, report the concrete failure and do not post a replacement request.
