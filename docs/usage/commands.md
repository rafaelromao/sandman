# Commands Reference

## `sandman init`

Scaffolds `.sandman/` configuration files in the current directory.

`sandman init` also syncs the shared `sandman` skill folder into `~/.agents/skills/sandman/` using the configured review command.

```bash
sandman init [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--build-tools` | `""` | Build tools preset (`generic`, `dotnet`, `go`, `node`, `python`, `rust`, `elixir`, `ruby`, `java`) |
| `--tool-version` | `""` | Version selector (`latest`, `lts`, `repo`, or semver shorthand) |
| `--agent` | `""` | Default built-in agent preset for `init` (`opencode`) |
| `--model` | `""` | Default model for the agent |
| `--parallel` | `-1` | Default parallel container count (`-1` = use config default 1) |
| `--review-command` | `""` | Review command stored as `review_command` in project config; defaults to `/sandman review` (requires `sandman review` to be running) |
| `--retries` | `-1` | Persist `retries` in scaffolded config. `-1` keeps the built-in default of `3`; `0` disables retries |
| `--parallel-reviews` | `-1` | Persist `parallel_reviews` in scaffolded config (default `1`) |
| `--run-idle-timeout` | `-1` | Persist `run_idle_timeout` (seconds) in scaffolded config. `-1` keeps the built-in default of `1800`; `0` disables the heartbeat watchdog |

When `--tool-version` is omitted, `init` uses the preset resolver's interactive defaults: repo hints are offered when present, otherwise `latest`/`lts` choices are prompted.

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
| `--auto` | `sandman run --auto --count 3` | Auto Mode — Sandman selects up to `--count` issues from the candidate pool, or the `auto_max_count` config default if no count is given |
| Interactive picker | `sandman run` (in a TTY) | Opens a numbered list of open issues to select from |

Positional arguments (numbers and ranges) can be combined with `--label` and `--query`; Sandman resolves finite issue selections locally and uses search only when it needs to expand open-ended ranges or evaluate a query that cannot be matched locally.

### Execution flags

| Flag | Default | Description |
|------|---------|-------------|
| `--parallel` | `parallel` from config (1) | Maximum concurrent agent runs; `0` falls back to `parallel` |
| `--start-delay` | config `start_delay` (0) | Wait this many seconds after any `AgentRun` finishes before starting the next one; `0` disables pacing |
| `--sandbox` | config default (`podman`) | Sandbox mode: `podman`, `docker`, or `worktree` |
| `--base-branch` | config `git.base_branch` (`main`) | Base branch to fetch from origin before each `AgentRun` starts |
| `--container-capacity` | config default (4) | Max concurrent agent runs per container; `0` = unlimited, `1` = one agent per container |
| `--max-containers` | config default (0) | Max containers; `0` = no cap (unbounded pool growth) |
| `--retries` | `0` | Number of times to retry a failed run; `--auto` sets this to `3` silently |
| `--override` | `false` | Clear artifacts before running (deletes prior worktree, logs, and events; force-checkout worktree to expected branch on mismatch or detached HEAD) |
| `--continue` | `false` | Continue the latest AgentRun for selected issues by reusing the stored task file and prior run settings |
| `--dangerously-skip-permissions` | `true` for container runs, `false` for worktree runs | Skip permission checks for agent runs |
| `--include-dependencies` | `false` | Auto-expand batch with transitive blockers |
| `--label` | — | Select issues by label |
| `--query` | — | Select issues by GitHub search query |
| `--auto` | — | Auto Mode — let Sandman choose which issues to run, capped to `--count` or `auto_max_count` from config |
| `--count` | `0` | Candidate cap for Auto Mode; `0` means unlimited when `--auto` is set |
| `--prompt` | — | Inline prompt template (overrides file-based templates) |
| `--template` | — | Path to prompt template file |
| `--prompt-arg` | — | Custom template substitution (`KEY=VALUE`, repeatable) |
| `--model` | `model` from config | Override the model passed to the agent for built-in presets |
| `--agent` | `agent` from config (`opencode`) | Built-in agent preset for this run |
| `--run-id` | — | Batch-level identifier for prompt-only runs; must start with a letter and contain only alphanumeric characters, hyphens, and underscores; cannot be combined with issue selection |
| `--run-idle-timeout` | `0` | Treat an AgentRun as stuck if it produces no output for N seconds; `0` disables the timeout |
| `--branch` | `""` | Branch name for prompt-only runs; overrides the default `sandman/<slug>-<timestamp>` shape (prompt-only mode only) |
| `--reconcile-stranded` | `true` | Auto-recover stranded worktrees when the main repo is checked out on a `sandman/N-…` branch (see ADR-0027) |
| `--no-reconcile-stranded` | `false` | Opt out of stranded-worktree auto-recovery (negative form of `--reconcile-stranded`) |

