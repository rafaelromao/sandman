# Configuration

Sandman reads configuration from `.sandman/config.yaml` in the project root. You can also read and write individual fields via `sandman config get/set`.

## Full schema

```yaml
# Default agent provider name (maps to an entry under `agents` or a built-in preset).
agent: opencode

# Build tools preset for the container image (generic, go).
build_tools: generic

# Review command injected into the prompt template.
review_command: /oc review

# Maximum number of concurrent agent runs.
default_parallel: 4

# Maximum concurrent agent runs per ContainerSandbox.
container_capacity: 4

# Maximum number of ContainerSandbox instances.
# 0 means auto mode: create the minimum needed for active runs.
max_containers: 0

# Directory for git worktrees.
worktree_dir: .sandman/worktrees

# Sandbox mode: podman (default), docker, or worktree.
sandbox: podman

# Git configuration for agent commits.
git:
  author_name: Dev
  author_email: dev@example.com
  default_branch: main

# Agent provider definitions.
agents:
  opencode:
    preset: opencode
    model: gpt-4.1
  claude-code:
    preset: claude-code
    config_dirs:
      - ~/.claude
    config_files:
      - ~/.claude.json
  custom-agent:
    command: "custom-agent --prompt {{.PromptFile}}"
    env:
      API_KEY: ${API_KEY}
```

## Agent providers

Each entry under `agents` defines how Sandman invokes an AI coding agent.

### Using a built-in preset

```yaml
agents:
  opencode:
    preset: opencode
```

Available presets: `opencode`, `claude-code`, `codex`, `pi`. Each preset provides a default command template, config directories, and config files.

### Agent models

`agents.<name>.model` sets the `AgentModel` for a built-in preset.

```yaml
agents:
  opencode:
    preset: opencode
    model: gpt-4.1
```

Precedence:

- `sandman run --model` overrides `agents.<name>.model`
- If neither is set, Sandman leaves the agent's own default model alone
- `model` is only valid for built-in presets; custom command providers manage model selection themselves

Use `sandman config get` and `sandman config set` for the active top-level agent selection before you edit `agents.<name>.model` in `.sandman/config.yaml`:

```bash
sandman config get agent
# opencode

sandman config set agent claude-code
```

### Custom agent command

```yaml
agents:
  my-agent:
    command: "my-agent --file {{.PromptFile}}"
    env:
      MY_KEY: ${MY_KEY}
```

The `command` field supports Go `text/template` syntax with the key `{{.PromptFile}}` (resolved to the relative path of the rendered prompt file). Commands without template placeholders are passed through unchanged.

### Prompt templates

Sandman's prompt lifecycle has three steps:

- **Default Prompt** — the embedded canonical template in `internal/prompt/default_prompt.md`
- **Project Prompt Template** — `.sandman/prompt.md`, created from the Default Prompt by `sandman init` and materialized on run when missing
- **Prompt** — `.sandman/rendered-prompt.md`, the rendered instruction file passed to the agent

The following built-in substitution keys are available in prompt templates:

| Key | Description |
|-----|-------------|
| `{{ISSUE_NUMBER}}` | GitHub issue number |
| `{{ISSUE_TITLE}}` | Issue title |
| `{{ISSUE_BODY}}` | Issue body |
| `{{SOURCE_BRANCH}}` | Branch the agent starts from |
| `{{TARGET_BRANCH}}` | Branch the agent will commit to |
| `{{BRANCH}}` | Alias for `{{SOURCE_BRANCH}}` |
| `{{DEFAULT_BRANCH}}` | Alias for `{{TARGET_BRANCH}}` |
| `{{REVIEW_COMMAND}}` | Review command from config or `--review-command` |

Custom keys can be passed at runtime using the `--prompt-arg KEY=VALUE` flag on `sandman run` and referenced as `{{KEY}}` in the template.

`sandman retry` replays the stored prompt source, prompt args, and review command when the prior run recorded them.
It also replays the stored `AgentModel`, so retries use the same model as the original run when one was recorded.

### Overriding preset defaults

You can override specific fields of a preset while keeping the defaults for others:

```yaml
agents:
  opencode:
    preset: opencode
    config_dirs:
      - ~/.config/opencode
      - /shared/opencode-config
```

### Agent provider fields

| Field | Description |
|-------|-------------|
| `preset` | Built-in preset name to use as a base (opencode, claude-code, codex, pi) |
| `command` | Custom command template; overrides the preset command |
| `env` | Environment variables to set when running the agent. Supports `${VAR}` substitution from the host environment |
| `config_dirs` | Directories to resolve into the container sandbox via a temporary copy (e.g., `~/.claude`). Symlinks are dereferenced during the copy (see ADR-0008). `~` is expanded to the user's home directory. Missing directories are silently skipped |
| `config_files` | Individual files to resolve into the container sandbox via a temporary copy (e.g., `~/.claude.json`). Symlinks are dereferenced during the copy (see ADR-0008). `~` is expanded to the user's home directory. Missing files are silently skipped |
| `keychain_auth` | Whether the agent requires OS keychain access. **Not supported in container mode** — Sandman rejects the batch with an error. Use file-based auth instead |

## Container scheduling configuration

| Key | Default | Description |
|-----|---------|-------------|
| `container_capacity` | `4` | Max concurrent agent runs per `ContainerSandbox`. `1` means isolated execution (one agent per container) |
| `max_containers` | `0` | Max `ContainerSandbox` instances. `0` = auto mode: create the minimum needed for active runs given `container_capacity`. An explicit positive value caps total container-backed concurrency |

When `max_containers=0` and `container_capacity=4` with 6 active runs, Sandman creates 2 containers (4 + 2). When the `max_containers` limit is reached and all containers are at capacity, additional runs queue until capacity frees up.

See [Sandbox Modes](sandbox-modes.md) for detailed scheduling behavior.

## CLI config commands

Use `sandman config get` and `sandman config set` to read and write individual fields:

```bash
sandman config get default_parallel
# 4

sandman config set container_capacity 2
sandman config set git.author_name "My Name"
```

Use `sandman config get` and `sandman config set` for top-level config keys; edit `.sandman/config.yaml` directly for nested agent settings like `agents.<name>.model`.

See [Commands Reference](commands.md) for the full list of supported dot-notation keys.
