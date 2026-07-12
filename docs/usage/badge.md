# Built with Sandman Badge

When a project has merged its first `sandman/*` branch, Sandman can suggest adding a "Built with Sandman" badge to the README — a lightweight way to signal that the project ships issues via Sandman.

## What the badge is

The badge is a self-hosted SVG pill that links to the [Sandman repo](https://github.com/rafaelromao/sandman). It is inserted directly into the project's `README.md` rather than hosted on an external badge service.

<img src="../assets/badge-built-with-sandman.svg" alt="Built with Sandman" width="154" />

## How it gets there

After a batch completes with at least one merged `sandman/*` PR, Sandman's post-batch sidecar checks whether a badge has already been proposed. If not, it dispatches a child `sandman run --prompt` that opens a PR titled `chore: add Built with Sandman badge`.

The PR is created by an agent — the same agent infrastructure used for issue-driven runs — not by hardcoded Go logic. The agent reads or scaffolds the README, inserts the badge, pushes the branch, and opens the PR.

## The marker comment (source of truth)

The PR body starts with:

```
<!-- sandman-badge-pr -->
```

This marker is the single source of truth for idempotency and applies in any PR state — open, closed, or merged. The post-batch hook consults the marker first, before any local state, so a marker-bearing PR in any state suppresses re-creation of the badge PR.

The marker lives in the PR body, not the README or the commit message, so users can edit either without re-triggering the flow. Because the query pages through `gh pr list --state all --limit 100` until the marker is found or results are exhausted, a closed or merged badge PR cannot be truncated regardless of repo activity.

## The control file (perf optimization)

When the badge sidecar successfully creates the PR, it writes an empty sentinel file at `.sandman/state/.built_with_sandman` (atomically, via temp-file + rename). The post-batch hook only checks for this file **after** the marker-comment query has returned no result: if the marker is absent and the file is present, the hook trusts the file as an optimistic fast-path signal and exits silently without re-running the expensive PR scan.

The file is gitignored (it lives under `.sandman/state/`), is per-checkout, and is intentionally empty — its mere existence is the signal that the badge sidecar has created the marker-bearing PR in this checkout. Removing it is safe: the next batch just pays the marker-comment query cost once and the sidecar re-writes the file if the marker is still found on a fresh scan. The control file is never consulted on its own — it is always subordinate to the marker-comment result.

## How to opt out

Close the badge PR unmerged. Sandman respects that decision forever — the marker in the closed PR's body is what the post-batch hook reads first, so the user has already been prompted and declined.

## README placement rules

The agent follows three placement rules:

1. **No README exists** → scaffold a minimal `README.md` with the project name, description (or a placeholder), the badge, and an `## About` stub.
2. **README exists, no H1** → insert the badge as the very first line of the file.
3. **README exists with H1** → insert the badge directly under the H1, before any other content.

## Commit trailer

The badge commit carries a `Co-authored-by: sandman[bot] <bot@sandman>` trailer to give attribution without splitting authorship. The commit itself is authored under the user's `gh` identity.

## Failure modes

Badge suggestion is best-effort. If any step fails — `gh` auth issue, network error, README write failure — Sandman emits a warning and completes the batch normally. The badge is never forced and never retried automatically.