### Flag interactions

- `--auto` accepts the same filters as regular Sandman runs (`--label`, `--query`, or explicit issue args). It is mutually exclusive with combining `--label` and `--query` together (the same restriction regular runs have)
- `--auto` silently sets `--retries=3` and applies conservative defaults (`--parallel=1`, `--container-capacity=1`, `--max-containers=1`) when those flags are not provided; the run summary shows "(3 retries)" on failure
- `--count N` (or `auto_max_count: N` in config) caps the candidate pool to `N`; `0` means unlimited
- Positional arguments (numbers and ranges) can be combined with `--label` or `--query` — finite selections are resolved locally; open-ended ranges and unsupported queries still use GitHub search
- If `--prompt` or `--template` is used with no issue arguments, `--label`, `--query`, or `--auto`, and the final selected prompt omits `{{ISSUE_NUMBER}}`, `{{ISSUE_TITLE}}`, and `{{ISSUE_BODY}}`, Sandman enters prompt-only mode and skips GitHub issue lookup
- If any issue selection is provided, Sandman stays in issue-driven mode even when `--prompt` or `--template` is set
- `run` preserves worktrees by default; use `sandman clean` to delete them
- `--parallel` limits total concurrent `AgentRun`s across all sandboxes
- `--start-delay` is batch-local pacing; it waits after any `AgentRun` finishes before the next start, and `0` disables the delay
- `--base-branch` controls which branch Sandman fetches from origin before each `AgentRun` starts and which branch new worktrees are cut from
- `--container-capacity` limits concurrent `AgentRun`s per `ContainerSandbox`
- `--container-capacity` accepts `0` as unlimited mode (no per-container cap)
- `--max-containers` caps the number of `ContainerSandbox` instances; `0` means no cap (unbounded pool growth)
- `--model` only applies to built-in presets; if omitted, Sandman uses `model` from config, falling back to the agent provider's configured model
- `--agent` selects which built-in preset to use for this run; if omitted, Sandman uses `agent` from config
- `--continue` cannot be combined with `--override`
- When `--max-containers` and `--container-capacity` together constrain concurrency below `--parallel`, the tighter limit wins
- `--reconcile-stranded` auto-recovers stranded worktrees when the main repo is checked out on a `sandman/N-…` branch (ADR-0027); `--no-reconcile-stranded` opts out of this auto-recovery

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

## `sandman run --continue`

Continue the last agent run for one or more issues. Reads the task file (`.sandman/task.md`) from each issue's worktree and passes it verbatim as the agent's resume prompt.

```bash
sandman run --continue <issue-number>...
```

Reuses the previously created branch and recorded agent and review command from the prior run, though `--agent` can override and `--model` falls back to `model` from config when omitted. It also replays the stored base branch from the prior run for prompt rendering and event metadata only, ignoring current base-branch config changes. When no task file exists, an empty task template is used as the resume prompt (with a warning on stderr).

| Flag | Default | Description |
|------|---------|-------------|
| `--model` | `model` from config | Override the model for the continued run |
| `--agent` | prior run's agent | Override the agent preset for the continued run |
| `--run-id` | — | Continue the most recent prompt-only run by its batch-level identifier; must start with a letter and contain only alphanumeric characters, hyphens, and underscores; cannot be combined with issue numbers. Reads the prior task file from the existing worktree and reuses the same branch for the continued run. When the most recent Issue-0 event is a review run (not a prompt-only run), `sandman run --continue` skips it and selects the prior prompt-only run instead — or errors if none exists. |
| `--dangerously-skip-permissions` | `true` for container runs, `false` for worktree runs | Skip permission checks for the continued run |
| `--run-idle-timeout` | `0` | Treat an AgentRun as stuck if it produces no output for N seconds; `0` disables the timeout |

## `sandman clean`

Clean up sandbox resources and stale worktrees.

```bash
sandman clean [flags]
```

