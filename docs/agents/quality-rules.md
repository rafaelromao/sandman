# Quality Rules for the PR Reviewer

This doc explains how the PR review prompt (`internal/prompt/default_pr_review_prompt.md`)
applies the quality rules in `internal/prompt/quality_rules.md`, and how to
edit those rules without breaking the reviewer.

## What the reviewer checks

The reviewer's primary contract is the **linked issue's acceptance criteria**.
The quality rules are a **secondary smoke test** that catches design-smell
regressions the issue might not have called out — deep nesting, OC and SOLID
violations, mixed concerns, etc.

The rules never block the PR on their own. They escalate to `Important` only
when the **60% threshold** is crossed (see
[`quality_rules.md`](../../internal/prompt/quality_rules.md) for the
counting model).

## How the reviewer judges a rule

For each rule in `quality_rules.md`, the reviewer must decide whether the
rule's `Applies to` tag matches the file's language family:

- **`[all]`** — applies to any language with the relevant construct. The
  reviewer applies the rule regardless of language.
- **`[OOP]`** — applies to languages with classes and methods. The reviewer
  skips these for Go, Rust, C, or pure-functional files.

Skipped rules do not count toward the 60% threshold. This is the only
language-sensitivity the rules file encodes.

## Severity flow

1. The reviewer counts distinct smelly locations (`n`) and total locations
   reviewed (`t`).
2. Each smelly location produces a finding citing every rule it breaks.
3. If `n / t < 0.6`, every finding is a `Nit` (or omitted).
4. If `n / t >= 0.6`, at least one representative finding is filed as
   `Important` and references this doc plus `quality_rules.md`. Other
   findings stay `Nit` or `Important` at the reviewer's discretion.
5. Quality findings are **never** `Blocking`. A `Blocking` severity must
   always reference an acceptance criterion, a documented standard, or a
   correctness/safety defect — never a quality rule.

## Editing the rules

`internal/prompt/quality_rules.md` is the single source of truth. The
reviewer prompt references it by path. To change a rule:

1. Edit `internal/prompt/quality_rules.md`.
2. Update the rule's `Applies to` tag if the rule's scope changed.
3. Do **not** move the file — the prompt hardcodes the relative path
   `internal/prompt/quality_rules.md`.
4. Do **not** add new sections without a matching update to this doc, or
   the reviewer and the docs will drift.

## Adding a new rule

1. Add the rule to the right section (complexity signals, OC, or SOLID).
2. Pick the narrowest `Applies to` tag that covers the languages you care
   about. Default to `[OOP]`; widen to `[all]` only if the rule's spirit
   holds for non-OO languages too.
3. State the rule in **reviewer-actionable** language, not abstract
   principle. Bad: *"a class should have only one reason to change"*.
   Good: *"flag a class/module whose purpose cannot be stated in one
   short sentence without 'and'"*.
4. If the new rule changes what counts as a "smelly location", update the
   Counting model section.

## Removing a rule

1. Remove the rule from `quality_rules.md`.
2. Remove any local references in this doc.

No version bump is required — the rules file is consumed by reference, not
imported as code.

## Out of scope for the rules

- **Performance regressions** — covered by the correctness/safety check
  in the review prompt, not the quality rules.
- **Style preferences** — explicitly excluded from the reviewer's scope
  (formatting, import order, comment phrasing, renames without behaviour
  impact).
- **Domain-specific heuristics** — the rules are language-agnostic.
  Domain smells belong in the linked issue's acceptance criteria or in an
  ADR cited from the issue body.
