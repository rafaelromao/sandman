# ADR-0006: Built-in agent presets and Claude Code naming

## Status

proposed

## Context

Sandman previously treated agent providers as repo-local command strings. That made built-in providers hard to share and forced each repository to repeat knowledge about commands, config mount paths, and auth constraints.

The project also used the outdated phrase "Cloud Code" in some domain and user-facing text. The product name should be standardized to "Claude Code" so the vocabulary stays consistent across docs and code.

This change affects multiple modules: config loading, batch execution, container startup, scaffolded config, and user-facing documentation.

## Decision

We will model built-in agent providers as first-class presets keyed by `opencode`, `claude-code`, `codex`, and `pi`.

1. Sandman will resolve a preset into provider metadata such as command, config mount paths, and auth flags.
2. Repo config may select a built-in provider with `preset` and may still define a custom provider with `command`.
3. Custom provider configuration remains supported and continues to override built-in defaults when explicitly configured.
4. User-facing and domain-facing text will use "Claude Code" consistently.

## Consequences

### Positive

- Built-in providers are defined once and reused across repositories.
- Runtime code can attach provider-specific metadata without depending on handwritten command strings.
- Repo config becomes clearer about the difference between selecting a preset and defining a custom provider.
- Domain language stays aligned with the product name.

### Negative

- The config model is slightly more structured and requires a resolver.
- Existing repositories that relied on handwritten built-in commands may need to move to `preset`.

### Neutral

- Custom providers continue to work.
- The scaffolded config now documents preset-based configuration instead of repeating built-in command strings.
