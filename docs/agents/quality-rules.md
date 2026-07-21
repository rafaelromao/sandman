# Quality Rules for the PR Reviewer

This doc explains how the PR review prompt (`internal/prompt/default_pr_review_prompt.md`)
applies the quality rules in `internal/prompt/quality_rules.md`, and how to
edit those rules without breaking the reviewer.

## What the reviewer checks

The reviewer's primary contract is the **linked issue's acceptance criteria**.
The quality rules are a **secondary smoke test** that catches design-smell
regressions the issue might not have called out — deep nesting, control-flow
clarity, mixed concerns, mutable state, etc.

The rules never block the PR on their own. Severity is assigned per changed
location, never per ratio of smells to locations. A `Blocking` severity must
always reference an acceptance criterion, a documented standard, or a
correctness/safety defect — never a quality rule.

## How the reviewer judges a rule

For each rule in `quality_rules.md`, the reviewer must decide whether one of
its **construct tags** matches the changed code:

- **`[control-flow]`** — functions, methods, lambdas, scripts, branches,
  loops, exception paths, or pattern matches.
- **`[functional]`** — higher-order functions, callbacks, pipelines,
  comprehensions, recursion, or algebraic/pattern-matched transformations.
- **`[OOP]`** — stateful domain objects, inheritance, polymorphic
  contracts, or public class APIs. Skip for Go, Rust, or pure-functional
  files unless the changed code actually defines a stateful object or a
  public contract.
- **`[public-api]`** — exported or externally consumed functions, methods,
  types, modules, or schemas.

A file may match more than one tag. Skipped rules do not contribute to any
finding; they are not counted at all.

## Severity flow

1. The reviewer collects evidence per changed location: a construct-specific
   readability concern, a complexity metric above the configured threshold
   with a concrete explanation, or nothing at all.
2. Each smelly location produces a finding citing the rule it breaks and
   its construct tag.
3. Construct-specific readability concerns are filed as `Nit` or omitted.
4. Cognitive or cyclomatic complexity above the configured threshold with a
   concrete explanation is filed as `Important`.
5. Metric breaches without a demonstrated readability concern are reported
   in the Quality check summary only; they do not become findings.
6. Quality findings are **never** `Blocking`.

## Per-finding severity table

The full table lives in `quality_rules.md` under `## Per-finding severity`:

| Evidence | Default action |
| --- | --- |
| Construct-specific readability concern with a concrete fix | `Nit` or omit |
| Cognitive or cyclomatic complexity above the configured signal threshold with a concrete explanation of why the changed control flow is hard to verify or modify | `Important` |
| Metric breach without a demonstrated readability or change-risk concern | Report in the metrics summary only |

## Tool precedence

Tool precedence is also defined in `quality_rules.md`:

1. Cognitive complexity default threshold **15** (25 for C-family languages
   when no stricter setting exists).
2. Cyclomatic complexity default threshold **20** when repository tooling
   provides none.
3. When a repository configures a linter or analyzer, its reported metric
   and threshold win over the defaults.
4. Do not request extraction merely to lower a score; request a change
   only when a named decision, branch, effect boundary, or domain
   transformation can become clearer.

## Editing the rules

`internal/prompt/quality_rules.md` is the single source of truth. The
reviewer prompt references it by path. To change a rule:

1. Edit `internal/prompt/quality_rules.md`.
2. Update the construct tags if the rule's scope changed.
3. Do **not** move the file — the prompt hardcodes the relative path
   `internal/prompt/quality_rules.md`.
4. Do **not** add new sections without a matching update to this doc, or
   the reviewer and the docs will drift.

## Adding a new rule

1. Add the rule to the right section (metrics, complexity signals,
   functional, SOLID, mutable state, or public API contract).
2. Pick the narrowest construct tag set that covers the languages you
   care about. Default to `[control-flow]`.
3. State the rule in **reviewer-actionable** language, not abstract
   principle. Bad: *"a class should have only one reason to change"*.
   Good: *"flag a class/module whose purpose cannot be stated in one
   short sentence without 'and'"*.
4. If the new rule changes how severity is assigned, update the
   `Per-finding severity` table in `quality_rules.md`.

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

## When the rules file is absent

If `internal/prompt/quality_rules.md` is absent from the repository being
reviewed, the prompt renders the verdict `Quality rules unavailable in this
repository; no built-in quality-rule evaluation was applied.` and skips
the Quality check sub-sections. The reviewer may still apply the
repository's own documented standards under the existing standards review.
