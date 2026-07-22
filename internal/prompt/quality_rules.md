# PR Review Quality Rules

Smoke-test rules the PR reviewer applies to the diff in addition to the
acceptance criteria, ADRs, and correctness checks defined in the review
prompt.

These rules are **secondary**. A finding under these rules never blocks the PR
on its own — the issue's acceptance criteria still take precedence. Severity
is assigned per-location and never reaches `Blocking`.

## Counting model

- Count **distinct code locations** that smell, not rule-touches. A single
  function that violates several rules is one finding citing each rule it
  breaks.
- "The diff" means every file added or modified by the PR, not the whole
  repo.
- The unit of a "location" is a top-level function/method. For files without
  functions (configs, schemas, top-level scripts), a location is a
  top-level block of logic that contains control flow.

## Construct tags

Each rule below carries one or more **construct tags**. Apply a rule when the
diff uses the construct it names. A file can match more than one tag.

- `[control-flow]` — applies to functions, methods, lambdas, scripts,
  branches, loops, exception paths, or pattern matches in any language.
- `[functional]` — applies to higher-order functions, callbacks, pipelines,
  comprehensions, recursion, or algebraic/pattern-matched transformations.
- `[OOP]` — applies to stateful domain objects, inheritance, polymorphic
  contracts, or public class APIs. For Go, Rust, or pure-functional
  languages, skip these rules unless the changed code defines a stateful
  object or a public contract.
- `[public-api]` — applies to exported or externally consumed functions,
  methods, types, modules, or schemas.

## Metrics

Thresholds below are **signals**, not pass/fail. Reviewers must state
explicitly which analyzer they used (or that they made a manual
assessment). Per-location severity is described after the table.

1. **Cognitive complexity**: a configured signal for each changed function,
   method, lambda, or top-level control-flow block. Default threshold **15**;
   25 for C-family languages (C, C++, Objective-C) when no stricter setting
   exists. See the Cognitive Complexity white paper for the precise counting
   rules.
2. **Cyclomatic complexity**: a configured signal for the same units. Default
   threshold **20** when repository tooling provides none. Apply the
   language-specific counting definition; do not hand-convert a boolean
   operator count into a cyclomatic complexity.
3. **tool precedence**: this is the rule that resolves conflicts between
   analyzer output and manual estimates. When a repository configures a
   linter or analyzer, its reported metric and threshold win.
4. **No metric theatre**: do not request extraction merely to lower a score.
   Request a change only when a named decision, branch, effect boundary, or
   domain transformation can become clearer.

## Per-finding severity

Assign severity per changed location, not per rule.

| Evidence | Default action |
| --- | --- |
| A construct-specific readability concern with a concrete, small fix | `Nit` or omit |
| Cognitive complexity above the configured signal threshold, or cyclomatic complexity above its configured signal threshold, plus a concrete explanation of why the changed control flow is hard to verify or modify | `Important` |
| A metric breach without a demonstrated readability or change-risk concern | Report in the metrics summary only |

## Complexity signals — `[control-flow]`

1. **Deep nesting** — 4 or more levels of `if`/`for`/`while`/`try` inside
   each other.
2. **Long function** — roughly 50+ lines doing more than one thing.
3. **Arrow / growing-indent** — guard clauses piled on top of each other,
   or happy path indented further than the error path.
4. **Long switch / if-else chain** — many cases with non-trivial bodies, or
   fallthrough across cases.
5. **Mixed concerns** — input parsing, business logic, and I/O all in the
   same function body.

## Functional code — `[functional]`

1. **Nested transformation**: flag nested callbacks, lambdas,
   comprehensions, or recursive branches when an intermediate transformation
   has domain meaning but is unnamed, or when nesting hides error/effect
   order. Do not flag a short, conventional pipeline.
2. **Effect boundary**: flag a pipeline that interleaves I/O, state
   mutation, asynchronous scheduling, or exception handling with substantial
   pure transformation such that order and failure behavior cannot be
   reasoned about locally.
3. **Pattern-match contract**: for public transformations, flag a
   non-exhaustive or catch-all pattern when a new variant can silently take
   an unintended path and the compiler or repository conventions do not
   already enforce exhaustiveness. Use a correctness severity when it can
   actually fail at runtime.
4. **Opaque composition**: flag point-free or combinator-heavy code only when
   a reader cannot identify the input, output, or effect from the surrounding
   names and types. Prefer a named local transformation over mechanically
   unrolling a pipeline.

## SOLID

1. **Single Responsibility (SRP)** `[OOP]` `[public-api]`: flag a class/module whose purpose cannot be stated in one short sentence without "and".
2. **Open/Closed (OCP)** `[public-api]`: flag conditionals only when an established, repeated contract is already present and the change duplicates a known extension seam. Skip for trivial changes.
3. **Liskov Substitution (LSP)** `[OOP]`: flag an override or public interface only with a concrete caller-visible contract violation, such as a strengthened precondition, returns narrower types, incompatible no-op, or surprising exception. Skip for documented intentional narrowing.
4. **Interface Segregation (ISP)** `[OOP]`: flag a class implementing an interface whose methods it does not use, or a public API surface whose callers only touch a subset.
5. **Dependency Inversion (DIP)** `[OOP]`: flag a high-level module importing or constructing a concrete low-level dependency directly. Concrete constructors and `init` functions are out of scope for this rule.

## Mutable state — `[OOP]` `[public-api]`

Flag exposed mutable instance state when mutation can bypass the type's
invariant. Exempt immutable value types, DTOs, schemas, and intentionally
public data carriers.

## Public API contract — `[public-api]`

1. **Naming**: flag a name only when its scope makes its meaning
   ambiguous. Do not flag conventional short local names, loop indices, or
   receivers.
2. **Members**: when a type is overloaded or has multiple members with
   similar shapes, parameter names, order, and semantics must remain
   consistent across members.

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

If `.sandman/reviews/quality-rules.md` is absent from the repository being
reviewed (the review daemon materialises the file into the per-PR worktree
before launching the agent), render exactly:

> `Quality rules unavailable in this repository; no built-in quality-rule evaluation was applied.`

Do not cite Sandman's internal path in another project. The reviewer may
still apply the repository's own documented standards under the existing
standards review.
