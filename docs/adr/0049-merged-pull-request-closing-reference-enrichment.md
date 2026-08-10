# ADR-0049: Merged Pull-Request Closing-Reference Enrichment

## Status

proposed

## Context

ADR-0004 establishes REST as the API style for native issue dependency queries. Merged pull requests expose a separate native relationship, `closingIssuesReferences`, which is not available in the supported REST pull-request payload. The orchestrator needs that relationship when confirming that a merged pull request completed the tracked issue, while still retaining body-based closing intent if metadata enrichment is unavailable.

## Decision

Keep native issue dependency queries REST-only as decided by ADR-0004. For merged pull-request verification only, allow `gh api graphql` to enrich the parsed pull request with native closing references. This enrichment is best-effort: a GraphQL failure must return the already parsed pull request so its body can be used as a fallback.

This decision supersedes ADR-0004 only for merged pull-request closing-reference metadata; ADR-0004 remains authoritative for native issue dependencies.

## Consequences

### Positive

- Merged pull-request verification can recognize all native closing references.
- A transient metadata failure does not discard body-based closing intent.
- Native issue dependency queries retain their existing REST-only architecture.

### Negative

- The CLI client maintains one narrowly scoped GraphQL query in addition to its REST calls.
- GitHub schema changes may require updating the merged pull-request enrichment query.
