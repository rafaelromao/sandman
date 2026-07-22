# ADR-0012: Rename delegate-review to pr-review

## Status

accepted

## Context

Sandman has a delegated PR review mode exposed to users as `sandman delegate-review`. The current name is longer than the rest of the mode set and can be confused with issue delegation workflows. The implementation already treats this mode as a dedicated PR review loop, so the public name should match that purpose.

## Decision

We will rename the delegated review mode from `delegate-review` to `pr-review` across the repo, embedded skill tree, docs, tests, and user-facing routing.

## Consequences

### Positive

- The mode name matches what it does: PR review.
- The shared sandman skill routes stay consistent with the CLI surface.
- The rename removes an awkward outlier in the mode list.

### Negative

- Existing muscle memory for `sandman delegate-review` will break.
- The rename requires updates across docs, tests, and embedded skill paths.

### Neutral

- The underlying review loop behavior does not change.
- Installed skill trees will update on the next sync.
