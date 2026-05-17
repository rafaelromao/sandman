# ADR-0009: Stabilize container-backed smoke and e2e tests

## Status

accepted

## Context

Sandman relies on container-backed smoke and e2e tests to verify the real agent flows, including image builds, container startup, worktree orchestration, and PR creation. These suites are intentionally high-fidelity: they run the CLI against real container runtimes and real agent binaries.

During this session, those suites exposed a cluster of failures rather than one isolated bug:

- Smoke preflight mounted `/.cache` twice, once via tmpfs and once via config mounts, which caused container startup to fail.
- Smoke preflight used `sh -lc`, which reset `PATH` and hid `mise` shims that were needed for the Go preset.
- The scaffolded container image placed `mise` state and cache under `/.local` and `/.cache`, which were also tmpfs-mounted by runtime startup and therefore hid installed toolchains.
- Container-backed config mounting flattened nested host paths, breaking agent config resolution for directories like `~/.config/opencode`.
- The shared-container PR-flow path restored preserved worktree metadata too early, which raced concurrent runs.

The immediate symptom was that smoke and e2e tests passed only by skipping or by falling back to ad hoc host package installation. That made the suites unreliable as a regression signal.

## Decision

We stabilized the suites by aligning test scaffolding, container runtime behavior, and path restoration rules:

- Preserve container mount targets when resolving config mounts, instead of flattening them to basename-only paths.
- Avoid tmpfs collisions by skipping `/.config`, `/.local`, and `/.cache` mounts when those paths are already occupied by user config mounts.
- Move scaffolded `mise` state and cache under `/usr/local/share/mise` so runtime tmpfs mounts do not hide installed toolchains.
- Use plain `sh -c` for smoke preflight checks so the shell does not reset `PATH` and hide `mise` shims.
- Restore preserved worktree `.git` metadata after batch completion, not inside per-run cleanup paths that can race in shared-container mode.
- Add regression coverage for the mount behavior and test helper assumptions.

## Consequences

### Positive

- Smoke and e2e suites now fail for real regressions instead of harness bugs.
- Container-backed tests better reflect the runtime Sandman actually creates for agents.
- Shared-container and preserved-worktree behavior is less fragile under concurrency.

### Negative

- The runtime and scaffold layers now encode more mount/path policy, so future changes need to respect those assumptions.
- The test harness is slightly more opinionated about shell behavior and container layout.

### Neutral

- These changes do not alter the public CLI contract, only the reliability of internal execution paths.
- The temporary `.gitconfig` copy is an implementation detail and should remain invisible to users.

## Postmortem

### What failed

We treated the smoke/e2e harness as if it were just another test layer, but it was actually a second product: a full environment generator for agent execution. Small mount and PATH mistakes broke the entire signal.

### Why it failed

- Mount semantics were under-specified for nested config paths.
- Runtime tmpfs mounts and scaffolded `mise` paths overlapped.
- Shell invocation in tests was too aggressive and rewrote the environment.
- Shared-container cleanup assumed sequential execution when the batch runner can overlap runs.

### What we changed

- Made mount targets explicit and preserved.
- Moved runtime-managed state out of tmpfs-backed paths.
- Tightened preflight assertions to validate the actual environment rather than incidental shell state.
- Moved restore logic to a batch-level boundary.

### Follow-up

- Keep container/backed regression tests focused on environment composition, not just agent output.
- Treat any future `/.cache` or `/.local` mount change as a high-risk change requiring smoke/e2e coverage.
