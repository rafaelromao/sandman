# Commands Reference

## `sandman init`

Scaffolds `.sandman/` configuration files in the current directory.

```bash
sandman init [flags]
```

| Flag | Description |
|------|-------------|
| `--build-tools` | Build tools preset (`generic` or `go`) |
| `--tool-version` | Version selector (`latest`, `lts`, `repo`, or semver shorthand) |
| `--agent` | Built-in agent preset (`opencode`, `claude-code`, `codex`, `pi`) |

If flags are omitted, interactive prompts guide you through the choices.

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

### Flag interactions

- `--next` is mutually exclusive with issue arguments, `--label`, and `--query`
- `--include-dependencies` is mutually exclusive with `--interactive`
- `--interactive` requires exactly one issue
- `--parallel` limits total concurrent `AgentRun`s across all sandboxes
- `--container-capacity` limits concurrent `AgentRun`s per `ContainerSandbox`
- `--max-containers` caps the number of `ContainerSandbox` instances; `0` means auto-scale
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

Reuses the previously created branch. Useful after a transient failure.

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
| `git.author_name` | string | `Dev` |
| `git.author_email` | string | `dev@example.com` |
