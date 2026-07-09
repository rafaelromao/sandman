# Architecture

How Sandman is put together, and why it is put together that way.

The architecture docs are written for contributors and curious operators. They describe the seams, the on-disk layout, and the load-bearing design decisions behind Sandman. Agent-facing equivalents live under [`../agents/`](../agents/) and the project's domain vocabulary lives at [`../../CONTEXT.md`](../../CONTEXT.md).

## Pages

- [Disk Layout](disk-layout.md) — canonical on-disk tree, per-artifact ownership table, and migration notes
- [Overview](overview.md) — event-sourced state, DI seams, and the factory model

## Related

- [`../../AGENTS.md`](../../AGENTS.md) — operational rules for agents working in the repo
- [`../../CONTEXT.md`](../../CONTEXT.md) — full domain glossary
- [`../adr/`](../adr/) — accepted and proposed architectural decisions (MADR template)
- [`../agents/portal-layout.md`](../agents/portal-layout.md) — portal runs table CSS invariants