| Flag | Description |
|------|-------------|
| `--archived` | Remove archived batches |
| `--dry-run` | Print intended deletions without performing I/O |
| `--stale` | Recover stale runs in dead batches by emitting `run.aborted` events |
| `--orphaned` | Remove orphaned test batch directories (no matching run.started event and no live daemon socket) |

`--stale` is mutually exclusive with `--archived`, `--dry-run`, and `--orphaned`. `--orphaned` is mutually exclusive with `--archived` and `--stale`. `--archived` and `--dry-run` can be combined.

## `sandman archive`

Archive completed run directories. Every subcommand is per-row aware. `archive run` and `archive batch` are distinct subcommands with different contracts.

```bash
sandman archive run <runId>
sandman archive batch <batchId>
sandman archive older-than <days>
sandman archive stale
```

| Subcommand | Description |
|------------|-------------|
| `run <runId>` | Move `runs/<runId>/` from `.sandman/batches/<batchId>/` to `.sandman/archive/<batchId>/runs/<runId>/`. The targeted row's `run.json.Status` must be terminal; sibling rows and the batch daemon stay untouched. Persists an `archivePath` recovery record for crash recovery. The HTTP `POST /api/runs/archive` endpoint shares this per-row contract. |
| `batch <batchId>` | Move the whole batch directory from `.sandman/batches/<batchId>/` to `.sandman/archive/<batchId>/`. The batch daemon must be gone; sibling rows are not applicable. Flips the entry-level `status` to `archived`. CLI-only — not exposed via HTTP. |
| `older-than <days>` | Walk every `run.json` across all batches and archive each terminal row older than the cutoff. Already-archived rows are skipped. Sibling rows and live batch daemons stay untouched. `<days>` must be a non-negative integer; `0` archives every dead batch. |
| `stale` | Chain the same status-fix logic as `clean --stale` (emit `run.aborted` for unterminated runs in dead batches), then walk every `run.json` and archive each terminal row. Live batches are skipped entirely.

