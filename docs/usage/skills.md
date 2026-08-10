# Sandman Skills

Sandman syncs the full shared `sandman` skill folder during `sandman init`. It lives at `~/.agents/skills/sandman/` and is regenerated when `review_command` changes.

## What it contains

The installed folder mirrors the local Sandman skill and includes routed subskills for:

- implement
- tdd
- code-review (self-review and daemon-review contexts)
- pr-review
- back-merge
- pr-merge

> **Note:** The `tdd` and `code-review` skills were originally created by Matt Pocock. We strongly recommend checking out his work at [aihero.dev](https://www.aihero.dev/).

`docs/usage/default-task-prompt.md` now acts as an AFK bootstrap that passes issue context, branch context, and the configured review command into the installed `sandman` skill.

## Using the skills directly

You can also load `sandman-implement`, `sandman-code-review`, and `sandman-pr-review` directly in OpenCode for a local run without `sandman run`. Use `sandman-code-review` in self-review context for an implementor's own changes; the review daemon uses its daemon-review context with supplied pull-request information and writes the reviewer decision artifact without managing the pull request. The same autonomous workflow, guardrails, and terminal conditions apply; the skills do not wait for operator input.

## Container access

Sandman mounts `~/.agents` into built-in agent containers so the shared skill is visible in container-backed runs.

## Review command

`{{REVIEW_COMMAND}}` is rendered from project config. `sandman init --review-command` seeds that value, and `sandman config set review_command ...` updates both config and the installed shared skill tree.

`{{REVIEW_TIMEOUT}}` is rendered into each current AgentRun Task as the
effective delegated review response budget in seconds. It is deliberately not
written into the globally shared skill tree, so repositories with different
policies cannot overwrite one another's active run context.

If Sandman detects local edits under `~/.agents/skills/sandman/`, it asks before overwriting in a TTY. In non-interactive mode it fails instead of silently replacing those edits.
