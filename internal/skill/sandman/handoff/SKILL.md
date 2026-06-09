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

1. Summarize completed work.
2. List any pending work.
3. Note blockers.
4. Capture key decisions.
5. State one next step.
6. Write the file to `.sandman/handoff.md`.

## Stop conditions

- Do not invent completion.
- Keep next step singular and specific.
- If uncertain about original intent, consult `.sandman/rendered-prompt.md` before continuing.
- **Do not write a handoff document if the PR is already merged.** A post-merge handoff is misleading — it suggests there is work to resume, but there is none. If the PR is merged, skip writing `.sandman/handoff.md` entirely.
