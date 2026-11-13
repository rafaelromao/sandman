---
name: sandman-pr-review
description: Compatibility entrypoint for the standalone pull-request review workflow. Delegates to sandman-review-cycle without owning a second review contract or lifecycle state.
---

# PR Review Compatibility

`sandman-pr-review` is retained for callers that already load the historical
skill name. It is only a compatibility facade: load `sandman-review-cycle` and
follow that skill's current standalone contract in full.

The canonical cycle owns CI waiting, guarded request delivery through
`sandman-review-request`, request-scoped observation and classification,
feedback application, re-request rules, bounded diagnostics, and all
standalone in-memory state. This facade must not copy those procedures, create
a competing state model, write managed lifecycle artifacts, or ask the caller
to manually translate to the next capability.

When this facade is loaded, immediately hand off the repository, pull-request
number, current head, configured `{{REVIEW_COMMAND}}`, and the current
standalone context to `sandman-review-cycle`. A restart begins from live
pull-request state; it does not import or trust managed lifecycle state.
