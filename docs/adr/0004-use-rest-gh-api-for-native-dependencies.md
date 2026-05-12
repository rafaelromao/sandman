# ADR-0004: Use GitHub REST API via `gh api` for Native Dependency Queries

## Status

accepted

## Context

To fetch GitHub's native issue dependency relationships (the `blocked_by`/`blocking` fields visible in the issue UI), we need to choose between:
1. **REST API via `gh api`**: Uses the existing CLI-based client pattern, works with standard preview headers. The `issue_dependencies_summary` field in the GET issue response provides counts, but the actual blocking issue numbers require an additional endpoint or a preview media type.
2. **GraphQL API via `gh api graphql`**: More precise queries, can fetch exactly the blocking issue numbers in one request. However, it introduces a second query language and client pattern into the codebase.

The `gh` CLI's `issue view --json` does not currently expose dependency fields. We must use `gh api` directly.

Alternatives considered:
- **GraphQL**: More efficient for nested data, but adds complexity to `CLIClient` which is currently REST-only.
- **REST with `issue_dependencies_summary`**: Only gives counts, not issue numbers. Not sufficient.
- **REST with a dedicated dependencies endpoint or preview header**: Gets actual issue numbers, stays within the existing REST paradigm.

## Decision

We will use `gh api repos/{owner}/{repo}/issues/{number}` with the `application/vnd.github+json` media type (which includes `issue_dependencies_summary`) and, if the actual blocker numbers are not present in that response, fall back to querying the issue timeline/events API for `cross-referenced` events with the `marked_as_duplicate`/`blocked_by` event types. If GitHub's API surface for this is still evolving, we will prefer the most stable REST endpoint that returns actual blocker issue numbers.

Specifically:
- `CLIClient.FetchIssue` will call `gh api repos/{owner}/{repo}/issues/{number}` and parse the response for both `issue_dependencies_summary` (as a hint) and any nested dependency arrays.
- If the REST response does not include the actual blocker numbers directly, we will use `gh api repos/{owner}/{repo}/issues/{number}/events` and filter for events of type `cross-referenced` or the dependency-specific event types.

This keeps the client in the existing `gh api` REST pattern without introducing GraphQL.

## Consequences

### Positive

- Consistent with existing `CLIClient` implementation.
- No GraphQL schema or query language to maintain.
- Works with existing `gh` CLI authentication.

### Negative

- May require multiple REST calls per issue (issue + events) to get full blocker numbers.
- If GitHub changes the event types or preview headers, the parser may break silently.

### Neutral

- If `gh issue view --json` later adds dependency fields, we can migrate `FetchIssue` without changing the public interface.
