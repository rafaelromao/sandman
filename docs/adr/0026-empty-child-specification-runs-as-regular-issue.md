# ADR-0026: Empty-child Specification runs as a regular issue

## Status

proposed

## Context

ADR-0025 introduced Specification expansion: Sandman detects Specification-shaped issues (body contains `## Problem Statement`, `## Solution`, `## User Stories`), harvests their child issues, and replaces the Specification with its accepted children in the batch. Step 5 of that decision records the "no-children rejection": a Specification whose accepted-child set is empty fails resolution with `no child issues for specification #<n>`.

The rejection was added as a guardrail — the original intent was loud failure rather than silent denial. But the guardrail creates downstream friction in a legitimate workflow: a maintainer who writes a Specification-style ticket to describe a single vertical slice, intending that slice to be the work item itself, cannot run it without either editing the body, hand-copying child numbers, or abandoning the Specification shape entirely. All three defeat the body-as-source-of-truth convention ADR-0025 leans on.

The broadened-detector carve-out at `internal/batch/spec.go:213–230` already implements an analogous pass-through for the non-spec-shaped case: when `IsSpecification(body)` is false but `HasChildren(ctx, n)` is true, and `ListSubIssues` returns zero results, the issue runs as a regular issue via `addUnique(num)` without error. The asymmetry is principled — the strict-spec path is louder because the body shape is misleading when it claims children but delivers none — but the operational outcome (a single-row batch) is the same.

This ADR softens the strict-spec guardrail at ADR-0025 §5: when a Specification-shaped issue has zero accepted children, it runs as a regular issue instead of failing resolution. The broadened-detector carve-out is unchanged.

## Decision

When `IsSpecification(body)` is true and `collectAcceptedChildren` returns an empty accepted set, the resolver falls through to running the issue itself as a regular, non-expanded batch row:

1. **Carve-out location** — `expandOne` (`internal/batch/spec.go:191`), after the call to `collectAcceptedChildren` returns.
2. **`collectAcceptedChildren` contract** — refactors to return `(nil, nil)` for both empty-child conditions (no candidates harvested; every candidate dropped at `## Parent` verification). Hard errors (fetch failures, context cancellation) retain their existing error returns. The function's contract becomes "report what was accepted"; the policy of what to do when accepted is empty moves to the call site that already owns the broadened-detector carve-out policy.
3. **`addUnique(num)` call** — the issue number is included in the output row set via `addUnique`.
4. **Log line** — `running specification #<n> as a regular issue (no children)` is written to the warning writer, giving operators a visible, grep-able record of the fall-through.
5. **Recursive consistency** — the carve-out fires at every depth where the strict-spec body shape is true and the harvest is empty. Same log line, same grep target, whether the empty-child spec is the user's typed input or a transitive candidate.

The broadened-detector silent pass-through at `spec.go:222` and `spec.go:225` is unchanged. The asymmetry (strict-spec logs; broadened-detector is silent) is intentional: the strict-spec log line is diagnostic when the body shape is misleading; the broadened-detector silent pass-through is the correct default when the body makes no spec claim.

## Alternatives Considered

- **Keep hard-error** — Rejecting with `no child issues for specification #<n>` is loud and unambiguous, but forces the operator to edit the Specification body or hand-copy child numbers. The guardrail's original intent (loud failure, not denial) is better served by a visible log line and a regular run.
- **Warn-and-skip** — Log a warning and drop the issue from the batch silently. This would suppress the issue entirely rather than running it, which defeats the single-slice Specification use case.
- **Flag-gated behavior** — A `--strict-spec` flag that preserves the hard-error path. Correctly isolates operators who want the old behavior, but adds a flag axis that most operators will never flip. The carve-out is the better default; a future ADR can revisit flag-gating if real operators express the need.

## Consequences

### Positive

- A Specification that describes a single vertical slice is now a first-class Sandman input. Maintainers can write `sandman run #<spec>` and get a run, not an error.
- Symmetry with the broadened-detector carve-out: both paths now produce a single-row batch when the child harvest is empty, rather than diverging in outcome.
- The log line `running specification #<n> as a regular issue (no children)` makes the fall-through visible in operator logs without being surprising.

### Negative

- `collectAcceptedChildren` refactors from returning a hard error for empty-child cases to returning `(nil, nil)`. Callers that previously used the error to detect the empty-child condition must adapt to the new contract.
- Two distinct log shapes for empty-child cases: strict-spec produces `running specification #<n> as a regular issue (no children)`, while broadened-detector is silent (by design). Operators scanning for empty-child runs grep only the strict-spec log line.

### Neutral

- `CONTEXT.md` `SpecificationResolver` entry (currently: "fails the resolution") is updated inline to: "runs as a single-row batch, logged as `running specification #<n> as a regular issue (no children)`, per ADR-0026". No new glossary term is introduced.
- ADR-0025 §5 remains the authoritative source for the original guardrail language being softened; this ADR is the authoritative source for the new carve-out behavior.
