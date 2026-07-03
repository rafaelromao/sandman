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

      <a href="https://github.com/rafaelromao/sandman">
        <img src="https://raw.githubusercontent.com/rafaelromao/sandman/main/assets/badge-built-with-sandman.svg" alt="Built with Sandman" width="154" />
      </a>

     ## About

     A description of this project and what it does.
     ```

2. If `README.md` exists with a top-level `# ` heading (H1):
   - Insert the badge HTML as the first line directly under the H1 (before any other body content).
   - Badge HTML:
     ```html
     <a href="https://github.com/rafaelromao/sandman">
       <img src="https://raw.githubusercontent.com/rafaelromao/sandman/main/assets/badge-built-with-sandman.svg" alt="Built with Sandman" width="154" />
     </a>
     ```

3. If `README.md` exists with no H1:
   - Insert the badge HTML as the very first line of the file.

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

  The badge links to https://github.com/rafaelromao/sandman.
  ```
- Do not edit any file other than `README.md`.
- Do not auto-merge the PR.
- Do not add CI, workflow, or configuration changes.

## Control file

After the PR is created successfully (the `gh pr create` call returns the
new PR URL), create an empty sentinel file at
`.sandman/.built_with_sandman` so subsequent Sandman batches in this
checkout can short-circuit the `gh pr list` check:

```sh
tmp="$(mktemp .sandman/.built_with_sandman.XXXXXX)"
: > "$tmp"
mv "$tmp" ".sandman/.built_with_sandman"
```

The file is intentionally empty — its mere existence is the signal that
the badge has already been proposed in this checkout. It is gitignored
and recreated automatically by every successful badge sidecar run. Use
the temp-file + `mv` (rename) pattern above so the file is either fully
present or fully absent on disk — never half-written.
