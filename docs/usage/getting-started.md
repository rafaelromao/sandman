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
- **`.sandman/prompt.md`** — Project Prompt Template seeded from Sandman's Default Prompt

The scaffolded config also includes the default agent commit identity:

- `git.author_name: Sandman`
- `git.author_email: sandman.support@gmail.com`

Override both fields with `sandman config set` if you want a project-specific identity. If you clear or omit these fields, Sandman stops injecting commit identity and Git falls back to whatever other config or environment your process provides.

The `init` command interactively prompts you for:

- **Agent preset** — which built-in agent to configure (opencode, claude-code, codex, pi)
- **Build tools preset** — container recipe for the image (generic, dotnet, go, node, or python)
- **Tool version** — version selector for the build toolchain

Sandman auto-detects repo hints and defaults to the matching BuildToolsPreset when it finds .NET, Go, Node, or Python project files; otherwise it falls back to `generic`.

You can skip the prompts by passing flags:

```bash
sandman init --agent opencode --build-tools node
```

## First run

Once initialized, pick an open GitHub issue to delegate:

```bash
sandman run 42
```

Sandman will:

1. Fetch issue #42 from GitHub
2. Create a git worktree at `.sandman/worktrees/sandman/42-<slugified-title>`
3. Render the Project Prompt Template with issue metadata
4. Launch the configured AI agent inside the sandbox
5. Stream agent output to the terminal, prefixed with `[#42]`
6. Log structured events to `.sandman/events.jsonl`

When the agent finishes, check the result:

```bash
sandman history
```

### Attaching to a running daemon

Open a second terminal while a `sandman run` is in progress to stream its output live:

```bash
sandman attach
```

Attach reads from the daemon's control socket at `.sandman/run.sock` and exits with code 0 when the batch finishes and the socket closes.

## Next steps

- [Commands Reference](commands.md) — full list of CLI commands and flags
- [Configuration](configuration.md) — understanding and customizing `.sandman/config.yaml`
- [Sandbox Modes](sandbox-modes.md) — choosing between worktree and container isolation
- [Workflows](workflows.md) — running multiple issues, labels, queries, and dependency-aware execution
