# Codeindex Discovery Strategy

Session-level policy for fast, scoped code discovery. Prevents context pollution from broad searches and wrong-file reads.

This file is loaded into every OpenCode session via the `instructions` field in `opencode.json`. It runs once per session — the `sandman-index` sub-skill (under `sandman`) is the complementary reference layer (loaded on demand).

## Session initialization

On every new session, immediately run:

1. `codeindex` — warm the symbol index
2. `codeindex high-blast --threshold 10` — identify high-blast-radius files

Include the high-blast output in session context. High-blast files are where wrong edits cost the most (rework sessions that burn 5M+ tokens).

## Mandatory precondition

Before every `grep`, `glob`, or `rg` for code discovery:

```
codeindex lookup <symbol> or codeindex search "<concept>"
→ matches?  read matched file:line directly, skip grep/glob
→ empty?    refine query 2-3 times, then fall back to grep/glob
```

This is not a suggestion. A grep/glob that should have been preceded by codeindex is a wasted call — it floods context with irrelevant matches and triggers unnecessary exploratory reads.

## Direct-read exceptions

Skip the precondition only when the task is purely about:

- Configuration files, scripts, logs, or generated artifacts
- Exact implementation details after codeindex has already narrowed the search

## Subagent rule

Pass this precondition verbatim to every subagent spawned via `task`:

> Before any broad search or file read, run `codeindex lookup <symbol>` or `codeindex search "concept"` to narrow targets. Skip only for config files, logs, or generated artifacts.

## Relationship with `sandman-index` sub-skill

| Layer | File | Scope | When |
|-------|------|-------|------|
| **Policy** (this file) | `docs/agents/codeindex-strategy.md` | Session init + precondition | Every session, once |
| **Reference** (`sandman-index`) | `/.agents/skills/sandman/index/SKILL.md` | Command ref, refinement, discipline | On demand, loaded by agent |

The policy says **when** and **whether**. The skill says **how**. They complement, not duplicate.
