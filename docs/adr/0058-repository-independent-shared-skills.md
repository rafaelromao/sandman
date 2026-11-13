# ADR-0058: Repository-independent shared skills

## Status

proposed

## Context

Sandman installs its skills once per user, at `~/.agents/skills/sandman`, and
every repository on the machine shares that tree. Containers mount it, and the
runtime recognises helper commands at that path.

`sandman init` and `sandman config set review_command` rendered the
repository's review command into the shared tree. When two repositories used
different reviewers, the last write won.

- **Managed Runs.** A Run's Task could name its repository's command while the
  request skill named another. The runtime registers only triggers that use
  the repository's command, so such a Run stalled at review.
- **Quoting.** The raw value was inserted inside bash double quotes, so a
  command containing `"`, `$` or a backtick broke the review cycle.
- **Unfilled placeholders.** The standalone `sandman-run` skill also carried
  branch, base-branch and timeout placeholders copied from the managed Task
  prompt. Nothing filled them.
- **Hardcoded base branch.** The implementation skill assumed `main` and
  opened pull requests against the hosting service's default branch.

ADR-0050 already kept the review timeout out of the shared tree for the same
reason.

## Decision

Extend ADR-0050 to every repository and run value. Skill text is identical in
every installation, global or scoped to a repository, and contains no
install-time values; skill sync copies the files byte for byte.

- **Managed Runs** take every value from the Run's Task: review command, base
  branch, current branch and review timeout. Task values always win.
- **Standalone use** resolves each value at run time from the first source that
  provides it. The table below lists the sources in order.
- **Configuration only.** `sandman config set review_command` no longer
  re-syncs skills, and `sandman init --review-command` only stores
  configuration.
- **Quoting.** The review cycle passes the resolved review command to its
  helpers as one quoted argument.
- **Helper location.** Helpers find their sibling helpers relative to their own
  location. The review cycle looks for helpers in `SANDMAN_SKILL_ROOT`, then
  the repository's `.agents/skills/sandman`, then `~/.agents/skills/sandman`.
- **Any `/sandman` review command.** `sandman run` requires the review daemon
  for any review command containing `/sandman`, so the daemon answers any
  comment that starts with the `/sandman` command, optionally after one
  mention. A repository may therefore set a review command such as
  `/sandman check`. The focus is the text after `/sandman review`, or after
  `/sandman` for another command. The daemon still answers `/sandman review`
  anywhere in a comment, and redaction already strips every `/sandman` from
  its own posts.
- **Per-repository base branch.** A repository records its base branch once.
  `sandman init` records `git.base_branch` from `--base-branch`, else
  `origin/HEAD`, else `main`. Runs open pull requests against the resolved
  base, and the "already resolved" verification compares against
  `origin/<base>`.

| Value | Standalone sources, in order |
|---|---|
| Review command | caller → `review_command` in `.sandman/config.yaml` → `/sandman review` |
| Base branch | caller → open pull request's base → `git.base_branch` → repository default branch |
| Current branch | caller → checked-out branch |
| Review timeout | caller → `review_timeout` → 1800 seconds |

## Consequences

### Positive

- Repositories with different reviewers or base branches share one skill tree
  without overwriting one another.
- A managed Run never sees a review command that differs from its Task.
- The skills work when installed into a repository, without a copy under the
  user's home directory.
- Any review command, including ones with shell metacharacters, reaches the
  helpers verbatim.

### Negative

- Standalone agents resolve values at run time, which adds a step and relies
  on the agent reading `.sandman/config.yaml`.
- A tree rendered by an earlier version keeps its old text until the next
  `sandman init`. Managed Runs are unaffected because the Task supplies the
  values.
