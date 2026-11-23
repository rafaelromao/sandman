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

Use the current harness's native subagent capability (for example, Agent, Task,
or subagent), with conversation inheritance disabled. Select by the capabilities
actually exposed in this session, not by the agent product or CLI version.
Native delegation owns the child identity, permissions and model selection.
Keep the default model; leave model overrides unset.

If the harness cannot do that, return `REVIEW_SUBAGENT_UNAVAILABLE` with the next
action `configure review_command`. Fail closed; never substitute self-review
or silently post a delegated-review trigger.

```json
{"protocol":"blind-review/v1","state":"refused","reason":"REVIEW_SUBAGENT_UNAVAILABLE","next_action":"configure review_command"}
```

## Workflow

1. Resolve the installed helper directory alongside this skill. The helpers
   require POSIX `sh`, `jq`, `gh`, and `sha256sum` or `shasum`, and read the
   sibling `github-v1.sh` they share. The generated
   Debian bookworm container supports GitHub CLI `2.23.0` and jq `1.6`; these
   helpers do not require newer versions.
2. Record the checkout's HEAD and `git status --porcelain=v1 --untracked-files=all`.
   Require a clean checkout and HEAD equal to the supplied head. Create a
   fresh temporary directory outside the worktree with `mktemp -d` for this
   round's prompt and report. Never use the shared temporary root itself as
   the report directory.
3. Run `review-prompt-v1.sh` with `--repository`, `--pull-request`, `--head-sha`
   and `--run-dir` (the temporary directory). Save its exact stdout as the prompt.
   A failed lookup or render ends this round without publication.
   Keep that file as the authoritative input throughout delivery.
4. Spawn one fresh general-purpose subagent through that native capability,
   in the current worktree and without conversation inheritance. Use only
   documented arguments exposed by the harness. Choose the delivery form it
   supports:

   - **Direct input:** When a programmable call or file-input facility can
     deliver the rendered file unchanged, use it as the review instructions.
   - **File pointer:** When the tool accepts only an assistant-authored string,
     pass the minimal handoff below, substituting absolute paths. The child
     reads the authoritative file in full before reviewing; it follows that
     file, not a rewritten or summarized prompt. Ensure the child can access
     the worktree and external round directory under its normal permissions.

     ```text
     You are the fresh blind reviewer, not the review orchestrator.
     Worktree: <absolute worktree path>
     Review prompt file: <absolute round prompt path>
     Read that file in full and follow it as your authoritative review instructions.
     Write the decision atomically to: <absolute round directory>/decision.md
     Review read-only; the parent owns publication and snapshot validation.
     Do not spawn another reviewer or run the blind-review orchestration workflow.
     ```

   These are equivalent deliveries of the same review input. The first user
   message need not itself contain the review prompt. Pass no implementation
   summary, hints, conversation history, plan, or desired verdict. The rendered
   prompt defines scope, review rules and the report; the handoff only locates it
   and separates reviewer and parent responsibilities.

   Do not start an agent CLI/server, discover or export parent sessions, or
   invoke the bundled legacy transport helpers to replace native delegation.
   If fresh isolated delegation or required file access is unavailable, return
   the refusal above with the concrete diagnostic. After a child is admitted,
   wait for that child's completion using the harness's documented completion
   mechanism; an asynchronous start is not a finished round. Preserve its
   identity on failure or uncertain completion rather than launching a duplicate.
   Require a non-empty `decision.md` produced by the reviewer before proceeding.
5. Re-read HEAD, the full worktree status and the pull request's live head.
   Require all three to match the pre-review snapshot. If the reviewer changed
   the worktree, return `REVIEW_READ_ONLY_VIOLATION`, preserving the changes and
   report for diagnosis. Never publish or accept that review.
6. Run `publish-v1.sh` with `--repository`, `--pull-request`, `--head-sha`
   and `--decision-file`. It publishes once and confirms the posted comment:
   the live body must equal what was posted, or equal it followed by exactly
   one recognised host footer block. Return its `blind-review/v1` envelope
   without reinterpreting its verdict. On uncertain publication, preserve the
   report and diagnostic; re-observe the live pull request before any later
   mutation. An uncertain publication never posts a replacement.
7. Remove only this round's temporary files after confirmed publication, unless
   the caller requested their retention for diagnostics or test verification.

### Hosts that alter or restrict GitHub access

A managed sandbox (a hosted agent session, for example) can restrict GitHub in
two ways. The round still completes in both, without weakening what it
requires.

- **GraphQL unavailable.** Each helper tries its GraphQL-backed call first.
  Only when that call fails does it probe a minimal GraphQL query. When GraphQL
  itself is unavailable, the helper uses the REST equivalents of the same
  reads, takes the closing reference from the pull request body alone, posts
  the publication once and confirms the posted comment by its identifier.
  Selection is by that probe, never from the host or product name. A failure
  while GraphQL works is still a refusal, the choice is fixed before any
  post, and a failed post is never retried or replaced through the other
  route. The envelope and the prompt are the same in both modes; only a notice
  on standard error differs.
- **Footer appended to comments.** A host may append a separator line and a
  known footer line to every comment it publishes. That one recognised host
  footer block is not part of the report: the confirmation, the approving
  verdict and the head binding are judged without it. Nothing may sit between
  the verdict trailer, the identity line and the block, and a second block,
  added text, an altered identity or verdict, an unknown footer or another head
  keep the publication unconfirmed. The identity still binds the head only; it
  is not authentication.

Every round is published, including requested changes. The next round reads
the prior reports from the pull request just like a daemon reviewer.
Approval applies only to the reviewed head. A new head requires a new round.
Formal requested changes, failing checks and merge conflicts remain blocking.

There is no polling window or review-timeout minimum for this synchronous
round. The caller's ten-pass-per-head cycle budget and harness limits apply.
This capability never writes managed lifecycle or review-registration state.
