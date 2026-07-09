# Overview

Sandman is a CLI tool that orchestrates AFK (Away From Keyboard) coding agents in isolated sandboxes. It turns a clear GitHub issue into an autonomous delivery loop: plan the implementation, run the agent in an isolated environment, store progress in `task.md`, drive review, and merge the PR when the gates pass.

## What Sandman does

- Fetches GitHub issues via the `gh` CLI
- Renders prompt templates for AI coding agents
- Creates isolated sandboxes (git worktrees or containers)
- Orchestrates parallel agent execution with dependency-aware scheduling
- Logs structured events for observability
- Serves a local portal for watching current repo runs in the browser

## The delivery loop

```
Specification  ->  Sandman  ->  Validation
```

You choose the issue. Sandman owns the implementation path:

1. Create an isolated sandbox (worktree or container)
2. Render the task prompt (`task.md`)
3. Plan the implementation
4. Build with TDD and targeted checks
5. Self-review and open a PR
6. Drive review to merge

## What Sandman is not

- **Not a SaaS.** State is local under `.sandman/`.
- **Not a code generator.** Output is reviewed, merged PRs.
- **Not a spec tool.** The spec layer is upstream — use whatever planning process produces clear GitHub issues.
- **Not a replacement for validation.** Treat agent output the way you treat fast human output: as input to validation, not a release confidence signal.

## See also

- [Quick Start](quickstart.md) — the 5-minute path from install to first merged PR
- [Installation](install.md) — full prerequisites and setup
- [Concepts](concepts.md) — the Batch / AgentRun / Sandbox model in prose
- [Positioning](../help/positioning.md) — how Sandman relates to SDD and Loop Engineering
