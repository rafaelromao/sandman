# ADR-0046: Widened parent-section matcher accepts "Part of" headings

## Status

accepted

## Context

ADR-0042 widened the parent-section matcher (`parentHeadingPattern` in `internal/batch/spec_parse.go`) from the literal `^##\s+Parent\s*$` to a substring match on the word "parent", so `## Parent`, `## Parent area`, `## Parent spec`, and `## Parents` are all recognised as parent sections. The widening made the threeterm umbrella-spec pattern work: each leaf child carries a `## Parent area` section that cites the umbrella spec among its references, and the verifier accepts the leaf.

ADR-0045 widened the children-section matcher on the parent side (any H2 containing `children` or `child` is a children section) so a threeterm area-spec body listing its leaves under `## Leaf children` is detected as a Specification. That fix is not sufficient end-to-end because the verifier on the candidate side is still anchored to the "parent" substring. The leaf issue [threeterm #232](https://github.com/rafaelromao/threeterm/issues/232) declares its parent under `## Part of #305` (not `## Parent`), so the widened children-heading detector harvests the leaf row, but the verifier `HasParentSectionBacklinkTo` rejects the candidate and the resolver logs `"running issue #305 as a regular issue (no children)"`. The expansion contract from issue #2486 / [threeterm #305](https://github.com/rafaelromao/threeterm/issues/305) — that a leaf listed under a `## Leaf children` heading should be considered a possible child and expanded — therefore fails.

The implementor's contract on the original ticket was the broader criterion: **issue 232 listed under a section containing the word `children` or `child` in the title should be enough to make Sandman consider it a possible Specification and try to handle it as such**. "Try to handle it as such" requires the verifier to accept the threeterm leaf style, not just the spec detector.

## Decision

1. **Widened parent-section matcher.** `parentHeadingPattern` widens from `(?im)^##\s+[^\n]*parent[^\n]*\s*$` to `(?im)^##\s+[^\n]*(?:parent|part of)[^\n]*\s*$`. The matcher now accepts any H2 whose heading text contains the substring `parent` (case-insensitive) **or** the literal word sequence `part of` (case-insensitive substring, with the literal space). The widening is the symmetric counterpart of the broadened `childrenHeadingPattern` introduced in ADR-0045: where the children detector accepts any H2 containing `children`/`child`, the parent detector accepts any H2 starting `parent`/`part of`. Both sides of the parent/child heading contract move together.
2. **"Part of" is a literal substring, not just "part".** The alternative uses the literal `part of` rather than just `part`, so `## Partner` and `## Particular implementation` are still rejected — the substring "part" alone would create false positives in headings like `## Partner` (p-a-r-t-n-e-r) that do not declare a parent relationship. The literal sequence requires the trailing "of" to keep the matcher scoped to headings that semantically declare a parent. The ceiling is verified by `TestHasParentSectionBacklinkTo_ParticularHeadingDoesNotMatch`.
3. **No verifier semantics change.** `HasParentSectionBacklinkTo` still requires the spec number to be cited in the matched section. A `## Part of #305` heading that cites `#305` accepts the candidate as a child of `305`; a `## Part of #999` heading is rejected as a candidate for `305`. Multi-ref and cross-section union semantics (ADR-0042 §3) are unchanged.
4. **No `Client` interface change.** The fix is a parser-layer regex update. The `github.Client` interface is unchanged. No new flags, no new config keys. No new files in `internal/cmd`, `internal/github`, or `internal/sandbox`.

## Consequences

### Positive

- The end-to-end contract from [threeterm #305](https://github.com/rafaelromao/threeterm/issues/305) / #232 now works: the parent body lists the leaf under `## Leaf children`, the leaf body declares its parent under `## Part of #305`, and the resolver expands `#305` to `[232]` as the implementor expected.
- The verifier continues to enforce the spec-citation requirement, so unrelated references in non-parent sections are still rejected. The planning-context `#58` reference in threeterm #305's body (where `#58` is the root spec, not the area spec) is still filtered out by the verifier because the `#58` body backlogs the root spec (`## Parent\n\n#1\n\n...`) rather than `#305`.
- The literal `part of` substring keeps the false-positive ceiling tight: `## Partner`, `## Particular implementation`, and `## Participating teams` are all rejected because they do not start with the literal `part of` sequence.

### Negative

- The verifier is now slightly looser than before. A heading like `## Part of the larger plan` would today be considered a parent-section candidate; after the change, it would be matched. The verifier still requires the spec number to be cited in the section, so the false-positive ceiling is bounded by the per-candidate `## Parent` check rather than by the heading match — but a body that incidentally drops the spec number inside a `## Part of` heading could be misread. The risk is small enough that the trade-off favours the broader matcher, given the threeterm pattern and the bounded-by-verifier check.
- The widening is asymmetric in two ways: the children-side matcher (ADR-0045) accepts any H2 containing `children`/`child`, but the parent-side matcher requires the literal `part of` rather than just `part`. The asymmetry is deliberate — `parent` and `part of` are both unambiguous parent-declaration vocabulary, while `part` alone collides with too many innocuous headings. A future ADR can broaden the parent matcher further if new synonyms (e.g. `sub-spec of`) appear in the wild.

### Neutral

- `parentHeadingPattern` is the symmetric counterpart of `childrenHeadingPattern` (ADR-0045). The matcher widens but does not narrow: existing test sub-cases for `## Parent` (shorthand, full URL, URL with fragment, `## parent` lowercase, missing section, no reference, `### Parent` H3 rejection, indented next-heading boundary, multi-ref, multi-section union) remain pinned and continue to pass.
- ADR-0021 step 3 (parent verification) is amended in place by this ADR; the high-level flow described there (spec detection → harvest → verify → flatten → dedup) is unchanged. ADR-0045 §1 (the broadened children-heading matcher) is the symmetric counterpart on the children side; together they close the gap surfaced by issue #2486.
- The smoke and e2e tiers are unaffected because no CLI flag changes.