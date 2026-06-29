---
name: sandman-index
description: Use when you need to find symbols, trace dependencies, assess blast radius, or explore codebase structure. Also use when the user mentions codeindex, symbol lookup, blast radius, dependency analysis, or asks how code is connected. Covers all codeindex commands and query refinement when results are empty.
---

# sandman-index

Symbol index for fast, scoped code discovery. Keeps large codebases navigable without flooding context with line-by-line file reads.

## Hard rule

**Run `codeindex` before any broad exploration, grep, or file open for code discovery.** Not a suggestion — a rule. Violation wastes tokens and risks wrong file selection.

The rule applies to the primary agent and every sub-agent spawned via the `task` tool.

## Core commands

| Command | Purpose |
|---------|---------|
| `codeindex lookup <symbol>` | Find where a function, type, method, or symbol is defined (file + line). |
| `codeindex search "<concept>"` | Hybrid semantic + keyword + graph search for cross-cutting concepts. |
| `codeindex dependencies <file>` | Show imports and imported-by relationships for a file. |
| `codeindex impact <file>` | Blast-radius report: how many files break if this changes. |
| `codeindex high-blast --threshold N` | List the riskiest files with blast score ≥ N. |

## Command selection

Use the smallest command that answers the question:

- **Definition location** → `codeindex lookup <symbol>`
- **Cross-cutting concept** → `codeindex search "concept"`
- **What depends on a file** → `codeindex dependencies <file>`
- **Impact before editing** → `codeindex impact <file>`
- **Risky change areas** → `codeindex high-blast --threshold N`

## Scope note

`codeindex` indexes **symbol definitions** (functions, types, methods) — not file names or content. A search for "http handler" finds zero results because the symbols are `ServeHTTP`, `HandleFunc`, `Handler`.

## When results are empty

If your first query returns nothing:

1. **Refine the query** — try shorter core concepts or known symbol patterns
2. **Try specific names** — e.g., `HandleFunc` instead of "http handler"
3. **Minimum 2-3 refined queries before falling back to glob/grep**

Only after refining should you fall back to `glob` or `grep`.

## Read discipline

After a `codeindex` query:
- Read only the files the query identifies as likely relevant
- Avoid opening adjacent files "just in case"
- Avoid repo-wide grep unless codeindex cannot answer

## Direct-read exceptions

Skip codeindex and read directly when the task is purely about:

- Configuration files, scripts, logs, or generated artifacts
- Exact implementation details after codeindex has already narrowed the search

## Sub-agent rule

When spawning a sub-agent via `task` tool for code discovery, include this verbatim:

> **Before any broad search or file read, run `codeindex lookup <symbol>` or `codeindex search "concept"` to narrow targets. This is a mandatory repo rule — violation wastes context.**

Do not skip even for seemingly simple lookups.