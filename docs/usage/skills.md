# Sandman Skills

Sandman installs a shared `sandman` skill during `sandman init`. The skill lives at `~/.agents/skills/sandman/SKILL.md` and is only written when missing, so local customization is preserved.

## What it contains

The skill owns the full Sandman workflow:

- plan
- implement
- review
- merge
- continuation

`docs/usage/default-prompt.md` now acts as a bootstrap that passes issue context, branch context, and the runtime review command into that shared skill.

## Container access

Sandman mounts `~/.agents` into built-in agent containers so the shared skill is visible in container-backed runs.

## Review command

`/oc review` stays parameterized at runtime. Sandman still resolves `{{REVIEW_COMMAND}}` from config or `--review-command` before the prompt is rendered.
