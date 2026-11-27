# Configuration

Sandman reads configuration from `.sandman/config.yaml` in the project root. You can also read and write individual fields via `sandman config get/set`.

## Full schema

```yaml
# Default built-in agent preset used by `sandman run` when `--agent` is omitted
# (opencode or claude).
agent: opencode

# Default model passed to the agent when `--model` is omitted. It applies only
# to agents that use the same preset as `agent`; another preset uses its own
# configured model or its preset default (opencode/big-pickle, sonnet).
# Falls back to the agent provider's configured model if empty; with nothing
# configured, claude runs on sonnet and opencode on its own CLI default.
model: opencode/big-pickle

# Optional implementation model variant. The Sandman CLI always uses --variant;
# Sandman translates it to each built-in agent's native model/variant syntax.
# Empty disables the configured variant. OpenCode v2 uses its preset model when
# a variant is set without a resolved model.
variant: ""

# Default agent preset used for delegated review runs when `--agent` is omitted
# in `sandman review`. Blind review uses the harness's default model instead.
review_agent: opencode

# Default model passed to the delegated review agent when `--model` is omitted
# in `sandman review`. Defaults to the review agent's preset default model when
# unset. Blind review does not use it.
review_model: opencode/big-pickle

# Optional provider-specific delegated review model variant. Empty disables
# forwarding it to the built-in review command. Blind review does not use it.
review_variant: ""

# Build tools preset for the container image (generic, dotnet, go, node, python, elixir, ruby, rust, java).
build_tools: generic

# Review command rendered into each Run's Task, and read by the shared skills
# when you use them directly. Empty (or absent/whitespace-only) selects blind
# review: the implementor spawns a fresh, context-free subagent and publishes
# its decision. Set a command, such as /sandman review, to delegate to an
# external reviewer. A command containing /sandman requires `sandman review` to
# be running before `sandman run`/`continue` will start. The daemon answers
# any comment that starts with /sandman, so /sandman <anything> works too.
review_command: ""

# Maximum number of concurrent agent runs.
parallel: 1

# Maximum concurrent review-daemon runs.
parallel_reviews: 1

# Idle timeout in seconds for agent runs. When the agent produces no new log
# output for this duration, the heartbeat watchdog aborts the run.
# 0 disables the watchdog.
# Default: 3600 (60 minutes).
run_idle_timeout: 3600

# Cumulative delegated review response budget in seconds for the implementor
# side of the PR-review loop. The minimum is 240 seconds; default: 1800.
review_timeout: 1800

# Number of times to retry a failed AgentRun before recording it as failed.
# 0 disables retries.
# Default: 3.
retries: 3

# Additional literal OpenCode context-limit error phrases. Phrases are trimmed,
# lowercased, and matched case-insensitively alongside Sandman's built-in list.
# Empty or duplicate normalized phrases are rejected.
context_error_phrases:
  - provider context exhausted

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

# Remove successful implementation worktrees after terminal cleanup.
# Review worktrees and unsuccessful runs are always preserved.
# Default: true.
cleanup_worktrees: true

# Compact a continuation Task larger than this many bytes.
# 0 disables automatic compaction; otherwise at least 32768.
# Default: 49152 (suits agents with a context window of about 250k tokens).
task_compact_bytes: 49152

# Git configuration for branch management.
git:
  # Branch new runs start from and pull requests target. `sandman init`
  # records the repository's default branch (origin/HEAD), else main;
  # `sandman init --base-branch <branch>` records another one.
  base_branch: main

# Sandman installs the default built-in agent in scaffolded Dockerfiles and mounts the shared skills directory.
```

## Built-in presets

Sandman has three built-in presets: `opencode` (OpenCode v1), `opencode-v2`, and `claude`. New `sandman init` projects default to `opencode-v2`; reinitialization preserves an existing agent selection. The runtime fallback for configs without an agent remains `opencode` for compatibility. `sandman init --agent <preset>` installs the chosen preset into the scaffolded Dockerfile and writes it as both `agent` and `review_agent`, with the preset's default model as `model` and `review_model`.

When you use the `opencode` preset, install the `opencode-shell-strategy` plugin first. Sandman runs OpenCode without a TTY/PTY, so this plugin prevents interactive shell commands from hanging during runs. OpenCode subagents inherit the same instructions.

