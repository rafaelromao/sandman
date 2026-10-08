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
deadline, and diagnostic remediation attempts per pull-request head. Managed CI
keeps its 30-minute hard deadline; delegated review keeps its configured
confirmed-request deadline. The earlier deadline controls the next remediation.
Bound owned work by the historical three in-session lifecycle relaunches (entry
excluded) and configured ordinary retries, rather than durable cumulative repair
exhaustion. A fresh executor gets a fresh relaunch allowance; old repair ledgers
cannot reject its entry. Standalone CI retains 60 minutes and three fixes per
current head within one invocation.

Recognized implementation quota waits retain five hours of accumulated completed
ten-minute polling across re-entry/restart, followed by the final boundary probe
and configured fresh-session ordinary retries. Capacity delay and downtime do
not consume polling. Persisted expected exhaustion is diagnostic, not a hard
operation deadline. Quota recovery retains the same five-minute ownerless grace;
normalizing legacy estimates does not renew that grace. Reviewer launches retain
backoff, request/head artifact claims, and durable decision publication without
a cumulative three-launch exhaustion gate. These boundaries restore the
[pre-week exit baseline](../development/afk-exit-regression-baseline.md).

When an external poll interval elapses, the awaiting row joins a FIFO priority
queue when its opportunity is eligible. A row that received an execution chance
less than 10 minutes ago yields to ordinary queued rows unless no ordinary row
is queued or an ordinary row has started since that chance. An exactly
10-minute-old chance is eligible. An eligible awaited row is selected before
ordinary queued rows for the next permitted free slot, without preempting
executing rows or bypassing effective parallelism, container capacity, or start
delay.

## Consequences

Independent rows can use released capacity while dependents remain queued.
External waits remain bounded and restart-safe, independently of repair launch
counts. Terminal exits still follow historical retry, in-session relaunch,
review-pass, timeout, authentication, conflict, and verification rules; durable
repair bookkeeping adds no new terminal prerequisite. Eligible awaited rows
resume promptly while logical dependency ownership remains held until the row's
terminal lifecycle outcome; the cooldown prevents a group of awaiting rows from
starving ordinary queued work.
