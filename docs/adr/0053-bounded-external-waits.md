# ADR-0053: Bound external waits and release execution capacity

## Status

accepted

## Context

An open pull request can wait on CI or delegated review after its implementation
agent exits. Holding an execution slot during that external work prevents
independent rows from progressing, while releasing the row itself would allow
dependents to start before their prerequisite terminalizes.

## Decision

Keep logical row ownership until a terminal lifecycle result, but release the
execution slot between external observations. Persist CI wait identity,
deadline, and remediation attempts per pull-request head. CI and review keep
independent deadlines; the earlier deadline controls the next remediation.
Only CI, CI-deadline, and merge-conflict remediation consume the CI budget.
When an external poll interval elapses, the awaiting row joins a FIFO priority
queue. A ready awaited row is selected before ordinary queued rows for the next
permitted free slot, without preempting executing rows or bypassing effective
parallelism, container capacity, or start delay.

## Consequences

Independent rows can use released capacity while dependents remain queued.
External waits are bounded and restart-safe. A same-head remediation budget can
terminalize deterministically instead of polling forever. Ready awaited rows
resume promptly while logical dependency ownership remains held until the row's
terminal lifecycle outcome.
