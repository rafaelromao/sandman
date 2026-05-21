# Sandman

A terminal-native CLI tool for orchestrating AFK coding agents in isolated sandboxes.

## Quick Start

```bash
# Prerequisites: Go 1.24+, Git, gh CLI, and an AI agent (opencode or pi)
# 1. Install
go install github.com/rafaelromao/sandman/cmd/sandman@latest

# 2. Initialize a project
cd my-repo && sandman init

# 3. Run agents for GitHub issues
sandman run 42 43

# 4. Check progress
sandman status
sandman history
```

## Overview

Sandman manages the lifecycle of automated coding workflows:

- Fetches GitHub issues via the `gh` CLI
- Renders prompt templates for AI coding agents
- Creates isolated sandboxes (git worktrees or containers)
- Orchestrates parallel agent execution with dependency-aware scheduling
- Logs structured events for observability

## Documentation

| Guide | Description |
|-------|-------------|
| [Getting Started](docs/usage/getting-started.md) | Prerequisites, installation, and first project setup |
| [Commands Reference](docs/usage/commands.md) | All CLI commands, flags, and issue selection modes |
| [Configuration](docs/usage/configuration.md) | Full config schema, default agent, and CLI config |
| [Default Prompt](docs/usage/default-prompt.md) | Canonical prompt text, lifecycle, and section-by-section guide |
| [Sandbox Modes](docs/usage/sandbox-modes.md) | Worktree vs container-backed sandboxing |
| [Workflows](docs/usage/workflows.md) | Running agents, dependency-aware execution, retry and cleanup |
| [Monitoring and Debugging](docs/usage/monitoring.md) | Status, history, event log, and troubleshooting |
| [Agent Compatibility](docs/usage/agent-compatibility.md) | Built-in agent presets and container auth model |

## Config Overview

Sandman reads from `.sandman/config.yaml`. Key fields:

```yaml
default_agent: opencode
default_parallel: 4
review_command: /oc review
sandbox: podman              # podman, docker, or worktree
container_capacity: 4        # agent runs per container
max_containers: 0            # auto mode; or set a fixed limit
git:
  author_name: Sandman
  author_email: sandman.support@gmail.com
  default_branch: main
installed_agents:
  - opencode
  - pi
```

`sandman init` writes `git.author_name` and `git.author_email` with the Sandman default identity. If you clear them, Sandman stops injecting identity and Git falls back to whatever other config or environment your process provides, without mutating your host git config.

See [Configuration](docs/usage/configuration.md) for the full schema.

## Development

```bash
make check    # Format, vet, test
make build    # Build binary
make install  # Install to $GOPATH/bin
```

Smoke tests (opt-in):

```bash
SANDMAN_SMOKE_PROVIDERS=opencode,pi go test -tags smoke ./internal/cmd -run Smoke
```

## License

MIT
