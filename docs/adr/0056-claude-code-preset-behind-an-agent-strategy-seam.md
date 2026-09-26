# ADR-0056: Re-add Claude Code as a second built-in agent preset behind an agent strategy seam

## Status

proposed

## Context

Sandman runs only OpenCode. Since April 2026 Anthropic blocks Claude
subscription OAuth in third-party harnesses, OpenCode included, so operators
with Claude Pro, Max, Team, or Enterprise seats cannot spend that quota through
Sandman. Running the unmodified `claude` binary in print mode (`claude -p`) is
the sanctioned way to use a subscription unattended.

Two earlier Claude Code presets were removed. ADR-0006 was edited in place when
the first one went away, and ADR-0020 records the real cost behind every
removal: agent-specific behaviour was chosen by comparing the preset name or
the command string at each call site, so every agent feature needed a parity
branch in the run loop, the orchestrator, the review daemon, and the scaffold.

## Decision

**Agent strategy seam.** Agent-specific run-loop behaviour lives behind one
`agentStrategy` interface in `internal/batch`, selected once per launch by
`strategyFor(preset, command)`: command flags (model, variant), the launch
environment, session selection, output parsing, and failure classification
(context rollover, usage limits, and whether a usage limit is awaited). The
selector uses a registry keyed by preset. A strategy also learns whether the
launch runs the preset's own command template: a custom command under a
built-in preset keeps the preset's failure classification and environment
rules but receives no injected flags, session selection, parsing, or
usage-limit awaiting, which is exactly how OpenCode custom commands already
behaved. Presets without a strategy, including preset-less custom providers,
get a passthrough strategy.

No agent-name comparison survives outside the selector and the data
registries (`config.BuiltInAgentPresets` and the scaffold's installer table);
a source-scanning test fails when one appears. The `config` and `scaffold`
packages cannot depend on the run loop, so they keep per-agent facts as data:
`AgentPreset.DefaultModel` and a per-agent Dockerfile installer table. The
OpenCode strategy is a pure move of the previous code, pinned by a golden
captured before the seam existed.

**The `claude` preset.** `config.BuiltInAgentPresets` gains `claude`, which
runs `claude -p --output-format stream-json --verbose` with `--continue`,
`--dangerously-skip-permissions`, `--name '<session name>'`, `--model`, and
`--effort` rendered from the existing template keys. `stream-json` with
`--verbose` is the only print-mode format that streams while the agent works,
which the idle-timeout heartbeat needs. The preset exports
`DISABLE_AUTOUPDATER`, `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`, and
`IS_SANDBOX` (containers run as root under podman, and Claude Code refuses to
skip permission prompts as root outside a recognized sandbox), mounts
`~/.claude`, `~/.agents`, and `~/.claude.json`, and keeps transcripts, caches,
and logs out of the per-batch snapshot. Authentication stays file or
environment based: host login credentials in worktree mode, and
`~/.claude/.credentials.json` or `CLAUDE_CODE_OAUTH_TOKEN` in containers.

**Session reuse is a per-strategy choice.** The Claude strategy reuses a
conversation through Claude Code's working-directory-scoped `--continue`, which
includes print-mode sessions. Each row owns one worktree, so the worktree
identifies the conversation: no session ID is parsed and no `session.json` is
written. `session.json` stays the OpenCode session identity and ADR-0055 is
unchanged. In container mode the batch config snapshot is bind-mounted
read-write, so transcripts written during a run survive an await re-entry.

**Automatic compaction replaces context rollover for Claude.** Claude Code
compacts on its own in print mode; a failed compaction ends the attempt, and
the ordinary retry starts fresh from the preserved Task. The context-rollover
detector and the `context-exhausted` retry reason stay OpenCode-only.

**Usage-limit detection is a per-strategy rule on a preset-specific line.**
OpenCode keeps its `Error:` line rule. For Claude, only the final stream-json
`result` record with a top-level `is_error: true` qualifies, and it must
contain one of the documented session, weekly, or model limit messages; spend
and budget limits are not awaited because they do not reset within the waiting
window. A recognised limit enters the existing usage-limit await, whose
re-entry renders `--continue`. The review daemon's daemon-wide quota gate
(ADR-0029) now applies to any review agent whose strategy awaits usage limits.

**Claude output is rendered like OpenCode's.** The Claude strategy's output
parser turns stream-json records into readable lines (agent text, tool labels,
tool errors, permission denials, and a result summary) and drops progress
records. Because rendering removes the raw `result` record, the parser applies
the usage-limit rule to the raw record itself and reports it to the run loop.

**Models follow the agent's preset.** Each preset declares a default model
(`opencode/big-pickle`, `sonnet`). The global `model` key applies only to
agents that share the default agent's preset; another preset uses its own
configured model or its preset default. `review_model` defaults to the review
agent's preset default, and a review agent override that changes preset never
inherits a model configured for another provider.

**The scaffold installs the default agent through the installer table.** The
Dockerfile's `installed-agents` header lists the installed presets, and
container pre-flight requires it to contain the run agent's preset, so a run
whose agent is missing from the image fails before any container starts. The
`default-agent` header is compared with config only when the run uses the
default agent; a run on another agent does not depend on the default agent.
An explicit `init --agent` on an existing project switches the preserved
config's default agent and models to that preset.

**Skills are bridged by symlink.** Claude Code discovers skills only under
`~/.claude/skills/<name>/SKILL.md`. Skill sync keeps writing the shared tree
to `~/.agents/skills/sandman` and links `~/.claude/skills/sandman` and
`~/.claude/skills/sandman-<mode>` into it. The per-batch snapshot dereferences
symlinks (ADR-0008), so containers see plain files.

ADR-0006 and ADR-0020 stay immutable. ADR-0020's "single built-in agent"
statement is superseded for naming only; its parity concern is answered by the
strategy seam.

## Consequences

### Positive

- Operators can run Sandman on a Claude subscription through the unmodified
  Claude Code binary, for implementation runs and for the review daemon.
- Adding or removing an agent touches a preset entry, an installer entry, and
  one strategy type; the run loop, orchestrator, and daemon do not change.
- OpenCode behaviour is unchanged and pinned by a golden test.

### Negative

- Capabilities that are not wired for Claude are limitations rather than
  features: exact-ID session identity, context rollover, the host/sandbox
  version-drift warning, and macOS Keychain
  credentials in containers. `docs/usage/agent-compatibility.md` lists each
  one with its workaround.
- Claude's usage-limit rule depends on the documented message text and on the
  result record's framing, which must be confirmed against a real capture.
- Subscription limits are per seat and shared with claude.ai; parallel AFK
  batches exhaust them faster than interactive use.

### Neutral

- Custom commands under a built-in preset keep the preset's failure
  classification and environment rules, exactly as before the seam.
- Skill links appear in `~/.claude/skills` for every Sandman user, not only
  for operators who run Claude Code.
