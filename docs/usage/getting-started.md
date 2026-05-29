# Getting Started

## Prerequisites

- [Go](https://go.dev/dl/) 1.24 or later
- [Git](https://git-scm.com/)
- [`gh` CLI](https://cli.github.com/) — authenticated and with `repo` scope
- An AI coding agent: [OpenCode](https://opencode.ai/) or [Pi](https://pi.dev)
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

- **`.sandman/config.yaml`** — Sandman configuration with the selected default agent preset
- **`.sandman/Dockerfile`** — Container image definition for container-backed sandboxing
- **`.sandman/prompt.md`** — Project Prompt Template seeded from Sandman's Default Prompt

Sandman also installs the shared `sandman` skill folder into `~/.agents/skills/sandman/` if it does not already exist.

Agent commits use your host Git identity. Before the first run, make sure your Git config resolves both values:

- `user.name`
- `user.email`

Sandman resolves them from `~/.gitconfig`, then the host global/XDG Git config, then repo-local `.git/config`, and stops early if either value is missing.

The `init` command interactively prompts you for:

- **Default agent preset** — which built-in agent to use by default (opencode or pi)
- **Build tools preset** — container recipe for the image (generic, dotnet, go, node, or python)
- **Tool version** — version selector for the build toolchain

Sandman auto-detects repo hints and defaults to the matching BuildToolsPreset when it finds .NET, Go, Node, or Python project files; otherwise it falls back to `generic`.

You can skip the prompts by passing flags:

```bash
sandman init --default-agent opencode --build-tools node
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

If you want a prompt-only run instead, use `--prompt` or `--template` with no issue arguments, `--label`, `--query`, or `--ralph`, and omit issue placeholders like `{{ISSUE_NUMBER}}`, `{{ISSUE_TITLE}}`, and `{{ISSUE_BODY}}`:

```bash
sandman run --base-branch main --prompt "Return only OK."
```

That creates a no-issue run with a `sandman/<slug>-<timestamp>` branch.

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
