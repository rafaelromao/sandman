**Change-request title must follow Conventional Commits** (`feat:`, `fix:`, `docs:`, …) — see [`AGENTS.md`](../AGENTS.md#branching-and-versioning-rules). The `CI / semantic-pull-request` status check blocks merge when the title is non-conventional.

If this change request is part of a multi-issue initiative that ships under a `feat/<feature-name>` branch, target the feature branch instead of `main`; see [`AGENTS.md`](../AGENTS.md#feature-branches).

## What surface is this PR modifying?

- [ ] Go code
- [ ] Agent docs / prompts
- [ ] Domain docs / ADRs
- [ ] Build / CI infrastructure
- [ ] Other (please describe)

## Description

<!-- Describe the change and the problem it solves -->

## Related Issue

<!-- Link to the issue this PR addresses, if any -->
Fixes #(issue number)

## Checklist

- [ ] I have read the [Contributing Guide](CONTRIBUTING.md)
- [ ] `make check` passes locally
- [ ] I have updated `CHANGELOG.md` with a summary of my changes (if user-facing)
- [ ] For agent doc changes: I have verified the affected agent behavior in a sandbox (if applicable)

## Additional Notes

<!-- Any other context or notes for reviewers -->
