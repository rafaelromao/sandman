# ADR-0059: Blind review is the default pull-request review

## Status

accepted

## Context

Sandman's default review command was `/sandman review`, which only works when a
separate review daemon is running and authenticated as a reviewer. A repository
without that daemon either stalls at review or must configure another external
reviewer. The default must complete the implement-review loop without an
external process, while delegated review remains available for operators who
want an independent reviewer.

The implementor already has the published pull request and its worktree. It can
spawn a subagent, but that subagent must not inherit the implementor's plan,
conversation, or conclusions. The review prompt must remain the same one a
daemon reviewer receives, or the two paths would apply different standards.

The evidence is implementor-published and normally uses the operator's own
GitHub account. The tracker does not let an author formally approve their own
change, and Sandman does not require a separate review account. The trust model
therefore has to be explicit rather than implied by the review command.

ADR-0058 keeps repository and run values out of the shared skill tree. An empty
review command is a real repository value, so it must round-trip through
configuration, the Task, events, and the standalone skills without being
mistaken for a missing value.

## Decision

An absent, empty, or whitespace-only `review_command` selects **blind review**. A
non-empty command keeps the existing delegated-review flow unchanged. Setting
the command to `""` is an explicit selection, not a fallback that re-enables the
daemon.

A blind review round works as follows.

- **One round.** The implementor resolves the pull request and current head,
  requires a clean checkout at that head, and spawns one fresh general-purpose
  subagent in the same worktree. The subagent receives no implementation
   summary, hints, conversation history, plan, or desired verdict. Native harness
   delegation delivers the rendered prompt directly or a minimal file-pointer
   handoff that tells the reviewer to read and follow that prompt in full.
   Prompt parity applies to the authoritative review input, not the transport
   wrapper or the first user message. It uses the harness's default
  model, not `review_agent`, `review_model`, or `review_variant`.
- **Exact prompt parity.** One pure derivation, shared with the review daemon,
  produces the prompt inputs: acceptance criteria from the linked work item,
  prior-review existence and context from comments, formal reviews, inline
  comments, and the review snapshot header. The standalone skill ships the same
  template. With no trigger there is no review focus. The subagent reads
  `.sandman/reviews/quality-rules.md` exactly as a daemon reviewer does and
  reports it unavailable when missing.
- **Read-only review.** The reviewer may read the pull request and worktree and
  writes its report to a temporary `RUN_DIR` outside the worktree. The
  implementor re-reads HEAD, the full worktree status, and the live pull-request
  head after the round. A changed worktree or head is a
  `REVIEW_READ_ONLY_VIOLATION`: the report is preserved for diagnosis and never
  published or accepted.
- **Publication.** Every round is published, including requested changes, as a
  top-level pull-request comment. The body is byte-identical in shape to a
  daemon publication: `/sandman` redaction, the closed-set verdict trailer, and
  the publication identity last. A blind publication uses the fixed
  `blind-review` trigger key and the reviewed head. The next round reads prior
  publications from the pull request the same way a daemon reviewer reads prior
  reviews.
- **Evidence.** The newest current-head blind publication decides the outcome. A
  trusted `approved` verdict is current-head approval and gives ready-to-merge
  only when CI is green and the head is mergeable. A `changes_requested` verdict
  is actionable feedback and resumes the same RunID. A publication for another
  head, an unknown verdict, or no publication yields no evidence. A configured
  command ignores blind publications.
- **Fail closed.** A harness that cannot spawn a fresh, context-free subagent
  returns `REVIEW_SUBAGENT_UNAVAILABLE` and the next action to configure
  `review_command`. The implementor never reviews its own change in its own
   context and never silently posts a delegated-review trigger instead.
   Native delegation is selected by exposed capabilities, independent of the
   agent product or CLI version. It does not depend on launching another agent
   server or discovering/exporting parent sessions. Missing native isolation or
   access to the authoritative input remains a refusal, not self-review.
- **Limits.** `review_timeout`, the trigger prefix, the request envelope, and the
  polling plan apply only to delegated review. A blind round is synchronous and
  has no timeout minimum or polling window. The standalone cycle keeps its
  ten-passes-per-head budget and its CI allowance.
- **Lifecycle.** With no current-head blind approval and no pending CI, green CI
  and an open pull request resume the same RunID with reason
  `BLIND_REVIEW_REQUIRED` (gate `blind-review-required`). It is recoverable,
  implementor-owned work, never `REVIEW_REQUEST_REQUIRED`, an expired request, or
  the old idle hold. A relaunch that leaves the head and blind evidence unchanged
  records its signature in `stalled_resumes` and holds; an exhausted relaunch
  allowance holds under `hold:<RunID>`. Explicit abort revokes the resume, and
  restart rehydrates the evidence from events.
- **Daemon unchanged.** The review daemon still answers comments that begin with
  `/sandman`, whether or not a review command is configured. A blind default
  posts no trigger; delegated review adds the configured trigger on top of the
  same daemon.

### Trust model

The publication author is deliberately not filtered, consistent with informal
approval. The identity binds the report to one reviewed head and one fixed
trigger key, and the verdict trailer binds the decision to that publication. It
is **not authentication**: any account able to comment on the pull request can
post a report carrying a valid trailer, and Sandman does not verify that the
commenter is a distinct reviewer. The accepted trust boundary is the
repository's GitHub permissions plus the operator's own-account workflow, with
required checks and a mergeable head still gating merge. This tradeoff is
accepted so the default AFK loop works without a second reviewer identity;
operators who need an independent reviewer configure a delegated command.

## Consequences

### Positive

- A repository completes implementation, review, and merge with no review
  daemon, review credentials, or external reviewer configured.
- Blind and daemon reviews apply the same acceptance criteria, prior-review
  context, quality rules, redaction, and verdict trailer.
- A fresh subagent cannot rationalize the implementor's own reasoning, and a
  read-only verification catches a reviewer that mutates the worktree.
- Review evidence is a normal current-head publication, so restart, abort, and
  stall handling reuse the existing lifecycle machinery.
- Delegated review remains available and unchanged for repositories that want
  an external reviewer.

### Negative

- The implementor can publish evidence from the same account, and Sandman cannot
  authenticate a distinct review author.
- Blind review consumes implementor execution capacity and a harness subagent
  rather than an independent review run.
- A harness without context-free subagents cannot use the default path and must
  configure a delegated command.
- Operators must treat pull-request comment permission as review permission.

### Neutral

- `review_timeout` and polling configuration remain meaningful only for
  delegated review.
- The review daemon still runs and answers explicit `/sandman` triggers, so a
  mixed repository can use both paths deliberately.
