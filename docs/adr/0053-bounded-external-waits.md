# ADR-0053: Bound external waits and release execution capacity

## Status

proposed

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

## Consequences

Independent rows can use released capacity while dependents remain queued.
External waits are bounded and restart-safe. A same-head remediation budget can
terminalize deterministically instead of polling forever.
