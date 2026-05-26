# Sandman Skills

Sandman installs the full shared `sandman` skill folder during `sandman init`. It lives at `~/.agents/skills/sandman/` and is only written when missing, so local customization is preserved.

## What it contains

The installed folder mirrors the local Sandman skill and includes routed subskills for:

- implement
- tdd
- review
- delegate-review
- merge
- pr-merge
- continuation

`docs/usage/default-prompt.md` now acts as a bootstrap that passes issue context, branch context, and the runtime review command into the installed `sandman` skill.

## Container access

Sandman mounts `~/.agents` into built-in agent containers so the shared skill is visible in container-backed runs.

## Review command

`/oc review` stays parameterized at runtime. Sandman still resolves `{{REVIEW_COMMAND}}` from config or `--review-command` before the prompt is rendered.
