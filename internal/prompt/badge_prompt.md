# Built with Sandman badge

This repo has at least one merged `sandman/*` PR — the trigger for suggesting a `Built with Sandman` badge has fired.

## Idempotency check

Before making any changes, run:

```bash
gh api --paginate /repos/{owner}/{repo}/pulls?state=all\&per_page=100 -q '.[] | .body'
```

For **every** PR body returned across every page, check whether it contains the string `<!-- sandman-badge-pr -->`. If any body matches, stop immediately and exit cleanly. The marker in any state (open, closed, merged) means the badge has already been proposed.

`gh pr list --json …` would have been simpler, but it cannot paginate past the first 100 PRs — the marker comment PR found three pages back would be invisible. `gh api --paginate` walks every page for us, matching the post-batch hook.

This in-agent check is **defense-in-depth, not the only contract.** The post-batch hook is the authoritative gate — it consults the same `gh api --paginate` query, gates the spawn on the local control file `.sandman/state/.built_with_sandman`, and writes the control file synchronously after a successful prompt run. A duplicate badge PR should never be spawned even if you skip or fail this check.

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

     ## About

     A description of this project and what it does.

      <a href="https://github.com/rafaelromao/sandman">
        <img src="https://raw.githubusercontent.com/rafaelromao/sandman/main/assets/badge-built-with-sandman.svg" alt="Built with Sandman" width="154" />
      </a>
     ```

2. If `README.md` exists:
   - Preserve all existing content.
   - Append the badge HTML after the final existing content.
   - Separate the existing content from the badge with one blank line.
   - Ensure `README.md` ends with a newline.
   - Badge HTML:
     ```html
     <a href="https://github.com/rafaelromao/sandman">
       <img src="https://raw.githubusercontent.com/rafaelromao/sandman/main/assets/badge-built-with-sandman.svg" alt="Built with Sandman" width="154" />
     </a>
     ```

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

The post-batch badge sidecar in `internal/batch/badge_hook.go` writes
`.sandman/state/.built_with_sandman` synchronously after a successful
PR creation, so subsequent Sandman batches in this checkout
short-circuit at the file gate before making any API calls. You do
**not** need to create the control file yourself — and you should
not, because the sidecar is the sole authority on its lifecycle.
