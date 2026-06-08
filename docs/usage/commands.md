# Commands Reference

## `sandman init`

Scaffolds `.sandman/` configuration files in the current directory.

`sandman init` also syncs the shared `sandman` skill folder into `~/.agents/skills/sandman/` using the configured review command.

```bash
sandman init [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--build-tools` | `""` | Build tools preset (`generic`, `dotnet`, `go`, `node`, `python`) |
| `--tool-version` | `""` | Version selector (`latest`, `lts`, `repo`, or semver shorthand) |
| `--default-agent` | `""` | Default built-in agent preset for `init` (`opencode` or `pi`) |
| `--review-command` | `""` | Review command stored as `review_command` in project config; defaults to `/oc review` |
| `--retries` | `-1` | Persist `retries` in scaffolded config. `-1` keeps the built-in default of `3`; `0` disables retries |
| `--run-idle-timeout` | `-1` | Persist `run_idle_timeout` (seconds) in scaffolded config. `-1` keeps the built-in default of `1800`; `0` disables the heartbeat watchdog |

When `--tool-version` is omitted, `init` infers `repo` as the version selector, reading version hints from the repo when available. If flags are completely omitted and no repo hints are found, interactive prompts guide you through the choices.

## `sandman run`

Run an AFK agent for selected GitHub issues.

```bash
sandman run [issue...] [flags]
```

### Issue selection modes

Issue-driven runs require at least one selection mode. `--prompt` and `--template` can enter prompt-only mode only when no issue selection is provided and the final prompt omits issue placeholders.

| Mode | Example | Description |
|------|---------|-------------|
| Explicit numbers | `sandman run 42 43` | One or more issue numbers |
| Issue range | `sandman run 42:45` | Range expression `start:end` (inclusive). `:45` starts from 1, `42:` is unbounded end |
| `--label` | `sandman run --label ready-for-agent` | All open issues with the given label |
| `--query` | `sandman run --query "label:bug is:open"` | GitHub search query |
| `--ralph` | `sandman run --ralph 3` | N lowest-numbered open issues labeled `ready-for-agent` |
| Interactive picker | `sandman run` (in a TTY) | Opens a numbered list of open issues to select from |

Positional arguments (numbers and ranges) can be combined with `--label` and `--query`; Sandman resolves finite issue selections locally and uses search only when it needs to expand open-ended ranges or evaluate a query that cannot be matched locally.

### Execution flags

| Flag | Default | Description |
|------|---------|-------------|
| `--parallel` | `default_parallel` from config (4) | Maximum concurrent agent runs; `0` falls back to `default_parallel` |
| `--start-delay` | config `start_delay` (0) | Wait this many seconds after any `AgentRun` finishes before starting the next one; `0` disables pacing |
| `--sandbox` | config default (`podman`) | Sandbox mode: `podman`, `docker`, or `worktree` |
| `--base-branch` | config `git.base_branch` (`main`) | Base branch to fetch from origin before each `AgentRun` starts |
| `--container-capacity` | config default (4) | Max concurrent agent runs per container; `0` = unlimited, `1` = one agent per container |
| `--max-containers` | config default (0) | Max containers; `0` = no cap (unbounded pool growth) |
| `--retries` | `0` | Number of times to retry a failed run; `--ralph` sets this to `3` silently |
| `--force` | `false` | Clear artifacts before running (deletes prior worktree and logs for the issue) |
| `--dangerously-skip-permissions` | `true` for container runs, `false` for worktree runs | Skip permission checks for agent runs |
| `--include-dependencies` | `false` | Auto-expand batch with transitive blockers |
| `--label` | — | Select issues by label |
| `--query` | — | Select issues by GitHub search query |
| `--ralph` | — | Select N lowest-numbered open `ready-for-agent` issues |
| `--prompt` | — | Inline prompt template (overrides file-based templates) |
| `--template` | — | Path to prompt template file |
| `--prompt-arg` | — | Custom template substitution (`KEY=VALUE`, repeatable) |
| `--model` | `default_model` from config | Override the model passed to the agent for built-in presets |
| `--agent` | `default_agent` from config (`opencode`) | Built-in agent preset for this run |

### Flag interactions

