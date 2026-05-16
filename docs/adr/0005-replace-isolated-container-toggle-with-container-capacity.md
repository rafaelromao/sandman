# ADR-0005: Replace isolated container toggle with container capacity

## Status

accepted

## Context

Sandman's container-backed sandboxing was previously described as two user-facing modes:

- shared container mode: multiple AgentRuns execute in one ContainerSandbox
- isolated container mode: one ContainerSandbox per AgentRun

That framing matched an earlier boolean toggle, but it no longer describes the runtime behavior precisely enough.

The orchestrator now needs to express two independent constraints:

- how many AgentRuns may execute concurrently inside one ContainerSandbox
- how many ContainerSandboxes a Batch may create in total

Those constraints are better modeled as capacity and pool limits than as a binary isolated-versus-shared choice. They also match the actual scheduling behavior more closely:

- a container may host more than one AgentRun, but only up to a configured per-container capacity
- when demand exceeds available capacity, AgentRuns queue until capacity frees up or another container is created
- container pooling is scoped to a single Batch; idle containers may be reused by later AgentRuns in that Batch and are stopped when the Batch completes
- `max_containers=0` means auto mode, where Sandman scales the pool up to the minimum number of containers needed for the currently active AgentRuns

Keeping the old shared-versus-isolated vocabulary would mislead users into thinking there are still two distinct container products or flags, when in practice there is one container-backed sandbox strategy with tunable scheduling limits.

## Decision

We will describe container-backed sandboxing in terms of **ContainerCapacity** and **MaxContainers** instead of separate shared and isolated modes.

Specifically:

1. `ContainerCapacity` defines the maximum concurrent AgentRuns per ContainerSandbox.
2. `MaxContainers` defines the maximum number of ContainerSandboxes available to one Batch.
3. `max_containers=0` means auto mode: Sandman may create up to the minimum number of containers needed for the active AgentRuns, given the configured ContainerCapacity.
4. User-facing domain documentation will treat container pooling as batch-scoped reuse rather than a cross-batch warm pool.
5. User-facing documentation will stop presenting `--isolated-containers` or separate shared-versus-isolated container modes as current concepts.

## Consequences

### Positive

- The domain language matches runtime behavior more closely.
- Users can reason about throughput, queueing, and resource limits using explicit terms.
- The model scales naturally from one-container batches to multi-container batches without introducing extra modes.
- Batch-scoped reuse is explicit, which avoids implying cross-batch container lifetime guarantees that Sandman does not make.

### Negative

- Users familiar with the old isolated-versus-shared language must learn a more scheduler-oriented model.
- Some older discussions and ADRs will remain historically accurate but use superseded vocabulary.

### Neutral

- `sandbox: worktree` remains a separate non-container sandbox strategy.
- This ADR changes domain language and architecture guidance; it does not, by itself, require cross-batch container reuse.
