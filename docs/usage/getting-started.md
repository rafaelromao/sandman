# Getting Started

## Prerequisites

- [Go](https://go.dev/dl/) 1.24 or later
- [Git](https://git-scm.com/)
- [`gh` CLI](https://cli.github.com/) — authenticated and with `repo` scope
- An AI coding agent: [OpenCode](https://opencode.ai/), [Claude Code](https://claude.com/product/claude-code), [Codex](https://openai.com/codex/), or [Pi](https://pi.dev)
- (Optional but recommended) [Podman](https://podman.io/) or [Docker](https://docker.com/) for container-backed sandboxing

## Installation

### Quick install

```bash
go install github.com/rafaelromao/sandman/cmd/sandman@latest
```

### Build from source

```bash
git clone https://github.com/rafaelromao/sandman.git
cd sandman
make build
# Optionally install to $GOPATH/bin
make install
```

## Initialize a project

Navigate to a git repository where you want to run AFK agents and run:

```bash
sandman init
```

This scaffolds the `.sandman/` directory with:

- **`.sandman/config.yaml`** — Sandman configuration with the selected agent preset
- **`.sandman/Dockerfile`** — Container image definition for container-backed sandboxing
- **`.sandman/prompt.md`** — Default agent prompt template

The `init` command interactively prompts you for:

- **Agent preset** — which built-in agent to configure (opencode, claude-code, codex, pi)
- **Build tools preset** — language-specific tooling for the container image (generic or go)
- **Tool version** — version selector for the build toolchain

You can skip the prompts by passing flags:

```bash
sandman init --agent opencode --build-tools go
```

## First run

Once initialized, pick an open GitHub issue to delegate:

```bash
sandman run 42
```

Sandman will:

1. Fetch issue #42 from GitHub
2. Create a git worktree at `.sandman/worktrees/sandman/42-<slugified-title>`
3. Render the prompt template with issue metadata
4. Launch the configured AI agent inside the sandbox
5. Stream agent output to the terminal, prefixed with `[#42]`
6. Log structured events to `.sandman/events.jsonl`

When the agent finishes, check the result:

```bash
sandman history
```

## Next steps

- [Commands Reference](commands.md) — full list of CLI commands and flags
- [Configuration](configuration.md) — understanding and customizing `.sandman/config.yaml`
- [Sandbox Modes](sandbox-modes.md) — choosing between worktree and container isolation
- [Workflows](workflows.md) — running multiple issues, labels, queries, and dependency-aware execution
