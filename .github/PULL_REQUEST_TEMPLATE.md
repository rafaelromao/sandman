**Change-request title must follow Conventional Commits** (`feat:`, `fix:`, `docs:`, …) — see [`AGENTS.md`](../AGENTS.md#branching-and-versioning-rules). The `CI / semantic-pull-request` status check blocks merge when the title is non-conventional.

If this change request is part of a multi-issue initiative that ships under a `feat/<feature-name>` branch, target the feature branch instead of `main`; see [`AGENTS.md`](../AGENTS.md#feature-branches).

## What surface is this PR modifying?

- [ ] Go code
- [ ] Agent docs / prompts
- [ ] Domain docs / ADRs
- [ ] Build / CI infrastructure
- [ ] Other (please describe)

## Description

<!-- Summarize what changed and why it solves the problem. -->

## Related Issue

<!-- Link to the issue this PR addresses, if any -->
Fixes #(issue number)

## Verification

<!-- List actual local commands and outcomes, plus known failures or skips. Report external CI only for the current pull-request head and include when it was observed; if not observed, say so. -->

## Checklist

- [ ] I have read the [Contributing Guide](CONTRIBUTING.md)
- [ ] `make check` passes locally
- [ ] I have updated `CHANGELOG.md` with a summary of my changes (if user-facing)
- [ ] For agent doc changes: I have verified the affected agent behavior in a sandbox (if applicable)

## Additional Notes

<!-- Any other context or notes for reviewers -->
