# ADR-0045: Widened children-heading matcher for Specification detection

## Status

proposed

## Context

ADR-0021 introduced Specification expansion: Sandman detects a Specification by body shape, harvests candidate child issues from the body, comments, native sub-issues, and a search fallback, then verifies each candidate against a `## Parent` backlink. ADR-0034 amended the empty-child case to a single-row batch. ADR-0042 widened the **parent-section** matcher to any H2 whose heading contains the word "parent" (case-insensitive substring) so the threeterm umbrella-spec pattern (`## Parent area` heading inside each leaf child) is recognised.

The **children-section** matcher did not get the symmetric widening. `childrenHeadingPattern` in `internal/github/cli_client.go` still matches only the literal `## Children` and `## Child Issues` headings. A threeterm area-spec body (threeterm issue [#305](https://github.com/rafaelromao/threeterm/issues/305), reproduced below) lists its leaf children under `## Leaf children` paired with a markdown table:

```text
## Leaf children

| Slug | Issue |
| --- | --- |
| `01v1-rust-toolchain-and-cargo-build` | [#232](https://github.com/rafaelromao/threeterm/issues/232) |
```

Because the heading text is not `Children` or `Child Issues`, `ParseChildrenFromBody` returns an empty list, `IsSpecification` returns false, and the resolver passes the body through unchanged instead of expanding it to its leaf row `#232`. The implementor's contract on this ticket is the criterion that closes the gap: **issue 232 listed under a section containing the word `children` or `child` in the title should be enough to make Sandman consider it a possible Specification and try to handle it as such**.

The bullet parser that backs `ParseChildrenFromBody` also only recognises bullet-shaped lines (`- #N`, `[#N]`, `[text](url)`), so even with the heading widened, a children table would still slip through. The parser must accept the markdown table row shape used by threeterm area specs.

## Decision

1. **Widened children-heading matcher.** `childrenHeadingPattern` widens from `(?im)^\s*##\s+(?:children|child issues)\s*$` to `(?im)^\s*##\s+[^\n]*child[^\n]*\s*$` — any H2 whose heading text contains the substring `child` (case-insensitive). This mirrors the broadened `parentHeadingPattern` in `internal/batch/spec_parse.go`. The matcher widens but does not narrow; existing test sub-cases for `## Children` and `## Child Issues` (shorthand, full URL, URL with fragment, case-insensitive headings, inline-colon rejection) remain pinned and continue to pass.
2. **Markdown-table row issue pattern.** A new `tableRowIssuePattern` in `internal/github/cli_client.go` matches an issue reference inside a markdown table row (`|[^\n]*(...)|$`). The pattern is shape-compatible with `bulletIssuePattern` (same three capture groups for `#N`, `[#N]`, `[text](url)`), so `parseBulletsInSection` can reuse the same `issueNumberFromMatch` helper without per-pattern branching. `ParseChildrenFromBody` now calls `parseBulletsInSection(section, bulletIssuePattern, bulletLinePattern, tableRowIssuePattern)`.
3. **No verifier change.** The verifier (`HasParentSectionBacklinkTo` widened in ADR-0042) is unchanged. Candidates harvested from the widened section still pass the existing per-candidate `## Parent` check, so unrelated references in other sections (e.g. the planning-context link to a root spec in threeterm issue #305) are rejected by the verifier rather than accepted.
4. **No new GitHub operations.** The fix is purely a parsing-layer change. The `Client` interface in `internal/github/github.go` is unchanged. No new flags, no new config keys, no new tests outside `internal/github/cli_client_test.go` and `internal/batch/spec_test.go`.

## Consequences

### Positive

- A threeterm area-spec body (issue #305) now expands to its leaf row `#232` instead of being passed through unchanged. The implementor's stated criterion ("a section containing `children` or `child` should be enough to make Sandman consider it a possible spec and try to handle it as such") is satisfied.
- Spec authors can name the children-section heading freely as long as it contains the word `child` or `children`. `## Children`, `## Child Issues`, `## Leaf children`, `## Children in this area`, `## Child tasks`, `## Sub-children`, `## Each child issue with its slug` all work.
- Markdown-table children lists, used by threeterm area specs, are accepted by the same parser machinery as bullet lists, so the fix does not require a second pass.
- The verifier (ADR-0042) is unchanged, so unrelated planning-context references (e.g. threeterm #305 citing #58 as its parent spec) are still rejected at the per-candidate `## Parent` check rather than leaking through as false-positive children.

### Negative

- The children-section matcher is now looser than before. A heading like `## Why child safety matters` would today be excluded; after the change, it would be considered a children-section candidate. The verifier still requires the spec number to be cited in the candidate's `## Parent` section, so the false-positive ceiling is bounded by the verifier, not by the heading match — but a body that incidentally drops a children-section-shaped reference inside a heading that mentions "child" could be misread. The risk is small enough that the trade-off favours the broader matcher, given the threeterm pattern and the bounded-by-verifier check.
- The table-row pattern is permissive about table content: it matches any line that starts with `|`, contains an issue reference, and ends at the line boundary. Cells that happen to embed a `#N` reference in prose that is not a child declaration would now be picked up. The verifier still requires a `## Parent` backlink in the candidate, so the false-positive ceiling is again bounded by the verifier.

### Neutral

- `internal/github/cli_client.go` gains `tableRowIssuePattern` alongside the existing `bulletIssuePattern` and `bulletLinePattern`. The `parseBulletsInSection` helper accepts variadic patterns, so the existing `## Blocked by` heading (which still uses only the two bullet patterns) is unaffected.
- ADR-0042 §2 (the broadened parent-section matcher) is the symmetric counterpart of this ADR's §1. ADR-0021 step 2 (child discovery) is amended in place; the high-level flow described there (spec detection → harvest → verify → flatten → dedup) is unchanged. ADR-0044 (open-issue scan as last-resort harvest) is unchanged; its broadened `HasParentSectionBacklinkTo` filter still feeds candidates into the same verifier.
- The smoke and e2e tiers are unaffected because no CLI flag changes.