The `claude` preset (experimental in v1) runs the unmodified Claude Code CLI in print mode and is how you use a Claude subscription with Sandman. See [Agent Compatibility](agent-compatibility.md#claude-code) for authentication, permissions, and its limitations.

A `claude-agent/<model>` model, OpenCode v2's provider that starts the Claude Code CLI, is accepted only on the `claude` agent: on any other agent every launch would fail, because only the `claude` preset installs and signs in Claude Code. Sandman refuses the pairing in `model` with `agent`, `review_model` with `review_agent`, and `agents.<name>.model`, and in the `--agent` / `--model` flags of `run`, `review` and `init` (and `init --review-model`); see [Agent Compatibility > Claude models through OpenCode](agent-compatibility.md#claude-models-through-opencode). A config that already holds such a pairing fails to load, so every command that reads it stops, `sandman config set`, `sandman init` and a running `sandman review` daemon's next batch included: edit `.sandman/config.yaml` by hand, for example to `agent: claude` and `model: sonnet`.

For container runs on a subscription, export `CLAUDE_CODE_OAUTH_TOKEN` from `claude setup-token` in the shell that runs Sandman rather than adding it to `env`: the preset passes it to containers by name only, so the value stays off command lines and out of `.sandman/`. See [Agent Compatibility > Authentication](agent-compatibility.md#authentication).

Both built-in presets also see `~/.agents`, which is where Sandman installs the shared skill folder.

`sandman run --agent` selects the agent preset per invocation (built-in `opencode`, `opencode-v2`, or `claude`, or a custom provider under `agents`). `sandman config set agent` changes the project default. A container run whose agent is not listed in the Dockerfile's `# sandman installed-agents:` header fails before any container starts; a run on an agent other than the default does not depend on the default agent's metadata.

Custom providers are defined under the top-level `agents` map in `.sandman/config.yaml`:

```yaml
agents:
  my-agent:
    command: my-agent --prompt {{.PromptFile}}
    model: my-agent/model-name
    env:
      MY_AGENT_API_KEY: xxx
    config_dirs: ["~/.config/my-agent"]
    config_files: ["~/.my-agent/config.json"]
```

A command receives the prompt's path as `{{.PromptFile}}`. To hand an agent the
prompt text, redirect it on stdin (`my-agent < "{{.PromptFile}}"`): Linux limits
one argument to 128 KiB. Setting `command` for a built-in preset replaces what
Sandman does around the launch; see
[Agent Compatibility](agent-compatibility.md).

Each entry may set `command`, `model`, `preset`, `env`, `config_dirs`, `config_files`, `keychain_auth`, and `permission_mode`. `permission_mode` applies only to presets whose command line takes a permission mode (`claude`); see [Agent Compatibility > Permissions](agent-compatibility.md#permissions). When `preset` is set, the custom entry inherits the built-in preset's defaults before applying overrides. See `internal/config/config.go:Agents` and `ResolveAgentProvider`.

Session reuse is a run-time choice rather than a configuration
default. `sandman run --continue` starts a fresh agent conversation;
`sandman run --continue --reuse-session` selects the prior conversation for
each continued row. Runtime-owned re-entry after an external wait selects
reuse automatically. OpenCode resumes the prior Run's exact session, and
missing sessions use one `--continue` fallback only for these selected reuse
launches. Claude Code resumes the worktree's most recent conversation with its
own `--continue`.

### Base branch

`git.base_branch` is the repository's base branch: every Run starts from it and
opens its pull request against it, and the "already resolved" check compares
against it. Set it once per repository, with `sandman init --base-branch <branch>`
or `sandman config set git.base_branch <branch>`; without either, `sandman init`
records the branch `origin/HEAD` points to, else `main`.

Use `sandman run --base-branch` to override `git.base_branch` for a single invocation.
Before each run, Sandman fetches `origin/<base>`, and new run worktrees start from
that fetched remote-tracking branch. If the local base branch is checked out in
one clean worktree, Sandman fast-forwards it with `git merge --ff-only`. A dirty
or multiply checked-out base, or a base that is ahead of or diverged from origin,
is left unchanged with a warning; Sandman never rewrites a checked-out base or
resets a diverged local branch to origin.

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
| `{{REVIEW_COMMAND}}` | Review command from project config; empty selects blind review |
| `{{REVIEW_TIMEOUT}}` | Effective delegated review response budget in seconds; not used by blind review |

Custom keys can be passed at runtime using the `--prompt-arg KEY=VALUE` flag on `sandman run` and referenced as `{{KEY}}` in the template.

See [Sandman Skills](skills.md) for the shared workflow details.

The default task prompt is managed-run specific: it loads
`sandman-review-request` for one stateless trigger and leaves observation,
waiting, resume, and terminal decisions to the runtime. Direct standalone
agent use should load `sandman-run`, which composes implementation,
`sandman-review-cycle`, and merge. Standalone cycle state is in memory only;
after a restart it starts again from live pull-request state and does not trust
managed lifecycle artifacts.

`sandman run --continue` uses current CLI flags / config defaults for tunables (agent, model, parallel, retries, sandbox, container tunables, review command, review timeout, etc.) and replays only the worktree identity from the prior run: the preserved branch, the stored base branch (because the worktree was cut from it), the prior run id, the `.sandman/task.md` contents, and the issue mode. Before replay, Sandman places one canonical continuation freshness guard after all persisted task state so blockers, next actions, and the delegated review timeout must be revalidated. CLI overrides on the `--continue` invocation still win over both config defaults and stored values. When no task file exists, an empty task template is used with a warning on stderr.

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

`start_delay` is batch-local pacing behavior. It applies across sandbox modes, starts only after the first run completes, and does not change container capacity or max container scheduling. Ready awaited rows remain subject to this delay; they are prioritized only after a start is permitted.

## Idle timeout

| Key | Default | Description |
|-----|---------|-------------|
| `run_idle_timeout` | `3600` | Seconds of inactivity before the heartbeat watchdog aborts the run. `0` disables the watchdog |

`run_idle_timeout` detects a stalled agent and records `run.idle_timeout` before ending its attempt. Supported OpenCode/Claude resettable usage limits instead enter waiting and release capacity. When the selected strategy supplies a trustworthy provider reset time, implementation recovery schedules near that reset; absent or untrusted evidence uses ten-minute fallback polling. The five-hour allowance counts only scheduled wait duration after a wait returns successfully, capped by the remaining allowance; interruption/cancellation, capacity delay, and process downtime consume none. The due time and completed accounting survive re-entry/restart, and the supported conversation is reused. After the final boundary probe, configured ordinary fresh-session retries remain available. Missing or invalid accounting disables further quota waits while permitting bounded execution. The expected exhaustion estimate is diagnostic, not an admission or ownership deadline. The review daemon retains a separate fixed ten-minute quota schedule. Recovery reopens batch admission; a failed quota owner retires only its own pause. The watchdog is independent of this allowance; `--run-idle-timeout` overrides it for one invocation. See the [state-machine contract](../architecture/run-state-machine.md).

## Worktree cleanup

| Key | Default | Description |
|-----|---------|-------------|
| `cleanup_worktrees` | `true` | Remove successful implementation worktrees after completion; `false` preserves them |

This setting only controls successful implementation runs. Review worktrees and unsuccessful or blocked runs remain available for inspection regardless of this setting.

## Task compaction

| Key | Default | Description |
|-----|---------|-------------|
| `task_compact_bytes` | `49152` | Size in bytes above which an implementation launch compacts its continuation Task. `0` disables automatic compaction; other values must be at least `32768` |

Every continuation, retry and resume sends the Task (`.sandman/task.md`) back to the agent, so a Task that keeps growing costs tokens on every launch. Above the threshold, Sandman first writes the complete Task to `.sandman/task-history/` in the worktree. It then moves the oldest sections the agent added, keeping the newest ones that fit three quarters of the threshold, and cuts `## Next Step` to its first 4 KiB. The template and issue sections, every plan, status lines and Sandman's own sections are kept. A `## Sandman Task History` section names the archive, and the run log records the sizes. Review runs and other prompt-only runs are never compacted.

Task text costs about 4.4 bytes per token, so the default is about 11k tokens, 4.5% of a 250k-token context window. As a guide, use about 0.2 bytes per token of context window:

| Context window | `task_compact_bytes` |
|---|---|
| 200k | `40960` |
| 250k | `49152` |
| 400k | `81920` |
| 1M | `98304` |

To compact one Task now, whatever its size, continue with `sandman run --continue --compact <issue>`.

## Context rollover phrases

| Key | Default | Description |
|-----|---------|-------------|
| `context_error_phrases` | empty | Additional literal phrases that identify an OpenCode context-limit error |

Use this list when an OpenCode provider introduces new context-exhaustion wording. Entries are global additions for the built-in `opencode` AgentPreset, are trimmed and lowercased during configuration load, and match case-insensitively alongside built-in phrases. Empty entries and duplicates after normalization are rejected. These phrases never apply to other AgentPresets and do not override rate-limit, throttling, or service-unavailable exclusions.

## Review command

`review_command` selects the pull-request review path for every Run and for
direct standalone skill use:

| Value | Selected path |
|-------|---------------|
| absent, empty, or whitespace-only | **Blind review** — the implementor spawns a fresh, context-free subagent and publishes its decision |
| non-empty command | **Delegated review** — the implementor posts that command as a trigger and the external reviewer answers |

An absent key and an explicitly empty value both select blind review;
`review_command: ""` is a real selection, not a missing value. That state
round-trips through configuration, `config get`, the scaffold, the Run's Task,
and `run.started` / `run.continued` events. Setting it to an empty string
(`sandman config set review_command ""`) selects blind review explicitly.

Blind review uses the harness's default model, not `review_agent`,
`review_model`, or `review_variant`. Those values configure the review daemon
and delegated review runs. `review_timeout` and its polling plan also apply only
to delegated review. See [Reviews](reviews.md#blind-review-default).

## Delegated review timeout

| Key | Default | Description |
|-----|---------|-------------|
| `review_timeout` | `1800` | Response budget in seconds for one confirmed delegated PR review request. Values below `240` are rejected |

`review_timeout` governs only the implementor-side delegated review response budget. The budget starts when the trigger is confirmed. For a managed request serviced by the Sandman review daemon, recorded reviewer quota pauses do not count; standalone and other delegated reviewers keep a wall-clock deadline. A later confirmed trigger receives a fresh budget, while retries and continuations of the same trigger keep the remaining time. The setting does not change reviewer execution time, CI deadlines, implementation quota, idle timeout, or the review-pass limit. `--review-timeout` overrides the project value for one run or continuation. The effective value is rendered into the current Task and recorded in `run.started` / `run.continued` payloads. Blind review has no timeout or polling window; a blind round is synchronous implementor-owned work.

Delegated review and current-head CI waits retain logical row ownership but
release the execution slot between observations. Review uses its confirmed
request deadline; CI uses an independent durable 30-minute deadline per identified
current-head check execution. A new head or verified same-head rerun starts a new
CI generation; observing or restarting the same execution preserves its deadline.
Dependents remain held
while the prerequisite awaits external progress; a deadline expiry, CI failure,
or merge conflict resumes remediation work. Implementation recovery retains
three in-session lifecycle relaunches in addition to entry and configured ordinary
retries; old cumulative repair ledgers cannot prevent a fresh executor from acting.
Standalone CI instead permits 60 minutes and three fixes per current head within
one invocation, resetting on a new head or fresh invocation. Reviewer launch
failures retain backoff (up to ten minutes) and publication recovery without a
three-launch gate;
the ten-pass review loop and configured request deadline retain their bounds.

The request deadline belongs to one confirmed trigger. A feedback resume can
renew review with a new trigger and deadline, while a continuation of the same
trigger keeps its original deadline across restart. Invalid or mismatched
evidence cannot authorize merge; the canonical registration is the runtime's
source for the current request and head.

## CLI config commands

Use `sandman config get` and `sandman config set` to read and write individual fields:

```bash
sandman config get parallel
sandman config get parallel_reviews
sandman config set parallel_reviews 8
sandman config set container_capacity 2
sandman config set start_delay 5
sandman config set run_idle_timeout 3600
sandman config set review_timeout 3600
sandman config set review_command ""
sandman config set model opencode/BigPickle
sandman config set review_variant "provider/foo bar"
sandman config set git.base_branch main
```

Use `sandman config get` and `sandman config set` for top-level config keys.

Changing `review_command`, `review_timeout`, or `git.base_branch` with `sandman config set` only updates this file. The shared `~/.agents/skills/sandman/` tree is the same for every repository and carries none of these values; each Run's Task supplies them, and the skills read them from this file when you use them directly. See [Sandman Skills](skills.md#repository-and-run-values). A `review_command` set to an empty string selects blind review; it is not treated as unset.

Sandman does not store a separate commit identity in project config. Agent commits use your host Git identity resolved in this order:

1. `~/.gitconfig`
2. Host global/XDG Git config such as `~/.config/git/config`
3. Repo-local `.git/config`

If `user.name` or `user.email` is still missing after that lookup, `sandman run` fails before the agent starts.

See [Commands Reference](commands.md) for the full list of supported dot-notation keys.

For build-tool preset detection and language version selectors, see [Scaffolding and Supported Languages](scaffolding.md).
