# Quick Start

From zero to first agent run in five minutes.

## Install

```bash
go install github.com/rafaelromao/sandman/cmd/sandman@v1.0.0-rc.1
```

Requires Go 1.25+, Git, and the [`gh` CLI](https://cli.github.com/) authenticated with `repo` scope.

## Initialize a project

```bash
cd my-repo
sandman init
```

This scaffolds `.sandman/` with config, a Dockerfile, and prompt templates. It also installs the shared `sandman` skill into `~/.agents/skills/sandman/`.

Make sure your Git identity is set — Sandman commits under your name:

```bash
git config --global user.name "Your Name"
git config --global user.email "you@example.com"
```

## Run an agent

```bash
sandman run 42
```

Sandman fetches the issue, creates an isolated worktree, renders the task prompt, and launches the agent. Output streams to the terminal.

## Check progress

```bash
sandman status     # active runs
sandman history    # completed runs
sandman portal     # browser dashboard at http://127.0.0.1:5000
```

## OpenCode setup

Sandman runs OpenCode headlessly. Install the shell-strategy plugin first:

```bash
git clone https://github.com/JRedeker/opencode-shell-strategy.git ~/.config/opencode/plugin/shell-strategy
```

Add it to `~/.config/opencode/opencode.json`:

```json
{
  "instructions": [
    "~/.config/opencode/plugin/shell-strategy/shell_strategy.md"
  ]
}
```

Restart OpenCode after installing.

## See also

- [Installation](install.md) — full prerequisites, build from source, init details
- [Concepts](concepts.md) — understand Batch, AgentRun, Sandbox before running real work
- [Commands](../usage/commands.md) — every CLI flag
- [Workflows](../usage/workflows.md) — by issue range, label, query, auto mode
