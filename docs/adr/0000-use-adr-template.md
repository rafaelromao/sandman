# ADR-0000: Use MADR-style ADR template

## Status

accepted

## Context

Sandman is a CLI tool for orchestrating AFK coding agents. As the project grows, we need a lightweight but rigorous way to record architectural decisions that affect agent behavior, sandbox isolation strategies, and the domain model. Without explicit records, decisions risk being re-litigated or lost as the codebase evolves.

The project already has a strong domain vocabulary convention (`CONTEXT.md`) and agent-facing documentation (`CLAUDE.md`). ADRs should complement these, not duplicate them — they capture *why* a decision was made, while `CONTEXT.md` captures *what* the terms mean.

## Decision

We will use the MADR (Markdown Architectural Decision Records) format for all ADRs in this project.

Specifically, each ADR will contain:

- **Title and number** — sequential, zero-padded to 4 digits
- **Status** — one of `proposed`, `accepted`, `rejected`, `superseded`
- **Context** — the forces at play, including technical, social, and project-specific constraints
- **Decision** — the response to those forces, stated in full sentences
- **Consequences** — what becomes easier or harder as a result, including positive, negative, and neutral effects

ADRs live in `docs/adr/` and are immutable once accepted or rejected. Superseded ADRs remain in place with a note pointing to their replacement.

## Consequences

### Positive

- Decisions are explicit, searchable, and version-controlled.
- New contributors can understand the evolution of the architecture without reading the entire git history.
- Agent prompts can reference ADRs when explaining architectural constraints.

### Negative

- Writing an ADR adds overhead to every significant decision.
- There's a risk of ADR bloat if trivial decisions are documented.

### Neutral

- ADRs do not replace code comments or inline documentation — they complement them.
- The MADR format is widely used in the Go community, making it familiar to potential contributors.
