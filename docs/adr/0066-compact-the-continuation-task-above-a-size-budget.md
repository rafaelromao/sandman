# ADR-0066: Compact the continuation Task above a size budget

## Status

accepted

## Context

Every continuation, retry, context recovery and runtime re-entry sends the Task
(`.sandman/task.md`) back to the agent as its prompt. The Task is the
agent-owned checkpoint, and Sandman preserved all of it except the runtime
sections it composes. Nothing bounded its size. One Run reached 119 KB:

- the agent appended a new checkpoint section on every attempt and prepended to
  `## Next Step` instead of rewriting it (57 KB);
- `## Review Evidence` stored the reviewer's 11 KB report three times (38 KB).

Measured on real sessions of one model, Task text costs about 4.4 bytes per
token, and each session starts with about 22.9k tokens of fixed overhead. That
Run's implementation session began at 48.7k tokens, about 19% of a 250k-token
window, before any work. The agent also rereads the whole Task each time it
updates it.

## Decision

- **Budget.** Implementation launches compact a Task larger than
  `task_compact_bytes` (default 48 KiB, about 11k tokens or 4.5% of a 250k
  window), aiming for three quarters of it. Startup plus one full reread then
  stays under about a quarter of the window. `0` disables the automatic rule;
  other values must be at least 32 KiB because the built-in template alone is
  about 24 KB. A useful guide is about 0.2 bytes per token of context window.
- **Deterministic and lossless.** Sandman never asks an agent to summarise the
  Task. It writes the complete Task to a new file under
  `.sandman/task-history/` in the worktree before writing the compacted one,
  and names that file in a `## Sandman Task History` section. An archive is
  published by hard-linking a complete temp file and never replaces an
  existing file; a second compaction in the same second takes the next free
  name, and a file system without hard links fails the archive. The section ends with a marker, so a later compaction
  replaces only what Sandman wrote and treats text after it as the agent's.
  An archive failure launches the Task unchanged.
- **Structure is recorded, not inferred.** A fresh implementation render
  records its H2 headings in `.sandman/task-anchors`. Those headings (template
  and issue text) are the Task's structure. Sandman's own sections are never
  anchors. The render also records which `## Next Step` is the template's
  own, since an issue can carry one too; only the template's is capped. Tasks
  rendered before this decision fall back to the template's headings and its
  position relative to the issue body. If that template has no heading after
  the issue body other than Sandman's, nothing marks where the issue's own
  sections end, so nothing after the last template heading moves.
- **What moves.** Sections the agent added after the last anchor are history.
  The fewest oldest of them move for the Task, with a pointer naming exactly
  what moved, to fit; a result that would not be smaller is not written. `## Next Step` and what the agent
  attached to it keep their first 4 KiB, a prefix that never resumes past the
  cut. A section that holds a kept heading inside a code block the agent left
  open is kept whole. Anchored sections, every `## Plan`,
  status lines and Sandman's sections are kept byte for byte. A Task that
  cannot shrink is launched unchanged and the run log says why.
- **Where.** Compaction happens once, where the composed Task is written before
  an implementation launch, so every continuation path shares it. Reviews,
  quota probes of the reviewer, health repairs and other prompt-only launches
  are never compacted.
- **On request.** `sandman run --continue --compact` asks the first
  implementation launch of each continued issue the operator selected to
  compact its Task, even under the threshold. The request travels with the
  admitted row and is spent by its first launch, so a start refused before
  then changes no Task, and retries and later re-entries follow the threshold.
  Ready continuations and dependencies added to the batch are left to the
  automatic rule. The CLI itself writes no Task. A restart before the first
  launch drops the request.
- **At the source.** Review evidence stores each long repeated string once,
  referring later copies to the first by JSON pointer. The shared skills ask
  the agent to keep the Task a current-state checkpoint.

This amends the continuation Task ownership rule: Sandman preserves all Task
content except what compaction moves into the archive.

## Consequences

Long Runs keep a bounded prompt, and nothing is lost. The heuristics (later
sections are newer, the beginning of `## Next Step` is current) can keep an
older checkpoint or move a newer one; the archive and the live repository
cover both cases.

The built-in template and a current review report are kept, so a Task with a
large report can stay above the target. A replay with that Task's shape (98 KB
once its review report is stored once) launches at about 46 KB. The real 119 KB
Task would reach about 50 KB, above the threshold, so its later launches would
log that it cannot shrink further until the report is answered. With session reuse, the agent's earlier conversation still
holds the old Task; compaction shrinks only the file and the new prompt.