See the [Archive section in `monitoring.md`](monitoring.md#archive) for detailed semantics including the per-row `Runs[]` record and the whole-batch archive invariant.

## `sandman attach`

Attach to a running sandman daemon and stream its output.

```bash
sandman attach
```

If no daemon is running (no socket under `.sandman/batches/` exists), prints a clear error. Otherwise connects to the daemon's control socket and reads raw bytes to stdout until the socket closes (EOF).

Useful for monitoring a long-running batch from a separate terminal.

## `sandman portal`

Serve a local browser portal for the current repository's Sandman instances.

```bash
sandman portal [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--port` | `5000` | Port to bind on the chosen host |
| `--host` | `127.0.0.1` (or `SANDMAN_PORTAL_HOST` if set) | Host/interface to bind on. Use `0.0.0.0` to expose on all interfaces. |

The portal is repo-scoped: it scans the current repository's `.sandman/batches/` tree on each poll and shows every live Sandman instance it finds there, plus run status and logs from the event and log files.

By default the server binds to `127.0.0.1` so it is not reachable from other machines. Pass `--host` (or set `SANDMAN_PORTAL_HOST`) to bind on a different interface, for example `0.0.0.0` to expose the portal on all interfaces.

Use it when you want a browser view of multiple runs in the same repo.

## `sandman review`

Run a Sandman agent to review a pull request.

```bash
sandman review [pr-numbers...]
```

When one or more PR numbers are given as positional arguments, posts a single review comment for each PR and exits. With no arguments, starts the review daemon that polls open PRs every 60s for `/sandman review` comments and launches review agents.

Examples:
```bash
sandman review 42
sandman review 42 43
sandman review 42:45
sandman review 42:
sandman review :45
sandman review 42 --agent opencode --model opencode/big-pickle
```

The daemon's review path is **daemon-as-poster**: the reviewer agent writes its body to `<runDir>/decision.md`, and the daemon reads the file, runs it through the `RedactBody` redactor (S1, `internal/review/redactor.go`) which applies the regex `(?i)/sandman` → `sandman` to strip every leading-slash `sandman` substring, and posts the redacted body via `gh pr comment`. The redactor is the load-bearing safety net for the no-self-loop invariant — it runs out-of-band of the LLM, so the bot's body can never contain the trigger substring regardless of what the prompt rule says. The `processPR` self-defence sniff `LooksLikeBotReviewBody` survives as a belt-and-braces backstop: a body that structurally looks like a previous bot review (carries the `## Previous review progress` markdown heading AND the literal `/sandman review` trigger substring) is dropped before `ParseTrigger` runs — no batch run, no eyes reaction. The redactor is the primary defence; the sniff is defence-in-depth. See [ADR-0014 §Daemon-side redaction](../adr/0014-sandman-review-daemon-and-guard.md#daemon-side-redaction) for the full rationale.

| Flag | Default | Description |
|------|---------|-------------|
| `--parallel` | `0` | Override parallel_reviews for this run; `0` uses the configured value |
| `--container-capacity` | `0` | Maximum concurrent agent runs per container; `0` means unlimited |
| `--max-containers` | `0` | Maximum number of containers to run at once; `0` means no cap (unbounded pool) |
| `--agent` | `""` | Override `default_review_agent` for this run |
| `--model` | `""` | Override `default_review_model` for this run |
| `--sandbox` | `"worktree"` | Sandbox mode for the review run |

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
| `model` | string | `opencode/big-pickle` |
| `review_agent` | string | `opencode` |
| `review_model` | string | `opencode/big-pickle` |
| `build_tools` | string | `node` |
| `parallel` | int | `1` |
| `parallel_reviews` | int | `1` |
| `start_delay` | int | `0` |
| `run_idle_timeout` | int | `1800` |
| `retries` | int | `3` |
| `auto_max_count` | int | `50` |
| `review_command` | string | `/sandman review` |
| `container_capacity` | int | `4` |
| `max_containers` | int | `0` |
| `worktree_dir` | string | `.sandman/worktrees` |
| `sandbox` | string | `podman` |
| `git.base_branch` | string | `main` |

`sandman config set review_command ...` also re-syncs the shared `sandman` skill tree. If local edits are detected under `~/.agents/skills/sandman/`, Sandman prompts before overwriting in a TTY and fails in non-interactive mode.

Agent commits use your host Git identity, not Sandman config keys. Sandman resolves `user.name` and `user.email` from `~/.gitconfig`, then host global/XDG config, then repo-local `.git/config`.

## `sandman stranded`

Detect sandman-managed worktrees whose HEAD points to a different branch than their directory name expects.

```bash
sandman stranded [--json]
```

A *stranded worktree* is a sandman-managed worktree whose HEAD points to a different branch than its directory name expects. This can happen when a previous run was interrupted after creating the worktree but before checking out the correct branch.

`sandman stranded` works from the main repo root or from inside any sandman worktree. It reads the configured `worktree_dir` from `.sandman/config.yaml` and resolves the path to match `git worktree list` output correctly, including absolute, tilde-prefixed, and relative `worktree_dir` values.

The command parses `git worktree list --porcelain`, reads the configured `worktree_dir` from `.sandman/config.yaml` (defaults to `.sandman/worktrees`), matches worktrees under that directory whose directory name follows the `sandman/<number>-<slug>` pattern, and compares the actual branch against the expected branch derived from the directory name. For each mismatch it prints a one-line remediation command:

```
Worktree /path/.sandman/worktrees/sandman/724-foo is on refs/heads/main, expected refs/heads/sandman/724-foo. Run: git -C /path/.sandman/worktrees/sandman/724-foo checkout -f sandman/724-foo
```

`sandman stranded` is non-destructive: it never checks out branches or removes worktrees automatically. It exits 0 on success, including when no stranded worktrees are found.

For machine-readable output, pass `--json` to print the structured result list instead of the human-readable remediation lines:

```bash
sandman stranded --json
```

`--reconcile-stranded` (enabled by default) auto-recovers stranded worktrees when the main repo is checked out on a `sandman/N-…` branch (ADR-0027); `--no-reconcile-stranded` opts out.

For details on false positives with prompt-only branches and the `git checkout -f` warning, see [Troubleshooting > Stranded worktrees](#troubleshooting-stranded-worktrees).

## Troubleshooting

### Stranded worktrees

For the full `sandman stranded` command reference, see [`sandman stranded`](#sandman-stranded) above.

### E2E test side effects

Interrupted or failed e2e runs leave behind worktrees, orphaned batch directories, and temp directories that can accumulate and cause subsequent runs to fail (especially in quota-constrained CI and worktree-based sandboxes).

For a full description of what accumulates and how to clean it up, see [Testing > Side effects and cleanup](testing.md#side-effects-and-cleanup).
