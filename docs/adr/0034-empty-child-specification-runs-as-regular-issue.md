# ADR-0034: Empty-child Specification runs as a regular issue

## Status

accepted; amended to restore the broadened-detector silent pass-through (see §Decision step 4)

## Context

ADR-0021 introduced Specification expansion: Sandman detects Specifications by their body shape (canonical sections or children declarations), harvests their child issues, and replaces the Specification with its accepted children in the batch. Step 5 of that decision records the "no-children rejection": a Specification whose accepted-child set is empty fails resolution with `no child issues for specification #<n>`.

The rejection was added as a guardrail — the original intent was loud failure rather than silent denial. But the guardrail creates downstream friction in a legitimate workflow: a maintainer who writes a Specification-style Issue to describe a single vertical slice, intending that slice to be the work item itself, cannot run it without either editing the body, hand-copying child numbers, or abandoning the Specification shape entirely. All three defeat the body-as-source-of-truth convention ADR-0021 leans on.

The broadened-detector carve-out at `internal/batch/spec.go` already implements an analogous pass-through for the non-spec-shaped case: when `IsSpecification(body)` is false and `HasChildren(ctx, n)` is false (or errored), and `ListSubIssues` returns zero results, the issue runs as a regular issue via `addUnique(num)` and emits no log line. The asymmetry is principled — the strict-spec path is louder because the body shape is misleading when it claims children but delivers none, while a non-spec-shaped body has made no such claim — but the operational outcome (a single-row batch) is the same.

This ADR softens the strict-spec guardrail at ADR-0021 §5: when a Specification-shaped issue has zero accepted children, it runs as a regular issue instead of failing resolution. The broadened-detector carve-out is unchanged.

## Decision

When `IsSpecification(body)` is true and `collectAcceptedChildren` returns an empty accepted set, the resolver falls through to running the issue itself as a regular, non-expanded batch row:

1. **Carve-out location** — `expandOne` (`internal/batch/spec.go`), after the call to `collectAcceptedChildren` returns.
2. **`collectAcceptedChildren` contract** — refactors to return `(nil, nil)` for both empty-child conditions (no candidates harvested; every candidate dropped at `## Parent` verification). Hard errors (fetch failures, context cancellation) retain their existing error returns. The function's contract becomes "report what was accepted"; the policy of what to do when accepted is empty moves to the call site that already owns the broadened-detector carve-out policy.
3. **`addUnique(num)` call** — the issue number is included in the output row set via `addUnique`.
4. **Log line** (strict-spec only) — the strict-spec path emits `running issue #<n> as a regular issue (no children)` to the warning writer when `IsSpecification(body)` is true but the accepted-child harvest is empty, giving operators a visible, grep-able record of the misleading-body fall-through. The broadened-detector path is silent: a non-spec-shaped body has made no children claim, so its pass-through is not surprising and needs no operator-visible log line.
5. **Recursive consistency** — the carve-out fires at every depth where the harvest is empty. Same log line, same grep target, whether the empty-child spec is the user's typed input or a transitive candidate.

The broadened-detector pass-through is silent. Operators grep the strict-spec path's log line for body-shape mismatches; range-selected or label-selected inputs that the broadened detector correctly runs as flat issues produce no log noise.

## Alternatives Considered

- **Keep hard-error** — Rejecting with `no child issues for specification #<n>` is loud and unambiguous, but forces the operator to edit the Specification body or hand-copy child numbers. The guardrail's original intent (loud failure, not denial) is better served by a visible log line and a regular run.
- **Warn-and-skip** — Log a warning and drop the issue from the batch silently. This would suppress the issue entirely rather than running it, which defeats the single-slice Specification use case.
- **Flag-gated behavior** — A `--strict-spec` flag that preserves the hard-error path. Correctly isolates operators who want the old behavior, but adds a flag axis that most operators will never flip. The carve-out is the better default; a future ADR can revisit flag-gating if real operators express the need.

## Consequences

### Positive

- A Specification that describes a single vertical slice is now a first-class Sandman input. Maintainers can write `sandman run #<n>` and get a run, not an error.
- Symmetry of outcome with the broadened-detector carve-out: both paths now produce a single-row batch when the child harvest is empty, rather than diverging in outcome. The asymmetry is the log line — only the strict-spec path emits one.
- The strict-spec log line `running issue #<n> as a regular issue (no children)` makes the misleading-body fall-through visible in operator logs without being surprising; the broadened-detector pass-through stays silent so range- and label-selected inputs that the detector correctly leaves flat do not produce log noise.

### Negative

- `collectAcceptedChildren` refactors from returning a hard error for empty-child cases to returning `(nil, nil)`. Callers that previously used the error to detect the empty-child condition must adapt to the new contract.
- The #2329 unification of the strict-spec and broadened-detector log lines is reverted: the broadened-detector path is silent again. Only the strict-spec path emits `running issue #<n> as a regular issue (no children)`; operators scanning for body-shape mismatches grep that single line and use the broader `expanded specification #<n>` / `flattened specification #<n>` lines for the accepted-children cases.

### Neutral

- `CONTEXT.md` `SpecificationResolver` entry was updated to reflect the new carve-out behaviour. No new glossary term is introduced.
- ADR-0021 §5 remains the authoritative source for the original guardrail language being softened; this ADR is the authoritative source for the carve-out behavior.
- Issue #2329 further refines the contract: the body-shape gate is gone; the canonical-shape sections remain a valid spec signal alongside the children-content check. (The accompanying log-line unification from #2329 is reverted by this amendment — see step 4.)
