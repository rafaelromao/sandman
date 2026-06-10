# ADR-0024: Remove Pi agent support

## Status

accepted

## Context

Sandman previously shipped two built-in agent presets: `opencode` and `pi`. Pi was treated as a first-class citizen throughout the codebase and documentation — it appeared in CONTEXT.md, ADRs, usage docs, config scaffolding, test env vars, and issue templates.

Over time, maintaining Pi parity became pure overhead with no corresponding user demand. Every feature that touched agent behaviour required a Pi counterpart: config snapshots (ADR-0016/ADR-0017), test model env vars (ADR-0020), scaffolded Dockerfiles, command templates, and documentation. The cost of carrying Pi-specific branches, test cases, and documentation edits was not offset by organic usage.

The project has converged on OpenCode as the single built-in agent. Removing Pi simplifies the codebase, documentation, and test matrix with no loss of functionality for any known user.

## Decision

Remove Pi agent support from Sandman. Specifically:

1. Remove all Pi mentions from `CONTEXT.md`, user-facing documentation (`docs/usage/getting-started.md`, `docs/usage/configuration.md`, `docs/usage/commands.md`, `docs/usage/agent-compatibility.md`), `README.md`, `CHANGELOG.md`, and the issue template at `.github/ISSUE_TEMPLATE/agent_improvement.md`.
2. ADR-0006 (built-in agent presets) and ADR-0020 (test model env vars) are left as immutable historical records; this ADR serves as the current decision that supersedes the Pi-specific context in those documents.
3. Mark ADR-0017 (Pi config snapshot split) as superseded by this ADR.
4. Remove the `pi` entry from `config.BuiltInAgentPresets` and all Pi-specific branches in Go source code. Go code changes are tracked separately (issue #782) and are out of scope for this ADR.

No ADR files are deleted — superseded ADRs remain in the repository as historical records.

## Consequences

### Positive

- Documentation, ADRs, and vocabulary no longer mention a removed agent.
- New contributors and agents see a single, supported built-in agent.
- Removing Pi-specific text reduces maintenance surface and future doc-editing overhead.

### Negative

- Users who relied on the `pi` preset can no longer use it out of the box. They must configure a custom provider or migrate to OpenCode.
- The historical record of Pi's existence is preserved in the repository via ADR-0017, ADR-0020, and the git history of the files modified by this change.

### Neutral

- ADR-0017 remains on disk with a superseded status; readers are directed to this ADR for context on the removal.
- The Go source removal (issue #782) handles the runtime deletion of Pi-specific code paths.
