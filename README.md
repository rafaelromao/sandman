# Sandman

A terminal-native CLI tool for orchestrating AFK coding agents in isolated sandboxes.

## Overview

Sandman manages the lifecycle of automated coding workflows:

- Fetches GitHub issues via the `gh` CLI
- Renders prompt templates for AI coding agents
- Creates isolated sandboxes (git worktrees or containers)
- Orchestrates parallel agent execution
- Logs structured events for observability

## Installation

```bash
go install github.com/rafaelromao/sandman/cmd/sandman@latest
```

## Quick Start

```bash
# Initialize a Sandman project in the current directory
sandman init

# Run an AFK agent for specific issues
sandman run 42 43 44

# Check status of current and recent agent runs
sandman status

# View event log
sandman history
```

## Configuration

Sandman reads configuration from `.sandman/config.yaml`:

```yaml
agent: opencode
default_parallel: 4
worktree_dir: .sandman/worktrees
sandbox: worktree
git:
  author_name: Dev
  author_email: dev@example.com
agents:
  opencode:
    name: opencode
    command: "opencode --worktree {{.Worktree}}"
    env:
      API_KEY: ${API_KEY}
```

## Development

```bash
# Run tests
go test ./...

# Run linter and vet
go vet ./...

# Format code
gofmt -w ./...
```

## License

MIT
