# Agent Compatibility

Sandman includes built-in presets for two AI coding agents: `opencode` and `pi`.

## Built-in presets

| Preset | Display Name | Command Template |
|--------|-------------|------------------|
| `opencode` | OpenCode | `opencode run "$(cat {{.PromptFile}})"` |
| `pi` | Pi | `pi --print --provider <provider> --model <model> "$(cat {{.PromptFile}})"` |

## Model selection

Sandman wires `AgentModel` only for built-in presets. `sandman run --model` overrides the config value for the selected built-in agent. Pi expects `provider/model` and Sandman splits it into separate provider and model flags.

`sandman run --model` takes precedence over the configured default model.

Sandman does not hardcode default models for built-in agents because those defaults vary by tool, account, and provider.

## Compatibility matrix

| Preset | Worktree | Container | Keychain Auth | Host Config Paths |
|--------|----------|-----------|---------------|-------------------|
| `opencode` | Yes | Yes | No | `~/.config/opencode`, `~/.local/share/opencode`, `~/.claude` |
| `pi` | Yes | Yes | No | `~/.pi` |

Both presets support worktree and container-backed sandboxing.

## Container auth model

**Keychain auth is not supported in container mode.** If a built-in agent has `keychain_auth: true` and a container sandbox is selected, Sandman rejects the batch with a clear error message.

To use an agent inside a container:

1. Disable OS keychain integration for the agent CLI
2. Use file-based authentication (e.g., API keys stored in config files)
3. Sandman resolves config files and directories into the container via a temporary copy (see ADR-0008)

OpenCode stores its session tokens in `~/.claude` alongside its own config. Sandman resolves those paths into the container automatically when using the `opencode` preset, and the agent should be configured to use file-based auth.

## Worktree management

Sandman manages worktrees itself — the agent does not need to create or switch branches. Sandman:

1. Creates a git worktree at `.sandman/worktrees/sandman/<issue-number>-<slugified-title>`
2. Checks out the default branch as a starting point
3. The agent works inside this pre-created worktree directory
4. When the agent finishes, Sandman records the branch for commit history

Agents should work within the current directory and use standard git operations (add, commit, push) as needed.

## Container mode specifics

- Container mode is Linux-first. On macOS or Windows, container runtimes (Docker/Podman) must be configured to run Linux containers
- Config directories and files with `~` are expanded to the user's home directory on the host before being resolved into a temporary copy for the container
- Missing config directories and files are silently skipped (no error if an optional config path does not exist)
- Container images are built from `.sandman/Dockerfile` at project initialization
