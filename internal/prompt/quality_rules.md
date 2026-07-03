# PR Review Quality Rules

Smoke-test rules the PR reviewer applies to the diff in addition to the
acceptance criteria, ADRs, and correctness checks defined in the review prompt.

These rules are **secondary**. A finding under these rules never blocks the PR
on its own — the issue's acceptance criteria still take precedence. A finding
here only ever escalates to `Important` when the threshold below is crossed.

## Counting model

- Count **distinct code locations** that smell, not rule-touches. A single
  function that violates several rules is one finding citing each rule it
  breaks.
- "The diff" means every file added or modified by the PR, not the whole
  repo.
- The unit of a "location" is a top-level function/method. For files without
  functions (configs, schemas, top-level scripts), a location is a
  top-level block of logic that contains control flow.

## Threshold

Let `n` be the number of distinct smelly locations in the diff, and `t` the
total number of locations reviewed.

- If `n / t >= 0.75` → at least one representative finding must be filed as
  `Important`. The rest stay `Important` or `Nit` at the reviewer's
  discretion.
- If `n / t < 0.75` → findings are `Nit` or omitted.

In both cases, every `Nit` must cite the specific rule it breaks.

## Per-language applicability

For each rule below, the reviewer must judge whether it applies to the
language of the file being reviewed. Skip rules whose `Applies to` tag does
not match the file's language family. The tags are:

- `[all]` — applies to any language with the relevant control flow or
  construct.
- `[OOP]` — applies to languages with classes and methods (Java, C#, C++,
  Python, Ruby, PHP, Kotlin, Swift, etc.). For Go, Rust, or pure-functional
  languages, skip these rules.

## Complexity signals — `[all]`

1. **Deep nesting** — 4 or more levels of `if`/`for`/`while`/`try` inside
   each other.
2. **Long function** — roughly 50+ lines doing more than one thing.
3. **Arrow / growing-indent** — guard clauses piled on top of each other,
   or happy path indented further than the error path.
4. **Long switch / if-else chain** — many cases with non-trivial bodies, or
   fallthrough across cases.
5. **Boolean-heavy condition** — three or more boolean operators in a
   single condition.
6. **Mixed concerns** — input parsing, business logic, and I/O all in the
   same function body.

## Object Calisthenics

1. **One level of indentation per method** — flag methods with 2+ nesting
   levels. `Applies to: [OOP]`
2. **Don't use the `else` keyword** — flag `if/else` blocks; prefer early
   return or guard clauses. `Applies to: [all]`
3. **Wrap all primitives and strings** — flag raw primitives with domain
   meaning passed across function boundaries (e.g. `string userId` instead
   of a `UserID` type). Skip for one-off locals.
   `Applies to: [OOP]`
4. **First-class collections** — flag a class whose main field is a
   collection of related objects and that has no other behaviour. Skip for
   transient locals. `Applies to: [OOP]`
5. **One dot per line** — flag chained method calls (`.a().b().c()`) and
   long property chains (`obj.a.b.c`). Skip for fluent builders.
   `Applies to: [all]`
6. **Don't abbreviate** — flag short cryptic names (`usr`, `mgr`, `tmp`,
   `data`, `info`) used outside local scopes. `Applies to: [all]`
7. **Keep entities small** — flag classes/modules over ~150 lines or with
   more than ~10 public methods. `Applies to: [OOP]`
8. **No classes with more than two instance variables** — flag classes
   with 3+ fields. Skip for value objects and DTOs.
   `Applies to: [OOP]`
9. **No getters/setters/properties/public fields** — flag exposed mutable
   state outside of value objects and DTOs. `Applies to: [OOP]`

## SOLID

1. **Single Responsibility (SRP)** — flag a class/module whose purpose
   cannot be stated in one short sentence without "and".
   `Applies to: [OOP]`
2. **Open/Closed (OCP)** — flag behaviour added by editing existing code
   with conditionals rather than extending it. Skip for trivial changes.
   `Applies to: [all]`
3. **Liskov Substitution (LSP)** — flag an override that narrows the
   parent's contract (throws where parent doesn't, returns narrower
   types, silently no-ops). Skip for documented intentional narrowing.
   `Applies to: [OOP]`
4. **Interface Segregation (ISP)** — flag a class implementing an
   interface whose methods it does not use, or a public API surface
   whose callers only touch a subset. `Applies to: [OOP]`
5. **Dependency Inversion (DIP)** — flag a high-level module importing or
   constructing a concrete low-level dependency directly. Concrete
   constructors and `init` functions are out of scope for this rule.
   `Applies to: [OOP]`

## Severity recap

- Per-finding: `Nit` (or omit) by default.
- Aggregate: if the 75% threshold is crossed, file at least one
  representative finding as `Important` and reference this file.
- Never combine a quality-rules finding with a `Blocking` severity.
  Quality smells do not block the PR.
- Skip per-language inapplicable rules before counting toward the
  threshold.
