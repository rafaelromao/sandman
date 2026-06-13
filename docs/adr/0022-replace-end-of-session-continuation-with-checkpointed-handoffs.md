# ADR-0022: Replace end-of-session continuation with checkpointed handoffs

## Status

superseded

> Superseded by the checklist-in-`.sandman/task.md` simplification, which folds checkpoint state into the task file and removes the separate handoff file.

## Context

Sandman's `sandman run --continue` flow re-runs the latest AgentRun for an issue with a fresh prompt while reusing the prior run's branch, base branch, agent, and review command. The original implementation paired that flow with a `sandman-continuation` skill that wrote a single end-of-session summary file (`.sandman/continuation-context.md`) before exit. The next `sandman run --continue` invocation prepended that file to a freshly rendered `continue-prompt.md` so the agent could pick up where the previous run left off.

That model had two structural problems.

First, the summary was written exactly once, at the end of the run. If the agent was interrupted mid-workflow (context exhaustion, machine reboot, manual `kill`, AFK timeout) the file simply was not written, and the next run started from a blank prompt and re-did the work that the prior session had already finished. There was no intermediate state the orchestrator could consult to know which stages of the plan/implement/review/merge/continuation flow had already completed.

Second, the file and skill names used the word "continuation", which collided conceptually with the `sandman run --continue` CLI flow. The skill was a piece of meta-infrastructure that *every* Sandman workflow mode needed to call on exit, but its name invited confusion with the user-facing continuation flow — a run that ended with a continuation-context file was not the same thing as a continuation replay, but readers had to read both definitions to be sure.

## Decision

Replace the `sandman-continuation` skill and `.sandman/continuation-context.md` file with a `sandman-handoff` skill and a checkpointed `.sandman/handoff.md` file. The new file is written at four explicit, named stages of the `sandman-implement` workflow:

1. `plan-approved` — after the TDD plan is approved via subagent consensus.
2. `implementation-committed` — after the implementation commits land on the branch.
3. `pr-created` — after the pull request is opened against the base branch.
4. `pr-review-finished` — after the delegated PR review returns approval or a hard blocker.

Each stage records the same five fields: completed work, pending items, blockers, key decisions, and the single most important next step. The `## Stage:` line at the top of the file is informational only — the agent reads its next instruction from the `## Next Step` field directly. The Go orchestrator and `sandman run --continue` no longer parse the stage name.

The Go orchestrator and `sandman run --continue` pass the verbatim handoff document content (or an empty handoff template if the file is missing) as the resume prompt. The agent reads the `## Next Step` field and follows it directly, without any meta-prompt wrapping from the orchestrator. If `handoff.md` is missing, the empty template instructs the agent to "Continue the work."

> **Note:** ADR-0023 partially supersedes this paragraph. The resume prompt now wraps the handoff body in a structured prompt with `## Prior Context`, `## Source Prompt`, `## New Instruction`, and `## Update Handoff Context` sections. The `## Stage:`, `## Last Skill:`, and `## Last Skill Status:` headings from the handoff document are surfaced as-is in the `## New Instruction` block. The rendered prompt file is referenced by path (not inlined). See ADR-0023 for the current design.

The four checkpoints replace the single end-of-session summary. Every workflow mode that ends — `sandman-implement`, the orchestrator's retry path, and `sandman run --continue` itself — writes the same file at the relevant stage, so a single handoff file per worktree captures the full state of the run.

## Consequences

### Positive

- An interrupted run can resume from the last completed checkpoint instead of redoing finished work. The orchestrator and the continue command both consume the same `handoff.md` and apply the same verbatim-handoff resume logic.
- One file per worktree (`.sandman/handoff.md`) replaces the previous one-file-per-run model. There is no need to track separate context and prompt files in the worktree state.
- The naming no longer collides with the `sandman run --continue` CLI flow. `Handoff` is the persisted state; `Continue` is the action that reads it.

### Negative

- Existing `.sandman/continuation-context.md` files from in-flight runs are orphaned. There is no backward-compatibility shim: a run that wrote the old filename but never wrote the new one will fall through to the "missing handoff" branch and start from the empty handoff template. The agents that produced those files were running on a developer worktree, so the cost is low; production runs do not persist worktree state across restarts.
- The handoff file becomes mandatory infrastructure. Every `sandman-implement` step that previously wrote a continuation context now has to remember to call `sandman-handoff`. A missed checkpoint reverts to starting from scratch on resume. Mitigation: the `sandman-implement` skill's checklist and the four explicit stage names make the checkpoints hard to skip silently.
- A single end-of-session summary is replaced by multiple intermediate checkpoints. Readers following a run have to consult the `## Stage:` line to know where in the workflow the snapshot was taken. Mitigation: the stage name is the first heading in the file, so it is the first thing a reader sees.
- The `## Stage:` line is no longer parsed by the Go orchestrator. The agent reads its next instruction from the `## Next Step` field directly. The four stage names remain for human readability but are not a closed vocabulary enforced by Go code.

### Neutral

- The Go rename (#681), the verbatim-handoff resume (#735), and the orchestrator retry path (#676) shipped the file-path and resume-prompt changes incrementally. This ADR records the decisions those changes implemented.
- The `Continuation` glossary term in `CONTEXT.md` retains its original meaning (the AgentRun request mode that skips prompt template rendering) and is updated only to point at the new filename. It is not removed.
