# Blind cycle

This is the empty-command branch of `sandman-review-cycle`. All cycle state is
in memory. A fresh invocation reconstructs its starting point from the live
pull request, never from managed lifecycle files.

## Workflow

1. Resolve the repository, pull-request number, base branch and implementation
   branch as in the parent skill. Confirm the checkout is the published branch.
   Keep it checked out throughout. Preserve the existing Step 2 CI allowance:
   sixty minutes and three fix-and-push attempts for the current head.
2. Read the live head, required checks and mergeability. Resolve conflicts
   through `sandman-back-merge`; diagnose and repair failing required checks.
   Follow the parent's Step 2 CI loop until checks are green. A new head starts
   a new CI allowance. Record the parent's structured CI or conflict diagnostic
   when autonomous repair cannot proceed.
3. Record the current head and pass count. A changed head invalidates all prior
   approval and resets the ten-pass budget. A new invocation also starts fresh.
   An unchanged head never extends a previously exhausted pass allowance within
   this invocation.
4. Load `sandman-blind-review` and run exactly one round with the verified
   repository, pull-request number and head. Publish every round, not just an
   approving round. Increment the pass count.
5. Require a confirmed `blind-review/v1` envelope for that exact repository,
   pull request and unchanged head. Re-read the published report and live
   formal reviews. Formal requested changes remain blocking.
   - **Approved**, with no Blocking or Important findings or outstanding
     formal requested changes: mark PR-Review complete and return current-head
     approval to the composing workflow. A formal approval is not required.
   - **Changes requested:** read the published findings, verify their scope,
     make the minimal repair, test, commit and push. Return to Step 2, not merge.
   - **Ambiguous or unknown:** publish a clarification note on the pull request
     explaining the ambiguity; run a fresh blind round so it receives that note
     in the same authoritative history used by a daemon review.
   - **Unavailable, refused or uncertain:** preserve the exact diagnostic and
     next executable action; fail closed. An uncertain publication requires
     live read-back before another post, never a replacement post.
6. Repeat through ten passes per head. With unresolved feedback at the
   unchanged head after ten passes, record `REVIEW_PASSES_EXHAUSTED`, the head,
   report identity, remaining blockers and next executable action. Leave
   PR-Review unchecked and the pull request open.

The review timeout, trigger prefix, request envelope, poll plan and 240-second
minimum belong to delegated review only. They never apply to a synchronous
blind round. This branch never posts a review-command trigger or writes managed
registrations, waits, events or terminal outcomes.
