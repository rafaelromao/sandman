# ADR-0023: Handoff points to rendered prompt and tracks last skill

## Status

accepted

## Context

ADR-0022 replaced the `sandman-continuation` skill and `.sandman/continuation-context.md` with a `sandman-handoff` skill and a checkpointed `.sandman/handoff.md` file written at four named stages. Each stage records completed work, pending items, blockers, key decisions, and the next step. The resume logic passes the handoff document verbatim as the agent's prompt, and the agent reads `## Next Step` directly.

That design had two gaps.

First, the handoff file had no pointer back to the original rendered prompt. After a successful handoff resume, the agent sees `## Next Step` and acts on it, but the original task description — the rendered prompt that spawned the run — was not directly referenced. If the agent needed to re-read the original intent (to double-check a requirement, understand why a decision was made, or resolve ambiguity in the next step), the only way to find it was to search the file system or the orchestrator's logs. A machine-readable pointer in the handoff file would make the provenance explicit and resolvable.

Second, the handoff file tracked the workflow stage (`plan-approved`, `implementation-committed`, `pr-created`, `pr-review-finished`) but not which sub-skill the previous run was executing when the checkpoint was written. A resumed agent inherits the stage but not the sub-skill name, making it harder to decide whether to reload a skill or skip ahead. The orchestrator needed to recompute the skill context on resume rather than reading it directly from the handoff.

## Decision

Add three new structured fields to `.sandman/handoff.md`, layered on top of the four existing stage fields from ADR-0022:

1. **`## Source Prompt: .sandman/task.md`** — placed immediately after `## Stage:` (the second heading in the file). This is a fixed-path pointer to the rendered prompt file in the same `.sandman/` directory. The resume prompt references this file by path without inlining its content; the agent can read the file if it needs the original task description. The field is machine-readable (the path is always `.sandman/task.md` relative to the worktree root) and human-readable (the heading and path together make provenance obvious).

2. **`## Last Skill`** — a free-form string (not a closed enum) naming the sandman sub-skill the previous run was executing when the checkpoint was written. Examples: `sandman-implement`, `sandman-tdd`, `sandman-self-review`, `sandman-pr-review`, `sandman-handoff`. The field is free-form because the orchestrator may load new sub-skills over time; a closed enum would require an ADR update for every new skill name.

3. **`## Last Skill Status`** — a two-part field with a status (`complete` or `incomplete`) and one line of context. Examples: `complete — all tests pass and review feedback addressed` or `incomplete — interrupted during vertical-slice 3 of 5`. The context line gives the resumed agent enough information to decide whether to reload and re-enter the skill or to mark it done and proceed.

The resume prompt logic (`sandman run --continue` and the orchestrator's retry path) uses the `## Source Prompt` pointer to reference `.sandman/task.md` by path in the resume prompt. It does not inline the rendered prompt content — the agent reads the file on demand. The `## Last Skill` and `## Last Skill Status` fields are surfaced in the resume prompt as-is, so the agent can decide whether to reload the previous sub-skill.

## Consequences

### Positive

- The handoff file becomes a self-contained provenance record. A reader (human or agent) can follow `## Source Prompt` to the exact rendered prompt that started the run, without searching logs or worktree state.
- The `## Last Skill` and `## Last Skill Status` fields let a resumed agent skip directly to the right sub-skill without recomputing context from the stage name alone.
- Not inlining the rendered prompt content keeps the resume prompt compact. The agent loads the rendered prompt on demand via a file read, which is a trivial operation.
- The free-form `## Last Skill` field requires no code change when a new sub-skill name is introduced — only the skill that writes the handoff needs to know its own name.

### Negative

- Three more fields in the handoff file increase its size modestly. Mitigation: each field is a single line plus (for `## Last Skill Status`) one context line.
- Agents that write the handoff must now remember to set all three new fields. Mitigation: the `sandman-handoff` skill's template includes placeholders for all three, so skill authors fill them in rather than constructing the file from scratch.
- Existing handoff files from runs that completed before this ADR are missing the three new fields. The resume logic treats missing fields as absent — the agent sees an empty `## Source Prompt:`, `## Last Skill`, and `## Last Skill Status`. Mitigation: the resume prompt already handles absent fields gracefully (it falls through to "Continue the work.").

### Neutral

- The `## Source Prompt: .sandman/task.md` path is a fixed constant. If the rendered prompt file is ever renamed or moved, this field must be updated in the template. A future ADR could parameterize the path if the naming becomes variable.
- Follow-up references: ADR-0022.
