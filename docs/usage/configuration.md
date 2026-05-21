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
  author_name: Sandman
  author_email: sandman.support@gmail.com
  default_branch: main

# Sandman installs both built-in agents in scaffolded Dockerfiles.
installed_agents:
  - opencode
  - pi
```

## Built-in agents

Sandman supports two built-in presets: `opencode` and `pi`. Both are installed into scaffolded Dockerfiles. `opencode` is the default `default_agent`.

`sandman run --agent` selects one of those built-ins per invocation. `sandman config set default_agent` changes the project default.

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

`sandman retry` replays the stored prompt source, prompt args, review command, and model when the prior run recorded them.

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
sandman config set git.author_email "me@example.com"
```

Use `sandman config get` and `sandman config set` for top-level config keys.

`sandman init` writes `git.author_name: Sandman` and `git.author_email: sandman.support@gmail.com` into new project configs so the default agent commit identity is explicit. Sandman injects that identity into the agent process and does not write it to your host git config or repo-local `.git/config`. If you clear these fields, Sandman stops injecting identity and Git falls back to whatever other config or environment your process provides.

See [Commands Reference](commands.md) for the full list of supported dot-notation keys.
