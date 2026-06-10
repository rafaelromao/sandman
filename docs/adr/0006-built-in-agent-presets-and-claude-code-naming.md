# ADR-0006: Built-in agent presets and Claude Code naming

## Status

accepted

## Context

Sandman previously treated agent providers as repo-local command strings. That made built-in providers hard to share and forced each repository to repeat knowledge about commands, config source paths, and auth constraints.

This change affects multiple modules: config loading, batch execution, container startup, scaffolded config, and user-facing documentation.

## Decision

We will model built-in agent providers as first-class presets keyed by `opencode` and `pi`.

1. Sandman will resolve a preset into provider metadata such as command, config source paths, and auth flags.
2. Repo config selects the default built-in agent via `agent` and can choose a run-time agent via `sandman run --agent`.
3. Sandman no longer supports repo-local custom providers in `.sandman/config.yaml`.
4. User-facing and domain-facing text will use only supported built-in agent names.

## Consequences

### Positive

- Built-in providers are defined once and reused across repositories.
- Runtime code can attach provider-specific metadata without depending on handwritten command strings.
- Repo config is smaller and clearer.
- Domain language stays aligned with supported built-in agents.

### Negative

- The config model is slightly more structured and requires a resolver.
- Existing repositories that relied on handwritten built-in commands must migrate to the supported built-ins.

### Neutral

- The scaffolded config now documents the default built-in agent and installed built-ins.
