# Sandman Skills

Sandman syncs the full shared `sandman` skill folder during `sandman init`. It lives at `~/.agents/skills/sandman/` and is regenerated when `review_command` changes.

## What it contains

The installed folder mirrors the local Sandman skill and includes routed subskills for:

- implement
- tdd
- code-review (self-review and daemon-review contexts)
- run (standalone implementation, review-cycle, and merge composition)
- review-request (stateless trigger delivery)
- review-cycle (standalone in-memory review observation)
- pr-review
- back-merge
- pr-merge

> **Note:** The `tdd` and `code-review` skills were originally created by Matt Pocock. We strongly recommend checking out his work at [aihero.dev](https://www.aihero.dev/).

`docs/usage/default-task-prompt.md` now acts as an AFK bootstrap that passes issue context, branch context, and the configured review command into the installed `sandman` skill.

## Using the skills directly

You can load `sandman-run` directly in OpenCode or Claude Code for the
standalone workflow without a host `sandman` binary. It composes
`sandman-implement`, `sandman-review-cycle`, and `sandman-pr-merge` in that
order. The focused `sandman-review-request` capability delivers one guarded
request; `sandman-review-cycle` owns standalone CI, observation, feedback, and
re-request behavior. `sandman-pr-review` remains a compatibility facade over
the cycle. The shared skills form an AFK workflow and do not wait for operator
input.

Managed implementation uses `sandman-review-request` only for delivery. After
confirmation, the runtime owns observation, evidence, waiting, resume, and
terminal decisions. The durable request state remains runtime-owned, including
matching current-request evidence. Standalone `sandman-run` keeps review-cycle state in memory
for its session; a restart reconstructs from live pull-request state and does
not import or trust managed lifecycle artifacts.

The compatibility `sandman-pr-review` entrypoint delegates to the same
standalone review-cycle contract. This keeps standalone composition
implementation-first, review-cycle-second, and merge-last, while managed runs
load only stateless request delivery.

## Claude Code discovery

Claude Code discovers skills only as `~/.claude/skills/<name>/SKILL.md`, one level deep. Skill sync therefore also creates symlinks into the shared folder:

- `~/.claude/skills/sandman` points to `~/.agents/skills/sandman`
- `~/.claude/skills/sandman-<mode>` points to each mode's folder, for example `sandman-implement`

Sync creates `~/.claude/skills` when it is missing, repoints existing symlinks, and leaves a real file or directory at a link path untouched with a warning. The links are refreshed on every `sandman init` and `review_command` change.

## Container access

Sandman mounts `~/.agents` into built-in agent containers so the shared skill is visible in container-backed runs. The `claude` preset also mounts `~/.claude`; the container config snapshot copies symlinks as plain files, so the `sandman-*` skills are present inside the container.

## Review command

`{{REVIEW_COMMAND}}` is rendered from project config. `sandman init --review-command` seeds that value, and `sandman config set review_command ...` updates both config and the installed shared skill tree.

`{{REVIEW_TIMEOUT}}` is rendered into each current AgentRun Task as the
effective delegated review response budget in seconds. It is deliberately not
written into the globally shared skill tree, so repositories with different
policies cannot overwrite one another's active run context.

Standalone use of `sandman-review-cycle` uses the versioned portable observer
entry point for one confirmed request and retains the request envelope only in
the active session. Managed implementation runs use the runtime's canonical
request record and do not invoke the standalone cycle. The request envelope
identifies the pull request, current head, confirmed trigger, effective timeout,
and absolute deadline. The command returns one structured
JSON result (`pending`, `responded`, `timed_out`, or `unavailable`). A responded
result carries both the preserved raw snapshot and an additive
`review-classification/v1` object scoped to the confirmed trigger and current
head. The classification records the active request window, source evidence,
canonical timestamps, head status, and formal requested-changes precedence for
the existing approval and feedback rules. Its temporary transport is discarded
when the standalone session ends; it is not a restart or managed lifecycle
record.

If Sandman detects local edits under `~/.agents/skills/sandman/`, it asks before overwriting in a TTY. In non-interactive mode it fails instead of silently replacing those edits. Built-in containers mount this tree at `/.agents/skills/sandman/`, and the helper is invoked through `sh`, so the wait does not depend on a host `sandman` binary.
