# Default Prompt

Sandman's default prompt is a bootstrap layer. The full workflow lives in the shared skill installed at `~/.agents/skills/sandman/SKILL.md`.

## Bootstrap prompt

<!-- default-prompt:start -->
    # Sandman Bootstrap

    Implement the issue and follow the shared Sandman skill at `~/.agents/skills/sandman/SKILL.md`.

    ## Issue

    Issue #{{ISSUE_NUMBER}}: {{ISSUE_TITLE}}

    ## Context

    {{ISSUE_BODY}}

    ## Runtime Context

    - Current branch: `{{BRANCH}}`
    - Base branch: `{{BASE_BRANCH}}`
    - Review command: `{{REVIEW_COMMAND}}`

    ## Expectations

    - Use the shared skill as the workflow source of truth.
    - Keep this prompt as the bootstrap layer only.
<!-- default-prompt:end -->

## Lifecycle

- `sandman init` copies the bootstrap prompt to `.sandman/prompt.md`.
- `sandman init` installs the shared skill to `~/.agents/skills/sandman/SKILL.md` only when it is missing.
- Sandman mounts `~/.agents/skills` into container runs so the shared skill stays visible there.
- `{{REVIEW_COMMAND}}` still resolves at render time, so the default `/oc review` can be overridden in config or via CLI.
