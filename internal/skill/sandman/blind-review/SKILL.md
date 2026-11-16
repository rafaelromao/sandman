---
name: sandman-blind-review
description: Run one blind pull-request review with a fresh subagent, publish its decision, and return current-head evidence when no review command is configured.
---

# Blind Review

Run one round only. The caller owns CI, remediation, repetition and merge.
An empty review command selects this capability; a configured command selects
`sandman-review-request` instead.

## Preconditions

- The pull request is open and published; required checks are green.
- The caller supplies the repository, pull-request number and current head.
- The checkout is on the implementation branch, clean and at that exact head.
- The harness can spawn a fresh subagent without inheriting conversation context.

If the harness cannot do that, return `REVIEW_SUBAGENT_UNAVAILABLE` with the next
action `configure review_command`. Fail closed; never substitute self-review
or silently post a delegated-review trigger.

```json
{"protocol":"blind-review/v1","state":"refused","reason":"REVIEW_SUBAGENT_UNAVAILABLE","next_action":"configure review_command"}
```

## Workflow

1. Resolve the installed helper directory alongside this skill. The helpers
   require POSIX `sh`, `jq`, `gh`, and `sha256sum` or `shasum`.
2. Record the checkout's HEAD and `git status --porcelain=v1 --untracked-files=all`.
   Require a clean checkout and HEAD equal to the supplied head. Create a
   temporary directory outside the worktree for this round's prompt and report.
3. Run `review-prompt-v1.sh` with `--repository`, `--pull-request`, `--head-sha`
   and `--run-dir` (the temporary directory). Save its exact stdout as the prompt.
   A failed lookup or render ends this round without publication.
   Keep that file as the authoritative input throughout delivery.
4. Spawn one fresh general-purpose subagent in the current worktree, using the
   harness's default model. Its entire user message is the rendered prompt,
   byte for byte, including every blank line, indentation and final newline.
   Prefer a documented file-input or programmable tool-call facility: read the
   file into a string and pass that string directly to the native spawn call,
   without passing its contents through an assistant-generated response.
   Use only facilities and argument names exposed by the current harness.

   For OpenCode's string-only native spawning, use the file-backed transport
   matching the caller's selected protocol: `spawn-opencode-v1.sh` for
   `opencode`, `spawn-opencode-v2.sh` for `opencode-v2`. V1 additionally requires
   `curl`. Invoke it through one foreground native shell tool call in this
   worktree, with `--prompt-file` followed by the **literal absolute path** to
   this round's unique temporary prompt file. Put the helper filename and
   literal path directly in that command; do not hide them behind variables,
   another script or a background launch.

   The helper identifies exactly one live owning call in an exact-worktree
   root session, creates a fresh native child linked to that parent, inherits
   its model and variant, sends file bytes mechanically and verifies readback.
   V1 owns and stops only its temporary loopback server; V2 uses the CLI's
   authenticated service context and never stops that service. Permission
   requests, ambiguous ownership, unsupported agent settings or API errors
   fail closed. Preserve the helper diagnostic directory on failure.
   Never substitute manual prompt copying, a file attachment, a fork or a
   separate unlinked agent run.
   Pass no implementation summary, hints, conversation history,
   plan, or extra instructions. The prompt contains the review scope and the
   search restriction. Wait for its report in the temporary `decision.md`.
5. Re-read HEAD, the full worktree status and the pull request's live head.
   Require all three to match the pre-review snapshot. If the reviewer changed
   the worktree, return `REVIEW_READ_ONLY_VIOLATION`, preserving the changes and
   report for diagnosis. Never publish or accept that review.
6. Run `publish-v1.sh` with `--repository`, `--pull-request`, `--head-sha`
   and `--decision-file`. It publishes once and confirms the exact comment.
   Return its `blind-review/v1` envelope without reinterpreting its verdict.
   On uncertain publication, preserve the report and diagnostic; re-observe
   the live pull request before any later mutation.
7. Remove only this round's temporary files after confirmed publication.

Every round is published, including requested changes. The next round reads
the prior reports from the pull request just like a daemon reviewer.
Approval applies only to the reviewed head. A new head requires a new round.
Formal requested changes, failing checks and merge conflicts remain blocking.

There is no polling window or review-timeout minimum for this synchronous
round. The caller's ten-pass-per-head cycle budget and harness limits apply.
This capability never writes managed lifecycle or review-registration state.
