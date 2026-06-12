# Agent Compatibility

Sandman includes one built-in preset: `opencode`.

## Built-in preset

| Preset | Display Name | Command Template |
|--------|-------------|------------------|
| `opencode` | OpenCode | `opencode run "$(cat {{.PromptFile}})"` |

## OpenCode shell strategy

Sandman uses OpenCode in a headless environment, so `opencode` must have the `opencode-shell-strategy` plugin installed before it is used with Sandman. The plugin teaches OpenCode to avoid interactive shell commands that would hang without a TTY/PTY. OpenCode subagents inherit the same instructions.

### Install

```bash
git clone https://github.com/JRedeker/opencode-shell-strategy.git ~/.config/opencode/plugin/shell-strategy
```

Add the instruction file to `~/.config/opencode/opencode.json`:

```json
{
  "instructions": [
    "~/.config/opencode/plugin/shell-strategy/shell_strategy.md"
  ]
}
```

Restart OpenCode after installing it.

### What it prevents

| Command type | Hangs in Sandman | Safer alternative |
|-------------|------------------|--------------------|
| Package manager prompts | `npm init` | `npm init -y` |
| Git commit prompts | `git commit` | `git commit -m "msg"` |
| Merge prompts | `git merge branch` | `git merge --no-edit branch` |
| Interactive editors | `vim`, `nano`, `vi` | Use OpenCode file tools |
| Pagers / REPLs | `less`, `more`, `man`, `python` | Non-interactive flags or direct file tools |

## Model selection

Sandman wires `AgentModel` only for built-in presets. `sandman run --model` overrides the configured default.

The model resolution order is:

1. `--model` flag on `sandman run` or `sandman run --continue`
2. `model` from `.sandman/config.yaml`
3. The selected agent provider's configured model (e.g., from the agent's `model` field)

If none are set, no model flag is passed to the agent, leaving it to the agent's own default.

## Compatibility matrix

| Preset | Worktree | Container | Keychain Auth | Host Config Paths |
|--------|----------|-----------|---------------|-------------------|
| `opencode` | Yes | Yes | No | `~/.config/opencode`, `~/.local/share/opencode`, `~/.claude`, `~/.agents` |

The built-in preset supports worktree and container-backed sandboxing.

## Container auth model

**Keychain auth is not supported in container mode.** If a built-in agent has `keychain_auth: true` and a container sandbox is selected, Sandman rejects the batch with a clear error message.

To use an agent inside a container:

1. Disable OS keychain integration for the agent CLI
2. Use file-based authentication (e.g., API keys stored in config files)
3. Sandman resolves config files and directories into the container via a temporary copy (see ADR-0008)

OpenCode reads `CLAUDE.md` and `.claude/skills/` if they exist. Sandman resolves `~/.claude` into the container automatically so those files are available to the agent.

Sandman installs its shared skill into `~/.agents/skills`, so the built-in preset mounts `~/.agents` in container mode.

## Worktree management

Sandman manages worktrees itself — the agent does not need to create or switch branches. Sandman:

1. Creates a git worktree at `.sandman/worktrees/sandman/<issue-number>-<slugified-title>`
2. Checks out the base branch as a starting point
3. The agent works inside this pre-created worktree directory
4. When the agent finishes, Sandman records the branch for commit history

Agents should work within the current directory and use standard git operations (add, commit, push) as needed.

## Container mode specifics

- Container mode is Linux-first. On macOS or Windows, container runtimes (Docker/Podman) must be configured to run Linux containers
- Config directories and files with `~` are expanded to the user's home directory on the host before being resolved into a temporary copy for the container
- Missing config directories and files are silently skipped (no error if an optional config path does not exist)
- Container images are built from `.sandman/Dockerfile` at project initialization