- `--ralph` is mutually exclusive with issue arguments, `--label`, and `--query`
- `--ralph N` silently sets `--retries=3`; the run summary shows "(3 retries)" on failure
- Positional arguments (numbers and ranges) can be combined with `--label` or `--query` — finite selections are resolved locally; open-ended ranges and unsupported queries still use GitHub search
- If `--prompt` or `--template` is used with no issue arguments, `--label`, `--query`, or `--ralph`, and the final selected prompt omits `{{ISSUE_NUMBER}}`, `{{ISSUE_TITLE}}`, and `{{ISSUE_BODY}}`, Sandman enters prompt-only mode and skips GitHub issue lookup
- If any issue selection is provided, Sandman stays in issue-driven mode even when `--prompt` or `--template` is set
- `run` preserves worktrees by default; use `sandman clean` to delete them
- `--parallel` limits total concurrent `AgentRun`s across all sandboxes
- `--start-delay` is batch-local pacing; it waits after any `AgentRun` finishes before the next start, and `0` disables the delay
- `--base-branch` controls which branch Sandman fetches from origin before each `AgentRun` starts and which branch new worktrees are cut from
- `--container-capacity` limits concurrent `AgentRun`s per `ContainerSandbox`
- `--container-capacity` accepts `0` as unlimited mode (no per-container cap)
- `--max-containers` caps the number of `ContainerSandbox` instances; `0` means no cap (unbounded pool growth)
- `--model` only applies to built-in presets; if omitted, Sandman uses `default_model` from config, falling back to the agent provider's configured model
- `--agent` selects which built-in preset to use for this run; if omitted, Sandman uses `default_agent` from config
- Pi splits `provider/model` into separate provider and model flags, and errors if `/` is missing
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

## `sandman continue`

Continue the last agent run for a given issue with a fresh prompt plus prior continuation context.

```bash
sandman continue <issue-number> <prompt-text>
```

Reuses the previously created branch and recorded agent and review command from the prior run, though `--agent` can override and `--model` falls back to `default_model` from config when omitted. It also replays the stored base branch from the prior run for prompt rendering and event metadata only, ignoring current base-branch config changes. Then it prepends `.sandman/continuation-context.md` to `.sandman/continue-prompt.md` when present.

| Flag | Default | Description |
|------|---------|-------------|
| `--model` | `default_model` from config | Override the model for the continued run |
| `--agent` | prior run's agent | Override the agent preset for the continued run |
| `--dangerously-skip-permissions` | `true` for container runs, `false` for worktree runs | Skip permission checks for the continued run |

## `sandman clean`

Clean up sandbox resources and stale worktrees.

```bash
sandman clean [flags]
```

| Flag | Description |
|------|-------------|
| `--all` | Remove all worktrees and logs |
| `--success` | Remove worktrees and logs for successful runs only |
| `--failed` | Remove worktrees and logs for failed and cancelled runs (runs with `status: failure`) |

Exactly one flag is required.

## `sandman attach`

Attach to a running sandman daemon and stream its output.

```bash
sandman attach
```

If no daemon is running (no socket under `.sandman/runs/` exists), prints a clear error. Otherwise connects to the daemon's control socket and reads raw bytes to stdout until the socket closes (EOF).

Useful for monitoring a long-running batch from a separate terminal.

## `sandman portal`

Serve a local browser portal for the current repository's Sandman instances and launcher presets.

```bash
sandman portal [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--port` | `5000` | Port to bind on `0.0.0.0` |

The portal is repo-scoped: it scans the current repository's `.sandman/runs/` tree on each poll and shows every live Sandman instance it finds there, plus run status and logs from the event and log files. It also exposes a launcher for repo-scoped command presets.

Use it when you want a browser view of multiple runs in the same repo and a launcher for common Sandman commands.

## `sandman config`

Manage Sandman configuration via dot-notation keys.

```bash
sandman config get <key>
sandman config set <key> <value>
```

### Supported keys

| Key | Type | Example |
|-----|------|---------|
| `default_agent` | string | `opencode` |
| `default_model` | string | `opencode/deepseek-v4-flash-free` |
| `build_tools` | string | `node` |
| `default_parallel` | int | `4` |
| `start_delay` | int | `0` |
| `review_command` | string | `/oc review` |
| `container_capacity` | int | `4` |
| `max_containers` | int | `0` |
| `worktree_dir` | string | `.sandman/worktrees` |
| `sandbox` | string | `podman` |
| `git.base_branch` | string | `main` |

`sandman config set review_command ...` also re-syncs the shared `sandman` skill tree. If local edits are detected under `~/.agents/skills/sandman/`, Sandman prompts before overwriting in a TTY and fails in non-interactive mode.

Agent commits use your host Git identity, not Sandman config keys. Sandman resolves `user.name` and `user.email` from `~/.gitconfig`, then host global/XDG config, then repo-local `.git/config`.
