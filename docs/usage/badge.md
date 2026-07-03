# Built with Sandman Badge

When a project has merged its first `sandman/*` branch, Sandman can suggest adding a "Built with Sandman" badge to the README — a lightweight way to signal that the project ships issues via Sandman.

## What the badge is

The badge is a self-hosted SVG pill that links to the [Sandman repo](https://github.com/rafaelromao/sandman). It is inserted directly into the project's `README.md` rather than hosted on an external badge service.

## How it gets there

After a batch completes with at least one merged `sandman/*` PR, Sandman's post-batch sidecar checks whether a badge has already been proposed. If not, it dispatches a child `sandman run --prompt` that opens a PR titled `chore: add Built with Sandman badge`.

The PR is created by an agent — the same agent infrastructure used for issue-driven runs — not by hardcoded Go logic. The agent reads or scaffolds the README, inserts the badge, pushes the branch, and opens the PR.

## The control file (fast path)

When the badge sidecar successfully creates the PR, it writes an empty sentinel file at `.sandman/.built_with_sandman` (atomically, via temp-file + rename). The post-batch hook checks for this file **before** running the expensive `gh pr list --state all --limit 100` scan: if the file is present, the hook treats the badge as already proposed and exits silently.

The file is gitignored (it lives under `.sandman/`), is per-checkout, and is intentionally empty — its mere existence is the signal. Removing it has no harmful effect: the next batch just pays the `gh pr list` cost once and the sidecar re-writes the file if the marker comment is still found.

## The marker comment (fallback)

The PR body starts with:

```
<!-- sandman-badge-pr -->
```

This marker is the single source of truth for idempotency. Any PR (open, closed, or merged) whose body contains this marker suppresses re-creation of the badge PR. The marker lives in the PR body, not the README or the commit message, so users can edit either without re-triggering the flow.

## How to opt out

Close the badge PR unmerged. Sandman respects that decision forever — the marker in the closed PR's body signals that the user has already been prompted and declined.

## README placement rules

The agent follows three placement rules:

1. **No README exists** → scaffold a minimal `README.md` with the project name, description (or a placeholder), the badge, and an `## About` stub.
2. **README exists, no H1** → insert the badge as the very first line of the file.
3. **README exists with H1** → insert the badge directly under the H1, before any other content.

## Commit trailer

The badge commit carries a `Co-authored-by: sandman[bot] <bot@sandman>` trailer to give attribution without splitting authorship. The commit itself is authored under the user's `gh` identity.

## Failure modes

Badge suggestion is best-effort. If any step fails — `gh` auth issue, network error, README write failure — Sandman emits a warning and completes the batch normally. The badge is never forced and never retried automatically.