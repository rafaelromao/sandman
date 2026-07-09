# Sandman

<a href="https://github.com/rafaelromao/sandman">
  <img src="https://raw.githubusercontent.com/rafaelromao/sandman/main/assets/badge-built-with-sandman.svg" alt="Built with Sandman" width="154" />
</a>

CLI tool for orchestrating AFK coding agents in isolated sandboxes.

## Quick Start

```bash
# Prerequisites: Go 1.24+, Git, gh CLI, and an AI agent (OpenCode)
# 1. Install
go install github.com/rafaelromao/sandman/cmd/sandman@latest

# 2. Initialize a project
cd my-repo && sandman init

# 3. Run agents for GitHub issues
sandman run 42 43
sandman run 42:45
sandman run 42:45 --label bug

# 4. Check progress
sandman status
sandman history

# 5. Open the browser portal for current repo runs
sandman portal
```

OpenCode needs the `opencode-shell-strategy` plugin before Sandman can use the `opencode` preset.

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
| [Getting Started](docs/usage/getting-started.md) | Prerequisites, installation, and first project setup |
| [Concepts](docs/usage/concepts.md) | The Batch / AgentRun / Sandbox / PRD model in prose |
| [Commands Reference](docs/usage/commands.md) | All CLI commands, flags, and issue selection modes |
| [Configuration](docs/usage/configuration.md) | Full config schema, default agent, and CLI config |
| [Workflows](docs/usage/workflows.md) | Running agents, dependency-aware execution, continue and cleanup |
| [Sandbox Modes](docs/usage/sandbox-modes.md) | Worktree vs container-backed sandboxing |
| [Portal](docs/usage/portal.md) | Local browser dashboard for repo-scoped Sandman runs |
| [Default Task Prompt](docs/usage/default-task-prompt.md) | Bootstrap task prompt, lifecycle, and task file |
| [Skills](docs/usage/skills.md) | Shared Sandman skill installation and workflow |
| [Badge](docs/usage/badge.md) | Built with Sandman badge — trigger, idempotency, and opt-out |
| [Monitoring and Debugging](docs/usage/monitoring.md) | Status, history, event log, and idle timeout |
| [Troubleshooting](docs/usage/troubleshooting.md) | Common failure modes and the first thing to try for each |
| [Agent Compatibility](docs/usage/agent-compatibility.md) | Built-in agent presets and container auth model |
| [FAQ](docs/usage/faq.md) | Questions people ask before installing |

Other:

- [Architecture Overview](docs/architecture/overview.md) and [Disk Layout](docs/architecture/disk-layout.md)
- [Positioning](docs/POSITIONING.md) — what Sandman is and isn't, in plain language
- The browser-rendered docs portal at [`docs/documentation.html`](docs/documentation.html) wraps every guide plus the ADRs.

## Config Overview

Sandman reads from `.sandman/config.yaml`. Key fields:

```yaml
agent: opencode
parallel: 1
review_command: /sandman review
sandbox: podman              # podman, docker, or worktree
container_capacity: 4        # agent runs per container; 0 = unlimited (no per-container cap)
max_containers: 0            # auto mode; or set a fixed limit
git:
  base_branch: main
```

Sandman uses your host Git identity for agent commits. It resolves `user.name` and `user.email` from `~/.gitconfig`, then the host global/XDG Git config, then repo-local `.git/config`, and fails early if either value is missing.

See [Configuration](docs/usage/configuration.md) for the full schema.

## Development

```bash
make check    # Format, vet, test
make build    # Build binary
make install  # Install to $GOPATH/bin
```

See [Running smoke and e2e tests](docs/usage/testing.md) for the full gate list, the per-agent model override, and how to run targeted scenarios.

## License

MIT
