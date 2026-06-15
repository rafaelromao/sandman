# Built with Sandman badge

This repo has at least one merged `sandman/*` PR — the trigger for suggesting a `Built with Sandman` badge has fired.

## Idempotency check

Before making any changes, run `gh pr list --state all --limit 100 --json body`. If **any** PR body contains the string `<!-- sandman-badge-pr -->`, stop immediately and exit cleanly. The marker in any state (open, closed, merged) means the badge has already been proposed.

## Context

Merged Sandman PRs in this repo:
{{MERGED_PRS}}

## Instructions

1. If `README.md` does not exist:
   - Read the repo name with `gh repo view --json name`.
   - Read the repo description with `gh repo view --json description`. If empty, use `<Project description goes here.>`.
   - Create `README.md` containing:
     ```markdown
     # <repo name>

     <description>

     [![Built with Sandman](https://raw.githubusercontent.com/rafaelromao/sandman/main/assets/badge-built-with-sandman.svg)](https://github.com/rafaelromao/sandman)

     ## About

     A description of this project and what it does.
     ```

2. If `README.md` exists with a top-level `# ` heading (H1):
   - Insert the badge markdown as the first line directly under the H1 (before any other body content).
   - Badge markdown: `[![Built with Sandman](https://raw.githubusercontent.com/rafaelromao/sandman/main/assets/badge-built-with-sandman.svg)](https://github.com/rafaelromao/sandman)`

3. If `README.md` exists with no H1:
   - Insert the badge markdown as the very first line of the file.

## Branch and commit

- Branch name: `sandman/built-with-sandman`
- Base branch: read from `gh repo view --json defaultBranchRef`
- Single commit on this branch, touching only `README.md` (or creating it)
- Commit author: your own `gh` auth identity
- Commit trailer: `Co-authored-by: sandman[bot] <bot@sandman>`

## PR creation

- Title: `chore: add Built with Sandman badge`
- Body: must start with `<!-- sandman-badge-pr -->` on its own line, followed by:
  ```
  Suggested by Sandman — feel free to close this PR if you don't want the badge.

  This repo has shipped at least one Sandman-built change.
  The badge links to https://github.com/rafaelromao/sandman.
  ```
- Do not edit any file other than `README.md`.
- Do not auto-merge the PR.
- Do not add CI, workflow, or configuration changes.
