# Sandman

A terminal-native CLI tool for orchestrating AFK coding agents in isolated sandboxes.

## Overview

Sandman manages the lifecycle of automated coding workflows:

- Fetches GitHub issues via the `gh` CLI
- Renders prompt templates for AI coding agents
- Creates isolated sandboxes (git worktrees or containers)
- Orchestrates parallel agent execution
- Logs structured events for observability

## Prerequisites

- [Go](https://go.dev/dl/) 1.24 or later
- [Git](https://git-scm.com/)
- An AI coding agent such as [opencode](https://opencode.ai/), [Claude Code](https://claude.com/product/claude-code), [Codex](https://openai.com/codex/), or [Pi](https://pi.dev)
- [`gh` CLI](https://cli.github.com/) (optional but recommended for GitHub integration)

## Installation

### Quick install

Install the latest release directly to `$GOPATH/bin`:

```bash
go install github.com/rafaelromao/sandman/cmd/sandman@latest
```

### Build from source

```bash
# Clone the repository
git clone https://github.com/rafaelromao/sandman.git
cd sandman

# Build the binary
make build

# Optionally install to $GOPATH/bin
make install
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

> **Note:** If you built from source without `make install`, use `./sandman` instead of `sandman`.

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

Use the Makefile for common development tasks:

```bash
# Format, vet, and test
make check

# Build the binary
make build

# Install to $GOPATH/bin
make install

# Clean build artifacts
make clean
```

### Alternative: raw Go commands

If you prefer not to use `make`:

```bash
# Run tests
go test ./...

# Run linter and vet
go vet ./...

# Format code
gofmt -w .
```

## License

MIT
