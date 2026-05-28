# Task

Implement issue #{{ISSUE_NUMBER}}: {{ISSUE_TITLE}}

## Context

{{ISSUE_BODY}}

## Runtime Context

- Work in the current Sandman-created worktree on `{{BRANCH}}` (`{{SOURCE_BRANCH}}`).
- To implement the issue, call `sandman implement` first. Write continuation context by loading the `sandman-continuation`. Do this even if the previous step failed. If the PR is fully approved, call `sandman pr-merge` next.
