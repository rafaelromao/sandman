# ADR-0051: Explicit child declarations authorize Specification edges

## Status

proposed

## Context

Specification expansion historically required every discovered candidate to repeat the parent relationship in a child-side `Parent` section. That made the parent-side `Children` declaration and GitHub's native sub-issue relationship insufficient on their own, even though both are explicit relationship data. It also made a parent run as a regular issue when its children omitted or contradicted the redundant backlink.

The resolver must still reject incidental references found in ordinary prose, comments, and search results. Those sources do not express an explicit parent-owned child edge and need the existing backlink verification to distinguish a real child from context.

## Decision

1. A structured entry under a recognized `Children`, `Child Issues`, or `Subissues` H2 is an **explicit child declaration**. Explicit declarations authorize the parent-to-child edge without requiring a child-side backlink.
2. The children heading matcher keeps its existing case-insensitive H2 forms containing `child` or `children` and additionally accepts headings containing `subissues`, `sub-issues`, or `sub issues`, with optional surrounding heading text. H3-or-deeper headings do not match.
3. Explicit child declarations use the existing structured-entry grammar: bullets, titled issue links, trailing annotations, and markdown table rows. Plain prose references under a matching heading remain ordinary references unless they are parsed as structured entries.
4. Native GitHub sub-issue relationships are explicit child declarations and authorize their children without a child-side backlink. No new GitHub API operation is introduced.
5. Explicit declarations from the body and native sub-issue relationships are unioned with existing candidates in first-occurrence order. A later explicit source upgrades a candidate that was first found in prose without changing its position; duplicates still produce one child row.
6. An explicit declaration wins over an absent or conflicting child-side backlink without warning. A backlink remains useful corroborating metadata and remains the acceptance signal for ordinary body prose, comments, search results, and open-issue-scan candidates.
7. Empty matching sections remain non-detecting. Authorized nested Specifications still recurse, and existing ancestor filtering, user-input bypass, child fetching/errors, retained parent rows, in-memory completion gates, closed-child filtering, and cycle bounds remain unchanged.

## Consequences

### Positive

- Parent-owned planning is sufficient to expand a Specification; child bodies no longer need duplicated metadata.
- Native GitHub sub-issues behave as first-class relationships.
- Incidental prose, comments, search results, and scan results retain their existing false-positive protection.
- Source union, ordering, deduplication, and retained-parent execution semantics remain stable.

### Negative

- A stale or overly broad explicit child list can authorize a child without a contradictory-backlink warning; the parent-side declaration is intentionally authoritative.
- Existing tests and documentation that describe universal child-side backlink verification must be updated to the new source-specific contract.

### Neutral

- Parent-style backlink parsing remains available for ordinary body, comment, search, and open-issue-scan candidates and as optional corroborating metadata for explicit child declarations.

### Relationship

This proposal clarifies the candidate source scope described by ADR-0021 and leaves accepted ADR records immutable. If accepted, the repository ADR workflow can mark any affected predecessor records as superseded.
