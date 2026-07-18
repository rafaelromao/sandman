# Sandman

<a href="https://github.com/rafaelromao/sandman">
  <img src="https://raw.githubusercontent.com/rafaelromao/sandman/main/assets/badge-built-with-sandman.svg" alt="Built with Sandman" width="154" />
</a>

CLI tool for orchestrating AFK coding agents in isolated sandboxes.

## Quick Start

```bash
# Prerequisites: Git, gh and OpenCode properly configured

# 1. Initialize a project
cd my-repo && sandman init

# 2. Run the review daemon (optional)
sandman review

# 3. Open the browser portal (optional)
sandman portal

# 4. Implement your GitHub issues AFK
sandman run 42 43
```

## Overview

Sandman manages the lifecycle of automated coding workflows:

- Fetches GitHub issues via the `gh` CLI
- Renders prompt templates for AI coding agents
- Creates isolated sandboxes (git worktrees or containers)
- Orchestrates parallel agent execution with dependency-aware scheduling
- Logs structured events for observability
- Serves a local portal for watching current repo runs in the browser (binds to `127.0.0.1` by default; use `--host 0.0.0.0` or `SANDMAN_PORTAL_HOST=0.0.0.0` to expose on all interfaces)

## Documentation

| Guide | Description |
|-------|-------------|
| [Overview](docs/get-started/overview.md) | What Sandman is, the delivery loop, and what it is not |
| [Quick Start](docs/get-started/quickstart.md) | The five-minute path from install to first run |
| [Installation](docs/get-started/install.md) | Prerequisites, installation, OpenCode setup, and project setup |
| [Concepts](docs/get-started/concepts.md) | The Batch / AgentRun / Sandbox / Specification model in prose |
| [Commands Reference](docs/usage/commands.md) | All CLI commands, flags, and issue selection modes |
| [Scaffolding and Supported Languages](docs/usage/scaffolding.md) | `sandman init`, generated files, build-tool presets, and language version selection |
| [Configuration](docs/usage/configuration.md) | Full config schema, default agent, and CLI config |
| [Workflows](docs/usage/workflows.md) | Running agents, dependency-aware execution, continue and cleanup |
| [Sandbox Modes](docs/usage/sandbox-modes.md) | Worktree vs container-backed sandboxing |
| [Portal](docs/usage/portal.md) | Local browser dashboard for repo-scoped Sandman runs |
| [Reviews](docs/usage/reviews.md) | Review daemon and review-run output |
| [Default Task Prompt](docs/usage/default-task-prompt.md) | Bootstrap task prompt, lifecycle, and task file |
| [Skills](docs/usage/skills.md) | Shared Sandman skill installation and workflow |
| [Badge](docs/usage/badge.md) | Built with Sandman badge — trigger, idempotency, and opt-out |
| [Monitoring and Debugging](docs/usage/monitoring.md) | Status, history, event log, and idle timeout |
| [Troubleshooting](docs/help/troubleshooting.md) | Common failure modes and the first thing to try for each |
| [Agent Compatibility](docs/usage/agent-compatibility.md) | Built-in agent presets and container auth model |
| [FAQ](docs/help/faq.md) | Questions people ask before installing |

Other:

- [Architecture Overview](docs/architecture/overview.md) and [Disk Layout](docs/architecture/disk-layout.md)
- [Positioning](docs/help/positioning.md) — what Sandman is and isn't, in plain language
- [Development docs](docs/development/README.md) — contributor setup, project structure, architecture guidance, testing, and the [Documentation page](docs/development/documentation.md) covering docs and embedded-skill rules

## Development

Contributing to Sandman? See the [Development docs](docs/development/README.md).

## License

MIT
