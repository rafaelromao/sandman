# ADR-0002: Make shared container execution the default sandbox mode

## Status

accepted

## Context

Sandman supports three sandbox modes:

- **Worktree**: git worktrees with no container isolation. The most relaxed option.
- **Shared container**: a single ContainerSandbox hosts multiple Worktrees, one per AgentRun. The middle ground.
- **Isolated containers**: one container per AgentRun. The strongest isolation, highest overhead.

Originally, `worktree` was the default. This was the simplest option but provided no process or filesystem isolation beyond git's own worktree mechanism. As Sandman evolved, the shared container mode emerged as the sensible default: it provides isolation without the overhead of a container per run, and it avoids the risks of running untrusted agent code directly on the host.

## Decision

We will make **shared container execution** the default sandbox mode:

1. Change `DefaultSandbox` in `internal/config/config.go` from `"worktree"` to `"podman"`.
2. Update the scaffolder to generate `sandbox: podman` in newly created `.sandman/config.yaml`.
3. Introduce `sandbox.ResolveRuntime` to detect the available container runtime at startup:
   - Primary: `podman`
   - Fallback: `docker` (if podman is not installed)
   - If neither is installed: fail fast with a clear error
   - `worktree` can only be selected explicitly (no silent fallback)
4. Update CLI help text to reflect Podman as the default.
5. Update README.md and CONTEXT.md to document the new default.

## Consequences

### Positive

- Better out-of-the-box isolation: new users get container sandboxing by default.
- Lower overhead than isolated containers: one container per batch instead of one per issue.
- Graceful degradation: if podman is missing, docker is tried automatically.
- Clear failure mode: if no container runtime is available, the user gets an explicit error with instructions.

### Negative

- Users without podman or docker must explicitly opt into `sandbox: worktree`.
- Existing configs without an explicit `sandbox` key will switch to container mode on the next run, which may surprise users who do not have a container runtime installed.
- The `batch` end-to-end test suite now requires mocking or explicitly requesting worktree mode to avoid depending on a real container runtime.

### Neutral

- `--isolated-containers` remains opt-in and unaffected.
- The `worktree` mode remains fully supported for users who prefer it.
