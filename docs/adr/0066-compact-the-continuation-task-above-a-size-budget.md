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
- **Structure is marked, not inferred.** Telling template, issue and agent
  text apart from headings alone is ambiguous: issue bodies, agent checkpoints
  and edited sections can all reuse template titles. A fresh implementation
  render therefore writes comment markers into the template before
  substituting anything into it, carrying a per-render nonce recorded in
  `.sandman/task-anchors`: before the template's `## Next Step`, after the
  heading of the section that follows it (or with a distinct marker when it is
  last), and after the template and everything substituted into it. Only
  sections Sandman removes and rewrites at launches may follow the end marker.
  Issue text cannot forge the nonce. A Next Step with a placeholder anywhere
  from its heading to the next one is not marked: what it expands to is
  unknown, and it can be the issue body. When Sandman rewrites a section at a
  launch, that section ends at a marker, so rewriting the one above Next Step
  neither removes its marker nor puts lines inside it. The editable step
  starts at the marked `## Next Step` heading.
- **Unmarked Tasks.** A Task without markers, such as one rendered before this
  decision, is never compacted automatically; the run log says that
  `--continue --compact` can. On request it is located only for the built-in
  layout: the template puts the issue body alone under `## Issue Context`
  before `## Runtime Context` and ends with the built-in `## Completion
  Requirements`, and the Task holds the current issue body and those
  requirements verbatim. Only its `## Next Step` section is capped, and only
  when the Task holds exactly one before the requirements and the template's
  holds no placeholder: nothing proves that the sections after it were added
  by the agent rather than the template. This cannot prove that an older
  template had no section after the requirements, which would move into the
  archive; the operator accepts that by asking. Anything else is left
  unchanged.
- **What moves.** Only what follows the structure is history. The fewest
  oldest sections move for the Task, with a pointer naming exactly what moved,
  to fit; a result that would not be smaller is not written. The located Next
  Step keeps its first 4 KiB, a prefix that never resumes past the cut. It is
  cut at a line break outside code blocks; when a long line or block would
  leave less than half the limit used that way, it is cut inside and an open
  block closed, unless the block's delimiter and its closer do not both fit.
  The rest of the structure, every `## Plan`, execution checklists, status
  lines and Sandman's sections are kept byte for byte. So is a section whose
  code block swallowed one of those, unless the block is clearly a
  language-tagged example that starts with that heading while a real one
  exists. Any section with a line a continuation takes for a Runtime Context
  heading, even inside a code block, is also kept, so compaction never moves
  review settings. How a continuation picks the section it writes them to is
  unchanged. If an agent removes the Next Step markers the step is kept whole;
  if it removes the closing marker and the layout cannot be verified, the Task
  is left unchanged.
- **Code blocks.** They are read across the whole Task. One that crosses the
  end of the structure stays whole, with the pointer after it, and a Next Step
  whose bounds fall inside one is not capped.
- **Where.** Compaction happens once, where the composed Task is written before
  an implementation launch, so every continuation path shares it. Reviews,
  quota probes of the reviewer, health repairs and other prompt-only launches
  are never compacted.
- **On request.** `sandman run --continue --compact` asks the first
  implementation launch of each continued issue the operator selected to
  compact its Task, even under the threshold, aiming for the same size.
  Selecting a Specification selects the issues it expands to. The request
  travels with the admitted row and is spent by its first launch, so a start
  refused before then changes no Task, and retries and later re-entries follow
  the threshold. Ready continuations and dependencies added to the batch are
  left to the automatic rule. The CLI itself writes no Task. A restart before
  the first launch drops the request. Command and environment validation and
  opening the required run log all happen before the continuation Task or
  archive is written. If the sandbox definitively refuses before spawning the
  launcher, Sandman restores the previous Task, whether or not anything moved,
  and removes any unused archive. It retains the archive if restoration fails
  or the launcher started. A failed atomic Task write also removes the unused
  archive; the run log reports success only once the launch is no longer
  subject to this rollback.
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
large report can stay above the target. A replay with that Task's shape,
marked as a fresh render (98 KB once its review report is stored once),
launches at about 46 KB. The real 119 KB Task predates the markers: it is
compacted only on request, to 93 KB, of which 38 KB is review evidence the old
code stored three times. With session reuse, the agent's earlier conversation
still holds the old Task; compaction shrinks only the file and the new prompt.
