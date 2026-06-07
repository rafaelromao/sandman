# Sandman

A terminal-native CLI tool for orchestrating AFK coding agents in isolated sandboxes.

## Quick Start

```bash
# Prerequisites: Go 1.24+, Git, gh CLI, and an AI agent (OpenCode or Pi)
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
- Serves a local portal for watching current repo runs in the browser

## Documentation

| Guide | Description |
|-------|-------------|
| [Getting Started](docs/usage/getting-started.md) | Prerequisites, installation, and first project setup |
| [Commands Reference](docs/usage/commands.md) | All CLI commands, flags, and issue selection modes |
| [Configuration](docs/usage/configuration.md) | Full config schema, default agent, and CLI config |
| [Portal](docs/usage/portal.md) | Local browser dashboard for repo-scoped Sandman runs |
| [Default Prompt](docs/usage/default-prompt.md) | Bootstrap prompt, lifecycle, and skill handoff |
| [Skills](docs/usage/skills.md) | Shared Sandman skill installation and workflow |
| [Sandbox Modes](docs/usage/sandbox-modes.md) | Worktree vs container-backed sandboxing |
| [Workflows](docs/usage/workflows.md) | Running agents, dependency-aware execution, continue and cleanup |
| [Monitoring and Debugging](docs/usage/monitoring.md) | Status, history, event log, and troubleshooting |
| [Agent Compatibility](docs/usage/agent-compatibility.md) | Built-in agent presets and container auth model |

## Config Overview

Sandman reads from `.sandman/config.yaml`. Key fields:

```yaml
default_agent: opencode
default_parallel: 4
review_command: /oc review
sandbox: podman              # podman, docker, or worktree
container_capacity: 4        # agent runs per container; 0 uses default container capacity behavior
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

Smoke tests (opt-in):

```bash
SANDMAN_TEST_PROVIDERS=opencode,pi go test -tags smoke ./internal/cmd -run Smoke
```

E2E tests (opt-in):

```bash
SANDMAN_TEST_PROVIDERS=opencode,pi go test -tags e2e ./internal/cmd -run PRFlow
```

Gated end-to-end scenarios (opt-in, no build tag required):

```bash
SANDMAN_E2E_GATES=batch go test -run TestRunBatch_EndToEnd ./internal/batch
SANDMAN_E2E_GATES=continue_multi go test -tags e2e -run TestContinueFlow_PodmanSandboxBinarySupportsMultipleIssues ./internal/cmd
SANDMAN_E2E_GATES=all go test ./...
```

`SANDMAN_TEST_PROVIDERS` and `SANDMAN_E2E_GATES` accept a comma list, `all`, or `*`.

Pi smoke/e2e require `pi-free` installed in active Pi setup.

## License

MIT
