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
- `max_containers=0` means no cap: the orchestrator grows the container pool without bound to accommodate active AgentRuns

Keeping the old shared-versus-isolated vocabulary would mislead users into thinking there are still two distinct container products or flags, when in practice there is one container-backed sandbox strategy with tunable scheduling limits.

## Decision

We will describe container-backed sandboxing in terms of **ContainerCapacity** and **MaxContainers** instead of separate shared and isolated modes.

Specifically:

1. `ContainerCapacity` defines the maximum concurrent AgentRuns per ContainerSandbox.
   `ContainerCapacity=0` means unlimited mode: any number of AgentRuns may execute concurrently inside one ContainerSandbox.
2. `MaxContainers` defines the maximum number of ContainerSandboxes available to one Batch.
3. `max_containers=0` means no cap: the orchestrator grows the container pool without bound to accommodate active AgentRuns. An explicit positive value caps the total number of ContainerSandboxes.
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

## Post-acceptance note (2026-06-03)

Documentation prior to this update was inconsistent about the semantics of `container_capacity=0` and `max_containers=0`:

- `container_capacity=0` was described as "auto/default mode" in some docs and "unlimited" in others. The code behavior is unlimited: when `capacity == 0`, the pool acquire loop skips the per-container cap check (`p.capacity > 0 && entry.active >= p.capacity` evaluates false), allowing any number of concurrent runs per container.
- `max_containers=0` was described as "auto mode: create the minimum needed" in some docs and "auto-scale to minimum" in others. The code behavior is no cap: when `maxContainers == 0`, the condition `p.maxContainers == 0 || len(p.shared) < p.maxContainers` is always true, so the pool grows without bound.

This ADR formally records the chosen semantics:

- `container_capacity=0` → **unlimited** (no per-container cap; matches code behavior)
- `max_containers=0` → **no cap** (orchestrator grows the container pool without bound)

The contradiction between the old docs and the code has been resolved in favor of the code behavior.
