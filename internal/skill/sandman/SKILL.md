---
name: sandman
description: Routes to Sandman modes for planning, work-item implementation, test-driven development, code review, change-request review, back-merge, and change-request merge workflows. Use when user mentions sandman or asks for plan, implement, tdd, code-review, pr-review, back-merge, or pr-merge modes.
---

# Sandman

## Quick start

Use one mode explicitly:

```text
sandman plan
sandman implement
sandman tdd
sandman code-review
sandman pr-review
sandman back-merge
sandman pr-merge
```

## Modes

- `plan` -> `sandman-plan`
- `implement` -> `sandman-implement`
- `tdd` -> `sandman-tdd`
- `code-review` -> `sandman-code-review` (self-review or daemon-review context)
- `pr-review` -> `sandman-pr-review`
- `back-merge` -> `sandman-back-merge`
- `pr-merge` -> `sandman-pr-merge`

## Use

Load the matching subskill for the requested mode and follow it end to end.

Use `sandman code-review` in self-review context when reviewing the current implementation diff. The review daemon uses the same skill in daemon-review context with its supplied pull-request context and review worktree.
