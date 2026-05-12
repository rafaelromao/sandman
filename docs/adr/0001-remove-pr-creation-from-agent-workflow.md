# ADR-0001: Remove PR creation from agent workflow

## Status

accepted

## Context

Sandman originally orchestrated the full agent lifecycle including prompt rendering, sandbox execution, and pull-request creation via `gh pr create`. After manual workflow explorations, we determined that PR creation should be driven by the agent itself via instructions in the prompt, rather than by Sandman orchestrating it after the agent run.

This changes the boundary of Sandman's responsibility: it still creates branches, renders prompts, and manages sandboxes, but it no longer interacts with GitHub to open pull requests. The agent is expected to handle `gh pr create` (or equivalent) if the workflow requires it.

## Decision

We will remove all PR creation logic from Sandman:

1. Remove `CreatePR` from the `github.Client` interface and `CLIClient` implementation.
2. Remove `Finalize` from `AgentRun` and the `PRURL` field from `AgentRunResult`.
3. Simplify `Runnable.Run` and `AgentRun.Run` signatures by dropping `github.Client` and `defaultBranch` parameters.
4. Replace `pr_url` with `branch` in the `run.finished` event payload.
5. Remove `pr_template` from the configuration model.
6. Remove `ReadRunResult` / `RunResult` from the `Sandbox` interface and implementations, since they were only consumed by PR creation.
7. Update the `history` command to display branch name instead of PR URL.

## Consequences

### Positive

- Simpler Sandman core: fewer GitHub permissions, less CLI surface, and no PR-specific state to manage.
- Agents gain full control over PR timing, title, body, and template choice.
- Reduced risk of Sandman failing after a successful agent run because of PR creation errors.

### Negative

- Agents must be explicitly instructed (via prompt) to create PRs if that is desired.
- The `history` command no longer surfaces a direct link to review output; users must navigate from branch name.

### Neutral

- `.github/PULL_REQUEST_TEMPLATE.md` and `CONTRIBUTING.md` are kept as human-facing project files and are unaffected.
