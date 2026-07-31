# ADR-0042: Robust Specification detection and generalized parent-section matching

## Status

accepted

## Context

ADR-0021 introduced Specification expansion: Sandman detects a Specification by body shape, harvests candidate child issues from the body, comments, native sub-issues, and a search fallback, then verifies each candidate against a `## Parent` backlink that must cite the spec. ADR-0034 amended the empty-child case to a single-row batch with an operator-visible log line.

Two structural cases have surfaced since that make today's contract brittle.

First, candidate harvesting today walks the entire issue body for issue references with no carve-out for blocker-shaped sections. The body of an issue commonly carries a `## Blocked by` (or `## Depends on` / `## Blocked-by`) heading that names the issues this work is waiting on. Those references are dependency edges, not child declarations, and a real Specification body can mix `## Child Issues` and `## Blocked by` headings — today's resolver harvests both indiscriminately, so a blocked-by number can be added as a candidate child if the parent happens to be a Specification. The carve-out should mirror the existing heading vocabulary that `parseBlockedByHeading` recognizes, so the source of truth stays in one place.

Second, the parent-verification step is anchored to the literal H2 heading `## Parent`. Real-world specs frequently use a different heading name. The umbrella-spec pattern in threeterm (issue [#58](https://github.com/rafaelromao/threeterm/issues/58) listing parent-area issues #1–#22 alongside the actual vertical-slice children #232–#253) carries the parent declaration under `## Parent area` in the child issues ([#232](https://github.com/rafaelromao/threeterm/issues/232), [#234](https://github.com/rafaelromao/threeterm/issues/234), and the rest): the H2 text contains the word `parent` but does not match `^## Parent$`, so the verifier rejects the candidates even though the body shape makes it clear the parent is the spec.

Real candidate issues like #234 cite both their intermediate parent (`#61`) and the spec (`#58`) inside the same `## Parent area` section. Today, `ExtractParentReference` returns `(0, false)` when a candidate's `## Parent` section contains multiple references, so even fixing the heading match would still drop these multi-ref bodies. The verifier must accept a candidate whose parent section contains the spec among other references.

## Decision

1. **Blocker-section carve-out for body harvesting.** The candidate-discovery path walks H2 sections of the issue body in order and skips any section whose heading text equals `Blocked by`, `Depends on`, or `Blocked-by` (case-insensitive). The remaining non-blocker sections supply the prose-references and the per-section bullet-reference harvest that today come from `ExtractIssueReferences(body)` and `github.ParseChildrenFromBody(body)`. Comment references, native sub-issues, and the search fallback are unchanged. Detection (the spec-shape gate) is unaffected — the carve-out only governs what is harvested from the body.
2. **Generalized parent-section matcher.** `parentHeadingPattern` widens from `(?im)^##\s+Parent\s*$` to a pattern that matches any H2 whose heading text contains the word `parent` (case-insensitive substring). All existing test sub-cases for `## Parent` (shorthand, full URL, URL with fragment, `## parent` lowercase, missing section, no reference, `### Parent` H3 rejection, indented next-heading boundary) remain pinned and continue to pass — the matcher widens but does not narrow. The extraction semantics stay single-section, single-reference for the existing `ExtractParentReference` (used today by the verifier probe); a new helper `HasParentSectionBacklinkTo(body, parent)` collects all issue references across every matching parent-section H2 and returns true iff `parent` is among them.
3. **Multi-ref parent-section acceptance.** When a candidate's parent section cites several issues and the originating spec is one of them, the candidate is accepted. This unlocks the threeterm pattern where each child cites both the intermediate parent area and the umbrella spec inside the same `## Parent area` heading.
4. **Backwards compatibility.** All 47 existing tests in `internal/batch/spec_test.go` continue to pass unchanged. New tests pin the new behaviour.

## Consequences

### Positive

- A real-world hierarchical spec like threeterm #58 expands to its vertical-slice children (#232–#253) instead of falling through to the empty-child carve-out and running as a single regular issue, which was the operational friction that motivated the change.
- The blocker-section carve-out removes a class of false-positive child candidates that today could be harvested from `## Blocked by` heads; the existing `parseBlockedByHeading` vocabulary is the single source of truth.
- Spec authors can name the parent-section heading freely as long as it contains the word "parent" — `## Parent`, `## Parent area`, `## Parent spec`, `## Parents`, even sub-headings like `## Parent area notes` all work.

### Negative

- The parent-section matcher is now looser than before. A heading like `## Why parent matters` would today be excluded; after the change, it would be considered a parent-section candidate. The verifier still requires the spec number to be cited in the section, so the false-positive ceiling is bounded by the operator's authorship discipline — but a body that incidentally drops the spec number inside a prose section whose heading mentions "parent" could be misread. The risk is small enough that the trade-off favours the broader matcher, given the threeterm pattern and the bounded-by-spec-number check.
- Multi-ref parent sections are accepted where today they would be rejected. Existing bodies that happen to have multiple refs in `## Parent` and rely on the rejection behaviour will now be accepted if the spec is among them. The verifier probe (`ExtractParentReference`) still returns `(0, false)` for multi-ref, so the broader acceptance is gated behind the new `HasParentSectionBacklinkTo` call site and the existing test for multi-ref single-section cases will not silently broaden. We add explicit new tests for the multi-ref case so the new behaviour is pinned.
- The verifier is no longer "the candidate points to the spec, exclusively." Code that introspects `## Parent` for other reasons (none today, but future risk) must use the same matcher and must accept multi-ref.

### Neutral

- `internal/batch/spec_parse.go` gains `parentSectionReferences`, `HasParentSectionBacklinkTo`, `isBlockerHeading`, and `bodyReferencesOutsideBlockerSections`. `ExtractParentReference` keeps its existing signature and is preserved for callers that probe "is there any parent backlink?" without needing the multi-ref / multi-section semantics.
- ADR-0021 step 2 (child discovery) and step 3 (parent verification) are amended in place by this ADR; the high-level flow described there (spec detection → harvest → verify → flatten → dedup) is unchanged. ADR-0021's wording around "no other gate" still applies at the spec-detection level; this ADR tightens only the harvest and verify gates.
- The smoke and e2e tiers are unaffected because no CLI flag changes.
- **Bidirectional expansion is preserved.** When a child cites both an intermediate parent area and the umbrella spec in the same parent-section, `Resolve` accepts the child under either expansion and the `addUnique` closure in `Resolve` dedupes to a single output row when both expansions run side-by-side. Real-world example: threeterm #232 cites both #59 and #58 in its `## Parent area` section, and `sandman run #58 #59` produces `[232, …deduped]` rather than `[…, 232, 232, …]`. When a child only cites one of the two, the other expansion correctly drops it (the verifier rejects candidates whose parent section doesn't include the originating spec). This is the existing dedup machinery under the new parent-section matcher; no extra logic is added, but a future change to "smart merging" or "ancestor-set skip" heuristics should not be introduced without revisiting this paragraph.
