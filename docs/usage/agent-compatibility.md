# Agent Compatibility

Sandman includes two built-in presets: `opencode` and `claude`. Claude Code
support is available but experimental in v1; OpenCode remains the default.

## Built-in presets

| Preset | Display Name | Default model | Command Template |
|--------|-------------|---------------|------------------|
| `opencode` | OpenCode | `opencode/big-pickle` | `opencode run --format json ...` |
| `claude` | Claude Code | `sonnet` | `claude -p --output-format stream-json --verbose ...` |

The `opencode` preset uses OpenCode's structured JSON output so Sandman can retain
the first session identity in each Run's `session.json` while rendering text,
tool, and error events into the normal readable output. A runtime-owned re-entry
uses that exact session. An operator must pass `--continue --reuse-session` to
request the same behavior for an ordinary continuation; plain `--continue` is
fresh. Custom agent commands are not rewritten or placed on this state path.

The `claude` preset runs the unmodified Claude Code CLI in print mode. It is the
way to use a Claude Pro, Max, Team, or Enterprise subscription with Sandman; see
[Claude Code](#claude-code).

Each preset's agent-specific behaviour (command flags, session reuse, output
handling, and failure classification) is selected once per launch from the
preset. A custom `command` under a built-in preset keeps that preset's failure
classification and environment rules, but Sandman injects no model, variant, or
session flags into it.

## OpenCode shell strategy

Sandman uses OpenCode in a headless environment, so `opencode` must have the `opencode-shell-strategy` plugin installed before it is used with Sandman. The plugin teaches OpenCode to avoid interactive shell commands that would hang without a TTY/PTY. OpenCode subagents inherit the same instructions.

### Install

```bash
git clone https://github.com/JRedeker/opencode-shell-strategy.git ~/.config/opencode/plugin/shell-strategy
```

Add the instruction file to `~/.config/opencode/opencode.json`:

```json
{
  "instructions": [
    "~/.config/opencode/plugin/shell-strategy/shell_strategy.md"
  ]
}
```

Restart OpenCode after installing it.

### What it prevents

| Command type | Hangs in Sandman | Safer alternative |
|-------------|------------------|--------------------|
| Package manager prompts | `npm init` | `npm init -y` |
| Git commit prompts | `git commit` | `git commit -m "msg"` |
| Merge prompts | `git merge branch` | `git merge --no-edit branch` |
| Interactive editors | `vim`, `nano`, `vi` | Use OpenCode file tools |
| Pagers / REPLs | `less`, `more`, `man`, `python` | Non-interactive flags or direct file tools |

## Claude Code

The `claude` preset renders:

```text
claude -p --output-format stream-json --verbose [--continue] [--dangerously-skip-permissions | --permission-mode '<permission_mode>' | --allowedTools 'Edit(/.sandman/**)' --permission-mode dontAsk] [--name '<session name>'] [--model '<model>'] [--effort '<variant>'] "$(cat .sandman/task.md)"
```

- `stream-json` with `--verbose` is the only print-mode format that streams while
  the agent works, which the idle-timeout heartbeat (`run_idle_timeout`) needs.
