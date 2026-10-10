---
name: sandman
description: Routes to Sandman modes for planning, work-item implementation, standalone run composition, test-driven development, code review, change-request review, back-merge, and change-request merge workflows. Use when user mentions sandman or asks for plan, implement, run, tdd, code-review, pr-review, back-merge, or pr-merge modes.
---

# Sandman

## Quick start

Use one mode explicitly:

```text
sandman plan
sandman implement
sandman tdd
sandman code-review
sandman run
sandman pr-review
sandman back-merge
sandman pr-merge
```

## Modes

- `plan` -> `sandman-plan`
- `implement` -> `sandman-implement`
- `tdd` -> `sandman-tdd`
- `code-review` -> `sandman-code-review` (self-review or daemon-review context)
- `run` -> `sandman-run` (standalone implementation, review cycle, and merge composition)
- `pr-review` -> `sandman-pr-review`
- `back-merge` -> `sandman-back-merge`
- `pr-merge` -> `sandman-pr-merge`

## Capabilities

The focused review skills are capabilities, not separate lifecycle modes:

- `sandman-review-request` delivers one managed review trigger without owning lifecycle state.
- `sandman-review-cycle` observes and repairs one standalone review cycle in memory.

The `sandman-run` composition loads the standalone cycle automatically. Managed
runs load the request capability while the runtime owns waiting and decisions.

## Use

Load the matching subskill for the requested mode and follow it end to end.

`run` is the standalone composition entrypoint for direct agent use. A managed
CLI run keeps runtime-owned registration, waiting, evidence, resume, and
terminal decisions; it uses `review-request` for delivery and does not route
through `sandman-run` or `sandman-review-cycle`.

Use `sandman code-review` in self-review context when reviewing the current implementation diff. The review daemon uses the same skill in daemon-review context with its supplied pull-request context and review worktree.

## Continuing Work

`sandman run --continue` preserves the worktree and Task but starts a fresh
OpenCode conversation. Use `sandman run --continue --reuse-session` only when
the prior conversation is intentionally part of the continuation. Runtime-owned
re-entry after an external pull-request wait may reuse the exact prior session
automatically. If that exact session is unavailable, Sandman makes one narrow
OpenCode continuation fallback; unrelated failures are not retried.
