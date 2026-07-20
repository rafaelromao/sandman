# Issue body formats

Sandman reads issue-body references when it builds dependency and Specification relationships. The formats below are accepted for blocker references and child issue references.

## Blocked by

### Inline references

These inline phrases identify blockers:

```text
Blocked by #123
Depends on #123
Blocked-by: #123
Blocked by [#123](https://github.com/example/project/issues/123)
```

The phrase is case-insensitive. The issue number may be written as `#123` or as a Markdown link to an issue URL.

### Heading sections

A blocker list may use any of these H2 headings:

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

## Children

### Inline references

Child issues may be declared inline with these phrases:

```text
Children: #123
Child Issues: #123
children #123
child issues: #123
Children [#123](https://github.com/example/project/issues/123)
```

The phrase is case-insensitive. The issue number may be written as `#123` or as a Markdown link to an issue URL.

### Heading sections

A Specification can list child issues under a `## Children` or `## Child Issues` section:

```text
## Children
## Child Issues
```

Accepted list entries follow the same rules as Blocked by heading sections: bare issue numbers, linked issue numbers, titled issue links, and bullets with trailing annotations are all recognized. The section ends at the next H2 heading.

```text
## Children
- #201
- [Database setup](https://github.com/example/project/issues/202)
- [Issue #203: API seams](https://github.com/example/project/issues/203) (T2)
```

Child discovery also considers issue comments and GitHub-native sub-issue relationships (REST API `ListSubIssues`). Candidates are deduplicated in first-occurrence order and then checked against the child's `## Parent` reference.

## GitHub-native relationships

For `BlockedBy`, body references are merged with blocker relationships returned by GitHub's native issue API. The result is the union of both sources, with duplicate issue numbers kept once. A blocker found through either source participates in the same dependency resolution and execution rules.
