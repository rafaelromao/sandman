# Issue body formats

Sandman reads issue-body references when it builds dependency and Specification relationships. The formats below are accepted for blocker references and child issue references. Inline phrase parsing was retired because it caught incidental prose mentions across the tracker; the supported contracts are explicit headings and the GitHub-native data.

## Blocked by

A blocker list must use one of these H2 headings:

```text
## Blocked by
## Depends on
## Blocked-by
```

Accepted list entries include bare issue numbers, linked issue numbers, and titled issue links:

```text
- #123
- [#124](https://github.com/example/project/issues/124)
- [Issue title](https://github.com/example/project/issues/125)
```

A titled issue link may also be the only content on its line without a bullet:

```text
[Issue title](https://github.com/example/project/issues/126)
```

A bullet may include annotation after the issue reference. The annotation is ignored when it does not contain another issue reference:

```text
- [Issue #288](https://github.com/example/project/issues/288) (T2: URL tree and auth/CSRF seams)
- [Issue #289: T2 identity seams](https://github.com/example/project/issues/289)
- #290 — follow-up
```

The bullet prefix is required when trailing annotation follows a titled issue link. An unbulleted link followed by prose is not treated as a blocker reference. Only links containing an `/issues/<number>` path are accepted; unrelated URLs are ignored. The section ends at the next H2 heading.

Inline phrases such as `Blocked by #123`, `Depends on #123`, or `Blocked-by: #123` outside an explicit heading are NOT recognized as blocker declarations. They appear in many issue bodies as prose mentions (for example inside child-list annotations like `- #10 (blocked by #2319)`) and treating them as authoritative made unrelated parents appear blocked. The recommended migration is to move them under a `## Blocked by` heading.

## Children

A parent can list child issues under any H2 heading whose title contains the word `children` or `child` (case-insensitive), or contains `subissues`, `sub-issues`, or `sub issues`. The canonical headings below are the most common form; any `## Leaf children`, `## Child tasks`, `## Children in this area`, or `## Subissues for area-01` variant is also recognised as a children section. The widened match mirrors the broadened `## Parent` heading pattern in `internal/batch/spec_parse.go` and is what lets a threeterm-style body that lists leaf children under `## Leaf children` (issue #305) be detected as a specification and expanded, instead of being passed through unchanged.

```text
## Children
## Child Issues
## Leaf children
## Children in this area
## Subissues
## Sub-issues for area-01
## Sub Issues
```

Accepted list entries follow the same rules as `## Blocked by`: bare issue numbers, linked issue numbers, titled issue links, and bullets with trailing annotations. Markdown table rows that frame the issue reference with `|` delimiters are also accepted, so a children table under a `## Leaf children` heading feeds the parser that backs the rest of the bullet list. The section ends at the next H2 heading.

```text
## Children
- #201
- [Database setup](https://github.com/example/project/issues/202)
- [Issue #203: API seams](https://github.com/example/project/issues/203) (T2)

## Leaf children
| Slug | Issue |
| --- | --- |
| `01v1-rust-toolchain-and-cargo-build` | [#232](https://github.com/example/project/issues/232) |
```

Child references may also occur in surrounding body prose. Both `#N` shorthand and full issue URLs containing `/issues/N` are recognized. Text before or after a reference is allowed, so titles and trailing annotations are presentation text. Any additional `#N` or `/issues/N` reference in that text is also a separate candidate.

Child discovery also considers issue comments and GitHub-native sub-issue relationships. Structured entries under a recognized child section and native sub-issue relationships are explicit child declarations and do not require a child-side `## Parent` reference. Candidates from ordinary body prose, comments, and search results still require a matching parent-style reference; a parent-side declaration wins if the same candidate appears in both sources.

Inline phrases such as `Children: #123`, `Child Issues: #123`, or `Subissues: #123` are NOT recognized as authoritative child declarations. The recommended migration is to move them under a recognized H2 section.

## GitHub-native relationships

For `BlockedBy`, body references are merged with blocker relationships returned by GitHub's native issue API. The result is the union of both sources, with duplicate issue numbers kept once. A blocker found through either source participates in the same dependency resolution and execution rules.
