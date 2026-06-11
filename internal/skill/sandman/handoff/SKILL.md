---
name: sandman-handoff
description: Writes Sandman handoff context before exit so the next run can resume cleanly. Use when user says sandman handoff, needs handoff state, or wants the handoff step from Sandman's default prompt.
---

# Handoff

## Goal

Write `.sandman/handoff.md` in the current worktree before exiting.

## Template

```markdown
## Stage: <name>
## Completed
(what was implemented, committed, or merged)

## Pending
(what remains unfinished)

## Blockers
(anything preventing completion)

## Key Decisions
(significant design choices made)

## Next Step
(single most important next action)
```

## Workflow

1. Re-read `.sandman/rendered-prompt.md` before continuing.
2. Summarize completed work.
3. List any pending work.
4. Note blockers. Before carrying forward any registered blocker, verify it hasn't been solved already — check recent git log, diff, open PRs, or issue status. Do not blindly copy blockers from previous handoffs.
5. Capture key decisions.
6. State one next step.
7. Write the file to `.sandman/handoff.md`.

## Stop conditions

- Do not invent completion.
- Keep next step singular and specific.
- If uncertain about original intent, consult `.sandman/rendered-prompt.md` before continuing.
- **Do not write a handoff document if the PR is already merged.** A post-merge handoff is misleading — it suggests there is work to resume, but there is none. If the PR is merged, skip writing `.sandman/handoff.md` entirely.
