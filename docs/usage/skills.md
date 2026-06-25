# Sandman Skills

Sandman syncs the full shared `sandman` skill folder during `sandman init`. It lives at `~/.agents/skills/sandman/` and is regenerated when `review_command` changes.

## What it contains

The installed folder mirrors the local Sandman skill and includes routed subskills for:

- implement
- tdd
- review
- pr-review
- back-merge
- pr-merge

> **Note:** The `tdd` and `review` skills were originally created by Matt Pocock. We strongly recommend checking out his work at [aihero.dev](https://www.aihero.dev/).

`docs/usage/default-task-prompt.md` now acts as a bootstrap that passes issue context, branch context, and the configured review command into the installed `sandman` skill.

## Container access

Sandman mounts `~/.agents` into built-in agent containers so the shared skill is visible in container-backed runs.

## Review command

`{{REVIEW_COMMAND}}` is rendered from project config. `sandman init --review-command` seeds that value, and `sandman config set review_command ...` updates both config and the installed shared skill tree.

If Sandman detects local edits under `~/.agents/skills/sandman/`, it asks before overwriting in a TTY. In non-interactive mode it fails instead of silently replacing those edits.