- `--continue` is rendered only when Sandman selects session reuse; see
  [Session reuse](#session-reuse).
- `--dangerously-skip-permissions` follows the same default as every agent: on for
  container runs, off for worktree runs; see [Permissions](#permissions).
- `--permission-mode '<permission_mode>'` is rendered when the agent sets
  `permission_mode`, and replaces `--dangerously-skip-permissions`; see
  [Permissions](#permissions).
- `--allowedTools 'Edit(/.sandman/**)' --permission-mode dontAsk` is rendered only
  for the review daemon's [health repairs](reviews.md#health-repair), and never
  together with `--dangerously-skip-permissions` or a configured
  `permission_mode`.
- `--name` carries the Run's session name, which the lingering-process check
  looks for after the run.
- `--model` receives the resolved model and `--effort` receives the model variant
  (`variant`/`review_variant`); Claude Code validates both.

The preset exports `DISABLE_AUTOUPDATER=1`, `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`,
`CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS=0`, and `IS_SANDBOX=1`, and mounts `~/.claude`, `~/.agents`, and `~/.claude.json` into
containers. Transcripts, caches, and logs under `~/.claude` stay out of the
per-batch config snapshot; credentials, settings, skills, agents, commands,
plugins, and `CLAUDE.md` are copied.

Without `CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS=0`, print mode terminates
background tasks (background shell commands, subagents) that are still running
600 seconds after the main turn ends, and logs `Background tasks still running
after 600s; terminating`. Sandman leaves them running: a run whose background
work hangs stops producing output and is ended by `run_idle_timeout`, then takes
the ordinary retry path.

Claude Code reads `AGENTS.md` natively from version 2.1.277; scaffolded images pin
a newer version.

### Install

On the host (worktree runs, and to create auth for containers):

```bash
npm install -g @anthropic-ai/claude-code
```

Then sign in once with `claude` and `/login`. `sandman init --agent claude`
scaffolds a Dockerfile that installs the pinned `@anthropic-ai/claude-code`
package. The package ships a native binary; on the base image's Node.js it prints
an `EBADENGINE` warning at install time and still installs.

### Authentication

| Sandbox | Host | How Claude Code authenticates | Notes |
|---------|------|-------------------------------|-------|
| worktree | macOS or Linux | The host login (macOS Keychain or `~/.claude/.credentials.json`), or `ANTHROPIC_API_KEY` / `CLAUDE_CODE_OAUTH_TOKEN` exported in your shell | Worktree runs inherit the host environment |
| container | macOS or Linux (recommended) | `CLAUDE_CODE_OAUTH_TOKEN` from `claude setup-token`, exported in the shell that runs Sandman | The container receives the variable by name only; its value never appears on a command line or under `.sandman/`, and `~/.claude/.credentials.json` stays out of the snapshot |
| container | Linux | `~/.claude/.credentials.json` rides along in the `~/.claude` snapshot and is reconciled with the host around every launch | The snapshot lives under `.sandman/batches/<batch-id>/config/` and is kept with the batch; keep `.sandman/` gitignored |
| container | macOS | The Keychain never reaches a container. Export the token as above, set it in the agent's env (below), or use `ANTHROPIC_API_KEY` | An agent env value is exported on the rendered command line and is visible to `ps` on the host during the run |

Subscription sign-ins refresh with single-use refresh tokens: whichever copy
of `.credentials.json` refreshes first invalidates every other copy. Without
`CLAUDE_CODE_OAUTH_TOKEN`, Sandman reconciles the snapshot copy with the host
copy before and after every launch, so a launch hours into a batch (for example
after a usage-limit wait) uses the credentials the host refreshed meanwhile,
and a token a container refreshed reaches the host. A refresh on both sides
during the same launch can still invalidate one of them; the token avoids
refreshes altogether:

```bash
claude setup-token                     # prints sk-ant-oat01-...
export CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-...
sandman run 42
```

A launch that still cannot authenticate is relaunched once on the reconciled
credentials, then the run stops with reason `AGENT_AUTHENTICATION` instead of
spending its retries; sign in again or export the token, then resume it with
`sandman run --continue`.

Alternatively, add the token to `.sandman/config.yaml`:

```yaml
agents:
  claude:
    preset: claude
    env:
      CLAUDE_CODE_OAUTH_TOKEN: sk-ant-oat01-...
```

Without a token, a macOS container run fails at once with `Not logged in · Please run /login` (the stream's `system/init` record shows `"apiKeySource":"none"`); see [Troubleshooting > Claude Code runs fail in containers](../help/troubleshooting.md#claude-code-runs-fail-in-containers). If `claude` is not on your `PATH` but the Claude desktop app is installed, run `claude setup-token` with the copy the app manages under `~/Library/Application Support/Claude/claude-code/<version>/claude.app/Contents/MacOS/claude`.

`claude setup-token` creates a one-year token for your subscription (Pro, Max,
Team, or Enterprise); it is not refreshed automatically, and an expired token
fails the run like any other authentication error. Treat `.sandman/config.yaml`
as a secret when it carries a token. `keychain_auth: true` is rejected in
container mode for every preset.

Inside Claude Code, a cloud provider configuration wins over
`ANTHROPIC_AUTH_TOKEN`, which wins over `ANTHROPIC_API_KEY`, then `apiKeyHelper`,
then `CLAUDE_CODE_OAUTH_TOKEN`, then the `/login` credentials. An exported
`ANTHROPIC_API_KEY` therefore takes precedence over your subscription login. Do
not add `--bare` to a custom command: bare mode ignores the subscription login
and `CLAUDE_CODE_OAUTH_TOKEN`.

### Subscription use

Sandman spawns the unmodified Claude Code binary, authenticated with your own
seat, for your own repositories. That is the use Anthropic's Claude Code terms
allow for subscription login; Sandman never collects or stores tokens for other
users. Keep these limits in mind:

- Subscription limits are per seat and shared with claude.ai: a rolling
  five-hour window and a weekly window. Parallel AFK work (`parallel` above 1,
  the review daemon, retries) exhausts them faster than interactive use.
  Mitigations: `parallel: 1`, usage credits, or `ANTHROPIC_API_KEY` for bulk
  batches.
- Pro and Max are consumer plans for individuals. Company repositories should
  use a Team or Enterprise seat, or a Console API key.
- For shared, high-parallelism, or fleet-style unattended use, prefer
  `ANTHROPIC_API_KEY`: an OAuth token is tied to the subscription of the person
  who created it.
- Team and Enterprise administrators may restrict the login method; tokens from
  `claude setup-token` honour a forced login method.

### Permissions

A print-mode session has no one to answer permission prompts, so without
`--dangerously-skip-permissions` Claude Code denies every edit and shell command
that would prompt. Container runs pass the flag by default. For worktree runs,
pass `--dangerously-skip-permissions` explicitly, or allow the tools you need
through `permissions.allow` rules in your own `~/.claude/settings.json`.

The `~/.claude` snapshot that container runs receive carries your Claude Code
settings, and Claude Code also reads the machine's managed settings. All of them
apply inside the container. When they set `permissions.defaultMode` (for example
to `plan`) and disable bypass mode (`disableBypassPermissionsMode`), Claude Code
ignores `--dangerously-skip-permissions` and starts the session in that default
mode. Sandman does not work around administrator policy; it makes the effect
visible and lets you choose a permitted mode.

Like the OpenCode preset's `OPENCODE_PERMISSION`, the preset has built-in
permission handling that you can replace:

- **Built-in** (no `permission_mode`): container runs pass
  `--dangerously-skip-permissions`, worktree runs pass nothing.
- **Custom** (`permission_mode` set): every launch, container or worktree, passes
  `--permission-mode <mode>` instead of `--dangerously-skip-permissions`, and
  keeps Sandman's `--model`, `--effort`, `--continue`, and `--name` flags. On the
  command line the mode overrides a `defaultMode` from user or managed settings.

```yaml
agents:
  claude:
    preset: claude
    permission_mode: acceptEdits
```

Sandman accepts Claude Code's modes `acceptEdits`, `auto`, `bypassPermissions`,
`dontAsk`, and `manual`. It rejects `plan`, which cannot make changes, and it
rejects `permission_mode` on agents whose command line takes no mode (OpenCode,
custom providers) and on a custom `command` (put `--permission-mode` in the
command instead). Pick a mode your administrator allows: Claude Code refuses a
disabled mode such as `bypassPermissions` under `disableBypassPermissionsMode`.
In print mode, prompts that a mode does not pre-approve are denied, so
`acceptEdits` allows edits but denies shell commands that are not allowed by
your settings' `permissions.allow` rules.

A session that still starts in `plan` mode cannot do the run's work. Sandman
reads the effective mode from the session's first record and stops the run at
once with an error naming the mode and the `permission_mode` setting, instead of
waiting for the idle timeout. An implementation run fails with reason
`AGENT_PERMISSION_MODE` and is not retried; a review request is retried through
the review daemon's launch backoff and starts no health repair.

The review daemon's health repairs run on the host without skipping permissions.
Their launch allows edits under `.sandman/` and keeps every other prompting call
denied; see [Reviews > Health repair](reviews.md#health-repair). Managed
settings with `allowManagedPermissionRulesOnly` make Claude Code ignore
command-line rules such as `--allowedTools`, in Sandman's commands and in your
own.

Podman runs the agent as root inside the container, and Claude Code refuses
`--dangerously-skip-permissions` as root outside a recognized sandbox. The preset
exports `IS_SANDBOX=1` for that check, which Claude Code 2.1.283 honours: the
session starts in `bypassPermissions` mode. If a later version refuses again,
use a custom command that pre-approves tools instead of skipping permission
checks:

```yaml
agents:
  claude:
    preset: claude
    command: >-
      claude -p --output-format stream-json --verbose
      --allowedTools "Bash" "Edit" "Write" "Read" "Glob" "Grep"
      --permission-mode dontAsk "$(cat {{.PromptFile}})"
```

`--allowedTools` takes several values, so end its list with another option, as
`--permission-mode dontAsk` does here; otherwise Claude Code reads the prompt as
one more tool rule.

A custom command receives no `--model`, `--effort`, or `--continue` from Sandman;
add `--model <alias>` to it directly.

Bypass mode does not disable every check. Claude Code still denies some compound
shell commands (`Permission denied: Bash` followed by `This Bash command
contains multiple operations`), and the agent normally retries with simpler
commands. If your `~/.claude/settings.json` enables Claude Code's own sandbox,
the copied settings make every container run print `Sandbox disabled: ...
bubblewrap (bwrap) not installed, socat not installed`. The container is
already the isolation boundary, so the warning is harmless; add `bubblewrap
socat` to the Dockerfile's `apt-get install` line if you want Claude Code's
sandbox inside the container too.

### Session reuse

Sandman records the Claude Code session ID of each Run in the Run's
`session.json`, as soon as Claude Code reports it. It is the `agentSessionId`
in the Portal's Run details. Sandman reuses a Claude conversation on the same
paths that reuse an OpenCode session: runtime-owned await re-entry (including
usage-limit polling) and `sandman run --continue --reuse-session`. On those
paths it renders `--resume <session-id>` for the exact recorded session. If no
valid session is recorded, or Claude Code no longer has it, the Run starts a
fresh conversation from its preserved Task. Plain `--continue` always starts
fresh. Sandman never renders Claude Code's own `--continue`: every container
worktree shares the `/workspace` path, so "the latest conversation in this
directory" could be another repository's.

Worktree runs use your own `HOME`, so their transcripts are already in the
host's `~/.claude/projects`. In container mode the host's `~/.claude/projects`
is live-mounted. Sandman creates it if it is missing. Transcripts are written to the host directly, so
they outlive the batch and you can resume them from the host (see
[Resuming an agent session from the host](monitoring.md#resuming-an-agent-session-from-the-host)).
Do not set `CLAUDE_CODE_SKIP_PROMPT_HISTORY` or add `--no-session-persistence`
if you want reuse.

### Usage limits

In print mode Claude Code ends the run at a subscription limit instead of
waiting for the reset. Sandman recognises the limit on the final stream-json
`result` record (a top-level `"type":"result"` with `"is_error":true`) when it
contains one of these messages: `hit your session limit`, `hit your weekly
limit`, `hit your Opus limit`, `hit your Sonnet limit`, or `Fable limit reached`.
The run then emits `run.await` with `await_reason: usage-limit`, probes every
ten minutes for five hours of accumulated completed polling, and resumes the
same conversation with `--continue`. Re-entry/restart preserves consumed polling;
capacity delay and downtime consume none. After the final boundary probe,
configured ordinary fresh-session retries remain available. The expected
exhaustion timestamp is diagnostic, not a hard deadline. Spend and budget limits (`monthly spend limit`,
`shared budget`, ...) are not awaited. The review daemon applies the same rule
to `review_agent: claude` and enters its daemon-wide quota pause.

### Readable logs

The built-in command's stream-json is rendered into readable lines in the
terminal, `run.log`, and the portal, in the same style as OpenCode runs:

```text
Claude Code 2.1.283 · model claude-opus-5-5 · permissions bypassPermissions · session ed547cbf-...
Reading the issue first.
$ gh issue view 2514 --json body
→ Read /workspace/internal/cmd/run.go
→ Skill "sandman-implement"
Permission denied: Bash
Tool error: This Bash command contains multiple operations. ...
Result: success · 12 turns · 1m1s · $1.23
```

Agent text is kept as written; tool calls become one-line labels (`$ <command>`
for shell commands, `→ <Tool> <detail>` otherwise); failed tool results,
permission denials, compaction, and non-`allowed` rate-limit warnings get their
own line; the final `result` record becomes a summary with turns, duration, and
cost, followed by `Error: <message>` when the run failed. Thinking blocks,
thinking-token estimates, command lists, partial stream events, and
long-running tool heartbeats are dropped,
successful tool output is not repeated, and lines that are not JSON (Claude
Code's own warnings) pass through unchanged. Usage limits are recognised on the
raw `result` record before it is rendered. Dropped records still refresh
`run.log`'s modification time, so a long tool call or long thinking keeps
counting as activity for `run_idle_timeout`.

A custom `command` under the `claude` preset keeps its own output unchanged.

### Supported and limited capabilities

| Capability | `claude` preset |
|------------|-----------------|
| Worktree and container runs, prompts, pull-request lifecycle, retries, verification, dependencies, events, portal, badge, analyzers | Supported |
| Idle-timeout heartbeat | Supported (streams with `stream-json --verbose`) |
| `--model`, per-preset default model, `variant` as `--effort` | Supported |
| Session reuse on await re-entry and `--continue --reuse-session` | Supported through `--resume <session-id>` with the exact recorded session |
| Implementation usage limits | Supported through usage-limit waiting (10-minute polls, 5-hour window, same-session resume); the review daemon separately recovers reviewer quota |
| Scaffolded Dockerfile install and `installed-agents` pre-flight | Supported |
| Skill discovery | Supported through `~/.claude/skills` links; see [Skills](skills.md) |
| Exact-ID session identity (`session.json`), shown in Portal Run details | Supported |
| Session transcripts visible to host Claude Code | Supported: container runs live-mount `~/.claude/projects` (every host transcript is visible inside the container) |
| Context-limit rollover and the `context-exhausted` retry reason | Limitation by design: Claude Code compacts automatically; a failed compaction takes the ordinary retry path |
| Readable `run.log` rendering | Supported for the built-in command; custom commands keep raw output |
| Host/sandbox version-drift warning | Limitation: OpenCode only; the image pins a version and disables auto-update |
| macOS Keychain credentials in containers | Limitation for every preset: use `claude setup-token` |
| `context_error_phrases` | OpenCode only |
| Token and cost accounting | Not tracked by Sandman; the `result` record carries the values |

## Model selection

Sandman wires `AgentModel` only for built-in presets running their own command
template. `sandman run --model` overrides the configured default.

The model resolution order is:

1. `--model` flag on `sandman run` or `sandman run --continue`
2. `model` from `.sandman/config.yaml`, only when the selected agent uses the same preset as the default `agent`
3. The selected agent provider's configured model (e.g., from the agent's `model` field)
4. For an agent on a different preset than the default `agent`, that preset's default model (`opencode/big-pickle` or `sonnet`)

If none are set, the `claude` preset still runs on its default model (`sonnet`), because Claude Code would otherwise pick the account's default, often the most expensive model. The `opencode` preset passes no model flag and leaves the choice to OpenCode's own default.

Claude Code accepts model aliases (`sonnet`, `opus`, `haiku`, `fable`, `opusplan`,
`sonnet[1m]`) or full model names, never `provider/model` identifiers.

## Compatibility matrix

| Preset | Worktree | Container | Keychain Auth | Host Config Paths |
|--------|----------|-----------|---------------|-------------------|
| `opencode` | Yes | Yes | No | `~/.config/opencode`, `~/.local/share/opencode`, `~/.claude`, `~/.agents` |
| `claude` | Yes | Yes | No | `~/.claude`, `~/.agents`, `~/.claude.json` |

Both built-in presets support worktree and container-backed sandboxing.

## Container auth model

**Keychain auth is not supported in container mode.** If a built-in agent has `keychain_auth: true` and a container sandbox is selected, Sandman rejects the batch with a clear error message.

To use an agent inside a container:

1. Disable OS keychain integration for the agent CLI
2. Use file-based authentication (e.g., API keys stored in config files) or a token in the agent's `env`
3. Sandman resolves config files and directories into the container via a temporary copy

OpenCode reads `CLAUDE.md` and `.claude/skills/` if they exist. Sandman resolves `~/.claude` into the container automatically so those files are available to the agent.

Sandman installs its shared skill into `~/.agents/skills`, so both built-in presets mount `~/.agents` in container mode.

## Worktree management

Sandman manages worktrees itself — the agent does not need to create or switch branches. Sandman:

1. Creates a git worktree at `<worktree_dir>/<issue-number>-<slugified-title>` (default `.sandman/worktrees/<issue>-<slug>`). The directory does not include a feature prefix even when `--base-branch` selects a feature branch; the git branch name itself also remains `<issue>-<slug>` without a feature prefix due to ref-namespace constraints (see ADR-0040 and `internal/sandbox/worktree.go:66`)
2. Checks out the base branch as a starting point
3. The agent works inside this pre-created worktree directory
4. When the agent finishes, Sandman records the branch for commit history

Agents should work within the current directory and use standard git operations (add, commit, push) as needed.

## Container mode specifics

- Container mode is Linux-first. On macOS or Windows, container runtimes (Docker/Podman) must be configured to run Linux containers
- Config directories and files with `~` are expanded to the user's home directory on the host before being resolved into a temporary copy for the container
- Missing config directories and files are silently skipped (no error if an optional config path does not exist)
- Container images are built from `.sandman/Dockerfile` at project initialization
- A container run requires the Dockerfile's `# sandman installed-agents:` header to list the run agent's preset; add a second agent to the image and the header together (for example `# sandman installed-agents: opencode, claude`). The `# sandman default-agent:` header is checked against config only for runs of the default agent
