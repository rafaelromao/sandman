# Commands Reference

## `sandman init`

Scaffolds `.sandman/` configuration files in the current directory.

```bash
sandman init [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--build-tools` | `""` | Build tools preset (`generic`, `go`, `python`) |
| `--tool-version` | `""` | Version selector (`latest`, `lts`, `repo`, or semver shorthand) |
| `--agent` | `""` | Built-in agent preset (`opencode`, `claude-code`, `codex`, `pi`) |

When `--tool-version` is omitted, `init` infers `repo` as the version selector, reading version hints from the repo when available. If flags are completely omitted and no repo hints are found, interactive prompts guide you through the choices.

## `sandman run`

Run an AFK agent for selected GitHub issues.

```bash
sandman run [issue...] [flags]
```

### Issue selection modes

Exactly one selection mode is required:

| Mode | Example | Description |
|------|---------|-------------|
| Explicit numbers | `sandman run 42 43` | One or more issue numbers |
| `--label` | `sandman run --label ready-for-agent` | All open issues with the given label |
| `--query` | `sandman run --query "label:bug is:open"` | GitHub search query |
| `--next` | `sandman run --next 3` | N lowest-numbered open issues labeled `ready-for-agent` |
| Interactive picker | `sandman run` (in a TTY) | Opens a numbered list of open issues to select from |

### Execution flags

| Flag | Default | Description |
|------|---------|-------------|
| `--parallel` | `default_parallel` from config (4) | Maximum concurrent agent runs |
| `--preserve` | `false` | Keep worktrees after successful runs |
| `--debug` | `false` | Print worktree path and instructions on failure |
| `--sandbox` | config default (`podman`) | Sandbox mode: `podman`, `docker`, or `worktree` |
| `--container-capacity` | config default (4) | Max concurrent agent runs per container; `1` = isolated container |
| `--max-containers` | config default (0) | Max containers; `0` = auto mode |
| `--interactive` | `false` | Run agent in interactive mode (requires exactly one issue) |
| `--include-dependencies` | `false` | Auto-expand batch with transitive blockers |
| `--label` | — | Select issues by label |
| `--query` | — | Select issues by GitHub search query |
| `--next` | — | Select N lowest-numbered open `ready-for-agent` issues |
| `--prompt` | — | Inline prompt template (overrides file-based templates) |
| `--template` | — | Path to prompt template file |
| `--prompt-arg` | — | Custom template substitution (`KEY=VALUE`, repeatable) |
| `--review-command` | — | Review command injected into `{{REVIEW_COMMAND}}` |
| `--model` | — | Override the `AgentModel` for built-in presets |

### Flag interactions

- `--next` is mutually exclusive with issue arguments, `--label`, and `--query`
- `--include-dependencies` is mutually exclusive with `--interactive`
- `--interactive` requires exactly one issue
- `--parallel` limits total concurrent `AgentRun`s across all sandboxes
- `--container-capacity` limits concurrent `AgentRun`s per `ContainerSandbox`
- `--max-containers` caps the number of `ContainerSandbox` instances; `0` means auto-scale
- `--model` only applies to built-in presets and overrides any `agents.<name>.model` value
- When `--max-containers` and `--container-capacity` together constrain concurrency below `--parallel`, the tighter limit wins

## `sandman status`

Show currently active (in-progress) agent runs.

```bash
sandman status
```

Reads `.sandman/events.jsonl` and displays runs that have started but not yet finished, with elapsed time.

## `sandman history`

Show all completed agent runs from the event log.

```bash
sandman history
```

Displays each completed run with status, duration, and branch name.

## `sandman retry`

Retry the last agent run for a given issue.

```bash
sandman retry <issue-number>
```

Reuses the previously created branch and recorded `AgentModel`. Useful after a transient failure.

## `sandman clean`

Clean up sandbox resources and stale worktrees.

```bash
sandman clean [flags]
```

| Flag | Description |
|------|-------------|
| `--all` | Remove all worktrees and logs |
| `--success` | Remove worktrees and logs for successful runs only |
| `--failed` | Remove worktrees and logs for failed runs only |

Exactly one flag is required.

## `sandman attach`

Attach to a running sandman daemon and stream its output.

```bash
sandman attach
```

If no daemon is running (`.sandman/run.sock` doesn't exist), prints a clear error. Otherwise connects to the daemon's control socket and reads raw bytes to stdout until the socket closes (EOF).

Useful for monitoring a long-running batch from a separate terminal.

## `sandman config`

Manage Sandman configuration via dot-notation keys.

```bash
sandman config get <key>
sandman config set <key> <value>
```

### Supported keys

| Key | Type | Example |
|-----|------|---------|
| `agent` | string | `opencode` |
| `build_tools` | string | `go` |
| `default_parallel` | int | `4` |
| `review_command` | string | `/oc review` |
| `container_capacity` | int | `4` |
| `max_containers` | int | `0` |
| `worktree_dir` | string | `.sandman/worktrees` |
| `sandbox` | string | `podman` |
| `git.default_branch` | string | `main` |
| `git.author_name` | string | `My Name` |
| `git.author_email` | string | `me@example.com` |

If you do not set these keys explicitly, Sandman uses the runtime fallback identity `Sandman <sandman.support@gmail.com>` for agent-created commits.
