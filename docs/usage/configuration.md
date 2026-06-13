# Configuration

Sandman reads configuration from `.sandman/config.yaml` in the project root. You can also read and write individual fields via `sandman config get/set`.

## Full schema

```yaml
# Default built-in agent preset used by `sandman run` when `--agent` is omitted.
agent: opencode

# Default model passed to the agent when `--model` is omitted.
# Falls back to the agent provider's configured model if empty.
model: opencode/big-pickle

# Build tools preset for the container image (generic, go, node, python).
build_tools: generic

# Review command injected into the prompt template and shared skill install.
# Defaults to /sandman review, which requires `sandman review` to be
# running before `sandman run`/`continue`/`--ralph` will start. Set
# to /oc review (or any command that does not contain /sandman) to
# opt out of the review daemon guard.
review_command: /sandman review

# Maximum number of concurrent agent runs.
parallel: 4

# Maximum concurrent review-daemon runs.
parallel_reviews: 4

# Idle timeout in seconds for agent runs. When the agent produces no new log
# output for this duration, the heartbeat watchdog aborts the run.
# 0 disables the watchdog (runs never abort due to inactivity).
# Default: 1800 (30 minutes).
run_idle_timeout: 1800

# Number of times to retry a failed AgentRun before recording it as failed.
# 0 disables retries. `sandman run --ralph` silently sets this to 3 for the
# invocation if you do not pass `--retries` on the CLI.
# Default: 3.
retries: 3

# Maximum concurrent agent runs per ContainerSandbox.
# 0 means unlimited (no per-container cap; any number of runs may execute concurrently inside one container).
container_capacity: 4

# Batch pacing delay in seconds after any AgentRun finishes.
# 0 disables pacing.
start_delay: 0

# Maximum number of ContainerSandbox instances.
# 0 = no cap (unbounded pool growth — Sandman creates as many containers as needed for active runs).
max_containers: 0

# Directory for git worktrees.
worktree_dir: .sandman/worktrees

# Sandbox mode: podman (default), docker, or worktree.
sandbox: podman

# Git configuration for branch management.
git:
  base_branch: main

# Sandman installs the built-in agent in scaffolded Dockerfiles and mounts the shared skills directory.
```

## Built-in preset

Sandman has one built-in preset: `opencode`. It is installed into scaffolded Dockerfiles and is the default `agent`.

When you use the `opencode` preset, install the `opencode-shell-strategy` plugin first. Sandman runs OpenCode without a TTY/PTY, so this plugin prevents interactive shell commands from hanging during runs. OpenCode subagents inherit the same instructions.

The built-in preset also sees `~/.agents`, which is where Sandman installs the shared skill folder.

`sandman run --agent` selects the built-in preset per invocation. `sandman config set agent` changes the project default.

Use `sandman run --base-branch` to override `git.base_branch` for a single invocation.

### Prompt templates

Sandman's prompt lifecycle has four steps:

- **Default Task Prompt** — the embedded bootstrap template in `internal/prompt/default-task-prompt.md`
- **Project Prompt Template** — `.sandman/prompt.md`, created from the Default Task Prompt by `sandman init` and materialized on run when missing
- **Sandman Skill** — the shared skill folder in `~/.agents/skills/sandman/`, installed by `sandman init`
- **Prompt** — `.sandman/task.md`, the rendered instruction file passed to the agent

The following built-in substitution keys are available in prompt templates:

| Key | Description |
|-----|-------------|
| `{{ISSUE_NUMBER}}` | GitHub issue number |
| `{{ISSUE_TITLE}}` | Issue title |
| `{{ISSUE_BODY}}` | Issue body |
| `{{SOURCE_BRANCH}}` | Branch the agent starts from |
| `{{BASE_BRANCH}}` | Branch the agent will rebase/PR against |
| `{{BRANCH}}` | Alias for `{{SOURCE_BRANCH}}` |
| `{{REVIEW_COMMAND}}` | Review command from project config |

Custom keys can be passed at runtime using the `--prompt-arg KEY=VALUE` flag on `sandman run` and referenced as `{{KEY}}` in the template.

See [Sandman Skills](skills.md) for the shared workflow details.

`sandman run --continue` replays the stored branch, base branch, agent, and review command from the prior run. It ignores current `--base-branch` or config changes for that continuation, resolves the model from `--model` or `model`, reads the task file (`.sandman/task.md`) from the worktree, and passes its contents verbatim as the agent's resume prompt. The task file now carries three structured fields — `## Source Prompt`, `## Last Skill`, and `## Last Skill Status` — added by [ADR-0023](../adr/0023-handoff-points-to-rendered-prompt-and-captures-last-skill.md) on top of the existing stage fields. When no task file exists, an empty task template is used with a warning on stderr.

## Container scheduling configuration

| Key | Default | Description |
|-----|---------|-------------|
| `container_capacity` | `4` | Max concurrent agent runs per `ContainerSandbox`. `0` = unlimited (no per-container cap), `1` = one agent per container |
| `max_containers` | `0` | Max `ContainerSandbox` instances. `0` = no cap (unbounded pool growth — Sandman creates as many containers as needed for active runs). An explicit positive value caps total container-backed concurrency |

When `max_containers=0` (no cap, unbounded pool growth — Sandman creates as many containers as needed for active runs) and `container_capacity=4` with 6 active runs, Sandman creates 2 containers (4 + 2). When a positive `max_containers` limit is reached and all containers are at capacity, additional runs queue until capacity frees up.

See [Sandbox Modes](sandbox-modes.md) for detailed scheduling behavior.

## Batch pacing

| Key | Default | Description |
|-----|---------|-------------|
| `start_delay` | `0` | Wait this many seconds after any `AgentRun` finishes before starting the next one. `0` disables batch pacing |

`start_delay` is batch-local pacing behavior. It applies across sandbox modes, starts only after the first run completes, and does not change container capacity or max container scheduling.

## Idle timeout

| Key | Default | Description |
|-----|---------|-------------|
| `run_idle_timeout` | `1800` | Seconds of inactivity before the heartbeat watchdog aborts the run. `0` disables the watchdog (runs never abort due to inactivity) |

`run_idle_timeout` detects when an agent has stalled (e.g., blocked on an interactive prompt, deadlocked, or looping). When triggered, the watchdog kills the agent process and marks the run as `aborted`. A `run.idle_timeout` event is written to the event log for diagnostics. The `--run-idle-timeout` CLI flag overrides the config value for a single invocation.

## CLI config commands

Use `sandman config get` and `sandman config set` to read and write individual fields:

```bash
sandman config get parallel
sandman config get parallel_reviews
sandman config set parallel_reviews 8
sandman config set container_capacity 2
sandman config set start_delay 5
sandman config set run_idle_timeout 3600
sandman config set model opencode/BigPickle
sandman config set git.base_branch main
```

Use `sandman config get` and `sandman config set` for top-level config keys.

When you change `review_command` with `sandman config set`, Sandman also regenerates the shared `~/.agents/skills/sandman/` tree. If that installed tree has local edits, Sandman prompts before overwriting in a TTY and fails in non-interactive mode.

Sandman does not store a separate commit identity in project config. Agent commits use your host Git identity resolved in this order:

1. `~/.gitconfig`
2. Host global/XDG Git config such as `~/.config/git/config`
3. Repo-local `.git/config`

If `user.name` or `user.email` is still missing after that lookup, `sandman run` fails before the agent starts.

See [Commands Reference](commands.md) for the full list of supported dot-notation keys.
