# Configuration

Sandman reads configuration from `.sandman/config.yaml` in the project root. You can also read and write individual fields via `sandman config get/set`.

## Full schema

```yaml
# Default built-in agent preset used by `sandman run` when `--agent` is omitted.
default_agent: opencode

# Build tools preset for the container image (generic, go, node, python).
build_tools: generic

# Review command injected into the prompt template.
review_command: /oc review

# Maximum number of concurrent agent runs.
default_parallel: 4

# Maximum concurrent agent runs per ContainerSandbox.
# 0 means auto/default mode: use the default container capacity behavior.
container_capacity: 4

# Batch pacing delay in seconds after any AgentRun finishes.
# 0 disables pacing.
start_delay: 0

# Maximum number of ContainerSandbox instances.
# 0 means auto mode: create the minimum needed for active runs.
max_containers: 0

# Directory for git worktrees.
worktree_dir: .sandman/worktrees

# Sandbox mode: podman (default), docker, or worktree.
sandbox: podman

# Git configuration for branch management.
git:
  base_branch: main

# Sandman installs both built-in agents in scaffolded Dockerfiles and mounts the shared skills directory.
installed_agents:
  - opencode
  - pi
```

## Built-in agents

Sandman supports two built-in presets: `opencode` and `pi`. Both are installed into scaffolded Dockerfiles. `opencode` is the default `default_agent`.

Both built-in presets also see `~/.agents`, which is where Sandman installs the shared skill folder.

`sandman run --agent` selects one of those built-ins per invocation. `sandman config set default_agent` changes the project default.

Use `sandman run --base-branch` to override `git.base_branch` for a single invocation.

### Prompt templates

Sandman's prompt lifecycle has four steps:

- **Default Prompt** — the embedded bootstrap template in `internal/prompt/default_prompt.md`
- **Project Prompt Template** — `.sandman/prompt.md`, created from the Default Prompt by `sandman init` and materialized on run when missing
- **Sandman Skill** — the shared skill folder in `~/.agents/skills/sandman/`, installed by `sandman init`
- **Prompt** — `.sandman/rendered-prompt.md`, the rendered instruction file passed to the agent

The following built-in substitution keys are available in prompt templates:

| Key | Description |
|-----|-------------|
| `{{ISSUE_NUMBER}}` | GitHub issue number |
| `{{ISSUE_TITLE}}` | Issue title |
| `{{ISSUE_BODY}}` | Issue body |
| `{{SOURCE_BRANCH}}` | Branch the agent starts from |
| `{{BASE_BRANCH}}` | Branch the agent will rebase/PR against |
| `{{BRANCH}}` | Alias for `{{SOURCE_BRANCH}}` |
| `{{REVIEW_COMMAND}}` | Review command from config or `--review-command` |

Custom keys can be passed at runtime using the `--prompt-arg KEY=VALUE` flag on `sandman run` and referenced as `{{KEY}}` in the template.

See [Sandman Skills](skills.md) for the shared workflow details.

`sandman continue` replays the stored branch, base branch, agent, model, and review command from the prior run. It ignores current `--base-branch` or config changes for that continuation, then prepends `.sandman/continuation-context.md` to `.sandman/continue-prompt.md` when present.

## Container scheduling configuration

| Key | Default | Description |
|-----|---------|-------------|
| `container_capacity` | `4` | Max concurrent agent runs per `ContainerSandbox`. `0` = auto/default mode, `1` means one agent per container |
| `max_containers` | `0` | Max `ContainerSandbox` instances. `0` = auto mode: create the minimum needed for active runs given `container_capacity`. An explicit positive value caps total container-backed concurrency |

When `max_containers=0` and `container_capacity=4` with 6 active runs, Sandman creates 2 containers (4 + 2). When the `max_containers` limit is reached and all containers are at capacity, additional runs queue until capacity frees up.

See [Sandbox Modes](sandbox-modes.md) for detailed scheduling behavior.

## Batch pacing

| Key | Default | Description |
|-----|---------|-------------|
| `start_delay` | `0` | Wait this many seconds after any `AgentRun` finishes before starting the next one. `0` disables batch pacing |

`start_delay` is batch-local pacing behavior. It applies across sandbox modes, starts only after the first run completes, and does not change container capacity or max container scheduling.

## CLI config commands

Use `sandman config get` and `sandman config set` to read and write individual fields:

```bash
sandman config get default_parallel 4
sandman config set container_capacity 2
sandman config set start_delay 5
sandman config set git.base_branch main
```

Use `sandman config get` and `sandman config set` for top-level config keys.

Sandman does not store a separate commit identity in project config. Agent commits use your host Git identity resolved in this order:

1. `~/.gitconfig`
2. Host global/XDG Git config such as `~/.config/git/config`
3. Repo-local `.git/config`

If `user.name` or `user.email` is still missing after that lookup, `sandman run` fails before the agent starts.

See [Commands Reference](commands.md) for the full list of supported dot-notation keys.
