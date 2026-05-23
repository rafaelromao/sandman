# ADR-0003: Dependency-Aware Batch Execution

## Status

accepted

## Context

Sandman currently executes all AgentRuns in a batch concurrently, subject only to a semaphore limit. There is no concept of ordering or blocking between issues. In practice, some issues cannot be worked on until others are resolved — for example, a refactoring issue must complete before a feature issue that depends on the new structure.

GitHub provides two ways to express these relationships:
1. **Informal body references** — e.g., `Blocked by #42` in the issue description
2. **Native dependency fields** — `issue_dependencies_summary.blocked_by` and related API fields

Without dependency awareness, Sandman may waste compute on issues that will fail or produce invalid branches because their prerequisites are unmet. Worse, agents may produce conflicting changes when working on dependent issues simultaneously.

Alternatives considered:
- **Only body parsing**: Simple, but misses dependencies set via the GitHub UI. Fragile to formatting variations.
- **Only native API**: Reliable for UI-set dependencies, but misses body references that predate the feature or are used in repos without the native feature enabled.
- **Both sources (union)**: Captures the maximum dependency graph. More complex, but avoids silent misses.

Dependency execution semantics considered:
- **Completion dependency**: A can start once B finishes, regardless of B's success. Risk: A runs against a broken state.
- **Success dependency**: A can start only if B succeeds. If B fails, A is skipped with status `"blocked"`. More conservative, avoids wasted work.
- **State dependency**: A can start only if B is closed on GitHub (by any means). Requires polling GitHub state mid-batch, which is complex and race-prone.

Batch expansion behavior considered:
- **Strict mode (default)**: Error out if any blocker is not in the requested batch. Forces the user to be explicit.
- **Auto-expand mode (`--include-dependencies`)**: Recursively fetch and include transitive blockers. Convenient, but may surprise users with a larger batch than requested.

## Decision

We will implement dependency-aware batch execution with the following rules:

1. **Dual-source dependency detection**: Sandman parses both the issue body (for phrases like `blocked by`, `depends on`, `blocked-by` followed by `#<number>`) and queries the GitHub REST API for native `blocked_by` relationships. The union of both sources forms the `BlockedBy` set for each issue.

2. **Success dependency semantics**: An AgentRun for issue A cannot start until every issue in A's `BlockedBy` set has an AgentRun that completed with status `"success"`. If any blocker fails, A is skipped with status `"blocked"` and a `run.blocked` event is logged.

3. **Strict mode by default**: When resolving the batch, if any blocker is outside the initially requested set of issues, `RunBatch` returns an error listing the missing blockers. This forces explicit batch composition.

4. **`--include-dependencies` flag**: When provided, Sandman recursively fetches all transitive blockers, expands the batch to include them, topologically sorts the full graph, and executes the expanded batch.

5. **Cycle detection**: Before execution begins, Sandman runs a DFS cycle check on the full dependency graph. If a cycle is detected, the batch errors out immediately.

6. **Topological ordering with parallelism**: Issues at the same dependency level still run concurrently, respecting the existing `--parallel` semaphore. Only dependency edges enforce serial ordering.

## Consequences

### Positive

- Eliminates wasted agent runs on issues whose prerequisites failed.
- Allows users to express dependencies in whichever GitHub feature they prefer (body text or native fields).
- Preserves existing parallelism for independent issues.

### Negative

- `FetchIssue` must now make additional API calls, increasing latency and rate-limit exposure.
- Cycle detection and topological sort add complexity to the batch orchestrator.
- `--include-dependencies` may silently expand a small batch into a very large one.

### Neutral

- The `Issue` struct gains a `BlockedBy []int` field.
- `AgentRunResult.Status` gains `"blocked"` as a valid value.
