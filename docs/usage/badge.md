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

This marker is the single source of truth for idempotency and applies in any PR state — open, closed, or merged. The post-batch hook walks every page of the pull-request list until the marker is found or the scan is exhausted, so a closed or merged badge PR cannot be truncated regardless of repo activity. The pagination mechanism (REST via `gh api --paginate`) is documented in the relevant ADR alongside the rest of the badge hook's design.

The marker lives in the PR body, not the README or the commit message, so users can edit either without re-triggering the flow.

## The control file (the API gate)

When the badge sidecar successfully creates the PR, it writes an empty sentinel file at `.sandman/state/.built_with_sandman` (atomically, via temp-file + rename). The post-batch hook consults this file **first** on every batch: when the file is present, neither the marker scan nor the spawn runs. The file is the authoritative short-circuit; the marker-comment scan only runs when the file is absent.

The file is gitignored (it lives under `.sandman/state/`), is per-checkout, and is intentionally empty — its mere existence is the signal that the badge sidecar has created the marker-bearing PR in this checkout. Removing it is safe: the next batch pays the marker-comment query cost once and the sidecar re-writes the file if a successful run happens again. The control file is the primary gate, not a perf optimisation.

## How to opt out

Close the badge PR unmerged. Sandman respects that decision forever — the marker in the closed PR's body is what the marker-comment scan reads, and the post-batch hook only writes the control file after a fresh successful spawn.

## README placement rules

The agent follows three placement rules:

1. **No README exists** → scaffold a minimal `README.md` with the project name, description (or a placeholder), the badge, and an `## About` stub.
2. **README exists, no H1** → insert the badge as the very first line of the file.
3. **README exists with H1** → insert the badge directly under the H1, before any other content.

## Commit trailer

The badge commit carries a `Co-authored-by: sandman[bot] <bot@sandman>` trailer to give attribution without splitting authorship. The commit itself is authored under the user's `gh` identity.

## Failure modes

Badge suggestion is best-effort and **silent** to the operator. If any step fails — `gh` auth issue, network error, README write failure, marker-comment scan failure — the hook completes the batch normally with no operator-visible noise. The hook returns instead of raising a sentinel warn-line; the next batch retries harmlessly via the standard post-batch trigger. The badge is never forced and never retried automatically.